//go:build integration

package main

// Integration leg for delegate tmux windows (design §9.14): the real binary
// runs a scripted foreground delegate under a fake tmux on PATH. The fake
// records every invocation so the test asserts the exact argv marshalling —
// one detached window following the child session dir, closed on success.

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
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

func TestSmokeDelegateTmuxWindow(t *testing.T) {
	bin := buildBinary(t)

	// Fake tmux records argv one argument per line under an argc header, so a
	// space in a path can never be confused with an argument separator.
	logPath := filepath.Join(t.TempDir(), "tmux.log")
	binDir := t.TempDir()
	fake := `#!/bin/sh
{
  printf 'argc=%d\n' "$#"
  for a in "$@"; do printf '%s\n' "$a"; done
} >> "$FAKE_TMUX_LOG"
if [ "$1" = "new-window" ]; then printf '@1\n'; fi
exit 0
`
	if err := os.WriteFile(filepath.Join(binDir, "tmux"), []byte(fake), 0o755); err != nil {
		t.Fatalf("write fake tmux: %v", err)
	}
	t.Setenv("FAKE_TMUX_LOG", logPath)
	t.Setenv("TMUX", "/tmp/fake-tmux-socket,0,0")
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	mock := &recordingMock{scripts: []string{
		delegateCallTurn("call_delegate", "inspect only"),
		textTurn("child report"),
		textTurn("parent done"),
	}}
	srv := httptest.NewServer(mock)
	defer srv.Close()

	cmd, stdout, errBuf, home := startHarness(t, bin, srv.URL+"/v1", "-delegate-tmux", "-delegate-tmux-layout", "window", "-p", "delegate the inspection")
	outBytes, _ := io.ReadAll(stdout)
	if err := cmd.Wait(); err != nil {
		t.Fatalf("harness exited with error: %v; stderr=%s", err, errBuf.String())
	}
	if !strings.Contains(string(outBytes), "parent done") {
		t.Errorf("final assistant text should be on stdout, got %q (stderr=%s)", outBytes, errBuf.String())
	}
	if strings.Contains(errBuf.String(), "delegate_tmux") {
		t.Errorf("no degradation warning expected inside tmux, stderr=%s", errBuf.String())
	}

	// The scripted delegate completed: parent tool call, child prompt, parent final.
	if reqs := mock.recorded(); len(reqs) != 3 {
		t.Fatalf("delegate flow should produce 3 requests, got %d", len(reqs))
	}

	// The child session directory exists on disk.
	children, err := filepath.Glob(filepath.Join(home, "state", "harness", "sessions", "*", "children", "delegate_*"))
	if err != nil || len(children) != 1 {
		t.Fatalf("child session dirs = %v, err=%v, want exactly 1", children, err)
	}
	resolvedBin, err := filepath.EvalSymlinks(bin)
	if err != nil {
		resolvedBin = bin
	}

	invocations := parseRecordedInvocations(t, logPath)
	var newWindow, killWindow [][]string
	for _, argv := range invocations {
		switch argv[0] {
		case "new-window":
			newWindow = append(newWindow, argv)
		case "kill-window":
			killWindow = append(killWindow, argv)
		}
	}
	if len(newWindow) != 1 {
		t.Fatalf("new-window invocations = %d, want 1: %v", len(newWindow), invocations)
	}
	wantWindow := []string{
		"new-window", "-d", "-a", "-P", "-F", "#{window_id}", "--",
		resolvedBin, "session", "replay", "--follow", "--", children[0],
	}
	if !equalStringSlices(newWindow[0], wantWindow) {
		t.Fatalf("new-window argv:\ngot:  %v\nwant: %v", newWindow[0], wantWindow)
	}
	if len(killWindow) != 1 || !equalStringSlices(killWindow[0], []string{"kill-window", "-t", "@1"}) {
		t.Fatalf("a successful child closes its window exactly once, got %v", killWindow)
	}
}

