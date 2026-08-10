package agent

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"strings"
	"sync"

	"harness/internal/llm"
	"harness/internal/tools"
)

// Repeated-identical-failure guard (design §8.1). Session audits show most
// repeated tool errors are the model re-issuing the exact same failing call
// unchanged. Unlike the turn-level turnGuard, this guard watches individual
// calls and hard-blocks them before dispatch. The error text is part of the
// identity, so a fix-and-rerun loop whose failures differ (go build → fix →
// go build) never trips it.
const (
	// identicalFailWarnAt is the consecutive identical-failure count at which
	// the error result gains a steering hint.
	identicalFailWarnAt = 2
	// identicalFailBlockAt is the attempt number at which the call is
	// hard-blocked without dispatching: the warn at identicalFailWarnAt was
	// already ignored once.
	identicalFailBlockAt = 3
)

// failKey identifies one logical tool call: tool name plus the normalized
// (key-order-insensitive) input hash.
type failKey struct {
	name      string
	inputHash string
}

// failRecord tracks the last error hash and consecutive identical-failure
// count for one failKey.
type failRecord struct {
	errorHash string
	attempts  int
}

// failureGuard is the per-prompt repeated-identical-failure state. It lives on
// the Agent only while RunAdmittedPromptWithContext executes, so a user
// re-prompting "run the tests again" always starts fresh. The mutex is
// required because default-parallel scheduling dispatches calls concurrently.
type failureGuard struct {
	mu      sync.Mutex
	records map[failKey]failRecord
}

func newFailureGuard() *failureGuard {
	return &failureGuard{records: make(map[failKey]failRecord)}
}

// beforeCall reports whether the call must be blocked instead of dispatched,
// returning the pre-baked error result. Blocked attempts are deliberately not
// recorded: they are not dispatches and must not grow the streak.
func (g *failureGuard) beforeCall(call llm.ToolCall) (llm.ToolResult, bool) {
	key := failKey{name: call.Name, inputHash: llm.NormalizedToolCallHash(call.Input)}
	g.mu.Lock()
	defer g.mu.Unlock()
	rec := g.records[key]
	if rec.attempts < identicalFailBlockAt-1 {
		return llm.ToolResult{}, false
	}
	return llm.ToolResult{
		ForID:     call.ID,
		Text:      identicalFailureBlockText(call, rec.attempts),
		IsError:   true,
		ErrorKind: llm.ToolErrorBlocked,
	}, true
}

// afterCall folds one dispatch result into the guard. A successful call that
// reports mutated paths resets the whole map: an edit or write is a strategy
// change, so re-running the same failing check afterward is legitimate.
// Read-only successes deliberately do not reset. An error increments the
// streak when its text matches the previous failure and gains a steering hint
// at the warn threshold.
func (g *failureGuard) afterCall(reg *tools.Registry, call llm.ToolCall, res llm.ToolResult) llm.ToolResult {
	if !res.IsError {
		if _, mutated := reg.MutatedPaths(call); mutated {
			g.mu.Lock()
			g.records = make(map[failKey]failRecord)
			g.mu.Unlock()
		}
		return res
	}
	key := failKey{name: call.Name, inputHash: llm.NormalizedToolCallHash(call.Input)}
	errHash := fmt.Sprintf("%x", sha256.Sum256([]byte(res.Text)))
	g.mu.Lock()
	defer g.mu.Unlock()
	rec := g.records[key]
	if rec.errorHash == errHash {
		rec.attempts++
	} else {
		rec = failRecord{errorHash: errHash, attempts: 1}
	}
	g.records[key] = rec
	if rec.attempts == identicalFailWarnAt {
		res.Text += identicalFailureWarnText(call, rec.attempts)
	}
	return res
}

// identicalFailureWarnText is appended to the model-visible error when the
// same call fails the same way for the identicalFailWarnAt-th time.
func identicalFailureWarnText(call llm.ToolCall, attempts int) string {
	return fmt.Sprintf("\n[loop guard] this exact call%s has now failed %d times with the identical error. Do not re-issue it unchanged: re-read the file, verify the path exists, or change your approach — a third identical attempt will be blocked before it runs.",
		callTarget(call), attempts)
}

// identicalFailureBlockText is the whole model-visible error for a blocked
// call; it stands alone because the tool never ran.
func identicalFailureBlockText(call llm.ToolCall, attempts int) string {
	return fmt.Sprintf("[loop guard] blocked: this exact call%s already failed %d times with the identical error, so it was not run. Do not re-issue it: re-read the file, verify the path exists, change your approach, or stop and report the blocker.",
		callTarget(call), attempts)
}

// callTarget extracts a short human identifier (path, command, pattern, or
// task) from a call's input so guard messages name the offending file or
// command. Unknown shapes stay unnamed.
func callTarget(call llm.ToolCall) string {
	var args struct {
		Path    string   `json:"path"`
		Command string   `json:"command"`
		Argv    []string `json:"argv"`
		Pattern string   `json:"pattern"`
		Task    string   `json:"task"`
	}
	if json.Unmarshal(call.Input, &args) != nil {
		return ""
	}
	target := args.Path
	if target == "" {
		target = args.Command
	}
	if target == "" {
		target = strings.Join(args.Argv, " ")
	}
	if target == "" {
		target = args.Pattern
	}
	if target == "" {
		target = args.Task
	}
	target = strings.TrimSpace(target)
	if target == "" {
		return ""
	}
	const maxTarget = 120
	if len(target) > maxTarget {
		target = target[:maxTarget] + "…"
	}
	return " (" + target + ")"
}
