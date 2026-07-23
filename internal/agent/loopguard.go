package agent

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"harness/internal/llm"
)

// Runaway guardrails (design §8.1). All detection state lives in the per-run
// turnGuard frame — never on the stateless, concurrently shared tools.Registry —
// so it is race-free and a legitimate distant re-read in a later turn is never
// penalized.
const (
	// repeatSteerThreshold is how many turns in a row must produce an
	// identical (tool calls + results) signature before one steering nudge is
	// injected. Results are part of the signature so legitimate polling or test
	// re-runs whose output changes never trip the guard.
	repeatSteerThreshold = 3
	// repeatBreak is the hard-stop threshold for a byte-identical successful
	// repeat: after one steer (repeatSteerThreshold) the model has been warned, so
	// a run that keeps re-issuing the exact same calls with the exact same results
	// is finalized rather than left to burn turns/tokens — the success-loop
	// analogue of errorStormBreak.
	repeatBreak = 8
	// errorStormSteer / errorStormBreak count consecutive turns whose tool
	// results were all errors: steer once at the first, hard-stop at the second.
	errorStormSteer = 5
	errorStormBreak = 10
)

const repeatSteer = "[loop guard] The last several tool calls repeated with identical results. Stop repeating them: change your approach, try different inputs or another tool, or stop and report the blocker. Do not re-issue the same calls expecting a different outcome."

const errorStormSteerMsg = "[loop guard] Several consecutive tool calls have all failed. Re-read the latest error output and change your approach, or stop and report what is blocking you — do not keep retrying the same way."

const wrapUpSteer = "[turn budget] You are about to reach this prompt's turn limit. Stop calling tools now and reply with a final message: summarize what you completed, what remains, and any next steps."

// turnGuard is the per-run runaway-protection state. The zero value is ready to
// use; one is created per RunPrompt call and discarded when the turn ends.
type turnGuard struct {
	lastCallSig   string // signature of the previous turn's calls+results
	repeatRuns    int    // consecutive turns with that identical signature
	repeatSteered bool   // steering already injected for the current repeat streak
	errorRuns     int    // consecutive turns whose tool results were all errors
	errorSteered  bool   // steering already injected for the current error streak
	wrapUpSteered bool   // one-shot maxTurns wrap-up steering injected
}

// recordTools folds one turn's tool calls and results into the guard's
// sliding window. calls and results are positionally aligned (results[i] answers
// calls[i]).
func (g *turnGuard) recordTools(calls []llm.ToolCall, results []llm.ContentBlock) {
	sig := callSetSignature(calls, results)
	if sig != "" && sig == g.lastCallSig {
		g.repeatRuns++
	} else {
		g.lastCallSig = sig
		g.repeatRuns = 1
		g.repeatSteered = false
	}
	if allErrors(results) {
		g.errorRuns++
	} else {
		g.errorRuns = 0
		g.errorSteered = false
	}
}

// steerMessage returns the single steering nudge to inject this turn, or
// "" when none is due. Repetition and the error storm share one slot so a turn
// that is both repeating and erroring is nudged once, not twice.
func (g *turnGuard) steerMessage() string {
	if g.repeatRuns >= repeatSteerThreshold && !g.repeatSteered {
		g.repeatSteered = true
		return repeatSteer
	}
	if g.errorRuns >= errorStormSteer && g.errorRuns < errorStormBreak && !g.errorSteered {
		g.errorSteered = true
		return errorStormSteerMsg
	}
	return ""
}

// shouldBreakErrors reports whether the error storm has reached the hard stop.
func (g *turnGuard) shouldBreakErrors() bool { return g.errorRuns >= errorStormBreak }

// shouldBreakRepeat reports whether a byte-identical successful repeat has
// reached the hard stop, mirroring shouldBreakErrors. The signature includes
// tool results, so only genuinely stuck loops (same calls, same output) ever
// reach it; a call whose output changes resets the streak.
func (g *turnGuard) shouldBreakRepeat() bool { return g.repeatRuns >= repeatBreak }

