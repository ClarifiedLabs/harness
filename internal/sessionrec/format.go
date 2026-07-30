// Package sessionrec is the one canonical recorder for session replay events.
// Both the interactive parent session (internal/ui accumulatingSink) and
// delegate child sessions (internal/delegate childSink) record through it, so
// child and parent raw.ndjson logs carry identical Display lines, pricing, and
// tool metadata by construction. The formatters here are the same ones the
// live renderer uses (internal/ui keeps thin wrappers), so live output and
// replay never drift.
package sessionrec

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"harness/internal/agent"
	"harness/internal/llm"
	"harness/internal/session"
	"harness/internal/tools"
)

// ToolResultLine renders the one-line tool summary used by live output and
// session replay.
func ToolResultLine(call llm.ToolCall, result llm.ToolResult) string {
	return fmt.Sprintf("[%s]%s → %s", call.Name, FormatToolArgs(call.Name, call.Input), ResultSummary(result))
}

// FormatArgs renders a tool call's input object as space-prefixed key=value
// pairs in a stable (sorted) order. String values are quoted when they contain
// whitespace; non-scalar values (objects, arrays) are summarized by their JSON
// so the line stays one row.
func FormatArgs(input json.RawMessage) string {
	if len(input) == 0 {
		return ""
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(input, &obj); err != nil {
		return ""
	}
	keys := make([]string, 0, len(obj))
	for k := range obj {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	var b strings.Builder
	for _, k := range keys {
		fmt.Fprintf(&b, " %s=%s", k, FormatValue(obj[k]))
	}
	return b.String()
}

// FormatToolArgs renders the compact argument summary for one tool call. The
// edit tool gets a dedicated path/edit-count form; everything else falls back
// to the generic key=value rendering.
func FormatToolArgs(name string, input json.RawMessage) string {
	if name == "edit" {
		if args := formatEditArgs(input); args != "" {
			return args
		}
	}
	return FormatArgs(input)
}

func formatEditArgs(input json.RawMessage) string {
	var args struct {
		Files []struct {
			Path  string            `json:"path"`
			Edits []json.RawMessage `json:"edits"`
		} `json:"files"`
	}
	if err := json.Unmarshal(input, &args); err != nil || len(args.Files) == 0 {
		return ""
	}
	paths := make([]string, 0, len(args.Files))
	edits := 0
	for _, file := range args.Files {
		if file.Path != "" {
			paths = append(paths, file.Path)
		}
		edits += len(file.Edits)
	}
	if len(args.Files) == 1 {
		return fmt.Sprintf(" path=%s edits=%d", FormatScalar(args.Files[0].Path), edits)
	}
	return fmt.Sprintf(" files=%d edits=%d paths=%s", len(args.Files), edits, FormatScalar(strings.Join(paths, ",")))
}

// FormatScalar clips and quotes one scalar value for an args line.
func FormatScalar(s string) string {
	s = Clip(s, 60)
	if strings.ContainsAny(s, " \t\r\n") {
		return fmt.Sprintf("%q", s)
	}
	return s
}

// FormatValue renders one JSON value compactly for an args line. Strings with
// whitespace are quoted; long strings are clipped.
func FormatValue(raw json.RawMessage) string {
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return FormatScalar(s)
	}
	return Clip(strings.TrimSpace(string(raw)), 60)
}

// ResultSummary describes a tool result for the arrow target: an error marker
// for is_error results, else a line count (when multi-line) and byte size.
func ResultSummary(result llm.ToolResult) string {
	if result.IsError {
		return "error: " + Clip(FirstLine(result.Text), 80)
	}
	n := len(result.Text)
	lines := CountLines(result.Text)
	size := tools.HumanBytes(n)
	prefix := ""
	if result.Truncated {
		if result.OriginalBytes > 0 {
			prefix = fmt.Sprintf("truncated %s of %s, ", tools.HumanBytes(result.ShownBytes), tools.HumanBytes(result.OriginalBytes))
		} else {
			prefix = "truncated, "
		}
	}
	var textSummary string
	if lines <= 1 {
		if n == 0 {
			textSummary = prefix + "(empty), " + size
		} else {
			textSummary = prefix + size
		}
	} else {
		textSummary = fmt.Sprintf("%s%d lines, %s", prefix, lines, size)
	}
	if len(result.Content) == 0 {
		return textSummary
	}
	return textSummary + " + " + RichImagesSummary(result.Content)
}

// RichImagesSummary summarizes the image blocks attached to a tool result.
func RichImagesSummary(content []llm.ContentBlock) string {
	mimeSet := make(map[string]struct{}, len(content))
	decodedBytes := 0
	for _, image := range content {
		mime := image.ImageMediaType
		if mime == "" {
			mime = "unknown"
		}
		mimeSet[mime] = struct{}{}
		decodedBytes += imageDisplayBytes(image)
	}
	mimes := make([]string, 0, len(mimeSet))
	for mime := range mimeSet {
		mimes = append(mimes, mime)
	}
	sort.Strings(mimes)
	label := "images"
	if len(content) == 1 {
		label = "image"
	}
	return fmt.Sprintf("%d %s (%s, %s)", len(content), label, strings.Join(mimes, ", "), tools.HumanBytes(decodedBytes))
}

// RichImageMetadata renders the one-line bracketed metadata for one attached
// image.
func RichImageMetadata(image llm.ContentBlock) string {
	parts := []string{"image", image.ImageMediaType, tools.HumanBytes(imageDisplayBytes(image))}
	if image.ImageWidth > 0 && image.ImageHeight > 0 {
		parts = append(parts, fmt.Sprintf("%dx%d", image.ImageWidth, image.ImageHeight))
	}
	if image.ImageDetail != "" {
		parts = append(parts, "detail="+image.ImageDetail)
	}
	return "[" + strings.Join(parts, ", ") + "]"
}

func imageDisplayBytes(image llm.ContentBlock) int {
	if image.ImageBytes > 0 {
		return image.ImageBytes
	}
	return decodedBase64Size(image.ImageData)
}

func decodedBase64Size(data string) int {
	n := len(data) * 3 / 4
	if strings.HasSuffix(data, "==") {
		n -= 2
	} else if strings.HasSuffix(data, "=") {
		n--
	}
	if n < 0 {
		return 0
	}
	return n
}

// UsageLine renders the per-prompt summary with cumulative totals (design §10):
//
//	[prompt: 3 turns · 12.4k (15.0k) in / 1.8k (2.0k) out · $0.071 ($0.102) · 4.3s]
//
// Per-prompt values are shown first; parenthesised values are cumulative across
// the session. Cumulative cost is omitted for models with no price entry;
// per-prompt cost is also omitted when the model has no price entry.
func UsageLine(u agent.PromptUsage, elapsed time.Duration, cost float64, costKnown bool, cumIn, cumOut int, cumCost float64) string {
	var b strings.Builder
	fmt.Fprintf(&b, "[prompt: %s · %s (%s) in / %s (%s) out",
		TurnPhrase(u.Turns),
		HumanTokens(u.Usage.InputTokens), HumanTokens(cumIn),
		HumanTokens(u.Usage.OutputTokens), HumanTokens(cumOut))
	// Cache reads and reasoning tokens are billed and material to cost; surface
	// them (with the cache-hit ratio) when non-zero (r15).
	if u.Usage.CacheReadTokens > 0 {
		fmt.Fprintf(&b, " · cache %s read", HumanTokens(u.Usage.CacheReadTokens))
		if ratio := CacheHitRatio(u.Usage); ratio > 0 {
			fmt.Fprintf(&b, " (%d%%)", ratio)
		}
	}
	if u.Usage.ReasoningTokens > 0 {
		fmt.Fprintf(&b, " · %s reasoning", HumanTokens(u.Usage.ReasoningTokens))
	}
	if u.Usage.CacheWrite1hTokens > 0 {
		fmt.Fprintf(&b, " · %s cache write (1h)", HumanTokens(u.Usage.CacheWrite1hTokens))
	}
	if costKnown {
		fmt.Fprintf(&b, " · $%.3f ($%.3f)", cost, cumCost)
	}
	if u.Context.Total > 0 {
		fmt.Fprintf(&b, " · ctx %s/%s", HumanTokens(u.Context.Total), HumanTokens(u.Context.Window))
		if u.Context.PayloadTotal > 0 && u.Context.PayloadTotal != u.Context.Total {
			fmt.Fprintf(&b, " payload %s", HumanTokens(u.Context.PayloadTotal))
		}
		if u.Context.System > 0 || u.Context.Tools > 0 || u.Context.Messages > 0 {
			fmt.Fprintf(&b, " (sys %s tools %s msgs %s)",
				HumanTokens(u.Context.System), HumanTokens(u.Context.Tools), HumanTokens(u.Context.Messages))
		}
	}
	fmt.Fprintf(&b, " · %s]", HumanDuration(elapsed))
	return b.String()
}

// TurnUsageLine renders the per-turn summary:
//
//	[turn: 1 · 1.0s · $0.003 · ctx 12% 15.0k/128.0k │ prompt 4.3s]
func TurnUsageLine(u agent.TurnUsage, elapsed, promptElapsed time.Duration) string {
	var b strings.Builder
	fmt.Fprintf(&b, "[turn: %d · %s", u.Turn, HumanDuration(elapsed))
	// Turn cost is known once the model stream closes: TurnComplete fires after
	// the final usage frame, so Usage carries the turn's input/output/cache
	// totals and either the proxy-priced cost or, for direct providers, a cost
	// priced by the sink against the model registry.
	if u.Usage.CostKnown {
		fmt.Fprintf(&b, " · $%.3f", u.Usage.CostUSD)
	}
	if used := ContextUsed(u.Context); u.Context.Window > 0 && used > 0 {
		fmt.Fprintf(&b, " · ctx %d%% %s/%s", ContextPercent(u.Context), HumanTokens(used), HumanTokens(u.Context.Window))
	}
	if promptElapsed >= 0 {
		fmt.Fprintf(&b, " │ prompt %s", HumanDuration(promptElapsed))
	}
	b.WriteByte(']')
	return b.String()
}

// TurnPhrase renders a turn count with singular/plural agreement.
func TurnPhrase(n int) string {
	if n == 1 {
		return "1 turn"
	}
	return fmt.Sprintf("%d turns", n)
}

// FirstLine returns the first line of s.
func FirstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}

