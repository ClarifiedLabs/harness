package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"harness/internal/llm"
	"harness/internal/session"
)

func TestDefaultStackMarathonCaseContract(t *testing.T) {
	c := defaultStackMarathonCase()
	if c.Name != "default_stack_marathon" || c.Setup == nil || c.Score == nil || c.HelperCommand != defaultStackVerifierCommand {
		t.Fatalf("default-stack case = %+v", c)
	}
	if !c.RestartBetweenPrompts || len(c.RestartPhases) != defaultStackPhaseCount || defaultStackPhaseCount != 18 {
		t.Fatalf("restart phases = %d, want 18", len(c.RestartPhases))
	}
	if c.MinimumRunTokens != 1_000_000 || c.RunTimeout != 4*time.Hour {
		t.Fatalf("long-horizon bounds = %d, %s", c.MinimumRunTokens, c.RunTimeout)
	}
	baseline, candidate := c.variant("baseline"), c.variant("candidate")
	if baseline.Agent != "auto" || candidate.Agent != "auto" || !baseline.Helper || !candidate.Helper {
		t.Fatalf("variants = baseline %+v, candidate %+v", baseline, candidate)
	}
	var baselineConfig, candidateConfig map[string]any
	if err := json.Unmarshal([]byte(baseline.Config), &baselineConfig); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal([]byte(candidate.Config), &candidateConfig); err != nil {
		t.Fatal(err)
	}
	if baselineConfig["stagnation_nudge"] != false || candidateConfig["stagnation_nudge"] != true {
		t.Fatalf("stagnation variants = %v, %v", baselineConfig["stagnation_nudge"], candidateConfig["stagnation_nudge"])
	}
	delete(baselineConfig, "stagnation_nudge")
	delete(candidateConfig, "stagnation_nudge")
	if !reflect.DeepEqual(baselineConfig, candidateConfig) {
		t.Fatalf("non-treatment config differs\nbaseline=%v\ncandidate=%v", baselineConfig, candidateConfig)
	}
	for i, phase := range c.RestartPhases {
		attempt := i%defaultStackAttempts + 1
		milestone := i / defaultStackAttempts
		if attempt == defaultStackAttempts {
			if phase.After == nil {
				t.Fatalf("milestone %d final attempt has no oracle transition", milestone+1)
			}
			if got, want := phase.CompactAfter, milestone+1 < len(defaultStackMilestones); got != want {
				t.Fatalf("milestone %d compact = %t, want %t", milestone+1, got, want)
			}
		} else if phase.After != nil || phase.CompactAfter {
			t.Fatalf("phase %d has an early transition", i+1)
		}
	}
}

func TestDefaultStackOracleRejectsInitialAndAcceptsReferenceProject(t *testing.T) {
	dir := initFlowbenchTestRepo(t)
	if err := setupDefaultStackMarathon(dir); err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(dir, defaultStackFixture)
	if passed, _ := runDefaultStackTestSuite(dir, 0); passed {
		t.Fatal("initial kv-core milestone unexpectedly passed")
	}
	writeKVReferenceFiles(t, filepath.Join(root, "kvstore"), kvCheckpointReferenceFiles)
	requireDefaultStackMilestonePass(t, dir, 0)
	if err := advanceDefaultStackMilestone(dir, 0); err != nil {
		t.Fatal(err)
	}
	if passed, _ := runDefaultStackTestSuite(dir, 1); passed {
		t.Fatal("kv-core reference unexpectedly passed durability milestone")
	}
	writeKVReferenceFiles(t, filepath.Join(root, "kvstore"), kvFinalReferenceFiles)
	requireDefaultStackMilestonePass(t, dir, 1)
	if err := advanceDefaultStackMilestone(dir, 1); err != nil {
		t.Fatal(err)
	}
	if passed, _ := runDefaultStackTestSuite(dir, 2); passed {
		t.Fatal("initial planner-core milestone unexpectedly passed")
	}
	writeDefaultStackReferencePackage(t, root, "planner")
	requireDefaultStackMilestonePass(t, dir, 2)
	if err := advanceDefaultStackMilestone(dir, 2); err != nil {
		t.Fatal(err)
	}
	requireDefaultStackMilestonePass(t, dir, 3)
	if err := advanceDefaultStackMilestone(dir, 3); err != nil {
		t.Fatal(err)
	}
	if passed, _ := runDefaultStackTestSuite(dir, 4); passed {
		t.Fatal("initial framing-core milestone unexpectedly passed")
	}
	writeDefaultStackReferencePackage(t, root, "framing")
	requireDefaultStackMilestonePass(t, dir, 4)
	if err := advanceDefaultStackMilestone(dir, 4); err != nil {
		t.Fatal(err)
	}
	requireDefaultStackMilestonePass(t, dir, 5)
	if err := advanceDefaultStackMilestone(dir, 5); err != nil {
		t.Fatal(err)
	}

	scores := make([]float64, defaultStackPhaseCount)
	for i := range scores {
		scores[i] = defaultStackMilestones[i/defaultStackAttempts].score
	}
	m := metrics{
		DefaultStackEvaluatorResults: defaultStackPhaseCount,
		DefaultStackEvaluatorAccepts: defaultStackPhaseCount,
		EvaluatorScoreProgression:    scores,
		EvaluatorBestScore:           100,
		EvaluatorBestScoreAvailable:  true,
		EvaluatorAcceptedAfterResume: true,
		Compactions:                  len(defaultStackMilestones) - 1,
		Prompts:                      defaultStackPhaseCount,
	}
	got := scoreDefaultStackMarathon(scoreInput{Variant: "baseline", Worktree: dir, Metrics: m})
	if !got.Pass {
		t.Fatalf("reference project score = %+v", got)
	}
	m.DefaultStackControlMutations = 1
	if got := scoreDefaultStackMarathon(scoreInput{Variant: "baseline", Worktree: dir, Metrics: m}); got.Pass {
		t.Fatal("host-owned milestone mutation passed")
	}
}

