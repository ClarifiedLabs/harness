// Package ui drives the harness from stdin: a streaming renderer implementing
// the agent's EventSink, an interactive REPL with meta-commands, and a one-shot
// mode for piping a single prompt (design §10).
package ui

import (
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"

	"harness/internal/agent"
	"harness/internal/diff"
	"harness/internal/llm"
	"harness/internal/markdown"
	"harness/internal/tools"
)

// ANSI styling is emitted only when RenderOptions.Color is set. Rendering stays
// legible without color (design §2, §10).
const (
	ansiDim              = "\x1b[2m"
	ansiReset            = "\x1b[0m"
	TimestampShortLayout = "15:04:05"
	TimestampFullLayout  = "2006-01-02 15:04:05"
)

// snippetLines caps the verbose result preview (design §10: "first ~5 lines").
const snippetLines = 5

// RenderOptions configures a Renderer. Color is decided by the caller (TTY check
// plus NO_COLOR / -no-color); Now is injected so the per-turn duration is
// deterministic in tests (design §10, §13).
type RenderOptions struct {
	Color                   bool
	Markdown                bool
	Verbose                 bool
	ToolStream              bool
	Quiet                   bool
	SuppressReasoningOutput bool
	Model                   string
	Registry                *llm.Registry
	Now                     func() time.Time
	TimestampLayout         string
	Width                   func() int
}

// Renderer implements agent.EventSink: assistant text streams to out, while tool
// one-liners, the usage line, and notices go to errw so one-shot stdout carries
// only the model's answer (design §10).
type Renderer struct {
	out                     io.Writer
	errw                    io.Writer
	color                   bool
	markdown                bool
	verbose                 bool
	toolStream              bool
	quiet                   bool
	suppressReasoningOutput bool
	model                   string
	registry                *llm.Registry
	now                     func() time.Time
	timestampLayout         string
	width                   func() int

	turnStart         time.Time
	assistantLineOpen bool
	assistantMarkdown *markdown.Stream
	pending           map[string]llm.ToolCall // tool_use id -> call, awaiting its result
	pendingToolUses   []string

	cumInput  int
	cumOutput int
	cumCost   float64

	activeModelCost    float64
	largeRequestWarned bool
}

// NewRenderer builds a Renderer. A nil Now defaults to time.Now.
func NewRenderer(out, errw io.Writer, opts RenderOptions) *Renderer {
	now := opts.Now
	if now == nil {
		now = time.Now
	}
	return &Renderer{
		out:                     out,
		errw:                    errw,
		color:                   opts.Color,
		markdown:                opts.Markdown,
		verbose:                 opts.Verbose,
		toolStream:              opts.ToolStream,
		quiet:                   opts.Quiet,
		suppressReasoningOutput: opts.SuppressReasoningOutput,
		model:                   opts.Model,
		registry:                opts.Registry,
		now:                     now,
		timestampLayout:         opts.TimestampLayout,
		width:                   opts.Width,
		pending:                 make(map[string]llm.ToolCall),
	}
}

// StartTurn records the turn's start instant for the duration in the usage line.
// The driver calls it immediately before agent.RunTurn.
func (r *Renderer) StartTurn() {
	r.turnStart = r.now()
	r.activeModelCost = 0
	r.largeRequestWarned = false
	r.assistantMarkdown = nil
}

// SetModel updates the model used for subsequent usage/cost summaries.
func (r *Renderer) SetModel(model string) { r.model = model }

// SetCumulativeUsage seeds the session totals used by per-turn usage lines.
func (r *Renderer) SetCumulativeUsage(inputTokens, outputTokens int, costUSD float64) {
	r.cumInput = inputTokens
	r.cumOutput = outputTokens
	r.cumCost = costUSD
}

func (r *Renderer) TextDelta(text string) {
	if text == "" {
		return
	}
	if r.markdown {
		r.ensureAssistantMarkdown()
		io.WriteString(r.out, r.assistantMarkdown.Write(text))
		r.assistantLineOpen = r.assistantMarkdown.LineOpen()
		return
	}
	io.WriteString(r.out, text)
	r.assistantLineOpen = !strings.HasSuffix(text, "\n")
}

func (r *Renderer) ReasoningSummary(text string) {
	if r.suppressReasoningOutput {
		return
	}
	r.flushToolUseStarts()
	r.finishAssistantLine()
	io.WriteString(r.out, r.reasoningSummaryBlock(text))
}

func (r *Renderer) ReasoningSummaryStatus(text string) {
	if r.suppressReasoningOutput {
		return
	}
	r.flushToolUseStarts()
	r.finishAssistantLine()
	io.WriteString(r.errw, r.reasoningSummaryBlock(text))
}

