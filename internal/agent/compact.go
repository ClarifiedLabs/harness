package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"harness/internal/hooks"
	"harness/internal/llm"
	"harness/internal/retry"
	"harness/prompts"
)

// defaultKeepTurns is how many whole turns compaction preserves verbatim; everything
// older is summarized into one message (design §12).
const defaultKeepTurns = 4

// compactThresholdPct is the fraction of the context window at which the
// post-turn trigger fires: reported input tokens ≥ 78% leaves headroom for the
// summary call plus the next turn (design §12).
const compactThresholdPct = 78

// overThreshold reports whether tokens crosses the compaction trigger for the
// current window. compactBudget is the same fraction expressed as a token
// budget; together they keep the threshold arithmetic in one place.
func (a *Agent) overThreshold(tokens int) bool {
	return tokens*100 >= a.window()*compactThresholdPct
}

func (a *Agent) compactBudget() int {
	return a.window() * compactThresholdPct / 100
}

// bytesPerToken is a coarse token estimate used only by the degradation ladder,
// which must decide whether a compacted transcript still overflows without a
// tokenizer or another model round-trip (design §12).
const bytesPerToken = 4

const (
	defaultSummaryMaxTokens      = 2048
	defaultSummaryToolResultSize = 4096
)

// CompactionArchive is handed to the optional archive callback before old
// messages are removed from the active transcript.
type CompactionArchive struct {
	Messages []llm.Message
	Summary  string
	Usage    llm.Usage
}

// CompactionArchiver preserves raw compacted messages and returns a reference
// suitable for inclusion in the active summary.
type CompactionArchiver func(context.Context, CompactionArchive) (string, error)

// summaryHeader prefixes the replacement message so the model recognizes the
// collapsed history (design §12).
const summaryHeader = "=== Summary of earlier conversation ===\n"

// MaybeCompact compacts the transcript when lastInputTokens (the input tokens
// the final step of the just-finished turn reported) is at least
// compactThresholdPct of the model's context window; otherwise it is a no-op
// (design §12, §8.1). It returns the summary call's usage (zero when no
// compaction ran), a changed flag reporting whether the transcript was actually
// rewritten, and any error. The caller folds the usage into session totals and
// uses changed to decide whether to reset its trigger state (r-churn).
func (a *Agent) MaybeCompact(ctx context.Context, lastInputTokens int, sink EventSink) (llm.Usage, bool, error) {
	if !a.overThreshold(lastInputTokens) {
		return llm.Usage{}, false, nil
	}
	return a.compactTriggered(ctx, sink, "auto")
}

// Compact collapses every turn older than the last keepTurns into a single
// model-written summary message, keeping the system prompt (it lives on
// Request.System) and the recent turns verbatim (design §12). The summary call's
// usage is returned for the session totals. On a summary-call error the
// transcript is left fully intact and the error is returned, with a warning
// reported via the sink — a visible context-length failure beats silent data
// loss. The result always satisfies the §4 invariant: kept turns are whole, so
// no tool_use/tool_result pair is ever split.
func (a *Agent) Compact(ctx context.Context, sink EventSink) (llm.Usage, error) {
	u, _, err := a.compact(ctx, sink, "manual")
	return u, err
}

// compact returns the summary-call usage, a changed flag (true only when the
// live transcript was actually rewritten), and any error. A no-op (nothing old
// enough to summarize and the transcript already within budget, or a PreCompact
// block) returns changed=false so the mid-loop caller does not churn its trigger
// state every model turn.
func (a *Agent) compact(ctx context.Context, sink EventSink, trigger string) (llm.Usage, bool, error) {
	return a.compactInternal(ctx, sink, trigger, false)
}

// compactTriggered is used when a measured request footprint or provider
// overflow says the active context is too large. In a single long tool turn
// there may be no older turn to summarize and the byte estimate can still be
// optimistic, so force the current-turn shrink path instead of no-oping.
func (a *Agent) compactTriggered(ctx context.Context, sink EventSink, trigger string) (llm.Usage, bool, error) {
	return a.compactInternal(ctx, sink, trigger, true)
}

