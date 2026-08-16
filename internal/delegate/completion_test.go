package delegate

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"harness/internal/llm"
	"harness/internal/llm/llmtest"
	"harness/internal/session"
	"harness/internal/tools"
)

func completionBlock(body string) string {
	return completionFence + "\n" + body + "\n```"
}

func TestParseCompletionReportContractsAndFallbacks(t *testing.T) {
	validGeneral := completionBlock(`{"outcome":"complete","unresolved_requirements":0,"evidence":[],"unresolved_questions":[]}`)
	validImplementation := completionBlock(`{"outcome":"complete","unresolved_requirements":0,"changed_files":[],"verification":[{"check":"go test ./...","status":"not_run","detail":"Go is unavailable"}]}`)
	validReview := completionBlock(`{"outcome":"complete","unresolved_requirements":0,"coverage":"complete","unreviewed_scope":[]}`)
	tooManyFiles := make([]string, maxCompletionListItems+1)
	for i := range tooManyFiles {
		tooManyFiles[i] = "x"
	}
	oversizedList := `{"outcome":"partial","unresolved_requirements":1,"changed_files":["` + strings.Join(tooManyFiles, `","`) + `"],"verification":[{"check":"test","status":"passed"}]}`
	tooMuchEvidence := make([]string, maxCompletionEvidenceItems+1)
	for i := range tooMuchEvidence {
		tooMuchEvidence[i] = `{"path":"x"}`
	}
	oversizedEvidence := `{"outcome":"partial","unresolved_requirements":1,"evidence":[` + strings.Join(tooMuchEvidence, ",") + `],"unresolved_questions":[]}`

	tests := []struct {
		name       string
		text       string
		contract   string
		outcome    string
		validation string
		prose      string
	}{
		{name: "general valid and stripped", text: "Evidence summary.\n\n" + validGeneral, contract: session.ChildCompletionContractGeneral, outcome: session.ChildCompletionOutcomeComplete, validation: session.ChildCompletionValidationValid, prose: "Evidence summary."},
		{name: "ordinary JSON fence ignored", text: "```json\n{}\n```\n\n" + validGeneral, contract: session.ChildCompletionContractGeneral, outcome: session.ChildCompletionOutcomeComplete, validation: session.ChildCompletionValidationValid, prose: "```json\n{}\n```"},
		{name: "implementation explicit fields", text: validImplementation, contract: session.ChildCompletionContractImplementation, outcome: session.ChildCompletionOutcomeComplete, validation: session.ChildCompletionValidationValid, prose: ""},
		{name: "review explicit coverage", text: validReview, contract: session.ChildCompletionContractReview, outcome: session.ChildCompletionOutcomeComplete, validation: session.ChildCompletionValidationValid, prose: ""},
		{name: "missing", text: "useful legacy prose", contract: session.ChildCompletionContractGeneral, outcome: session.ChildCompletionOutcomeUnknown, validation: session.ChildCompletionValidationMissing, prose: "useful legacy prose"},
		{name: "malformed JSON", text: "keep\n" + completionBlock(`{"outcome":`), contract: session.ChildCompletionContractGeneral, outcome: session.ChildCompletionOutcomeUnknown, validation: session.ChildCompletionValidationMalformed, prose: "keep\n" + completionBlock(`{"outcome":`)},
		{name: "unknown field", text: completionBlock(`{"outcome":"complete","unresolved_requirements":0,"evidence":[],"unresolved_questions":[],"extra":true}`), contract: session.ChildCompletionContractGeneral, outcome: session.ChildCompletionOutcomeUnknown, validation: session.ChildCompletionValidationMalformed},
		{name: "complete unresolved", text: completionBlock(`{"outcome":"complete","unresolved_requirements":1,"evidence":[],"unresolved_questions":[]}`), contract: session.ChildCompletionContractGeneral, outcome: session.ChildCompletionOutcomeUnknown, validation: session.ChildCompletionValidationInvalid},
		{name: "unresolved missing", text: completionBlock(`{"outcome":"complete","evidence":[],"unresolved_questions":[]}`), contract: session.ChildCompletionContractGeneral, outcome: session.ChildCompletionOutcomeUnknown, validation: session.ChildCompletionValidationInvalid},
		{name: "unresolved over cap", text: completionBlock(`{"outcome":"partial","unresolved_requirements":1000001,"evidence":[],"unresolved_questions":[]}`), contract: session.ChildCompletionContractGeneral, outcome: session.ChildCompletionOutcomeUnknown, validation: session.ChildCompletionValidationInvalid},
		{name: "general rejects implementation fields", text: completionBlock(`{"outcome":"partial","unresolved_requirements":1,"changed_files":[],"evidence":[],"unresolved_questions":[]}`), contract: session.ChildCompletionContractGeneral, outcome: session.ChildCompletionOutcomeUnknown, validation: session.ChildCompletionValidationInvalid},
		{name: "implementation rejects general fields", text: completionBlock(`{"outcome":"partial","unresolved_requirements":1,"changed_files":[],"verification":[{"check":"test","status":"passed"}],"evidence":[]}`), contract: session.ChildCompletionContractImplementation, outcome: session.ChildCompletionOutcomeUnknown, validation: session.ChildCompletionValidationInvalid},
		{name: "implementation fields missing degrades to partial", text: completionBlock(`{"outcome":"partial","unresolved_requirements":1}`), contract: session.ChildCompletionContractImplementation, outcome: session.ChildCompletionOutcomePartial, validation: session.ChildCompletionValidationPartialFields, prose: ""},
		{name: "review fields missing degrades to partial", text: completionBlock(`{"outcome":"partial","unresolved_requirements":1}`), contract: session.ChildCompletionContractReview, outcome: session.ChildCompletionOutcomePartial, validation: session.ChildCompletionValidationPartialFields, prose: ""},
		{name: "review partial contract fields still invalid", text: completionBlock(`{"outcome":"partial","unresolved_requirements":1,"coverage":"partial"}`), contract: session.ChildCompletionContractReview, outcome: session.ChildCompletionOutcomeUnknown, validation: session.ChildCompletionValidationInvalid},
		{name: "bounded list", text: completionBlock(oversizedList), contract: session.ChildCompletionContractImplementation, outcome: session.ChildCompletionOutcomeUnknown, validation: session.ChildCompletionValidationOversized},
		{name: "bounded evidence", text: completionBlock(oversizedEvidence), contract: session.ChildCompletionContractGeneral, outcome: session.ChildCompletionOutcomeUnknown, validation: session.ChildCompletionValidationOversized},
		{name: "oversized block", text: completionBlock(strings.Repeat("x", maxCompletionBlockBytes+1)), contract: session.ChildCompletionContractGeneral, outcome: session.ChildCompletionOutcomeUnknown, validation: session.ChildCompletionValidationOversized},
		{name: "duplicate", text: validGeneral + "\n" + validGeneral, contract: session.ChildCompletionContractGeneral, outcome: session.ChildCompletionOutcomeUnknown, validation: session.ChildCompletionValidationDuplicate, prose: validGeneral + "\n" + validGeneral},
		{name: "not final", text: validGeneral + "\ntrailing prose", contract: session.ChildCompletionContractGeneral, outcome: session.ChildCompletionOutcomeUnknown, validation: session.ChildCompletionValidationMalformed, prose: validGeneral + "\ntrailing prose"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			report, prose := parseCompletionReport(tc.text, tc.contract)
			if report.Outcome != tc.outcome || report.ValidationStatus != tc.validation || report.Contract != tc.contract {
				t.Fatalf("report = %+v", report)
			}
			if report.ValidationStatus == session.ChildCompletionValidationValid && report.Source != session.ChildCompletionSourceDeclared {
				t.Fatalf("valid source = %q", report.Source)
			}
			if report.ValidationStatus != session.ChildCompletionValidationValid && report.ValidationStatus != session.ChildCompletionValidationPartialFields && report.Source != session.ChildCompletionSourceCompatibility {
				t.Fatalf("fallback source = %q", report.Source)
			}
			usable := report.ValidationStatus == session.ChildCompletionValidationValid || report.ValidationStatus == session.ChildCompletionValidationPartialFields
			wantProse := tc.prose
			if wantProse == "" && !usable {
				wantProse = tc.text
			}
			if prose != wantProse {
				t.Fatalf("prose = %q, want %q", prose, wantProse)
			}
		})
	}
}