func TestDefaultStackVerifierWritesBoundedEvidence(t *testing.T) {
	dir := initFlowbenchTestRepo(t)
	if err := setupDefaultStackMarathon(dir); err != nil {
		t.Fatal(err)
	}
	var output strings.Builder
	if code := runDefaultStackVerifier(dir, strings.NewReader(""), &output); code != 0 {
		t.Fatalf("verifier code = %d", code)
	}
	result := decodeEvaluatorOutput(t, output.String())
	if result.Accepted || result.Score == nil || *result.Score != 0 || result.RemainingRequirements == nil || *result.RemainingRequirements != 1 {
		t.Fatalf("initial result = %+v", result)
	}
	if !strings.Contains(result.EvidenceRef, "01-kv-core-rejected-") || !strings.Contains(result.Reason, result.EvidenceRef) {
		t.Fatalf("initial evidence = %+v", result)
	}
	data, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(result.EvidenceRef)))
	if err != nil || len(data) == 0 || len(data) > 32_000 || !strings.Contains(string(data), "HIDDEN BEHAVIORAL TEST DIAGNOSTICS") {
		t.Fatalf("evidence bytes = %d, err = %v", len(data), err)
	}

	root := filepath.Join(dir, defaultStackFixture)
	writeKVReferenceFiles(t, filepath.Join(root, "kvstore"), kvCheckpointReferenceFiles)
	output.Reset()
	if code := runDefaultStackVerifier(dir, strings.NewReader(""), &output); code != 0 {
		t.Fatalf("accepted verifier code = %d", code)
	}
	accepted := decodeEvaluatorOutput(t, output.String())
	if !accepted.Accepted || accepted.Score == nil || *accepted.Score != defaultStackMilestones[0].score || accepted.RemainingRequirements == nil || *accepted.RemainingRequirements != 0 {
		t.Fatalf("accepted result = %+v", accepted)
	}
}

func TestDefaultStackTokenFloorIsAnExplicitGate(t *testing.T) {
	c := defaultStackMarathonCase()
	validMetrics := metrics{
		TotalTokens: 1_200_000, Turns: 100,
		DefaultStackEvaluatorResults: defaultStackPhaseCount,
		EvaluatorBestScore:           100, EvaluatorBestScoreAvailable: true,
	}
	records := []runRecord{
		{Model: "test:model", Repetition: 1, Variant: "baseline", Metrics: validMetrics, Score: score{Pass: true}},
		{Model: "test:model", Repetition: 1, Variant: "candidate", Metrics: validMetrics, Score: score{Pass: true}},
	}
	records[0].Metrics.TotalTokens = 999_999
	agg := summarize(c, records)
	if agg.MinimumRunTokens != 1_000_000 || agg.RunsMeetingTokenFloor != 1 || !containsSubstring(agg.Failures, "long-horizon token floor coverage 1/2") {
		t.Fatalf("token-floor aggregate = %+v", agg)
	}
}

