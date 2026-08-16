// Command harness is the entrypoint: it loads configuration, connects to
// harness-model-proxy, constructs the tool registry and agent, wires SIGINT
// handling, prints the session path, and dispatches to the interactive REPL or
// one-shot mode.
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"harness/internal/agent"
	"harness/internal/agentdef"
	"harness/internal/background"
	"harness/internal/buildinfo"
	"harness/internal/cli"
	"harness/internal/config"
	"harness/internal/configmeta"
	"harness/internal/delegate"
	"harness/internal/goal"
	"harness/internal/handoff"
	"harness/internal/hooks"
	"harness/internal/inputimage"
	"harness/internal/llm"
	"harness/internal/logging"
	"harness/internal/markdown"
	"harness/internal/mcptools"
	modelclient "harness/internal/modelproxy/client"
	"harness/internal/modelproxy/protocol"
	"harness/internal/otel"
	"harness/internal/plan"
	"harness/internal/reasoningprofile"
	"harness/internal/runstream"
	"harness/internal/session"
	"harness/internal/skills"
	"harness/internal/sysprompt"
	"harness/internal/term"
	"harness/internal/term/highlight"
	"harness/internal/tmux"
	"harness/internal/todo"
	"harness/internal/tools"
	"harness/internal/tracing"
	"harness/internal/ui"
)

const modelProxyCheckTimeout = 2 * time.Second

const rgSystemHint = "When you search for text or files, reach first for `rg` or `rg --files`; they are much faster than alternatives like `grep`."

func main() {
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT)
	defer signal.Stop(sigCh)

	os.Exit(run(environment{
		args:         os.Args[1:],
		stdin:        os.Stdin,
		stdout:       os.Stdout,
		stderr:       os.Stderr,
		getenv:       os.Getenv,
		lookupEnv:    os.LookupEnv,
		now:          time.Now,
		colorTTY:     isTTY(os.Stdout),
		stdinPiped:   pipedStdin(os.Stdin),
		prewarmCache: true,
		sigCh:        sigCh,
		terminalRows: defaultTerminalRows,
		terminalCols: defaultTerminalCols,
	}))
}

// environment carries everything run depends on, so the wiring is testable with
// injected readers/writers, env, clock, TTY/pipe flags, and signal channel
// (design §13: no dependence on real time or terminals in tests). A nil sigCh
// disables SIGINT handling (tests).
type environment struct {
	args         []string
	stdin        io.Reader
	stdout       io.Writer
	stderr       io.Writer
	getenv       func(string) string
	lookupEnv    func(string) (string, bool)
	now          func() time.Time
	colorTTY     bool // stdout is a terminal (gates color)
	stdinPiped   bool // stdin is piped/redirected (gates one-shot stdin read)
	prewarmCache bool // issue a background prompt-cache warm-up at interactive startup
	sigCh        chan os.Signal

	terminalRows func() int
	terminalCols func() int
	agentSleep   func(time.Duration)
	// promptFinished is a test/embedding hook invoked after a prompt's final
	// session save, including after run has returned for a forced exit.
	promptFinished func()
}

func (env environment) envLookup() func(string) (string, bool) {
	if env.lookupEnv != nil {
		return env.lookupEnv
	}
	if env.getenv == nil {
		return func(string) (string, bool) { return "", false }
	}
	return func(name string) (string, bool) {
		value := env.getenv(name)
		return value, value != ""
	}
}

func harnessLoadOptions(env environment, args []string) config.LoadOptions {
	getenv := env.getenv
	if getenv == nil {
		getenv = func(string) string { return "" }
	}
	return config.LoadOptions{
		Args:              args,
		LookupEnv:         env.envLookup(),
		DefaultConfigPath: filepath.Join(defaultConfigDir(getenv), "config.json"),
		Defaults: config.RuntimeDefaults{
			ModelProxyURL: protocol.DefaultURL,
			MCPProxyURL:   resolveMCPProxy(""),
			HistoryPath:   session.HistoryPath(stateDir(getenv)),
			Agent:         agentdef.Default,
			TmuxActive:    getenv("TMUX") != "",
		},
	}
}

func writeInformationalJSON(w io.Writer, value any) error {
	encoder := json.NewEncoder(w)
	encoder.SetEscapeHTML(false)
	encoder.SetIndent("", "  ")
	return encoder.Encode(value)
}

func signalCancelContext(sigCh <-chan os.Signal) (context.Context, context.CancelFunc, func() bool) {
	ctx, cancel := context.WithCancel(context.Background())
	var interrupted atomic.Bool
	if sigCh != nil {
		go func() {
			select {
			case _, ok := <-sigCh:
				if ok {
					interrupted.Store(true)
				}
				cancel()
			case <-ctx.Done():
			}
		}()
	}
	return ctx, cancel, interrupted.Load
}

