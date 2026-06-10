package lspproxy

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"sync"
	"time"

	"harness/internal/buildinfo"
	"harness/internal/mcp/jsonrpc"
)

// defaultPositionEncoding is what LSP assumes when the server advertises none.
const defaultPositionEncoding = "utf-16"

// clientCapabilitiesJSON is the minimal set of client capabilities the shim
// advertises. It requests UTF-16 columns (the shim converts), plaintext-or-
// markdown hover, link-style definitions, hierarchical document symbols, and
// publishDiagnostics, and tells the server the shim answers configuration and
// workspaceFolders callbacks.
const clientCapabilitiesJSON = `{
  "general": {"positionEncodings": ["utf-16"]},
  "textDocument": {
    "synchronization": {"didSave": false, "willSave": false},
    "hover": {"contentFormat": ["plaintext", "markdown"]},
    "definition": {"linkSupport": true},
    "references": {},
    "documentSymbol": {"hierarchicalDocumentSymbolSupport": true},
    "rename": {},
    "publishDiagnostics": {}
  },
  "workspace": {"symbol": {}, "workspaceFolders": true, "configuration": true}
}`

// lspClient drives one language-server child over the LSP wire protocol. It owns
// a jsonrpc.Peer over the Content-Length codec and tracks the negotiated
// position encoding.
type lspClient struct {
	peer   *jsonrpc.Peer
	root   string
	logger *slog.Logger

	mu               sync.Mutex
	positionEncoding string
	diags            map[string][]Diagnostic
	seen             map[string]bool // uri → a publish arrived since MarkDocPending
	diagSignal       chan struct{}   // closed+replaced on each publishDiagnostics
}

// newClient wraps conn (the child's stdio) in an lspClient. root is the
// workspace root, used for rootUri and the workspaceFolders callback. It does
// not perform the handshake; call Initialize.
func newClient(conn io.ReadWriteCloser, root string, logger *slog.Logger) *lspClient {
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	c := &lspClient{
		root:             root,
		logger:           logger,
		positionEncoding: defaultPositionEncoding,
		diags:            make(map[string][]Diagnostic),
		seen:             make(map[string]bool),
		diagSignal:       make(chan struct{}),
	}
	c.peer = jsonrpc.NewPeerWithCodec(conn, NewDecoder(conn), NewEncoder(conn), jsonrpc.PeerOptions{
		Handlers: map[string]jsonrpc.Handler{
			// Servers block on these callbacks; an unanswered one stalls the server,
			// so each gets a benign default reply.
			"workspace/configuration":        c.handleConfiguration,
			"workspace/workspaceFolders":     c.handleWorkspaceFolders,
			"client/registerCapability":      replyNull,
			"client/unregisterCapability":    replyNull,
			"window/showMessageRequest":      replyNull,
			"window/workDoneProgress/create": replyNull,
		},
		Notifications: map[string]jsonrpc.NotificationHandler{
			"textDocument/publishDiagnostics": c.handlePublishDiagnostics,
			// window/logMessage, window/showMessage, $/progress, telemetry/event and
			// the like are intentionally unhandled; the peer tolerates unknown
			// notifications.
		},
		Logger: logger,
	})
	return c
}

// handleConfiguration answers workspace/configuration with one null per
// requested item, meaning "no configuration; use your defaults."
func (c *lspClient) handleConfiguration(ctx context.Context, params json.RawMessage) (json.RawMessage, *jsonrpc.Error) {
	var p struct {
		Items []json.RawMessage `json:"items"`
	}
	_ = json.Unmarshal(params, &p)
	nulls := make([]json.RawMessage, len(p.Items))
	for i := range nulls {
		nulls[i] = json.RawMessage("null")
	}
	out, _ := json.Marshal(nulls)
	return out, nil
}

// handleWorkspaceFolders answers with the single root folder the shim drives.
func (c *lspClient) handleWorkspaceFolders(ctx context.Context, params json.RawMessage) (json.RawMessage, *jsonrpc.Error) {
	out, _ := json.Marshal([]WorkspaceFolder{{URI: uriForPath(c.root), Name: workspaceName(c.root)}})
	return out, nil
}

// replyNull answers an inbound request with null (accept-and-ignore).
func replyNull(ctx context.Context, params json.RawMessage) (json.RawMessage, *jsonrpc.Error) {
	return json.RawMessage("null"), nil
}

