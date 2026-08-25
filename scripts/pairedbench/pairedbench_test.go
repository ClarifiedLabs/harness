package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"harness/internal/config"
	"harness/internal/llm"
	"harness/internal/session"
	"harness/internal/tools"
)

func initPairedbenchTestRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	run := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	run("init", "-q")
	run("config", "user.email", "pairedbench@example.test")
	run("config", "user.name", "Pairedbench Test")
	if err := os.WriteFile(filepath.Join(dir, "seed"), []byte("seed\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run("add", "seed")
	run("commit", "-qm", "test: seed")
	return dir
}

func TestBenchmarkArgsPinConfigModelAndReasoning(t *testing.T) {
	args := benchmarkArgs("provider:model", "/sessions/run", "/isolated/config.json", "auto", "xhigh")
	joined := " " + strings.Join(args, " ") + " "
	for _, want := range []string{
		" -config /isolated/config.json ",
		" -model provider:model ",
		" -reasoning xhigh ",
		" -agent auto ",
		" -session /sessions/run ",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("benchmark args %q missing %q", joined, want)
		}
	}
}

func TestNormalizeBenchmarkReasoning(t *testing.T) {
	for input, want := range map[string]string{"": "medium", "medium": "medium", "XHIGH": "xhigh", "default": "default"} {
		got, err := normalizeBenchmarkReasoning(input)
		if err != nil || got != want {
			t.Errorf("normalizeBenchmarkReasoning(%q) = %q, %v; want %q", input, got, err, want)
		}
	}
	if _, err := normalizeBenchmarkReasoning("impossible"); err == nil {
		t.Fatal("invalid benchmark reasoning was accepted")
	}
}

func TestValidateMetricsModelRejectsAgentOverride(t *testing.T) {
	if err := validateMetricsModel("requested:model", metrics{ModelTarget: "agent:model"}); err == nil || !strings.Contains(err.Error(), "agent:model") {
		t.Fatalf("validateMetricsModel error = %v, want mismatch", err)
	}
	if err := validateMetricsModel("requested:model", metrics{ModelTarget: "requested:model"}); err != nil {
		t.Fatalf("validateMetricsModel matching target: %v", err)
	}
}

func TestCollectMetricsReconstructsDirectedAndLegacyStagnationTraces(t *testing.T) {
	for _, directed := range []bool{false, true} {
		name := "legacy"
		if directed {
			name = "directed"
		}
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			state := session.Session{Messages: []llm.Message{{
				Role: llm.RoleAssistant, Phase: llm.AssistantPhaseFinal,
				Content: []llm.ContentBlock{{Kind: llm.BlockText, Text: "READY"}},
			}}}
			if err := state.Save(dir); err != nil {
				t.Fatal(err)
			}
			for i, step := range stagnationSteps {
				handler := stagnationScoreHandler
				if step.lane == "latency" {
					handler = stagnationLatencyHandler
				}
				direction := ""
				if directed {
					direction = step.scoreDirection
				}
				if err := session.AppendEvent(dir, session.Event{
					Type: session.EventEvaluatorResult, Prompt: i + 1, Turn: 1,
					EvaluatorResult: &session.EvaluatorResultSnapshot{
						Handler: handler, Accepted: step.accepted, Score: step.score,
						ScoreDirection: direction, Candidate: step.candidate,
						RemainingRequirements: step.remainingRequirements,
					},
				}); err != nil {
					t.Fatal(err)
				}
			}
			m, err := collectMetrics(dir)
			if err != nil {
				t.Fatal(err)
			}
			if m.StagnationEvaluatorResults != 12 || m.StagnationEvaluatorRejections != 11 || m.StagnationEvaluatorAccepts != 1 {
				t.Fatalf("evaluator lifecycle = %+v", m)
			}
			if directed && !candidateStagnationTrace(m) {
				t.Fatalf("directed trace = %+v", m)
			}
			if !directed && !legacyStagnationTrace(m) {
				t.Fatalf("legacy trace = %+v", m)
			}
		})
	}
}

func TestCollectMetricsMeasuresStagnationRecoveryAfterDurableNudge(t *testing.T) {
	dir := t.TempDir()
	state := session.Session{Messages: []llm.Message{{
		Role: llm.RoleAssistant, Phase: llm.AssistantPhaseFinal,
		Content: []llm.ContentBlock{{Kind: llm.BlockText, Text: "done"}},
	}}}
	if err := state.Save(dir); err != nil {
		t.Fatal(err)
	}
	score0, score1 := 0.0, 1.0
	remaining1, remaining0 := 1, 0
	evidence := filepath.ToSlash(filepath.Join(stagnationRecoveryFixture, stagnationRecoveryEvidenceDir, stagnationRecoveryEvidenceFile))
	events := []session.Event{
		{Type: session.EventEvaluatorResult, Prompt: 1, Turn: 1, EvaluatorResult: &session.EvaluatorResultSnapshot{Handler: stagnationRecoveryHandler, Score: &score0, ScoreDirection: "maximize", Candidate: "strategy-repeat", RemainingRequirements: &remaining1, EvidenceRef: evidence}},
		{Type: session.EventEvaluatorResult, Prompt: 2, Turn: 1, EvaluatorResult: &session.EvaluatorResultSnapshot{Handler: stagnationRecoveryHandler, Score: &score0, ScoreDirection: "maximize", Candidate: "strategy-repeat", RemainingRequirements: &remaining1, EvidenceRef: evidence}},
		{Type: session.EventEvaluatorResult, Prompt: 3, Turn: 1, EvaluatorResult: &session.EvaluatorResultSnapshot{Handler: stagnationRecoveryHandler, Score: &score0, ScoreDirection: "maximize", Candidate: "strategy-repeat", RemainingRequirements: &remaining1, EvidenceRef: evidence}},
		{Type: session.EventStagnationNudge, Prompt: 3, Turn: 1, StagnationNudge: &session.StagnationNudgeSnapshot{Threshold: 2, Streak: 2}},
		{Type: session.EventToolStart, Prompt: 3, Turn: 2, ToolID: "read", Tool: "read", Input: json.RawMessage(fmt.Sprintf(`{"path":%q}`, evidence))},
		{Type: session.EventToolResult, Prompt: 3, Turn: 2, ToolID: "read", Tool: "read"},
		{Type: session.EventToolStart, Prompt: 3, Turn: 3, ToolID: "edit", Tool: "edit", Input: json.RawMessage(`{"path":".pairedbench-stagnation-recovery/candidate.txt"}`)},
		{Type: session.EventToolResult, Prompt: 3, Turn: 3, ToolID: "edit", Tool: "edit"},
		{Type: session.EventEvaluatorResult, Prompt: 4, Turn: 1, EvaluatorResult: &session.EvaluatorResultSnapshot{Handler: stagnationRecoveryHandler, Accepted: true, Score: &score1, ScoreDirection: "maximize", Candidate: "strategy-alternate-17", RemainingRequirements: &remaining0}},
	}
	for _, event := range events {
		if err := session.AppendEvent(dir, event); err != nil {
			t.Fatal(err)
		}
	}
	m, err := collectMetrics(dir)
	if err != nil {
		t.Fatal(err)
	}
	if m.StagnationNudgeEvents != 1 || m.RecoveryEvaluatorResults != 4 || m.RecoveryEvaluatorRejections != 3 || m.RecoveryEvaluatorAccepts != 1 {
		t.Fatalf("recovery lifecycle metrics = %+v", m)
	}
	if m.RecoveryToolCallsBeforeNudge != 0 || m.RecoveryToolCallsAfterNudge != 2 || !m.RecoveryAccepted || !m.RecoveryAcceptedAfterNudge || m.RecoveryFailures != 0 {
		t.Fatalf("recovery ordering metrics = %+v", m)
	}
	if m.MaxNoImprovementStreak != 2 {
		t.Fatalf("recovery projection metrics = %+v", m)
	}
}

func TestBenchmarkEnvIsolatesGoCache(t *testing.T) {
	t.Setenv("GOCACHE", "/host/shared-cache")
	isolated := filepath.Join(t.TempDir(), "go-cache")
	env := benchmarkEnv(isolated)
	var got []string
	for _, item := range env {
		if strings.HasPrefix(item, "GOCACHE=") {
			got = append(got, item)
		}
	}
	if len(got) != 1 || got[0] != "GOCACHE="+isolated {
		t.Fatalf("GOCACHE entries = %v, want one isolated entry", got)
	}
}

func TestBenchmarkEnvDisablesIntegrations(t *testing.T) {
	t.Setenv("HARNESS_LSP_SERENA_ENABLE", "true")
	env := benchmarkEnv("")
	for _, key := range []string{"HARNESS_MCP_ENABLE", "HARNESS_LSP_ENABLE", "HARNESS_LSP_SERENA_ENABLE"} {
		if got := envValue(env, key); got != "false" {
			t.Fatalf("%s = %q, want \"false\"", key, got)
		}
	}
}

func TestBenchmarkEnvPrependsOpaqueEvaluatorCommand(t *testing.T) {
	t.Setenv("PATH", "/usr/bin")
	helper := filepath.Join(t.TempDir(), "bin")
	env := benchmarkEnv("", helper)
	want := helper + string(os.PathListSeparator) + "/usr/bin"
	if got := envValue(env, "PATH"); got != want {
		t.Fatalf("PATH = %q, want %q", got, want)
	}
}

func envValue(env []string, key string) string {
	prefix := key + "="
	for _, item := range env {
		if strings.HasPrefix(item, prefix) {
			return strings.TrimPrefix(item, prefix)
		}
	}
	return ""
}

func TestDryRunAlternatesPairs(t *testing.T) {
	c := allCases()["search_context"]
	records := dryRunRecords(runConfig{
		Case:         c,
		BaselineSHA:  "before",
		CandidateSHA: "after",
		Models:       []string{"model"},
		Repetitions:  3,
	})
	got := make([]string, 0, len(records))
	for _, record := range records {
		got = append(got, record.Variant)
	}
	want := []string{"baseline", "candidate", "candidate", "baseline", "baseline", "candidate"}
	if len(got) != len(want) {
		t.Fatalf("variants = %v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("variants = %v, want %v", got, want)
		}
	}
}

func TestParallelDryRunShowsRoundOrder(t *testing.T) {
	records := dryRunRecords(runConfig{
		Case:           allCases()["search_context"],
		BaselineSHA:    "before",
		CandidateSHA:   "after",
		Models:         []string{"model-a", "model-b"},
		Repetitions:    2,
		ParallelModels: true,
	})
	var got []string
	for _, record := range records {
		got = append(got, fmt.Sprintf("%s:%d:%s", record.Model, record.Repetition, record.Variant))
	}
	want := []string{
		"model-a:1:baseline", "model-b:1:baseline",
		"model-a:1:candidate", "model-b:1:candidate",
		"model-a:2:candidate", "model-b:2:candidate",
		"model-a:2:baseline", "model-b:2:baseline",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("parallel dry-run order = %v, want %v", got, want)
	}
}

