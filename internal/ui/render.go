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
	"sync"
	"time"
	"unicode/utf8"

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

const finalAnswerSeparator = "\n---\n\n"

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
	// SuppressUsage drops the per-turn usage/cost line. It defaults false so
	// even -quiet runs still print the single cost line; set it for a fully
	// silent piped run (r25).
	SuppressUsage bool
	// LiveStatus enables the in-place wait-time counter and the during-turn
	// input line (r12 + during-turn input). Gated by the caller to an
	// interactive, non-quiet TTY; tests set it explicitly.
	LiveStatus      bool
	Model           string
	Registry        *llm.Registry
	Now             func() time.Time
	TimestampLayout string
	Width           func() int
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
	suppressUsage           bool
	suppressReasoningOutput bool
	model                   string
	registry                *llm.Registry
	now                     func() time.Time
	timestampLayout         string
	width                   func() int

	turnStart         time.Time
	promptStart       time.Time
	assistantLineOpen bool
	assistantMarkdown *markdown.Stream
	assistantPhase    string

	visiblePreFinalOutput bool
	visibleFinalOutput    bool
	finalSeparatorPrinted bool

	pending         map[string]llm.ToolCall // tool_use id -> call, awaiting its result
	pendingToolUses []string

	cumInput  int
	cumOutput int
	cumCost   float64

	activeModelCost    float64
	largeRequestWarned bool

	// turnCost carries the per-turn cost priced by the App against the
	// turn's own model (r63). When turnCostSet is false TurnComplete falls
	// back to pricing against r.model.
	turnCost      float64
	turnCostKnown bool
	turnCostSet   bool

	// warnedNoPrice tracks models for which the one-time "no price" notice has
	// already been emitted (r16).
	warnedNoPrice map[string]bool
	// compactionWarned guards the one-time "approaching compaction" notice (r27).
	compactionWarned bool

	// Live wait-time counter + during-turn input line (r12 + during-turn
	// input). statusMu guards every field below and serialises the ticker
	// goroutine against the synchronous event-sink writes so the two never
	// interleave terminal bytes.
	liveStatus        bool
	statusMu          sync.Mutex
	statusActive      bool      // in a wait; the ticker should keep the line painted
	statusDrawn       bool      // a status line is currently on the terminal
	statusLabel       string    // e.g. "model: turn 3" or "tool: grep"
	statusStart       time.Time // when the current wait began
	statusCtxPct      int       // context percent to append, 0 omits (r27)
	statusInput       string    // during-turn typed buffer shown after "> "
	statusInputCursor int       // rune index of the edit cursor within statusInput
	ticker            *time.Ticker
	tickerStop        chan struct{}
	tickerDone        chan struct{}
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
		suppressUsage:           opts.SuppressUsage,
		liveStatus:              opts.LiveStatus && !opts.Quiet,
		model:                   opts.Model,
		registry:                opts.Registry,
		now:                     now,
		timestampLayout:         opts.TimestampLayout,
		width:                   opts.Width,
		pending:                 make(map[string]llm.ToolCall),
	}
}

// StartPrompt records when the last user prompt was submitted. The live status
// line uses it for the total elapsed time across all model/tool waits in the
// prompt turn.
func (r *Renderer) StartPrompt() {
	now := r.now()
	r.statusMu.Lock()
	r.promptStart = now
	r.statusMu.Unlock()
}

// StartTurn records the turn's start instant for the duration in the usage line.
// The driver calls it immediately before agent.RunTurn. If StartPrompt was not
// called (older tests and direct callers), the prompt total starts here too.
func (r *Renderer) StartTurn() {
	now := r.now()
	r.turnStart = now
	r.statusMu.Lock()
	if r.promptStart.IsZero() {
		r.promptStart = now
	}
	r.statusMu.Unlock()
	r.activeModelCost = 0
	r.largeRequestWarned = false
	r.compactionWarned = false
	r.turnCost = 0
	r.turnCostKnown = false
	r.turnCostSet = false
	r.assistantMarkdown = nil
	r.assistantPhase = ""
	r.visiblePreFinalOutput = false
	r.visibleFinalOutput = false
	r.finalSeparatorPrinted = false
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
	r.statusClear()
	r.writeFinalSeparatorIfNeeded()
	if r.markdown {
		r.ensureAssistantMarkdown()
		io.WriteString(r.out, r.assistantMarkdown.Write(text))
		r.assistantLineOpen = r.assistantMarkdown.LineOpen()
		r.markAssistantTextVisible()
		return
	}
	io.WriteString(r.out, text)
	r.assistantLineOpen = !strings.HasSuffix(text, "\n")
	r.markAssistantTextVisible()
}

