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
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"harness/internal/agent"
	"harness/internal/background"
	"harness/internal/goal"
	"harness/internal/handoff"
	"harness/internal/hooks"
	"harness/internal/inputimage"
	"harness/internal/llm"
	"harness/internal/otel"
	"harness/internal/plan"
	"harness/internal/reasoningprofile"
	"harness/internal/replprompt"
	"harness/internal/runstream"
	"harness/internal/session"
	"harness/internal/sessionrec"
	"harness/internal/skills"
	"harness/internal/term"
	"harness/internal/todo"
	"harness/internal/tools"
)

const (
	bracketedPasteStart     = "\x1b[200~"
	bracketedPasteEnd       = "\x1b[201~"
	shiftTabPrewarmDebounce = 500 * time.Millisecond
)

// ModelSelection is the runtime model/provider bundle returned by App.SwitchModel.
type ModelSelection struct {
	Provider              string
	Model                 string
	RegistryModel         string
	BaseURL               string
	Runtime               llm.Provider
	ContextWindow         int // agent override; 0 means use the registry
	Reasoning             llm.ReasoningConfig
	BaseTargetID          string
	ReasoningReplayDomain string
	Variant               string
	FastTargetID          string
	ServerTools           []llm.ServerTool
	ResponsesStateful     bool
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
	Name                  string
	Tools                 *tools.Registry
	System                string
	Provider              string
	Model                 string
	RegistryModel         string
	BaseURL               string
	Runtime               llm.Provider
	ContextWindow         int
	Reasoning             llm.ReasoningConfig
	BaseTargetID          string
	ReasoningReplayDomain string
	Variant               string
	FastTargetID          string
	ServerTools           []llm.ServerTool
	ResponsesStateful     bool
	ReasoningSet          bool
}

// LSPServerStatus and LSPStatus are UI-neutral snapshots supplied by the
// harness runtime. LoadedRoots are initialized, currently-live server roots;
// Available only means the configured command is present on PATH.
type LSPServerStatus struct {
	Name        string
	Languages   []string
	Command     string
	Available   bool
	LoadedRoots []string
}

type LSPStatus struct {
	Enabled            bool
	Tools              []string
	AvailableLanguages []string
	LoadedLanguages    []string
	Servers            []LSPServerStatus
}

// LSPSelection is returned by an enable/disable command. A non-nil Tools
// registry and non-empty System are installed together before the next prompt,
// ensuring model disclosure and dispatch stay in sync.
type LSPSelection struct {
	Tools  *tools.Registry
	System string
	Status LSPStatus
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

	Provider              string
	Model                 string
	RegistryModel         string
	BaseURL               string
	Registry              *llm.Registry
	System                string
	Reasoning             llm.ReasoningConfig
	BaseTargetID          string
	ReasoningReplayDomain string
	Variant               string
	FastTargetID          string
	ImageDetail           string
	PendingImages         []inputimage.Loaded
	Hooks                 *hooks.Runner
	HookContext           []string
	Background            *background.Manager

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
	// HandoffSwitchAgent may select an implementation agent that is hidden from
	// ordinary root interactive selection. Nil falls back to SwitchAgent.
	HandoffSwitchAgent func(name string) (AgentSelection, error)

	// RefreshMCP, when set, is consulted at the idle-prompt boundary (just
	// before a typed prompt starts) to pick up proxy tool-list changes.
	// It is called with the current agent name; a non-nil registry replaces the
	// agent's tools and notice is rendered. A nil registry means "no change".
	// nil disables the hook (one-shot mode and tests leave it nil).
	RefreshMCP func(ctx context.Context, agentName string) (*tools.Registry, string)
	// ControlLSP handles session-local status/enable/disable. nil means the
	// embedding did not wire native LSP support.
	ControlLSP func(action, agentName string) (LSPSelection, error)

	todoPromptStatusBeforeUsage       bool
	todoPromptStatusBeforeUsagePrompt int
	planPromptStatusBeforeUsage       bool
	planPromptStatusBeforeUsagePrompt int
	Todos                             *todo.Store
	Plans                             *plan.Store
	// Goal holds the /goal command's session state, persisted in state.json and
	// reset on /clear. nil disables persistence and the autonomous continuation
	// loop.
	Goal *goal.Store
	// GoalMaxContinuations is the safety cap for autonomous continuations per
	// goal; 0 means unlimited. Reaching the cap auto-pauses the goal.
	GoalMaxContinuations int
	// GoalAutoContinue enables the REPL idle-boundary continuation loop. It is
	// wired from the same interactive-session condition as handoff.
	GoalAutoContinue bool
	// WorkflowStatusFunc optionally exposes authoritative bounded workflow state
	// supplied by an embedding orchestrator. Harness does not infer it from text.
	WorkflowStatusFunc func() agent.WorkflowStatus
	// Handoff carries a pending plan->implementation handoff requested by the
	// handoff tool, consumed at the prompt boundary. nil disables.
	Handoff *handoff.Pending
	// HandoffAgent is the default agent a handoff switches to when the request
	// names none. Empty falls back to the built-in default agent.
	HandoffAgent string

	otelSink            *otel.Sink
	otelRecordedSession string

	SessionPath    string // current save path; /clear rotates it
	SessionTree    *session.Tree
	SessionBuild   session.BuildMetadata
	SessionRuntime session.RuntimeProfile
	StateDir       string    // for rotating to a fresh auto-save path on /clear
	Created        time.Time // session creation time (preserved across saves)
	PromptNumber   int       // last started prompt, persisted for replay numbering
	Now            func() time.Time
	// RunStream, when set, mirrors the durable session event stream and the
	// prompt boundary envelopes to the JSON run stream on stdout (design §10,
	// -format json run modes). nil keeps stdout human-facing.
	RunStream *runstream.Writer
	// BeforeSessionPathChange reserves a newly rotated session path before App
	// begins using it. nil leaves path ownership to the embedding.
	BeforeSessionPathChange func(string) error
	OnSessionPathChanged    func(string)
	// OnPromptFinished observes completion after the per-prompt session save.
	// It is primarily useful to coordinate embedders and tests whose process
	// remains alive after a forced REPL exit.
	OnPromptFinished func()
	// onInputDelivered is a test seam invoked while completed input publication
	// is serialized with prompt completion.
	onInputDelivered func()

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

	// Steer, when set, tries to route a prepared model-bound prompt submitted
	// during a running prompt into the agent as an in-prompt steering message
	// (injected before the next model request) instead of queuing it for the next
	// prompt. It returns whether the agent accepted the input; false lets the
	// caller queue it without loss. nil disables steering. Non-model-bound input
	// (shell escapes, /commands, /edit) is never steered.
	Steer func(agent.SteerInput) bool
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

	// lastPromptInterrupted is set by the run closure when a prompt ends because
	// of context cancellation. It is read by the REPL loop to pause an active goal
	// after a user interruption; deadline expiry does not count as interruption.
	lastPromptInterrupted bool

	// pendingAPIContinuation is process-local recovery state for /continue. A
	// non-nil state is distinct from an armed continuation with no request-only
	// context; it is deliberately not persisted across resume.
	pendingAPIContinuation *apiContinuationState
}

