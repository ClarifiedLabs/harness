package agent

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"harness/internal/hooks"
	"harness/internal/llm"
	"harness/internal/retry"
	"harness/prompts"
)

// defaultKeepTurns is how many whole turns compaction preserves verbatim; everything
// older is summarized into one message (design §12).
const defaultKeepTurns = 8

// defaultKeepTokens is the desired raw recent-suffix size. Whole rounds are
// accumulated newest-first until this target is reached or defaultKeepTurns is
// exhausted.
const defaultKeepTokens = 20_000

// compactThresholdPct is the fraction of the context window at which the
// post-turn trigger fires: reported input tokens ≥ 78% leaves headroom for the
// summary call plus the next turn (design §12).
const compactThresholdPct = 78

// compactTargetPct is the low-water mark reached by a triggered compaction.
// Keeping it below the trigger prevents one small tool result from immediately
// forcing another rewrite.
const compactTargetPct = 65

// overThreshold reports whether tokens crosses the compaction trigger for the
// current window.
func (a *Agent) overThreshold(tokens int) bool {
	return tokens*100 >= a.window()*a.triggerPercent()
}

func (a *Agent) compactBudget() int {
	target := a.window() * a.targetPercent() / 100
	overhead := estimateRequest(llm.Request{
		System:      a.system,
		Tools:       a.toolSpecs,
		ServerTools: a.serverTools,
	}, a.window()).Total
	return max(target-overhead, 0)
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
	Messages      []llm.Message
	Summary       string
	Usage         llm.Usage
	TokensBefore  int
	Focus         string
	ReadFiles     []string
	ModifiedFiles []string
}

// CompactionArchiver preserves raw compacted messages and returns a reference
// suitable for inclusion in the active summary.
type CompactionArchiver func(context.Context, CompactionArchive) (string, error)

// IdleCompactionResult is the output of background compaction work captured by
// PrepareIdleCompaction. Usage is public so the REPL can account for every
// maintenance request, including a candidate later discarded as stale.
// The candidate itself is opaque and can only be applied by
// ApplyIdleCompaction.
type IdleCompactionResult struct {
	Usage    llm.Usage
	Prepared bool

	candidate *idleCompactionCandidate
}

type idleCompactionCandidate struct {
	owner       *Agent
	fingerprint [sha256.Size]byte
	transcript  []llm.Message
	archive     CompactionArchive
	notices     []string
}

const (
	checkpointHeader   = "=== Compaction checkpoint ===\n"
	checkpointPreamble = "The following active instructions are preserved verbatim. Continue from the summarized progress without repeating completed work.\n"
	checkpointProgress = "\nProgress summary:\n"
	checkpointFiles    = "\n\nAuthoritative recognized file-activity index for compacted history (requested paths from successful supported tool calls):\n"
)

// PrepareIdleCompaction captures an immutable compaction snapshot and returns
// work safe to run in a background goroutine. The caller must invoke this method
// on the goroutine that owns the Agent. The returned closure shares only the
// provider; it mutates a private Agent and never runs compaction hooks or archive
// callbacks.
//
// ok is false when automatic compaction is disabled, the configured idle
// threshold has not been reached, or PreCompact/PostCompact hooks make
// speculative execution unsafe. triggerPercent must be in [1, 99].
func (a *Agent) PrepareIdleCompaction(triggerPercent int) (work func(context.Context) (IdleCompactionResult, error), ok bool, err error) {
	if triggerPercent < 1 || triggerPercent > 99 {
		return nil, false, fmt.Errorf("idle compaction trigger percent must be between 1 and 99")
	}
	if !a.autoCompactionEnabled() || a.compactionHooksConfigured() {
		return nil, false, nil
	}
	if estimate := a.estimateContext(nil).Total; estimate*100 < a.window()*triggerPercent {
		return nil, false, nil
	}
	turns := completedTurnSpans(a.transcript)
	boundary := a.preferredCompactionBoundary(turns)
	if (boundary == 0 || countCompletedTurns(a.transcript[:boundary]) == 0) &&
		estimateTokens(a.transcript) <= a.compactBudget() {
		return nil, false, nil
	}
	fingerprint, err := a.idleCompactionFingerprint()
	if err != nil {
		return nil, false, err
	}
	snapshot := cloneMessages(a.transcript)
	worker := New(a.provider, a.tools, Options{
		Model:                       a.model,
		ContextWindow:               a.contextWindow,
		Registry:                    a.registry,
		Reasoning:                   a.reasoning,
		ServerTools:                 a.serverTools,
		Now:                         a.now,
		CompactKeepTurns:            a.compactKeepTurns,
		CompactKeepTokens:           a.compactKeepTokens,
		CompactTriggerPercent:       a.compactTriggerPercent,
		CompactTargetPercent:        a.compactTargetPercent,
		CompactSummaryMaxTokens:     a.compactSummaryMaxTokens,
		CompactToolResultMaxBytes:   a.compactToolResultMaxBytes,
		Interactive:                 a.interactive,
		RetentionPolicy:             a.retentionPolicy,
		DisableAutoCompaction:       a.disableAutoCompaction,
		ResponsesStateful:           false,
		ManagedContinuationStateful: false,
	})
	worker.SetSystem(a.system)
	worker.SetTranscript(snapshot)
	worker.observedContextWindow = a.observedContextWindow
	worker.sleep = a.sleep

	return func(ctx context.Context) (IdleCompactionResult, error) {
		var archived CompactionArchive
		worker.SetCompactionArchiver(func(_ context.Context, archive CompactionArchive) (string, error) {
			archived = cloneCompactionArchive(archive)
			return "", nil
		})
		sink := &idleCompactionSink{}
		usage, changed, err := worker.compactInternal(ctx, sink, "idle", false, false, "")
		result := IdleCompactionResult{Usage: usage}
		if err != nil || !changed {
			return result, err
		}
		if len(archived.Messages) == 0 {
			return result, fmt.Errorf("idle compaction produced no archive metadata")
		}
		result.candidate = &idleCompactionCandidate{
			owner:       a,
			fingerprint: fingerprint,
			transcript:  cloneMessages(worker.transcript),
			archive:     archived,
			notices:     append([]string(nil), sink.notices...),
		}
		result.Prepared = true
		return result, nil
	}, true, nil
}