func TestToolAccuracyCasesRegistered(t *testing.T) {
	cases := allCases()
	for _, name := range []string{"edit_precision", "edit_drift_recovery", "known_path_batching", "unknown_path_discovery"} {
		c, ok := cases[name]
		if !ok || c.Setup == nil || c.Score == nil {
			t.Fatalf("case %q = %+v", name, c)
		}
	}
	if cases["edit_drift_recovery"].SecondPrompt == "" || cases["edit_drift_recovery"].BetweenPrompts == nil {
		t.Fatal("drift case is not configured as a two-phase run")
	}
	knownPrompt := cases["known_path_batching"].Prompt
	if !strings.Contains(knownPrompt, "Use argv-form rg calls through shell") ||
		!strings.Contains(knownPrompt, "Independent repository lookups may be issued together") {
		t.Fatalf("known-path prompt lacks valid directory-scoped search guidance: %s", knownPrompt)
	}
	for _, count := range []int{2, 8, 18, 36, 72} {
		name := fmt.Sprintf("read_scale_%03d", count)
		c, ok := cases[name]
		if !ok || c.Setup == nil || c.Score == nil || !strings.Contains(c.Prompt, fmt.Sprintf("item-%03d.txt", count)) {
			t.Fatalf("scale case %q = %+v", name, c)
		}
	}
}

func TestRetiredExperimentalCasesAreNotRegistered(t *testing.T) {
	cases := allCases()
	for _, name := range []string{"trajectory_memory", "conditional_supervisor"} {
		if _, ok := cases[name]; ok {
			t.Errorf("retired case %q remains registered", name)
		}
	}
}

func TestStagnationDetectionCaseUsesIdenticalToolFreeVariants(t *testing.T) {
	c := allCases()["stagnation_detection"]
	if c.Setup == nil || c.Score == nil || c.Acceptance != acceptanceStagnation || !c.RestartBetweenPrompts || c.HelperCommand != stagnationEvaluatorCommand || len(c.RestartPhases) != stagnationPhaseCount {
		t.Fatalf("stagnation case = %+v", c)
	}
	for i, phase := range c.RestartPhases {
		if phase.Prompt != stagnationPrompt(i+1) || !strings.Contains(phase.Prompt, "READY") || !strings.Contains(phase.Prompt, "without calling any tool") {
			t.Fatalf("phase %d = %+v", i+1, phase)
		}
	}
	baseline, candidate := c.variant("baseline"), c.variant("candidate")
	if baseline.Agent != "independent" || candidate.Agent != "independent" || !baseline.Helper || !candidate.Helper || baseline.Config != candidate.Config {
		t.Fatalf("stagnation variants = baseline %+v candidate %+v", baseline, candidate)
	}
	if variantPromptDigest(c, "baseline") != variantPromptDigest(c, "candidate") {
		t.Fatal("identical stagnation variants have different prompt contracts")
	}
	for _, want := range []string{stagnationScoreHandler, stagnationLatencyHandler, stagnationEvaluatorCommand + " score", stagnationEvaluatorCommand + " latency"} {
		if !strings.Contains(candidate.Config, want) {
			t.Fatalf("stagnation config lacks %q: %s", want, candidate.Config)
		}
	}
	var decoded map[string]any
	if err := json.Unmarshal([]byte(candidate.Config), &decoded); err != nil {
		t.Fatalf("stagnation config is invalid JSON: %v", err)
	}
}

func TestRunStagnationEvaluatorEmitsOneDeterministicResultPerPhase(t *testing.T) {
	dir := t.TempDir()
	root := filepath.Join(dir, stagnationFixture)
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, stagnationStateFile), []byte("0\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for phase, want := range stagnationSteps {
		var outputs []stagnationEvaluatorOutput
		for _, lane := range []string{"score", "latency"} {
			payload := fmt.Sprintf(`{"prompt_id":%d,"can_block":true}`, phase+1)
			var stdout bytes.Buffer
			if code := runStagnationEvaluator(dir, []string{lane}, strings.NewReader(payload), &stdout); code != 0 {
				t.Fatalf("phase %d lane %s exit = %d", phase+1, lane, code)
			}
			if strings.TrimSpace(stdout.String()) == "" {
				continue
			}
			var got stagnationEvaluatorOutput
			if err := json.Unmarshal(stdout.Bytes(), &got); err != nil {
				t.Fatalf("phase %d lane %s output: %v", phase+1, lane, err)
			}
			outputs = append(outputs, got)
		}
		if len(outputs) != 1 {
			t.Fatalf("phase %d outputs = %+v", phase+1, outputs)
		}
		got := outputs[0]
		if got.Accepted != want.accepted || !reflect.DeepEqual(got.Score, want.score) || got.ScoreDirection != want.scoreDirection || got.Candidate != want.candidate || !reflect.DeepEqual(got.RemainingRequirements, want.remainingRequirements) {
			t.Fatalf("phase %d output = %+v, want %+v", phase+1, got, want)
		}
		if (got.Reason == "") != got.Accepted {
			t.Fatalf("phase %d accepted=%t reason=%q", phase+1, got.Accepted, got.Reason)
		}
	}
	state, err := os.ReadFile(filepath.Join(dir, stagnationFixture, stagnationStateFile))
	if err != nil || string(state) != "12\n" {
		t.Fatalf("final helper state = %q, %v", state, err)
	}
	var stdout bytes.Buffer
	if code := runStagnationEvaluator(dir, []string{"latency"}, strings.NewReader(`{"prompt_id":12,"can_block":false}`), &stdout); code != 0 || stdout.Len() != 0 {
		t.Fatalf("non-blockable evaluator = exit %d output %q", code, stdout.String())
	}
}

func TestStagnationRecoveryCaseDiffersOnlyByOptInSwitch(t *testing.T) {
	c := allCases()["stagnation_recovery"]
	if c.Setup == nil || c.Score == nil || c.Acceptance != acceptanceStagnationRecovery || !c.RestartBetweenPrompts || c.HelperCommand != stagnationRecoveryCommand || len(c.RestartPhases) != stagnationRecoveryPhaseCount {
		t.Fatalf("stagnation recovery case = %+v", c)
	}
	for i, phase := range c.RestartPhases {
		if phase.Prompt != stagnationRecoveryPrompt(i+1) || !strings.Contains(phase.Prompt, "READY") || !strings.Contains(phase.Prompt, "[host strategy reset]") {
			t.Fatalf("phase %d = %+v", i+1, phase)
		}
	}
	baseline, candidate := c.variant("baseline"), c.variant("candidate")
	if baseline.Agent != "independent" || candidate.Agent != "independent" || !baseline.Helper || !candidate.Helper {
		t.Fatalf("recovery variants = baseline %+v candidate %+v", baseline, candidate)
	}
	var baselineConfig, candidateConfig map[string]any
	if err := json.Unmarshal([]byte(baseline.Config), &baselineConfig); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal([]byte(candidate.Config), &candidateConfig); err != nil {
		t.Fatal(err)
	}
	if baselineConfig["stagnation_nudge"] != false || candidateConfig["stagnation_nudge"] != true {
		t.Fatalf("recovery switches = baseline %v candidate %v", baselineConfig["stagnation_nudge"], candidateConfig["stagnation_nudge"])
	}
	delete(baselineConfig, "stagnation_nudge")
	delete(candidateConfig, "stagnation_nudge")
	if !reflect.DeepEqual(baselineConfig, candidateConfig) {
		t.Fatalf("recovery configs differ beyond stagnation_nudge:\nbaseline=%v\ncandidate=%v", baselineConfig, candidateConfig)
	}
	if variantPromptDigest(c, "baseline") == variantPromptDigest(c, "candidate") {
		t.Fatal("recovery variants have the same contract digest")
	}
	for name, variant := range map[string]benchmarkVariant{"baseline": baseline, "candidate": candidate} {
		path := filepath.Join(t.TempDir(), "config.json")
		if err := os.WriteFile(path, []byte(variant.Config), 0o600); err != nil {
			t.Fatal(err)
		}
		resolved, err := config.Load(config.LoadOptions{
			Args: []string{"-config", path}, WorkingDir: t.TempDir(),
			LookupEnv: func(string) (string, bool) { return "", false },
		})
		if err != nil {
			t.Fatalf("load %s recovery config: %v", name, err)
		}
		if resolved.Config.StagnationNudge != (name == "candidate") {
			t.Fatalf("resolved %s config = %+v", name, resolved.Config)
		}
	}
}