func (a *Agent) compactInternal(ctx context.Context, sink EventSink, trigger string, forceCurrent bool) (llm.Usage, bool, error) {
	if a.hooks != nil && a.hooks.HasEvent(hooks.PreCompact) {
		res := a.hooks.Run(ctx, hooks.PreCompact, trigger, hooks.Payload{"trigger": trigger})
		for _, notice := range res.Notices {
			sink.Notice(notice)
		}
		if res.Block {
			reason := res.Reason()
			if reason == "" {
				reason = "blocked by PreCompact hook"
			}
			sink.Notice("[compact skipped: " + reason + "]")
			return llm.Usage{}, false, nil
		}
	}

	starts := turnStarts(a.transcript)
	keepTurns := a.keepTurns()
	if len(starts) <= keepTurns {
		// Nothing older than the kept turns to summarize. If the transcript still
		// fits, this is a genuine no-op. If a single ballooning turn has pushed it
		// over budget, the summarize path can't help (there is nothing older to
		// fold), so shrink the current transcript in place rather than ship an
		// oversized request to the provider (never wedge, design §12).
		if !forceCurrent && estimateTokens(a.transcript) <= a.compactBudget() {
			return llm.Usage{}, false, nil
		}
		changed, err := a.degradeCurrent(sink)
		return llm.Usage{}, changed, err
	}

	boundary := starts[len(starts)-keepTurns]
	older := a.transcript[:boundary]
	kept := a.transcript[boundary:]

	summary, usage, err := a.summarize(ctx, prompts.CompactionSummary(), older)
	if err != nil {
		sink.Notice(fmt.Sprintf("[compact failed: %v; keeping full transcript]", err))
		return llm.Usage{}, false, err
	}
	if a.archiveCompaction != nil {
		ref, err := a.archiveCompaction(ctx, CompactionArchive{
			Messages: older,
			Summary:  summary,
			Usage:    usage,
		})
		if err != nil {
			sink.Notice(fmt.Sprintf("[compact archive failed: %v; keeping full transcript]", err))
			return llm.Usage{}, false, err
		}
		if ref != "" {
			summary += "\n\nRaw compacted transcript archive: " + ref
		}
	}

	collapsed := len(older)
	before := estimateTokens(a.transcript)
	compacted := make([]llm.Message, 0, 1+len(kept))
	compacted = append(compacted, a.summaryMessage(summary))
	// Deep-copy the kept turns before the in-place degrade/trim below: they alias
	// the live transcript's Content, and a post-degrade ValidateTranscript failure
	// must leave the live transcript fully intact (the rollback guarantee).
	compacted = append(compacted, cloneMessages(kept)...)

	// Degradation ladder: shrink further while the estimate still overflows
	// (design §12). Never wedge.
	compacted = a.degrade(compacted, starts)
	// r54: when collapsing the older turns reclaimed little — the kept turns
	// dominate — trim their large read-only tool results in place rather than pay
	// for another summarization pass.
	if reclaimedTooLittle(before, compacted) {
		a.trimToolResults(compacted, sink)
	}
	if err := llm.ValidateTranscript(compacted); err != nil {
		sink.Notice(fmt.Sprintf("[compact failed: compacted transcript invalid: %v; keeping full transcript]", err))
		return llm.Usage{}, false, err
	}

	a.transcript = compacted
	a.validatedPrefix = 0 // the transcript was rewritten; re-validate from scratch (r62)
	a.resetResponseState()
	sink.Notice(compactionReport(a.registry, a.model, collapsed, usage))
	if a.hooks != nil && a.hooks.HasEvent(hooks.PostCompact) {
		res := a.hooks.Run(ctx, hooks.PostCompact, trigger, hooks.Payload{"trigger": trigger})
		for _, notice := range res.Notices {
			sink.Notice(notice)
		}
		if len(res.AdditionalContext) > 0 {
			if receiver, ok := sink.(HookContextReceiver); ok {
				receiver.AddHookContext(res.AdditionalContext)
			}
		}
		if res.Block {
			reason := res.Reason()
			if reason == "" {
				reason = "blocked by PostCompact hook"
			}
			sink.Notice("[post-compact hook blocked after compaction: " + reason + "]")
		}
	}
	return usage, true, nil
}

