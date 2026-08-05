package agent

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"harness/internal/llm"
	"harness/internal/tools"
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
	// commandRepeatSteer / commandRepeatBreak catch a shell-command loop whose
	// JSON input keeps changing only because the model rewrites the downstream
	// pipeline. This is deliberately slower than the exact-repeat guard because
	// changing output filters can be legitimate short-lived investigation.
	commandRepeatSteer = 4
	commandRepeatBreak = 12
	// errorStormSteer / errorStormBreak count consecutive turns whose tool
	// results were all errors: steer once at the first, hard-stop at the second.
	errorStormSteer           = 5
	errorStormBreak           = 10
	orientationSteerThreshold = 3
	semanticSteerThreshold    = 12
	maxEvidenceSignatures     = 256
)

const repeatSteer = "[loop guard] The last several tool calls repeated with identical results. Stop repeating them: change your approach, try different inputs or another tool, or stop and report the blocker. Do not re-issue the same calls expecting a different outcome."

const commandRepeatSteerMsg = "[loop guard] The last several tool turns ran the same underlying shell command while changing only its output pipeline. Stop re-running it: use the evidence already collected, inspect or change the relevant code, or report the blocker. If repeated sampling is genuinely needed, batch it in one command."

const errorStormSteerMsg = "[loop guard] Several consecutive tool calls have all failed. Re-read the latest error output and change your approach, or stop and report what is blocking you — do not keep retrying the same way."

const orientationSteer = "[efficiency] The last several turns each performed one repository lookup. Coissue independent read_file, search, glob, or list_dir calls in one turn; use read_file paths[] when the files are already known."

const semanticProgressSteer = "[progress] The recent turns have remained in inspection without explicit progress. Synthesize the evidence, take the next concrete action appropriate to the task, validate the current result, or report the blocker."

const wrapUpSteer = "[turn budget closure] The prompt is approaching its turn limit. Stop broad exploration. Use tools only for necessary artifact, mutation, validation, submission, or escalation actions, then report the resulting state, remaining requirements, and blockers."

// turnGuard is the per-run runaway-protection state. The zero value is ready to
// use; one is created per RunPrompt call and discarded when the turn ends.
type turnGuard struct {
	lastCallSig              string // signature of the previous turn's calls+results
	repeatRuns               int    // consecutive turns with that identical signature
	repeatSteered            bool   // steering already injected for the current repeat streak
	lastCommandSig           string // signature of the previous run_command pipeline head
	commandRuns              int    // consecutive turns with that underlying shell command
	commandSteered           bool   // steering already injected for the current command streak
	errorRuns                int    // consecutive turns whose tool results were all errors
	errorSteered             bool   // steering already injected for the current error streak
	orientationRuns          int    // consecutive turns with one unbatched orientation lookup
	orientationSteered       bool
	semanticRuns             int // consecutive inspection-only turns without explicit progress
	semanticSteered          bool
	evidence                 boundedEvidence
	turnBudgetClosureSteered bool // one-shot early turn-budget closure steering injected
}

// GuardSteerReason is the bounded diagnostic reason for an injected guard nudge.
type GuardSteerReason string

const (
	GuardSteerRepeat          GuardSteerReason = "repeat"
	GuardSteerCommandRepeat   GuardSteerReason = "command_repeat"
	GuardSteerBatching        GuardSteerReason = "batching"
	GuardSteerPhaseTransition GuardSteerReason = "phase_transition"
	GuardSteerErrorStorm      GuardSteerReason = "error_storm"
)

// ToolActivityCounts reports operation counts rather than just outer tool-call
// counts, so a batched read_file/steps call remains visible as one model action.
type ToolActivityCounts struct {
	Inspect    int
	Mutate     int
	Verify     int
	Wait       int
	Coordinate int
	Other      int
}

// TurnProgress is diagnostics-only. It is emitted after a tool turn and never
// enters model history or influences tool dispatch, permissions, or hard stops.
type TurnProgress struct {
	Turn                    int
	ToolCalls               int
	Operations              int
	Activity                ToolActivityCounts
	ErrorCount              int
	BatchedOperationCount   int
	SingleLookupCount       int
	InspectionOnly          bool
	NoExplicitProgress      bool
	ExplicitProgress        bool
	SuccessfulMutation      bool
	VerificationAttempt     bool
	SuccessfulVerification  bool
	SuccessfulWait          bool
	SuccessfulCoordination  bool
	NewEvidence             bool
	NewEvidenceCount        int
	UserSteer               bool
	RepeatStreak            int
	CommandRepeatStreak     int
	ErrorStreak             int
	SingleLookupStreak      int
	InspectionNoProgressRun int
	SteerReason             GuardSteerReason
}

