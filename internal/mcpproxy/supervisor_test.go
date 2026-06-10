package mcpproxy

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"os/exec"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

// syscallZero is the no-op signal used to probe whether a pid is still alive.
var syscallZero = syscall.Signal(0)

// helperSpawn returns a spawn func that launches the TestHelperProcess fake MCP
// server with the given extra env. It mirrors the canonical os/exec test idiom.
func helperSpawn(t *testing.T, env map[string]string) func() *exec.Cmd {
	t.Helper()
	return func() *exec.Cmd {
		// Canonical os/exec test idiom: re-exec the test binary into the helper.
		cmd := exec.Command(os.Args[0], "-test.run=TestHelperProcess$") // nosemgrep: dangerous-exec-command
		cmd.Env = append(os.Environ(), "HELPER_MODE=mcp")
		for k, v := range env {
			cmd.Env = append(cmd.Env, k+"="+v)
		}
		return cmd
	}
}

// testLogBuf is a thread-safe buffer for capturing log output in tests.
type testLogBuf struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *testLogBuf) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *testLogBuf) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func newTestLogger(w *testLogBuf) *slog.Logger {
	return slog.New(slog.NewTextHandler(w, &slog.HandlerOptions{Level: slog.LevelDebug}))
}

// waitFor polls fn until it returns true or the deadline elapses. Used only to
// await an async state transition driven by the supervisor's own goroutines;
// coordination of test logic uses channels, not sleeps.
func waitFor(t *testing.T, d time.Duration, fn func() bool) {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if fn() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("condition not met within %s", d)
}

func newStdioSupervisor(t *testing.T, rs ResolvedServer, env map[string]string, logger *slog.Logger, onChanged func()) *Supervisor {
	t.Helper()
	sup := NewSupervisor(rs, logger)
	sup.onToolsChanged = onChanged
	sup.spawn = helperSpawn(t, env)
	sup.sleep = func(context.Context, time.Duration) {} // no real backoff waits
	return sup
}

func TestSupervisorStdioInitAndList(t *testing.T) {
	var lb testLogBuf
	rs := ResolvedServer{Name: "h", Transport: TransportStdio, Command: "helper"}
	sup := newStdioSupervisor(t, rs, map[string]string{"HELPER_TOOLS": "echo,ping"}, newTestLogger(&lb), nil)

	ctx := t.Context()
	sup.Start(ctx)
	defer sup.Shutdown(context.Background())

	waitFor(t, 5*time.Second, func() bool { return sup.State() == StateReady })
	tools := sup.Tools()
	if len(tools) != 2 {
		t.Fatalf("want 2 tools, got %d (%+v)", len(tools), tools)
	}
	names := []string{tools[0].Name, tools[1].Name}
	if !slices.Contains(names, "echo") || !slices.Contains(names, "ping") {
		t.Fatalf("unexpected tool names: %v", names)
	}
}

func TestSupervisorStdioEchoCall(t *testing.T) {
	rs := ResolvedServer{Name: "h", Transport: TransportStdio, Command: "helper"}
	sup := newStdioSupervisor(t, rs, nil, slog.New(slog.DiscardHandler), nil)
	ctx := t.Context()
	sup.Start(ctx)
	defer sup.Shutdown(context.Background())
	waitFor(t, 5*time.Second, func() bool { return sup.State() == StateReady })

	res, err := sup.CallTool(context.Background(), "echo", json.RawMessage(`{"hello":"world"}`))
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	if res.IsError {
		t.Fatalf("unexpected error result: %+v", res)
	}
	if len(res.Content) != 1 || res.Content[0].Text != `{"hello":"world"}` {
		t.Fatalf("echo round-trip wrong: %+v", res.Content)
	}
}