// CountLines counts text lines: a trailing newline does not add an empty line.
func CountLines(s string) int {
	if s == "" {
		return 0
	}
	n := strings.Count(s, "\n")
	if !strings.HasSuffix(s, "\n") {
		n++
	}
	return n
}

// Clip truncates s to max bytes, appending an ellipsis when truncated.
func Clip(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max] + "…"
}

// HumanTokens renders a token count compactly: 12400 -> "12.4k".
func HumanTokens(n int) string {
	if n < 1000 {
		return fmt.Sprintf("%d", n)
	}
	return fmt.Sprintf("%.1fk", float64(n)/1000)
}

// HumanDuration renders an elapsed turn duration: "4.3s" or "850ms".
func HumanDuration(d time.Duration) string {
	if d < time.Second {
		return fmt.Sprintf("%dms", d.Milliseconds())
	}
	return fmt.Sprintf("%.1fs", d.Seconds())
}

// ModelRequestIssueLine renders the durable one-line model API diagnostic
// stored on model_request replay events.
func ModelRequestIssueLine(event llm.ModelRequestEvent) string {
	var b strings.Builder
	if event.Outcome == llm.ModelRequestOutcomeTerminal {
		b.WriteString("[error: model API")
	} else {
		b.WriteString("[model API")
	}
	if event.StatusCode != 0 {
		fmt.Fprintf(&b, " %d", event.StatusCode)
	}
	if event.Code != "" {
		fmt.Fprintf(&b, " (%s)", event.Code)
	}
	if message := strings.Join(strings.Fields(event.Message), " "); message != "" {
		b.WriteString(": ")
		b.WriteString(message)
	}
	if event.Outcome == llm.ModelRequestOutcomeRetrying {
		b.WriteString("; retrying")
		if event.RetryDelayMS > 0 {
			b.WriteString(" in ")
			b.WriteString(FormatRetryDelay(event.RetryDelayMS))
		}
	}
	if event.ProxyRequestID != 0 {
		fmt.Fprintf(&b, "; proxy request %d", event.ProxyRequestID)
	}
	if event.UpstreamRequestID != "" {
		fmt.Fprintf(&b, "; upstream request %s", event.UpstreamRequestID)
	}
	if event.TraceID != "" {
		fmt.Fprintf(&b, "; trace %s", event.TraceID)
	}
	b.WriteByte(']')
	return b.String()
}