type apiContinuationState struct {
	requestContext []string
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
  /continue        retry after the latest live prompt ended with an API error
  /compact [focus] force compaction now with optional one-shot summary focus
  /tree [entry]    browse the conversation tree and branch in this session
  /fork [entry]    branch before a prior prompt into a new session
  /clone           clone the current branch into a new session
  /context [file]  dump current model context, or save it as JSON
  /prompt          show the full system prompt, including runtime hints
  /usage           cumulative session tokens and cost
  /max-turns [n]   show or set turns per prompt for this session (<=0 is unlimited)
  /tools [--raw]   list available tools, or dump model-facing definitions as JSON
  /lsp [status|enable|disable]
                    inspect or toggle native LSP tools for this session
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
  /goal [text]     set, view, clear, pause, or resume the active autonomous goal
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
	// Renderer callbacks and host-side notices can run concurrently while a
	// prompt is active. Make the renderer's coordinator the App-wide output
	// owner even for embedders that supplied the same raw writers separately.
	// The main binary already wires these writers explicitly; normalizing here
	// keeps the App contract safe for other callers and tests too.
	if app.Renderer != nil && app.Renderer.output != nil {
		app.Out = app.Renderer.output.Stdout()
		app.Errw = app.Renderer.output.Stderr()
	}
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
	if reader.editor != nil {
		reader.editor.skillNames = sortedSkillNames(app.Skills)
	}
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
	// Serialize completed reader delivery with prompt completion publication. If
	// a read is delivered first, the promptDone branch can always drain it before
	// scheduling an autonomous continuation, even when select chose promptDone.
	var promptBoundary sync.Mutex
	deliverInput := func(result replReadResult) {
		promptBoundary.Lock()
		inputs <- result
		if app.onInputDelivered != nil {
			app.onInputDelivered()
		}
		promptBoundary.Unlock()
	}
	go func() {
		for req := range readReq {
			input, ok, err := reader.read(req)
			deliverInput(replReadResult{input: input, ok: ok, err: err})
			if !ok {
				break
			}
		}
		// The reader has ended (EOF or a terminal read error). Keep draining
		// readReq and replying with an inert ended result so a requestRead that
		// races the end of input does not block forever on an orphaned channel.
		// The main loop treats ended results as no-ops and exits via inputEnded.
		for range readReq {
			deliverInput(replReadResult{input: replInput{ended: true}})
		}
	}()
	defer close(readReq)

	var (
		promptPrinted                bool
		readPending                  bool
		inputEnded                   bool
		inputErr                     error
		active                       bool
		activeReadPause              bool
		plainPromptRead              bool
		prompt                       string
		pendingPrefill               string // text deposited into the next prompt
		pendingPrefillModelPrompt    bool   // submitted prefill bypasses command/shell dispatch
		pendingPrefillPasted         bool   // retained pure-paste classification across boundaries
		pendingPrefillPasteSummaries []pasteSummary
		queued                       []replInput
		preparedQueued               []agent.SteerInput
		promptDone                   <-chan struct{}
		restoreEsc                   func() error
		escPresses                   escapePresses
		pendingShiftTabPrewarm       <-chan time.Time
		pendingIdleCompaction        <-chan time.Time
		idleCompactionDone           <-chan idleCompactionFinished
		cancelIdleCompactionWork     context.CancelFunc
		idleCompactionDiscard        bool
		idleCompactionModelKey       string
		idleCompactionStarted        time.Time
		idleCompactionTrigger        int
		idleCompactionContext        int
		idleCompactionMessages       int
		pendingGoalCheckpoint        bool
	)
	var goalChanges <-chan struct{}
	if app.Goal != nil {
		goalChanges = app.Goal.Changes()
	}

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
		compactionsBefore := app.Agent.CompactionCount()
		applied, err := app.Agent.ApplyIdleCompaction(context.Background(), sink, finished.result)
		app.addCompactions(app.Agent.CompactionCount() - compactionsBefore)
		sink.FlushEvents()
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
		app.lastPromptInterrupted = false
		active = true
		app.promptActive = true
		plainPromptRead = false
		promptPrinted = false
		escPresses.reset()
		if usePromptEditor {
			// Keep the terminal in raw/echo-off mode for the whole prompt so typed
			// keystrokes feed the live during-prompt input line instead of garbling
			// scrolling output. Leave bracketed paste enabled: active-turn input uses
			// the same explicit paste parser and timing fallback as the idle prompt.
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
			promptBoundary.Lock()
			done <- struct{}{}
			promptBoundary.Unlock()
		}()
	}
	startPromptRun := func(ctx context.Context, prompt string, resolveSkillMentions, attachPromptImages, goalPrompt, goalContinuation bool, goalRevision uint64) bool {
		admissionOK := true
		opts := promptOptions{
			resolveSkillMentions: resolveSkillMentions,
			attachPromptImages:   attachPromptImages,
			preflightContext:     ctx,
		}
		if goalPrompt {
			opts.beforeBegin = func(begin func() bool) (uint64, uint64, bool) {
				if app.Goal == nil {
					admissionOK = false
					return 0, 0, false
				}
				count, admittedRevision, generation, admitted, capped := app.Goal.AdmitPrompt(goalRevision, app.GoalMaxContinuations, goalContinuation, begin)
				admissionOK = admitted
				if capped {
					app.goalContinuationCapped(count)
				} else if admitted && goalContinuation {
					app.saveOrWarn(app.SessionPath)
				}
				return admittedRevision, generation, admitted
			}
		}
		run, ok := app.preparePromptRun(prompt, opts)
		if !ok {
			if goalPrompt && admissionOK && (ctx == nil || ctx.Err() == nil) {
				app.pauseGoalAfterRejectedPrompt(goalRevision)
			}
			return false
		}
		startRun(run)
		return true
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
	startPromptInteraction := func(prompt string, resolveSkillMentions, attachPromptImages, goalPrompt, goalContinuation bool, goalRevision uint64) (exit bool, code int) {
		interruptionRevision, ownsActiveGoal := goalRevision, goalPrompt
		if !ownsActiveGoal && app.Goal != nil && app.GoalAutoContinue {
			interruptionRevision, ownsActiveGoal = app.Goal.ActiveRevisionSnapshot()
		}
		cancelShiftTabPrewarm()
		if app.Renderer != nil {
			app.Renderer.SubmittedPromptSeparator()
			app.Renderer.StartPrompt()
		}
		ctx, cancel, interrupted := exitContext()
		err := app.refreshMCP(ctx)
		if interrupted() || errors.Is(err, context.Canceled) {
			cancel()
			if ownsActiveGoal {
				app.pauseGoalAfterInterruption(interruptionRevision)
			}
			return true, ExitInterrupt
		}
		if errors.Is(err, context.DeadlineExceeded) {
			cancel()
			return true, ExitInterrupt
		}
		started := startPromptRun(ctx, prompt, resolveSkillMentions, attachPromptImages, goalPrompt, goalContinuation, goalRevision)
		wasInterrupted := interrupted() || errors.Is(ctx.Err(), context.Canceled)
		deadlineExceeded := errors.Is(ctx.Err(), context.DeadlineExceeded)
		cancel()
		if !started && (wasInterrupted || deadlineExceeded) {
			if wasInterrupted && ownsActiveGoal {
				app.pauseGoalAfterInterruption(interruptionRevision)
			}
			return true, ExitInterrupt
		}
		return false, ExitOK
	}
	startDetachedWaitContinuation := func() (exit bool, code int) {
		cancelShiftTabPrewarm()
		if app.Renderer != nil {
			app.Renderer.SubmittedPromptSeparator()
			app.Renderer.StartPrompt()
		}
		ctx, cancel, interrupted := exitContext()
		err := app.refreshMCP(ctx)
		if interrupted() || errors.Is(err, context.Canceled) {
			cancel()
			return true, ExitInterrupt
		}
		if errors.Is(err, context.DeadlineExceeded) {
			cancel()
			return true, ExitInterrupt
		}
		cancel()
		run, ok := app.prepareDetachedWaitContinuation()
		if !ok {
			return false, ExitOK
		}
		startRun(run)
		return false, ExitOK
	}
	startAPIContinuation := func() (exit bool, code int) {
		cancelShiftTabPrewarm()
		if app.Renderer != nil {
			app.Renderer.SubmittedPromptSeparator()
			app.Renderer.StartPrompt()
		}
		ctx, cancel, interrupted := exitContext()
		err := app.refreshMCP(ctx)
		if interrupted() || errors.Is(err, context.Canceled) {
			cancel()
			return true, ExitInterrupt
		}
		if errors.Is(err, context.DeadlineExceeded) {
			cancel()
			return true, ExitInterrupt
		}
		cancel()
		run, ok := app.prepareAPIContinuation()
		if !ok {
			return false, ExitOK
		}
		startRun(run)
		return false, ExitOK
	}
	startPreparedPromptInteraction := func(input agent.SteerInput) (exit bool, code int) {
		interruptionRevision, ownsActiveGoal := uint64(0), false
		if app.Goal != nil && app.GoalAutoContinue {
			interruptionRevision, ownsActiveGoal = app.Goal.ActiveRevisionSnapshot()
		}
		cancelShiftTabPrewarm()
		if app.Renderer != nil {
			app.echoEditedPrompt(prompt, input.Text)
			app.Renderer.SubmittedPromptSeparator()
			app.Renderer.StartPrompt()
		}
		ctx, cancel, interrupted := exitContext()
		err := app.refreshMCP(ctx)
		cancel()
		if interrupted() || errors.Is(err, context.Canceled) {
			if ownsActiveGoal {
				app.pauseGoalAfterInterruption(interruptionRevision)
			}
			return true, ExitInterrupt
		}
		if errors.Is(err, context.DeadlineExceeded) {
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
			pendingPrefillPasteSummaries = clonePasteSummaries(input.pasteSummaries)
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
				pendingPrefillPasteSummaries = nil
			} else {
				// Without the prompt editor there is no way to prefill for
				// review; echo the loaded text and submit it directly,
				// bypassing command and shell dispatch while preserving normal
				// prompt enrichment for editor output.
				app.echoEditedPrompt(prompt, action.prefill)
				return startPromptInteraction(action.prefill, true, true, false, false, 0)
			}
			return false, ExitOK
		}
		if action.continueAfterAPIError {
			return startAPIContinuation()
		}
		if action.run {
			if input.echoWhenDequeued || action.echoEditedPrompt {
				app.echoEditedPrompt(prompt, action.prompt)
			}
			return startPromptInteraction(action.prompt, action.resolveSkillMentions, action.attachPromptImages, action.goalPrompt, action.goalContinuation, action.goalRevision)
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
	// reclaimIdleEditorForAutonomous uses the same handoff as goal
	// auto-continuation: a delivered line wins, a non-empty draft becomes editable
	// prefill, and only an empty canceled editor read allows host-created work.
	reclaimIdleEditorForAutonomous := func() (retry bool, exit bool, code int) {
		if !usePromptEditor || !readPending {
			return false, false, ExitOK
		}
		reader.cancelPromptRead()
		res := <-inputs
		readPending = false
		reader.drainPromptCancel()
		switch {
		case res.input.ended:
			inputEnded = true
			return true, false, ExitOK
		case !res.ok:
			setInputEnded(res.err)
			return true, false, ExitOK
		case res.input.deposit:
			promptPrinted = false
			pendingPrefill = res.input.text
			pendingPrefillModelPrompt = false
			pendingPrefillPasted = res.input.pasted
			pendingPrefillPasteSummaries = clonePasteSummaries(res.input.pasteSummaries)
			return pendingPrefill != "", false, ExitOK
		default:
			if exit, code := handleIdleReadResult(res); exit {
				return true, true, code
			}
			return true, false, ExitOK
		}
	}

	if initialPrompt != nil {
		interruptionRevision, ownsActiveGoal := uint64(0), false
		if app.Goal != nil && app.GoalAutoContinue {
			interruptionRevision, ownsActiveGoal = app.Goal.ActiveRevisionSnapshot()
		}
		if app.Renderer != nil {
			app.Renderer.StartPrompt()
		}
		ctx, cancel, interrupted := exitContext()
		err := app.refreshMCP(ctx)
		if interrupted() || errors.Is(err, context.Canceled) {
			cancel()
			if ownsActiveGoal {
				app.pauseGoalAfterInterruption(interruptionRevision)
			}
			return finish(ExitInterrupt)
		}
		if errors.Is(err, context.DeadlineExceeded) {
			cancel()
			return finish(ExitInterrupt)
		}
		started := startPromptRun(ctx, *initialPrompt, true, true, false, false, 0)
		wasInterrupted := interrupted() || errors.Is(ctx.Err(), context.Canceled)
		deadlineExceeded := errors.Is(ctx.Err(), context.DeadlineExceeded)
		cancel()
		if !started && (wasInterrupted || deadlineExceeded) {
			if wasInterrupted && ownsActiveGoal {
				app.pauseGoalAfterInterruption(interruptionRevision)
			}
			return finish(ExitInterrupt)
		}
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
			case <-goalChanges:
				// Prompt execution owns transcript/session state, so defer the
				// checkpoint until that owner publishes completion.
				pendingGoalCheckpoint = true
			case <-promptDone:
				if app.Renderer != nil {
					app.Renderer.StopProgress()
				}
				if usePromptEditor {
					// Release the blocked during-prompt keystroke read so any
					// unsubmitted partial buffer becomes the next prompt's editable
					// prefill. A line already submitted with Enter is queued below.
					// The terminal stays in raw mode and bracketed paste remains enabled
					// for the line editor.
					if readPending {
						reader.cancelPromptRead()
						res := <-inputs
						readPending = false
						if res.ok && res.input.deposit {
							pendingPrefill = res.input.text
							pendingPrefillModelPrompt = false
							pendingPrefillPasted = res.input.pasted
							pendingPrefillPasteSummaries = clonePasteSummaries(res.input.pasteSummaries)
						} else if res.ok && !res.input.escape && !res.input.interrupt && (res.input.text != "" || res.input.edit) {
							res.input = app.markQueuedSubmission(res.input, 0)
							queued = append(queued, res.input)
						} else {
							// The read returned via a keystroke (interrupt/Esc)
							// rather than the cancel; the typed buffer is intact.
							buffered := reader.promptBufferInput()
							pendingPrefill = buffered.text
							pendingPrefillModelPrompt = false
							pendingPrefillPasted = buffered.pasted
							pendingPrefillPasteSummaries = clonePasteSummaries(buffered.pasteSummaries)
						}
						reader.drainPromptCancel()
					}
					// When no read is pending an EOF-driven deposit was already
					// stashed in pendingPrefill via the active inputs case; leave
					// it as-is.
				} else {
					disableActivePromptTerm()
				}
				active = false
				activeReadPause = false
				promptDone = nil
				app.promptActive = false
				if pendingGoalCheckpoint {
					app.saveOrWarn(app.SessionPath)
					pendingGoalCheckpoint = false
				}
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
				if !app.lastPromptInterrupted {
					app.pauseGoalAtContinuationCap()
				}
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
						if exit, code := startPromptInteraction(implementationStartPrompt, true, false, false, false, 0); exit {
							return finish(code)
						}
						continue
					}
					if approvalInterrupted {
						return finish(ExitInterrupt)
					}
				}
				// Prefer a line the user submitted while the prompt was finishing
				// over autonomous continuation, even when promptDone won the select
				// race. A still-blocked read does not suppress goal work.
				if !usePromptEditor && readPending {
					select {
					case res := <-inputs:
						readPending = false
						switch {
						case res.input.ended:
							inputEnded = true
						case !res.ok:
							setInputEnded(res.err)
						default:
							res.input = app.markQueuedSubmission(res.input, 0)
							queued = append(queued, res.input)
						}
					default:
					}
				}
				if !usePromptEditor && readPending {
					// A plain read started during the prompt is still
					// blocked. Let it collect the next line in canonical mode;
					// starting the raw prompt editor now would leave no prompt
					// drawn and no terminal echo until that stale read finishes.
					plainPromptRead = true
				} else if !usePromptEditor {
					plainPromptRead = false
					enableIdlePromptTerm()
				}
				// A resolved detached wait gets the first autonomous slot after all
				// user input, recovered steer, drafts, and handoff work. Its outcome is
				// still consumed only by the next model request's request-context drain.
				if !app.lastPromptInterrupted && !inputEnded && len(queued) == 0 && len(preparedQueued) == 0 && pendingPrefill == "" && !app.hasPendingHandoffRequest() && app.Background != nil && app.Background.DetachedWaitPending() {
					if exit, code := startDetachedWaitContinuation(); exit {
						return finish(code)
					}
					continue
				}
				// Autonomous goal continuation: after a non-interrupted prompt, if
				// there is no queued user input and an active goal remains, queue the
				// next continuation prompt to run as a normal user turn.
				if !app.lastPromptInterrupted && !inputEnded && len(queued) == 0 && len(preparedQueued) == 0 && pendingPrefill == "" {
					if cont, ok := app.goalContinuationReady(); ok {
						queued = append(queued, replInput{text: cont.Text, modelPrompt: true, goalPrompt: true, goalContinuation: true, goalRevision: cont.Revision})
						continue
					}
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
					pendingPrefillPasted = input.pasted
					pendingPrefillPasteSummaries = clonePasteSummaries(input.pasteSummaries)
					activeReadPause = true
					continue
				}
				if input.escapeTail {
					escPresses.reset()
					continue
				}
				if input.escape {
					if input.text != "" {
						queued = append(queued, app.markQueuedSubmission(replInput{text: input.text}, 0))
					}
					if escPresses.press(app.clock()()) && app.Interrupt != nil {
						app.Interrupt.CancelPrompt()
					}
					continue
				}
				escPresses.reset()
				// Steer gate: never steer while earlier input still waits in
				// either queue, or a later accepted steer would recover ahead
				// of it at prompt completion. Queueing keeps submission order
				// by construction.
				disposition := steerNotModelBound
				var prepared *agent.SteerInput
				if len(queued) == 0 && len(preparedQueued) == 0 {
					disposition, prepared = app.steerDuringPrompt(input)
				}
				if prepared != nil {
					preparedQueued = append(preparedQueued, *prepared)
				}
				switch disposition {
				case steerAccepted:
					app.submissionIndicator("steer queued", input.text, 0)
					continue
				case steerPrepRejected:
					continue
				case steerQueuedPrepared:
					app.submissionIndicator("queued for next prompt", input.text, len(prepared.Images))
					continue
				}
				input = app.markQueuedSubmission(input, 0)
				queued = append(queued, input)
			}
			continue
		}

		// Prefer a line that the reader has already delivered over autonomous
		// continuation at every idle boundary, including one woken by a shared
		// child-agent goal transition.
		if !inputEnded && readPending && len(queued) == 0 && len(preparedQueued) == 0 && pendingPrefill == "" {
			select {
			case res := <-inputs:
				if exit, code := handleIdleReadResult(res); exit {
					return finish(code)
				}
				continue
			default:
			}
		}

		// Detached wait outcomes outrank goal auto-continuation but remain below all
		// already-delivered user work. Reclaim a raw editor read exactly as goal work
		// does, preserving any non-empty draft for the user.
		if !app.lastPromptInterrupted && !inputEnded && len(queued) == 0 && len(preparedQueued) == 0 && pendingPrefill == "" && !app.hasPendingHandoffRequest() && app.Background != nil && app.Background.DetachedWaitPending() {
			retry, exit, code := reclaimIdleEditorForAutonomous()
			if exit {
				return finish(code)
			}
			if retry {
				continue
			}
			if app.Background.DetachedWaitPending() {
				if exit, code := startDetachedWaitContinuation(); exit {
					return finish(code)
				}
				continue
			}
		}

		// An active goal continues at every idle boundary before waiting for fresh
		// input. This starts restored goals. It shares the raw-editor reclaim path
		// above so delivered input and editable drafts retain their existing priority.
		if !inputEnded && len(queued) == 0 && len(preparedQueued) == 0 && pendingPrefill == "" {
			if cont, ok := app.goalContinuationReady(); ok {
				retry, exit, code := reclaimIdleEditorForAutonomous()
				if exit {
					return finish(code)
				}
				if retry {
					continue
				}
				queued = append(queued, replInput{text: cont.Text, modelPrompt: true, goalPrompt: true, goalContinuation: true, goalRevision: cont.Revision})
			}
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
				app.printTodoStatus(false)
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
			requestRead(replReadRequest{prompt: prompt, promptEditor: usePromptEditor, replPrompt: true, prefill: pendingPrefill, prefillModelPrompt: pendingPrefillModelPrompt, prefillPasted: pendingPrefillPasted, prefillPasteSummaries: pendingPrefillPasteSummaries})
			pendingPrefill = ""
			pendingPrefillModelPrompt = false
			pendingPrefillPasted = false
			pendingPrefillPasteSummaries = nil
		}
		var detachedWaitReady <-chan struct{}
		if !app.lastPromptInterrupted && !inputEnded && len(queued) == 0 && len(preparedQueued) == 0 && pendingPrefill == "" && !app.hasPendingHandoffRequest() && app.Background != nil {
			// Subscribe before an observer publishes an outcome. The manager closes
			// this open channel when a detached wait resolves; checking pending here
			// would miss that transition while the REPL is otherwise idle.
			detachedWaitReady = app.Background.DetachedWaitReady()
		}
		select {
		case <-exit:
			// SIGINT exit request at the idle prompt (design §8.4).
			return finish(ExitInterrupt)
		case <-pendingIdleCompaction:
			startIdleCompaction()
		case <-detachedWaitReady:
			// The signal is level-triggered. Do not drain here; loop through the
			// user/draft priority checks and let the next model request own context
			// consumption.
			cancelIdleCompaction()
		case <-goalChanges:
			// Shared child-agent tools can transition the root goal while the
			// REPL is idle. Persist the transition and wake the idle boundary,
			// while still preferring a user line already delivered by the reader.
			cancelIdleCompaction()
			app.saveOrWarn(app.SessionPath)
			select {
			case res := <-inputs:
				if exit, code := handleIdleReadResult(res); exit {
					return finish(code)
				}
			default:
				promptPrinted = false
			}
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
	var contextUsed, contextTotal int
	if t.Uses("context") || t.Uses("context_pct_used") || t.Uses("context_tokens_used") || t.Uses("context_tokens_total") {
		if app.Agent != nil {
			est := app.Agent.EstimateContext()
			contextUsed = sessionrec.ContextUsed(est)
			contextTotal = est.Window
		}
	}
	return replprompt.Values{
		Agent:              app.AgentName,
		CWD:                cwd,
		Hostname:           hostname,
		GitBranch:          gitBranch,
		Model:              app.Model,
		Reasoning:          app.promptReasoningLabel(),
		ViMode:             viMode,
		ContextTokensUsed:  contextUsed,
		ContextTokensTotal: contextTotal,
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
	text           string
	pasted         bool
	pasteSummaries []pasteSummary
	edit           bool
	cycleAgent     bool
	escape         bool
	escapeTail     bool
	interrupt      bool
	interactive    bool
	// echoWhenDequeued marks input the user submitted while another prompt was
	// active. If it later starts a model prompt from the queue, replay that prompt
	// line before the separator. Host-created goal continuations share the queue
	// but deliberately leave this false.
	echoWhenDequeued bool
	// modelPrompt marks input already classified as model-bound prompt text. It
	// bypasses prompt-level command and shell dispatch while preserving normal
	// prompt enrichment.
	modelPrompt bool
	// goalPrompt marks rendered goal-driving input so rejection pauses the goal.
	goalPrompt bool
	// goalContinuation marks an autonomous goal prompt. Its continuation count is
	// consumed only after prompt hooks and other admission checks succeed.
	goalContinuation bool
	// goalRevision binds goal-driving input to the exact state it was rendered from.
	goalRevision uint64
	// deposit marks an accumulated buffer that did not end with Enter. During a
	// prompt, or when an autonomous continuation reclaims an idle editor read, it
	// is handed back as editable prefill in the next prompt.
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
	// prefillPasted and prefillPasteSummaries retain paste classification and
	// collapsed display ranges across active-turn deposits and Shift-Tab.
	prefillPasted         bool
	prefillPasteSummaries []pasteSummary
}

type replAction struct {
	prompt                string
	run                   bool
	exit                  bool
	shell                 bool
	shellCommand          string
	echoEditedPrompt      bool
	resolveSkillMentions  bool
	attachPromptImages    bool
	goalPrompt            bool
	goalContinuation      bool
	goalRevision          uint64
	continueAfterAPIError bool
	// prefill deposits text into the next prompt as editable content instead
	// of running a turn. Used when returning from an external editor so the
	// user can review before submitting.
	prefill    string
	prefillSet bool
	// prefillModelPrompt marks the eventual submitted prefill as model-bound text.
	prefillModelPrompt bool
}

type replCommandResult struct {
	exit                  bool
	prompt                string
	prefill               string
	prefillSet            bool
	resolveSkillMentions  bool
	attachPromptImages    bool
	goalPrompt            bool
	goalRevision          uint64
	continueAfterAPIError bool
}

const (
	implementationStartPrompt          = "Continue the active approved work now."
	detachedBackgroundWaitCause        = "detached_background_wait"
	detachedBackgroundWaitContinuation = "A detached `background_jobs` wait has resolved. Review its result in request context and continue the task."
)

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

// steerAccepted invokes the configured steering callback and broadcasts only a
// successful user steer to blocked background waits. Keeping this at the UI
// boundary excludes agent-generated steering and rejected/queued input.
func (app *App) steerAccepted(input agent.SteerInput) bool {
	if app.Steer == nil || !app.Steer(input) {
		return false
	}
	if app.Background != nil {
		app.Background.NotifyAcceptedSteer()
	}
	return true
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

// steerDisposition reports how steerDuringPrompt disposed of a during-prompt
// submission so the run loop can word a submission indicator without conflating
// accepted steering with queueing.
type steerDisposition int

const (
	// steerNotModelBound marks input the caller must queue as-is: shell escapes,
	// /commands, /edit requests, empty text, or any input when steering is
	// disabled or the steer gate is closed.
	steerNotModelBound steerDisposition = iota
	// steerAccepted marks input handed to the agent steer queue. This reports
	// submission only; the steer may still be recovered as the next prompt if
	// the running prompt ends before a tool round injects it.
	steerAccepted
	// steerPrepRejected marks input consumed but dropped during preparation
	// (a hook rejected it or prompt preparation failed). The input is handled;
	// the caller queues nothing and prints no indicator.
	steerPrepRejected
	// steerQueuedPrepared marks a prepared model-bound input the bounded steer
	// queue refused; the caller appends the returned SteerInput to
	// preparedQueued so it runs as the next prompt without repeating hooks.
	steerQueuedPrepared
)

// steerDuringPrompt routes a during-prompt-submitted input into the agent as a
// in-prompt steering message when steering is enabled and the input is
// model-bound (would start a prompt at idle), reporting the disposition. For
// steerQueuedPrepared the returned input contains that exact model-bound
// content so the caller can run it later without repeating hooks or losing
// consumed images/context. steerNotModelBound input (shell escapes, /commands,
// /edit requests) and any input when Steer is nil is left for the caller to
// queue unchanged. The classification mirrors handlePromptInput's prefix
// dispatch but performs no command side effects.
func (app *App) steerDuringPrompt(input replInput) (steerDisposition, *agent.SteerInput) {
	if app.Steer == nil {
		return steerNotModelBound, nil
	}
	if input.escape || input.interrupt || input.deposit || input.edit {
		return steerNotModelBound, nil
	}
	line := input.text
	if line == "" {
		return steerNotModelBound, nil
	}
	if !input.interactive && !input.pasted {
		return steerNotModelBound, nil
	}
	prepareAndSteer := func(text string, opts promptOptions) (steerDisposition, *agent.SteerInput) {
		steered, err := app.prepareSteerInput(text, opts)
		if err != nil {
			return steerPrepRejected, nil
		}
		if app.steerAccepted(steered) {
			return steerAccepted, nil
		}
		return steerQueuedPrepared, &steered
	}
	if input.pasted {
		return prepareAndSteer(line, promptOptions{})
	}
	if input.interactive {
		// Mirror handlePromptInput's escape-prefix stripping so a steered !!foo
		// or //foo reaches the model as !foo / /foo, exactly as it would at the
		// idle prompt.
		if strings.HasPrefix(line, "!!") || strings.HasPrefix(line, "//") {
			return prepareAndSteer(line[1:], promptOptions{resolveSkillMentions: true, attachPromptImages: true})
		}
		// !shell escapes and /commands (including /edit) are not model input —
		// leave them queued for the idle prompt.
		if strings.HasPrefix(line, "!") || strings.HasPrefix(line, "/") {
			return steerNotModelBound, nil
		}
	}
	return prepareAndSteer(line, promptOptions{resolveSkillMentions: true, attachPromptImages: true})
}

// submissionIndicator prints a dim one-line notice above the live status line
// reporting how a during-prompt submission was disposed. The wording never
// promises delivery: a queued steer is still recovered as the next prompt when
// the running prompt ends before a tool round injects it.
func (app *App) submissionIndicator(disposition, text string, images int) {
	if app.Renderer == nil {
		return
	}
	app.Renderer.Notice("[" + disposition + ": " + submissionPreview(text, images) + "]")
}

// markQueuedSubmission records the immediate disposition of user input and
// marks it for a normal prompt-line echo if it later starts a model request.
// Keeping both effects together prevents prompt-boundary recovery paths from
// silently queueing input without the notice shown by the ordinary active-read
// path.
func (app *App) markQueuedSubmission(input replInput, images int) replInput {
	app.submissionIndicator("queued for next prompt", input.text, images)
	input.echoWhenDequeued = true
	return input
}

// submissionPreview collapses a submission to its first whitespace-normalized
// line, truncated with an ellipsis, and notes any attached images.
func submissionPreview(text string, images int) string {
	line, _, _ := strings.Cut(text, "\n")
	line = strings.Join(strings.Fields(line), " ")
	const maxPreview = 72
	runes := []rune(line)
	if len(runes) > maxPreview {
		line = string(runes[:maxPreview-1]) + "…"
	}
	if images > 0 {
		plural := ""
		if images > 1 {
			plural = "s"
		}
		line += fmt.Sprintf(" (+%d image%s)", images, plural)
	}
	return line
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
		return replAction{prompt: line, run: true, resolveSkillMentions: true, attachPromptImages: true, goalPrompt: input.goalPrompt, goalContinuation: input.goalContinuation, goalRevision: input.goalRevision}
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
		if result.continueAfterAPIError {
			return replAction{continueAfterAPIError: true}
		}
		if result.prompt != "" {
			return replAction{prompt: result.prompt, run: true, resolveSkillMentions: result.resolveSkillMentions, attachPromptImages: result.attachPromptImages, goalPrompt: result.goalPrompt, goalRevision: result.goalRevision}
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
	// prompt start; onPromptInput renders the live display buffer and cursor, and cancelable
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
		input, ok, err := rr.editor.readPrefilledWithPasteState(req.prompt, req.prefill, req.prefillPasted, req.prefillPasteSummaries)
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
	rr.editor.resetPasteTracking(rr.promptState, false)
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
		result, err := rr.editor.readKey(&rr.promptVi, s, &rr.promptHistory, "", true)
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
	input := rr.promptBufferInput()
	input.deposit = true
	rr.resetPromptBuffer()
	return input
}

// resetPromptBuffer clears the during-prompt buffer and cursor and emits an empty
// status line so a closed-out read leaves no stale input painted.
func (rr *replReader) resetPromptBuffer() {
	if rr.promptState != nil {
		rr.promptState.buf = nil
		rr.promptState.cursor = 0
		rr.promptState.clearPasteSummaries()
		rr.editor.resetPasteTracking(rr.promptState, false)
	}
	rr.emitPromptInput()
}

func (rr *replReader) emitPromptInput() {
	if rr.onPromptInput != nil {
		if rr.promptState != nil {
			rr.onPromptInput(string(rr.promptState.displayRunes()), rr.promptState.displayCursor())
			return
		}
		rr.onPromptInput("", 0)
	}
}

// cancelPromptRead releases a blocked prompt read so it deposits its buffer; a
// no-op without a cancelable reader.
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

// promptBufferInput returns the current during-prompt buffer and its paste
// display/classification metadata without consuming it.
func (rr *replReader) promptBufferInput() replInput {
	if rr.promptState == nil {
		return replInput{}
	}
	return replInput{
		text:           string(rr.promptState.buf),
		pasted:         rr.editor.purePaste,
		pasteSummaries: clonePasteSummaries(rr.promptState.pasteSummaries),
	}
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
	case "/continue":
		if arg != "" {
			fmt.Fprintln(app.Errw, "usage: /continue")
		} else if !app.apiContinuationAvailable() {
			fmt.Fprintln(app.Errw, "[nothing to continue; the last prompt did not end with an API error]")
		} else {
			return replCommandResult{continueAfterAPIError: true}
		}
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
	case "/prompt":
		fmt.Fprintln(app.Errw, app.System)
	case "/usage":
		fmt.Fprintln(app.Errw, app.usageSummary())
	case "/max-turns":
		app.maxTurnsCommand(arg)
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
	case "/goal":
		return app.goalCommand(arg)
	case "/skills":
		fmt.Fprintln(app.Errw, app.skillsSummary())
	case "/tools":
		app.toolsCommand(arg)
	case "/lsp":
		app.lspCommand(arg)
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

// goalCommand handles /goal show/clear/pause/resume/set.
func (app *App) goalCommand(arg string) replCommandResult {
	if app.Goal == nil {
		fmt.Fprintln(app.Errw, "[goals unavailable]")
		return replCommandResult{}
	}
	if !app.GoalAutoContinue {
		fmt.Fprintln(app.Errw, "[goals are only available in interactive sessions]")
		return replCommandResult{}
	}
	arg = strings.TrimSpace(arg)
	switch arg {
	case "":
		app.showGoalStatus()
	case "clear":
		if app.Goal.Snapshot() == nil {
			fmt.Fprintln(app.Errw, "[no goal set]")
		} else {
			app.Goal.Clear()
			fmt.Fprintln(app.Errw, "[goal cleared]")
			app.saveOrWarn(app.SessionPath)
		}
	case "pause":
		if app.Goal.Pause() {
			fmt.Fprintln(app.Errw, "[goal paused]")
			app.saveOrWarn(app.SessionPath)
		} else {
			fmt.Fprintln(app.Errw, "[no active goal]")
		}
	case "resume":
		if app.Goal.Snapshot() == nil {
			fmt.Fprintln(app.Errw, "[no goal set]")
		} else if err := app.Goal.Resume(); err != nil {
			fmt.Fprintf(app.Errw, "[goal error: %v]\n", err)
		} else {
			fmt.Fprintln(app.Errw, "[goal resumed]")
			app.saveOrWarn(app.SessionPath)
			preview := app.Goal.ContinuationPreview(app.GoalMaxContinuations)
			return replCommandResult{prompt: preview.Text, goalPrompt: true, goalRevision: preview.Revision}
		}
	default:
		// Set a new objective. Exact-match subcommands above mean "/goal clear the
		// backlog" sets an objective rather than clearing.
		replaced := app.Goal.Snapshot() != nil
		if err := app.Goal.Set(arg); err != nil {
			fmt.Fprintf(app.Errw, "[goal error: %v]\n", err)
			return replCommandResult{}
		}
		if replaced {
			fmt.Fprintf(app.Errw, "[goal replaced: %s]\n", app.Goal.Objective())
		} else {
			fmt.Fprintf(app.Errw, "[goal set: %s]\n", app.Goal.Objective())
		}
		app.saveOrWarn(app.SessionPath)
		preview := app.Goal.ContinuationPreview(app.GoalMaxContinuations)
		return replCommandResult{prompt: preview.Text, goalPrompt: true, goalRevision: preview.Revision}
	}
	return replCommandResult{}
}

func (app *App) showGoalStatus() {
	state := app.Goal.Snapshot()
	if state == nil {
		fmt.Fprintln(app.Errw, "[no goal set]")
		return
	}
	continuations := fmt.Sprintf("%d", state.Continuations)
	if app.GoalMaxContinuations > 0 {
		continuations += fmt.Sprintf("/%d", app.GoalMaxContinuations)
	}
	elapsed := app.clock()().Sub(state.SetAt).Round(time.Second)
	if elapsed < 0 {
		elapsed = 0
	}
	fmt.Fprintf(app.Errw, "goal: %s\nobjective: %s\ncontinuations: %s\nelapsed: %s\n", state.Status, state.Objective, continuations, elapsed)
}

func (app *App) goalOnPromptEnd(ctx context.Context, err error, revision uint64, ownedActiveGoal bool) {
	app.lastPromptInterrupted = errors.Is(err, context.Canceled)
	if !app.lastPromptInterrupted || app.Goal == nil {
		return
	}
	paused := false
	if generation, ok := goal.GenerationFromContext(ctx, app.Goal); ok {
		paused = app.Goal.PauseActiveGeneration(generation)
	} else if ownedActiveGoal {
		paused = app.Goal.PauseActiveRevision(revision)
	}
	if paused {
		fmt.Fprintln(app.Errw, "[goal paused; /goal resume to continue]")
	}
}

// goalContinuationReady reports whether the REPL should auto-continue an active
// goal at the current idle boundary. It returns the continuation prompt and
// true when a continuation should run. It applies the safety cap and pauses the
// goal when the cap is reached.
func (app *App) goalContinuationReady() (goal.PromptPreview, bool) {
	if app.Goal == nil || !app.GoalAutoContinue || !app.Goal.Active() {
		return goal.PromptPreview{}, false
	}
	if app.pauseGoalAtContinuationCap() {
		return goal.PromptPreview{}, false
	}
	preview := app.Goal.NextContinuationPreview(app.GoalMaxContinuations)
	return preview, preview.Text != ""
}

func (app *App) pauseGoalAfterRejectedPrompt(revision uint64) {
	if app.Goal == nil || !app.Goal.PauseActiveRevision(revision) {
		return
	}
	fmt.Fprintln(app.Errw, "[goal paused because its continuation prompt was rejected; /goal resume to continue]")
	app.saveOrWarn(app.SessionPath)
}

func (app *App) pauseGoalAfterInterruption(revision uint64) {
	if app.Goal == nil || !app.Goal.PauseActiveRevision(revision) {
		return
	}
	fmt.Fprintln(app.Errw, "[goal paused; /goal resume to continue]")
	app.saveOrWarn(app.SessionPath)
}

func (app *App) goalContinuationCapped(continuations int) {
	fmt.Fprintf(app.Errw, "[goal paused after %d continuations; /goal resume to continue]\n", continuations)
	app.saveOrWarn(app.SessionPath)
}

func (app *App) pauseGoalAtContinuationCap() bool {
	if app.Goal == nil {
		return false
	}
	continuations, paused := app.Goal.PauseAtContinuationCap(app.GoalMaxContinuations)
	if !paused {
		return false
	}
	app.goalContinuationCapped(continuations)
	return true
}

// knownCommands is the meta-command vocabulary used for "did you mean …?"
// suggestions on an unknown command (r59).
var knownCommands = []string{
	"/help", "/exit", "/quit", "/clear", "/continue", "/compact", "/tree", "/fork", "/clone", "/context", "/prompt", "/usage",
	"/max-turns", "/tools", "/lsp", "/image", "/edit", "/save", "/model", "/reasoning", "/effort", "/fast",
	"/agent", "/mode", "/plan", "/auto", "/handoff", "/background", "/goal", "/skills", "/vi",
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
	if ctx := app.todoRequestContext(); ctx != "" {
		out = append(out, ctx)
	}
	if ctx := app.goalRequestContext(); ctx != "" {
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
	if selection.ReasoningReplayDomain == "" {
		selection.ReasoningReplayDomain = selection.BaseTargetID
	}
	baseChanged := oldBaseTargetID == "" || selection.BaseTargetID != oldBaseTargetID
	responseState := app.Agent.ResponseState()
	app.Agent.SetProvider(selection.Runtime)
	app.Agent.SetModel(selection.Model, selection.ContextWindow)
	app.Agent.SetReasoningReplayDomain(selection.ReasoningReplayDomain)
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
	app.refreshOTelIdentity()
	if app.Hooks != nil {
		app.Hooks.SetModel(app.Model)
	}
	app.BaseURL = selection.BaseURL
	app.Reasoning = selection.Reasoning
	app.BaseTargetID = selection.BaseTargetID
	app.ReasoningReplayDomain = selection.ReasoningReplayDomain
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

func (app *App) maxTurnsCommand(arg string) {
	if arg != "" {
		maxTurns, err := strconv.Atoi(arg)
		if err != nil {
			fmt.Fprintf(app.Errw, "[max-turns failed: %q is not an integer; <=0 means unlimited]\n", arg)
			return
		}
		app.Agent.SetMaxTurns(maxTurns)
	}
	if app.Agent.MaxTurns() <= 0 {
		fmt.Fprintln(app.Errw, "[max turns: unlimited]")
		return
	}
	fmt.Fprintf(app.Errw, "[max turns: %d]\n", app.Agent.MaxTurns())
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
	return app.applyAgentSwitchUsing(name, prewarm, app.SwitchAgent)
}

func (app *App) applyAgentSwitchUsing(name string, prewarm bool, switchAgent func(string) (AgentSelection, error)) error {
	if switchAgent == nil {
		return fmt.Errorf("agent switch unavailable")
	}
	oldProvider, oldModel := app.Provider, app.Model
	selection, err := switchAgent(name)
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
	if selection.ReasoningReplayDomain == "" {
		selection.ReasoningReplayDomain = selection.BaseTargetID
	}
	app.BaseTargetID = selection.BaseTargetID
	app.ReasoningReplayDomain = selection.ReasoningReplayDomain
	app.Agent.SetReasoningReplayDomain(selection.ReasoningReplayDomain)
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
	app.refreshOTelIdentity()
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

// prepareHandoff assembles a handoff request from any pending
// handoff tool request plus the given /handoff options: it
// validates the latest recorded plan and resolves the target agent. Failures are
// reported on app.Errw. handoffCommand (TTY
// approval) and the JSON run driver (protocol approval) share it.
func (app *App) prepareHandoff(arg string) (handoff.Request, bool) {
	if app.SwitchAgent == nil {
		fmt.Fprintln(app.Errw, "[handoff unavailable]")
		return handoff.Request{}, false
	}
	opts, err := parseHandoffCommandOptions(arg)
	if err != nil {
		fmt.Fprintf(app.Errw, "[handoff: %v; usage: %s]\n", err, handoffCommandUsage)
		return handoff.Request{}, false
	}
	var req handoff.Request
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
	if app.Plans == nil {
		fmt.Fprintln(app.Errw, "[handoff: record_plan must record a plan first]")
		return handoff.Request{}, false
	}
	latest, ok := app.Plans.Latest()
	if !ok {
		fmt.Fprintln(app.Errw, "[handoff: record_plan must record a plan first]")
		return handoff.Request{}, false
	}
	if req.PlanPath != "" && req.PlanPath != latest.Path {
		fmt.Fprintln(app.Errw, "[handoff: the recorded plan changed; request implementation again]")
		return handoff.Request{}, false
	}
	req.PlanPath = latest.Path
	target := req.Agent
	if target == "" {
		target = app.HandoffAgent
	}
	if target == "" {
		target = "auto"
	}
	req.Agent = target
	return req, true
}

// handoffCommand handles /handoff [-a agent] [-m model] [message]: hand off to
// an implementation agent to carry out the most recently recorded plan, after
// interactive approval. It consumes any request the handoff tool
// recorded, applies manual overrides and guidance, and
// switches with a clean, plan-seeded context.
func (app *App) handoffCommand(arg string, readLine func(string) (string, error)) bool {
	req, ok := app.prepareHandoff(arg)
	if !ok {
		return false
	}
	latest, _ := app.Plans.Latest()
	displayBrief := plan.Render(latest)
	if app.Renderer != nil {
		displayBrief = app.Renderer.FormatMarkdown(displayBrief)
	}
	fmt.Fprintf(app.Errw, "Implementation plan:\n%s\n", displayBrief)

	approval := fmt.Sprintf("Hand off to %q", req.Agent)
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
// planning transcript (recoverable), then seeds the complete approved plan.
// The switch is attempted before any destructive step so a failed switch leaves
// the session and recorded plan untouched.
func (app *App) handoffToImplementation(req handoff.Request) bool {
	switchAgent := app.HandoffSwitchAgent
	if switchAgent == nil {
		switchAgent = app.SwitchAgent
	}
	if err := app.applyAgentSwitchUsing(req.Agent, true, switchAgent); err != nil {
		fmt.Fprintf(app.Errw, "[handoff failed: %v]\n", err)
		return false
	}
	if req.Model != "" {
		if !app.switchModel(req.Model, app.Reasoning) {
			return false
		}
	}
	if app.SessionPath != "" {
		summary := "implementation handoff"
		if app.Plans != nil {
			if latest, ok := app.Plans.Latest(); ok {
				summary = "implementation handoff: " + latest.Title
			}
		}
		if _, err := session.SaveCompaction(app.SessionPath, session.Compaction{
			Time:     app.clock()(),
			Summary:  summary,
			Messages: app.Agent.Transcript(),
		}); err != nil {
			fmt.Fprintf(app.Errw, "[handoff: archive failed: %v]\n", err)
			return false
		}
	}
	if app.Plans == nil {
		fmt.Fprintln(app.Errw, "[handoff: plan store unavailable]")
		return false
	}
	latest, ok := app.Plans.Latest()
	if !ok || latest.Path != req.PlanPath {
		fmt.Fprintln(app.Errw, "[handoff failed: recorded plan changed]")
		return false
	}
	seed := "=== Implementation handoff ===\nImplement the complete approved plan below.\n\nRecorded plan: " + latest.Path + "\n\n" + plan.Render(latest)
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
	app.clearAPIContinuation()
	app.Agent.SetTranscript(seedMessages)
	app.Agent.SetResponseState(nil)
	if app.Todos != nil {
		app.Todos.Replace(nil)
	}
	app.saveOrWarn(app.SessionPath)
	fmt.Fprintf(app.Errw, "[handed off to %s; implementing the plan from a clean context seeded by %s]\n", req.Agent, latest.Path)
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
		app.recordEvent(session.Event{Type: session.EventNotice, Text: notice, Time: app.clock()()})
	}
	return nil
}

// clear resets the conversation and rotates to a fresh auto-save file (design
// §10, §11). Cumulative usage resets with the conversation.
func (app *App) clear() {
	created := app.clock()()
	path := session.DefaultPath(app.StateDir, created)
	if app.BeforeSessionPathChange != nil {
		if err := app.BeforeSessionPathChange(path); err != nil {
			fmt.Fprintf(app.Errw, "[clear failed: lock new session: %v]\n", err)
			return
		}
	}
	app.clearAPIContinuation()
	app.RecordOTelSession()
	if app.Background != nil {
		app.stopBackgroundJobs()
		app.saveOrWarn(app.SessionPath)
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
	if app.Goal != nil {
		app.Goal.Clear()
	}
	app.SetUsage(session.UsageTotals{})
	app.usageByModel = nil
	app.Created = created
	cwd, _ := os.Getwd()
	app.SessionTree = session.NewTree(app.Created, cwd, "", "")
	app.PromptNumber = 0
	app.todoPromptStatusBeforeUsage = false
	app.todoPromptStatusBeforeUsagePrompt = 0
	app.planPromptStatusBeforeUsage = false
	app.planPromptStatusBeforeUsagePrompt = 0
	app.SessionPath = path
	app.refreshOTelIdentity()
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
	preflightContext     context.Context
	// beforeBegin runs after all prompt enrichment and hooks accept the prompt and
	// receives the callback that records the prompt. It returns the active goal
	// revision and identity generation owned by the run; false cancels admission.
	// Goal admission invokes begin while holding the goal-store lock so shared child
	// tools cannot make the rendered text stale between validation and transcript
	// insertion.
	beforeBegin func(begin func() bool) (goalRevision, goalGeneration uint64, admitted bool)
}

type preparedPrompt struct {
	prompt          string
	images          []inputimage.Loaded
	promptContext   []string
	skillInjections int
}

func cloneRequestContext(contexts []string) []string {
	return append([]string(nil), contexts...)
}

func (app *App) clearAPIContinuation() {
	app.pendingAPIContinuation = nil
}

func (app *App) apiContinuationAvailable() bool {
	return app != nil && app.pendingAPIContinuation != nil
}

func (app *App) apiContinuationContext() ([]string, bool) {
	if !app.apiContinuationAvailable() {
		return nil, false
	}
	return cloneRequestContext(app.pendingAPIContinuation.requestContext), true
}

// finishPromptRun centralizes process-local /continue eligibility for every
// interactive model-bound run. Cancellation takes precedence even if an error
// chain also contains an APIError.
func (app *App) finishPromptRun(err error, requestContext []string) {
	if err == nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		app.clearAPIContinuation()
		return
	}
	var apiErr *llm.APIError
	if errors.As(err, &apiErr) {
		app.pendingAPIContinuation = &apiContinuationState{requestContext: cloneRequestContext(requestContext)}
		return
	}
	app.clearAPIContinuation()
}

func (app *App) runPrompt(prompt string) {
	if run, ok := app.preparePromptRun(prompt, promptOptions{resolveSkillMentions: true, attachPromptImages: true}); ok {
		run()
	}
}

func (app *App) preparePromptRun(prompt string, opts promptOptions) (func(), bool) {
	prepared, err := app.preparePrompt(prompt, opts, true)
	if err != nil {
		return nil, false
	}
	requestContext := app.promptHookContext(prepared.promptContext)
	preflightCtx := opts.preflightContext
	if preflightCtx == nil {
		preflightCtx = context.Background()
	}
	promptID := 0
	goalRevision, goalGeneration, goalActive := uint64(0), uint64(0), false
	var admission agent.PromptAdmission
	begin := func() bool {
		if preflightCtx.Err() != nil {
			return false
		}
		app.clearAPIContinuation()
		admission = app.Agent.AdmitPromptContent(prepared.prompt, imageBlocks(prepared.images))
		promptID = app.beginPrompt(prepared.prompt, prepared.images)
		app.recordSkillInjections(promptID, 1, prepared.skillInjections)
		return true
	}
	if opts.beforeBegin != nil {
		var admitted bool
		goalRevision, goalGeneration, admitted = opts.beforeBegin(begin)
		goalActive = admitted
		if !admitted {
			if app.Renderer != nil {
				app.Renderer.StopProgress()
			}
			return nil, false
		}
	} else if app.Goal != nil {
		var admitted bool
		goalRevision, goalGeneration, goalActive, admitted = app.Goal.AdmitAnyPrompt(begin)
		goalActive = goalActive && app.GoalAutoContinue
		if !admitted {
			if app.Renderer != nil {
				app.Renderer.StopProgress()
			}
			return nil, false
		}
	} else if !begin() {
		if app.Renderer != nil {
			app.Renderer.StopProgress()
		}
		return nil, false
	}
	ctx := context.Background()
	if app.Goal != nil {
		ctx = goal.WithGeneration(ctx, app.Goal, goalGeneration)
	}
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
		err := app.Agent.RunAdmittedPromptWithContext(ctx, admission, requestContext, promptID, sink)
		app.finishPromptRun(err, requestContext)
		sink.FlushEvents()
		app.goalOnPromptEnd(ctx, err, goalRevision, goalActive)
		if err != nil && !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) && !sink.terminalModelErrorDisplayed {
			fmt.Fprintf(app.Errw, "[error: %v]\n", err)
		}
		app.saveOrWarn(app.SessionPath)
	}, true
}

// prepareDetachedWaitContinuation admits the small host-created continuation
// without invoking human prompt hooks, skill resolution, pending-image handling,
// goal admission, or implementation handoff behavior. It still uses the normal
// prompt runner so tool refresh, accounting, persistence, and request context
// delivery remain identical to a top-level prompt.
func (app *App) prepareDetachedWaitContinuation() (func(), bool) {
	admission, promptID := app.admitInternalPrompt(detachedBackgroundWaitContinuation, detachedBackgroundWaitCause)
	requestContext := app.promptHookContext(nil)
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
		err := app.Agent.RunAdmittedPromptWithContext(ctx, admission, requestContext, promptID, sink)
		app.finishPromptRun(err, requestContext)
		sink.FlushEvents()
		// Deliberately do not call goalOnPromptEnd or alter its interruption state:
		// this host-created turn must not consume, pause, or otherwise mutate a goal.
		if err != nil && !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) && !sink.terminalModelErrorDisplayed {
			fmt.Fprintf(app.Errw, "[error: %v]\n", err)
		}
		app.saveOrWarn(app.SessionPath)
	}, true
}

// prepareAPIContinuation starts a fresh accounting prompt from the existing
// transcript boundary. It deliberately skips prompt admission, hooks, skills,
// pending images, goal admission, and EventUser recording.
func (app *App) prepareAPIContinuation() (func(), bool) {
	requestContext, ok := app.apiContinuationContext()
	if !ok {
		return nil, false
	}
	app.clearAPIContinuation()
	promptID := app.beginContinuationPrompt()
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
		sink.Notice("[continuing after API error]")
		err := app.Agent.ContinuePromptWithContext(ctx, requestContext, promptID, sink)
		app.finishPromptRun(err, requestContext)
		sink.FlushEvents()
		// A host recovery attempt never admits, advances, or pauses a goal.
		if err != nil && !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) && !sink.terminalModelErrorDisplayed {
			fmt.Fprintf(app.Errw, "[error: %v]\n", err)
		}
		app.saveOrWarn(app.SessionPath)
	}, true
}

func (app *App) preparePrompt(prompt string, opts promptOptions, stopProgressOnBlock bool) (preparedPrompt, error) {
	var skillInjections int
	if opts.resolveSkillMentions {
		var ok bool
		prompt, skillInjections, ok = app.resolveSkillMentionContext(prompt)
		if !ok {
			if app.Renderer != nil && stopProgressOnBlock {
				app.Renderer.StopProgress()
			}
			return preparedPrompt{}, errors.New("prompt skill resolution failed")
		}
	}
	ctx := opts.preflightContext
	if ctx == nil {
		ctx = context.Background()
	}
	promptHook := app.runPromptSubmitHook(ctx, prompt, app.PromptNumber+1)
	if err := ctx.Err(); err != nil {
		if app.Renderer != nil && stopProgressOnBlock {
			app.Renderer.StopProgress()
		}
		return preparedPrompt{}, err
	}
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
		return preparedPrompt{}, fmt.Errorf("prompt blocked: %s", reason)
	}
	pendingUnsupportedNotice := len(app.PendingImages) > 0 && !app.currentModelSupportsImages()
	images := app.takePendingImages()
	if opts.attachPromptImages {
		images = app.attachPromptImageReferences(prompt, images, pendingUnsupportedNotice)
	}
	promptContext := append([]string(nil), promptHook.AdditionalContext...)
	return preparedPrompt{prompt: prompt, images: images, promptContext: promptContext, skillInjections: skillInjections}, nil
}

