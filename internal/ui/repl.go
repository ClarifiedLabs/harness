package ui

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"harness/internal/agent"
	"harness/internal/background"
	"harness/internal/hooks"
	"harness/internal/inputimage"
	"harness/internal/llm"
	"harness/internal/plan"
	"harness/internal/reasoningprofile"
	"harness/internal/replprompt"
	"harness/internal/session"
	"harness/internal/skills"
	"harness/internal/term"
	"harness/internal/todo"
	"harness/internal/tools"
	"harness/prompts"
)

const (
	bracketedPasteStart     = "\x1b[200~"
	bracketedPasteEnd       = "\x1b[201~"
	shiftTabPrewarmDebounce = 500 * time.Millisecond
)

// ModelSelection is the runtime model/provider bundle returned by App.SwitchModel.
type ModelSelection struct {
	Provider          string
	Model             string
	RegistryModel     string
	BaseURL           string
	Runtime           llm.Provider
	ContextWindow     int // agent override; 0 means use the registry
	Reasoning         llm.ReasoningConfig
	BaseTargetID      string
	Variant           string
	FastTargetID      string
	ServerTools       []llm.ServerTool
	ResponsesStateful bool
	// ReasoningSet says Reasoning intentionally replaces the requested config,
	// including zero value for provider default.
	ReasoningSet bool
}

// AgentSummary is one configured agent row for /agent listing.
type AgentSummary struct {
	Name        string
	Description string
	Model       string
	Delegatable bool
}

// AgentSelection is the runtime agent bundle returned by App.SwitchAgent: the
// new tool registry, fully reassembled system prompt, and model target runtime
// for subsequent turns.
type AgentSelection struct {
	Name              string
	Tools             *tools.Registry
	System            string
	Provider          string
	Model             string
	RegistryModel     string
	BaseURL           string
	Runtime           llm.Provider
	ContextWindow     int
	Reasoning         llm.ReasoningConfig
	BaseTargetID      string
	Variant           string
	FastTargetID      string
	ServerTools       []llm.ServerTool
	ResponsesStateful bool
	ReasoningSet      bool
}

// App bundles the dependencies the REPL and one-shot driver need. main builds it
// from the resolved config, provider factory, tool registry, and renderer
// (design §10). The agent owns the running transcript; App tracks the cumulative
// session usage and the current save path (rotated by /clear).
type App struct {
	Agent    *agent.Agent
	Renderer *Renderer
	Out      io.Writer
	Errw     io.Writer
	Logger   *slog.Logger
	// DiagnosticLogger persists compatibility diagnostics without rendering
	// them through the ordinary terminal log handler.
	DiagnosticLogger *slog.Logger

	Provider      string
	Model         string
	RegistryModel string
	BaseURL       string
	Registry      *llm.Registry
	System        string
	Reasoning     llm.ReasoningConfig
	BaseTargetID  string
	Variant       string
	FastTargetID  string
	ImageDetail   string
	PendingImages []inputimage.Loaded
	Hooks         *hooks.Runner
	HookContext   []string
	Background    *background.Manager

	AvailableModels        []string
	SwitchModel            func(model string, reasoning llm.ReasoningConfig) (ModelSelection, error)
	PickModel              func(PickerIO) (string, error)
	PickerPageSize         int
	SetReasoning           func(model string, reasoning llm.ReasoningConfig) error
	SaveDefaultModel       func(provider, model string, reasoning llm.ReasoningConfig) error
	SaveReplEditMode       func(mode string) error
	PromptDefaultModelSave bool

	// Prewarm, when set, kicks off a background prompt-cache warm-up for the
	// current agent/model/provider/system snapshot. The REPL calls it after a
	// cache-invalidating event (agent/model switch, compaction) so the next real
	// request reads a warm prefix (r43). nil disables it (piped/one-shot, tests).
	Prewarm func()
	// SchedulePrewarm, when installed by the REPL loop, routes a switch-driven
	// warm-up (/agent, /model, startup agent resolution) through the shift-tab
	// debounce so rapid cycling or resolution bursts warm only the settled
	// selection. Nil falls back to an immediate prewarm.
	SchedulePrewarm func()
	// promptActive tracks a running prompt so prewarm() defers mid-prompt
	// warm-ups — a running prompt refreshes the cache prefix every turn, so a
	// mid-prompt prewarm is pure waste. Requests coalesce into pendingPrewarm,
	// fired once when the prompt completes (the switched-to agent's prefix is
	// genuinely cold then, including after interrupt/error exits).
	promptActive   bool
	pendingPrewarm bool
	// shiftTabPrewarmAfter is test-injectable; production uses time.After.
	shiftTabPrewarmAfter func(time.Duration) <-chan time.Time

	// IdleCompactionAfter enables speculative interactive compaction after the
	// REPL has remained idle for this duration. Zero disables it.
	IdleCompactionAfter          time.Duration
	IdleCompactionTriggerPercent int
	// idleCompactionAfter is test-injectable; production uses time.After.
	idleCompactionAfter func(time.Duration) <-chan time.Time

	AgentName             string         // current agent definition name
	AvailableAgents       []AgentSummary // sorted agent names/descriptions for /agent listing
	RefreshAgentSummaries func() []AgentSummary
	SwitchAgent           func(name string) (AgentSelection, error)

	// RefreshMCP, when set, is consulted at the idle-prompt boundary (just
	// before a typed prompt starts) to pick up proxy tool-list changes.
	// It is called with the current agent name; a non-nil registry replaces the
	// agent's tools and notice is rendered. A nil registry means "no change".
	// nil disables the hook (one-shot mode and tests leave it nil).
	RefreshMCP func(ctx context.Context, agentName string) (*tools.Registry, string)

	// Todos holds the model's current todo list (the update_todos tool's store),
	// persisted in state.json and reset on /clear. nil disables persistence.
	Todos *todo.Store

	// The REPL sink can print the prompt todo status immediately before the
	// per-prompt usage line so usage is the last status line before the next prompt.
	// These fields let the idle prompt avoid printing that same todo block again.
	todoPromptStatusBeforeUsage       bool
	todoPromptStatusBeforeUsagePrompt int

	// Plans holds the recorded plans (the record_plan tool's store), persisted
	// in state.json and reset on /clear. nil disables persistence.
	Plans *plan.Store
	// The REPL prints the latest recorded plan's path after record_plan and again
	// before the per-prompt usage line (mirroring the todo status). These fields
	// let the idle prompt avoid printing that same plan line again.
	planPromptStatusBeforeUsage       bool
	planPromptStatusBeforeUsagePrompt int
	// Handoff carries a pending plan->implementation handoff requested by the
	// request_implementation tool, consumed at the prompt boundary. nil disables.
	Handoff *plan.Pending
	// HandoffAgent is the default agent a handoff switches to when the request
	// names none. Empty falls back to the built-in default agent.
	HandoffAgent string

	SessionPath          string // current save path; /clear rotates it
	SessionTree          *session.Tree
	StateDir             string    // for rotating to a fresh auto-save path on /clear
	Created              time.Time // session creation time (preserved across saves)
	PromptNumber         int       // last started prompt, persisted for replay numbering
	Now                  func() time.Time
	OnSessionPathChanged func(string)
	// OnPromptFinished observes completion after the per-prompt session save.
	// It is primarily useful to coordinate embedders and tests whose process
	// remains alive after a forced REPL exit.
	OnPromptFinished func()

	// History configuration (bash-style REPL history persistence).
	// HistFile is the path to the history file (empty disables persistence).
	// HistFileSize caps entries stored on disk (0 disables persistence).
	// HistSize caps entries loaded into memory (0 disables recall).
	HistFile     string
	HistFileSize int
	HistSize     int

	// Interrupt is the optional SIGINT state machine. When set, the REPL marks
	// prompt boundaries so ^C cancels a prompt rather than the whole process
	// (design §8.4). Tests leave it nil.
	Interrupt *agent.InterruptWatcher
	// ForceExit receives the watcher's second-Ctrl-C exit request in one-shot
	// mode, where no REPL select loop exists to observe it.
	ForceExit <-chan struct{}

	// Steer, when set, routes a prepared model-bound prompt submitted during a
	// running prompt into the agent as an in-prompt steering message (injected before
	// the next model request) instead of queuing it for the next prompt. nil
	// disables steering and queues the input for the next prompt. Non-model-bound
	// during-prompt input (shell escapes, /commands, /edit) is never steered.
	Steer func(agent.SteerInput)
	// DrainSteer recovers prepared steer input the running prompt never consumed
	// (set alongside Steer). The REPL runs it as the next prompt at prompt completion so the
	// input is not lost. Returns an empty input when nothing remains or steering
	// is off.
	DrainSteer func() agent.SteerInput

	// Prompt is the REPL input prompt format.
	Prompt string

	// PromptEditMode selects the raw prompt editor keymap: "emacs" (default)
	// or "vi". It applies only to interactive TTY prompts.
	PromptEditMode string

	// SetPromptEditMode switches the raw prompt editor keymap at runtime
	// (e.g. via /vi on|off). The runner sets it; callers may leave it nil
	// outside the REPL.
	SetPromptEditMode func(mode string)

	// OpenEditor launches an editor for a temp prompt file. nil uses
	// $VISUAL, then $EDITOR, then vi. Tests inject this to edit deterministically.
	OpenEditor func(path string) error
	// RunShellCommand runs a TTY-only !command escape from the prompt. nil uses
	// the user's shell. Run wraps it with the same terminal handoff hooks used by
	// the external editor.
	RunShellCommand func(command string) error
	// BeforeEditor/AfterEditor temporarily hand the terminal back to the editor.
	// Run installs these hooks; tests and non-REPL callers can leave them nil.
	BeforeEditor func()
	AfterEditor  func()

	// Skills is the discovered skills map for /skills listing and
	// $skillName invocation (design §10). nil disables both features.
	Skills map[string]skills.Skill

	// SkillDirs is the list of scanned skill directories with their scopes,
	// used by /skills to group output by source location.
	SkillDirs []skills.Dir

	// DisabledTools lists optional built-in tools that could not be registered
	// (e.g., rg when ripgrep is not installed). Used by /tools.
	DisabledTools []tools.DisabledTool

	// SummaryWidth returns the terminal width for command summaries. nil or a
	// non-positive value disables forced wrapping.
	SummaryWidth func() int

	usage        session.UsageTotals            // cumulative aggregate across the session
	usageByModel map[string]session.UsageTotals // per model target cumulative, for accurate per-model cost

	maintenanceMu      sync.Mutex
	pendingMaintenance []queuedMaintenanceUsage
}

type queuedMaintenanceUsage struct {
	agent.MaintenanceUsage
	modelKey string
	prewarm  *agent.PrewarmResult
}

type idleCompactionFinished struct {
	result agent.IdleCompactionResult
	err    error
}

// helpText lists the meta-commands (design §10).
const helpText = `commands:
  /help            list commands
  /exit, /quit     save and exit
  /clear           reset conversation; rotate to a fresh session directory
  /compact [focus] force compaction now with optional one-shot summary focus
  /tree [entry]    browse the conversation tree and branch in this session
  /fork [entry]    branch before a prior prompt into a new session
  /clone           clone the current branch into a new session
  /context [file]  dump current model context, or save it as JSON
  /usage           cumulative session tokens and cost
  /tools           list available tools (built-in, MCP, and disabled)
  /image [opts]    attach an image to the next prompt, list, or clear
  /edit [draft]    open $VISUAL/$EDITOR (or vi) for a multi-line prompt
  /save [file]     force save (optionally elsewhere)
  /model [target]  pick a configured model target, or switch directly
  /reasoning [profile] list or set the reasoning profile
  /effort [profile]    alias for /reasoning
  /fast [on|off|status] toggle the model's fast tier
  /agent [name]    list agents, or switch to agent
  /mode [name]     alias for /agent
  /plan            alias for /agent plan
  /auto            alias for /agent auto
  /handoff [-a agent] [-m model] [message]
                    hand off the recorded plan with optional implementation guidance
  /background [id] list background jobs, inspect one, or cancel with "cancel <id>"
  /skills          list available skills
  /vi on|off       enable or disable vi-style prompt editing
  !command         run a local shell command at an interactive prompt
  @path<Tab>       complete a literal file reference; image refs attach when supported
  $skillName       mention a skill to load via SKILL.md
Interrupt a running prompt with Ctrl-C or double-Esc; press Ctrl-C again to force exit if cancellation stalls. Input submitted while it runs is injected before the next turn when possible, otherwise queued as the next prompt.
Ctrl-G opens the editor from the prompt; paths with spaces complete as @"..."; lines starting with / are commands; // sends a literal leading slash; !! escapes a literal !; $$ escapes a literal $`

func (app *App) clock() func() time.Time {
	if app.Now != nil {
		return app.Now
	}
	return time.Now
}

// Run drives the interactive REPL: it reads lines from in, dispatches
// meta-commands, and runs one agent prompt interaction per submitted prompt,
// saving the session after every prompt (design §10, §11).
//
// exit carries SIGINT exit requests (design §8.4); a nil channel disables them.
// Run owns the final save in every exit path — /exit, EOF (^D), and SIGINT — so
// no second goroutine ever touches the transcript or session file concurrently
// with an in-flight prompt. It returns 0 on /exit, /quit, or EOF, and
// ExitInterrupt (130) on a SIGINT exit request. Input is scanned in an
// on-demand helper goroutine so an exit request received while idle at the
// prompt is acted on immediately rather than blocking on the next line. During
// an active prompt the same helper also preserves typeahead and observes Esc-Esc
// without competing with an external editor launched from the idle prompt.
func Run(in io.Reader, app *App, exit <-chan struct{}) int {
	return runWithInitialPrompt(in, app, exit, promptLineEditorEnabled(in, app.Errw), nil)
}

// RunWithInitialPrompt drives the interactive REPL after immediately starting
// one prompt interaction from initialPrompt. The initial prompt is always treated as
// user text, never as a REPL command or shell escape.
func RunWithInitialPrompt(in io.Reader, app *App, exit <-chan struct{}, initialPrompt string) int {
	return runWithInitialPrompt(in, app, exit, promptLineEditorEnabled(in, app.Errw), &initialPrompt)
}

func run(in io.Reader, app *App, exit <-chan struct{}, usePromptEditor bool) int {
	return runWithInitialPrompt(in, app, exit, usePromptEditor, nil)
}

