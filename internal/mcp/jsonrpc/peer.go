package jsonrpc

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"sync"
	"sync/atomic"
)

// ErrPeerClosed is returned by pending Calls when the peer shuts down. Callers
// classify connection drops with errors.Is(err, ErrPeerClosed).
var ErrPeerClosed = errors.New("jsonrpc: peer closed")

// ErrPeerBlocked is returned by best-effort nonblocking sends when the peer's
// outbound queue is full.
var ErrPeerBlocked = errors.New("jsonrpc: peer output buffer full")

// Handler handles an inbound request. Returning (result, nil) sends a success
// response; returning (_, *Error) sends that error response. ctx is cancelled
// when the peer shuts down.
type Handler func(ctx context.Context, params json.RawMessage) (json.RawMessage, *Error)

// HandlerID is like Handler but also receives the inbound request's id. The MCP
// server uses it to register each in-flight request's id so a later
// notifications/cancelled can cancel the matching call. The id is never null
// (it identifies a request, not a notification).
type HandlerID func(ctx context.Context, id ID, params json.RawMessage) (json.RawMessage, *Error)

// NotificationHandler handles an inbound notification; it sends no response.
type NotificationHandler func(ctx context.Context, params json.RawMessage)

// Reader decodes framed messages from a transport. The newline Decoder satisfies
// it; an alternate framing (e.g. the LSP Content-Length codec) can supply its
// own, letting the Peer drive any JSON-RPC stream.
type Reader interface {
	Decode() (Message, error)
}

// Writer encodes framed messages to a transport. The newline Encoder satisfies
// it.
type Writer interface {
	Encode(Message) error
}

// PeerOptions configures a Peer. Handlers and Notifications are the inbound
// method routers; an unknown request method auto-replies CodeMethodNotFound and
// an unknown notification is ignored. Logger is optional and used only for
// drop/diagnostic logging.
type PeerOptions struct {
	Handlers      map[string]Handler
	Notifications map[string]NotificationHandler
	// HandlersWithID routes inbound requests whose handler needs the request id
	// (for in-flight cancellation tracking). A method present here takes
	// precedence over the same method in Handlers.
	HandlersWithID map[string]HandlerID
	Logger         *slog.Logger
}

// CallOpts customizes a single Call. OnCancel, if set, is invoked exactly once
// with the request id if the Call is abandoned because ctx was cancelled. The
// MCP client layer uses it to emit notifications/cancelled.
type CallOpts struct {
	OnCancel func(id ID)
}

// outBufferSize bounds the writer's queue. A full channel applies backpressure
// to callers, which is acceptable flow control.
const outBufferSize = 64

type pending struct {
	result json.RawMessage
	err    *Error
}

// Peer drives a single connection in both directions: it correlates outbound
// Calls with inbound responses and dispatches inbound requests/notifications.
type Peer struct {
	closer io.Closer
	dec    Reader
	enc    Writer
	opts   PeerOptions
	logger *slog.Logger

	idCounter atomic.Int64
	closed    atomic.Bool

	out chan Message // sole path to the Encoder; drained by one writer goroutine

	mu      sync.Mutex
	waiters map[ID]chan pending

	// ctx is cancelled when the peer shuts down, propagating to in-flight
	// handler goroutines.
	ctx    context.Context
	cancel context.CancelFunc

	done     chan struct{}
	closeErr error
	once     sync.Once
}

// NewPeer starts the read loop and a single writer goroutine over rwc using the
// newline codec. The writer goroutine is the sole owner of the Encoder, so
// outbound messages never interleave mid-line.
func NewPeer(rwc io.ReadWriteCloser, opts PeerOptions) *Peer {
	return NewPeerWithCodec(rwc, NewDecoder(rwc), NewEncoder(rwc), opts)
}

// NewPeerWithCodec is NewPeer with an injected framing. closer is closed on
// shutdown; dec/enc must read from and write to the same underlying transport.
// It lets a caller drive the Peer over a non-newline framing (e.g. the LSP
// Content-Length codec) while reusing all correlation and dispatch logic.
func NewPeerWithCodec(closer io.Closer, dec Reader, enc Writer, opts PeerOptions) *Peer {
	logger := opts.Logger
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	ctx, cancel := context.WithCancel(context.Background())
	p := &Peer{
		closer:  closer,
		dec:     dec,
		enc:     enc,
		opts:    opts,
		logger:  logger,
		out:     make(chan Message, outBufferSize),
		waiters: make(map[ID]chan pending),
		ctx:     ctx,
		cancel:  cancel,
		done:    make(chan struct{}),
	}
	go p.writeLoop()
	go p.readLoop()
	return p
}