func TestSupervisorStderrLogged(t *testing.T) {
	var lb testLogBuf
	rs := ResolvedServer{Name: "noisy", Transport: TransportStdio, Command: "helper"}
	sup := newStdioSupervisor(t, rs, map[string]string{"HELPER_STDERR": "downstream-noise-line"}, newTestLogger(&lb), nil)
	ctx := t.Context()
	sup.Start(ctx)
	defer sup.Shutdown(context.Background())
	waitFor(t, 5*time.Second, func() bool { return sup.State() == StateReady })
	waitFor(t, 2*time.Second, func() bool { return strings.Contains(lb.String(), "downstream-noise-line") })
	if !strings.Contains(lb.String(), "noisy") {
		t.Fatalf("stderr log missing server name attr: %s", lb.String())
	}
}

// TestSupervisorStderrBurstDoesNotWedgeCalls is the regression for the
// over-long-stderr-line drain bug: a downstream server that emits a single
// newline-free stderr chunk larger than the 1 MB scanner buffer must not wedge
// its own tool calls. Before the io.Copy(io.Discard) fallback, the scanner
// stopped for good on the buffer-overflow error, the child then blocked on
// write(2) once the OS pipe buffer filled, and every subsequent tools/call hung
// until shutdown. The burst is 2 MB to exceed the 1 MB cap with margin.
func TestSupervisorStderrBurstDoesNotWedgeCalls(t *testing.T) {
	rs := ResolvedServer{Name: "noisy", Transport: TransportStdio, Command: "helper"}
	sup := newStdioSupervisor(t, rs, map[string]string{"HELPER_STDERR_BURST": "2097152"}, slog.New(slog.DiscardHandler), nil)
	ctx := t.Context()
	sup.Start(ctx)
	defer sup.Shutdown(context.Background())
	waitFor(t, 5*time.Second, func() bool { return sup.State() == StateReady })

	// With the drain fallback, the child never blocks on stderr, so this call
	// completes; without it the call hangs until the test's CallTool ctx fires.
	callCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	res, err := sup.CallTool(callCtx, "echo", json.RawMessage(`{"after":"burst"}`))
	if err != nil {
		t.Fatalf("CallTool after stderr burst: %v", err)
	}
	if res.IsError {
		t.Fatalf("unexpected error result after stderr burst: %+v", res)
	}
	if len(res.Content) != 1 || res.Content[0].Text != `{"after":"burst"}` {
		t.Fatalf("echo round-trip wrong after stderr burst: %+v", res.Content)
	}
}

func TestSupervisorCrashRestartsAndRecaches(t *testing.T) {
	var changed atomic.Int32
	rs := ResolvedServer{Name: "h", Transport: TransportStdio, Command: "helper"}
	sup := newStdioSupervisor(t, rs,
		map[string]string{
			"HELPER_EXIT_AFTER_CALLS": "1",
			"HELPER_TOOLS":            "echo",
		},
		slog.New(slog.DiscardHandler),
		func() { changed.Add(1) })
	ctx := t.Context()
	sup.Start(ctx)
	defer sup.Shutdown(context.Background())
	waitFor(t, 5*time.Second, func() bool { return sup.State() == StateReady })

	// The initial successful start fires onToolsChanged (nil -> [echo]).
	waitFor(t, 2*time.Second, func() bool { return changed.Load() >= 1 })

	// One call; the helper exits afterwards, forcing a restart.
	if _, err := sup.CallTool(context.Background(), "echo", json.RawMessage(`{}`)); err != nil {
		t.Fatalf("CallTool: %v", err)
	}

	// Await the restart: a second successful initialize re-caches the tools.
	waitFor(t, 5*time.Second, func() bool { return sup.Starts() >= 2 && sup.State() == StateReady })
	if len(sup.Tools()) != 1 || sup.Tools()[0].Name != "echo" {
		t.Fatalf("tools not re-cached after restart: %+v", sup.Tools())
	}

	// A subsequent call succeeds against the restarted child.
	res, err := sup.CallTool(context.Background(), "echo", json.RawMessage(`{"after":"restart"}`))
	if err != nil {
		t.Fatalf("CallTool after restart: %v", err)
	}
	if res.IsError {
		t.Fatalf("call after restart errored: %+v", res)
	}
}

