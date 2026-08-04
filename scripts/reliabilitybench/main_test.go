package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"harness/internal/llm"
	"harness/internal/session"
)

func TestReadFixtureOutcomesRejectsAmbiguousJSON(t *testing.T) {
	for _, tc := range []struct {
		name    string
		content string
		wantErr bool
	}{
		{name: "valid", content: `{"a":{"task_completed":true,"expected_state_matches":true}}`},
		{name: "duplicate fixture", content: `{"a":{"task_completed":true,"expected_state_matches":true},"a":{"task_completed":true,"expected_state_matches":true}}`, wantErr: true},
		{name: "duplicate field", content: `{"a":{"task_completed":true,"task_completed":false,"expected_state_matches":true}}`, wantErr: true},
		{name: "wrong field type", content: `{"a":{"task_completed":"yes","expected_state_matches":true}}`, wantErr: true},
		{name: "null field", content: `{"a":{"task_completed":null,"expected_state_matches":true}}`, wantErr: true},
		{name: "missing field", content: `{"a":{"task_completed":true}}`, wantErr: true},
		{name: "unknown field", content: `{"a":{"task_completed":true,"expected_state_matches":true,"extra":false}}`, wantErr: true},
		{name: "oversized", content: strings.Repeat(" ", maxFixtureOutcomeBytes+1), wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "outcomes.json")
			if err := os.WriteFile(path, []byte(tc.content), 0o644); err != nil {
				t.Fatal(err)
			}
			outcomes, err := readFixtureOutcomes(path)
			if (err != nil) != tc.wantErr {
				t.Fatalf("readFixtureOutcomes error = %v, outcomes=%+v", err, outcomes)
			}
		})
	}
}

func TestBuildComparisonUsesDenominatorsAndAvailability(t *testing.T) {
	baseline := session.AnalysisReport{
		Sessions: 2,
		Usage:    session.UsageAnalysis{Inclusive: session.UsageSlice{Available: true, Complete: true, InputTokens: 100, CacheReadTokens: 40}},
		Storage: session.StorageAnalysis{
			Available: true,
			State:     session.StorageComponent{Status: "complete"}, Tree: session.StorageComponent{Status: "complete"}, Raw: session.StorageComponent{Status: "complete"},
			Compactions: session.StorageComponent{Status: "missing"}, ToolResults: session.StorageComponent{Status: "missing"},
			ContextResetEntries: 5, SnapshotResetEntries: 4, DeltaResetEntries: 1, SnapshotPayloadBytes: 1_000, DeltaPayloadBytes: 100, TotalBytes: 10_000,
		},
		Execution: session.ExecutionAnalysis{
			Completeness: "complete", Prompts: 4, CompletedPrompts: 3, ToolResults: 10, ToolErrors: 2,
		},
		Telemetry: session.TelemetryAnalysis{
			Closure:  session.ClosureAnalysis{Available: true, Prompts: 4, TurnBudgetExhausted: 1},
			Workflow: session.WorkflowAnalysis{Available: true, Prompts: 4, Supplied: 2},
			Progress: session.ProgressAnalysis{
				Available:                     true,
				MaxInspectionNoProgressStreak: session.AnalysisValue{Available: true, Observed: true, Value: 5},
				TurnsToFirstMutation:          session.AnalysisValue{Available: true, Observed: true, Value: 4},
				BatchingSteers:                2,
				BatchingCompliant:             1,
			},
			Hooks: session.HookAnalysis{Available: true},
		},
	}
	baseline.LimitExceededStreams = 2
	candidate := baseline
	candidate.LimitExceededStreams = 1
	candidate.Sessions = 3
	candidate.Execution.CompletedPrompts = 4
	candidate.Execution.ToolErrors = 1
	candidate.Telemetry.Progress.MaxInspectionNoProgressStreak.Value = 3
	candidate.Telemetry.Progress.TurnsToFirstMutation.Value = 2
	candidate.Telemetry.Progress.TurnsToFirstVerification = session.AnalysisValue{Available: true, Observed: true, Value: 6}
	candidate.Telemetry.Hooks.Diagnostics = 2
	candidate.Usage.Inclusive.InputTokens = 90
	candidate.Usage.Inclusive.CacheReadTokens = 45
	candidate.Storage.DeltaResetEntries = 2
	candidate.Storage.TotalBytes = 9_000

	result := buildComparison(baseline, candidate)
	if result.Baseline.LimitExceededStreams != 2 || result.Candidate.LimitExceededStreams != 1 {
		t.Fatalf("limit-exceeded stream summaries = baseline %d candidate %d", result.Baseline.LimitExceededStreams, result.Candidate.LimitExceededStreams)
	}
	metric := func(name string) comparisonMetric {
		t.Helper()
		for _, candidate := range result.Metrics {
			if candidate.Name == name {
				return candidate
			}
		}
		t.Fatalf("missing metric %q", name)
		return comparisonMetric{}
	}
	completion := metric("prompt completion")
	if !completion.Available || completion.Baseline != 75 || completion.Candidate != 100 || completion.Delta != 25 {
		t.Fatalf("completion = %+v", completion)
	}
	budget := metric("turn-budget exhaustion")
	if !budget.Available || budget.Baseline != 25 || budget.Candidate != 25 {
		t.Fatalf("turn budget = %+v", budget)
	}
	progress := metric("max inspection/no-progress streak")
	if !progress.Available || progress.Delta != -2 || progress.Direction != "lower" {
		t.Fatalf("progress = %+v", progress)
	}
	if metric("turns to first successful verification").Available {
		t.Fatal("one-sided verification observation should be unavailable")
	}
	if metric("hook timeout").Available {
		t.Fatal("zero baseline hook denominator should be unavailable")
	}
	if input := metric("uncached input tokens"); !input.Available || input.Delta != -10 {
		t.Fatalf("uncached input = %+v", input)
	}
	if cache := metric("cache-read tokens"); !cache.Available || cache.Delta != 5 {
		t.Fatalf("cache read = %+v", cache)
	}
	if resets := metric("delta reset entries"); !resets.Available || resets.Baseline != 1 || resets.Candidate != 2 {
		t.Fatalf("delta resets = %+v", resets)
	}
	before := time.Now()
	candidate.Before = &before
	cutoff := buildComparison(baseline, candidate)
	for _, got := range cutoff.Metrics {
		if got.Name == "session storage" && got.Available {
			t.Fatalf("cutoff storage metric = %+v, want unavailable", got)
		}
	}
}

