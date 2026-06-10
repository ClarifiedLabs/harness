package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sync"

	"harness/internal/mcp/jsonrpc"
)

// maxListToolsPages caps tools/list pagination so a buggy server returning a
// never-advancing cursor cannot loop forever.
const maxListToolsPages = 1000

// Client is an MCP client over a single transport. It performs the initialize
// handshake, lists tools (paging internally), and calls tools. It auto-replies
// to inbound pings and routes tools/list_changed notifications to a hook.
type Client struct {
	transport Transport
	logger    *slog.Logger

	info           Implementation
	onToolsChanged func()

	mu                sync.Mutex
	initialized       bool
	serverCaps        ServerCapabilities
	negotiatedVersion string
}

// ClientOptions configures a Client.
type ClientOptions struct {
	Info           Implementation
	OnToolsChanged func()
	Logger         *slog.Logger
}

// NewClient wires a jsonrpc.Peer over rwc and returns a Client. The peer
// auto-replies to inbound ping with {} and routes tools/list_changed to
// OnToolsChanged. Initialize must be called before ListTools/CallTool.
func NewClient(rwc io.ReadWriteCloser, opts ClientOptions) *Client {
	logger := opts.Logger
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	peerOpts := jsonrpc.PeerOptions{
		Handlers: map[string]jsonrpc.Handler{
			MethodPing: func(ctx context.Context, params json.RawMessage) (json.RawMessage, *jsonrpc.Error) {
				return json.RawMessage(`{}`), nil
			},
		},
		Notifications: map[string]jsonrpc.NotificationHandler{
			NotifToolsListChanged: func(ctx context.Context, params json.RawMessage) {
				if opts.OnToolsChanged != nil {
					opts.OnToolsChanged()
				}
			},
		},
		Logger: logger,
	}
	peer := jsonrpc.NewPeer(rwc, peerOpts)
	return NewClientTransport(peerTransport{peer: peer}, opts)
}

// NewClientTransport returns a Client over an already-built Transport. It does
// not register the peer-level ping/list_changed handlers (a non-peer transport
// like HTTP has no inbound request channel for them); NewClient is the wiring
// for stream transports. Provided for HTTP transport reuse.
func NewClientTransport(t Transport, opts ClientOptions) *Client {
	logger := opts.Logger
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	return &Client{
		transport:      t,
		logger:         logger,
		info:           opts.Info,
		onToolsChanged: opts.OnToolsChanged,
	}
}

// VersionError reports a protocol-version negotiation failure. Supported is
// populated from a -32602 error's data when present; Got is the server-selected
// version on the success-but-unsupported path. Err wraps the underlying
// *jsonrpc.Error if any.
type VersionError struct {
	Requested string
	Supported []string
	Got       string
	Err       error
}

func (e *VersionError) Error() string {
	if e.Got != "" {
		return fmt.Sprintf("mcp: unsupported protocol version: requested %q, server selected %q", e.Requested, e.Got)
	}
	if len(e.Supported) > 0 {
		return fmt.Sprintf("mcp: unsupported protocol version: requested %q, server supports %v", e.Requested, e.Supported)
	}
	return fmt.Sprintf("mcp: unsupported protocol version: requested %q", e.Requested)
}

func (e *VersionError) Unwrap() error { return e.Err }

// Initialize performs the MCP handshake: it sends initialize, negotiates the
// protocol version, records server capabilities, and sends
// notifications/initialized. It returns *VersionError on a version mismatch
// (without closing the transport — the caller controls lifecycle). Calling it
// twice is an error.
func (c *Client) Initialize(ctx context.Context) (*InitializeResult, error) {
	// Claim the initialized flag up front in one critical section so two
	// concurrent callers cannot both pass the guard (no TOCTOU window). On any
	// failure before negotiation completes we roll it back so the caller can
	// retry.
	c.mu.Lock()
	if c.initialized {
		c.mu.Unlock()
		return nil, errors.New("mcp: client already initialized")
	}
	c.initialized = true
	c.mu.Unlock()

	rollback := func() {
		c.mu.Lock()
		c.initialized = false
		c.mu.Unlock()
	}

	params := InitializeParams{
		ProtocolVersion: ProtocolVersion,
		Capabilities:    ClientCapabilities{Experimental: json.RawMessage(`{}`)},
		ClientInfo:      c.info,
	}
	raw, err := json.Marshal(params)
	if err != nil {
		rollback()
		return nil, fmt.Errorf("mcp: marshal initialize params: %w", err)
	}

	resRaw, callErr := c.transport.Call(ctx, MethodInitialize, raw)
	if callErr != nil {
		rollback()
		// (a) Server rejected the version with a JSON-RPC error (commonly -32602).
		var je *jsonrpc.Error
		if errors.As(callErr, &je) && je.Code == jsonrpc.CodeInvalidParams {
			ve := &VersionError{Requested: ProtocolVersion, Err: je}
			if len(je.Data) > 0 {
				var data struct {
					Supported []string `json:"supported"`
					Requested string   `json:"requested"`
				}
				if json.Unmarshal(je.Data, &data) == nil {
					ve.Supported = data.Supported
					if data.Requested != "" {
						ve.Requested = data.Requested
					}
				}
			}
			return nil, ve
		}
		return nil, fmt.Errorf("mcp: initialize: %w", callErr)
	}

	var result InitializeResult
	if err := json.Unmarshal(resRaw, &result); err != nil {
		rollback()
		return nil, fmt.Errorf("mcp: decode initialize result: %w", err)
	}

	// (b) Success but the server selected a version we cannot speak.
	if !Supports(result.ProtocolVersion) {
		rollback()
		return nil, &VersionError{Requested: ProtocolVersion, Got: result.ProtocolVersion}
	}

	// (c) Success: record state (initialized is already claimed), then announce
	// we are initialized.
	c.mu.Lock()
	c.serverCaps = result.Capabilities
	c.negotiatedVersion = result.ProtocolVersion
	c.mu.Unlock()

	// Stream transports negotiate the version in-band; an HTTP transport must
	// echo it as a header on every subsequent request. Notify it via the
	// optional interface so the Transport contract stays minimal.
	if pv, ok := c.transport.(interface{ SetProtocolVersion(string) }); ok {
		pv.SetProtocolVersion(result.ProtocolVersion)
	}

	if err := c.transport.Notify(ctx, NotifInitialized, json.RawMessage(`{}`)); err != nil {
		rollback()
		return nil, fmt.Errorf("mcp: send initialized notification: %w", err)
	}
	return &result, nil
}

