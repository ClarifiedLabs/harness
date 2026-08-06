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

// clientCapabilitiesJSON advertises the agent-relevant request surface exposed
// by the shim. UI-only LSP features (semantic coloring, folding, selection
// ranges, document colors) are deliberately absent. The client accepts snippet
// completions as text but never executes server commands returned by actions.
const clientCapabilitiesJSON = `{
  "general": {"positionEncodings": ["utf-16"]},
  "textDocument": {
    "synchronization": {"didSave": false, "willSave": false},
    "hover": {"contentFormat": ["plaintext", "markdown"]},
    "declaration": {"linkSupport": true},
    "definition": {"linkSupport": true},
    "typeDefinition": {"linkSupport": true},
    "implementation": {"linkSupport": true},
    "references": {},
    "signatureHelp": {"signatureInformation": {"documentationFormat": ["plaintext", "markdown"], "parameterInformation": {"labelOffsetSupport": true}, "activeParameterSupport": true}},
    "completion": {"completionItem": {"documentationFormat": ["plaintext", "markdown"], "snippetSupport": true}},
    "documentHighlight": {},
    "documentSymbol": {"hierarchicalDocumentSymbolSupport": true},
    "callHierarchy": {},
    "typeHierarchy": {},
    "inlayHint": {},
    "codeAction": {"dataSupport": true, "resolveSupport": {"properties": ["edit"]}},
    "formatting": {},
    "rangeFormatting": {},
    "rename": {},
    "publishDiagnostics": {}
  },
  "workspace": {"symbol": {"resolveSupport": {"properties": ["location.range"]}}, "workspaceFolders": true, "configuration": true, "applyEdit": false}
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
	return c.locations(ctx, "textDocument/definition", uri, pos)
}

// Declaration resolves textDocument/declaration at pos.
func (c *lspClient) Declaration(ctx context.Context, uri string, pos Position) ([]Location, error) {
	return c.locations(ctx, "textDocument/declaration", uri, pos)
}

// TypeDefinition resolves textDocument/typeDefinition at pos.
func (c *lspClient) TypeDefinition(ctx context.Context, uri string, pos Position) ([]Location, error) {
	return c.locations(ctx, "textDocument/typeDefinition", uri, pos)
}

// Implementation resolves textDocument/implementation at pos.
func (c *lspClient) Implementation(ctx context.Context, uri string, pos Position) ([]Location, error) {
	return c.locations(ctx, "textDocument/implementation", uri, pos)
}

func (c *lspClient) locations(ctx context.Context, method, uri string, pos Position) ([]Location, error) {
	raw, err := jsonCall(ctx, c.peer, method, TextDocumentPositionParams{
		TextDocument: TextDocumentIdentifier{URI: uri},
		Position:     pos,
	})
	if err != nil {
		return nil, err
	}
	return parseLocations(raw)
}

// SignatureHelp resolves callable signatures at pos.
func (c *lspClient) SignatureHelp(ctx context.Context, uri string, pos Position) (*SignatureHelp, error) {
	raw, err := jsonCall(ctx, c.peer, "textDocument/signatureHelp", TextDocumentPositionParams{
		TextDocument: TextDocumentIdentifier{URI: uri}, Position: pos,
	})
	if err != nil {
		return nil, err
	}
	if string(raw) == "null" {
		return nil, nil
	}
	var help SignatureHelp
	if err := json.Unmarshal(raw, &help); err != nil {
		return nil, fmt.Errorf("lspproxy: decode signature help: %w", err)
	}
	return &help, nil
}

// Completion resolves completion candidates at pos.
func (c *lspClient) Completion(ctx context.Context, uri string, pos Position) ([]CompletionItem, error) {
	raw, err := jsonCall(ctx, c.peer, "textDocument/completion", CompletionParams{
		TextDocument: TextDocumentIdentifier{URI: uri}, Position: pos,
	})
	if err != nil {
		return nil, err
	}
	return parseCompletionItems(raw)
}

// DocumentHighlights resolves occurrences in the current document.
func (c *lspClient) DocumentHighlights(ctx context.Context, uri string, pos Position) ([]DocumentHighlight, error) {
	raw, err := jsonCall(ctx, c.peer, "textDocument/documentHighlight", TextDocumentPositionParams{
		TextDocument: TextDocumentIdentifier{URI: uri}, Position: pos,
	})
	if err != nil {
		return nil, err
	}
	var out []DocumentHighlight
	if string(raw) == "null" {
		return nil, nil
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("lspproxy: decode document highlights: %w", err)
	}
	return out, nil
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

// PrepareCallHierarchy locates the hierarchy item at pos.
func (c *lspClient) PrepareCallHierarchy(ctx context.Context, uri string, pos Position) ([]CallHierarchyItem, error) {
	raw, err := jsonCall(ctx, c.peer, "textDocument/prepareCallHierarchy", TextDocumentPositionParams{
		TextDocument: TextDocumentIdentifier{URI: uri}, Position: pos,
	})
	if err != nil {
		return nil, err
	}
	var out []CallHierarchyItem
	if string(raw) == "null" {
		return nil, nil
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("lspproxy: decode call hierarchy item: %w", err)
	}
	return out, nil
}

func (c *lspClient) IncomingCalls(ctx context.Context, item CallHierarchyItem) ([]CallHierarchyIncomingCall, error) {
	raw, err := jsonCall(ctx, c.peer, "callHierarchy/incomingCalls", map[string]any{"item": item})
	if err != nil {
		return nil, err
	}
	var out []CallHierarchyIncomingCall
	if string(raw) == "null" {
		return nil, nil
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("lspproxy: decode incoming calls: %w", err)
	}
	return out, nil
}

func (c *lspClient) OutgoingCalls(ctx context.Context, item CallHierarchyItem) ([]CallHierarchyOutgoingCall, error) {
	raw, err := jsonCall(ctx, c.peer, "callHierarchy/outgoingCalls", map[string]any{"item": item})
	if err != nil {
		return nil, err
	}
	var out []CallHierarchyOutgoingCall
	if string(raw) == "null" {
		return nil, nil
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("lspproxy: decode outgoing calls: %w", err)
	}
	return out, nil
}

func (c *lspClient) PrepareTypeHierarchy(ctx context.Context, uri string, pos Position) ([]TypeHierarchyItem, error) {
	raw, err := jsonCall(ctx, c.peer, "textDocument/prepareTypeHierarchy", TextDocumentPositionParams{
		TextDocument: TextDocumentIdentifier{URI: uri}, Position: pos,
	})
	if err != nil {
		return nil, err
	}
	var out []TypeHierarchyItem
	if string(raw) == "null" {
		return nil, nil
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("lspproxy: decode type hierarchy item: %w", err)
	}
	return out, nil
}

func (c *lspClient) TypeHierarchy(ctx context.Context, direction string, item TypeHierarchyItem) ([]TypeHierarchyItem, error) {
	method := "typeHierarchy/supertypes"
	if direction == "subtypes" {
		method = "typeHierarchy/subtypes"
	}
	raw, err := jsonCall(ctx, c.peer, method, map[string]any{"item": item})
	if err != nil {
		return nil, err
	}
	var out []TypeHierarchyItem
	if string(raw) == "null" {
		return nil, nil
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("lspproxy: decode type hierarchy: %w", err)
	}
	return out, nil
}

func (c *lspClient) InlayHints(ctx context.Context, uri string, r Range) ([]InlayHint, error) {
	raw, err := jsonCall(ctx, c.peer, "textDocument/inlayHint", DocumentRangeParams{
		TextDocument: TextDocumentIdentifier{URI: uri}, Range: r,
	})
	if err != nil {
		return nil, err
	}
	var out []InlayHint
	if string(raw) == "null" {
		return nil, nil
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("lspproxy: decode inlay hints: %w", err)
	}
	return out, nil
}

func (c *lspClient) CodeActions(ctx context.Context, uri string, r Range, diagnostics []Diagnostic, only []string) ([]CodeAction, error) {
	raw, err := jsonCall(ctx, c.peer, "textDocument/codeAction", CodeActionParams{
		TextDocument: TextDocumentIdentifier{URI: uri}, Range: r,
		Context: CodeActionContext{Diagnostics: diagnostics, Only: only},
	})
	if err != nil {
		return nil, err
	}
	return parseCodeActions(raw)
}

func (c *lspClient) ResolveCodeAction(ctx context.Context, action CodeAction) (CodeAction, error) {
	raw, err := jsonCall(ctx, c.peer, "codeAction/resolve", action)
	if err != nil {
		return CodeAction{}, err
	}
	var out CodeAction
	if err := json.Unmarshal(raw, &out); err != nil {
		return CodeAction{}, fmt.Errorf("lspproxy: decode resolved code action: %w", err)
	}
	return out, nil
}

func (c *lspClient) Formatting(ctx context.Context, uri string, r *Range, options FormattingOptions) ([]TextEdit, error) {
	var (
		raw json.RawMessage
		err error
	)
	if r == nil {
		raw, err = jsonCall(ctx, c.peer, "textDocument/formatting", DocumentFormattingParams{
			TextDocument: TextDocumentIdentifier{URI: uri}, Options: options,
		})
	} else {
		raw, err = jsonCall(ctx, c.peer, "textDocument/rangeFormatting", DocumentRangeFormattingParams{
			TextDocument: TextDocumentIdentifier{URI: uri}, Range: *r, Options: options,
		})
	}
	if err != nil {
		return nil, err
	}
	return parseTextEdits(raw)
}

// Diagnostics returns the latest pushed diagnostics without waiting.
func (c *lspClient) Diagnostics(uri string) []Diagnostic {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]Diagnostic(nil), c.diags[uri]...)
}

// Rename resolves textDocument/rename and returns the resulting cross-file edits
// (the shim never applies them; the caller renders a plan).
func (c *lspClient) Rename(ctx context.Context, uri string, pos Position, newName string) ([]FileEdits, error) {
	raw, err := c.renameRaw(ctx, uri, pos, newName)
	if err != nil {
		return nil, err
	}
	return parseWorkspaceEdit(raw)
}

// RenameTextEdits resolves textDocument/rename and rejects WorkspaceEdit file
// operations. It is used by the mutating tool, which only applies text edits.
func (c *lspClient) RenameTextEdits(ctx context.Context, uri string, pos Position, newName string) ([]FileEdits, error) {
	raw, err := c.renameRaw(ctx, uri, pos, newName)
	if err != nil {
		return nil, err
	}
	return parseWorkspaceEditStrict(raw)
}

func (c *lspClient) renameRaw(ctx context.Context, uri string, pos Position, newName string) (json.RawMessage, error) {
	return jsonCall(ctx, c.peer, "textDocument/rename", RenameParams{
		TextDocument: TextDocumentIdentifier{URI: uri},
		Position:     pos,
		NewName:      newName,
	})
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