// handlePublishDiagnostics records the latest diagnostics for a URI and wakes any
// waiter.
func (c *lspClient) handlePublishDiagnostics(ctx context.Context, params json.RawMessage) {
	var p PublishDiagnosticsParams
	if err := json.Unmarshal(params, &p); err != nil {
		return
	}
	c.mu.Lock()
	c.diags[p.URI] = p.Diagnostics
	c.seen[p.URI] = true
	close(c.diagSignal)
	c.diagSignal = make(chan struct{})
	c.mu.Unlock()
}

// MarkDocPending clears the "a publish has arrived" flag for uri, so a following
// WaitDiagnostics blocks for the NEXT publish rather than returning a stale one.
// The manager calls it right before didOpen/didChange.
func (c *lspClient) MarkDocPending(uri string) {
	c.mu.Lock()
	delete(c.seen, uri)
	c.mu.Unlock()
}

// WaitDiagnostics blocks until a publishDiagnostics for uri arrives (since the
// last MarkDocPending) or timeout elapses. ok reports whether a publish was
// observed; on timeout it returns the last-known diagnostics (possibly empty)
// with ok=false. err is non-nil only if ctx is cancelled.
func (c *lspClient) WaitDiagnostics(ctx context.Context, uri string, timeout time.Duration) (diags []Diagnostic, ok bool, err error) {
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	for {
		c.mu.Lock()
		seen := c.seen[uri]
		latest := c.diags[uri]
		sig := c.diagSignal
		c.mu.Unlock()
		if seen {
			return latest, true, nil
		}
		select {
		case <-sig:
			// A publish arrived (for some URI); re-check this URI's flag.
		case <-timer.C:
			return latest, false, nil
		case <-ctx.Done():
			return nil, false, ctx.Err()
		}
	}
}

// Initialize runs the LSP initialize/initialized handshake and records the
// server's negotiated position encoding. initOptions, if non-nil, is passed
// verbatim as initializationOptions.
func (c *lspClient) Initialize(ctx context.Context, initOptions json.RawMessage) (*InitializeResult, error) {
	params := InitializeParams{
		ProcessID:             os.Getpid(),
		RootURI:               uriForPath(c.root),
		WorkspaceFolders:      []WorkspaceFolder{{URI: uriForPath(c.root), Name: workspaceName(c.root)}},
		Capabilities:          json.RawMessage(clientCapabilitiesJSON),
		ClientInfo:            ClientInfo{Name: "harness lsp", Version: buildinfo.Version},
		InitializationOptions: initOptions,
	}
	raw, err := jsonCall(ctx, c.peer, "initialize", params)
	if err != nil {
		return nil, err
	}
	var res InitializeResult
	if err := json.Unmarshal(raw, &res); err != nil {
		return nil, fmt.Errorf("lspproxy: decode initialize result: %w", err)
	}
	if enc := res.Capabilities.PositionEncoding; enc != "" {
		c.mu.Lock()
		c.positionEncoding = enc
		c.mu.Unlock()
	}
	if err := c.peer.Notify("initialized", json.RawMessage(`{}`)); err != nil {
		return nil, err
	}
	return &res, nil
}

// DidOpen tells the server a document is open, sending its full text. The shim
// reads text from disk; the manager tracks which documents are open.
func (c *lspClient) DidOpen(uri, languageID string, version int, text string) error {
	return jsonNotify(c.peer, "textDocument/didOpen", DidOpenParams{
		TextDocument: TextDocumentItem{URI: uri, LanguageID: languageID, Version: version, Text: text},
	})
}

// DidChange resyncs a previously-opened document with full text (the shim uses
// full-document sync only).
func (c *lspClient) DidChange(uri string, version int, text string) error {
	return jsonNotify(c.peer, "textDocument/didChange", DidChangeParams{
		TextDocument:   VersionedTextDocumentIdentifier{URI: uri, Version: version},
		ContentChanges: []TextDocumentContentChangeEvent{{Text: text}},
	})
}

// Definition resolves textDocument/definition at pos, normalizing the
// polymorphic result into a flat list of locations.
func (c *lspClient) Definition(ctx context.Context, uri string, pos Position) ([]Location, error) {
	raw, err := jsonCall(ctx, c.peer, "textDocument/definition", TextDocumentPositionParams{
		TextDocument: TextDocumentIdentifier{URI: uri},
		Position:     pos,
	})
	if err != nil {
		return nil, err
	}
	return parseLocations(raw)
}

