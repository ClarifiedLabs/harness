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

func runRunCommandResult(t *testing.T, args map[string]any) (RunResult, error) {
	t.Helper()
	input, err := json.Marshal(args)
	if err != nil {
		t.Fatal(err)
	}
	return (runCommand{}).RunResult(context.Background(), input)
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
			for _, property := range []string{"command", "argv", "steps", "name", "output_mode"} {
				if _, ok := props[property]; !ok {
					t.Fatalf("schema missing %s property: %s", property, modelRaw)
				}
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
			if strings.Contains(tc.tool.Description(), "background_lease") &&
				!strings.Contains(tc.tool.Description(), "does not restrict command behavior") {
				t.Fatalf("description should distinguish lease scheduling from command safety: %q", tc.tool.Description())
			}
			if _, ok := rawProps["resource_key"]; ok {
				t.Fatalf("schema should not advertise legacy top-level resource_key: %s", tc.tool.Schema())
			}
			if _, ok := rawProps["access"]; ok {
				t.Fatalf("schema should not advertise ambiguous top-level access: %s", tc.tool.Schema())
			}
			if tc.tool.background != nil {
				lease, ok := rawProps["background_lease"].(map[string]any)
				if !ok {
					t.Fatalf("background schema missing typed background_lease: %s", tc.tool.Schema())
				}
				description, _ := lease["description"].(string)
				if !strings.Contains(description, "does not restrict command behavior") {
					t.Fatalf("background_lease description is ambiguous: %q", description)
				}
			}
		})
	}
}

func TestRunCommandTopLevelOutputModes(t *testing.T) {
	largeOutput := strings.Repeat("x", runCommandAutoReceiptBytes+100)
	largeArgs := map[string]any{
		"argv":  []string{"sh", "-c", "cat; printf '\\nSUMMARY ok\\n'"},
		"stdin": largeOutput,
		"name":  "go test",
	}

	t.Run("auto compacts large success", func(t *testing.T) {
		result, err := runRunCommandResult(t, largeArgs)
		if err != nil {
			t.Fatal(err)
		}
		for _, want := range []string{"PASS go test", "SUMMARY ok", "output)", "[exit code: 0]"} {
			if !strings.Contains(result.Text, want) {
				t.Fatalf("receipt missing %q:\n%s", want, result.Text)
			}
		}
		if len(result.Text) >= 1024 || strings.Count(result.Text, "x") >= runCommandAutoReceiptBytes {
			t.Fatalf("large success receipt is not compact: %d bytes", len(result.Text))
		}
		if !strings.Contains(result.OriginalText, largeOutput) || !strings.Contains(result.OriginalText, "[exit code: 0]") {
			t.Fatalf("large success original was not preserved: %d bytes", len(result.OriginalText))
		}
	})

	t.Run("auto preserves small success", func(t *testing.T) {
		result, err := runRunCommandResult(t, map[string]any{
			"argv": []string{"printf", "small output"},
		})
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(result.Text, "small output") || result.OriginalText != "" {
			t.Fatalf("small auto result = %+v", result)
		}
	})

	t.Run("receipt compacts small success", func(t *testing.T) {
		result, err := runRunCommandResult(t, map[string]any{
			"argv":        []string{"printf", "small output"},
			"name":        "small check",
			"output_mode": "receipt",
		})
		if err != nil {
			t.Fatal(err)
		}
		// A small unclipped success is fully shown in the receipt, so nothing is
		// archived and the result is not marked truncated.
		if !strings.Contains(result.Text, "PASS small check") ||
			!strings.Contains(result.Text, "small output") {
			t.Fatalf("receipt result = %+v", result)
		}
		if result.OriginalText != "" {
			t.Fatalf("small receipt should not archive: %+v", result)
		}
		if strings.Contains(result.Text, "archived") {
			t.Fatalf("small receipt should not carry an archive hint:\n%s", result.Text)
		}
	})

	t.Run("receipt archives clipped success", func(t *testing.T) {
		args := make(map[string]any, len(largeArgs)+1)
		for key, value := range largeArgs {
			args[key] = value
		}
		args["output_mode"] = "receipt"
		result, err := runRunCommandResult(t, args)
		if err != nil {
			t.Fatal(err)
		}
		if result.OriginalText == "" {
			t.Fatalf("clipped receipt should archive its original: %+v", result)
		}
		if !strings.Contains(result.OriginalText, largeOutput) {
			t.Fatalf("clipped receipt original missing output: %d bytes", len(result.OriginalText))
		}
	})

	t.Run("full preserves large success", func(t *testing.T) {
		args := make(map[string]any, len(largeArgs)+1)
		for key, value := range largeArgs {
			args[key] = value
		}
		args["output_mode"] = "full"
		result, err := runRunCommandResult(t, args)
		if err != nil {
			t.Fatal(err)
		}
		if len(result.Text) <= runCommandAutoReceiptBytes || result.OriginalText != "" ||
			!strings.Contains(result.Text, "SUMMARY ok") {
			t.Fatalf("full result = text %d bytes, original %d bytes", len(result.Text), len(result.OriginalText))
		}
	})
}

