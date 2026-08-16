package session

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestValidateChildCompletionReport(t *testing.T) {
	tests := []struct {
		name     string
		report   ChildCompletionReport
		contract string
		want     string
	}{
		{name: "complete", report: ChildCompletionReport{Outcome: ChildCompletionOutcomeComplete, Contract: ChildCompletionContractGeneral}, contract: ChildCompletionContractGeneral, want: ChildCompletionValidationValid},
		{name: "retired partial", report: ChildCompletionReport{Outcome: "partial", Contract: ChildCompletionContractImplementation}, contract: ChildCompletionContractImplementation, want: ChildCompletionValidationInvalid},
		{name: "retired failed", report: ChildCompletionReport{Outcome: "failed", Contract: ChildCompletionContractGeneral}, contract: ChildCompletionContractGeneral, want: ChildCompletionValidationInvalid},
		{name: "blocked", report: ChildCompletionReport{Outcome: ChildCompletionOutcomeBlocked, Blockers: []string{"missing credentials"}, Contract: ChildCompletionContractReview}, contract: ChildCompletionContractReview, want: ChildCompletionValidationValid},
		{name: "bad outcome", report: ChildCompletionReport{Outcome: "done", Contract: ChildCompletionContractGeneral}, contract: ChildCompletionContractGeneral, want: ChildCompletionValidationInvalid},
		{name: "wrong contract", report: ChildCompletionReport{Outcome: ChildCompletionOutcomeComplete, Contract: ChildCompletionContractReview}, contract: ChildCompletionContractGeneral, want: ChildCompletionValidationInvalid},
		{name: "blocked without blocker", report: ChildCompletionReport{Outcome: ChildCompletionOutcomeBlocked, Contract: ChildCompletionContractGeneral}, contract: ChildCompletionContractGeneral, want: ChildCompletionValidationInvalid},
		{name: "oversized blocker", report: ChildCompletionReport{Outcome: ChildCompletionOutcomeBlocked, Blockers: []string{strings.Repeat("x", childCompletionMaxStringBytes+1)}, Contract: ChildCompletionContractGeneral}, contract: ChildCompletionContractGeneral, want: ChildCompletionValidationOversized},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := validateChildCompletionReport(tc.report, tc.contract); got != tc.want {
				t.Fatalf("validation = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestChildCompletionJSONContainsOnlyMinimalFields(t *testing.T) {
	report := ChildCompletionReport{
		Outcome: ChildCompletionOutcomeBlocked, Blockers: []string{"missing credentials"},
		Contract: ChildCompletionContractImplementation, Source: ChildCompletionSourceDeclared,
		ValidationStatus: ChildCompletionValidationValid,
	}
	encoded, err := json.Marshal(report)
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(encoded, &fields); err != nil {
		t.Fatal(err)
	}
	want := []string{"outcome", "blockers", "contract", "source", "validation_status"}
	if len(fields) != len(want) {
		t.Fatalf("persisted fields = %v; JSON=%s", fields, encoded)
	}
	for _, key := range want {
		if _, ok := fields[key]; !ok {
			t.Errorf("persisted JSON missing %q: %s", key, encoded)
		}
	}
}

func TestChildCompletionJSONRejectsRetiredRichSchema(t *testing.T) {
	retired := `{"outcome":"partial","unresolved_requirements":2,"blockers":[],"changed_files":["x.go"],"verification":[{"check":"go test ./...","status":"passed"}],"contract":"implementation","source":"child_declared","validation_status":"valid"}`
	var report ChildCompletionReport
	if err := json.Unmarshal([]byte(retired), &report); err == nil {
		t.Fatalf("retired completion schema decoded as current metadata: %+v", report)
	}
}