// References resolves textDocument/references at pos.
func (c *lspClient) References(ctx context.Context, uri string, pos Position, includeDeclaration bool) ([]Location, error) {
	raw, err := jsonCall(ctx, c.peer, "textDocument/references", ReferenceParams{
		TextDocument: TextDocumentIdentifier{URI: uri},
		Position:     pos,
		Context:      ReferenceContext{IncludeDeclaration: includeDeclaration},
	})
	if err != nil {
		return nil, err
	}
	return parseLocations(raw)
}

// Hover resolves textDocument/hover at pos, returning normalized plaintext.
func (c *lspClient) Hover(ctx context.Context, uri string, pos Position) (string, error) {
	raw, err := jsonCall(ctx, c.peer, "textDocument/hover", TextDocumentPositionParams{
		TextDocument: TextDocumentIdentifier{URI: uri},
		Position:     pos,
	})
	if err != nil {
		return "", err
	}
	return parseHoverContents(raw)
}

// DocumentSymbols resolves textDocument/documentSymbol for a whole file.
func (c *lspClient) DocumentSymbols(ctx context.Context, uri string) ([]Symbol, error) {
	raw, err := jsonCall(ctx, c.peer, "textDocument/documentSymbol", DocumentSymbolParams{
		TextDocument: TextDocumentIdentifier{URI: uri},
	})
	if err != nil {
		return nil, err
	}
	return parseSymbols(raw)
}

// WorkspaceSymbols resolves workspace/symbol for a query across the workspace.
func (c *lspClient) WorkspaceSymbols(ctx context.Context, query string) ([]Symbol, error) {
	raw, err := jsonCall(ctx, c.peer, "workspace/symbol", WorkspaceSymbolParams{Query: query})
	if err != nil {
		return nil, err
	}
	return parseSymbols(raw)
}

// Rename resolves textDocument/rename and returns the resulting cross-file edits
// (the shim never applies them; the caller renders a plan).
func (c *lspClient) Rename(ctx context.Context, uri string, pos Position, newName string) ([]FileEdits, error) {
	raw, err := jsonCall(ctx, c.peer, "textDocument/rename", RenameParams{
		TextDocument: TextDocumentIdentifier{URI: uri},
		Position:     pos,
		NewName:      newName,
	})
	if err != nil {
		return nil, err
	}
	return parseWorkspaceEdit(raw)
}

// PositionEncoding returns the negotiated position encoding (utf-16 unless the
// server selected otherwise).
func (c *lspClient) PositionEncoding() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.positionEncoding
}

// Shutdown sends the LSP shutdown request (the graceful-teardown handshake; the
// caller follows with Exit).
func (c *lspClient) Shutdown(ctx context.Context) error {
	_, err := c.peer.Call(ctx, "shutdown", json.RawMessage("null"))
	return err
}

// Exit sends the LSP exit notification, asking the server to terminate.
func (c *lspClient) Exit() error {
	return c.peer.Notify("exit", nil)
}

// Done is closed when the underlying connection ends (server exit, EOF, or
// Close), so a supervisor can detect a dead child.
func (c *lspClient) Done() <-chan struct{} {
	return c.peer.Done()
}

// Close shuts down the underlying peer (and thus the connection).
func (c *lspClient) Close() error {
	return c.peer.Close()
}

// jsonCall marshals params, sends method as a request, and returns the raw
// result. It centralizes the marshal-then-Call boilerplate for typed requests.
func jsonCall(ctx context.Context, peer *jsonrpc.Peer, method string, params any) (json.RawMessage, error) {
	raw, err := json.Marshal(params)
	if err != nil {
		return nil, fmt.Errorf("lspproxy: encode %s params: %w", method, err)
	}
	return peer.Call(ctx, method, raw)
}

// jsonNotify marshals params and sends method as a notification.
func jsonNotify(peer *jsonrpc.Peer, method string, params any) error {
	raw, err := json.Marshal(params)
	if err != nil {
		return fmt.Errorf("lspproxy: encode %s params: %w", method, err)
	}
	return peer.Notify(method, raw)
}
