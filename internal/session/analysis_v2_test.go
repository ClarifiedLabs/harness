package session

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"testing"
	"time"

	"harness/internal/llm"
)

func TestDeriveExecutionIdentityDetectsSwitchesAndMissingFields(t *testing.T) {
	metadata := analysisMetadata{agent: "code", provider: "fixture", model: "model"}
	stable := deriveExecutionIdentity([]Event{
		{Type: EventTurnAttemptStart, Agent: "code", Provider: "fixture", Model: "model"},
		{Type: EventTurnAttemptStart, Agent: "code", Provider: "fixture", Model: "model"},
	}, "complete", metadata)
	if !stable.Available || !stable.Stable || stable.Attempts != 2 {
		t.Fatalf("stable execution identity = %+v", stable)
	}
	switched := deriveExecutionIdentity([]Event{
		{Type: EventTurnAttemptStart, Agent: "code", Provider: "fixture", Model: "model"},
		{Type: EventTurnAttemptStart, Agent: "plan", Provider: "fixture", Model: "model"},
	}, "complete", metadata)
	if !switched.Available || switched.Stable {
		t.Fatalf("switched execution identity = %+v", switched)
	}
	missing := deriveExecutionIdentity([]Event{{Type: EventTurnAttemptStart, Provider: "fixture", Model: "model"}}, "complete", metadata)
	if missing.Available || missing.Stable {
		t.Fatalf("missing execution identity = %+v", missing)
	}
}