func TestBuildComparisonFixturePromotionGates(t *testing.T) {
	makeHierarchy := func(root string, tokens int, cost float64) session.HierarchyAnalysis {
		rootUsage := session.UsageSlice{Available: true, Complete: true, TotalTokens: tokens - 10, KnownCostUSD: cost - .1, CostComplete: true}
		childUsage := session.UsageSlice{Available: true, Complete: true, TotalTokens: 10, KnownCostUSD: .1, CostComplete: true}
		return session.HierarchyAnalysis{
			RootPath:  root,
			Sessions:  1,
			Execution: session.ExecutionAnalysis{Completeness: "complete", Prompts: 1, CompletedPrompts: 1},
			Workflow:  session.WorkflowAnalysis{Available: true, Prompts: 1, Supplied: 1, CompletionSourceAvailable: true, CompletionSourceReports: 1},
			Usage: session.UsageAnalysis{
				RootConversational: rootUsage, DescendantConversational: childUsage,
				Inclusive: session.UsageSlice{Available: true, Complete: true, TotalTokens: tokens, KnownCostUSD: cost, CostComplete: true},
			},
			Storage: session.StorageAnalysis{Available: true, TotalBytes: int64(tokens * 10)},
		}
	}
	baseline := session.AnalysisReport{Version: session.AnalysisVersion, Path: "/baseline"}
	candidate := session.AnalysisReport{Version: session.AnalysisVersion, Path: "/candidate"}
	baselineOutcomes := make(map[string]fixtureOutcome)
	candidateOutcomes := make(map[string]fixtureOutcome)
	for i, fixture := range []string{"a", "b", "c"} {
		baselineRoot, candidateRoot := "/baseline/"+fixture, "/candidate/"+fixture
		baseline.Hierarchies = append(baseline.Hierarchies, makeHierarchy(baselineRoot, 100+i*10, 1+float64(i)/10))
		candidate.Hierarchies = append(candidate.Hierarchies, makeHierarchy(candidateRoot, 90+i*10, .9+float64(i)/10))
		identity := session.ExecutionIdentityAnalysis{Available: true, Stable: true, Attempts: 1, Agent: "code", Provider: "fixture", Model: "model"}
		baseline.Items = append(baseline.Items, session.SessionAnalysis{Path: baselineRoot, RootPath: baselineRoot, Agent: "code", Provider: "fixture", Model: "model", ExecutionIdentity: identity})
		candidate.Items = append(candidate.Items, session.SessionAnalysis{Path: candidateRoot, RootPath: candidateRoot, Agent: "code", Provider: "fixture", Model: "model", ExecutionIdentity: identity})
		baselineOutcomes[fixture] = fixtureOutcome{TaskCompleted: true, ExpectedStateMatches: true}
		candidateOutcomes[fixture] = fixtureOutcome{TaskCompleted: true, ExpectedStateMatches: true}
	}

	result := buildComparisonWithInputs(baseline, candidate, baselineOutcomes, candidateOutcomes, 3)
	if result.Verdict.Status != "promote" || result.Verdict.MatchedSamples != 3 || len(result.Fixtures) != 3 {
		t.Fatalf("promotion verdict = %+v fixtures=%+v", result.Verdict, result.Fixtures)
	}
	badOutcomes := make(map[string]fixtureOutcome, len(candidateOutcomes))
	for key, value := range candidateOutcomes {
		badOutcomes[key] = value
	}
	badOutcomes["b"] = fixtureOutcome{TaskCompleted: true, ExpectedStateMatches: false}
	if got := buildComparisonWithInputs(baseline, candidate, baselineOutcomes, badOutcomes, 3).Verdict; got.Status != "reject" {
		t.Fatalf("correctness regression verdict = %+v", got)
	}
	baseline.Hierarchies[0].Usage.Reconciliation = session.UsageReconciliation{Available: true, Matches: false}
	if got := buildComparisonWithInputs(baseline, candidate, baselineOutcomes, candidateOutcomes, 3).Verdict; got.Status != "insufficient_data" || got.Reason != "baseline usage reconciliation failed" {
		t.Fatalf("baseline reconciliation verdict = %+v", got)
	}
	baseline.Hierarchies[0].Usage.Reconciliation = session.UsageReconciliation{}
	candidate.Hierarchies[0].Usage.Reconciliation = session.UsageReconciliation{Available: true, Matches: false}
	if got := buildComparisonWithInputs(baseline, candidate, baselineOutcomes, candidateOutcomes, 3).Verdict; got.Status != "reject" || got.Reason != "candidate usage reconciliation failed" {
		t.Fatalf("candidate reconciliation verdict = %+v", got)
	}
	candidate.Hierarchies[0].Usage.Reconciliation = session.UsageReconciliation{}
	delete(candidateOutcomes, "c")
	if got := buildComparisonWithInputs(baseline, candidate, baselineOutcomes, candidateOutcomes, 3).Verdict; got.Status != "insufficient_data" {
		t.Fatalf("missing outcomes verdict = %+v", got)
	}
	if got := buildComparisonWithInputs(baseline, candidate, baselineOutcomes, candidateOutcomes, 4).Verdict; got.Status != "insufficient_data" || got.MatchedSamples != 3 {
		t.Fatalf("sample-size verdict = %+v", got)
	}
}