// degradeCurrent shrinks the live transcript in place when it is over budget but
// has nothing older than the kept turns to summarize — a single ballooning turn.
// It trims large read-only results, then hard-truncates the largest blocks until
// the estimate fits, so an oversized request never reaches the provider (never
// wedge, design §12). All mutation happens on a deep copy, so a post-shrink
// ValidateTranscript failure leaves the live transcript fully intact (the
// rollback guarantee). It returns whether the transcript was actually replaced.
func (a *Agent) degradeCurrent(sink EventSink) (bool, error) {
	before := estimateTokens(a.transcript)
	budget := a.compactBudget()
	compacted := cloneMessages(a.transcript)
	a.trimToolResults(compacted, sink)
	truncateUntilFits(compacted, budget)
	after := estimateTokens(compacted)
	if after >= before {
		// Nothing left to shrink; ship the oversized request rather than churn an
		// identical rewrite. Surface it so the wedge risk is visible, not silent.
		sink.Notice("[compact: transcript over budget but nothing left to shrink]")
		return false, nil
	}
	if err := llm.ValidateTranscript(compacted); err != nil {
		sink.Notice(fmt.Sprintf("[compact failed: shrunk transcript invalid: %v; keeping full transcript]", err))
		return false, err
	}
	a.transcript = compacted
	a.validatedPrefix = 0 // the transcript was rewritten; re-validate from scratch (r62)
	a.resetResponseState()
	sink.Notice(fmt.Sprintf("[compacted: shrank oversized turn in place · ~%s → ~%s]", kiloTokens(before), kiloTokens(after)))
	return true, nil
}

// cloneMessages returns a copy of msgs in which each message's Content slice is a
// fresh, independent slice. The in-place degrade/trim helpers only ever replace
// whole ContentBlock fields (never mutate a backing array), so a shallow copy of
// each Content slice is enough to keep mutations off the live transcript — the
// same approach prepareSummaryMessages uses for the summary input.
func cloneMessages(msgs []llm.Message) []llm.Message {
	out := make([]llm.Message, len(msgs))
	for i, m := range msgs {
		out[i] = m
		out[i].Content = make([]llm.ContentBlock, len(m.Content))
		copy(out[i].Content, m.Content)
	}
	return out
}

// GenerateSummary runs one tool-less summarization pass over the full current
// transcript using the given system instruction, returning the summary text and
// the call's usage. It backs the plan->implementation handoff (with the handoff
// brief prompt) and is independent of the compaction trigger/keep-turn logic.
func (a *Agent) GenerateSummary(ctx context.Context, system string) (string, llm.Usage, error) {
	return a.summarize(ctx, system, a.transcript)
}

// summarize runs one tool-less model call over the older messages, with the
// given system instruction, and returns the summary text and the call's usage.
func (a *Agent) summarize(ctx context.Context, system string, older []llm.Message) (string, llm.Usage, error) {
	prepared := prepareSummaryMessages(older, a.summaryToolResultMaxBytes())
	chunks := splitSummaryChunks(prepared, a.summaryChunkBudget())
	if len(chunks) <= 1 {
		return a.summarizeOne(ctx, system, prepared)
	}

	var total llm.Usage
	summaries := make([]llm.Message, 0, len(chunks))
	for i, chunk := range chunks {
		summary, usage, err := a.summarizeOne(ctx, system, chunk)
		if err != nil {
			return "", llm.Usage{}, err
		}
		total = add(total, usage)
		summaries = append(summaries, textMessageAt(a.now(), llm.RoleUser, fmt.Sprintf("Chunk %d summary:\n%s", i+1, summary)))
	}
	final, usage, err := a.summarizeOne(ctx, system, summaries)
	if err != nil {
		return "", llm.Usage{}, err
	}
	return final, add(total, usage), nil
}

