package tools

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"harness/internal/llm"
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

// r41: run_command uses a non-login shell (-c) instead of -lc, so the
// login-profile chain is not sourced on every call.
func TestShellCommandUsesNonLoginShell(t *testing.T) {
	cmd := shellCommand("echo hi")
	if len(cmd.Args) < 2 {
		t.Fatalf("unexpected shell args: %v", cmd.Args)
	}
	if cmd.Args[1] != "-c" {
		t.Errorf("shell flag = %q, want -c (non-login)", cmd.Args[1])
	}
}

func TestParseLoginPATHOutput(t *testing.T) {
	if got := parseLoginPATHOutput("profile banner\n" + loginPATHSentinel + "/usr/bin:/bin\n"); got != "/usr/bin:/bin" {
		t.Errorf("parseLoginPATHOutput = %q, want /usr/bin:/bin", got)
	}
	if got := parseLoginPATHOutput("no sentinel present"); got != "" {
		t.Errorf("missing sentinel should yield empty, got %q", got)
	}
}

func TestMergePATH(t *testing.T) {
	sep := string(filepath.ListSeparator)
	if got := mergePATH("/a"+sep+"/b", "/b"+sep+"/c"); got != "/a"+sep+"/b"+sep+"/c" {
		t.Errorf("mergePATH dedup/append = %q", got)
	}
	if got := mergePATH("", "/x"); got != "/x" {
		t.Errorf("empty current: %q", got)
	}
	if got := mergePATH("/x", ""); got != "/x" {
		t.Errorf("empty login: %q", got)
	}
}

func TestSetEnvPATH(t *testing.T) {
	env := []string{"A=1", "PATH=/old", "B=2"}
	got := setEnvPATH(env, "/new")
	if env[1] != "PATH=/old" {
		t.Errorf("input slice was mutated: %v", env)
	}
	if !slices.Contains(got, "PATH=/new") {
		t.Errorf("PATH not replaced: %v", got)
	}
	appended := setEnvPATH([]string{"A=1"}, "/p")
	if appended[len(appended)-1] != "PATH=/p" {
		t.Errorf("PATH not appended when absent: %v", appended)
	}
}

// Integration: the once-resolved login PATH makes a tool reachable to a shell
// command even though it is only on the login PATH.
func TestRunCommandUsesResolvedLoginPATH(t *testing.T) {
	dir := t.TempDir()
	makeExecutable(t, filepath.Join(dir, "harnesstool42"), "#!/bin/sh\necho found-it\n")

	orig := loginPATHResolver
	loginPATHResolver = func() string { return dir }
	loginPATHOnce = sync.Once{}
	loginPATHCached = ""
	t.Cleanup(func() {
		loginPATHResolver = orig
		loginPATHOnce = sync.Once{}
		loginPATHCached = ""
	})

	out, err := runRunCommand(t, map[string]any{"command": "harnesstool42"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(out, "found-it") {
		t.Errorf("command on the resolved login PATH was not found: %q", out)
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
			if _, ok := props["steps"]; !ok {
				t.Fatalf("schema missing steps property: %s", modelRaw)
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
			if !strings.Contains(tc.tool.Description(), "ordered steps") {
				t.Fatalf("description should advertise steps: %q", tc.tool.Description())
			}
			if !strings.Contains(tc.tool.Description(), "argv as an array of strings") {
				t.Fatalf("description should advertise argv shape: %q", tc.tool.Description())
			}
		})
	}
}

func TestRunCommandStepsReturnCompactReceiptsAndOriginal(t *testing.T) {
	tool := runCommand{}
	input := json.RawMessage(`{
		"steps": [
			{"name":"first check","command":"printf 'verbose first output'"},
			{"name":"second check","argv":["printf","verbose second output"]}
		]
	}`)
	result, err := tool.RunResult(context.Background(), input)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"PASS first check", "PASS second check"} {
		if !strings.Contains(result.Text, want) {
			t.Errorf("receipt missing %q:\n%s", want, result.Text)
		}
	}
	if strings.Contains(result.Text, "verbose first output") || strings.Contains(result.Text, "verbose second output") {
		t.Fatalf("successful output leaked into compact receipt:\n%s", result.Text)
	}
	for _, want := range []string{"verbose first output", "verbose second output", "[exit code: 0]"} {
		if !strings.Contains(result.OriginalText, want) {
			t.Errorf("full transcript missing %q:\n%s", want, result.OriginalText)
		}
	}
}