func TestParseCompletionReportPartialFieldsFillsDefaults(t *testing.T) {
	// The observed failure mode: children emit the generic core only. Each
	// contract must preserve the declared outcome and unresolved count, fill
	// empty defaults for its contract fields, and stay a usable report.
	generic := `{"outcome":"partial","unresolved_requirements":2,"blockers":[]}`
	for _, tc := range []struct {
		contract string
		check    func(CompletionReport) error
	}{{
		contract: session.ChildCompletionContractGeneral,
		check: func(r CompletionReport) error {
			if r.Evidence == nil || len(r.Evidence) != 0 {
				return fmt.Errorf("evidence = %#v, want empty non-nil default", r.Evidence)
			}
			if r.UnresolvedQuestions == nil || len(r.UnresolvedQuestions) != 0 {
				return fmt.Errorf("unresolved_questions = %#v, want empty non-nil default", r.UnresolvedQuestions)
			}
			return nil
		},
	}, {
		contract: session.ChildCompletionContractReview,
		check: func(r CompletionReport) error {
			if r.Coverage != "" {
				return fmt.Errorf("coverage = %q, want empty default", r.Coverage)
			}
			if r.UnreviewedScope == nil || len(r.UnreviewedScope) != 0 {
				return fmt.Errorf("unreviewed_scope = %#v, want empty non-nil default", r.UnreviewedScope)
			}
			return nil
		},
	}, {
		contract: session.ChildCompletionContractImplementation,
		check: func(r CompletionReport) error {
			if r.ChangedFiles == nil || len(r.ChangedFiles) != 0 {
				return fmt.Errorf("changed_files = %#v, want empty non-nil default", r.ChangedFiles)
			}
			if r.Verification == nil || len(r.Verification) != 0 {
				return fmt.Errorf("verification = %#v, want empty non-nil default", r.Verification)
			}
			return nil
		},
	}} {
		t.Run(tc.contract, func(t *testing.T) {
			report, prose := parseCompletionReport("summary.\n"+completionBlock(generic), tc.contract)
			if report.ValidationStatus != session.ChildCompletionValidationPartialFields {
				t.Fatalf("validation = %q, want partial_fields", report.ValidationStatus)
			}
			if report.Outcome != session.ChildCompletionOutcomePartial || report.UnresolvedRequirements != 2 {
				t.Fatalf("declared core not preserved: %+v", report)
			}
			if report.Source != session.ChildCompletionSourceDeclared {
				t.Fatalf("source = %q, want child_declared", report.Source)
			}
			if err := tc.check(report); err != nil {
				t.Fatal(err)
			}
			if prose != "summary." {
				t.Fatalf("prose = %q", prose)
			}
		})
	}
}

