package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"harness/internal/agent"
	"harness/internal/config"
	"harness/internal/llm"
	"harness/internal/mcp"
	"harness/internal/tools"
)

func TestLSPRuntimeUsesNewGenerationAfterDisable(t *testing.T) {
	catalog := &tools.Registry{}
	r, err := newLSPRuntime(t.Context(), config.LSPConfig{Enable: true}, catalog, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	defer r.Shutdown()
	old, _ := r.snapshot()
	tool, ok := catalog.Lookup("lsp_definition")
	if !ok {
		t.Fatal("LSP tool missing")
	}
	r.SetEnabled(false)
	if _, err := tool.Run(t.Context(), json.RawMessage(`{}`)); err == nil {
		t.Fatal("stale tool remained enabled")
	}
	r.SetEnabled(true)
	current, enabled := r.snapshot()
	if !enabled || current == old {
		t.Fatal("enable reused the retired manager")
	}
	r.Shutdown()
	r.SetEnabled(true)
	if r.Enabled() {
		t.Fatal("final shutdown allowed re-enabling")
	}
}

func TestLSPRuntimeShutdownJoinsPrewarm(t *testing.T) {
	t.Chdir(t.TempDir()) // No project evidence: never launch installed servers.
	r, err := newLSPRuntime(t.Context(), config.LSPConfig{Enable: true, Prewarm: true}, &tools.Registry{}, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	done := r.prewarmDone
	if done == nil {
		t.Fatal("prewarm did not start")
	}
	r.Shutdown()
	select {
	case <-done:
	default:
		t.Fatal("shutdown left prewarm running")
	}
}

func TestTerminationSignalExitsActivePrompt(t *testing.T) {
	for _, sig := range []os.Signal{syscall.SIGTERM, syscall.SIGHUP} {
		t.Run(sig.String(), func(t *testing.T) {
			signals := make(chan os.Signal, 1)
			cancelled, exited := make(chan struct{}), make(chan struct{})
			watcher := agent.NewInterruptWatcher(signals, time.Now, func() { close(exited) })
			watcher.BeginPrompt(func() { close(cancelled) })
			stop := watcher.Start()
			defer stop()
			signals <- sig
			awaitLifecycle(t, cancelled)
			awaitLifecycle(t, exited)
		})
	}
}

func TestStartupSignalReceiverIsJoinedBeforeHandoff(t *testing.T) {
	signals := make(chan os.Signal, 1)
	_, stopStartup, _ := signalCancelContext(signals)
	stopStartup()
	exited := make(chan struct{})
	watcher := agent.NewInterruptWatcher(signals, time.Now, func() { close(exited) })
	stop := watcher.Start()
	defer stop()
	signals <- syscall.SIGTERM
	awaitLifecycle(t, exited)
}

func TestTextPipedInputCanBeTerminated(t *testing.T) {
	r, w := io.Pipe()
	defer r.Close()
	defer w.Close()
	exit := make(chan struct{})
	done := make(chan error, 1)
	go func() { _, err := buildPromptWithExit(exit, "", r, true); done <- err }()
	close(exit)
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("input error = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("termination blocked on piped input")
	}
}

func awaitLifecycle(t *testing.T, done <-chan struct{}) {
	t.Helper()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("lifecycle operation did not finish")
	}
}

// A fake MCP owner whose downstream child has a separate process group. EOF
// starts orderly cleanup, gated by a pipe so tests can race reconnect and exit
// without sleeps. Only that owner can normally clean its downstream group.
func TestLifecycleLocalHelper(t *testing.T) {
	mode := os.Getenv("HARNESS_LIFECYCLE_HELPER")
	if mode == "" {
		return
	}
	if mode == "worker" {
		fmt.Fprintln(os.Stdout, os.Getpid())
		r, w, _ := os.Pipe()
		defer w.Close()
		_, _ = io.Copy(io.Discard, r)
		os.Exit(0)
	}
	worker := exec.Command(os.Args[0], "-test.run=^TestLifecycleLocalHelper$")
	worker.Env = append(os.Environ(), "HARNESS_LIFECYCLE_HELPER=worker")
	worker.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	ready, err := worker.StdoutPipe()
	if err != nil {
		os.Exit(2)
	}
	if err := worker.Start(); err != nil {
		os.Exit(2)
	}
	var pid int
	if _, err := fmt.Fscanln(ready, &pid); err != nil {
		os.Exit(2)
	}
	events, err := os.OpenFile(os.Getenv("HARNESS_LIFECYCLE_EVENTS"), os.O_WRONLY, 0)
	if err != nil {
		os.Exit(2)
	}
	fmt.Fprintln(events, "spawn", os.Getpid(), pid)
	conn := mcp.NewStdioConn(os.Stdin, os.Stdout)
	_ = mcp.Serve(context.Background(), conn, mcp.ServerOptions{Info: mcp.Implementation{Name: "fake", Version: "0"}, Provider: &lifecycleLocalProvider{conn: conn}})
	fmt.Fprintln(events, "close", os.Getpid(), pid)
	gate, err := os.Open(os.Getenv("HARNESS_LIFECYCLE_GATE"))
	if err != nil {
		os.Exit(2)
	}
	var one [1]byte
	_, _ = gate.Read(one[:])
	_ = syscall.Kill(-pid, syscall.SIGTERM)
	_ = worker.Wait()
	os.Exit(0)
}

type lifecycleLocalProvider struct{ conn io.Closer }

func (*lifecycleLocalProvider) ListTools(context.Context, string) (mcp.ListToolsResult, error) {
	return mcp.ListToolsResult{Tools: []mcp.Tool{{Name: "ping", InputSchema: json.RawMessage(`{"type":"object"}`)}}}, nil
}
func (p *lifecycleLocalProvider) CallTool(_ context.Context, _ string, args json.RawMessage) (*mcp.CallToolResult, error) {
	if strings.Contains(string(args), "disconnect") {
		_ = p.conn.Close()
	}
	return &mcp.CallToolResult{Content: []mcp.ContentBlock{{Type: "text", Text: "pong"}}}, nil
}

func TestLocalReconnectExitJoinsOldNestedOwner(t *testing.T) {
	for _, serena := range []bool{false, true} {
		t.Run(fmt.Sprintf("serena=%v", serena), func(t *testing.T) {
			dir := t.TempDir()
			eventsPath, gatePath := filepath.Join(dir, "events"), filepath.Join(dir, "gate")
			for _, path := range []string{eventsPath, gatePath} {
				if err := syscall.Mkfifo(path, 0600); err != nil {
					t.Fatal(err)
				}
			}
			events, err := os.OpenFile(eventsPath, os.O_RDWR, 0)
			if err != nil {
				t.Fatal(err)
			}
			defer events.Close()
			gate, err := os.OpenFile(gatePath, os.O_RDWR, 0)
			if err != nil {
				t.Fatal(err)
			}
			defer gate.Close()
			reader := bufio.NewReader(events)
			readEvent := func(want string) (int, int) {
				t.Helper()
				var kind string
				var parent, worker int
				if _, err := fmt.Fscanln(reader, &kind, &parent, &worker); err != nil {
					t.Fatal(err)
				}
				if kind != want {
					t.Fatalf("event = %q, want %s", kind, want)
				}
				if kind == "spawn" {
					t.Cleanup(func() { _ = syscall.Kill(-worker, syscall.SIGKILL) })
				}
				return parent, worker
			}
			env := map[string]string{"HARNESS_LIFECYCLE_HELPER": "owner", "HARNESS_LIFECYCLE_EVENTS": eventsPath, "HARNESS_LIFECYCLE_GATE": gatePath}
			catalog := &tools.Registry{}
			logger := slog.New(slog.DiscardHandler)
			var cleanup func()
			var ok bool
			toolName := "mcp__local__ping"
			if serena {
				_, cleanup, ok = setupSerena(t.Context(), config.SerenaConfig{Command: os.Args[0], Args: []string{"-test.run=^TestLifecycleLocalHelper$"}, Env: env}, catalog, logger)
				toolName = "mcp__serena__ping"
			} else {
				_, _, cleanup, ok = setupLocalMCP(t.Context(), config.LocalMCPConfig{Command: os.Args[0], Args: []string{"-test.run=^TestLifecycleLocalHelper$"}, Env: env}, true, catalog, logger)
			}
			// Release both generations before any failure-path cleanup tries to join.
			defer func() { _, _ = gate.Write([]byte{1, 1}); cleanup() }()
			if !ok {
				t.Fatal("MCP setup failed")
			}
			oldPID, oldWorker := readEvent("spawn")
			result := catalog.Dispatch(t.Context(), llm.ToolCall{ID: "drop", Name: toolName, Input: json.RawMessage(`{"disconnect":true}`)})
			if !result.IsError {
				t.Fatal("disconnect unexpectedly succeeded")
			}
			readEvent("close")
			result = catalog.Dispatch(t.Context(), llm.ToolCall{ID: "retry", Name: toolName, Input: json.RawMessage(`{}`)})
			if result.IsError {
				t.Fatalf("reconnect: %s", result.Text)
			}
			_, newWorker := readEvent("spawn")
			closed := make(chan struct{})
			go func() { defer close(closed); cleanup() }()
			readEvent("close")
			if err := syscall.Kill(oldPID, 0); err != nil {
				t.Fatalf("old owner killed before downstream cleanup: %v", err)
			}
			_, _ = gate.Write([]byte{1, 1})
			awaitLifecycle(t, closed)
			for _, pid := range []int{oldWorker, newWorker} {
				if err := syscall.Kill(pid, 0); !errors.Is(err, syscall.ESRCH) {
					t.Fatalf("downstream %d survived cleanup: %v", pid, err)
				}
			}
		})
	}
}