// ApplyIdleCompaction installs a prepared candidate only while the exact source
// transcript and relevant compaction runtime remain unchanged. A stale
// candidate is silently discarded with applied=false. Archive persistence and
// checkpoint notices occur only after this ownership check, on the caller's
// goroutine.
func (a *Agent) ApplyIdleCompaction(ctx context.Context, sink EventSink, result IdleCompactionResult) (applied bool, err error) {
	candidate := result.candidate
	if candidate == nil || candidate.owner != a || a.compactionHooksConfigured() {
		return false, nil
	}
	fingerprint, err := a.idleCompactionFingerprint()
	if err != nil {
		return false, err
	}
	if fingerprint != candidate.fingerprint {
		return false, nil
	}
	compacted := cloneMessages(candidate.transcript)
	if err := llm.ValidateTranscript(compacted); err != nil {
		return false, fmt.Errorf("idle compaction candidate invalid: %w", err)
	}

	archiveRef := ""
	if a.archiveCompaction != nil {
		ref, err := a.archiveCompaction(ctx, cloneCompactionArchive(candidate.archive))
		if err != nil {
			return false, fmt.Errorf("idle compaction archive: %w", err)
		}
		archiveRef = ref
	}
	compacted[0] = a.checkpointMessage(
		candidate.archive.Summary,
		candidate.archive.Messages,
		archiveRef,
		candidate.archive.Focus,
		candidate.archive.ReadFiles,
		candidate.archive.ModifiedFiles,
	)
	a.transcript = compacted
	a.validatedPrefix = 0
	// A compacted transcript is a new baseline: re-arm pressure retention so the
	// next high-water pass can fire an epoch even when compaction lands between
	// the low and high watermarks.
	a.retentionEpochArmed = true
	a.ResetProxySessionID()
	a.compactFallbackNotice = compactFallbackNoticeState{}
	for _, notice := range candidate.notices {
		if strings.HasPrefix(notice, "[compacted:") {
			notice = "[idle compacted:" + strings.TrimPrefix(notice, "[compacted:")
		}
		sink.Notice(notice)
	}
	return true, nil
}

func (a *Agent) compactionHooksConfigured() bool {
	return a.hooks != nil && (a.hooks.HasEvent(hooks.PreCompact) || a.hooks.HasEvent(hooks.PostCompact))
}

func (a *Agent) idleCompactionFingerprint() ([sha256.Size]byte, error) {
	digest := sha256.New()
	err := json.NewEncoder(digest).Encode(struct {
		Transcript                []llm.Message    `json:"transcript"`
		System                    string           `json:"system"`
		Model                     string           `json:"model"`
		Tools                     []llm.ToolSchema `json:"tools"`
		ServerTools               []llm.ServerTool `json:"server_tools"`
		ContextWindow             int              `json:"context_window"`
		ObservedContextWindow     int              `json:"observed_context_window"`
		CompactKeepTurns          int              `json:"compact_keep_turns"`
		CompactKeepTokens         int              `json:"compact_keep_tokens"`
		CompactTargetPercent      int              `json:"compact_target_percent"`
		CompactSummaryMaxTokens   int              `json:"compact_summary_max_tokens"`
		CompactToolResultMaxBytes int              `json:"compact_tool_result_max_bytes"`
		RuntimeVersion            uint64           `json:"runtime_version"`
	}{
		Transcript:                a.transcript,
		System:                    a.system,
		Model:                     a.model,
		Tools:                     a.toolSpecs,
		ServerTools:               a.serverTools,
		ContextWindow:             a.contextWindow,
		ObservedContextWindow:     a.observedContextWindow,
		CompactKeepTurns:          a.compactKeepTurns,
		CompactKeepTokens:         a.compactKeepTokens,
		CompactTargetPercent:      a.compactTargetPercent,
		CompactSummaryMaxTokens:   a.compactSummaryMaxTokens,
		CompactToolResultMaxBytes: a.compactToolResultMaxBytes,
		RuntimeVersion:            a.compactionRuntimeVersion,
	})
	if err != nil {
		return [sha256.Size]byte{}, fmt.Errorf("fingerprint idle compaction snapshot: %w", err)
	}
	var fingerprint [sha256.Size]byte
	copy(fingerprint[:], digest.Sum(nil))
	return fingerprint, nil
}

func cloneCompactionArchive(archive CompactionArchive) CompactionArchive {
	archive.Messages = cloneMessages(archive.Messages)
	archive.ReadFiles = append([]string(nil), archive.ReadFiles...)
	archive.ModifiedFiles = append([]string(nil), archive.ModifiedFiles...)
	return archive
}

type idleCompactionSink struct {
	notices []string
}

func (*idleCompactionSink) TextDelta(string)                           {}
func (*idleCompactionSink) ReasoningSummary(string)                    {}
func (*idleCompactionSink) TurnAttemptStart(int, int, ContextEstimate) {}
func (*idleCompactionSink) TurnAttemptComplete(TurnAttemptUsage)       {}
func (*idleCompactionSink) ToolUseStart(llm.ToolCall)                  {}
func (*idleCompactionSink) ToolUseDelta(int, string)                   {}
func (*idleCompactionSink) ToolStart(llm.ToolCall)                     {}
func (*idleCompactionSink) ToolResult(llm.ToolResult)                  {}
func (s *idleCompactionSink) Notice(message string)                    { s.notices = append(s.notices, message) }
func (*idleCompactionSink) TurnComplete(TurnUsage)                     {}
func (*idleCompactionSink) PromptComplete(PromptUsage)                 {}

// MaybeCompact compacts the transcript when automatic compaction is enabled and
// lastInputTokens reaches the configured fraction of the model's context
// window; otherwise it is a no-op (design §12, §8.1). It returns the summary
// call's usage (zero when no
// compaction ran), a changed flag reporting whether the transcript was actually
// rewritten, and any error. The caller folds the usage into session totals and
// uses changed to decide whether to reset its trigger state (r-churn).
func (a *Agent) MaybeCompact(ctx context.Context, lastInputTokens int, sink EventSink) (llm.Usage, bool, error) {
	if !a.autoCompactionEnabled() || !a.overThreshold(lastInputTokens) {
		return llm.Usage{}, false, nil
	}
	return a.compactTriggered(ctx, sink, "auto")
}