func TestParseCompletionReportStillInvalidatesWrongContent(t *testing.T) {
	for _, tc := range []struct {
		name     string
		body     string
		contract string
	}{
		{name: "bad outcome enum", body: `{"outcome":"done","unresolved_requirements":0}`, contract: session.ChildCompletionContractGeneral},
		{name: "complete with unresolved", body: `{"outcome":"complete","unresolved_requirements":1}`, contract: session.ChildCompletionContractGeneral},
		{name: "blocked without blockers", body: `{"outcome":"blocked","unresolved_requirements":1}`, contract: session.ChildCompletionContractGeneral},
		{name: "blocked with empty blockers", body: `{"outcome":"blocked","unresolved_requirements":1,"blockers":[]}`, contract: session.ChildCompletionContractGeneral},
		{name: "unresolved missing", body: `{"outcome":"complete","evidence":[]}`, contract: session.ChildCompletionContractGeneral},
		{name: "partial contract fields on review", body: `{"outcome":"partial","unresolved_requirements":1,"coverage":"partial"}`, contract: session.ChildCompletionContractReview},
		{name: "complete review with partial coverage", body: `{"outcome":"complete","unresolved_requirements":0,"coverage":"partial","unreviewed_scope":["src/"]}`, contract: session.ChildCompletionContractReview},
		{name: "foreign field on review", body: `{"outcome":"partial","unresolved_requirements":1,"coverage":"partial","unreviewed_scope":[],"evidence":[]}`, contract: session.ChildCompletionContractReview},
		{name: "verification only on implementation", body: `{"outcome":"partial","unresolved_requirements":1,"verification":[]}`, contract: session.ChildCompletionContractImplementation},
	} {
		t.Run(tc.name, func(t *testing.T) {
			report, _ := parseCompletionReport(completionBlock(tc.body), tc.contract)
			if report.ValidationStatus != session.ChildCompletionValidationInvalid {
				t.Fatalf("validation = %q, want invalid", report.ValidationStatus)
			}
			if report.Outcome != session.ChildCompletionOutcomeUnknown || report.Source != session.ChildCompletionSourceCompatibility {
				t.Fatalf("invalid report not discarded: %+v", report)
			}
		})
	}
}