// run wires everything together and returns the process exit code (design §10
// exit codes: 0 ok, 1 runtime, 2 usage, 130 interrupted).
func runRoot(env environment, invocation cli.Invocation) (exitCode int) {
	stdin := env.stdin
	stdout := env.stdout
	stderr := env.stderr
	getenv := env.getenv
	now := env.now
	if now == nil {
		now = time.Now
	}
	result, err := config.LoadParsed(harnessLoadOptions(env, nil), invocation.Flags)
	if err != nil {
		fmt.Fprintf(stderr, "harness: %v\n", err)
		return ui.ExitUsage
	}
	if result.Run.Help {
		if err := commandCatalog(env).WriteHelp(stdout, "root"); err != nil {
			fmt.Fprintf(stderr, "harness: help: %v\n", err)
			return ui.ExitRuntime
		}
		return ui.ExitOK
	}
	if result.Run.Version {
		fmt.Fprintln(stdout, buildinfo.Line("harness"))
		return ui.ExitOK
	}
	cfg := result.Config
	runOptions := result.Run
	jsonInformational := runOptions.ShowAgents || runOptions.ShowModels || runOptions.CheckModelProxy
	jsonRunMode := runOptions.OutputFormat == "json" && !runOptions.DebugRequest && !jsonInformational
	jsonRunStreamMode := ""
	if jsonRunMode {
		// The TTY REPL has no JSON mode: without -p the session must be driven by
		// NDJSON messages on piped stdin (interactive JSON sessions). These are
		// invocation errors, so they remain ordinary stderr usage errors.
		switch {
		case !runOptions.PromptSet && runOptions.InitialPromptSet:
			fmt.Fprintln(stderr, "harness: -i is not supported with -format json; use -p or pipe stdin")
			return ui.ExitUsage
		case !runOptions.PromptSet && !env.stdinPiped:
			fmt.Fprintln(stderr, "harness: -format json without -p: nothing to read JSON messages from; use -p or pipe stdin")
			return ui.ExitUsage
		case runOptions.PromptSet:
			jsonRunStreamMode = runstream.ModeOneshot
		default:
			jsonRunStreamMode = runstream.ModeInteractive
		}
	}

	rawStdout := stdout
	var startupDiagnostics *jsonRunDiagnostics
	jsonRunCommitted := false
	var startupCtx context.Context
	var stopStartup func()
	var startupInterrupted func() bool
	var startupWriteAbort <-chan struct{}
	var jsonInterrupts *jsonRunInterrupts
	if jsonRunMode {
		startupDiagnostics = &jsonRunDiagnostics{}
		stderr = startupDiagnostics
		// Start watching as soon as JSON mode is valid, including proxy and runtime
		// setup. Register Stop before the finalizer so the watcher remains live while
		// a startup_error write is in progress.
		jsonInterrupts = newJSONRunInterrupts(env.sigCh, now)
		startupCtx = jsonInterrupts.Context()
		startupWriteAbort = startupCtx.Done()
		stopStartup = jsonInterrupts.StopStartup
		startupInterrupted = jsonInterrupts.StartupInterrupted
		defer jsonInterrupts.Stop()
		defer func() {
			if jsonRunCommitted || exitCode == ui.ExitOK {
				return
			}
			diagnostic := startupDiagnostics.snapshotAndSeal()
			if diagnostic == "" {
				if exitCode == ui.ExitInterrupt {
					diagnostic = "startup interrupted"
				} else {
					diagnostic = "startup failed"
				}
			}
			_ = runstream.WriteStartupErrorWithAbort(rawStdout, runstream.StartupError{
				Mode:     jsonRunStreamMode,
				ExitCode: exitCode,
				Error:    diagnostic,
			}, startupWriteAbort)
		}()
	}

	configWritePath := result.ConfigPath
	if configWritePath == "" {
		configWritePath = filepath.Join(defaultConfigDir(getenv), "config.json")
	}
	explicitReasoningOutput := result.Sources["reasoning_summary"].Kind == configmeta.SourceFlag && cfg.ReasoningSummary != "" && cfg.ReasoningSummary != "none"
	suppressReasoningOutput := runOptions.Quiet && !explicitReasoningOutput
	var proxyTracer *tracing.Tracer
	if cfg.TraceProxy {
		var err error
		proxyTracer, err = tracing.NewTracer(true)
		if err != nil {
			fmt.Fprintf(stderr, "harness: trace proxy: %v\n", err)
			return ui.ExitRuntime
		}
	}
	proxyURL := cfg.ModelProxyURL
	if proxyURL == "" {
		proxyURL = protocol.DefaultURL
	}
	proxyClient, err := modelclient.New(proxyURL, nil, modelclient.WithAPIKey(cfg.ModelProxyAPIKey), modelclient.WithTracer(proxyTracer))
	if err != nil {
		fmt.Fprintf(stderr, "harness: %v\n", err)
		return ui.ExitUsage
	}
	if !jsonRunMode {
		startupCtx, stopStartup, startupInterrupted = signalCancelContext(env.sigCh)
		defer stopStartup()
	}
	if runOptions.ShowAgents || runOptions.ShowModels {
		var agents *agentsListOutput
		if runOptions.ShowAgents {
			var err error
			agents, err = buildAgentsListOutput(cfg)
			if err != nil {
				fmt.Fprintf(stderr, "harness: agents: %v\n", err)
				return ui.ExitUsage
			}
		}
		var models *modelsListOutput
		if runOptions.ShowModels {
			catalog, err := checkModelProxy(startupCtx, proxyClient)
			if err != nil {
				if startupInterrupted() || errors.Is(err, context.Canceled) {
					return ui.ExitInterrupt
				}
				fmt.Fprintf(stderr, "harness: model proxy: %v\n", err)
				return ui.ExitRuntime
			}
			if startupInterrupted() {
				return ui.ExitInterrupt
			}
			models = buildModelsListOutput(catalog)
		}
		if runOptions.OutputFormat == "json" {
			out := infoOutput{Version: 1}
			if agents != nil {
				out.DefaultAgent = agents.DefaultAgent
				out.SelectedAgent = agents.SelectedAgent
				out.Agents = agents.Agents
			}
			if models != nil {
				out.ProviderCount = models.ProviderCount
				out.ModelCount = models.ModelCount
				out.Models = sortedModelListEntries(models.Models)
			}
			if err := writeInformationalJSON(stdout, out); err != nil {
				fmt.Fprintf(stderr, "harness: info: %v\n", err)
				return ui.ExitRuntime
			}
			return ui.ExitOK
		}
		if agents != nil {
			fmt.Fprint(stdout, formatAgentsListText(*agents))
			if models != nil {
				fmt.Fprintln(stdout)
			}
		}
		if models != nil {
			fmt.Fprint(stdout, formatModelsListText(*models))
		}
		return ui.ExitOK
	}
	if runOptions.CheckModelProxy {
		catalog, err := checkModelProxy(startupCtx, proxyClient)
		if err != nil {
			if startupInterrupted() || errors.Is(err, context.Canceled) {
				return ui.ExitInterrupt
			}
			fmt.Fprintf(stderr, "harness: model proxy: %v\n", err)
			return ui.ExitRuntime
		}
		if startupInterrupted() {
			return ui.ExitInterrupt
		}
		if runOptions.OutputFormat == "json" {
			out := infoOutput{
				Version:       1,
				ModelProxyURL: proxyClient.URL(),
				ProviderCount: 0,
				ModelCount:    catalogModelCount(catalog),
			}
			if err := writeInformationalJSON(stdout, out); err != nil {
				fmt.Fprintf(stderr, "harness: model proxy: %v\n", err)
				return ui.ExitRuntime
			}
		} else {
			fmt.Fprintf(stdout, "model proxy ok: %s (%d targets)\n", proxyClient.URL(), catalogModelCount(catalog))
		}
		return ui.ExitOK
	}
	// The JSON run modes give stdout exclusively to the NDJSON event stream:
	// assistant text and reasoning flow as assistant_delta/reasoning_summary
	// events, so the human renderer's stdout path is muted by discarding the
	// coordinator stdout. Its stderr path uses startupDiagnostics and therefore
	// captures startup diagnostics before discarding all active-run display output.
	coordinatorOut := stdout
	if jsonRunMode {
		coordinatorOut = io.Discard
	}
	terminalOutput := ui.NewOutputCoordinator(coordinatorOut, stderr)
	stdout = terminalOutput.Stdout()
	stderr = terminalOutput.Stderr()

	// Lock a resumed session before reading or repairing any of its files. The
	// same lock remains active until the process exits or an interactive command
	// rotates to a new session directory.
	var activeSessionLock *session.Lock
	var pendingSessionLock *session.Lock
	var retiredSessionLocks []*session.Lock
	defer func() {
		if pendingSessionLock != nil {
			_ = pendingSessionLock.Close()
		}
		if activeSessionLock != nil {
			_ = activeSessionLock.Close()
		}
		for _, lock := range retiredSessionLocks {
			_ = lock.Close()
		}
	}()
	switchSessionLock := func(path string) error {
		next, err := session.AcquireLock(path)
		if err != nil {
			return err
		}
		previous := activeSessionLock
		activeSessionLock = next
		if previous != nil {
			_ = previous.Close()
		}
		return nil
	}
	prepareSessionLockChange := func(path string) error {
		if pendingSessionLock != nil {
			return errors.New("session: another path change is pending")
		}
		next, err := session.AcquireLock(path)
		if err != nil {
			return err
		}
		pendingSessionLock = next
		return nil
	}
	commitSessionLockChange := func() {
		if pendingSessionLock == nil {
			return
		}
		previous := activeSessionLock
		activeSessionLock = pendingSessionLock
		pendingSessionLock = nil
		if previous != nil {
			// Canceled background children can finish their final checkpoint after a
			// path rotation returns. Retain ownership of old roots until process exit
			// so they cannot race a resume of the prior session.
			retiredSessionLocks = append(retiredSessionLocks, previous)
		}
	}

	// Load a resumed session up front: its saved agent selects the tool set and
	// any agent-specific model target when no -agent flag overrides it.
	var resumed *session.Session
	if runOptions.Resume != "" {
		if err := switchSessionLock(runOptions.Resume); err != nil {
			fmt.Fprintf(stderr, "harness: resume %s: %v\n", runOptions.Resume, err)
			return ui.ExitRuntime
		}
		s, err := session.Load(runOptions.Resume)
		if err != nil {
			fmt.Fprintf(stderr, "harness: resume %s: %v\n", runOptions.Resume, err)
			return ui.ExitRuntime
		}
		resumed = &s
		if s.Recovery != nil {
			fmt.Fprintf(
				stderr,
				"[recovered active session boundary: %s, prompt %d, turn %d]\n",
				s.Recovery.Phase,
				s.Recovery.Prompt,
				s.Recovery.Turn,
			)
		}
		if s.RecoveryWarning != "" {
			fmt.Fprintf(stderr, "[ignored unreadable active-turn checkpoint: %s]\n", s.RecoveryWarning)
		}
	}
	startedAt := now()
	created := startedAt
	if resumed != nil && !resumed.Created.IsZero() {
		created = resumed.Created
	}
	if resumed != nil {
		abandoned, skipped, err := session.AbandonRunningChildren(runOptions.Resume, startedAt)
		if err != nil {
			fmt.Fprintf(stderr, "harness: resume child sessions: %v\n", err)
			return ui.ExitRuntime
		}
		if abandoned > 0 {
			fmt.Fprintf(stderr, "[marked %d interrupted child session(s) abandoned]\n", abandoned)
		}
		if skipped > 0 {
			fmt.Fprintf(stderr, "[skipped %d unreadable child session(s)]\n", skipped)
		}
	}
	sessionPath := runOptions.Session
	if sessionPath == "" {
		if runOptions.Resume != "" {
			sessionPath = runOptions.Resume
		} else {
			sessionPath = session.DefaultPath(stateDir(getenv), created)
		}
	}
	// Debug requests do not persist a new session. A debug request with -resume
	// still holds the source lock because loading it may perform recovery and child
	// cleanup. All ordinary runs lock their write destination before setup proceeds.
	if !runOptions.DebugRequest && (runOptions.Resume == "" || filepath.Clean(sessionPath) != filepath.Clean(runOptions.Resume)) {
		if err := switchSessionLock(sessionPath); err != nil {
			fmt.Fprintf(stderr, "harness: session %s: %v\n", sessionPath, err)
			return ui.ExitRuntime
		}
	}
	resumeCloned := false
	resumeCloneFrom := ""
	resumeCloneTo := ""
	if resumed != nil && runOptions.Session != "" && filepath.Clean(runOptions.Session) != filepath.Clean(runOptions.Resume) {
		cloneCreated := now()
		resumeCloneFrom = resumed.Tree.ActiveLeaf
		cloneCWD := resumed.CWD
		if current, cwdErr := os.Getwd(); cwdErr == nil {
			cloneCWD = current
		}
		cloneTree, err := resumed.Tree.Extract(resumeCloneFrom, cloneCreated, cloneCWD)
		if err != nil {
			fmt.Fprintf(stderr, "harness: clone resumed session: %v\n", err)
			return ui.ExitRuntime
		}
		resumeCloneTo, err = cloneTree.AppendBranch(resumeCloneFrom, resumeCloneFrom, resumeCloneFrom, "", "")
		if err != nil {
			fmt.Fprintf(stderr, "harness: clone resumed session: %v\n", err)
			return ui.ExitRuntime
		}
		cloneMessages, err := cloneTree.BuildContext()
		if err != nil {
			fmt.Fprintf(stderr, "harness: clone resumed session: %v\n", err)
			return ui.ExitRuntime
		}
		resumed.Tree = cloneTree
		resumed.Messages = cloneMessages
		resumed.Created = cloneCreated
		resumed.Updated = cloneCreated
		resumed.Prompt = 0
		resumed.ProxySessionID = ""
		resumed.CacheAffinityID = ""
		resumed.ResponseState = nil
		resumed.Usage = session.UsageTotals{}
		resumed.UsageByModel = nil
		created = cloneCreated
		resumeCloned = true
	}
	logger, diagnosticLogger, diagnosticsSink, err := newHarnessLogger(terminalOutput.Stderr(), cfg.LogLevel, sessionPath, !runOptions.DebugRequest)
	if err != nil {
		fmt.Fprintf(stderr, "harness: %v\n", err)
		return ui.ExitUsage
	}
	if len(runOptions.Images) > 0 && !runOptions.PromptSet && !runOptions.InitialPromptSet {
		fmt.Fprintln(stderr, "harness: -image requires -p one-shot mode or -i initial interactive prompt")
		return ui.ExitUsage
	}

	interactiveSession := !runOptions.PromptSet && !env.stdinPiped
	// machineInteractive: an embedding app drives the session with NDJSON
	// messages on piped stdin (design §10 interactive JSON mode). It shares
	// the interactive-only tool/steer wiring but not TTY-coupled behaviors
	// (goal auto-continue loop, startup pickers, prewarm, idle compaction).
	machineInteractive := runOptions.OutputFormat == "json" && env.stdinPiped && !runOptions.PromptSet
	agents, err := resolveConfiguredAgents(cfg)
	if err != nil {
		fmt.Fprintf(stderr, "harness: %v\n", err)
		return ui.ExitUsage
	}
	if interactiveSession || machineInteractive {
		enableInteractivePlanHandoff(agents)
	}
	agentName := cfg.Agent
	agentSource := result.Sources["agent"]
	agentExplicit := agentSource.Kind != configmeta.SourceDefault && agentSource.Kind != configmeta.SourceDerived
	if resumed != nil && resumed.Agent != "" {
		if !agentExplicit {
			agentName = resumed.Agent
		} else if cfg.Agent != resumed.Agent {
			fmt.Fprintf(stderr, "harness: session agent %q overridden by %q (%s %s wins)\n", resumed.Agent, cfg.Agent, agentSource.Kind, agentSource.Name)
		}
	}
	if agentName == "" {
		agentName = agentdef.Default
	}
	startupAgent, ok := agents[agentName]
	if !ok {
		fmt.Fprintf(stderr, "harness: unknown agent %q (available: %s)\n", agentName, strings.Join(agentdef.Names(agents), ", "))
		return ui.ExitUsage
	}

	catalog, err := proxyClient.Catalog(startupCtx)
	if err != nil {
		if startupInterrupted() || errors.Is(err, context.Canceled) {
			return ui.ExitInterrupt
		}
		fmt.Fprintf(stderr, "harness: model proxy: %v\n", err)
		return ui.ExitRuntime
	}
	modelRegistry := modelclient.Registry(catalog)
	modelRegistry.SetDefaultContextWindow(cfg.DefaultContextWindow)
	reasoning := llm.ReasoningConfig{
		Profile: cfg.Reasoning,
	}
	// A startup agent with a pinned reasoning profile sets the session base
	// profile (model compatibility is enforced by the validation below).
	if startupAgent.Reasoning != "" {
		reasoning.Profile = startupAgent.Reasoning
	}
	startProvider, startModel := agentModelInputs(startupAgent, cfg.Provider, cfg.Model)
	selection, err := resolveCatalogSelection(catalog, startProvider, startModel, cfg.Provider)
	if err != nil {
		configuredSelectionUnavailable := startModel != "" || startProvider != ""
		if configuredSelectionUnavailable && (runOptions.PromptSet || env.stdinPiped) {
			fmt.Fprintf(stderr, "harness: %v\n", err)
			return ui.ExitUsage
		}
		reader := bufio.NewReader(stdin)
		stdin = reader
		readStartupLine := func(prompt string) (string, error) {
			if _, err := fmt.Fprint(stderr, prompt); err != nil {
				return "", err
			}
			line, err := reader.ReadString('\n')
			if err != nil {
				if errors.Is(err, io.EOF) && line != "" {
					return strings.TrimSpace(line), nil
				}
				return "", err
			}
			return strings.TrimSpace(line), nil
		}
		if configuredSelectionUnavailable {
			fmt.Fprintf(stderr, "harness: %v\n", err)
			if _, err := readStartupLine("Press Enter to select a different model."); err != nil {
				fmt.Fprintf(stderr, "harness: model selection: %v\n", err)
				return ui.ExitUsage
			}
			fmt.Fprintln(stderr)
		}
		selection, err = pickStartupModel(readStartupLine, stderr, catalog, pickerPageSize(env))
		if err != nil {
			if errors.Is(err, ui.ErrPickerCancelled) {
				fmt.Fprintln(stderr, "harness: model selection cancelled")
			} else {
				fmt.Fprintf(stderr, "harness: model selection: %v\n", err)
			}
			return ui.ExitUsage
		}
		cfg.Provider = selection.Provider
		cfg.Model = selection.Model
		reasoning.Summary = effectiveReasoningSummary(cfg.ReasoningSummary, reasoningModeForProvider(catalog, selection.Provider), interactiveSession, suppressReasoningOutput)
		reasoning, err = pickStartupReasoningProfile(readStartupLine, stderr, modelRegistry, selection.RegistryModel, reasoning)
		if err != nil {
			if errors.Is(err, ui.ErrPickerCancelled) {
				fmt.Fprintln(stderr, "harness: model selection cancelled")
			} else {
				fmt.Fprintf(stderr, "harness: model selection: %v\n", err)
			}
			return ui.ExitUsage
		}
		if err := validateReasoningConfig(modelRegistry, selection.RegistryModel, reasoningModeForProvider(catalog, selection.Provider), reasoning); err != nil {
			fmt.Fprintf(stderr, "harness: %v\n", err)
			return ui.ExitUsage
		}
		saveDefault := false
		if !env.stdinPiped {
			saveDefault, err = ui.PromptSaveDefaultModel(readStartupLine, stderr, selection.Provider, selection.Model)
			if err != nil {
				if errors.Is(err, ui.ErrPickerCancelled) {
					fmt.Fprintln(stderr, "harness: default model save cancelled")
				} else {
					fmt.Fprintf(stderr, "harness: default model save: %v\n", err)
				}
				return ui.ExitUsage
			}
		}
		if saveDefault {
			configPath := configWritePath
			if err := saveSelectedModel(configPath, selection.Provider, selection.Model, reasoning); err != nil {
				fmt.Fprintf(stderr, "harness: save selected model: %v\n", err)
				return ui.ExitRuntime
			}
			fmt.Fprintf(stderr, "harness: saved selected model to %s\n", configPath)
		}
	}
	cfg.Provider = selection.Provider
	cfg.Model = selection.Model
	registryModel := selection.RegistryModel
	serverTools := webSearchServerToolsForModel(cfg.Provider, modelRegistry, registryModel, cfg.WebSearch)
	reasoning.Summary = effectiveReasoningSummary(cfg.ReasoningSummary, reasoningModeForProvider(catalog, selection.Provider), interactiveSession, suppressReasoningOutput)
	if err := validateReasoningConfig(modelRegistry, registryModel, reasoningModeForProvider(catalog, selection.Provider), reasoning); err != nil {
		fmt.Fprintf(stderr, "harness: %v\n", err)
		return ui.ExitUsage
	}

	// System prompt composition (design §8.5). -system-prompt may be an @file
	// reference. When set, it replaces only the static prompt; runtime sections
	// such as env, user/project AGENTS.md, skills, capability hints, and agent
	// prompts are still composed.
	configuredSystemPrompt, err := resolveAtFile(cfg.SystemPrompt)
	if err != nil {
		fmt.Fprintf(stderr, "harness: -system-prompt: %v\n", err)
		return ui.ExitUsage
	}
	cfg.SystemPrompt = configuredSystemPrompt
	// The env block must report the absolute working directory so the model can
	// reason about and resolve absolute file paths (design §8.5: `cwd:
	// /Users/twt/project`). Without an explicit Dir, EnvContext falls back to the
	// literal ".", which tells the agent its cwd is the string "." — useless for
	// path reasoning. An os.Getwd failure leaves Dir empty (the "." fallback), the
	// best we can do.
	wd, _ := os.Getwd()
	// AGENTS.md auto-discovery: include user-level instructions from
	// ~/.agents/AGENTS.md and project-specific instructions from the directory
	// harness was launched from. Missing files are silently ignored; a read error
	// on an existing file is fatal so the user isn't silently surprised.
	userAgentsPath := userAgentsMDPath(getenv)
	userAgentsMD, err := loadAgentsMDFile(userAgentsPath)
	if err != nil {
		fmt.Fprintf(stderr, "harness: %v\n", err)
		return ui.ExitRuntime
	}
	projectAgentsPath := projectAgentsMDPath(wd)
	projectAgentsMD, err := loadAgentsMDFile(projectAgentsPath)
	if err != nil {
		fmt.Fprintf(stderr, "harness: %v\n", err)
		return ui.ExitRuntime
	}
	warnLargeAgentsMD(stderr, cfg.AgentsMDWarnBytes, userAgentsPath, userAgentsMD)
	warnLargeAgentsMD(stderr, cfg.AgentsMDWarnBytes, projectAgentsPath, projectAgentsMD)
	// Skills discovery: scan project and user-level .agents/skills/ directories
	// for SKILL.md files, build a catalog for the system prompt, and surface
	// any warnings to stderr. Skills are disclosed via file-read activation so
	// the model uses its existing read tool to load them on demand.
	var skillWarnings skills.Warnings
	skillDirs := skills.AncestorSkillDirs(wd, homeDir(getenv))
	discoveredSkills := skills.Discover(skillDirs, &skillWarnings)
	for _, w := range skillWarnings {
		fmt.Fprintf(stderr, "skills: %s\n", w)
	}
	effectiveContextWindow := llm.EffectiveContextWindow(cfg.ContextWindow, modelRegistry.ContextWindow(registryModel))
	skillCatalogBudget := skills.CatalogBudget(effectiveContextWindow)
	skillsCatalog, skillCatalogReport := skills.BuildCatalogBudgeted(discoveredSkills, skillCatalogBudget)
	if skillCatalogReport.Omitted > 0 || skillCatalogReport.TruncatedCount > 0 {
		fmt.Fprintf(stderr, "skills: catalog budget omitted %d and truncated %d of %d skills\n",
			skillCatalogReport.Omitted, skillCatalogReport.TruncatedCount, skillCatalogReport.Total)
	}
	var runtimeHints []string
	if ripgrepAvailable() {
		runtimeHints = append(runtimeHints, rgSystemHint)
	}
	var lspHint string

	// buildSystem assembles the full system prompt for a given agent prompt,
	// reusing every other input. The configured system prompt replaces only the
	// static built-in instructions. Runtime hints precede the agent prompt so the
	// selected agent's instructions remain the final layer.
	buildSystem := func(agentPrompt string) string {
		hints := slices.Clone(runtimeHints)
		if strings.TrimSpace(lspHint) != "" {
			hints = append(hints, lspHint)
		}
		return sysprompt.Build(sysprompt.Options{
			StaticPrompt:    configuredSystemPrompt,
			NoEnv:           cfg.NoEnv,
			UserAgentsMD:    userAgentsMD,
			ProjectAgentsMD: projectAgentsMD,
			SkillsCatalog:   skillsCatalog,
			RuntimeHints:    hints,
			AgentPrompt:     agentPrompt,
			Env:             sysprompt.EnvOptions{Dir: wd},
		})
	}

	backgroundManager := background.NewManager(background.Options{
		MaxContextBytes: cfg.ToolResultMaxBytes,
		Now:             now,
	})

	// Agent definitions (tool-gating layer). The tool catalog holds every
	// constructible tool; each agent selects a subset, realized by Subset so the
	// runtime advertises and dispatches only that agent's tools. Built once and
	// shared with /agent and the /mode alias (write_tmp_file holds a per-run temp dir).
	toolCatalog, disabledTools := tools.CatalogWithOptions(tools.Options{
		MaxResultBytes:                cfg.ToolResultMaxBytes,
		MaxResultLines:                cfg.ToolResultMaxLines,
		ReadDefaultLimit:              cfg.ReadDefaultLimit,
		ReadResultBytes:               cfg.ReadResultMaxBytes,
		ReadResultLines:               cfg.ReadResultMaxLines,
		Background:                    backgroundManager,
		DispatchTimeout:               time.Duration(cfg.ToolTimeoutSeconds) * time.Second,
		ShellTimeoutSeconds:           cfg.ShellTimeoutSeconds,
		ShellBackgroundTimeoutSeconds: cfg.ShellBackgroundTimeoutSeconds,
	})
	backgroundManager.SetResultPreparer(toolCatalog.PrepareResultWithOriginal)
	for _, disabled := range disabledTools {
		logger.Warn(disabled.Message(), logging.Category("cli_tools"))
	}
	build := buildinfo.Current()
	sessionBuild := session.BuildMetadata{
		Version:  build.Version,
		Commit:   build.Commit,
		Date:     build.Date,
		Modified: build.Modified,
	}
	sessionRuntime := session.RuntimeProfile{
		RetentionPolicy:           cfg.RetentionPolicy,
		ContextWindow:             cfg.ContextWindow,
		ToolResultMaxBytes:        cfg.ToolResultMaxBytes,
		ToolResultMaxLines:        cfg.ToolResultMaxLines,
		CompactToolResultMaxBytes: cfg.CompactToolResultMaxBytes,
		CompactTimeoutSeconds:     cfg.CompactTimeoutSeconds,
		ResponsesStateful:         responsesStatefulForProvider(cfg, catalog, cfg.Provider),
		DelegateMaxTurns:          cfg.DelegateMaxTurns,
		DelegateMaxActive:         cfg.DelegateMaxActive,
		DelegateMaxDescendants:    cfg.DelegateMaxDescendants,
		Prewarm:                   env.prewarmCache && !env.stdinPiped && prewarmForProvider(catalog, cfg.Provider),
		SearchBackend:             searchBackend(),
	}
	delegateState := delegate.NewState(delegate.Runtime{
		ProviderName:          cfg.Provider,
		Model:                 cfg.Model,
		ReasoningReplayDomain: selection.ReasoningReplayDomain,
		ContextWindow:         cfg.ContextWindow,
		MaxOutputTokens:       cfg.MaxOutputTokens,
		Registry:              modelRegistry,
		Reasoning:             reasoning,
		ServerTools:           serverTools,
		ResponsesStateful:     responsesStatefulForProvider(cfg, catalog, cfg.Provider),
		Agent:                 agentName,
		SessionPath:           sessionPath,
		Depth:                 0,
		MaxPromptTokens:       cfg.MaxPromptTokens,
		MaxPromptCostUSD:      cfg.MaxPromptCostUSD,
		Build:                 sessionBuild,
		RuntimeProfile:        sessionRuntime,
	})
	// pendingMCP is assigned below (interactive REPL only) before any turn can run,
	// so this closure — invoked lazily at delegation time — captures the live value.
	// It lets the delegate launch tolerate not-yet-discovered mcp__ tools exactly as
	// startup does, instead of failing Subset during the async discovery window.
	var pendingMCP *asyncMCPRegistration
	resolveDelegate := func(runtime delegate.Runtime, name string) (delegate.Launch, error) {
		return resolveDelegateLaunch(runtime, name, agents, toolCatalog, pendingMCP, catalog, proxyClient, buildSystem, cfg)
	}
	var delegateActivity *delegate.ActivityRegistry
	var delegateFeed *delegate.ActivityFeed
	if !runOptions.Quiet {
		switch cfg.DelegateOutput {
		case config.DelegateOutputStatus:
			delegateActivity = delegate.NewActivityRegistry(nil)
		case config.DelegateOutputLines:
			delegateFeed = delegate.NewActivityFeed()
			delegateActivity = delegate.NewActivityRegistry(delegateFeed)
		}
	}
	// The tmux delegate viewer is display-only: construction degrades to nil
	// with one warning outside tmux, and the runner swallows every open
	// failure. Shutdown (nil-safe) kills any lingering windows at exit.
	var childViewer *tmux.Viewer
	if cfg.DelegateTmux {
		delegateTmuxSource := result.Sources["delegate_tmux"]
		delegateTmuxExplicit := delegateTmuxSource.Kind == configmeta.SourceFlag || delegateTmuxSource.Kind == configmeta.SourceEnvironment || delegateTmuxSource.Kind == configmeta.SourceFile
		childViewer = setupDelegateTmuxViewer(cfg, getenv, stderr, logger, delegateTmuxExplicit, runOptions.Quiet)
		defer childViewer.Shutdown()
	}
	todoStore := todo.NewStore()
	planStore := plan.NewStore()
	delegateOpts := delegate.Options{
		MaxTurns:                  cfg.DelegateMaxTurns,
		MaxDepth:                  cfg.DelegateMaxDepth,
		MaxActiveDescendants:      cfg.DelegateMaxActive,
		MaxTotalDescendants:       cfg.DelegateMaxDescendants,
		CompactKeepTurns:          cfg.CompactKeepTurns,
		CompactKeepTokens:         cfg.CompactKeepTokens,
		CompactTriggerPercent:     cfg.CompactTriggerPercent,
		CompactTargetPercent:      cfg.CompactTargetPercent,
		DisableAutoCompaction:     !cfg.CompactAutoEnabled,
		CompactSummaryMaxTokens:   cfg.CompactSummaryMaxTokens,
		CompactTimeout:            time.Duration(cfg.CompactTimeoutSeconds) * time.Second,
		CompactToolResultMaxBytes: cfg.CompactToolResultMaxBytes,
		RetentionKeepTurns:        cfg.RetentionKeepTurns,
		RetentionResultHeadBytes:  cfg.RetentionResultHeadBytes,
		RetentionPolicy:           agent.RetentionPolicy(cfg.RetentionPolicy),
		ShowDiffs:                 cfg.ShowDiffs,
		Now:                       now,
		AgentCandidates: func(delegate.Runtime) []delegate.AgentCandidate {
			return delegateAgentCandidates(agents)
		},
		ActivityRegistry: delegateActivity,
	}
	if cfg.DelegateTmux {
		// The closure stays non-nil even when setup degraded to a nil viewer:
		// a mid-run Shutdown then quietly stops new windows instead of
		// re-arming the feature.
		delegateOpts.OpenChildView = func(start delegate.ChildView) (delegate.ChildViewHandle, error) {
			return childViewer.Open(tmux.View{Name: start.Name, Dir: start.Dir, Log: start.Log})
		}
	}
	delegateRunner := delegate.NewRunner(delegateState.Snapshot, resolveDelegate, delegateOpts)
	toolCatalog.Register(delegate.NewTool(delegateRunner, backgroundManager))
	toolCatalog.Register(background.NewJobsTool(backgroundManager))
	handoffPending := handoff.NewPending()
	toolCatalog.Register(todo.NewTool(todoStore))
	toolCatalog.Register(plan.NewTool(planStore, func() string { return delegateState.Snapshot().SessionPath }))
	names := agentdef.ImplementationAgentNames(agents)
	if len(names) == 0 {
		names = []string{"auto"}
	}
	toolCatalog.Register(tools.NewHandoff(handoffPending, planStore, interactiveSession || machineInteractive, names))
	// Goals are managed exclusively by the interactive /goal command.
	goalStore := goal.NewStore()
	// MCP (opt-in): one-shot runs synchronously so the single request can use MCP
	// tools immediately. Interactive REPL starts remote HTTP discovery in the
	// background and applies discovered tools at a prompt boundary, so an
	// unreachable proxy never delays launch. MCP never fails startup; on any error
	// it warns and continues with no MCP tools.
	var mcpConn *mcptools.Conn
	var mcpSummary mcptools.Summary
	if cfg.MCP.Enable {
		if runOptions.PromptSet {
			conn, summary, cleanup, ok := setupMCP(startupCtx, cfg.MCP, toolCatalog, logger, proxyTracer)
			defer cleanup()
			if startupInterrupted() {
				return ui.ExitInterrupt
			}
			if ok {
				mcpConn, mcpSummary = conn, summary
			}
		} else {
			conn, pending, cleanup, ok := setupMCPAsync(cfg.MCP, logger, proxyTracer)
			defer cleanup()
			if ok {
				mcpConn, pendingMCP = conn, pending
			}
		}
	}
	// Local MCP service: when explicitly enabled, harness spawns the configured
	// local stdio MCP child and registers its tools too. Its surface is static,
	// so it needs no live refresh: the refresh hook below is wired only to the
	// HTTP conn, whose transport never pushes list_changed.
	var localSummary mcptools.Summary
	if localMCPEnabled(cfg.MCP.Local, runOptions.PromptSet) {
		_, summary, cleanup, ok := setupLocalMCP(startupCtx, cfg.MCP.Local, cfg.MCP.Local.EnableSet, toolCatalog, logger)
		defer cleanup()
		if startupInterrupted() {
			return ui.ExitInterrupt
		}
		if ok {
			localSummary = summary
		}
	}
	var lspSummary mcptools.Summary
	var lspControl *lspRuntime
	// Interactive runs prepare the static LSP surface even when it starts
	// disabled, so /lsp enable can expose it without restarting. Language-server
	// processes remain lazy and no binary is launched here.
	if cfg.LSP.Enable || !runOptions.PromptSet {
		runtime, err := newLSPRuntime(startupCtx, cfg.LSP, toolCatalog, logger)
		if startupInterrupted() {
			return ui.ExitInterrupt
		}
		if err != nil {
			logger.Warn(fmt.Sprintf("lsp: cannot initialize: %v; LSP tools unavailable", err), logging.Category("lsp"))
		} else {
			lspControl = runtime
			defer runtime.Shutdown()
			lspSummary = runtime.ActiveSummary()
			lspHint = runtime.SystemHint()
			if runtime.enabled {
				logger.Info(fmt.Sprintf("lsp: registered %d tools", runtime.summary.Total), logging.Category("lsp"))
			}
		}
	}
	var serenaSummary mcptools.Summary
	if cfg.LSP.Serena.Enable {
		summary, cleanup, ok := setupSerena(startupCtx, cfg.LSP.Serena, toolCatalog, logger)
		defer cleanup()
		if startupInterrupted() {
			return ui.ExitInterrupt
		}
		if ok {
			serenaSummary = summary
			if summary.Total > 0 {
				runtimeHints = append(runtimeHints, serenaSystemHint)
			}
		}
	}
	// Agent mcp_tools policy controls automatic exposure for discovered external
	// tools: disabled, read_only, or all. LSP tools are first-class but still use
	// this read-only exposure gate so whitelist agents stay locked down. Capture
	// MCP-exposing agents BEFORE augmenting them, so the refresh hook can
	// re-derive their allowed lists.
	// The name lists are empty when MCP/LSP are disabled, making this a no-op.
	mcpBases := mcpExposingAgentBases(agents)
	lspExplicit := captureLSPExplicitTools(agents, mcpBases)
	// Cap the discovered remote MCP surface before combining with local MCP and
	// LSP names (those have their own gating). The same limits feed the interactive
	// refresh hook below so async-discovered tools are capped identically.
	mcpLim := mcpLimitsFromConfig(cfg.MCP)
	cappedMCPNames, cappedMCPReadOnly := capRemoteMCPNames(mcpSummary.Names, mcpSummary.ReadOnlyNames, mcpLim, logger)
	mcpNames := make([]string, 0, len(cappedMCPNames)+len(localSummary.Names)+len(serenaSummary.Names))
	mcpNames = append(mcpNames, cappedMCPNames...)
	mcpNames = append(mcpNames, localSummary.Names...)
	mcpNames = append(mcpNames, serenaSummary.Names...)
	mcpReadOnlyNames := make([]string, 0, len(cappedMCPReadOnly)+len(localSummary.ReadOnlyNames)+len(serenaSummary.ReadOnlyNames))
	mcpReadOnlyNames = append(mcpReadOnlyNames, cappedMCPReadOnly...)
	mcpReadOnlyNames = append(mcpReadOnlyNames, localSummary.ReadOnlyNames...)
	mcpReadOnlyNames = append(mcpReadOnlyNames, serenaSummary.ReadOnlyNames...)
	augmentAgentsWithMCP(agents, mcpNames, mcpReadOnlyNames)
	if lspControl != nil {
		applyLSPExposure(agents, lspControl.summary, lspControl.enabled, lspExplicit)
	} else {
		applyLSPExposure(agents, mcptools.Summary{}, false, lspExplicit)
	}
	// Expand @file references in agent prompts once at startup: a bad reference
	// fails fast (rather than on a later /agent switch), and the cached text means
	// switching never touches the filesystem.
	for name, a := range agents {
		expanded, err := resolveAtFile(a.Prompt)
		if err != nil {
			fmt.Fprintf(stderr, "harness: agent %q prompt: %v\n", name, err)
			return ui.ExitUsage
		}
		a.Prompt = expanded
		agents[name] = a
	}

	currentAgent, ok := agents[agentName]
	if !ok {
		fmt.Fprintf(stderr, "harness: unknown agent %q (available: %s)\n", agentName, strings.Join(agentdef.Names(agents), ", "))
		return ui.ExitUsage
	}
	toolRegistry, err := subsetForAgentTools(toolCatalog, currentAgent.AllowedTools, pendingMCP)
	if err != nil {
		fmt.Fprintf(stderr, "harness: agent %q: %v\n", agentName, err)
		return ui.ExitUsage
	}
	systemPrompt := buildSystem(currentAgent.Prompt)
	var hookRunner *hooks.Runner
	if !cfg.Hooks.Empty() {
		hookRunner = &hooks.Runner{
			Config: cfg.Hooks,
			CWD:    wd,
			Model:  cfg.Model,
		}
	}

	switchAgent := func(name string) (ui.AgentSelection, error) {
		a, ok := agents[name]
		if !ok {
			return ui.AgentSelection{}, fmt.Errorf("unknown agent %q (available: %s)", name, strings.Join(agentdef.Names(agents), ", "))
		}
		reg, err := subsetForAgentTools(toolCatalog, a.AllowedTools, pendingMCP)
		if err != nil {
			return ui.AgentSelection{}, err
		}
		snap := delegateState.Snapshot()
		next, err := resolveAgentCatalogSelection(catalog, a, snap.ProviderName, snap.Model)
		if err != nil {
			return ui.AgentSelection{}, err
		}
		mode := reasoningModeForProvider(catalog, next.Provider)
		nextReasoning := compatibleReasoningForModel(modelRegistry, next.RegistryModel, mode, agentBaseReasoning(a, reasoning))
		if nextReasoning.Summary == "" && cfg.ReasoningSummary == "" {
			nextReasoning.Summary = effectiveReasoningSummary(cfg.ReasoningSummary, mode, interactiveSession, suppressReasoningOutput)
		}
		if err := validateReasoningConfig(modelRegistry, next.RegistryModel, mode, nextReasoning); err != nil {
			return ui.AgentSelection{}, err
		}
		system := buildSystem(a.Prompt)
		runtime := proxyClient.Provider(next.Provider)
		snap.Provider = runtime
		snap.ProviderName = next.Provider
		snap.Model = next.Model
		snap.ReasoningReplayDomain = next.ReasoningReplayDomain
		snap.ContextWindow = cfg.ContextWindow
		snap.MaxOutputTokens = cfg.MaxOutputTokens
		snap.System = system
		snap.Reasoning = nextReasoning
		snap.ServerTools = webSearchServerToolsForModel(next.Provider, modelRegistry, next.RegistryModel, cfg.WebSearch)
		snap.ResponsesStateful = responsesStatefulForProvider(cfg, catalog, next.Provider)
		snap.Agent = a.Name
		snap.ToolNames = reg.Names()
		delegateState.Set(snap)
		return ui.AgentSelection{
			Name:                  a.Name,
			Tools:                 reg,
			System:                system,
			Provider:              next.Provider,
			Model:                 next.Model,
			RegistryModel:         next.RegistryModel,
			BaseURL:               proxyClient.URL(),
			Runtime:               runtime,
			ContextWindow:         cfg.ContextWindow,
			Reasoning:             nextReasoning,
			BaseTargetID:          next.BaseTargetID,
			ReasoningReplayDomain: next.ReasoningReplayDomain,
			Variant:               next.Variant,
			FastTargetID:          next.FastTargetID,
			ServerTools:           snap.ServerTools,
			ReasoningSet:          true,
			ResponsesStateful:     snap.ResponsesStateful,
		}, nil
	}

	var controlLSP func(action, agentName string) (ui.LSPSelection, error)
	if lspControl != nil && !runOptions.PromptSet {
		controlLSP = func(action, currentAgentName string) (ui.LSPSelection, error) {
			wantEnabled := lspControl.enabled
			switch action {
			case "status":
				previousHint := lspHint
				status := lspControl.Status()
				lspHint = lspControl.SystemHint()
				selection := ui.LSPSelection{Status: status}
				if lspHint != previousHint {
					if definition, ok := agents[currentAgentName]; ok {
						selection.System = buildSystem(definition.Prompt)
						snapshot := delegateState.Snapshot()
						snapshot.System = selection.System
						delegateState.Set(snapshot)
					}
				}
				return selection, nil
			case "enable":
				wantEnabled = true
			case "disable":
				wantEnabled = false
			default:
				return ui.LSPSelection{}, fmt.Errorf("unknown action %q", action)
			}
			previousEnabled := lspControl.enabled
			previousHint := lspHint
			previousSummary := lspSummary
			changed := wantEnabled != previousEnabled
			lspControl.SetEnabled(wantEnabled)
			lspSummary = lspControl.ActiveSummary()
			lspHint = lspControl.SystemHint()
			applyLSPExposure(agents, lspControl.summary, lspControl.enabled, lspExplicit)
			selection := ui.LSPSelection{}
			if !changed {
				selection.Status = lspControl.Status()
				return selection, nil
			}
			definition, ok := agents[currentAgentName]
			if !ok {
				lspControl.SetEnabled(previousEnabled)
				lspHint, lspSummary = previousHint, previousSummary
				applyLSPExposure(agents, lspControl.summary, previousEnabled, lspExplicit)
				return ui.LSPSelection{}, fmt.Errorf("unknown agent %q", currentAgentName)
			}
			registry, err := subsetForAgentTools(toolCatalog, definition.AllowedTools, pendingMCP)
			if err != nil {
				lspControl.SetEnabled(previousEnabled)
				lspHint, lspSummary = previousHint, previousSummary
				applyLSPExposure(agents, lspControl.summary, previousEnabled, lspExplicit)
				return ui.LSPSelection{}, err
			}
			selection.Tools = registry
			selection.System = buildSystem(definition.Prompt)
			selection.Status = lspControl.Status()
			snapshot := delegateState.Snapshot()
			snapshot.ToolNames = registry.Names()
			snapshot.System = selection.System
			delegateState.Set(snapshot)
			return selection, nil
		}
	}

	provider := proxyClient.Provider(cfg.Provider)

	switchModel := func(input string, nextReasoning llm.ReasoningConfig) (ui.ModelSelection, error) {
		input = strings.TrimSpace(input)
		if input == "" {
			return ui.ModelSelection{}, fmt.Errorf("model is required")
		}
		next, err := resolveCatalogSelection(catalog, "", input, cfg.Provider)
		if err != nil {
			// Near miss: fall back to the picker's prefix/substring matcher over
			// the catalog's model ids (r24). A unique match switches; several
			// list the candidates; none keeps the original error.
			match, candidates := fuzzyMatchModel(catalog, input)
			if match == "" {
				if len(candidates) > 1 {
					return ui.ModelSelection{}, fmt.Errorf("%q matched no model exactly; did you mean: %s", input, strings.Join(candidates, ", "))
				}
				return ui.ModelSelection{}, err
			}
			next, err = resolveCatalogSelection(catalog, "", match, cfg.Provider)
			if err != nil {
				return ui.ModelSelection{}, err
			}
		}
		mode := reasoningModeForProvider(catalog, next.Provider)
		if nextReasoning.Summary == "" && cfg.ReasoningSummary == "" {
			nextReasoning.Summary = effectiveReasoningSummary(cfg.ReasoningSummary, mode, interactiveSession, suppressReasoningOutput)
		}
		nextReasoning = compatibleReasoningForModel(modelRegistry, next.RegistryModel, mode, nextReasoning)
		if err := validateReasoningConfig(modelRegistry, next.RegistryModel, mode, nextReasoning); err != nil {
			return ui.ModelSelection{}, err
		}
		runtime := proxyClient.Provider(next.Provider)
		snap := delegateState.Snapshot()
		snap.Provider = runtime
		snap.ProviderName = next.Provider
		snap.Model = next.Model
		snap.ReasoningReplayDomain = next.ReasoningReplayDomain
		snap.ContextWindow = cfg.ContextWindow
		snap.MaxOutputTokens = cfg.MaxOutputTokens
		snap.Reasoning = nextReasoning
		snap.ServerTools = webSearchServerToolsForModel(next.Provider, modelRegistry, next.RegistryModel, cfg.WebSearch)
		snap.ResponsesStateful = responsesStatefulForProvider(cfg, catalog, next.Provider)
		delegateState.Set(snap)
		reasoning = nextReasoning
		serverTools = snap.ServerTools
		return ui.ModelSelection{
			Provider:              next.Provider,
			Model:                 next.Model,
			RegistryModel:         next.RegistryModel,
			BaseURL:               proxyClient.URL(),
			Runtime:               runtime,
			ContextWindow:         cfg.ContextWindow,
			Reasoning:             nextReasoning,
			BaseTargetID:          next.BaseTargetID,
			ReasoningReplayDomain: next.ReasoningReplayDomain,
			Variant:               next.Variant,
			FastTargetID:          next.FastTargetID,
			ServerTools:           snap.ServerTools,
			ReasoningSet:          true,
			ResponsesStateful:     snap.ResponsesStateful,
		}, nil
	}

	ag := agent.New(provider, toolRegistry, agent.Options{
		MaxTurns:                  cfg.MaxTurns,
		MaxPromptTokens:           cfg.MaxPromptTokens,
		MaxOutputTokens:           cfg.MaxOutputTokens,
		MaxPromptCostUSD:          cfg.MaxPromptCostUSD,
		Model:                     cfg.Model,
		ContextWindow:             cfg.ContextWindow,
		Registry:                  modelRegistry,
		Reasoning:                 reasoning,
		ReasoningReplayDomain:     selection.ReasoningReplayDomain,
		ServerTools:               serverTools,
		Now:                       now,
		CompactKeepTurns:          cfg.CompactKeepTurns,
		CompactKeepTokens:         cfg.CompactKeepTokens,
		CompactTriggerPercent:     cfg.CompactTriggerPercent,
		CompactTargetPercent:      cfg.CompactTargetPercent,
		DisableAutoCompaction:     !cfg.CompactAutoEnabled,
		CompactSummaryMaxTokens:   cfg.CompactSummaryMaxTokens,
		CompactTimeout:            time.Duration(cfg.CompactTimeoutSeconds) * time.Second,
		CompactToolResultMaxBytes: cfg.CompactToolResultMaxBytes,
		Hooks:                     hookRunner,
		ShowDiffs:                 cfg.ShowDiffs,
		ResponsesStateful:         responsesStatefulForProvider(cfg, catalog, cfg.Provider),
		RetentionPolicy:           agent.RetentionPolicy(cfg.RetentionPolicy),
		RetentionFloorTokens:      cfg.RetentionFloorTokens,
		RetentionKeepTurns:        cfg.RetentionKeepTurns,
		RetentionResultHeadBytes:  cfg.RetentionResultHeadBytes,
		Interactive:               interactiveSession,
		Steer:                     !cfg.NoSteer,
	})
	if env.agentSleep != nil {
		ag.SetSleep(env.agentSleep)
	}

	var totals session.UsageTotals
	var resumedUsageByModel map[string]session.UsageTotals
	var resumeResponseState *llm.ResponseState

	// Resume restores a prior transcript; flags win over the file's
	// provider/model with a warning (design §11). The agent was resolved above;
	// the tool registry already reflects it.
	if resumed != nil {
		s := *resumed
		if s.Provider != "" && s.Provider != cfg.Provider {
			fmt.Fprintf(stderr, "harness: session provider %q overridden by %q (flags win)\n", s.Provider, cfg.Provider)
		}
		if s.Model != "" && s.Model != cfg.Model {
			fmt.Fprintf(stderr, "harness: session model %q overridden by %q (flags win)\n", s.Model, cfg.Model)
		}
		ag.SetTranscript(s.Messages)
		todoStore.Restore(s.Todos)
		planStore.Replace(s.Plan)
		goalStore.Restore(s.Goal)
		resumedUsageByModel = s.UsageByModel
		totals = s.Usage
		// A resumed session keeps its saved full system prompt unless a static
		// system_prompt override is set.
		if cfg.SystemPrompt == "" && s.System != "" {
			systemPrompt = s.System
		}
		if sessionResponseStateCompatible(cfg, catalog, s, cfg.Provider, cfg.Model) {
			resumeResponseState = s.ResponseState
		}
		ag.SetProxySessionID(s.ProxySessionID)
		ag.SetCacheAffinityID(s.CacheAffinityID)
	}
	ag.SetSystem(systemPrompt)
	activeToolNames := toolRegistry.Names()

	delegateState.Set(delegate.Runtime{
		Provider:              provider,
		ProviderName:          cfg.Provider,
		Model:                 cfg.Model,
		ContextWindow:         cfg.ContextWindow,
		MaxOutputTokens:       cfg.MaxOutputTokens,
		Registry:              modelRegistry,
		Reasoning:             reasoning,
		ReasoningReplayDomain: selection.ReasoningReplayDomain,
		ServerTools:           serverTools,
		ResponsesStateful:     responsesStatefulForProvider(cfg, catalog, cfg.Provider),
		System:                systemPrompt,
		Agent:                 agentName,
		ToolNames:             activeToolNames,
		SessionPath:           sessionPath,
		CacheAffinityID:       ag.CacheAffinityID(),
		Depth:                 0,
		MaxPromptTokens:       cfg.MaxPromptTokens,
		MaxPromptCostUSD:      cfg.MaxPromptCostUSD,
		Build:                 sessionBuild,
		RuntimeProfile:        sessionRuntime,
	})
	if hookRunner != nil {
		hookRunner.SetSession(sessionPath)
	}
	// The delegate tool schema reads delegateState.ToolNames. Agent construction
	// cached tool specs before delegateState had the final runtime, so refresh the
	// same registry after the runtime is seeded.
	ag.SetTools(toolRegistry)
	ag.SetResponseState(resumeResponseState)

	if runOptions.DebugRequest {
		includePrompt, prompt, images, err := debugRequestPrompt(runOptions, stdin, env.stdinPiped, modelRegistry.SupportsInputModality(registryModel, "image"))
		if err != nil {
			fmt.Fprintf(stderr, "harness: debug request: %v\n", err)
			return ui.ExitRuntime
		}
		if len(runOptions.Images) > 0 && len(images) == 0 {
			fmt.Fprintf(stderr, "[image skipped: model %s does not support image input]\n", registryModel)
		}
		out := buildDebugRequestOutput(ag, cfg, registryModel, agentName, includePrompt, prompt, images)
		if err := writeInformationalJSON(stdout, out); err != nil {
			fmt.Fprintf(stderr, "harness: debug request: %v\n", err)
			return ui.ExitRuntime
		}
		return ui.ExitOK
	}

	color := !cfg.NoColor && env.colorTTY
	renderer := ui.NewRenderer(stdout, stderr, ui.RenderOptions{
		Output:     terminalOutput,
		Color:      color,
		ColorTheme: highlightColorTheme(cfg.ColorTheme),
		Markdown:   env.colorTTY,
		Verbose:    cfg.Verbose,
		ToolStream: cfg.ToolStream,
		Quiet:      runOptions.Quiet,
		// -quiet still prints the single per-prompt cost line on a TTY (r25);
		// a piped -quiet run stays fully silent for scripting.
		SuppressUsage:           runOptions.Quiet && !env.colorTTY,
		SuppressReasoningOutput: suppressReasoningOutput,
		// The in-place wait counter and during-prompt input line need a TTY; the
		// renderer also gates them off under -quiet (r12 + during-prompt input).
		LiveStatus:               env.colorTTY,
		DelegateActivity:         delegateActivity,
		DelegateFeed:             delegateFeed,
		DisableDelegateStatus:    cfg.DelegateOutput == config.DelegateOutputOff,
		Model:                    registryModel,
		Registry:                 modelRegistry,
		CompactionTriggerPercent: cfg.CompactTriggerPercent,
		DisableAutoCompaction:    !cfg.CompactAutoEnabled,
		Now:                      now,
		TimestampLayout:          timestampLayout(cfg.TimestampMode),
		Width:                    env.terminalCols,
	})

	app := &ui.App{
		Agent:                  ag,
		Renderer:               renderer,
		Out:                    terminalOutput.Stdout(),
		Errw:                   terminalOutput.Stderr(),
		Logger:                 logger,
		DiagnosticLogger:       diagnosticLogger,
		Provider:               cfg.Provider,
		Model:                  cfg.Model,
		RegistryModel:          registryModel,
		BaseURL:                proxyClient.URL(),
		Registry:               modelRegistry,
		System:                 systemPrompt,
		Reasoning:              reasoning,
		BaseTargetID:           selection.BaseTargetID,
		ReasoningReplayDomain:  selection.ReasoningReplayDomain,
		Variant:                selection.Variant,
		FastTargetID:           selection.FastTargetID,
		ImageDetail:            cfg.ImageDetail,
		Hooks:                  hookRunner,
		Background:             backgroundManager,
		AvailableModels:        modelRegistry.Models(),
		SwitchModel:            switchModel,
		PickModel:              catalogModelPicker(catalog),
		PickerPageSize:         pickerPageSize(env),
		PromptDefaultModelSave: !env.stdinPiped,
		SetReasoning: func(model string, nextReasoning llm.ReasoningConfig) error {
			providerName := providerForReasoningModel(catalog, delegateState.Snapshot().ProviderName, model)
			if err := validateReasoningConfig(modelRegistry, model, reasoningModeForProvider(catalog, providerName), nextReasoning); err != nil {
				return err
			}
			reasoning = nextReasoning
			snap := delegateState.Snapshot()
			snap.Reasoning = nextReasoning
			delegateState.Set(snap)
			return nil
		},
		SaveDefaultModel: func(provider, model string, reasoning llm.ReasoningConfig) error {
			return saveSelectedModel(configWritePath, provider, model, reasoning)
		},
		SaveReplEditMode: func(mode string) error {
			return config.SaveReplEditMode(configWritePath, mode)
		},
		AgentName:       agentName,
		AvailableAgents: agentSummaries(agents, activeToolNames),
		RefreshAgentSummaries: func() []ui.AgentSummary {
			return agentSummaries(agents, delegateState.Snapshot().ToolNames)
		},
		SwitchAgent:                  switchAgent,
		ControlLSP:                   controlLSP,
		Todos:                        todoStore,
		Plans:                        planStore,
		Goal:                         goalStore,
		GoalMaxContinuations:         cfg.GoalMaxContinuations,
		GoalAutoContinue:             interactiveSession,
		Handoff:                      handoffPending,
		HandoffAgent:                 cfg.HandoffAgent,
		IdleCompactionAfter:          time.Duration(cfg.CompactIdleAfterSeconds) * time.Second,
		IdleCompactionTriggerPercent: cfg.CompactIdleTriggerPercent,
		SessionPath:                  sessionPath,
		SessionBuild:                 sessionBuild,
		SessionRuntime:               sessionRuntime,
		SessionTree: func() *session.Tree {
			if resumed != nil {
				return resumed.Tree
			}
			return nil
		}(),
		StateDir:                stateDir(getenv),
		Created:                 created,
		Now:                     now,
		BeforeSessionPathChange: prepareSessionLockChange,
		OnSessionPathChanged: func(path string) {
			defer commitSessionLockChange()
			snap := delegateState.Snapshot()
			snap.SessionPath = path
			snap.CacheAffinityID = ag.CacheAffinityID()
			delegateState.Set(snap)
			if diagnosticsSink != nil {
				diagnosticsSink.SetDir(path)
			}
		},
		OnPromptFinished: env.promptFinished,
		Prompt:           cfg.ReplPrompt,
		PromptEditMode:   cfg.ReplEditMode,
		HistFile:         cfg.HistFile,
		HistFileSize:     cfg.HistFileSize,
		HistSize:         cfg.HistSize,
		Skills:           discoveredSkills,
		SkillDirs:        skillDirs,
		DisabledTools:    disabledTools,
		SummaryWidth:     env.terminalCols,
	}
	// If HistFile was not explicitly configured, derive it from StateDir.
	if app.HistFile == "" {
		app.HistFile = session.HistoryPath(app.StateDir)
	}
	if resumed != nil {
		app.PromptNumber = resumed.Prompt
		if len(resumed.Todos) > 0 {
			app.ArmTodoContext()
		}
	}
	// Wire the MCP tool-list refresh hook for the interactive REPL only: one-shot
	// runs a single prompt with tools discovered before the request, so it needs no hook.
	if mcpConn != nil && !runOptions.PromptSet {
		staticSummary := func() mcptools.Summary {
			return mergeMCPSummaries(localSummary, lspSummary, serenaSummary)
		}
		refreshMCP := newMCPRefresherDynamic(mcpConn, toolCatalog, agents, mcpBases, mcpSummary, staticSummary, logger, pendingMCP, mcpLim)
		app.RefreshMCP = func(ctx context.Context, agentName string) (*tools.Registry, string) {
			reg, notice := refreshMCP(ctx, agentName)
			if reg != nil {
				if lspControl != nil {
					applyLSPExposure(agents, lspControl.summary, lspControl.enabled, lspExplicit)
					if definition, ok := agents[agentName]; ok {
						if reconciled, err := subsetForAgentTools(toolCatalog, definition.AllowedTools, pendingMCP); err == nil {
							reg = reconciled
						} else {
							logger.Warn(fmt.Sprintf("lsp: tool exposure refresh failed: %v; keeping refreshed tools", err), logging.Category("lsp"))
						}
					}
				}
				snap := delegateState.Snapshot()
				snap.ToolNames = reg.Names()
				delegateState.Set(snap)
			}
			return reg, notice
		}
	}
	ag.SetCompactionArchiver(func(ctx context.Context, archive agent.CompactionArchive) (string, error) {
		ref, err := session.SaveCompaction(app.SessionPath, session.Compaction{
			Time:           now(),
			Summary:        archive.Summary,
			SummarySource:  archive.SummarySource,
			FallbackReason: archive.FallbackReason,
			Usage:          archive.Usage,
			Messages:       archive.Messages,
			Focus:          archive.Focus,
			ReadFiles:      archive.ReadFiles,
			ModifiedFiles:  archive.ModifiedFiles,
		})
		if err != nil {
			return "", err
		}
		if err := app.PrepareCompaction(ag.Transcript(), len(archive.Messages), archive.Summary, ref, archive.TokensBefore, archive.Focus, archive.ReadFiles, archive.ModifiedFiles); err != nil {
			return "", err
		}
		return ref, nil
	})
	// OTEL exporter: opt-in. Hostname defaults to the short OS hostname;
	// an explicitly empty otel.hostname disables host.name.
	hostname := cfg.OTel.Hostname
	if cfg.OTel.Enabled && !cfg.OTel.HostnameSet {
		if h, err := os.Hostname(); err == nil {
			h = strings.TrimSpace(h)
			if h != "" {
				if idx := strings.Index(h, "."); idx != -1 {
					h = h[:idx]
				}
				hostname = strings.TrimSpace(h)
			}
		}
	}
	if cfg.OTel.Enabled {
		otelCfg := otel.Config{
			Enabled:            cfg.OTel.Enabled,
			Endpoint:           cfg.OTel.Endpoint,
			Protocol:           cfg.OTel.Protocol,
			Timeout:            time.Duration(cfg.OTel.TimeoutSeconds) * time.Second,
			ServiceName:        cfg.OTel.ServiceName,
			Hostname:           hostname,
			Headers:            cfg.OTel.Headers,
			ResourceAttributes: cfg.OTel.ResourceAttributes,
		}
		if otelCfg.Protocol == "" {
			otelCfg.Protocol = "http/json"
		}
		if otelCfg.ServiceName == "" {
			otelCfg.ServiceName = "harness"
		}
		if otelCfg.Timeout == 0 {
			otelCfg.Timeout = 5 * time.Second
		}
		sessionID := ""
		if sessionPath != "" {
			sessionID = filepath.Base(sessionPath)
		}
		exp, err := otel.NewExporter(otelCfg, buildinfo.Current(), sessionID, cfg.Provider, cfg.Model, agentName, cfg.OTel.ResourceAttributes)
		if err != nil {
			fmt.Fprintf(stderr, "harness: otel: %v\n", err)
			return ui.ExitUsage
		}
		otelSink := otel.NewSink(exp, toolRegistry, cfg.Provider, cfg.Model, agentName, false)
		otelSink.SetIdentity(sessionID, cfg.Provider, cfg.Model, agentName)
		otelSink.RecordSkillCatalog(skillCatalogReport)
		app.SetOTel(otelSink)
		defer func() {
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			_ = exp.Export(ctx)
		}()
		periodicCtx, cancelPeriodic := context.WithCancel(context.Background())
		defer cancelPeriodic()
		exp.SetPeriodic(periodicCtx)
		defer app.RecordOTelSession()
	}

	app.SetUsage(totals)
	app.SetUsageByModel(resumedUsageByModel)
	if resumeCloned {
		if err := session.AppendEvent(app.SessionPath, session.Event{
			Time:        now(),
			Type:        session.EventBranch,
			Display:     fmt.Sprintf("[clone: %s → %s; working directory unchanged]", resumeCloneFrom, resumeCloneTo),
			FromEntryID: resumeCloneFrom,
			ToEntryID:   resumeCloneTo,
			Purpose:     "clone",
		}); err != nil {
			fmt.Fprintf(stderr, "[session event log failed: %v]\n", err)
		}
	}
	if hookRunner != nil {
		source := "startup"
		if resumeCloned {
			source = "clone"
		} else if resumed != nil {
			source = "resume"
		}
		app.RunSessionStartHookWithContext(startupCtx, source)
	}
	if startupInterrupted() {
		return ui.ExitInterrupt
	}

	// JSON mode keeps its startup watcher through stream admission so a SIGINT
	// cannot be lost while swapping to active-run behavior. Text modes retain the
	// existing separate startup and active watchers.
	exitCh := make(chan struct{})
	if jsonRunMode {
		if watcher := jsonInterrupts.Watcher(); watcher != nil {
			exitCh = jsonInterrupts.ExitCh()
			app.Interrupt = watcher
			app.ForceExit = exitCh
		}
	} else if env.sigCh != nil {
		stopStartup()
		var exitOnce sync.Once
		watcher := agent.NewInterruptWatcher(env.sigCh, now, func() {
			exitOnce.Do(func() { close(exitCh) })
		})
		stop := watcher.Start()
		defer stop()
		app.Interrupt = watcher
		app.ForceExit = exitCh
	} else {
		stopStartup()
	}

	// Mid-prompt steering: route input submitted during a running prompt into the
	// agent as the next turn's input (design §8.1). Disabled
	// by -no-steer, and only wired for interactive sessions — one-shot mode has
	// no during-prompt input. Interactive JSON sessions steer via prompt messages.
	if !cfg.NoSteer && (interactiveSession || machineInteractive) {
		steerAgent := ag
		app.Steer = func(input agent.SteerInput) bool { return steerAgent.SteerContent(input) }
		app.DrainSteer = func() agent.SteerInput { return steerAgent.DrainSteerContent() }
	}

	// One-shot mode: a single prompt, then exit (design §10).
	if runOptions.PromptSet {
		var prompt string
		var err error
		if jsonRunMode {
			prompt, err = buildPromptWithStartupContext(startupCtx, runOptions.Prompt, stdin, env.stdinPiped)
		} else {
			prompt, err = ui.BuildPrompt(runOptions.Prompt, stdin, env.stdinPiped)
		}
		if jsonRunMode && (startupInterrupted() || errors.Is(err, context.Canceled)) {
			return ui.ExitInterrupt
		}
		if err != nil {
			fmt.Fprintf(stderr, "harness: read prompt: %v\n", err)
			return ui.ExitRuntime
		}
		images, err := loadConfiguredImages(runOptions.Images, modelRegistry.SupportsInputModality(registryModel, "image"))
		if err != nil {
			fmt.Fprintf(stderr, "harness: image: %v\n", err)
			return ui.ExitRuntime
		}
		if len(runOptions.Images) > 0 && len(images) == 0 {
			fmt.Fprintf(stderr, "[image skipped: model %s does not support image input]\n", registryModel)
		}
		app.PendingImages = images
		var stream *runstream.Writer
		if jsonRunMode {
			if !jsonInterrupts.Commit(func() {
				startupDiagnostics.commit()
				jsonRunCommitted = true
				stopStartup()
				stream = runstream.NewWriterWithAbort(rawStdout, runstream.RunStart{
					Mode:      runstream.ModeOneshot,
					SessionID: filepath.Base(sessionPath),
					Agent:     agentName,
					Provider:  cfg.Provider,
					Model:     registryModel,
					Images:    len(images),
					Time:      now(),
				}, stderr, exitCh)
				app.RunStream = stream
			}) {
				return ui.ExitInterrupt
			}
			stream.WaitForStart()
		}
		fmt.Fprintf(stderr, "session: %s\n", sessionPath)
		fmt.Fprintln(stderr, ui.ProviderLine(cfg.Provider, cfg.Model, registryModel, reasoning, modelRegistry))
		if resumed != nil {
			ui.PrintResumeRecap(app, resumed)
		}
		code := ui.OneShot(app, prompt)
		select {
		case <-exitCh:
			code = ui.ExitInterrupt
		default:
		}
		if stream != nil {
			if err := stream.Close(runstream.RunEnd{ExitCode: code}); err != nil && code == ui.ExitOK {
				code = ui.ExitRuntime
			}
		}
		return code
	}

	// Interactive JSON mode: NDJSON input messages on piped stdin drive the
	// session (design §10); the JSON run stream owns stdout.
	if machineInteractive {
		var stream *runstream.Writer
		if !jsonInterrupts.Commit(func() {
			startupDiagnostics.commit()
			jsonRunCommitted = true
			stopStartup()
			stream = runstream.NewWriterWithAbort(rawStdout, runstream.RunStart{
				Mode:      runstream.ModeInteractive,
				SessionID: filepath.Base(sessionPath),
				Agent:     agentName,
				Provider:  cfg.Provider,
				Model:     registryModel,
				Time:      now(),
			}, stderr, exitCh)
			app.RunStream = stream
		}) {
			return ui.ExitInterrupt
		}
		stream.WaitForStart()
		fmt.Fprintf(stderr, "session: %s\n", sessionPath)
		fmt.Fprintln(stderr, ui.ProviderLine(cfg.Provider, cfg.Model, registryModel, reasoning, modelRegistry))
		if resumed != nil {
			ui.PrintResumeRecap(app, resumed)
		}
		code := ui.RunJSON(stdin, app)
		select {
		case <-exitCh:
			code = ui.ExitInterrupt
		default:
		}
		if err := stream.Close(runstream.RunEnd{ExitCode: code}); err != nil && code == ui.ExitOK {
			code = ui.ExitRuntime
		}
		return code
	}

	if runOptions.InitialPromptSet {
		images, err := loadConfiguredImages(runOptions.Images, modelRegistry.SupportsInputModality(registryModel, "image"))
		if err != nil {
			fmt.Fprintf(stderr, "harness: image: %v\n", err)
			return ui.ExitRuntime
		}
		if len(runOptions.Images) > 0 && len(images) == 0 {
			fmt.Fprintf(stderr, "[image skipped: model %s does not support image input]\n", registryModel)
		}
		app.PendingImages = images
	}

	// Pre-warm in the background only when the proxy target advertises a
	// zero-generation warmup. Generated one-token completions duplicate prompt
	// processing and can become very expensive on resumed transcripts. Piped and
	// one-shot runs are also excluded because there is no idle user time in which
	// to hide startup latency. The snapshot is captured synchronously here; only
	// the stream runs in the goroutine, so it never races the loop.
	prewarm := func() {
		if !prewarmForProvider(catalog, app.Provider) {
			return
		}
		if warm, ok := app.Agent.PrewarmFunc(); ok {
			modelKey := app.RegistryModel
			if modelKey == "" {
				modelKey = app.Model
			}
			go func() {
				ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
				defer cancel()
				result := warm(ctx)
				if result.Usage != (llm.Usage{}) || result.ResponseState != nil {
					app.QueuePrewarmResultForModel(modelKey, result)
				}
			}()
		}
	}
	if env.prewarmCache && !env.stdinPiped {
		if !runOptions.InitialPromptSet {
			prewarm()
		}
		// Re-warm after cache-invalidating events (agent/model/provider switch,
		// compaction); the REPL captures a fresh snapshot at call time (r43).
		app.Prewarm = prewarm
	}

	// Interactive REPL. ui.Run owns the session save in every exit path,
	// including SIGINT, so the exit-save never races an in-flight prompt's own save
	// or usage update (design §8.4); main only forwards the exit request.
	fmt.Fprintf(stderr, "session: %s\n", sessionPath)
	fmt.Fprintln(stderr, ui.ProviderLine(cfg.Provider, cfg.Model, registryModel, reasoning, modelRegistry))
	// Surface the active agent and the discoverability cues that were otherwise
	// invisible: /help for commands and how to interrupt a turn (r58, r23).
	fmt.Fprintf(stderr, "agent: %s · type /help for commands · interrupt a turn with Ctrl-C or double-Esc\n", agentName)
	if resumed != nil {
		ui.PrintResumeRecap(app, resumed)
	}
	if runOptions.InitialPromptSet {
		return ui.RunWithInitialPrompt(stdin, app, exitCh, runOptions.InitialPrompt)
	}
	return ui.Run(stdin, app, exitCh)
}

