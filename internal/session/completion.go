package session

import (
	"bytes"
	"encoding/json"
	"strings"
	"unicode/utf8"
)

const (
	childCompletionMaxBlockerItems = 32
	childCompletionMaxStringBytes  = 1024
)

// UnmarshalJSON rejects metadata from completion schema versions other than the
// current minimal shape. Analyze older sessions with the matching Harness binary.
func (r *ChildCompletionReport) UnmarshalJSON(data []byte) error {
	type reportAlias ChildCompletionReport
	var decoded reportAlias
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&decoded); err != nil {
		return err
	}
	*r = ChildCompletionReport(decoded)
	return nil
}

// validateChildCompletionReport revalidates persisted host metadata.
func validateChildCompletionReport(report ChildCompletionReport, expectedContract string) string {
	if report.Contract != expectedContract {
		return ChildCompletionValidationInvalid
	}
	switch report.Outcome {
	case ChildCompletionOutcomeComplete, ChildCompletionOutcomeBlocked:
	default:
		return ChildCompletionValidationInvalid
	}
	if report.Outcome == ChildCompletionOutcomeBlocked && len(report.Blockers) == 0 {
		return ChildCompletionValidationInvalid
	}
	if !validCompletionStrings(report.Blockers, childCompletionMaxBlockerItems, childCompletionMaxStringBytes, false) {
		return ChildCompletionValidationOversized
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
