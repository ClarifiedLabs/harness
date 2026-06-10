// Package mcptools adapts proxy-discovered MCP tools to the harness
// tools.Tool interface. Each tool proxies tools/call over a shared, reconnecting
// connection to the MCP proxy. It lives outside internal/tools (mirroring
// internal/delegate) to avoid an import cycle: it imports both internal/mcp and
// internal/tools, and must not pull internal/llm or internal/agent into the
// proxy's dependency graph.
package mcptools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"harness/internal/mcp"
	"harness/internal/mcp/jsonrpc"
	"harness/internal/retry"
)

// initTimeout bounds the MCP initialize handshake on a fresh connection.
const initTimeout = 10 * time.Second

// Options configures a Conn.
type Options struct {
	// Endpoint is the HTTP proxy URL.
	Endpoint string
	// Headers are static request headers (e.g. Authorization) sent on every
	// request to the proxy.
	Headers map[string]string
	Info    mcp.Implementation // clientInfo (harness name/version)
	Logger  *slog.Logger

	// Dial is an internal test seam for stream transports that can deliver
	// tools/list_changed notifications. Production callers should leave it nil
	// and use Endpoint.
	Dial func(ctx context.Context) (io.ReadWriteCloser, error)

	// dial and now are unexported test seams. dial returns the raw transport for
	// a fresh stream connection; the Conn wraps it in a real *mcp.Client. It is
	// used only by tests that need server-push notifications. now supplies the
	// clock for backoff gating. Both default to production behavior.
	dial func(ctx context.Context) (io.ReadWriteCloser, error)
	now  func() time.Time
}

// Conn is a shared, lazily-reconnecting wrapper around a single *mcp.Client
// session to the proxy. It spawns no goroutines of its own: reconnection is
// synchronous on the calling goroutine, gated by a backoff timer so a down
// proxy does not trigger a reconnect storm. The *mcp.Client owns its own read
// goroutine, which Close tears down.
type Conn struct {
	info   mcp.Implementation
	logger *slog.Logger

	// dial is the stream transport seam used only by tests (nil for production).
	// endpoint and headers configure the production HTTP family.
	dial     func(ctx context.Context) (io.ReadWriteCloser, error)
	endpoint string
	headers  map[string]string
	now      func() time.Time

	dirty atomic.Bool // set by OnToolsChanged, consumed at prompt boundaries

	mu     sync.Mutex
	client *mcp.Client // nil when disconnected
	// http is the persistent streamable-HTTP transport, created lazily on first
	// connect and REUSED across reconnects (matching the proxy supervisor's
	// connectHTTP): after a session expiry the transport has cleared its session,
	// so a fresh Client over the SAME transport re-runs Initialize and
	// establishes a new session. nil only when tests inject dial.
	http     *mcp.HTTPTransport
	lastErr  error
	nextTry  time.Time // backoff gate; before this, ensure fast-fails
	failures int
}

// NewConn returns a Conn that dials the proxy lazily on first use. It does not
// connect until CallTool or ListTools is called.
func NewConn(opts Options) *Conn {
	logger := opts.Logger
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	now := opts.now
	if now == nil {
		now = time.Now
	}
	c := &Conn{
		info:   opts.Info,
		logger: logger,
		now:    now,
	}

	dial := opts.dial
	if dial == nil {
		dial = opts.Dial
	}
	if dial != nil {
		c.dial = dial
	} else {
		c.endpoint = opts.Endpoint
		c.headers = opts.Headers
	}
	return c
}

// ListTools returns the proxy's tools, reconnecting lazily if needed.
func (c *Conn) ListTools(ctx context.Context) ([]mcp.Tool, error) {
	cl, err := c.ensure(ctx)
	if err != nil {
		return nil, err
	}
	tools, err := cl.ListTools(ctx)
	if err != nil && isConnError(err) {
		c.drop(cl)
	}
	return tools, err
}

// CallTool invokes name over the shared connection, reconnecting lazily if
// needed. A connection drop is classified and drops the client so the next call
// reconnects; the error is returned as-is so Dispatch renders it.
func (c *Conn) CallTool(ctx context.Context, name string, args json.RawMessage) (*mcp.CallToolResult, error) {
	cl, err := c.ensure(ctx)
	if err != nil {
		return nil, err
	}
	res, err := cl.CallTool(ctx, name, args)
	if err != nil && isConnError(err) {
		c.drop(cl)
	}
	return res, err
}

// ensure returns a live client, lazily reconnecting under a backoff gate. It
// holds the mutex across the dial+initialize so two callers cannot race two
// half-open connections; this serializes reconnects, which is acceptable since
// reconnects are rare and the steady state returns the cached client immediately.
// Trusted read-only MCP tools may share this connection concurrently, so a
// reconnect is deliberately a single contended operation rather than a reconnect
// storm.
func (c *Conn) ensure(ctx context.Context) (*mcp.Client, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.client != nil {
		return c.client, nil
	}

	now := c.now()
	if now.Before(c.nextTry) {
		return nil, fmt.Errorf("mcp proxy unavailable (retry in %s): %w",
			c.nextTry.Sub(now).Round(time.Millisecond), c.lastErr)
	}

	cl, err := c.connect(ctx)
	if err != nil {
		c.lastErr = err
		c.nextTry = c.now().Add(retry.Next(c.failures, 0))
		c.failures++
		return nil, err
	}
	c.failures = 0
	c.client = cl
	return cl, nil
}