func TestRunStagnationRecoveryEvaluatorRequiresFourthPhaseRepair(t *testing.T) {
	dir := t.TempDir()
	root := filepath.Join(dir, stagnationRecoveryFixture)
	if err := os.MkdirAll(filepath.Join(root, stagnationRecoveryEvidenceDir), 0o755); err != nil {
		t.Fatal(err)
	}
	for path, content := range map[string]string{
		stagnationRecoveryCandidateFile: stagnationRecoveryInitial,
		stagnationRecoveryStateFile:     "0\n",
		filepath.Join(stagnationRecoveryEvidenceDir, stagnationRecoveryEvidenceFile): stagnationRecoveryEvidence,
	} {
		if err := os.WriteFile(filepath.Join(root, path), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	for phase := 1; phase <= stagnationRecoveryPhaseCount; phase++ {
		if phase == stagnationRecoveryPhaseCount {
			if err := os.WriteFile(filepath.Join(root, stagnationRecoveryCandidateFile), []byte(stagnationRecoveryFinal), 0o600); err != nil {
				t.Fatal(err)
			}
		}
		payload := fmt.Sprintf(`{"prompt_id":%d,"can_block":true}`, phase)
		var stdout bytes.Buffer
		if code := runStagnationRecoveryEvaluator(dir, strings.NewReader(payload), &stdout); code != 0 {
			t.Fatalf("phase %d exit = %d", phase, code)
		}
		var got stagnationRecoveryOutput
		if err := json.Unmarshal(stdout.Bytes(), &got); err != nil {
			t.Fatalf("phase %d output: %v", phase, err)
		}
		wantAccepted := phase == stagnationRecoveryPhaseCount
		wantScore := 0.0
		if wantAccepted {
			wantScore = 1
		}
		wantRemaining := 0
		if !wantAccepted {
			wantRemaining = 1
		}
		if got.Accepted != wantAccepted || (got.Reason == "") != wantAccepted || got.Score != wantScore || got.RemainingRequirements != wantRemaining {
			t.Fatalf("phase %d output = %+v", phase, got)
		}
	}
	state, err := os.ReadFile(filepath.Join(root, stagnationRecoveryStateFile))
	if err != nil || string(state) != "4\n" {
		t.Fatalf("final helper state = %q, %v", state, err)
	}
	var stdout bytes.Buffer
	if code := runStagnationRecoveryEvaluator(dir, strings.NewReader(`{"prompt_id":4,"can_block":false}`), &stdout); code != 0 || stdout.Len() != 0 {
		t.Fatalf("non-blockable recovery evaluator = exit %d output %q", code, stdout.String())
	}
}

func TestScoreStagnationRecoveryRequiresExactPostNudgeRepair(t *testing.T) {
	dir := t.TempDir()
	cmd := exec.Command("git", "init", "-q")
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git init: %v: %s", err, out)
	}
	if err := setupStagnationRecovery(dir); err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(dir, stagnationRecoveryFixture)
	if err := os.WriteFile(filepath.Join(root, stagnationRecoveryCandidateFile), []byte(stagnationRecoveryFinal), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, stagnationRecoveryStateFile), []byte("4\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	valid := scoreInput{Worktree: dir, Metrics: metrics{
		StagnationNudgeEvents:    1,
		RecoveryEvaluatorResults: 4, RecoveryEvaluatorRejections: 3, RecoveryEvaluatorAccepts: 1,
		RecoveryToolCallsAfterNudge: 2, RecoveryAcceptedAfterNudge: true,
	}}
	if got := scoreStagnationRecovery(valid); !got.Pass {
		t.Fatalf("valid recovery score = %+v", got)
	}
	early := valid
	early.Metrics.RecoveryToolCallsBeforeNudge = 3
	if got := scoreStagnationRecovery(early); !got.Pass {
		t.Fatalf("exact but pre-reset recovery correctness score = %+v", got)
	}
	spontaneous := valid
	spontaneous.Metrics = metrics{
		RecoveryEvaluatorResults: 4, RecoveryEvaluatorRejections: 3, RecoveryEvaluatorAccepts: 1,
		RecoveryToolCallsBeforeNudge: 5, RecoveryAccepted: true,
	}
	if got := scoreStagnationRecovery(spontaneous); !got.Pass {
		t.Fatalf("exact no-nudge baseline recovery score = %+v", got)
	}
	if err := os.WriteFile(filepath.Join(root, stagnationRecoveryEvidenceDir, stagnationRecoveryEvidenceFile), []byte("tampered\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := scoreStagnationRecovery(valid); got.Pass || !containsAnyFold(strings.Join(got.Reasons, "\n"), "evidence was modified") {
		t.Fatalf("tampered evidence score = %+v", got)
	}
}

func TestRunRestartBenchmarkUsesSameSessionAcrossProcesses(t *testing.T) {
	dir := t.TempDir()
	script := filepath.Join(dir, "fake-harness")
	body := "#!/bin/sh\nprintf '%s\\n' \"$*\" >> \"$CALLS\"\nprintf 'ran %s\\n' \"$*\"\n"
	if err := os.WriteFile(script, []byte(body), 0o700); err != nil {
		t.Fatal(err)
	}
	calls := filepath.Join(dir, "calls.txt")
	env := append(os.Environ(), "CALLS="+calls)
	firstArgs := append(benchmarkArgs("provider:model", filepath.Join(dir, "session"), filepath.Join(dir, "config.json"), "auto", "medium"), "-p", "phase one")
	between := 0
	c := benchmarkCase{SecondPrompt: "phase two", BetweenPrompts: func(string) error { between++; return nil }}
	stdout, _, err := runRestartBenchmark(context.Background(), script, firstArgs, env, c, dir, filepath.Join(dir, "session"))
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(calls)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if between != 1 || len(lines) != 2 || !strings.Contains(lines[0], "-session "+filepath.Join(dir, "session")) || !strings.Contains(lines[0], "-p phase one") || !strings.Contains(lines[1], "-resume "+filepath.Join(dir, "session")) || !strings.Contains(lines[1], "-p phase two") {
		t.Fatalf("restart calls=%q between=%d", lines, between)
	}
	if strings.Count(string(stdout), "ran ") != 2 {
		t.Fatalf("restart stdout = %q", stdout)
	}
}

func TestRunRestartBenchmarkRunsExplicitPhaseSequence(t *testing.T) {
	dir := t.TempDir()
	script := filepath.Join(dir, "fake-harness")
	body := "#!/bin/sh\nprintf '%s\\n' \"$*\" >> \"$CALLS\"\nprintf 'ran %s\\n' \"$*\"\n"
	if err := os.WriteFile(script, []byte(body), 0o700); err != nil {
		t.Fatal(err)
	}
	calls := filepath.Join(dir, "calls.txt")
	env := append(os.Environ(), "CALLS="+calls)
	phases := []benchmarkPhase{
		{Prompt: "repair checkpoint"},
		{Prompt: "validate checkpoint"},
		{Prompt: "repair final"},
		{Prompt: "validate final"},
	}
	firstArgs := append(benchmarkArgs("provider:model", filepath.Join(dir, "session"), filepath.Join(dir, "config.json"), "auto", "medium"), "-p", phases[0].Prompt)
	stdout, _, err := runRestartBenchmark(context.Background(), script, firstArgs, env, benchmarkCase{RestartPhases: phases}, dir, filepath.Join(dir, "session"))
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(calls)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) != len(phases) {
		t.Fatalf("restart calls = %q", lines)
	}
	for i, phase := range phases {
		mode := "-resume " + filepath.Join(dir, "session")
		if i == 0 {
			mode = "-session " + filepath.Join(dir, "session")
		}
		if !strings.Contains(lines[i], mode) || !strings.Contains(lines[i], "-p "+phase.Prompt) {
			t.Fatalf("phase %d call = %q, want %q and prompt %q", i+1, lines[i], mode, phase.Prompt)
		}
	}
	if strings.Count(string(stdout), "ran ") != len(phases) {
		t.Fatalf("restart stdout = %q", stdout)
	}
}

func TestRunInteractiveBenchmarkUsesPromptBoundaryHook(t *testing.T) {
	script := `read first
printf '%s\n' '{"type":"prompt_end","id":"phase-1"}'
read second
printf '%s\n' '{"type":"prompt_end","id":"phase-2"}'
read shutdown
printf '%s\n' '{"type":"run_end","exit_code":0}'`
	called := 0
	c := benchmarkCase{Prompt: "plan", SecondPrompt: "apply", BetweenPrompts: func(string) error { called++; return nil }}
	stdout, _, err := runInteractiveBenchmark(exec.Command("sh", "-c", script), c, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if called != 1 {
		t.Fatalf("hook calls = %d", called)
	}
	if !strings.Contains(string(stdout), `"id":"phase-2"`) {
		t.Fatalf("stdout = %s", stdout)
	}
}

func TestRunInteractiveBenchmarkWaitsForStderrReader(t *testing.T) {
	script := `read first
printf '%s\n' '{"type":"prompt_end","id":"phase-1"}'
read second
printf '%s\n' '{"type":"prompt_end","id":"phase-2"}'
read shutdown
printf '%s\n' '{"type":"run_end","exit_code":0}'
exec 1>&-
sleep 0.05 &`
	c := benchmarkCase{Prompt: "plan", SecondPrompt: "apply"}
	if _, _, err := runInteractiveBenchmark(exec.Command("sh", "-c", script), c, t.TempDir()); err != nil {
		t.Fatal(err)
	}
}

func TestFlattenJSONStrings(t *testing.T) {
	got := flattenJSONStrings(json.RawMessage(`{"steps":[{"argv":["go","test","./..."]}],"stop_on_failure":true}`))
	joined := ""
	for _, value := range got {
		joined += value + " "
	}
	for _, want := range []string{"go", "test", "./..."} {
		if !contains(got, want) {
			t.Fatalf("flattened strings %q missing %q", joined, want)
		}
	}
}

func TestKnownPathContractEvidenceRequiresExactShellRGInputsAndSuccess(t *testing.T) {
	root := ".pairedbench-tool-accuracy/known"
	validSearches := []string{
		`{"argv":["rg","-n","Widget\\(","` + root + `"]}`,
		`{"argv":["rg","--fixed-strings","State{","` + root + `"]}`,
		`{"argv":["rg","Marker[0-9]+","` + root + `"]}`,
	}
	validCommand := `{"steps":[{"argv":["printf","STEP_ALPHA\n"]},{"argv":["printf","STEP_BETA\n"]}],"output_mode":"full"}`
	evidence := func(searches []string, failedSearch int, commandInput string, commandError bool) (int, int) {
		var events []session.Event
		for i, input := range searches {
			id := fmt.Sprintf("search-%d", i)
			events = append(events,
				session.Event{Type: session.EventToolStart, ToolID: id, Tool: "shell", Input: json.RawMessage(input)},
				session.Event{Type: session.EventToolResult, ToolID: id, Tool: "shell", ResultError: i == failedSearch},
			)
		}
		events = append(events,
			session.Event{Type: session.EventToolStart, ToolID: "command", Tool: "shell", Input: json.RawMessage(commandInput)},
			session.Event{Type: session.EventToolResult, ToolID: "command", Tool: "shell", ResultError: commandError},
		)
		return successfulKnownPathContracts(events)
	}
	if searches, commands := evidence(validSearches, -1, validCommand, false); searches != 3 || commands != 1 {
		t.Fatalf("valid evidence = %d/%d, want 3/1", searches, commands)
	}
	withOutputOnlyFlag := replaceString(validSearches, `"Marker[0-9]+"`, `"-o","Marker[0-9]+"`)
	if searches, _ := evidence(withOutputOnlyFlag, -1, validCommand, false); searches != 3 {
		t.Fatalf("output-only rg flag rejected: %d searches", searches)
	}

	batchedSearches := `{"steps":[{"argv":["rg","-F","Widget(",".pairedbench-tool-accuracy/known"]},{"argv":["rg","-F","State{",".pairedbench-tool-accuracy/known"]},{"argv":["rg","Marker[0-9]+",".pairedbench-tool-accuracy/known"]}]}`
	batchedEvents := []session.Event{
		{Type: session.EventToolStart, ToolID: "searches", Tool: "shell", Input: json.RawMessage(batchedSearches)},
		{Type: session.EventToolResult, ToolID: "searches", Tool: "shell"},
	}
	if searches, _ := successfulKnownPathContracts(batchedEvents); searches != 3 {
		t.Fatalf("batched shell search evidence = %d, want 3", searches)
	}
	failedOutcome := []session.Event{
		{Type: session.EventToolStart, ToolID: "search", Tool: "shell", Input: json.RawMessage(validSearches[0])},
		{Type: session.EventToolResult, ToolID: "search", Tool: "shell", ResultMetrics: map[string]int{
			tools.CommandMetricOutcomeAvailable: 1,
			tools.CommandMetricFailed:           1,
		}},
		{Type: session.EventToolStart, ToolID: "command", Tool: "shell", Input: json.RawMessage(validCommand)},
		{Type: session.EventToolResult, ToolID: "command", Tool: "shell", ResultMetrics: map[string]int{
			tools.CommandMetricOutcomeAvailable: 1,
			tools.CommandMetricCancelled:        1,
		}},
	}
	if searches, commands := successfulKnownPathContracts(failedOutcome); searches != 0 || commands != 0 {
		t.Fatalf("failed shell outcomes counted as evidence: %d/%d", searches, commands)
	}

	tests := []struct {
		name         string
		searches     []string
		failedSearch int
		command      string
		commandError bool
		wantSearch   int
		wantCommand  int
	}{
		{name: "literal regex unescaped", searches: replaceString(validSearches, `Widget\\(`, `Widget(`), failedSearch: -1, command: validCommand, wantSearch: 2, wantCommand: 1},
		{name: "pattern changed", searches: replaceString(validSearches, "Marker[0-9]+", "Marker.*"), failedSearch: -1, command: validCommand, wantSearch: 2, wantCommand: 1},
		{name: "scope broadened", searches: replaceString(validSearches, root, "."), failedSearch: -1, command: validCommand, wantSearch: 0, wantCommand: 1},
		{name: "search execution failed", searches: validSearches, failedSearch: 1, command: validCommand, wantSearch: 2, wantCommand: 1},
		{name: "empty second step", searches: validSearches, failedSearch: -1, command: strings.Replace(validCommand, `{"argv":["printf","STEP_BETA\n"]}`, `{}`, 1), wantSearch: 3},
		{name: "wrong step input", searches: validSearches, failedSearch: -1, command: strings.Replace(validCommand, "STEP_BETA", "STEP_OTHER", 1), wantSearch: 3},
		{name: "compact output", searches: validSearches, failedSearch: -1, command: strings.Replace(validCommand, `"full"`, `"receipt"`, 1), wantSearch: 3},
		{name: "command execution failed", searches: validSearches, failedSearch: -1, command: validCommand, commandError: true, wantSearch: 3},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			searches, commands := evidence(tt.searches, tt.failedSearch, tt.command, tt.commandError)
			if searches != tt.wantSearch || commands != tt.wantCommand {
				t.Fatalf("evidence = %d/%d, want %d/%d", searches, commands, tt.wantSearch, tt.wantCommand)
			}
		})
	}
	commandWithNames := strings.ReplaceAll(validCommand, `{"argv"`, `{"name":"step","argv"`)
	if _, commands := evidence(validSearches, -1, commandWithNames, false); commands != 1 {
		t.Fatalf("cosmetic command step names rejected: %d", commands)
	}
	commandWithHarmlessControls := strings.Replace(validCommand, `{"steps"`, `{"cwd":"/tmp/worktree","stop_on_failure":true,"steps"`, 1)
	if _, commands := evidence(validSearches, -1, commandWithHarmlessControls, false); commands != 1 {
		t.Fatalf("semantically default command controls rejected: %d", commands)
	}
}