func TestRunCommandAutoBoundsFailureAndPreservesClippedOriginal(t *testing.T) {
	largeOutput := strings.Repeat("x", runCommandFailureOutputBytes+1000)
	result, err := runRunCommandResult(t, map[string]any{
		"command": "cat; printf '\\nfailure-tail\\n'; exit 7",
		"stdin":   largeOutput,
		"name":    "failing test",
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"FAIL failing test", "failure-tail", "[showing output tail]", "[exit code: 7]"} {
		if !strings.Contains(result.Text, want) {
			t.Fatalf("failure receipt missing %q:\n%s", want, result.Text)
		}
	}
	if len(result.Text) > runCommandFailureOutputBytes+512 {
		t.Fatalf("failure receipt = %d bytes, want bounded diagnostic", len(result.Text))
	}
	if !strings.Contains(result.OriginalText, largeOutput) || !strings.Contains(result.OriginalText, "[exit code: 7]") {
		t.Fatalf("failure original was not preserved: %d bytes", len(result.OriginalText))
	}
	if outcome, ok := CommandResultOutcome(llm.ToolResult{Metrics: result.Metrics}); !ok || outcome != CommandOutcomeFailed || result.Metrics[CommandMetricExitCode] != 7 {
		t.Fatalf("failure outcome metrics = %+v, outcome=%v available=%v", result.Metrics, outcome, ok)
	}

	small, err := runRunCommandResult(t, map[string]any{"command": "printf small-failure; exit 2"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(small.Text, "small-failure") || small.OriginalText != "" {
		t.Fatalf("small failure result = %+v", small)
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
	if outcome, ok := CommandResultOutcome(llm.ToolResult{Metrics: result.Metrics}); !ok || outcome != CommandOutcomeFailed ||
		result.Metrics[CommandMetricStepsFailed] != 1 || result.Metrics[CommandMetricStepsSkipped] != 1 {
		t.Fatalf("batch outcome metrics = %+v, outcome=%v available=%v", result.Metrics, outcome, ok)
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

func TestRunCommandStepsSupportsBatchNameAndFullOutput(t *testing.T) {
	result, err := runRunCommandResult(t, map[string]any{
		"name": "checks", "output_mode": "full",
		"steps": []map[string]any{{"name": "one", "argv": []string{"printf", "STEP_ONE"}}, {"name": "two", "argv": []string{"printf", "STEP_TWO"}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"== checks ==", "==> one <==", "STEP_ONE", "==> two <==", "STEP_TWO"} {
		if !strings.Contains(result.Text, want) {
			t.Fatalf("full step output missing %q:\n%s", want, result.Text)
		}
	}
	if result.OriginalText != "" {
		t.Fatalf("full output should not need archive: %+v", result)
	}
}

func TestRunCommandRejectsInvalidOutputMode(t *testing.T) {
	_, err := runRunCommandResult(t, map[string]any{
		"command":     "true",
		"output_mode": "verbose",
	})
	if err == nil || !strings.Contains(err.Error(), `output_mode must be "auto", "receipt", or "full"`) {
		t.Fatalf("invalid output mode error = %v", err)
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

func TestRunCommandTopLevelDispatchArchivesReceiptOriginal(t *testing.T) {
	r := &Registry{}
	r.Register(runCommand{})
	res := r.Dispatch(context.Background(), llm.ToolCall{
		ID:    "top-level",
		Name:  "run_command",
		Input: json.RawMessage(`{"argv":["printf","verbose-success"],"name":"check","output_mode":"receipt"}`),
	})
	// A small unclipped success is fully shown in the receipt, so dispatch does
	// not archive or truncate.
	if res.IsError || res.Truncated {
		t.Fatalf("dispatch result = %+v", res)
	}
	if !strings.Contains(res.Text, "PASS check") || !strings.Contains(res.Text, "verbose-success") {
		t.Fatalf("receipt dispatch result = %+v", res)
	}
	if res.OriginalText != "" {
		t.Fatalf("unclipped receipt should not archive: %+v", res)
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
	if !strings.HasPrefix(out, "background job bg_test started (resource: ") ||
		!strings.HasSuffix(out, ", access: exclusive)") {
		t.Fatalf("start output = %q", out)
	}
	if starter.req.ResourceKey == "" || starter.req.Access != BackgroundAccessExclusive {
		t.Fatalf("job lease = %q/%q, want canonical cwd/exclusive", starter.req.ResourceKey, starter.req.Access)
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
	if !strings.HasPrefix(out, "background job bg_test started (resource: ") ||
		!strings.HasSuffix(out, ", access: exclusive)") {
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

func TestRunCommandBackgroundPreservesReceiptOriginal(t *testing.T) {
	starter := &fakeBackgroundStarter{}
	_, err := runTool(t, runCommand{background: starter}, map[string]any{
		"argv":        []string{"printf", "background output"},
		"name":        "background check",
		"output_mode": "receipt",
		"background":  true,
	})
	if err != nil {
		t.Fatalf("start background receipt: %v", err)
	}
	result, err := starter.req.Run(context.Background(), "bg_test")
	if err != nil {
		t.Fatalf("background run: %v", err)
	}
	if !strings.Contains(result.Text, "PASS background check") ||
		!strings.Contains(result.Text, "background output") {
		t.Fatalf("background receipt result = %+v", result)
	}
	if result.OriginalText != "" {
		t.Fatalf("unclipped background receipt should not archive: %+v", result)
	}
}

func TestRunCommandBackgroundLeaseOverrideAndForegroundValidation(t *testing.T) {
	starter := &fakeBackgroundStarter{}
	resource := t.TempDir()
	out, err := runTool(t, runCommand{background: starter}, map[string]any{
		"command":    "printf read-only",
		"background": true,
		"background_lease": map[string]any{
			"resource_key": resource,
			"access":       BackgroundAccessReadOnly,
		},
	})
	if err != nil {
		t.Fatalf("background override: %v", err)
	}
	wantResource, err := CanonicalBackgroundResource(resource)
	if err != nil {
		t.Fatalf("canonical resource: %v", err)
	}
	if starter.req.ResourceKey != wantResource || starter.req.Access != BackgroundAccessReadOnly {
		t.Fatalf("job lease = %q/%q, want %q/read_only", starter.req.ResourceKey, starter.req.Access, wantResource)
	}
	if !strings.Contains(out, "access: read_only") {
		t.Fatalf("start output = %q", out)
	}

	_, err = runTool(t, runCommand{background: starter}, map[string]any{
		"command": "printf foreground",
		"background_lease": map[string]any{
			"resource_key": resource,
			"access":       BackgroundAccessExclusive,
		},
	})
	if err == nil || !strings.Contains(err.Error(), "requires background:true") {
		t.Fatalf("foreground lease error = %v", err)
	}
}

func TestRunCommandAcceptsLegacyTopLevelBackgroundLeaseAliases(t *testing.T) {
	starter := &fakeBackgroundStarter{}
	resource := t.TempDir()
	_, err := runTool(t, runCommand{background: starter}, map[string]any{
		"command":      "printf compatibility",
		"background":   true,
		"resource_key": resource,
		"access":       BackgroundAccessReadOnly,
	})
	if err != nil {
		t.Fatalf("legacy background lease: %v", err)
	}
	wantResource, err := CanonicalBackgroundResource(resource)
	if err != nil {
		t.Fatal(err)
	}
	if starter.req.ResourceKey != wantResource || starter.req.Access != BackgroundAccessReadOnly {
		t.Fatalf("legacy lease = %q/%q, want %q/read_only", starter.req.ResourceKey, starter.req.Access, wantResource)
	}
}

func TestRunCommandRejectsNestedAndLegacyLeaseTogether(t *testing.T) {
	_, err := runTool(t, runCommand{background: &fakeBackgroundStarter{}}, map[string]any{
		"command":      "true",
		"background":   true,
		"resource_key": t.TempDir(),
		"background_lease": map[string]any{
			"access": BackgroundAccessReadOnly,
		},
	})
	if err == nil || !strings.Contains(err.Error(), "not both") {
		t.Fatalf("mixed lease error = %v", err)
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

func TestRunCommandReceiptTimeoutPreservesOriginalWhenWaitIncomplete(t *testing.T) {
	oldUnit := processTimeoutUnit
	oldGrace := processReapGrace
	processTimeoutUnit = 25 * time.Millisecond
	processReapGrace = 25 * time.Millisecond
	oldKill := killProcessGroup
	killProcessGroup = func(int) {}
	t.Cleanup(func() {
		processTimeoutUnit = oldUnit
		processReapGrace = oldGrace
		killProcessGroup = oldKill
	})

	result, err := runRunCommandResult(t, map[string]any{
		// Short, unclipped partial output, but the wait does not finish: the
		// receipt drops the partial-reap signal, so the original must be kept.
		"command":         `echo started; sleep 5`,
		"timeout_seconds": 1,
		"output_mode":     "receipt",
	})
	if err != nil {
		t.Fatalf("timeout must report a result, not a tool error: %v", err)
	}
	if result.OriginalText == "" {
		t.Fatalf("incomplete-wait receipt must keep its original: %+v", result)
	}
	if !strings.Contains(result.OriginalText, "wait did not finish") {
		t.Fatalf("original should carry the partial-reap signal:\n%s", result.OriginalText)
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
