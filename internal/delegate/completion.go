package delegate

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"unicode/utf8"

	"harness/internal/session"
)

// CompletionReport is the semantic outcome of a child run. It is deliberately
// independent of agent loop termination and child lifecycle status.
type CompletionReport = session.ChildCompletionReport

type completionVerification = session.ChildCompletionVerification
type completionEvidence = session.ChildCompletionEvidence

const (
	completionFence              = "```harness-completion"
	maxCompletionBlockBytes      = 32 << 10
	maxCompletionListItems       = 128
	maxCompletionStringBytes     = 1024
	maxCompletionDetailBytes     = 2048
	maxCompletionEvidenceItems   = 128
	maxCompletionBlockerItems    = 32
	maxCompletionVerification    = 32
	maxCompletionQuestionItems   = 64
	maxCompletionUnreviewedItems = 128
)

type declaredCompletionReport struct {
	Outcome                string                    `json:"outcome"`
	UnresolvedRequirements *int                      `json:"unresolved_requirements"`
	Blockers               []string                  `json:"blockers,omitempty"`
	ChangedFiles           *[]string                 `json:"changed_files,omitempty"`
	Verification           *[]completionVerification `json:"verification,omitempty"`
	Coverage               *string                   `json:"coverage,omitempty"`
	UnreviewedScope        *[]string                 `json:"unreviewed_scope,omitempty"`
	Evidence               *[]completionEvidence     `json:"evidence,omitempty"`
	UnresolvedQuestions    *[]string                 `json:"unresolved_questions,omitempty"`
}

func completionContract(mode, agentName string) string {
	switch {
	case mode == ModeImplementation:
		return session.ChildCompletionContractImplementation
	case strings.EqualFold(strings.TrimSpace(agentName), "review"):
		return session.ChildCompletionContractReview
	default:
		return session.ChildCompletionContractGeneral
	}
}

func unknownCompletion(contract, source, validation string) CompletionReport {
	return CompletionReport{
		Outcome:          session.ChildCompletionOutcomeUnknown,
		Contract:         contract,
		Source:           source,
		ValidationStatus: validation,
	}
}