func (r *Renderer) ModelTurnStart(modelTurn, attempt int, ctx agent.ContextEstimate) {
	r.flushToolUseStarts()
	if attempt <= 1 && !r.largeRequestWarned {
		if line := largeRequestWarning(ctx); line != "" {
			r.dimLine(line)
			r.largeRequestWarned = true
		}
	}
	if attempt <= 1 {
		r.dimLine(fmt.Sprintf("[model: turn %d waiting]", modelTurn))
		return
	}
	r.dimLine(fmt.Sprintf("[model: turn %d retry %d waiting]", modelTurn, attempt-1))
}

func (r *Renderer) ModelTurnComplete(usage agent.ModelTurnUsage) {
	r.writeModelTurnComplete(usage)
}

func (r *Renderer) writeModelTurnComplete(usage agent.ModelTurnUsage) string {
	defer r.flushToolUseStarts()
	if r.registry == nil {
		return ""
	}
	cost, known := r.registry.Cost(r.model, usage.Usage)
	if !known {
		return ""
	}
	r.activeModelCost += cost
	line := modelTurnCostLine(usage, cost, r.activeModelCost, r.cumCost+r.activeModelCost)
	r.dimLine(line)
	return line
}

func (r *Renderer) ToolUseStart(call llm.ToolCall) {
	if !r.toolStream || r.quiet {
		return
	}
	r.pendingToolUses = append(r.pendingToolUses, fmt.Sprintf("[tool-call: %s id=%s]", call.Name, call.ID))
}

func (r *Renderer) ToolUseDelta(_ int, _ string) {}

// ToolStart stashes the call so ToolResult can render name+args+summary on one
// line once the result is known.
func (r *Renderer) ToolStart(call llm.ToolCall) {
	r.flushToolUseStarts()
	r.pending[call.ID] = call
	r.dimLine(fmt.Sprintf("[tool: %s started%s]", call.Name, formatToolArgs(call.Name, call.Input)))
}

func (r *Renderer) ToolResult(result llm.ToolResult) {
	r.flushToolUseStarts()
	call := r.pending[result.ForID]
	delete(r.pending, result.ForID)

	r.dimLine(ToolResultLine(call, result))

	if r.verbose {
		for _, s := range snippet(result.Text) {
			r.dimLine("  " + s)
		}
	}
}

func (r *Renderer) ToolDiff(_ llm.ToolCall, text string) {
	r.flushToolUseStarts()
	r.finishAssistantLine()
	if text == "" {
		return
	}
	if r.color {
		text = diff.Colorize(text)
	}
	io.WriteString(r.errw, text)
	if !strings.HasSuffix(text, "\n") {
		fmt.Fprintln(r.errw)
	}
}

func (r *Renderer) Notice(msg string) {
	r.flushToolUseStarts()
	r.dimLine(msg)
}

func (r *Renderer) TurnComplete(usage agent.TurnUsage) {
	r.flushToolUseStarts()
	r.finishAssistantLine()
	elapsed := r.now().Sub(r.turnStart)

	// Accumulate session totals for the cumulative readout.
	var cost float64
	if r.registry != nil {
		cost, _ = r.registry.Cost(r.model, usage.Usage)
	}
	r.cumInput += usage.Usage.InputTokens
	r.cumOutput += usage.Usage.OutputTokens
	r.cumCost += cost

	r.dimLine(usageLine(r.registry, r.model, usage, elapsed, r.cumInput, r.cumOutput, r.cumCost))
}

func (r *Renderer) flushToolUseStarts() {
	if len(r.pendingToolUses) == 0 {
		return
	}
	lines := r.pendingToolUses
	r.pendingToolUses = nil
	for _, line := range lines {
		r.dimLine(line)
	}
}

// dimLine writes one line to errw, wrapping it in dim ANSI codes when color is
// enabled. When quiet mode is on the line is silently dropped.
func (r *Renderer) dimLine(s string) {
	if r.quiet {
		return
	}
	r.finishAssistantLine()
	s = r.timestampStatusLine(s)
	if r.color {
		fmt.Fprintf(r.errw, "%s%s%s\n", ansiDim, s, ansiReset)
		return
	}
	fmt.Fprintln(r.errw, s)
}

func (r *Renderer) timestampStatusLine(s string) string {
	if r.timestampLayout == "" || !strings.HasPrefix(s, "[") {
		return s
	}
	return "[" + r.now().Format(r.timestampLayout) + " " + strings.TrimPrefix(s, "[")
}

