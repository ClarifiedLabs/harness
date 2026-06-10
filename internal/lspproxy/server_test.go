package lspproxy

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"harness/internal/mcp"
	"harness/internal/mcp/jsonrpc"
)

// TestHelperProcess is re-invoked as a child to act as a fake LSP server speaking
// the Content-Length framing over its stdio. Its behavior is selected by
// LSP_HELPER_MODE. It is not a real test.
func TestHelperProcess(t *testing.T) {
	mode := os.Getenv("LSP_HELPER_MODE")
	if mode == "" {
		return
	}
	switch mode {
	case "crash":
		os.Exit(1)
	case "lsp":
		runFakeLSPStdio()
	}
	os.Exit(0)
}

// runFakeLSPStdio serves a minimal LSP server over os.Stdin/os.Stdout: it answers
// initialize, replies to shutdown, and exits on the exit notification.
func runFakeLSPStdio() {
	conn := mcp.NewStdioConn(os.Stdin, os.Stdout)
	var once sync.Once
	done := make(chan struct{})
	finish := func() { once.Do(func() { close(done) }) }
	peer := jsonrpc.NewPeerWithCodec(conn, NewDecoder(conn), NewEncoder(conn), jsonrpc.PeerOptions{
		Handlers: map[string]jsonrpc.Handler{
			"initialize": func(ctx context.Context, p json.RawMessage) (json.RawMessage, *jsonrpc.Error) {
				return json.RawMessage(`{"capabilities":{}}`), nil
			},
			"shutdown": func(ctx context.Context, p json.RawMessage) (json.RawMessage, *jsonrpc.Error) {
				return json.RawMessage("null"), nil
			},
		},
		Notifications: map[string]jsonrpc.NotificationHandler{
			"initialized": func(ctx context.Context, p json.RawMessage) {},
			"exit":        func(ctx context.Context, p json.RawMessage) { finish() },
		},
	})
	select {
	case <-done:
	case <-peer.Done():
	}
	_ = peer.Close()
}

// helperSpawn returns a spawn seam that re-invokes this test binary as a fake LSP
// child in the given mode.
func helperSpawn(mode string, counter *int32) func() *exec.Cmd {
	return func() *exec.Cmd {
		if counter != nil {
			atomic.AddInt32(counter, 1)
		}
		cmd := exec.Command(os.Args[0], "-test.run=TestHelperProcess$") // nosemgrep: dangerous-exec-command

		cmd.Env = append(os.Environ(), "LSP_HELPER_MODE="+mode)
		return cmd
	}
}

func TestServerInstanceEnsureIdempotent(t *testing.T) {
	inst := newServerInstance(ResolvedServer{Name: "fake", Command: []string{"unused"}}, "/tmp/proj", nil)
	inst.spawn = helperSpawn("lsp", nil)
	t.Cleanup(func() { inst.shutdown(context.Background()) })

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c1, err := inst.ensure(ctx)
	if err != nil {
		t.Fatalf("ensure: %v", err)
	}
	c2, err := inst.ensure(ctx)
	if err != nil || c1 != c2 {
		t.Fatalf("second ensure returned a different client (%p vs %p), err %v", c1, c2, err)
	}
	if inst.Starts() != 1 {
		t.Fatalf("starts = %d, want 1", inst.Starts())
	}
}

func TestServerInstanceRelaunchesAfterDeath(t *testing.T) {
	inst := newServerInstance(ResolvedServer{Name: "fake", Command: []string{"unused"}}, "/tmp/proj", nil)
	inst.spawn = helperSpawn("lsp", nil)
	t.Cleanup(func() { inst.shutdown(context.Background()) })

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c1, err := inst.ensure(ctx)
	if err != nil {
		t.Fatalf("ensure: %v", err)
	}
	_ = c1.Close() // simulate the child dying
	<-c1.Done()

	c2, err := inst.ensure(ctx)
	if err != nil {
		t.Fatalf("re-ensure: %v", err)
	}
	if c1 == c2 {
		t.Fatal("expected a fresh client after death")
	}
	if inst.Starts() != 2 {
		t.Fatalf("starts = %d, want 2", inst.Starts())
	}
}

func TestServerInstanceBackoffCapAndRevive(t *testing.T) {
	var spawns int32
	inst := newServerInstance(ResolvedServer{Name: "fake", Command: []string{"unused"}}, "/tmp/proj", nil)
	inst.spawn = helperSpawn("crash", &spawns)

	now := time.Unix(1000, 0)
	inst.clock = func() time.Time { return now }

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// Drive maxRestarts consecutive failures, advancing past each short backoff
	// gate between attempts (but not after the last, so the cooldown stays ahead).
	for i := range maxRestarts {
		if _, err := inst.ensure(ctx); err == nil {
			t.Fatalf("attempt %d: expected failure", i)
		}
		if i < maxRestarts-1 {
			now = now.Add(time.Hour)
		}
	}
	if got := atomic.LoadInt32(&spawns); got != maxRestarts {
		t.Fatalf("spawns after cap = %d, want %d", got, maxRestarts)
	}

	// Now in the failed cooldown: an ensure before the cooldown elapses must
	// fast-fail WITHOUT spawning again.
	now = now.Add(time.Second)
	if _, err := inst.ensure(ctx); err == nil {
		t.Fatal("expected fast-fail during cooldown")
	}
	if got := atomic.LoadInt32(&spawns); got != maxRestarts {
		t.Fatalf("spawns during cooldown = %d, want %d (no respawn)", got, maxRestarts)
	}

	// After the cooldown elapses, the next ensure revives (spawns once more).
	now = now.Add(failedCooldown + time.Second)
	if _, err := inst.ensure(ctx); err == nil {
		t.Fatal("expected failure on revive attempt (still crashing)")
	}
	if got := atomic.LoadInt32(&spawns); got != maxRestarts+1 {
		t.Fatalf("spawns after revive = %d, want %d", got, maxRestarts+1)
	}
}