func replaceString(values []string, old, new string) []string {
	out := append([]string(nil), values...)
	for i := range out {
		out[i] = strings.ReplaceAll(out[i], old, new)
	}
	return out
}

func TestDiscoveryTargetsFixtureThroughShell(t *testing.T) {
	for _, test := range []struct {
		input string
		want  bool
	}{
		{`{"argv":["rg","--files",".pairedbench-tool-accuracy/discovery"]}`, true},
		{`{"argv":["rg","--hidden","--files","--glob","*.txt",".pairedbench-tool-accuracy/discovery"]}`, true},
		{`{"argv":["find",".pairedbench-tool-accuracy/discovery","-type","f"]}`, true},
		{`{"argv":["find",".pairedbench-tool-accuracy/discovery","-type","f","-name","shard-*-hidden.txt"]}`, true},
		{`{"command":"find .pairedbench-tool-accuracy/discovery -type f | sort"}`, true},
		{`{"command":"find .pairedbench-tool-accuracy/discovery -type f 2>/dev/null | sort"}`, true},
		{`{"argv":["sh","-c","rg --files .pairedbench-tool-accuracy/discovery | sort"]}`, true},
		{`{"argv":["sh","-c","rg --files .pairedbench-tool-accuracy/discovery 2>/dev/null | sort; echo ---; find .pairedbench-tool-accuracy/discovery -type f 2>/dev/null | sort"]}`, true},
		{`{"steps":[{"argv":["rg","--files",".pairedbench-tool-accuracy/discovery"]}]}`, true},
		{`{"argv":["rg","--files","."]}`, false},
		{`{"argv":["rg","Discover",".pairedbench-tool-accuracy/discovery"]}`, false},
		{`{"argv":["find",".pairedbench-tool-accuracy/discovery","-name","missing-*"]}`, false},
		{`{"argv":["rg","--files","--glob","missing-*",".pairedbench-tool-accuracy/discovery"]}`, false},
	} {
		if got := shellDiscoversFixture(json.RawMessage(test.input)); got != test.want {
			t.Errorf("shellDiscoversFixture(%s) = %v, want %v", test.input, got, test.want)
		}
	}
}

func TestReadPathNormalizesAbsoluteFixturePath(t *testing.T) {
	input := json.RawMessage(`{"path":"/tmp/worktree/.pairedbench-tool-accuracy/discovery/shard-01-hidden.txt"}`)
	want := ".pairedbench-tool-accuracy/discovery/shard-01-hidden.txt"
	if got := readPath(input); got != want {
		t.Fatalf("absolute fixture path normalized to %q, want %q", got, want)
	}
}

func TestRemovedTypedSearchEventsDoNotSatisfyKnownPathOracle(t *testing.T) {
	searchInput := json.RawMessage(`{"queries":[{"pattern":"Widget\\(","paths":[".pairedbench-tool-accuracy/known"]},{"pattern":"State\\{","paths":[".pairedbench-tool-accuracy/known"]},{"pattern":"Marker[0-9]+","paths":[".pairedbench-tool-accuracy/known"]}]}`)
	inspectInput := json.RawMessage(`{"operations":[{"tool":"search","input":{"queries":[{"pattern":"Marker[0-9]+","paths":[".pairedbench-tool-accuracy/known"]}]}}]}`)
	for _, tool := range []string{"search", "inspect", "rg", "grep"} {
		input := searchInput
		if tool == "inspect" {
			input = inspectInput
		}
		events := []session.Event{
			{Type: session.EventToolStart, ToolID: "typed", Tool: tool, Input: input},
			{Type: session.EventToolResult, ToolID: "typed", Tool: tool},
		}
		if searches, commands := successfulKnownPathContracts(events); searches != 0 || commands != 0 {
			t.Errorf("removed %s events satisfied known-path oracle: %d searches, %d commands", tool, searches, commands)
		}
	}
}

func TestRemovedTypedDiscoveryEventsDoNotSatisfyDiscoveryOracle(t *testing.T) {
	inputs := map[string]json.RawMessage{
		"glob":     json.RawMessage(`{"root":".pairedbench-tool-accuracy/discovery","pattern":"*.txt"}`),
		"list_dir": json.RawMessage(`{"path":".pairedbench-tool-accuracy/discovery"}`),
		"search":   json.RawMessage(`{"queries":[{"pattern":"Discover","paths":[".pairedbench-tool-accuracy/discovery"],"max_files":18}]}`),
	}
	for tool, input := range inputs {
		event := session.Event{Type: session.EventToolStart, Tool: tool, Input: input}
		if discoveryStartTargetsFixture(event) {
			t.Errorf("removed %s event satisfied discovery oracle", tool)
		}
		if got := repositoryLookupOperationCount(tool, input); got != 0 {
			t.Errorf("removed %s event counted as %d repository lookups", tool, got)
		}
	}
}

func TestSuccessfulDriftRereadRequiresSuccessfulCorrelatedResult(t *testing.T) {
	readInput := json.RawMessage(`{"path":".pairedbench-tool-accuracy/edit-drift.txt"}`)
	tests := []struct {
		name   string
		events []session.Event
		want   bool
	}{
		{
			name: "direct success",
			events: []session.Event{
				{Type: session.EventToolStart, Prompt: 2, ToolID: "read", Tool: "read", Input: readInput},
				{Type: session.EventToolResult, Prompt: 2, ToolID: "read", Tool: "read"},
			},
			want: true,
		},
		{
			name: "direct failure",
			events: []session.Event{
				{Type: session.EventToolStart, Prompt: 2, ToolID: "read", Tool: "read", Input: readInput},
				{Type: session.EventToolResult, Prompt: 2, ToolID: "read", Tool: "read", ResultError: true},
			},
		},
		{
			name: "removed read_file tool",
			events: []session.Event{
				{Type: session.EventToolStart, Prompt: 2, ToolID: "read", Tool: "read_file", Input: readInput},
				{Type: session.EventToolResult, Prompt: 2, ToolID: "read", Tool: "read_file"},
			},
		},
		{
			name: "phase one",
			events: []session.Event{
				{Type: session.EventToolStart, Prompt: 1, ToolID: "read", Tool: "read", Input: readInput},
				{Type: session.EventToolResult, Prompt: 1, ToolID: "read", Tool: "read"},
			},
		},
		{
			name: "uncorrelated success",
			events: []session.Event{
				{Type: session.EventToolStart, Prompt: 2, ToolID: "read", Tool: "read", Input: readInput},
				{Type: session.EventToolResult, Prompt: 2, ToolID: "other", Tool: "read"},
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := successfulDriftReread(tt.events); got != tt.want {
				t.Fatalf("successfulDriftReread = %t, want %t", got, tt.want)
			}
		})
	}
}

func TestValidateArchivedRecordChecksContractAndEvents(t *testing.T) {
	c := allCases()["edit_drift_recovery"]
	dir := t.TempDir()
	raw := []byte("events\n")
	if err := os.WriteFile(filepath.Join(dir, "raw.ndjson"), raw, 0o600); err != nil {
		t.Fatal(err)
	}
	record := runRecord{
		Version: runRecordVersion, Completed: true, SessionDir: dir,
		Reasoning:    "medium",
		PromptSHA256: promptDigest(c), OracleVersion: oracleContractVersion,
		EventsSHA256: digestString(string(raw)), Score: score{Pass: true},
	}
	if err := validateArchivedRecord(record, c, "medium"); err != nil {
		t.Fatalf("valid archived record: %v", err)
	}
	for name, mutate := range map[string]func(*runRecord){
		"version":   func(r *runRecord) { r.Version-- },
		"prompt":    func(r *runRecord) { r.PromptSHA256 = "stale" },
		"oracle":    func(r *runRecord) { r.OracleVersion = "stale" },
		"events":    func(r *runRecord) { r.EventsSHA256 = "stale" },
		"reasoning": func(r *runRecord) { r.Reasoning = "xhigh" },
	} {
		t.Run(name, func(t *testing.T) {
			bad := record
			mutate(&bad)
			if err := validateArchivedRecord(bad, c, "medium"); err == nil {
				t.Fatal("invalid archived record was accepted")
			}
		})
	}
}

func TestValidateArchivedRecordChecksVariantAgentAndConfig(t *testing.T) {
	c := allCases()["stagnation_recovery"]
	dir := t.TempDir()
	raw := []byte("events\n")
	if err := os.WriteFile(filepath.Join(dir, "raw.ndjson"), raw, 0o600); err != nil {
		t.Fatal(err)
	}
	variant := c.variant("candidate")
	record := runRecord{
		Version: runRecordVersion, Completed: true, Variant: "candidate", SessionDir: dir,
		Reasoning: "medium",
		Agent:     variant.Agent, ConfigSHA256: digestString(variant.Config),
		PromptSHA256: variantPromptDigest(c, "candidate"), OracleVersion: oracleContractVersion,
		EventsSHA256: digestString(string(raw)), Score: score{Pass: true},
	}
	if err := validateArchivedRecord(record, c, "medium"); err != nil {
		t.Fatalf("valid custom-variant record: %v", err)
	}
	if _, got := evaluateArchivedCase(c, scoreInput{}, record.Score); !got.Pass {
		t.Fatalf("archived exact-oracle score was recomputed without its removed worktree: %+v", got)
	}
	badAgent := record
	badAgent.Agent = "auto"
	if err := validateArchivedRecord(badAgent, c, "medium"); err == nil || !strings.Contains(err.Error(), "agent") {
		t.Fatalf("agent mismatch error = %v", err)
	}
	badConfig := record
	badConfig.ConfigSHA256 = "stale"
	if err := validateArchivedRecord(badConfig, c, "medium"); err == nil || !strings.Contains(err.Error(), "config hash") {
		t.Fatalf("config mismatch error = %v", err)
	}
}

func TestShellInvokesGit(t *testing.T) {
	for _, input := range []string{
		`{"argv":["git","status","--short"]}`,
		`{"steps":[{"command":"git status --short"},{"command":"git diff --stat"}]}`,
	} {
		if !shellInvokesGit(json.RawMessage(input)) {
			t.Fatalf("git invocation not detected in %s", input)
		}
	}
	if shellInvokesGit(json.RawMessage(`{"command":"printf 'git status'"}`)) {
		t.Fatal("quoted git text counted as an invocation")
	}
}