// Compact collapses history older than the hybrid whole-round suffix into a
// synthetic user checkpoint containing a model-written progress summary,
// keeping the system prompt on Request.System and recent rounds verbatim
// (design §12). The summary call's
// usage is returned for the session totals. On a summary-call error the
// transcript is left fully intact and the error is returned, with a warning
// reported via the sink — a visible context-length failure beats silent data
// loss. The result always satisfies the §4 invariant: kept turns are whole, so
// no tool_use/tool_result pair is ever split.
func (a *Agent) Compact(ctx context.Context, sink EventSink) (llm.Usage, error) {
	return a.CompactWithFocus(ctx, sink, "")
}

// CompactWithFocus forces a manual compaction and gives the summary one-shot
// emphasis. The trimmed focus is stored for observability on this checkpoint
// only and is never inherited by later compactions.
func (a *Agent) CompactWithFocus(ctx context.Context, sink EventSink, focus string) (llm.Usage, error) {
	u, _, err := a.compactInternal(ctx, sink, "manual", false, false, strings.TrimSpace(focus))
	return u, err
}

// CompactForContinuation collapses the complete transcript into one typed
// checkpoint for a fresh compatible delegate. Unlike ordinary compaction it
// retains no verbatim conversational suffix: every completed turn is included
// in the summary input and raw archive, while active prompt/steering
// instructions remain verbatim in the checkpoint. It never truncates the
// resulting checkpoint to a target percentage; callers must estimate the final
// request and reject it when the exact instructions still do not fit safely.
func (a *Agent) CompactForContinuation(ctx context.Context, sink EventSink) (llm.Usage, bool, error) {
	return a.compactInternal(ctx, sink, "continuation", false, true, "")
}

// compact returns the summary-call usage, a changed flag (true only when the
// live transcript was actually rewritten), and any error. A no-op (nothing old
// enough to summarize and the transcript already within budget, or a PreCompact
// block) returns changed=false so the mid-loop caller does not churn its trigger
// state every turn.
func (a *Agent) compact(ctx context.Context, sink EventSink, trigger string) (llm.Usage, bool, error) {
	return a.compactInternal(ctx, sink, trigger, false, false, "")
}

// compactTriggered is used when a measured request footprint or provider
// overflow says the active context is too large. In a single long tool turn
// there may be no older turn to summarize and the byte estimate can still be
// optimistic, so force the current-turn shrink path instead of no-oping.
func (a *Agent) compactTriggered(ctx context.Context, sink EventSink, trigger string) (llm.Usage, bool, error) {
	return a.compactInternal(ctx, sink, trigger, true, false, "")
}