type debugRequestOutput struct {
	Version              int                    `json:"version"`
	Provider             string                 `json:"provider"`
	Model                string                 `json:"model"`
	RegistryModel        string                 `json:"registry_model"`
	Agent                string                 `json:"agent"`
	Reasoning            llm.ReasoningConfig    `json:"reasoning"`
	ResponsesStateful    bool                   `json:"responses_stateful"`
	UsedPreviousResponse bool                   `json:"used_previous_response"`
	PromptIncluded       bool                   `json:"prompt_included"`
	ToolNames            []string               `json:"tool_names"`
	ToolCount            int                    `json:"tool_count"`
	MessageCount         int                    `json:"message_count"`
	Request              llm.Request            `json:"request"`
	Context              agent.ContextEstimate  `json:"context_estimate"`
	RequestBytes         debugRequestByteCounts `json:"request_bytes"`
}

type debugRequestByteCounts struct {
	System         int `json:"system"`
	Tools          int `json:"tools"`
	Messages       int `json:"messages"`
	RequestContext int `json:"request_context"`
	Total          int `json:"total"`
}

func debugRequestPrompt(runOptions config.RunOptions, stdin io.Reader, stdinPiped, supportsImages bool) (bool, string, []llm.ContentBlock, error) {
	var prompt string
	includePrompt := false
	switch {
	case runOptions.PromptSet:
		p, err := ui.BuildPrompt(runOptions.Prompt, stdin, stdinPiped)
		if err != nil {
			return false, "", nil, fmt.Errorf("read prompt: %w", err)
		}
		prompt = p
		includePrompt = true
	case runOptions.InitialPromptSet:
		prompt = runOptions.InitialPrompt
		includePrompt = true
	}
	if !includePrompt {
		return false, "", nil, nil
	}
	images, err := loadConfiguredImages(runOptions.Images, supportsImages)
	if err != nil {
		return false, "", nil, fmt.Errorf("image: %w", err)
	}
	return true, prompt, loadedImageBlocks(images), nil
}

