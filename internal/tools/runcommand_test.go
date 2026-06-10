package tools

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func runRunCommand(t *testing.T, args map[string]any) (string, error) {
	return runTool(t, runCommand{}, args)
}

func TestRunCommandEchoExitZero(t *testing.T) {
	out, err := runRunCommand(t, map[string]any{"command": "echo hello"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(out, "hello") {
		t.Errorf("missing echoed output: %q", out)
	}
	if !strings.Contains(out, "[exit code: 0]") {
		t.Errorf("missing exit code marker: %q", out)
	}
}

func TestRunCommandNonZeroExitNotError(t *testing.T) {
	out, err := runRunCommand(t, map[string]any{"command": "exit 1"})
	if err != nil {
		t.Fatalf("non-zero exit must not be a tool error: %v", err)
	}
	if !strings.Contains(out, "[exit code: 1]") {
		t.Errorf("missing exit code 1 marker: %q", out)
	}
}

func TestRunCommandCombinedStdoutStderr(t *testing.T) {
	// Interleaved writes to both streams must appear in one buffer.
	out, err := runRunCommand(t, map[string]any{"command": "echo out; echo err 1>&2"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(out, "out") || !strings.Contains(out, "err") {
		t.Errorf("combined output must contain both streams: %q", out)
	}
}

func TestRunCommandCwdHonored(t *testing.T) {
	dir := t.TempDir()
	mustWrite(t, filepath.Join(dir, "marker.txt"), "x\n")
	out, err := runRunCommand(t, map[string]any{"command": "ls", "cwd": dir})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(out, "marker.txt") {
		t.Errorf("command did not run in cwd %q: %q", dir, out)
	}
}

func TestRunCommandMissingCwd(t *testing.T) {
	_, err := runRunCommand(t, map[string]any{"command": "echo hi", "cwd": filepath.Join(t.TempDir(), "does-not-exist")})
	if err == nil {
		t.Fatal("expected error for missing cwd")
	}
}

func TestRunCommandStdinWired(t *testing.T) {
	out, err := runRunCommand(t, map[string]any{"command": "cat", "stdin": "hello stdin\n"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(out, "hello stdin") {
		t.Errorf("stdin not wired to command: %q", out)
	}
	if !strings.Contains(out, "[exit code: 0]") {
		t.Errorf("missing exit code marker: %q", out)
	}
}

func TestRunCommandArgvRunsWithoutShell(t *testing.T) {
	out, err := runRunCommand(t, map[string]any{"argv": []string{"printf", "%s", "hello argv"}})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(out, "hello argv") {
		t.Errorf("missing argv output: %q", out)
	}
	if !strings.Contains(out, "[exit code: 0]") {
		t.Errorf("missing exit code marker: %q", out)
	}
}

func TestRunCommandRejectsCommandAndArgvTogether(t *testing.T) {
	_, err := runRunCommand(t, map[string]any{"command": "echo shell", "argv": []string{"echo", "argv"}})
	if err == nil {
		t.Fatal("expected error for command and argv together")
	}
	if !strings.Contains(err.Error(), "command or argv") {
		t.Fatalf("error = %v", err)
	}
}

func TestRunCommandMissingCommand(t *testing.T) {
	_, err := runRunCommand(t, map[string]any{})
	if err == nil {
		t.Fatal("expected error for missing command or argv")
	}
}

func TestRunCommandModelSchemaAvoidsTopLevelComposition(t *testing.T) {
	tests := []struct {
		name string
		tool runCommand
	}{
		{name: "foreground", tool: runCommand{}},
		{name: "background", tool: runCommand{background: &fakeBackgroundStarter{}}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var schema map[string]any
			modelRaw := modelSchema(tc.tool.Schema())
			if err := json.Unmarshal(modelRaw, &schema); err != nil {
				t.Fatalf("schema JSON: %v", err)
			}
			for _, key := range []string{"oneOf", "anyOf", "allOf"} {
				if _, ok := schema[key]; ok {
					t.Fatalf("schema has top-level %s rejected by Anthropic: %s", key, modelRaw)
				}
			}
			props, ok := schema["properties"].(map[string]any)
			if !ok {
				t.Fatalf("schema properties missing: %s", modelRaw)
			}
			if _, ok := props["command"]; !ok {
				t.Fatalf("schema missing command property: %s", modelRaw)
			}
			if _, ok := props["argv"]; !ok {
				t.Fatalf("schema missing argv property: %s", modelRaw)
			}

			var rawSchema map[string]any
			if err := json.Unmarshal(tc.tool.Schema(), &rawSchema); err != nil {
				t.Fatalf("raw schema JSON: %v", err)
			}
			rawProps, ok := rawSchema["properties"].(map[string]any)
			if !ok {
				t.Fatalf("raw schema properties missing: %s", tc.tool.Schema())
			}
			argv, ok := rawProps["argv"].(map[string]any)
			if !ok {
				t.Fatalf("raw schema argv property has unexpected shape: %s", tc.tool.Schema())
			}
			argvDesc, _ := argv["description"].(string)
			if !strings.Contains(argvDesc, "not a shell string or JSON-encoded array") {
				t.Fatalf("argv description should reject stringified argv arrays: %q", argvDesc)
			}
			if !strings.Contains(tc.tool.Description(), "exactly one of command or argv") {
				t.Fatalf("description should carry command/argv exclusivity rule: %q", tc.tool.Description())
			}
			if !strings.Contains(tc.tool.Description(), "not a shell string or JSON-encoded array") {
				t.Fatalf("description should reject stringified argv arrays: %q", tc.tool.Description())
			}
		})
	}
}

type fakeBackgroundStarter struct {
	req BackgroundJobRequest
}

func (f *fakeBackgroundStarter) StartBackgroundJob(req BackgroundJobRequest) (BackgroundJobInfo, error) {
	f.req = req
	return BackgroundJobInfo{ID: "bg_test", Status: "running"}, nil
}

func TestRunCommandBackgroundStartsJob(t *testing.T) {
	starter := &fakeBackgroundStarter{}
	out, err := runTool(t, runCommand{background: starter}, map[string]any{
		"command":    "echo background",
		"background": true,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if out != "background job bg_test started" {
		t.Fatalf("start output = %q", out)
	}
	if starter.req.Kind != "run_command" {
		t.Fatalf("job kind = %q, want run_command", starter.req.Kind)
	}
	if starter.req.Description != "echo background" {
		t.Fatalf("job description = %q", starter.req.Description)
	}
	if starter.req.Run == nil {
		t.Fatal("background job runner missing")
	}

	result, err := starter.req.Run(context.Background(), "bg_test")
	if err != nil {
		t.Fatalf("background run: %v", err)
	}
	if !strings.Contains(result.Text, "background") || !strings.Contains(result.Text, "[exit code: 0]") {
		t.Fatalf("background result = %q", result.Text)
	}
}

func TestRunCommandBackgroundArgvStartsJob(t *testing.T) {
	starter := &fakeBackgroundStarter{}
	out, err := runTool(t, runCommand{background: starter}, map[string]any{
		"argv":       []string{"printf", "%s", "background argv"},
		"background": true,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if out != "background job bg_test started" {
		t.Fatalf("start output = %q", out)
	}
	if starter.req.Description != "printf %s background argv" {
		t.Fatalf("job description = %q", starter.req.Description)
	}
	result, err := starter.req.Run(context.Background(), "bg_test")
	if err != nil {
		t.Fatalf("background run: %v", err)
	}
	if !strings.Contains(result.Text, "background argv") || !strings.Contains(result.Text, "[exit code: 0]") {
		t.Fatalf("background result = %q", result.Text)
	}
}

func TestRunCommandBackgroundRequiresStarter(t *testing.T) {
	_, err := runRunCommand(t, map[string]any{
		"command":    "echo background",
		"background": true,
	})
	if err == nil {
		t.Fatal("expected error when background manager is unavailable")
	}
	if !strings.Contains(err.Error(), "background manager") {
		t.Fatalf("error = %v", err)
	}
}

// The timeout test exercises a real subprocess kill (sanctioned exception).
// A sleeping child in its own process group must be killed when the timeout
// fires, and the partial output captured before the kill must be reported.
func TestRunCommandTimeoutKillsGroup(t *testing.T) {
	oldUnit := processTimeoutUnit
	processTimeoutUnit = 250 * time.Millisecond
	t.Cleanup(func() { processTimeoutUnit = oldUnit })

	start := time.Now()
	out, err := runRunCommand(t, map[string]any{
		"command":         "echo started; sleep 30",
		"timeout_seconds": 1,
	})
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("timeout must report a result, not a tool error: %v", err)
	}
	// Must not have waited anywhere near the 30s sleep.
	if elapsed > 10*time.Second {
		t.Errorf("command was not killed promptly: took %v", elapsed)
	}
	if !strings.Contains(out, "started") {
		t.Errorf("partial output before kill not reported: %q", out)
	}
	if !strings.Contains(out, "timed out") {
		t.Errorf("timeout should be noted in output: %q", out)
	}
}

func TestRunProcessTimeoutReturnsPartialOutputWhenWaitDoesNotFinish(t *testing.T) {
	oldUnit := processTimeoutUnit
	oldGrace := processReapGrace
	processTimeoutUnit = 25 * time.Millisecond
	processReapGrace = 25 * time.Millisecond
	t.Cleanup(func() {
		processTimeoutUnit = oldUnit
		processReapGrace = oldGrace
	})

	pidFile := filepath.Join(t.TempDir(), "pid")
	oldKill := killProcessGroup
	killProcessGroup = func(int) {}
	var cmd *exec.Cmd
	t.Cleanup(func() {
		killProcessGroup = oldKill
		data, err := os.ReadFile(pidFile)
		if err == nil {
			pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
			if err == nil && pid > 0 {
				_ = syscall.Kill(-pid, syscall.SIGKILL)
				return
			}
		}
		if cmd != nil && cmd.Process != nil {
			_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		}
	})

	cmd = exec.Command("sh", "-c", `echo $$ > "$PIDFILE"; echo started; sleep 30`)
	cmd.Env = append(os.Environ(), "PIDFILE="+pidFile)

	start := time.Now()
	out, err := runProcess(context.Background(), cmd, 1)
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("timeout must report a result, not a tool error: %v", err)
	}
	if elapsed > 5*time.Second {
		t.Fatalf("timeout waited for process exit instead of returning captured output: took %v", elapsed)
	}
	if !strings.Contains(out, "started") {
		t.Fatalf("partial output before timeout not reported: %q", out)
	}
	if !strings.Contains(out, "timed out after 1s") {
		t.Fatalf("timeout should be noted in output: %q", out)
	}
	if !strings.Contains(out, "wait did not finish") {
		t.Fatalf("unfinished wait should be noted in output: %q", out)
	}
}

func TestRunCommandDoesNotWaitForBackgroundChildHoldingStdout(t *testing.T) {
	start := time.Now()
	out, err := runRunCommand(t, map[string]any{
		"command":         "echo started; sleep 30 &",
		"timeout_seconds": 5,
	})
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("background child should not be a tool error: %v", err)
	}
	if elapsed > 3*time.Second {
		t.Fatalf("command waited for background child: took %v", elapsed)
	}
	if !strings.Contains(out, "started") {
		t.Fatalf("partial output not reported: %q", out)
	}
	if strings.Contains(out, "timed out") {
		t.Fatalf("direct process exited; background child must not force a timeout: %q", out)
	}
	if !strings.Contains(out, "[exit code: 0]") {
		t.Fatalf("direct process exit code not reported: %q", out)
	}
}

func TestRunCommandDoesNotStopOnTTYJobControl(t *testing.T) {
	if !hasForegroundTTY() {
		t.Skip("no foreground controlling terminal")
	}

	oldUnit := processTimeoutUnit
	oldGrace := processReapGrace
	processTimeoutUnit = 250 * time.Millisecond
	processReapGrace = 50 * time.Millisecond
	t.Cleanup(func() {
		processTimeoutUnit = oldUnit
		processReapGrace = oldGrace
	})

	out, err := runRunCommand(t, map[string]any{
		"command":         `go test harness/internal/term -run TestResetOnRealTTY -count=1 -v && echo tty-termios-ok`,
		"timeout_seconds": 40,
	})
	if err != nil {
		t.Fatalf("terminal command should not be a tool error: %v", err)
	}
	if !strings.Contains(out, "tty-termios-ok") {
		t.Fatalf("terminal command did not complete: %q", out)
	}
	if strings.Contains(out, "timed out") {
		t.Fatalf("terminal command was stopped as a background process group: %q", out)
	}
}

func TestProcessTimeoutHasNoMaximumCap(t *testing.T) {
	if got := resolveProcessTimeoutSeconds(601); got != 601 {
		t.Fatalf("resolveProcessTimeoutSeconds(601) = %d, want 601", got)
	}
	if got := resolveProcessTimeoutSeconds(3600); got != 3600 {
		t.Fatalf("resolveProcessTimeoutSeconds(3600) = %d, want 3600", got)
	}
}
