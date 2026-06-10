package jsonrpc

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"sync"
	"testing"
)

// pipePeers wires two peers over net.Pipe and registers them for cleanup.
func pipePeers(t *testing.T, aOpts, bOpts PeerOptions) (*Peer, *Peer) {
	t.Helper()
	ca, cb := net.Pipe()
	a := NewPeer(ca, aOpts)
	b := NewPeer(cb, bOpts)
	t.Cleanup(func() {
		a.Close()
		b.Close()
	})
	return a, b
}

func TestCallResponseCorrelation(t *testing.T) {
	// B echoes the method name back as the result.
	echo := func(method string) Handler {
		return func(ctx context.Context, params json.RawMessage) (json.RawMessage, *Error) {
			return json.RawMessage(strconv.Quote(method)), nil
		}
	}
	handlers := map[string]Handler{}
	const n = 50
	for i := range n {
		m := fmt.Sprintf("m%d", i)
		handlers[m] = echo(m)
	}
	a, _ := pipePeers(t, PeerOptions{}, PeerOptions{Handlers: handlers})

	var wg sync.WaitGroup
	errCh := make(chan error, n)
	for i := range n {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			method := fmt.Sprintf("m%d", i)
			res, err := a.Call(context.Background(), method, nil)
			if err != nil {
				errCh <- fmt.Errorf("call %s: %w", method, err)
				return
			}
			var got string
			if err := json.Unmarshal(res, &got); err != nil {
				errCh <- err
				return
			}
			if got != method {
				errCh <- fmt.Errorf("cross-talk: method %s got result %q", method, got)
			}
		}(i)
	}
	wg.Wait()
	close(errCh)
	for err := range errCh {
		t.Error(err)
	}
}

func TestCallUnknownMethod(t *testing.T) {
	a, _ := pipePeers(t, PeerOptions{}, PeerOptions{})
	_, err := a.Call(context.Background(), "nope", nil)
	var je *Error
	if !errors.As(err, &je) {
		t.Fatalf("expected *Error, got %v", err)
	}
	if je.Code != CodeMethodNotFound {
		t.Fatalf("code = %d, want %d", je.Code, CodeMethodNotFound)
	}
}

func TestNotificationRouting(t *testing.T) {
	got := make(chan string, 1)
	notifications := map[string]NotificationHandler{
		"event": func(ctx context.Context, params json.RawMessage) {
			got <- string(params)
		},
	}
	a, _ := pipePeers(t, PeerOptions{}, PeerOptions{Notifications: notifications})

	if err := a.Notify("event", json.RawMessage(`{"k":1}`)); err != nil {
		t.Fatalf("notify: %v", err)
	}
	if v := <-got; v != `{"k":1}` {
		t.Fatalf("notification params = %s", v)
	}
	// Unknown notification must be ignored (no panic, no response). If it
	// produced a response the peer would still function; assert a subsequent
	// notification still arrives.
	if err := a.Notify("unknown", nil); err != nil {
		t.Fatalf("notify unknown: %v", err)
	}
	if err := a.Notify("event", json.RawMessage(`{"k":2}`)); err != nil {
		t.Fatalf("notify: %v", err)
	}
	if v := <-got; v != `{"k":2}` {
		t.Fatalf("notification params = %s", v)
	}
}

// TestInvalidInboundMessagesDropped feeds the peer messages that classify as
// KindInvalid ({}) and as a no-waiter KindResponse (an id-only object), then
// proves the peer dropped them without panic and is still alive by serving a
// subsequent Call. This closes the route() default-arm coverage gap.
func TestInvalidInboundMessagesDropped(t *testing.T) {
	ca, cb := net.Pipe()
	a := NewPeer(ca, PeerOptions{})
	t.Cleanup(func() { a.Close() })

	// Drive the far end by hand: push invalid lines, then answer A's Call.
	far := make(chan error, 1)
	go func() {
		enc := NewEncoder(cb)
		dec := NewDecoder(cb)
		// A message with neither method nor id -> KindInvalid -> dropped.
		if _, err := cb.Write([]byte("{}\n")); err != nil {
			far <- err
			return
		}
		// An id-only object -> KindResponse with no waiter -> dropped.
		if _, err := cb.Write([]byte(`{"jsonrpc":"2.0","id":42}` + "\n")); err != nil {
			far <- err
			return
		}
		// A's read loop must still be running: read its request and reply.
		req, err := dec.Decode()
		if err != nil {
			far <- err
			return
		}
		if req.Kind() != KindRequest || req.Method != "alive" {
			far <- fmt.Errorf("unexpected request: %+v", req)
			return
		}
		far <- enc.Encode(NewResponse(*req.ID, json.RawMessage(`"ok"`)))
	}()

	res, err := a.Call(context.Background(), "alive", nil)
	if err != nil {
		t.Fatalf("peer not alive after invalid inbound messages: %v", err)
	}
	if string(res) != `"ok"` {
		t.Fatalf("result = %s, want \"ok\"", res)
	}
	if err := <-far; err != nil {
		t.Fatalf("far end: %v", err)
	}
}