func runWithInitialPrompt(in io.Reader, app *App, exit <-chan struct{}, usePromptEditor bool, initialPrompt *string) int {
	if app.Created.IsZero() {
		app.Created = app.clock()()
	}

	promptFormat := app.Prompt
	if promptFormat == "" {
		promptFormat = replprompt.DefaultFormat
	}
	promptTemplate, err := replprompt.Compile(promptFormat)
	if err != nil {
		fmt.Fprintf(app.Errw, "[repl prompt error: %v]\n", err)
		promptTemplate, _ = replprompt.Compile(replprompt.DefaultFormat)
	}
	renderPrompt := func() string {
		viMode := ""
		if usePromptEditor {
			viMode = idleViMode(app.PromptEditMode)
		}
		return promptTemplate.Render(app.promptValues(promptTemplate, viMode))
	}

	// Restore a usable terminal before the first prompt (termios sane plus an
	// emulator soft reset), in case a prior process left it in raw, no-echo,
	// or mouse-reporting state. Targets /dev/tty directly; no-op without one.
	var restorePromptTerm func() error
	disableIdlePromptTerm := func() {
		_ = term.SetBracketedPaste(false)
		if restorePromptTerm != nil {
			_ = restorePromptTerm()
			restorePromptTerm = nil
		}
		if usePromptEditor && promptEditMode(app.PromptEditMode) == promptEditModeVi {
			_ = term.SetCursorShape(term.CursorShapeDefault)
		}
	}
	enableIdlePromptTerm := func() {
		if err := term.Reset(); err != nil {
			fmt.Fprintf(app.Errw, "[term reset: %v]\n", err)
		}
		if usePromptEditor {
			if cleanup, err := term.EnablePromptRawMode(); err == nil {
				restorePromptTerm = cleanup
			}
		} else if cleanup, err := term.EnableCtrlGLineEnd(); err == nil {
			restorePromptTerm = cleanup
		}
		_ = term.SetBracketedPaste(true)
	}
	enableIdlePromptTerm()
	defer disableIdlePromptTerm()

	prevBeforeEditor, prevAfterEditor := app.BeforeEditor, app.AfterEditor
	app.BeforeEditor = func() {
		disableIdlePromptTerm()
		if prevBeforeEditor != nil {
			prevBeforeEditor()
		}
	}
	app.AfterEditor = func() {
		if prevAfterEditor != nil {
			prevAfterEditor()
		}
		enableIdlePromptTerm()
	}
	defer func() {
		app.BeforeEditor = prevBeforeEditor
		app.AfterEditor = prevAfterEditor
	}()

	reader := newREPLReader(in, app.Errw, usePromptEditor, app.PromptEditMode)
	if output := outputCoordinatorFromWriter(app.Errw); output != nil && reader.editor != nil {
		output.setPromptEditor(reader.editor)
		defer output.setPromptEditor(nil)
	}
	app.SetPromptEditMode = func(mode string) {
		if reader.editor != nil {
			reader.editor.setEditMode(mode)
		}
	}
	// When the prompt template uses a {vimode} placeholder, re-render the prompt
	// for the current vi mode at each mode transition (Esc/i/a/...) so the label
	// flips live during a read. The closure mirrors renderPrompt but with the
	// editor's current mode; it is nil in emacs mode and for templates without a
	// vimode variant, so behavior is unchanged there and in tests.
	if usePromptEditor && reader.editor != nil && promptTemplate.UsesViMode() {
		reader.editor.viPrompt = func(m viMode) string {
			return promptTemplate.Render(app.promptValues(promptTemplate, viModeName(m)))
		}
	}
	// Render the during-prompt typed buffer live on the status line (during-prompt
	// input). The reader calls this from its read goroutine; SetInputLine is
	// mutex-guarded so it never interleaves with the agent's renderer writes.
	if usePromptEditor && app.Renderer != nil {
		reader.onPromptInput = func(buf string, cursor int) { app.Renderer.SetInputLine(buf, cursor) }
	}
	// Load and configure REPL history persistence (bash-style HISTFILE/HISTFILESIZE/HISTSIZE).
	// The in-memory editor receives a pre-loaded slice and a callback that appends each new
	// entry to the on-disk history file. Errors are warned but never fatal.
	if usePromptEditor && reader.editor != nil && app.HistFile != "" {
		if entries, err := session.LoadHistory(app.HistFile, app.HistFileSize, app.HistSize); err != nil {
			fmt.Fprintf(app.Errw, "[history load error: %v]\n", err)
		} else {
			reader.editor.SetInitialHistory(entries)
		}
		reader.editor.onNewHistory = func(entry string) {
			if err := session.AppendHistory(app.HistFile, entry); err != nil {
				fmt.Fprintf(app.Errw, "[history save error: %v]\n", err)
			}
		}
	}
	readReq := make(chan replReadRequest)
	inputs := make(chan replReadResult, 1)
	go func() {
		for req := range readReq {
			input, ok, err := reader.read(req)
			inputs <- replReadResult{input: input, ok: ok, err: err}
			if !ok {
				break
			}
		}
		// The reader has ended (EOF or a terminal read error). Keep draining
		// readReq and replying with an inert ended result so a requestRead that
		// races the end of input does not block forever on an orphaned channel.
		// The main loop treats ended results as no-ops and exits via inputEnded.
		for range readReq {
			inputs <- replReadResult{input: replInput{ended: true}}
		}
	}()
	defer close(readReq)

	var (
		promptPrinted             bool
		readPending               bool
		inputEnded                bool
		inputErr                  error
		active                    bool
		activeReadPause           bool
		plainPromptRead           bool
		prompt                    string
		pendingPrefill            string // text deposited into the next prompt
		pendingPrefillModelPrompt bool   // submitted prefill bypasses command/shell dispatch
		pendingPrefillPasted      bool   // retained pure-paste classification across Shift-Tab
		queued                    []replInput
		preparedQueued            []agent.SteerInput
		promptDone                <-chan struct{}
		restoreEsc                func() error
		escPresses                escapePresses
		pendingShiftTabPrewarm    <-chan time.Time
		pendingIdleCompaction     <-chan time.Time
		idleCompactionDone        <-chan idleCompactionFinished
		cancelIdleCompactionWork  context.CancelFunc
		idleCompactionDiscard     bool
		idleCompactionModelKey    string
		idleCompactionStarted     time.Time
		idleCompactionTrigger     int
		idleCompactionContext     int
		idleCompactionMessages    int
	)

	prewarmAfter := app.shiftTabPrewarmAfter
	if prewarmAfter == nil {
		prewarmAfter = time.After
	}
	prewarm := app.Prewarm
	cancelShiftTabPrewarm := func() {
		pendingShiftTabPrewarm = nil
	}
	scheduleShiftTabPrewarm := func() {
		if prewarm == nil {
			return
		}
		pendingShiftTabPrewarm = prewarmAfter(shiftTabPrewarmDebounce)
	}
	// Existing immediate prewarm paths remain immediate and also invalidate any
	// older Shift-Tab timer. Restore the caller's callback when the REPL exits.
	if prewarm != nil {
		app.Prewarm = func() {
			cancelShiftTabPrewarm()
			prewarm()
		}
		defer func() { app.Prewarm = prewarm }()
	}
	// All switch-driven prewarms (/agent, /model, startup resolution) ride the
	// same debounce as Shift-Tab cycling so bursts warm only the settled
	// selection. Restore the caller's hook when the REPL exits.
	prevSchedulePrewarm := app.SchedulePrewarm
	app.SchedulePrewarm = scheduleShiftTabPrewarm
	defer func() { app.SchedulePrewarm = prevSchedulePrewarm }()

	idleAfter := app.idleCompactionAfter
	if idleAfter == nil {
		idleAfter = time.After
	}
	cancelIdleCompaction := func() {
		pendingIdleCompaction = nil
		if cancelIdleCompactionWork != nil {
			idleCompactionDiscard = true
			cancelIdleCompactionWork()
		}
	}
	scheduleIdleCompaction := func() {
		if app.IdleCompactionAfter <= 0 || pendingIdleCompaction != nil || idleCompactionDone != nil {
			return
		}
		pendingIdleCompaction = idleAfter(app.IdleCompactionAfter)
	}
	startIdleCompaction := func() {
		pendingIdleCompaction = nil
		trigger := app.IdleCompactionTriggerPercent
		if trigger == 0 {
			trigger = 35
		}
		work, ok, err := app.Agent.PrepareIdleCompaction(trigger)
		if err != nil {
			app.renderHookNotices([]string{"[idle compact failed: " + err.Error() + "]"})
			return
		}
		if !ok {
			return
		}
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan idleCompactionFinished, 1)
		idleCompactionDone = done
		cancelIdleCompactionWork = cancel
		idleCompactionDiscard = false
		idleCompactionModelKey = app.usageKey()
		idleCompactionStarted = time.Now()
		idleCompactionTrigger = trigger
		idleCompactionContext = app.Agent.EstimateContext().Total
		idleCompactionMessages = len(app.Agent.Transcript())
		go func() {
			result, err := work(ctx)
			done <- idleCompactionFinished{result: result, err: err}
		}()
	}
	finishIdleCompaction := func(finished idleCompactionFinished, allowApply bool) bool {
		discard := idleCompactionDiscard
		modelKey := idleCompactionModelKey
		started := idleCompactionStarted
		trigger := idleCompactionTrigger
		contextBefore := idleCompactionContext
		messagesBefore := idleCompactionMessages
		if cancelIdleCompactionWork != nil {
			cancelIdleCompactionWork()
		}
		idleCompactionDone = nil
		cancelIdleCompactionWork = nil
		idleCompactionDiscard = false
		idleCompactionModelKey = ""
		idleCompactionStarted = time.Time{}
		idleCompactionTrigger = 0
		idleCompactionContext = 0
		idleCompactionMessages = 0
		record := func(outcome string, contextAfter, messagesAfter int) {
			app.recordEvent(session.Event{
				Type:       session.EventIdleCompaction,
				Prompt:     app.PromptNumber,
				DurationMS: time.Since(started).Milliseconds(),
				IdleCompaction: &session.IdleCompactionSnapshot{
					Outcome:             outcome,
					TriggerPercent:      trigger,
					ContextTokensBefore: contextBefore,
					ContextTokensAfter:  contextAfter,
					MessagesBefore:      messagesBefore,
					MessagesAfter:       messagesAfter,
				},
			})
		}
		if finished.result.Usage != (llm.Usage{}) {
			app.addMaintenanceUsageForModel("idle_compaction", finished.result.Usage, modelKey)
		}
		if !allowApply || discard {
			record("discarded", 0, 0)
			return false
		}
		if finished.err != nil {
			record("failed", 0, 0)
			if !errors.Is(finished.err, context.Canceled) && !errors.Is(finished.err, context.DeadlineExceeded) {
				app.renderHookNotices([]string{"[idle compact failed: " + finished.err.Error() + "]"})
			}
			return false
		}
		if !finished.result.Prepared {
			record("no_change", contextBefore, messagesBefore)
			return false
		}
		sink := newAccumulatingSink(app.Renderer, app, app.PromptNumber)
		applied, err := app.Agent.ApplyIdleCompaction(context.Background(), sink, finished.result)
		if err != nil {
			record("failed", 0, 0)
			app.renderHookNotices([]string{"[idle compact failed: " + err.Error() + "]"})
			return false
		}
		if !applied {
			record("discarded", 0, 0)
			return false
		}
		record("applied", app.Agent.EstimateContext().Total, len(app.Agent.Transcript()))
		app.saveOrWarn(app.SessionPath)
		app.prewarm()
		return true
	}
	// deferIdleCompaction settles an idle-compaction worker that finished while
	// a prompt run owns app usage/renderer/session state. It queues the usage
	// for drainMaintenanceUsage and stashes the discard event for the promptDone
	// branch instead of touching that shared state on the select goroutine.
	deferredIdleEvents := []session.Event{}
	deferIdleCompaction := func(finished idleCompactionFinished) {
		modelKey := idleCompactionModelKey
		started := idleCompactionStarted
		trigger := idleCompactionTrigger
		contextBefore := idleCompactionContext
		messagesBefore := idleCompactionMessages
		if cancelIdleCompactionWork != nil {
			cancelIdleCompactionWork()
		}
		idleCompactionDone = nil
		cancelIdleCompactionWork = nil
		idleCompactionDiscard = false
		idleCompactionModelKey = ""
		idleCompactionStarted = time.Time{}
		idleCompactionTrigger = 0
		idleCompactionContext = 0
		idleCompactionMessages = 0
		if finished.result.Usage != (llm.Usage{}) {
			app.QueueMaintenanceUsageForModel(modelKey, agent.MaintenanceUsage{Purpose: "idle_compaction", Usage: finished.result.Usage})
		}
		deferredIdleEvents = append(deferredIdleEvents, session.Event{
			Type:       session.EventIdleCompaction,
			Prompt:     app.PromptNumber,
			DurationMS: time.Since(started).Milliseconds(),
			IdleCompaction: &session.IdleCompactionSnapshot{
				Outcome:             "discarded",
				TriggerPercent:      trigger,
				ContextTokensBefore: contextBefore,
				MessagesBefore:      messagesBefore,
			},
		})
	}
	finishOutstandingIdleCompaction := func() {
		cancelIdleCompaction()
		if idleCompactionDone == nil {
			return
		}
		select {
		case finished := <-idleCompactionDone:
			finishIdleCompaction(finished, false)
		default:
			app.recordEvent(session.Event{
				Type:       session.EventIdleCompaction,
				Prompt:     app.PromptNumber,
				DurationMS: time.Since(idleCompactionStarted).Milliseconds(),
				IdleCompaction: &session.IdleCompactionSnapshot{
					Outcome:             "discarded",
					TriggerPercent:      idleCompactionTrigger,
					ContextTokensBefore: idleCompactionContext,
					MessagesBefore:      idleCompactionMessages,
				},
			})
			idleCompactionDone = nil
			cancelIdleCompactionWork = nil
			idleCompactionDiscard = false
			idleCompactionModelKey = ""
			idleCompactionStarted = time.Time{}
			idleCompactionTrigger = 0
			idleCompactionContext = 0
			idleCompactionMessages = 0
		}
	}

	requestRead := func(req replReadRequest) {
		if readPending || inputEnded {
			return
		}
		readPending = true
		readReq <- req
	}
	setInputEnded := func(err error) {
		inputEnded = true
		inputErr = err
	}
	warnInputErr := func() {
		if inputErr != nil {
			fmt.Fprintf(app.Errw, "[input error: %v]\n", inputErr)
			inputErr = nil
		}
	}
	finish := func(code int) int {
		cancelShiftTabPrewarm()
		finishOutstandingIdleCompaction()
		if app.Renderer != nil {
			app.Renderer.StopProgress()
			app.Renderer.finishAssistantLine()
		}
		app.stopBackgroundJobs()
		app.saveOrWarn(app.SessionPath)
		app.printExitUsageSummary()
		return code
	}
	enableActivePromptTerm := func() {
		_ = term.SetBracketedPaste(false)
		if cleanup, err := term.EnableEscLineEnd(); err == nil {
			restoreEsc = cleanup
		}
		reader.setEscapeLineEnd(true)
	}
	disableActivePromptTerm := func() {
		reader.setEscapeLineEnd(false)
		if restoreEsc != nil {
			_ = restoreEsc()
			restoreEsc = nil
		}
		_ = term.SetBracketedPaste(true)
	}
	forceFinish := func() int {
		cancelShiftTabPrewarm()
		cancelIdleCompaction()
		disableActivePromptTerm()
		if app.Renderer != nil {
			app.Renderer.StopProgress()
			app.Renderer.finishAssistantLine()
		}
		// The active prompt goroutine may be stuck in a provider that ignored
		// cancellation. Do not race it through session/background mutation; the
		// process exits immediately after Run returns.
		return ExitInterrupt
	}
	startRun := func(run func()) {
		cancelIdleCompaction()
		done := make(chan struct{}, 1)
		active = true
		app.promptActive = true
		plainPromptRead = false
		promptPrinted = false
		escPresses.reset()
		if usePromptEditor {
			// Keep the terminal in raw/echo-off mode for the whole prompt so typed
			// keystrokes feed the live during-prompt input line instead of garbling
			// scrolling output. Bracketed paste is suppressed so a paste arrives
			// as plain keystrokes the capture can accumulate (during-prompt input).
			_ = term.SetBracketedPaste(false)
			reader.beginPromptCapture()
			if app.Renderer != nil {
				app.Renderer.SetInputLine("", 0)
			}
			activeReadPause = false
		} else {
			activeReadPause = queuedContainsEditor(queued)
			disableIdlePromptTerm()
			enableActivePromptTerm()
		}
		promptDone = done
		go func() {
			run()
			done <- struct{}{}
		}()
	}
	startPromptRun := func(prompt string, resolveSkillMentions, attachPromptImages bool) {
		run, ok := app.preparePromptRun(prompt, promptOptions{resolveSkillMentions: resolveSkillMentions, attachPromptImages: attachPromptImages})
		if !ok {
			return
		}
		startRun(run)
	}
	startPreparedPrompt := func(input agent.SteerInput) {
		run, ok := app.prepareSteeredPrompt(input)
		if !ok {
			return
		}
		startRun(run)
	}
	readCommandLine := func(label string) (string, error) {
		if len(queued) > 0 {
			if _, err := fmt.Fprint(app.Errw, label); err != nil {
				return "", err
			}
			input := queued[0]
			queued = queued[1:]
			return strings.TrimSpace(input.text), nil
		}
		req := replReadRequest{}
		if usePromptEditor {
			req = replReadRequest{prompt: label, promptEditor: true}
		} else if _, err := fmt.Fprint(app.Errw, label); err != nil {
			return "", err
		}
		input, ok, err := reader.read(req)
		if !ok {
			if err != nil {
				return "", err
			}
			return "", io.EOF
		}
		return strings.TrimSpace(input.text), nil
	}
	// applyAction dispatches one input at the idle prompt — both the queued-
	// typeahead drain and the fresh read use it — and reports whether the REPL
	// should exit.
	exitContext := func() (context.Context, context.CancelFunc, func() bool) {
		ctx, cancel := context.WithCancel(context.Background())
		var interrupted atomic.Bool
		if exit != nil {
			go func() {
				select {
				case <-exit:
					interrupted.Store(true)
					cancel()
				case <-ctx.Done():
				}
			}()
		}
		return ctx, cancel, interrupted.Load
	}
	startPromptInteraction := func(prompt string, resolveSkillMentions, attachPromptImages bool) (exit bool, code int) {
		cancelShiftTabPrewarm()
		if app.Renderer != nil {
			app.Renderer.SubmittedPromptSeparator()
			app.Renderer.StartPrompt()
		}
		ctx, cancel, interrupted := exitContext()
		err := app.refreshMCP(ctx)
		cancel()
		if interrupted() || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return true, ExitInterrupt
		}
		startPromptRun(prompt, resolveSkillMentions, attachPromptImages)
		return false, ExitOK
	}
	startPreparedPromptInteraction := func(input agent.SteerInput) (exit bool, code int) {
		cancelShiftTabPrewarm()
		if app.Renderer != nil {
			app.Renderer.StartPrompt()
		}
		ctx, cancel, interrupted := exitContext()
		err := app.refreshMCP(ctx)
		cancel()
		if interrupted() || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return true, ExitInterrupt
		}
		startPreparedPrompt(input)
		return false, ExitOK
	}
	applyAction := func(input replInput) (exit bool, code int) {
		if input.cycleAgent {
			pendingPrefill = input.text
			pendingPrefillModelPrompt = input.modelPrompt
			pendingPrefillPasted = input.pasted
			promptPrinted = false
			if app.cycleAgent() {
				scheduleShiftTabPrewarm()
			}
			return false, ExitOK
		}
		action := app.handlePromptInput(input, readCommandLine)
		promptPrinted = false
		if action.exit {
			return true, ExitOK
		}
		if action.shell {
			app.runShellEscape(action.shellCommand)
			return false, ExitOK
		}
		if action.prefillSet || action.prefill != "" {
			if usePromptEditor {
				pendingPrefill = action.prefill
				pendingPrefillModelPrompt = action.prefillModelPrompt
				pendingPrefillPasted = false
			} else {
				// Without the prompt editor there is no way to prefill for
				// review; echo the loaded text and submit it directly,
				// bypassing command and shell dispatch while preserving normal
				// prompt enrichment for editor output.
				app.echoEditedPrompt(prompt, action.prefill)
				return startPromptInteraction(action.prefill, true, true)
			}
			return false, ExitOK
		}
		if action.run {
			if action.echoEditedPrompt {
				app.echoEditedPrompt(prompt, action.prompt)
			}
			return startPromptInteraction(action.prompt, action.resolveSkillMentions, action.attachPromptImages)
		}
		return false, ExitOK
	}
	handleIdleReadResult := func(res replReadResult) (exit bool, code int) {
		readPending = false
		cancelIdleCompaction()
		if res.input.ended {
			inputEnded = true
			return false, ExitOK
		}
		if plainPromptRead {
			plainPromptRead = false
			enableIdlePromptTerm()
		}
		if !res.ok {
			setInputEnded(res.err)
			return false, ExitOK
		}
		if res.input.interrupt {
			return true, ExitInterrupt
		}
		return applyAction(res.input)
	}

	if initialPrompt != nil {
		if app.Renderer != nil {
			app.Renderer.StartPrompt()
		}
		ctx, cancel, interrupted := exitContext()
		err := app.refreshMCP(ctx)
		cancel()
		if interrupted() || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return finish(ExitInterrupt)
		}
		startPromptRun(*initialPrompt, true, true)
	}

	for {
		if active {
			if !activeReadPause {
				req := replReadRequest{}
				if usePromptEditor {
					req.promptEdit = true
				}
				requestRead(req)
			}
			select {
			case <-exit:
				return forceFinish()
			case finished := <-idleCompactionDone:
				deferIdleCompaction(finished)
			case <-promptDone:
				if app.Renderer != nil {
					app.Renderer.StopProgress()
				}
				if usePromptEditor {
					// Release the blocked during-prompt keystroke read so any
					// unsubmitted partial buffer becomes the next prompt's editable
					// prefill. A line already submitted with Enter is queued below.
					// The terminal stays in raw mode for the line editor; only
					// bracketed paste is restored.
					if readPending {
						reader.cancelPromptRead()
						res := <-inputs
						readPending = false
						if res.ok && res.input.deposit {
							pendingPrefill = res.input.text
							pendingPrefillModelPrompt = false
							pendingPrefillPasted = false
						} else if res.ok && !res.input.escape && !res.input.interrupt && (res.input.text != "" || res.input.edit) {
							queued = append(queued, res.input)
						} else {
							// The read returned via a keystroke (interrupt/Esc)
							// rather than the cancel; the typed buffer is intact.
							pendingPrefill = reader.promptBuffer()
							pendingPrefillModelPrompt = false
							pendingPrefillPasted = false
						}
						reader.drainPromptCancel()
					}
					// When no read is pending an EOF-driven deposit was already
					// stashed in pendingPrefill via the active inputs case; leave
					// it as-is.
					_ = term.SetBracketedPaste(true)
				} else {
					disableActivePromptTerm()
				}
				active = false
				activeReadPause = false
				promptDone = nil
				app.promptActive = false
				// A prewarm requested mid-prompt (agent/model switch) fires now,
				// once, for the settled selection: the prompt kept its own prefix
				// warm, but the switched-to agent's is genuinely cold — including
				// after interrupt/error exits.
				app.releasePendingPrewarm()
				escPresses.reset()
				// The run no longer owns usage/renderer/session state: flush the
				// accounting deferred while it was active, exactly once, before the
				// next beginPrompt or save drains the queue.
				for _, ev := range deferredIdleEvents {
					app.recordEvent(ev)
				}
				deferredIdleEvents = nil
				app.drainMaintenanceUsage()
				// Recover any steer submitted during the prompt that the loop never
				// consumed (the prompt ended without another turn to inject into, or
				// was broken by budget/cancel). Run it as the next prompt so the
				// input is not silently lost, ahead of other queued input.
				if leftover := app.drainLeftoverSteer(); !steerInputEmpty(leftover) {
					preparedQueued = append([]agent.SteerInput{leftover}, preparedQueued...)
				}
				if app.hasPendingHandoffRequest() {
					approvalInterrupted := false
					readHandoffLine := readCommandLine
					if !usePromptEditor && readPending {
						readHandoffLine = func(label string) (string, error) {
							if _, err := fmt.Fprint(app.Errw, label); err != nil {
								return "", err
							}
							select {
							case <-exit:
								approvalInterrupted = true
								return "", context.Canceled
							case res := <-inputs:
								readPending = false
								if res.input.ended {
									inputEnded = true
									return "", io.EOF
								}
								if !res.ok {
									setInputEnded(res.err)
									if res.err != nil {
										return "", res.err
									}
									return "", io.EOF
								}
								if res.input.interrupt {
									approvalInterrupted = true
									return "", context.Canceled
								}
								return strings.TrimSpace(res.input.text), nil
							}
						}
					}
					if app.handoffCommand("", readHandoffLine) {
						if exit, code := startPromptInteraction(implementationStartPrompt, true, false); exit {
							return finish(code)
						}
						continue
					}
					if approvalInterrupted {
						return finish(ExitInterrupt)
					}
				}
				if !usePromptEditor && readPending {
					// A plain read started during the prompt is still
					// blocked. Let it collect the next line in canonical mode;
					// starting the raw prompt editor now would leave no prompt
					// drawn and no terminal echo until that stale read finishes.
					plainPromptRead = true
				} else if !usePromptEditor {
					enableIdlePromptTerm()
				}
			case res := <-inputs:
				readPending = false
				if res.input.ended {
					inputEnded = true
					continue
				}
				if !res.ok {
					setInputEnded(res.err)
					continue
				}
				input := res.input
				if input.interrupt {
					if app.Interrupt != nil {
						app.Interrupt.InterruptPrompt()
					}
					continue
				}
				if input.deposit {
					// Reached only on EOF during a prompt (the cancel-driven deposit is
					// drained at prompt completion). Stash it and stop reading
					// until the prompt ends.
					pendingPrefill = input.text
					pendingPrefillModelPrompt = false
					pendingPrefillPasted = false
					activeReadPause = true
					continue
				}
				if input.escapeTail {
					escPresses.reset()
					continue
				}
				if input.escape {
					if input.text != "" {
						queued = append(queued, replInput{text: input.text})
					}
					if escPresses.press(app.clock()()) && app.Interrupt != nil {
						app.Interrupt.CancelPrompt()
					}
					continue
				}
				escPresses.reset()
				if app.steerDuringPrompt(input) {
					continue
				}
				queued = append(queued, input)
			}
			continue
		}

		if len(preparedQueued) > 0 {
			input := preparedQueued[0]
			preparedQueued = preparedQueued[1:]
			if exit, code := startPreparedPromptInteraction(input); exit {
				return finish(code)
			}
			continue
		}

		if len(queued) > 0 {
			input := queued[0]
			queued = queued[1:]
			if input.interrupt {
				return finish(ExitInterrupt)
			}
			if exit, code := applyAction(input); exit {
				return finish(code)
			}
			continue
		}
		if inputEnded {
			warnInputErr()
			return finish(ExitOK)
		}
		scheduleIdleCompaction()
		if !promptPrinted {
			prompt = renderPrompt()
			app.pollBackgroundNotices()
			if !app.todoPromptStatusPrintedBeforeUsageForPrompt(app.PromptNumber) {
				app.printTodoPromptStatus()
			}
			if !app.planPromptStatusPrintedBeforeUsageForPrompt(app.PromptNumber) {
				app.printPlanStatus(plan.DisplayCurrent)
			}
			if !usePromptEditor || plainPromptRead {
				fmt.Fprint(app.Errw, prompt)
			}
			promptPrinted = true
		}
		if !plainPromptRead {
			requestRead(replReadRequest{prompt: prompt, promptEditor: usePromptEditor, replPrompt: true, prefill: pendingPrefill, prefillModelPrompt: pendingPrefillModelPrompt, prefillPasted: pendingPrefillPasted})
			pendingPrefill = ""
			pendingPrefillModelPrompt = false
			pendingPrefillPasted = false
		}
		select {
		case <-exit:
			// SIGINT exit request at the idle prompt (design §8.4).
			return finish(ExitInterrupt)
		case <-pendingIdleCompaction:
			startIdleCompaction()
		case finished := <-idleCompactionDone:
			// Prefer already-submitted input over applying a candidate that
			// became ready at the same instant.
			select {
			case res := <-inputs:
				finishIdleCompaction(finished, false)
				if exit, code := handleIdleReadResult(res); exit {
					return finish(code)
				}
			default:
				if finishIdleCompaction(finished, true) {
					promptPrinted = false
				}
			}
		case <-pendingShiftTabPrewarm:
			expired := pendingShiftTabPrewarm
			// If a submitted line and the debounce become ready together, honor the
			// input first so a real prompt can cancel the delayed warm-up. Non-model
			// commands still allow the already-settled warm-up to run.
			select {
			case res := <-inputs:
				if exit, code := handleIdleReadResult(res); exit {
					return finish(code)
				}
				if inputEnded {
					pendingShiftTabPrewarm = nil
				} else if pendingShiftTabPrewarm == expired {
					pendingShiftTabPrewarm = nil
					prewarm()
				}
			default:
				pendingShiftTabPrewarm = nil
				prewarm()
			}
		case res := <-inputs:
			if exit, code := handleIdleReadResult(res); exit {
				return finish(code)
			}
		}
	}
}