// ModelRequestDisplayLine returns the durable display line for a model_request
// replay event, if any. Only upstream failures that cannot recover by
// discarding a stale continuation anchor produce a line; every other state is
// diagnostics-only in the structured event payload.
func ModelRequestDisplayLine(event llm.ModelRequestEvent) string {
	switch event.State {
	case llm.ModelRequestUpstreamAttemptFailed, llm.ModelRequestFailed:
		if !continuationFailureMayRecover(event) {
			return ModelRequestIssueLine(event)
		}
	}
	return ""
}

func continuationFailureMayRecover(event llm.ModelRequestEvent) bool {
	if event.Outcome != llm.ModelRequestOutcomeTerminal {
		return false
	}
	code := strings.ToLower(event.Code)
	if strings.Contains(code, "previous_response") || strings.Contains(code, "previous_interaction") {
		return true
	}
	message := strings.ToLower(event.Message)
	return strings.Contains(message, "previous_response_id") ||
		strings.Contains(message, "previous response") ||
		strings.Contains(message, "previous_interaction_id") ||
		strings.Contains(message, "previous interaction")
}

// FormatRetryDelay renders a retry backoff delay in whole milliseconds.
func FormatRetryDelay(milliseconds int64) string {
	return (time.Duration(milliseconds) * time.Millisecond).Round(time.Millisecond).String()
}

