// Package ui drives the harness from stdin: a streaming renderer implementing
// the agent's EventSink, an interactive REPL with meta-commands, and a one-shot
// mode for piping a single prompt (design §10).
package ui

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"harness/internal/agent"
	"harness/internal/delegate"
	"harness/internal/llm"
	"harness/internal/markdown"
	"harness/internal/sessionrec"
	"harness/internal/term/highlight"
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

// submittedPromptRule is the separator drawn between a submitted REPL prompt and
// the model output that follows and between commentary and a final answer. It is
// a fixed, modest width rather than the terminal width so a later resize narrower
// never leaves an ugly over-long rule.
const submittedPromptRule = markdown.HorizontalRule

// RenderOptions configures a Renderer. Color is decided by the caller (TTY check
// plus NO_COLOR / -no-color); Now is injected so durations are
// deterministic in tests (design §10, §13).
type RenderOptions struct {
	Output                  *OutputCoordinator
	Color                   bool
	ColorTheme              highlight.Theme
	Markdown                bool
	Verbose                 bool
	ToolStream              bool
	Quiet                   bool
	SuppressReasoningOutput bool
	// SuppressUsage drops the per-prompt usage/cost line. It defaults false so
	// even -quiet runs still print the single cost line; set it for a fully
	// silent piped run (r25).
	SuppressUsage bool
	// LiveStatus enables the in-place wait-time counter and the during-prompt
	// input line (r12 + during-prompt input). Gated by the caller to an
	// interactive, non-quiet TTY; tests set it explicitly.
	LiveStatus               bool
	DelegateActivity         *delegate.ActivityRegistry
	DelegateFeed             *delegate.ActivityFeed
	DisableDelegateStatus    bool
	Model                    string
	Registry                 *llm.Registry
	CompactionTriggerPercent int
	DisableAutoCompaction    bool
	Now                      func() time.Time
	TimestampLayout          string
	Width                    func() int
}

// Renderer implements agent.EventSink: assistant text streams to out, while tool
// one-liners, the usage line, and notices go to errw so one-shot stdout carries
// only the model's answer (design §10).
type Renderer struct {
	output                  *OutputCoordinator
	out                     io.Writer
	errw                    io.Writer
	color                   bool
	colorTheme              highlight.Theme
	markdown                bool
	verbose                 bool
	toolStream              bool
	quiet                   bool
	suppressUsage           bool
	suppressReasoningOutput bool
	model                   string
	registry                *llm.Registry
	compactionWarnPercent   int
	disableAutoCompaction   bool
	now                     func() time.Time
	timestampLayout         string
	width                   func() int

	promptRunStart   time.Time
	currentTurnStart time.Time
	promptStart      time.Time

	renderMu          sync.Mutex // guards assistant/Markdown and logical live-status state
	assistantLineOpen bool
	assistantMarkdown *markdown.Stream
	assistantPhase    string
	activityBoundary  chan struct{}

	visiblePreFinalOutput bool
	visibleFinalOutput    bool
	finalSeparatorPrinted bool

	pending         map[string]llm.ToolCall // tool_use id -> call, awaiting its result
	pendingToolUses []string

	cumInput  int
	cumOutput int
	cumCost   float64

	largeRequestWarned bool

	// promptCost carries the per-prompt cost priced by the App against the
	// prompt's model (r63). When promptCostSet is false PromptComplete falls
	// back to pricing against r.model.
	promptCost      float64
	promptCostKnown bool
	promptCostSet   bool

	// warnedNoPrice tracks models for which the one-time "no price" notice has
	// already been emitted (r16).
	warnedNoPrice map[string]bool
	// compactionWarned guards the one-time "approaching compaction" notice (r27).
	compactionWarned bool

	// Live wait-time counter + during-prompt input line (r12 + during-prompt
	// input). renderMu guards every field below; OutputCoordinator serializes
	// the resulting physical terminal writes.
	liveStatus            bool
	delegateActivity      *delegate.ActivityRegistry
	disableDelegateStatus bool
	statusActive          bool                  // in a wait; the ticker should keep the line painted
	statusDrawn           bool                  // a status line is currently on the terminal
	statusLabel           string                // e.g. "turn: 3" or "tool: grep args=[\"x\"]"
	statusStart           time.Time             // when the current wait began
	statusCtx             agent.ContextEstimate // context usage to append for model waits (r27)
	statusProgress        any                   // foreground delegate live-progress closure, or nil
	statusBgProgress      []any                 // background delegate progress closures while joining
	statusModel           string                // proxy correlation/retry/cancellation state
	statusInput           string                // during-prompt typed buffer shown after "> "
	statusInputCursor     int                   // rune index of the edit cursor within statusInput
	ticker                *time.Ticker
	tickerStop            chan struct{}
	tickerDone            chan struct{}

	delegateFeed    *delegate.ActivityFeed
	activityMu      sync.Mutex
	activityCursor  uint64
	activityControl chan activityControl
	activityDone    chan struct{}
}

type activityControl struct {
	through uint64
	stop    bool
	ack     chan struct{}
}

// NewRenderer builds a Renderer. A nil Now defaults to time.Now.
func NewRenderer(out, errw io.Writer, opts RenderOptions) *Renderer {
	now := opts.Now
	if now == nil {
		now = time.Now
	}
	output := opts.Output
	if output == nil {
		output = NewOutputCoordinator(out, errw)
	}
	activityFeed := opts.DelegateFeed
	if opts.Quiet {
		activityFeed = nil
	}
	return &Renderer{
		output:                  output,
		out:                     output.Stdout(),
		errw:                    output.Stderr(),
		color:                   opts.Color,
		colorTheme:              opts.ColorTheme,
		markdown:                opts.Markdown,
		verbose:                 opts.Verbose,
		toolStream:              opts.ToolStream,
		quiet:                   opts.Quiet,
		suppressReasoningOutput: opts.SuppressReasoningOutput,
		suppressUsage:           opts.SuppressUsage,
		liveStatus:              opts.LiveStatus && !opts.Quiet,
		delegateActivity:        opts.DelegateActivity,
		delegateFeed:            activityFeed,
		disableDelegateStatus:   opts.DisableDelegateStatus,
		model:                   opts.Model,
		registry:                opts.Registry,
		compactionWarnPercent:   resolvedCompactionWarnPercent(opts.CompactionTriggerPercent),
		disableAutoCompaction:   opts.DisableAutoCompaction,
		now:                     now,
		timestampLayout:         opts.TimestampLayout,
		width:                   opts.Width,
		pending:                 make(map[string]llm.ToolCall),
		activityBoundary:        make(chan struct{}),
	}
}

// StartPrompt records when the last user prompt was submitted. The live status
// line uses it for the total elapsed time across all model/tool waits in the
// prompt.
func (r *Renderer) StartPrompt() {
	now := r.now()
	r.renderMu.Lock()
	r.promptStart = now
	r.renderMu.Unlock()
	r.startActivityConsumer()
}