func TestSetupWorkBugAndWorkspaceDigest(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git unavailable")
	}
	dir := t.TempDir()
	run := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	run("init", "-q")
	run("config", "user.email", "test@example.com")
	run("config", "user.name", "Test")
	if err := os.MkdirAll(filepath.Join(dir, "internal", "todo"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "internal", "todo", "todo.go"), []byte("func x() {\n\t"+todoBugOld+"\n\t}\n}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("readme\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run("add", ".")
	run("commit", "-qm", "initial")
	if err := setupGitWorkspace(dir); err != nil {
		t.Fatal(err)
	}
	readme, err := os.ReadFile(filepath.Join(dir, "README.md"))
	if err != nil || !strings.HasSuffix(string(readme), "\nPaired benchmark workspace note.  \n") {
		t.Fatalf("workspace README note = %q, %v", readme, err)
	}
	note, err := os.ReadFile(filepath.Join(dir, "pairedbench-note.txt"))
	if err != nil || string(note) != "untracked pairedbench note\n" {
		t.Fatalf("workspace untracked note = %q, %v", note, err)
	}
	first, err := fixtureDigest(dir)
	if err != nil {
		t.Fatal(err)
	}
	second, err := fixtureDigest(dir)
	if err != nil {
		t.Fatal(err)
	}
	if first != second {
		t.Fatalf("fixture digest unstable: %s != %s", first, second)
	}
}

func TestSummarizeAcceptance(t *testing.T) {
	c := allCases()["search_context"]
	var records []runRecord
	for _, model := range defaultModels {
		for rep := 1; rep <= 3; rep++ {
			records = append(records,
				runRecord{Model: model, Variant: "baseline", Score: score{Pass: true}, Metrics: metrics{TotalTokens: 100, RGToReadTransitions: 2}},
				runRecord{Model: model, Variant: "candidate", Score: score{Pass: true}, Metrics: metrics{TotalTokens: 70, RGToReadTransitions: 0, UsedSearch: true}},
			)
		}
	}
	agg := summarize(c, records)
	if !agg.Accepted {
		t.Fatalf("aggregate rejected: %v", agg.Failures)
	}
}

func TestStagnationAcceptanceRequiresBothProjectionOracles(t *testing.T) {
	c := allCases()["stagnation_detection"]
	legacy := metrics{
		TotalTokens: 100, Turns: 23,
		MaxNoImprovementStreak: 2,
		StagnationBaselines:    3, StagnationImprovements: 1,
		StagnationPlateaus: 2, StagnationRegressions: 1,
		StagnationIndeterminate: 5, UnorderedScoreEvaluations: 8,
		StagnationLaneResets: 1,
	}
	directed := metrics{
		TotalTokens: 250, Turns: 30,
		MaxNoImprovementStreak: 2,
		StagnationBaselines:    3, StagnationImprovements: 3,
		StagnationPlateaus: 2, StagnationRegressions: 3,
		StagnationIndeterminate: 1, StagnationLaneResets: 1,
	}
	records := []runRecord{
		{Model: "provider:model", Repetition: 1, Variant: "baseline", Score: score{Pass: true}, Metrics: legacy},
		{Model: "provider:model", Repetition: 1, Variant: "candidate", Score: score{Pass: true}, Metrics: directed},
	}
	agg := summarize(c, records)
	if !agg.Accepted || agg.BaselineLegacyStagnationRuns != 1 || agg.CandidateStagnationDetections != 1 {
		t.Fatalf("stagnation aggregate = %+v", agg)
	}

	brokenCandidate := append([]runRecord(nil), records...)
	brokenCandidate[1].Metrics.StagnationRegressions = 2
	if rejected := summarize(c, brokenCandidate); rejected.Accepted || !strings.Contains(strings.Join(rejected.Failures, "\n"), "candidate detection coverage") {
		t.Fatalf("broken candidate trace was accepted: %+v", rejected)
	}

	brokenBaseline := append([]runRecord(nil), records...)
	brokenBaseline[0].Metrics.UnorderedScoreEvaluations = 7
	if rejected := summarize(c, brokenBaseline); rejected.Accepted || !strings.Contains(strings.Join(rejected.Failures, "\n"), "baseline projection coverage") {
		t.Fatalf("broken baseline trace was accepted: %+v", rejected)
	}
}

func TestStagnationRecoveryAcceptanceRequiresOneShotPostNudgeRecovery(t *testing.T) {
	c := allCases()["stagnation_recovery"]
	var records []runRecord
	for rep := 1; rep <= 3; rep++ {
		records = append(records,
			runRecord{Model: "provider:model", Repetition: rep, Variant: "baseline", Score: score{Pass: false}, Metrics: metrics{
				TotalTokens: 100, Turns: 8, RecoveryFailures: 1,
			}},
			runRecord{Model: "provider:model", Repetition: rep, Variant: "candidate", Score: score{Pass: true}, Metrics: metrics{
				TotalTokens: 150, Turns: 9, StagnationNudgeEvents: 1,
				RecoveryToolCallsAfterNudge: 2, RecoveryAcceptedAfterNudge: true,
			}},
		)
	}
	agg := summarize(c, records)
	if !agg.Accepted || agg.BaselinePasses != 0 || agg.CandidatePasses != 3 || agg.CandidateRecoveryNudgeRuns != 3 || agg.CandidateRecoveriesAfterNudge != 3 || agg.CandidateResetDrivenRecoveries != 3 || agg.Adoptions != 3 || agg.PrimaryReductionPct != 100 {
		t.Fatalf("stagnation recovery aggregate = %+v", agg)
	}

	missingNudge := append([]runRecord(nil), records...)
	missingNudge[1].Metrics.StagnationNudgeEvents = 0
	if rejected := summarize(c, missingNudge); rejected.Accepted || !strings.Contains(strings.Join(rejected.Failures, "\n"), "strategy-reset coverage") {
		t.Fatalf("missing candidate nudge was accepted: %+v", rejected)
	}

	unexpectedBaseline := append([]runRecord(nil), records...)
	unexpectedBaseline[0].Metrics.StagnationNudgeEvents = 1
	if rejected := summarize(c, unexpectedBaseline); rejected.Accepted || !strings.Contains(strings.Join(rejected.Failures, "\n"), "baseline runs unexpectedly") {
		t.Fatalf("unexpected baseline nudge was accepted: %+v", rejected)
	}

	earlyCandidate := append([]runRecord(nil), records...)
	earlyCandidate[1].Metrics.RecoveryToolCallsBeforeNudge = 1
	if rejected := summarize(c, earlyCandidate); rejected.Accepted || !strings.Contains(strings.Join(rejected.Failures, "\n"), "clean reset-driven recovery coverage") {
		t.Fatalf("pre-reset candidate work was accepted: %+v", rejected)
	}

	tiedRecovery := append([]runRecord(nil), records...)
	for i := range tiedRecovery {
		if tiedRecovery[i].Variant == "baseline" {
			tiedRecovery[i].Score.Pass = true
			tiedRecovery[i].Metrics.RecoveryFailures = 0
		}
	}
	if rejected := summarize(c, tiedRecovery); rejected.Accepted || !strings.Contains(strings.Join(rejected.Failures, "\n"), "did not improve over baseline") {
		t.Fatalf("tied baseline recovery was accepted: %+v", rejected)
	}
}

func TestStagnationRecoveryAcceptanceAllowsOneCleanAdoptionMissInNine(t *testing.T) {
	c := allCases()["stagnation_recovery"]
	var records []runRecord
	for model := 1; model <= 3; model++ {
		for rep := 1; rep <= 3; rep++ {
			candidateMetrics := metrics{
				TotalTokens: 150, Turns: 9, StagnationNudgeEvents: 1,
				RecoveryToolCallsAfterNudge: 2, RecoveryAcceptedAfterNudge: true,
			}
			if model == 1 && rep == 1 {
				candidateMetrics.RecoveryToolCallsBeforeNudge = 1
			}
			modelName := fmt.Sprintf("provider:model-%d", model)
			records = append(records,
				runRecord{Model: modelName, Repetition: rep, Variant: "baseline", Score: score{Pass: false}, Metrics: metrics{
					TotalTokens: 100, Turns: 8, RecoveryFailures: 1,
				}},
				runRecord{Model: modelName, Repetition: rep, Variant: "candidate", Score: score{Pass: true}, Metrics: candidateMetrics},
			)
		}
	}

	agg := summarize(c, records)
	if !agg.Accepted || agg.CandidateResetDrivenRecoveries != 8 || agg.Adoptions != 8 {
		t.Fatalf("stagnation recovery aggregate = %+v", agg)
	}
}

func TestStagnationRecoveryAcceptanceAllowsOnePostResetRecoveryMissInNine(t *testing.T) {
	c := allCases()["stagnation_recovery"]
	var records []runRecord
	for model := 1; model <= 3; model++ {
		for rep := 1; rep <= 3; rep++ {
			candidatePass := true
			candidateMetrics := metrics{
				TotalTokens: 150, Turns: 9, StagnationNudgeEvents: 1,
				RecoveryToolCallsAfterNudge: 2, RecoveryAcceptedAfterNudge: true,
			}
			if model == 1 && rep == 1 {
				candidatePass = false
				candidateMetrics.RecoveryToolCallsAfterNudge = 0
				candidateMetrics.RecoveryAcceptedAfterNudge = false
				candidateMetrics.RecoveryFailures = 1
			}
			modelName := fmt.Sprintf("provider:model-%d", model)
			records = append(records,
				runRecord{Model: modelName, Repetition: rep, Variant: "baseline", Score: score{Pass: false}, Metrics: metrics{
					TotalTokens: 100, Turns: 8, RecoveryFailures: 1,
				}},
				runRecord{Model: modelName, Repetition: rep, Variant: "candidate", Score: score{Pass: candidatePass}, Metrics: candidateMetrics},
			)
		}
	}

	agg := summarize(c, records)
	if !agg.Accepted || agg.CandidatePasses != 8 || agg.CandidateRecoveriesAfterNudge != 8 || agg.CandidateResetDrivenRecoveries != 8 || agg.Adoptions != 8 {
		t.Fatalf("stagnation recovery aggregate = %+v", agg)
	}
}

func TestToolAccuracyAcceptanceRequiresPositiveEfficiencyAndErrorReduction(t *testing.T) {
	c := allCases()["known_path_batching"]
	var records []runRecord
	for _, model := range defaultModels {
		for rep := 1; rep <= 3; rep++ {
			records = append(records,
				runRecord{Model: model, Repetition: rep, Variant: "baseline", Score: score{Pass: true}, Metrics: metrics{TotalTokens: 100, Turns: 4, ToolErrors: 2}},
				runRecord{Model: model, Repetition: rep, Variant: "candidate", Score: score{Pass: true}, Metrics: metrics{TotalTokens: 90, Turns: 4, ToolErrors: 1, ToolCalls: map[string]int{"read": 2, "shell": 4}, ExactKnownPathSearches: 3, ExactKnownPathCommands: 1, CoissuedReadTurns: 1, CoissuedLookupTurns: 1}},
			)
		}
	}
	agg := summarize(c, records)
	if !agg.Accepted {
		t.Fatalf("tool accuracy aggregate rejected: %v", agg.Failures)
	}
	for i := range records {
		if records[i].Variant == "candidate" {
			records[i].Metrics.TotalTokens = 101
		}
	}
	agg = summarize(c, records)
	if agg.Accepted || !containsAnyFold(strings.Join(agg.Failures, "\n"), "tokens did not decrease") {
		t.Fatalf("aggregate token regression was not rejected: %+v", agg)
	}
	for i := range records {
		if records[i].Variant == "candidate" {
			records[i].Metrics.TotalTokens = 90
			records[i].Metrics.ToolErrors = 3
		}
	}
	agg = summarize(c, records)
	if agg.Accepted || !containsAnyFold(strings.Join(agg.Failures, "\n"), "tool errors increased", "reduction") {
		t.Fatalf("error regression was not rejected: %+v", agg)
	}
}

func TestRecoveredEditMissClassification(t *testing.T) {
	tests := []struct {
		name           string
		events         []session.Event
		scorePass      bool
		wantRecovered  int
		wantUnresolved int
		wantUnrelated  int
		wantEffective  int
	}{
		{
			name: "recovered",
			events: []session.Event{
				{Type: session.EventToolResult, Tool: "edit", Turn: 1, ResultError: true, ErrorKind: string(llm.ToolErrorEditOldTextNotFound)},
				{Type: session.EventToolResult, Tool: "edit", Turn: 2},
			},
			scorePass: true, wantRecovered: 1, wantEffective: 0,
		},
		{
			name: "unresolved",
			events: []session.Event{
				{Type: session.EventToolResult, Tool: "edit", Turn: 1, ResultError: true, ErrorKind: string(llm.ToolErrorEditOldTextNotFound)},
			},
			scorePass: true, wantUnresolved: 1, wantEffective: 1,
		},
		{
			name: "timely recovery plus unresolved miss",
			events: []session.Event{
				{Type: session.EventToolResult, Tool: "edit", Turn: 1, ResultError: true, ErrorKind: string(llm.ToolErrorEditOldTextNotFound)},
				{Type: session.EventToolResult, Tool: "edit", Turn: 2},
				{Type: session.EventToolResult, Tool: "edit", Turn: 3, ResultError: true, ErrorKind: string(llm.ToolErrorEditOldTextNotFound)},
			},
			scorePass: true, wantRecovered: 1, wantUnresolved: 1, wantEffective: 1,
		},
		{
			name: "over budget",
			events: []session.Event{
				{Type: session.EventToolResult, Tool: "edit", Turn: 1, ResultError: true, ErrorKind: string(llm.ToolErrorEditOldTextNotFound)},
				{Type: session.EventToolResult, Tool: "edit", Turn: 4},
			},
			scorePass: true, wantRecovered: 1, wantEffective: 1,
		},
		{
			name: "oracle failure",
			events: []session.Event{
				{Type: session.EventToolResult, Tool: "edit", Turn: 1, ResultError: true, ErrorKind: string(llm.ToolErrorEditOldTextNotFound)},
				{Type: session.EventToolResult, Tool: "edit", Turn: 2},
			},
			wantRecovered: 1, wantEffective: 1,
		},
		{
			name: "unrelated nested errors remain",
			events: []session.Event{
				{Type: session.EventToolResult, Tool: "edit", Turn: 1, ResultError: true, ErrorKind: string(llm.ToolErrorEditOldTextNotFound)},
				{Type: session.EventToolResult, Tool: "edit", Turn: 2},
				{Type: session.EventToolResult, Tool: "inspect", Turn: 3, ResultError: true, ErrorKind: string(llm.ToolErrorBatchFailed), ResultMetrics: map[string]int{"operation_errors": 2}},
			},
			scorePass: true, wantRecovered: 1, wantUnrelated: 3, wantEffective: 3,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := metrics{ErrorKinds: map[string]int{}}
			recovery := editRecoveryState{}
			for _, event := range tt.events {
				observeToolResult(&m, &recovery, event)
			}
			finishEditRecovery(&m, recovery)
			m = classifyEffectiveToolErrors("edit_drift_recovery", m, score{Pass: tt.scorePass})
			if m.RecoveredEditMisses != tt.wantRecovered || m.UnresolvedEditFailures != tt.wantUnresolved ||
				m.UnrelatedToolErrors != tt.wantUnrelated || m.EffectiveToolErrors != tt.wantEffective {
				t.Fatalf("metrics = %+v", m)
			}
		})
	}
}

