package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"harness/internal/hooks"
	"harness/internal/llm"
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
// compaction ran) so the caller can fold it into the session totals.
func (a *Agent) MaybeCompact(ctx context.Context, lastInputTokens int, sink EventSink) (llm.Usage, error) {
	if !a.overThreshold(lastInputTokens) {
		return llm.Usage{}, nil
	}
	return a.compact(ctx, sink, "auto")
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
	return a.compact(ctx, sink, "manual")
}

func (a *Agent) compact(ctx context.Context, sink EventSink, trigger string) (llm.Usage, error) {
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
			return llm.Usage{}, nil
		}
	}

	starts := turnStarts(a.transcript)
	keepTurns := a.keepTurns()
	if len(starts) <= keepTurns {
		// Nothing older than the kept turns to summarize.
		return llm.Usage{}, nil
	}

	boundary := starts[len(starts)-keepTurns]
	older := a.transcript[:boundary]
	kept := a.transcript[boundary:]

	summary, usage, err := a.summarize(ctx, prompts.CompactionSummary(), older)
	if err != nil {
		sink.Notice(fmt.Sprintf("[compact failed: %v; keeping full transcript]", err))
		return llm.Usage{}, err
	}
	if a.archiveCompaction != nil {
		ref, err := a.archiveCompaction(ctx, CompactionArchive{
			Messages: older,
			Summary:  summary,
			Usage:    usage,
		})
		if err != nil {
			sink.Notice(fmt.Sprintf("[compact archive failed: %v; keeping full transcript]", err))
			return llm.Usage{}, err
		}
		if ref != "" {
			summary += "\n\nRaw compacted transcript archive: " + ref
		}
	}

	collapsed := len(older)
	compacted := make([]llm.Message, 0, 1+len(kept))
	compacted = append(compacted, a.summaryMessage(summary))
	compacted = append(compacted, kept...)

	// Degradation ladder: shrink further while the estimate still overflows
	// (design §12). Never wedge.
	compacted = a.degrade(compacted, starts)
	if err := llm.ValidateTranscript(compacted); err != nil {
		sink.Notice(fmt.Sprintf("[compact failed: compacted transcript invalid: %v; keeping full transcript]", err))
		return llm.Usage{}, err
	}

	a.transcript = compacted
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
	return usage, nil
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
	req := llm.Request{
		Model:     a.model,
		System:    system,
		Messages:  older,
		MaxTokens: a.summaryMaxTokens(),
		Reasoning: a.reasoning,
	}
	var text []byte
	var usage llm.Usage
	for ev, err := range a.provider.Stream(ctx, req) {
		if err != nil {
			return "", llm.Usage{}, err
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
		}
	}
	return string(text), usage, nil
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

	// Rung 2: keep only the last turn.
	lastStart := starts[len(starts)-1]
	lastTurn := a.transcript[lastStart:]
	compacted = append([]llm.Message{compacted[0]}, lastTurn...)
	if estimateTokens(compacted) <= budget {
		return compacted
	}

	// Rung 3: hard-truncate the largest tool results in place until it fits.
	// Each pass removes the current overage from the single largest result; a
	// pass that cannot shrink anything further stops the loop so we never wedge.
	for estimateTokens(compacted) > budget {
		excessBytes := (estimateTokens(compacted) - budget) * bytesPerToken
		if !truncateLargestBlock(compacted, excessBytes) {
			break
		}
	}
	return compacted
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
				size = len(b.ImageData)
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

// estimateTokens approximates the token footprint of a message list by its byte
// size. Coarse by design: it only gates the degradation ladder (design §12).
func estimateTokens(msgs []llm.Message) int {
	bytes := 0
	for _, m := range msgs {
		for _, b := range m.Content {
			bytes += len(b.Text) + len(b.ResultText) + len(b.ToolInput) + len(b.ToolName)
			bytes += len(b.ImageData) + len(b.ImageMediaType) + len(b.ImageDetail) + len(b.ImageName)
		}
	}
	return bytes / bytesPerToken
}

func estimateRequest(req llm.Request, window int) ContextEstimate {
	systemBytes := len(req.System)
	toolBytes := 0
	for _, t := range req.Tools {
		toolBytes += len(t.Name) + len(t.Description) + len(t.Parameters)
	}
	messageBytes := 0
	for _, m := range req.Messages {
		messageBytes += len(m.Role)
		for _, b := range m.Content {
			messageBytes += len(b.Kind) + len(b.Text) + len(b.ToolUseID) + len(b.ToolName) + len(b.ToolInput) +
				len(b.ResultForID) + len(b.ResultText) + len(b.ImageMediaType) + len(b.ImageData) +
				len(b.ImageDetail) + len(b.ImageName)
		}
	}
	messageBytes += len(llm.RequestContextText(req.RequestContext))
	est := ContextEstimate{
		System:   systemBytes / bytesPerToken,
		Tools:    toolBytes / bytesPerToken,
		Messages: messageBytes / bytesPerToken,
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