func TestBuildComparisonCanPromoteCurrentAnalyzerOutputBeforeStructuredCompletion(t *testing.T) {
	makeCorpus := func(name string, inputTokens int, cost float64) session.AnalysisReport {
		t.Helper()
		corpus := filepath.Join(t.TempDir(), name)
		root := filepath.Join(corpus, "fixture-a")
		usage := llm.Usage{InputTokens: inputTokens, CostUSD: cost, CostKnown: true}
		state := session.Session{ID: "fixture-a", Agent: "code", Provider: "fixture", Model: "model", Build: session.BuildMetadata{Version: name, Commit: "test"}, Usage: session.UsageTotals{Usage: usage, CostUSD: cost}}
		if err := state.Save(root); err != nil {
			t.Fatal(err)
		}
		for _, event := range []session.Event{
			{Type: session.EventTurnAttemptStart, Prompt: 1, Turn: 1, Attempt: 1, Agent: "code", Provider: "fixture", Model: "model"},
			{Type: session.EventTurnAttemptUsage, Prompt: 1, Turn: 1, Attempt: 1, Usage: &usage},
			{Type: session.EventPromptUsage, Prompt: 1, TelemetryVersion: session.ReliabilityTelemetryVersion},
		} {
			if err := session.AppendEvent(root, event); err != nil {
				t.Fatal(err)
			}
		}
		report, err := session.AnalyzeCorpus(corpus, session.AnalyzeOptions{})
		if err != nil {
			t.Fatal(err)
		}
		return report
	}
	baseline := makeCorpus("baseline", 10, .1)
	candidate := makeCorpus("candidate", 9, .09)
	outcomes := map[string]fixtureOutcome{"fixture-a": {TaskCompleted: true, ExpectedStateMatches: true}}
	result := buildComparisonWithInputs(baseline, candidate, outcomes, outcomes, 1)
	if result.Verdict.Status != "promote" || len(result.Fixtures) != 1 || result.Fixtures[0].Baseline.CompletionSourceAvailable || result.Fixtures[0].Candidate.CompletionSourceAvailable || result.Fixtures[0].Baseline.Provider != "fixture" || result.Fixtures[0].Baseline.Model != "model" || result.Fixtures[0].Baseline.Cohort.Key == "" {
		t.Fatalf("current analyzer promotion = %+v", result)
	}
	candidate.Items[0].Model = "different-model"
	candidate.Items[0].ExecutionIdentity.Model = "different-model"
	if got := buildComparisonWithInputs(baseline, candidate, outcomes, outcomes, 1).Verdict; got.Status != "insufficient_data" || got.Reason != "matched fixture execution identity differs" {
		t.Fatalf("model identity verdict = %+v", got)
	}
	candidate.Items[0].Model = "model"
	candidate.Items[0].ExecutionIdentity.Model = "model"
	candidate.Items[0].Agent = "plan"
	candidate.Items[0].ExecutionIdentity.Agent = "plan"
	if got := buildComparisonWithInputs(baseline, candidate, outcomes, outcomes, 1).Verdict; got.Status != "insufficient_data" || got.Reason != "matched fixture execution identity differs" {
		t.Fatalf("root agent identity verdict = %+v", got)
	}
	candidate.Items[0].Agent = "code"
	candidate.Items[0].ExecutionIdentity.Agent = "code"
	candidate.Items[0].ExecutionIdentity.Stable = false
	if got := buildComparisonWithInputs(baseline, candidate, outcomes, outcomes, 1).Verdict; got.Status != "insufficient_data" || got.Reason != "matched fixture execution identity is unavailable" {
		t.Fatalf("switched execution identity verdict = %+v", got)
	}
	candidate.Items[0].ExecutionIdentity.Stable = true
	baselineChildIdentity := session.ExecutionIdentityAnalysis{Available: true, Stable: true, Attempts: 1, Agent: "explore", Provider: "fixture", Model: "child-model"}
	candidateChildIdentity := baselineChildIdentity
	candidateChildIdentity.Model = "different-child-model"
	baseline.Items = append(baseline.Items, session.SessionAnalysis{Path: filepath.Join(baseline.Hierarchies[0].RootPath, "children", "child"), RootPath: baseline.Hierarchies[0].RootPath, Agent: "explore", Provider: "fixture", Model: "child-model", ExecutionIdentity: baselineChildIdentity})
	candidate.Items = append(candidate.Items, session.SessionAnalysis{Path: filepath.Join(candidate.Hierarchies[0].RootPath, "children", "child"), RootPath: candidate.Hierarchies[0].RootPath, Agent: "explore", Provider: "fixture", Model: "different-child-model", ExecutionIdentity: candidateChildIdentity})
	baseline.Hierarchies[0].Sessions, candidate.Hierarchies[0].Sessions = 2, 2
	if got := buildComparisonWithInputs(baseline, candidate, outcomes, outcomes, 1).Verdict; got.Status != "insufficient_data" || got.Reason != "matched fixture execution identity differs" {
		t.Fatalf("child model identity verdict = %+v", got)
	}
	candidate.Items[1].Model = "child-model"
	candidate.Items[1].ExecutionIdentity.Model = "child-model"
	candidate.Items[1].Agent = ""
	candidate.Items[1].ExecutionIdentity.Agent = ""
	if got := buildComparisonWithInputs(baseline, candidate, outcomes, outcomes, 1).Verdict; got.Status != "insufficient_data" || got.Reason != "matched fixture execution identity is unavailable" {
		t.Fatalf("missing child agent identity verdict = %+v", got)
	}
	baseline.Items = baseline.Items[:1]
	candidate.Items = candidate.Items[:1]
	baseline.Hierarchies[0].Sessions, candidate.Hierarchies[0].Sessions = 1, 1
	before := time.Now()
	candidate.Before = &before
	if got := buildComparisonWithInputs(baseline, candidate, outcomes, outcomes, 1).Verdict; got.Status != "insufficient_data" || got.Reason != "cutoff-prefix corpora cannot receive an automatic verdict" {
		t.Fatalf("cutoff verdict = %+v", got)
	}
}