// parseCompletionReport inspects only one final tagged fenced block. Invalid or
// absent blocks never fail a useful child run and never remove child prose.
func parseCompletionReport(text, contract string) (CompletionReport, string) {
	if strings.Count(text, completionFence) == 0 {
		return unknownCompletion(contract, session.ChildCompletionSourceCompatibility, session.ChildCompletionValidationMissing), text
	}
	if strings.Count(text, completionFence) != 1 {
		return unknownCompletion(contract, session.ChildCompletionSourceCompatibility, session.ChildCompletionValidationDuplicate), text
	}

	start := strings.Index(text, completionFence)
	if start > 0 && text[start-1] != '\n' {
		return unknownCompletion(contract, session.ChildCompletionSourceCompatibility, session.ChildCompletionValidationMalformed), text
	}
	rest := text[start+len(completionFence):]
	var bodyStart int
	switch {
	case strings.HasPrefix(rest, "\n"):
		bodyStart = 1
	case strings.HasPrefix(rest, "\r\n"):
		bodyStart = 2
	default:
		return unknownCompletion(contract, session.ChildCompletionSourceCompatibility, session.ChildCompletionValidationMalformed), text
	}
	rest = rest[bodyStart:]
	closeAt := strings.LastIndex(rest, "\n```")
	if closeAt < 0 || strings.TrimSpace(rest[closeAt+len("\n```"):]) != "" {
		return unknownCompletion(contract, session.ChildCompletionSourceCompatibility, session.ChildCompletionValidationMalformed), text
	}
	body := rest[:closeAt]
	if len(body) > maxCompletionBlockBytes {
		return unknownCompletion(contract, session.ChildCompletionSourceCompatibility, session.ChildCompletionValidationOversized), text
	}

	var declared declaredCompletionReport
	decoder := json.NewDecoder(strings.NewReader(body))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&declared); err != nil {
		return unknownCompletion(contract, session.ChildCompletionSourceCompatibility, session.ChildCompletionValidationMalformed), text
	}
	if err := requireJSONEOF(decoder); err != nil {
		return unknownCompletion(contract, session.ChildCompletionSourceCompatibility, session.ChildCompletionValidationMalformed), text
	}
	status := validateDeclaredCompletion(declared, contract)
	if status == session.ChildCompletionValidationInvalid || status == session.ChildCompletionValidationOversized {
		return unknownCompletion(contract, session.ChildCompletionSourceCompatibility, status), text
	}

	partial := status == session.ChildCompletionValidationPartialFields
	report := CompletionReport{
		Outcome:                declared.Outcome,
		UnresolvedRequirements: *declared.UnresolvedRequirements,
		Blockers:               declared.Blockers,
		Contract:               contract,
		Source:                 session.ChildCompletionSourceDeclared,
		ValidationStatus:       session.ChildCompletionValidationValid,
	}
	if partial {
		// Preserve the declared outcome and unresolved count; fill the omitted
		// contract fields with empty defaults so downstream consumers see a
		// complete-shaped report.
		report.ValidationStatus = session.ChildCompletionValidationPartialFields
		if declared.ChangedFiles == nil && contract == session.ChildCompletionContractImplementation {
			report.ChangedFiles = []string{}
		}
		if declared.Verification == nil && contract == session.ChildCompletionContractImplementation {
			report.Verification = []completionVerification{}
		}
		if declared.Coverage == nil && contract == session.ChildCompletionContractReview {
			report.Coverage = ""
		}
		if declared.UnreviewedScope == nil && contract == session.ChildCompletionContractReview {
			report.UnreviewedScope = []string{}
		}
		if declared.Evidence == nil && contract == session.ChildCompletionContractGeneral {
			report.Evidence = []completionEvidence{}
		}
		if declared.UnresolvedQuestions == nil && contract == session.ChildCompletionContractGeneral {
			report.UnresolvedQuestions = []string{}
		}
	}
	if declared.ChangedFiles != nil {
		report.ChangedFiles = *declared.ChangedFiles
	}
	if declared.Verification != nil {
		report.Verification = *declared.Verification
	}
	if declared.Coverage != nil {
		report.Coverage = *declared.Coverage
	}
	if declared.UnreviewedScope != nil {
		report.UnreviewedScope = *declared.UnreviewedScope
	}
	if declared.Evidence != nil {
		report.Evidence = *declared.Evidence
	}
	if declared.UnresolvedQuestions != nil {
		report.UnresolvedQuestions = *declared.UnresolvedQuestions
	}
	return report, strings.TrimSpace(text[:start])
}

func requireJSONEOF(decoder *json.Decoder) error {
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		if err == nil {
			return fmt.Errorf("multiple JSON values")
		}
		return err
	}
	return nil
}