func (r *Renderer) AssistantPhase(phase string) {
	if !llm.ValidAssistantPhase(phase) || phase == "" {
		return
	}
	r.assistantPhase = phase
}

func (r *Renderer) ReasoningSummary(text string) {
	if r.suppressReasoningOutput {
		return
	}
	r.statusClear()
	r.flushToolUseStarts()
	r.finishAssistantLine()
	block := r.reasoningSummaryBlock(text)
	if block == "" {
		return
	}
	io.WriteString(r.out, block)
	r.visiblePreFinalOutput = true
}

func (r *Renderer) ReasoningSummaryStatus(text string) {
	if r.suppressReasoningOutput {
		return
	}
	r.statusClear()
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
	pct := contextPercent(ctx)
	r.maybeWarnCompaction(pct)
	if r.liveStatus {
		label := fmt.Sprintf("model: turn %d", modelTurn)
		if attempt > 1 {
			label = fmt.Sprintf("model: turn %d retry %d", modelTurn, attempt-1)
		}
		r.beginWait(label, pct)
		return
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
	if !usage.Usage.CostKnown {
		r.maybeWarnNoPrice()
		return ""
	}
	cost := usage.Usage.CostUSD
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
	// Tick during the (possibly long) tool-execution gap, not just model
	// waits (r12). The next output line erases this counter again.
	if r.liveStatus {
		r.beginWait(fmt.Sprintf("tool: %s", call.Name), 0)
	}
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
	r.statusClear()
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
	r.StopProgress()
	r.flushToolUseStarts()
	r.finishAssistantLine()
	elapsed := r.now().Sub(r.turnStart)

	// Accumulate session totals for the cumulative readout. The App prices the
	// turn against its own model and forwards it via SetTurnCost so a mid-turn
	// model switch is not mispriced against the renderer's model (r63).
	cost, costKnown := usage.Usage.CostUSD, usage.Usage.CostKnown
	if r.turnCostSet {
		cost, costKnown = r.turnCost, r.turnCostKnown
	}
	r.cumInput += usage.Usage.InputTokens
	r.cumOutput += usage.Usage.OutputTokens
	if costKnown {
		r.cumCost += cost
	}

	r.usageOutput(usageLine(usage, elapsed, cost, costKnown, r.cumInput, r.cumOutput, r.cumCost))
}

// SetTurnCost records the turn cost priced by the App against the turn's own
// model, consumed by the next TurnComplete (r63).
func (r *Renderer) SetTurnCost(cost float64, known bool) {
	r.turnCost = cost
	r.turnCostKnown = known
	r.turnCostSet = true
}

// usageOutput writes the per-turn usage line. It honours -quiet only when
// SuppressUsage is set, so a plain -quiet run still prints the single cost line
// (r25). The status line is cleared first so the counter never lingers above it.
func (r *Renderer) usageOutput(line string) {
	if r.suppressUsage {
		return
	}
	r.statusClear()
	r.finishAssistantLine()
	out := r.timestampStatusLine(line)
	if r.color {
		fmt.Fprintf(r.errw, "%s%s%s\n", ansiDim, out, ansiReset)
		return
	}
	fmt.Fprintln(r.errw, out)
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
	r.statusClear()
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

// --- Live wait-time counter + during-turn input line (r12 + during-turn input) ---
//
// The counter is a single transient status line repainted in place with
// \r\x1b[2K (no scroll region, no sticky bar). beginWait activates it; any
// scrolling write erases it via statusClear, which the synchronous event-sink
// methods call before touching out/errw. A time.Ticker repaints it ~1/sec while
// a wait is in progress; the ticker and the foreground writers are serialised by
// statusMu and a stop-and-drain handshake so their bytes never interleave.

// beginWait activates (or refreshes) the transient counter for a model wait or a
// tool-execution gap. It finishes any open assistant line first so the counter
// sits on its own row and erasing it never clobbers streamed content.
func (r *Renderer) beginWait(label string, ctxPct int) {
	if !r.liveStatus {
		return
	}
	r.finishAssistantLine()
	r.statusMu.Lock()
	r.statusLabel = label
	r.statusStart = r.now()
	r.statusCtxPct = ctxPct
	r.statusActive = true
	r.ensureTickerLocked()
	r.paintLocked()
	r.statusMu.Unlock()
}

// statusClear erases the on-screen counter and deactivates it. Every method that
// writes scrolling output to out/errw calls this first; it is a no-op unless a
// live counter is currently drawn.
func (r *Renderer) statusClear() {
	if !r.liveStatus {
		return
	}
	r.statusMu.Lock()
	r.eraseLocked()
	r.statusActive = false
	r.statusMu.Unlock()
}

// SetInputLine updates the during-turn typed buffer shown after "> " on the
// counter line, along with the rune index of the edit cursor, and repaints if a
// wait is active. Empty restores the bare counter. cursor is clamped into
// [0, len(buf)] so a stale index never positions the terminal cursor off-row.
func (r *Renderer) SetInputLine(buf string, cursor int) {
	if !r.liveStatus {
		return
	}
	r.statusMu.Lock()
	r.statusInput = buf
	r.statusInputCursor = cursor
	if r.statusActive {
		r.paintLocked()
	}
	r.statusMu.Unlock()
}

// StopProgress erases the counter, clears the typed buffer, and stops the ticker,
// draining its goroutine so no stray repaint can follow. Idempotent; called at
// every turn boundary (TurnComplete, the REPL turn-done handoff, and one-shot).
func (r *Renderer) StopProgress() {
	if !r.liveStatus {
		return
	}
	r.statusMu.Lock()
	r.eraseLocked()
	r.statusActive = false
	r.statusInput = ""
	r.statusInputCursor = 0
	r.promptStart = time.Time{}
	t, stop, done := r.ticker, r.tickerStop, r.tickerDone
	r.ticker, r.tickerStop, r.tickerDone = nil, nil, nil
	r.statusMu.Unlock()
	if t != nil {
		t.Stop()
		close(stop)
		<-done
	}
}

func (r *Renderer) ensureTickerLocked() {
	if r.ticker != nil {
		return
	}
	r.ticker = time.NewTicker(time.Second)
	stop := make(chan struct{})
	done := make(chan struct{})
	r.tickerStop = stop
	r.tickerDone = done
	t := r.ticker
	go func() {
		defer close(done)
		for {
			select {
			case <-stop:
				return
			case <-t.C:
				r.tick()
			}
		}
	}()
}

// tick repaints the counter on a timer. It paints only while a wait is active,
// so a tick that races StopProgress (which sets statusActive=false under the
// same mutex) is a no-op.
func (r *Renderer) tick() {
	r.statusMu.Lock()
	if r.statusActive {
		r.paintLocked()
	}
	r.statusMu.Unlock()
}

// eraseLocked clears a drawn counter line. Caller holds statusMu.
func (r *Renderer) eraseLocked() {
	if !r.statusDrawn {
		return
	}
	io.WriteString(r.errw, "\r\x1b[2K")
	r.statusDrawn = false
}

// paintLocked redraws the counter in place. Caller holds statusMu. When a
// during-turn buffer is present it parks the terminal cursor at the edit column
// on the same single row, so cursor-motion keys (arrows/Home/End) land visibly.
func (r *Renderer) paintLocked() {
	text, cursorCol, hasInput := r.statusTextLocked()
	var b strings.Builder
	b.WriteString("\r\x1b[2K")
	if r.color {
		b.WriteString(ansiDim)
		b.WriteString(text)
		b.WriteString(ansiReset)
	} else {
		b.WriteString(text)
	}
	if hasInput {
		// The line is clamped to a single row, so \r returns to its start; move
		// right to the edit column. Coloring is irrelevant to cursor movement.
		b.WriteString("\r")
		if cursorCol > 0 {
			fmt.Fprintf(&b, "\x1b[%dC", cursorCol)
		}
	}
	io.WriteString(r.errw, b.String())
	r.statusDrawn = true
}

// statusTextLocked renders the counter, clipped to the terminal width so it
// never wraps (a wrapped line would defeat the single-line \r\x1b[2K erase). It
// also reports the terminal column for the during-turn edit cursor and whether a
// typed buffer is present. Caller holds statusMu.
func (r *Renderer) statusTextLocked() (text string, cursorCol int, hasInput bool) {
	now := r.now()
	elapsedSecs := nonNegativeSeconds(now.Sub(r.statusStart))
	var b strings.Builder
	fmt.Fprintf(&b, "[%s · %ds", r.statusLabel, elapsedSecs)
	if !r.promptStart.IsZero() {
		fmt.Fprintf(&b, " · total %ds", nonNegativeSeconds(now.Sub(r.promptStart)))
	}
	if r.statusCtxPct > 0 {
		fmt.Fprintf(&b, " · ctx %d%%", r.statusCtxPct)
	}
	b.WriteByte(']')
	maxW := r.outputWidth() - 1
	if r.statusInput == "" {
		return clipDisplayTail(b.String(), maxW), 0, false
	}
	prefix := b.String() + " > "
	text, cursorCol = clipStatusLine(prefix, sanitizeInputLine(r.statusInput), r.statusInputCursor, maxW)
	return text, cursorCol, true
}

// clipStatusLine fits the during-turn status line into maxW display columns and
// reports the 0-based terminal column for the edit cursor. prefix is the counter
// bracket plus the " > " separator; input is the already-sanitized typed buffer
// (newlines collapsed to spaces, so a rune index maps 1:1 to a display column);
// cursor is a rune index into input in [0, len(input)].
//
// When the whole line fits, it is shown verbatim with the cursor at its true
// column. When it overflows a horizontal window follows the cursor: tail-anchored
// while typing (a leading "…" marks the hidden head), scrolling left so the
// cursor stays visible (a trailing "…" marks the hidden tail) when the cursor
// moves ahead of that window. The window is always clamped to a single row so the
// \r\x1b[2K redraw and the cursor park stay correct.
func nonNegativeSeconds(d time.Duration) int {
	secs := int(d.Seconds())
	if secs < 0 {
		return 0
	}
	return secs
}

func clipStatusLine(prefix, input string, cursor int, maxW int) (text string, cursorCol int) {
	if maxW <= 0 {
		return "", 0
	}
	pre := []rune(prefix)
	in := []rune(input)
	if cursor < 0 {
		cursor = 0
	}
	if cursor > len(in) {
		cursor = len(in)
	}
	full := make([]rune, 0, len(pre)+len(in))
	full = append(full, pre...)
	full = append(full, in...)
	cursorRune := len(pre) + cursor

	if displayWidth(string(full)) <= maxW {
		return string(full), displayWidth(string(full[:cursorRune]))
	}

	// Tail-anchored window: include as many trailing runes as fit, reserving one
	// column for the leading "…".
	lo := len(full)
	width := 0
	for lo > 0 {
		w := runeWidth(full[lo-1])
		if width+w > maxW-1 {
			break
		}
		width += w
		lo--
	}
	hi := len(full)

	if cursorRune < lo {
		// The cursor is left of the tail window; re-anchor it to the first visible
		// rune and grow rightward to fill the budget, reserving a column for a
		// trailing "…" while hidden tail remains.
		lo = cursorRune
		budget := maxW
		if lo > 0 {
			budget-- // leading "…"
		}
		hi = lo
		width = 0
		for hi < len(full) {
			w := runeWidth(full[hi])
			reserve := 0
			if hi+1 < len(full) {
				reserve = 1 // room for a trailing "…"
			}
			if width+w+reserve > budget {
				break
			}
			width += w
			hi++
		}
	}

	var b strings.Builder
	if lo > 0 {
		b.WriteRune('…')
	}
	b.WriteString(string(full[lo:hi]))
	if hi < len(full) {
		b.WriteRune('…')
	}
	cursorCol = displayWidth(string(full[lo:cursorRune]))
	if lo > 0 {
		cursorCol++ // account for the leading "…"
	}
	return b.String(), cursorCol
}

// maybeWarnNoPrice emits a one-time-per-model notice when the active model has
// no configured price, so a silent $0 is never mistaken for "free" (r16).
func (r *Renderer) maybeWarnNoPrice() {
	if r.registry == nil || r.registry.HasPrice(r.model) {
		return
	}
	if r.warnedNoPrice[r.model] {
		return
	}
	if r.warnedNoPrice == nil {
		r.warnedNoPrice = map[string]bool{}
	}
	r.warnedNoPrice[r.model] = true
	r.dimLine(fmt.Sprintf("[note: no price configured for %q; cost is not reported for this model]", r.model))
}

// maybeWarnCompaction emits a one-time notice as the context fills toward the
// compaction threshold so a surprise compaction is foreshadowed (r27).
func (r *Renderer) maybeWarnCompaction(pct int) {
	if r.compactionWarned || pct < compactionWarnPercent {
		return
	}
	r.compactionWarned = true
	r.dimLine(fmt.Sprintf("[notice: context at %d%%; approaching compaction]", pct))
}

const compactionWarnPercent = 80

// contextPercent is the share of the model's context window in use, for the
// counter and the compaction notice (r27).
func contextPercent(ctx agent.ContextEstimate) int {
	if ctx.Window <= 0 {
		return 0
	}
	used := ctx.Total
	if ctx.PayloadTotal > used {
		used = ctx.PayloadTotal
	}
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

// cacheHitRatio is the percentage of input tokens served from cache (r15).
func cacheHitRatio(u llm.Usage) int {
	total := u.InputTokens + u.CacheReadTokens
	if total <= 0 {
		return 0
	}
	return u.CacheReadTokens * 100 / total
}

// sanitizeInputLine collapses control characters (notably the newlines that
// Enter inserts during a turn) to spaces so the multi-line buffer renders on the
// single counter row.
func sanitizeInputLine(s string) string {
	return strings.Map(func(r rune) rune {
		if r == '\n' || r == '\r' || r == '\t' || r < 0x20 {
			return ' '
		}
		return r
	}, s)
}

// clipDisplayTail trims s to at most max display columns, dropping leading runes
// (so the cursor end stays visible) and prefixing "…" when truncated. A
// conservative wide-rune width keeps East-Asian text from wrapping.
func clipDisplayTail(s string, max int) string {
	if max <= 0 {
		return ""
	}
	if displayWidth(s) <= max {
		return s
	}
	runes := []rune(s)
	width := 0
	i := len(runes)
	for i > 0 {
		w := runeWidth(runes[i-1])
		if width+w > max-1 { // reserve one column for the leading marker
			break
		}
		width += w
		i--
	}
	return "…" + string(runes[i:])
}

func displayWidth(s string) int {
	w := 0
	for _, r := range s {
		w += runeWidth(r)
	}
	return w
}

// runeWidth is a minimal display-cell width: 2 for the common East-Asian wide
// and emoji ranges, 1 otherwise. It is stdlib-only and intentionally
// approximate — enough to keep the counter from wrapping.
func runeWidth(r rune) int {
	if r == utf8.RuneError {
		return 1
	}
	switch {
	case r >= 0x1100 && r <= 0x115F, // Hangul Jamo
		r >= 0x2E80 && r <= 0x303E,   // CJK radicals, Kangxi
		r >= 0x3041 && r <= 0x33FF,   // Hiragana, Katakana, CJK symbols
		r >= 0x3400 && r <= 0x4DBF,   // CJK Ext A
		r >= 0x4E00 && r <= 0x9FFF,   // CJK Unified
		r >= 0xA000 && r <= 0xA4CF,   // Yi
		r >= 0xAC00 && r <= 0xD7A3,   // Hangul syllables
		r >= 0xF900 && r <= 0xFAFF,   // CJK compat
		r >= 0xFE30 && r <= 0xFE4F,   // CJK compat forms
		r >= 0xFF00 && r <= 0xFF60,   // Fullwidth forms
		r >= 0xFFE0 && r <= 0xFFE6,   // Fullwidth signs
		r >= 0x1F300 && r <= 0x1FAFF, // emoji & pictographs
		r >= 0x20000 && r <= 0x3FFFD: // CJK Ext B+
		return 2
	}
	return 1
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

func (r *Renderer) writeFinalSeparatorIfNeeded() {
	if r.assistantPhase != llm.AssistantPhaseFinal ||
		!r.visiblePreFinalOutput ||
		r.visibleFinalOutput ||
		r.finalSeparatorPrinted {
		return
	}
	r.finishAssistantLine()
	io.WriteString(r.out, finalAnswerSeparator)
	r.finalSeparatorPrinted = true
}

func (r *Renderer) markAssistantTextVisible() {
	switch r.assistantPhase {
	case llm.AssistantPhaseFinal:
		r.visibleFinalOutput = true
	case llm.AssistantPhaseCommentary:
		r.visiblePreFinalOutput = true
	}
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
func usageLine(u agent.TurnUsage, elapsed time.Duration, cost float64, costKnown bool, cumIn, cumOut int, cumCost float64) string {
	var b strings.Builder
	fmt.Fprintf(&b, "[turn: %s · %s (%s) in / %s (%s) out",
		modelTurnPhrase(u.ModelTurns),
		humanTokens(u.Usage.InputTokens), humanTokens(cumIn),
		humanTokens(u.Usage.OutputTokens), humanTokens(cumOut))
	// Cache reads and reasoning tokens are billed and material to cost; surface
	// them (with the cache-hit ratio) when non-zero (r15).
	if u.Usage.CacheReadTokens > 0 {
		fmt.Fprintf(&b, " · cache %s read", humanTokens(u.Usage.CacheReadTokens))
		if ratio := cacheHitRatio(u.Usage); ratio > 0 {
			fmt.Fprintf(&b, " (%d%%)", ratio)
		}
	}
	if u.Usage.ReasoningTokens > 0 {
		fmt.Fprintf(&b, " · %s reasoning", humanTokens(u.Usage.ReasoningTokens))
	}
	if costKnown {
		fmt.Fprintf(&b, " · $%.3f ($%.3f)", cost, cumCost)
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