// StartPromptRun records the prompt execution start for its usage line.
// The driver calls it immediately before agent.RunPrompt. If StartPrompt was not
// called (older tests and direct callers), the prompt total starts here too.
func (r *Renderer) StartPromptRun() {
	now := r.now()
	r.promptRunStart = now
	r.currentTurnStart = time.Time{}
	r.renderMu.Lock()
	if r.promptStart.IsZero() {
		r.promptStart = now
	}
	r.renderMu.Unlock()
	r.largeRequestWarned = false
	r.compactionWarned = false
	r.promptCost = 0
	r.promptCostKnown = false
	r.promptCostSet = false
	r.renderMu.Lock()
	r.assistantMarkdown = nil
	r.assistantLineOpen = false
	r.assistantPhase = ""
	r.visiblePreFinalOutput = false
	r.visibleFinalOutput = false
	r.finalSeparatorPrinted = false
	r.signalActivityBoundaryLocked()
	r.renderMu.Unlock()
}

// SetModel updates the model used for subsequent usage/cost summaries.
func (r *Renderer) SetModel(model string) { r.model = model }

// SetCumulativeUsage seeds the session totals used by per-prompt usage lines.
func (r *Renderer) SetCumulativeUsage(inputTokens, outputTokens int, costUSD float64) {
	r.cumInput = inputTokens
	r.cumOutput = outputTokens
	r.cumCost = costUSD
}

// FormatMarkdown renders a complete Markdown block using the same enablement,
// ANSI, and terminal-width policy as streamed assistant text.
func (r *Renderer) FormatMarkdown(text string) string {
	return markdown.Render(text, markdown.Options{
		Enabled:    r.markdown,
		ANSI:       r.color,
		ColorTheme: r.colorTheme,
		Width:      r.outputWidth(),
	})
}

func (r *Renderer) TextDelta(text string) {
	if text == "" {
		return
	}
	r.renderMu.Lock()
	r.statusClearLocked()
	r.writeFinalSeparatorIfNeededLocked()
	if r.markdown {
		r.ensureAssistantMarkdownLocked()
		io.WriteString(r.out, r.assistantMarkdown.Write(text))
		r.assistantLineOpen = r.assistantMarkdown.LineOpen()
	} else {
		io.WriteString(r.out, text)
		r.assistantLineOpen = !strings.HasSuffix(text, "\n")
	}
	r.markAssistantTextVisibleLocked()
	lineOpen := r.assistantLineOpen
	r.signalActivityBoundaryLocked()
	r.renderMu.Unlock()
	r.resumeLiveModelWaitAfterAssistantText(text, lineOpen)
}

func (r *Renderer) AssistantPhase(phase string) {
	if !llm.ValidAssistantPhase(phase) || phase == "" {
		return
	}
	r.renderMu.Lock()
	r.assistantPhase = phase
	r.renderMu.Unlock()
}

func (r *Renderer) ReasoningSummary(text string) {
	if r.suppressReasoningOutput {
		return
	}
	r.flushToolUseStarts()
	r.renderMu.Lock()
	defer r.renderMu.Unlock()
	r.statusClearLocked()
	r.finishAssistantLineLocked()
	block := r.reasoningSummaryBlock(text)
	if block == "" {
		return
	}
	io.WriteString(r.out, block)
	r.visiblePreFinalOutput = true
	r.signalActivityBoundaryLocked()
}

func (r *Renderer) ReasoningSummaryStatus(text string) {
	if r.suppressReasoningOutput {
		return
	}
	r.flushToolUseStarts()
	r.renderMu.Lock()
	defer r.renderMu.Unlock()
	r.statusClearLocked()
	r.finishAssistantLineLocked()
	io.WriteString(r.errw, r.reasoningSummaryBlock(text))
	r.signalActivityBoundaryLocked()
}

func (r *Renderer) TurnAttemptStart(turn, attempt int, ctx agent.ContextEstimate) {
	r.flushToolUseStarts()
	r.renderMu.Lock()
	r.statusModel = ""
	r.renderMu.Unlock()
	if attempt <= 1 {
		r.currentTurnStart = r.now()
	}
	if attempt <= 1 && !r.largeRequestWarned {
		if line := largeRequestWarning(ctx); line != "" {
			r.dimLine(line)
			r.largeRequestWarned = true
		}
	}
	pct := contextPercent(ctx)
	r.maybeWarnCompaction(pct)
	if r.liveStatus {
		label := fmt.Sprintf("turn: %d", turn)
		if attempt > 1 {
			label = fmt.Sprintf("turn: %d attempt %d", turn, attempt)
		}
		r.beginWait(label, ctx)
		return
	}
	if attempt <= 1 {
		r.dimLine(fmt.Sprintf("[turn: %d waiting]", turn))
		return
	}
	r.dimLine(fmt.Sprintf("[turn: %d attempt %d waiting]", turn, attempt))
}

func (r *Renderer) TurnAttemptComplete(usage agent.TurnAttemptUsage) {
	r.finishTurnAttempt(usage)
}

// ModelRequestEvent renders and tracks diagnostics-only provider lifecycle
// state. It returns the durable display line, if any, for raw session replay.
func (r *Renderer) ModelRequestEvent(event llm.ModelRequestEvent) string {
	// A previous-response/interaction rejection is terminal for this proxy
	// request, but the agent can discard the stale anchor and resend full
	// context immediately. Do not eagerly render it as a terminal prompt
	// error. The structured event is still persisted and logged by the sink;
	// if recovery does not succeed, the caller's final error path renders the
	// returned error normally.
	line := sessionrec.ModelRequestDisplayLine(event)
	if line != "" {
		if event.Outcome == llm.ModelRequestOutcomeTerminal {
			r.writeDimLine(line)
		} else {
			r.dimLine(line)
		}
	}

	activity := r.delegateActivitySnapshot()
	r.renderMu.Lock()
	r.statusModel = modelRequestStatus(event)
	if r.liveStatus && r.statusLabel != "" {
		r.statusActive = true
		r.ensureTickerLocked()
		r.paintLocked(activity)
	}
	r.renderMu.Unlock()
	return line
}

// CancelRequested immediately makes a graceful cancellation visible while the
// HTTP/provider stack unwinds.
func (r *Renderer) CancelRequested() {
	activity := r.delegateActivitySnapshot()
	r.renderMu.Lock()
	r.statusModel = "cancelling; Ctrl-C again to force exit"
	if r.liveStatus && r.statusLabel != "" {
		r.statusActive = true
		r.ensureTickerLocked()
		r.paintLocked(activity)
	}
	r.renderMu.Unlock()
}

// CompactionStart begins a transient live wait while old context is summarized.
// Non-live and quiet renderers intentionally produce no fallback line.
func (r *Renderer) CompactionStart() {
	r.beginWait("context: compacting", agent.ContextEstimate{})
}

// CompactionComplete ends only the current wait phase. It leaves the turn-wide
// ticker, prompt timer, and during-prompt input intact so automatic compaction can
// transition directly into the next model wait.
func (r *Renderer) CompactionComplete() {
	r.endWait()
}

// PromptWorkWaitStart begins a transient live wait while join-required
// background delegates finish.
func (r *Renderer) PromptWorkWaitStart() {
	r.beginWait("background: waiting for delegates", agent.ContextEstimate{})
}

// SetBackgroundProgress attaches the live-progress closures of the outstanding
// background delegate jobs so the wait ticker can summarize their activity while
// the parent is blocked joining them. A nil slice clears them. progress entries
// are opaque func() agent.DelegateProgressSnapshot closures type-asserted at
// render time; entries that are not the expected closure type are skipped.
func (r *Renderer) SetBackgroundProgress(progress []any) {
	if len(progress) == 0 {
		r.drainActivity()
	}
	if !r.liveStatus {
		return
	}
	r.renderMu.Lock()
	r.statusBgProgress = progress
	r.renderMu.Unlock()
}

