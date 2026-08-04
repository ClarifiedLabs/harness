package session

import (
	"bytes"
	"encoding/json"
	"strings"
	"unicode/utf8"
)

// ChildCompletionMaxUnresolvedRequirements bounds child-declared cardinality.
// Analyzer aggregation revalidates persisted reports against the same ceiling.
const ChildCompletionMaxUnresolvedRequirements = 1_000_000

const (
	childCompletionMaxListItems       = 128
	childCompletionMaxStringBytes     = 1024
	childCompletionMaxDetailBytes     = 2048
	childCompletionMaxEvidenceItems   = 128
	childCompletionMaxBlockerItems    = 32
	childCompletionMaxVerification    = 32
	childCompletionMaxQuestionItems   = 64
	childCompletionMaxUnreviewedItems = 128
)

// UnmarshalJSON preserves whether the required unresolved_requirements field
// was physically present in persisted metadata instead of conflating omission
// with a declared zero.
func (r *ChildCompletionReport) UnmarshalJSON(data []byte) error {
	type reportAlias ChildCompletionReport
	var decoded reportAlias
	if err := json.Unmarshal(data, &decoded); err != nil {
		return err
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		return err
	}
	*r = ChildCompletionReport(decoded)
	raw, ok := fields["unresolved_requirements"]
	r.unresolvedRequirementsPresent = ok && !bytes.Equal(bytes.TrimSpace(raw), []byte("null"))
	return nil
}

// validateChildCompletionReport revalidates a persisted host-declared report.
// It returns valid, invalid, or oversized and never returns metadata-authored
// strings, keeping analyzer map keys bounded to the host vocabulary.
func validateChildCompletionReport(report ChildCompletionReport, expectedContract string) string {
	if report.Contract != expectedContract {
		return ChildCompletionValidationInvalid
	}
	switch report.Outcome {
	case ChildCompletionOutcomeComplete, ChildCompletionOutcomePartial,
		ChildCompletionOutcomeBlocked, ChildCompletionOutcomeFailed:
	default:
		return ChildCompletionValidationInvalid
	}
	if !report.unresolvedRequirementsPresent || report.UnresolvedRequirements < 0 || report.UnresolvedRequirements > ChildCompletionMaxUnresolvedRequirements ||
		report.Outcome == ChildCompletionOutcomeComplete && report.UnresolvedRequirements != 0 {
		return ChildCompletionValidationInvalid
	}
	if report.Outcome == ChildCompletionOutcomeBlocked && len(report.Blockers) == 0 {
		return ChildCompletionValidationInvalid
	}
	if !validCompletionStrings(report.Blockers, childCompletionMaxBlockerItems, childCompletionMaxStringBytes, false) {
		return ChildCompletionValidationOversized
	}

	switch expectedContract {
	case ChildCompletionContractImplementation:
		if report.ChangedFiles == nil || report.Verification == nil || len(report.Verification) == 0 ||
			report.Coverage != "" || report.UnreviewedScope != nil || report.Evidence != nil || report.UnresolvedQuestions != nil {
			return ChildCompletionValidationInvalid
		}
		if !validCompletionStrings(report.ChangedFiles, childCompletionMaxListItems, childCompletionMaxStringBytes, false) || len(report.Verification) > childCompletionMaxVerification {
			return ChildCompletionValidationOversized
		}
		for _, verification := range report.Verification {
			if !validCompletionString(verification.Check, childCompletionMaxStringBytes, false) || !validCompletionString(verification.Detail, childCompletionMaxDetailBytes, true) {
				return ChildCompletionValidationOversized
			}
			switch verification.Status {
			case "passed":
			case "not_run":
				if strings.TrimSpace(verification.Detail) == "" {
					return ChildCompletionValidationInvalid
				}
			case "failed":
				if report.Outcome == ChildCompletionOutcomeComplete {
					return ChildCompletionValidationInvalid
				}
			default:
				return ChildCompletionValidationInvalid
			}
		}
	case ChildCompletionContractReview:
		if report.ChangedFiles != nil || report.Verification != nil || report.UnreviewedScope == nil ||
			report.Evidence != nil || report.UnresolvedQuestions != nil {
			return ChildCompletionValidationInvalid
		}
		if report.Coverage != "complete" && report.Coverage != "partial" {
			return ChildCompletionValidationInvalid
		}
		if report.Outcome == ChildCompletionOutcomeComplete && (report.Coverage != "complete" || len(report.UnreviewedScope) != 0) {
			return ChildCompletionValidationInvalid
		}
		if !validCompletionStrings(report.UnreviewedScope, childCompletionMaxUnreviewedItems, childCompletionMaxStringBytes, false) {
			return ChildCompletionValidationOversized
		}
	case ChildCompletionContractGeneral:
		if report.ChangedFiles != nil || report.Verification != nil || report.Coverage != "" || report.UnreviewedScope != nil ||
			report.Evidence == nil || report.UnresolvedQuestions == nil {
			return ChildCompletionValidationInvalid
		}
		if len(report.Evidence) > childCompletionMaxEvidenceItems || len(report.UnresolvedQuestions) > childCompletionMaxQuestionItems {
			return ChildCompletionValidationOversized
		}
		for _, evidence := range report.Evidence {
			if !validCompletionString(evidence.Path, childCompletionMaxStringBytes, false) || !validCompletionString(evidence.Symbol, childCompletionMaxStringBytes, true) {
				return ChildCompletionValidationOversized
			}
		}
		if !validCompletionStrings(report.UnresolvedQuestions, childCompletionMaxQuestionItems, childCompletionMaxStringBytes, false) {
			return ChildCompletionValidationOversized
		}
	default:
		return ChildCompletionValidationInvalid
	}
	return ChildCompletionValidationValid
}

func validCompletionStrings(values []string, maxItems, maxBytes int, allowEmpty bool) bool {
	if len(values) > maxItems {
		return false
	}
	for _, value := range values {
		if !validCompletionString(value, maxBytes, allowEmpty) {
			return false
		}
	}
	return true
}

func validCompletionString(value string, maxBytes int, allowEmpty bool) bool {
	return utf8.ValidString(value) && len(value) <= maxBytes && (allowEmpty || strings.TrimSpace(value) != "")
}
