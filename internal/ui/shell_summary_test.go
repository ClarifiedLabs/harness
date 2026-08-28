package ui

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"harness/internal/llm"
	"harness/internal/tools"
)

// shellResultMetrics builds the diagnostics metrics shell attaches to a
// top-level (non-step) result.
func shellResultMetrics(kind string, exit int) map[string]int {
	metrics := map[string]int{
		tools.CommandMetricOutcomeAvailable: 1,
		tools.CommandMetricExitCode:         exit,
	}
	switch kind {
	case "ok":
		metrics[tools.CommandMetricSucceeded] = 1
	case "failed":
		metrics[tools.CommandMetricFailed] = 1
	case "cancelled":
		metrics[tools.CommandMetricCancelled] = 1
	case "cancelled timed out":
		metrics[tools.CommandMetricCancelled] = 1
		metrics[tools.CommandMetricTimedOut] = 1
	case "timed out":
		metrics[tools.CommandMetricFailed] = 1
		metrics[tools.CommandMetricTimedOut] = 1
	}
	return metrics
}

// stepResultMetrics builds the metrics shellSteps attaches to a step batch.
func stepResultMetrics(kind string, exit, total, executed int) map[string]int {
	metrics := map[string]int{
		tools.CommandMetricOutcomeAvailable: 1,
		tools.CommandMetricExitCode:         exit,
		tools.CommandMetricStepsTotal:       total,
		tools.CommandMetricStepsExecuted:    executed,
		tools.CommandMetricStepsSkipped:     total - executed,
	}
	switch kind {
	case "ok":
		metrics[tools.CommandMetricSucceeded] = 1
	case "failed":
		metrics[tools.CommandMetricFailed] = 1
		metrics[tools.CommandMetricStepsFailed] = 1
	case "timed out":
		metrics[tools.CommandMetricFailed] = 1
		metrics[tools.CommandMetricTimedOut] = 1
		metrics[tools.CommandMetricStepsTimedOut] = 1
	}
	return metrics
}