func (app *App) prepareSteerInput(prompt string, opts promptOptions) (agent.SteerInput, error) {
	prepared, err := app.preparePrompt(prompt, opts, false)
	if err != nil {
		return agent.SteerInput{}, err
	}
	return agent.SteerInput{
		Text:             prepared.prompt,
		Images:           imageBlocks(prepared.images),
		RequestContext:   prepared.promptContext,
		DeliveryMetadata: skillInjectionMetadata(prepared.skillInjections),
	}, nil
}

func (app *App) prepareSteeredPrompt(input agent.SteerInput) (func(), bool) {
	if steerInputEmpty(input) {
		return nil, false
	}
	requestContext := app.promptHookContext(input.RequestContext)
	goalRevision, goalGeneration, goalActive := uint64(0), uint64(0), false
	var admission agent.PromptAdmission
	begin := func() bool {
		app.clearAPIContinuation()
		admission = app.Agent.AdmitPromptContent(input.Text, input.Images)
		return true
	}
	if app.Goal != nil {
		var admitted bool
		goalRevision, goalGeneration, goalActive, admitted = app.Goal.AdmitAnyPrompt(begin)
		if !admitted {
			return nil, false
		}
		goalActive = goalActive && app.GoalAutoContinue
	} else {
		begin()
	}
	promptID := app.beginPrompt(input.Text, nil)
	app.recordSkillInjections(promptID, 1, skillInjectionsFromMetadata(input.DeliveryMetadata))
	ctx := context.Background()
	if app.Goal != nil {
		ctx = goal.WithGeneration(ctx, app.Goal, goalGeneration)
	}
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
		err := app.Agent.RunAdmittedPromptWithContext(ctx, admission, requestContext, promptID, sink)
		app.finishPromptRun(err, requestContext)
		sink.FlushEvents()
		app.goalOnPromptEnd(ctx, err, goalRevision, goalActive)
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
	compactionsBefore := app.Agent.CompactionCount()
	u, err := app.Agent.CompactWithFocus(ctx, sink, focus)
	app.addCompactions(app.Agent.CompactionCount() - compactionsBefore)
	sink.FlushEvents()
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
		app.Renderer.SetCumulativeUsage(u.InputTokens, u.OutputTokens, u.CostUSD, u.Compactions)
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
		app.Renderer.SetCumulativeUsage(b.InputTokens, b.OutputTokens, app.usage.CostUSD, app.usage.Compactions)
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
	app.usage.Compactions += u.Compactions
	if app.usageByModel == nil {
		app.usageByModel = map[string]session.UsageTotals{}
	}
	bucket := app.usageByModel[modelKey]
	addTotals(&bucket, u.Usage, cost)
	app.usageByModel[modelKey] = bucket
	if app.Renderer != nil {
		active := app.usageByModel[app.usageKey()]
		app.Renderer.SetCumulativeUsage(active.InputTokens, active.OutputTokens, app.usage.CostUSD, app.usage.Compactions)
	}
}

