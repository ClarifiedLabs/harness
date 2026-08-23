package agent

import (
	"fmt"

	"harness/internal/llm"
)

// Intra-turn call sanity guards (design §8.1). The turn-level turnGuard and the
// per-call failureGuard both react only after damage is observable across
// turns or failures; neither sees a single degenerate response that emits
// hundreds of duplicate or excessive tool calls at once. Session audits show
// models occasionally degenerate into emitting the exact same call hundreds of
// times in one turn (a repetition loop stopped only by the output-token cap),
// and unrestricted dispatch of such a batch exhausts the machine's process
// table. These guards bound what one turn may dispatch before any side effect:
// exact duplicates within a stage are suppressed, and dispatches are capped.
const (
	// maxDispatchedCallsPerTurn bounds how many calls one turn may actually
	// execute. Calls beyond the limit are not run: they return guard errors so
	// the transcript stays closed and the model can re-issue surviving work in
	// a later turn. Legitimate turns batch a handful of calls; 128 is generous.
	maxDispatchedCallsPerTurn = 128
	// maxConcurrentToolRuns bounds how many tool executions one agent runs at
	// once. Default-parallel dispatch starts one worker per call; without a
	// bound a large batch forks hundreds of processes simultaneously and
	// starves the machine (fork EAGAIN). The bound is per-agent so a delegate
	// child can never deadlock against permits held by its parent's dispatch.
	maxConcurrentToolRuns = 32
)

// planCallSuppression marks calls that must not run: exact duplicates within
// one stage beyond the first occurrence, and surviving calls beyond the
// per-turn dispatch limit. The returned map keys are global call indexes and
// the values are the model-visible guard error text. Calls that never dispatch
// (malformed streamed input, hosted echo calls) are left to their existing
// synthesized-result paths and consume neither the duplicate identity nor the
// dispatch budget. Suppression is stage-scoped for duplicates because an
// identical call re-issued in a later stage is a legitimate re-run after
// mutations; within one stage, identical calls run concurrently and can only
// multiply load.
func planCallSuppression(calls []llm.ToolCall, stages []callStage) (map[int]string, int, int) {
	suppressed := make(map[int]string)
	dispatched := 0
	duplicates := 0
	overLimit := 0
	for _, stage := range stages {
		seen := make(map[failKey]struct{})
		for i := stage.start; i < stage.end; i++ {
			call := calls[i]
			if call.InvalidInputError != "" || call.Name == kimiWebSearchToolName {
				continue
			}
			key := failKey{name: call.Name, inputHash: llm.NormalizedToolCallHash(call.Input)}
			if _, dup := seen[key]; dup {
				suppressed[i] = duplicateCallText(call)
				duplicates++
				continue
			}
			seen[key] = struct{}{}
			dispatched++
			if dispatched > maxDispatchedCallsPerTurn {
				suppressed[i] = overLimitCallText(call)
				overLimit++
			}
		}
	}
	return suppressed, duplicates, overLimit
}

// duplicateCallText is the whole model-visible error for a suppressed
// duplicate; it stands alone because the call never ran.
func duplicateCallText(call llm.ToolCall) string {
	return fmt.Sprintf("[loop guard] blocked: this exact call%s duplicates an identical call earlier in this stage, so it was not run. Calls within one stage run concurrently, so identical duplicates only multiply load. To repeat a check, use one command with an explicit loop or count flag (for example go test -count=N), or re-issue the call in a later stage or turn.",
		callTarget(call))
}

// overLimitCallText is the whole model-visible error for a call suppressed by
// the per-turn dispatch limit.
func overLimitCallText(call llm.ToolCall) string {
	return fmt.Sprintf("[loop guard] blocked: this turn already reached the %d-call dispatch limit, so this call%s was not run. Split the work across turns: dispatch the highest-priority calls first, read their results, then continue in the next turn.",
		maxDispatchedCallsPerTurn, callTarget(call))
}

// suppressionNotice is the one-line audit notice emitted when a turn's batch
// tripped the sanity guards. The delegate activity feed allowlists it via
// safeNoticePatterns.
func suppressionNotice(duplicates, overLimit int) string {
	return fmt.Sprintf("[loop guard] suppressed %d duplicate and %d over-limit tool calls this turn (dispatch limit %d); suppressed calls returned guard errors without running",
		duplicates, overLimit, maxDispatchedCallsPerTurn)
}
