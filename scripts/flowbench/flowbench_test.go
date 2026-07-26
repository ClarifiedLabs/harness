package main

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"harness/internal/llm"
)

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

func TestRunCommandInvokesGit(t *testing.T) {
	for _, input := range []string{
		`{"argv":["git","status","--short"]}`,
		`{"steps":[{"command":"git status --short"},{"command":"git diff --stat"}]}`,
	} {
		if !runCommandInvokesGit(json.RawMessage(input)) {
			t.Fatalf("git invocation not detected in %s", input)
		}
	}
	if runCommandInvokesGit(json.RawMessage(`{"command":"printf 'git status'"}`)) {
		t.Fatal("quoted git text counted as an invocation")
	}
}

func TestSetupTodoBugAndWorkspaceDigest(t *testing.T) {
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
				runRecord{Model: model, Variant: "candidate", Score: score{Pass: true}, Metrics: metrics{TotalTokens: 70, RGToReadTransitions: 0, UsedSearchContext: true}},
			)
		}
	}
	agg := summarize(c, records)
	if !agg.Accepted {
		t.Fatalf("aggregate rejected: %v", agg.Failures)
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
					Metrics:    metrics{TotalTokens: 80, UsedSearchContext: true},
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
	for _, want := range []string{"Unpaired median tokens:", "Paired-median token saving: 20.0%"} {
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
					Metrics:    metrics{TotalTokens: tokens[1], UsedSearchContext: true},
				},
			)
		}
	}
	agg := summarize(c, records)
	if agg.TokenSavingPct != 10 {
		t.Fatalf("paired median token saving = %.1f%%, want 10%%", agg.TokenSavingPct)
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
		t.Fatal("zero todo-only turns without a todo call counted as adoption")
	}
	if !adopted("todo_coissue", metrics{ToolCalls: map[string]int{"update_todos": 1}}) {
		t.Fatal("coissued todo call did not count as adoption")
	}
	if adopted("todo_coissue", metrics{
		ToolCalls:              map[string]int{"update_todos": 1},
		AvoidableTodoOnlyTurns: 1,
	}) {
		t.Fatal("todo-only turn counted as adoption")
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
