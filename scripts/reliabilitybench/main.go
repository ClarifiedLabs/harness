// Command reliabilitybench compares transcript-free reliability telemetry from
// two harness session corpora.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"harness/internal/session"
)

const comparisonVersion = 1

type corpusSummary struct {
	Path                   string                    `json:"path"`
	Before                 *time.Time                `json:"before"`
	Roots                  int                       `json:"roots"`
	Sessions               int                       `json:"sessions"`
	MissingStreams         int                       `json:"missing_streams"`
	IncompleteStreams      int                       `json:"incomplete_streams"`
	MalformedStreams       int                       `json:"malformed_streams"`
	SymlinkStreams         int                       `json:"symlink_streams"`
	MalformedChildMetadata int                       `json:"malformed_child_metadata"`
	Completeness           map[string]int            `json:"completeness"`
	Execution              session.ExecutionAnalysis `json:"execution"`
	Telemetry              session.TelemetryAnalysis `json:"telemetry"`
	Coverage               session.TelemetryCoverage `json:"telemetry_coverage"`
}

type comparisonMetric struct {
	Name      string  `json:"name"`
	Unit      string  `json:"unit"`
	Available bool    `json:"available"`
	Baseline  float64 `json:"baseline,omitempty"`
	Candidate float64 `json:"candidate,omitempty"`
	Delta     float64 `json:"delta,omitempty"`
	Direction string  `json:"preferred_direction,omitempty"`
}

type comparison struct {
	Version   int                `json:"version"`
	Baseline  corpusSummary      `json:"baseline"`
	Candidate corpusSummary      `json:"candidate"`
	Metrics   []comparisonMetric `json:"metrics"`
}

func main() {
	baselinePath := flag.String("baseline", "", "baseline session or corpus directory")
	candidatePath := flag.String("candidate", "", "candidate session or corpus directory")
	beforeText := flag.String("before", "", "include events at or before RFC3339 timestamp")
	format := flag.String("format", "text", "output format: text or json")
	flag.Parse()
	if *baselinePath == "" || *candidatePath == "" || flag.NArg() != 0 || (*format != "text" && *format != "json") {
		fmt.Fprintln(os.Stderr, "usage: go run ./scripts/reliabilitybench -baseline DIR -candidate DIR [-before RFC3339] [-format text|json]")
		os.Exit(2)
	}
	var before time.Time
	var err error
	if *beforeText != "" {
		before, err = time.Parse(time.RFC3339Nano, *beforeText)
		if err != nil {
			fmt.Fprintf(os.Stderr, "reliabilitybench: invalid -before: %v\n", err)
			os.Exit(2)
		}
	}
	baseline, err := session.AnalyzeCorpus(*baselinePath, session.AnalyzeOptions{Before: before})
	if err != nil {
		fmt.Fprintf(os.Stderr, "reliabilitybench: baseline: %v\n", err)
		os.Exit(1)
	}
	candidate, err := session.AnalyzeCorpus(*candidatePath, session.AnalyzeOptions{Before: before})
	if err != nil {
		fmt.Fprintf(os.Stderr, "reliabilitybench: candidate: %v\n", err)
		os.Exit(1)
	}
	result := buildComparison(baseline, candidate)
	if *format == "json" {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(result); err != nil {
			fmt.Fprintf(os.Stderr, "reliabilitybench: output: %v\n", err)
			os.Exit(1)
		}
		return
	}
	if err := writeComparisonText(os.Stdout, result); err != nil {
		fmt.Fprintf(os.Stderr, "reliabilitybench: output: %v\n", err)
		os.Exit(1)
	}
}