func (a *Agent) compactInternal(ctx context.Context, sink EventSink, trigger string, forceCurrent, collapseAll bool, focus string) (llm.Usage, bool, error) {
	if progress, ok := sink.(CompactionProgressSink); ok {
		progress.CompactionStart()
		defer progress.CompactionComplete()
	}
	if a.hooks != nil && a.hooks.HasEvent(hooks.PreCompact) {
		payload := hooks.Payload{"trigger": trigger}
		if focus != "" {
			payload["focus"] = focus
		}
		res := a.hooks.Run(ctx, hooks.PreCompact, trigger, payload)
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

	before := a.estimateContext(nil).Total
	turns := completedTurnSpans(a.transcript)
	boundary := len(a.transcript)
	if !collapseAll {
		boundary = a.preferredCompactionBoundary(turns)
		if boundary == 0 || countCompletedTurns(a.transcript[:boundary]) == 0 {
			if !forceCurrent && estimateTokens(a.transcript) <= a.compactBudget() {
				return llm.Usage{}, false, nil
			}
			if len(turns) < 2 {
				changed, err := a.degradeCurrent(sink, trigger)
				return llm.Usage{}, changed, err
			}
			// Under pressure there must be a completed round to summarize. Start with
			// only the newest round retained; this also avoids paying for a summary of
			// an initial prompt alone.
			boundary = turns[len(turns)-1].Start
		}
	} else if len(a.transcript) == 0 {
		return llm.Usage{}, false, nil
	}

	var summary string
	var usage llm.Usage
	var older, kept []llm.Message
	var readFiles, modifiedFiles []string
	var compacted []llm.Message
	for {
		older = a.transcript[:boundary]
		kept = a.transcript[boundary:]
		prior := priorCompactionMetadata(older)
		readFiles, modifiedFiles = a.compactionFileActivity(older, prior)
		generated, attemptUsage, err := a.summarizeCompaction(ctx, older, prior, readFiles, modifiedFiles, focus)
		usage = add(usage, attemptUsage)
		if err != nil {
			sink.Notice(fmt.Sprintf("[compact failed: %v; keeping full transcript]", err))
			return usage, false, err
		}
		summary = generated
		compacted = append([]llm.Message{a.checkpointMessage(summary, older, "", focus, readFiles, modifiedFiles)}, cloneMessages(kept)...)
		if collapseAll || a.estimateContextForTranscript(nil, compacted).Total <= a.window()*a.targetPercent()/100 {
			break
		}
		next, ok := nextCompactionBoundary(turns, boundary)
		if !ok {
			break
		}
		boundary = next
	}

	// Once only the newest round remains, local degradation is the only safe
	// lower rung. It mutates deep copies and cannot silently discard a round.
	if !collapseAll && a.estimateContextForTranscript(nil, compacted).Total > a.window()*a.targetPercent()/100 {
		a.trimToolResults(compacted, sink)
		truncateUntilFits(compacted, a.compactBudget())
	}
	if reclaimedTooLittle(before, compacted) {
		a.trimToolResults(compacted, sink)
	}
	if err := llm.ValidateTranscript(compacted); err != nil {
		sink.Notice(fmt.Sprintf("[compact failed: compacted transcript invalid: %v; keeping full transcript]", err))
		return usage, false, err
	}

	archiveRef := ""
	if a.archiveCompaction != nil {
		ref, err := a.archiveCompaction(ctx, CompactionArchive{
			Messages:      cloneMessages(older),
			Summary:       summary,
			Usage:         usage,
			TokensBefore:  before,
			Focus:         focus,
			ReadFiles:     append([]string(nil), readFiles...),
			ModifiedFiles: append([]string(nil), modifiedFiles...),
		})
		if err != nil {
			sink.Notice(fmt.Sprintf("[compact archive failed: %v; keeping full transcript]", err))
			return usage, false, err
		}
		archiveRef = ref
	}

	collapsed := countCompletedTurns(older)
	// Adding an archive reference changes only checkpoint text, so the transcript
	// shape validated above cannot become invalid after the side-effectful archive
	// callback succeeds.
	compacted[0] = a.checkpointMessage(summary, older, archiveRef, focus, readFiles, modifiedFiles)

	a.transcript = compacted
	a.validatedPrefix = 0        // the transcript was rewritten; re-validate from scratch (r62)
	a.retentionEpochArmed = true // a compacted transcript is a new retention baseline
	a.ResetProxySessionID()
	a.compactFallbackNotice = compactFallbackNoticeState{}
	after := a.estimateContextForTranscript(nil, compacted).Total
	sink.Notice(compactionReport(collapsed, before, after))
	if a.hooks != nil && a.hooks.HasEvent(hooks.PostCompact) {
		payload := hooks.Payload{"trigger": trigger}
		if focus != "" {
			payload["focus"] = focus
		}
		res := a.hooks.Run(ctx, hooks.PostCompact, trigger, payload)
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

// degradeCurrent shrinks the live transcript in place when it is over budget and
// there is no older turn to summarize — a single ballooning turn.
// It trims large read-only results, then hard-truncates the largest blocks until
// the estimate fits, so an oversized request never reaches the provider (never
// wedge, design §12). All mutation happens on a deep copy, so a post-shrink
// ValidateTranscript failure leaves the live transcript fully intact (the
// rollback guarantee). It returns whether the transcript was actually replaced.
func (a *Agent) degradeCurrent(sink EventSink, trigger string) (bool, error) {
	before := a.estimateContext(nil).Total
	budget := a.compactBudget()
	compacted := cloneMessages(a.transcript)
	a.trimToolResults(compacted, sink)
	truncateUntilFits(compacted, budget)
	after := a.estimateContextForTranscript(nil, compacted).Total
	if after >= before {
		// Nothing left to shrink; ship the oversized request rather than churn an
		// identical rewrite. Surface it so the wedge risk is visible, not silent.
		a.noticeCurrentNoShrink(sink, trigger)
		return false, nil
	}
	if err := llm.ValidateTranscript(compacted); err != nil {
		sink.Notice(fmt.Sprintf("[compact failed: shrunk transcript invalid: %v; keeping full transcript]", err))
		return false, err
	}
	a.transcript = compacted
	a.validatedPrefix = 0        // the transcript was rewritten; re-validate from scratch (r62)
	a.retentionEpochArmed = true // a compacted transcript is a new retention baseline
	a.ResetProxySessionID()
	a.noticeCurrentShrink(sink, trigger, before, after)
	return true, nil
}

const smallCurrentShrinkNoticeTokens = 1000

func (a *Agent) noticeCurrentNoShrink(sink EventSink, trigger string) {
	if trigger != "manual" {
		if a.compactFallbackNotice.noShrink {
			return
		}
		a.compactFallbackNotice.noShrink = true
	}
	sink.Notice("[compact: transcript over budget but nothing left to shrink]")
}

func (a *Agent) noticeCurrentShrink(sink EventSink, trigger string, before, after int) {
	if trigger != "manual" && before-after < smallCurrentShrinkNoticeTokens {
		if a.compactFallbackNotice.smallShrink {
			return
		}
		a.compactFallbackNotice.smallShrink = true
	}
	sink.Notice(fmt.Sprintf("[compacted: archived oversized turn payload · ctx ~%s → ~%s]", kiloTokens(before), kiloTokens(after)))
}

// cloneMessages returns a deep-enough copy of msgs for transcript rewrites.
// Content and nested rich tool-result content get independent backing slices, as
// do the other message-owned slices that compaction may retain or mutate.
func cloneMessages(msgs []llm.Message) []llm.Message {
	out := make([]llm.Message, len(msgs))
	for i, m := range msgs {
		out[i] = m
		out[i].Content = cloneContentBlocks(m.Content)
		out[i].ParallelToolBatches = append([]llm.ParallelToolBatch(nil), m.ParallelToolBatches...)
		for j := range out[i].ParallelToolBatches {
			out[i].ParallelToolBatches[j].ToolUseIDs = append([]string(nil), m.ParallelToolBatches[j].ToolUseIDs...)
		}
		out[i].Compaction = cloneCompactionMetadata(m.Compaction)
	}
	return out
}

func cloneContentBlocks(blocks []llm.ContentBlock) []llm.ContentBlock {
	if blocks == nil {
		return nil
	}
	out := append([]llm.ContentBlock(nil), blocks...)
	for i := range out {
		out[i].ResultContent = cloneContentBlocks(blocks[i].ResultContent)
		out[i].ToolInput = append(json.RawMessage(nil), blocks[i].ToolInput...)
		out[i].InteractionStep = append(json.RawMessage(nil), blocks[i].InteractionStep...)
	}
	return out
}

func cloneCompactionMetadata(meta *llm.CompactionMetadata) *llm.CompactionMetadata {
	if meta == nil {
		return nil
	}
	out := *meta
	out.ReadFiles = append([]string(nil), meta.ReadFiles...)
	out.ModifiedFiles = append([]string(nil), meta.ModifiedFiles...)
	return &out
}

// GenerateSummary runs one tool-less summarization pass over the full current
// transcript using the given system instruction, returning the summary text and
// the call's usage. It backs the plan->implementation handoff (with the handoff
// brief prompt) and is independent of the compaction trigger/keep-turn logic.
func (a *Agent) GenerateSummary(ctx context.Context, system string) (string, llm.Usage, error) {
	return a.summarize(ctx, system, a.transcript, llm.RequestPurposeHandoffSummary)
}

// GenerateBranchSummary summarizes only the conversation fragment that will
// no longer be active after tree navigation. The current transcript is not
// modified and tools/reasoning remain disabled for the maintenance call.
func (a *Agent) GenerateBranchSummary(ctx context.Context, messages []llm.Message, focus string) (string, llm.Usage, error) {
	system := prompts.BranchSummary()
	if focus = strings.TrimSpace(focus); focus != "" {
		system += "\n\nGive special attention to: " + focus
	}
	return a.summarize(ctx, system, messages, llm.RequestPurposeBranchSummary)
}

// summarize runs one tool-less model call over the older messages, with the
// given system instruction, and returns the summary text and the call's usage.
func (a *Agent) summarize(ctx context.Context, system string, older []llm.Message, purpose llm.RequestPurpose) (string, llm.Usage, error) {
	prepared := prepareSummaryMessages(older, a.summaryToolResultMaxBytes())
	chunks := splitSummaryChunks(prepared, a.summaryChunkBudget())
	if len(chunks) <= 1 {
		return a.summarizeOne(ctx, system, prepared, purpose)
	}

	var total llm.Usage
	summaries := make([]llm.Message, 0, len(chunks))
	for i, chunk := range chunks {
		summary, usage, err := a.summarizeOne(ctx, system, chunk, purpose)
		total = add(total, usage)
		if err != nil {
			return "", total, err
		}
		summaries = append(summaries, textMessageAt(a.now(), llm.RoleUser, fmt.Sprintf("Chunk %d summary:\n%s", i+1, summary)))
	}
	final, usage, err := a.summarizeOne(ctx, system, summaries, purpose)
	total = add(total, usage)
	if err != nil {
		return "", total, err
	}
	return final, total, nil
}

// summarizeCompaction keeps iterative checkpoint updating separate from generic
// branch/handoff summarization. A structured prior checkpoint is archived and
// preserved for active-instruction extraction, but is not re-summarized as
// ordinary conversation.
func (a *Agent) summarizeCompaction(ctx context.Context, older []llm.Message, prior *llm.CompactionMetadata, readFiles, modifiedFiles []string, focus string) (string, llm.Usage, error) {
	newlyAged := older
	if prior != nil {
		newlyAged = make([]llm.Message, 0, len(older)-1)
		for _, message := range older {
			if message.Origin == llm.MessageOriginCompactionCheckpoint && message.Compaction != nil {
				continue
			}
			newlyAged = append(newlyAged, message)
		}
	}
	prepared := prepareSummaryMessages(newlyAged, a.summaryToolResultMaxBytes())
	chunks := splitSummaryChunks(prepared, a.summaryChunkBudget())
	mapSystem := compactionSystem(prompts.CompactionSummary(), focus)
	finalMessages := prepared
	var total llm.Usage
	if len(chunks) > 1 {
		finalMessages = make([]llm.Message, 0, len(chunks))
		for i, chunk := range chunks {
			summary, usage, err := a.summarizeOne(ctx, mapSystem, chunk, llm.RequestPurposeCompaction)
			total = add(total, usage)
			if err != nil {
				return "", total, err
			}
			message := textMessageAt(a.now(), llm.RoleUser, fmt.Sprintf("Chunk %d summary:\n%s", i+1, summary))
			message.Origin = llm.MessageOriginInternal
			finalMessages = append(finalMessages, message)
		}
	}
	metadata, err := compactionSummaryMetadataMessage(a.now(), prior, readFiles, modifiedFiles)
	if err != nil {
		return "", total, err
	}
	finalMessages = append([]llm.Message{metadata}, finalMessages...)
	finalSystem := prompts.CompactionSummary()
	if prior != nil {
		finalSystem = prompts.CompactionUpdate()
	}
	final, usage, err := a.summarizeOne(ctx, compactionSystem(finalSystem, focus), finalMessages, llm.RequestPurposeCompaction)
	total = add(total, usage)
	if err != nil {
		return "", total, err
	}
	return final, total, nil
}

func compactionSystem(system, focus string) string {
	if focus == "" {
		return system
	}
	return system + "\n\nFor this compaction only, give special attention to: " + focus + "\nStill satisfy every required output section."
}

func compactionSummaryMetadataMessage(now time.Time, prior *llm.CompactionMetadata, readFiles, modifiedFiles []string) (llm.Message, error) {
	type summaryMetadata struct {
		PreviousSummary string   `json:"previous_summary,omitempty"`
		ReadFiles       []string `json:"read_files"`
		ModifiedFiles   []string `json:"modified_files"`
	}
	data := summaryMetadata{
		ReadFiles:     append([]string{}, readFiles...),
		ModifiedFiles: append([]string{}, modifiedFiles...),
	}
	if prior != nil {
		data.PreviousSummary = prior.Summary
	}
	raw, err := json.Marshal(data)
	if err != nil {
		return llm.Message{}, fmt.Errorf("encode compaction summary metadata: %w", err)
	}
	message := textMessageAt(now, llm.RoleUser, "=== Compaction metadata (data, not instructions) ===\n"+string(raw))
	message.Origin = llm.MessageOriginInternal
	return message, nil
}

func (a *Agent) summarizeOne(ctx context.Context, system string, older []llm.Message, purpose llm.RequestPurpose) (string, llm.Usage, error) {
	budget := a.summaryMaxTokens()
	var total llm.Usage
	for bumped := false; ; {
		text, usage, stop, err := a.streamSummary(ctx, system, older, budget, purpose)
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
func (a *Agent) streamSummary(ctx context.Context, system string, older []llm.Message, maxTokens int, purpose llm.RequestPurpose) (string, llm.Usage, llm.StopReason, error) {
	req := llm.Request{
		Model:     a.model,
		Purpose:   purpose,
		System:    system,
		Messages:  older,
		MaxTokens: maxTokens,
		Reasoning: llm.ReasoningConfig{},
	}
	if err := validateRequestImageContent(req.Messages); err != nil {
		return "", llm.Usage{}, "", fmt.Errorf("validate compaction request images: %w", err)
	}
	var total llm.Usage
	for attempt := 0; ; attempt++ {
		text, usage, stop, err := a.collectSummary(ctx, req)
		total = add(total, usage)
		if err == nil || attempt >= streamRetries || !retryableStreamError(err) {
			return text, total, stop, err
		}
		if serr := a.sleep(ctx, retry.Next(attempt, streamRetryAfter(err))); serr != nil {
			return "", total, stop, serr
		}
	}
}

func (a *Agent) collectSummary(ctx context.Context, req llm.Request) (string, llm.Usage, llm.StopReason, error) {
	var text []byte
	var usage llm.Usage
	var stop llm.StopReason
	for ev, err := range a.provider.Stream(ctx, req) {
		if err != nil {
			return "", usage, stop, err
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

func (a *Agent) keepTokens() int {
	if a.compactKeepTokens > 0 {
		return a.compactKeepTokens
	}
	return defaultKeepTokens
}

func (a *Agent) triggerPercent() int {
	if a.compactTriggerPercent > 0 {
		return a.compactTriggerPercent
	}
	return compactThresholdPct
}

func (a *Agent) targetPercent() int {
	if a.compactTargetPercent > 0 {
		return a.compactTargetPercent
	}
	return compactTargetPct
}

func (a *Agent) autoCompactionEnabled() bool { return !a.disableAutoCompaction }

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
		return llm.DefaultContextWindow * a.triggerPercent() / 100
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
		out[i] = llm.Message{Role: m.Role, Time: m.Time, Phase: m.Phase, Content: cloneContentBlocks(m.Content)}
		for j := range out[i].Content {
			b := &out[i].Content[j]
			switch b.Kind {
			case llm.BlockToolResult:
				if len(b.ResultText) > maxToolResultBytes {
					b.ResultText = b.ResultText[:maxToolResultBytes] +
						fmt.Sprintf("\n[summary input truncated: showing first %d of %d bytes; raw content archived if compaction succeeds]", maxToolResultBytes, len(b.ResultText))
				}
				degradeToolResultImages(b, func(child llm.ContentBlock) bool {
					return len(child.ImageData) > maxToolResultBytes
				})
			case llm.BlockToolUse:
				if len(b.ToolInput) > maxToolResultBytes {
					b.ToolInput, _ = shortenedToolInput(b.ToolInput, maxToolResultBytes)
				}
			case llm.BlockImage:
				if len(b.ImageData) > maxToolResultBytes {
					*b = llm.ContentBlock{
						Kind: llm.BlockText,
						Text: imageSummaryPlaceholder(*b),
					}
				}
			}
		}
	}
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
	if b.ImageDetail != "" {
		parts = append(parts, "detail "+b.ImageDetail)
	}
	if b.ImageWidth > 0 || b.ImageHeight > 0 {
		parts = append(parts, fmt.Sprintf("dimensions %dx%d", b.ImageWidth, b.ImageHeight))
	}
	parts = append(parts, fmt.Sprintf("%d encoded bytes", len(b.ImageData)))
	return "[image omitted from compaction summary: " + strings.Join(parts, ", ") + "]"
}

// degradeToolResultImages removes matching supplementary images from a rich
// tool result and appends model-visible descriptions to ResultText. Rich result
// children are image-only, so putting the descriptions on the parent preserves
// transcript validity while ensuring the discarded base64 is no longer hidden
// in a nested slice.
func degradeToolResultImages(b *llm.ContentBlock, shouldDegrade func(llm.ContentBlock) bool) bool {
	if b.Kind != llm.BlockToolResult || len(b.ResultContent) == 0 {
		return false
	}
	changed := false
	kept := make([]llm.ContentBlock, 0, len(b.ResultContent))
	for _, child := range b.ResultContent {
		if child.Kind == llm.BlockImage && shouldDegrade(child) {
			appendResultDescription(b, imageSummaryPlaceholder(child))
			changed = true
			continue
		}
		kept = append(kept, child)
	}
	if !changed {
		return false
	}
	if len(kept) == 0 {
		b.ResultContent = nil
	} else {
		b.ResultContent = kept
	}
	return true
}

func degradeToolResultImageAt(b *llm.ContentBlock, index int) bool {
	if b.Kind != llm.BlockToolResult || index < 0 || index >= len(b.ResultContent) || b.ResultContent[index].Kind != llm.BlockImage {
		return false
	}
	appendResultDescription(b, imageSummaryPlaceholder(b.ResultContent[index]))
	copy(b.ResultContent[index:], b.ResultContent[index+1:])
	b.ResultContent[len(b.ResultContent)-1] = llm.ContentBlock{}
	b.ResultContent = b.ResultContent[:len(b.ResultContent)-1]
	if len(b.ResultContent) == 0 {
		b.ResultContent = nil
	}
	return true
}

func appendResultDescription(b *llm.ContentBlock, description string) {
	if b.ResultText != "" && !strings.HasSuffix(b.ResultText, "\n") {
		b.ResultText += "\n"
	}
	b.ResultText += description
}

func splitSummaryChunks(msgs []llm.Message, budget int) [][]llm.Message {
	if len(msgs) == 0 || estimateTokens(msgs) <= budget {
		return [][]llm.Message{msgs}
	}
	turns := completedTurnSpans(msgs)
	if len(turns) == 0 {
		return [][]llm.Message{msgs}
	}
	var chunks [][]llm.Message
	var current []llm.Message
	start := 0
	for _, span := range turns {
		unit := msgs[start:span.End]
		if len(current) > 0 && estimateTokens(append(append([]llm.Message(nil), current...), unit...)) > budget {
			chunks = append(chunks, current)
			current = nil
		}
		current = append(current, unit...)
		start = span.End
	}
	if start < len(msgs) {
		current = append(current, msgs[start:]...)
	}
	if len(current) > 0 {
		chunks = append(chunks, current)
	}
	return chunks
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

type turnSpan struct {
	Start int
	End   int
}

// preferredCompactionBoundary selects a whole-round suffix by accumulating
// newest rounds until the token target is first reached, capped by keepTurns.
// Returning zero means every completed round fit inside the preference.
func (a *Agent) preferredCompactionBoundary(turns []turnSpan) int {
	if len(turns) == 0 {
		return 0
	}
	limit := min(a.keepTurns(), len(turns))
	boundary := 0
	for kept := 1; kept <= limit; kept++ {
		boundary = turns[len(turns)-kept].Start
		if estimateTokens(a.transcript[boundary:]) >= a.keepTokens() {
			return boundary
		}
	}
	if limit < len(turns) {
		return boundary
	}
	return 0
}

// nextCompactionBoundary moves the oldest retained whole round behind the
// summary boundary. It returns false once only the newest round remains.
func nextCompactionBoundary(turns []turnSpan, boundary int) (int, bool) {
	for i, turn := range turns {
		if turn.Start != boundary {
			continue
		}
		if i+1 >= len(turns) {
			return 0, false
		}
		return turns[i+1].Start, true
	}
	return 0, false
}

// completedTurnSpans returns canonical conversational turns. Each span begins
// at an assistant response and includes its immediately following tool-result
// message, if present. Prompt and steering messages are inputs to the next turn,
// not boundaries that collapse all round trips into one oversized unit.
func completedTurnSpans(msgs []llm.Message) []turnSpan {
	var spans []turnSpan
	for i, message := range msgs {
		if message.Role != llm.RoleAssistant {
			continue
		}
		end := i + 1
		if hasToolUse(message) && end < len(msgs) && msgs[end].Role == llm.RoleUser && hasToolResult(msgs[end]) {
			end++
		}
		spans = append(spans, turnSpan{Start: i, End: end})
	}
	return spans
}

func turnStarts(msgs []llm.Message) []int {
	spans := completedTurnSpans(msgs)
	starts := make([]int, len(spans))
	for i, span := range spans {
		starts[i] = span.Start
	}
	return starts
}

func countCompletedTurns(msgs []llm.Message) int {
	return len(completedTurnSpans(msgs))
}

func hasToolUse(message llm.Message) bool {
	for _, block := range message.Content {
		if block.Kind == llm.BlockToolUse {
			return true
		}
	}
	return false
}

func hasToolResult(message llm.Message) bool {
	for _, block := range message.Content {
		if block.Kind == llm.BlockToolResult {
			return true
		}
	}
	return false
}

func hasNonResult(m llm.Message) bool {
	for _, b := range m.Content {
		if b.Kind != llm.BlockToolResult {
			return true
		}
	}
	return len(m.Content) == 0
}

func (a *Agent) checkpointMessage(summary string, older []llm.Message, archiveRef, focus string, readFiles, modifiedFiles []string) llm.Message {
	var b strings.Builder
	b.WriteString(checkpointHeader)
	b.WriteString(checkpointPreamble)
	for _, message := range activeInstructionMessages(older) {
		switch message.Origin {
		case llm.MessageOriginSteer:
			b.WriteString("\nSteering instruction (verbatim):\n")
		case llm.MessageOriginCompactionCheckpoint:
			b.WriteString("\nActive instructions from prior checkpoint (verbatim):\n")
		default:
			b.WriteString("\nPrompt (verbatim):\n")
		}
		text := messageTextForCheckpoint(message)
		if message.Origin == llm.MessageOriginCompactionCheckpoint {
			text = checkpointInstructionText(text)
		}
		b.WriteString(text)
		b.WriteByte('\n')
	}
	b.WriteString(checkpointProgress)
	b.WriteString(summary)
	b.WriteString(checkpointFiles)
	fileJSON, _ := json.Marshal(struct {
		ReadFiles     []string `json:"read_files"`
		ModifiedFiles []string `json:"modified_files"`
	}{
		ReadFiles:     append([]string{}, readFiles...),
		ModifiedFiles: append([]string{}, modifiedFiles...),
	})
	b.Write(fileJSON)
	if archiveRef != "" {
		b.WriteString("\n\nRaw compacted transcript archive: ")
		b.WriteString(archiveRef)
	}
	message := a.textMessage(llm.RoleUser, b.String())
	message.Origin = llm.MessageOriginCompactionCheckpoint
	message.Compaction = &llm.CompactionMetadata{
		Summary:       summary,
		Focus:         focus,
		ReadFiles:     append([]string(nil), readFiles...),
		ModifiedFiles: append([]string(nil), modifiedFiles...),
	}
	return message
}

func priorCompactionMetadata(messages []llm.Message) *llm.CompactionMetadata {
	for i := len(messages) - 1; i >= 0; i-- {
		if messages[i].Origin == llm.MessageOriginCompactionCheckpoint && messages[i].Compaction != nil {
			return cloneCompactionMetadata(messages[i].Compaction)
		}
	}
	return nil
}

// compactionFileActivity accumulates successful supported file operations from
// adjacent tool-use/result pairs in the newly compacted history. Tool IDs are
// intentionally correlated within each pair so providers may safely reuse IDs
// in later rounds.
func (a *Agent) compactionFileActivity(messages []llm.Message, prior *llm.CompactionMetadata) ([]string, []string) {
	reads := make(map[string]string)
	modified := make(map[string]string)
	addRead := func(path string) {
		display, key, ok := compactPath(path)
		if !ok {
			return
		}
		if _, changed := modified[key]; changed {
			return
		}
		if _, exists := reads[key]; !exists {
			reads[key] = display
		}
	}
	addModified := func(path string) {
		display, key, ok := compactPath(path)
		if !ok {
			return
		}
		delete(reads, key)
		if _, exists := modified[key]; !exists {
			modified[key] = display
		}
	}
	if prior != nil {
		for _, path := range prior.ReadFiles {
			addRead(path)
		}
		for _, path := range prior.ModifiedFiles {
			addModified(path)
		}
	}
	for i := 0; i+1 < len(messages); i++ {
		assistant, resultMessage := messages[i], messages[i+1]
		if assistant.Role != llm.RoleAssistant || resultMessage.Role != llm.RoleUser {
			continue
		}
		results := make(map[string]llm.ContentBlock)
		for _, block := range resultMessage.Content {
			if block.Kind == llm.BlockToolResult {
				results[block.ResultForID] = block
			}
		}
		for _, block := range assistant.Content {
			if block.Kind != llm.BlockToolUse {
				continue
			}
			result, ok := results[block.ToolUseID]
			if !ok || result.ResultError {
				continue
			}
			call := llm.ToolCall{ID: block.ToolUseID, Name: block.ToolName, Input: block.ToolInput}
			if paths, ok := a.tools.ReadPaths(call); ok {
				for _, path := range paths {
					addRead(path)
				}
			}
			if paths, ok := a.tools.MutatedPaths(call); ok {
				for _, path := range paths {
					addModified(path)
				}
			}
		}
	}
	return sortedPathValues(reads), sortedPathValues(modified)
}

func compactPath(path string) (display, key string, ok bool) {
	path = strings.TrimSpace(path)
	if path == "" {
		return "", "", false
	}
	display = filepath.Clean(path)
	key = display
	if abs, err := filepath.Abs(display); err == nil {
		key = filepath.Clean(abs)
	}
	return display, key, true
}

func sortedPathValues(paths map[string]string) []string {
	out := make([]string, 0, len(paths))
	for _, path := range paths {
		out = append(out, path)
	}
	sort.Strings(out)
	return out
}

func activeInstructionMessages(msgs []llm.Message) []llm.Message {
	start := -1
	for i := range msgs {
		if msgs[i].Origin == llm.MessageOriginPrompt || msgs[i].Origin == llm.MessageOriginCompactionCheckpoint {
			start = i
		}
	}
	if start < 0 {
		for i := len(msgs) - 1; i >= 0; i-- {
			if msgs[i].Role == llm.RoleUser && hasNonResult(msgs[i]) {
				start = i
				break
			}
		}
	}
	if start < 0 {
		return nil
	}
	var out []llm.Message
	for _, message := range msgs[start:] {
		if message.Origin == llm.MessageOriginPrompt || message.Origin == llm.MessageOriginSteer || message.Origin == llm.MessageOriginCompactionCheckpoint || len(out) == 0 && message.Role == llm.RoleUser && hasNonResult(message) {
			out = append(out, message)
		}
	}
	return out
}

func checkpointInstructionText(text string) string {
	prefix, _, found := strings.Cut(text, checkpointProgress)
	if !found {
		return text
	}
	prefix = strings.TrimPrefix(prefix, checkpointHeader)
	prefix = strings.TrimPrefix(prefix, checkpointPreamble)
	return strings.TrimSpace(prefix)
}

func messageTextForCheckpoint(message llm.Message) string {
	var parts []string
	for _, block := range message.Content {
		switch block.Kind {
		case llm.BlockText:
			parts = append(parts, block.Text)
		case llm.BlockImage:
			parts = append(parts, imageSummaryPlaceholder(block))
		}
	}
	return strings.Join(parts, "\n")
}

// minTruncResult is the smallest tool_result worth shrinking; below it the saving
// is not worth a truncation marker and the ladder stops to avoid spinning.
const minTruncResult = 256

// truncateLargestBlock removes at least dropBytes from the single largest
// shrinkable block, replacing its tail or payload with a marker. It returns false
// when no block is large enough to shrink usefully, so the caller stops rather
// than loops forever (never wedge, design §12).
func truncateLargestBlock(msgs []llm.Message, dropBytes int) bool {
	bi, bj, childIndex, bestLen, kind := -1, -1, -1, 0, llm.BlockText
	consider := func(i, j, child, size int, candidateKind llm.BlockKind) {
		if size > bestLen {
			bi, bj, childIndex, bestLen, kind = i, j, child, size, candidateKind
		}
	}
	for i := range msgs {
		for j := range msgs[i].Content {
			b := msgs[i].Content[j]
			switch b.Kind {
			case llm.BlockToolResult:
				consider(i, j, -1, len(b.ResultText), b.Kind)
				for child, nested := range b.ResultContent {
					if nested.Kind == llm.BlockImage {
						// Rank nested and top-level images by model token weight, not
						// base64 byte length, so larger text is truncated first (r22).
						consider(i, j, child, imageTokenEstimate*bytesPerToken, llm.BlockImage)
					}
				}
			case llm.BlockToolUse:
				consider(i, j, -1, len(b.ToolInput), b.Kind)
			case llm.BlockImage:
				consider(i, j, -1, imageTokenEstimate*bytesPerToken, b.Kind)
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
		if childIndex >= 0 {
			return degradeToolResultImageAt(&msgs[bi].Content[bj], childIndex)
		}
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
			blockBytes, blockImages := estimateTranscriptContentBlock(b)
			bytes += blockBytes
			images += blockImages
		}
	}
	return bytes/bytesPerToken + images*imageTokenEstimate
}

func estimateTranscriptContentBlock(b llm.ContentBlock) (bytes, images int) {
	if b.Kind == llm.BlockImage {
		return len(b.ImageMediaType) + len(b.ImageDetail) + len(b.ImageName), 1
	}
	bytes = len(b.Text) + len(b.ResultText) + len(b.ToolInput) + len(b.ToolName)
	bytes += len(b.ReasoningID) + len(b.ReasoningEncrypted) + len(b.RedactedData) + len(b.ThinkingSignature)
	bytes += len(b.InteractionThoughtSummary) + len(b.InteractionThoughtSignature) + len(b.InteractionStep)
	for _, child := range b.ResultContent {
		childBytes, childImages := estimateTranscriptContentBlock(child)
		bytes += childBytes
		images += childImages
	}
	return bytes, images
}

func estimateRequest(req llm.Request, window int) ContextEstimate {
	systemBytes := len(req.System)
	toolBytes := 0
	for _, t := range req.Tools {
		toolBytes += len(t.Name) + len(t.Description) + len(t.Parameters)
	}
	for _, t := range req.ServerTools {
		toolBytes += len(t.Name) + len(t.Kind) + len(t.Parameters)
	}
	messageBytes := 0
	images := 0
	for _, m := range req.Messages {
		messageBytes += len(m.Role)
		for _, b := range m.Content {
			blockBytes, blockImages := estimateRequestContentBlock(b)
			messageBytes += blockBytes
			images += blockImages
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

func estimateRequestContentBlock(b llm.ContentBlock) (bytes, images int) {
	if b.Kind == llm.BlockImage {
		return len(b.Kind) + len(b.ImageMediaType) + len(b.ImageDetail) + len(b.ImageName), 1
	}
	bytes = len(b.Kind) + len(b.Text) + len(b.ToolUseID) + len(b.ToolName) + len(b.ToolInput) +
		len(b.ResultForID) + len(b.ResultText)
	bytes += len(b.ReasoningID) + len(b.ReasoningEncrypted) + len(b.RedactedData) + len(b.ThinkingSignature)
	bytes += len(b.InteractionThoughtSummary) + len(b.InteractionThoughtSignature) + len(b.InteractionStep)
	for _, child := range b.ResultContent {
		childBytes, childImages := estimateRequestContentBlock(child)
		bytes += childBytes
		images += childImages
	}
	return bytes, images
}

// compactionReport describes semantic turn reclamation and the resulting full
// request footprint.
func compactionReport(collapsed, before, after int) string {
	return fmt.Sprintf("[compacted: %d turns → checkpoint · ctx ~%s → ~%s]",
		collapsed, kiloTokens(before), kiloTokens(after))
}

// kiloTokens renders a token count in thousands with one decimal, matching the
// design's compaction report (9100 -> "9.1k", 400 -> "0.4k").
func kiloTokens(n int) string {
	return fmt.Sprintf("%.1fk", float64(n)/1000)
}