func (app *App) promptValues(t *replprompt.Template, viMode string) replprompt.Values {
	var cwd string
	if t.Uses("cwd") || t.Uses("git_branch") {
		var err error
		cwd, err = os.Getwd()
		if err != nil {
			cwd = ""
		}
	}
	var hostname string
	if t.UsesHostname() {
		var err error
		hostname, err = os.Hostname()
		if err != nil {
			hostname = ""
		}
	}
	var gitBranch string
	if t.Uses("git_branch") {
		gitBranch = replprompt.CurrentGitBranch(cwd)
	}
	return replprompt.Values{
		Agent:     app.AgentName,
		CWD:       cwd,
		Hostname:  hostname,
		GitBranch: gitBranch,
		Model:     app.Model,
		Reasoning: app.promptReasoningLabel(),
		ViMode:    viMode,
	}
}

// idleViMode returns the raw-prompt vi mode name to render at idle boundaries
// (before a read starts). The editor always begins a read in insert mode, so
// vi mode renders "insert"; emacs mode (and any non-vi value) renders "" so a
// {vimode} placeholder stays empty outside vi mode.
func idleViMode(editMode string) string {
	if promptEditMode(editMode) == promptEditModeVi {
		return "insert"
	}
	return ""
}

// viModeName maps an editor viMode to the raw mode name carried by
// replprompt.Values, so the viPrompt callback can re-render the prompt for the
// current mode on each transition. Non-vi modes map to "" (empty label).
func viModeName(m viMode) string {
	switch m {
	case viModeInsert:
		return "insert"
	case viModeNormal:
		return "normal"
	default:
		return ""
	}
}

func promptLineEditorEnabled(in io.Reader, w io.Writer) bool {
	inf, ok := in.(*os.File)
	if !ok || !term.IsTerminal(inf) {
		return false
	}
	wf, ok := unwrapOutputWriter(w).(*os.File)
	return ok && term.IsTerminal(wf)
}

type replInput struct {
	text        string
	pasted      bool
	edit        bool
	cycleAgent  bool
	escape      bool
	escapeTail  bool
	interrupt   bool
	interactive bool
	// modelPrompt marks input already classified as model-bound prompt text. It
	// bypasses prompt-level command and shell dispatch while preserving normal
	// prompt enrichment.
	modelPrompt bool
	// deposit marks an accumulated during-prompt buffer that did not end with Enter;
	// it is handed back as editable prefill in the next prompt.
	deposit bool
	// ended marks an inert reply the read goroutine sends after the reader has
	// ended (EOF or a terminal read error) so a racing requestRead does not block
	// on an orphaned channel. The main loop ignores these and exits via inputEnded.
	ended bool
}

type replReadResult struct {
	input replInput
	ok    bool
	err   error
}

type replReadRequest struct {
	prompt       string
	promptEditor bool
	// replPrompt marks the main idle REPL prompt. Only that prompt may use the
	// configured live vi-mode prompt renderer; auxiliary reads keep their own
	// context-specific labels while still using the raw line editor.
	replPrompt bool
	// promptEdit routes the read through the during-prompt keystroke capture: echo
	// stays off, keystrokes accumulate into a shared buffer rendered live on the
	// status line, and the read returns only on Ctrl-C, Esc, or cancellation
	// (during-prompt input).
	promptEdit bool
	// prefill seeds the prompt editor with editable text, used to deposit a partial
	// during-prompt buffer or external editor output into the next prompt.
	prefill string
	// prefillModelPrompt marks the submitted prefill as model-bound prompt text;
	// used for external editor output reviewed in the line editor.
	prefillModelPrompt bool
	// prefillPasted retains pure-paste literal classification across Shift-Tab.
	prefillPasted bool
}

type replAction struct {
	prompt               string
	run                  bool
	exit                 bool
	shell                bool
	shellCommand         string
	echoEditedPrompt     bool
	resolveSkillMentions bool
	attachPromptImages   bool
	// prefill deposits text into the next prompt as editable content instead
	// of running a turn. Used when returning from an external editor so the
	// user can review before submitting.
	prefill    string
	prefillSet bool
	// prefillModelPrompt marks the eventual submitted prefill as model-bound text.
	prefillModelPrompt bool
}

type replCommandResult struct {
	exit                 bool
	prompt               string
	prefill              string
	prefillSet           bool
	resolveSkillMentions bool
	attachPromptImages   bool
}

const implementationStartPrompt = "Begin implementing the recorded plan now."

type escapePresses struct {
	last time.Time
	seen bool
}

func (p *escapePresses) press(now time.Time) bool {
	if p.seen && now.Sub(p.last) <= time.Second {
		p.reset()
		return true
	}
	p.last = now
	p.seen = true
	return false
}

func (p *escapePresses) reset() {
	p.last = time.Time{}
	p.seen = false
}

// drainLeftoverSteer recovers prepared steer input the just-finished prompt
// never injected, returning it so the run loop can queue it as the next prompt. It
// returns an empty input when steering is disabled.
func (app *App) drainLeftoverSteer() agent.SteerInput {
	if app.DrainSteer == nil {
		return agent.SteerInput{}
	}
	return app.DrainSteer()
}

func steerInputEmpty(input agent.SteerInput) bool {
	if strings.TrimSpace(input.Text) != "" {
		return false
	}
	if len(input.Images) > 0 {
		return false
	}
	for _, item := range input.RequestContext {
		if strings.TrimSpace(item) != "" {
			return false
		}
	}
	return true
}

// steerDuringPrompt routes a during-prompt-submitted input into the agent as a
// in-prompt steering message when steering is enabled and the input is
// model-bound (would start a prompt at idle). It returns true when it
// consumed the input by steering. Non-model-bound input (shell escapes,
// /commands, /edit requests) and any input when Steer is nil return false so the
// caller queues them for the next prompt. The classification
// mirrors handlePromptInput's prefix dispatch but performs no side effects,
// since /commands and /edit must not run inside an active prompt.
func (app *App) steerDuringPrompt(input replInput) bool {
	if app.Steer == nil {
		return false
	}
	if input.escape || input.interrupt || input.deposit || input.edit {
		return false
	}
	line := input.text
	if line == "" {
		return false
	}
	if !input.interactive && !input.pasted {
		return false
	}
	if input.pasted {
		if steered, ok := app.prepareSteerInput(line, promptOptions{}); ok {
			app.Steer(steered)
		}
		return true
	}
	if input.interactive {
		// Mirror handlePromptInput's escape-prefix stripping so a steered !!foo
		// or //foo reaches the model as !foo / /foo, exactly as it would at the
		// idle prompt.
		if strings.HasPrefix(line, "!!") || strings.HasPrefix(line, "//") {
			if steered, ok := app.prepareSteerInput(line[1:], promptOptions{resolveSkillMentions: true, attachPromptImages: true}); ok {
				app.Steer(steered)
			}
			return true
		}
		// !shell escapes and /commands (including /edit) are not model input —
		// leave them queued for the idle prompt.
		if strings.HasPrefix(line, "!") || strings.HasPrefix(line, "/") {
			return false
		}
	}
	if steered, ok := app.prepareSteerInput(line, promptOptions{resolveSkillMentions: true, attachPromptImages: true}); ok {
		app.Steer(steered)
	}
	return true
}

func (app *App) handlePromptInput(input replInput, readCommandLine func(string) (string, error)) replAction {
	if input.escape {
		return replAction{}
	}
	line := input.text
	if line == "" && !input.edit {
		return replAction{}
	}
	if input.edit {
		if prompt, ok := app.editPrompt(line); ok {
			return replAction{prefill: prompt, prefillSet: true, prefillModelPrompt: true}
		}
		return replAction{}
	}
	if input.modelPrompt {
		return replAction{prompt: line, run: true, resolveSkillMentions: true, attachPromptImages: true}
	}
	if input.pasted {
		return replAction{prompt: line, run: true}
	}
	if input.interactive && strings.HasPrefix(line, "!!") {
		return replAction{prompt: line[1:], run: true, resolveSkillMentions: true, attachPromptImages: true} // !! escapes one literal leading !
	}
	if input.interactive && strings.HasPrefix(line, "!") {
		command := strings.TrimSpace(line[1:])
		if command == "" {
			return replAction{}
		}
		return replAction{shell: true, shellCommand: command}
	}
	if strings.HasPrefix(line, "//") {
		return replAction{prompt: line[1:], run: true, resolveSkillMentions: true, attachPromptImages: true} // // escapes one literal leading slash
	}
	if strings.HasPrefix(line, "/") {
		cmd, arg := commandFields(line)
		if cmd == "/edit" {
			if prompt, ok := app.editPrompt(arg); ok {
				return replAction{prefill: prompt, prefillSet: true, prefillModelPrompt: true}
			}
			return replAction{}
		}
		result := app.command(line, readCommandLine)
		if result.exit {
			return replAction{exit: true}
		}
		if result.prefillSet {
			return replAction{prefill: result.prefill, prefillSet: true, prefillModelPrompt: true}
		}
		if result.prompt != "" {
			return replAction{prompt: result.prompt, run: true, resolveSkillMentions: result.resolveSkillMentions, attachPromptImages: result.attachPromptImages}
		}
		return replAction{}
	}
	return replAction{prompt: line, run: true, resolveSkillMentions: true, attachPromptImages: true}
}

func (app *App) echoEditedPrompt(replPrompt, submitted string) {
	if f, ok := unwrapOutputWriter(app.Errw).(*os.File); ok && term.IsTerminal(f) {
		fmt.Fprintf(app.Errw, "\r\x1b[2K%s%s\n", replPrompt, submitted)
		return
	}
	fmt.Fprintln(app.Errw, submitted)
}

func commandFields(line string) (cmd, arg string) {
	cmd, arg, _ = strings.Cut(strings.TrimSpace(line), " ")
	return cmd, strings.TrimSpace(arg)
}

func inputMayOpenEditor(input replInput) bool {
	if input.edit {
		return true
	}
	if input.pasted {
		return false
	}
	cmd, _ := commandFields(input.text)
	return cmd == "/edit"
}

func queuedContainsEditor(inputs []replInput) bool {
	for _, input := range inputs {
		if inputMayOpenEditor(input) {
			return true
		}
	}
	return false
}

// cancelableReader wraps an io.Reader so a blocked read can be released without
// losing buffered bytes. A pump goroutine copies the underlying stream into a
// channel; Read serves from there and returns errReadCanceled when cancel()
// fires, leaving any not-yet-delivered bytes queued for the next Read. This lets
// the REPL hand the terminal from the during-prompt keystroke capture back to the
// full line editor at a prompt boundary without dropping a keystroke
// (during-prompt input).
type cancelableReader struct {
	chunks   chan readChunk
	leftover []byte
	err      error         // sticky terminal error once the pump reports one
	cancel   chan struct{} // buffered(1); a queued token cancels the next Read
	// pending counts bytes the pump has read off the underlying fd but Read has
	// not yet returned to the caller (queued chunk + leftover). It lets readiness
	// probes see input the eager pump already drained off the fd, which
	// WaitReadable on that fd can no longer report (during-prompt escape decoding).
	pending atomic.Int64
}

type readChunk struct {
	data []byte
	err  error
}

var errReadCanceled = errors.New("repl: read canceled")

func newCancelableReader(in io.Reader) *cancelableReader {
	cr := &cancelableReader{
		chunks: make(chan readChunk, 1),
		cancel: make(chan struct{}, 1),
	}
	go func() {
		buf := make([]byte, 4096)
		for {
			n, err := in.Read(buf)
			if n > 0 {
				// Count the bytes as buffered Go-side before handing them off, so a
				// concurrent buffered() probe never undercounts input the pump has
				// already pulled off the fd.
				cr.pending.Add(int64(n))
				cr.chunks <- readChunk{data: append([]byte(nil), buf[:n]...)}
			}
			if err != nil {
				cr.chunks <- readChunk{err: err}
				return
			}
		}
	}()
	return cr
}

func (cr *cancelableReader) Read(p []byte) (int, error) {
	if len(cr.leftover) > 0 {
		n := copy(p, cr.leftover)
		cr.leftover = cr.leftover[n:]
		cr.pending.Add(int64(-n))
		return n, nil
	}
	if cr.err != nil {
		// The pump has stopped; report the terminal error persistently so a read
		// after EOF returns EOF rather than blocking on a dead channel.
		return 0, cr.err
	}
	select {
	case <-cr.cancel:
		return 0, errReadCanceled
	case chunk := <-cr.chunks:
		if chunk.err != nil {
			cr.err = chunk.err
			return 0, chunk.err
		}
		n := copy(p, chunk.data)
		cr.leftover = chunk.data[n:]
		cr.pending.Add(int64(-n))
		return n, nil
	}
}

// buffered reports how many bytes the pump has read off the underlying fd but
// not yet returned through Read (the queued chunk plus any leftover). Readiness
// probes OR this in so a split escape sequence whose tail the pump already
// drained off the fd is still seen as available (during-prompt escape decoding).
func (cr *cancelableReader) buffered() int {
	if n := cr.pending.Load(); n > 0 {
		return int(n)
	}
	return 0
}

// cancel queues a cancel token so the next Read (whether already blocked or not
// yet started) returns errReadCanceled exactly once; queuing the token rather
// than closing a channel avoids losing a cancel that races read startup.
func (cr *cancelableReader) cancelRead() {
	select {
	case cr.cancel <- struct{}{}:
	default:
	}
}

// drainCancel clears an unconsumed cancel token so a later read is not spuriously
// canceled (e.g. when the canceled read happened to return via a keystroke).
func (cr *cancelableReader) drainCancel() {
	select {
	case <-cr.cancel:
	default:
	}
}

type replReader struct {
	r             *bufio.Reader
	editor        *promptLineEditor
	paste         strings.Builder
	inPaste       bool
	escapeLineEnd atomic.Bool

	// During-prompt keystroke capture. The active prompt shares the
	// promptLineEditor's lineEditState/viLineState/history so it gets the same
	// editing grammar (Ctrl-A/E/B/F, arrows, word motions, kill commands, full vi
	// mode, vi-mode line-aware up/down history) as the idle prompt. The only
	// difference is display:
	// the idle prompt redraws the multi-row terminal region, while the active prompt mirrors
	// buf/cursor onto the single status line via onPromptInput (it cannot use the
	// multi-row redraw while output streams). promptState is created fresh at each
	// prompt start; onPromptInput renders the live buffer and cursor, and cancelable
	// releases a blocked read so a partial buffer can be deposited at the prompt boundary.
	promptState   *lineEditState
	promptVi      viLineState
	promptHistory lineEditHistory
	onPromptInput func(string, int)
	cancelable    *cancelableReader
}

func newREPLReader(in io.Reader, promptWriter io.Writer, promptEditor bool, editMode string) *replReader {
	rr := &replReader{}
	source := in
	if promptEditor {
		// The interactive path needs cancelable reads so a during-prompt keystroke
		// capture can hand the terminal back to the line editor at prompt end.
		rr.cancelable = newCancelableReader(in)
		source = rr.cancelable
	}
	r := bufio.NewReader(source)
	rr.r = r
	if promptEditor {
		rr.editor = newPromptLineEditorWithReader(r, promptWriter)
		rr.editor.setEditMode(editMode)
		if f, ok := in.(*os.File); ok {
			cancelable := rr.cancelable
			// The non-bracketed paste-burst heuristic is a fallback for terminals that
			// do not honor bracketed paste: it detects a fast keystroke burst and
			// treats newlines as inserts (filling the buffer for review) instead of
			// submitting prematurely. It is interactive-only (a real fd) and on by
			// default; HARNESS_REPL_PASTE_HEURISTIC=off disables it.
			rr.editor.configurePasteHeuristic(pasteHeuristicEnabled(), time.Now)
			rr.editor.escapeSequenceReady = func(timeout time.Duration) bool {
				// The pump eagerly drains f, so a split escape sequence's tail may
				// already sit in the cancelableReader's Go-side buffers where
				// WaitReadable(f) can no longer see it. Consult those buffers first
				// so arrow/Home/End keys are not mis-read as bare Esc + literals.
				if cancelable != nil && cancelable.buffered() > 0 {
					return true
				}
				return term.WaitReadable(f, timeout)
			}
		}
	}
	return rr
}

func (rr *replReader) setEscapeLineEnd(enabled bool) {
	rr.escapeLineEnd.Store(enabled)
}

func (rr *replReader) read(req replReadRequest) (replInput, bool, error) {
	if req.promptEdit {
		return rr.readDuringPrompt()
	}
	if req.promptEditor && rr.editor != nil {
		restoreViPrompt := rr.editor.viPrompt
		restoreNoHistory := rr.editor.noHistory
		restoreCycleAgent := rr.editor.cycleAgent
		rr.editor.cycleAgent = req.replPrompt
		if !req.replPrompt {
			rr.editor.viPrompt = nil
			rr.editor.noHistory = true
		}
		defer func() {
			rr.editor.viPrompt = restoreViPrompt
			rr.editor.noHistory = restoreNoHistory
			rr.editor.cycleAgent = restoreCycleAgent
		}()
		input, ok, err := rr.editor.readPrefilledClassified(req.prompt, req.prefill, req.prefillPasted)
		if ok {
			input.interactive = true
			if req.prefillModelPrompt {
				input.modelPrompt = true
			}
		}
		return input, ok, err
	}
	for {
		line, terminator, err := readTerminalLine(rr.r, rr.escapeLineEnd.Load())
		if line != "" || terminator != lineTermNone {
			if input, emit := rr.handleLine(line, terminator); emit {
				return input, true, nil
			}
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				if rr.inPaste && rr.paste.Len() > 0 {
					input := replInput{text: normalizePastedNewlines(rr.paste.String()), pasted: true}
					rr.paste.Reset()
					rr.inPaste = false
					return input, true, nil
				}
				return replInput{}, false, nil
			}
			return replInput{}, false, err
		}
	}
}

// beginPromptCapture seeds a fresh during-prompt editor state (empty buffer, insert
// mode, history anchored at the end) for the upcoming readDuringPrompt. It is called at
// prompt start so the live status line begins empty.
func (rr *replReader) beginPromptCapture() {
	rr.promptState = &lineEditState{}
	rr.promptVi = viLineState{mode: viModeInsert}
	rr.promptHistory = rr.editor.historyState()
}

