package delegate

import (
	"bytes"
	"context"
	"encoding/json"
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

func TestParseCompletionReportAndFallbacks(t *testing.T) {
	valid := completionBlock(`{"outcome":"complete"}`)
	retiredRich := completionBlock(`{"outcome":"complete","unresolved_requirements":0,"changed_files":["x.go"],"verification":[{"check":"go test ./...","status":"passed"}]}`)
	blocked := completionBlock(`{"outcome":"blocked","blockers":["missing credentials"]}`)
	tooMany := make([]string, maxCompletionBlockerItems+1)
	for i := range tooMany {
		tooMany[i] = "x"
	}
	tooManyJSON, err := json.Marshal(map[string]any{"outcome": "blocked", "blockers": tooMany})
	if err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name       string
		text       string
		contract   string
		outcome    string
		validation string
		prose      string
	}{
		{name: "valid and stripped", text: "Markdown summary.\n\n" + valid, contract: session.ChildCompletionContractGeneral, outcome: session.ChildCompletionOutcomeComplete, validation: session.ChildCompletionValidationValid, prose: "Markdown summary."},
		{name: "ordinary JSON fence ignored", text: "```json\n{}\n```\n\n" + valid, contract: session.ChildCompletionContractGeneral, outcome: session.ChildCompletionOutcomeComplete, validation: session.ChildCompletionValidationValid, prose: "```json\n{}\n```"},
		{name: "retired rich fields rejected", text: retiredRich, contract: session.ChildCompletionContractImplementation, outcome: session.ChildCompletionOutcomeUnknown, validation: session.ChildCompletionValidationMalformed},
		{name: "retired partial outcome", text: completionBlock(`{"outcome":"partial"}`), contract: session.ChildCompletionContractGeneral, outcome: session.ChildCompletionOutcomeUnknown, validation: session.ChildCompletionValidationInvalid},
		{name: "retired failed outcome", text: completionBlock(`{"outcome":"failed"}`), contract: session.ChildCompletionContractGeneral, outcome: session.ChildCompletionOutcomeUnknown, validation: session.ChildCompletionValidationInvalid},
		{name: "CRLF footer", text: "Markdown\r\n\r\n```harness-completion\r\n{\"outcome\":\"complete\"}\r\n```", contract: session.ChildCompletionContractGeneral, outcome: session.ChildCompletionOutcomeComplete, validation: session.ChildCompletionValidationValid, prose: "Markdown"},
		{name: "blocked with blocker", text: blocked, contract: session.ChildCompletionContractReview, outcome: session.ChildCompletionOutcomeBlocked, validation: session.ChildCompletionValidationValid, prose: ""},
		{name: "missing optional footer", text: "useful Markdown", contract: session.ChildCompletionContractGeneral, outcome: session.ChildCompletionOutcomeUnknown, validation: session.ChildCompletionValidationMissing, prose: "useful Markdown"},
		{name: "malformed JSON", text: "keep\n" + completionBlock(`{"outcome":`), contract: session.ChildCompletionContractGeneral, outcome: session.ChildCompletionOutcomeUnknown, validation: session.ChildCompletionValidationMalformed, prose: "keep\n" + completionBlock(`{"outcome":`)},
		{name: "bad outcome", text: completionBlock(`{"outcome":"done"}`), contract: session.ChildCompletionContractGeneral, outcome: session.ChildCompletionOutcomeUnknown, validation: session.ChildCompletionValidationInvalid},
		{name: "blocked without blocker", text: completionBlock(`{"outcome":"blocked"}`), contract: session.ChildCompletionContractGeneral, outcome: session.ChildCompletionOutcomeUnknown, validation: session.ChildCompletionValidationInvalid},
		{name: "oversized blocker list", text: completionBlock(string(tooManyJSON)), contract: session.ChildCompletionContractGeneral, outcome: session.ChildCompletionOutcomeUnknown, validation: session.ChildCompletionValidationOversized},
		{name: "oversized block", text: completionBlock(strings.Repeat("x", maxCompletionBlockBytes+1)), contract: session.ChildCompletionContractGeneral, outcome: session.ChildCompletionOutcomeUnknown, validation: session.ChildCompletionValidationOversized},
		{name: "duplicate", text: valid + "\n" + valid, contract: session.ChildCompletionContractGeneral, outcome: session.ChildCompletionOutcomeUnknown, validation: session.ChildCompletionValidationDuplicate, prose: valid + "\n" + valid},
		{name: "not final", text: valid + "\ntrailing prose", contract: session.ChildCompletionContractGeneral, outcome: session.ChildCompletionOutcomeUnknown, validation: session.ChildCompletionValidationMalformed, prose: valid + "\ntrailing prose"},
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
			if report.ValidationStatus != session.ChildCompletionValidationValid && report.Source != session.ChildCompletionSourceCompatibility {
				t.Fatalf("fallback source = %q", report.Source)
			}
			wantProse := tc.prose
			if wantProse == "" && report.ValidationStatus != session.ChildCompletionValidationValid {
				wantProse = tc.text
			}
			if prose != wantProse {
				t.Fatalf("prose = %q, want %q", prose, wantProse)
			}
		})
	}
}