func TestSupervisorListChangedRefreshesTools(t *testing.T) {
	var changed atomic.Int32
	rs := ResolvedServer{Name: "h", Transport: TransportStdio, Command: "helper"}
	sup := newStdioSupervisor(t, rs,
		map[string]string{
			"HELPER_EMIT_LIST_CHANGED": "1",
			"HELPER_TOOLS":             "echo",
			"HELPER_TOOLS2":            "echo,added",
		},
		slog.New(slog.DiscardHandler),
		func() { changed.Add(1) })
	ctx := t.Context()
	sup.Start(ctx)
	defer sup.Shutdown(context.Background())
	waitFor(t, 5*time.Second, func() bool { return sup.State() == StateReady })

	// The first call makes the helper emit tools/list_changed (and keep running),
	// switching its advertised tool set to [echo, added].
	if _, err := sup.CallTool(context.Background(), "echo", json.RawMessage(`{}`)); err != nil {
		t.Fatalf("CallTool: %v", err)
	}

	// The supervisor refreshes its cache and fires onToolsChanged.
	waitFor(t, 5*time.Second, func() bool { return len(sup.Tools()) == 2 })
	tools := sup.Tools()
	if !slices.Contains([]string{tools[0].Name, tools[1].Name}, "added") {
		t.Fatalf("refreshed tools missing new tool: %+v", tools)
	}
	if changed.Load() < 2 {
		t.Fatalf("onToolsChanged should fire on refresh, count=%d", changed.Load())
	}
}

func TestSupervisorFivePermanentFail(t *testing.T) {
	var lb testLogBuf
	rs := ResolvedServer{Name: "broken", Transport: TransportStdio, Command: "nonexistent"}
	sup := NewSupervisor(rs, newTestLogger(&lb))
	// Spawn a binary that does not exist so Start fails immediately every time.
	sup.spawn = func() *exec.Cmd {
		return exec.Command("/nonexistent/definitely/not/here/binary-xyz")
	}
	var sleeps atomic.Int32
	sup.sleep = func(context.Context, time.Duration) { sleeps.Add(1) }

	ctx := t.Context()
	sup.Start(ctx)
	defer sup.Shutdown(context.Background())

	waitFor(t, 5*time.Second, func() bool { return sup.State() == StateFailed })
	if !strings.Contains(lb.String(), "disabled after 5 restart attempts") {
		t.Fatalf("missing permanent-failure log: %s", lb.String())
	}
	// 5 attempts => exactly 5 backoff sleeps (one before each retry, including
	// the first restart). It must not loop forever.
	if got := sleeps.Load(); got > 5 {
		t.Fatalf("too many backoff sleeps: %d", got)
	}
}

func TestSupervisorUnavailableCallIsErrorResult(t *testing.T) {
	rs := ResolvedServer{Name: "broken", Transport: TransportStdio, Command: "nonexistent"}
	sup := NewSupervisor(rs, slog.New(slog.DiscardHandler))
	sup.spawn = func() *exec.Cmd { return exec.Command("/nonexistent/binary-xyz") }
	sup.sleep = func(context.Context, time.Duration) {}
	ctx := t.Context()
	sup.Start(ctx)
	defer sup.Shutdown(context.Background())
	waitFor(t, 5*time.Second, func() bool { return sup.State() == StateFailed })

	res, err := sup.CallTool(context.Background(), "echo", json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("unavailable call should return an isError RESULT, not an error: %v", err)
	}
	if res == nil || !res.IsError {
		t.Fatalf("want IsError result, got %+v", res)
	}
	if len(res.Content) == 0 || !strings.Contains(res.Content[0].Text, "broken") {
		t.Fatalf("error content should name the server: %+v", res)
	}
	if !strings.Contains(res.Content[0].Text, "unavailable") {
		t.Fatalf("error content should say unavailable: %+v", res)
	}
}