func TestResponseIDStringAndNumberDoNotCollide(t *testing.T) {
	ca, cb := net.Pipe()
	a := NewPeer(ca, PeerOptions{})
	t.Cleanup(func() { a.Close() })

	done := make(chan error, 1)
	go func() {
		dec := NewDecoder(cb)
		enc := NewEncoder(cb)
		req, err := dec.Decode()
		if err != nil {
			done <- err
			return
		}
		if req.ID == nil {
			done <- fmt.Errorf("request missing id")
			return
		}
		wrong := StringID(req.ID.String())
		if err := enc.Encode(NewResponse(wrong, json.RawMessage(`"wrong"`))); err != nil {
			done <- err
			return
		}
		done <- enc.Encode(NewResponse(*req.ID, json.RawMessage(`"right"`)))
	}()

	res, err := a.Call(context.Background(), "m", nil)
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	if string(res) != `"right"` {
		t.Fatalf("response id collision delivered %s, want \"right\"", res)
	}
	if err := <-done; err != nil {
		t.Fatalf("far end: %v", err)
	}
}

func TestHandlerPanicBecomesInternalError(t *testing.T) {
	handlers := map[string]Handler{
		"boom": func(ctx context.Context, params json.RawMessage) (json.RawMessage, *Error) {
			panic("kaboom")
		},
	}
	a, _ := pipePeers(t, PeerOptions{}, PeerOptions{Handlers: handlers})
	_, err := a.Call(context.Background(), "boom", nil)
	var je *Error
	if !errors.As(err, &je) {
		t.Fatalf("expected *Error, got %v", err)
	}
	if je.Code != CodeInternal {
		t.Fatalf("code = %d, want %d", je.Code, CodeInternal)
	}
}

// TestHandlerWithIDReceivesRequestID proves an id-aware handler sees the inbound
// request id, that it takes precedence over a same-method plain Handler, and
// that a panic in it still becomes CodeInternal.
func TestHandlerWithIDReceivesRequestID(t *testing.T) {
	gotID := make(chan string, 1)
	bOpts := PeerOptions{
		// A plain Handler for the same method must be shadowed by the id-aware one.
		Handlers: map[string]Handler{
			"track": func(ctx context.Context, params json.RawMessage) (json.RawMessage, *Error) {
				return json.RawMessage(`"plain"`), nil
			},
		},
		HandlersWithID: map[string]HandlerID{
			"track": func(ctx context.Context, id ID, params json.RawMessage) (json.RawMessage, *Error) {
				gotID <- id.String()
				return json.RawMessage(`"id-aware"`), nil
			},
			"boom": func(ctx context.Context, id ID, params json.RawMessage) (json.RawMessage, *Error) {
				panic("kaboom")
			},
		},
	}
	a, _ := pipePeers(t, PeerOptions{}, bOpts)

	res, err := a.Call(context.Background(), "track", nil)
	if err != nil {
		t.Fatalf("call track: %v", err)
	}
	if string(res) != `"id-aware"` {
		t.Fatalf("id-aware handler should take precedence, got %s", res)
	}
	id := <-gotID
	if id == "" {
		t.Fatal("handler received an empty (unset) id")
	}

	_, err = a.Call(context.Background(), "boom", nil)
	var je *Error
	if !errors.As(err, &je) || je.Code != CodeInternal {
		t.Fatalf("panic in id-aware handler: got %v, want CodeInternal", err)
	}
}

func TestCallContextCancel(t *testing.T) {
	release := make(chan struct{})
	entered := make(chan struct{})
	handlers := map[string]Handler{
		"slow": func(ctx context.Context, params json.RawMessage) (json.RawMessage, *Error) {
			close(entered)
			<-release
			return json.RawMessage(`{}`), nil
		},
	}
	a, _ := pipePeers(t, PeerOptions{}, PeerOptions{Handlers: handlers})

	ctx, cancel := context.WithCancel(context.Background())
	var onCancelCount int32
	var mu sync.Mutex
	cancelled := make(chan struct{})
	opts := CallOpts{OnCancel: func(id ID) {
		mu.Lock()
		onCancelCount++
		mu.Unlock()
		close(cancelled)
	}}

	resCh := make(chan error, 1)
	go func() {
		_, err := a.CallWith(ctx, "slow", nil, opts)
		resCh <- err
	}()

	<-entered // handler is running; the request was delivered
	cancel()

	err := <-resCh
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context.Canceled, got %v", err)
	}
	<-cancelled
	mu.Lock()
	if onCancelCount != 1 {
		t.Fatalf("OnCancel fired %d times, want 1", onCancelCount)
	}
	mu.Unlock()

	// Release the handler so its (now orphaned) response reaches A's read loop;
	// it must be dropped without panic. A fresh call afterward proves the peer
	// is still alive (its read loop processed the late response cleanly).
	close(release)
	if _, err := a.Call(context.Background(), "still-alive", nil); !errors.As(err, new(*Error)) {
		t.Fatalf("peer not alive after late response: %v", err)
	}
}