type boundedEvidence struct {
	seen  map[[sha256.Size]byte]struct{}
	order [][sha256.Size]byte
}

// aggregateTurnProgress classifies tool activity and detects bounded new result
// evidence. Classification is observational only: dispatch already completed.
func (g *turnGuard) aggregateTurnProgress(registry *tools.Registry, turn int, calls []llm.ToolCall, results []llm.ContentBlock) TurnProgress {
	progress := TurnProgress{Turn: turn, ToolCalls: len(calls), InspectionOnly: len(calls) > 0}
	for i, call := range calls {
		activity := registry.CallActivity(call)
		operations := activity.OperationCount
		progress.Operations += operations
		switch activity.Class {
		case tools.ActivityInspect:
			progress.Activity.Inspect += operations
			if operations == 1 && !activity.Batched {
				progress.SingleLookupCount++
			}
		case tools.ActivityMutate:
			progress.Activity.Mutate += operations
			progress.InspectionOnly = false
		case tools.ActivityVerify:
			progress.Activity.Verify += operations
			progress.InspectionOnly = false
		case tools.ActivityWait:
			progress.Activity.Wait += operations
			progress.InspectionOnly = false
		case tools.ActivityCoordinate:
			progress.Activity.Coordinate += operations
			progress.InspectionOnly = false
		default:
			progress.Activity.Other += operations
			progress.InspectionOnly = false
		}
		if activity.Batched {
			progress.BatchedOperationCount += operations
		}

		failed := i >= len(results) || results[i].Kind != llm.BlockToolResult || results[i].ResultError
		if failed {
			progress.ErrorCount++
		}
		switch activity.Class {
		case tools.ActivityMutate:
			progress.SuccessfulMutation = progress.SuccessfulMutation || !failed
		case tools.ActivityVerify:
			progress.VerificationAttempt = true
			progress.SuccessfulVerification = progress.SuccessfulVerification || !failed
		case tools.ActivityWait:
			progress.SuccessfulWait = progress.SuccessfulWait || !failed
		case tools.ActivityCoordinate:
			progress.SuccessfulCoordination = progress.SuccessfulCoordination || !failed && activity.ExplicitProgress
		}
		if i < len(results) && results[i].Kind == llm.BlockToolResult && g.evidence.add(toolResultEvidence(results[i])) {
			progress.NewEvidenceCount++
		}
	}
	progress.NewEvidence = progress.NewEvidenceCount > 0
	progress.ExplicitProgress = progress.SuccessfulMutation || progress.VerificationAttempt || progress.SuccessfulWait || progress.SuccessfulCoordination
	progress.NoExplicitProgress = !progress.ExplicitProgress
	return progress
}

// recordTurn folds one completed tool turn into exact-repeat/error hard-stop
// state and advisory progress state. The former calculations are unchanged.
func (g *turnGuard) recordTurn(calls []llm.ToolCall, results []llm.ContentBlock, progress *TurnProgress) {
	sig := callSetSignature(calls, results)
	if sig != "" && sig == g.lastCallSig {
		g.repeatRuns++
	} else {
		g.lastCallSig = sig
		g.repeatRuns = 1
		g.repeatSteered = false
	}
	commandSig := commandPipelineSignature(calls)
	if commandSig != "" && commandSig == g.lastCommandSig {
		g.commandRuns++
	} else {
		g.lastCommandSig = commandSig
		g.commandRuns = 0
		if commandSig != "" {
			g.commandRuns = 1
		}
		g.commandSteered = false
	}
	if allErrors(results) {
		g.errorRuns++
	} else {
		g.errorRuns = 0
		g.errorSteered = false
	}
	if progress.SingleLookupCount == 1 && progress.ToolCalls == 1 {
		g.orientationRuns++
	} else {
		g.orientationRuns = 0
		g.orientationSteered = false
	}
	if progress.InspectionOnly && progress.NoExplicitProgress {
		g.semanticRuns++
	} else {
		g.semanticRuns = 0
		g.semanticSteered = false
	}
	g.snapshotStreaks(progress)
}