// Capabilities returns the negotiated server capabilities (valid after a
// successful Initialize).
func (c *Client) Capabilities() ServerCapabilities {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.serverCaps
}

// NegotiatedVersion returns the protocol version selected during Initialize.
func (c *Client) NegotiatedVersion() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.negotiatedVersion
}

// ListTools returns all tools, following nextCursor pagination internally. It
// returns nil without error if the server did not advertise a tools capability.
// A server returning a non-advancing cursor is capped at maxListToolsPages.
func (c *Client) ListTools(ctx context.Context) ([]Tool, error) {
	c.mu.Lock()
	hasTools := c.serverCaps.Tools != nil
	c.mu.Unlock()
	if !hasTools {
		return nil, nil
	}

	var all []Tool
	cursor := ""
	for page := 0; ; page++ {
		if page >= maxListToolsPages {
			return nil, fmt.Errorf("mcp: tools/list exceeded %d pages (non-advancing cursor?)", maxListToolsPages)
		}
		params := ListToolsParams{Cursor: cursor}
		raw, err := json.Marshal(params)
		if err != nil {
			return nil, fmt.Errorf("mcp: marshal tools/list params: %w", err)
		}
		resRaw, err := c.transport.Call(ctx, MethodListTools, raw)
		if err != nil {
			return nil, fmt.Errorf("mcp: tools/list: %w", err)
		}
		var result ListToolsResult
		if err := json.Unmarshal(resRaw, &result); err != nil {
			return nil, fmt.Errorf("mcp: decode tools/list result: %w", err)
		}
		all = append(all, result.Tools...)
		if result.NextCursor == "" {
			return all, nil
		}
		cursor = result.NextCursor
	}
}

// CallTool invokes one tool. A JSON-RPC error (unknown tool, bad params) is
// returned as a non-nil error wrapping *jsonrpc.Error. A tool-execution failure
// is a successful return with IsError true. On ctx cancellation the client
// best-effort emits notifications/cancelled before returning ctx.Err().
func (c *Client) CallTool(ctx context.Context, name string, args json.RawMessage) (*CallToolResult, error) {
	params := CallToolParams{Name: name, Arguments: args}
	raw, err := json.Marshal(params)
	if err != nil {
		return nil, fmt.Errorf("mcp: marshal tools/call params: %w", err)
	}

	resRaw, callErr := c.call(ctx, MethodCallTool, raw)
	if callErr != nil {
		return nil, callErr
	}

	var result CallToolResult
	if err := json.Unmarshal(resRaw, &result); err != nil {
		return nil, fmt.Errorf("mcp: decode tools/call result: %w", err)
	}
	return &result, nil
}

// call routes through the transport's cancellation-aware path when available so
// a cancelled request emits notifications/cancelled with the matching id. A
// transport that does not implement cancelTransport (e.g. HTTP) falls back to a
// plain Call.
func (c *Client) call(ctx context.Context, method string, params json.RawMessage) (json.RawMessage, error) {
	ct, ok := c.transport.(cancelTransport)
	if !ok {
		return c.transport.Call(ctx, method, params)
	}
	return ct.CallCancelable(ctx, method, params, func(id json.RawMessage) {
		cp := CancelledParams{RequestID: id}
		body, err := json.Marshal(cp)
		if err != nil {
			return
		}
		// Best-effort: the context that cancelled the call is done, so use a
		// fresh background context to deliver the notification.
		_ = c.transport.Notify(context.Background(), NotifCancelled, body)
	})
}

// Ping sends a ping request and returns nil on a successful empty reply.
func (c *Client) Ping(ctx context.Context) error {
	if _, err := c.transport.Call(ctx, MethodPing, nil); err != nil {
		return fmt.Errorf("mcp: ping: %w", err)
	}
	return nil
}

// Close closes the underlying transport.
func (c *Client) Close() error {
	return c.transport.Close()
}