func TestCompletionSystemPromptIsMarkdownFirstAndFooterOptional(t *testing.T) {
	prompt := completionSystemPrompt("base")
	for _, want := range []string{"useful Markdown report", "may optionally end", "harness-completion", `{"outcome":"complete"}`, `{"outcome":"blocked","blockers":`, "Omit the footer rather than guessing"} {
		if !strings.Contains(prompt, want) {
			t.Errorf("prompt missing %q: %s", want, prompt)
		}
	}
	for _, retired := range []string{"unresolved_requirements", "changed_files", "verification as", "unreviewed_scope", "unresolved_questions", `"outcome":"partial"`, `"outcome":"failed"`} {
		if strings.Contains(prompt, retired) {
			t.Errorf("prompt retained contract-specific field %q: %s", retired, prompt)
		}
	}
}

func TestCompletionReceiptKeepsOptionalFooterConcise(t *testing.T) {
	if got := completionReceipt(CompletionReport{Outcome: session.ChildCompletionOutcomeComplete, ValidationStatus: session.ChildCompletionValidationValid}); got != "[delegate completion: complete]" {
		t.Fatalf("valid receipt = %q", got)
	}
	if got := completionReceipt(unknownCompletion(session.ChildCompletionContractGeneral, session.ChildCompletionSourceCompatibility, session.ChildCompletionValidationMissing)); got != "[delegate completion: unreported]" {
		t.Fatalf("missing receipt = %q", got)
	}
	if got := completionReceipt(unknownCompletion(session.ChildCompletionContractGeneral, session.ChildCompletionSourceHost, session.ChildCompletionValidationUnavailable)); !strings.Contains(got, "host/unavailable") {
		t.Fatalf("host failure receipt = %q", got)
	}
}

func TestRunnerUsesOnlyFinalAssistantCompletionReport(t *testing.T) {
	childTools := &tools.Registry{}
	childTools.Register(fakeChildTool{name: "read", out: "contents"})
	early := completionBlock(`{"outcome":"complete"}`)
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
	final := "Scoped findings.\n\n" + completionBlock(`{"outcome":"blocked","blockers":["remaining work"]}`)
	fixture := newContinuationFixture(t, 100_000, false, llmtest.Step{
		Events: []llm.StreamEvent{{Kind: llm.EventTextDelta, Text: final}},
		Stop:   llm.StopEndTurn,
	})
	result, err := fixture.runner.Run(context.Background(), RunRequest{Task: "inspect", ChildID: "reported"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if result.TerminationReason != "model_completed" || result.Completion.Outcome != session.ChildCompletionOutcomeBlocked {
		t.Fatalf("termination/completion = %q %+v", result.TerminationReason, result.Completion)
	}
	if strings.Contains(result.Report, completionFence) || !strings.Contains(result.Report, "Scoped findings.") || !strings.Contains(result.Report, "delegate completion: blocked") {
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