// recordTools retains the small legacy test helper while production passes the
// richer aggregate through recordTurn.
func (g *turnGuard) recordTools(calls []llm.ToolCall, results []llm.ContentBlock) {
	progress := TurnProgress{ToolCalls: len(calls), InspectionOnly: len(calls) > 0, NoExplicitProgress: true}
	if isSingleOrientationTurn(calls) {
		progress.SingleLookupCount = 1
	}
	g.recordTurn(calls, results, &progress)
}

func (g *turnGuard) resetForUserSteer(progress *TurnProgress) {
	g.repeatRuns = 0
	g.repeatSteered = false
	g.lastCommandSig = ""
	g.commandRuns = 0
	g.commandSteered = false
	g.errorRuns = 0
	g.errorSteered = false
	g.orientationRuns = 0
	g.orientationSteered = false
	g.semanticRuns = 0
	g.semanticSteered = false
	progress.UserSteer = true
	g.snapshotStreaks(progress)
}

func (g *turnGuard) snapshotStreaks(progress *TurnProgress) {
	progress.RepeatStreak = g.repeatRuns
	progress.CommandRepeatStreak = g.commandRuns
	progress.ErrorStreak = g.errorRuns
	progress.SingleLookupStreak = g.orientationRuns
	progress.InspectionNoProgressRun = g.semanticRuns
}

// nextSteer returns one typed advisory reason and message. Semantic stagnation
// has no break condition and cannot terminate a run.
func (g *turnGuard) nextSteer() (GuardSteerReason, string) {
	if g.repeatRuns >= repeatSteerThreshold && !g.repeatSteered {
		g.repeatSteered = true
		return GuardSteerRepeat, repeatSteer
	}
	if g.commandRuns >= commandRepeatSteer && !g.commandSteered && g.repeatRuns < repeatSteerThreshold {
		g.commandSteered = true
		return GuardSteerCommandRepeat, commandRepeatSteerMsg
	}
	if g.orientationRuns >= orientationSteerThreshold && !g.orientationSteered {
		g.orientationSteered = true
		return GuardSteerBatching, orientationSteer
	}
	if g.semanticRuns >= semanticSteerThreshold && !g.semanticSteered {
		g.semanticSteered = true
		return GuardSteerPhaseTransition, semanticProgressSteer
	}
	if g.errorRuns >= errorStormSteer && g.errorRuns < errorStormBreak && !g.errorSteered {
		g.errorSteered = true
		return GuardSteerErrorStorm, errorStormSteerMsg
	}
	return "", ""
}

// steerMessage is retained for focused legacy tests.
func (g *turnGuard) steerMessage() string {
	_, message := g.nextSteer()
	return message
}

func (b *boundedEvidence) add(signature [sha256.Size]byte) bool {
	if b.seen == nil {
		b.seen = make(map[[sha256.Size]byte]struct{}, maxEvidenceSignatures)
	}
	if _, ok := b.seen[signature]; ok {
		return false
	}
	if len(b.order) == maxEvidenceSignatures {
		delete(b.seen, b.order[0])
		copy(b.order, b.order[1:])
		b.order = b.order[:len(b.order)-1]
	}
	b.seen[signature] = struct{}{}
	b.order = append(b.order, signature)
	return true
}

func toolResultEvidence(result llm.ContentBlock) [sha256.Size]byte {
	h := sha256.New()
	_, _ = h.Write([]byte(result.ToolName))
	_, _ = h.Write([]byte{0})
	_, _ = h.Write([]byte(strconv.FormatBool(result.ResultError)))
	_, _ = h.Write([]byte{0})
	_, _ = h.Write([]byte(result.ResultText))
	for _, child := range result.ResultContent {
		_, _ = h.Write([]byte{0})
		_, _ = h.Write([]byte(child.Kind))
		_, _ = h.Write([]byte{0})
		_, _ = h.Write([]byte(child.ImageMediaType))
		_, _ = h.Write([]byte{0})
		_, _ = h.Write([]byte(child.ImageDetail))
		_, _ = h.Write([]byte{0})
		_, _ = h.Write([]byte(strconv.Itoa(child.ImageWidth)))
		_, _ = h.Write([]byte{0})
		_, _ = h.Write([]byte(strconv.Itoa(child.ImageHeight)))
		_, _ = h.Write([]byte{0})
		_, _ = h.Write([]byte(child.ImageData))
	}
	var signature [sha256.Size]byte
	copy(signature[:], h.Sum(nil))
	return signature
}

func isSingleOrientationTurn(calls []llm.ToolCall) bool {
	if len(calls) != 1 {
		return false
	}
	call := calls[0]
	switch call.Name {
	case "glob", "list_dir", "git_readonly":
		return true
	case "read_file":
		var args struct {
			Paths []string `json:"paths"`
		}
		_ = json.Unmarshal(call.Input, &args)
		return len(args.Paths) < 2
	case "search":
		return true
	default:
		return false
	}
}