// Call sends a request and blocks until the matching response, ctx cancellation,
// or peer shutdown. A JSON-RPC error response is returned as the *Error (use
// errors.As); peer shutdown returns ErrPeerClosed.
func (p *Peer) Call(ctx context.Context, method string, params json.RawMessage) (json.RawMessage, error) {
	return p.CallWith(ctx, method, params, CallOpts{})
}

// CallWith is Call with per-call options.
func (p *Peer) CallWith(ctx context.Context, method string, params json.RawMessage, opts CallOpts) (json.RawMessage, error) {
	id := IntID(p.idCounter.Add(1))
	key := id
	ch := make(chan pending, 1)

	p.mu.Lock()
	p.waiters[key] = ch
	p.mu.Unlock()

	if err := p.send(NewRequest(id, method, params)); err != nil {
		p.removeWaiter(key)
		return nil, err
	}

	select {
	case resp := <-ch:
		return resp.result, errOf(resp)
	case <-ctx.Done():
		// A response delivered just as ctx was cancelled still wins: the waiter
		// channel is buffered, so a value already sent is observable. Draining it
		// first avoids firing OnCancel (and emitting notifications/cancelled) for
		// a request that actually completed.
		p.removeWaiter(key)
		select {
		case resp := <-ch:
			return resp.result, errOf(resp)
		default:
		}
		if opts.OnCancel != nil {
			opts.OnCancel(id)
		}
		return nil, ctx.Err()
	case <-p.done:
		// A response delivered just as the peer shut down still wins: the
		// waiter channel is buffered, so a value already sent is observable.
		select {
		case resp := <-ch:
			return resp.result, errOf(resp)
		default:
			return nil, ErrPeerClosed
		}
	}
}

func errOf(resp pending) error {
	if resp.err != nil {
		return resp.err
	}
	return nil
}

// Notify sends a fire-and-forget notification.
func (p *Peer) Notify(method string, params json.RawMessage) error {
	return p.send(NewNotification(method, params))
}

// TryNotify sends a notification without blocking on a full outbound queue.
// It is intended for best-effort fan-out paths; ordinary request/response flow
// should keep using the blocking send for backpressure.
func (p *Peer) TryNotify(method string, params json.RawMessage) error {
	return p.trySend(NewNotification(method, params))
}

// Done returns a channel closed when the read loop exits (EOF, error, or Close).
func (p *Peer) Done() <-chan struct{} {
	return p.done
}

// Err returns the terminal error after Done is closed: io.EOF on a clean close,
// ErrPeerClosed on an explicit Close, or the underlying read error otherwise.
func (p *Peer) Err() error {
	<-p.done
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.closeErr
}

// Close shuts the peer down: it stops the writer, closes rwc, and fails all
// pending Calls with ErrPeerClosed. It is idempotent.
func (p *Peer) Close() error {
	p.shutdown(ErrPeerClosed)
	<-p.done
	return nil
}

// send enqueues m for the writer goroutine, unless the peer is shutting down.
// The out channel is buffered (outBufferSize); once it fills, send blocks the
// caller until the writer drains an entry or a write error triggers shutdown.
// That backpressure is intentional flow control, not a deadlock.
func (p *Peer) send(m Message) error {
	if p.closed.Load() {
		return ErrPeerClosed
	}
	select {
	case p.out <- m:
		return nil
	case <-p.done:
		return ErrPeerClosed
	}
}

func (p *Peer) trySend(m Message) error {
	if p.closed.Load() {
		return ErrPeerClosed
	}
	select {
	case p.out <- m:
		return nil
	case <-p.done:
		return ErrPeerClosed
	default:
		return ErrPeerBlocked
	}
}

func (p *Peer) removeWaiter(key ID) {
	p.mu.Lock()
	delete(p.waiters, key)
	p.mu.Unlock()
}

// writeLoop is the sole owner of the Encoder. It drains the buffered out
// channel one message at a time; while it is mid-Encode a full out buffer
// applies backpressure to senders (see send). A write error triggers shutdown.
func (p *Peer) writeLoop() {
	for {
		select {
		case m := <-p.out:
			if err := p.enc.Encode(m); err != nil {
				p.logger.Debug("jsonrpc: write failed", "err", err)
				p.shutdown(err)
				return
			}
		case <-p.done:
			return
		}
	}
}