// SetToolProgress attaches (progress != nil) or clears (nil) the live-progress
// closure for the currently running foreground tool call so its wait ticker can
// show child-run activity. progress is an opaque func() agent.DelegateProgressSnapshot
// closure type-asserted at render time.
func (r *Renderer) SetToolProgress(name string, progress any) {
	if progress == nil {
		r.drainActivity()
	}
	if !r.liveStatus {
		return
	}
	r.renderMu.Lock()
	r.statusProgress = progress
	r.renderMu.Unlock()
}

// PromptWorkWaitComplete clears the background-work wait before the parent
// resumes model work or completes the prompt.
func (r *Renderer) PromptWorkWaitComplete() {
	r.drainActivity()
	r.endWait()
}

// HandoffSummaryStart begins a transient live wait while /handoff generates the
// planning brief shown for approval.
func (r *Renderer) HandoffSummaryStart() {
	r.beginWait("handoff: generating brief", agent.ContextEstimate{})
}

// HandoffSummaryComplete clears the handoff-summary wait before the brief or an
// error is printed.
func (r *Renderer) HandoffSummaryComplete() {
	r.endWait()
}

func (r *Renderer) finishTurnAttempt(usage agent.TurnAttemptUsage) {
	defer r.flushToolUseStarts()
	if !usage.Usage.CostKnown {
		r.maybeWarnNoPrice()
		return
	}
	// Successful conversational attempts are summarized at the logical turn
	// boundary. Retry costs remain available in the persisted attempt event.
}

// TurnComplete closes one conversational turn without ending the surrounding
// prompt. It returns the persisted display line.
func (r *Renderer) TurnComplete(usage agent.TurnUsage) string {
	r.endWait()
	r.flushToolUseStarts()
	r.finishAssistantLine()
	line := turnUsageLine(usage, r.now().Sub(r.currentTurnStart), r.now().Sub(r.promptStart))
	r.dimLine(line)
	return line
}

func (r *Renderer) ToolUseStart(call llm.ToolCall) {
	// A streamed tool call is still model work: after visible commentary the
	// provider may spend seconds generating hidden function-call arguments and the
	// final usage frame before local tool execution can begin. Resume the live
	// counter for that gap so the CLI does not look idle between model output and
	// tool-start lines.
	if r.liveStatus {
		label := "turn: tool call"
		if call.Name != "" {
			label = "turn: tool call " + call.Name
		}
		r.beginWait(label, agent.ContextEstimate{})
	}
	if !r.toolProgress() || r.quiet {
		return
	}
	r.pendingToolUses = append(r.pendingToolUses, fmt.Sprintf("[tool-call: %s id=%s]", call.Name, call.ID))
}

func (r *Renderer) ToolUseDelta(_ int, _ string) {}

func (r *Renderer) resumeLiveModelWaitAfterAssistantText(delta string, lineOpen bool) {
	// Markdown buffers an incomplete source line, so LineOpen remains false while
	// a token is merely pending. Restarting the status line then would flush that
	// token and finish it with a newline. A newline-terminated delta guarantees
	// there is no partial source line for beginWait to flush.
	if !r.liveStatus || lineOpen || !strings.HasSuffix(delta, "\n") {
		return
	}
	r.renderMu.Lock()
	label := r.statusLabel
	statusCtx := r.statusCtx
	r.renderMu.Unlock()
	if label != "" {
		r.beginWait(label, statusCtx)
	}
}

// ToolStart stashes the call so ToolResult can render name+args+summary on one
// line once the result is known.
func (r *Renderer) ToolStart(call llm.ToolCall) {
	r.flushToolUseStarts()
	r.pending[call.ID] = call
	if r.toolProgress() {
		r.dimLine(fmt.Sprintf("[tool: %s started%s]", call.Name, formatToolArgs(call.Name, call.Input)))
	}
	// Tick during the (possibly long) tool-execution gap, not just model
	// waits (r12). The next output line erases this counter again.
	if r.liveStatus {
		r.beginWait(fmt.Sprintf("tool: %s%s", call.Name, formatToolArgs(call.Name, call.Input)), agent.ContextEstimate{})
	}
}

func (r *Renderer) ToolResult(result llm.ToolResult) {
	r.drainActivity()
	r.flushToolUseStarts()
	call := r.pending[result.ForID]
	delete(r.pending, result.ForID)

	r.dimLine(ToolResultLine(call, result))

	if r.verbose {
		for _, s := range snippet(result.Text) {
			r.dimLine("  " + s)
		}
		for _, image := range result.Content {
			r.dimLine("  " + richImageMetadata(image))
		}
	}
}

func (r *Renderer) ToolDiff(_ llm.ToolCall, path, text string) {
	r.flushToolUseStarts()
	r.renderMu.Lock()
	defer r.renderMu.Unlock()
	r.statusClearLocked()
	r.finishAssistantLineLocked()
	if text == "" {
		return
	}
	if r.color {
		text = colorizeDiff(path, text, r.colorTheme)
	}
	io.WriteString(r.errw, text)
	if !strings.HasSuffix(text, "\n") {
		fmt.Fprintln(r.errw)
	}
}

// colorizeDiff syntax-highlights a unified diff: added and removed lines get
// a tinted background with a colored sigil, and line content is highlighted
// in the mutated file's language (plain when the language is unknown).
func colorizeDiff(path, text string, theme highlight.Theme) string {
	return highlight.ColorizeDiffWithTheme(path, text, theme)
}

func (r *Renderer) toolProgress() bool {
	return r.toolStream || r.verbose
}

func (r *Renderer) Notice(msg string) {
	r.flushToolUseStarts()
	r.dimLine(msg)
}

func (r *Renderer) PromptComplete(usage agent.PromptUsage) {
	r.StopProgress()
	r.flushToolUseStarts()
	r.finishAssistantLine()
	elapsed := r.now().Sub(r.promptRunStart)

	// Accumulate session totals for the cumulative readout. The App prices the
	// prompt against its own model and forwards it via SetPromptCost so a mid-prompt
	// model switch is not mispriced against the renderer's model (r63).
	cost, costKnown := usage.Usage.CostUSD, usage.Usage.CostKnown
	if r.promptCostSet {
		cost, costKnown = r.promptCost, r.promptCostKnown
	}
	r.cumInput += usage.Usage.InputTokens
	r.cumOutput += usage.Usage.OutputTokens
	if costKnown {
		r.cumCost += cost
	}

	r.usageOutput(usageLine(usage, elapsed, cost, costKnown, r.cumInput, r.cumOutput, r.cumCost))
}

// SetPromptCost records the prompt cost priced by the App against its own
// model, consumed by the next PromptComplete (r63).
func (r *Renderer) SetPromptCost(cost float64, known bool) {
	r.promptCost = cost
	r.promptCostKnown = known
	r.promptCostSet = true
}

// usageOutput writes the per-prompt usage line. It honours -quiet only when
// SuppressUsage is set, so a plain -quiet run still prints the single cost line
// (r25). The status line is cleared first so the counter never lingers above it.
func (r *Renderer) usageOutput(line string) {
	if r.suppressUsage {
		return
	}
	r.renderMu.Lock()
	defer r.renderMu.Unlock()
	r.statusClearLocked()
	r.finishAssistantLineLocked()
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
	r.writeDimLine(s)
}

