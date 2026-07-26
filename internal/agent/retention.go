package agent

import (
	"fmt"
	"strings"

	"harness/internal/llm"
	"harness/internal/toolresult"
)

// retentionImageKeepTurns is how many recent turns keep their images verbatim;
// an image older than this is replaced with a text placeholder before the next
// request (design §12, r20). Images cost a roughly fixed token price, but a
// stale screenshot is rarely needed again and re-sending its base64 every turn
// is pure waste.
const retentionImageKeepTurns = 2

const (
	// Retention should pre-empt the 78% compaction trigger with enough room for
	// another large tool result. Provider-backed calibration found that later
	// 65% and 70% epochs could still cross into compaction on the next result;
	// 60% avoided that collision and materially reduced uncached input versus
	// age-based retention.
	retentionPressureHighPct = 60
	retentionPressureLowPct  = 50
)

// retentionTrimMarker is the idempotency sentinel left in a tool result the
// retention pass has already shrunk, so repeated passes never re-trim it.
const retentionTrimMarker = "[older tool output trimmed"

// applyRetention shrinks the live transcript in place before a model request so
// large stale tool outputs and aged images are not re-sent verbatim every turn
// (design §12, r9+r20). It is a pure local edit — no model round-trip — and only
// ever shortens text or swaps an image for a text placeholder, so the §4
// transcript invariant is preserved. The pass is idempotent: already-trimmed or
// already-archived blocks are skipped.
func (a *Agent) applyRetention(sink EventSink) bool {
	return a.applyRetentionPolicy(sink, a.estimateContext(nil).Total).changed
}

type retentionPass struct {
	event    RetentionEvent
	observed bool
	changed  bool
}

// applyRetentionPolicy selects pressure-triggered epochs for the default auto
// policy, regardless of provider continuation support. Pressure epochs use
// hysteresis: after one epoch, retention does not run again until the full
// context falls below the low-water mark. Compaction remains the safety net when
// an epoch cannot reclaim enough. Explicit age mode preserves the legacy path
// for experiments and compatibility.
func (a *Agent) applyRetentionPolicy(sink EventSink, contextTokens int) retentionPass {
	policy := a.retentionPolicy
	if policy == RetentionPolicyDisabled {
		return retentionPass{}
	}
	if policy == RetentionPolicyAuto {
		policy = RetentionPolicyPressure
	}
	if len(a.transcript) == 0 {
		return retentionPass{}
	}
	starts := turnStarts(a.transcript)
	resultBoundary := keepBoundary(starts, a.keepTurns())
	imageBoundary := keepBoundary(starts, retentionImageKeepTurns)
	if resultBoundary == 0 && imageBoundary == 0 {
		return retentionPass{} // nothing old enough to shrink
	}

	event := RetentionEvent{
		Policy:              "age",
		Trigger:             "turn_age",
		ContextTokensBefore: contextTokens,
	}
	observed := false
	if policy == RetentionPolicyPressure {
		event.Policy = "pressure_epoch"
		event.Trigger = "context_pressure"
		window := a.window()
		if window <= 0 {
			return retentionPass{}
		}
		if contextTokens*100 <= window*retentionPressureLowPct {
			a.retentionEpochArmed = true
		}
		if !a.retentionEpochArmed || contextTokens*100 < window*retentionPressureHighPct {
			return retentionPass{}
		}
		a.retentionEpochArmed = false
		observed = true
	}
	event.BytesBefore = retentionTranscriptBytes(a.transcript)
	readOnly := a.readOnlyResultIDsIn(a.transcript)
	for i := range a.transcript {
		for j := range a.transcript[i].Content {
			b := &a.transcript[i].Content[j]
			blockChanged := false
			switch b.Kind {
			case llm.BlockToolResult:
				// Only read-only results are re-derivable on demand, so only they
				// are safe to drop the body of.
				if i < resultBoundary && readOnly[b.ResultForID] {
					blockChanged = a.trimToolResultBlock(b, sink) || blockChanged
				}
				if i < imageBoundary {
					blockChanged = degradeToolResultImages(b, func(llm.ContentBlock) bool { return true }) || blockChanged
				}
			case llm.BlockImage:
				if i < imageBoundary {
					*b = llm.ContentBlock{Kind: llm.BlockText, Text: imageSummaryPlaceholder(*b)}
					blockChanged = true
				}
			}
			if blockChanged {
				event.BlocksTrimmed++
			}
		}
	}
	event.BytesAfter = retentionTranscriptBytes(a.transcript)
	event.ContextTokensAfter = a.estimateContext(nil).Total
	changed := event.BlocksTrimmed > 0
	if changed {
		observed = true
	}
	return retentionPass{event: event, observed: observed, changed: changed}
}

func (a *Agent) managedContinuationActive() bool {
	return a.managedContinuationStateful && !a.disableManagedContinuation
}