func TestRunCommandStepsStopOnFailure(t *testing.T) {
	dir := t.TempDir()
	marker := filepath.Join(dir, "should-not-exist")
	tool := runCommand{}
	input, err := json.Marshal(map[string]any{
		"steps": []map[string]any{
			{"name": "fails", "command": "printf 'bad output'; exit 7"},
			{"name": "skipped", "argv": []string{"touch", marker}},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	result, err := tool.RunResult(context.Background(), input)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"FAIL fails", "exit 7", "bad output", "SKIP 1 remaining step"} {
		if !strings.Contains(result.Text, want) {
			t.Errorf("receipt missing %q:\n%s", want, result.Text)
		}
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("later step ran despite stop_on_failure: %v", err)
	}
}

func TestRunCommandStepsCanContinueAfterFailure(t *testing.T) {
	dir := t.TempDir()
	marker := filepath.Join(dir, "continued")
	tool := runCommand{}
	input, err := json.Marshal(map[string]any{
		"stop_on_failure": false,
		"steps": []map[string]any{
			{"name": "fails", "command": "exit 3"},
			{"name": "continues", "argv": []string{"touch", marker}},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	result, err := tool.RunResult(context.Background(), input)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(result.Text, "FAIL fails") || !strings.Contains(result.Text, "PASS continues") {
		t.Fatalf("continue receipt:\n%s", result.Text)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("later step did not run: %v", err)
	}
}

func TestRunCommandStepsInheritCwdAndTimeout(t *testing.T) {
	dir := t.TempDir()
	tool := runCommand{}
	input, err := json.Marshal(map[string]any{
		"cwd":             dir,
		"timeout_seconds": 7,
		"steps": []map[string]any{
			{"name": "cwd", "command": "pwd"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	result, err := tool.RunResult(context.Background(), input)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(result.OriginalText, dir) {
		t.Fatalf("step did not inherit cwd:\n%s", result.OriginalText)
	}
	if d, ok := tool.SelfTimeout(input); !ok || d != 7*time.Second {
		t.Fatalf("step SelfTimeout = %s, %t; want 7s", d, ok)
	}
}

func TestRunCommandStepsValidation(t *testing.T) {
	tests := []struct {
		name  string
		input map[string]any
		want  string
	}{
		{"top level command", map[string]any{"command": "true", "steps": []map[string]any{{"command": "true"}}}, "steps or a top-level"},
		{"top level stdin", map[string]any{"stdin": "x", "steps": []map[string]any{{"command": "true"}}}, "top-level stdin"},
		{"background", map[string]any{"background": true, "steps": []map[string]any{{"command": "true"}}}, "background"},
		{"missing step command", map[string]any{"steps": []map[string]any{{"name": "empty"}}}, "steps[0]"},
		{"step command and argv", map[string]any{"steps": []map[string]any{{"command": "true", "argv": []string{"true"}}}}, "steps[0]"},
		{"bad step timeout", map[string]any{"steps": []map[string]any{{"command": "true", "timeout_seconds": -1}}}, "timeout_seconds"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := runRunCommand(t, tc.input)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want %q", err, tc.want)
			}
		})
	}
}

func TestRunCommandStepsDispatchArchivesSuppressedOutput(t *testing.T) {
	r := &Registry{}
	r.Register(runCommand{})
	res := r.Dispatch(context.Background(), llm.ToolCall{
		ID:    "steps",
		Name:  "run_command",
		Input: json.RawMessage(`{"steps":[{"name":"test","command":"printf verbose-success"}]}`),
	})
	if res.IsError || !res.Truncated {
		t.Fatalf("dispatch result = %+v", res)
	}
	if strings.Contains(res.Text, "verbose-success") || !strings.Contains(res.Text, "PASS test") {
		t.Fatalf("model receipt = %q", res.Text)
	}
	if !strings.Contains(res.OriginalText, "verbose-success") {
		t.Fatalf("archival original = %q", res.OriginalText)
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