func TestRecoveryClassifiesPendingMissesIndividually(t *testing.T) {
	m := metrics{ErrorKinds: map[string]int{}}
	recovery := editRecoveryState{}
	for _, event := range []session.Event{
		{Type: session.EventToolResult, Tool: "edit", Turn: 1, ResultError: true, ErrorKind: string(llm.ToolErrorEditOldTextNotFound)},
		{Type: session.EventToolResult, Tool: "edit", Turn: 2, ResultError: true, ErrorKind: string(llm.ToolErrorEditOldTextNotFound)},
		{Type: session.EventToolResult, Tool: "edit", Turn: 3},
	} {
		observeToolResult(&m, &recovery, event)
	}
	finishEditRecovery(&m, recovery)
	if m.RecoveredEditMisses != 2 || m.TimelyRecoveredEditMisses != 2 || m.EditRecoveryTurns != 2 || m.UnresolvedEditFailures != 0 {
		t.Fatalf("metrics = %+v", m)
	}

	m = metrics{ErrorKinds: map[string]int{}}
	recovery = editRecoveryState{}
	for _, event := range []session.Event{
		{Type: session.EventToolResult, Tool: "edit", Turn: 1, ResultError: true, ErrorKind: string(llm.ToolErrorEditOldTextNotFound)},
		{Type: session.EventToolResult, Tool: "edit", Turn: 3, ResultError: true, ErrorKind: string(llm.ToolErrorEditOldTextNotFound)},
		{Type: session.EventToolResult, Tool: "edit", Turn: 4},
	} {
		observeToolResult(&m, &recovery, event)
	}
	finishEditRecovery(&m, recovery)
	m = classifyEffectiveToolErrors("edit_drift_recovery", m, score{Pass: true})
	if m.RecoveredEditMisses != 2 || m.TimelyRecoveredEditMisses != 1 || m.EffectiveToolErrors != 1 {
		t.Fatalf("mixed-window metrics = %+v", m)
	}
}

func TestEvaluateArchivedEditCaseUsesRecordedOracle(t *testing.T) {
	m := metrics{ToolErrors: 1, RecoverableEditMisses: 1, RecoveredEditMisses: 1, TimelyRecoveredEditMisses: 1}
	gotMetrics, gotScore := evaluateArchivedCase(benchmarkCase{Name: "edit_drift_recovery"}, scoreInput{Metrics: m}, score{Pass: true})
	if !gotScore.Pass || gotMetrics.EffectiveToolErrors != 0 {
		t.Fatalf("archived result = (%+v, %+v)", gotMetrics, gotScore)
	}
}

func TestKnownAndUnknownPathScoresEnforceSeparateFlows(t *testing.T) {
	known := allCases()["known_path_batching"]
	if !strings.Contains(known.Prompt, "contract-01.txt") || !strings.Contains(known.Prompt, "contract-18.txt") {
		t.Fatalf("known-path prompt does not enumerate fixture paths: %s", known.Prompt)
	}
	knownMetrics := metrics{
		ToolCalls:              map[string]int{"read": 1, "shell": 4},
		SuccessfulReadPaths:    contractFixturePaths("known", "contract-%02d.txt"),
		SearchQueries:          3,
		ExactKnownPathSearches: 3,
		ExactKnownPathCommands: 1,
	}
	if got := scoreKnownPathBatching(scoreInput{Stdout: "Marker01 Marker18 STEP_ALPHA STEP_BETA", FixtureBefore: "same", FixtureAfter: "same", Metrics: knownMetrics}); !got.Pass {
		t.Fatalf("known-path score rejected valid flow: %v", got.Reasons)
	}
	knownMetrics.ExactKnownPathSearches = 2
	if got := scoreKnownPathBatching(scoreInput{Stdout: "Marker01 Marker18 STEP_ALPHA STEP_BETA", FixtureBefore: "same", FixtureAfter: "same", Metrics: knownMetrics}); got.Pass {
		t.Fatal("known-path score accepted incomplete searches")
	}
	knownMetrics.ExactKnownPathSearches = 3
	knownMetrics.SuccessfulReadPaths = knownMetrics.SuccessfulReadPaths[:17]
	if got := scoreKnownPathBatching(scoreInput{Stdout: "Marker01 Marker18 STEP_ALPHA STEP_BETA", FixtureBefore: "same", FixtureAfter: "same", Metrics: knownMetrics}); got.Pass {
		t.Fatal("known-path score accepted a missing read path")
	}
	knownMetrics.SuccessfulReadPaths = contractFixturePaths("known", "contract-%02d.txt")
	knownMetrics.ExactKnownPathCommands = 0
	if got := scoreKnownPathBatching(scoreInput{Stdout: "Marker01 Marker18 STEP_ALPHA STEP_BETA", FixtureBefore: "same", FixtureAfter: "same", Metrics: knownMetrics}); got.Pass {
		t.Fatal("known-path score accepted assistant text without exact successful command evidence")
	}

	discoveryPaths := contractFixturePaths("discovery", "shard-%02d-hidden.txt")
	validDiscovery := metrics{
		ToolCalls:           map[string]int{"shell": 1, "read": 1},
		SuccessfulReadPaths: []string{discoveryPaths[0], discoveryPaths[len(discoveryPaths)-1]},
		DiscoveryBeforeRead: true,
	}
	in := scoreInput{Stdout: "Discover01 Discover18", FixtureBefore: "same", FixtureAfter: "same", Metrics: validDiscovery}
	if got := scoreUnknownPathDiscovery(in); !got.Pass {
		t.Fatalf("unknown-path score rejected valid flow: %v", got.Reasons)
	}
	for name, mutate := range map[string]func(*metrics){
		"guessed before discovery": func(m *metrics) { m.ReadBeforeDiscovery = true },
		"missing successful read":  func(m *metrics) { m.SuccessfulReadPaths = m.SuccessfulReadPaths[:1] },
	} {
		t.Run(name, func(t *testing.T) {
			bad := validDiscovery
			mutate(&bad)
			in.Metrics = bad
			if got := scoreUnknownPathDiscovery(in); got.Pass {
				t.Fatalf("invalid discovery flow passed: %+v", bad)
			}
		})
	}
}

func TestReadScaleScoreRequiresEveryReadAndExactFixture(t *testing.T) {
	const count = 8
	paths := readScaleFixturePaths(count)
	valid := scoreInput{
		Stdout:        "Scale001 Scale004 Scale008",
		FixtureBefore: "same",
		FixtureAfter:  "same",
		Metrics:       metrics{SuccessfulDirectReadPaths: paths},
	}
	if got := scoreReadScale(valid, count); !got.Pass {
		t.Fatalf("valid read-scale flow rejected: %v", got.Reasons)
	}
	for name, mutate := range map[string]func(*scoreInput){
		"missing path": func(in *scoreInput) { in.Metrics.SuccessfulDirectReadPaths = paths[:len(paths)-1] },
		"inspect only": func(in *scoreInput) {
			in.Metrics.SuccessfulDirectReadPaths = nil
			in.Metrics.SuccessfulReadPaths = paths
		},
		"missing marker": func(in *scoreInput) { in.Stdout = "Scale001 Scale008" },
		"modified":       func(in *scoreInput) { in.FixtureAfter = "changed" },
	} {
		t.Run(name, func(t *testing.T) {
			bad := valid
			mutate(&bad)
			if got := scoreReadScale(bad, count); got.Pass {
				t.Fatalf("invalid read-scale flow passed: %+v", bad)
			}
		})
	}
}