func TestConciseShellResultLineForms(t *testing.T) {
	cases := []struct {
		name   string
		input  string
		result llm.ToolResult
		want   string
	}{
		{
			name:   "top-level argv with name",
			input:  `{"name":"Unit tests","argv":["make","test"]}`,
			result: llm.ToolResult{ForID: "c", Text: "PASS\nok\n", Metrics: shellResultMetrics("ok", 0)},
			want:   "[shell] Unit tests · ok → 2 lines, 8B",
		},
		{
			name:   "top-level command without name",
			input:  `{"command":"make test"}`,
			result: llm.ToolResult{ForID: "c", Text: "x\n", Metrics: shellResultMetrics("failed", 2)},
			want:   "[shell] make test · failed (exit 2) → 2B",
		},
		{
			name: "steps with name",
			input: `{"name":"Final diff whitespace","steps":[` +
				`{"name":"git diff --check","argv":["git","diff","--check"]},` +
				`{"argv":["go","build","./..."]}],"output_mode":"full","cwd":"."}`,
			result: llm.ToolResult{
				ForID: "c", Text: "a\nb\n",
				Metrics: stepResultMetrics("ok", 0, 2, 2),
			},
			want: "[shell] steps=2 Final diff whitespace: git diff --check; go build ./... · ok → 2 lines, 4B",
		},
		{
			name: "steps without name",
			input: `{"steps":[` +
				`{"argv":["make","build"]},{"argv":["make","test"]},{"argv":["git","status"]}]}`,
			result: llm.ToolResult{
				ForID: "c", Text: "done\n",
				Metrics: stepResultMetrics("ok", 0, 3, 3),
			},
			want: "[shell] steps=3 make build; make test; git status · ok → 5B",
		},
		{
			name: "more than three steps collapse with semicolon continuation",
			input: `{"steps":[` +
				`{"argv":["cmd1"]},{"argv":["cmd2"]},{"argv":["cmd3"]},{"argv":["cmd4"]},{"argv":["cmd5"]}]}`,
			result: llm.ToolResult{
				ForID: "c", Text: "done\n",
				Metrics: stepResultMetrics("ok", 0, 5, 5),
			},
			want: "[shell] steps=5 cmd1; cmd2; cmd3; +2 more · ok → 5B",
		},
		{
			name: "comma inside a step label stays unsplit",
			input: `{"steps":[` +
				`{"name":"rg -n \"a,b\" file","argv":["rg","-n","a,b","file"]},{"argv":["true"]}]}`,
			result: llm.ToolResult{
				ForID: "c", Text: "no match\n",
				Metrics: stepResultMetrics("ok", 0, 2, 2),
			},
			want: `[shell] steps=2 rg -n "a,b" file; true · ok → 9B`,
		},
		{
			name: "step label falls back to command text",
			input: `{"steps":[` +
				`{"command":"make install && make test"}]}`,
			result: llm.ToolResult{
				ForID: "c", Text: "done\n",
				Metrics: stepResultMetrics("failed", 1, 1, 1),
			},
			want: "[shell] steps=1 make install && make test · failed (exit 1) → 5B",
		},
		{
			name: "partial step run reports progress",
			input: `{"steps":[` +
				`{"argv":["false"]},{"argv":["true"]},{"argv":["true"]}]}`,
			result: llm.ToolResult{
				ForID: "c", Text: "FAIL step 1\nSKIP 2 remaining step(s)\n",
				Metrics: stepResultMetrics("failed", 1, 3, 1),
			},
			want: "[shell] steps=3 false; true; true · failed (exit 1) · ran 1/3, skipped 2 → 2 lines, 37B",
		},
		{
			name:  "cancelled result",
			input: `{"argv":["sleep","10"]}`,
			result: llm.ToolResult{
				ForID: "c", Text: "[cancelled; process group killed]\n",
				Metrics: shellResultMetrics("cancelled", -1),
			},
			want: "[shell] sleep 10 · cancelled → 34B",
		},
		{
			name:  "timed out result",
			input: `{"argv":["sleep","10"],"timeout_seconds":1}`,
			result: llm.ToolResult{
				ForID: "c", Text: "[timed out after 1s; process group killed]\n",
				Metrics: shellResultMetrics("timed out", -1),
			},
			want: "[shell] sleep 10 · timed out → 43B",
		},
		{
			name:   "failed with zero exit code renders bare failed",
			input:  `{"argv":["make","test"]}`,
			result: llm.ToolResult{ForID: "c", Text: "boom\n", Metrics: shellResultMetrics("failed", 0)},
			want:   "[shell] make test · failed → 5B",
		},
		{
			name:   "signal-killed failure hides the negative exit code",
			input:  `{"argv":["make","test"]}`,
			result: llm.ToolResult{ForID: "c", Text: "boom\n", Metrics: shellResultMetrics("failed", -1)},
			want:   "[shell] make test · failed → 5B",
		},
		{
			name: "cancelled result carrying the timed-out flag renders timed out",
			input: `{"argv":["sleep","10"]}`,
			result: llm.ToolResult{
				ForID: "c", Text: "[cancelled]\n",
				Metrics: shellResultMetrics("cancelled timed out", -1),
			},
			want: "[shell] sleep 10 · timed out → 12B",
		},
		{
			name:   "name with embedded newline collapses to spaces",
			input:  `{"name":"Line one\nLine two","argv":["make","test"]}`,
			result: llm.ToolResult{ForID: "c", Text: "ok\n", Metrics: shellResultMetrics("ok", 0)},
			want:   "[shell] Line one Line two · ok → 3B",
		},
		{
			name:   "long name is clipped like a command",
			input:  `{"name":"` + strings.Repeat("a", 100) + `","argv":["true"]}`,
			result: llm.ToolResult{ForID: "c", Text: "ok\n", Metrics: shellResultMetrics("ok", 0)},
			want:   "[shell] " + strings.Repeat("a", 79) + "… · ok → 3B",
		},
		{
			name:   "argv encoded as a JSON string is tolerated",
			input:  `{"argv":"[\"make\",\"test\"]"}`,
			result: llm.ToolResult{ForID: "c", Text: "ok\n", Metrics: shellResultMetrics("ok", 0)},
			want:   "[shell] make test · ok → 3B",
		},
		{
			name:   "no metrics falls back to detailed",
			input:  `{"argv":["make","test"]}`,
			result: llm.ToolResult{ForID: "c", Text: "ok\n"},
			want:   `[shell] argv=["make","test"] → 3B`,
		},
		{
			name:   "malformed input falls back to detailed",
			input:  `{"argv":`,
			result: llm.ToolResult{ForID: "c", Text: "ok\n", Metrics: shellResultMetrics("ok", 0)},
			want:   "[shell] → 3B",
		},
		{
			name:   "error result falls back to detailed",
			input:  `{"argv":["make","test"]}`,
			result: llm.ToolResult{ForID: "c", Text: "boom", IsError: true},
			want:   `[shell] argv=["make","test"] → error: boom`,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var out, errw bytes.Buffer
			r := NewRenderer(&out, &errw, RenderOptions{ConciseShell: true})
			r.ToolStart(llm.ToolCall{ID: "c", Name: "shell", Input: json.RawMessage(tc.input)})
			r.ToolResult(tc.result)
			got := strings.TrimSuffix(errw.String(), "\n")
			if got != tc.want {
				t.Fatalf("concise shell line =\n  %q\nwant\n  %q", got, tc.want)
			}
		})
	}
}