func (r *Renderer) reasoningSummaryBlock(text string) string {
	text = strings.TrimSpace(text)
	if text == "" {
		return ""
	}
	header := "[reasoning]"
	if r.timestampLayout != "" {
		header = "[" + r.now().Format(r.timestampLayout) + " reasoning]"
	}
	body := markdown.Render(text, markdown.Options{
		Enabled: true,
		ANSI:    r.color,
		Width:   r.outputWidth(),
		Prefix:  "  ",
	})
	var b strings.Builder
	b.WriteString(header)
	b.WriteByte('\n')
	if body != "" {
		b.WriteString(body)
		if !strings.HasSuffix(body, "\n") {
			b.WriteByte('\n')
		}
	}
	b.WriteString("[end reasoning]\n")
	return b.String()
}

func (r *Renderer) outputWidth() int {
	if r.width != nil {
		if width := r.width(); width > 0 {
			return width
		}
	}
	return markdown.DefaultWidth
}

func (r *Renderer) finishAssistantLine() {
	r.flushAssistantMarkdown()
	if !r.assistantLineOpen {
		return
	}
	fmt.Fprintln(r.out)
	r.assistantLineOpen = false
	if r.assistantMarkdown != nil {
		r.assistantMarkdown.CloseLine()
	}
}

func (r *Renderer) flushAssistantMarkdown() {
	if !r.markdown || r.assistantMarkdown == nil {
		return
	}
	io.WriteString(r.out, r.assistantMarkdown.Flush())
	r.assistantLineOpen = r.assistantMarkdown.LineOpen()
}

func (r *Renderer) ensureAssistantMarkdown() {
	if r.assistantMarkdown != nil {
		return
	}
	r.assistantMarkdown = markdown.NewStream(markdown.Options{
		Enabled: true,
		ANSI:    r.color,
		Width:   r.outputWidth(),
	})
}

// formatArgs renders a tool call's input object as space-prefixed key=value
// pairs in a stable (sorted) order. String values are quoted when they contain
// whitespace; non-scalar values (objects, arrays) are summarized by their JSON
// so the line stays one row.
func formatArgs(input json.RawMessage) string {
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
		fmt.Fprintf(&b, " %s=%s", k, formatValue(obj[k]))
	}
	return b.String()
}

func formatToolArgs(name string, input json.RawMessage) string {
	if name == "edit" {
		if args := formatEditArgs(input); args != "" {
			return args
		}
	}
	return formatArgs(input)
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
		return fmt.Sprintf(" path=%s edits=%d", formatScalar(args.Files[0].Path), edits)
	}
	return fmt.Sprintf(" files=%d edits=%d paths=%s", len(args.Files), edits, formatScalar(strings.Join(paths, ",")))
}

func formatScalar(s string) string {
	s = clip(s, 60)
	if strings.ContainsAny(s, " \t\r\n") {
		return fmt.Sprintf("%q", s)
	}
	return s
}

// formatValue renders one JSON value compactly for an args line. Strings with
// whitespace are quoted; long strings are clipped.
func formatValue(raw json.RawMessage) string {
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return formatScalar(s)
	}
	return clip(strings.TrimSpace(string(raw)), 60)
}

// resultSummary describes a tool result for the arrow target: an error marker
// for is_error results, else a line count (when multi-line) and byte size.
func resultSummary(result llm.ToolResult) string {
	if result.IsError {
		return "error: " + clip(firstLine(result.Text), 80)
	}
	n := len(result.Text)
	lines := countLines(result.Text)
	size := tools.HumanBytes(n)
	prefix := ""
	if result.Truncated {
		if result.OriginalBytes > 0 {
			prefix = fmt.Sprintf("truncated %s of %s, ", tools.HumanBytes(result.ShownBytes), tools.HumanBytes(result.OriginalBytes))
		} else {
			prefix = "truncated, "
		}
	}
	if lines <= 1 {
		if n == 0 {
			return prefix + "(empty), " + size
		}
		return prefix + size
	}
	return fmt.Sprintf("%s%d lines, %s", prefix, lines, size)
}

// ToolResultLine renders the one-line tool summary used by live output and
// session replay.
func ToolResultLine(call llm.ToolCall, result llm.ToolResult) string {
	return fmt.Sprintf("[%s]%s → %s", call.Name, formatToolArgs(call.Name, call.Input), resultSummary(result))
}