func TestCloseFailsPendingCalls(t *testing.T) {
	entered := make(chan struct{})
	handlers := map[string]Handler{
		"hang": func(ctx context.Context, params json.RawMessage) (json.RawMessage, *Error) {
			close(entered)
			<-ctx.Done() // block until peer shuts down
			return nil, NewError(CodeInternal, "cancelled")
		},
	}
	ca, cb := net.Pipe()
	a := NewPeer(ca, PeerOptions{})
	b := NewPeer(cb, PeerOptions{Handlers: handlers})

	resCh := make(chan error, 1)
	go func() {
		_, err := a.Call(context.Background(), "hang", nil)
		resCh <- err
	}()
	<-entered

	if err := b.Close(); err != nil {
		t.Fatalf("close b: %v", err)
	}

	err := <-resCh
	if !errors.Is(err, ErrPeerClosed) {
		t.Fatalf("expected ErrPeerClosed, got %v", err)
	}

	// a's read loop sees EOF when b closes; Done closes and Err is set.
	<-a.Done()
	if a.Err() == nil {
		t.Fatal("a.Err() should be non-nil after Done")
	}
	a.Close()
}

func TestCleanCloseErrIsEOF(t *testing.T) {
	ca, cb := net.Pipe()
	a := NewPeer(ca, PeerOptions{})
	b := NewPeer(cb, PeerOptions{})
	// Close b's underlying conn cleanly by closing the peer; a should see EOF.
	b.Close()
	<-a.Done()
	if err := a.Err(); !errors.Is(err, io.EOF) {
		t.Fatalf("a.Err() = %v, want io.EOF", err)
	}
	a.Close()
}

func TestCloseIdempotent(t *testing.T) {
	ca, _ := net.Pipe()
	a := NewPeer(ca, PeerOptions{})
	if err := a.Close(); err != nil {
		t.Fatalf("close 1: %v", err)
	}
	if err := a.Close(); err != nil {
		t.Fatalf("close 2: %v", err)
	}
	if !errors.Is(a.Err(), ErrPeerClosed) {
		t.Fatalf("Err() = %v, want ErrPeerClosed", a.Err())
	}
}

func TestNotifyAfterCloseReturnsErrPeerClosed(t *testing.T) {
	ca, cb := net.Pipe()
	a := NewPeer(ca, PeerOptions{})

	if err := a.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	_ = cb.Close()

	for i := range outBufferSize {
		if err := a.Notify("closed", nil); !errors.Is(err, ErrPeerClosed) {
			t.Fatalf("notify %d after close = %v, want ErrPeerClosed", i, err)
		}
	}
}

// TestWriteInterleaveStress fires many large requests from two goroutines; the
// reader on the other end must see every line parse as valid JSON, proving the
// single-writer serialization holds.
func TestWriteInterleaveStress(t *testing.T) {
	ca, cb := net.Pipe()
	a := NewPeer(ca, PeerOptions{})
	t.Cleanup(func() { a.Close() })

	// cb is the raw reader; decode every line and assert it parses.
	const perGoroutine = 100
	const goroutines = 2
	total := perGoroutine * goroutines

	parsed := make(chan error, total)
	readerDone := make(chan struct{})
	go func() {
		defer close(readerDone)
		dec := NewDecoder(cb)
		for range total {
			m, err := dec.Decode()
			if err != nil {
				parsed <- err
				return
			}
			// Re-marshal params to confirm it is valid JSON.
			var v any
			if len(m.Params) > 0 {
				if err := json.Unmarshal(m.Params, &v); err != nil {
					parsed <- fmt.Errorf("invalid params line: %w", err)
					return
				}
			}
			parsed <- nil
		}
	}()

	bigPayload := json.RawMessage(`{"blob":"` + strings.Repeat("z", 2000) + `"}`)
	var wg sync.WaitGroup
	for range goroutines {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range perGoroutine {
				// Use Notify so we don't need a responder; framing is what matters.
				if err := a.Notify("stress", bigPayload); err != nil {
					return
				}
			}
		}()
	}
	wg.Wait()

	<-readerDone
	close(parsed)
	for err := range parsed {
		if err != nil {
			t.Fatalf("reader: %v", err)
		}
	}
}
