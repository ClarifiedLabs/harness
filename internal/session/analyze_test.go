package session

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"harness/internal/llm"
)

func TestAnalyzeCorpusRecursiveDiscoveryAndCutoff(t *testing.T) {
	base := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)
	corpus := t.TempDir()
	root := filepath.Join(corpus, "root-a")
	mustAppendAnalysisEvent(t, root, Event{Time: base, Type: EventTurnProgress, Prompt: 1, Turn: 1, TurnProgress: &TurnProgressSnapshot{InspectionOnly: true, InspectionNoProgressRun: 2}})
	mustAppendAnalysisEvent(t, root, Event{Time: base.Add(2 * time.Hour), Type: EventTurnProgress, Prompt: 1, Turn: 2, TurnProgress: &TurnProgressSnapshot{SuccessfulMutation: true}})

	child := filepath.Join(root, "children", "child")
	mustWriteAnalysisJSON(t, filepath.Join(child, "meta.json"), ChildMeta{ID: "child", ParentID: "root", Agent: "explore"})
	mustAppendAnalysisEvent(t, child, Event{Time: base, Type: EventPromptUsage, Prompt: 1, ClosureTrigger: "turn_budget", TurnBudgetExhausted: true, TelemetryVersion: ReliabilityTelemetryVersion})
	nested := filepath.Join(child, "children", "nested")
	if err := os.MkdirAll(nested, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(nested, "meta.json"), []byte("{bad"), 0o644); err != nil {
		t.Fatal(err)
	}

	incomplete := filepath.Join(corpus, "root-b")
	mustAppendAnalysisEvent(t, incomplete, Event{Time: base, Type: EventToolResult, Tool: "read"})
	f, err := os.OpenFile(filepath.Join(incomplete, eventLog), os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = f.WriteString(`{"type":"turn_progress"`)
	_ = f.Close()

	external := filepath.Join(t.TempDir(), "linked-session")
	mustAppendAnalysisEvent(t, external, Event{Time: base, Type: EventTurnProgress, TurnProgress: &TurnProgressSnapshot{SuccessfulMutation: true}})
	if err := os.Symlink(external, filepath.Join(corpus, "linked")); err != nil {
		t.Fatal(err)
	}

	report, err := AnalyzeCorpus(corpus, AnalyzeOptions{Before: base.Add(time.Hour)})
	if err != nil {
		t.Fatalf("AnalyzeCorpus: %v", err)
	}
	if report.Roots != 2 || report.Sessions != 4 {
		t.Fatalf("roots/sessions = %d/%d, want 2/4: %+v", report.Roots, report.Sessions, report.Items)
	}
	if report.MissingStreams != 1 || report.IncompleteStreams != 1 || report.MalformedChildMetadata != 1 {
		t.Fatalf("stream diagnostics = missing %d incomplete %d malformed-meta %d", report.MissingStreams, report.IncompleteStreams, report.MalformedChildMetadata)
	}
	if report.Storage.Raw.Status != "incomplete" {
		t.Fatalf("aggregate raw storage status = %q, want incomplete", report.Storage.Raw.Status)
	}
	if !report.Telemetry.Progress.Available || report.Telemetry.Progress.ToolTurns != 1 || report.Telemetry.Progress.TurnsToFirstMutation.Observed {
		t.Fatalf("cutoff progress = %+v", report.Telemetry.Progress)
	}
	if report.Telemetry.Closure.TurnBudgetExhausted != 1 || report.Telemetry.Workflow.Unsupplied != 1 {
		t.Fatalf("closure/workflow = %+v / %+v", report.Telemetry.Closure, report.Telemetry.Workflow)
	}
	if report.Coverage.Sessions != 4 || report.Coverage.Progress != 2 || report.Coverage.Hooks != 1 || report.Coverage.Closure != 1 || report.Coverage.Workflow != 1 {
		t.Fatalf("coverage = %+v", report.Coverage)
	}
	if report.Execution.Prompts != 2 || report.Execution.CompletedPrompts != 1 || report.Completeness["complete"] != 1 || report.Completeness["incomplete"] != 2 || report.Completeness["unavailable"] != 1 {
		t.Fatalf("execution/completeness = %+v / %+v", report.Execution, report.Completeness)
	}
	for _, item := range report.Items {
		if strings.Contains(item.Path, "linked") {
			t.Fatalf("followed symlink: %s", item.Path)
		}
	}
}

