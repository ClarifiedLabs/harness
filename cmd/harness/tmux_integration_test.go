//go:build integration

package main

// Integration legs for delegate tmux views (design §9.14): the real harness
// binary and real tmux CLI run against a private tmux server socket. Unit tests
// retain exact argv coverage; these tests assert the resulting tmux state and
// cleanup without touching any ambient tmux server.

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// delegateCallTurn scripts an assistant turn that calls the delegate tool
// (OpenAI streaming tool-call shape), like toolCallTurn with a different tool.
func delegateCallTurn(callID, task string) string {
	start := sseChunk(map[string]any{
		"choices": []any{map[string]any{
			"delta": map[string]any{"tool_calls": []any{map[string]any{
				"index": 0, "id": callID,
				"function": map[string]any{"name": "delegate", "arguments": ""},
			}}},
			"finish_reason": nil,
		}},
	})
	taskJSON, _ := json.Marshal(task)
	args := sseChunk(map[string]any{
		"choices": []any{map[string]any{
			"delta": map[string]any{"tool_calls": []any{map[string]any{
				"index":    0,
				"function": map[string]any{"arguments": "{\"task\":" + string(taskJSON) + "}"},
			}}},
			"finish_reason": nil,
		}},
	})
	done := sseChunk(map[string]any{
		"choices": []any{map[string]any{
			"delta": map[string]any{}, "finish_reason": "tool_calls",
		}},
	})
	return strings.Join([]string{start, "", args, "", done, "", "data: [DONE]", ""}, "\n")
}

type isolatedTmux struct {
	binary     string
	socket     string
	session    string
	serverPID  string
	baseWindow string
	parentPane string
	target     string
	paneTarget string
}

func startIsolatedTmux(t *testing.T) *isolatedTmux {
	t.Helper()
	binary, err := exec.LookPath("tmux")
	if err != nil {
		t.Skip("tmux is not available on PATH")
	}
	server := &isolatedTmux{
		binary:  binary,
		socket:  filepath.Join(t.TempDir(), "tmux.sock"),
		session: fmt.Sprintf("harness_test_%d_%d", os.Getpid(), time.Now().UnixNano()),
	}
	server.run(t, "-f", "/dev/null", "new-session", "-d", "-s", server.session, "-x", "120", "-y", "40", "sleep 120")
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = exec.CommandContext(ctx, server.binary, "-S", server.socket, "kill-server").Run()
	})
	server.serverPID = server.run(t, "display-message", "-p", "-t", server.session, "#{pid}")
	server.baseWindow = server.run(t, "display-message", "-p", "-t", server.session+":0", "#{window_id}")
	server.parentPane = server.run(t, "display-message", "-p", "-t", server.session+":0.0", "#{pane_id}")
	server.target = server.session
	server.paneTarget = server.session + ":0"
	return server
}