func (a *Agent) summarizeOne(ctx context.Context, system string, older []llm.Message) (string, llm.Usage, error) {
	budget := a.summaryMaxTokens()
	var total llm.Usage
	for bumped := false; ; {
		text, usage, stop, err := a.streamSummary(ctx, system, older, budget)
		total = add(total, usage)
		if err != nil {
			return "", total, err
		}
		// r33: a max-tokens-truncated summary silently loses its tail; grant a
		// larger budget and retry once before accepting the truncated result.
		if stop == llm.StopMaxTokens && !bumped {
			bumped = true
			budget *= 2
			continue
		}
		return text, total, nil
	}
}

// streamSummary runs one tool-less summarization request, re-requesting from
// scratch on a retryable mid-stream failure (r32) so a transient error does not
// abort compaction near the threshold. Reasoning is disabled: a summary needs no
// thinking budget (r13, mirrors PrewarmRequest). It returns the assembled text,
// usage, and the stop reason.
func (a *Agent) streamSummary(ctx context.Context, system string, older []llm.Message, maxTokens int) (string, llm.Usage, llm.StopReason, error) {
	req := llm.Request{
		Model:     a.model,
		System:    system,
		Messages:  older,
		MaxTokens: maxTokens,
		Reasoning: llm.ReasoningConfig{},
	}
	for attempt := 0; ; attempt++ {
		text, usage, stop, err := a.collectSummary(ctx, req)
		if err == nil || attempt >= streamRetries || !retryableStreamError(err) {
			return text, usage, stop, err
		}
		if serr := a.sleep(ctx, retry.Next(attempt, streamRetryAfter(err))); serr != nil {
			return "", llm.Usage{}, stop, serr
		}
	}
}

func (a *Agent) collectSummary(ctx context.Context, req llm.Request) (string, llm.Usage, llm.StopReason, error) {
	var text []byte
	var usage llm.Usage
	var stop llm.StopReason
	for ev, err := range a.provider.Stream(ctx, req) {
		if err != nil {
			return "", llm.Usage{}, stop, err
		}
		switch ev.Kind {
		case llm.EventTextDelta:
			text = append(text, ev.Text...)
		case llm.EventUsage:
			if ev.Usage != nil {
				usage = mergeUsage(usage, *ev.Usage)
			}
		case llm.EventDone:
			if ev.Usage != nil {
				usage = mergeUsage(usage, *ev.Usage)
			}
			stop = ev.StopReason
		}
	}
	return string(text), usage, stop, nil
}

func (a *Agent) keepTurns() int {
	if a.compactKeepTurns > 0 {
		return a.compactKeepTurns
	}
	return defaultKeepTurns
}

func (a *Agent) summaryMaxTokens() int {
	if a.compactSummaryMaxTokens > 0 {
		return a.compactSummaryMaxTokens
	}
	return defaultSummaryMaxTokens
}

func (a *Agent) summaryToolResultMaxBytes() int {
	if a.compactToolResultMaxBytes < 0 {
		return 0
	}
	if a.compactToolResultMaxBytes > 0 {
		return a.compactToolResultMaxBytes
	}
	return defaultSummaryToolResultSize
}

func (a *Agent) summaryChunkBudget() int {
	budget := a.compactBudget()
	if budget <= 0 {
		return llm.DefaultContextWindow * compactThresholdPct / 100
	}
	// Use half the trigger budget so the summary instruction and provider
	// overhead have room even when estimates are optimistic.
	return max(budget/2, 1000)
}

