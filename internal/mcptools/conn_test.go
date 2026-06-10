package mcptools

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"harness/internal/mcp"
	"harness/internal/mcp/jsonrpc"
)

// countingDial wraps an inner dial seam, counting invocations.
type countingDial struct {
	inner func(ctx context.Context) (io.ReadWriteCloser, error)
	count atomic.Int32
}

func (d *countingDial) dial(ctx context.Context) (io.ReadWriteCloser, error) {
	d.count.Add(1)
	return d.inner(ctx)
}

func echoResult() *mcp.CallToolResult {
	return &mcp.CallToolResult{Content: []mcp.ContentBlock{{Type: "text", Text: "ok"}}}
}

func TestConnFirstCallDialsOnce(t *testing.T) {
	g := &fakeProxy{provider: &scriptedProvider{
		tools:  []mcp.Tool{{Name: "mcp__s__t"}},
		result: echoResult(),
	}}
	cd := &countingDial{inner: g.dial}
	conn := NewConn(Options{dial: cd.dial})
	defer conn.Close()

	for i := range 3 {
		if _, err := conn.CallTool(context.Background(), "mcp__s__t", json.RawMessage(`{}`)); err != nil {
			t.Fatalf("CallTool %d: %v", i, err)
		}
	}
	if got := cd.count.Load(); got != 1 {
		t.Fatalf("dial count = %d, want 1 (connection reused)", got)
	}
}

// togglableDial wraps fakeProxy.dial but can be switched to fail, so a test
// can drive the dial-failure -> backoff-gate path deterministically.
type togglableDial struct {
	inner    func(ctx context.Context) (io.ReadWriteCloser, error)
	count    atomic.Int32
	failNext atomic.Bool
}

var errDialFail = errors.New("dial refused")

func (d *togglableDial) dial(ctx context.Context) (io.ReadWriteCloser, error) {
	d.count.Add(1)
	if d.failNext.Load() {
		return nil, errDialFail
	}
	return d.inner(ctx)
}

func TestConnDropThenBackoffGateThenReconnect(t *testing.T) {
	g := &fakeProxy{provider: &scriptedProvider{
		tools:  []mcp.Tool{{Name: "mcp__s__t"}},
		result: echoResult(),
	}}
	td := &togglableDial{inner: g.dial}

	clock := &fakeClock{t: time.Unix(1000, 0)}
	conn := NewConn(Options{dial: td.dial, now: clock.now})
	defer conn.Close()

	// First call connects.
	if _, err := conn.CallTool(context.Background(), "mcp__s__t", json.RawMessage(`{}`)); err != nil {
		t.Fatalf("first CallTool: %v", err)
	}
	if td.count.Load() != 1 {
		t.Fatalf("dial count after first call = %d, want 1", td.count.Load())
	}

	// Drop the proxy session out from under the connection, and arrange for the
	// next dial to fail so the backoff gate is set.
	g.closeSession(t)
	td.failNext.Store(true)

	// Next call observes a connection error and drops the client.
	_, err := conn.CallTool(context.Background(), "mcp__s__t", json.RawMessage(`{}`))
	if err == nil {
		t.Fatal("expected error after proxy drop, got nil")
	}
	if !isConnError(err) {
		t.Fatalf("post-drop error not classified as conn error: %v", err)
	}

	// The next call attempts a redial, which fails and arms the backoff gate.
	_, err = conn.CallTool(context.Background(), "mcp__s__t", json.RawMessage(`{}`))
	if !errors.Is(err, errDialFail) {
		t.Fatalf("expected dial failure, got %v", err)
	}
	dialsAfterFail := td.count.Load()

	// Position the clock strictly before nextTry so the gate is deterministically
	// closed regardless of the jittered backoff draw (which can be as small as 0).
	conn.mu.Lock()
	nextTry := conn.nextTry
	conn.mu.Unlock()
	clock.mu.Lock()
	clock.t = nextTry.Add(-time.Nanosecond)
	clock.mu.Unlock()

	// A call within the backoff window fast-fails WITHOUT dialing.
	_, err = conn.CallTool(context.Background(), "mcp__s__t", json.RawMessage(`{}`))
	if err == nil {
		t.Fatal("expected unavailable error within backoff window")
	}
	if !errors.Is(err, errDialFail) {
		t.Fatalf("unavailable error does not wrap lastErr: %v", err)
	}
	if td.count.Load() != dialsAfterFail {
		t.Fatalf("dialed within backoff window: count %d -> %d", dialsAfterFail, td.count.Load())
	}

	// Advance the clock past the backoff gate and let the dial succeed; the next
	// call redials successfully.
	clock.advance(time.Hour)
	td.failNext.Store(false)
	if _, err := conn.CallTool(context.Background(), "mcp__s__t", json.RawMessage(`{}`)); err != nil {
		t.Fatalf("CallTool after backoff window: %v", err)
	}
	if td.count.Load() != dialsAfterFail+1 {
		t.Fatalf("dial count = %d, want %d (one redial)", td.count.Load(), dialsAfterFail+1)
	}
	// Failures reset on a successful reconnect.
	conn.mu.Lock()
	failures := conn.failures
	conn.mu.Unlock()
	if failures != 0 {
		t.Fatalf("failures = %d after successful reconnect, want 0", failures)
	}
}