func TestCompletionContractSystemPromptIncludesCommonAndSpecificSchema(t *testing.T) {
	tests := []struct {
		contract string
		specific []string
		// exampleKeys must all appear inside the fenced example block, so a
		// generic core-only example cannot satisfy the assertion.
		exampleKeys []string
	}{
		{contract: session.ChildCompletionContractImplementation, specific: []string{"changed_files", "verification", "not_run"}, exampleKeys: []string{"outcome", "unresolved_requirements", "blockers", "changed_files", "verification"}},
		{contract: session.ChildCompletionContractReview, specific: []string{"coverage", "unreviewed_scope"}, exampleKeys: []string{"outcome", "unresolved_requirements", "blockers", "coverage", "unreviewed_scope"}},
		{contract: session.ChildCompletionContractGeneral, specific: []string{"evidence", "unresolved_questions"}, exampleKeys: []string{"outcome", "unresolved_requirements", "blockers", "evidence", "unresolved_questions"}},
	}
	for _, tc := range tests {
		t.Run(tc.contract, func(t *testing.T) {
			prompt := completionContractSystemPrompt("base", tc.contract)
			for _, want := range append([]string{"harness-completion", "outcome", "unresolved_requirements", "blockers", "complete", "zero unresolved"}, tc.specific...) {
				if !strings.Contains(prompt, want) {
					t.Errorf("prompt missing %q: %s", want, prompt)
				}
			}
			start := strings.Index(prompt, completionFence)
			if start < 0 {
				t.Fatalf("prompt has no fenced example: %s", prompt)
			}
			block := prompt[start:]
			if end := strings.Index(block[len(completionFence):], "```"); end >= 0 {
				block = block[:len(completionFence)+end]
			}
			lines := strings.SplitN(block, "\n", 2)
			if len(lines) != 2 {
				t.Fatalf("fenced example has no body: %s", block)
			}
			var decoded map[string]json.RawMessage
			if err := json.Unmarshal([]byte(strings.TrimSpace(lines[1])), &decoded); err != nil {
				t.Fatalf("fenced example is not valid JSON: %v\n%s", err, block)
			}
			for _, key := range tc.exampleKeys {
				if _, ok := decoded[key]; !ok {
					t.Errorf("example JSON missing %q: %s", key, block)
				}
			}
		})
	}
}