// readDuringPrompt captures keystrokes during an active prompt with echo off, sharing the
// promptLineEditor's full editing grammar via handleKey (duringPrompt=true). The
// buffer is mirrored live on the status line via onPromptInput. It returns to the
// caller on Enter (queued next-prompt submission), Ctrl-C (interrupt), bare Esc (for
// double-Esc cancel), Ctrl-G (edit), cancellation (depositing a partial buffer),
// or EOF. Shift-Enter/raw LF inserts a newline.
func (rr *replReader) readDuringPrompt() (replInput, bool, error) {
	if rr.promptState == nil {
		rr.beginPromptCapture()
	}
	s := rr.promptState
	for {
		r, _, err := rr.r.ReadRune()
		if err != nil {
			if errors.Is(err, errReadCanceled) {
				return rr.depositPromptBuffer(), true, nil
			}
			if errors.Is(err, io.EOF) {
				if dep := rr.depositPromptBuffer(); dep.text != "" {
					return dep, true, nil
				}
				return replInput{}, false, nil
			}
			return replInput{}, false, err
		}
		result, err := rr.editor.handleKey(&rr.promptVi, s, &rr.promptHistory, "", r, true)
		if err != nil {
			if errors.Is(err, errReadCanceled) {
				return rr.depositPromptBuffer(), true, nil
			}
			return replInput{}, false, err
		}
		if result.done {
			if result.redraw {
				rr.emitPromptInput()
			}
			if result.input.interrupt {
				return replInput{interrupt: true}, true, nil
			}
			if result.input.text != "" || result.input.pasted || result.input.interactive {
				// Enter during a prompt: the editor committed the buffer as queued
				// next-prompt input. Hand it to the run loop, which queues it to run
				// after the current prompt; capture keeps reading further input.
				return result.input, true, nil
			}
			if result.input.edit {
				// Ctrl-G during a prompt: hand the buffer to the run loop as an edit
				// request (queued, then $EDITOR opens after the prompt ends).
				text := string(s.buf)
				rr.resetPromptBuffer()
				return replInput{text: text, edit: true}, true, nil
			}
			if result.input.escape {
				return replInput{escape: true}, true, nil
			}
			// EOF with no buffer (ok=false): end the read without a deposit.
			if dep := rr.depositPromptBuffer(); dep.text != "" {
				return dep, true, nil
			}
			return replInput{}, false, nil
		}
		if result.redraw {
			rr.emitPromptInput()
		}
	}
}

// depositPromptBuffer returns the accumulated buffer as an editable deposit and
// resets the prompt-edit state for the next prompt.
func (rr *replReader) depositPromptBuffer() replInput {
	text := ""
	if rr.promptState != nil {
		text = string(rr.promptState.buf)
	}
	rr.resetPromptBuffer()
	return replInput{text: text, deposit: true}
}

// resetPromptBuffer clears the during-prompt buffer and cursor and emits an empty
// status line so a closed-out read leaves no stale input painted.
func (rr *replReader) resetPromptBuffer() {
	if rr.promptState != nil {
		rr.promptState.buf = nil
		rr.promptState.cursor = 0
		rr.promptState.clearPasteSummaries()
	}
	rr.emitPromptInput()
}

func (rr *replReader) emitPromptInput() {
	if rr.onPromptInput != nil {
		if rr.promptState != nil {
			rr.onPromptInput(string(rr.promptState.buf), rr.promptState.cursor)
			return
		}
		rr.onPromptInput("", 0)
	}
}

// cancelPromptRead releases a blocked during-prompt keystroke read so it deposits
// its buffer; a no-op without a cancelable reader.
func (rr *replReader) cancelPromptRead() {
	if rr.cancelable != nil {
		rr.cancelable.cancelRead()
	}
}

// drainPromptCancel clears any unconsumed cancel token so a later prompt read is
// not spuriously canceled.
func (rr *replReader) drainPromptCancel() {
	if rr.cancelable != nil {
		rr.cancelable.drainCancel()
	}
}

// promptBuffer returns the current during-prompt buffer without consuming it.
func (rr *replReader) promptBuffer() string {
	if rr.promptState == nil {
		return ""
	}
	return string(rr.promptState.buf)
}

type lineTerminator byte

const (
	lineTermNone       lineTerminator = 0
	lineTermNewline    lineTerminator = '\n'
	lineTermEdit       lineTerminator = '\a'
	lineTermEscape     lineTerminator = '\x1b'
	lineTermEscapeTail lineTerminator = 0x80
)

func readTerminalLine(r *bufio.Reader, escapeLineEnd bool) (line string, terminator lineTerminator, err error) {
	var b strings.Builder
	for {
		c, err := r.ReadByte()
		if err != nil {
			return b.String(), lineTermNone, err
		}
		switch c {
		case '\n':
			line := b.String()
			line = strings.TrimSuffix(line, "\r")
			return line, lineTermNewline, nil
		case byte(lineTermEdit):
			return b.String(), lineTermEdit, nil
		default:
			if escapeLineEnd && c == byte(lineTermEscape) {
				consumed, err := consumeBufferedEscapeTail(r)
				if err != nil {
					return b.String(), lineTermNone, err
				}
				if consumed {
					return b.String(), lineTermEscapeTail, nil
				}
				return b.String(), lineTermEscape, nil
			}
			b.WriteByte(c)
		}
	}
}

func consumeBufferedEscapeTail(r *bufio.Reader) (bool, error) {
	if r == nil || r.Buffered() == 0 {
		return false, nil
	}
	next, err := r.Peek(1)
	if err != nil {
		return false, err
	}
	if len(next) == 0 || (next[0] != '[' && next[0] != 'O') {
		return false, nil
	}
	introducer, err := r.ReadByte()
	if err != nil {
		return false, err
	}
	switch introducer {
	case '[':
		for {
			c, err := r.ReadByte()
			if err != nil {
				return true, err
			}
			if c >= '@' && c <= '~' {
				return true, nil
			}
		}
	case 'O':
		_, err := r.ReadByte()
		return true, err
	default:
		return false, nil
	}
}

func (rr *replReader) handleLine(line string, terminator lineTerminator) (replInput, bool) {
	if !rr.inPaste {
		start := strings.Index(line, bracketedPasteStart)
		if start < 0 {
			if terminator == lineTermEscapeTail || isSplitEscapeTail(line, terminator) {
				if line == "" {
					return replInput{}, false
				}
				return replInput{text: line, escapeTail: true}, true
			}
			return replInput{text: line, edit: terminator == lineTermEdit, escape: terminator == lineTermEscape}, true
		}
		rr.inPaste = true
		rr.paste.WriteString(line[:start])
		line = line[start+len(bracketedPasteStart):]
	}

	end := strings.Index(line, bracketedPasteEnd)
	if end >= 0 {
		rr.paste.WriteString(line[:end])
		// Normalize only the pasted portion: a bare-CR- or CRLF-terminated paste
		// otherwise leaks raw carriage returns into the submitted text.
		text := normalizePastedNewlines(rr.paste.String()) + line[end+len(bracketedPasteEnd):]
		rr.paste.Reset()
		rr.inPaste = false
		return replInput{text: text, pasted: true}, true
	}

	rr.paste.WriteString(line)
	switch terminator {
	case lineTermNewline:
		rr.paste.WriteByte('\n')
	case lineTermEdit:
		rr.paste.WriteByte(byte(lineTermEdit))
	}
	return replInput{}, false
}

func isSplitEscapeTail(line string, terminator lineTerminator) bool {
	if terminator != lineTermEscape || line == "" {
		return false
	}
	switch line[0] {
	case '[':
		return hasTerminalFinalByte(line[1:])
	case 'O':
		return len(line) >= 2 && line[1] >= '@' && line[1] <= '~'
	default:
		return false
	}
}

func hasTerminalFinalByte(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] >= '@' && s[i] <= '~' {
			return true
		}
	}
	return false
}

// command dispatches a meta-command line.
func (app *App) command(line string, readCommandLine func(string) (string, error)) replCommandResult {
	cmd, arg := commandFields(line)

	switch cmd {
	case "/help":
		fmt.Fprintln(app.Errw, helpText)
	case "/exit", "/quit":
		return replCommandResult{exit: true}
	case "/clear":
		app.clear()
	case "/compact":
		app.compact(arg)
	case "/tree":
		prefill, set := app.treeCommand(arg, readCommandLine)
		return replCommandResult{prefill: prefill, prefillSet: set}
	case "/fork":
		prefill, set := app.forkCommand(arg, readCommandLine)
		return replCommandResult{prefill: prefill, prefillSet: set}
	case "/clone":
		app.cloneCommand()
	case "/context":
		app.contextDump(arg)
	case "/usage":
		fmt.Fprintln(app.Errw, app.usageSummary())
	case "/image":
		app.imageCommand(arg)
	case "/edit":
		if prompt, ok := app.editPrompt(arg); ok {
			if run, ok := app.preparePromptRun(prompt, promptOptions{resolveSkillMentions: true, attachPromptImages: true}); ok {
				run()
			}
		}
	case "/save":
		path := app.SessionPath
		if arg != "" {
			path = arg
		}
		if err := app.save(path); err != nil {
			fmt.Fprintf(app.Errw, "[save failed: %v]\n", err)
		} else {
			fmt.Fprintf(app.Errw, "[saved %s]\n", path)
		}
	case "/model":
		if arg == "" {
			app.pickModel(readCommandLine)
		} else {
			app.switchModelAndPromptDefault(arg, app.Reasoning, readCommandLine)
		}
	case "/reasoning":
		app.reasoningCommand(arg)
	case "/effort":
		app.effort(arg)
	case "/fast":
		app.fastCommand(arg)
	case "/agent", "/mode":
		if arg == "" {
			fmt.Fprintln(app.Errw, app.agentSummary())
		} else {
			app.switchAgent(arg)
		}
	case "/plan":
		if arg == "" {
			arg = "plan"
		}
		app.switchAgent(arg)
	case "/auto":
		if arg == "" {
			arg = "auto"
		}
		app.switchAgent(arg)
	case "/handoff":
		if app.handoffCommand(arg, readCommandLine) {
			return replCommandResult{prompt: implementationStartPrompt, resolveSkillMentions: true}
		}
	case "/background":
		app.backgroundCommand(arg)
	case "/skills":
		fmt.Fprintln(app.Errw, app.skillsSummary())
	case "/tools":
		fmt.Fprintln(app.Errw, app.toolsSummary())
	case "/vi":
		app.viCommand(arg)
	default:
		if suggestion := suggestCommand(cmd); suggestion != "" {
			fmt.Fprintf(app.Errw, "unknown command %q; did you mean %s? (type /help)\n", cmd, suggestion)
		} else {
			fmt.Fprintf(app.Errw, "unknown command %q; type /help\n", cmd)
		}
	}
	return replCommandResult{}
}

// knownCommands is the meta-command vocabulary used for "did you mean …?"
// suggestions on an unknown command (r59).
var knownCommands = []string{
	"/help", "/exit", "/quit", "/clear", "/compact", "/tree", "/fork", "/clone", "/context", "/usage",
	"/tools", "/image", "/edit", "/save", "/model", "/reasoning", "/effort", "/fast",
	"/agent", "/mode", "/plan", "/auto", "/handoff", "/background", "/skills", "/vi",
}

// suggestCommand returns the closest known command to cmd, or "" when nothing is
// close enough. A shared prefix wins; otherwise the nearest by edit distance
// within a small threshold scaled to the command length.
func suggestCommand(cmd string) string {
	cmd = strings.ToLower(strings.TrimSpace(cmd))
	if len(cmd) < 2 {
		return ""
	}
	best, bestDist := "", 1<<30
	for _, known := range knownCommands {
		if strings.HasPrefix(known, cmd) || strings.HasPrefix(cmd, known) {
			return known
		}
		if d := levenshtein(cmd, known); d < bestDist {
			best, bestDist = known, d
		}
	}
	// Allow roughly one edit per three characters of the typed command.
	if bestDist <= 1+len(cmd)/3 {
		return best
	}
	return ""
}

// levenshtein is the standard edit distance between a and b (stdlib only).
func levenshtein(a, b string) int {
	ra, rb := []rune(a), []rune(b)
	prev := make([]int, len(rb)+1)
	curr := make([]int, len(rb)+1)
	for j := range prev {
		prev[j] = j
	}
	for i := 1; i <= len(ra); i++ {
		curr[0] = i
		for j := 1; j <= len(rb); j++ {
			cost := 1
			if ra[i-1] == rb[j-1] {
				cost = 0
			}
			curr[j] = min(prev[j]+1, curr[j-1]+1, prev[j-1]+cost)
		}
		prev, curr = curr, prev
	}
	return prev[len(rb)]
}

func (app *App) imageCommand(arg string) {
	arg = strings.TrimSpace(arg)
	if arg == "" {
		fmt.Fprintln(app.Errw, app.pendingImagesSummary())
		return
	}
	if arg == "--clear" {
		app.PendingImages = nil
		fmt.Fprintln(app.Errw, "[images cleared]")
		return
	}
	att, err := parseImageCommandArg(arg, app.ImageDetail)
	if err != nil {
		fmt.Fprintf(app.Errw, "[image failed: %v]\n", err)
		return
	}
	if !app.currentModelSupportsImages() {
		fmt.Fprintln(app.Errw, app.imageUnsupportedNotice())
		return
	}
	loaded, err := inputimage.Load(att)
	if err != nil {
		fmt.Fprintf(app.Errw, "[image failed: %v]\n", err)
		return
	}
	next := append(append([]inputimage.Loaded(nil), app.PendingImages...), loaded)
	if err := inputimage.ValidateTotal(next); err != nil {
		fmt.Fprintf(app.Errw, "[image failed: %v]\n", err)
		return
	}
	app.PendingImages = next
	fmt.Fprintf(app.Errw, "[image attached: %s %s %d bytes detail=%s]\n", loaded.Info.Name, loaded.Info.MediaType, loaded.Info.Bytes, loaded.Info.Detail)
}

func parseImageCommandArg(arg, defaultDetail string) (inputimage.Attachment, error) {
	if strings.HasPrefix(arg, "--detail=") {
		detail, path, _ := strings.Cut(strings.TrimPrefix(arg, "--detail="), " ")
		return inputimage.ParseSpec(strings.TrimSpace(path), detail)
	}
	if strings.HasPrefix(arg, "--detail ") {
		rest := strings.TrimSpace(strings.TrimPrefix(arg, "--detail "))
		detail, path, ok := strings.Cut(rest, " ")
		if !ok {
			return inputimage.Attachment{}, fmt.Errorf("--detail requires a value and path")
		}
		return inputimage.ParseSpec(strings.TrimSpace(path), detail)
	}
	if strings.HasPrefix(arg, "--") {
		return inputimage.Attachment{}, fmt.Errorf("unknown /image option")
	}
	return inputimage.ParseSpec(arg, defaultDetail)
}

func (app *App) pendingImagesSummary() string {
	if len(app.PendingImages) == 0 {
		return "[images: none]"
	}
	var b strings.Builder
	fmt.Fprintf(&b, "[images: %d queued]", len(app.PendingImages))
	for i, img := range app.PendingImages {
		fmt.Fprintf(&b, "\n  %d. %s %s %d bytes detail=%s", i+1, img.Info.Name, img.Info.MediaType, img.Info.Bytes, img.Info.Detail)
	}
	return b.String()
}

func (app *App) attachPromptImageReferences(prompt string, images []inputimage.Loaded, unsupportedNoticePrinted bool) []inputimage.Loaded {
	refs := promptFileReferences(prompt)
	if len(refs) == 0 {
		return images
	}
	seen := map[string]bool{}
	var candidates []struct {
		ref  string
		path string
	}
	for _, ref := range refs {
		if !inputimage.HasSupportedExtension(ref) {
			continue
		}
		path := resolvePromptRefLoadPath(ref)
		key := promptRefLoadKey(path)
		if seen[key] {
			continue
		}
		seen[key] = true
		candidates = append(candidates, struct {
			ref  string
			path string
		}{ref: ref, path: path})
	}
	if len(candidates) == 0 {
		return images
	}
	if !app.currentModelSupportsImages() {
		if !unsupportedNoticePrinted {
			fmt.Fprintln(app.Errw, app.imageUnsupportedNotice())
		}
		return images
	}
	for _, candidate := range candidates {
		loaded, err := inputimage.Load(inputimage.Attachment{Path: candidate.path, Detail: app.ImageDetail})
		if err != nil {
			fmt.Fprintf(app.Errw, "[image failed: %s: %v]\n", candidate.ref, err)
			continue
		}
		next := append(append([]inputimage.Loaded(nil), images...), loaded)
		if err := inputimage.ValidateTotal(next); err != nil {
			fmt.Fprintf(app.Errw, "[image failed: %s: %v]\n", candidate.ref, err)
			continue
		}
		images = next
		fmt.Fprintf(app.Errw, "[image attached: %s %s %d bytes detail=%s]\n", loaded.Info.Name, loaded.Info.MediaType, loaded.Info.Bytes, loaded.Info.Detail)
	}
	return images
}

func resolvePromptRefLoadPath(ref string) string {
	if strings.HasPrefix(ref, "~/") {
		home, err := os.UserHomeDir()
		if err == nil && home != "" {
			return filepath.Join(home, strings.TrimPrefix(ref, "~/"))
		}
	}
	return ref
}

func promptRefLoadKey(path string) string {
	abs, err := filepath.Abs(path)
	if err == nil {
		return abs
	}
	return filepath.Clean(path)
}

func (app *App) contextDump(path string) {
	data, err := json.MarshalIndent(app.contextRequest(), "", "  ")
	if err != nil {
		fmt.Fprintf(app.Errw, "[context failed: %v]\n", err)
		return
	}
	data = append(data, '\n')
	if path == "" {
		_, _ = app.Errw.Write(data)
		return
	}
	if err := writeContextFile(path, data); err != nil {
		fmt.Fprintf(app.Errw, "[context save failed: %v]\n", err)
		return
	}
	fmt.Fprintf(app.Errw, "[context saved %s]\n", path)
}

func writeContextFile(path string, data []byte) error {
	dir := filepath.Dir(path)
	if dir != "." && dir != "" {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
	}
	return os.WriteFile(path, data, 0o600)
}

func (app *App) contextRequest() llm.Request {
	out := app.promptHookContext(nil)
	// Render the full todo list, not the changed-only reminder: /context answers
	// "what context does the model have" for the user, and the unchanged list is
	// still model context via the transcript's update_todos result. Going through
	// todoRequestContext here would show nothing after the first real injection.
	if ctx := app.todoContextDisplay(); ctx != "" {
		out = append(out, ctx)
	}
	return app.Agent.ContextRequestWithContext(out)
}

func (app *App) pickModel(readLine func(string) (string, error)) {
	if app.PickModel == nil {
		fmt.Fprintln(app.Errw, app.modelSummary())
		return
	}
	fmt.Fprintf(app.Errw, "current model: %s\n", modelDisplayName(app.Provider, app.Model))
	model, err := app.PickModel(PickerIO{
		ReadLine: readLine,
		Writer:   app.Errw,
		PageSize: app.PickerPageSize,
	})
	if err != nil {
		if errors.Is(err, ErrPickerCancelled) {
			fmt.Fprintln(app.Errw, "[model selection cancelled]")
			return
		}
		fmt.Fprintf(app.Errw, "[model selection failed: %v]\n", err)
		return
	}
	reasoning, err := app.promptReasoningProfile(model, app.Reasoning, readLine)
	if err != nil {
		if errors.Is(err, ErrPickerCancelled) {
			fmt.Fprintln(app.Errw, "[model selection cancelled]")
			return
		}
		fmt.Fprintf(app.Errw, "[model selection failed: %v]\n", err)
		return
	}
	app.switchModelAndPromptDefault(model, reasoning, readLine)
}

// modelSummary renders the current model plus the configured models available
// for quick switching.
func (app *App) modelSummary() string {
	models := append([]string(nil), app.AvailableModels...)
	if app.Registry != nil {
		models = append(models, app.Registry.Models()...)
	}
	models = uniqueModels(models, app.Model)

	var b strings.Builder
	fmt.Fprintf(&b, "current model: %s  proxy-url: %s\n", modelDisplayName(app.Provider, app.Model), app.BaseURL)
	b.WriteString("available models:")
	if len(models) == 0 {
		b.WriteString(" none configured")
		return b.String()
	}
	for _, model := range models {
		if model == app.Model {
			fmt.Fprintf(&b, "\n  %s (current)", model)
		} else {
			fmt.Fprintf(&b, "\n  %s", model)
		}
	}
	return b.String()
}

func uniqueModels(models []string, current string) []string {
	seen := make(map[string]bool, len(models)+1)
	var out []string
	for _, model := range models {
		if model == "" || seen[model] {
			continue
		}
		seen[model] = true
		out = append(out, model)
	}
	if current != "" && !seen[current] {
		out = append(out, current)
	}
	sort.Strings(out)
	return out
}

