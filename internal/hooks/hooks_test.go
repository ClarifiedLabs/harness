package hooks

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestDecodeFileAcceptsWrapperAndAliases(t *testing.T) {
	cfg, err := DecodeFile([]byte(`{
		"hooks": {
			"PreToolUse": [
				{
					"matcher": "^shell$",
					"hooks": [
						{"type":"command","command":"printf ok","timeout":3,"statusMessage":"Checking"}
					]
				}
			]
		}
	}`))
	if err != nil {
		t.Fatalf("DecodeFile: %v", err)
	}
	groups := cfg.Groups(PreToolUse)
	if len(groups) != 1 {
		t.Fatalf("PreToolUse groups = %d, want 1", len(groups))
	}
	if !groups[0].matches("shell") || groups[0].matches("read") {
		t.Fatalf("matcher did not behave as expected")
	}
	h := groups[0].Hooks[0]
	if h.TimeoutSeconds != 3 || h.StatusMessage != "Checking" {
		t.Fatalf("aliases not decoded: %+v", h)
	}
}

func TestLoadFilesAppendsInOrderAndUsesBaseDir(t *testing.T) {
	dir := t.TempDir()
	first := filepath.Join(dir, "first.json")
	second := filepath.Join(dir, "second.json")
	if err := os.WriteFile(first, []byte(`{"PreToolUse":[{"hooks":[{"type":"command","command":"printf first"}]}]}`), 0o644); err != nil {
		t.Fatalf("write first: %v", err)
	}
	if err := os.WriteFile(second, []byte(`{"hooks":{"PreToolUse":[{"hooks":[{"type":"command","command":"printf second"}]}]}}`), 0o644); err != nil {
		t.Fatalf("write second: %v", err)
	}
	cfg, err := LoadFiles(dir, []string{"first.json", "second.json"})
	if err != nil {
		t.Fatalf("LoadFiles: %v", err)
	}
	groups := cfg.Groups(PreToolUse)
	if len(groups) != 2 {
		t.Fatalf("groups = %d, want 2", len(groups))
	}
	if groups[0].Hooks[0].Command != "printf first" || groups[1].Hooks[0].Command != "printf second" {
		t.Fatalf("groups out of order: %+v", groups)
	}
}

func TestRunnerBlocksAndPassesPayloadOnStdin(t *testing.T) {
	dir := t.TempDir()
	payloadPath := filepath.Join(dir, "payload.json")
	cfg, err := DecodeEventMap([]byte(`{
		"PreToolUse": [
			{
				"hooks": [
					{"type":"command","command":"cat > ` + shellQuote(payloadPath) + `; printf '{\"decision\":\"block\",\"reason\":\"no writes\"}'"}
				]
			}
		]
	}`))
	if err != nil {
		t.Fatalf("DecodeEventMap: %v", err)
	}
	runner := &Runner{Config: cfg, CWD: dir, SessionID: "s1", TranscriptPath: "s1", Model: "m1"}
	res := runner.Run(context.Background(), PreToolUse, "write", Payload{"tool_name": "write"})
	if !res.Block || res.Reason() != "no writes" {
		t.Fatalf("result = %+v, want block reason", res)
	}
	payload, err := os.ReadFile(payloadPath)
	if err != nil {
		t.Fatalf("read payload: %v", err)
	}
	if !strings.Contains(string(payload), `"hook_event_name":"PreToolUse"`) || !strings.Contains(string(payload), `"tool_name":"write"`) {
		t.Fatalf("payload missing fields: %s", payload)
	}
}

func TestRunnerExitCodeTwoBlocksPlainOutput(t *testing.T) {
	cfg, err := DecodeEventMap([]byte(`{"UserPromptSubmit":[{"hooks":[{"type":"command","command":"printf blocked; exit 2"}]}]}`))
	if err != nil {
		t.Fatalf("DecodeEventMap: %v", err)
	}
	res := (&Runner{Config: cfg}).Run(context.Background(), UserPromptSubmit, "", nil)
	if !res.Block || res.Reason() != "blocked" {
		t.Fatalf("result = %+v, want block from exit code 2", res)
	}
}