// readLoop decodes inbound messages and routes them by Kind.
func (p *Peer) readLoop() {
	for {
		m, err := p.dec.Decode()
		if err != nil {
			p.shutdown(err)
			return
		}
		p.route(m)
	}
}

func (p *Peer) route(m Message) {
	switch m.Kind() {
	case KindResponse:
		p.deliverResponse(m)
	case KindRequest:
		p.dispatchRequest(m)
	case KindNotification:
		p.dispatchNotification(m)
	default: // KindInvalid
		// KindInvalid means neither a method nor an id, so there is no one to
		// answer; drop it. (A methodless id-bearing message is a KindResponse
		// and is handled there, dropped as a no-waiter response.)
		p.logger.Debug("jsonrpc: dropping invalid message")
	}
}

func (p *Peer) deliverResponse(m Message) {
	key := *m.ID
	p.mu.Lock()
	ch, ok := p.waiters[key]
	if ok {
		delete(p.waiters, key)
	}
	p.mu.Unlock()
	if !ok {
		p.logger.Debug("jsonrpc: response with no waiter", "id", key.String())
		return
	}
	ch <- pending{result: m.Result, err: m.Error}
}

func (p *Peer) dispatchRequest(m Message) {
	id := *m.ID
	// A HandlerID (id-aware) handler takes precedence over a plain Handler for
	// the same method.
	if handler, ok := p.opts.HandlersWithID[m.Method]; ok {
		go func() {
			result, rpcErr := p.callHandlerID(handler, id, m.Params)
			if rpcErr != nil {
				p.send(NewErrorResponse(id, rpcErr))
				return
			}
			p.send(NewResponse(id, result))
		}()
		return
	}
	handler, ok := p.opts.Handlers[m.Method]
	if !ok {
		p.send(NewErrorResponse(id, Errorf(CodeMethodNotFound, "method not found: %s", m.Method)))
		return
	}
	// Dispatch in a new goroutine so a slow handler never blocks the read loop.
	go func() {
		result, rpcErr := p.callHandler(handler, m.Params)
		if rpcErr != nil {
			p.send(NewErrorResponse(id, rpcErr))
			return
		}
		p.send(NewResponse(id, result))
	}()
}

// callHandler runs handler with panic recovery, converting a panic into a
// CodeInternal error.
func (p *Peer) callHandler(handler Handler, params json.RawMessage) (result json.RawMessage, rpcErr *Error) {
	defer func() {
		if r := recover(); r != nil {
			p.logger.Debug("jsonrpc: handler panic", "recover", r)
			result = nil
			rpcErr = Errorf(CodeInternal, "handler panic: %v", r)
		}
	}()
	return handler(p.ctx, params)
}

// callHandlerID runs an id-aware handler with the same panic recovery.
func (p *Peer) callHandlerID(handler HandlerID, id ID, params json.RawMessage) (result json.RawMessage, rpcErr *Error) {
	defer func() {
		if r := recover(); r != nil {
			p.logger.Debug("jsonrpc: handler panic", "recover", r)
			result = nil
			rpcErr = Errorf(CodeInternal, "handler panic: %v", r)
		}
	}()
	return handler(p.ctx, id, params)
}

func (p *Peer) dispatchNotification(m Message) {
	handler, ok := p.opts.Notifications[m.Method]
	if !ok {
		return // tolerate unknown notifications
	}
	go func() {
		defer func() {
			if r := recover(); r != nil {
				p.logger.Debug("jsonrpc: notification handler panic", "recover", r)
			}
		}()
		handler(p.ctx, m.Params)
	}()
}

// shutdown records the terminal error once, cancels in-flight handlers, closes
// rwc, and signals done. Pending Calls observe the closed done channel and
// return ErrPeerClosed; clearing waiters without delivery prevents a waiter
// from receiving a value after shutdown begins.
func (p *Peer) shutdown(cause error) {
	p.once.Do(func() {
		p.closed.Store(true)

		p.mu.Lock()
		p.closeErr = cause
		p.waiters = make(map[ID]chan pending)
		p.mu.Unlock()

		p.cancel()
		close(p.done)
		p.closer.Close()
	})
}