func (s *isolatedTmux) run(t *testing.T, args ...string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	fullArgs := append([]string{"-S", s.socket}, args...)
	cmd := exec.CommandContext(ctx, s.binary, fullArgs...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("tmux %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func (s *isolatedTmux) configureHarnessEnvironment(t *testing.T) {
	t.Helper()
	t.Setenv("TMUX", fmt.Sprintf("%s,%s,0", s.socket, s.serverPID))
	t.Setenv("TMUX_PANE", s.parentPane)
}

type tmuxWindowState struct {
	id     string
	name   string
	active bool
}

func (s *isolatedTmux) windows(t *testing.T) []tmuxWindowState {
	t.Helper()
	out := s.run(t, "list-windows", "-t", s.target, "-F", "#{window_id}\t#{window_name}\t#{window_active}")
	var states []tmuxWindowState
	for _, line := range strings.Split(out, "\n") {
		fields := strings.SplitN(line, "\t", 3)
		if len(fields) != 3 {
			t.Fatalf("malformed tmux window state %q", line)
		}
		states = append(states, tmuxWindowState{id: fields[0], name: fields[1], active: fields[2] == "1"})
	}
	return states
}

type tmuxPaneState struct {
	id    string
	title string
	left  int
}

func (s *isolatedTmux) panes(t *testing.T) []tmuxPaneState {
	t.Helper()
	out := s.run(t, "list-panes", "-t", s.paneTarget, "-F", "#{pane_id}\t#{pane_title}\t#{pane_left}")
	var states []tmuxPaneState
	for _, line := range strings.Split(out, "\n") {
		fields := strings.SplitN(line, "\t", 3)
		if len(fields) != 3 {
			t.Fatalf("malformed tmux pane state %q", line)
		}
		left, err := strconv.Atoi(fields[2])
		if err != nil {
			t.Fatalf("pane left coordinate %q: %v", fields[2], err)
		}
		states = append(states, tmuxPaneState{id: fields[0], title: fields[1], left: left})
	}
	return states
}

func (s *isolatedTmux) option(t *testing.T, scope, target, name string) string {
	t.Helper()
	return s.run(t, "show-options", scope, "-v", "-t", target, name)
}

type requestGate struct {
	started chan struct{}
	release chan struct{}
	once    sync.Once
}

func newRequestGate() *requestGate {
	return &requestGate{started: make(chan struct{}), release: make(chan struct{})}
}

func (g *requestGate) beforeResponse(ctx context.Context, index int) {
	if index != 1 {
		return
	}
	close(g.started)
	select {
	case <-g.release:
	case <-ctx.Done():
	}
}

func (g *requestGate) open() {
	g.once.Do(func() { close(g.release) })
}

func waitForChildRequest(t *testing.T, gate *requestGate, cmd *exec.Cmd, errBuf *safeBuffer) {
	t.Helper()
	select {
	case <-gate.started:
	case <-time.After(10 * time.Second):
		gate.open()
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
		}
		t.Fatalf("delegate child request did not start; stderr=%s", errBuf.String())
	}
}

func cleanupHarnessProcess(t *testing.T, gate *requestGate, cmd *exec.Cmd) {
	t.Helper()
	t.Cleanup(func() {
		gate.open()
		if cmd.Process != nil && cmd.ProcessState == nil {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
		}
	})
}

func childSessionDir(t *testing.T, home string) string {
	t.Helper()
	children, err := filepath.Glob(filepath.Join(home, "state", "harness", "sessions", "*", "children", "delegate_*"))
	if err != nil || len(children) != 1 {
		t.Fatalf("child session dirs = %v, err=%v, want exactly 1", children, err)
	}
	return children[0]
}

func finishTmuxHarness(t *testing.T, gate *requestGate, cmd *exec.Cmd, stdout io.ReadCloser, errBuf *safeBuffer, mock *recordingMock) {
	t.Helper()
	gate.open()
	outBytes, err := io.ReadAll(stdout)
	if err != nil {
		t.Fatalf("read harness stdout: %v", err)
	}
	if err := cmd.Wait(); err != nil {
		t.Fatalf("harness exited with error: %v; stderr=%s", err, errBuf.String())
	}
	if !strings.Contains(string(outBytes), "parent done") {
		t.Errorf("final assistant text should be on stdout, got %q (stderr=%s)", outBytes, errBuf.String())
	}
	if strings.Contains(errBuf.String(), "delegate_tmux") {
		t.Errorf("no degradation warning expected inside tmux, stderr=%s", errBuf.String())
	}
	if reqs := mock.recorded(); len(reqs) != 3 {
		t.Fatalf("delegate flow should produce 3 requests, got %d", len(reqs))
	}
}

func TestSmokeDelegateTmuxWindow(t *testing.T) {
	server := startIsolatedTmux(t)
	server.configureHarnessEnvironment(t)
	bin := buildBinary(t)
	gate := newRequestGate()
	t.Cleanup(gate.open)
	mock := &recordingMock{
		scripts: []string{
			delegateCallTurn("call_delegate", "inspect only"),
			textTurn("child report"),
			textTurn("parent done"),
		},
		beforeResponse: gate.beforeResponse,
	}
	srv := httptest.NewServer(mock)
	t.Cleanup(func() {
		gate.open()
		srv.Close()
	})

	cmd, stdout, errBuf, home := startHarness(t, bin, srv.URL+"/v1", "-delegate-tmux", "-delegate-tmux-layout", "window", "-p", "delegate the inspection")
	cleanupHarnessProcess(t, gate, cmd)
	waitForChildRequest(t, gate, cmd, errBuf)
	childDir := childSessionDir(t, home)

	windows := server.windows(t)
	if len(windows) != 2 {
		t.Fatalf("live tmux windows = %+v, want base plus delegate", windows)
	}
	var delegateWindow tmuxWindowState
	for _, state := range windows {
		if state.id != server.baseWindow {
			delegateWindow = state
		}
	}
	if delegateWindow.id == "" {
		t.Fatalf("delegate window missing from %+v", windows)
	}
	if want := filepath.Base(childDir); delegateWindow.name != want {
		t.Fatalf("delegate window name = %q, want %q", delegateWindow.name, want)
	}
	if delegateWindow.active {
		t.Fatalf("delegate window %s is active; new-window -d should leave the base active", delegateWindow.id)
	}
	if got := server.option(t, "-w", delegateWindow.id, "remain-on-exit"); got != "on" {
		t.Fatalf("delegate remain-on-exit = %q, want on", got)
	}
	if got := server.option(t, "-w", delegateWindow.id, "automatic-rename"); got != "off" {
		t.Fatalf("delegate automatic-rename = %q, want off", got)
	}

	finishTmuxHarness(t, gate, cmd, stdout, errBuf, mock)
	after := server.windows(t)
	if len(after) != 1 || after[0].id != server.baseWindow {
		t.Fatalf("windows after child completion = %+v, want only base %s", after, server.baseWindow)
	}
}

func TestSmokeDelegateTmuxPane(t *testing.T) {
	server := startIsolatedTmux(t)
	server.configureHarnessEnvironment(t)
	bin := buildBinary(t)
	gate := newRequestGate()
	t.Cleanup(gate.open)
	mock := &recordingMock{
		scripts: []string{
			delegateCallTurn("call_delegate", "inspect only"),
			textTurn("child report"),
			textTurn("parent done"),
		},
		beforeResponse: gate.beforeResponse,
	}
	srv := httptest.NewServer(mock)
	t.Cleanup(func() {
		gate.open()
		srv.Close()
	})

	// No -delegate-tmux: the auto default inside tmux (TMUX/TMUX_PANE set by
	// configureHarnessEnvironment) is what enables the pane view here.
	cmd, stdout, errBuf, home := startHarness(t, bin, srv.URL+"/v1", "-p", "delegate the inspection")
	cleanupHarnessProcess(t, gate, cmd)
	waitForChildRequest(t, gate, cmd, errBuf)
	childDir := childSessionDir(t, home)

	panes := server.panes(t)
	if len(panes) != 2 {
		t.Fatalf("live tmux panes = %+v, want base plus delegate", panes)
	}
	var basePane, delegatePane tmuxPaneState
	for _, state := range panes {
		if state.id == server.parentPane {
			basePane = state
		} else {
			delegatePane = state
		}
	}
	if basePane.id == "" || delegatePane.id == "" {
		t.Fatalf("base/delegate pane missing from %+v", panes)
	}
	if delegatePane.left <= basePane.left {
		t.Fatalf("delegate pane left=%d, base left=%d; want right-hand split", delegatePane.left, basePane.left)
	}
	if want := filepath.Base(childDir); delegatePane.title != want {
		t.Fatalf("delegate pane title = %q, want %q", delegatePane.title, want)
	}
	if got := server.option(t, "-p", delegatePane.id, "remain-on-exit"); got != "on" {
		t.Fatalf("delegate pane remain-on-exit = %q, want on", got)
	}

	finishTmuxHarness(t, gate, cmd, stdout, errBuf, mock)
	after := server.panes(t)
	if len(after) != 1 || after[0].id != server.parentPane {
		t.Fatalf("panes after child completion = %+v, want only base %s", after, server.parentPane)
	}
}
