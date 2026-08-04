// Command reliabilitybench compares transcript-free reliability telemetry from
// two harness session corpora.
package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"harness/internal/session"
)

const (
	comparisonVersion      = 2
	defaultMinimumMatched  = 3
	maxFixtureOutcomeBytes = 8 << 20
)

type corpusSummary struct {
	Path                   string                     `json:"path"`
	Before                 *time.Time                 `json:"before"`
	Roots                  int                        `json:"roots"`
	Sessions               int                        `json:"sessions"`
	MissingStreams         int                        `json:"missing_streams"`
	IncompleteStreams      int                        `json:"incomplete_streams"`
	MalformedStreams       int                        `json:"malformed_streams"`
	SymlinkStreams         int                        `json:"symlink_streams"`
	LimitExceededStreams   int                        `json:"limit_exceeded_streams"`
	MalformedChildMetadata int                        `json:"malformed_child_metadata"`
	Completeness           map[string]int             `json:"completeness"`
	Execution              session.ExecutionAnalysis  `json:"execution"`
	Telemetry              session.TelemetryAnalysis  `json:"telemetry"`
	Coverage               session.TelemetryCoverage  `json:"telemetry_coverage"`
	Usage                  session.UsageAnalysis      `json:"usage"`
	Storage                session.StorageAnalysis    `json:"storage"`
	Distributions          session.UsageDistributions `json:"distributions"`
	Cohorts                []session.CohortAnalysis   `json:"cohorts"`
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

type fixtureOutcome struct {
	TaskCompleted        bool `json:"task_completed"`
	ExpectedStateMatches bool `json:"expected_state_matches"`
}

type modelIdentity struct {
	Scope    string `json:"scope"`
	Agent    string `json:"agent,omitempty"`
	Provider string `json:"provider"`
	Model    string `json:"model"`
	Count    int    `json:"count"`
}

type fixtureMetrics struct {
	Available                 bool                   `json:"available"`
	IdentityAvailable         bool                   `json:"identity_available"`
	Provider                  string                 `json:"provider,omitempty"`
	Model                     string                 `json:"model,omitempty"`
	Models                    []modelIdentity        `json:"models,omitempty"`
	Cohort                    session.CohortIdentity `json:"cohort"`
	ReconciliationAvailable   bool                   `json:"reconciliation_available"`
	ReconciliationMatches     bool                   `json:"reconciliation_matches"`
	CompletionSourceAvailable bool                   `json:"completion_source_available"`
	PromptCompletionPercent   float64                `json:"prompt_completion_percent,omitempty"`
	InclusiveTokens           int                    `json:"inclusive_tokens,omitempty"`
	RootTokens                int                    `json:"root_tokens,omitempty"`
	DescendantTokens          int                    `json:"descendant_tokens,omitempty"`
	KnownCostUSD              float64                `json:"known_cost_usd,omitempty"`
	CostComplete              bool                   `json:"cost_complete"`
	StorageBytes              int64                  `json:"storage_bytes,omitempty"`
	OutcomeAvailable          bool                   `json:"outcome_available"`
	TaskCompleted             bool                   `json:"task_completed"`
	ExpectedStateMatches      bool                   `json:"expected_state_matches"`
}

type fixtureComparison struct {
	Fixture            string         `json:"fixture"`
	Matched            bool           `json:"matched"`
	BaselineAvailable  bool           `json:"baseline_available"`
	CandidateAvailable bool           `json:"candidate_available"`
	BaselineAmbiguous  bool           `json:"baseline_ambiguous"`
	CandidateAmbiguous bool           `json:"candidate_ambiguous"`
	Baseline           fixtureMetrics `json:"baseline"`
	Candidate          fixtureMetrics `json:"candidate"`
}

type promotionVerdict struct {
	Status          string `json:"status"`
	Reason          string `json:"reason"`
	MatchedSamples  int    `json:"matched_samples"`
	RequiredSamples int    `json:"required_samples"`
}

type comparison struct {
	Version   int                 `json:"version"`
	Baseline  corpusSummary       `json:"baseline"`
	Candidate corpusSummary       `json:"candidate"`
	Metrics   []comparisonMetric  `json:"metrics"`
	Fixtures  []fixtureComparison `json:"fixtures"`
	Verdict   promotionVerdict    `json:"verdict"`
}

func main() {
	baselinePath := flag.String("baseline", "", "baseline session or corpus directory")
	candidatePath := flag.String("candidate", "", "candidate session or corpus directory")
	baselineOutcomesPath := flag.String("baseline-outcomes", "", "optional JSON fixture outcome map for the baseline")
	candidateOutcomesPath := flag.String("candidate-outcomes", "", "optional JSON fixture outcome map for the candidate")
	minimumMatched := flag.Int("min-matched", defaultMinimumMatched, "minimum matched fixtures required for an automatic verdict")
	beforeText := flag.String("before", "", "include events at or before RFC3339 timestamp")
	format := flag.String("format", "text", "output format: text or json")
	flag.Parse()
	if *baselinePath == "" || *candidatePath == "" || *minimumMatched <= 0 || flag.NArg() != 0 || (*format != "text" && *format != "json") {
		fmt.Fprintln(os.Stderr, "usage: go run ./scripts/reliabilitybench -baseline DIR -candidate DIR [-baseline-outcomes FILE -candidate-outcomes FILE] [-min-matched N] [-before RFC3339] [-format text|json]")
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
	baselineOutcomes, err := readFixtureOutcomes(*baselineOutcomesPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "reliabilitybench: baseline outcomes: %v\n", err)
		os.Exit(1)
	}
	candidateOutcomes, err := readFixtureOutcomes(*candidateOutcomesPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "reliabilitybench: candidate outcomes: %v\n", err)
		os.Exit(1)
	}
	result := buildComparisonWithInputs(baseline, candidate, baselineOutcomes, candidateOutcomes, *minimumMatched)
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

func readFixtureOutcomes(path string) (map[string]fixtureOutcome, error) {
	if path == "" {
		return nil, nil
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, maxFixtureOutcomeBytes+1))
	if err != nil {
		return nil, err
	}
	if len(data) > maxFixtureOutcomeBytes {
		return nil, fmt.Errorf("outcome file exceeds %d bytes", maxFixtureOutcomeBytes)
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	start, err := dec.Token()
	if err != nil {
		return nil, err
	}
	if delim, ok := start.(json.Delim); !ok || delim != '{' {
		return nil, fmt.Errorf("outcome file must be a JSON object")
	}
	outcomes := make(map[string]fixtureOutcome)
	for dec.More() {
		token, err := dec.Token()
		if err != nil {
			return nil, err
		}
		key, ok := token.(string)
		if !ok {
			return nil, fmt.Errorf("outcome fixture key is not a string")
		}
		if _, duplicate := outcomes[key]; duplicate {
			return nil, fmt.Errorf("duplicate outcome fixture %q", key)
		}
		outcome, err := decodeFixtureOutcome(dec, key)
		if err != nil {
			return nil, err
		}
		outcomes[key] = outcome
	}
	if _, err := dec.Token(); err != nil {
		return nil, err
	}
	var extra any
	if err := dec.Decode(&extra); err != io.EOF {
		if err == nil {
			return nil, fmt.Errorf("multiple JSON values")
		}
		return nil, err
	}
	return outcomes, nil
}

func decodeFixtureOutcome(dec *json.Decoder, fixture string) (fixtureOutcome, error) {
	start, err := dec.Token()
	if err != nil {
		return fixtureOutcome{}, err
	}
	if delim, ok := start.(json.Delim); !ok || delim != '{' {
		return fixtureOutcome{}, fmt.Errorf("outcome fixture %q must be a JSON object", fixture)
	}
	var outcome fixtureOutcome
	seen := make(map[string]bool, 2)
	for dec.More() {
		token, err := dec.Token()
		if err != nil {
			return fixtureOutcome{}, err
		}
		field, ok := token.(string)
		if !ok {
			return fixtureOutcome{}, fmt.Errorf("outcome fixture %q has a non-string field", fixture)
		}
		if seen[field] {
			return fixtureOutcome{}, fmt.Errorf("outcome fixture %q has duplicate field %q", fixture, field)
		}
		seen[field] = true
		if field != "task_completed" && field != "expected_state_matches" {
			return fixtureOutcome{}, fmt.Errorf("outcome fixture %q has unknown field %q", fixture, field)
		}
		valueToken, err := dec.Token()
		if err != nil {
			return fixtureOutcome{}, err
		}
		value, ok := valueToken.(bool)
		if !ok {
			return fixtureOutcome{}, fmt.Errorf("outcome fixture %q field %q must be a boolean", fixture, field)
		}
		if field == "task_completed" {
			outcome.TaskCompleted = value
		} else {
			outcome.ExpectedStateMatches = value
		}
	}
	if _, err := dec.Token(); err != nil {
		return fixtureOutcome{}, err
	}
	if !seen["task_completed"] || !seen["expected_state_matches"] {
		return fixtureOutcome{}, fmt.Errorf("outcome fixture %q must supply task_completed and expected_state_matches", fixture)
	}
	return outcome, nil
}

func buildComparison(baseline, candidate session.AnalysisReport) comparison {
	return buildComparisonWithInputs(baseline, candidate, nil, nil, defaultMinimumMatched)
}

func buildComparisonWithInputs(baseline, candidate session.AnalysisReport, baselineOutcomes, candidateOutcomes map[string]fixtureOutcome, minimumMatched int) comparison {
	result := comparison{
		Version:  comparisonVersion,
		Baseline: summarizeCorpus(baseline), Candidate: summarizeCorpus(candidate),
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
	add("usage-reconciliation violations", "count", "lower", float64(baseline.Telemetry.Invariants.UsageReconciliationViolations), float64(candidate.Telemetry.Invariants.UsageReconciliationViolations), baseline.Telemetry.Invariants.UsageReconciliationAvailable && candidate.Telemetry.Invariants.UsageReconciliationAvailable)
	usageAvailable := baseline.Usage.Inclusive.Available && baseline.Usage.Inclusive.Complete && candidate.Usage.Inclusive.Available && candidate.Usage.Inclusive.Complete
	add("uncached input tokens", "tokens", "lower", float64(baseline.Usage.Inclusive.InputTokens), float64(candidate.Usage.Inclusive.InputTokens), usageAvailable)
	add("cache-read tokens", "tokens", "lower", float64(baseline.Usage.Inclusive.CacheReadTokens), float64(candidate.Usage.Inclusive.CacheReadTokens), usageAvailable)
	addDistributionMetrics(add, baseline.Distributions, candidate.Distributions)
	storageAvailable := storageAnalysisComplete(baseline) && storageAnalysisComplete(candidate)
	add("context reset entries", "count", "lower", float64(baseline.Storage.ContextResetEntries), float64(candidate.Storage.ContextResetEntries), storageAvailable)
	add("snapshot reset entries", "count", "lower", float64(baseline.Storage.SnapshotResetEntries), float64(candidate.Storage.SnapshotResetEntries), storageAvailable)
	add("delta reset entries", "count", "lower", float64(baseline.Storage.DeltaResetEntries), float64(candidate.Storage.DeltaResetEntries), storageAvailable)
	add("snapshot reset payload", "bytes", "lower", float64(baseline.Storage.SnapshotPayloadBytes), float64(candidate.Storage.SnapshotPayloadBytes), storageAvailable)
	add("delta reset payload", "bytes", "lower", float64(baseline.Storage.DeltaPayloadBytes), float64(candidate.Storage.DeltaPayloadBytes), storageAvailable)
	add("session storage", "bytes", "lower", float64(baseline.Storage.TotalBytes), float64(candidate.Storage.TotalBytes), storageAvailable)

	result.Fixtures = buildFixtureComparisons(baseline, candidate, baselineOutcomes, candidateOutcomes)
	result.Verdict = evaluatePromotion(result.Fixtures, minimumMatched)
	if baseline.Before != nil || candidate.Before != nil {
		result.Verdict.Status = "insufficient_data"
		result.Verdict.Reason = "cutoff-prefix corpora cannot receive an automatic verdict"
	}
	return result
}

func storageAnalysisComplete(report session.AnalysisReport) bool {
	if report.Before != nil || !report.Storage.Available {
		return false
	}
	for _, status := range []string{
		report.Storage.State.Status,
		report.Storage.Tree.Status,
		report.Storage.Raw.Status,
		report.Storage.Compactions.Status,
		report.Storage.ToolResults.Status,
	} {
		if status != "complete" && status != "missing" {
			return false
		}
	}
	return true
}

func addDistributionMetrics(add func(string, string, string, float64, float64, bool), baseline, candidate session.UsageDistributions) {
	addInt := func(name string, b, c session.IntDistribution, field func(session.IntDistribution) int) {
		add(name, "tokens", "lower", float64(field(b)), float64(field(c)), b.Samples > 0 && c.Samples > 0)
	}
	addInt("inclusive tokens median", baseline.InclusiveTokens, candidate.InclusiveTokens, func(v session.IntDistribution) int { return v.Median })
	addInt("inclusive tokens p90", baseline.InclusiveTokens, candidate.InclusiveTokens, func(v session.IntDistribution) int { return v.P90 })
	addInt("root tokens median", baseline.RootTokens, candidate.RootTokens, func(v session.IntDistribution) int { return v.Median })
	addInt("root tokens p90", baseline.RootTokens, candidate.RootTokens, func(v session.IntDistribution) int { return v.P90 })
	addInt("descendant tokens median", baseline.DescendantTokens, candidate.DescendantTokens, func(v session.IntDistribution) int { return v.Median })
	addInt("descendant tokens p90", baseline.DescendantTokens, candidate.DescendantTokens, func(v session.IntDistribution) int { return v.P90 })
	add("inclusive known cost median", "usd", "lower", baseline.InclusiveKnownCostUSD.Median, candidate.InclusiveKnownCostUSD.Median, baseline.InclusiveKnownCostUSD.Samples > 0 && candidate.InclusiveKnownCostUSD.Samples > 0)
	add("inclusive known cost p90", "usd", "lower", baseline.InclusiveKnownCostUSD.P90, candidate.InclusiveKnownCostUSD.P90, baseline.InclusiveKnownCostUSD.Samples > 0 && candidate.InclusiveKnownCostUSD.Samples > 0)
}

func summarizeCorpus(report session.AnalysisReport) corpusSummary {
	return corpusSummary{
		Path: report.Path, Before: report.Before, Roots: report.Roots, Sessions: report.Sessions,
		MissingStreams: report.MissingStreams, IncompleteStreams: report.IncompleteStreams,
		MalformedStreams: report.MalformedStreams, SymlinkStreams: report.SymlinkStreams,
		LimitExceededStreams:   report.LimitExceededStreams,
		MalformedChildMetadata: report.MalformedChildMetadata, Completeness: report.Completeness,
		Execution: report.Execution, Telemetry: report.Telemetry, Coverage: report.Coverage,
		Usage: report.Usage, Storage: report.Storage, Distributions: report.Distributions, Cohorts: report.Cohorts,
	}
}

func buildFixtureComparisons(baseline, candidate session.AnalysisReport, baselineOutcomes, candidateOutcomes map[string]fixtureOutcome) []fixtureComparison {
	baselineFixtures, baselineAmbiguous := hierarchyByFixture(baseline)
	candidateFixtures, candidateAmbiguous := hierarchyByFixture(candidate)
	keys := make(map[string]struct{}, len(baselineFixtures)+len(candidateFixtures)+len(baselineAmbiguous)+len(candidateAmbiguous))
	for key := range baselineFixtures {
		keys[key] = struct{}{}
	}
	for key := range candidateFixtures {
		keys[key] = struct{}{}
	}
	for key := range baselineAmbiguous {
		keys[key] = struct{}{}
	}
	for key := range candidateAmbiguous {
		keys[key] = struct{}{}
	}
	ordered := make([]string, 0, len(keys))
	for key := range keys {
		ordered = append(ordered, key)
	}
	sort.Strings(ordered)
	rows := make([]fixtureComparison, 0, len(ordered))
	for _, key := range ordered {
		b, bok := baselineFixtures[key]
		c, cok := candidateFixtures[key]
		bamb, camb := baselineAmbiguous[key], candidateAmbiguous[key]
		row := fixtureComparison{
			Fixture: key, Matched: bok && cok && !bamb && !camb,
			BaselineAvailable: bok, CandidateAvailable: cok,
			BaselineAmbiguous: bamb, CandidateAmbiguous: camb,
		}
		if bok {
			row.Baseline = hierarchyMetrics(b, baseline, baselineOutcomes, key)
		}
		if cok {
			row.Candidate = hierarchyMetrics(c, candidate, candidateOutcomes, key)
		}
		rows = append(rows, row)
	}
	return rows
}

func hierarchyByFixture(report session.AnalysisReport) (map[string]session.HierarchyAnalysis, map[string]bool) {
	out := make(map[string]session.HierarchyAnalysis, len(report.Hierarchies))
	ambiguous := make(map[string]bool)
	for _, hierarchy := range report.Hierarchies {
		key := filepath.Base(filepath.Clean(hierarchy.RootPath))
		if key == "." || key == string(filepath.Separator) || key == "" {
			key = "root"
		}
		if _, exists := out[key]; exists {
			delete(out, key)
			ambiguous[key] = true
			continue
		}
		if !ambiguous[key] {
			out[key] = hierarchy
		}
	}
	return out, ambiguous
}

func rootItem(report session.AnalysisReport, rootPath string) session.SessionAnalysis {
	for _, item := range report.Items {
		if filepath.Clean(item.Path) == filepath.Clean(rootPath) {
			return item
		}
	}
	return session.SessionAnalysis{}
}

func hierarchyModelIdentity(report session.AnalysisReport, hierarchy session.HierarchyAnalysis) ([]modelIdentity, bool) {
	counts := make(map[modelIdentity]int)
	items := 0
	for _, item := range report.Items {
		if filepath.Clean(item.RootPath) != filepath.Clean(hierarchy.RootPath) {
			continue
		}
		items++
		execution := item.ExecutionIdentity
		if !execution.Available || !execution.Stable ||
			execution.Agent == "" || execution.Provider == "" || execution.Model == "" ||
			execution.Agent != item.Agent || execution.Provider != item.Provider || execution.Model != item.Model {
			return nil, false
		}
		identity := modelIdentity{Scope: "descendant", Agent: execution.Agent, Provider: execution.Provider, Model: execution.Model}
		if filepath.Clean(item.Path) == filepath.Clean(hierarchy.RootPath) {
			identity.Scope = "root"
		}
		counts[identity]++
	}
	if items == 0 || items != hierarchy.Sessions {
		return nil, false
	}
	identities := make([]modelIdentity, 0, len(counts))
	for identity, count := range counts {
		identity.Count = count
		identities = append(identities, identity)
	}
	sort.Slice(identities, func(i, j int) bool {
		a, b := identities[i], identities[j]
		if a.Scope != b.Scope {
			return a.Scope < b.Scope
		}
		if a.Agent != b.Agent {
			return a.Agent < b.Agent
		}
		if a.Provider != b.Provider {
			return a.Provider < b.Provider
		}
		return a.Model < b.Model
	})
	return identities, true
}

func sameModelIdentity(a, b []modelIdentity) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func hierarchyMetrics(hierarchy session.HierarchyAnalysis, report session.AnalysisReport, outcomes map[string]fixtureOutcome, key string) fixtureMetrics {
	rootItem := rootItem(report, hierarchy.RootPath)
	models, identityAvailable := hierarchyModelIdentity(report, hierarchy)
	rootUsage := addUsage(hierarchy.Usage.RootConversational, hierarchy.Usage.RootMaintenance)
	children := addUsage(hierarchy.Usage.DescendantConversational, hierarchy.Usage.DescendantMaintenance)
	metrics := fixtureMetrics{
		Available:                 hierarchy.Execution.Completeness == "complete" && hierarchy.Usage.Inclusive.Available && hierarchy.Usage.Inclusive.Complete,
		IdentityAvailable:         identityAvailable,
		Provider:                  rootItem.Provider,
		Model:                     rootItem.Model,
		Models:                    models,
		Cohort:                    hierarchy.Cohort,
		ReconciliationAvailable:   hierarchy.Usage.Reconciliation.Available,
		ReconciliationMatches:     hierarchy.Usage.Reconciliation.Matches,
		CompletionSourceAvailable: hierarchy.Workflow.CompletionSourceAvailable && hierarchy.Workflow.CompletionSourceReports > 0,
		InclusiveTokens:           hierarchy.Usage.Inclusive.TotalTokens,
		RootTokens:                rootUsage.TotalTokens, DescendantTokens: children.TotalTokens,
		KnownCostUSD: hierarchy.Usage.Inclusive.KnownCostUSD,
		CostComplete: hierarchy.Usage.Inclusive.CostComplete,
		StorageBytes: hierarchy.Storage.TotalBytes,
	}
	if hierarchy.Execution.Prompts > 0 {
		metrics.PromptCompletionPercent = 100 * float64(hierarchy.Execution.CompletedPrompts) / float64(hierarchy.Execution.Prompts)
	}
	if outcome, ok := outcomes[key]; ok {
		metrics.OutcomeAvailable = true
		metrics.TaskCompleted = outcome.TaskCompleted
		metrics.ExpectedStateMatches = outcome.ExpectedStateMatches
	}
	return metrics
}

func addUsage(a, b session.UsageSlice) session.UsageSlice {
	return session.UsageSlice{
		Available:          a.Available || b.Available,
		InputTokens:        a.InputTokens + b.InputTokens,
		CacheReadTokens:    a.CacheReadTokens + b.CacheReadTokens,
		CacheWriteTokens:   a.CacheWriteTokens + b.CacheWriteTokens,
		CacheWrite1hTokens: a.CacheWrite1hTokens + b.CacheWrite1hTokens,
		OutputTokens:       a.OutputTokens + b.OutputTokens,
		ReasoningTokens:    a.ReasoningTokens + b.ReasoningTokens,
		TotalTokens:        a.TotalTokens + b.TotalTokens,
		KnownCostUSD:       a.KnownCostUSD + b.KnownCostUSD,
		CostComplete:       a.CostComplete && b.CostComplete,
	}
}

func evaluatePromotion(rows []fixtureComparison, minimumMatched int) promotionVerdict {
	verdict := promotionVerdict{Status: "insufficient_data", RequiredSamples: minimumMatched}
	for _, row := range rows {
		if !row.Matched {
			verdict.Reason = "fixture sets differ or contain ambiguous basenames"
			return verdict
		}
		verdict.MatchedSamples++
	}
	if verdict.MatchedSamples < minimumMatched {
		verdict.Reason = "insufficient matching fixture sample size"
		return verdict
	}
	var baselineTokens, candidateTokens []int
	var baselineCosts, candidateCosts []float64
	for _, row := range rows {
		if !row.Baseline.Available || !row.Candidate.Available {
			verdict.Reason = "matched fixture usage is incomplete"
			return verdict
		}
		if !row.Baseline.IdentityAvailable || !row.Candidate.IdentityAvailable {
			verdict.Reason = "matched fixture execution identity is unavailable"
			return verdict
		}
		if !sameModelIdentity(row.Baseline.Models, row.Candidate.Models) {
			verdict.Reason = "matched fixture execution identity differs"
			return verdict
		}
		if row.Baseline.ReconciliationAvailable && !row.Baseline.ReconciliationMatches {
			verdict.Reason = "baseline usage reconciliation failed"
			return verdict
		}
		if row.Candidate.ReconciliationAvailable && !row.Candidate.ReconciliationMatches {
			verdict.Status = "reject"
			verdict.Reason = "candidate usage reconciliation failed"
			return verdict
		}
		if !row.Baseline.OutcomeAvailable || !row.Candidate.OutcomeAvailable {
			verdict.Reason = "task and expected repository-state outcomes are required"
			return verdict
		}
		if !row.Baseline.CostComplete || !row.Candidate.CostComplete {
			verdict.Reason = "inclusive cost coverage is incomplete"
			return verdict
		}
		if !row.Candidate.TaskCompleted || !row.Candidate.ExpectedStateMatches {
			verdict.Status = "reject"
			verdict.Reason = "candidate did not complete the task with the expected repository state"
			return verdict
		}
		baselineTokens = append(baselineTokens, row.Baseline.InclusiveTokens)
		candidateTokens = append(candidateTokens, row.Candidate.InclusiveTokens)
		baselineCosts = append(baselineCosts, row.Baseline.KnownCostUSD)
		candidateCosts = append(candidateCosts, row.Candidate.KnownCostUSD)
	}
	if nearestRankInt(candidateTokens, .5) > nearestRankInt(baselineTokens, .5) || nearestRankInt(candidateTokens, .9) > nearestRankInt(baselineTokens, .9) || nearestRankFloat(candidateCosts, .5) > nearestRankFloat(baselineCosts, .5) || nearestRankFloat(candidateCosts, .9) > nearestRankFloat(baselineCosts, .9) {
		verdict.Status = "reject"
		verdict.Reason = "candidate regressed median or p90 inclusive token/cost usage"
		return verdict
	}
	verdict.Status = "promote"
	verdict.Reason = "correctness gates passed and inclusive token/cost distributions did not regress"
	return verdict
}

func nearestRankInt(values []int, percentile float64) int {
	values = append([]int(nil), values...)
	sort.Ints(values)
	return values[int(math.Ceil(percentile*float64(len(values))))-1]
}

func nearestRankFloat(values []float64, percentile float64) float64 {
	values = append([]float64(nil), values...)
	sort.Float64s(values)
	return values[int(math.Ceil(percentile*float64(len(values))))-1]
}

func writeComparisonText(w io.Writer, result comparison) error {
	var b strings.Builder
	fmt.Fprintf(&b, "Reliability benchmark comparison v%d\n", result.Version)
	fmt.Fprintf(&b, "  baseline:  %s (%d roots, %d sessions)\n", result.Baseline.Path, result.Baseline.Roots, result.Baseline.Sessions)
	fmt.Fprintf(&b, "  candidate: %s (%d roots, %d sessions)\n", result.Candidate.Path, result.Candidate.Roots, result.Candidate.Sessions)
	fmt.Fprintf(&b, "  verdict: %s (%s; matched %d, required %d)\n", result.Verdict.Status, result.Verdict.Reason, result.Verdict.MatchedSamples, result.Verdict.RequiredSamples)
	b.WriteString("  delta = candidate - baseline\n\n")
	fmt.Fprintf(&b, "%-42s %12s %12s %12s\n", "metric", "baseline", "candidate", "delta")
	for _, metric := range result.Metrics {
		if !metric.Available {
			fmt.Fprintf(&b, "%-42s %12s %12s %12s\n", metric.Name, "unavailable", "unavailable", "—")
			continue
		}
		format := "%.0f"
		switch metric.Unit {
		case "percent":
			format = "%.1f%%"
		case "usd":
			format = "$%.6f"
		}
		base := fmt.Sprintf(format, metric.Baseline)
		candidate := fmt.Sprintf(format, metric.Candidate)
		delta := fmt.Sprintf(format, metric.Delta)
		fmt.Fprintf(&b, "%-42s %12s %12s %12s\n", metric.Name, base, candidate, delta)
	}
	b.WriteString("\nCohorts\n")
	writeCohorts := func(label string, cohorts []session.CohortAnalysis) {
		if len(cohorts) == 0 {
			fmt.Fprintf(&b, "  %s: unavailable\n", label)
			return
		}
		for i, cohort := range cohorts {
			if i == 20 {
				fmt.Fprintf(&b, "  %s: ... %d more cohorts\n", label, len(cohorts)-i)
				break
			}
			build := cohort.Cohort.Build.Version
			if cohort.Cohort.Build.Commit != "" {
				build += "@" + cohort.Cohort.Build.Commit
			}
			if cohort.Cohort.Build.Modified {
				build += "+modified"
			}
			fmt.Fprintf(&b, "  %s: key=%s build=%s roots=%d sessions=%d retention=%s context=%d\n", label, cohort.Cohort.Key, build, cohort.Roots, cohort.Sessions, cohort.Cohort.Runtime.RetentionPolicy, cohort.Cohort.Runtime.ContextWindow)
		}
	}
	writeCohorts("baseline", result.Baseline.Cohorts)
	writeCohorts("candidate", result.Candidate.Cohorts)
	b.WriteString("\nFixture-matched hierarchy rows\n")
	for _, row := range result.Fixtures {
		if !row.Matched {
			fmt.Fprintf(&b, "  %s: unmatched (baseline=%t candidate=%t; ambiguous=%t/%t)\n", row.Fixture, row.BaselineAvailable, row.CandidateAvailable, row.BaselineAmbiguous, row.CandidateAmbiguous)
			continue
		}
		fmt.Fprintf(&b, "  %s: %s/%s; cohort %s -> %s; tokens %d -> %d (root %d -> %d, child %d -> %d); cost $%.6f -> $%.6f; correctness %t/%t -> %t/%t\n", row.Fixture, row.Baseline.Provider, row.Baseline.Model, row.Baseline.Cohort.Key, row.Candidate.Cohort.Key, row.Baseline.InclusiveTokens, row.Candidate.InclusiveTokens, row.Baseline.RootTokens, row.Candidate.RootTokens, row.Baseline.DescendantTokens, row.Candidate.DescendantTokens, row.Baseline.KnownCostUSD, row.Candidate.KnownCostUSD, row.Baseline.TaskCompleted, row.Baseline.ExpectedStateMatches, row.Candidate.TaskCompleted, row.Candidate.ExpectedStateMatches)
	}
	_, err := io.WriteString(w, b.String())
	return err
}
