package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"sync"

	"harness/internal/mcp/jsonrpc"
)

// ToolProvider supplies tools to a Server. The proxy implements it so all
// aggregation logic stays out of the Server. A tool-execution failure is
// returned as a *CallToolResult with IsError true; a protocol failure (unknown
// tool, bad params) is returned as a *jsonrpc.Error.
type ToolProvider interface {
	ListTools(ctx context.Context, cursor string) (ListToolsResult, error)
	CallTool(ctx context.Context, name string, args json.RawMessage) (*CallToolResult, error)
}

// ServerOptions configures a Server session.
type ServerOptions struct {
	Info        Implementation
	Provider    ToolProvider
	ListChanged bool
	Logger      *slog.Logger
	// OnSession, if set, is called once with the session handle after the peer
	// is up and before initialize. The proxy subscribes the session for
	// tools/list_changed fan-out through it.
	OnSession func(s *ServerSession)
}

// ServerSession is a handle to a live server session, exposed to the proxy so
// it can push notifications and observe session lifetime. It is created before
// initialize and remains valid until the session ends.
type ServerSession struct {
	peer *jsonrpc.Peer
}

// NotifyToolsListChanged sends a tools/list_changed notification to the client.
func (s *ServerSession) NotifyToolsListChanged() error {
	return s.peer.Notify(NotifToolsListChanged, json.RawMessage(`{}`))
}

// TryNotifyToolsListChanged sends a tools/list_changed notification without
// blocking on a full peer queue.
func (s *ServerSession) TryNotifyToolsListChanged() error {
	return s.peer.TryNotify(NotifToolsListChanged, json.RawMessage(`{}`))
}

// Done returns a channel closed when the session ends.
func (s *ServerSession) Done() <-chan struct{} { return s.peer.Done() }

// Close ends the session.
func (s *ServerSession) Close() error { return s.peer.Close() }

// server holds the per-session mutable state shared by the method handlers.
type server struct {
	opts   ServerOptions
	logger *slog.Logger

	mu          sync.Mutex
	initialized bool
	clientInfo  Implementation
	inflight    map[jsonrpc.ID]context.CancelFunc
}

// Serve runs one MCP session over rwc until EOF, error, or ctx cancellation. A
// clean client disconnect returns nil. It blocks; the proxy calls it in a
// goroutine per accepted connection.
//
// Teardown is best-effort: on return the peer is closed, which cancels the ctx
// passed to in-flight provider calls (via the peer's shutdown). Serve does not
// join those handler goroutines, so a provider that ignores its ctx may briefly
// outlive the session; callers that need a hard join must bound provider work
// themselves.
func Serve(ctx context.Context, rwc io.ReadWriteCloser, opts ServerOptions) error {
	logger := opts.Logger
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	s := &server{
		opts:     opts,
		logger:   logger,
		inflight: make(map[jsonrpc.ID]context.CancelFunc),
	}

	peerOpts := jsonrpc.PeerOptions{
		Handlers: map[string]jsonrpc.Handler{
			MethodInitialize: s.handleInitialize,
			MethodPing:       s.handlePing,
			MethodListTools:  s.handleListTools,
		},
		HandlersWithID: map[string]jsonrpc.HandlerID{
			MethodCallTool: s.handleCallTool,
		},
		Notifications: map[string]jsonrpc.NotificationHandler{
			NotifInitialized: s.handleInitializedNotif,
			NotifCancelled:   s.handleCancelledNotif,
		},
		Logger: logger,
	}
	peer := jsonrpc.NewPeer(rwc, peerOpts)
	defer peer.Close()

	if opts.OnSession != nil {
		opts.OnSession(&ServerSession{peer: peer})
	}

	select {
	case <-peer.Done():
		// A clean client disconnect (EOF) or an explicit close is not an error.
		if err := peer.Err(); err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, jsonrpc.ErrPeerClosed) {
			return err
		}
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (s *server) handlePing(ctx context.Context, params json.RawMessage) (json.RawMessage, *jsonrpc.Error) {
	// ping is answered even before initialize.
	return json.RawMessage(`{}`), nil
}

func (s *server) handleInitialize(ctx context.Context, params json.RawMessage) (json.RawMessage, *jsonrpc.Error) {
	// Negotiate: echo the client's version if we support it, else offer ours.
	p, raw, jerr := initializePayload(params, s.opts.Info, s.opts.ListChanged)
	if jerr != nil {
		return nil, jerr
	}

	s.mu.Lock()
	s.clientInfo = p.ClientInfo
	s.initialized = true
	s.mu.Unlock()

	return raw, nil
}

func (s *server) handleInitializedNotif(ctx context.Context, params json.RawMessage) {
	// initialize already flips the initialized flag; the notification is
	// informational here. Recorded for completeness/symmetry.
}

func (s *server) ready() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.initialized
}

func (s *server) handleListTools(ctx context.Context, params json.RawMessage) (json.RawMessage, *jsonrpc.Error) {
	if !s.ready() {
		return nil, jsonrpc.NewError(jsonrpc.CodeInvalidRequest, "server not initialized")
	}
	return listToolsPayload(ctx, s.opts.Provider, params)
}

func (s *server) handleCallTool(ctx context.Context, id jsonrpc.ID, params json.RawMessage) (json.RawMessage, *jsonrpc.Error) {
	if !s.ready() {
		return nil, jsonrpc.NewError(jsonrpc.CodeInvalidRequest, "server not initialized")
	}
	p, jerr := decodeCallToolParams(params)
	if jerr != nil {
		return nil, jerr
	}

	// Register the call's context so a notifications/cancelled with this id can
	// cancel it. Keyed by the full JSON-RPC id so string and numeric ids do not
	// collide.
	callCtx, cancel := context.WithCancel(ctx)
	key := id
	// Best-effort: notifications and requests are dispatched on separate
	// goroutines, so a cancel that races ahead of handleCallTool registering its
	// in-flight entry finds no match and is silently dropped. This is acceptable
	// per the MCP spec (cancellation is advisory); it is not a guarantee.
	s.mu.Lock()
	s.inflight[key] = cancel
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		delete(s.inflight, key)
		s.mu.Unlock()
		cancel()
	}()

	return callToolPayload(callCtx, s.opts.Provider, p)
}

func (s *server) handleCancelledNotif(ctx context.Context, params json.RawMessage) {
	var p CancelledParams
	if err := json.Unmarshal(params, &p); err != nil {
		return
	}
	id, ok := canonicalID(p.RequestID)
	if !ok {
		return
	}
	s.mu.Lock()
	cancel := s.inflight[id]
	s.mu.Unlock()
	if cancel != nil {
		cancel()
	}
}

// canonicalID parses a raw JSON id into a comparable jsonrpc.ID, so a cancelled
// requestId matches the inbound request id regardless of incidental formatting
// (whitespace, number style). It returns false if raw is not a valid non-null id.
func canonicalID(raw json.RawMessage) (jsonrpc.ID, bool) {
	var id jsonrpc.ID
	if err := id.UnmarshalJSON(raw); err != nil {
		return jsonrpc.ID{}, false
	}
	return id, true
}