func prepareSummaryMessages(msgs []llm.Message, maxToolResultBytes int) []llm.Message {
	if maxToolResultBytes == 0 {
		return msgs
	}
	out := make([]llm.Message, len(msgs))
	for i, m := range msgs {
		out[i] = llm.Message{Role: m.Role, Time: m.Time, Phase: m.Phase, Content: make([]llm.ContentBlock, len(m.Content))}
		copy(out[i].Content, m.Content)
		for j, b := range out[i].Content {
			switch {
			case b.Kind == llm.BlockToolResult && len(b.ResultText) > maxToolResultBytes:
				out[i].Content[j].ResultText = b.ResultText[:maxToolResultBytes] +
					fmt.Sprintf("\n[summary input truncated: showing first %d of %d bytes; raw content archived if compaction succeeds]", maxToolResultBytes, len(b.ResultText))
			case b.Kind == llm.BlockToolUse && len(b.ToolInput) > maxToolResultBytes:
				out[i].Content[j].ToolInput = summaryToolInput(b.ToolInput, maxToolResultBytes)
			case b.Kind == llm.BlockImage && len(b.ImageData) > maxToolResultBytes:
				out[i].Content[j] = llm.ContentBlock{
					Kind: llm.BlockText,
					Text: imageSummaryPlaceholder(b),
				}
			}
		}
	}
	return out
}

func summaryToolInput(raw json.RawMessage, maxBytes int) json.RawMessage {
	out, _ := shortenedToolInput(raw, maxBytes)
	return out
}

func shortenedToolInput(raw json.RawMessage, maxBytes int) (json.RawMessage, bool) {
	preview := string(raw)
	if len(preview) > maxBytes {
		preview = preview[:maxBytes]
	}
	b, err := json.Marshal(map[string]any{
		"_truncated":     "tool input shortened for compaction summary",
		"preview":        preview,
		"shown_bytes":    len(preview),
		"original_bytes": len(raw),
	})
	if err != nil {
		return json.RawMessage(`{"_truncated":"tool input omitted for compaction summary"}`), true
	}
	return b, len(b) < len(raw)
}

func imageSummaryPlaceholder(b llm.ContentBlock) string {
	var parts []string
	if b.ImageName != "" {
		parts = append(parts, "name "+b.ImageName)
	}
	if b.ImageMediaType != "" {
		parts = append(parts, "media "+b.ImageMediaType)
	}
	parts = append(parts, fmt.Sprintf("%d bytes", len(b.ImageData)))
	return "[image omitted from compaction summary: " + strings.Join(parts, ", ") + "]"
}

func splitSummaryChunks(msgs []llm.Message, budget int) [][]llm.Message {
	if len(msgs) == 0 || estimateTokens(msgs) <= budget {
		return [][]llm.Message{msgs}
	}
	starts := turnStarts(msgs)
	if len(starts) == 0 {
		return [][]llm.Message{msgs}
	}
	var chunks [][]llm.Message
	var current []llm.Message
	for i, start := range starts {
		end := len(msgs)
		if i+1 < len(starts) {
			end = starts[i+1]
		}
		turn := msgs[start:end]
		if len(current) > 0 && estimateTokens(append(append([]llm.Message(nil), current...), turn...)) > budget {
			chunks = append(chunks, current)
			current = nil
		}
		current = append(current, turn...)
	}
	if len(current) > 0 {
		chunks = append(chunks, current)
	}
	return chunks
}

// degrade applies the lower rungs of the ladder when the compacted transcript's
// estimate still exceeds budget: first drop to only the last turn, then
// hard-truncate the largest tool results in place (design §12). compacted is
// [summary, ...keptTurns]; starts indexes the pre-compaction transcript so the
// last turn's start can be located.
func (a *Agent) degrade(compacted []llm.Message, starts []int) []llm.Message {
	budget := a.compactBudget()
	if estimateTokens(compacted) <= budget {
		return compacted
	}

	// Rung 2: keep only the last turn. Deep-copy it: it aliases the live
	// transcript and rung 3 truncates it in place, which must not corrupt the live
	// transcript if validation later fails (the rollback guarantee).
	lastStart := starts[len(starts)-1]
	lastTurn := cloneMessages(a.transcript[lastStart:])
	compacted = append([]llm.Message{compacted[0]}, lastTurn...)
	if estimateTokens(compacted) <= budget {
		return compacted
	}

	// Rung 3: hard-truncate the largest tool results in place until it fits.
	truncateUntilFits(compacted, budget)
	return compacted
}