func TestConciseShellCommandLineClipping(t *testing.T) {
	long := strings.Repeat("a", 100)
	var out, errw bytes.Buffer
	r := NewRenderer(&out, &errw, RenderOptions{ConciseShell: true})
	r.ToolStart(llm.ToolCall{ID: "c", Name: "shell", Input: json.RawMessage(`{"command":"` + long + `"}`)})
	r.ToolResult(llm.ToolResult{ForID: "c", Text: "ok\n", Metrics: shellResultMetrics("ok", 0)})
	got := strings.TrimSuffix(errw.String(), "\n")
	if !strings.HasPrefix(got, "[shell] "+strings.Repeat("a", 79)) {
		t.Fatalf("clipped command line lost its head: %q", got)
	}
	if !strings.HasSuffix(strings.SplitN(got, " · ", 2)[0], "…") {
		t.Fatalf("clipped command line missing ellipsis: %q", got)
	}
}

func TestConciseShellBackgroundLaunch(t *testing.T) {
	dir := t.TempDir()
	cases := []struct {
		name  string
		input string
		jobID string
		want  string
	}{
		{
			name:  "lease with resource under cwd is relativized",
			input: `{"name":"Final full root test gate","argv":["make","test"],"background":true,"background_lease":{"access":"read_only","resource_key":"` + dir + `/monorepo"}}`,
			jobID: "j3",
			want:  "[shell] Final full root test gate · background job j3 (read_only @ monorepo) → started",
		},
		{
			name:  "lease resource outside cwd stays absolute",
			input: `{"argv":["make","test"],"background":true,"background_lease":{"access":"exclusive","resource_key":"/elsewhere/repo"}}`,
			jobID: "j9",
			want:  "[shell] make test · background job j9 (exclusive @ /elsewhere/repo) → started",
		},
		{
			name:  "no lease renders just the job id",
			input: `{"command":"make test","background":true}`,
			jobID: "j1",
			want:  "[shell] make test · background job j1 → started",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var out, errw bytes.Buffer
			r := NewRenderer(&out, &errw, RenderOptions{ConciseShell: true, CWD: dir})
			call := llm.ToolCall{ID: "c", Name: "shell", Input: json.RawMessage(tc.input)}
			r.ToolStart(call)
			r.ToolResult(llm.ToolResult{
				ForID: "c", Text: "background job started", BackgroundJobID: tc.jobID,
				Metrics: map[string]int{
					tools.CommandMetricOutcomeAvailable: 1,
					tools.CommandMetricSucceeded:        1,
				},
			})
			got := strings.TrimSuffix(errw.String(), "\n")
			if got != tc.want {
				t.Fatalf("background launch line =\n  %q\nwant\n  %q", got, tc.want)
			}
		})
	}
}

// TestConciseShellBackgroundLaunchRequiresContract keeps launch receipts
// without the command-outcome metrics (legacy recordings, foreign results)
// on the canonical detailed line.
func TestConciseShellBackgroundLaunchRequiresContract(t *testing.T) {
	dir := t.TempDir()
	var out, errw bytes.Buffer
	r := NewRenderer(&out, &errw, RenderOptions{ConciseShell: true, CWD: dir})
	call := llm.ToolCall{ID: "c", Name: "shell", Input: json.RawMessage(`{"command":"make test","background":true}`)}
	result := llm.ToolResult{ForID: "c", Text: "background job started", BackgroundJobID: "j1"}
	r.ToolStart(call)
	r.ToolResult(result)
	got := strings.TrimSuffix(errw.String(), "\n")
	if want := ToolResultLine(call, result, dir); got != want {
		t.Fatalf("contractless launch line =\n  %q\nwant detailed\n  %q", got, want)
	}
}

func TestConciseShellDisabledOrVerboseKeepsDetailedLine(t *testing.T) {
	input := json.RawMessage(`{"name":"Unit tests","argv":["make","test"]}`)
	result := llm.ToolResult{ForID: "c", Text: "ok\n", Metrics: shellResultMetrics("ok", 0)}
	for _, tc := range []struct {
		name string
		opts RenderOptions
	}{
		{name: "conciseShell off", opts: RenderOptions{}},
		{name: "verbose", opts: RenderOptions{ConciseShell: true, Verbose: true}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var out, errw bytes.Buffer
			r := NewRenderer(&out, &errw, tc.opts)
			r.ToolStart(llm.ToolCall{ID: "c", Name: "shell", Input: input})
			r.ToolResult(result)
			got := strings.TrimSuffix(errw.String(), "\n")
			want := `[shell] argv=["make","test"] name="Unit tests" → 3B`
			if tc.name == "verbose" {
				want = "[tool: shell started argv=[\"make\",\"test\"] name=\"Unit tests\"]\n" + want + "\n  ok"
			}
			if got != want {
				t.Fatalf("detailed line =\n  %q\nwant\n  %q", got, want)
			}
		})
	}
}