func (app *App) switchModel(model string, reasoning llm.ReasoningConfig) bool {
	if app.SwitchModel == nil {
		fmt.Fprintln(app.Errw, "[model switch unavailable]")
		return false
	}
	oldProvider, oldModel, oldBaseTargetID := app.Provider, app.Model, app.BaseTargetID
	selection, err := app.SwitchModel(model, reasoning)
	if err != nil {
		fmt.Fprintf(app.Errw, "[model switch failed: %v]\n", err)
		return false
	}
	if selection.Runtime == nil {
		fmt.Fprintln(app.Errw, "[model switch failed: no provider was created]")
		return false
	}
	if selection.Model == "" {
		selection.Model = model
	}
	if selection.Provider == "" {
		selection.Provider = app.Provider
	}
	if !selection.ReasoningSet && selection.Reasoning.Empty() && !reasoning.Empty() {
		selection.Reasoning = reasoning
	}
	if selection.BaseTargetID == "" {
		selection.BaseTargetID = selection.Model
	}
	baseChanged := oldBaseTargetID == "" || selection.BaseTargetID != oldBaseTargetID
	responseState := app.Agent.ResponseState()
	app.Agent.SetProvider(selection.Runtime)
	app.Agent.SetModel(selection.Model, selection.ContextWindow)
	app.Agent.SetReasoning(selection.Reasoning)
	app.Agent.SetServerTools(selection.ServerTools)
	app.Agent.SetResponsesStateful(selection.ResponsesStateful)
	if baseChanged {
		app.Agent.ResetProxySessionID()
	} else if selection.ResponsesStateful &&
		responseState != nil &&
		responseState.AnchorMessages >= 0 &&
		responseState.AnchorMessages <= len(app.Agent.Transcript()) &&
		llm.MatchesMessageFingerprint(
			app.Agent.Transcript()[:responseState.AnchorMessages],
			responseState.AnchorDigest,
		) {
		app.Agent.SetResponseState(responseState)
	}
	if selection.RegistryModel == "" {
		selection.RegistryModel = selection.Model
	}
	app.Renderer.SetModel(selection.RegistryModel)
	app.Provider = selection.Provider
	app.Model = selection.Model
	app.RegistryModel = selection.RegistryModel
	if app.Hooks != nil {
		app.Hooks.SetModel(app.Model)
	}
	app.BaseURL = selection.BaseURL
	app.Reasoning = selection.Reasoning
	app.BaseTargetID = selection.BaseTargetID
	app.Variant = selection.Variant
	app.FastTargetID = selection.FastTargetID
	fmt.Fprintf(app.Errw, "[model switched: model=%s proxy-url=%s reasoning=%s]\n", modelDisplayName(app.Provider, app.Model), app.BaseURL, app.reasoningLabel())
	if oldProvider != app.Provider || oldModel != app.Model {
		app.onModelChanged()
	}
	if baseChanged {
		app.schedulePrewarm() // the new underlying model/provider invalidated the warm cache prefix (r43)
	}
	return true
}

// prewarm triggers a background prompt-cache warm-up after a cache-invalidating
// event, when one is wired (r43). Mid-prompt requests coalesce into a single
// warm-up fired at prompt completion: a running prompt refreshes the cache
// prefix every turn, so warming during it is pure waste.
func (app *App) prewarm() {
	if app.Prewarm == nil {
		return
	}
	if app.promptActive {
		app.pendingPrewarm = true
		return
	}
	app.Prewarm()
}

// releasePendingPrewarm fires a warm-up deferred while a prompt was active,
// once, at prompt completion. An app exit with a pending warm-up drops it.
func (app *App) releasePendingPrewarm() {
	if !app.pendingPrewarm {
		return
	}
	app.pendingPrewarm = false
	app.prewarm()
}

// schedulePrewarm routes a switch-driven warm-up through the REPL's debounce
// when installed, so rapid agent/model cycling or startup resolution warms
// only the settled selection; otherwise it prewarms immediately.
func (app *App) schedulePrewarm() {
	if app.SchedulePrewarm != nil {
		app.SchedulePrewarm()
		return
	}
	app.prewarm()
}

func (app *App) switchModelAndPromptDefault(model string, reasoning llm.ReasoningConfig, readLine func(string) (string, error)) {
	if !app.switchModel(model, reasoning) {
		return
	}
	app.promptSaveDefaultModel(readLine)
}

func (app *App) promptSaveDefaultModel(readLine func(string) (string, error)) {
	if app.SaveDefaultModel == nil || !app.PromptDefaultModelSave {
		return
	}
	save, err := PromptSaveDefaultModel(readLine, app.Errw, app.Provider, app.Model)
	if err != nil {
		if errors.Is(err, ErrPickerCancelled) {
			fmt.Fprintln(app.Errw, "[default model save cancelled]")
			return
		}
		fmt.Fprintf(app.Errw, "[default model save failed: %v]\n", err)
		return
	}
	if !save {
		return
	}
	if err := app.SaveDefaultModel(app.Provider, app.Model, app.Reasoning); err != nil {
		fmt.Fprintf(app.Errw, "[default model save failed: %v]\n", err)
		return
	}
	fmt.Fprintln(app.Errw, "[default model saved]")
}

func (app *App) effort(arg string) {
	app.reasoningCommand(arg)
}

func (app *App) viCommand(arg string) {
	switch strings.ToLower(arg) {
	case "", "status":
		mode := app.PromptEditMode
		if mode == "" {
			mode = "emacs"
		}
		fmt.Fprintf(app.Errw, "[vi mode: %s]\n", mode)
	case "on", "vi", "vim":
		app.setEditMode("vi")
	case "off", "emacs":
		app.setEditMode("emacs")
	default:
		fmt.Fprintf(app.Errw, "[vi failed: unknown option %q; use on, off, or status]\n", arg)
	}
}

func (app *App) setEditMode(mode string) {
	app.PromptEditMode = mode
	if app.SetPromptEditMode != nil {
		app.SetPromptEditMode(mode)
	}
	if app.SaveReplEditMode != nil {
		if err := app.SaveReplEditMode(mode); err != nil {
			fmt.Fprintf(app.Errw, "[default edit mode save failed: %v]\n", err)
		}
	}
	label := mode
	if label == "" {
		label = "emacs"
	}
	fmt.Fprintf(app.Errw, "[edit mode: %s]\n", label)
}

func (app *App) reasoningCommand(arg string) {
	fields := strings.Fields(arg)
	if len(fields) == 0 {
		fmt.Fprintln(app.Errw, app.reasoningSummary())
		return
	}
	fail := func(format string, args ...any) {
		fmt.Fprintf(app.Errw, "[reasoning failed: "+format+"]\n", args...)
	}
	set := func(reasoning llm.ReasoningConfig) {
		if err := app.validateReasoningForModel(app.currentRegistryModel(), reasoning); err != nil {
			fail("%v", err)
			return
		}
		if err := app.setReasoning(reasoning); err != nil {
			fail("%v", err)
			return
		}
		fmt.Fprintf(app.Errw, "[reasoning: %s]\n", app.reasoningLabel())
	}
	cmd := strings.ToLower(fields[0])
	switch cmd {
	case "summary":
		if len(fields) != 2 {
			fail("summary requires auto, concise, detailed, or none")
			return
		}
		summary, ok := normalizeReasoningSummaryInput(fields[1])
		if !ok {
			fail("invalid summary %q", fields[1])
			return
		}
		reasoning := app.Reasoning
		reasoning.Summary = summary
		set(reasoning)
	default:
		if len(fields) != 1 {
			fail("reasoning profile takes one value")
			return
		}
		profile, ok := reasoningprofile.Normalize(fields[0])
		if !ok {
			fail("invalid profile %q for model %q (supported: %s)", fields[0], app.currentRegistryModel(), reasoningprofile.ChoicesLabel())
			return
		}
		reasoning := app.Reasoning
		reasoning.Profile = profile
		set(reasoning)
	}
}

func (app *App) setReasoning(reasoning llm.ReasoningConfig) error {
	if app.SetReasoning != nil {
		if err := app.SetReasoning(app.currentRegistryModel(), reasoning); err != nil {
			return err
		}
	}
	app.Reasoning = reasoning
	if app.Agent != nil {
		app.Agent.SetReasoning(reasoning)
		app.Agent.ResetProxySessionID()
	}
	return nil
}

func (app *App) fastCommand(arg string) {
	action := strings.ToLower(strings.TrimSpace(arg))
	if action == "" {
		if strings.EqualFold(app.Variant, "fast") {
			action = "off"
		} else {
			action = "on"
		}
	}
	switch action {
	case "status":
		if app.FastTargetID == "" {
			fmt.Fprintln(app.Errw, "[fast mode unavailable for current model]")
			return
		}
		if strings.EqualFold(app.Variant, "fast") {
			fmt.Fprintln(app.Errw, "[fast mode: on]")
		} else {
			fmt.Fprintln(app.Errw, "[fast mode: off]")
		}
	case "on":
		if app.FastTargetID == "" {
			fmt.Fprintln(app.Errw, "[fast mode unavailable for current model]")
			return
		}
		if strings.EqualFold(app.Variant, "fast") {
			fmt.Fprintln(app.Errw, "[fast mode: on]")
			return
		}
		if app.switchModel(app.FastTargetID, app.Reasoning) {
			fmt.Fprintln(app.Errw, "[fast mode: on]")
		}
	case "off":
		if !strings.EqualFold(app.Variant, "fast") {
			fmt.Fprintln(app.Errw, "[fast mode: off]")
			return
		}
		if app.switchModel(app.BaseTargetID, app.Reasoning) {
			fmt.Fprintln(app.Errw, "[fast mode: off]")
		}
	default:
		fmt.Fprintln(app.Errw, "[fast mode failed: expected on, off, or status]")
	}
}

func (app *App) reasoningSummary() string {
	model := app.currentRegistryModel()
	var b strings.Builder
	fmt.Fprintf(&b, "current reasoning: %s\n", app.reasoningLabel())
	info, ok := app.reasoningInfoForModel(model)
	if !ok {
		fmt.Fprintf(&b, "available controls for %s: unknown", model)
		return b.String()
	}
	if !info.Supported {
		fmt.Fprintf(&b, "available controls for %s: none (model does not support reasoning)", model)
		return b.String()
	}
	fmt.Fprintf(&b, "available controls for %s:", model)
	fmt.Fprintf(&b, "\n  profile: %s", strings.Join(reasoningprofile.Choices(), ", "))
	b.WriteString("\n  summary: auto, concise, detailed, none")
	return b.String()
}

func (app *App) promptReasoningProfile(model string, reasoning llm.ReasoningConfig, readLine func(string) (string, error)) (llm.ReasoningConfig, error) {
	info, ok := app.reasoningInfoForModel(model)
	if !ok || !info.Supported {
		return reasoning, nil
	}
	current := strings.TrimSpace(reasoning.Profile)
	if normalized, ok := reasoningprofile.Normalize(current); ok {
		current = normalized
	}
	_, currentValid := reasoningprofile.Normalize(reasoning.Profile)
	for {
		prompt := fmt.Sprintf("Reasoning profile (%s; current: %s): ", reasoningprofile.ChoicesLabel(), reasoningProfilePromptCurrent(current, currentValid))
		input, err := readLine(prompt)
		if err != nil {
			return reasoning, err
		}
		input = strings.TrimSpace(input)
		if input == "" {
			if currentValid {
				return reasoning, nil
			}
			reasoning.Profile = ""
			return reasoning, nil
		}
		if strings.EqualFold(input, "q") {
			return reasoning, ErrPickerCancelled
		}
		profile, ok := reasoningprofile.Normalize(input)
		if !ok {
			fmt.Fprintf(app.Errw, "Invalid reasoning profile %q (supported: %s)\n", input, reasoningprofile.ChoicesLabel())
			continue
		}
		reasoning.Profile = profile
		return reasoning, nil
	}
}

func reasoningProfilePromptCurrent(current string, valid bool) string {
	if current == "" {
		return "provider default"
	}
	if valid {
		return current
	}
	return current + " (not valid for this model; Enter uses provider default)"
}

func PromptSaveDefaultModel(readLine func(string) (string, error), w io.Writer, provider, model string) (bool, error) {
	for {
		input, err := readLine(fmt.Sprintf("Save %s as the default model? (y/N): ", modelDisplayName(provider, model)))
		if err != nil {
			return false, err
		}
		switch strings.ToLower(strings.TrimSpace(input)) {
		case "", "n", "no":
			return false, nil
		case "y", "yes":
			return true, nil
		case "q":
			return false, ErrPickerCancelled
		default:
			fmt.Fprintln(w, `Please answer "yes" or "no".`)
		}
	}
}

func modelDisplayName(provider, model string) string {
	if provider == "" || provider == model {
		return model
	}
	return provider + ":" + model
}

func normalizeReasoningSummaryInput(input string) (string, bool) {
	switch strings.ToLower(strings.TrimSpace(input)) {
	case "", "default", "provider-default", "none", "off", "false", "disabled", "disable":
		return "", true
	case "auto", "concise", "detailed":
		return strings.ToLower(strings.TrimSpace(input)), true
	case "on", "true", "enabled", "enable":
		return "auto", true
	default:
		return "", false
	}
}

func reasoningSummaryDisplayEnabled(summary string) bool {
	switch strings.ToLower(strings.TrimSpace(summary)) {
	case "auto", "concise", "detailed", "on", "true", "enabled", "enable":
		return true
	default:
		return false
	}
}

func (app *App) validateReasoningForModel(model string, reasoning llm.ReasoningConfig) error {
	reasoning.Profile = strings.ToLower(strings.TrimSpace(reasoning.Profile))
	reasoning.Summary = strings.ToLower(strings.TrimSpace(reasoning.Summary))
	if reasoning.Empty() {
		return nil
	}
	if profile, ok := reasoningprofile.Normalize(reasoning.Profile); ok {
		reasoning.Profile = profile
	} else {
		return fmt.Errorf("invalid reasoning profile %q (want %s)", reasoning.Profile, reasoningprofile.ChoicesLabel())
	}
	info, ok := app.reasoningInfoForModel(model)
	if !ok {
		return nil
	}
	if !info.Supported {
		return fmt.Errorf("model %q does not support reasoning controls", model)
	}
	return nil
}

func (app *App) reasoningInfoForModel(model string) (*llm.ReasoningInfo, bool) {
	if app.Registry == nil {
		return nil, false
	}
	for _, key := range app.reasoningLookupKeys(model) {
		info, ok := app.Registry.Lookup(key)
		if ok && info.Reasoning != nil {
			return info.Reasoning, true
		}
	}
	return nil, false
}

func (app *App) reasoningLookupKeys(model string) []string {
	var keys []string
	add := func(key string) {
		key = strings.TrimSpace(key)
		if key == "" {
			return
		}
		for _, existing := range keys {
			if existing == key {
				return
			}
		}
		keys = append(keys, key)
	}
	add(model)
	add(app.currentRegistryModel())
	if app.Provider != "" {
		add(app.Provider + ":" + model)
		add(app.Provider + ":" + app.Model)
	}
	return keys
}

func (app *App) currentRegistryModel() string {
	if app.RegistryModel != "" {
		return app.RegistryModel
	}
	if app.Provider != "" && app.Model != "" {
		if app.Registry != nil {
			if _, ok := app.Registry.Lookup(app.Provider + ":" + app.Model); ok {
				return app.Provider + ":" + app.Model
			}
		}
	}
	if app.Model != "" {
		return app.Model
	}
	return "unknown"
}

func (app *App) promptReasoningLabel() string {
	return reasoningprofile.Label(app.Reasoning.Profile)
}

func (app *App) reasoningLabel() string {
	if app.Reasoning.Empty() {
		return "provider default"
	}
	var parts []string
	if profile := strings.TrimSpace(app.Reasoning.Profile); profile != "" {
		parts = append(parts, "profile="+profile)
	}
	if summary := strings.TrimSpace(app.Reasoning.Summary); summary != "" {
		parts = append(parts, "summary="+summary)
	}
	return strings.Join(parts, ",")
}

// agentSummary renders the current agent plus available agents and descriptions,
// marking the current one.
func (app *App) agentSummary() string {
	if app.RefreshAgentSummaries != nil {
		app.AvailableAgents = app.RefreshAgentSummaries()
	}
	var b strings.Builder
	fmt.Fprintf(&b, "current agent: %s [%s]\n", app.AgentName, app.currentAgentModelSummary())
	b.WriteString("available agents:")
	if len(app.AvailableAgents) == 0 {
		b.WriteString(" none configured")
		return b.String()
	}
	labels := make([]string, len(app.AvailableAgents))
	for i, a := range app.AvailableAgents {
		label := a.Name
		if a.Name == app.AgentName {
			label += " (current)"
		}
		labels[i] = label
	}
	rows := make([]NameDescription, 0, len(app.AvailableAgents))
	for i, a := range app.AvailableAgents {
		modelInfo := app.agentModelSummary(a)
		parts := []string{"[" + modelInfo + "]"}
		if a.Delegatable {
			parts = append(parts, "[delegatable]")
		}
		if strings.TrimSpace(a.Description) != "" {
			parts = append(parts, a.Description)
		}
		rows = append(rows, NameDescription{
			Name:        labels[i],
			Description: strings.Join(parts, " "),
		})
	}
	b.WriteByte('\n')
	WriteNameDescriptionList(&b, rows, NameDescriptionListOptions{Indent: "  ", Width: app.summaryWidth()})
	return strings.TrimSuffix(b.String(), "\n")
}

func (app *App) currentAgentModelSummary() string {
	if app.Provider != "" || app.Model != "" {
		return modelDisplayName(app.Provider, app.Model)
	}
	return "unknown"
}

func (app *App) agentModelSummary(a AgentSummary) string {
	model := strings.TrimSpace(a.Model)
	if model == "" {
		return "inherit current"
	}
	return model
}

func (app *App) switchAgent(name string) {
	if err := app.applyAgentSwitch(name); err != nil {
		fmt.Fprintf(app.Errw, "[agent switch failed: %v]\n", err)
	}
}

// nextAgentName returns the configured agent after the current one, wrapping to
// the first entry. AvailableAgents is already in canonical lexical order. If the
// current name is absent, the first entry is the recovery target.
func (app *App) nextAgentName() (string, bool) {
	if len(app.AvailableAgents) == 0 || app.SwitchAgent == nil {
		return "", false
	}
	next := 0
	for i, summary := range app.AvailableAgents {
		if summary.Name == app.AgentName {
			next = (i + 1) % len(app.AvailableAgents)
			break
		}
	}
	name := app.AvailableAgents[next].Name
	if name == app.AgentName {
		return "", false
	}
	return name, true
}

// cycleAgent applies the next configured agent through the same full switch path
// as /agent, but leaves prewarming to the idle-loop debounce.
func (app *App) cycleAgent() bool {
	name, ok := app.nextAgentName()
	if !ok {
		return false
	}
	if err := app.applyAgentSwitchWithPrewarm(name, false); err != nil {
		fmt.Fprintf(app.Errw, "[agent switch failed: %v]\n", err)
		return false
	}
	return true
}

// applyAgentSwitch performs the agent switch and reports an error instead of
// printing it, so callers that must abort on failure (the handoff) can. The
// /agent command wraps it and prints failures. Callers route the warm-up
// through the REPL debounce (schedulePrewarm); Shift-Tab uses
// applyAgentSwitchWithPrewarm to defer only that warm-up to the idle loop.
func (app *App) applyAgentSwitch(name string) error {
	return app.applyAgentSwitchWithPrewarm(name, true)
}

func (app *App) applyAgentSwitchWithPrewarm(name string, prewarm bool) error {
	if app.SwitchAgent == nil {
		return fmt.Errorf("agent switch unavailable")
	}
	oldProvider, oldModel := app.Provider, app.Model
	selection, err := app.SwitchAgent(name)
	if err != nil {
		return err
	}
	app.Agent.SetTools(selection.Tools)
	app.Agent.SetSystem(selection.System)
	if selection.Runtime != nil {
		app.Agent.SetProvider(selection.Runtime)
	}
	if selection.Model != "" {
		app.Agent.SetModel(selection.Model, selection.ContextWindow)
	}
	if selection.ReasoningSet {
		app.Reasoning = selection.Reasoning
		app.Agent.SetReasoning(selection.Reasoning)
	}
	if selection.BaseTargetID == "" {
		selection.BaseTargetID = selection.Model
	}
	app.BaseTargetID = selection.BaseTargetID
	app.Variant = selection.Variant
	app.FastTargetID = selection.FastTargetID
	app.Agent.SetServerTools(selection.ServerTools)
	app.Agent.SetResponsesStateful(selection.ResponsesStateful)
	app.Agent.ResetProxySessionID()
	app.AgentName = selection.Name
	app.System = selection.System // so saved sessions capture the agent's prompt
	if selection.Provider != "" {
		app.Provider = selection.Provider
	}
	if selection.Model != "" {
		app.Model = selection.Model
	}
	if app.Hooks != nil {
		app.Hooks.SetModel(app.Model)
	}
	if selection.RegistryModel == "" {
		selection.RegistryModel = app.Model
	}
	app.RegistryModel = selection.RegistryModel
	if app.Renderer != nil {
		app.Renderer.SetModel(selection.RegistryModel)
	}
	if selection.BaseURL != "" {
		app.BaseURL = selection.BaseURL
	}
	fmt.Fprintf(app.Errw, "[agent switched: %s]\n", selection.Name)
	fmt.Fprintln(app.Errw, ProviderLine(app.Provider, app.Model, app.currentRegistryModel(), app.Reasoning, app.Registry))
	if oldProvider != app.Provider || oldModel != app.Model {
		app.onModelChanged()
		fmt.Fprintln(app.Errw, "[warning: model target changed; the new model may start without prompt cache, increasing token usage or cost]")
	}
	// The agent's tools/system (and possibly model/provider) changed, so re-warm
	// the cache prefix in the background (r43) — debounced so rapid cycling
	// warms only the settled selection — unless the idle Shift-Tab path is
	// deferring this switch's warm-up.
	if prewarm {
		app.schedulePrewarm()
	}
	return nil
}

