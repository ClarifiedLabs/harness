package agent

// Fixed notice texts emitted through EventSink.Notice. The delegate activity
// feed allows only a fixed set of notices plus patterned ones
// (internal/delegate safeFixedNotices / safeNoticePatterns); the constants
// below are the single source of truth shared with that allowlist so a
// reworded notice cannot silently stop matching.
const (
	// NoticeCancelled reports a prompt interrupted before completion.
	NoticeCancelled = "[cancelled]"
	// NoticeStoppedMaxTokens reports a provider stop for reaching the output token limit.
	NoticeStoppedMaxTokens = "[stopped: model reached max tokens]"
	// NoticeStoppedStopSequence reports a provider stop on a stop sequence.
	NoticeStoppedStopSequence = "[stopped: stop sequence matched]"
	// NoticeContextOverflowCompacting reports an input-overflow retry after compaction.
	NoticeContextOverflowCompacting = "[context overflow: compacting and retrying request]"
	// NoticeResponsesStateDisabledRejected reports disabling stored responses
	// after the provider rejected them.
	NoticeResponsesStateDisabledRejected = "[responses state disabled: provider rejected stored responses; retrying stateless]"
	// NoticeResponsesStateResetUnavailable reports falling back to full
	// context after a stored previous response became unavailable.
	NoticeResponsesStateResetUnavailable = "[responses state reset: previous response unavailable; retrying with full context]"
	// NoticeReasoningReplayDisabled reports disabling reasoning replay after
	// the provider rejected encrypted reasoning content.
	NoticeReasoningReplayDisabled = "[reasoning replay disabled: provider rejected encrypted content; retrying without opaque reasoning]"
	// NoticeCompactNothingToShrink reports a compaction trigger with nothing
	// left to remove from the transcript.
	NoticeCompactNothingToShrink = "[compact: transcript over budget but nothing left to shrink]"
)