func (app *App) addCompactions(count int) {
	if count <= 0 {
		return
	}
	app.usage.Compactions += count
	if app.Renderer != nil {
		active := app.usageByModel[app.usageKey()]
		app.Renderer.SetCumulativeUsage(active.InputTokens, active.OutputTokens, app.usage.CostUSD, app.usage.Compactions)
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
		app.Renderer.SetCumulativeUsage(b.InputTokens, b.OutputTokens, app.usage.CostUSD, app.usage.Compactions)
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
		usage.Compactions += current.Compactions
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
		Build:           app.SessionBuild,
		Runtime:         app.SessionRuntime,
		System:          app.System,
		Agent:           app.AgentName,
		ProxySessionID:  app.Agent.ProxySessionID(),
		CacheAffinityID: app.Agent.CacheAffinityID(),
		Prompt:          app.PromptNumber,
		Messages:        app.Agent.Transcript(),
		Tree:            app.SessionTree,
		ResponseState:   app.Agent.ResponseState(),
		Plan:            app.planSnapshot(),
		Todos:           app.todoSnapshot(),
		Goal:            app.goalSnapshot(),
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
	app.ArmTodoContext()
	return nil
}

func (app *App) planSnapshot() *plan.Plan {
	if app.Plans == nil {
		return nil
	}
	latest, ok := app.Plans.Latest()
	if !ok {
		return nil
	}
	return &latest
}

func (app *App) todoSnapshot() []todo.Item {
	if app.Todos == nil {
		return nil
	}
	return app.Todos.Snapshot()
}

// goalSnapshot returns the current goal for persistence, or nil when the goal
// store is not wired (one-shot mode and tests leave it nil).
func (app *App) goalSnapshot() *goal.State {
	if app.Goal == nil {
		return nil
	}
	return app.Goal.Snapshot()
}

// goalRequestContext returns the active-goal reminder for inclusion in model
// request context. It is regenerated each request so it survives compaction.
func (app *App) goalRequestContext() string {
	if app.Goal == nil || !app.GoalAutoContinue {
		return ""
	}
	return app.Goal.Reminder()
}

func (app *App) beginPrompt(prompt string, images []inputimage.Loaded) int {
	return app.beginPromptWithPurpose(prompt, images, "")
}

func (app *App) beginContinuationPrompt() int {
	app.drainMaintenanceUsage()
	app.PromptNumber++
	return app.PromptNumber
}

// beginPromptWithPurpose records an optional host-derived cause on the normal
// prompt boundary. The transcript admission remains prompt-origin so compaction
// and session trees recognize it as a real top-level turn.
func (app *App) beginPromptWithPurpose(prompt string, images []inputimage.Loaded, purpose string) int {
	app.drainMaintenanceUsage()
	app.PromptNumber++
	app.recordEvent(session.Event{
		Time:    app.clock()(),
		Type:    session.EventUser,
		Prompt:  app.PromptNumber,
		Text:    prompt,
		Images:  sessionImages(images),
		Purpose: purpose,
	})
	return app.PromptNumber
}

func (app *App) admitInternalPrompt(prompt, purpose string) (agent.PromptAdmission, int) {
	app.clearAPIContinuation()
	admission := app.Agent.AdmitPromptContent(prompt, nil)
	return admission, app.beginPromptWithPurpose(prompt, nil, purpose)
}

func (app *App) runPromptSubmitHook(ctx context.Context, prompt string, promptID int) hooks.Result {
	if app.Hooks == nil || !app.Hooks.HasEvent(hooks.UserPromptSubmit) {
		return hooks.Result{}
	}
	res := app.Hooks.Run(ctx, hooks.UserPromptSubmit, "", hooks.Payload{
		"prompt_id": promptID,
		"prompt":    prompt,
	})
	app.recordHookDiagnostics(promptID, res.Diagnostics)
	app.renderHookNotices(res.Notices)
	return res
}

// RunSessionStartHook runs the session-start hooks without cancellation. It is
// used for session lifecycle transitions initiated from the interactive UI.
func (app *App) RunSessionStartHook(source string) {
	app.RunSessionStartHookWithContext(context.Background(), source)
}

// RunSessionStartHookWithContext runs the session-start hooks with ctx so root
// startup can stop a slow hook when the user interrupts it.
func (app *App) RunSessionStartHookWithContext(ctx context.Context, source string) {
	if app.Hooks == nil || !app.Hooks.HasEvent(hooks.SessionStart) {
		return
	}
	res := app.Hooks.Run(ctx, hooks.SessionStart, source, hooks.Payload{"source": source})
	app.recordHookDiagnostics(0, res.Diagnostics)
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
		app.recordEvent(session.Event{Type: session.EventNotice, Text: notice})
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

// ArmTodoContext schedules one recovery reminder after a transcript rewrite.
func (app *App) ArmTodoContext() {
	if app != nil && app.Todos != nil {
		app.Todos.RequireRequestContext()
	}
}

func (app *App) todoRequestContext() string {
	if app.Todos == nil || !app.agentHasTool("update_todos") {
		return ""
	}
	return app.Todos.PendingRequestContext()
}

func (app *App) backgroundRequestContext(archiver agent.ToolResultArchiver) []string {
	if app.Background == nil {
		return nil
	}
	contexts := app.Background.DrainCompletedContext(archiver)
	if sink, ok := archiver.(interface {
		BackgroundJobDiagnostic(background.CompletedDiagnostic) error
	}); ok {
		for _, diagnostic := range app.Background.PeekCompletedDiagnostics() {
			if err := sink.BackgroundJobDiagnostic(diagnostic); err == nil {
				app.Background.AcknowledgeCompletedDiagnostic(diagnostic.ID)
			}
		}
	}
	return contexts
}

func (app *App) recordCompletedBackgroundDiagnostics() {
	if app == nil || app.Background == nil {
		return
	}
	diagnostics := app.Background.PeekCompletedDiagnostics()
	if len(diagnostics) == 0 {
		return
	}
	sink := newAccumulatingSink(app.Renderer, app, app.PromptNumber)
	for _, diagnostic := range diagnostics {
		if err := sink.BackgroundJobDiagnostic(diagnostic); err == nil {
			app.Background.AcknowledgeCompletedDiagnostic(diagnostic.ID)
		}
	}
	// Avoid FlushEvents here because it would redundantly drain the manager.
	sink.rec.Flush()
}

func (app *App) pollBackgroundNotices() {
	if app.Background == nil {
		return
	}
	app.recordCompletedBackgroundDiagnostics()
	for _, job := range app.Background.DrainNoticeSnapshots() {
		notice := formatBackgroundCompletionNotice(job)
		if app.Renderer != nil {
			app.Renderer.Notice(notice)
		} else {
			fmt.Fprintln(app.Errw, notice)
		}
		app.recordEvent(session.Event{Type: session.EventNotice, Text: notice, Time: app.clock()()})
	}
}

func formatBackgroundCompletionNotice(job background.Snapshot) string {
	var b strings.Builder
	switch job.Status {
	case background.StatusCompleted:
		fmt.Fprintf(&b, "[background: %s completed", job.ID)
	case background.StatusCanceled, background.StatusAbandoned:
		fmt.Fprintf(&b, "[background: %s %s", job.ID, job.Status)
	default:
		fmt.Fprintf(&b, "[background: %s failed: %s", job.ID, job.Error)
	}
	if job.Kind == "delegate" {
		totals := session.UsageTotals{
			Usage:       job.Result.Usage,
			CostUSD:     job.Result.Usage.CostUSD,
			Compactions: job.Result.Compactions,
		}
		writeUsageTotals(&b, "; child session: ", totals, " · "+compactionPhrase(totals.Compactions))
	}
	if job.Status == background.StatusCompleted && job.Result.TranscriptPath != "" {
		fmt.Fprintf(&b, "; transcript %s", job.Result.TranscriptPath)
	}
	b.WriteByte(']')
	return b.String()
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

func (app *App) markTodoPromptStatusPrintedBeforeUsage(prompt int) {
	app.todoPromptStatusBeforeUsage = true
	app.todoPromptStatusBeforeUsagePrompt = prompt
}

func (app *App) todoPromptStatusPrintedBeforeUsageForPrompt(prompt int) bool {
	return app.todoPromptStatusBeforeUsage && app.todoPromptStatusBeforeUsagePrompt == prompt
}

func (app *App) printPlanStatus(state plan.DisplayState) bool {
	if app.Plans == nil || !app.agentHasTool("record_plan") {
		return false
	}
	latest, ok := app.Plans.Latest()
	if !ok || latest.Path == "" {
		return false
	}
	line := plan.RenderLatestWithState(&latest, state)
	if line == "" {
		return false
	}
	fmt.Fprintln(app.Errw, line)
	return true
}

func (app *App) markPlanPromptStatusPrintedBeforeUsage(prompt int) {
	app.planPromptStatusBeforeUsage = true
	app.planPromptStatusBeforeUsagePrompt = prompt
}

func (app *App) planPromptStatusPrintedBeforeUsageForPrompt(prompt int) bool {
	return app.planPromptStatusBeforeUsage && app.planPromptStatusBeforeUsagePrompt == prompt
}

const backgroundShutdownWait = time.Second

func (app *App) stopBackgroundJobs() {
	if app.Background == nil {
		return
	}
	app.recordCompletedBackgroundDiagnostics()
	if forceExitRequested(app.ForceExit) {
		app.Background.Shutdown()
	} else {
		app.Background.ShutdownAndWait(backgroundShutdownWait)
	}
	app.recordCompletedBackgroundDiagnostics()
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

func (app *App) recordHookDiagnostics(prompt int, diagnostics []hooks.Diagnostic) {
	for _, diagnostic := range diagnostics {
		app.recordEvent(session.Event{
			Type:           session.EventHookDiagnostic,
			Prompt:         prompt,
			HookDiagnostic: sessionrec.HookDiagnosticSnapshot(diagnostic),
		})
	}
}

func (app *App) recordEvent(ev session.Event) {
	if ev.Time.IsZero() {
		ev.Time = app.clock()()
	}
	if app.SessionPath != "" {
		if err := session.AppendEvent(app.SessionPath, ev); err != nil {
			fmt.Fprintf(app.Errw, "[session event log failed: %v]\n", err)
			return
		}
	}
	if app.RunStream != nil {
		app.RunStream.Mirror(ev)
	}
}

// usageSummary renders the cumulative session usage for /usage (design §10).
func (app *App) usageSummary() string {
	app.drainMaintenanceUsage()
	return app.usageReport("session")
}

// usageReport renders cumulative session usage under the given label. With at
// most one model it is a single aggregate line; with several it breaks
// down per model target and always ends with the session-total cost.
func (app *App) usageReport(label string) string {
	var b strings.Builder
	if len(app.usageByModel) <= 1 {
		writeUsageTotals(&b, "["+label+": ", app.usage, " · "+compactionPhrase(app.usage.Compactions))
		b.WriteByte(']')
		return b.String()
	}
	fmt.Fprintf(&b, "[%s by model:", label)
	for _, key := range slices.Sorted(maps.Keys(app.usageByModel)) {
		writeUsageTotals(&b, "\n  "+key+": ", app.usageByModel[key], "")
	}
	fmt.Fprintf(&b, "\n  total · %s · $%.4f]", compactionPhrase(app.usage.Compactions), app.usage.CostUSD)
	return b.String()
}

// compactionPhrase formats the always-present cumulative session count.
func compactionPhrase(count int) string {
	if count == 1 {
		return "1 compaction"
	}
	return fmt.Sprintf("%d compactions", count)
}

// writeUsageTotals writes one usage line: prefix, token counts, afterUsage,
// then cost when non-zero.
func writeUsageTotals(b *strings.Builder, prefix string, u session.UsageTotals, afterUsage string) {
	fmt.Fprintf(b, "%s%d input / %d cached input / %d output / %d reasoning",
		prefix, u.InputTokens, u.CacheReadTokens, u.OutputTokens, u.ReasoningTokens)
	if u.CacheWriteTokens > 0 {
		fmt.Fprintf(b, " / %d cache write", u.CacheWriteTokens)
	}
	if u.CacheWrite1hTokens > 0 {
		fmt.Fprintf(b, " / %d cache write (1h)", u.CacheWrite1hTokens)
	}
	b.WriteString(afterUsage)
	if u.CostUSD > 0 {
		fmt.Fprintf(b, " · $%.4f", u.CostUSD)
	}
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

func (app *App) toolsCommand(arg string) {
	switch strings.ToLower(strings.TrimSpace(arg)) {
	case "":
		fmt.Fprintln(app.Errw, app.toolsSummary())
	case "raw", "--raw":
		req := app.Agent.ContextRequest()
		dump := struct {
			Tools       []llm.ToolSchema `json:"tools"`
			ServerTools []llm.ServerTool `json:"server_tools,omitempty"`
		}{
			Tools:       req.Tools,
			ServerTools: req.ServerTools,
		}
		data, err := json.MarshalIndent(dump, "", "  ")
		if err != nil {
			fmt.Fprintf(app.Errw, "[tools failed: %v]\n", err)
			return
		}
		_, _ = app.Errw.Write(append(data, '\n'))
	default:
		fmt.Fprintln(app.Errw, "usage: /tools [--raw]")
	}
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

func (app *App) lspCommand(arg string) {
	if app.ControlLSP == nil {
		fmt.Fprintln(app.Errw, "[lsp: unavailable]")
		return
	}
	action := strings.ToLower(strings.TrimSpace(arg))
	if action == "" {
		action = "status"
	}
	switch action {
	case "status", "enable", "disable":
	default:
		fmt.Fprintln(app.Errw, "usage: /lsp [status|enable|disable]")
		return
	}
	selection, err := app.ControlLSP(action, app.AgentName)
	if err != nil {
		fmt.Fprintf(app.Errw, "[lsp: %v]\n", err)
		return
	}
	runtimeChanged := false
	if selection.Tools != nil {
		app.Agent.SetTools(selection.Tools)
		runtimeChanged = true
	}
	if selection.System != "" && selection.System != app.System {
		app.System = selection.System
		app.Agent.SetSystem(selection.System)
		runtimeChanged = true
	}
	if runtimeChanged {
		app.Agent.ResetProxySessionID()
		app.schedulePrewarm()
	}
	fmt.Fprintln(app.Errw, formatLSPStatus(selection.Status))
}

func formatLSPStatus(status LSPStatus) string {
	state := "disabled"
	if status.Enabled {
		state = "enabled"
	}
	var b strings.Builder
	fmt.Fprintf(&b, "lsp: %s\n", state)
	fmt.Fprintf(&b, "  configured tools: %d\n", len(status.Tools))
	fmt.Fprintf(&b, "  available languages: %s\n", listOrNone(status.AvailableLanguages))
	fmt.Fprintf(&b, "  loaded languages: %s", listOrNone(status.LoadedLanguages))
	if len(status.Servers) == 0 {
		return b.String()
	}
	b.WriteString("\n  servers:")
	for _, server := range status.Servers {
		serverState := "missing"
		if server.Available {
			serverState = "available"
		}
		if len(server.LoadedRoots) > 0 {
			serverState = "loaded"
		}
		fmt.Fprintf(&b, "\n    %s (%s): %s", server.Name, strings.Join(server.Languages, ", "), serverState)
		if len(server.LoadedRoots) > 0 {
			fmt.Fprintf(&b, " [%s]", strings.Join(server.LoadedRoots, ", "))
		} else if server.Command != "" && !server.Available {
			fmt.Fprintf(&b, " [%s not on PATH]", server.Command)
		}
	}
	return b.String()
}

func listOrNone(values []string) string {
	if len(values) == 0 {
		return "none"
	}
	return strings.Join(values, ", ")
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
	rec                         *sessionrec.Recorder
	otel                        *otel.Sink
	prompt                      int
	printTodoPromptBeforeUsage  bool
	printPlanPromptBeforeUsage  bool
	planHadPlanAtStart          bool
	planPathAtStart             string
	reasoningOutput             bool
	promptUsage                 agent.PromptUsage // last PromptComplete, priced; JSON run modes report it in prompt_end
	pendingNames                map[string]string
	pendingOTel                 map[string]pendingOTelTool
	otelToolNames               []string
	todoTurn                    int
	turn                        int
	attempt                     int
	inMaintenance               bool
	promptStart                 time.Time
	terminalModelErrorDisplayed bool
	attemptText                 strings.Builder
	finalText                   string
}

// SetOTel installs the concrete OTEL sink before prompts begin. Keeping this
// typed prevents optional-interface drift from silently disabling telemetry.
func (app *App) SetOTel(sink *otel.Sink) {
	app.otelSink = sink
	app.refreshOTelIdentity()
}

func (app *App) refreshOTelIdentity() {
	if app == nil || app.otelSink == nil {
		return
	}
	app.otelSink.SetIdentity(filepath.Base(app.SessionPath), app.Provider, app.Model, app.AgentName)
}

// RecordOTelSession emits one final lifecycle snapshot for the current session.
// It is safe to call from both /clear and process shutdown.
func (app *App) RecordOTelSession() {
	if app == nil || app.otelSink == nil || app.SessionPath == "" || app.otelRecordedSession == app.SessionPath {
		return
	}
	app.refreshOTelIdentity()
	app.otelRecordedSession = app.SessionPath
	usage := app.usage.Usage
	totalTokens := usage.InputTokens + usage.OutputTokens + usage.CacheReadTokens + usage.CacheWriteTokens + usage.CacheWrite1hTokens + usage.ReasoningTokens
	app.otelSink.RecordSession(app.usage.CostUSD, totalTokens)
	if app.Agent != nil {
		app.otelSink.RecordContext(otelContextComposition(app.Agent.Transcript()))
	}
	if app.Background == nil {
		return
	}
	for _, job := range app.Background.List() {
		if job.Kind != "delegate" {
			continue
		}
		status, terminationReason := job.Status, job.Status
		agentName := job.Agent
		usage := job.Result.Usage
		turns, compactions := 0, 0
		if progress, ok := job.Progress.(func() agent.DelegateProgressSnapshot); ok {
			turns = progress().Turn
		}
		if job.Result.TranscriptPath != "" {
			if raw, err := os.ReadFile(filepath.Join(job.Result.TranscriptPath, "meta.json")); err == nil {
				var meta session.ChildMeta
				if json.Unmarshal(raw, &meta) == nil {
					status, terminationReason, turns, usage = meta.Status, meta.TerminationReason, meta.TurnsUsed, meta.Usage
					if meta.Agent != "" {
						agentName = meta.Agent
					}
				}
			}
			if child, err := session.Load(job.Result.TranscriptPath); err == nil {
				compactions = child.Usage.Compactions
			}
		}
		if status == "" || status == "running" {
			status, terminationReason = "abandoned", "abandoned"
		} else if terminationReason == "" {
			terminationReason = status
		}
		if job.Error != "" && terminationReason == status {
			terminationReason = "error"
		}
		app.otelSink.RecordDelegate(agentName, status, terminationReason, turns, usage, compactions)
	}
}

func otelContextComposition(messages []llm.Message) otel.ContextComposition {
	composition := otel.ContextComposition{Messages: len(messages)}
	var addBlock func(llm.Role, llm.ContentBlock)
	addBlock = func(role llm.Role, block llm.ContentBlock) {
		composition.Blocks++
		switch block.Kind {
		case llm.BlockText:
			if role == llm.RoleAssistant {
				composition.AssistantTextBytes += len(block.Text)
			} else {
				composition.UserTextBytes += len(block.Text)
			}
		case llm.BlockImage:
			if block.ImageEncodedBytes > 0 {
				composition.ImageEncodedBytes += block.ImageEncodedBytes
			} else {
				composition.ImageEncodedBytes += len(block.ImageData)
			}
		case llm.BlockToolUse:
			composition.ToolInputBytes += len(block.ToolInput)
		case llm.BlockToolResult:
			composition.ToolResultBytes += len(block.ResultText)
			for _, nested := range block.ResultContent {
				addBlock(role, nested)
			}
		case llm.BlockThinking:
			composition.ReasoningTextBytes += len(block.Thinking)
			composition.ReasoningOpaqueBytes += len(block.ThinkingSignature)
		case llm.BlockRedactedThinking:
			composition.ReasoningOpaqueBytes += len(block.RedactedData)
		case llm.BlockReasoning:
			composition.ReasoningOpaqueBytes += len(block.ReasoningID) + len(block.ReasoningEncrypted)
		case llm.BlockInteractionThought:
			composition.ReasoningTextBytes += len(block.InteractionThoughtSummary)
			composition.ReasoningOpaqueBytes += len(block.InteractionThoughtSignature)
		case llm.BlockInteractionStep:
			composition.ReasoningOpaqueBytes += len(block.InteractionStep)
		}
	}
	for _, message := range messages {
		for _, block := range message.Content {
			addBlock(message.Role, block)
		}
	}
	return composition
}

type pendingOTelTool struct {
	name     string
	started  time.Time
	activity tools.Activity
	input    json.RawMessage
}

func (s *accumulatingSink) pendingToolInputs(id string) json.RawMessage {
	if p, ok := s.pendingOTel[id]; ok {
		return p.input
	}
	return nil
}

func newAccumulatingSink(r *Renderer, app *App, prompt int) *accumulatingSink {
	s := &accumulatingSink{
		r: r, app: app, prompt: prompt,
		pendingNames: make(map[string]string),
		pendingOTel:  make(map[string]pendingOTelTool),
	}
	if app != nil && app.Now != nil {
		s.promptStart = app.Now()
	} else {
		s.promptStart = time.Now()
	}
	if app != nil {
		s.otel = app.otelSink
		var mirror func(session.Event)
		if app.RunStream != nil {
			mirror = app.RunStream.Mirror
		}
		s.rec = sessionrec.New(sessionrec.Config{
			Dir:                app.SessionPath,
			Prompt:             prompt,
			Agent:              app.AgentName,
			ModelTarget:        app.RegistryModel,
			Provider:           app.Provider,
			Model:              app.Model,
			Clock:              app.clock(),
			ReasoningSummaries: reasoningSummaryDisplayEnabled(app.Reasoning.Summary),
			Mirror:             mirror,
			PriceTurnUsage: func(u llm.Usage) (float64, bool) {
				// Price against the App's active model so a mid-prompt model
				// switch is not mispriced (r63).
				return app.Registry.Cost(app.usageKey(), u)
			},
			PricePromptUsage: app.promptCost,
			PromptUsageLine: func(u agent.PromptUsage, promptElapsed time.Duration, cost float64, costKnown bool) string {
				// Runs after App.addUsage refreshed the renderer's cumulative
				// totals, so the recorded line matches the live one.
				return usageLine(u, promptElapsed, cost, costKnown, s.r.cumInput, s.r.cumOutput, s.r.cumCost, s.r.cumCompactions)
			},
			OnError: func(err error) {
				fmt.Fprintf(app.Errw, "[session event log failed: %v]\n", err)
			},
		})
	}
	return s
}

func newREPLSink(r *Renderer, app *App, prompt int) *accumulatingSink {
	s := newAccumulatingSink(r, app, prompt)
	s.printTodoPromptBeforeUsage = true
	s.printPlanPromptBeforeUsage = true
	if app.Plans != nil {
		if latest, ok := app.Plans.Latest(); ok && latest.Path != "" {
			s.planHadPlanAtStart = true
			s.planPathAtStart = latest.Path
		}
	}
	s.reasoningOutput = true
	return s
}

func (s *accumulatingSink) planDisplayState() plan.DisplayState {
	if s.app.Plans == nil {
		return plan.DisplayCurrent
	}
	latest, ok := s.app.Plans.Latest()
	if !ok || latest.Path == "" {
		return plan.DisplayCurrent
	}
	if !s.planHadPlanAtStart {
		return plan.DisplayRecorded
	}
	if latest.Path != s.planPathAtStart {
		return plan.DisplayUpdated
	}
	return plan.DisplayCurrent
}

func (s *accumulatingSink) recordEvent(ev session.Event) {
	if s == nil || s.rec == nil {
		return
	}
	s.rec.Append(ev)
}

func (s *accumulatingSink) FlushEvents() {
	if s == nil || s.rec == nil {
		return
	}
	s.drainBackgroundJobDiagnostics()
	s.rec.Flush()
}

func (s *accumulatingSink) TextDelta(text string) {
	s.attemptText.WriteString(text)
	s.r.TextDelta(text)
	s.rec.TextDelta(text)
}

func (s *accumulatingSink) AssistantPhase(phase string) {
	if !llm.ValidAssistantPhase(phase) || phase == "" {
		return
	}
	s.r.AssistantPhase(phase)
	s.rec.AssistantPhase(phase)
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
	s.rec.ReasoningSummary(text)
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
	if s.app != nil && s.app.Todos != nil && s.app.agentHasTool("update_todos") {
		s.app.Todos.CommitModelRound(turn != s.todoTurn)
		s.todoTurn = turn
	}
	s.attemptText.Reset()
	s.otelToolNames = nil
	s.turn = turn
	s.attempt = attempt
	s.r.TurnAttemptStart(turn, attempt, ctx)
	s.rec.TurnAttemptStart(turn, attempt, ctx)
}

func (s *accumulatingSink) TurnAttemptAbandoned(turn, attempt int) {
	s.attemptText.Reset()
	s.rec.TurnAttemptAbandoned(turn, attempt)
}

func (s *accumulatingSink) TurnAttemptComplete(u agent.TurnAttemptUsage) {
	s.r.TurnAttemptComplete(u)
	s.rec.TurnAttemptComplete(u)
}

func (s *accumulatingSink) ModelRequestEvent(event llm.ModelRequestEvent) {
	line := s.r.ModelRequestEvent(event)
	if line != "" && event.Outcome == llm.ModelRequestOutcomeTerminal {
		s.terminalModelErrorDisplayed = true
	}
	if s.otel != nil {
		s.otel.ModelRequestEvent(event)
	}
	s.rec.ModelRequestEvent(event)
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
	attrs := []any{
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
	if len(event.ResponsePayload) > 0 {
		attrs = append(attrs, "api_response_payload", json.RawMessage(event.ResponsePayload))
	}
	return attrs
}

func (s *accumulatingSink) ToolUseStart(c llm.ToolCall) {
	s.r.ToolUseStart(c)
}

func (s *accumulatingSink) ToolUseDelta(index int, delta string) {
	s.r.ToolUseDelta(index, delta)
}

func (s *accumulatingSink) ToolStart(c llm.ToolCall) {
	s.pendingNames[c.ID] = c.Name
	s.otelToolNames = append(s.otelToolNames, c.Name)
	if s.otel != nil || (s.app != nil && s.app.Agent != nil) {
		activity := tools.Activity{Class: tools.ActivityOther, OperationCount: 1}
		if s.app != nil && s.app.Agent != nil {
			activity = s.app.Agent.ToolActivity(c)
		}
		s.pendingOTel[c.ID] = pendingOTelTool{name: c.Name, started: time.Now(), activity: activity, input: append(json.RawMessage(nil), c.Input...)}
	}
	s.r.ToolStart(c)
	s.rec.ToolStart(c)
}

func (s *accumulatingSink) ToolResult(res llm.ToolResult) {
	name := s.pendingNames[res.ForID]
	delete(s.pendingNames, res.ForID)
	if res.BackgroundJobID != "" && s.app != nil && s.app.Background != nil {
		if identity, ok := s.rec.PendingToolIdentity(res.ForID); ok {
			s.app.Background.SetDiagnosticIdentity(res.BackgroundJobID, background.DiagnosticIdentity{
				Agent:       identity.Agent,
				ModelTarget: identity.ModelTarget,
				Provider:    identity.Provider,
				APIType:     identity.APIType,
				Model:       identity.Model,
			})
		}
	}
	pendingOTel := s.pendingOTel[res.ForID]
	delete(s.pendingOTel, res.ForID)
	if s.otel != nil {
		input := append(json.RawMessage(nil), pendingOTel.input...)
		timeSince := time.Since(pendingOTel.started).Milliseconds()
		durMS := int64(-1)
		if !pendingOTel.started.IsZero() {
			durMS = timeSince
		}
		toolName := name
		if toolName == "" {
			toolName = pendingOTel.name
		}
		s.otel.ToolResultWithName(toolName, res, durMS, pendingOTel.activity)
		if toolName == "shell" && len(input) > 0 {
			s.otel.RecordCommands(input)
		}
	}
	s.r.ToolResult(res)
	s.rec.ToolResult(res)
	if name == "update_todos" && !res.IsError {
		s.app.printTodoStatus(true)
	}
	if name == "record_plan" && !res.IsError {
		s.app.printPlanStatus(s.planDisplayState())
	}
}

func (s *accumulatingSink) ToolDiff(call llm.ToolCall, path, text string) {
	s.r.ToolDiff(call, path, text)
	s.rec.ToolDiff(call, path, text)
}

func (s *accumulatingSink) BackgroundJobDiagnostic(diagnostic background.CompletedDiagnostic) error {
	if s == nil || s.rec == nil {
		return nil
	}
	var identity sessionrec.ExecutionIdentity
	if diagnostic.Identity != nil {
		identity = sessionrec.ExecutionIdentity{
			Agent:       diagnostic.Identity.Agent,
			ModelTarget: diagnostic.Identity.ModelTarget,
			Provider:    diagnostic.Identity.Provider,
			APIType:     diagnostic.Identity.APIType,
			Model:       diagnostic.Identity.Model,
		}
	}
	return s.rec.BackgroundJobResultWithIdentity(
		diagnostic.ID,
		diagnostic.Kind,
		diagnostic.Status,
		diagnostic.Duration,
		diagnostic.Metrics,
		identity,
	)
}

func (s *accumulatingSink) drainBackgroundJobDiagnostics() {
	if s == nil || s.app == nil || s.app.Background == nil {
		return
	}
	for _, diagnostic := range s.app.Background.PeekCompletedDiagnostics() {
		if err := s.BackgroundJobDiagnostic(diagnostic); err == nil {
			s.app.Background.AcknowledgeCompletedDiagnostic(diagnostic.ID)
		}
	}
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
	s.rec.Notice(msg, turn)
}

// SteerDelivered reports that queued in-prompt steer input was injected into
// the transcript: the dim line renders above the live status line and is
// recorded as a sessionrec Display line via Notice.
func (s *accumulatingSink) SteerDelivered(input agent.SteerInput) {
	s.Notice("[steer sent: " + submissionPreview(input.Text, 0) + "]")
	for range skillInjectionsFromMetadata(input.DeliveryMetadata) {
		if s.otel != nil {
			s.otel.RecordSkill("explicit", "injected")
		}
		s.recordEvent(session.Event{
			Type:    session.EventSkillActivation,
			Prompt:  s.prompt,
			Turn:    max(s.turn, 1),
			Purpose: "explicit",
			Summary: "injected",
		})
	}
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
	s.finalText = s.attemptText.String()
	if !u.Usage.CostKnown {
		u.Usage.CostUSD, u.Usage.CostKnown = s.app.Registry.Cost(s.app.usageKey(), u.Usage)
	}
	if s.otel != nil {
		s.otel.RecordTurnSummary(s.otelToolNames)
		s.otel.TurnComplete(u)
	}
	s.r.TurnComplete(u)
	s.rec.TurnComplete(u)
}

// FinalText returns the most recent assistant text emitted during this prompt,
// including partial text from a terminal failed attempt.
func (s *accumulatingSink) FinalText() string {
	if s.attemptText.Len() > 0 {
		return s.attemptText.String()
	}
	return s.finalText
}

func (s *accumulatingSink) MaintenanceComplete(u agent.MaintenanceUsage) {
	if s.otel != nil {
		s.otel.MaintenanceComplete(u)
	}
	s.rec.MaintenanceComplete(u)
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
	s.recordEvent(session.Event{
		Type:                session.EventCheckpoint,
		Prompt:              s.prompt,
		Turn:                checkpoint.Turn,
		Purpose:             string(checkpoint.Kind),
		DurationMS:          elapsed.Milliseconds(),
		MessageCount:        len(s.app.Agent.Transcript()),
		ClosureTrigger:      string(checkpoint.Usage.ClosureTrigger),
		ClosureTurn:         checkpoint.Usage.ClosureTurn,
		TurnBudgetExhausted: checkpoint.Usage.TurnBudgetExhausted,
		WorkflowStatus:      sessionrec.WorkflowStatusSnapshot(checkpoint.Usage.WorkflowStatus),
	})
}

func (s *accumulatingSink) ClosureStarted(event agent.ClosureEvent) {
	s.rec.ClosureStarted(event)
}

func (s *accumulatingSink) TurnProgress(progress agent.TurnProgress) {
	if s.otel != nil {
		s.otel.TurnProgress(progress)
	}
	s.rec.TurnProgress(progress)
}

func (s *accumulatingSink) HookDiagnostic(diagnostic hooks.Diagnostic) {
	s.rec.HookDiagnostic(diagnostic)
}

func (s *accumulatingSink) WorkflowStatus() agent.WorkflowStatus {
	if s == nil || s.app == nil || s.app.WorkflowStatusFunc == nil {
		return agent.WorkflowStatus{}
	}
	return s.app.WorkflowStatusFunc()
}

func (s *accumulatingSink) RetentionApplied(event agent.RetentionEvent) {
	if s.otel != nil {
		s.otel.RetentionApplied(event)
	}
	s.recordEvent(session.Event{
		Type:      session.EventRetention,
		Prompt:    s.prompt,
		Turn:      s.turn + 1,
		Retention: sessionrec.RetentionSnapshot(event),
	})
}

func (s *accumulatingSink) AddHookContext(ctx []string) {
	s.app.AddHookContext(ctx)
}

func (s *accumulatingSink) TranscriptRewritten() {
	s.app.ArmTodoContext()
}

func (s *accumulatingSink) RequestContext() []string {
	var out []string
	if ctx := s.app.todoRequestContext(); ctx != "" {
		out = append(out, ctx)
	}
	if ctx := s.app.goalRequestContext(); ctx != "" {
		out = append(out, ctx)
	}
	out = append(out, s.app.backgroundRequestContext(s)...)
	return out
}

// PeekRequestContext mirrors RequestContext without consuming completed
// background context, so post-prompt size estimates never eat context that
// still needs to reach the model on a later real request.
func (s *accumulatingSink) PeekRequestContext() []string {
	var out []string
	if ctx := s.app.todoRequestContext(); ctx != "" {
		out = append(out, ctx)
	}
	if ctx := s.app.goalRequestContext(); ctx != "" {
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
	cost, costKnown := s.app.promptCost(u.Usage)
	s.promptUsage = u
	if !s.promptUsage.Usage.CostKnown {
		s.promptUsage.Usage.CostUSD, s.promptUsage.Usage.CostKnown = cost, costKnown
	}
	if s.printTodoPromptBeforeUsage {
		s.r.StopProgress()
		s.r.flushToolUseStarts()
		s.r.finishAssistantLine()
		if s.app.printTodoStatus(false) {
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
	if s.otel != nil {
		s.otel.PromptComplete(u, time.Since(s.promptStart))
		if s.app != nil && s.app.Agent != nil {
			all := s.app.Agent.Transcript()
			var batches [][]string
			for _, m := range all {
				for _, b := range m.ParallelToolBatches {
					if len(b.ToolUseIDs) >= 2 {
						batches = append(batches, append([]string(nil), b.ToolUseIDs...))
					}
				}
			}
			s.otel.RecordParallel(batches)
		}
	}
	s.r.PromptComplete(u)
	s.app.addUsage(u)
	s.rec.PromptComplete(u)
}