// truncateUntilFits hard-truncates the single largest shrinkable block in msgs
// repeatedly until the estimate fits budget or nothing can shrink further. Each
// pass removes the current overage from the largest block; a pass that cannot
// shrink anything stops the loop so we never wedge (design §12). It mutates msgs
// in place.
func truncateUntilFits(msgs []llm.Message, budget int) {
	for estimateTokens(msgs) > budget {
		excessBytes := (estimateTokens(msgs) - budget) * bytesPerToken
		if !truncateLargestBlock(msgs, excessBytes) {
			break
		}
	}
}

// turnStarts returns the indices in msgs where a turn begins: every user message
// that carries genuine user content (not solely tool_result blocks). A
// tool_result-only user message continues the current turn — it answers the
// preceding assistant's tool calls — so it never starts a new one. Keeping turns
// whole this way guarantees the §4 invariant survives compaction.
func turnStarts(msgs []llm.Message) []int {
	var starts []int
	for i, m := range msgs {
		if m.Role == llm.RoleUser && hasNonResult(m) {
			starts = append(starts, i)
		}
	}
	return starts
}

func hasNonResult(m llm.Message) bool {
	for _, b := range m.Content {
		if b.Kind != llm.BlockToolResult {
			return true
		}
	}
	return len(m.Content) == 0
}

func (a *Agent) summaryMessage(summary string) llm.Message {
	return a.textMessage(llm.RoleUser, summaryHeader+summary)
}

// minTruncResult is the smallest tool_result worth shrinking; below it the saving
// is not worth a truncation marker and the ladder stops to avoid spinning.
const minTruncResult = 256

// truncateLargestBlock removes at least dropBytes from the single largest
// shrinkable block, replacing its tail or payload with a marker. It returns false
// when no block is large enough to shrink usefully, so the caller stops rather
// than loops forever (never wedge, design §12).
func truncateLargestBlock(msgs []llm.Message, dropBytes int) bool {
	bi, bj, bestLen, kind := -1, -1, 0, llm.BlockText
	for i := range msgs {
		for j := range msgs[i].Content {
			b := msgs[i].Content[j]
			var size int
			switch b.Kind {
			case llm.BlockToolResult:
				size = len(b.ResultText)
			case llm.BlockToolUse:
				size = len(b.ToolInput)
			case llm.BlockImage:
				// Rank images by their token weight, not base64 byte length, so a
				// large text result is truncated before an image is dropped (r22).
				size = imageTokenEstimate * bytesPerToken
			}
			if size > bestLen {
				bi, bj, bestLen, kind = i, j, size, b.Kind
			}
		}
	}
	if bi < 0 || bestLen < minTruncResult {
		return false
	}
	orig := msgs[bi].Content[bj].ResultText
	if kind == llm.BlockToolUse {
		orig = string(msgs[bi].Content[bj].ToolInput)
	}
	if kind == llm.BlockImage {
		msgs[bi].Content[bj] = llm.ContentBlock{
			Kind: llm.BlockText,
			Text: imageSummaryPlaceholder(msgs[bi].Content[bj]),
		}
		return true
	}
	keep := len(orig) - dropBytes
	if keep < minTruncResult {
		keep = minTruncResult // floor: always leave a usable head
	}
	marker := fmt.Sprintf("\n[truncated: %d of %d bytes shown after compaction]", keep, len(orig))
	replacement := orig[:keep] + marker
	if len(replacement) >= len(orig) {
		return false // already at the floor; shrinking further is not worthwhile
	}
	if kind == llm.BlockToolUse {
		for keep >= 0 {
			input, ok := shortenedToolInput(json.RawMessage(orig), keep)
			if ok {
				msgs[bi].Content[bj].ToolInput = input
				return true
			}
			if keep == 0 {
				break
			}
			keep /= 2
		}
		return false
	} else {
		msgs[bi].Content[bj].ResultText = replacement
	}
	return true
}