func loadedImageBlocks(images []inputimage.Loaded) []llm.ContentBlock {
	if len(images) == 0 {
		return nil
	}
	blocks := make([]llm.ContentBlock, 0, len(images))
	for _, image := range images {
		blocks = append(blocks, image.Block)
	}
	return blocks
}

func buildDebugRequestOutput(ag *agent.Agent, cfg config.Config, registryModel, agentName string, includePrompt bool, prompt string, images []llm.ContentBlock) debugRequestOutput {
	snap := ag.DebugRequest(includePrompt, prompt, images, nil)
	toolNames := ag.ToolNames()
	return debugRequestOutput{
		Version:              1,
		Provider:             cfg.Provider,
		Model:                cfg.Model,
		RegistryModel:        registryModel,
		Agent:                agentName,
		Reasoning:            snap.Request.Reasoning,
		ResponsesStateful:    snap.Request.StoreResponse,
		UsedPreviousResponse: snap.UsedPrevious,
		PromptIncluded:       includePrompt,
		ToolNames:            toolNames,
		ToolCount:            len(toolNames),
		MessageCount:         len(snap.Request.Messages),
		Request:              snap.Request,
		Context:              snap.Estimate,
		RequestBytes:         debugRequestBytes(snap.Request),
	}
}

func debugRequestBytes(req llm.Request) debugRequestByteCounts {
	out := debugRequestByteCounts{
		System:         len(req.System),
		RequestContext: len(llm.RequestContextText(req.RequestContext)),
	}
	for _, t := range req.Tools {
		out.Tools += len(t.Name) + len(t.Description) + len(t.Parameters)
	}
	for _, t := range req.ServerTools {
		out.Tools += len(t.Name) + len(t.Kind) + len(t.Parameters)
	}
	for _, m := range req.Messages {
		out.Messages += len(m.Role) + len(m.Phase)
		for _, b := range m.Content {
			out.Messages += debugContentBlockBytes(b)
		}
	}
	out.Total = out.System + out.Tools + out.Messages + out.RequestContext
	return out
}