func TestConnCtxCancelDoesNotDrop(t *testing.T) {
	// A provider that blocks until its context is cancelled, so we can cancel an
	// in-flight call and confirm the connection survives.
	bp := &blockingProvider{tools: []mcp.Tool{{Name: "mcp__s__t"}}}
	g := &fakeProxy{}
	cd := &countingDial{inner: g.dialWith(bp)}
	conn := NewConn(Options{dial: cd.dial})
	defer conn.Close()

	// Warm the connection with a quick non-blocking call path: cancel the first
	// call after it is in flight.
	ctx, cancel := context.WithCancel(context.Background())
	bp.entered = make(chan struct{}, 1)
	go func() {
		<-bp.entered
		cancel()
	}()
	_, err := conn.CallTool(ctx, "mcp__s__t", json.RawMessage(`{}`))
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context.Canceled, got %v", err)
	}
	if cd.count.Load() != 1 {
		t.Fatalf("dial count = %d after cancelled call, want 1", cd.count.Load())
	}

	// The connection must still be live: a follow-up call reuses it (no redial).
	bp.unblock = true
	bp.result = echoResult()
	if _, err := conn.CallTool(context.Background(), "mcp__s__t", json.RawMessage(`{}`)); err != nil {
		t.Fatalf("follow-up CallTool: %v", err)
	}
	if cd.count.Load() != 1 {
		t.Fatalf("dial count = %d after follow-up, want 1 (connection reused)", cd.count.Load())
	}
}

func TestConnConcurrentDropNoDoubleClose(t *testing.T) {
	g := &fakeProxy{provider: &scriptedProvider{
		tools:  []mcp.Tool{{Name: "mcp__s__t"}},
		result: echoResult(),
	}}
	cd := &countingDial{inner: g.dial}
	conn := NewConn(Options{dial: cd.dial})
	defer conn.Close()

	// Connect.
	if _, err := conn.CallTool(context.Background(), "mcp__s__t", json.RawMessage(`{}`)); err != nil {
		t.Fatalf("first CallTool: %v", err)
	}
	g.closeSession(t)

	// Fire many concurrent calls that all observe the drop; none must double-close.
	var wg sync.WaitGroup
	for range 16 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = conn.CallTool(context.Background(), "mcp__s__t", json.RawMessage(`{}`))
		}()
	}
	wg.Wait()
	// If we got here under -race without panic, the identity-checked drop held.
}

func TestConnDirtyOnToolsChanged(t *testing.T) {
	g := &fakeProxy{
		provider:    &scriptedProvider{tools: []mcp.Tool{{Name: "mcp__s__t"}}, result: echoResult()},
		listChanged: true,
	}
	conn := NewConn(Options{dial: g.dial})
	defer conn.Close()

	if conn.Dirty() {
		t.Fatal("Dirty() = true before any notification")
	}
	// Connect (this brings the session up).
	if _, err := conn.ListTools(context.Background()); err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	g.notifyListChanged(t)

	// The notification handler runs on the client read goroutine; poll for it.
	if !waitFor(func() bool { return conn.Dirty() }) {
		t.Fatal("Dirty() never became true after tools/list_changed")
	}
	conn.ClearDirty()
	if conn.Dirty() {
		t.Fatal("Dirty() = true after ClearDirty()")
	}
}

func TestConnNoGoroutineLeak(t *testing.T) {
	base := runtime.NumGoroutine()
	for range 5 {
		g := &fakeProxy{provider: &scriptedProvider{
			tools:  []mcp.Tool{{Name: "mcp__s__t"}},
			result: echoResult(),
		}}
		conn := NewConn(Options{dial: g.dial})
		if _, err := conn.CallTool(context.Background(), "mcp__s__t", json.RawMessage(`{}`)); err != nil {
			t.Fatalf("CallTool: %v", err)
		}
		g.closeSession(t)
		// Trigger the drop.
		_, _ = conn.CallTool(context.Background(), "mcp__s__t", json.RawMessage(`{}`))
		_ = conn.Close()
	}
	if !waitFor(func() bool { return runtime.NumGoroutine() <= base+2 }) {
		t.Fatalf("goroutine leak: started near %d, ended at %d", base, runtime.NumGoroutine())
	}
}

func TestIsConnError(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"peer closed", jsonrpc.ErrPeerClosed, true},
		{"wrapped peer closed", errors.New("x: " + jsonrpc.ErrPeerClosed.Error()), false},
		{"errorf wrapped peer closed", wrap(jsonrpc.ErrPeerClosed), true},
		{"eof", io.EOF, true},
		{"unexpected eof", io.ErrUnexpectedEOF, true},
		{"net closed", net.ErrClosed, true},
		{"op error", &net.OpError{Op: "read", Err: errors.New("boom")}, true},
		{"ctx canceled", context.Canceled, false},
		{"ctx deadline", context.DeadlineExceeded, false},
		{"plain error", errors.New("nope"), false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isConnError(tt.err); got != tt.want {
				t.Fatalf("isConnError(%v) = %v, want %v", tt.err, got, tt.want)
			}
		})
	}
}

func wrap(err error) error { return &wrapErr{err} }

type wrapErr struct{ err error }

func (w *wrapErr) Error() string { return "wrapped: " + w.err.Error() }
func (w *wrapErr) Unwrap() error { return w.err }

// fakeClock is a deterministic, injectable clock.
type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *fakeClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

// waitFor polls cond up to a bounded iteration budget, yielding between checks.
// It avoids sleeps per the test guidelines.
func waitFor(cond func() bool) bool {
	for range 100000 {
		if cond() {
			return true
		}
		runtime.Gosched()
	}
	return cond()
}