func TestDefaultStackEvaluatorAssetLeakDetection(t *testing.T) {
	for _, test := range []struct {
		tool  string
		input string
		want  bool
	}{
		{tool: "read", input: `{"path":".flowbench-default-stack/planner/graph.go"}`},
		{tool: "read", input: `{"path":".flowbench-default-stack/evidence/01.txt"}`},
		{tool: "shell", input: `{"argv":["harness-flowbench-default-stack-evaluate"]}`, want: true},
		{tool: "read", input: `{"path":"scripts/flowbench/default_stack_test.go"}`, want: true},
	} {
		if got := defaultStackEvaluatorAssetLeak(test.tool, json.RawMessage(test.input)); got != test.want {
			t.Errorf("defaultStackEvaluatorAssetLeak(%s, %s) = %t, want %t", test.tool, test.input, got, test.want)
		}
	}
}

func TestDefaultStackControlMutationDetection(t *testing.T) {
	for _, test := range []struct {
		paths []string
		want  bool
	}{
		{paths: []string{".flowbench-default-stack/planner/graph.go"}},
		{paths: []string{".flowbench-default-stack/MILESTONE.md"}, want: true},
		{paths: []string{".flowbench-default-stack/milestone.txt"}, want: true},
		{paths: []string{"/tmp/worktree/.flowbench-default-stack/MILESTONE.md"}, want: true},
	} {
		if got := defaultStackControlMutation(test.paths); got != test.want {
			t.Errorf("defaultStackControlMutation(%v) = %t, want %t", test.paths, got, test.want)
		}
	}
}

func TestCollectMetricsCountsDefaultStackControlMutation(t *testing.T) {
	dir := t.TempDir()
	state := session.Session{Messages: []llm.Message{{
		Role: llm.RoleAssistant, Phase: llm.AssistantPhaseFinal,
		Content: []llm.ContentBlock{{Kind: llm.BlockText, Text: "done"}},
	}}}
	if err := state.Save(dir); err != nil {
		t.Fatal(err)
	}
	if err := session.AppendEvent(dir, session.Event{
		Type: session.EventToolMutation,
		ToolMutation: &session.ToolMutationSnapshot{Paths: []string{
			".flowbench-default-stack/milestone.txt",
		}},
	}); err != nil {
		t.Fatal(err)
	}
	m, err := collectMetrics(dir)
	if err != nil {
		t.Fatal(err)
	}
	if m.DefaultStackControlMutations != 1 {
		t.Fatalf("default-stack control mutations = %d, want 1", m.DefaultStackControlMutations)
	}
}

func TestDefaultStackMilestoneUsesExactHostContract(t *testing.T) {
	dir := initFlowbenchTestRepo(t)
	if err := setupDefaultStackMarathon(dir); err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(dir, defaultStackFixture)
	if _, err := os.Stat(filepath.Join(root, defaultStackLegacyPhaseFile)); !os.IsNotExist(err) {
		t.Fatalf("legacy numeric milestone state exists: %v", err)
	}
	if index, err := readDefaultStackMilestone(dir); err != nil || index != 0 {
		t.Fatalf("initial milestone = %d, %v", index, err)
	}
	if err := os.WriteFile(filepath.Join(root, defaultStackMilestoneFile), []byte("4\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := readDefaultStackMilestone(dir); err == nil {
		t.Fatal("numeric model-authored milestone state was accepted")
	}
}

func requireDefaultStackMilestonePass(t *testing.T, dir string, index int) {
	t.Helper()
	if passed, diagnostics := runDefaultStackTestSuite(dir, index); !passed {
		t.Fatalf("milestone %d reference failed: %s", index+1, diagnostics)
	}
}

func writeDefaultStackReferencePackage(t *testing.T, root, packageName string) {
	t.Helper()
	destination := filepath.Join(root, packageName)
	entries, err := os.ReadDir(destination)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if !entry.IsDir() && strings.HasSuffix(entry.Name(), ".go") {
			if err := os.Remove(filepath.Join(destination, entry.Name())); err != nil {
				t.Fatal(err)
			}
		}
	}
	source := filepath.Join("testdata", "default_stack_reference", packageName)
	entries, err = os.ReadDir(source)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".go") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(source, entry.Name()))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(destination, entry.Name()), data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

func containsSubstring(values []string, substring string) bool {
	for _, value := range values {
		if strings.Contains(value, substring) {
			return true
		}
	}
	return false
}