func debugContentBlockBytes(b llm.ContentBlock) int {
	total := len(b.Kind) + len(b.Text) + len(b.ImageMediaType) + len(b.ImageData) +
		len(b.ImageDetail) + len(b.ImageName) + len(b.ToolUseID) + len(b.ToolName) +
		len(b.ToolInput) + len(b.ResultForID) + len(b.ResultText) + len(b.Thinking) +
		len(b.ThinkingSignature) + len(b.RedactedData) + len(b.ReasoningID) + len(b.ReasoningEncrypted)
	for _, child := range b.ResultContent {
		total += debugContentBlockBytes(child)
	}
	return total
}

func resolveConfiguredAgents(cfg config.Config) (map[string]agentdef.Definition, error) {
	agents := agentdef.Resolve(fileAgentDefinitions(cfg.Agents))
	if err := agentdef.Validate(agents); err != nil {
		return nil, err
	}
	return agents, nil
}

func enableInteractivePlanHandoff(agents map[string]agentdef.Definition) {
	planning, ok := agents["plan"]
	if !ok {
		return
	}
	for _, name := range planning.AllowedTools {
		if name == "handoff" {
			return
		}
	}
	planning.AllowedTools = append(planning.AllowedTools, "handoff")
	agents["plan"] = planning
}