func validateDeclaredCompletion(report declaredCompletionReport, contract string) string {
	switch report.Outcome {
	case session.ChildCompletionOutcomeComplete, session.ChildCompletionOutcomePartial,
		session.ChildCompletionOutcomeBlocked, session.ChildCompletionOutcomeFailed:
	default:
		return session.ChildCompletionValidationInvalid
	}
	if report.UnresolvedRequirements == nil || *report.UnresolvedRequirements < 0 ||
		*report.UnresolvedRequirements > session.ChildCompletionMaxUnresolvedRequirements ||
		report.Outcome == session.ChildCompletionOutcomeComplete && *report.UnresolvedRequirements != 0 {
		return session.ChildCompletionValidationInvalid
	}
	if report.Outcome == session.ChildCompletionOutcomeBlocked && len(report.Blockers) == 0 {
		return session.ChildCompletionValidationInvalid
	}
	if !boundedStrings(report.Blockers, maxCompletionBlockerItems, maxCompletionStringBytes, false) {
		return session.ChildCompletionValidationOversized
	}

	// partialFields reports whether the declared block omitted the fields this
	// contract requires. A generic core-only block is still a usable report —
	// the host fills empty defaults — so it degrades to partial_fields rather
	// than being discarded as invalid.
	var partialFields bool
	foreign := func() bool {
		return report.ChangedFiles != nil || report.Verification != nil || report.Coverage != nil ||
			report.UnreviewedScope != nil || report.Evidence != nil || report.UnresolvedQuestions != nil
	}
	switch contract {
	case session.ChildCompletionContractImplementation:
		partialFields = report.ChangedFiles == nil || report.Verification == nil || len(*report.Verification) == 0
		if !partialFields && (report.Coverage != nil || report.UnreviewedScope != nil || report.Evidence != nil || report.UnresolvedQuestions != nil) {
			return session.ChildCompletionValidationInvalid
		}
		if partialFields && foreign() {
			// Some contract fields are present but a required one is missing;
			// only a fully generic core counts as a degradable partial report.
			return session.ChildCompletionValidationInvalid
		}
		if report.ChangedFiles != nil && !boundedStrings(*report.ChangedFiles, maxCompletionListItems, maxCompletionStringBytes, false) ||
			report.Verification != nil && len(*report.Verification) > maxCompletionVerification {
			return session.ChildCompletionValidationOversized
		}
		for _, verification := range report.verificationOrDefault() {
			if !boundedString(verification.Check, maxCompletionStringBytes, false) || !boundedString(verification.Detail, maxCompletionDetailBytes, true) {
				return session.ChildCompletionValidationOversized
			}
			switch verification.Status {
			case "passed":
			case "not_run":
				if strings.TrimSpace(verification.Detail) == "" {
					return session.ChildCompletionValidationInvalid
				}
			case "failed":
				if report.Outcome == session.ChildCompletionOutcomeComplete {
					return session.ChildCompletionValidationInvalid
				}
			default:
				return session.ChildCompletionValidationInvalid
			}
		}
	case session.ChildCompletionContractReview:
		partialFields = report.Coverage == nil || report.UnreviewedScope == nil
		if !partialFields && (report.ChangedFiles != nil || report.Verification != nil || report.Evidence != nil || report.UnresolvedQuestions != nil) {
			return session.ChildCompletionValidationInvalid
		}
		if partialFields && foreign() {
			return session.ChildCompletionValidationInvalid
		}
		for _, coverage := range report.coverageOrDefault() {
			if coverage != "complete" && coverage != "partial" {
				return session.ChildCompletionValidationInvalid
			}
			if report.Outcome == session.ChildCompletionOutcomeComplete && coverage == "complete" && len(report.unreviewedOrDefault()) != 0 {
				return session.ChildCompletionValidationInvalid
			}
		}
		if report.UnreviewedScope != nil && !boundedStrings(*report.UnreviewedScope, maxCompletionUnreviewedItems, maxCompletionStringBytes, false) {
			return session.ChildCompletionValidationOversized
		}
	case session.ChildCompletionContractGeneral:
		partialFields = report.Evidence == nil || report.UnresolvedQuestions == nil
		if !partialFields && (report.ChangedFiles != nil || report.Verification != nil || report.Coverage != nil || report.UnreviewedScope != nil) {
			return session.ChildCompletionValidationInvalid
		}
		if partialFields && foreign() {
			return session.ChildCompletionValidationInvalid
		}
		for _, evidence := range report.evidenceOrDefault() {
			if !boundedString(evidence.Path, maxCompletionStringBytes, false) || !boundedString(evidence.Symbol, maxCompletionStringBytes, true) {
				return session.ChildCompletionValidationOversized
			}
		}
		if report.UnresolvedQuestions != nil && len(*report.UnresolvedQuestions) > maxCompletionQuestionItems {
			return session.ChildCompletionValidationOversized
		}
		if report.UnresolvedQuestions != nil && !boundedStrings(*report.UnresolvedQuestions, maxCompletionQuestionItems, maxCompletionStringBytes, false) {
			return session.ChildCompletionValidationOversized
		}
	default:
		return session.ChildCompletionValidationInvalid
	}
	if partialFields {
		return session.ChildCompletionValidationPartialFields
	}
	return ""
}