func (app *App) hasPendingHandoffRequest() bool {
	if app.Handoff == nil {
		return false
	}
	_, ok := app.Handoff.Peek()
	return ok
}

const handoffCommandUsage = "/handoff [-a agent] [-m model] [message]"

type handoffCommandOptions struct {
	Agent   string
	Model   string
	Message string
}

// parseHandoffCommandOptions parses leading -a/-m options and preserves the
// remainder as one user message. Options intentionally stop at the first message
// word; -- allows a message to start with a dash.
func parseHandoffCommandOptions(arg string) (handoffCommandOptions, error) {
	var opts handoffCommandOptions
	rest := strings.TrimSpace(arg)
	for rest != "" {
		token, next := splitHandoffCommandToken(rest)
		switch token {
		case "--":
			opts.Message = strings.TrimSpace(next)
			return opts, nil
		case "-a", "-m":
			value, remaining := splitHandoffCommandToken(next)
			if value == "" || strings.HasPrefix(value, "-") {
				return handoffCommandOptions{}, fmt.Errorf("%s requires a value", token)
			}
			if token == "-a" {
				opts.Agent = value
			} else {
				opts.Model = value
			}
			rest = remaining
		default:
			if strings.HasPrefix(token, "-") {
				return handoffCommandOptions{}, fmt.Errorf("unknown option %q", token)
			}
			opts.Message = strings.TrimSpace(rest)
			return opts, nil
		}
	}
	return opts, nil
}

func splitHandoffCommandToken(s string) (token, rest string) {
	s = strings.TrimLeft(s, " \t")
	if i := strings.IndexAny(s, " \t"); i >= 0 {
		return s[:i], strings.TrimLeft(s[i+1:], " \t")
	}
	return s, ""
}

// handoffCommand handles /handoff [-a agent] [-m model] [message]: hand off to
// an implementation agent to carry out the most recently recorded plan, after
// interactive approval. It consumes any request the request_implementation tool
// recorded, applies manual overrides and guidance, fills in the brief, and
// switches with a clean, plan-seeded context.
func (app *App) handoffCommand(arg string, readLine func(string) (string, error)) bool {
	if app.SwitchAgent == nil {
		fmt.Fprintln(app.Errw, "[handoff unavailable]")
		return false
	}
	opts, err := parseHandoffCommandOptions(arg)
	if err != nil {
		fmt.Fprintf(app.Errw, "[handoff: %v; usage: %s]\n", err, handoffCommandUsage)
		return false
	}
	var req plan.HandoffRequest
	if app.Handoff != nil {
		if pending, ok := app.Handoff.Take(); ok {
			req = pending
		}
	}
	if opts.Agent != "" {
		req.Agent = opts.Agent
	}
	if opts.Model != "" {
		req.Model = opts.Model
	}
	if opts.Message != "" {
		req.Message = opts.Message
	}
	if req.PlanPath == "" && app.Plans != nil {
		if latest, ok := app.Plans.Latest(); ok {
			req.PlanPath = latest.Path
		}
	}
	if req.PlanPath == "" {
		fmt.Fprintln(app.Errw, "[handoff: no recorded plan; record one with record_plan first]")
		return false
	}
	req.Brief = strings.TrimSpace(req.Brief)
	if req.Brief == "" {
		if app.Renderer != nil {
			app.Renderer.HandoffSummaryStart()
		}
		brief, usage, err := app.Agent.GenerateSummary(context.Background(), prompts.HandoffSummary())
		if app.Renderer != nil {
			app.Renderer.HandoffSummaryComplete()
		}
		if usage != (llm.Usage{}) {
			app.addMaintenanceUsage("handoff_summary", usage)
		}
		if err != nil {
			fmt.Fprintf(app.Errw, "[handoff: could not generate brief: %v]\n", err)
			return false
		}
		req.Brief = strings.TrimSpace(brief)
	}
	displayBrief := req.Brief
	if app.Renderer != nil {
		displayBrief = app.Renderer.FormatMarkdown(displayBrief)
	}
	fmt.Fprintf(app.Errw, "Handoff brief:\n%s\n", displayBrief)

	target := req.Agent
	if target == "" {
		target = app.HandoffAgent
	}
	if target == "" {
		target = "auto"
	}
	req.Agent = target

	approval := fmt.Sprintf("Hand off to %q", target)
	if req.Model != "" {
		approval += fmt.Sprintf(" using model %q", req.Model)
	}
	input, err := readLine(fmt.Sprintf("%s to implement %s? (y/N): ", approval, req.PlanPath))
	if err != nil {
		fmt.Fprintf(app.Errw, "[handoff cancelled: %v]\n", err)
		return false
	}
	switch strings.ToLower(strings.TrimSpace(input)) {
	case "y", "yes":
		return app.handoffToImplementation(req)
	default:
		fmt.Fprintln(app.Errw, "[handoff cancelled]")
		return false
	}
}

// handoffToImplementation switches the session to the implementation agent with
// a clean context: it switches agent (and model when requested), archives the
// planning transcript (recoverable), then reseeds the transcript with a pointer
// to the recorded plan plus the brief. The implementation agent reads the plan
// as its task spec. The switch is attempted before any destructive step so a
// failed switch leaves the session — and the recorded plan — untouched.
func (app *App) handoffToImplementation(req plan.HandoffRequest) bool {
	if err := app.applyAgentSwitch(req.Agent); err != nil {
		fmt.Fprintf(app.Errw, "[handoff failed: %v]\n", err)
		return false
	}
	if req.Model != "" {
		if !app.switchModel(req.Model, app.Reasoning) {
			return false
		}
	}
	if app.SessionPath != "" {
		if _, err := session.SaveCompaction(app.SessionPath, session.Compaction{
			Time:     app.clock()(),
			Summary:  req.Brief,
			Messages: app.Agent.Transcript(),
		}); err != nil {
			fmt.Fprintf(app.Errw, "[handoff: archive failed: %v]\n", err)
			return false
		}
	}
	seed := fmt.Sprintf("=== Implementation handoff ===\nYour task is specified in the recorded plan — read it now:\n%s\n\nContext from planning (how it was produced and this environment):\n%s",
		req.PlanPath, req.Brief)
	if req.Message != "" {
		seed += "\n\nAdditional input from the user:\n" + req.Message
	}
	seedMessages := []llm.Message{{
		Role:    llm.RoleUser,
		Time:    app.clock()(),
		Content: []llm.ContentBlock{{Kind: llm.BlockText, Text: seed}},
	}}
	if err := app.ensureSessionTree(); err != nil {
		fmt.Fprintf(app.Errw, "[handoff: tree update failed: %v]\n", err)
		return false
	}
	if err := app.SessionTree.AppendContextReset(seedMessages, "handoff"); err != nil {
		fmt.Fprintf(app.Errw, "[handoff: tree update failed: %v]\n", err)
		return false
	}
	app.Agent.SetTranscript(seedMessages)
	app.Agent.SetResponseState(nil)
	if app.Todos != nil {
		app.Todos.Replace(nil) // the implementation agent builds its own task list
	}
	app.saveOrWarn(app.SessionPath)
	fmt.Fprintf(app.Errw, "[handed off to %s; implementing from a clean context seeded by %s]\n", req.Agent, req.PlanPath)
	return true
}

// refreshMCP applies any pending proxy tool-list change at the idle-prompt
// boundary, mirroring switchAgent's Agent.SetTools swap. It is a no-op when no
// hook is installed or the hook reports no change, so MCP-disabled runs (the
// default) and the one-shot path pay nothing.
func (app *App) refreshMCP(ctx context.Context) error {
	if app.RefreshMCP == nil {
		return nil
	}
	sel, notice := app.RefreshMCP(ctx, app.AgentName)
	if err := ctx.Err(); err != nil {
		return err
	}
	if sel == nil {
		return nil
	}
	app.Agent.SetTools(sel)
	app.Agent.ResetProxySessionID()
	if notice != "" {
		fmt.Fprintln(app.Errw, notice)
	}
	return nil
}

// clear resets the conversation and rotates to a fresh auto-save file (design
// §10, §11). Cumulative usage resets with the conversation.
func (app *App) clear() {
	if app.Background != nil {
		app.Background.Clear()
	}
	// Echo the totals being discarded so a /clear never silently wipes the
	// session's accumulated token/cost spend (r26).
	if app.usage.InputTokens != 0 || app.usage.OutputTokens != 0 || app.usage.CostUSD != 0 {
		fmt.Fprintln(app.Errw, app.usageReport("cleared session"))
	}
	app.Agent.SetTranscript(nil)
	app.Agent.ResetSessionIDs()
	if app.Todos != nil {
		app.Todos.Replace(nil)
	}
	if app.Plans != nil {
		app.Plans.Replace(nil)
	}
	app.SetUsage(session.UsageTotals{})
	app.usageByModel = nil
	app.Created = app.clock()()
	cwd, _ := os.Getwd()
	app.SessionTree = session.NewTree(app.Created, cwd, "", "")
	app.PromptNumber = 0
	app.todoPromptStatusBeforeUsage = false
	app.todoPromptStatusBeforeUsagePrompt = 0
	app.SessionPath = session.DefaultPath(app.StateDir, app.Created)
	if app.OnSessionPathChanged != nil {
		app.OnSessionPathChanged(app.SessionPath)
	}
	if app.Hooks != nil {
		app.Hooks.SetSession(app.SessionPath)
		app.RunSessionStartHook("clear")
	}
	fmt.Fprintf(app.Errw, "[cleared; new session %s]\n", app.SessionPath)
}

// runPrompt runs one prompt interaction, accumulates usage, and saves the
// session. An error does not end the REPL (the next prompt may recover).
type promptOptions struct {
	resolveSkillMentions bool
	attachPromptImages   bool
}

type preparedPrompt struct {
	prompt        string
	images        []inputimage.Loaded
	promptContext []string
}

func (app *App) runPrompt(prompt string) {
	if run, ok := app.preparePromptRun(prompt, promptOptions{resolveSkillMentions: true, attachPromptImages: true}); ok {
		run()
	}
}

func (app *App) preparePromptRun(prompt string, opts promptOptions) (func(), bool) {
	prepared, ok := app.preparePrompt(prompt, opts, true)
	if !ok {
		return nil, false
	}
	promptID := app.beginPrompt(prepared.prompt, prepared.images)
	ctx := context.Background()
	var cancel context.CancelFunc
	if app.Interrupt != nil {
		ctx, cancel = context.WithCancel(ctx)
		app.Interrupt.BeginPrompt(func() {
			if app.Renderer != nil {
				app.Renderer.CancelRequested()
			}
			cancel()
		})
	}

	app.Renderer.StartPromptRun()
	return func() {
		if app.OnPromptFinished != nil {
			defer app.OnPromptFinished()
		}
		if app.Interrupt != nil {
			defer func() {
				app.Interrupt.EndPrompt()
				cancel()
			}()
		}

		sink := newREPLSink(app.Renderer, app, promptID)
		err := app.Agent.RunPromptContentWithContext(ctx, prepared.prompt, imageBlocks(prepared.images), app.promptHookContext(prepared.promptContext), promptID, sink)
		if err != nil && !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) && !sink.terminalModelErrorDisplayed {
			fmt.Fprintf(app.Errw, "[error: %v]\n", err)
		}
		app.saveOrWarn(app.SessionPath)
	}, true
}

func (app *App) preparePrompt(prompt string, opts promptOptions, stopProgressOnBlock bool) (preparedPrompt, bool) {
	var skillContext []string
	if opts.resolveSkillMentions {
		var ok bool
		prompt, skillContext, ok = app.resolveSkillMentionContext(prompt)
		if !ok {
			if app.Renderer != nil {
				if stopProgressOnBlock {
					app.Renderer.StopProgress()
				}
			}
			return preparedPrompt{}, false
		}
	}
	promptHook := app.runPromptSubmitHook(context.Background(), prompt, app.PromptNumber+1)
	if promptHook.Block {
		reason := promptHook.Reason()
		if reason == "" {
			reason = "blocked by UserPromptSubmit hook"
		}
		if app.Renderer != nil {
			app.Renderer.Notice("[prompt blocked: " + reason + "]")
			if stopProgressOnBlock {
				app.Renderer.StopProgress()
			}
		} else {
			fmt.Fprintf(app.Errw, "[prompt blocked: %s]\n", reason)
		}
		return preparedPrompt{}, false
	}
	pendingUnsupportedNotice := len(app.PendingImages) > 0 && !app.currentModelSupportsImages()
	images := app.takePendingImages()
	if opts.attachPromptImages {
		images = app.attachPromptImageReferences(prompt, images, pendingUnsupportedNotice)
	}
	promptContext := append([]string(nil), promptHook.AdditionalContext...)
	promptContext = append(promptContext, skillContext...)
	return preparedPrompt{prompt: prompt, images: images, promptContext: promptContext}, true
}

func (app *App) prepareSteerInput(prompt string, opts promptOptions) (agent.SteerInput, bool) {
	prepared, ok := app.preparePrompt(prompt, opts, false)
	if !ok {
		return agent.SteerInput{}, false
	}
	return agent.SteerInput{
		Text:           prepared.prompt,
		Images:         imageBlocks(prepared.images),
		RequestContext: prepared.promptContext,
	}, true
}

func (app *App) prepareSteeredPrompt(input agent.SteerInput) (func(), bool) {
	if steerInputEmpty(input) {
		return nil, false
	}
	promptID := app.beginPrompt(input.Text, nil)
	ctx := context.Background()
	var cancel context.CancelFunc
	if app.Interrupt != nil {
		ctx, cancel = context.WithCancel(ctx)
		app.Interrupt.BeginPrompt(func() {
			if app.Renderer != nil {
				app.Renderer.CancelRequested()
			}
			cancel()
		})
	}

	app.Renderer.StartPromptRun()
	return func() {
		if app.OnPromptFinished != nil {
			defer app.OnPromptFinished()
		}
		if app.Interrupt != nil {
			defer func() {
				app.Interrupt.EndPrompt()
				cancel()
			}()
		}

		sink := newREPLSink(app.Renderer, app, promptID)
		err := app.Agent.RunPromptContentWithContext(ctx, input.Text, input.Images, app.promptHookContext(input.RequestContext), promptID, sink)
		if err != nil && !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) && !sink.terminalModelErrorDisplayed {
			fmt.Fprintf(app.Errw, "[error: %v]\n", err)
		}
		app.saveOrWarn(app.SessionPath)
	}, true
}

// compact forces compaction now (/compact, design §12). The summary call's usage
// is folded into the cumulative session totals so /usage stays accurate, and the
// session is saved with the collapsed transcript. A summary-call error is already
// warned about via the sink by Compact; the transcript is left intact.
func (app *App) compact(focus string) {
	ctx := context.Background()
	sink := newAccumulatingSink(app.Renderer, app, app.PromptNumber)
	u, err := app.Agent.CompactWithFocus(ctx, sink, focus)
	if u != (llm.Usage{}) {
		app.addMaintenanceUsage("compaction", u)
	}
	if err != nil {
		return
	}
	app.saveOrWarn(app.SessionPath)
	// Compaction rewrote the transcript prefix, invalidating the warm cache;
	// re-warm in the background (r43).
	app.prewarm()
}

// SetUsage seeds the cumulative session totals, used when resuming a session so
// /usage and saved totals continue from the prior run (design §11).
func (app *App) SetUsage(u session.UsageTotals) {
	app.usage = u
	if app.Renderer != nil {
		app.Renderer.SetCumulativeUsage(u.InputTokens, u.OutputTokens, u.CostUSD)
	}
}

// SetUsageByModel seeds the per-model usage buckets on resume. When byModel is
// empty but the aggregate is non-zero, it seeds one bucket under the current
// model so the aggregate remains visible after resume.
func (app *App) SetUsageByModel(byModel map[string]session.UsageTotals) {
	if len(byModel) == 0 {
		if app.usage.InputTokens != 0 || app.usage.OutputTokens != 0 || app.usage.CostUSD != 0 {
			app.usageByModel = map[string]session.UsageTotals{app.usageKey(): app.usage}
		}
		return
	}
	app.usageByModel = make(map[string]session.UsageTotals, len(byModel))
	maps.Copy(app.usageByModel, byModel)
	if app.Renderer != nil {
		b := app.usageByModel[app.usageKey()]
		app.Renderer.SetCumulativeUsage(b.InputTokens, b.OutputTokens, app.usage.CostUSD)
	}
}

// usageKey is the target key for the active model's usage bucket.
func (app *App) usageKey() string {
	model := app.RegistryModel
	if model == "" {
		model = app.Model
	}
	return model
}

// addUsage folds one prompt's usage into the session aggregate and the active
// model's bucket, then refreshes the live cumulative readout to show the active
// model's tokens with the session-total cost.
// promptCost prices usage against the App's active model, used both for
// cumulative accounting and to feed the renderer's per-prompt line so a
// mid-prompt model switch is not mispriced against a stale model (r63).
func (app *App) promptCost(u llm.Usage) (float64, bool) {
	return u.CostUSD, u.CostKnown
}

func (app *App) addUsage(u agent.PromptUsage) {
	app.addUsageForModel(u, app.usageKey())
}

func (app *App) addUsageForModel(u agent.PromptUsage, modelKey string) {
	cost, _ := app.promptCost(u.Usage)
	addTotals(&app.usage, u.Usage, cost)
	if app.usageByModel == nil {
		app.usageByModel = map[string]session.UsageTotals{}
	}
	bucket := app.usageByModel[modelKey]
	addTotals(&bucket, u.Usage, cost)
	app.usageByModel[modelKey] = bucket
	if app.Renderer != nil {
		active := app.usageByModel[app.usageKey()]
		app.Renderer.SetCumulativeUsage(active.InputTokens, active.OutputTokens, app.usage.CostUSD)
	}
}

// addMaintenanceUsage accounts for a model call that supports the session but
// is not a conversational turn, and records it separately in replay metadata.
func (app *App) addMaintenanceUsage(purpose string, usage llm.Usage) {
	app.addMaintenanceUsageForModel(purpose, usage, app.usageKey())
}

func (app *App) addMaintenanceUsageForModel(purpose string, usage llm.Usage, modelKey string) {
	app.addUsageForModel(agent.PromptUsage{Usage: usage, Maintenance: usage}, modelKey)
	app.recordEvent(session.Event{
		Type:    session.EventMaintenanceUsage,
		Prompt:  app.PromptNumber,
		Purpose: purpose,
		Usage:   &usage,
	})
}

// QueueMaintenanceUsage accepts accounting from background maintenance work.
// The REPL drains the queue on its owning goroutine before prompts, usage
// reports, and saves, so renderer and session state remain single-threaded.
func (app *App) QueueMaintenanceUsage(usage agent.MaintenanceUsage) {
	app.QueueMaintenanceUsageForModel(app.usageKey(), usage)
}

// QueueMaintenanceUsageForModel pins background accounting to the model
// snapshot that started the work, even if the active model changes before the
// REPL drains the queue.
func (app *App) QueueMaintenanceUsageForModel(modelKey string, usage agent.MaintenanceUsage) {
	app.maintenanceMu.Lock()
	app.pendingMaintenance = append(app.pendingMaintenance, queuedMaintenanceUsage{
		MaintenanceUsage: usage,
		modelKey:         modelKey,
	})
	app.maintenanceMu.Unlock()
}

// QueuePrewarmResultForModel routes a background prewarm completion through
// the same owner-goroutine queue as maintenance accounting.
func (app *App) QueuePrewarmResultForModel(modelKey string, result agent.PrewarmResult) {
	app.maintenanceMu.Lock()
	app.pendingMaintenance = append(app.pendingMaintenance, queuedMaintenanceUsage{
		MaintenanceUsage: agent.MaintenanceUsage{Purpose: "prewarm", Usage: result.Usage},
		modelKey:         modelKey,
		prewarm:          &result,
	})
	app.maintenanceMu.Unlock()
}