func ripgrepAvailable() bool {
	_, err := exec.LookPath("rg")
	return err == nil
}

func searchBackend() string {
	if ripgrepAvailable() {
		return "rg"
	}
	return "go"
}

func resolvedConfigAgentName(cfg config.Config, agents map[string]agentdef.Definition) (string, error) {
	agentName := cfg.Agent
	if agentName == "" {
		agentName = agentdef.Default
	}
	if _, ok := agents[agentName]; !ok {
		return "", fmt.Errorf("unknown agent %q (available: %s)", agentName, strings.Join(agentdef.Names(agents), ", "))
	}
	return agentName, nil
}

type infoOutput struct {
	Version       int              `json:"version"`
	DefaultAgent  string           `json:"default_agent,omitempty"`
	SelectedAgent string           `json:"selected_agent,omitempty"`
	Agents        []agentListEntry `json:"agents,omitempty"`
	ProviderCount int              `json:"provider_count,omitempty"`
	ModelCount    int              `json:"model_count,omitempty"`
	Models        []modelListEntry `json:"models,omitempty"`
	ModelProxyURL string           `json:"model_proxy_url,omitempty"`
}

type agentsListOutput struct {
	DefaultAgent  string
	SelectedAgent string
	Agents        []agentListEntry
}

type agentListEntry struct {
	Name         string   `json:"name"`
	Description  string   `json:"description"`
	AllowedTools []string `json:"allowed_tools"`
	MCPTools     string   `json:"mcp_tools"`
	HasPrompt    bool     `json:"has_prompt"`
	Model        string   `json:"model,omitempty"`
	Selected     bool     `json:"selected"`
}

func buildAgentsListOutput(cfg config.Config) (*agentsListOutput, error) {
	agents, err := resolveConfiguredAgents(cfg)
	if err != nil {
		return nil, err
	}
	selected, err := resolvedConfigAgentName(cfg, agents)
	if err != nil {
		return nil, err
	}
	out := &agentsListOutput{
		DefaultAgent:  agentdef.Default,
		SelectedAgent: selected,
	}
	for _, name := range agentdef.Names(agents) {
		agent := agents[name]
		out.Agents = append(out.Agents, agentListEntry{
			Name:         name,
			Description:  agent.Description,
			AllowedTools: append([]string(nil), agent.AllowedTools...),
			MCPTools:     string(agent.MCPTools),
			HasPrompt:    strings.TrimSpace(agent.Prompt) != "",
			Model:        agent.Model,
			Selected:     name == selected,
		})
	}
	return out, nil
}

func checkModelProxy(ctx context.Context, proxyClient *modelclient.Client) (protocol.Catalog, error) {
	ctx, cancel := context.WithTimeout(ctx, modelProxyCheckTimeout)
	defer cancel()
	return proxyClient.Catalog(ctx)
}

func catalogModelCount(catalog protocol.Catalog) int {
	return len(catalog.Targets)
}

type modelsListOutput struct {
	ProviderCount int
	ModelCount    int
	Models        []modelListEntry
}

type modelListEntry struct {
	TargetID                 string     `json:"target_id"`
	DisplayName              string     `json:"display_name,omitempty"`
	ProviderLabel            string     `json:"provider_label,omitempty"`
	ModelLabel               string     `json:"model_label,omitempty"`
	ContextWindow            int        `json:"context_window,omitempty"`
	InputModalities          []string   `json:"input_modalities,omitempty"`
	ServerTools              []string   `json:"server_tools,omitempty"`
	BaseTargetID             string     `json:"base_target_id,omitempty"`
	Variant                  string     `json:"variant,omitempty"`
	APIType                  string     `json:"api_type,omitempty"`
	ContinuationStateful     bool       `json:"continuation_stateful,omitempty"`
	Prewarm                  bool       `json:"prewarm,omitempty"`
	PricePerMillionTokensUSD *llm.Price `json:"price_per_million_tokens_usd,omitempty"`
	Reasoning                bool       `json:"reasoning"`
}

func buildModelsListOutput(catalog protocol.Catalog) *modelsListOutput {
	models := catalogModelListRows(catalog)
	return &modelsListOutput{
		ProviderCount: 0,
		ModelCount:    len(models),
		Models:        models,
	}
}

func sortedModelListEntries(models []modelListEntry) []modelListEntry {
	sorted := append([]modelListEntry(nil), models...)
	sort.SliceStable(sorted, func(i, j int) bool {
		return sorted[i].TargetID < sorted[j].TargetID
	})
	return sorted
}

func formatAgentsListText(out agentsListOutput) string {
	var b strings.Builder
	b.WriteString("agents:\n")
	rows := make([]ui.NameDescription, 0, len(out.Agents))
	for _, agent := range out.Agents {
		name := agent.Name
		if agent.Selected {
			name += " (selected)"
		}
		parts := []string{
			"[" + agentListModelSummary(agent.Model) + "]",
			"[mcp: " + agent.MCPTools + "]",
		}
		if strings.TrimSpace(agent.Description) != "" {
			parts = append(parts, agent.Description)
		}
		rows = append(rows, ui.NameDescription{
			Name:        name,
			Description: strings.Join(parts, " "),
		})
	}
	ui.WriteNameDescriptionList(&b, rows, ui.NameDescriptionListOptions{Indent: "  "})
	return b.String()
}

func formatModelsListText(out modelsListOutput) string {
	var b strings.Builder
	for _, row := range out.Models {
		fmt.Fprintf(&b, "%s\t%s\t%s\t%s\n", row.TargetID, modelListModalitiesText(row.InputModalities), modelListServerToolsText(row.ServerTools), modelListReasoningText(row.Reasoning))
	}
	return b.String()
}

func catalogModelListRows(catalog protocol.Catalog) []modelListEntry {
	var rows []modelListEntry
	for _, target := range catalog.Targets {
		if target.ID == "" {
			continue
		}
		rows = append(rows, modelListEntry{
			TargetID:                 target.ID,
			DisplayName:              strings.TrimSpace(target.DisplayName),
			ProviderLabel:            strings.TrimSpace(target.ProviderLabel),
			ModelLabel:               strings.TrimSpace(target.ModelLabel),
			ContextWindow:            target.ContextWindow,
			InputModalities:          append([]string(nil), target.InputModalities...),
			ServerTools:              append([]string(nil), target.ServerTools...),
			BaseTargetID:             strings.TrimSpace(target.BaseTargetID),
			Variant:                  strings.TrimSpace(target.Variant),
			APIType:                  strings.TrimSpace(target.APIType),
			ContinuationStateful:     target.ContinuationStateful,
			Prewarm:                  target.Prewarm,
			PricePerMillionTokensUSD: modelListPrice(target.Price),
			Reasoning:                target.Reasoning,
		})
	}
	return rows
}

func modelListModalitiesText(input []string) string {
	if len(input) == 0 {
		return "-"
	}
	cleaned := make([]string, 0, len(input))
	for _, modality := range input {
		modality = strings.TrimSpace(modality)
		if modality != "" {
			cleaned = append(cleaned, modality)
		}
	}
	if len(cleaned) == 0 {
		return "-"
	}
	return strings.Join(cleaned, ",")
}

func modelListServerToolsText(tools []string) string {
	if len(tools) == 0 {
		return "-"
	}
	cleaned := llm.NormalizeServerTools(tools)
	if len(cleaned) == 0 {
		return "-"
	}
	return strings.Join(cleaned, ",")
}

func agentListModelSummary(model string) string {
	model = strings.TrimSpace(model)
	if model == "" {
		return "default model"
	}
	return model
}

func modelListPrice(price llm.Price) *llm.Price {
	if price.IsZero() {
		return nil
	}
	out := price
	return &out
}

func modelListReasoningText(reasoning bool) string {
	if !reasoning {
		return "-"
	}
	return "reasoning"
}

func fileAgentDefinitions(agents map[string]config.FileAgentConfig) map[string]agentdef.FileDefinition {
	out := make(map[string]agentdef.FileDefinition, len(agents))
	for name, fa := range agents {
		out[name] = agentdef.FileDefinition{
			Description:     fa.Description,
			AllowedTools:    fa.AllowedTools,
			MCPTools:        fa.MCPTools,
			WorkspaceAccess: fa.WorkspaceAccess,
			Prompt:          fa.Prompt,
			Model:           fa.Model,
			Reasoning:       fa.Reasoning,
		}
	}
	return out
}

// resolveHarnessLauncher returns the path used to launch this process without
// resolving symlinks. Package-manager links such as /opt/homebrew/bin/harness
// survive upgrades even when their current versioned target does not.
func resolveHarnessLauncher(argv0 string) (string, error) {
	argv0 = strings.TrimSpace(argv0)
	if argv0 != "" {
		if strings.ContainsRune(argv0, rune(os.PathSeparator)) {
			return filepath.Abs(argv0)
		}
		if path, err := exec.LookPath(argv0); err == nil {
			return path, nil
		}
	}
	return os.Executable()
}

