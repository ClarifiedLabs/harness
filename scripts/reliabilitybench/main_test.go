package main

import (
	"bytes"
	"strings"
	"testing"

	"harness/internal/session"
)

func TestBuildComparisonUsesDenominatorsAndAvailability(t *testing.T) {
	baseline := session.AnalysisReport{
		Sessions: 2,
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
	candidate := baseline
	candidate.Sessions = 3
	candidate.Execution.CompletedPrompts = 4
	candidate.Execution.ToolErrors = 1
	candidate.Telemetry.Progress.MaxInspectionNoProgressStreak.Value = 3
	candidate.Telemetry.Progress.TurnsToFirstMutation.Value = 2
	candidate.Telemetry.Progress.TurnsToFirstVerification = session.AnalysisValue{Available: true, Observed: true, Value: 6}
	candidate.Telemetry.Hooks.Diagnostics = 2

	result := buildComparison(baseline, candidate)
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
}

func TestWriteComparisonTextMarksUnavailableMetrics(t *testing.T) {
	result := buildComparison(session.AnalysisReport{}, session.AnalysisReport{})
	var out bytes.Buffer
	if err := writeComparisonText(&out, result); err != nil {
		t.Fatal(err)
	}
	text := out.String()
	if !strings.Contains(text, "delta = candidate - baseline") || !strings.Contains(text, "prompt completion") || !strings.Contains(text, "unavailable") {
		t.Fatalf("text = %q", text)
	}
}