func normalizeRetentionPolicy(policy RetentionPolicy) RetentionPolicy {
	switch policy {
	case "", RetentionPolicyAuto:
		return RetentionPolicyAuto
	case RetentionPolicyAge, RetentionPolicyPressure, RetentionPolicyDisabled:
		return policy
	default:
		return RetentionPolicyAuto
	}
}

func retentionTranscriptBytes(messages []llm.Message) int {
	total := 0
	for _, message := range messages {
		total += len(message.Role) + len(message.Phase)
		for _, block := range message.Content {
			total += retentionContentBlockBytes(block)
		}
	}
	return total
}

func retentionContentBlockBytes(block llm.ContentBlock) int {
	total := len(block.Text) + len(block.ResultText) + len(block.ToolInput) + len(block.ToolName)
	total += len(block.ImageData) + len(block.ImageMediaType) + len(block.ImageDetail) + len(block.ImageName)
	total += len(block.ReasoningID) + len(block.ReasoningEncrypted) + len(block.RedactedData) + len(block.ThinkingSignature)
	total += len(block.InteractionThoughtSummary) + len(block.InteractionThoughtSignature) + len(block.InteractionStep)
	for _, child := range block.ResultContent {
		total += retentionContentBlockBytes(child)
	}
	return total
}

// trimToolResults shrinks every large read-only tool result in msgs in place.
// It backs both the live retention pass over kept turns (r54) and is reused by
// compaction when summarizing reclaimed too little.
func (a *Agent) trimToolResults(msgs []llm.Message, sink EventSink) {
	readOnly := a.readOnlyResultIDsIn(msgs)
	for i := range msgs {
		for j := range msgs[i].Content {
			b := &msgs[i].Content[j]
			if b.Kind == llm.BlockToolResult && readOnly[b.ResultForID] {
				a.trimToolResultBlock(b, sink)
			}
		}
	}
}

// trimToolResultBlock replaces a large tool_result body with its head plus a
// recovery hint, archiving the full body through the sink when it supports it so
// the model can fetch the rest. It returns whether it changed the block. Small
// or already-trimmed/archived results are left untouched.
func (a *Agent) trimToolResultBlock(b *llm.ContentBlock, sink EventSink) bool {
	if b.Kind != llm.BlockToolResult || len(b.ResultText) <= defaultSummaryToolResultSize || retentionTrimmed(b.ResultText) {
		return false
	}
	full := b.ResultText
	head := full[:defaultSummaryToolResultSize]
	hint := genericRetentionHint(len(head), len(full))
	if archiver, ok := sink.(ToolResultArchiver); ok {
		if archive, err := archiver.ArchiveToolResult(llm.ToolResult{ForID: b.ResultForID, Text: full}); err == nil && archive.ModelPath != "" {
			hint = toolresult.ArchivedHint(archive.ModelPath)
		}
	}
	b.ResultText = head + "\n" + hint
	return true
}

// retentionTrimmed reports whether a tool-result body has already been shrunk by
// the retention pass or carries an archive reference, making it ineligible for
// further trimming.
func retentionTrimmed(text string) bool {
	return strings.Contains(text, retentionTrimMarker) || strings.Contains(text, toolresult.ArchivedHintMarker)
}

func genericRetentionHint(shown, total int) string {
	return fmt.Sprintf("%s to %d of %d bytes to save context; re-run the tool if you need the rest]", retentionTrimMarker, shown, total)
}

// reclaimReclaimPct is the floor below which a compaction is judged to have
// reclaimed too little, triggering the kept-turn tool-result trim (r54).
const retentionReclaimPct = 15

// reclaimedTooLittle reports whether compaction (summary + degrade) shrank the
// transcript by less than retentionReclaimPct of its pre-compaction size.
func reclaimedTooLittle(before int, compacted []llm.Message) bool {
	if before <= 0 {
		return false
	}
	after := estimateTokens(compacted)
	return (before-after)*100 < before*retentionReclaimPct
}

// keepBoundary returns the transcript index before which messages are older than
// the last keep turns. It returns 0 when there are not more than keep turns, so
// nothing qualifies as old.
func keepBoundary(starts []int, keep int) int {
	if keep <= 0 || len(starts) <= keep {
		return 0
	}
	return starts[len(starts)-keep]
}

// readOnlyResultIDsIn maps each tool_result id in msgs whose originating tool_use
// resolves to a read-only invocation. Used to confine body-dropping retention to
// outputs the model can regenerate on demand.
func (a *Agent) readOnlyResultIDsIn(msgs []llm.Message) map[string]bool {
	ids := map[string]bool{}
	for _, m := range msgs {
		if m.Role != llm.RoleAssistant {
			continue
		}
		for _, b := range m.Content {
			if b.Kind == llm.BlockToolUse && a.tools.CallReadOnly(llm.ToolCall{Name: b.ToolName, Input: b.ToolInput}) {
				ids[b.ToolUseID] = true
			}
		}
	}
	return ids
}