func TestUsageFromEventsMarksLegacyFoldedUsageUnavailable(t *testing.T) {
	legacyUsage := llm.Usage{InputTokens: 99, CostUSD: 1, CostKnown: true}
	conversation, maintenance := usageFromEvents([]Event{
		{Type: EventTurnComplete, Prompt: 1, Turn: 1, Usage: &legacyUsage},
		{Type: EventPromptUsage, Prompt: 1, Usage: &legacyUsage},
	}, "complete")
	if conversation.Available || maintenance.Available || conversation.TotalTokens != 0 || maintenance.TotalTokens != 0 {
		t.Fatalf("legacy folded usage must remain unavailable: conversation=%+v maintenance=%+v", conversation, maintenance)
	}
	root := filepath.Join(t.TempDir(), "legacy")
	if err := (Session{ID: "legacy", Usage: UsageTotals{Usage: legacyUsage, CostUSD: 1}}).Save(root); err != nil {
		t.Fatal(err)
	}
	mustAppendAnalysisEvent(t, root, Event{Type: EventTurnComplete, Prompt: 1, Turn: 1, Usage: &legacyUsage})
	mustAppendAnalysisEvent(t, root, Event{Type: EventPromptUsage, Prompt: 1, Usage: &legacyUsage})
	report, err := AnalyzeCorpus(root, AnalyzeOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if report.Usage.Inclusive.Available || report.Usage.Reconciliation.Available || report.Distributions.InclusiveTokens.Samples != 0 {
		t.Fatalf("legacy hierarchy entered accounting distributions: %+v / %+v", report.Usage, report.Distributions)
	}

	currentUsage := llm.Usage{InputTokens: 7}
	conversation, maintenance = usageFromEvents([]Event{
		{Type: EventTurnAttemptUsage, Prompt: 1, Turn: 1, Attempt: 1, Usage: &currentUsage},
		{Type: EventTurnComplete, Prompt: 1, Turn: 1, Usage: &currentUsage},
		{Type: EventPromptUsage, Prompt: 1, Usage: &currentUsage},
	}, "complete")
	if !conversation.Available || !maintenance.Available || conversation.TotalTokens != 7 || conversation.ModelCalls != 1 {
		t.Fatalf("current physical usage = conversation=%+v maintenance=%+v", conversation, maintenance)
	}
}

func TestUsageFromEventsRejectsDuplicateAndMixedLegacyRecords(t *testing.T) {
	physical := llm.Usage{InputTokens: 3, OutputTokens: 1, CostKnown: true, CostUSD: .01}
	conversation, _ := usageFromEvents([]Event{
		{Type: EventTurnAttemptStart, Prompt: 1, Turn: 1, Attempt: 1},
		{Type: EventTurnAttemptUsage, Prompt: 1, Turn: 1, Attempt: 1, Usage: &physical},
		{Type: EventTurnAttemptUsage, Prompt: 1, Turn: 1, Attempt: 1, Usage: &physical},
	}, "complete")
	if conversation.ModelCalls != 1 || conversation.TotalTokens != 4 || conversation.Complete {
		t.Fatalf("duplicate attempt accounting = %+v", conversation)
	}

	folded := llm.Usage{InputTokens: 10}
	conversation, maintenance := usageFromEvents([]Event{
		{Type: EventTurnAttemptStart, Prompt: 1, Turn: 1, Attempt: 1},
		{Type: EventTurnAttemptUsage, Prompt: 1, Turn: 1, Attempt: 1, Usage: &physical},
		{Type: EventPromptUsage, Prompt: 2, Usage: &folded},
	}, "complete")
	if conversation.Available || maintenance.Available {
		t.Fatalf("mixed legacy usage reported available: conversation=%+v maintenance=%+v", conversation, maintenance)
	}
	currentConversation, _ := usageFromEvents([]Event{
		{Type: EventTurnAttemptStart, Prompt: 1, Turn: 1, Attempt: 1},
		{Type: EventTurnAttemptUsage, Prompt: 1, Turn: 1, Attempt: 1, Usage: &physical},
	}, "complete")
	legacyConversation, _ := usageFromEvents([]Event{{Type: EventPromptUsage, Prompt: 1, Usage: &folded}}, "complete")
	hierarchyConversation := addUsageSlice(currentConversation, legacyConversation)
	if !hierarchyConversation.Available || hierarchyConversation.Complete {
		t.Fatalf("mixed physical stream hierarchy usage = %+v", hierarchyConversation)
	}
}

func TestAnalyzeV2UsageCohortReconciliationAndCutoff(t *testing.T) {
	base := time.Date(2026, 8, 3, 12, 0, 0, 0, time.UTC)
	root := filepath.Join(t.TempDir(), "fixture-a")
	rootState := Session{
		Build: BuildMetadata{Version: "v2-test", Commit: "baseline"},
		Runtime: RuntimeProfile{
			RetentionPolicy: "pressure", ContextWindow: 200_000,
			ToolResultMaxBytes: 64 << 10, ToolResultMaxLines: 2_000,
			CompactToolResultMaxBytes: 8 << 10, ResponsesStateful: true,
			DelegateMaxTurns: 20, Prewarm: true, SearchBackend: "rg",
		},
		Provider: "fake", Model: "root-model", Created: base, Updated: base,
		Usage: UsageTotals{
			Usage: llm.Usage{
				InputTokens: 165, CacheReadTokens: 10, CacheWriteTokens: 5,
				OutputTokens: 22, ReasoningTokens: 4, CostUSD: 1.15,
			},
			CostUSD: 1.15,
		},
	}
	if err := rootState.Save(root); err != nil {
		t.Fatal(err)
	}
	rootConversation := llm.Usage{InputTokens: 100, CacheReadTokens: 10, OutputTokens: 20, CostUSD: 1, CostKnown: true}
	rootMaintenance := llm.Usage{InputTokens: 10, OutputTokens: 2, CostUSD: .1, CostKnown: true}
	mustAppendAnalysisEvent(t, root, Event{Time: base, Type: EventTurnAttemptStart, Prompt: 1, Turn: 1, Attempt: 1})
	mustAppendAnalysisEvent(t, root, Event{Time: base, Type: EventTurnAttemptUsage, Prompt: 1, Turn: 1, Attempt: 1, Usage: &rootConversation})
	mustAppendAnalysisEvent(t, root, Event{Time: base, Type: EventMaintenanceUsage, Prompt: 1, Usage: &rootMaintenance})
	mustAppendAnalysisEvent(t, root, Event{Time: base, Type: EventPromptUsage, Prompt: 1, TelemetryVersion: ReliabilityTelemetryVersion})

	child := filepath.Join(root, "children", "child-a")
	childState := Session{
		Build:         BuildMetadata{Version: "child-build", Commit: "different"},
		Runtime:       RuntimeProfile{RetentionPolicy: "age", ContextWindow: 100_000},
		ParentSession: "root", Provider: "fake", Model: "child-model", Agent: "explore",
		Created: base, Updated: base,
	}
	if err := childState.Save(child); err != nil {
		t.Fatal(err)
	}
	mustWriteAnalysisJSON(t, filepath.Join(child, "meta.json"), ChildMeta{
		ID: "child-a", ParentID: "root", Agent: "explore", Provider: "fake", Model: "child-model",
		Build: childState.Build, Runtime: childState.Runtime, Status: ChildStatusCompleted,
	})
	childConversation := llm.Usage{InputTokens: 50, CacheWriteTokens: 5, ReasoningTokens: 4}
	childMaintenance := llm.Usage{InputTokens: 5, CostUSD: .05, CostKnown: true}
	mustAppendAnalysisEvent(t, child, Event{Time: base, Type: EventTurnAttemptStart, Prompt: 1, Turn: 1, Attempt: 1})
	mustAppendAnalysisEvent(t, child, Event{Time: base, Type: EventTurnAttemptUsage, Prompt: 1, Turn: 1, Attempt: 1, Usage: &childConversation})
	mustAppendAnalysisEvent(t, child, Event{Time: base, Type: EventMaintenanceUsage, Prompt: 1, Usage: &childMaintenance})
	mustAppendAnalysisEvent(t, child, Event{Time: base, Type: EventPromptUsage, Prompt: 1, TelemetryVersion: ReliabilityTelemetryVersion})

	report, err := AnalyzeCorpus(root, AnalyzeOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if report.Version != AnalysisVersion || len(report.Hierarchies) != 1 || len(report.Cohorts) != 1 || len(report.Items) != 2 {
		t.Fatalf("analysis shape = version %d hierarchies %d cohorts %d items %d", report.Version, len(report.Hierarchies), len(report.Cohorts), len(report.Items))
	}
	usage := report.Usage
	if usage.RootConversational.TotalTokens != 130 || usage.RootMaintenance.TotalTokens != 12 || usage.DescendantConversational.TotalTokens != 59 || usage.DescendantMaintenance.TotalTokens != 5 || usage.Inclusive.TotalTokens != 206 {
		t.Fatalf("usage split = %+v", usage)
	}
	if usage.Inclusive.ModelCalls != 4 || usage.Inclusive.PricedCalls != 3 || usage.Inclusive.UnpricedCalls != 1 || usage.Inclusive.CostComplete || math.Abs(usage.Inclusive.KnownCostUSD-1.15) > 1e-9 {
		t.Fatalf("price coverage = %+v", usage.Inclusive)
	}
	if !usage.Reconciliation.Available || !usage.Reconciliation.Matches || usage.Reconciliation.Discrepancies != 0 {
		t.Fatalf("reconciliation = %+v", usage.Reconciliation)
	}
	if report.Distributions.InclusiveTokens != (IntDistribution{Samples: 1, Median: 206, P90: 206}) || report.Distributions.InclusiveKnownCostUSD.Samples != 0 {
		t.Fatalf("distributions = %+v", report.Distributions)
	}
	if !report.Cohorts[0].Cohort.Available || report.Cohorts[0].Cohort.Build.Commit != "baseline" || report.Cohorts[0].Sessions != 2 {
		t.Fatalf("cohort = %+v", report.Cohorts[0])
	}
	for _, item := range report.Items {
		if item.CohortKey != report.Cohorts[0].Cohort.Key || item.RootPath != root {
			t.Fatalf("root ownership = %+v", item)
		}
		if item.Delegate && item.Build.Commit != "different" {
			t.Fatalf("child metadata overwritten by root cohort: %+v", item)
		}
	}
	if report.Storage.TotalBytes == 0 || report.Storage.State.Files != 2 || report.Storage.Tree.Files != 2 || report.Storage.Raw.Files != 2 {
		t.Fatalf("storage = %+v", report.Storage)
	}

	loaded, err := Load(root)
	if err != nil {
		t.Fatal(err)
	}
	loaded.Usage.InputTokens++
	if err := loaded.Save(root); err != nil {
		t.Fatal(err)
	}
	discrepant, err := AnalyzeCorpus(root, AnalyzeOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if discrepant.Usage.Reconciliation.Matches || discrepant.Telemetry.Invariants.UsageReconciliationViolations != 1 {
		t.Fatalf("discrepancy hidden: %+v / %+v", discrepant.Usage.Reconciliation, discrepant.Telemetry.Invariants)
	}

	prefix, err := AnalyzeCorpus(root, AnalyzeOptions{Before: base.Add(time.Minute)})
	if err != nil {
		t.Fatal(err)
	}
	if prefix.Usage.Reconciliation.Available || prefix.Distributions.InclusiveTokens.Samples != 0 || prefix.Usage.Inclusive.TotalTokens != 206 {
		t.Fatalf("cutoff used future state or dropped event usage: %+v", prefix.Usage)
	}
	if prefix.Storage.State.Status != "cutoff_incomplete" || prefix.Storage.Tree.Status != "cutoff_incomplete" || prefix.Storage.Raw.Status != "cutoff_incomplete" {
		t.Fatalf("cutoff storage statuses = %+v", prefix.Storage)
	}
}

func TestAnalyzeV2UnpricedIncompleteAndMissingUsageCoverage(t *testing.T) {
	root := filepath.Join(t.TempDir(), "root")
	mustAppendAnalysisEvent(t, root, Event{Type: EventTurnAttemptStart, Prompt: 1, Turn: 1, Attempt: 1})
	child := filepath.Join(root, "children", "missing")
	if err := os.MkdirAll(child, 0o755); err != nil {
		t.Fatal(err)
	}
	mustWriteAnalysisJSON(t, filepath.Join(child, "meta.json"), ChildMeta{ID: "missing"})

	report, err := AnalyzeCorpus(root, AnalyzeOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if report.Usage.RootConversational.ModelCalls != 1 || report.Usage.RootConversational.UnpricedCalls != 1 || report.Usage.RootConversational.CostComplete {
		t.Fatalf("unfinished call coverage = %+v", report.Usage.RootConversational)
	}
	if report.Usage.DescendantConversational.Available || report.Usage.Inclusive.Complete || report.Distributions.InclusiveTokens.Samples != 0 {
		t.Fatalf("missing child usage treated as complete: %+v", report.Usage)
	}
}

func TestAnalyzeV2StorageStatusesAndPrivacy(t *testing.T) {
	root := filepath.Join(t.TempDir(), "root")
	mustAppendAnalysisEvent(t, root, Event{Type: EventToolStart, Tool: "read_file"})
	if err := os.WriteFile(filepath.Join(root, treeFile), []byte("{malformed TOP SECRET TREE"), 0o644); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(t.TempDir(), "state.json")
	if err := os.WriteFile(target, []byte(`{"version":5}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(root, stateFile)); err != nil {
		t.Fatal(err)
	}

	report, err := AnalyzeCorpus(root, AnalyzeOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if report.Items[0].Storage.State.Status != "symlink" || report.Items[0].Storage.Tree.Status != "malformed" {
		t.Fatalf("storage statuses = %+v", report.Items[0].Storage)
	}
	var out bytes.Buffer
	if err := WriteAnalysisJSON(report, &out); err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(out.Bytes(), []byte("TOP SECRET")) {
		t.Fatalf("analysis leaked tree content: %s", out.String())
	}
}

func TestAnalyzeV2BoundsMetadataAndStorageDirectoryDepth(t *testing.T) {
	root := filepath.Join(t.TempDir(), "root")
	mustAppendAnalysisEvent(t, root, Event{Type: EventPromptUsage, Prompt: 1})
	state, err := os.OpenFile(filepath.Join(root, stateFile), os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	if err := state.Truncate(maxAnalysisMetadataBytes + 1); err != nil {
		state.Close()
		t.Fatal(err)
	}
	if err := state.Close(); err != nil {
		t.Fatal(err)
	}
	child := filepath.Join(root, "children", "oversized")
	if err := os.MkdirAll(child, 0o755); err != nil {
		t.Fatal(err)
	}
	meta, err := os.OpenFile(filepath.Join(child, "meta.json"), os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	if err := meta.Truncate(maxAnalysisMetadataBytes + 1); err != nil {
		meta.Close()
		t.Fatal(err)
	}
	if err := meta.Close(); err != nil {
		t.Fatal(err)
	}
	deep := filepath.Join(root, "artifacts", "tool-results")
	for i := 0; i < maxAnalysisStorageDepth+2; i++ {
		deep = filepath.Join(deep, fmt.Sprintf("d%02d", i))
	}
	if err := os.MkdirAll(deep, 0o755); err != nil {
		t.Fatal(err)
	}

	report, err := AnalyzeCorpus(root, AnalyzeOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if report.MalformedChildMetadata != 1 || report.Items[0].BuildAvailable || report.Items[0].Storage.State.Status != "limit_exceeded" || report.Items[0].Storage.ToolResults.Status != "limit_exceeded" {
		t.Fatalf("bounded analysis = malformed_meta %d item %+v storage %+v", report.MalformedChildMetadata, report.Items[0], report.Items[0].Storage)
	}
}

func TestAnalyzeV2ContextScopesWorkflowAndPercentiles(t *testing.T) {
	remainingA, remainingB := 7, 2
	telemetry := deriveTelemetry([]Event{
		{Type: EventModelRequest, Context: &ContextSnapshot{Total: 100, PayloadTotal: 40, ProviderInputTokens: 50, ProviderInputScope: string(llm.InputTokenCountScopeRequestPayload)}},
		{Type: EventModelRequest, Context: &ContextSnapshot{Total: 120, PayloadTotal: 45, ProviderInputTokens: 110, ProviderInputScope: string(llm.InputTokenCountScopeEffectiveContext)}},
		{Type: EventPromptUsage, Prompt: 1, TelemetryVersion: ReliabilityTelemetryVersion, WorkflowStatus: &WorkflowStatusSnapshot{Outcome: "in_progress", RemainingRequirements: &remainingA}},
		{Type: EventPromptUsage, Prompt: 2, TelemetryVersion: ReliabilityTelemetryVersion, WorkflowStatus: &WorkflowStatusSnapshot{Outcome: "complete", RemainingRequirements: &remainingB}},
	}, nil)
	if telemetry.Context.ProviderMaxByScope[string(llm.InputTokenCountScopeRequestPayload)] != 50 || telemetry.Context.ProviderMaxByScope[string(llm.InputTokenCountScopeEffectiveContext)] != 110 {
		t.Fatalf("scope maxima = %+v", telemetry.Context)
	}
	if telemetry.Invariants.NegativeContextViolations != 1 {
		t.Fatalf("compatible-scope violation = %+v", telemetry.Invariants)
	}
	if telemetry.Workflow.RemainingRequirementsTotal != 9 || telemetry.Workflow.RemainingRequirements != (IntDistribution{Samples: 2, Median: 2, P90: 7}) || telemetry.Workflow.CompletionSourceAvailable {
		t.Fatalf("workflow distribution = %+v", telemetry.Workflow)
	}
	values := []int{10, 1, 9, 2, 8, 3, 7, 4, 6, 5}
	if got := intDistribution(values); got != (IntDistribution{Samples: 10, Median: 5, P90: 9}) {
		t.Fatalf("nearest-rank distribution = %+v", got)
	}
}

func TestDeriveCompletionNormalizesUntrustedMetadataKeys(t *testing.T) {
	completion := deriveCompletion(&ChildMeta{Completion: &ChildCompletionReport{
		Outcome:          "private outcome",
		Contract:         "private contract",
		Source:           ChildCompletionSourceDeclared,
		ValidationStatus: "private validation",
	}}, true)
	if completion.Outcomes[ChildCompletionOutcomeUnknown] != 1 || completion.Contracts[ChildCompletionContractGeneral] != 1 || completion.Validation[ChildCompletionValidationInvalid] != 1 || completion.Unavailable != 1 {
		t.Fatalf("completion = %+v", completion)
	}
	encoded, err := json.Marshal(completion)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(encoded, []byte("private")) {
		t.Fatalf("completion leaked untrusted metadata keys: %s", encoded)
	}

	forged := deriveCompletion(&ChildMeta{Completion: &ChildCompletionReport{
		Outcome: ChildCompletionOutcomeComplete, Contract: ChildCompletionContractGeneral,
		Source: ChildCompletionSourceDeclared, ValidationStatus: ChildCompletionValidationValid,
	}}, true)
	if forged.Reports != 0 || forged.Unavailable != 1 || forged.Outcomes[ChildCompletionOutcomeUnknown] != 1 || forged.Validation[ChildCompletionValidationInvalid] != 1 {
		t.Fatalf("forged valid report was trusted: %+v", forged)
	}

	var omitted ChildMeta
	if err := json.Unmarshal([]byte(`{"status":"completed","completion":{"outcome":"partial","contract":"general","source":"child_declared","validation_status":"valid","evidence":[],"unresolved_questions":[]}}`), &omitted); err != nil {
		t.Fatal(err)
	}
	missingCount := deriveCompletion(&omitted, true)
	if missingCount.Reports != 0 || missingCount.Validation[ChildCompletionValidationInvalid] != 1 {
		t.Fatalf("omitted unresolved count was trusted: %+v", missingCount)
	}

	for _, tc := range []struct {
		name        string
		status      string
		termination string
	}{
		{name: "failed", status: ChildStatusFailed, termination: "error"},
		{name: "canceled", status: ChildStatusCanceled, termination: "cancelled"},
	} {
		t.Run("lifecycle_"+tc.name, func(t *testing.T) {
			meta := ChildMeta{Status: tc.status, TerminationReason: tc.termination, Completion: &ChildCompletionReport{
				Outcome: ChildCompletionOutcomeComplete, Contract: ChildCompletionContractGeneral,
				Evidence: []ChildCompletionEvidence{}, UnresolvedQuestions: []string{},
				Source: ChildCompletionSourceDeclared, ValidationStatus: ChildCompletionValidationValid,
			}}
			meta.Completion.unresolvedRequirementsPresent = true
			got := deriveCompletion(&meta, true)
			if got.Reports != 0 || got.Unavailable != 1 || got.Validation[ChildCompletionValidationInvalid] != 1 {
				t.Fatalf("forged declared report was trusted: %+v", got)
			}
		})
	}

	hostCanceled := deriveCompletion(&ChildMeta{
		Status: ChildStatusCanceled, TerminationReason: "cancelled",
		Completion: &ChildCompletionReport{Outcome: ChildCompletionOutcomeUnknown, Contract: ChildCompletionContractGeneral, Source: ChildCompletionSourceHost, ValidationStatus: ChildCompletionValidationUnavailable},
	}, true)
	if hostCanceled.Reports != 0 || hostCanceled.Unavailable != 1 || hostCanceled.Validation[ChildCompletionValidationUnavailable] != 1 {
		t.Fatalf("host canceled report = %+v", hostCanceled)
	}
}

func TestAnalyzeDelegateCompletionCoverageAndPrivacy(t *testing.T) {
	root := filepath.Join(t.TempDir(), "root")
	mustAppendAnalysisEvent(t, root, Event{Type: EventToolResult})

	valid := filepath.Join(root, "children", "valid")
	mustWriteAnalysisJSON(t, filepath.Join(valid, "meta.json"), ChildMeta{
		ID: "valid", Kind: "delegate", Status: ChildStatusCompleted, Mode: ChildCompletionContractImplementation,
		Completion: &ChildCompletionReport{
			Outcome: ChildCompletionOutcomePartial, UnresolvedRequirements: 2,
			Blockers:     []string{"SECRET_BLOCKER"},
			ChangedFiles: []string{"SECRET_PATH.go"},
			Verification: []ChildCompletionVerification{{Check: "SECRET_CHECK", Status: "passed", Detail: "SECRET_DETAIL"}},
			Contract:     ChildCompletionContractImplementation, Source: ChildCompletionSourceDeclared,
			ValidationStatus: ChildCompletionValidationValid,
		},
	})
	mustAppendAnalysisEvent(t, valid, Event{Type: EventToolResult})

	review := filepath.Join(root, "children", "review")
	mustWriteAnalysisJSON(t, filepath.Join(review, "meta.json"), ChildMeta{
		ID: "review", Kind: "delegate", Status: ChildStatusCompleted, Agent: "review",
		Completion: &ChildCompletionReport{
			Outcome: ChildCompletionOutcomePartial, UnresolvedRequirements: 1,
			Coverage: "partial", UnreviewedScope: []string{"SECRET_UNREVIEWED"},
			Contract: ChildCompletionContractReview, Source: ChildCompletionSourceDeclared, ValidationStatus: ChildCompletionValidationValid,
		},
	})
	mustAppendAnalysisEvent(t, review, Event{Type: EventToolResult})

	general := filepath.Join(root, "children", "general")
	mustWriteAnalysisJSON(t, filepath.Join(general, "meta.json"), ChildMeta{
		ID: "general", Kind: "delegate", Status: ChildStatusCompleted, Agent: "explore",
		Completion: &ChildCompletionReport{
			Outcome: ChildCompletionOutcomeBlocked, UnresolvedRequirements: 1, Blockers: []string{"SECRET_GENERAL_BLOCKER"},
			Evidence: []ChildCompletionEvidence{{Path: "SECRET_EVIDENCE_PATH", Symbol: "SECRET_SYMBOL"}}, UnresolvedQuestions: []string{"SECRET_QUESTION"},
			Contract: ChildCompletionContractGeneral, Source: ChildCompletionSourceDeclared, ValidationStatus: ChildCompletionValidationValid,
		},
	})
	mustAppendAnalysisEvent(t, general, Event{Type: EventToolResult})

	legacy := filepath.Join(root, "children", "legacy")
	mustWriteAnalysisJSON(t, filepath.Join(legacy, "meta.json"), ChildMeta{ID: "legacy", Kind: "delegate", Agent: "explore"})
	mustAppendAnalysisEvent(t, legacy, Event{Type: EventToolResult})

	report, err := AnalyzeCorpus(root, AnalyzeOptions{})
	if err != nil {
		t.Fatal(err)
	}
	completion := report.Telemetry.Completion
	if report.Coverage.CompletionApplicable != 4 || report.Coverage.CompletionValid != 3 || report.Coverage.CompletionCoverageFailures != 1 {
		t.Fatalf("completion coverage = %+v", report.Coverage)
	}
	if completion.Reports != 3 || completion.Unavailable != 1 || completion.Outcomes[ChildCompletionOutcomePartial] != 2 || completion.Outcomes[ChildCompletionOutcomeBlocked] != 1 || completion.Outcomes[ChildCompletionOutcomeUnknown] != 1 || completion.UnresolvedRequirementsTotal != 4 || completion.ImplementationVerificationReports != 1 || completion.ReviewCoverage["partial"] != 1 || completion.GeneralEvidenceReports != 1 || completion.ParentReworkAvailable {
		t.Fatalf("completion analysis = %+v", completion)
	}
	if len(report.Hierarchies) != 1 || report.Hierarchies[0].Completion.Reports != 3 || len(report.Cohorts) != 1 || report.Cohorts[0].Completion.Unavailable != 1 {
		t.Fatalf("hierarchy/cohort completion = %+v / %+v", report.Hierarchies, report.Cohorts)
	}
	var out bytes.Buffer
	if err := WriteAnalysisJSON(report, &out); err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{"SECRET_BLOCKER", "SECRET_PATH", "SECRET_CHECK", "SECRET_DETAIL", "SECRET_UNREVIEWED", "SECRET_GENERAL_BLOCKER", "SECRET_EVIDENCE_PATH", "SECRET_SYMBOL", "SECRET_QUESTION"} {
		if bytes.Contains(out.Bytes(), []byte(secret)) {
			t.Fatalf("completion detail %q leaked into analysis: %s", secret, out.String())
		}
	}
}

func TestStatsJSONIncludesReusableUsageAnalysis(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "session")
	usage := llm.Usage{InputTokens: 12, CacheReadTokens: 3, OutputTokens: 4, CostUSD: .25, CostKnown: true}
	saveStatsFixture(t, dir, Session{Provider: "fake", Model: "test"}, []Event{
		{Type: EventTurnAttemptStart, Prompt: 1, Turn: 1, Attempt: 1},
		{Type: EventTurnAttemptUsage, Prompt: 1, Turn: 1, Attempt: 1, Usage: &usage},
	})
	var out bytes.Buffer
	if err := StatsJSON(dir, &out); err != nil {
		t.Fatal(err)
	}
	var payload struct {
		Version int             `json:"version"`
		Usage   UsageAnalysis   `json:"usage"`
		Storage StorageAnalysis `json:"storage"`
	}
	if err := json.Unmarshal(out.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	if payload.Version != AnalysisVersion || payload.Usage.RootConversational.TotalTokens != 19 || payload.Usage.Inclusive.KnownCostUSD != .25 || !payload.Usage.Inclusive.CostComplete || !payload.Storage.Available || payload.Storage.TotalBytes == 0 {
		t.Fatalf("StatsJSON v2 payload = %+v", payload)
	}
}

func TestAnalyzeV2FrozenFixtureCorpus(t *testing.T) {
	report, err := AnalyzeCorpus(filepath.Join("testdata", "analyze-v2"), AnalyzeOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if report.Roots != 1 || report.Sessions != 2 || report.Usage.RootConversational.TotalTokens != 15 || report.Usage.RootMaintenance.TotalTokens != 6 || report.Usage.DescendantConversational.TotalTokens != 6 || report.Usage.Inclusive.TotalTokens != 27 {
		t.Fatalf("frozen fixture totals = roots %d sessions %d usage %+v", report.Roots, report.Sessions, report.Usage)
	}
	if !report.Usage.Reconciliation.Available || !report.Usage.Reconciliation.Matches || report.Usage.Inclusive.CostComplete || report.Usage.Inclusive.PricedCalls != 2 || report.Usage.Inclusive.UnpricedCalls != 1 {
		t.Fatalf("frozen fixture reconciliation/coverage = %+v", report.Usage)
	}
	if report.Storage.SnapshotResetEntries != 1 || report.Storage.SnapshotPayloadBytes == 0 || report.Storage.Compactions.Bytes == 0 || report.Storage.ToolResults.Bytes == 0 {
		t.Fatalf("frozen fixture storage = %+v", report.Storage)
	}
	var out bytes.Buffer
	if err := WriteAnalysisJSON(report, &out); err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(out.Bytes(), []byte("FROZEN SECRET")) {
		t.Fatalf("frozen fixture content leaked: %s", out.String())
	}
}

func TestCohortIdentitySeparatesModifiedBuilds(t *testing.T) {
	runtime := RuntimeProfile{RetentionPolicy: "pressure"}
	clean := cohortIdentity(BuildMetadata{Version: "dev", Commit: "abc"}, runtime, true)
	modified := cohortIdentity(BuildMetadata{Version: "dev", Commit: "abc", Modified: true}, runtime, true)
	if clean.Key == modified.Key || clean.Key == "" || modified.Key == "" {
		t.Fatalf("cohort keys clean=%q modified=%q", clean.Key, modified.Key)
	}
	if cohortIdentity(BuildMetadata{}, RuntimeProfile{}, false).Key != "unavailable" {
		t.Fatal("legacy metadata was not kept in an unavailable cohort")
	}
}