// imageTokenEstimate is the flat per-image token weight used by the context
// estimate and the degradation ladder. A base64 image is hundreds of KB of data
// yet costs the model a roughly fixed ~1.6k tokens, so counting its raw bytes at
// bytesPerToken wildly overstates it and would make every transcript with one
// image look near-overflow (design §12, r22).
const imageTokenEstimate = 1600

// estimateTokens approximates the token footprint of a message list. Text is
// counted by byte size; images are counted at a flat per-image weight rather
// than their base64 byte length. Coarse by design: it only gates the
// degradation ladder and retention reclaim check (design §12).
func estimateTokens(msgs []llm.Message) int {
	bytes, images := 0, 0
	for _, m := range msgs {
		for _, b := range m.Content {
			if b.Kind == llm.BlockImage {
				images++
				bytes += len(b.ImageMediaType) + len(b.ImageDetail) + len(b.ImageName)
				continue
			}
			bytes += len(b.Text) + len(b.ResultText) + len(b.ToolInput) + len(b.ToolName)
			bytes += len(b.ReasoningID) + len(b.ReasoningEncrypted) + len(b.RedactedData) + len(b.ThinkingSignature)
		}
	}
	return bytes/bytesPerToken + images*imageTokenEstimate
}

func estimateRequest(req llm.Request, window int) ContextEstimate {
	systemBytes := len(req.System)
	toolBytes := 0
	for _, t := range req.Tools {
		toolBytes += len(t.Name) + len(t.Description) + len(t.Parameters)
	}
	messageBytes := 0
	images := 0
	for _, m := range req.Messages {
		messageBytes += len(m.Role)
		for _, b := range m.Content {
			if b.Kind == llm.BlockImage {
				images++
				messageBytes += len(b.Kind) + len(b.ImageMediaType) + len(b.ImageDetail) + len(b.ImageName)
				continue
			}
			messageBytes += len(b.Kind) + len(b.Text) + len(b.ToolUseID) + len(b.ToolName) + len(b.ToolInput) +
				len(b.ResultForID) + len(b.ResultText)
			messageBytes += len(b.ReasoningID) + len(b.ReasoningEncrypted) + len(b.RedactedData) + len(b.ThinkingSignature)
		}
	}
	messageBytes += len(llm.RequestContextText(req.RequestContext))
	est := ContextEstimate{
		System:   systemBytes / bytesPerToken,
		Tools:    toolBytes / bytesPerToken,
		Messages: messageBytes/bytesPerToken + images*imageTokenEstimate,
		Window:   window,
	}
	est.Total = est.System + est.Tools + est.Messages
	est.PayloadSystem = est.System
	est.PayloadTools = est.Tools
	est.PayloadMessages = est.Messages
	est.PayloadTotal = est.Total
	return est
}

// compactionReport is the exact post-compaction notice (design §12):
//
//	[compacted: 38 messages → summary · 9.1k in / 0.4k out · $0.05]
//
// The cost segment is omitted for models with no price entry.
func compactionReport(registry *llm.Registry, model string, collapsed int, u llm.Usage) string {
	var b strings.Builder
	fmt.Fprintf(&b, "[compacted: %d messages → summary · %s in / %s out",
		collapsed, kiloTokens(u.InputTokens), kiloTokens(u.OutputTokens))
	if registry != nil {
		if usd, known := registry.Cost(model, u); known {
			fmt.Fprintf(&b, " · $%.2f", usd)
		}
	}
	b.WriteString("]")
	return b.String()
}

// kiloTokens renders a token count in thousands with one decimal, matching the
// design's compaction report (9100 -> "9.1k", 400 -> "0.4k").
func kiloTokens(n int) string {
	return fmt.Sprintf("%.1fk", float64(n)/1000)
}