func TestRunnerParsesTypedStopEvaluatorResult(t *testing.T) {
	cfg, err := DecodeEventMap([]byte(`{"Stop":[{"hooks":[{"name":"verify","command":"ignored"}]}]}`))
	if err != nil {
		t.Fatal(err)
	}
	runner := &Runner{Config: cfg, execute: func(context.Context, Handler, string, []byte) commandResult {
		return commandResult{Stdout: `{"accepted":false,"score":0,"score_direction":"maximize","candidate":"sha256:abc","remaining_requirements":2,"evidence_ref":"artifacts/verify.log","reason":"fix the failing test"}`}
	}}

	res := runner.Run(context.Background(), Stop, "", nil)
	if !res.Block || len(res.EvaluatorResults) != 1 {
		t.Fatalf("result = %+v, want one rejecting evaluator result", res)
	}
	got := res.EvaluatorResults[0]
	if got.Handler != "verify" || got.Accepted || got.Score == nil || *got.Score != 0 || got.ScoreDirection != ScoreDirectionMaximize || got.Candidate != "sha256:abc" ||
		got.RemainingRequirements == nil || *got.RemainingRequirements != 2 || got.EvidenceRef != "artifacts/verify.log" {
		t.Fatalf("evaluator result = %+v", got)
	}
	for _, want := range []string{`Evaluator "verify" rejected the candidate`, "score=0", "remaining_requirements=2", "fix the failing test"} {
		if !strings.Contains(res.Reason(), want) {
			t.Fatalf("reason %q missing %q", res.Reason(), want)
		}
	}
	if strings.Contains(res.Reason(), "score_direction") {
		t.Fatalf("shadow score direction entered corrective context: %q", res.Reason())
	}
	if len(res.Diagnostics) != 1 || res.Diagnostics[0].Outcome != OutcomeSuccess {
		t.Fatalf("diagnostics = %+v, want successful hook process", res.Diagnostics)
	}
}

func TestRunnerPreservesZeroScoreOnAcceptedEvaluatorResult(t *testing.T) {
	cfg, err := DecodeEventMap([]byte(`{"Stop":[{"hooks":[{"command":"ignored"}]}]}`))
	if err != nil {
		t.Fatal(err)
	}
	runner := &Runner{Config: cfg, execute: func(context.Context, Handler, string, []byte) commandResult {
		return commandResult{Stdout: `{"accepted":true,"score":0,"remaining_requirements":0}`}
	}}

	res := runner.Run(context.Background(), Stop, "", nil)
	if res.Block || len(res.EvaluatorResults) != 1 || res.EvaluatorResults[0].Score == nil || *res.EvaluatorResults[0].Score != 0 || res.EvaluatorResults[0].ScoreDirection != "" {
		t.Fatalf("result = %+v, want accepted evaluator result with score zero", res)
	}
	if res.EvaluatorResults[0].RemainingRequirements == nil || *res.EvaluatorResults[0].RemainingRequirements != 0 {
		t.Fatalf("remaining requirements = %+v, want explicit zero", res.EvaluatorResults[0].RemainingRequirements)
	}
}

func TestRunnerAcceptsRejectingEvaluatorResultWithExitCodeTwo(t *testing.T) {
	cfg, err := DecodeEventMap([]byte(`{"Stop":[{"hooks":[{"name":"verify","command":"ignored"}]}]}`))
	if err != nil {
		t.Fatal(err)
	}
	runner := &Runner{Config: cfg, execute: func(context.Context, Handler, string, []byte) commandResult {
		return commandResult{Code: 2, Stdout: `{"accepted":false,"candidate":"candidate-2","reason":"still failing"}`}
	}}

	res := runner.Run(context.Background(), Stop, "", nil)
	if !res.Block || len(res.EvaluatorResults) != 1 || res.EvaluatorResults[0].Candidate != "candidate-2" || !strings.Contains(res.Reason(), "still failing") {
		t.Fatalf("result = %+v, want typed rejection", res)
	}
	if len(res.Diagnostics) != 1 || res.Diagnostics[0].Outcome != OutcomeExitNonzero {
		t.Fatalf("diagnostics = %+v, want legacy exit_nonzero outcome", res.Diagnostics)
	}
}

