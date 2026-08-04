package session

import (
	"encoding/json"
	"testing"
)

func persistedCompletion(report ChildCompletionReport) ChildCompletionReport {
	report.unresolvedRequirementsPresent = true
	return report
}

func TestValidateChildCompletionReportContracts(t *testing.T) {
	validImplementation := persistedCompletion(ChildCompletionReport{
		Outcome: ChildCompletionOutcomeComplete, Contract: ChildCompletionContractImplementation,
		ChangedFiles: []string{}, Verification: []ChildCompletionVerification{{Check: "go test ./...", Status: "passed"}},
	})
	tests := []struct {
		name     string
		report   ChildCompletionReport
		contract string
		want     string
	}{
		{name: "implementation", report: validImplementation, contract: ChildCompletionContractImplementation, want: ChildCompletionValidationValid},
		{name: "wrong contract", report: validImplementation, contract: ChildCompletionContractGeneral, want: ChildCompletionValidationInvalid},
		{name: "missing unresolved count", report: ChildCompletionReport{Outcome: ChildCompletionOutcomePartial, Contract: ChildCompletionContractGeneral, Evidence: []ChildCompletionEvidence{}, UnresolvedQuestions: []string{}}, contract: ChildCompletionContractGeneral, want: ChildCompletionValidationInvalid},
		{name: "unresolved cap", report: persistedCompletion(ChildCompletionReport{Outcome: ChildCompletionOutcomePartial, UnresolvedRequirements: ChildCompletionMaxUnresolvedRequirements + 1, Contract: ChildCompletionContractGeneral, Evidence: []ChildCompletionEvidence{}, UnresolvedQuestions: []string{}}), contract: ChildCompletionContractGeneral, want: ChildCompletionValidationInvalid},
		{name: "cross contract field", report: persistedCompletion(ChildCompletionReport{Outcome: ChildCompletionOutcomePartial, UnresolvedRequirements: 1, Contract: ChildCompletionContractGeneral, ChangedFiles: []string{}, Evidence: []ChildCompletionEvidence{}, UnresolvedQuestions: []string{}}), contract: ChildCompletionContractGeneral, want: ChildCompletionValidationInvalid},
		{name: "oversized blocker", report: persistedCompletion(ChildCompletionReport{Outcome: ChildCompletionOutcomePartial, Contract: ChildCompletionContractGeneral, Blockers: []string{string(make([]byte, childCompletionMaxStringBytes+1))}, Evidence: []ChildCompletionEvidence{}, UnresolvedQuestions: []string{}}), contract: ChildCompletionContractGeneral, want: ChildCompletionValidationOversized},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := validateChildCompletionReport(tc.report, tc.contract); got != tc.want {
				t.Fatalf("validation = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestChildCompletionJSONPreservesExplicitEmptyContractArrays(t *testing.T) {
	original := ChildCompletionReport{
		Outcome: ChildCompletionOutcomeComplete, Contract: ChildCompletionContractImplementation,
		ChangedFiles: []string{}, Verification: []ChildCompletionVerification{{Check: "test", Status: "passed"}},
	}
	encoded, err := json.Marshal(original)
	if err != nil {
		t.Fatal(err)
	}
	var decoded ChildCompletionReport
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.ChangedFiles == nil || decoded.Verification == nil {
		t.Fatalf("explicit arrays lost across JSON round trip: %s", encoded)
	}
	if got := validateChildCompletionReport(decoded, ChildCompletionContractImplementation); got != ChildCompletionValidationValid {
		t.Fatalf("round-trip validation = %q; JSON=%s", got, encoded)
	}
}