// setupDelegateTmuxViewer builds the tmux delegate-view viewer when
// delegate_tmux is enabled. The feature is display-only: any setup failure
// degrades to a nil viewer plus one stderr warning when delegate_tmux was
// explicitly enabled (suppressed by -q); auto-enabled setups log at debug
// level only. The OpenChildView closure on a nil viewer returns an error the
// delegate runner swallows.
func setupDelegateTmuxViewer(cfg config.Config, getenv func(string) string, stderr io.Writer, logger *slog.Logger, explicit, quiet bool) *tmux.Viewer {
	warnf := func(format string, args ...any) {
		if explicit && !quiet {
			fmt.Fprintf(stderr, "harness: warning: "+format+"\n", args...)
			return
		}
		logger.Debug(fmt.Sprintf(format, args...))
	}
	if getenv("TMUX") == "" {
		warnf("delegate_tmux is enabled but TMUX is not set; delegate views disabled")
		return nil
	}
	// Preserve the invocation path rather than a versioned package-manager
	// target so long-running sessions keep working across in-place upgrades.
	harnessBin, err := resolveHarnessLauncher(os.Args[0])
	tmuxBin, lookErr := exec.LookPath("tmux")
	if err != nil || lookErr != nil {
		warnf("delegate_tmux is enabled but the harness or tmux binary could not be resolved; delegate views disabled")
		return nil
	}
	layout := tmux.Layout(cfg.DelegateTmuxLayout)
	parentPane := ""
	if layout == tmux.LayoutPane {
		parentPane = getenv("TMUX_PANE")
		if parentPane == "" {
			warnf("delegate_tmux_layout=%s but TMUX_PANE is not set; delegate views use windows", cfg.DelegateTmuxLayout)
			layout = tmux.LayoutWindow
		}
	}
	return tmux.NewViewer(tmux.Client{Binary: tmuxBin}, tmux.ViewerOptions{
		HarnessBinary: harnessBin,
		MaxWindows:    cfg.DelegateTmuxMaxWindows,
		Layout:        layout,
		ParentPane:    parentPane,
		Logger:        logger,
	})
}

func runSessionTimings(env environment, invocation cli.Invocation) int {
	dir, err := session.ResolveSessionDir(stateDir(env.getenv), invocation.Args[0])
	if err != nil {
		fmt.Fprintf(env.stderr, "harness: session timings: %v\n", err)
		return ui.ExitUsage
	}
	if err := session.Timings(dir, env.stdout); err != nil {
		fmt.Fprintf(env.stderr, "harness: session timings: %v\n", err)
		return ui.ExitRuntime
	}
	return ui.ExitOK
}

func runSessionStats(env environment, invocation cli.Invocation) int {
	format := cliLast(invocation.Flags, "format", "text")
	if format != "text" && format != "json" {
		fmt.Fprintln(env.stderr, "usage: harness session stats [--format text|json] <session-dir>")
		return ui.ExitUsage
	}
	dir, err := session.ResolveSessionDir(stateDir(env.getenv), invocation.Args[0])
	if err != nil {
		fmt.Fprintf(env.stderr, "harness: session stats: %v\n", err)
		return ui.ExitUsage
	}
	if format == "json" {
		err = session.StatsJSON(dir, env.stdout)
	} else {
		err = session.Stats(dir, env.stdout)
	}
	if err != nil {
		fmt.Fprintf(env.stderr, "harness: session stats: %v\n", err)
		return ui.ExitRuntime
	}
	return ui.ExitOK
}

func runSessionReplay(env environment, invocation cli.Invocation) int {
	values := invocation.Flags
	follow := cliBool(values, "follow")
	quiet := cliBool(values, "quiet")
	colorTheme, colorThemeSet := values.Last("color_theme")
	if !colorThemeSet {
		colorTheme = config.ColorThemeDark
	}
	loadOptions := harnessLoadOptions(env, nil)
	if configPath, configPathSet := values.Last("config"); configPathSet {
		loadOptions.Args = []string{"--config", configPath}
	}
	resolvedConfigPath, err := config.ResolveConfigPath(loadOptions)
	if err != nil {
		fmt.Fprintf(env.stderr, "harness: session replay: %v\n", err)
		_ = commandCatalog(env).WriteHelp(env.stderr, invocation.CommandID)
		return ui.ExitUsage
	}
	resolvedTheme, err := config.LoadColorTheme(colorTheme, colorThemeSet, env.envLookup(), resolvedConfigPath)
	if err != nil {
		fmt.Fprintf(env.stderr, "harness: session replay: %v\n", err)
		_ = commandCatalog(env).WriteHelp(env.stderr, invocation.CommandID)
		return ui.ExitUsage
	}

	dir := invocation.Args[0]
	opts := sessionReplayOptions(env, quiet, highlightColorTheme(resolvedTheme))
	if !follow {
		if err := session.Replay(dir, env.stdout, opts); err != nil {
			fmt.Fprintf(env.stderr, "harness: session replay: %v\n", err)
			return ui.ExitRuntime
		}
		return ui.ExitOK
	}

	ctx, cancel, interrupted := signalCancelContext(env.sigCh)
	defer cancel()
	if err := session.Follow(ctx, dir, env.stdout, opts); err != nil {
		if interrupted() && errors.Is(err, context.Canceled) {
			return ui.ExitInterrupt
		}
		fmt.Fprintf(env.stderr, "harness: session replay: %v\n", err)
		return ui.ExitRuntime
	}
	return ui.ExitOK
}

func sessionReplayOptions(env environment, quiet bool, colorTheme highlight.Theme) session.ReplayOptions {
	width := markdown.DefaultWidth
	if env.terminalCols != nil {
		if cols := env.terminalCols(); cols > 0 {
			width = cols
		}
	}
	return session.ReplayOptions{
		Markdown:   true,
		ANSI:       env.colorTTY && !envColorDisabled(env.getenv),
		ColorTheme: colorTheme,
		Width:      width,
		Quiet:      quiet,
	}
}

func highlightColorTheme(value string) highlight.Theme {
	if value == config.ColorThemeLight {
		return highlight.ThemeLight
	}
	return highlight.ThemeDark
}

func envColorDisabled(getenv func(string) string) bool {
	if getenv == nil {
		return false
	}
	if getenv("NO_COLOR") != "" {
		return true
	}
	disabled, err := strconv.ParseBool(getenv("HARNESS_NO_COLOR"))
	return err == nil && disabled
}

func loadConfiguredImages(images []config.ImageAttachment, supportsImages bool) ([]inputimage.Loaded, error) {
	if len(images) == 0 {
		return nil, nil
	}
	if !supportsImages {
		return nil, nil
	}
	loaded := make([]inputimage.Loaded, 0, len(images))
	for _, image := range images {
		img, err := inputimage.Load(inputimage.Attachment{Path: image.Path, Detail: image.Detail})
		if err != nil {
			return nil, fmt.Errorf("%s: %w", image.Path, err)
		}
		loaded = append(loaded, img)
	}
	if err := inputimage.ValidateTotal(loaded); err != nil {
		return nil, err
	}
	return loaded, nil
}

func timestampLayout(mode string) string {
	switch mode {
	case config.TimestampNone:
		return ""
	case config.TimestampFull:
		return ui.TimestampFullLayout
	default:
		return ui.TimestampShortLayout
	}
}

func defaultTerminalRows() int {
	rows, _, ok := term.Size()
	if !ok {
		return 0
	}
	return rows
}

func defaultTerminalCols() int {
	_, cols, ok := term.Size()
	if !ok {
		return 0
	}
	return cols
}

type catalogSelection struct {
	Provider              string
	Model                 string
	RegistryModel         string
	BaseTargetID          string
	ReasoningReplayDomain string
	Variant               string
	FastTargetID          string
}

func catalogSelectionForTarget(catalog protocol.Catalog, target protocol.Target) catalogSelection {
	baseTargetID := target.BaseTargetID
	if baseTargetID == "" {
		baseTargetID = target.ID
	}
	reasoningReplayDomain := target.ReasoningReplayDomain
	if reasoningReplayDomain == "" {
		// The conservative default is the exact base target.
		reasoningReplayDomain = baseTargetID
	}
	return catalogSelection{
		Provider:              target.ID,
		Model:                 target.ID,
		RegistryModel:         target.ID,
		BaseTargetID:          baseTargetID,
		ReasoningReplayDomain: reasoningReplayDomain,
		Variant:               target.Variant,
		FastTargetID:          fastTargetID(catalog, baseTargetID),
	}
}

func agentModelInputs(def agentdef.Definition, provider, model string) (string, string) {
	if def.Model != "" {
		return "", def.Model
	}
	return provider, model
}

func resolveAgentCatalogSelection(catalog protocol.Catalog, def agentdef.Definition, provider, model string) (catalogSelection, error) {
	nextProvider, nextModel := agentModelInputs(def, provider, model)
	return resolveCatalogSelection(catalog, nextProvider, nextModel, provider)
}

func resolveDelegateLaunch(runtime delegate.Runtime, name string, agents map[string]agentdef.Definition, catalog *tools.Registry, pending *asyncMCPRegistration, modelCatalog protocol.Catalog, proxyClient *modelclient.Client, buildSystem func(string) string, cfg config.Config) (delegate.Launch, error) {
	target := strings.TrimSpace(name)
	omittedAgent := target == ""
	if target == "" {
		target = runtime.Agent
	}
	if target == "" {
		target = agentdef.Default
	}
	def, ok := agents[target]
	if !ok {
		return delegate.Launch{}, fmt.Errorf("unknown agent %q (available: %s)", target, strings.Join(agentdef.Names(agents), ", "))
	}
	toolNames := def.AllowedTools
	if omittedAgent {
		toolNames = runtime.ToolNames
		if len(toolNames) == 0 {
			toolNames = def.AllowedTools
		}
	} else {
		// Validate against the live catalog through the same pending filter startup
		// uses: while async MCP discovery is in flight, undiscovered mcp__ names are
		// tolerated (filtered) so a delegate launched before the proxy responds does
		// not fail on a tool that is merely not-yet-registered. Once applied, the
		// filter is a no-op and the full check applies.
		sub, err := subsetForAgentTools(catalog, def.AllowedTools, pending)
		if err != nil {
			return delegate.Launch{}, err
		}
		if missing := delegate.MissingTools(sub.Names(), runtime.ToolNames); len(missing) > 0 {
			parent := runtime.Agent
			if parent == "" {
				parent = agentdef.Default
			}
			return delegate.Launch{}, fmt.Errorf("agent %q cannot be delegated to by parent agent %q: requires tools not available to parent: %s", target, parent, strings.Join(missing, ", "))
		}
	}
	reg, err := subsetForAgentTools(catalog, toolNames, pending)
	if err != nil {
		return delegate.Launch{}, err
	}

	provider := runtime.Provider
	providerName := runtime.ProviderName
	model := runtime.Model
	system := runtime.System
	launchReasoning := runtime.Reasoning
	reasoningReplayDomain := runtime.ReasoningReplayDomain
	serverTools := runtime.ServerTools
	if target != runtime.Agent {
		next, err := resolveAgentCatalogSelection(modelCatalog, def, runtime.ProviderName, runtime.Model)
		if err != nil {
			return delegate.Launch{}, err
		}
		mode := reasoningModeForProvider(modelCatalog, next.Provider)
		launchReasoning = compatibleReasoningForModel(runtime.Registry, next.RegistryModel, mode, agentBaseReasoning(def, runtime.Reasoning))
		if err := validateReasoningConfig(runtime.Registry, next.RegistryModel, mode, launchReasoning); err != nil {
			return delegate.Launch{}, err
		}
		providerName = next.Provider
		model = next.Model
		reasoningReplayDomain = next.ReasoningReplayDomain
		serverTools = webSearchServerToolsForModel(next.Provider, runtime.Registry, next.RegistryModel, cfg.WebSearch)
		provider = proxyClient.Provider(next.Provider)
		system = buildSystem(def.Prompt)
	}
	if system == "" {
		system = buildSystem(def.Prompt)
	}
	if provider == nil && providerName != "" {
		provider = proxyClient.Provider(providerName)
	}
	return delegate.Launch{
		Provider:              provider,
		ProviderName:          providerName,
		Model:                 model,
		ContextWindow:         runtime.ContextWindow,
		MaxOutputTokens:       runtime.MaxOutputTokens,
		Registry:              runtime.Registry,
		Reasoning:             launchReasoning,
		ReasoningReplayDomain: reasoningReplayDomain,
		ServerTools:           serverTools,
		ResponsesStateful:     responsesStatefulForProvider(cfg, modelCatalog, providerName),
		System:                system,
		Agent:                 target,
		Tools:                 reg,
	}, nil
}

// agentBaseReasoning returns base with its profile overridden by the agent's
// pinned reasoning profile when set, so a per-agent "reasoning" field controls
// that agent's thinking profile. Model compatibility is enforced afterward by
// compatibleReasoningForModel and validateReasoningConfig.
func agentBaseReasoning(def agentdef.Definition, base llm.ReasoningConfig) llm.ReasoningConfig {
	if def.Reasoning != "" {
		base.Profile = def.Reasoning
	}
	return base
}

func delegateAgentCandidates(agents map[string]agentdef.Definition) []delegate.AgentCandidate {
	names := agentdef.Names(agents)
	out := make([]delegate.AgentCandidate, 0, len(names))
	for _, name := range names {
		a := agents[name]
		out = append(out, delegate.AgentCandidate{Name: name, Description: a.Description, ToolNames: a.AllowedTools, WorkspaceAccess: a.WorkspaceAccess})
	}
	return out
}

func agentSummaries(agents map[string]agentdef.Definition, parentTools []string) []ui.AgentSummary {
	delegatable := make(map[string]bool)
	for _, name := range delegate.DelegatableAgentNames(parentTools, delegateAgentCandidates(agents)) {
		delegatable[name] = true
	}
	names := agentdef.Names(agents)
	out := make([]ui.AgentSummary, 0, len(names))
	for _, name := range names {
		a := agents[name]
		out = append(out, ui.AgentSummary{
			Name:        name,
			Description: a.Description,
			Model:       a.Model,
			Delegatable: delegatable[name],
		})
	}
	return out
}

func resolveCatalogSelection(catalog protocol.Catalog, provider, model, preferredProvider string) (catalogSelection, error) {
	provider = strings.TrimSpace(provider)
	model = strings.TrimSpace(model)
	if model == "" && provider != "" {
		model = provider
	} else if provider != "" && model != "" {
		if provider != model {
			qualified := provider + ":" + model
			if strings.HasPrefix(model, provider+":") {
				qualified = model
			}
			if target, ok := catalogTarget(catalog, qualified); ok {
				return catalogSelectionForTarget(catalog, target), nil
			}
			if target, ok := catalogTargetForProvider(catalog, provider, model); ok {
				return catalogSelectionForTarget(catalog, target), nil
			}
			if _, ok := catalogTarget(catalog, model); ok {
				return catalogSelection{}, fmt.Errorf("target %q belongs to a different provider than %q", model, provider)
			}
			model = qualified
		}
	}
	if model != "" {
		if target, ok := catalogTarget(catalog, model); ok {
			return catalogSelectionForTarget(catalog, target), nil
		}
		return catalogSelection{}, fmt.Errorf("target %q is not available from the model proxy", model)
	}
	return catalogSelection{}, fmt.Errorf("a model is required (-model or harness config model)")
}