// shouldBreakErrors reports whether the error storm has reached the hard stop.
func (g *turnGuard) shouldBreakErrors() bool { return g.errorRuns >= errorStormBreak }

// shouldBreakRepeat reports whether a byte-identical successful repeat has
// reached the hard stop, mirroring shouldBreakErrors. The signature includes
// tool results, so only genuinely stuck loops (same calls, same output) ever
// reach it; a call whose output changes resets the streak.
func (g *turnGuard) shouldBreakRepeat() bool { return g.repeatRuns >= repeatBreak }

// shouldBreakCommandRepeat reports whether the model ignored command-family
// steering and kept rewriting only the output pipeline around the same shell
// command. Its higher threshold keeps this fallback more conservative than an
// exact byte-for-byte repeat.
func (g *turnGuard) shouldBreakCommandRepeat() bool { return g.commandRuns >= commandRepeatBreak }

func turnBudgetClosureReserve(maxTurns int) int {
	if maxTurns <= 0 {
		return 0
	}
	if maxTurns <= 2 {
		return 1
	}
	reserve := maxTurns / 10
	if maxTurns%10 != 0 {
		reserve++
	}
	if reserve < 2 {
		reserve = 2
	}
	if reserve > 25 {
		reserve = 25
	}
	if reserve > maxTurns {
		reserve = maxTurns
	}
	return reserve
}

func shouldEnterTurnBudgetClosure(maxTurns, completedTurns int) bool {
	if maxTurns <= 0 {
		return false
	}
	return completedTurns >= maxTurns-turnBudgetClosureReserve(maxTurns)
}

// commandPipelineSignature identifies a narrowly defined command family: one
// foreground run_command shell invocation whose first unquoted pipeline segment
// is unchanged. It intentionally ignores only downstream pipe stages, not flags,
// argv calls, multi-step batches, working directories, or unrelated tool turns.
// This catches loops that keep appending grep/sed/awk wrappers without treating a
// fix-and-rerun command with changed arguments as the same attempt.
func commandPipelineSignature(calls []llm.ToolCall) string {
	if len(calls) != 1 || calls[0].Name != "run_command" {
		return ""
	}
	var input struct {
		Command    string `json:"command"`
		Cwd        string `json:"cwd"`
		Background bool   `json:"background"`
		Steps      []struct {
			Command string `json:"command"`
			Cwd     string `json:"cwd"`
		} `json:"steps"`
	}
	if err := json.Unmarshal(calls[0].Input, &input); err != nil || input.Background {
		return ""
	}
	command, cwd := input.Command, input.Cwd
	if len(input.Steps) > 0 {
		if len(input.Steps) != 1 {
			return ""
		}
		command = input.Steps[0].Command
		if input.Steps[0].Cwd != "" {
			cwd = input.Steps[0].Cwd
		}
	}
	head, piped := shellPipelineHead(command)
	if !piped || head == "" {
		return ""
	}
	digest := sha256.Sum256([]byte(cwd + "\x00" + head))
	return fmt.Sprintf("%x", digest)
}

// shellPipelineHead returns the text before the first unquoted shell pipe.
// Backslash and single/double quote tracking is intentionally small but enough
// to avoid mistaking regex and quoted data pipes for shell operators.
func shellPipelineHead(command string) (string, bool) {
	var singleQuoted, doubleQuoted, escaped bool
	for i := 0; i < len(command); i++ {
		ch := command[i]
		if escaped {
			escaped = false
			continue
		}
		if ch == '\\' && !singleQuoted {
			escaped = true
			continue
		}
		switch ch {
		case '\'':
			if !doubleQuoted {
				singleQuoted = !singleQuoted
			}
		case '"':
			if !singleQuoted {
				doubleQuoted = !doubleQuoted
			}
		case '|':
			if !singleQuoted && !doubleQuoted {
				return strings.TrimSpace(command[:i]), true
			}
		}
	}
	return strings.TrimSpace(command), false
}

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
	return u.InputTokens + u.CacheReadTokens + u.CacheWriteTokens + u.CacheWrite1hTokens + u.OutputTokens + u.ReasoningTokens
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

func commandRepeatLoopNotice(n int) string {
	return fmt.Sprintf("[stopped: %d tool turns repeated the same underlying shell command]", n)
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