// writeDimLine writes a status-shaped line even in quiet mode. Terminal model
// errors use it because quiet suppresses progress, not actionable failures.
func (r *Renderer) writeDimLine(s string) {
	r.renderMu.Lock()
	defer r.renderMu.Unlock()
	r.statusClearLocked()
	r.finishAssistantLineLocked()
	s = r.timestampStatusLine(s)
	if r.color {
		fmt.Fprintf(r.errw, "%s%s%s\n", ansiDim, s, ansiReset)
		return
	}
	fmt.Fprintln(r.errw, s)
}

// SubmittedPromptSeparator writes the separator between a submitted REPL prompt
// and the model output that follows: a single dim rule (submittedPromptRule)
// followed by a newline. Because it is a real scrolled line rather than the
// transient wait counter, it survives the counter's in-place erase and so still
// separates the prompt from streamed output after the counter is replaced. The
// rule is dimmed only when color is on. Unlike status lines, this separator is
// structural and is printed even in quiet mode.
func (r *Renderer) SubmittedPromptSeparator() {
	r.renderMu.Lock()
	defer r.renderMu.Unlock()
	r.statusClearLocked()
	r.finishAssistantLineLocked()
	fmt.Fprintln(r.errw, r.separatorRule())
}

func (r *Renderer) separatorRule() string {
	if r.color {
		return ansiDim + submittedPromptRule + ansiReset
	}
	return submittedPromptRule
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
		Enabled:    true,
		ANSI:       r.color,
		ColorTheme: r.colorTheme,
		Width:      r.outputWidth(),
		Prefix:     "  ",
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

// --- Live wait-time counter + during-prompt input line (r12 + during-prompt input) ---
//
// The counter is a single transient status line repainted in place with
// \r\x1b[2K (no scroll region, no sticky bar). beginWait activates it; any
// scrolling write erases it under renderMu before touching out/errw. A
// time.Ticker repaints it ~1/sec while
// a wait is in progress; the ticker and the foreground writers are serialised by
// renderMu and a stop-and-drain handshake so their logical state cannot race.

func (r *Renderer) startActivityConsumer() {
	if r.delegateFeed == nil {
		return
	}
	r.activityMu.Lock()
	running := r.activityDone != nil
	r.activityMu.Unlock()
	if running {
		r.stopActivityConsumer()
	}

	tail := r.delegateFeed.Tail()
	control := make(chan activityControl, 1)
	done := make(chan struct{})
	r.activityMu.Lock()
	if tail > r.activityCursor {
		r.activityCursor = tail
	}
	r.activityControl = control
	r.activityDone = done
	r.activityMu.Unlock()
	go r.consumeActivity(control, done)
}

func (r *Renderer) drainActivity() {
	r.controlActivity(false)
}

func (r *Renderer) stopActivityConsumer() {
	r.controlActivity(true)
}

func (r *Renderer) controlActivity(stop bool) {
	if r.delegateFeed == nil {
		return
	}
	through := r.delegateFeed.Tail()
	r.activityMu.Lock()
	control, done := r.activityControl, r.activityDone
	if done == nil {
		if through > r.activityCursor {
			r.activityCursor = through
		}
		r.activityMu.Unlock()
		return
	}
	r.activityMu.Unlock()

	ack := make(chan struct{})
	request := activityControl{through: through, stop: stop, ack: ack}
	select {
	case control <- request:
	case <-done:
		return
	}
	select {
	case <-ack:
		if stop {
			<-done
		}
	case <-done:
	}
}

func (r *Renderer) consumeActivity(control <-chan activityControl, done chan struct{}) {
	defer func() {
		r.activityMu.Lock()
		if r.activityDone == done {
			r.activityControl = nil
			r.activityDone = nil
		}
		r.activityMu.Unlock()
		close(done)
	}()

	for {
		cursor := r.currentActivityCursor()
		batch := r.delegateFeed.ReadAfter(cursor, 0)
		if len(batch.Items) > 0 {
			written, boundary := r.writeActivityBatch(batch)
			if written {
				r.setActivityCursor(batch.Through)
				continue
			}
			select {
			case request := <-control:
				if r.handleActivityControl(request) {
					return
				}
			case <-batch.Changed:
			case <-boundary:
			}
			continue
		}

		_, boundary := r.activityBoundaryState()
		select {
		case request := <-control:
			if r.handleActivityControl(request) {
				return
			}
		case <-batch.Changed:
		case <-boundary:
		}
	}
}

func (r *Renderer) handleActivityControl(request activityControl) bool {
	cursor := r.currentActivityCursor()
	for cursor < request.through {
		batch := r.delegateFeed.ReadAfter(cursor, request.through)
		if len(batch.Items) == 0 {
			break
		}
		written, _ := r.writeActivityBatch(batch)
		if !written {
			break
		}
		cursor = batch.Through
		r.setActivityCursor(cursor)
	}
	if request.stop && cursor < request.through {
		r.setActivityCursor(request.through)
	}
	close(request.ack)
	return request.stop
}

func (r *Renderer) currentActivityCursor() uint64 {
	r.activityMu.Lock()
	defer r.activityMu.Unlock()
	return r.activityCursor
}

func (r *Renderer) setActivityCursor(cursor uint64) {
	r.activityMu.Lock()
	if cursor > r.activityCursor {
		r.activityCursor = cursor
	}
	r.activityMu.Unlock()
}

func (r *Renderer) writeActivityBatch(batch delegate.FeedBatch) (bool, <-chan struct{}) {
	r.renderMu.Lock()
	defer r.renderMu.Unlock()
	if !r.assistantAtLineBoundaryLocked() {
		return false, r.activityBoundary
	}
	_ = r.output.WriteActivity([]byte(formatActivityBatch(batch)))
	return true, r.activityBoundary
}

func (r *Renderer) activityBoundaryState() (bool, <-chan struct{}) {
	r.renderMu.Lock()
	defer r.renderMu.Unlock()
	return r.assistantAtLineBoundaryLocked(), r.activityBoundary
}

func (r *Renderer) assistantAtLineBoundaryLocked() bool {
	if r.assistantLineOpen {
		return false
	}
	return r.assistantMarkdown == nil || r.assistantMarkdown.AtLineBoundary()
}

func (r *Renderer) signalActivityBoundaryLocked() {
	if !r.assistantAtLineBoundaryLocked() {
		return
	}
	close(r.activityBoundary)
	r.activityBoundary = make(chan struct{})
}

func formatActivityBatch(batch delegate.FeedBatch) string {
	var out strings.Builder
	for _, item := range batch.Items {
		if item.Kind == delegate.FeedItemGap {
			count := item.Gap.Last - item.Gap.First + 1
			noun := "events"
			if count == 1 {
				noun = "event"
			}
			fmt.Fprintf(&out, "[delegate output] omitted %d %s\n", count, noun)
			continue
		}
		if item.Kind != delegate.FeedItemEvent {
			continue
		}
		event := item.Event
		prefix := event.DisplayID
		if prefix == "" {
			prefix = event.ChildID
		}
		if prefix == "" {
			prefix = "?"
		}
		agentName := event.Agent
		if agentName == "" {
			agentName = "auto"
		}
		fmt.Fprintf(&out, "[delegate %s", prefix)
		fmt.Fprintf(&out, " %s", agentName)
		if event.Depth > 1 {
			fmt.Fprintf(&out, " depth=%d", event.Depth)
		}
		out.WriteString("] ")
		switch event.Kind {
		case delegate.ActivityEventStart:
			out.WriteString("started")
			if event.TranscriptPath != "" {
				fmt.Fprintf(&out, " · transcript %s", event.TranscriptPath)
			}
		case delegate.ActivityEventTurnStart:
			fmt.Fprintf(&out, "turn %d started", event.Turn)
			if event.Attempt > 1 {
				fmt.Fprintf(&out, " · attempt %d", event.Attempt)
			}
		case delegate.ActivityEventAssistant:
			if event.Continuation {
				out.WriteString("assistant+: ")
			} else {
				out.WriteString("assistant: ")
			}
			out.WriteString(event.Text)
		case delegate.ActivityEventReasoning:
			if event.Continuation {
				out.WriteString("reasoning+: ")
			} else {
				out.WriteString("reasoning: ")
			}
			out.WriteString(event.Text)
		case delegate.ActivityEventToolStart:
			fmt.Fprintf(&out, "%s started", event.Text)
		case delegate.ActivityEventToolComplete:
			fmt.Fprintf(&out, "%s completed", event.Text)
		case delegate.ActivityEventToolError:
			fmt.Fprintf(&out, "%s failed", event.Text)
		case delegate.ActivityEventNotice:
			fmt.Fprintf(&out, "notice: %s", event.Text)
		case delegate.ActivityEventRetry, delegate.ActivityEventModelIssue:
			out.WriteString(event.Text)
		case delegate.ActivityEventAttemptDiscarded:
			fmt.Fprintf(&out, "turn %d attempt %d discarded; retrying", event.Turn, event.Attempt)
		case delegate.ActivityEventTerminal:
			status := event.Status
			if status == "" {
				status = "finished"
			}
			out.WriteString(status)
			if event.Turn > 0 {
				fmt.Fprintf(&out, " · %s", turnCount(event.Turn))
			}
		default:
			out.WriteString(event.Text)
		}
		out.WriteByte('\n')
	}
	return out.String()
}

func turnCount(turns int) string {
	if turns == 1 {
		return "1 turn"
	}
	return fmt.Sprintf("%d turns", turns)
}

// beginWait activates (or refreshes) the transient counter for a model wait or a
// tool-execution gap. It finishes any open assistant line first so the counter
// sits on its own row and erasing it never clobbers streamed content.
func (r *Renderer) beginWait(label string, statusCtx agent.ContextEstimate) {
	if !r.liveStatus {
		return
	}
	r.finishAssistantLine()
	activity := r.delegateActivitySnapshot()
	r.renderMu.Lock()
	r.statusLabel = label
	r.statusStart = r.now()
	r.statusCtx = statusCtx
	r.statusActive = true
	r.ensureTickerLocked()
	r.paintLocked(activity)
	r.renderMu.Unlock()
}

// statusClearLocked erases and deactivates the on-screen counter before a
// scrolling write. Caller holds renderMu.
func (r *Renderer) statusClearLocked() {
	if !r.liveStatus {
		return
	}
	r.eraseLocked()
	r.statusActive = false
}

// endWait erases and deactivates a completed phase while preserving turn-wide
// status state. Unlike statusClear, it clears the label so later assistant text
// cannot resume a phase that has already completed.
func (r *Renderer) endWait() {
	if !r.liveStatus {
		return
	}
	r.renderMu.Lock()
	r.eraseLocked()
	r.statusActive = false
	r.statusLabel = ""
	r.statusCtx = agent.ContextEstimate{}
	r.statusProgress = nil
	r.statusBgProgress = nil
	r.statusModel = ""
	r.renderMu.Unlock()
}

// SetInputLine updates the during-prompt typed buffer shown after "> " on the
// counter line, along with the rune index of the edit cursor, and repaints if a
// wait is active. Empty restores the bare counter. cursor is clamped into
// [0, len(buf)] so a stale index never positions the terminal cursor off-row.
func (r *Renderer) SetInputLine(buf string, cursor int) {
	if !r.liveStatus {
		return
	}
	activity := r.delegateActivitySnapshot()
	r.renderMu.Lock()
	r.statusInput = buf
	r.statusInputCursor = cursor
	if r.statusActive {
		r.paintLocked(activity)
	}
	r.renderMu.Unlock()
}

// StopProgress erases the counter, clears the typed buffer, and stops the ticker,
// draining its goroutine so no stray repaint can follow. Idempotent; called at
// every prompt boundary (PromptComplete, the REPL prompt-done handoff, and one-shot).
func (r *Renderer) StopProgress() {
	r.finishAssistantLine()
	r.stopActivityConsumer()
	if !r.liveStatus {
		return
	}
	r.renderMu.Lock()
	r.eraseLocked()
	r.statusActive = false
	r.statusInput = ""
	r.statusInputCursor = 0
	r.statusLabel = ""
	r.statusCtx = agent.ContextEstimate{}
	r.statusProgress = nil
	r.statusBgProgress = nil
	r.statusModel = ""
	r.promptStart = time.Time{}
	t, stop, done := r.ticker, r.tickerStop, r.tickerDone
	r.ticker, r.tickerStop, r.tickerDone = nil, nil, nil
	r.renderMu.Unlock()
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
// same mutex) is a no-op. Registry state is snapshotted before the terminal
// mutex so delegate publishers never wait for terminal rendering.
func (r *Renderer) tick() {
	activity := r.delegateActivitySnapshot()
	r.renderMu.Lock()
	if r.statusActive {
		r.paintLocked(activity)
	}
	r.renderMu.Unlock()
}

// eraseLocked clears a drawn counter line. Caller holds renderMu.
func (r *Renderer) eraseLocked() {
	if !r.statusDrawn {
		return
	}
	r.output.ClearStatus()
	r.statusDrawn = false
}

// paintLocked redraws the counter in place. Caller holds renderMu. When a
// during-prompt buffer is present it parks the terminal cursor at the edit column
// on the same single row, so cursor-motion keys (arrows/Home/End) land visibly.
func (r *Renderer) paintLocked(activity delegate.ActivitySnapshot) {
	text, cursorCol, hasInput := r.statusTextLocked(activity)
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
	r.output.SetStatus([]byte(b.String()))
	r.statusDrawn = true
}

// statusTextLocked renders the counter, clipped to the terminal width so it
// never wraps (a wrapped line would defeat the single-line \r\x1b[2K erase). It
// also reports the terminal column for the during-prompt edit cursor and whether a
// typed buffer is present. Caller holds renderMu; activity was snapshotted before
// taking that mutex.
func (r *Renderer) statusTextLocked(activity delegate.ActivitySnapshot) (text string, cursorCol int, hasInput bool) {
	now := r.now()
	var base strings.Builder
	var compactBase strings.Builder
	var suffix strings.Builder
	if r.statusActive && r.statusLabel != "" {
		elapsedSecs := nonNegativeSeconds(now.Sub(r.statusStart))
		fmt.Fprintf(&base, "[%s · %ds", r.statusLabel, elapsedSecs)
		fmt.Fprintf(&compactBase, "[%s · %ds", compactWaitLabel(r.statusLabel), elapsedSecs)
		if used := contextUsed(r.statusCtx); r.statusCtx.Window > 0 && used > 0 {
			fmt.Fprintf(&base, " · ctx %d%% %s/%s", contextPercent(r.statusCtx), humanTokens(used), humanTokens(r.statusCtx.Window))
		}
		if r.statusModel != "" {
			fmt.Fprintf(&suffix, " · %s", r.statusModel)
		}
	} else {
		// A background child can outlive the parent prompt. In that state the
		// registry owns the row and no stale parent timing fields are displayed.
		base.WriteByte('[')
	}
	// The session total is set off from the current-turn fields with a distinct
	// "│" divider and placed last so the turn's own elapsed time and the running
	// prompt total are easy to tell apart at a glance.
	if r.statusActive && !r.promptStart.IsZero() {
		fmt.Fprintf(&suffix, " │ prompt %ds", nonNegativeSeconds(now.Sub(r.promptStart)))
	}

	maxW := r.outputWidth() - 1
	activeAuthoritative := r.delegateActivity != nil
	if activeAuthoritative && len(activity.Active) > 0 && !r.disableDelegateStatus {
		status, label := fitActiveDelegateStatus(base.String(), compactBase.String(), suffix.String(), activity, maxW)
		if r.statusInput == "" {
			return status, 0, false
		}
		input := sanitizeInputLine(r.statusInput)
		if displayWidth(status)+3+displayWidth(input) <= maxW {
			text, cursorCol = clipStatusLine(status+" > ", input, r.statusInputCursor, maxW)
			return text, cursorCol, true
		}
		return clipDelegateStatusInput(label, input, r.statusInputCursor, maxW)
	}

	var b strings.Builder
	b.WriteString(base.String())
	// The shared registry is authoritative whenever configured. Opaque progress
	// closures remain only as compatibility fallback for isolated renderers.
	if !r.disableDelegateStatus && !activeAuthoritative {
		if len(r.statusBgProgress) > 0 {
			writeBackgroundProgress(&b, r.statusBgProgress)
		} else if r.statusProgress != nil {
			writeDelegateProgress(&b, r.statusProgress)
		}
	}
	b.WriteString(suffix.String())
	b.WriteByte(']')
	if r.statusInput == "" {
		return clipDisplayTail(b.String(), maxW), 0, false
	}
	prefix := b.String() + " > "
	text, cursorCol = clipStatusLine(prefix, sanitizeInputLine(r.statusInput), r.statusInputCursor, maxW)
	return text, cursorCol, true
}

func (r *Renderer) delegateActivitySnapshot() delegate.ActivitySnapshot {
	if r.delegateActivity == nil || r.disableDelegateStatus {
		return delegate.ActivitySnapshot{}
	}
	return r.delegateActivity.Snapshot()
}

// compactWaitLabel removes tool arguments from the narrow-row fallback. The
// full label remains preferred, but an elapsed counter is more useful than a
// static delegate identity when arguments consume the whole terminal width.
func compactWaitLabel(label string) string {
	const toolPrefix = "tool: "
	rest, ok := strings.CutPrefix(label, toolPrefix)
	if !ok {
		return label
	}
	name, _, _ := strings.Cut(rest, " ")
	if name == "" {
		return label
	}
	return toolPrefix + name
}

// fitActiveDelegateStatus gives the current delegate identity priority over its
// activity body and optional model/prompt fields. If a full tool label is too
// wide, it uses compactBase so the current wait's elapsed counter remains
// visible instead of collapsing to a static delegate identity. Normal grammar
// is:
//
//	· delegate d1 explore: turn 2 · tool read_file
//	· 3 delegates · latest d4 plan: turn 1 · thinking
//
// The greatest registry activity sequence selects the displayed child; equal
// sequences are already broken deterministically by child ID in the registry.
func fitActiveDelegateStatus(base, compactBase, suffix string, snapshot delegate.ActivitySnapshot, maxW int) (string, string) {
	latest := snapshot.Recent
	label := strings.TrimSpace(latest.DisplayID + " " + latest.Agent)
	if label == "" {
		label = "delegate"
	}
	separator := " · "
	if base == "[" {
		separator = ""
	}
	marker := separator + "delegate "
	if len(snapshot.Active) > 1 {
		marker = fmt.Sprintf("%s%d delegates · latest ", separator, len(snapshot.Active))
	}
	body := activeDelegateBody(latest)
	ending := suffix + "]"
	fixed := base + marker + label
	if body != "" {
		full := fixed + ": " + body + ending
		if displayWidth(full) <= maxW {
			return full, label
		}
		// Activity is the first field clipped; the display ID and agent survive.
		budget := maxW - displayWidth(fixed+": "+ending)
		if budget > 0 {
			return fixed + ": " + clipDisplayHead(body, budget) + ending, label
		}
	}
	if line := fixed + ending; displayWidth(line) <= maxW {
		return line, label
	}
	// Drop optional model/prompt details only after the activity body.
	if line := fixed + "]"; displayWidth(line) <= maxW {
		return line, label
	}
	// Long foreground tool arguments can make base wider than the terminal before
	// delegate details are even considered. Retry with the argument-free wait
	// label and no optional suffix, preserving both the live counter and identity.
	if compactBase != "" && compactBase != base {
		compactFixed := compactBase + marker + label
		if body != "" {
			if line := compactFixed + ": " + body + "]"; displayWidth(line) <= maxW {
				return line, label
			}
			budget := maxW - displayWidth(compactFixed+": "+"]")
			if budget > 0 {
				return compactFixed + ": " + clipDisplayHead(body, budget) + "]", label
			}
		}
		if line := compactFixed + "]"; displayWidth(line) <= maxW {
			return line, label
		}
	}
	// The active identity is the final compact form. clipDisplayHead keeps the
	// short display ID before truncating a long or wide agent name.
	budget := maxW - 2
	if budget <= 0 {
		return clipDisplayHead("["+label+"]", maxW), label
	}
	return "[" + clipDisplayHead(label, budget) + "]", label
}

func activeDelegateBody(entry delegate.ActiveDelegate) string {
	parts := make([]string, 0, 4)
	if entry.Turn > 0 {
		turn := fmt.Sprintf("turn %d", entry.Turn)
		if entry.Attempt > 1 {
			turn += fmt.Sprintf(" attempt %d", entry.Attempt)
		}
		parts = append(parts, turn)
	}
	if entry.Activity != "" {
		parts = append(parts, entry.Activity)
	}
	if used := contextUsed(entry.Context); entry.Context.Window > 0 && used > 0 {
		parts = append(parts, fmt.Sprintf("ctx %d%%", contextPercent(entry.Context)))
	}
	if entry.Usage.CostKnown && entry.Usage.CostUSD > 0 {
		parts = append(parts, fmt.Sprintf("$%.3f", entry.Usage.CostUSD))
	}
	return strings.Join(parts, " · ")
}

// clipDelegateStatusInput falls back to a compact identity prefix and reserves
// a cursor-following input window. Thus narrow terminals retain both the child
// label and the edit cursor instead of tail-clipping the entire status prefix.
func clipDelegateStatusInput(label, input string, cursor, maxW int) (string, int, bool) {
	if maxW <= 0 {
		return "", 0, true
	}
	const fixedWidth = 5 // "[" + "] > "
	const preferredInputWidth = 4
	labelBudget := maxW - fixedWidth - preferredInputWidth
	id, _, _ := strings.Cut(strings.TrimSpace(label), " ")
	idWidth := displayWidth(id)
	if labelBudget < idWidth {
		// On very narrow rows, shrink the input window before truncating the
		// stable display ID. Keep at least one column for the edit cursor.
		labelBudget = min(idWidth, maxW-fixedWidth-1)
	}
	if labelBudget <= 0 {
		text, col := clipStatusLine("", input, cursor, maxW)
		return text, col, true
	}
	compactLabel := clipDelegateLabel(label, labelBudget)
	prefix := "[" + compactLabel + "] > "
	inputWidth := maxW - displayWidth(prefix)
	window, inputCol := clipStatusLine("", input, cursor, inputWidth)
	return prefix + window, displayWidth(prefix) + inputCol, true
}

func clipDelegateLabel(label string, budget int) string {
	if budget <= 0 {
		return ""
	}
	id, agentName, _ := strings.Cut(strings.TrimSpace(label), " ")
	if displayWidth(id) > budget {
		return clipDisplayHead(id, budget)
	}
	if agentName == "" || displayWidth(id)+1+displayWidth(agentName) <= budget {
		return strings.TrimSpace(id + " " + agentName)
	}
	// Preserve the complete stable ID before spending remaining columns on the
	// agent role. One leftover column is not useful enough to replace identity
	// with an ellipsis.
	remaining := budget - displayWidth(id) - 1
	if remaining < 2 {
		return id
	}
	return id + " " + clipDisplayHead(agentName, remaining)
}

func modelRequestStatus(event llm.ModelRequestEvent) string {
	request := ""
	if event.ProxyRequestID != 0 {
		request = fmt.Sprintf("proxy req %d", event.ProxyRequestID)
	}
	switch event.State {
	case llm.ModelRequestAccepted, llm.ModelRequestCompleted:
		return request
	case llm.ModelRequestRetryScheduled:
		return joinStatusParts(request, retryStatus(event))
	case llm.ModelRequestUpstreamAttemptFailed:
		if event.Outcome == llm.ModelRequestOutcomeRetrying {
			return joinStatusParts(request, retryStatus(event))
		}
		return joinStatusParts(request, "failed")
	case llm.ModelRequestFailed:
		return joinStatusParts(request, "failed")
	case llm.ModelRequestCancelled:
		return joinStatusParts(request, "cancelled")
	default:
		return request
	}
}

func retryStatus(event llm.ModelRequestEvent) string {
	status := "retrying"
	if event.StatusCode != 0 {
		status += fmt.Sprintf(" %d", event.StatusCode)
	}
	if event.RetryDelayMS > 0 {
		status += " in " + formatRetryDelay(event.RetryDelayMS)
	}
	return status
}

func joinStatusParts(left, right string) string {
	switch {
	case left == "":
		return right
	case right == "":
		return left
	default:
		return left + " · " + right
	}
}

func formatRetryDelay(milliseconds int64) string { return sessionrec.FormatRetryDelay(milliseconds) }

// clipStatusLine fits the during-prompt status line into maxW display columns and
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
	if r.disableAutoCompaction || r.compactionWarned || pct < r.compactionWarnPercent {
		return
	}
	r.compactionWarned = true
	r.dimLine(fmt.Sprintf("[notice: context at %d%%; approaching compaction]", pct))
}

func resolvedCompactionWarnPercent(trigger int) int {
	if trigger <= 0 {
		trigger = compactDefaultTriggerPercent
	}
	return max(trigger-5, 1)
}

const compactDefaultTriggerPercent = 78

// contextPercent is the share of the model's context window in use, for the
// counter and the compaction notice (r27).
func contextPercent(ctx agent.ContextEstimate) int { return sessionrec.ContextPercent(ctx) }

func contextUsed(ctx agent.ContextEstimate) int { return sessionrec.ContextUsed(ctx) }

// cacheHitRatio is the percentage of input tokens served from cache (r15).
func cacheHitRatio(u llm.Usage) int { return sessionrec.CacheHitRatio(u) }

// delegateProgressSnapshot type-asserts an opaque `any` to the concrete
// func() agent.DelegateProgressSnapshot closure carried through tools/background
// (which cannot import agent) and invokes it. ok is false when the value is nil
// or not the expected closure type, so non-delegate progress is silently skipped.
func delegateProgressSnapshot(progress any) (agent.DelegateProgressSnapshot, bool) {
	if progress == nil {
		return agent.DelegateProgressSnapshot{}, false
	}
	fn, ok := progress.(func() agent.DelegateProgressSnapshot)
	if !ok || fn == nil {
		return agent.DelegateProgressSnapshot{}, false
	}
	return fn(), true
}

// writeDelegateProgress appends one foreground delegate child run's live activity
// to the status line, e.g. "· turn 3 · 2 tools · 4.2k/200k ctx 2% · $0.04". A
// finished run is shown once (its final snapshot) before the wait clears. Zero
// state (no turn yet) renders nothing so a just-started run does not flicker.
func writeDelegateProgress(b *strings.Builder, progress any) {
	s, ok := delegateProgressSnapshot(progress)
	if !ok {
		return
	}
	if s.Turn == 0 && !s.Finished {
		return
	}
	if s.Turn > 0 {
		fmt.Fprintf(b, " · turn %d", s.Turn)
		if s.Attempt > 1 {
			fmt.Fprintf(b, " attempt %d", s.Attempt)
		}
	}
	if s.Tools > 0 {
		if s.Tools == 1 {
			b.WriteString(" · 1 tool")
		} else {
			fmt.Fprintf(b, " · %d tools", s.Tools)
		}
	}
	if used := contextUsed(s.Context); s.Context.Window > 0 && used > 0 {
		fmt.Fprintf(b, " · ctx %d%% %s/%s", contextPercent(s.Context), humanTokens(used), humanTokens(s.Context.Window))
	}
	if s.Usage.CostKnown && s.Usage.CostUSD > 0 {
		fmt.Fprintf(b, " · $%.3f", s.Usage.CostUSD)
	}
}

// writeBackgroundProgress summarizes the outstanding join-required background
// delegate jobs, e.g. "· 2 jobs: explore turn 5 · plan idle · $0.04". Idle jobs
// (no turn yet) render "idle"; finished jobs render their final state once.
// Jobs with no agent name fall back to the job kind.
func writeBackgroundProgress(b *strings.Builder, progress []any) {
	var segs []string
	var totalCost float64
	costKnown := true
	for _, p := range progress {
		s, ok := delegateProgressSnapshot(p)
		if !ok {
			continue
		}
		segs = append(segs, backgroundJobSegment(s))
		if s.Usage.CostKnown {
			totalCost += s.Usage.CostUSD
		} else {
			costKnown = false
		}
	}
	if len(segs) == 0 {
		return
	}
	if len(segs) == 1 {
		fmt.Fprintf(b, " · 1 job: %s", segs[0])
	} else {
		fmt.Fprintf(b, " · %d jobs: %s", len(segs), strings.Join(segs, " · "))
	}
	if costKnown && totalCost > 0 {
		fmt.Fprintf(b, " · $%.3f", totalCost)
	}
}

// backgroundJobSegment renders one job's live state for the background
// summary. A job with no activity yet is "idle". An agent name prefixes the
// state when present so concurrent jobs are distinguishable at a glance.
func backgroundJobSegment(s agent.DelegateProgressSnapshot) string {
	var b strings.Builder
	if s.Agent != "" {
		b.WriteString(s.Agent)
		b.WriteByte(' ')
	}
	if s.Turn == 0 && !s.Finished {
		b.WriteString("idle")
		return b.String()
	}
	if s.Turn > 0 {
		fmt.Fprintf(&b, "turn %d", s.Turn)
	}
	if s.Tools > 0 {
		if b.Len() > 0 {
			b.WriteByte(' ')
		}
		fmt.Fprintf(&b, "%d tools", s.Tools)
	}
	if s.Finished {
		if b.Len() > 0 {
			b.WriteByte(' ')
		}
		b.WriteString("done")
	}
	return b.String()
}

// sanitizeInputLine collapses control characters (notably the newlines that
// Enter inserts during a prompt) to spaces so the multi-line buffer renders on the
// single counter row.
func sanitizeInputLine(s string) string {
	return strings.Map(func(r rune) rune {
		if r == '\n' || r == '\r' || r == '\t' || r < 0x20 {
			return ' '
		}
		return r
	}, s)
}

// clipDisplayHead trims s to at most max display columns, retaining leading
// identity fields and appending an ellipsis when truncated.
func clipDisplayHead(s string, maxW int) string {
	if maxW <= 0 {
		return ""
	}
	if displayWidth(s) <= maxW {
		return s
	}
	if maxW == 1 {
		return "…"
	}
	var b strings.Builder
	width := 0
	for _, r := range s {
		w := runeWidth(r)
		if width+w > maxW-1 {
			break
		}
		b.WriteRune(r)
		width += w
	}
	b.WriteRune('…')
	return b.String()
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
	r.renderMu.Lock()
	defer r.renderMu.Unlock()
	r.finishAssistantLineLocked()
}

func (r *Renderer) finishAssistantLineLocked() {
	r.flushAssistantMarkdownLocked()
	if !r.assistantLineOpen {
		r.signalActivityBoundaryLocked()
		return
	}
	fmt.Fprintln(r.out)
	r.assistantLineOpen = false
	if r.assistantMarkdown != nil {
		r.assistantMarkdown.CloseLine()
	}
	r.signalActivityBoundaryLocked()
}

func (r *Renderer) flushAssistantMarkdownLocked() {
	if !r.markdown || r.assistantMarkdown == nil {
		return
	}
	io.WriteString(r.out, r.assistantMarkdown.Flush())
	r.assistantLineOpen = r.assistantMarkdown.LineOpen()
}

func (r *Renderer) ensureAssistantMarkdownLocked() {
	if r.assistantMarkdown != nil {
		return
	}
	r.assistantMarkdown = markdown.NewStream(markdown.Options{
		Enabled:    true,
		ANSI:       r.color,
		ColorTheme: r.colorTheme,
		Width:      r.outputWidth(),
	})
}

func (r *Renderer) writeFinalSeparatorIfNeededLocked() {
	if r.assistantPhase != llm.AssistantPhaseFinal ||
		!r.visiblePreFinalOutput ||
		r.visibleFinalOutput ||
		r.finalSeparatorPrinted {
		return
	}
	r.finishAssistantLineLocked()
	fmt.Fprintln(r.out, r.separatorRule())
	r.finalSeparatorPrinted = true
}

func (r *Renderer) markAssistantTextVisibleLocked() {
	switch r.assistantPhase {
	case llm.AssistantPhaseFinal:
		r.visibleFinalOutput = true
	case llm.AssistantPhaseCommentary:
		r.visiblePreFinalOutput = true
	}
}

// The one-line summary formatters below are thin wrappers over
// internal/sessionrec, the canonical home shared by live rendering and the
// session recorder, so live output and raw.ndjson can never drift.

func formatArgs(input json.RawMessage) string { return sessionrec.FormatArgs(input) }
func formatToolArgs(name string, input json.RawMessage) string {
	return sessionrec.FormatToolArgs(name, input)
}
func formatScalar(s string) string               { return sessionrec.FormatScalar(s) }
func formatValue(raw json.RawMessage) string     { return sessionrec.FormatValue(raw) }
func resultSummary(result llm.ToolResult) string { return sessionrec.ResultSummary(result) }
func richImageMetadata(image llm.ContentBlock) string {
	return sessionrec.RichImageMetadata(image)
}

// ToolResultLine renders the one-line tool summary used by live output and
// session replay.
func ToolResultLine(call llm.ToolCall, result llm.ToolResult) string {
	return sessionrec.ToolResultLine(call, result)
}

// usageLine renders the per-prompt summary with cumulative totals (design §10):
//
//	[prompt: 3 turns · 12.4k (15.0k) in / 1.8k (2.0k) out · $0.071 ($0.102) · 4.3s]
//
// Per-prompt values are shown first; parenthesised values are cumulative across
// the session. Cumulative cost is omitted for models with no price entry;
// per-prompt cost is also omitted when the model has no price entry.
func usageLine(u agent.PromptUsage, elapsed time.Duration, cost float64, costKnown bool, cumIn, cumOut int, cumCost float64) string {
	return sessionrec.UsageLine(u, elapsed, cost, costKnown, cumIn, cumOut, cumCost)
}

func turnUsageLine(u agent.TurnUsage, elapsed, promptElapsed time.Duration) string {
	return sessionrec.TurnUsageLine(u, elapsed, promptElapsed)
}

func largeRequestWarning(ctx agent.ContextEstimate) string {
	if ctx.Total == 0 && ctx.PayloadTotal == 0 && ctx.Tools == 0 {
		return ""
	}
	payload := ctx.PayloadTotal
	if payload == 0 {
		payload = ctx.Total
	}
	largeContext := ctx.Window > 0 && ctx.Total*2 >= ctx.Window
	largePayload := ctx.Window > 0 && payload*2 >= ctx.Window
	largeTools := ctx.Tools >= 10_000
	if !largeContext && !largePayload && !largeTools {
		return ""
	}

	if largeTools && !largeContext && !largePayload {
		return fmt.Sprintf("[warning: large tool schema payload: tools %s; large tool schemas can slow response start]",
			humanTokens(ctx.Tools))
	}

	note := "large payloads can slow response start"
	if ctx.Messages >= ctx.Tools && ctx.Messages >= ctx.System {
		note = "message/tool-result history dominates; cached or continued tokens still count toward the context window"
	}
	return fmt.Sprintf("[warning: large model context: ctx %s/%s payload estimate %s (sys %s tools %s msgs %s); %s]",
		humanTokens(ctx.Total), humanTokens(ctx.Window), humanTokens(payload),
		humanTokens(ctx.System), humanTokens(ctx.Tools), humanTokens(ctx.Messages), note)
}

func turnPhrase(n int) string { return sessionrec.TurnPhrase(n) }

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

func firstLine(s string) string { return sessionrec.FirstLine(s) }

// countLines counts text lines: a trailing newline does not add an empty line.
func countLines(s string) int { return sessionrec.CountLines(s) }

func clip(s string, max int) string { return sessionrec.Clip(s, max) }

// humanTokens renders a token count compactly: 12400 -> "12.4k".
func humanTokens(n int) string { return sessionrec.HumanTokens(n) }

// humanDuration renders an elapsed turn duration: "4.3s" or "850ms".
func humanDuration(d time.Duration) string { return sessionrec.HumanDuration(d) }