func TestRunnerUsesOnlyFinalAssistantCompletionReport(t *testing.T) {
	childTools := &tools.Registry{}
	childTools.Register(fakeChildTool{name: "read", out: "contents"})
	early := completionBlock(`{"outcome":"complete","unresolved_requirements":0,"evidence":[],"unresolved_questions":[]}`)
	provider := llmtest.New("fake",
		llmtest.Step{
			Events: []llm.StreamEvent{
				{Kind: llm.EventTextDelta, Text: early},
				{Kind: llm.EventToolCallDone, ToolID: "read", ToolName: "read", ToolInput: []byte(`{}`)},
			},
			Stop: llm.StopToolUse,
		},
		llmtest.Step{Events: []llm.StreamEvent{{Kind: llm.EventTextDelta, Text: "actual final prose"}}, Stop: llm.StopEndTurn},
	)
	runner := NewRunner(func() Runtime {
		return Runtime{Provider: provider, Model: "model", Registry: llm.NewRegistry(nil)}
	}, func(runtime Runtime, _ string) (Launch, error) {
		return Launch{Provider: runtime.Provider, Model: runtime.Model, Registry: runtime.Registry, Tools: childTools}, nil
	}, Options{MaxTurns: 2})

	result, err := runner.Run(context.Background(), RunRequest{Task: "inspect", ChildID: "final-only"}, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if result.Completion.Outcome != session.ChildCompletionOutcomeUnknown || result.Completion.ValidationStatus != session.ChildCompletionValidationMissing {
		t.Fatalf("completion = %+v", result.Completion)
	}
	if !strings.Contains(result.Report, "actual final prose") || strings.Contains(result.Report, completionFence) {
		t.Fatalf("report = %q", result.Report)
	}
}

func TestDelegateCompletionPersistsAndDoesNotInferFromTermination(t *testing.T) {
	final := "Scoped findings.\n\n" + completionBlock(`{"outcome":"partial","unresolved_requirements":2,"evidence":[{"path":"internal/delegate/delegate.go","symbol":"Runner.Run"}],"unresolved_questions":["promotion canary"]}`)
	fixture := newContinuationFixture(t, 100_000, false, llmtest.Step{
		Events: []llm.StreamEvent{{Kind: llm.EventTextDelta, Text: final}},
		Stop:   llm.StopEndTurn,
	})
	result, err := fixture.runner.Run(context.Background(), RunRequest{Task: "inspect", ChildID: "reported"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if result.TerminationReason != "model_completed" || result.Completion.Outcome != session.ChildCompletionOutcomePartial || result.Completion.UnresolvedRequirements != 2 {
		t.Fatalf("termination/completion = %q %+v", result.TerminationReason, result.Completion)
	}
	if strings.Contains(result.Report, completionFence) || !strings.Contains(result.Report, "Scoped findings.") || !strings.Contains(result.Report, "child_declared/valid") {
		t.Fatalf("parent report = %q", result.Report)
	}
	meta := readDelegateChildMeta(t, session.ChildSessionDir(fixture.sessionPath, "reported"))
	persistedJSON, persistedErr := json.Marshal(meta.Completion)
	resultJSON, resultErr := json.Marshal(result.Completion)
	if meta.Status != session.ChildStatusCompleted || meta.Completion == nil || persistedErr != nil || resultErr != nil || !bytes.Equal(persistedJSON, resultJSON) {
		t.Fatalf("persisted metadata = %+v; persisted JSON=%s (%v), result JSON=%s (%v)", meta, persistedJSON, persistedErr, resultJSON, resultErr)
	}
}

func TestDelegateHarnessFailureBeforeFinalReportIsHostUnknown(t *testing.T) {
	fixture := newContinuationFixture(t, 100_000, false, llmtest.Step{
		Events: []llm.StreamEvent{{Kind: llm.EventTextDelta, Text: "should not run"}},
		Stop:   llm.StopEndTurn,
	})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	result, err := fixture.runner.Run(ctx, RunRequest{Task: "inspect", ChildID: "cancelled"}, nil)
	if err == nil {
		t.Fatal("cancelled run returned nil error")
	}
	if result.Completion.Outcome != session.ChildCompletionOutcomeUnknown || result.Completion.Source != session.ChildCompletionSourceHost || result.Completion.ValidationStatus != session.ChildCompletionValidationUnavailable {
		t.Fatalf("completion = %+v", result.Completion)
	}
	meta := readDelegateChildMeta(t, session.ChildSessionDir(fixture.sessionPath, "cancelled"))
	if meta.Completion == nil || meta.Completion.Source != session.ChildCompletionSourceHost || meta.TerminationReason != "cancelled" {
		t.Fatalf("metadata = %+v", meta)
	}
}