func TestSmokeDelegateTmuxPane(t *testing.T) {
	bin := buildBinary(t)

	logPath := filepath.Join(t.TempDir(), "tmux.log")
	binDir := t.TempDir()
	fake := `#!/bin/sh
{
  printf 'argc=%d\n' "$#"
  for a in "$@"; do printf '%s\n' "$a"; done
} >> "$FAKE_TMUX_LOG"
if [ "$1" = "split-window" ]; then printf '%%1\n'; fi
exit 0
`
	if err := os.WriteFile(filepath.Join(binDir, "tmux"), []byte(fake), 0o755); err != nil {
		t.Fatalf("write fake tmux: %v", err)
	}
	t.Setenv("FAKE_TMUX_LOG", logPath)
	t.Setenv("TMUX", "/tmp/fake-tmux-socket,0,0")
	t.Setenv("TMUX_PANE", "%0")
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	mock := &recordingMock{scripts: []string{
		delegateCallTurn("call_delegate", "inspect only"),
		textTurn("child report"),
		textTurn("parent done"),
	}}
	srv := httptest.NewServer(mock)
	defer srv.Close()

	cmd, stdout, errBuf, home := startHarness(t, bin, srv.URL+"/v1", "-delegate-tmux", "-p", "delegate the inspection")
	outBytes, _ := io.ReadAll(stdout)
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

	children, err := filepath.Glob(filepath.Join(home, "state", "harness", "sessions", "*", "children", "delegate_*"))
	if err != nil || len(children) != 1 {
		t.Fatalf("child session dirs = %v, err=%v, want exactly 1", children, err)
	}
	resolvedBin, err := filepath.EvalSymlinks(bin)
	if err != nil {
		resolvedBin = bin
	}

	invocations := parseRecordedInvocations(t, logPath)
	var split, killPane [][]string
	for _, argv := range invocations {
		switch argv[0] {
		case "split-window":
			split = append(split, argv)
		case "kill-pane":
			killPane = append(killPane, argv)
		}
	}
	if len(split) != 1 {
		t.Fatalf("split-window invocations = %d, want 1: %v", len(split), invocations)
	}
	wantSplit := []string{
		"split-window", "-d", "-h", "-t", "%0", "-P", "-F", "#{pane_id}", "--",
		resolvedBin, "session", "replay", "--follow", "--", children[0],
	}
	if !equalStringSlices(split[0], wantSplit) {
		t.Fatalf("split-window argv:\ngot:  %v\nwant: %v", split[0], wantSplit)
	}
	if len(killPane) != 1 || !equalStringSlices(killPane[0], []string{"kill-pane", "-t", "%1"}) {
		t.Fatalf("a successful child closes its pane exactly once, got %v", killPane)
	}

	hasRemainOnExit := false
	hasPaneTitle := false
	for _, argv := range invocations {
		if len(argv) >= 6 && argv[0] == "set-option" && argv[1] == "-p" && argv[5] == "remain-on-exit" {
			hasRemainOnExit = true
		}
		if len(argv) >= 4 && argv[0] == "select-pane" && argv[1] == "-T" {
			hasPaneTitle = true
		}
	}
	if !hasRemainOnExit {
		t.Fatalf("missing per-pane remain-on-exit in %v", invocations)
	}
	if !hasPaneTitle {
		t.Fatalf("missing pane title in %v", invocations)
	}
}

// parseRecordedInvocations reads the fake tmux log into one argv slice per
// invocation, keyed by the argc= header lines.
func parseRecordedInvocations(t *testing.T, logPath string) [][]string {
	t.Helper()
	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("fake tmux never ran: %v", err)
	}
	var invocations [][]string
	var current []string
	remaining := 0
	for _, line := range strings.Split(strings.TrimRight(string(data), "\n"), "\n") {
		if strings.HasPrefix(line, "argc=") {
			if current != nil && remaining != 0 {
				t.Fatalf("truncated invocation in recorder log: %v", current)
			}
			current = nil
			if _, err := fmt.Sscanf(line, "argc=%d", &remaining); err != nil {
				t.Fatalf("bad recorder line %q: %v", line, err)
			}
			if remaining == 0 {
				invocations = append(invocations, []string{})
				current = nil
			}
			continue
		}
		current = append(current, line)
		remaining--
		if remaining == 0 {
			invocations = append(invocations, current)
			current = nil
		}
	}
	return invocations
}

func equalStringSlices(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