// usageLine renders the per-turn summary with cumulative totals (design §10):
//
//	[turn: 3 model turns · 12.4k (15.0k) in / 1.8k (2.0k) out · $0.071 ($0.102) · 4.3s]
//
// Per-turn values are shown first; parenthesised values are cumulative across
// the session. Cumulative cost is omitted for models with no price entry;
// per-turn cost is also omitted when the model has no price entry.
func usageLine(registry *llm.Registry, model string, u agent.TurnUsage, elapsed time.Duration, cumIn, cumOut int, cumCost float64) string {
	var b strings.Builder
	fmt.Fprintf(&b, "[turn: %s · %s (%s) in / %s (%s) out",
		modelTurnPhrase(u.ModelTurns),
		humanTokens(u.Usage.InputTokens), humanTokens(cumIn),
		humanTokens(u.Usage.OutputTokens), humanTokens(cumOut))
	if registry != nil {
		if _, known := registry.Cost(model, u.Usage); known {
			fmt.Fprintf(&b, " · $%.3f ($%.3f)", turnCost(registry, model, u.Usage), cumCost)
		}
	}
	if u.Context.Total > 0 {
		fmt.Fprintf(&b, " · ctx %s/%s", humanTokens(u.Context.Total), humanTokens(u.Context.Window))
		if u.Context.PayloadTotal > 0 && u.Context.PayloadTotal != u.Context.Total {
			fmt.Fprintf(&b, " payload %s", humanTokens(u.Context.PayloadTotal))
		}
		if u.Context.System > 0 || u.Context.Tools > 0 || u.Context.Messages > 0 {
			fmt.Fprintf(&b, " (sys %s tools %s msgs %s)",
				humanTokens(u.Context.System), humanTokens(u.Context.Tools), humanTokens(u.Context.Messages))
		}
	}
	fmt.Fprintf(&b, " · %s]", humanDuration(elapsed))
	return b.String()
}

func modelTurnCostLine(u agent.ModelTurnUsage, callCost, currentTurnCost, sessionCost float64) string {
	label := fmt.Sprintf("turn %d", u.ModelTurn)
	if u.Attempt > 1 {
		label = fmt.Sprintf("turn %d retry %d", u.ModelTurn, u.Attempt-1)
	}
	return fmt.Sprintf("[model: %s cost: $%.4f · totals: $%.4f prompt · $%.4f session]",
		label, callCost, currentTurnCost, sessionCost)
}

func largeRequestWarning(ctx agent.ContextEstimate) string {
	if ctx.Total == 0 && ctx.PayloadTotal == 0 && ctx.Tools == 0 {
		return ""
	}
	payload := ctx.PayloadTotal
	if payload == 0 {
		payload = ctx.Total
	}
	largeWindow := ctx.Window > 0 && (ctx.Total*2 >= ctx.Window || payload*2 >= ctx.Window)
	if !largeWindow && ctx.Tools < 10_000 {
		return ""
	}
	return fmt.Sprintf("[warning: large model request: ctx %s/%s payload %s (sys %s tools %s msgs %s); large prompts/tool schemas can slow response start]",
		humanTokens(ctx.Total), humanTokens(ctx.Window), humanTokens(payload),
		humanTokens(ctx.System), humanTokens(ctx.Tools), humanTokens(ctx.Messages))
}

func modelTurnPhrase(n int) string {
	if n == 1 {
		return "1 model turn"
	}
	return fmt.Sprintf("%d model turns", n)
}

// turnCost returns the USD cost for a single turn's usage. It returns 0 when
// the model is unknown.
func turnCost(registry *llm.Registry, model string, u llm.Usage) float64 {
	usd, _ := registry.Cost(model, u)
	return usd
}

// snippet returns the first snippetLines lines of s for the verbose preview.
func snippet(s string) []string {
	if s == "" {
		return nil
	}
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	if len(lines) > snippetLines {
		lines = lines[:snippetLines]
	}
	return lines
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}

// countLines counts text lines: a trailing newline does not add an empty line.
func countLines(s string) int {
	if s == "" {
		return 0
	}
	n := strings.Count(s, "\n")
	if !strings.HasSuffix(s, "\n") {
		n++
	}
	return n
}

func clip(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max] + "…"
}

// humanTokens renders a token count compactly: 12400 -> "12.4k".
func humanTokens(n int) string {
	if n < 1000 {
		return fmt.Sprintf("%d", n)
	}
	return fmt.Sprintf("%.1fk", float64(n)/1000)
}

// humanDuration renders an elapsed turn duration: "4.3s" or "850ms".
func humanDuration(d time.Duration) string {
	if d < time.Second {
		return fmt.Sprintf("%dms", d.Milliseconds())
	}
	return fmt.Sprintf("%.1fs", d.Seconds())
}