func TestSupervisorVersionErrorFailsNoRetry(t *testing.T) {
	var lb testLogBuf
	rs := ResolvedServer{Name: "h", Transport: TransportStdio, Command: "helper"}
	var spawns atomic.Int32
	sup := NewSupervisor(rs, newTestLogger(&lb))
	base := helperSpawn(t, map[string]string{"HELPER_BAD_VERSION": "1"})
	sup.spawn = func() *exec.Cmd { spawns.Add(1); return base() }
	sup.sleep = func(context.Context, time.Duration) {}
	ctx := t.Context()
	sup.Start(ctx)
	defer sup.Shutdown(context.Background())

	waitFor(t, 5*time.Second, func() bool { return sup.State() == StateFailed })
	// Give a moment to ensure no retry loop spins (state is terminal). The version
	// won't change, so we must not respawn beyond the initial attempt.
	if got := spawns.Load(); got != 1 {
		t.Fatalf("version error must not retry: spawned %d times", got)
	}
}

func TestSupervisorNoToolsCapIsReadyEmpty(t *testing.T) {
	rs := ResolvedServer{Name: "h", Transport: TransportStdio, Command: "helper"}
	sup := newStdioSupervisor(t, rs, map[string]string{"HELPER_NO_TOOLS_CAP": "1"}, slog.New(slog.DiscardHandler), nil)
	ctx := t.Context()
	sup.Start(ctx)
	defer sup.Shutdown(context.Background())
	waitFor(t, 5*time.Second, func() bool { return sup.State() == StateReady })
	if len(sup.Tools()) != 0 {
		t.Fatalf("server without tools cap should expose zero tools: %+v", sup.Tools())
	}
}

func TestSupervisorShutdownReapsChild(t *testing.T) {
	rs := ResolvedServer{Name: "h", Transport: TransportStdio, Command: "helper"}
	sup := newStdioSupervisor(t, rs, nil, slog.New(slog.DiscardHandler), nil)
	ctx := t.Context()
	sup.Start(ctx)
	waitFor(t, 5*time.Second, func() bool { return sup.State() == StateReady })

	pid := sup.childPID()
	if pid <= 0 {
		t.Fatalf("expected a live child pid, got %d", pid)
	}
	sup.Shutdown(context.Background())

	// The child must be gone (signal 0 fails once reaped).
	waitFor(t, 5*time.Second, func() bool {
		proc, err := os.FindProcess(pid)
		if err != nil {
			return true
		}
		return proc.Signal(syscallZero) != nil
	})
}

func TestSupervisorShutdownReapsChildBeforeReady(t *testing.T) {
	rs := ResolvedServer{Name: "h", Transport: TransportStdio, Command: "helper"}
	sup := newStdioSupervisor(t, rs, map[string]string{"HELPER_HANG_NO_INIT": "1"}, slog.New(slog.DiscardHandler), nil)
	ctx := t.Context()
	sup.Start(ctx)

	waitFor(t, 5*time.Second, func() bool { return sup.childPID() > 0 })
	pid := sup.childPID()
	if pid <= 0 {
		t.Fatalf("expected a live child pid, got %d", pid)
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		sup.Shutdown(context.Background())
	}()

	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("Shutdown did not return for pre-ready child")
	}

	waitFor(t, 5*time.Second, func() bool {
		proc, err := os.FindProcess(pid)
		if err != nil {
			return true
		}
		return proc.Signal(syscallZero) != nil
	})
}

func TestSupervisorInitListFailureReapsChild(t *testing.T) {
	rs := ResolvedServer{Name: "h", Transport: TransportStdio, Command: "helper"}
	sup := newStdioSupervisor(t, rs, map[string]string{"HELPER_FAIL_LIST": "1"}, slog.New(slog.DiscardHandler), nil)
	sup.sleep = func(ctx context.Context, _ time.Duration) { <-ctx.Done() }
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	sup.Start(ctx)
	defer sup.Shutdown(context.Background())

	waitFor(t, 5*time.Second, func() bool { return sup.childPID() > 0 })
	pid := sup.childPID()
	waitFor(t, 5*time.Second, func() bool { return sup.State() == StateRestarting })

	waitFor(t, 5*time.Second, func() bool {
		proc, err := os.FindProcess(pid)
		if err != nil {
			return true
		}
		return proc.Signal(syscallZero) != nil
	})
}