// callSetSignature builds an order-insensitive signature of a turn's tool
// calls and their results: per call, the tool name + canonicalized (sorted-key)
// JSON of its input + the result's error flag and text. Including the result
// keeps the guard conservative — identical calls that return different output
// (polling, a now-passing test) produce different signatures and never trip it.
func callSetSignature(calls []llm.ToolCall, results []llm.ContentBlock) string {
	if len(calls) == 0 {
		return ""
	}
	sigs := make([]string, len(calls))
	for i, c := range calls {
		var res string
		if i < len(results) && results[i].Kind == llm.BlockToolResult {
			flag := "ok"
			if results[i].ResultError {
				flag = "err"
			}
			res = flag + "\x00" + results[i].ResultText
			if len(results[i].ResultContent) > 0 {
				res += "\x00" + resultContentSignature(results[i].ResultContent)
			}
		}
		sigs[i] = c.Name + "\x00" + canonicalJSON(c.Input) + "\x00" + res
	}
	sort.Strings(sigs)
	return strings.Join(sigs, "\x01")
}

// resultContentSignature captures every model-visible image attribute that can
// make a rich tool result meaningfully different. The payload itself is
// represented only by a SHA-256 digest, keeping signatures bounded and ensuring
// base64 never appears in guard diagnostics or retained state.
func resultContentSignature(content []llm.ContentBlock) string {
	type imageSignature struct {
		Kind      llm.BlockKind `json:"kind"`
		MediaType string        `json:"media_type,omitempty"`
		Detail    string        `json:"detail,omitempty"`
		Width     int           `json:"width,omitempty"`
		Height    int           `json:"height,omitempty"`
		Digest    string        `json:"sha256"`
	}
	images := make([]imageSignature, 0, len(content))
	for _, child := range content {
		digest := sha256.Sum256([]byte(child.ImageData))
		images = append(images, imageSignature{
			Kind:      child.Kind,
			MediaType: child.ImageMediaType,
			Detail:    child.ImageDetail,
			Width:     child.ImageWidth,
			Height:    child.ImageHeight,
			Digest:    fmt.Sprintf("%x", digest),
		})
	}
	encoded, _ := json.Marshal(images)
	return string(encoded)
}

// canonicalJSON renders raw with object keys sorted (json.Marshal sorts map
// keys) so semantically identical inputs that differ only in key order compare
// equal; array order is preserved because argument order is significant.
func canonicalJSON(raw []byte) string {
	if len(raw) == 0 {
		return ""
	}
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return string(raw)
	}
	b, err := json.Marshal(v)
	if err != nil {
		return string(raw)
	}
	return string(b)
}

// allErrors reports whether a tool-results block list carries at least one
// result and every result is an error.
func allErrors(results []llm.ContentBlock) bool {
	sawResult := false
	for _, b := range results {
		if b.Kind != llm.BlockToolResult {
			continue
		}
		sawResult = true
		if !b.ResultError {
			return false
		}
	}
	return sawResult
}

// totalTokens is the cumulative token throughput of a usage accumulator —
// input (incl. cache) + output + reasoning — used to enforce the per-prompt token
// budget (design §8.1, r7).
func totalTokens(u llm.Usage) int {
	return u.InputTokens + u.CacheReadTokens + u.CacheWriteTokens + u.OutputTokens + u.ReasoningTokens
}

// errorStormNotice is the hard-stop notice for an unrelenting error storm,
// mirroring maxTurnsNotice's shape.
func errorStormNotice(n int) string {
	return fmt.Sprintf("[stopped: %d consecutive tool turns all failed]", n)
}

// repeatLoopNotice is the hard-stop notice for a byte-identical successful
// repeat loop, mirroring errorStormNotice's shape.
func repeatLoopNotice(n int) string {
	return fmt.Sprintf("[stopped: %d identical tool turns repeated with no change]", n)
}

// promptTokenBudgetNotice is the hard-stop notice when the per-prompt token budget
// is exhausted.
func promptTokenBudgetNotice(budget int) string {
	return fmt.Sprintf("[stopped: prompt token budget %d exceeded]", budget)
}

// promptCostBudgetNotice is the hard-stop notice when the per-prompt cost budget
// (USD) is exhausted, mirroring promptTokenBudgetNotice.
func promptCostBudgetNotice(budgetUSD, spentUSD float64) string {
	return fmt.Sprintf("[stopped: prompt cost budget $%.2f reached ($%.2f spent)]", budgetUSD, spentUSD)
}