// ContextPercent is the share of the model's context window in use (r27).
func ContextPercent(ctx agent.ContextEstimate) int {
	if ctx.Window <= 0 {
		return 0
	}
	used := ContextUsed(ctx)
	if used <= 0 {
		return 0
	}
	pct := used * 100 / ctx.Window
	if pct < 0 {
		return 0
	}
	if pct > 100 {
		pct = 100
	}
	return pct
}

// ContextUsed is the effective context occupancy: the larger of the estimated
// total and the measured payload.
func ContextUsed(ctx agent.ContextEstimate) int {
	return max(ctx.Total, ctx.PayloadTotal)
}

// CacheHitRatio is the percentage of input tokens served from cache (r15).
func CacheHitRatio(u llm.Usage) int {
	total := u.InputTokens + u.CacheReadTokens
	if total <= 0 {
		return 0
	}
	return u.CacheReadTokens * 100 / total
}

// ContextSnapshot maps an agent.ContextEstimate to its durable session-log
// form, returning nil when the estimate carries no data.
func ContextSnapshot(ctx agent.ContextEstimate) *session.ContextSnapshot {
	if ctx.Total == 0 && ctx.Window == 0 && ctx.System == 0 && ctx.Tools == 0 && ctx.Messages == 0 &&
		ctx.PayloadTotal == 0 && ctx.PayloadSystem == 0 && ctx.PayloadTools == 0 && ctx.PayloadMessages == 0 {
		return nil
	}
	return &session.ContextSnapshot{
		Total:           ctx.Total,
		Window:          ctx.Window,
		System:          ctx.System,
		Tools:           ctx.Tools,
		Messages:        ctx.Messages,
		Source:          ctx.Source,
		PayloadTotal:    ctx.PayloadTotal,
		PayloadSystem:   ctx.PayloadSystem,
		PayloadTools:    ctx.PayloadTools,
		PayloadMessages: ctx.PayloadMessages,
	}
}