func TestAnalyzeCorpusCountsInBandCommandFailures(t *testing.T) {
	root := t.TempDir()
	mustAppendAnalysisEvent(t, root, Event{Type: EventToolResult, Tool: "shell", ResultMetrics: map[string]int{"command_outcome_available": 1, "command_failed": 1}})
	mustAppendAnalysisEvent(t, root, Event{Type: EventToolResult, Tool: "shell", ResultMetrics: map[string]int{"command_outcome_available": 1, "command_cancelled": 1}})
	mustAppendAnalysisEvent(t, root, Event{Type: EventToolResult, Tool: "read", ResultError: true})
	report, err := AnalyzeCorpus(root, AnalyzeOptions{})
	if err != nil {
		t.Fatal(err)
	}
	got := report.Execution
	if got.ToolResults != 3 || got.ToolErrors != 1 || got.CommandResults != 2 || got.CommandFailures != 1 || got.CommandCancellations != 1 || got.EffectiveFailures != 2 {
		t.Fatalf("execution = %+v", got)
	}
}

func TestAnalyzeCorpusCountsBackgroundCommandDiagnosticsWithoutSecondToolResult(t *testing.T) {
	root := t.TempDir()
	mustAppendAnalysisEvent(t, root, Event{Type: EventToolStart, Tool: "shell"})
	mustAppendAnalysisEvent(t, root, Event{Type: EventToolResult, Tool: "shell"})
	mustAppendAnalysisEvent(t, root, Event{Type: EventBackgroundJobResult, Tool: "shell", ResultMetrics: map[string]int{
		"command_outcome_available": 1,
		"command_failed":            1,
		"command_steps_total":       2,
		"command_steps_failed":      1,
	}})
	report, err := AnalyzeCorpus(root, AnalyzeOptions{})
	if err != nil {
		t.Fatal(err)
	}
	got := report.Execution
	if got.ToolCalls != 1 || got.ToolResults != 1 || got.CommandResults != 1 ||
		got.CommandFailures != 1 || got.EffectiveFailures != 1 {
		t.Fatalf("background execution = %+v", got)
	}
}