// TestConciseShellFailureKeepsResultSummary pins the interaction between the
// new status segment and the canonical size summary: a failure still reports
// its output shape after the status.
func TestConciseShellFailureKeepsResultSummary(t *testing.T) {
	var out, errw bytes.Buffer
	r := NewRenderer(&out, &errw, RenderOptions{ConciseShell: true})
	r.ToolStart(llm.ToolCall{ID: "c", Name: "shell", Input: json.RawMessage(`{"argv":["make","test"]}`)})
	r.ToolResult(llm.ToolResult{
		ForID:   "c",
		Text:    "FAIL go vet ./...\n--- FAIL: TestX\n[exit code: 2]\n",
		Metrics: shellResultMetrics("failed", 2),
	})
	got := strings.TrimSuffix(errw.String(), "\n")
	want := "[shell] make test · failed (exit 2) → 3 lines, 49B"
	if got != want {
		t.Fatalf("failure line =\n  %q\nwant\n  %q", got, want)
	}
}

// TestConciseShellReportExamples reproduces the two lines from the bug report
// end to end through the renderer.
func TestConciseShellReportExamples(t *testing.T) {
	dir := t.TempDir()
	var out, errw bytes.Buffer
	r := NewRenderer(&out, &errw, RenderOptions{ConciseShell: true, CWD: dir})

	stepsCall := llm.ToolCall{
		ID:   "s1",
		Name: "shell",
		Input: json.RawMessage(`{"name":"Final diff whitespace","steps":[` +
			`{"name":"git diff --check","argv":["git","diff","--check"]},` +
			`{"argv":["go","build","./..."]}],` +
			`"output_mode":"full","cwd":".","stop_on_failure":true}`),
	}
	r.ToolStart(stepsCall)
	r.ToolResult(llm.ToolResult{
		ForID:   "s1",
		Text:    strings.Repeat("l\n", 13),
		Metrics: stepResultMetrics("ok", 0, 2, 2),
	})

	bgCall := llm.ToolCall{
		ID:   "s2",
		Name: "shell",
		Input: json.RawMessage(`{"name":"Final full root test gate","argv":["make","test"],` +
			`"background":true,"background_lease":{"access":"read_only","resource_key":"` + dir + `/nb/monorepo"}}`),
	}
	r.ToolStart(bgCall)
	r.ToolResult(llm.ToolResult{
		ForID: "s2", Text: "background job j3 started (resource: x, access: read_only)", BackgroundJobID: "j3",
		Metrics: map[string]int{
			tools.CommandMetricOutcomeAvailable: 1,
			tools.CommandMetricSucceeded:        1,
		},
	})

	got := errw.String()
	want := "[shell] steps=2 Final diff whitespace: git diff --check; go build ./... · ok → 13 lines, 26B\n" +
		"[shell] Final full root test gate · background job j3 (read_only @ nb/monorepo) → started\n"
	if got != want {
		t.Fatalf("concise shell lines =\n  %q\nwant\n  %q", got, want)
	}
}

// TestConciseShellPreservesDetailedRecordingForm guards the architectural
// invariant that the live projection never leaks into the canonical
// sessionrec.ToolResultLine used by recording and replay.
func TestConciseShellPreservesDetailedRecordingForm(t *testing.T) {
	call := llm.ToolCall{
		ID:    "c",
		Name:  "shell",
		Input: json.RawMessage(`{"name":"Unit tests","argv":["make","test"],"cwd":".","output_mode":"receipt"}`),
	}
	result := llm.ToolResult{ForID: "c", Text: "ok\n", Metrics: shellResultMetrics("ok", 0)}
	want := `[shell] argv=["make","test"] cwd=. name="Unit tests" output_mode=receipt → 3B`
	if got := ToolResultLine(call, result, ""); got != want {
		t.Fatalf("canonical detailed line changed:\n  got  %q\n  want %q", got, want)
	}

	var out, errw bytes.Buffer
	r := NewRenderer(&out, &errw, RenderOptions{ConciseShell: true})
	r.ToolStart(call)
	r.ToolResult(result)
	if strings.Contains(errw.String(), "output_mode=") {
		t.Fatalf("concise line leaked detailed keys: %q", errw.String())
	}
}