// fuzzyMatchModel resolves a near-miss model argument against the catalog's
// model ids: exact match wins, then a unique prefix, then a unique substring
// (case-insensitive). It returns the matched id, or the candidate list when the
// match is ambiguous so the caller can surface "did you mean …?" (r24).
func fuzzyMatchModel(catalog protocol.Catalog, input string) (match string, candidates []string) {
	input = strings.ToLower(strings.TrimSpace(input))
	if input == "" {
		return "", nil
	}
	for _, target := range catalog.Targets {
		for _, id := range append([]string{target.ID}, target.Aliases...) {
			if strings.ToLower(strings.TrimSpace(id)) == input {
				return target.ID, nil
			}
		}
	}
	pick := func(filter func(string) bool) []string {
		var out []string
		for _, target := range catalog.Targets {
			for _, id := range append([]string{target.ID}, target.Aliases...) {
				if filter(strings.ToLower(strings.TrimSpace(id))) {
					out = append(out, target.ID)
					break
				}
			}
		}
		return out
	}
	for _, stage := range []func(string) bool{
		func(id string) bool { return strings.HasPrefix(id, input) },
		func(id string) bool { return strings.Contains(id, input) },
	} {
		matches := pick(stage)
		if len(matches) == 1 {
			return matches[0], nil
		}
		if len(matches) > 1 {
			return "", clampStrings(matches, 8)
		}
	}
	return "", nil
}

// clampStrings caps a candidate list so a "did you mean" message stays short.
func clampStrings(s []string, max int) []string {
	if len(s) > max {
		return s[:max]
	}
	return s
}

func catalogTarget(catalog protocol.Catalog, id string) (protocol.Target, bool) {
	id = strings.TrimSpace(id)
	for _, target := range catalog.Targets {
		if catalogTargetMatches(target, id) {
			return target, true
		}
	}
	return protocol.Target{}, false
}

func catalogTargetForProvider(catalog protocol.Catalog, provider, id string) (protocol.Target, bool) {
	id = strings.TrimSpace(id)
	prefix := strings.TrimSpace(provider) + ":"
	for _, target := range catalog.Targets {
		if strings.HasPrefix(target.ID, prefix) && catalogTargetMatches(target, id) {
			return target, true
		}
	}
	return protocol.Target{}, false
}

func catalogTargetMatches(target protocol.Target, id string) bool {
	if target.ID == id {
		return true
	}
	for _, alias := range target.Aliases {
		if alias == id {
			return true
		}
	}
	return false
}

func fastTargetID(catalog protocol.Catalog, baseTargetID string) string {
	for _, target := range catalog.Targets {
		if target.BaseTargetID == baseTargetID && strings.EqualFold(target.Variant, "fast") {
			return target.ID
		}
	}
	return ""
}

func reasoningModeForProvider(catalog protocol.Catalog, providerID string) string {
	return "model-proxy"
}

func responsesStatefulForProvider(cfg config.Config, catalog protocol.Catalog, providerID string) bool {
	if !cfg.ResponsesStateful {
		return false
	}
	target, ok := catalogTarget(catalog, providerID)
	return ok && target.ContinuationStateful
}

func prewarmForProvider(catalog protocol.Catalog, providerID string) bool {
	target, ok := catalogTarget(catalog, providerID)
	return ok && target.Prewarm
}

func webSearchServerToolsForModel(provider string, registry *llm.Registry, model, mode string) []llm.ServerTool {
	if strings.ToLower(strings.TrimSpace(mode)) != "auto" || registry == nil {
		return nil
	}
	info, ok := registry.Lookup(model)
	if !ok {
		return nil
	}
	for _, tool := range info.ServerTools {
		if strings.EqualFold(strings.TrimSpace(tool), llm.ServerToolWebSearch) {
			// Best-effort tag of the provider-specific kind so the agent can tell a
			// Kimi builtin web-search call (which the client must echo) from other
			// providers' server-side search. provider is a "provider:model" target
			// id, so resolve against its bare provider prefix. The model proxy
			// re-resolves the kind authoritatively from the full provider config
			// before the wire call, so a missing or imperfect tag here never affects
			// the request.
			name := provider
			if before, _, ok := strings.Cut(provider, ":"); ok {
				name = before
			}
			kind := llm.WebSearchServerToolKind(name, "", "")
			return []llm.ServerTool{{Name: llm.ServerToolWebSearch, Kind: kind}}
		}
	}
	return nil
}

func sessionResponseStateCompatible(cfg config.Config, catalog protocol.Catalog, s session.Session, provider, model string) bool {
	if !responsesStatefulForProvider(cfg, catalog, provider) ||
		s.Provider != provider ||
		s.Model != model ||
		s.ResponseState == nil ||
		s.ResponseState.PreviousResponseID == "" ||
		s.ResponseState.AnchorDigest == "" ||
		s.ResponseState.AnchorMessages < 0 ||
		s.ResponseState.AnchorMessages > len(s.Messages) {
		return false
	}
	return llm.MatchesMessageFingerprint(
		s.Messages[:s.ResponseState.AnchorMessages],
		s.ResponseState.AnchorDigest,
	)
}

func effectiveReasoningSummary(configured, mode string, interactive, suppressOutput bool) string {
	if suppressOutput {
		return ""
	}
	configured = strings.ToLower(strings.TrimSpace(configured))
	switch configured {
	case "auto", "concise", "detailed":
		return configured
	}
	return ""
}

func providerForReasoningModel(catalog protocol.Catalog, fallbackProvider, model string) string {
	return strings.TrimSpace(model)
}

func catalogModelPicker(catalog protocol.Catalog) func(ui.PickerIO) (string, error) {
	targets := catalogTargetPickerEntries(catalog)
	if len(targets) == 0 {
		return nil
	}
	return func(pio ui.PickerIO) (string, error) {
		w := pio.Writer
		if w == nil {
			w = io.Discard
		}
		target, err := ui.Pick(pio.ReadLine, w, ui.PickerOptions[catalogTargetPick]{
			Items:       targets,
			PageSize:    pio.PageSize,
			Prompt:      "Model target (number/id, /search, n/p, q): ",
			Kind:        "model target",
			CancelError: ui.ErrPickerCancelled,
			PrintPage: func(w io.Writer, models []catalogModelPick, page, pageSize int, filter string) {
				ui.PrintModelPickerPage(w, "targets", models, page, pageSize, filter)
			},
		})
		if err != nil {
			return "", err
		}
		return target.target.ID, nil
	}
}

func pickStartupModel(readLine func(string) (string, error), w io.Writer, catalog protocol.Catalog, pageSize int) (catalogSelection, error) {
	picker := catalogModelPicker(catalog)
	if picker == nil {
		return catalogSelection{}, fmt.Errorf("model proxy catalog has no selectable models")
	}
	fmt.Fprintln(w, "Select a model target to use with harness.")
	input, err := picker(ui.PickerIO{
		ReadLine: readLine,
		Writer:   w,
		PageSize: pageSize,
	})
	if err != nil {
		return catalogSelection{}, err
	}
	return resolveCatalogSelection(catalog, "", input, "")
}

func pickStartupReasoningProfile(readLine func(string) (string, error), w io.Writer, registry *llm.Registry, model string, reasoning llm.ReasoningConfig) (llm.ReasoningConfig, error) {
	info, ok := reasoningInfoForModel(registry, model)
	if !ok || !info.Supported {
		return reasoning, nil
	}
	current := strings.TrimSpace(reasoning.Profile)
	if normalized, ok := reasoningprofile.Normalize(current); ok {
		current = normalized
	}
	_, currentValid := reasoningprofile.Normalize(reasoning.Profile)
	for {
		line, err := readLine(fmt.Sprintf("Reasoning profile (%s; current: %s): ", reasoningprofile.ChoicesLabel(), reasoningProfilePromptCurrent(current, currentValid)))
		if err != nil {
			return reasoning, err
		}
		if line == "" {
			if currentValid {
				return reasoning, nil
			}
			reasoning.Profile = ""
			return reasoning, nil
		}
		if strings.EqualFold(line, "q") {
			return reasoning, ui.ErrPickerCancelled
		}
		profile, ok := reasoningprofile.Normalize(line)
		if !ok {
			fmt.Fprintf(w, "Invalid reasoning profile %q (supported: %s)\n", line, reasoningprofile.ChoicesLabel())
			continue
		}
		reasoning.Profile = profile
		return reasoning, nil
	}
}

func reasoningInfoForModel(registry *llm.Registry, model string) (*llm.ReasoningInfo, bool) {
	if registry == nil {
		return nil, false
	}
	info, ok := registry.Lookup(model)
	if !ok || info.Reasoning == nil {
		return nil, false
	}
	return info.Reasoning, true
}

func reasoningProfilePromptCurrent(current string, valid bool) string {
	if strings.TrimSpace(current) == "" {
		return "provider default"
	}
	if valid {
		return current
	}
	return current + " (not valid for this model; Enter uses provider default)"
}

func saveSelectedModel(path, provider, model string, reasoning llm.ReasoningConfig) error {
	if provider != "" && !strings.Contains(model, ":") {
		model = provider + ":" + model
	}
	return config.SaveSelectedModel(path, model, reasoning.Profile)
}

type catalogModelPick struct {
	target protocol.Target
}

type catalogTargetPick = catalogModelPick

func catalogTargetPickerEntries(catalog protocol.Catalog) []catalogModelPick {
	entries := make([]catalogModelPick, 0, len(catalog.Targets))
	for _, target := range catalog.Targets {
		if target.ID == "" {
			continue
		}
		entries = append(entries, catalogModelPick{target: target})
	}
	return entries
}

func (m catalogModelPick) PickerID() string { return m.target.ID }
func (m catalogModelPick) PickerName() string {
	if m.target.DisplayName != "" {
		return m.target.DisplayName
	}
	return m.target.ID
}
func (m catalogModelPick) PickerPrice() string { return ui.FormatPickerPrice(m.target.Price) }

func validateReasoningConfig(registry *llm.Registry, model, _ string, reasoning llm.ReasoningConfig) error {
	reasoning.Profile = strings.ToLower(strings.TrimSpace(reasoning.Profile))
	reasoning.Summary = strings.ToLower(strings.TrimSpace(reasoning.Summary))
	if reasoning.Empty() {
		return nil
	}
	profile, ok := reasoningprofile.Normalize(reasoning.Profile)
	if !ok {
		return fmt.Errorf("invalid reasoning profile %q (want %s)", reasoning.Profile, reasoningprofile.ChoicesLabel())
	}
	reasoning.Profile = profile
	if registry == nil {
		return nil
	}
	info, ok := registry.Lookup(model)
	if !ok || info.Reasoning == nil {
		return nil
	}
	if !info.Reasoning.Supported {
		return fmt.Errorf("model %q does not support reasoning controls", model)
	}
	return nil
}

func compatibleReasoningForModel(registry *llm.Registry, model, _ string, reasoning llm.ReasoningConfig) llm.ReasoningConfig {
	reasoning.Profile = strings.ToLower(strings.TrimSpace(reasoning.Profile))
	reasoning.Summary = strings.ToLower(strings.TrimSpace(reasoning.Summary))
	if profile, ok := reasoningprofile.Normalize(reasoning.Profile); ok {
		reasoning.Profile = profile
	} else {
		reasoning.Profile = ""
	}
	if reasoning.Empty() {
		return reasoning
	}
	info, ok := reasoningInfoForModel(registry, model)
	if ok && !info.Supported {
		return llm.ReasoningConfig{}
	}
	return reasoning
}

func pickerPageSize(env environment) int {
	rows := 0
	if env.terminalRows != nil {
		rows = env.terminalRows()
	}
	return ui.PickerPageSize(rows)
}

func defaultConfigDir(getenv func(string) string) string {
	if home := getenv("HOME"); home != "" {
		return filepath.Join(home, ".config", "harness")
	}
	return filepath.Join(os.TempDir(), "harness-config")
}

// homeDir returns the user's home directory, or empty string if unavailable.
func homeDir(getenv func(string) string) string {
	return getenv("HOME")
}

// resolveAtFile expands a @file reference to the file's contents; a plain string
// is returned unchanged. A literal leading @ can be escaped as @@. @~/path is
// resolved through the current user's home directory.
func resolveAtFile(v string) (string, error) {
	if strings.HasPrefix(v, "@@") {
		return v[1:], nil
	}
	if strings.HasPrefix(v, "@") {
		path, err := expandAtFilePath(v[1:])
		if err != nil {
			return "", err
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return "", err
		}
		return string(data), nil
	}
	return v, nil
}

func expandAtFilePath(path string) (string, error) {
	if path == "~" || strings.HasPrefix(path, "~/") {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		if path == "~" {
			return home, nil
		}
		return filepath.Join(home, path[2:]), nil
	}
	return path, nil
}

func userAgentsMDPath(getenv func(string) string) string {
	home := homeDir(getenv)
	if home == "" {
		return ""
	}
	return filepath.Join(home, ".agents", "AGENTS.md")
}

func projectAgentsMDPath(dir string) string {
	if dir == "" {
		return ""
	}
	return filepath.Join(dir, "AGENTS.md")
}

// loadAgentsMDFile reads path when present. A missing or empty path returns an
// empty string with no error; other read failures are returned.
func loadAgentsMDFile(path string) (string, error) {
	if path == "" {
		return "", nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "", nil
		}
		return "", fmt.Errorf("reading %s: %w", path, err)
	}
	return string(data), nil
}

func warnLargeAgentsMD(w io.Writer, limit int, path, content string) {
	if limit <= 0 || content == "" || len(content) <= limit {
		return
	}
	fmt.Fprintf(w, "harness: warning: %s is %d bytes, above agents_md_warn_bytes=%d; including it in full\n", path, len(content), limit)
}

// stateDir returns the base directory for auto-saved sessions: $XDG_STATE_HOME
// or ~/.local/state (design §11).
func stateDir(getenv func(string) string) string {
	if x := getenv("XDG_STATE_HOME"); x != "" {
		return x
	}
	if home := getenv("HOME"); home != "" {
		return filepath.Join(home, ".local", "state")
	}
	return filepath.Join(os.TempDir(), "harness-state")
}

// isTTY reports whether f is a terminal, gating dim color (design §2, §10).
func isTTY(f *os.File) bool {
	info, err := f.Stat()
	if err != nil {
		return false
	}
	return info.Mode()&os.ModeCharDevice != 0
}

// pipedStdin reports whether stdin is piped/redirected (not a terminal), so
// one-shot mode knows to read it (design §10).
func pipedStdin(f *os.File) bool {
	info, err := f.Stat()
	if err != nil {
		return false
	}
	return info.Mode()&os.ModeCharDevice == 0
}