// connect builds a fresh client and runs the MCP initialize handshake under a
// bounded timeout. Production uses the HTTP transport; tests may inject a stream
// dialer. On any failure it closes the client so no goroutine or fd leaks. The
// initialize derives from the caller's ctx, so an interrupt aborts an in-flight
// connect. ensure holds c.mu across this call.
func (c *Conn) connect(ctx context.Context) (*mcp.Client, error) {
	if c.endpoint != "" {
		return c.connectHTTP(ctx)
	}

	rwc, err := c.dial(ctx)
	if err != nil {
		return nil, err
	}
	cl := mcp.NewClient(rwc, mcp.ClientOptions{
		Info: c.info,
		// Injected stream transports deliver tools/list_changed; the dirty flag
		// drives refresh tests. HTTP never delivers this notification, so
		// connectHTTP omits the hook.
		OnToolsChanged: func() { c.dirty.Store(true) },
		Logger:         c.logger,
	})
	return c.initialize(ctx, cl)
}

// connectHTTP builds a fresh client over the persistent HTTP transport (created
// lazily on first connect, reused across reconnects) and initializes it. No
// OnToolsChanged hook is wired: the streamable-HTTP transport has no inbound
// notification channel, so tools/list_changed never fires, Dirty() stays false,
// and the REPL refresh hook is a no-op for HTTP proxies (accepted v1 behavior).
func (c *Conn) connectHTTP(ctx context.Context) (*mcp.Client, error) {
	if c.http == nil {
		c.http = mcp.NewHTTPTransport(mcp.HTTPOptions{
			Endpoint: c.endpoint,
			Headers:  c.headers,
			Logger:   c.logger,
		})
	}
	cl := mcp.NewClientTransport(c.http, mcp.ClientOptions{
		Info:   c.info,
		Logger: c.logger,
	})
	return c.initialize(ctx, cl)
}

// initialize runs the MCP handshake on cl under a bounded timeout, closing cl on
// failure. For HTTP, closing cl closes the shared transport too; that is fine
// because a failed connect leaves no live client and the transport is rebuilt
// lazily on the next connect.
func (c *Conn) initialize(ctx context.Context, cl *mcp.Client) (*mcp.Client, error) {
	initCtx, cancel := context.WithTimeout(ctx, initTimeout)
	defer cancel()
	if _, err := cl.Initialize(initCtx); err != nil {
		_ = cl.Close()
		// An http connect failure may have closed the shared transport; drop it so
		// the next connect rebuilds a fresh one.
		c.http = nil
		return nil, err
	}
	return cl, nil
}

// drop closes cl and clears it under the mutex, but only if it is still the
// current client. The identity check means two concurrent calls that both
// observe a drop on the same client close it once, and a drop on a stale client
// (already replaced by a newer connection) is a no-op.
func (c *Conn) drop(cl *mcp.Client) {
	c.mu.Lock()
	if c.client != cl {
		c.mu.Unlock()
		return
	}
	c.client = nil
	c.mu.Unlock()
	_ = cl.Close()
}

// Dirty reports whether the proxy has signalled tools/list_changed since the
// last ClearDirty. The prompt-boundary refresh consumes it.
func (c *Conn) Dirty() bool { return c.dirty.Load() }

// ClearDirty resets the dirty flag.
func (c *Conn) ClearDirty() { c.dirty.Store(false) }

// Close closes the current client if any, and the persistent HTTP transport
// (best-effort DELETE of any live session). It is safe to call when
// disconnected. Closing the client already closes the transport, but a
// disconnected conn (client nil) may still hold a transport with a live session
// — e.g. after a drop on a non-session error — so the transport is closed
// directly too. HTTPTransport.Close clears its session id after the DELETE, so
// the two Close calls below emit at most one DELETE for one session.
func (c *Conn) Close() error {
	c.mu.Lock()
	cl := c.client
	tr := c.http
	c.client = nil
	c.http = nil
	c.mu.Unlock()
	var err error
	if cl != nil {
		err = cl.Close()
	}
	if tr != nil {
		// Best-effort: if cl != nil the client Close above already DELETEd this
		// session and cleared it, so this is a no-op; it matters only when cl is
		// nil but the transport still holds a session.
		_ = tr.Close()
	}
	return err
}

// isConnError reports whether err means the transport is dead (so the connection
// must be dropped and re-dialed/rebuilt), as opposed to an ordinary
// JSON-RPC/tool error or a context cancellation (which leave the link healthy).
//
// mcp.ErrSessionExpired (HTTP 404) counts as a drop: the transport has cleared
// its session, so the next call rebuilds a fresh Client over the same transport
// and re-runs Initialize, establishing a new session. The tool set is assumed
// stable across the re-initialize (Register runs only at startup over HTTP).
func isConnError(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, jsonrpc.ErrPeerClosed) ||
		errors.Is(err, io.EOF) ||
		errors.Is(err, io.ErrUnexpectedEOF) ||
		errors.Is(err, net.ErrClosed) ||
		errors.Is(err, mcp.ErrSessionExpired) {
		return true
	}
	var opErr *net.OpError
	return errors.As(err, &opErr)
}