func TestRunnerRejectsInvalidEvaluatorFieldsWithoutChangingLegacyControl(t *testing.T) {
	tests := []struct {
		name      string
		event     Event
		stdout    string
		wantBlock bool
	}{
		{name: "non Stop event", event: PreToolUse, stdout: `{"accepted":false}`},
		{name: "missing accepted", event: Stop, stdout: `{"score":1}`},
		{name: "accepted with remaining work", event: Stop, stdout: `{"accepted":true,"remaining_requirements":1}`},
		{name: "direction without score", event: Stop, stdout: `{"accepted":false,"score_direction":"maximize"}`},
		{name: "invalid score direction", event: Stop, stdout: `{"accepted":false,"score":1,"score_direction":"sideways"}`},
		{name: "accepted conflicts with legacy block", event: Stop, stdout: `{"decision":"block","accepted":true}`, wantBlock: true},
		{name: "candidate too long", event: Stop, stdout: fmt.Sprintf(`{"accepted":false,"candidate":%q}`, strings.Repeat("x", maxEvaluatorIdentifierLen+1))},
		{name: "multiline evidence reference", event: Stop, stdout: `{"accepted":false,"evidence_ref":"artifacts/a\nsecond line"}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg, err := DecodeEventMap([]byte(fmt.Sprintf(`{"%s":[{"hooks":[{"command":"ignored"}]}]}`, tt.event)))
			if err != nil {
				t.Fatal(err)
			}
			runner := &Runner{Config: cfg, execute: func(context.Context, Handler, string, []byte) commandResult {
				return commandResult{Stdout: tt.stdout}
			}}

			res := runner.Run(context.Background(), tt.event, "shell", nil)
			if res.Block != tt.wantBlock || len(res.EvaluatorResults) != 0 {
				t.Fatalf("result = %+v, want block=%t and no evaluator result", res, tt.wantBlock)
			}
			if len(res.Diagnostics) != 1 || res.Diagnostics[0].Outcome != OutcomeParseFailed {
				t.Fatalf("diagnostics = %+v, want parse_failed", res.Diagnostics)
			}
			if len(res.Notices) == 0 || !strings.Contains(res.Notices[0], "invalid evaluator result") {
				t.Fatalf("notices = %v", res.Notices)
			}
		})
	}
}

func TestRunnerStopsAfterCanceledHandler(t *testing.T) {
	cfg, err := DecodeEventMap([]byte(`{"SessionStart":[{"hooks":[{"type":"command","command":"first"},{"type":"command","command":"must-not-run"}]}]}`))
	if err != nil {
		t.Fatalf("DecodeEventMap: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	calls := 0
	runner := &Runner{
		Config: cfg,
		execute: func(context.Context, Handler, string, []byte) commandResult {
			calls++
			cancel()
			return commandResult{Code: -1, Canceled: true}
		},
	}

	res := runner.Run(ctx, SessionStart, "startup", nil)
	if calls != 1 {
		t.Fatalf("handler calls = %d, want only the canceled first handler", calls)
	}
	if len(res.Diagnostics) != 1 || res.Diagnostics[0].Outcome != OutcomeCanceled {
		t.Fatalf("diagnostics = %+v, want one canceled handler", res.Diagnostics)
	}
}

func TestRunnerTimeoutFailsOpen(t *testing.T) {
	oldUnit := hookTimeoutUnit
	hookTimeoutUnit = 25 * time.Millisecond
	t.Cleanup(func() { hookTimeoutUnit = oldUnit })

	cfg, err := DecodeEventMap([]byte(`{"PreToolUse":[{"hooks":[{"type":"command","command":"sleep 2","timeout_seconds":1}]}]}`))
	if err != nil {
		t.Fatalf("DecodeEventMap: %v", err)
	}
	start := time.Now()
	res := (&Runner{Config: cfg}).Run(context.Background(), PreToolUse, "shell", nil)
	if res.Block {
		t.Fatalf("timeout should fail open: %+v", res)
	}
	if time.Since(start) > 1500*time.Millisecond {
		t.Fatalf("timeout took too long: %s", time.Since(start))
	}
	if len(res.Notices) == 0 || !strings.Contains(res.Notices[0], "timed out") {
		t.Fatalf("timeout notice = %v", res.Notices)
	}
}

func TestDecodeHandlerBreakerSettingsAndValidation(t *testing.T) {
	cfg, err := DecodeEventMap([]byte(`{"PreToolUse":[{"hooks":[{"type":"command","name":"policy","command":"true","max_consecutive_timeouts":0,"timeout_cooldown_seconds":7}]}]}`))
	if err != nil {
		t.Fatalf("DecodeEventMap: %v", err)
	}
	hook := cfg.Groups(PreToolUse)[0].Hooks[0]
	if hook.Name != "policy" || hook.MaxConsecutiveTimeouts == nil || *hook.MaxConsecutiveTimeouts != 0 || hook.TimeoutCooldownSeconds != 7 {
		t.Fatalf("decoded handler = %+v", hook)
	}
	for _, body := range []string{
		`{"PreToolUse":[{"hooks":[{"command":"true","max_consecutive_timeouts":-1}]}]}`,
		`{"PreToolUse":[{"hooks":[{"command":"true","max_consecutive_timeouts":101}]}]}`,
		`{"PreToolUse":[{"hooks":[{"command":"true","timeout_cooldown_seconds":3601}]}]}`,
	} {
		if _, err := DecodeEventMap([]byte(body)); err == nil {
			t.Fatalf("DecodeEventMap(%s) succeeded, want validation error", body)
		}
	}
}

func TestTimeoutCircuitBreakerDiagnosticsAndHalfOpenProbe(t *testing.T) {
	cfg, err := DecodeEventMap([]byte(`{"PreToolUse":[{"hooks":[{"name":"lint-policy","command":"ignored","timeout_seconds":4,"timeout_cooldown_seconds":60}]}]}`))
	if err != nil {
		t.Fatalf("DecodeEventMap: %v", err)
	}
	now := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)
	invocations := 0
	result := commandResult{TimedOut: true}
	runner := &Runner{
		Config: cfg,
		now:    func() time.Time { return now },
		execute: func(context.Context, Handler, string, []byte) commandResult {
			invocations++
			now = now.Add(2 * time.Second)
			return result
		},
	}
	for wantCount := 1; wantCount <= defaultMaxTimeouts; wantCount++ {
		res := runner.Run(context.Background(), PreToolUse, "shell", Payload{"tool_use_id": "tool-1"})
		if len(res.Diagnostics) != 1 {
			t.Fatalf("timeout %d diagnostics = %+v", wantCount, res.Diagnostics)
		}
		diagnostic := res.Diagnostics[0]
		if diagnostic.Event != PreToolUse || diagnostic.Handler != "lint-policy" || diagnostic.Target != "shell" || diagnostic.ToolID != "tool-1" ||
			diagnostic.TimeoutSeconds != 4 || diagnostic.Elapsed != 2*time.Second || diagnostic.ConsecutiveTimeouts != wantCount || diagnostic.Outcome != OutcomeTimeout {
			t.Fatalf("timeout %d diagnostic = %+v", wantCount, diagnostic)
		}
		if diagnostic.CircuitOpen != (wantCount == defaultMaxTimeouts) {
			t.Fatalf("timeout %d circuit open = %t", wantCount, diagnostic.CircuitOpen)
		}
		if len(res.Notices) != 1 || !strings.Contains(res.Notices[0], "PreToolUse") || !strings.Contains(res.Notices[0], "lint-policy") {
			t.Fatalf("timeout notice = %v", res.Notices)
		}
	}

	skipped := runner.Run(context.Background(), PreToolUse, "shell", Payload{"tool_use_id": "tool-1"})
	if invocations != defaultMaxTimeouts || len(skipped.Diagnostics) != 1 || skipped.Diagnostics[0].Outcome != OutcomeCircuitOpen || !skipped.Diagnostics[0].CircuitOpen {
		t.Fatalf("cooldown call invoked=%d result=%+v", invocations, skipped)
	}

	now = skipped.Diagnostics[0].CircuitOpenUntil.Add(time.Second)
	result = commandResult{TimedOut: true}
	reopened := runner.Run(context.Background(), PreToolUse, "shell", nil)
	if invocations != defaultMaxTimeouts+1 || reopened.Diagnostics[0].Outcome != OutcomeTimeout || reopened.Diagnostics[0].ConsecutiveTimeouts != defaultMaxTimeouts+1 || !reopened.Diagnostics[0].CircuitOpen {
		t.Fatalf("half-open timeout = %+v invocations=%d", reopened, invocations)
	}

	now = reopened.Diagnostics[0].CircuitOpenUntil.Add(time.Second)
	result = commandResult{}
	closed := runner.Run(context.Background(), PreToolUse, "shell", nil)
	if invocations != defaultMaxTimeouts+2 || closed.Diagnostics[0].Outcome != OutcomeSuccess || closed.Diagnostics[0].ConsecutiveTimeouts != 0 || closed.Diagnostics[0].CircuitOpen {
		t.Fatalf("half-open success = %+v invocations=%d", closed, invocations)
	}
}

func TestTimeoutBreakerDisabledAndOtherFailuresDoNotIncrement(t *testing.T) {
	cfg, err := DecodeEventMap([]byte(`{"Stop":[{"hooks":[{"command":"ignored","max_consecutive_timeouts":0}]}]}`))
	if err != nil {
		t.Fatal(err)
	}
	invocations := 0
	runner := &Runner{Config: cfg, execute: func(context.Context, Handler, string, []byte) commandResult {
		invocations++
		return commandResult{TimedOut: true}
	}}
	for i := 0; i < 5; i++ {
		res := runner.Run(context.Background(), Stop, "", nil)
		if res.Diagnostics[0].ConsecutiveTimeouts != 0 || res.Diagnostics[0].CircuitOpen {
			t.Fatalf("disabled breaker diagnostic = %+v", res.Diagnostics[0])
		}
	}
	if invocations != 5 {
		t.Fatalf("disabled breaker invocations = %d, want 5", invocations)
	}

	cfg, err = DecodeEventMap([]byte(`{"Stop":[{"hooks":[{"command":"ignored"}]}]}`))
	if err != nil {
		t.Fatal(err)
	}
	results := []commandResult{
		{TimedOut: true},
		{StartErr: os.ErrNotExist},
		{Canceled: true},
		{Code: 1},
		{Stdout: "{not json"},
		{},
	}
	runner = &Runner{Config: cfg, execute: func(context.Context, Handler, string, []byte) commandResult {
		result := results[0]
		results = results[1:]
		return result
	}}
	wantOutcomes := []DiagnosticOutcome{OutcomeTimeout, OutcomeStartFailed, OutcomeCanceled, OutcomeExitNonzero, OutcomeParseFailed, OutcomeSuccess}
	wantCounts := []int{1, 0, 0, 0, 0, 0}
	for i := range wantOutcomes {
		diagnostic := runner.Run(context.Background(), Stop, "", nil).Diagnostics[0]
		if diagnostic.Outcome != wantOutcomes[i] || diagnostic.ConsecutiveTimeouts != wantCounts[i] {
			t.Fatalf("diagnostic %d = %+v, want outcome=%q count=%d", i, diagnostic, wantOutcomes[i], wantCounts[i])
		}
	}
}

func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "'\\''") + "'"
}