func TestMinimumCorrectnessPassesScalesToSelectedModels(t *testing.T) {
	for runs, want := range map[int]int{
		0: 0,
		3: 3,
		9: 8,
	} {
		if got := minimumCorrectnessPasses(runs); got != want {
			t.Fatalf("minimumCorrectnessPasses(%d) = %d, want %d", runs, got, want)
		}
	}
}

func TestWriteSummarySeparatesPairedAndUnpairedTokenMetrics(t *testing.T) {
	dir := t.TempDir()
	c := allCases()["search_context"]
	var records []runRecord
	for _, model := range defaultModels {
		for rep := 1; rep <= 3; rep++ {
			records = append(records,
				runRecord{
					Case:       c.Name,
					Model:      model,
					Repetition: rep,
					Variant:    "baseline",
					Score:      score{Pass: true},
					Metrics:    metrics{TotalTokens: 100, RGToReadTransitions: 2},
				},
				runRecord{
					Case:       c.Name,
					Model:      model,
					Repetition: rep,
					Variant:    "candidate",
					Score:      score{Pass: true},
					Metrics:    metrics{TotalTokens: 80, UsedSearch: true},
				},
			)
		}
	}
	if err := writeSummary(dir, c, records); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(dir, c.Name+"-summary.md"))
	if err != nil {
		t.Fatal(err)
	}
	got := string(data)
	if !strings.HasPrefix(got, "# Paired benchmark: "+c.Name+"\n") {
		t.Fatalf("summary heading = %q", strings.SplitN(got, "\n", 2)[0])
	}
	for _, want := range []string{
		"Unpaired median tokens:",
		"Paired-median token saving: 20.0%",
		"token savings [+20.0%, +20.0%, +20.0%] (improved/regressed/tied 3/0/0",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("summary missing %q:\n%s", want, got)
		}
	}
}

func TestSummarizeIncludesDeepSeekReportedCostWithoutPricingFlag(t *testing.T) {
	c := allCases()["search_context"]
	records := []runRecord{
		{
			Model:   "deepseek:deepseek-v4-pro",
			Variant: "baseline",
			Metrics: metrics{CostUSD: 0.0125, CostKnown: false},
		},
		{
			Model:   "deepseek:deepseek-v4-pro",
			Variant: "candidate",
			Metrics: metrics{CostUSD: 0.0075, CostKnown: false},
		},
	}
	agg := summarize(c, records)
	if got := agg.Models["deepseek:deepseek-v4-pro"].CostUSD; got != 0.02 {
		t.Fatalf("DeepSeek model cost = %f, want 0.02", got)
	}
}

func TestSummarizeUsesPairedMedianReductions(t *testing.T) {
	c := allCases()["search_context"]
	tokenPairs := [][2]int{{10, 9}, {100, 50}, {1000, 900}}
	var records []runRecord
	for _, model := range defaultModels {
		for rep, tokens := range tokenPairs {
			records = append(records,
				runRecord{
					Model:      model,
					Repetition: rep + 1,
					Variant:    "baseline",
					Score:      score{Pass: true},
					Metrics:    metrics{TotalTokens: tokens[0], RGToReadTransitions: 2},
				},
				runRecord{
					Model:      model,
					Repetition: rep + 1,
					Variant:    "candidate",
					Score:      score{Pass: true},
					Metrics:    metrics{TotalTokens: tokens[1], UsedSearch: true},
				},
			)
		}
	}
	agg := summarize(c, records)
	if agg.TokenSavingPct != 10 {
		t.Fatalf("paired median token saving = %.1f%%, want 10%%", agg.TokenSavingPct)
	}
}

func TestSummarizeReportsPairedDistributions(t *testing.T) {
	c := allCases()["known_path_batching"]
	records := []runRecord{
		{Model: "model", Repetition: 3, Variant: "candidate", Score: score{Pass: true}, Metrics: metrics{TotalTokens: 120, Turns: 5}},
		{Model: "model", Repetition: 1, Variant: "baseline", Score: score{Pass: true}, Metrics: metrics{TotalTokens: 100, Turns: 4}},
		{Model: "model", Repetition: 2, Variant: "candidate", Score: score{Pass: true}, Metrics: metrics{TotalTokens: 100, Turns: 4}},
		{Model: "model", Repetition: 3, Variant: "baseline", Score: score{Pass: true}, Metrics: metrics{TotalTokens: 100, Turns: 4}},
		{Model: "model", Repetition: 1, Variant: "candidate", Score: score{Pass: true}, Metrics: metrics{TotalTokens: 90, Turns: 3}},
		{Model: "model", Repetition: 2, Variant: "baseline", Score: score{Pass: true}, Metrics: metrics{TotalTokens: 100, Turns: 4}},
		{Model: "model", Repetition: 4, Variant: "baseline", Invalid: "interrupted", Metrics: metrics{TotalTokens: 1, Turns: 1}},
		{Model: "model", Repetition: 4, Variant: "candidate", Invalid: "interrupted", Metrics: metrics{TotalTokens: 1000, Turns: 10}},
	}
	agg := summarize(c, records)
	if agg.TokenSavingPct != 0 {
		t.Fatalf("aggregate token saving = %.1f%%, want 0%% after invalid pairs are excluded", agg.TokenSavingPct)
	}
	paired := agg.Models["model"].Paired
	if len(paired.Observations) != 3 {
		t.Fatalf("observations = %d, want 3", len(paired.Observations))
	}
	for i, want := range []int{1, 2, 3} {
		if paired.Observations[i].Repetition != want {
			t.Fatalf("observation %d repetition = %d, want %d", i, paired.Observations[i].Repetition, want)
		}
	}
	if paired.TokenImprovedPairs != 1 || paired.TokenRegressedPairs != 1 || paired.TokenTiedPairs != 1 {
		t.Fatalf("token signs = %d/%d/%d, want 1/1/1", paired.TokenImprovedPairs, paired.TokenRegressedPairs, paired.TokenTiedPairs)
	}
	if paired.TokenSavingMinPct != -20 || paired.TokenSavingMaxPct != 10 {
		t.Fatalf("token range = %.1f to %.1f, want -20 to 10", paired.TokenSavingMinPct, paired.TokenSavingMaxPct)
	}
	if paired.TurnImprovedPairs != 1 || paired.TurnRegressedPairs != 1 || paired.TurnTiedPairs != 1 || paired.MedianTurnDelta != 0 {
		t.Fatalf("turn distribution = %d/%d/%d median %.1f, want 1/1/1 median 0", paired.TurnImprovedPairs, paired.TurnRegressedPairs, paired.TurnTiedPairs, paired.MedianTurnDelta)
	}
}

func TestSummarizePrimaryReductionUsesAggregateMedians(t *testing.T) {
	c := allCases()["command_steps"]
	baseline := []int{3, 1, 3, 1, 1, 1, 0, 2, 0}
	candidate := []int{3, 2, 4, 0, 0, 0, 0, 0, 0}
	var records []runRecord
	for i := range baseline {
		records = append(records,
			runRecord{
				Model:      defaultModels[i/3],
				Repetition: i%3 + 1,
				Variant:    "baseline",
				Metrics:    metrics{CommandToCommandTransitions: baseline[i]},
			},
			runRecord{
				Model:      defaultModels[i/3],
				Repetition: i%3 + 1,
				Variant:    "candidate",
				Metrics:    metrics{CommandToCommandTransitions: candidate[i]},
			},
		)
	}
	agg := summarize(c, records)
	if agg.PrimaryBaselineMedian != 1 || agg.PrimaryCandidateMedian != 0 {
		t.Fatalf("primary medians = %.1f -> %.1f, want 1 -> 0", agg.PrimaryBaselineMedian, agg.PrimaryCandidateMedian)
	}
	if agg.PrimaryReductionPct != 100 {
		t.Fatalf("primary reduction = %.1f%%, want 100%%", agg.PrimaryReductionPct)
	}
}

func TestTodoAdoptionRequiresTodoCall(t *testing.T) {
	if adopted("todo_coissue", metrics{}) {
		t.Fatal("zero todo-only turns without a TODO call counted as adoption")
	}
	if !adopted("todo_coissue", metrics{ToolCalls: map[string]int{"update_todos": 1}}) {
		t.Fatal("coissued TODO call did not count as adoption")
	}
	if adopted("todo_coissue", metrics{
		ToolCalls:              map[string]int{"update_todos": 1},
		AvoidableTodoOnlyTurns: 1,
	}) {
		t.Fatal("todo-only turn counted as adoption")
	}
	if got := primaryValue(benchmarkCase{PrimaryMetric: "avoidable_todo_only_turns"}, metrics{AvoidableTodoOnlyTurns: 2}); got != 2 {
		t.Fatalf("todo-only primary metric = %d, want 2", got)
	}
}

func TestOrientationAdoptionAcceptsCoissuedDirectReads(t *testing.T) {
	known := metrics{
		ToolCalls:              map[string]int{"read": 18, "shell": 4},
		ExactKnownPathSearches: 3,
		ExactKnownPathCommands: 1,
		CoissuedReadTurns:      1,
		CoissuedLookupTurns:    1,
	}
	if !adopted("known_path_batching", known) {
		t.Fatal("coissued known-path reads did not count as adoption")
	}
	unknown := metrics{DiscoveryBeforeRead: true, ToolCalls: map[string]int{"read": 2}, CoissuedReadTurns: 1}
	if !adopted("unknown_path_discovery", unknown) {
		t.Fatal("coissued discovered-path reads did not count as adoption")
	}
	scalePaths := readScaleFixturePaths(36)
	if !adopted("read_scale_036", metrics{SuccessfulCoissuedReadPaths: scalePaths}) {
		t.Fatal("successful coissued read-scale coverage did not count as adoption")
	}
	if adopted("read_scale_036", metrics{CoissuedReadTurns: 1}) ||
		adopted("read_scale_036", metrics{SuccessfulCoissuedReadPaths: scalePaths[:35]}) {
		t.Fatal("read-scale adoption accepted unsuccessful or incomplete coissued reads")
	}
}

func TestBackgroundWaitAdoptionAllowsTimeoutRetry(t *testing.T) {
	for _, waits := range []int{1, 2} {
		if !adopted("background_wait", metrics{BackgroundWaits: waits}) {
			t.Fatalf("%d event-driven waits did not count as adoption", waits)
		}
	}
	if adopted("background_wait", metrics{}) {
		t.Fatal("no wait counted as adoption")
	}
	if adopted("background_wait", metrics{BackgroundWaits: 1, BackgroundPolls: 1}) {
		t.Fatal("polling wait flow counted as adoption")
	}
}

func TestScoreTodoCoissueAcceptsBinaryUnitDefault(t *testing.T) {
	input := "compact_tool_result_max_bytes internal/config cmd/harness CompactToolResultMaxBytes toolResultMaxBytes retention.go default 4 KiB"
	got := scoreTodoCoissue(scoreInput{Stdout: input})
	if !got.Pass {
		t.Fatalf("score rejected 4 KiB spelling: %v", got.Reasons)
	}
}

func TestScoreBackgroundWaitAcceptsExitStatus(t *testing.T) {
	got := scoreBackgroundWait(scoreInput{
		Stdout:  "AGENTS.md README.md smoke.md — Exit status: 0",
		Metrics: metrics{StartedRaceSuite: true},
	})
	if !got.Pass {
		t.Fatalf("score rejected successful exit status: %v", got.Reasons)
	}
}