func buildComparison(baseline, candidate session.AnalysisReport) comparison {
	result := comparison{
		Version:   comparisonVersion,
		Baseline:  summarizeCorpus(baseline),
		Candidate: summarizeCorpus(candidate),
	}
	add := func(name, unit, direction string, baselineValue, candidateValue float64, available bool) {
		metric := comparisonMetric{Name: name, Unit: unit, Direction: direction, Available: available}
		if available {
			metric.Baseline = baselineValue
			metric.Candidate = candidateValue
			metric.Delta = candidateValue - baselineValue
		}
		result.Metrics = append(result.Metrics, metric)
	}
	add("sessions", "count", "neutral", float64(baseline.Sessions), float64(candidate.Sessions), true)
	addRate := func(name, direction string, bn, bd, cn, cd int, available bool) {
		available = available && bd > 0 && cd > 0
		var bv, cv float64
		if available {
			bv, cv = 100*float64(bn)/float64(bd), 100*float64(cn)/float64(cd)
		}
		add(name, "percent", direction, bv, cv, available)
	}
	addRate("prompt completion", "higher", baseline.Execution.CompletedPrompts, baseline.Execution.Prompts, candidate.Execution.CompletedPrompts, candidate.Execution.Prompts, baseline.Execution.Completeness == "complete" && candidate.Execution.Completeness == "complete")
	addRate("tool error", "lower", baseline.Execution.ToolErrors, baseline.Execution.ToolResults, candidate.Execution.ToolErrors, candidate.Execution.ToolResults, true)
	addRate("turn-budget exhaustion", "lower", baseline.Telemetry.Closure.TurnBudgetExhausted, baseline.Telemetry.Closure.Prompts, candidate.Telemetry.Closure.TurnBudgetExhausted, candidate.Telemetry.Closure.Prompts, baseline.Telemetry.Closure.Available && candidate.Telemetry.Closure.Available)
	addRate("workflow status supplied", "higher", baseline.Telemetry.Workflow.Supplied, baseline.Telemetry.Workflow.Prompts, candidate.Telemetry.Workflow.Supplied, candidate.Telemetry.Workflow.Prompts, baseline.Telemetry.Workflow.Available && candidate.Telemetry.Workflow.Available)
	add("max inspection/no-progress streak", "turns", "lower", float64(baseline.Telemetry.Progress.MaxInspectionNoProgressStreak.Value), float64(candidate.Telemetry.Progress.MaxInspectionNoProgressStreak.Value), baseline.Telemetry.Progress.MaxInspectionNoProgressStreak.Available && candidate.Telemetry.Progress.MaxInspectionNoProgressStreak.Available)
	add("turns to first successful mutation", "turns", "lower", float64(baseline.Telemetry.Progress.TurnsToFirstMutation.Value), float64(candidate.Telemetry.Progress.TurnsToFirstMutation.Value), baseline.Telemetry.Progress.TurnsToFirstMutation.Observed && candidate.Telemetry.Progress.TurnsToFirstMutation.Observed)
	add("turns to first successful verification", "turns", "lower", float64(baseline.Telemetry.Progress.TurnsToFirstVerification.Value), float64(candidate.Telemetry.Progress.TurnsToFirstVerification.Value), baseline.Telemetry.Progress.TurnsToFirstVerification.Observed && candidate.Telemetry.Progress.TurnsToFirstVerification.Observed)
	addRate("batching-steer compliance", "higher", baseline.Telemetry.Progress.BatchingCompliant, baseline.Telemetry.Progress.BatchingSteers-baseline.Telemetry.Progress.BatchingPending, candidate.Telemetry.Progress.BatchingCompliant, candidate.Telemetry.Progress.BatchingSteers-candidate.Telemetry.Progress.BatchingPending, baseline.Telemetry.Progress.Available && candidate.Telemetry.Progress.Available)
	addRate("hook timeout", "lower", baseline.Telemetry.Hooks.Timeouts, baseline.Telemetry.Hooks.Diagnostics, candidate.Telemetry.Hooks.Timeouts, candidate.Telemetry.Hooks.Diagnostics, baseline.Telemetry.Hooks.Available && candidate.Telemetry.Hooks.Available)
	add("negative-context violations", "count", "lower", float64(baseline.Telemetry.Invariants.NegativeContextViolations), float64(candidate.Telemetry.Invariants.NegativeContextViolations), (baseline.Telemetry.Invariants.ContextAvailable || baseline.Telemetry.Invariants.RetentionAvailable) && (candidate.Telemetry.Invariants.ContextAvailable || candidate.Telemetry.Invariants.RetentionAvailable))
	add("inconsistent-retention violations", "count", "lower", float64(baseline.Telemetry.Invariants.InconsistentRetentionViolations), float64(candidate.Telemetry.Invariants.InconsistentRetentionViolations), baseline.Telemetry.Invariants.RetentionAvailable && candidate.Telemetry.Invariants.RetentionAvailable)
	return result
}

func summarizeCorpus(report session.AnalysisReport) corpusSummary {
	return corpusSummary{
		Path: report.Path, Before: report.Before, Roots: report.Roots, Sessions: report.Sessions,
		MissingStreams: report.MissingStreams, IncompleteStreams: report.IncompleteStreams,
		MalformedStreams: report.MalformedStreams, SymlinkStreams: report.SymlinkStreams,
		MalformedChildMetadata: report.MalformedChildMetadata, Completeness: report.Completeness,
		Execution: report.Execution, Telemetry: report.Telemetry, Coverage: report.Coverage,
	}
}

func writeComparisonText(w io.Writer, result comparison) error {
	var b strings.Builder
	fmt.Fprintf(&b, "Reliability benchmark comparison v%d\n", result.Version)
	fmt.Fprintf(&b, "  baseline:  %s (%d sessions)\n", result.Baseline.Path, result.Baseline.Sessions)
	fmt.Fprintf(&b, "  candidate: %s (%d sessions)\n", result.Candidate.Path, result.Candidate.Sessions)
	b.WriteString("  delta = candidate - baseline\n\n")
	fmt.Fprintf(&b, "%-42s %12s %12s %12s\n", "metric", "baseline", "candidate", "delta")
	for _, metric := range result.Metrics {
		if !metric.Available {
			fmt.Fprintf(&b, "%-42s %12s %12s %12s\n", metric.Name, "unavailable", "unavailable", "—")
			continue
		}
		format := "%.0f"
		if metric.Unit == "percent" {
			format = "%.1f%%"
		}
		base := fmt.Sprintf(format, metric.Baseline)
		candidate := fmt.Sprintf(format, metric.Candidate)
		delta := fmt.Sprintf(format, metric.Delta)
		fmt.Fprintf(&b, "%-42s %12s %12s %12s\n", metric.Name, base, candidate, delta)
	}
	_, err := io.WriteString(w, b.String())
	return err
}