func (app *App) drainMaintenanceUsage() {
	app.maintenanceMu.Lock()
	pending := app.pendingMaintenance
	app.pendingMaintenance = nil
	app.maintenanceMu.Unlock()
	for _, item := range pending {
		if item.prewarm != nil {
			app.Agent.ApplyPrewarmResult(*item.prewarm)
		}
		if item.Usage != (llm.Usage{}) {
			app.addMaintenanceUsageForModel(item.Purpose, item.Usage, item.modelKey)
		}
	}
}

// addTotals accumulates one model call's tokens and cost into dst.
func addTotals(dst *session.UsageTotals, u llm.Usage, cost float64) {
	dst.InputTokens += u.InputTokens
	dst.OutputTokens += u.OutputTokens
	dst.CacheReadTokens += u.CacheReadTokens
	dst.CacheWriteTokens += u.CacheWriteTokens
	dst.CacheWrite1hTokens += u.CacheWrite1hTokens
	dst.ReasoningTokens += u.ReasoningTokens
	dst.CostUSD += cost
}

// onModelChanged reports the per-model usage breakdown so the prior model's cost
// is visible, then resets the live cumulative readout to the new model's bucket
// while keeping the session-total cost. The caller has already updated
// app.Provider/Model/RegistryModel to the new model.
func (app *App) onModelChanged() {
	if app.usage.InputTokens != 0 || app.usage.OutputTokens != 0 {
		fmt.Fprintln(app.Errw, app.usageReport("session summary"))
	}
	if app.Renderer != nil {
		b := app.usageByModel[app.usageKey()]
		app.Renderer.SetCumulativeUsage(b.InputTokens, b.OutputTokens, app.usage.CostUSD)
	}
}

// saveOrWarn is the automatic-save path used by every place that saves without a
// user explicitly asking (after-prompt auto-save, exit saves, /compact). A failed
// save must never be silent: a visible warning beats silent data loss (design
// §11, §12), since a stale or missing on-disk transcript otherwise looks saved.
// The explicit /save command surfaces its own richer success/failure message and
// does not route through here.
func (app *App) saveOrWarn(path string) {
	if err := app.save(path); err != nil {
		fmt.Fprintf(app.Errw, "[save failed: %v]\n", err)
	}
}

// save writes the current transcript and usage totals to path (design §11).
func (app *App) save(path string) error {
	if path == "" {
		return nil
	}
	app.drainMaintenanceUsage()
	s, err := app.sessionSnapshot(nil)
	if err != nil {
		return err
	}
	return s.SaveConsolidated(path)
}

func (app *App) sessionSnapshot(current *agent.PromptUsage) (session.Session, error) {
	if err := app.ensureSessionTree(); err != nil {
		return session.Session{}, err
	}
	usage := app.usage
	var usageByModel map[string]session.UsageTotals
	if len(app.usageByModel) > 0 {
		usageByModel = make(map[string]session.UsageTotals, len(app.usageByModel))
		maps.Copy(usageByModel, app.usageByModel)
	}
	if current != nil {
		cost, known := app.promptCost(current.Usage)
		if !known && app.Registry != nil {
			cost, known = app.Registry.Cost(app.usageKey(), current.Usage)
		}
		if !known {
			cost = 0
		}
		addTotals(&usage, current.Usage, cost)
		if usageByModel == nil {
			usageByModel = make(map[string]session.UsageTotals)
		}
		bucket := usageByModel[app.usageKey()]
		addTotals(&bucket, current.Usage, cost)
		usageByModel[app.usageKey()] = bucket
	}
	return session.Session{
		Version:         session.Version,
		Provider:        app.Provider,
		Model:           app.Model,
		Created:         app.Created,
		Updated:         app.clock()(),
		System:          app.System,
		Agent:           app.AgentName,
		ProxySessionID:  app.Agent.ProxySessionID(),
		CacheAffinityID: app.Agent.CacheAffinityID(),
		Prompt:          app.PromptNumber,
		Messages:        app.Agent.Transcript(),
		Tree:            app.SessionTree,
		ResponseState:   app.Agent.ResponseState(),
		Todos:           app.todoSnapshot(),
		Plans:           app.planSnapshot(),
		Usage:           usage,
		UsageByModel:    usageByModel,
	}, nil
}

func (app *App) ensureSessionTree() error {
	if app.SessionTree != nil {
		return nil
	}
	cwd, _ := os.Getwd()
	tree, err := session.LinearTree(app.Created, cwd, app.Agent.Transcript())
	if err != nil {
		return fmt.Errorf("session tree: %w", err)
	}
	app.SessionTree = tree
	return nil
}

// PrepareCompaction binds the agent archive callback to the immutable tree.
// The next save observes the rewritten transcript and commits the checkpoint.
func (app *App) PrepareCompaction(before []llm.Message, olderCount int, summary, archiveRef string, tokensBefore int, focus string, readFiles, modifiedFiles []string) error {
	if err := app.ensureSessionTree(); err != nil {
		return err
	}
	if err := app.SessionTree.PrepareCompaction(before, olderCount, summary, archiveRef, tokensBefore, focus, readFiles, modifiedFiles); err != nil {
		return err
	}
	// Compaction rewrites the transcript and may drop the raw update_todos
	// result, so re-inject the todo reminder on the next request.
	if app.Todos != nil {
		app.Todos.ResetContextInjected()
	}
	return nil
}

// planSnapshot returns the recorded plans for persistence, or nil when the plan
// store is not wired (one-shot mode and tests leave it nil).
func (app *App) planSnapshot() []plan.Plan {
	if app.Plans == nil {
		return nil
	}
	return app.Plans.Snapshot()
}

// todoSnapshot returns the current todo list for persistence, or nil when the
// todo store is not wired (one-shot mode and tests leave it nil).
func (app *App) todoSnapshot() []todo.Item {
	if app.Todos == nil {
		return nil
	}
	return app.Todos.Snapshot()
}

func (app *App) beginPrompt(prompt string, images []inputimage.Loaded) int {
	app.drainMaintenanceUsage()
	app.PromptNumber++
	app.recordEvent(session.Event{
		Time:   app.clock()(),
		Type:   session.EventUser,
		Prompt: app.PromptNumber,
		Text:   prompt,
		Images: sessionImages(images),
	})
	return app.PromptNumber
}

func (app *App) runPromptSubmitHook(ctx context.Context, prompt string, promptID int) hooks.Result {
	if app.Hooks == nil || !app.Hooks.HasEvent(hooks.UserPromptSubmit) {
		return hooks.Result{}
	}
	res := app.Hooks.Run(ctx, hooks.UserPromptSubmit, "", hooks.Payload{
		"prompt_id": promptID,
		"prompt":    prompt,
	})
	app.renderHookNotices(res.Notices)
	return res
}

func (app *App) RunSessionStartHook(source string) {
	if app.Hooks == nil || !app.Hooks.HasEvent(hooks.SessionStart) {
		return
	}
	res := app.Hooks.Run(context.Background(), hooks.SessionStart, source, hooks.Payload{"source": source})
	app.renderHookNotices(res.Notices)
	if len(res.AdditionalContext) > 0 {
		app.AddHookContext(res.AdditionalContext)
	}
	if res.Block {
		reason := res.Reason()
		if reason == "" {
			reason = "blocked by SessionStart hook"
		}
		app.renderHookNotices([]string{"[session-start hook blocked; continuing: " + reason + "]"})
	}
}

func (app *App) renderHookNotices(notices []string) {
	for _, notice := range notices {
		if strings.TrimSpace(notice) == "" {
			continue
		}
		if app.Renderer != nil {
			app.Renderer.Notice(notice)
		} else {
			fmt.Fprintln(app.Errw, notice)
		}
	}
}

func (app *App) promptHookContext(promptContext []string) []string {
	out := make([]string, 0, len(app.HookContext)+len(promptContext))
	out = append(out, app.HookContext...)
	out = append(out, promptContext...)
	return out
}

func (app *App) requestContext(promptContext []string) []string {
	out := app.promptHookContext(promptContext)
	if ctx := app.todoRequestContext(); ctx != "" {
		out = append(out, ctx)
	}
	out = append(out, app.backgroundRequestContext(nil)...)
	return out
}

func (app *App) backgroundRequestContext(archiver agent.ToolResultArchiver) []string {
	if app.Background == nil {
		return nil
	}
	return app.Background.DrainCompletedContext(archiver)
}

func (app *App) pollBackgroundNotices() {
	if app.Background == nil {
		return
	}
	for _, notice := range app.Background.DrainNotices() {
		if app.Renderer != nil {
			app.Renderer.Notice(notice)
		} else {
			fmt.Fprintln(app.Errw, notice)
		}
	}
}

func (app *App) printTodoPromptStatus() bool {
	return app.printTodoStatus(false)
}

func (app *App) printTodoUpdateStatus() bool {
	return app.printTodoStatus(true)
}

func (app *App) printTodoStatus(includeEmpty bool) bool {
	if app.Todos == nil || !app.agentHasTool("update_todos") {
		return false
	}
	items := app.Todos.Snapshot()
	if len(items) == 0 && !includeEmpty {
		return false
	}
	fmt.Fprintln(app.Errw, todo.Render(items))
	return true
}

func (app *App) markTodoPromptStatusPrintedBeforeUsage(turn int) {
	app.todoPromptStatusBeforeUsage = true
	app.todoPromptStatusBeforeUsagePrompt = turn
}

func (app *App) todoPromptStatusPrintedBeforeUsageForPrompt(turn int) bool {
	return app.todoPromptStatusBeforeUsage && app.todoPromptStatusBeforeUsagePrompt == turn
}

// printPlanStatus prints a one-line pointer to the most recently recorded plan's
// file (mirroring the todo status). It prints only when the visible agent has
// record_plan and a plan with a path has been recorded, so the user always knows
// where the plan was written without the model having to say so. It is
// display-only and never part of the model's tool result or context.
func (app *App) printPlanStatus(state plan.DisplayState) bool {
	if app.Plans == nil || !app.agentHasTool("record_plan") {
		return false
	}
	line := plan.RenderLatest(app.Plans.Snapshot(), state)
	if line == "" {
		return false
	}
	fmt.Fprintln(app.Errw, line)
	return true
}

func (app *App) markPlanPromptStatusPrintedBeforeUsage(turn int) {
	app.planPromptStatusBeforeUsage = true
	app.planPromptStatusBeforeUsagePrompt = turn
}

func (app *App) planPromptStatusPrintedBeforeUsageForPrompt(turn int) bool {
	return app.planPromptStatusBeforeUsage && app.planPromptStatusBeforeUsagePrompt == turn
}

func (app *App) stopBackgroundJobs() {
	if app.Background != nil {
		app.Background.Shutdown()
	}
}

func (app *App) todoRequestContext() string {
	if app.Todos == nil || !app.agentHasTool("update_todos") {
		return ""
	}
	// Only re-render the reminder when the list changed since it was last
	// injected (see accumulatingSink.RequestContext, which marks the injection).
	// The list already lives in the transcript via the update_todos result, so
	// re-sending an unchanged reminder every turn is pure overhead.
	return app.Todos.ChangedRequestContext()
}

// todoContextDisplay renders the current list for display paths like /context,
// regardless of injection marking. It must never consume the change signal
// real requests rely on.
func (app *App) todoContextDisplay() string {
	if app.Todos == nil || !app.agentHasTool("update_todos") {
		return ""
	}
	return todo.RequestContext(app.Todos.Snapshot())
}

func (app *App) agentHasTool(name string) bool {
	if app.Agent == nil {
		return false
	}
	for _, toolName := range app.Agent.ToolNames() {
		if toolName == name {
			return true
		}
	}
	return false
}

func (app *App) currentModelSupportsImages() bool {
	if app.Registry == nil {
		return false
	}
	return app.Registry.SupportsInputModality(app.currentRegistryModel(), "image")
}

func (app *App) imageUnsupportedNotice() string {
	model := app.currentRegistryModel()
	if model == "" {
		model = app.Model
	}
	if model == "" {
		return "[image skipped: current model does not support image input]"
	}
	return fmt.Sprintf("[image skipped: model %s does not support image input]", model)
}

// AddHookContext keeps hook-generated context available for later turns
// without writing it into the saved transcript.
func (app *App) AddHookContext(ctx []string) {
	for _, item := range ctx {
		if strings.TrimSpace(item) != "" {
			app.HookContext = append(app.HookContext, item)
		}
	}
}

func (app *App) takePendingImages() []inputimage.Loaded {
	if len(app.PendingImages) == 0 {
		return nil
	}
	if !app.currentModelSupportsImages() {
		fmt.Fprintln(app.Errw, app.imageUnsupportedNotice())
		app.PendingImages = nil
		return nil
	}
	images := append([]inputimage.Loaded(nil), app.PendingImages...)
	app.PendingImages = nil
	return images
}

func imageBlocks(images []inputimage.Loaded) []llm.ContentBlock {
	if len(images) == 0 {
		return nil
	}
	blocks := make([]llm.ContentBlock, 0, len(images))
	for _, image := range images {
		blocks = append(blocks, image.Block)
	}
	return blocks
}

func sessionImages(images []inputimage.Loaded) []session.ImageInfo {
	if len(images) == 0 {
		return nil
	}
	out := make([]session.ImageInfo, 0, len(images))
	for _, image := range images {
		out = append(out, session.ImageInfo{
			Name:         image.Info.Name,
			Path:         image.Info.Path,
			MediaType:    image.Info.MediaType,
			Detail:       image.Info.Detail,
			Bytes:        image.Info.Bytes,
			EncodedBytes: image.Info.EncodedBytes,
			Width:        image.Info.Width,
			Height:       image.Info.Height,
		})
	}
	return out
}

func (app *App) recordEvent(ev session.Event) {
	if app.SessionPath == "" {
		return
	}
	if ev.Time.IsZero() {
		ev.Time = app.clock()()
	}
	if err := session.AppendEvent(app.SessionPath, ev); err != nil {
		fmt.Fprintf(app.Errw, "[session event log failed: %v]\n", err)
	}
}

// usageSummary renders the cumulative session usage for /usage (design §10).
func (app *App) usageSummary() string {
	app.drainMaintenanceUsage()
	return app.usageReport("session")
}

// usageReport renders cumulative session usage under the given label. With at
// most one model it is the single-line legacy format; with several it breaks
// down per model target and always ends with the session-total cost.
func (app *App) usageReport(label string) string {
	var b strings.Builder
	if len(app.usageByModel) <= 1 {
		writeUsageTotals(&b, "["+label+": ", app.usage, "]")
		return b.String()
	}
	fmt.Fprintf(&b, "[%s by model:", label)
	for _, key := range slices.Sorted(maps.Keys(app.usageByModel)) {
		writeUsageTotals(&b, "\n  "+key+": ", app.usageByModel[key], "")
	}
	fmt.Fprintf(&b, "\n  total · $%.4f]", app.usage.CostUSD)
	return b.String()
}

// writeUsageTotals writes one usage line: prefix, the token counts (cache write
// and cost shown only when non-zero), then suffix.
func writeUsageTotals(b *strings.Builder, prefix string, u session.UsageTotals, suffix string) {
	fmt.Fprintf(b, "%s%d input / %d cached input / %d output / %d reasoning",
		prefix, u.InputTokens, u.CacheReadTokens, u.OutputTokens, u.ReasoningTokens)
	if u.CacheWriteTokens > 0 {
		fmt.Fprintf(b, " / %d cache write", u.CacheWriteTokens)
	}
	if u.CacheWrite1hTokens > 0 {
		fmt.Fprintf(b, " / %d cache write (1h)", u.CacheWrite1hTokens)
	}
	if u.CostUSD > 0 {
		fmt.Fprintf(b, " · $%.4f", u.CostUSD)
	}
	b.WriteString(suffix)
}

func (app *App) printExitUsageSummary() {
	fmt.Fprintln(app.Errw, app.usageReport("session summary"))
	if app.SessionPath != "" {
		fmt.Fprintf(app.Errw, "resume with: harness -resume %s\n", app.SessionPath)
	}
}

// skillsSummary renders the available skills for /skills (design §10), grouped
// by source directory (local vs user skills).
func (app *App) skillsSummary() string {
	if len(app.Skills) == 0 {
		return "[no skills available]"
	}

	// Group skills by scope
	byScope := make(map[skills.Scope][]string)
	for name, s := range app.Skills {
		byScope[s.Scope] = append(byScope[s.Scope], name)
	}

	// Find directory paths for each scope
	scopePath := make(map[skills.Scope]string)
	for _, d := range app.SkillDirs {
		scopePath[d.Scope] = d.Path
	}

	var b strings.Builder

	// Build directory label (only user-scope sections render one)
	dirLabel := func(scope skills.Scope) string {
		if path, ok := scopePath[scope]; ok {
			return path
		}
		return "user"
	}

	// Print local (project) skills first, then user skills
	for _, scope := range []skills.Scope{skills.ScopeProject, skills.ScopeUser} {
		names := byScope[scope]
		if len(names) == 0 {
			continue
		}
		sort.Strings(names)

		if scope == skills.ScopeProject {
			b.WriteString("local skills:\n")
		} else {
			fmt.Fprintf(&b, "user skills (%s):\n", dirLabel(scope))
		}

		rows := make([]NameDescription, 0, len(names))
		for _, name := range names {
			s := app.Skills[name]
			rows = append(rows, NameDescription{Name: name, Description: s.Description})
		}
		WriteNameDescriptionList(&b, rows, NameDescriptionListOptions{
			Indent:     "  ",
			NamePrefix: "$",
			Width:      app.summaryWidth(),
		})
	}

	return b.String()
}

// toolsSummary renders the available tools for /tools: enabled built-in tools,
// enabled MCP tools (grouped by server), and disabled built-in tools with reasons.
func (app *App) toolsSummary() string {
	specs := app.Agent.ToolSpecs()

	var builtins, mcps []string
	descriptions := make(map[string]string, len(specs))
	for _, spec := range specs {
		descriptions[spec.Name] = spec.Description
		if isMCPToolName(spec.Name) {
			mcps = append(mcps, spec.Name)
		} else {
			builtins = append(builtins, spec.Name)
		}
	}

	var b strings.Builder

	// Enabled built-in tools
	if len(builtins) > 0 {
		b.WriteString("built-in tools:\n")
		rows := make([]NameDescription, 0, len(builtins))
		for _, name := range builtins {
			rows = append(rows, NameDescription{Name: name, Description: descriptions[name]})
		}
		WriteNameDescriptionList(&b, rows, NameDescriptionListOptions{Indent: "  ", Width: app.summaryWidth()})
	}

	// Enabled MCP tools, grouped by server
	if len(mcps) > 0 {
		if b.Len() > 0 {
			b.WriteByte('\n')
		}
		byServer := make(map[string][]string)
		for _, name := range mcps {
			label := mcpServerLabel(name)
			byServer[label] = append(byServer[label], name)
		}
		// Sort server labels for stable output
		labels := make([]string, 0, len(byServer))
		for l := range byServer {
			labels = append(labels, l)
		}
		sort.Strings(labels)
		b.WriteString("mcp tools:\n")
		for _, label := range labels {
			fmt.Fprintf(&b, "  [%s]\n", label)
			rows := make([]NameDescription, 0, len(byServer[label]))
			for _, name := range byServer[label] {
				rows = append(rows, NameDescription{Name: name, Description: descriptions[name]})
			}
			WriteNameDescriptionList(&b, rows, NameDescriptionListOptions{Indent: "    ", Width: app.summaryWidth()})
		}
	}

	// Disabled tools
	if len(app.DisabledTools) > 0 {
		if b.Len() > 0 {
			b.WriteByte('\n')
		}
		b.WriteString("disabled tools:\n")
		for _, d := range app.DisabledTools {
			fmt.Fprintf(&b, "  %s  (%s)\n", d.Name, d.Reason)
		}
	}

	if b.Len() == 0 {
		return "[no tools available]"
	}
	return b.String()
}

func (app *App) backgroundCommand(arg string) {
	if app.Background == nil {
		fmt.Fprintln(app.Errw, "[background: unavailable]")
		return
	}
	fields := strings.Fields(arg)
	if len(fields) == 0 {
		fmt.Fprintln(app.Errw, app.backgroundList())
		return
	}
	if fields[0] == "cancel" {
		if len(fields) < 2 {
			fmt.Fprintln(app.Errw, "[background: cancel requires a job id]")
			return
		}
		snap, ok := app.Background.Cancel(fields[1])
		if !ok {
			fmt.Fprintf(app.Errw, "[background: unknown job %q]\n", fields[1])
			return
		}
		fmt.Fprintf(app.Errw, "[background: %s %s]\n", snap.ID, snap.Status)
		return
	}
	snap, ok := app.Background.Get(fields[0])
	if !ok {
		fmt.Fprintf(app.Errw, "[background: unknown job %q]\n", fields[0])
		return
	}
	fmt.Fprintln(app.Errw, formatBackgroundSnapshot(snap))
}

