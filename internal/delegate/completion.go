package delegate

import (
	"encoding/json"
	"fmt"
	"strings"
	"unicode/utf8"

	"harness/internal/session"
)

// CompletionReport is the semantic outcome of a child run. It is deliberately
// independent of agent loop termination and child lifecycle status.
type CompletionReport = session.ChildCompletionReport

const (
	completionFence           = "```harness-completion"
	maxCompletionBlockBytes   = 32 << 10
	maxCompletionBlockerItems = 32
	maxCompletionStringBytes  = 1024
)

type declaredCompletionReport struct {
	Outcome  string   `json:"outcome"`
	Blockers []string `json:"blockers,omitempty"`
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

// parseCompletionReport inspects only one final tagged fenced block. A usable
// status footer is stripped from parent-facing Markdown; an absent or unusable
// footer preserves the complete response because the footer is optional.
func parseCompletionReport(text, contract string) (CompletionReport, string) {
	if !strings.Contains(text, completionFence) {
		return unknownCompletion(contract, session.ChildCompletionSourceCompatibility, session.ChildCompletionValidationMissing), text
	}
	if strings.Count(text, completionFence) != 1 {
		return unknownCompletion(contract, session.ChildCompletionSourceCompatibility, session.ChildCompletionValidationDuplicate), text
	}
	start := strings.Index(text, completionFence)
	if start > 0 && text[start-1] != '\n' {
		return unknownCompletion(contract, session.ChildCompletionSourceCompatibility, session.ChildCompletionValidationMalformed), text
	}
	bodyStart := start + len(completionFence)
	switch {
	case strings.HasPrefix(text[bodyStart:], "\r\n"):
		bodyStart += 2
	case strings.HasPrefix(text[bodyStart:], "\n"):
		bodyStart++
	default:
		return unknownCompletion(contract, session.ChildCompletionSourceCompatibility, session.ChildCompletionValidationMalformed), text
	}
	endRel := strings.Index(text[bodyStart:], "\n```")
	if endRel < 0 {
		return unknownCompletion(contract, session.ChildCompletionSourceCompatibility, session.ChildCompletionValidationMalformed), text
	}
	end := bodyStart + endRel
	blockEnd := end + len("\n```")
	if strings.TrimSpace(text[blockEnd:]) != "" {
		return unknownCompletion(contract, session.ChildCompletionSourceCompatibility, session.ChildCompletionValidationMalformed), text
	}
	body := []byte(text[bodyStart:end])
	if len(body) > maxCompletionBlockBytes {
		return unknownCompletion(contract, session.ChildCompletionSourceCompatibility, session.ChildCompletionValidationOversized), text
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(body, &fields); err != nil {
		return unknownCompletion(contract, session.ChildCompletionSourceCompatibility, session.ChildCompletionValidationMalformed), text
	}
	for field := range fields {
		if field != "outcome" && field != "blockers" {
			return unknownCompletion(contract, session.ChildCompletionSourceCompatibility, session.ChildCompletionValidationMalformed), text
		}
	}
	var declared declaredCompletionReport
	if err := json.Unmarshal(body, &declared); err != nil {
		return unknownCompletion(contract, session.ChildCompletionSourceCompatibility, session.ChildCompletionValidationMalformed), text
	}
	status := validateDeclaredCompletion(declared)
	if status != session.ChildCompletionValidationValid {
		return unknownCompletion(contract, session.ChildCompletionSourceCompatibility, status), text
	}
	report := CompletionReport{
		Outcome:          declared.Outcome,
		Blockers:         append([]string(nil), declared.Blockers...),
		Contract:         contract,
		Source:           session.ChildCompletionSourceDeclared,
		ValidationStatus: session.ChildCompletionValidationValid,
	}
	prose := strings.TrimSpace(text[:start])
	return report, prose
}

func validateDeclaredCompletion(report declaredCompletionReport) string {
	switch report.Outcome {
	case session.ChildCompletionOutcomeComplete, session.ChildCompletionOutcomeBlocked:
	default:
		return session.ChildCompletionValidationInvalid
	}
	if report.Outcome == session.ChildCompletionOutcomeBlocked && len(report.Blockers) == 0 {
		return session.ChildCompletionValidationInvalid
	}
	if !boundedStrings(report.Blockers, maxCompletionBlockerItems, maxCompletionStringBytes, false) {
		return session.ChildCompletionValidationOversized
	}
	return session.ChildCompletionValidationValid
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

func completionSystemPrompt(system string) string {
	instruction := `Write your final response as a useful Markdown report. Put substantive details such as findings, changed files, verification, evidence, unreviewed scope, and remaining work in that report rather than duplicating them into metadata.

If you know the final task outcome, you may optionally end with exactly one fenced JSON footer after the Markdown:
` + completionFence + `
{"outcome":"complete"}
` + "```" + `

or:
` + completionFence + `
{"outcome":"blocked","blockers":["what prevents completion"]}
` + "```" + `

The footer accepts only complete or blocked. Use complete only when all delegated work is done. Blocked requires at least one short blocker. Omit the footer rather than guessing. Do not emit it before the final response.`
	return strings.TrimSpace(system) + "\n\n[delegate completion status]\n" + instruction
}

func completionReceipt(report CompletionReport) string {
	if report.ValidationStatus == session.ChildCompletionValidationMissing {
		return "[delegate completion: unreported]"
	}
	line := fmt.Sprintf("[delegate completion: %s", report.Outcome)
	if report.Outcome == session.ChildCompletionOutcomeUnknown {
		line += fmt.Sprintf(", report %s/%s", report.Source, report.ValidationStatus)
	}
	return line + "]"
}