func TestBuildComparisonRequiresExactFixtureSets(t *testing.T) {
	usage := session.UsageAnalysis{Inclusive: session.UsageSlice{Available: true, Complete: true, CostComplete: true}}
	baseline := session.AnalysisReport{Path: "/baseline", Hierarchies: []session.HierarchyAnalysis{{RootPath: "/baseline/a", Usage: usage}}}
	candidate := session.AnalysisReport{Path: "/candidate", Hierarchies: []session.HierarchyAnalysis{
		{RootPath: "/candidate/a", Usage: usage},
		{RootPath: "/candidate/extra", Usage: usage},
	}}
	got := buildComparisonWithInputs(baseline, candidate, nil, nil, 1).Verdict
	if got.Status != "insufficient_data" || got.MatchedSamples != 1 || got.Reason != "fixture sets differ or contain ambiguous basenames" {
		t.Fatalf("extra candidate fixture verdict = %+v", got)
	}
}

func TestBuildComparisonRejectsAmbiguousFixtureBasenames(t *testing.T) {
	usage := session.UsageAnalysis{Inclusive: session.UsageSlice{Available: true, Complete: true, CostComplete: true}}
	baseline := session.AnalysisReport{Path: "/baseline", Hierarchies: []session.HierarchyAnalysis{
		{RootPath: "/baseline/group-one/same", Usage: usage},
		{RootPath: "/baseline/group-two/same", Usage: usage},
	}}
	candidate := session.AnalysisReport{Path: "/candidate", Hierarchies: []session.HierarchyAnalysis{
		{RootPath: "/candidate/same", Usage: usage},
	}}
	result := buildComparisonWithInputs(baseline, candidate, nil, nil, 1)
	if len(result.Fixtures) != 1 || result.Fixtures[0].Matched || !result.Fixtures[0].BaselineAmbiguous || result.Fixtures[0].CandidateAmbiguous || result.Verdict.Status != "insufficient_data" {
		t.Fatalf("ambiguous basename result = %+v", result)
	}
}

func TestWriteComparisonTextMarksUnavailableMetrics(t *testing.T) {
	result := buildComparison(session.AnalysisReport{}, session.AnalysisReport{})
	result.Baseline.Cohorts = []session.CohortAnalysis{{Cohort: session.CohortIdentity{Available: true, Key: "base-key", Build: session.BuildMetadata{Version: "v1", Commit: "abc"}}, Roots: 1, Sessions: 2}}
	result.Candidate.Cohorts = []session.CohortAnalysis{{Cohort: session.CohortIdentity{Available: true, Key: "candidate-key", Build: session.BuildMetadata{Version: "v2", Commit: "def", Modified: true}}, Roots: 1, Sessions: 2}}
	var out bytes.Buffer
	if err := writeComparisonText(&out, result); err != nil {
		t.Fatal(err)
	}
	text := out.String()
	if !strings.Contains(text, "delta = candidate - baseline") || !strings.Contains(text, "prompt completion") || !strings.Contains(text, "unavailable") || !strings.Contains(text, "base-key") || !strings.Contains(text, "v2@def+modified") {
		t.Fatalf("text = %q", text)
	}
}