func (app *App) backgroundList() string {
	jobs := app.Background.List()
	if len(jobs) == 0 {
		return "[background: no jobs]"
	}
	var b strings.Builder
	b.WriteString("background jobs:")
	for _, job := range jobs {
		fmt.Fprintf(&b, "\n  %s  %s", job.ID, job.Status)
		if job.Kind != "" {
			fmt.Fprintf(&b, "  %s", job.Kind)
		}
		if job.Agent != "" {
			fmt.Fprintf(&b, "  %s", job.Agent)
		}
		if job.Result.TranscriptPath != "" {
			fmt.Fprintf(&b, "  %s", job.Result.TranscriptPath)
		}
	}
	return b.String()
}

func formatBackgroundSnapshot(job background.Snapshot) string {
	var b strings.Builder
	fmt.Fprintf(&b, "[background: %s %s]", job.ID, job.Status)
	if job.Kind != "" {
		fmt.Fprintf(&b, "\nkind: %s", job.Kind)
	}
	if job.Agent != "" {
		fmt.Fprintf(&b, "\nagent: %s", job.Agent)
	}
	if job.Result.TranscriptPath != "" {
		fmt.Fprintf(&b, "\ntranscript: %s", job.Result.TranscriptPath)
	}
	if job.Error != "" {
		fmt.Fprintf(&b, "\nerror: %s", job.Error)
	}
	if strings.TrimSpace(job.Result.Text) != "" {
		fmt.Fprintf(&b, "\nresult:\n%s", strings.TrimSpace(job.Result.Text))
	}
	return b.String()
}

// mcpServerLabel extracts a display-friendly server label from an MCP tool
// name of the form mcp__<server>__<tool>. It mirrors mcptools.serverLabel.
func mcpServerLabel(name string) string {
	const prefix = "mcp__"
	rest, _ := strings.CutPrefix(name, prefix)
	label, _, _ := strings.Cut(rest, "__")
	if label == "" {
		return "(unknown)"
	}
	return label
}

func isMCPToolName(name string) bool {
	return strings.HasPrefix(name, "mcp__")
}

func (app *App) summaryWidth() int {
	if app.SummaryWidth == nil {
		return 0
	}
	return app.SummaryWidth()
}

// accumulatingSink forwards events to the renderer while accumulating cumulative
// token totals and cost for the session (design §10 /usage, §11 saved totals).
type accumulatingSink struct {
	r                           *Renderer
	app                         *App
	prompt                      int
	printTodoUpdate             bool
	printTodoPromptBeforeUsage  bool
	printPlanUpdate             bool
	printPlanPromptBeforeUsage  bool
	planCountAtPromptStart      int
	reasoningOutput             bool
	pending                     map[string]llm.ToolCall
	turn                        int
	attempt                     int
	inMaintenance               bool
	terminalModelErrorDisplayed bool
}

func newAccumulatingSink(r *Renderer, app *App, prompt int) *accumulatingSink {
	return &accumulatingSink{r: r, app: app, prompt: prompt, pending: make(map[string]llm.ToolCall)}
}

func newREPLSink(r *Renderer, app *App, prompt int) *accumulatingSink {
	s := newAccumulatingSink(r, app, prompt)
	s.printTodoUpdate = true
	s.printTodoPromptBeforeUsage = true
	s.printPlanUpdate = true
	s.printPlanPromptBeforeUsage = true
	if app.Plans != nil {
		s.planCountAtPromptStart = len(app.Plans.Snapshot())
	}
	s.reasoningOutput = true
	return s
}

func (s *accumulatingSink) planDisplayState() plan.DisplayState {
	current := 0
	if s.app.Plans != nil {
		current = len(s.app.Plans.Snapshot())
	}
	switch {
	case current <= s.planCountAtPromptStart:
		return plan.DisplayCurrent
	case s.planCountAtPromptStart == 0 && current == 1:
		return plan.DisplayRecorded
	default:
		return plan.DisplayUpdated
	}
}

func (s *accumulatingSink) TextDelta(text string) {
	s.r.TextDelta(text)
	s.app.recordEvent(session.Event{
		Type:    session.EventAssistantDelta,
		Prompt:  s.prompt,
		Turn:    s.turn,
		Text:    text,
		Attempt: s.attempt,
	})
}

func (s *accumulatingSink) AssistantPhase(phase string) {
	if !llm.ValidAssistantPhase(phase) || phase == "" {
		return
	}
	s.r.AssistantPhase(phase)
	s.app.recordEvent(session.Event{
		Type:    session.EventAssistantPhase,
		Prompt:  s.prompt,
		Turn:    s.turn,
		Phase:   phase,
		Attempt: s.attempt,
	})
}

func (s *accumulatingSink) ReasoningSummary(text string) {
	text = strings.TrimSpace(text)
	if text == "" || !reasoningSummaryDisplayEnabled(s.app.Reasoning.Summary) {
		return
	}
	if s.reasoningOutput {
		s.r.ReasoningSummary(text)
	} else {
		s.r.ReasoningSummaryStatus(text)
	}
	s.app.recordEvent(session.Event{
		Type:    session.EventReasoningSummary,
		Prompt:  s.prompt,
		Turn:    s.turn,
		Text:    text,
		Attempt: s.attempt,
	})
}

func (s *accumulatingSink) CompactionStart() {
	s.inMaintenance = true
	s.r.CompactionStart()
}

func (s *accumulatingSink) CompactionComplete() {
	s.r.CompactionComplete()
	s.inMaintenance = false
}

func (s *accumulatingSink) TurnAttemptStart(turn, attempt int, ctx agent.ContextEstimate) {
	s.turn = turn
	s.attempt = attempt
	s.r.TurnAttemptStart(turn, attempt, ctx)
	s.app.recordEvent(session.Event{
		Type:    session.EventTurnAttemptStart,
		Prompt:  s.prompt,
		Turn:    s.turn,
		Attempt: attempt,
		Context: contextSnapshot(ctx),
	})
}

func (s *accumulatingSink) TurnAttemptAbandoned(turn, attempt int) {
	s.app.recordEvent(session.Event{
		Type:    session.EventTurnAttemptAbandoned,
		Prompt:  s.prompt,
		Turn:    turn,
		Attempt: attempt,
		Display: fmt.Sprintf("[turn: %d attempt %d discarded; retrying]", turn, attempt),
	})
}

func (s *accumulatingSink) TurnAttemptComplete(u agent.TurnAttemptUsage) {
	s.r.TurnAttemptComplete(u)
	usage := u.Usage
	s.app.recordEvent(session.Event{
		Type:    session.EventTurnAttemptUsage,
		Prompt:  s.prompt,
		Turn:    u.Turn,
		Usage:   &usage,
		Attempt: u.Attempt,
	})
}

func (s *accumulatingSink) ModelRequestEvent(event llm.ModelRequestEvent) {
	line := s.r.ModelRequestEvent(event)
	if line != "" && event.Outcome == llm.ModelRequestOutcomeTerminal {
		s.terminalModelErrorDisplayed = true
	}
	copyEvent := event
	s.app.recordEvent(session.Event{
		Type:         session.EventModelRequest,
		Prompt:       s.prompt,
		Turn:         s.turn,
		Attempt:      s.attempt,
		Display:      line,
		ModelRequest: &copyEvent,
	})
	if s.app.DiagnosticLogger == nil {
		return
	}
	switch event.State {
	case llm.ModelRequestUpstreamAttemptFailed, llm.ModelRequestFailed:
		s.app.DiagnosticLogger.Warn("model API issue", modelRequestLogAttrs(s.prompt, s.turn, s.attempt, event)...)
	case llm.ModelRequestCancelled:
		s.app.DiagnosticLogger.Info("model request cancelled", modelRequestLogAttrs(s.prompt, s.turn, s.attempt, event)...)
	}
}

func modelRequestLogAttrs(prompt, turn, attempt int, event llm.ModelRequestEvent) []any {
	return []any{
		"prompt", prompt,
		"turn", turn,
		"attempt", attempt,
		"sequence", event.Sequence,
		"state", string(event.State),
		"outcome", string(event.Outcome),
		"proxy_instance_id", event.ProxyInstanceID,
		"proxy_request_id", event.ProxyRequestID,
		"upstream_request_id", event.UpstreamRequestID,
		"trace_id", event.TraceID,
		"span_id", event.SpanID,
		"target_id", event.TargetID,
		"provider", event.Provider,
		"api_type", event.APIType,
		"model", event.Model,
		"purpose", event.Purpose,
		"upstream_attempt", event.Attempt,
		"upstream_max_attempts", event.MaxAttempts,
		"api_status_code", event.StatusCode,
		"api_code", event.Code,
		"api_message", event.Message,
		"api_retryable", event.Retryable,
		"api_retry_after_ms", event.RetryAfterMS,
		"retry_delay_ms", event.RetryDelayMS,
		"attempt_duration_ms", event.AttemptDurationMS,
		"elapsed_ms", event.ElapsedMS,
		"error_stage", string(event.Stage),
	}
}

func (s *accumulatingSink) ToolUseStart(c llm.ToolCall) {
	s.r.ToolUseStart(c)
}

func (s *accumulatingSink) ToolUseDelta(index int, delta string) {
	s.r.ToolUseDelta(index, delta)
}

func (s *accumulatingSink) ToolStart(c llm.ToolCall) {
	s.pending[c.ID] = c
	s.r.ToolStart(c)
	s.app.recordEvent(session.Event{Type: session.EventToolStart, Prompt: s.prompt, Turn: s.turn, ToolID: c.ID, Tool: c.Name, Input: c.Input})
}

func (s *accumulatingSink) ToolResult(res llm.ToolResult) {
	call := s.pending[res.ForID]
	delete(s.pending, res.ForID)
	line := ToolResultLine(call, res)
	s.r.ToolResult(res)
	if s.printTodoUpdate && call.Name == "update_todos" && !res.IsError {
		s.app.printTodoUpdateStatus()
	}
	if s.printPlanUpdate && call.Name == "record_plan" && !res.IsError {
		s.app.printPlanStatus(s.planDisplayState())
	}
	s.app.recordEvent(session.Event{Type: session.EventToolResult, Prompt: s.prompt, Turn: s.turn, ToolID: res.ForID, Tool: call.Name, Display: line})
}

func (s *accumulatingSink) ToolDiff(call llm.ToolCall, path, text string) {
	s.r.ToolDiff(call, path, text)
	s.app.recordEvent(session.Event{
		Type:    session.EventToolDiff,
		Prompt:  s.prompt,
		Turn:    s.turn,
		ToolID:  call.ID,
		Tool:    call.Name,
		Display: strings.TrimRight(text, "\n"),
	})
}

func (s *accumulatingSink) ArchiveToolResult(res llm.ToolResult) (agent.ToolResultArchive, error) {
	ref, err := session.SaveToolResultArtifact(s.app.SessionPath, s.prompt, s.turn, res)
	if err != nil || ref == "" {
		return agent.ToolResultArchive{}, err
	}
	return agent.ToolResultArchive{
		DisplayPath: ref,
		ModelPath:   filepath.Join(s.app.SessionPath, ref),
	}, nil
}

func (s *accumulatingSink) Notice(msg string) {
	s.r.Notice(msg)
	turn := s.turn
	if s.inMaintenance {
		turn = 0
	}
	s.app.recordEvent(session.Event{Type: session.EventNotice, Prompt: s.prompt, Turn: turn, Display: msg})
}

func (s *accumulatingSink) ModelErrorDiagnostic(event agent.ModelErrorDiagnostic) {
	if s.app.DiagnosticLogger == nil || event.Diagnostic == nil || event.Diagnostic.Compatibility == nil {
		return
	}
	diagnostic := event.Diagnostic
	compatibility := diagnostic.Compatibility
	s.app.DiagnosticLogger.Warn("model compatibility diagnostic",
		"prompt", event.Prompt,
		"turn", event.Turn,
		"attempt", event.Attempt,
		"api_status_code", event.StatusCode,
		"api_code", event.Code,
		"api_message", event.Message,
		"error_stage", string(diagnostic.Stage),
		"proxy_instance_id", diagnostic.ProxyInstanceID,
		"proxy_request_id", diagnostic.ProxyRequestID,
		"upstream_request_id", diagnostic.UpstreamRequestID,
		"trace_id", diagnostic.TraceID,
		"span_id", diagnostic.SpanID,
		"target_id", diagnostic.TargetID,
		"provider", diagnostic.Provider,
		"api_type", diagnostic.APIType,
		"model", diagnostic.Model,
		"category", compatibility.Category,
		"reason", compatibility.Reason,
		"confidence", compatibility.Confidence,
		"remediation", compatibility.Remediation,
		"strategy", compatibility.Strategy,
		"multimodal_shape", diagnostic.MultimodalShape,
	)
}

func (s *accumulatingSink) TurnComplete(u agent.TurnUsage) {
	// Price the turn when the provider did not supply a cost (proxy streams set
	// CostKnown themselves), against the App's active model so a mid-prompt
	// model switch is not mispriced (r63). Persist the priced usage so replay
	// and session stats see the same cost as the display line.
	if !u.Usage.CostKnown {
		u.Usage.CostUSD, u.Usage.CostKnown = s.app.Registry.Cost(s.app.usageKey(), u.Usage)
	}
	line := s.r.TurnComplete(u)
	usage := u.Usage
	s.app.recordEvent(session.Event{
		Type:    session.EventTurnComplete,
		Prompt:  s.prompt,
		Turn:    u.Turn,
		Display: line,
		Usage:   &usage,
	})
}

func (s *accumulatingSink) MaintenanceComplete(u agent.MaintenanceUsage) {
	usage := u.Usage
	s.app.recordEvent(session.Event{
		Type:    session.EventMaintenanceUsage,
		Prompt:  s.prompt,
		Purpose: u.Purpose,
		Usage:   &usage,
	})
}

func (s *accumulatingSink) PromptCheckpoint(checkpoint agent.PromptCheckpoint) {
	if s == nil || s.app == nil || s.app.SessionPath == "" {
		return
	}
	state, err := s.app.sessionSnapshot(&checkpoint.Usage)
	if err != nil {
		fmt.Fprintf(s.app.Errw, "[checkpoint failed: %v]\n", err)
		return
	}
	started := time.Now()
	switch checkpoint.Kind {
	case agent.PromptCheckpointClosedTurn:
		err = session.SaveClosedTurnCheckpoint(s.app.SessionPath, state, s.prompt, checkpoint.Turn)
	default:
		err = session.SaveActiveTurnCheckpoint(
			s.app.SessionPath,
			state,
			string(checkpoint.Kind),
			s.prompt,
			checkpoint.Turn,
		)
	}
	elapsed := time.Since(started)
	if err != nil {
		fmt.Fprintf(s.app.Errw, "[checkpoint failed: %v]\n", err)
		return
	}
	s.app.recordEvent(session.Event{
		Type:         session.EventCheckpoint,
		Prompt:       s.prompt,
		Turn:         checkpoint.Turn,
		Purpose:      string(checkpoint.Kind),
		DurationMS:   elapsed.Milliseconds(),
		MessageCount: len(s.app.Agent.Transcript()),
	})
}

func (s *accumulatingSink) RetentionApplied(event agent.RetentionEvent) {
	s.app.recordEvent(session.Event{
		Type:      session.EventRetention,
		Prompt:    s.prompt,
		Turn:      s.turn + 1,
		Retention: retentionSnapshot(event),
	})
}

func retentionSnapshot(event agent.RetentionEvent) *session.RetentionSnapshot {
	return &session.RetentionSnapshot{
		Policy:              event.Policy,
		Trigger:             event.Trigger,
		BlocksTrimmed:       event.BlocksTrimmed,
		BytesBefore:         event.BytesBefore,
		BytesAfter:          event.BytesAfter,
		ContextTokensBefore: event.ContextTokensBefore,
		ContextTokensAfter:  event.ContextTokensAfter,
		ResponseStateReset:  event.ResponseStateReset,
		NextRequestStateful: event.NextRequestStateful,
	}
}

func (s *accumulatingSink) AddHookContext(ctx []string) {
	s.app.AddHookContext(ctx)
}

func (s *accumulatingSink) RequestContext() []string {
	var out []string
	if ctx := s.app.todoRequestContext(); ctx != "" {
		out = append(out, ctx)
		// This sink supplies context to real model requests, so record the list as
		// injected; todoRequestContext stays quiet until the list changes again.
		if s.app.Todos != nil {
			s.app.Todos.MarkContextInjected()
		}
	}
	out = append(out, s.app.backgroundRequestContext(s)...)
	return out
}

// PeekRequestContext mirrors RequestContext without consuming anything: the
// todo change marker stays unmarked and completed background context stays
// queued, so post-prompt size estimates never eat context that still needs to
// reach the model on a later real request.
func (s *accumulatingSink) PeekRequestContext() []string {
	var out []string
	if ctx := s.app.todoRequestContext(); ctx != "" {
		out = append(out, ctx)
	}
	if s.app.Background != nil {
		out = append(out, s.app.Background.PeekCompletedContext()...)
	}
	return out
}

func (s *accumulatingSink) PendingPromptWork() bool {
	return s.app.Background != nil && s.app.Background.PendingPromptWork()
}

func (s *accumulatingSink) WaitForPromptWork(ctx context.Context) (llm.Usage, error) {
	if s.app.Background == nil {
		return llm.Usage{}, nil
	}
	return s.app.Background.WaitForPromptWork(ctx)
}

func (s *accumulatingSink) PromptWorkWaitStart() {
	s.r.PromptWorkWaitStart()
	// Surface the outstanding background delegate jobs' live progress so the
	// wait ticker can summarize their activity while the parent joins them.
	var progress []any
	if s.app.Background != nil {
		for _, snap := range s.app.Background.List() {
			if snap.Status != background.StatusRunning {
				continue
			}
			if snap.Progress != nil {
				progress = append(progress, snap.Progress)
			}
		}
	}
	s.r.SetBackgroundProgress(progress)
}

func (s *accumulatingSink) PromptWorkWaitComplete() {
	s.r.SetBackgroundProgress(nil)
	s.r.PromptWorkWaitComplete()
}

// ToolProgress forwards a foreground tool call's live-progress closure to the
// renderer's wait ticker so child-run activity is visible while the (blocking)
// call runs. A nil progress clears it.
func (s *accumulatingSink) ToolProgress(call llm.ToolCall, progress any) {
	s.r.SetToolProgress(call.Name, progress)
}

func (s *accumulatingSink) DrainPromptWorkUsage() llm.Usage {
	if s.app.Background == nil {
		return llm.Usage{}
	}
	return s.app.Background.DrainPromptWorkUsage()
}

func (s *accumulatingSink) PromptComplete(u agent.PromptUsage) {
	// Price the prompt against the App's own model (not the renderer's) so an
	// in-prompt model switch is not mispriced, and hand it to the renderer (r63).
	cost, costKnown := s.app.promptCost(u.Usage)
	if s.printTodoPromptBeforeUsage {
		s.r.StopProgress()
		s.r.flushToolUseStarts()
		s.r.finishAssistantLine()
		if s.app.printTodoPromptStatus() {
			s.app.markTodoPromptStatusPrintedBeforeUsage(s.prompt)
		}
	}
	if s.printPlanPromptBeforeUsage {
		s.r.StopProgress()
		s.r.flushToolUseStarts()
		s.r.finishAssistantLine()
		if s.app.printPlanStatus(s.planDisplayState()) {
			s.app.markPlanPromptStatusPrintedBeforeUsage(s.prompt)
		}
	}
	s.r.SetPromptCost(cost, costKnown)
	s.r.PromptComplete(u)
	s.app.addUsage(u)
	// Regenerate the line for the session event record after cumulative totals
	// have been updated by PromptComplete above.
	line := usageLine(u, s.r.now().Sub(s.r.promptRunStart), cost, costKnown,
		s.r.cumInput, s.r.cumOutput, s.r.cumCost)
	usage := u.Usage
	s.app.recordEvent(session.Event{
		Type:              session.EventPromptUsage,
		Prompt:            s.prompt,
		Display:           line,
		Usage:             &usage,
		TerminationReason: string(u.TerminationReason),
	})
}

func contextSnapshot(ctx agent.ContextEstimate) *session.ContextSnapshot {
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
		PayloadTotal:    ctx.PayloadTotal,
		PayloadSystem:   ctx.PayloadSystem,
		PayloadTools:    ctx.PayloadTools,
		PayloadMessages: ctx.PayloadMessages,
	}
}