// verificationOrDefault exposes verification as a possibly-empty slice so the
// shared validation loop handles both present and defaulted reports.
func (r declaredCompletionReport) verificationOrDefault() []completionVerification {
	if r.Verification == nil {
		return nil
	}
	return *r.Verification
}

func (r declaredCompletionReport) coverageOrDefault() []string {
	if r.Coverage == nil {
		return nil
	}
	return []string{*r.Coverage}
}

func (r declaredCompletionReport) unreviewedOrDefault() []string {
	if r.UnreviewedScope == nil {
		return nil
	}
	return *r.UnreviewedScope
}

func (r declaredCompletionReport) evidenceOrDefault() []completionEvidence {
	if r.Evidence == nil {
		return nil
	}
	return *r.Evidence
}

func boundedStrings(values []string, maxItems, maxBytes int, allowEmpty bool) bool {
	if len(values) > maxItems {
		return false
	}
	for _, value := range values {
		if !boundedString(value, maxBytes, allowEmpty) {
			return false
		}
	}
	return true
}

func boundedString(value string, maxBytes int, allowEmpty bool) bool {
	return utf8.ValidString(value) && len(value) <= maxBytes && (allowEmpty || strings.TrimSpace(value) != "")
}

func completionContractSystemPrompt(system, contract string) string {
	// The fenced example shows every field the contract requires. Children
	// copy the example shape, so a generic core-only example produced reports
	// missing the contract fields and they were discarded as invalid.
	var example string
	switch contract {
	case session.ChildCompletionContractReview:
		example = `{"outcome":"partial","unresolved_requirements":1,"blockers":[],"coverage":"partial","unreviewed_scope":["..."]}`
	case session.ChildCompletionContractImplementation:
		example = `{"outcome":"partial","unresolved_requirements":1,"blockers":[],"changed_files":["..."],"verification":[{"check":"...","status":"passed","detail":"..."}]}`
	default:
		example = `{"outcome":"partial","unresolved_requirements":1,"blockers":[],"evidence":[{"path":"...","symbol":"..."}],"unresolved_questions":["..."]}`
	}
	common := "Your final response must end with exactly one block in this form:\n```harness-completion\n" + example + "\n```\nDo not emit that tagged block before the final response. Outcome must be complete, partial, blocked, or failed; unresolved_requirements is a required non-negative integer. Blockers is optional except that blocked requires at least one short blocker. A complete outcome requires zero unresolved requirements. Host fields contract, source, and validation_status must not be included."
	var instruction string
	switch contract {
	case session.ChildCompletionContractReview:
		instruction = "Review contract: include coverage (complete or partial) and unreviewed_scope (an explicit JSON array). A complete review requires coverage complete and an empty unreviewed_scope."
	case session.ChildCompletionContractImplementation:
		instruction = "Implementation contract: include changed_files as an explicit JSON array and verification as a non-empty array of {\"check\":\"...\",\"status\":\"passed|failed|not_run\",\"detail\":\"...\"}. A not_run result requires explanatory detail; a complete outcome cannot include failed verification."
	default:
		instruction = "Exploration/planning/general contract: include evidence as [{\"path\":\"...\",\"symbol\":\"...\"}] and unresolved_questions as an explicit JSON array."
	}
	return strings.TrimSpace(system) + "\n\n[delegate completion contract: " + contract + "]\n" + common + "\n" + instruction
}

func completionReceipt(report CompletionReport) string {
	line := fmt.Sprintf("[delegate completion: %s, unresolved %d, contract %s, report %s/%s]",
		report.Outcome, report.UnresolvedRequirements, report.Contract, report.Source, report.ValidationStatus)
	if report.Coverage != "" {
		line = strings.TrimSuffix(line, "]") + ", coverage " + report.Coverage + "]"
	}
	return line
}
