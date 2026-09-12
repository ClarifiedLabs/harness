package mcptools

import (
	"context"
	"errors"
	"io"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestCloseIsTerminalWithoutDialing(t *testing.T) {
	var calls atomic.Int32
	conn := NewConn(Options{Dial: func(context.Context) (io.ReadWriteCloser, error) {
		calls.Add(1)
		return nil, errors.New("unexpected dial")
	}})
	if err := conn.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.ListTools(context.Background()); !errors.Is(err, ErrClosed) {
		t.Fatalf("ListTools after Close: %v", err)
	}
	if _, err := conn.CallTool(context.Background(), "test", nil); !errors.Is(err, ErrClosed) {
		t.Fatalf("CallTool after Close: %v", err)
	}
	if calls.Load() != 0 {
		t.Fatal("closed connection attempted to reconnect")
	}
}

func TestCloseCancelsAndJoinsDial(t *testing.T) {
	entered := make(chan struct{})
	conn := NewConn(Options{Dial: func(ctx context.Context) (io.ReadWriteCloser, error) {
		close(entered)
		<-ctx.Done()
		return nil, ctx.Err()
	}})
	result := make(chan error, 1)
	go func() { _, err := conn.ListTools(context.Background()); result <- err }()
	<-entered
	closed := make(chan struct{})
	go func() { defer close(closed); _ = conn.Close() }()
	select {
	case <-closed:
	case <-time.After(time.Second):
		t.Fatal("Close blocked behind uncanceled dial")
	}
	if err := <-result; !errors.Is(err, context.Canceled) {
		t.Fatalf("dial error = %v", err)
	}
	if _, err := conn.ListTools(context.Background()); !errors.Is(err, ErrClosed) {
		t.Fatalf("late reconnect = %v", err)
	}
}

func TestConcurrentCloseDoesNotPermitReconnect(t *testing.T) {
	g := &fakeProxy{provider: &scriptedProvider{result: echoResult()}}
	cd := &countingDial{inner: g.dial}
	conn := NewConn(Options{Dial: cd.dial})
	if _, err := conn.ListTools(t.Context()); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for range 10 {
		wg.Go(func() { _ = conn.Close() })
	}
	wg.Wait()
	if _, err := conn.ListTools(t.Context()); !errors.Is(err, ErrClosed) {
		t.Fatalf("late ListTools = %v", err)
	}
	if cd.count.Load() != 1 {
		t.Fatalf("dial count = %d", cd.count.Load())
	}
}