func TestScoreSearchContextAcceptsExactBasenameCitations(t *testing.T) {
	input := "retention.go compact.go applyRetention keepBoundary trimToolResultBlock readOnlyResultIDsIn archive"
	got := scoreSearchContext(scoreInput{Stdout: input})
	if !got.Pass {
		t.Fatalf("score rejected exact basename citations: %v", got.Reasons)
	}
}

func TestReadOnlyScoresRejectWorkspaceMutation(t *testing.T) {
	tests := []struct {
		name  string
		score func(scoreInput) score
		out   string
	}{
		{
			name:  "search context",
			score: scoreSearchContext,
			out:   "retention.go compact.go applyRetention keepBoundary trimToolResultBlock readOnlyResultIDsIn archive",
		},
		{
			name:  "todo coissue",
			score: scoreTodoCoissue,
			out:   "compact_tool_result_max_bytes internal/config cmd/harness CompactToolResultMaxBytes toolResultMaxBytes retention.go default 4096",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := tc.score(scoreInput{
				Stdout:        tc.out,
				FixtureBefore: "before",
				FixtureAfter:  "after",
			})
			if got.Pass || !contains(got.Reasons, "model changed the prepared workspace") {
				t.Fatalf("mutation score = %+v, want workspace-change failure", got)
			}
		})
	}
}

func TestFinalAssistantTextRequiresFinalPhase(t *testing.T) {
	messages := []llm.Message{
		{Role: llm.RoleAssistant, Phase: llm.AssistantPhaseCommentary, Content: []llm.ContentBlock{{Kind: llm.BlockText, Text: "working"}}},
		{Role: llm.RoleAssistant, Phase: llm.AssistantPhaseFinal, Content: []llm.ContentBlock{
			{Kind: llm.BlockText, Text: "final"},
			{Kind: llm.BlockThinking, Thinking: "hidden"},
			{Kind: llm.BlockText, Text: "answer"},
		}},
	}
	if got := finalAssistantText(messages); got != "final\nanswer" {
		t.Fatalf("finalAssistantText = %q", got)
	}
	if got := finalAssistantText(messages[:1]); got != "" {
		t.Fatalf("commentary-only text = %q, want empty", got)
	}
	if got := assistantText(messages); got != "working\nfinal\nanswer" {
		t.Fatalf("assistantText = %q", got)
	}
}

func TestResumeRecordsPreservesInvalidSamplesForRerun(t *testing.T) {
	results := t.TempDir()
	c := benchmarkCase{Name: "resume_invalid", Prompt: "prompt"}
	cfg := runConfig{
		Results: results, Case: c,
		BaselineSHA: "baseline", CandidateSHA: "candidate",
		Models: []string{"provider:model"}, Repetitions: 1, Resume: true,
	}
	invalid := runRecord{
		Version: runRecordVersion, Case: c.Name, Model: "provider:model",
		Repetition: 1, Variant: "baseline", TargetSHA: targetSHA,
		HarnessSHA: cfg.BaselineSHA, Invalid: "interrupted", Completed: false,
	}
	if err := writeRecords(results, []runRecord{invalid}); err != nil {
		t.Fatal(err)
	}

	records, err := resumeRecords(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 1 || !reflect.DeepEqual(records[0], invalid) {
		t.Fatalf("resumeRecords returned %+v, want the invalid evidence preserved", records)
	}
	data, err := os.ReadFile(filepath.Join(results, c.Name+"-runs.json"))
	if err != nil {
		t.Fatal(err)
	}
	var persisted []runRecord
	if err := json.Unmarshal(data, &persisted); err != nil {
		t.Fatal(err)
	}
	if len(persisted) != 1 || persisted[0].Invalid == "" {
		t.Fatalf("persisted invalid evidence = %+v, want the interrupted record retained until rerun", persisted)
	}
	if _, err := os.Stat(filepath.Join(results, "matrix-runs.json")); !os.IsNotExist(err) {
		t.Fatalf("empty resume created matrix-runs.json: %v", err)
	}
}

func TestResumeRecordsPreservesMixedInvalidEvidenceAndCompletesOnlyValidKeys(t *testing.T) {
	results := t.TempDir()
	c := benchmarkCase{Name: "resume_mixed", Prompt: "prompt", Score: func(scoreInput) score { return score{Pass: true} }}
	cfg := runConfig{Results: results, Case: c, BaselineSHA: "baseline", CandidateSHA: "candidate", Models: []string{"provider:model"}, Repetitions: 1, Resume: true}
	sessionDir := filepath.Join(results, "valid-session")
	if err := (session.Session{Messages: []llm.Message{{Role: llm.RoleAssistant, Phase: llm.AssistantPhaseFinal, Content: []llm.ContentBlock{{Kind: llm.BlockText, Text: "final"}}}}}).Save(sessionDir); err != nil {
		t.Fatal(err)
	}
	if err := session.AppendEvent(sessionDir, session.Event{Type: session.EventModelRequest, ModelRequest: &llm.ModelRequestEvent{TargetID: "provider:model"}}); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(sessionDir, "raw.ndjson"))
	if err != nil {
		t.Fatal(err)
	}
	valid := runRecord{Version: runRecordVersion, Case: c.Name, Model: "provider:model", Repetition: 1, Variant: "baseline", TargetSHA: targetSHA, HarnessSHA: cfg.BaselineSHA, Completed: true, SessionDir: sessionDir, Reasoning: "medium", PromptSHA256: promptDigest(c), OracleVersion: oracleContractVersion, EventsSHA256: digestString(string(raw)), Score: score{Pass: true}}
	invalid := runRecord{Version: runRecordVersion, Case: c.Name, Model: "provider:model", Repetition: 1, Variant: "candidate", Order: 2, TargetSHA: targetSHA, HarnessSHA: cfg.CandidateSHA, Invalid: "interrupted", Started: time.Unix(123, 0).UTC()}
	if err := writeRecords(results, []runRecord{valid, invalid}); err != nil {
		t.Fatal(err)
	}
	records, err := resumeRecords(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 2 || !reflect.DeepEqual(records[1], invalid) {
		t.Fatalf("resumed records = %+v; invalid evidence changed or disappeared", records)
	}
	completed := completedRecordKeys(records)
	if !completed[recordKey(valid.Model, valid.Repetition, valid.Variant)] || completed[recordKey(invalid.Model, invalid.Repetition, invalid.Variant)] {
		t.Fatalf("completed keys = %+v, want valid only", completed)
	}
	replacement := valid
	replacement.Variant = "candidate"
	replacement.Order = 3
	replacement.HarnessSHA = cfg.CandidateSHA
	replacement.Metrics.TotalTokens = 80
	records = append(records, replacement)
	if got := summarize(c, records); got.Runs != 2 {
		t.Fatalf("summary runs = %d, want valid original plus replacement only", got.Runs)
	}
	if pairs := pairedRecords(records); len(pairs) != 1 || pairs[0].candidate.Invalid != "" {
		t.Fatalf("pairs = %+v, want replacement paired without invalid evidence", pairs)
	}
	if err := writeRecords(results, records); err != nil {
		t.Fatal(err)
	}
	reloaded, err := resumeRecords(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if len(reloaded) != 3 || !reflect.DeepEqual(reloaded[1], invalid) {
		t.Fatalf("second resume records = %+v; invalid evidence changed or disappeared", reloaded)
	}
	completed = completedRecordKeys(reloaded)
	for _, variant := range []string{"baseline", "candidate"} {
		if !completed[recordKey(valid.Model, valid.Repetition, variant)] {
			t.Fatalf("second resume completed keys = %+v, missing %s", completed, variant)
		}
	}
}

func TestPrepareRunDirPreservesInterruptedRunOnResume(t *testing.T) {
	parent := t.TempDir()
	runDir := filepath.Join(parent, "01-candidate")
	if err := os.MkdirAll(runDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(runDir, "partial.txt"), []byte("partial"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := prepareRunDir(runDir, true); err != nil {
		t.Fatal(err)
	}
	if entries, err := os.ReadDir(runDir); err != nil || len(entries) != 0 {
		t.Fatalf("fresh run directory = %v, %v", entries, err)
	}
	preserved, err := filepath.Glob(runDir + ".interrupted-*")
	if err != nil || len(preserved) != 1 {
		t.Fatalf("preserved directories = %v, %v", preserved, err)
	}
	if data, err := os.ReadFile(filepath.Join(preserved[0], "partial.txt")); err != nil || string(data) != "partial" {
		t.Fatalf("preserved partial data = %q, %v", data, err)
	}
}

func TestBaselineOrderFollowsAlternatingPairs(t *testing.T) {
	got := []int{
		baselineOrder(0, 3, 1),
		baselineOrder(0, 3, 2),
		baselineOrder(0, 3, 3),
		baselineOrder(1, 3, 1),
	}
	want := []int{1, 4, 5, 7}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("baseline orders = %v, want %v", got, want)
		}
	}
}

func TestParallelMatrixRoundsGroupOneRunPerModel(t *testing.T) {
	cfg := runConfig{Models: []string{"provider:a", "provider:b", "provider:c"}, Repetitions: 2}
	rounds := parallelMatrixRounds(cfg, map[string]bool{
		recordKey("provider:b", 1, "baseline"): true,
	})
	if len(rounds) != 4 {
		t.Fatalf("parallel rounds = %d, want 4", len(rounds))
	}
	wantVariants := []string{"baseline", "candidate", "candidate", "baseline"}
	for roundIndex, round := range rounds {
		seen := map[string]bool{}
		for _, job := range round {
			if job.Variant != wantVariants[roundIndex] {
				t.Errorf("round %d variant = %q, want %q", roundIndex, job.Variant, wantVariants[roundIndex])
			}
			if seen[job.Model] {
				t.Errorf("round %d contains model %q more than once", roundIndex, job.Model)
			}
			seen[job.Model] = true
			if got := matrixOrder(job.ModelIndex, cfg.Repetitions, job.Repetition, job.Variant); job.Order != got {
				t.Errorf("round %d job order = %d, want %d", roundIndex, job.Order, got)
			}
		}
		wantRuns := len(cfg.Models)
		if roundIndex == 0 {
			wantRuns--
		}
		if len(round) != wantRuns {
			t.Errorf("round %d runs = %d, want %d", roundIndex, len(round), wantRuns)
		}
	}
}

func TestExecuteRunRoundStartsEveryModelConcurrently(t *testing.T) {
	jobs := []matrixJob{{Model: "a"}, {Model: "b"}, {Model: "c"}}
	entered := make(chan string, len(jobs))
	release := make(chan struct{})
	results := executeRunRound(jobs, func(job matrixJob) matrixResult {
		entered <- job.Model
		<-release
		return matrixResult{Job: job, Record: runRecord{Model: job.Model}}
	})

	seen := map[string]bool{}
	for range jobs {
		select {
		case model := <-entered:
			seen[model] = true
		case <-time.After(5 * time.Second):
			t.Fatal("round did not start every model concurrently")
		}
	}
	close(release)
	for result := range results {
		if result.Record.Model != result.Job.Model {
			t.Errorf("result model = %q, job model = %q", result.Record.Model, result.Job.Model)
		}
	}
	if len(seen) != len(jobs) {
		t.Fatalf("started models = %v, want all jobs", seen)
	}
}
