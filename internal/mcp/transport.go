package mcp

import (
	"context"
	"encoding/json"

	"harness/internal/mcp/jsonrpc"
)

// Transport is the minimal client-side RPC surface a Client needs. A stream
// transport (a jsonrpc.Peer over stdio) and the HTTP transport both satisfy it,
// so the Client is transport-agnostic.
type Transport interface {
	// Call sends a request and returns its result payload. A JSON-RPC error
	// response is returned as a non-nil error wrapping *jsonrpc.Error.
	Call(ctx context.Context, method string, params json.RawMessage) (json.RawMessage, error)
	// Notify sends a fire-and-forget notification.
	Notify(ctx context.Context, method string, params json.RawMessage) error
	// Close releases the transport.
	Close() error
}

// cancelTransport is an optional interface a Transport may implement to support
// emitting notifications/cancelled when a Call's context is cancelled. The
// peer-backed transport implements it; the HTTP transport does not have
// to, since its Call is one POST per request and has no in-flight id to cancel.
// CallCancelable reports the cancelled request's raw id to onCancel exactly once
// if ctx is cancelled before the response arrives, so the Client can emit the
// matching notifications/cancelled.
type cancelTransport interface {
	CallCancelable(ctx context.Context, method string, params json.RawMessage, onCancel func(id json.RawMessage)) (json.RawMessage, error)
}

// peerTransport adapts a *jsonrpc.Peer to the Transport (and cancelTransport)
// interfaces. It is the transport used for stream connections such as downstream
// stdio pipes and test in-memory peers.
type peerTransport struct {
	peer *jsonrpc.Peer
}

func (t peerTransport) Call(ctx context.Context, method string, params json.RawMessage) (json.RawMessage, error) {
	return t.peer.Call(ctx, method, params)
}

func (t peerTransport) CallCancelable(ctx context.Context, method string, params json.RawMessage, onCancel func(id json.RawMessage)) (json.RawMessage, error) {
	opts := jsonrpc.CallOpts{OnCancel: func(id jsonrpc.ID) {
		raw, err := id.MarshalJSON()
		if err != nil {
			return
		}
		onCancel(json.RawMessage(raw))
	}}
	return t.peer.CallWith(ctx, method, params, opts)
}

func (t peerTransport) Notify(_ context.Context, method string, params json.RawMessage) error {
	return t.peer.Notify(method, params)
}

func (t peerTransport) Close() error {
	return t.peer.Close()
}