func TestAnalyzeCorpusBoundsDiscoveryDepth(t *testing.T) {
	root := filepath.Join(t.TempDir(), "root")
	mustAppendAnalysisEvent(t, root, Event{Type: EventPromptUsage, Prompt: 1})
	deep := root
	for range maxAnalysisDiscoveryDepth + 2 {
		deep = filepath.Join(deep, "children", "c")
	}
	if err := os.MkdirAll(deep, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := AnalyzeCorpus(root, AnalyzeOptions{}); err == nil {
		t.Fatal("AnalyzeCorpus succeeded past the discovery depth bound")
	}
}

func TestReadAnalysisEventsUsesBoundedSnapshotAndDropsBodies(t *testing.T) {
	dir := t.TempDir()
	canary := "raw-event-body-must-not-be-retained"
	line, err := json.Marshal(Event{
		Type: EventModelRequest, Text: canary,
		ModelRequest: &llm.ModelRequestEvent{State: llm.ModelRequestFailed, Message: canary, ResponsePayload: llm.DiagnosticPayload(`{"canary":"body"}`)},
	})
	if err != nil {
		t.Fatal(err)
	}
	data := append(append(append([]byte(nil), line...), '\n'), append(line, '\n')...)
	if err := os.WriteFile(filepath.Join(dir, eventLog), data, 0o644); err != nil {
		t.Fatal(err)
	}

	events, source, err := readAnalysisEventsWithLimits(dir, time.Time{}, analysisEventLimits{
		maxBytes: int64(len(line) + 1), maxLine: len(line) + 1, maxRecords: 10,
	})
	if err != nil {
		t.Fatal(err)
	}
	if source.Status != "limit_exceeded" || source.SnapshotBytes != int64(len(data)) || len(events) != 1 || events[0].Text != "" {
		t.Fatalf("bounded snapshot = source=%+v events=%+v", source, events)
	}
	if events[0].ModelRequest.Message != "" || events[0].ModelRequest.ResponsePayload != "" {
		t.Fatalf("retained model-request body: %+v", events[0].ModelRequest)
	}
	storage, err := analyzeStorage(dir, source, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if storage.Raw.Status != "limit_exceeded" || storage.Raw.Bytes != int64(len(data)) {
		t.Fatalf("raw storage = %+v", storage.Raw)
	}

	events, source, err = readAnalysisEventsWithLimits(dir, time.Time{}, analysisEventLimits{
		maxBytes: int64(len(data)), maxLine: len(line) - 1, maxRecords: 10,
	})
	if err != nil {
		t.Fatal(err)
	}
	if source.Status != "limit_exceeded" || len(events) != 0 {
		t.Fatalf("line limit = source=%+v events=%d", source, len(events))
	}

	events, source, err = readAnalysisEventsWithLimits(dir, time.Time{}, analysisEventLimits{
		maxBytes: int64(len(data)), maxLine: len(line) + 1, maxRecords: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	if source.Status != "limit_exceeded" || len(events) != 1 {
		t.Fatalf("record limit = source=%+v events=%d", source, len(events))
	}
}

func TestAnalyzeStorageCountsCandidateLineageWithoutReadingContents(t *testing.T) {
	dir := t.TempDir()
	files := map[string]string{
		filepath.Join("lineage", "state.json"):                `{"candidate":"TOP SECRET"}`,
		filepath.Join("lineage", "base.patch"):                "base patch\n",
		filepath.Join("lineage", "patches", "0001.patch"):     "entry patch\n",
		filepath.Join("lineage", "evidence", "0001.evidence"): "private evidence\n",
	}
	var wantBytes int64
	for name, content := range files {
		path := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		wantBytes += int64(len(content))
	}
	storage, err := analyzeStorage(dir, AnalysisSource{Status: "missing"}, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if storage.Lineage.Status != "complete" || storage.Lineage.Files != len(files) || storage.Lineage.Bytes != wantBytes || storage.TotalBytes != wantBytes {
		t.Fatalf("lineage storage = %+v; total=%d", storage.Lineage, storage.TotalBytes)
	}
	encoded, err := json.Marshal(storage)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(encoded, []byte("TOP SECRET")) || bytes.Contains(encoded, []byte("private evidence")) {
		t.Fatalf("storage analysis exposed content: %s", encoded)
	}
}

func TestDeriveTelemetryAvailabilityAndMetrics(t *testing.T) {
	legacy := deriveTelemetry([]Event{{Type: EventToolResult}, {Type: EventPromptUsage, Prompt: 1}}, nil)
	if legacy.Progress.Available || legacy.Hooks.Available || legacy.Closure.Available || legacy.Workflow.Available {
		t.Fatalf("legacy telemetry reported available: %+v", legacy)
	}
	if legacy.Progress.TurnsToFirstMutation.Value != 0 || legacy.Progress.TurnsToFirstMutation.Observed {
		t.Fatalf("legacy mutation milestone looks observed: %+v", legacy.Progress.TurnsToFirstMutation)
	}

	events := []Event{
		{Type: EventTurnProgress, Prompt: 1, Turn: 1, TurnProgress: &TurnProgressSnapshot{InspectionOnly: true, InspectionNoProgressRun: 3, SteerReason: "batching"}},
		{Type: EventTurnProgress, Prompt: 1, Turn: 2, TurnProgress: &TurnProgressSnapshot{SuccessfulMutation: true}},
		{Type: EventTurnProgress, Prompt: 1, Turn: 3, TurnProgress: &TurnProgressSnapshot{BatchedOperationCount: 2, SuccessfulVerification: true}},
		{Type: EventClosure, Prompt: 1, ClosureTrigger: "turn_budget"},
		{Type: EventPromptUsage, Prompt: 1, ClosureTrigger: "turn_budget", TurnBudgetExhausted: true, WorkflowStatus: &WorkflowStatusSnapshot{Outcome: "partial"}, TelemetryVersion: ReliabilityTelemetryVersion},
		{Type: EventPromptUsage, Prompt: 2, TelemetryVersion: ReliabilityTelemetryVersion},
		{Type: EventHookDiagnostic, HookDiagnostic: &HookDiagnosticSnapshot{Outcome: "timeout", CircuitOpen: true}},
		{Type: EventHookDiagnostic, HookDiagnostic: &HookDiagnosticSnapshot{Outcome: "circuit_open"}},
		{Type: EventModelRequest, Context: &ContextSnapshot{Total: -1}},
		{Type: EventRetention, Retention: &RetentionSnapshot{BytesBefore: 10, BytesAfter: 12, LocalEstimateTokensBefore: 10, LocalEstimateTokensAfter: 8}},
		{Type: EventLineageAdvance, LineageAdvance: &LineageAdvanceSnapshot{Sequence: 1, PatchBytes: 320, EvidenceBytes: 45}},
		{Type: EventLineageAdvance, LineageAdvance: &LineageAdvanceSnapshot{Sequence: 2, ParentSequence: 1, PatchBytes: 180, EvidenceBytes: 55}},
	}
	got := deriveTelemetry(events, nil)
	if got.Closure.Triggers["turn_budget"] != 1 || got.Closure.TurnBudgetExhausted != 1 {
		t.Fatalf("closure = %+v", got.Closure)
	}
	if got.Workflow.Supplied != 1 || got.Workflow.Unsupplied != 1 || got.Workflow.Outcomes["unknown"] != 1 {
		t.Fatalf("workflow = %+v", got.Workflow)
	}
	if got.Progress.MaxInspectionNoProgressStreak.Value != 3 || got.Progress.TurnsToFirstMutation.Value != 2 || got.Progress.TurnsToFirstVerification.Value != 3 {
		t.Fatalf("progress milestones = %+v", got.Progress)
	}
	if got.Progress.BatchingSteers != 1 || got.Progress.BatchingCompliant != 1 || got.Progress.BatchingNoncompliant != 0 {
		t.Fatalf("batching = %+v", got.Progress)
	}
	if got.Hooks.Timeouts != 1 || got.Hooks.CircuitOpened != 1 || got.Hooks.CircuitOpenSkips != 1 {
		t.Fatalf("hooks = %+v", got.Hooks)
	}
	if got.Invariants.NegativeContextViolations != 1 || got.Invariants.InconsistentRetentionViolations != 1 {
		t.Fatalf("invariants = %+v", got.Invariants)
	}
	if !got.Lineage.Available || got.Lineage.Advances != 2 || got.Lineage.PatchBytes != 500 || got.Lineage.MaxPatchBytes != 320 || got.Lineage.EvidenceBytes != 100 || got.Lineage.MaxEvidenceBytes != 55 {
		t.Fatalf("lineage = %+v", got.Lineage)
	}
}

func TestDeriveTrajectoryMeasuresBoundedShadowState(t *testing.T) {
	events := []Event{
		{Type: EventToolMutation, ToolMutation: &ToolMutationSnapshot{Paths: []string{"a.go", "b.go"}}},
		{Type: EventToolDiff, Path: "a.go"},
		{Type: EventEvaluatorResult, Prompt: 1, Turn: 1, EvaluatorResult: &EvaluatorResultSnapshot{Handler: "verify"}},
		{Type: EventBranch},
		{Type: EventToolMutation, ToolMutation: &ToolMutationSnapshot{Paths: []string{"c.go"}}},
		{Type: EventEvaluatorResult, Prompt: 2, Turn: 1, EvaluatorResult: &EvaluatorResultSnapshot{Handler: "verify", Accepted: true, Candidate: "candidate:two", EvidenceRef: "evidence/two"}},
	}
	got := deriveTrajectory(events)
	if !got.Available || got.Streams != 1 || got.Schema == 0 || got.Transitions != 6 || got.BranchResets != 1 || got.Evaluations != 2 || got.AcceptedEvaluations != 1 || got.RejectedEvaluations != 1 {
		t.Fatalf("trajectory churn = %+v", got)
	}
	if got.ActiveEvaluations != 1 || got.ActiveModifiedPaths != 1 || got.ActiveConfirmedPaths != 0 || got.MutationPathObservations != 3 || got.DiffPathConfirmations != 1 || got.UnconfirmedMutationPaths != 1 {
		t.Fatalf("trajectory attribution = %+v", got)
	}
	if got.StagnationBaselines != 1 || got.StagnationImprovements != 1 || got.ActiveNoImprovementStreak != 0 || got.MaxNoImprovementStreak != 0 {
		t.Fatalf("trajectory stagnation = %+v", got)
	}
	if got.MissingCandidateIDs != 1 || got.MissingEvidenceRefs != 1 || got.ProjectionBytes == 0 || got.MaxProjectionBytes != got.ProjectionBytes {
		t.Fatalf("trajectory completeness/size = %+v", got)
	}
	var aggregate TrajectoryAnalysis
	aggregate.add(got)
	aggregate.add(got)
	if aggregate.Streams != 2 || aggregate.Evaluations != 4 || aggregate.ProjectionBytes != 2*got.ProjectionBytes || aggregate.MaxProjectionBytes != got.ProjectionBytes {
		t.Fatalf("trajectory aggregate = %+v", aggregate)
	}
}

func TestDeriveTrajectoryReportsConservativeStagnationTelemetry(t *testing.T) {
	score0, scoreMinus1, score1, unordered := 0.0, -1.0, 1.0, 2.0
	remaining := 2
	events := []Event{
		{Type: EventEvaluatorResult, EvaluatorResult: &EvaluatorResultSnapshot{Handler: "verify", Score: &score0, ScoreDirection: "maximize", Candidate: "a"}},
		{Type: EventEvaluatorResult, EvaluatorResult: &EvaluatorResultSnapshot{Handler: "verify", Score: &score0, ScoreDirection: "maximize", Candidate: "b"}},
		{Type: EventEvaluatorResult, EvaluatorResult: &EvaluatorResultSnapshot{Handler: "verify", Score: &scoreMinus1, ScoreDirection: "maximize", Candidate: "c"}},
		{Type: EventStagnationNudge, StagnationNudge: &StagnationNudgeSnapshot{Threshold: 2, Streak: 2}},
		{Type: EventEvaluatorResult, EvaluatorResult: &EvaluatorResultSnapshot{Handler: "verify", Score: &score1, ScoreDirection: "maximize", Candidate: "d"}},
		{Type: EventEvaluatorResult, EvaluatorResult: &EvaluatorResultSnapshot{Handler: "verify", Score: &unordered, Candidate: "e"}},
		{Type: EventEvaluatorResult, EvaluatorResult: &EvaluatorResultSnapshot{Handler: "verify", Score: &unordered, Candidate: "f"}},
		{Type: EventBranch},
		{Type: EventEvaluatorResult, EvaluatorResult: &EvaluatorResultSnapshot{Handler: "requirements", RemainingRequirements: &remaining, Candidate: "g"}},
		{Type: EventEvaluatorResult, EvaluatorResult: &EvaluatorResultSnapshot{Handler: "requirements", RemainingRequirements: &remaining, Candidate: "h"}},
	}
	got := deriveTrajectory(events)
	if got.Evaluations != 8 || got.ActiveNoImprovementStreak != 1 || got.MaxNoImprovementStreak != 2 {
		t.Fatalf("stagnation streaks = %+v", got)
	}
	if got.StagnationBaselines != 2 || got.StagnationImprovements != 1 || got.StagnationPlateaus != 3 || got.StagnationRegressions != 1 || got.StagnationIndeterminate != 1 {
		t.Fatalf("stagnation classifications = %+v", got)
	}
	if got.UnorderedScoreEvaluations != 2 || got.StagnationLaneResets != 0 || got.StagnationNudges != 1 {
		t.Fatalf("stagnation comparability = %+v", got)
	}

	var aggregate TrajectoryAnalysis
	aggregate.add(got)
	aggregate.add(TrajectoryAnalysis{Available: true, Streams: 1, ActiveNoImprovementStreak: 3, MaxNoImprovementStreak: 4, StagnationPlateaus: 3, StagnationNudges: 2})
	if aggregate.ActiveNoImprovementStreak != 4 || aggregate.MaxNoImprovementStreak != 4 || aggregate.StagnationPlateaus != 6 || aggregate.StagnationNudges != 3 {
		t.Fatalf("stagnation aggregate = %+v", aggregate)
	}
}

func TestDeriveTelemetryAggregatesContextScopeAndRetention(t *testing.T) {
	got := deriveTelemetry([]Event{
		{Type: EventModelRequest, Context: &ContextSnapshot{Total: 120, PayloadTotal: 80, ProviderInputTokens: 70, ProviderInputScope: string(llm.InputTokenCountScopeRequestPayload)}},
		{Type: EventModelRequest, Context: &ContextSnapshot{Total: 200, PayloadTotal: 90, ProviderInputTokens: 180, ProviderInputScope: string(llm.InputTokenCountScopeEffectiveContext)}},
		{Type: EventRetention, Retention: &RetentionSnapshot{BlocksTrimmed: 2, BytesRemoved: 1200, EstimatedTokensRemoved: 300, ResponseStateReset: true, ContinuationStateReset: true, MeasurementAnchorReset: true}},
	}, nil)
	if !got.Context.Available || got.Context.Samples != 2 || got.Context.MaxTotalTokens != 200 || got.Context.MaxPayloadTokens != 90 || got.Context.ProviderCountScopes[string(llm.InputTokenCountScopeRequestPayload)] != 1 || got.Context.ProviderCountScopes[string(llm.InputTokenCountScopeEffectiveContext)] != 1 {
		t.Fatalf("context = %+v", got.Context)
	}
	if !got.Retention.Available || got.Retention.Epochs != 1 || got.Retention.BlocksTrimmed != 2 || got.Retention.EstimatedTokensRemoved != 300 || got.Retention.ResponseStateResets != 1 || got.Retention.ContinuationStateResets != 1 || got.Retention.MeasurementAnchorResets != 1 || got.Retention.NextRequestFull != 1 {
		t.Fatalf("retention = %+v", got.Retention)
	}
}

func TestAnalyzeCorpusDoesNotFollowSymlinks(t *testing.T) {
	base := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)
	t.Run("explicit root", func(t *testing.T) {
		realRoot := filepath.Join(t.TempDir(), "real")
		mustAppendAnalysisEvent(t, realRoot, Event{Time: base, Type: EventTurnProgress, TurnProgress: &TurnProgressSnapshot{SuccessfulMutation: true}})
		link := filepath.Join(t.TempDir(), "link")
		if err := os.Symlink(realRoot, link); err != nil {
			t.Fatal(err)
		}
		if _, err := AnalyzeCorpus(link, AnalyzeOptions{}); err == nil {
			t.Fatal("AnalyzeCorpus followed an explicit symlink")
		}
	})
	t.Run("children directory", func(t *testing.T) {
		root := filepath.Join(t.TempDir(), "root")
		mustAppendAnalysisEvent(t, root, Event{Time: base, Type: EventTurnProgress, Prompt: 1, Turn: 1, TurnProgress: &TurnProgressSnapshot{}})
		externalChildren := filepath.Join(t.TempDir(), "children")
		mustAppendAnalysisEvent(t, filepath.Join(externalChildren, "external"), Event{Time: base, Type: EventTurnProgress, TurnProgress: &TurnProgressSnapshot{SuccessfulMutation: true}})
		if err := os.Symlink(externalChildren, filepath.Join(root, "children")); err != nil {
			t.Fatal(err)
		}
		report, err := AnalyzeCorpus(root, AnalyzeOptions{})
		if err != nil {
			t.Fatal(err)
		}
		if report.Sessions != 1 || report.Telemetry.Progress.ToolTurns != 1 {
			t.Fatalf("followed symlinked children: %+v", report)
		}
	})
	t.Run("raw stream", func(t *testing.T) {
		root := filepath.Join(t.TempDir(), "root")
		if err := (Session{ID: "root"}).Save(root); err != nil {
			t.Fatal(err)
		}
		external := filepath.Join(t.TempDir(), "raw.ndjson")
		data, _ := json.Marshal(Event{Time: base, Type: EventTurnProgress, TurnProgress: &TurnProgressSnapshot{SuccessfulMutation: true}})
		if err := os.WriteFile(external, append(data, '\n'), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(external, filepath.Join(root, eventLog)); err != nil {
			t.Fatal(err)
		}
		report, err := AnalyzeCorpus(root, AnalyzeOptions{})
		if err != nil {
			t.Fatal(err)
		}
		if report.SymlinkStreams != 1 || report.Telemetry.Progress.Available || report.Items[0].Source.Status != "symlink" {
			t.Fatalf("followed symlinked raw stream: %+v", report)
		}
	})
}

func TestAnalyzeBeforeDoesNotUseFutureChildMetadata(t *testing.T) {
	root := filepath.Join(t.TempDir(), "root")
	mustAppendAnalysisEvent(t, root, Event{Type: EventToolResult})
	child := filepath.Join(root, "children", "child")
	mustWriteAnalysisJSON(t, filepath.Join(child, "meta.json"), ChildMeta{
		ID: "child", Status: ChildStatusCompleted, Updated: time.Date(2026, 8, 2, 0, 0, 0, 0, time.UTC),
		ClosureTrigger: "turn_budget", TurnBudgetExhausted: true, TelemetryVersion: ReliabilityTelemetryVersion,
	})
	report, err := AnalyzeCorpus(root, AnalyzeOptions{Before: time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)})
	if err != nil {
		t.Fatal(err)
	}
	if report.Telemetry.Closure.Available || report.Telemetry.Workflow.Available {
		t.Fatalf("future metadata affected cutoff: %+v", report.Telemetry)
	}
}

func TestMarkedPromptMakesZeroProgressAndHooksAvailable(t *testing.T) {
	got := deriveTelemetry([]Event{
		{Type: EventPromptUsage, Prompt: 1, TelemetryVersion: ReliabilityTelemetryVersion},
		{Type: EventPromptUsage, Prompt: 2}, // legacy prompt: not in the closure denominator
	}, nil)
	if !got.Closure.Available || got.Closure.Prompts != 1 || !got.Progress.Available || got.Progress.ToolTurns != 0 || !got.Progress.MaxInspectionNoProgressStreak.Available || !got.Hooks.Available || got.Hooks.Diagnostics != 0 {
		t.Fatalf("marked zero telemetry = %+v", got)
	}
}

func TestExecutionCompletenessMergeIsCommutative(t *testing.T) {
	values := []string{"complete", "unknown", "unavailable", "incomplete"}
	for _, left := range values {
		for _, right := range values {
			a := ExecutionAnalysis{Completeness: left}
			a.add(ExecutionAnalysis{Completeness: right})
			b := ExecutionAnalysis{Completeness: right}
			b.add(ExecutionAnalysis{Completeness: left})
			if a.Completeness != b.Completeness {
				t.Fatalf("merge %q/%q = %q/%q", left, right, a.Completeness, b.Completeness)
			}
		}
	}
}

func TestDeriveExecutionCountsAndCompleteness(t *testing.T) {
	events := []Event{
		{Type: EventTurnAttemptStart, Prompt: 1, Turn: 1},
		{Type: EventToolStart, Prompt: 1, Turn: 1, Tool: "edit"},
		{Type: EventToolResult, Prompt: 1, Turn: 1, Tool: "edit", ResultError: true},
		{Type: EventModelRequest, Prompt: 1, Turn: 1, ModelRequest: &llm.ModelRequestEvent{State: llm.ModelRequestFailed}},
		{Type: EventTurnComplete, Prompt: 1, Turn: 1},
		{Type: EventPromptUsage, Prompt: 1, TerminationReason: "turn_limit"},
	}
	got := deriveExecution(events, "complete")
	if got.Completeness != "complete" || got.Prompts != 1 || got.CompletedPrompts != 1 || got.Turns != 1 || got.ToolCalls != 1 || got.ToolResults != 1 || got.ToolErrors != 1 || got.ModelErrors != 1 || !got.TerminationAvailable || got.Terminations["turn_limit"] != 1 {
		t.Fatalf("execution = %+v", got)
	}
	if incomplete := deriveExecution(events[:5], "complete"); incomplete.Completeness != "incomplete" || incomplete.TerminationAvailable {
		t.Fatalf("incomplete execution = %+v", incomplete)
	}
	if unavailable := deriveExecution(nil, "missing"); unavailable.Completeness != "unavailable" {
		t.Fatalf("missing execution = %+v", unavailable)
	}
}

func TestAnalysisJSONDeterministicVersionedAndTranscriptFree(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "session")
	mustAppendAnalysisEvent(t, dir, Event{Type: EventUser, Text: "TOP SECRET TRANSCRIPT"})
	mustAppendAnalysisEvent(t, dir, Event{Type: EventToolStart, Tool: "shell", Input: json.RawMessage(`{"command":"TOP SECRET INPUT"}`)})
	mustAppendAnalysisEvent(t, dir, Event{Type: EventToolMutation, ToolMutation: &ToolMutationSnapshot{Paths: []string{"TOP SECRET PATH"}}})
	score := 1.0
	mustAppendAnalysisEvent(t, dir, Event{Type: EventEvaluatorResult, EvaluatorResult: &EvaluatorResultSnapshot{Score: &score, ScoreDirection: "maximize", Candidate: "TOP SECRET CANDIDATE", EvidenceRef: "TOP SECRET EVIDENCE"}})
	mustAppendAnalysisEvent(t, dir, Event{Type: EventStagnationNudge, StagnationNudge: &StagnationNudgeSnapshot{Threshold: 2, Streak: 2}})
	report, err := AnalyzeCorpus(dir, AnalyzeOptions{})
	if err != nil {
		t.Fatal(err)
	}
	var first, second bytes.Buffer
	if err := WriteAnalysisJSON(report, &first); err != nil {
		t.Fatal(err)
	}
	if err := WriteAnalysisJSON(report, &second); err != nil {
		t.Fatal(err)
	}
	if first.String() != second.String() {
		t.Fatalf("JSON is nondeterministic:\n%s\n%s", first.String(), second.String())
	}
	if strings.Contains(first.String(), "TOP SECRET") || strings.Contains(first.String(), "maximize") || !strings.Contains(first.String(), `"version": 12`) || !report.Telemetry.Trajectory.Available || report.Telemetry.Trajectory.StagnationNudges != 1 {
		t.Fatalf("JSON leaked transcript or omitted version:\n%s", first.String())
	}
}

func TestWriteAnalysisTextIsBounded(t *testing.T) {
	report := AnalysisReport{Version: AnalysisVersion, Path: "/corpus", Sessions: maxAnalysisTextSessions + 5}
	for i := 0; i < maxAnalysisTextSessions+5; i++ {
		report.Items = append(report.Items, SessionAnalysis{Path: filepath.Join("/corpus", strings.Repeat("x", i%5+1)), Source: AnalysisSource{Status: "complete"}})
	}
	var out bytes.Buffer
	if err := WriteAnalysisText(report, &out); err != nil {
		t.Fatal(err)
	}
	if strings.Count(out.String(), "events\n") != maxAnalysisTextSessions || !strings.Contains(out.String(), "5 more streams omitted") {
		t.Fatalf("text report was not bounded:\n%s", out.String())
	}
}

func TestStatsIncludesOperationalTelemetryTextAndJSON(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "session")
	if err := (Session{ID: "stats-analysis"}).Save(dir); err != nil {
		t.Fatalf("save session: %v", err)
	}
	mustAppendAnalysisEvent(t, dir, Event{Type: EventTurnProgress, Prompt: 1, Turn: 1, TurnProgress: &TurnProgressSnapshot{SuccessfulMutation: true}})
	mustAppendAnalysisEvent(t, dir, Event{Type: EventPromptUsage, Prompt: 1, TurnBudgetExhausted: true, TelemetryVersion: ReliabilityTelemetryVersion})
	mustAppendAnalysisEvent(t, dir, Event{Type: EventToolMutation, Prompt: 1, Turn: 1, ToolMutation: &ToolMutationSnapshot{Paths: []string{"changed.go"}}})
	var text bytes.Buffer
	if err := Stats(dir, &text); err != nil {
		t.Fatalf("Stats: %v", err)
	}
	for _, want := range []string{"Operational telemetry", "progress telemetry: available", "turn budgets exhausted: 1", "trajectory streams/transitions/evaluations/branch resets: 1 / 1 / 0 / 0"} {
		if !strings.Contains(text.String(), want) {
			t.Fatalf("Stats missing %q:\n%s", want, text.String())
		}
	}
	var encoded bytes.Buffer
	if err := StatsJSON(dir, &encoded); err != nil {
		t.Fatalf("StatsJSON: %v", err)
	}
	var payload struct {
		Telemetry TelemetryAnalysis `json:"telemetry"`
	}
	if err := json.Unmarshal(encoded.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	if !payload.Telemetry.Progress.Available || payload.Telemetry.Closure.TurnBudgetExhausted != 1 || !payload.Telemetry.Trajectory.Available || payload.Telemetry.Trajectory.ActiveModifiedPaths != 1 {
		t.Fatalf("StatsJSON telemetry = %+v", payload.Telemetry)
	}
}

func mustAppendAnalysisEvent(t *testing.T, dir string, event Event) {
	t.Helper()
	if err := AppendEvent(dir, event); err != nil {
		t.Fatalf("AppendEvent: %v", err)
	}
}

func mustWriteAnalysisJSON(t *testing.T, path string, value any) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
}
