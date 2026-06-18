// Command harness is the entrypoint: it loads configuration, connects to
// harness-model-proxy, constructs the tool registry and agent, wires SIGINT
// handling, prints the session path, and dispatches to the interactive REPL or
// one-shot mode.
package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"harness/internal/agent"
	"harness/internal/agentdef"
	"harness/internal/background"
	"harness/internal/buildinfo"
	"harness/internal/config"
	"harness/internal/delegate"
	"harness/internal/hooks"
	"harness/internal/inputimage"
	"harness/internal/llm"
	"harness/internal/logging"
	"harness/internal/markdown"
	"harness/internal/mcptools"
	modelclient "harness/internal/modelproxy/client"
	"harness/internal/modelproxy/protocol"
	"harness/internal/session"
	"harness/internal/skills"
	"harness/internal/sysprompt"
	"harness/internal/term"
	"harness/internal/todo"
	"harness/internal/tools"
	"harness/internal/ui"
	"harness/prompts"
)

const modelProxyCheckTimeout = 2 * time.Second

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
	now          func() time.Time
	colorTTY     bool // stdout is a terminal (gates color)
	stdinPiped   bool // stdin is piped/redirected (gates one-shot stdin read)
	prewarmCache bool // issue a background prompt-cache warm-up at interactive startup
	sigCh        chan os.Signal

	terminalRows func() int
	terminalCols func() int
	agentSleep   func(time.Duration)
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
func run(env environment) int {
	args := env.args
	stdin := env.stdin
	stdout := env.stdout
	stderr := env.stderr
	getenv := env.getenv
	now := env.now
	if now == nil {
		now = time.Now
	}
	if len(args) > 0 && args[0] == "--version" {
		fmt.Fprintln(stdout, buildinfo.Line("harness"))
		return ui.ExitOK
	}
	if len(args) > 0 && args[0] == "session" {
		return runSessionCommand(args[1:], stdout, stderr, sessionReplayOptions(env, argsQuiet(args)))
	}
	if len(args) > 0 && args[0] == "lsp" {
		return runLSPCommand(env, args[1:])
	}

	cfgPath := resolveConfigPath(args, getenv)

	cfg, err := config.Load(args, getenv, cfgPath)
	if err != nil {
		// -h/--help is a request, not a misuse: print the usage screen to stdout
		// and exit 0 (design §10).
		if errors.Is(err, config.ErrHelp) {
			config.Usage(stdout)
			return ui.ExitOK
		}
		fmt.Fprintf(stderr, "harness: %v\n", err)
		return ui.ExitUsage
	}
	explicitReasoningOutput := explicitReasoningOutputFlag(args)
	suppressReasoningOutput := cfg.Quiet && !explicitReasoningOutput
	if cfg.ShowConfig {
		out, err := buildShowConfigOutput(cfg)
		if err != nil {
			fmt.Fprintf(stderr, "harness: show config: %v\n", err)
			return ui.ExitUsage
		}
		if err := config.WriteResolved(stdout, out); err != nil {
			fmt.Fprintf(stderr, "harness: show config: %v\n", err)
			return ui.ExitRuntime
		}
		return ui.ExitOK
	}
	proxyURL := cfg.ModelProxyURL
	if proxyURL == "" {
		proxyURL = protocol.DefaultURL
	}
	proxyClient, err := modelclient.New(proxyURL, nil)
	if err != nil {
		fmt.Fprintf(stderr, "harness: %v\n", err)
		return ui.ExitUsage
	}
	startupCtx, stopStartup, startupInterrupted := signalCancelContext(env.sigCh)
	defer stopStartup()
	if cfg.ShowAgents || cfg.ShowModels {
		var agents *agentsListOutput
		if cfg.ShowAgents {
			var err error
			agents, err = buildAgentsListOutput(cfg)
			if err != nil {
				fmt.Fprintf(stderr, "harness: agents: %v\n", err)
				return ui.ExitUsage
			}
		}
		var models *modelsListOutput
		if cfg.ShowModels {
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
		if cfg.OutputFormat == "json" {
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
			if err := config.WriteResolved(stdout, out); err != nil {
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
	if cfg.CheckModelProxy {
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
		if cfg.OutputFormat == "json" {
			out := infoOutput{
				Version:       1,
				ModelProxyURL: proxyClient.URL(),
				ProviderCount: len(catalog.Providers),
				ModelCount:    catalogModelCount(catalog),
			}
			if err := config.WriteResolved(stdout, out); err != nil {
				fmt.Fprintf(stderr, "harness: model proxy: %v\n", err)
				return ui.ExitRuntime
			}
		} else {
			fmt.Fprintf(stdout, "model proxy ok: %s (%d providers, %d models)\n", proxyClient.URL(), len(catalog.Providers), catalogModelCount(catalog))
		}
		return ui.ExitOK
	}
	logger, err := logging.NewLogger(stderr, cfg.LogLevel)
	if err != nil {
		fmt.Fprintf(stderr, "harness: %v\n", err)
		return ui.ExitUsage
	}

	// Load a resumed session up front: its saved agent selects the tool set and
	// any agent-specific provider/model when no -agent flag overrides it.
	var resumed *session.Session
	if cfg.Resume != "" {
		s, err := session.Load(cfg.Resume)
		if err != nil {
			fmt.Fprintf(stderr, "harness: resume %s: %v\n", cfg.Resume, err)
			return ui.ExitRuntime
		}
		resumed = &s
	}
	if len(cfg.Images) > 0 && !cfg.PromptSet {
		fmt.Fprintln(stderr, "harness: -image requires -p one-shot mode")
		return ui.ExitUsage
	}

	agents, err := resolveConfiguredAgents(cfg)
	if err != nil {
		fmt.Fprintf(stderr, "harness: %v\n", err)
		return ui.ExitUsage
	}
	agentName := cfg.Agent
	if resumed != nil && resumed.Agent != "" {
		if cfg.Agent == "" {
			agentName = resumed.Agent
		} else if cfg.Agent != resumed.Agent {
			fmt.Fprintf(stderr, "harness: session agent %q overridden by %q (flags win)\n", resumed.Agent, cfg.Agent)
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
		Effort:       cfg.ReasoningEffort,
		Enabled:      cfg.ReasoningEnabled,
		BudgetTokens: cfg.ReasoningBudgetTokens,
	}
	interactiveSession := !cfg.PromptSet && !env.stdinPiped
	startProvider, startModel := agentModelInputs(startupAgent, cfg.Provider, cfg.Model)
	selection, err := resolveCatalogSelection(catalog, startProvider, startModel, cfg.Provider)
	if err != nil {
		configuredSelectionUnavailable := startModel != "" || startProvider != ""
		if configuredSelectionUnavailable && (cfg.PromptSet || env.stdinPiped) {
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
		reasoning, err = pickStartupReasoningEffort(readStartupLine, stderr, modelRegistry, selection.RegistryModel, reasoning)
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
			configPath := writableConfigPath(args, getenv)
			if err := config.SaveSelectedModel(configPath, selection.Provider, selection.Model, reasoning.Effort, reasoning.Enabled, reasoning.BudgetTokens); err != nil {
				fmt.Fprintf(stderr, "harness: save selected model: %v\n", err)
				return ui.ExitRuntime
			}
			fmt.Fprintf(stderr, "harness: saved selected model to %s\n", configPath)
		}
	}
	cfg.Provider = selection.Provider
	cfg.Model = selection.Model
	registryModel := selection.RegistryModel
	reasoning.Summary = effectiveReasoningSummary(cfg.ReasoningSummary, reasoningModeForProvider(catalog, selection.Provider), interactiveSession, suppressReasoningOutput)
	if err := validateReasoningConfig(modelRegistry, registryModel, reasoningModeForProvider(catalog, selection.Provider), reasoning); err != nil {
		fmt.Fprintf(stderr, "harness: %v\n", err)
		return ui.ExitUsage
	}

	// System prompt composition (design §8.5). -system-prompt may be an @file
	// reference. When set, it replaces only the static prompt; runtime sections
	// such as env, user/project AGENTS.md, skills, and agent prompts are still
	// composed.
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
	// the model uses its existing read_file tool to load them on demand.
	var skillWarnings skills.Warnings
	skillDirs := []skills.Dir{
		{Path: filepath.Join(wd, ".agents", "skills"), Scope: skills.ScopeProject},
		{Path: filepath.Join(homeDir(getenv), ".agents", "skills"), Scope: skills.ScopeUser},
	}
	discoveredSkills := skills.Discover(skillDirs, &skillWarnings)
	for _, w := range skillWarnings {
		fmt.Fprintf(stderr, "skills: %s\n", w)
	}
	skillsCatalog := skills.BuildCatalog(discoveredSkills)
	instructions := skills.Instructions(len(discoveredSkills))

	// buildSystem assembles the full system prompt for a given agent prompt,
	// reusing every other input. The configured system prompt replaces only the
	// static built-in instructions. The skills instructions block is appended
	// last, exactly as at startup, so an /agent switch reproduces the same
	// composition.
	buildSystem := func(agentPrompt string) string {
		s := sysprompt.Build(sysprompt.Options{
			StaticPrompt:    configuredSystemPrompt,
			NoEnv:           cfg.NoEnv,
			UserAgentsMD:    userAgentsMD,
			ProjectAgentsMD: projectAgentsMD,
			SkillsCatalog:   skillsCatalog,
			AgentPrompt:     agentPrompt,
			Env:             sysprompt.EnvOptions{Dir: wd},
		})
		if instructions != "" {
			s += "\n\n" + instructions
		}
		return s
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
		MaxResultBytes:       cfg.ToolResultMaxBytes,
		MaxResultLines:       cfg.ToolResultMaxLines,
		ReadFileDefaultLimit: cfg.ReadFileDefaultLimit,
		Background:           backgroundManager,
		SearchTools:          cfg.SearchTools,
	})
	for _, disabled := range disabledTools {
		logger.Warn(disabled.Message(), logging.Category("cli_tools"))
	}
	delegateState := delegate.NewState(delegate.Runtime{
		ProviderName:      cfg.Provider,
		Model:             cfg.Model,
		ContextWindow:     cfg.ContextWindow,
		Registry:          modelRegistry,
		Reasoning:         reasoning,
		ResponsesStateful: responsesStatefulForProvider(cfg, catalog, cfg.Provider),
		Agent:             agentName,
	})
	// pendingMCP is assigned below (interactive REPL only) before any turn can run,
	// so this closure — invoked lazily at delegation time — captures the live value.
	// It lets the delegate launch tolerate not-yet-discovered mcp__ tools exactly as
	// startup does, instead of failing Subset during the async discovery window.
	var pendingMCP *asyncMCPRegistration
	resolveDelegate := func(runtime delegate.Runtime, name string) (delegate.Launch, error) {
		return resolveDelegateLaunch(runtime, name, agents, toolCatalog, pendingMCP, catalog, proxyClient, buildSystem, cfg)
	}
	delegateOpts := delegate.Options{
		MaxTurns:                  cfg.DelegateMaxTurns,
		CompactKeepTurns:          cfg.CompactKeepTurns,
		CompactSummaryMaxTokens:   cfg.CompactSummaryMaxTokens,
		CompactToolResultMaxBytes: cfg.CompactToolResultMaxBytes,
		Now:                       now,
		AgentCandidates: func(delegate.Runtime) []delegate.AgentCandidate {
			return delegateAgentCandidates(agents)
		},
	}
	delegateRunner := delegate.NewRunner(delegateState.Snapshot, resolveDelegate, delegateOpts)
	// One todo store per process backs the update_todos tool; the App persists it
	// in state.json and reseeds it on resume below.
	todoStore := todo.NewStore()
	toolCatalog.Register(todo.NewTool(todoStore))
	toolCatalog.Register(delegate.NewTool(delegateRunner, backgroundManager))
	toolCatalog.Register(background.NewJobsTool(backgroundManager))
	// MCP (opt-in): one-shot runs synchronously so the single request can use MCP
	// tools immediately. Interactive REPL starts remote HTTP discovery in the
	// background and applies discovered tools at a prompt boundary, so an
	// unreachable proxy never delays launch. MCP never fails startup; on any error
	// it warns and continues with no MCP tools.
	var mcpConn *mcptools.Conn
	var mcpSummary mcptools.Summary
	if cfg.MCP.Enable {
		if cfg.PromptSet {
			conn, summary, cleanup, ok := setupMCP(startupCtx, cfg.MCP, toolCatalog, logger)
			defer cleanup()
			if startupInterrupted() {
				return ui.ExitInterrupt
			}
			if ok {
				mcpConn, mcpSummary = conn, summary
			}
		} else {
			conn, pending, cleanup, ok := setupMCPAsync(cfg.MCP, logger)
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
	if localMCPEnabled(cfg.MCP.Local, cfg.PromptSet) {
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
	if cfg.LSP.Enable {
		summary, cleanup, ok := setupLSP(startupCtx, cfg.LSP, toolCatalog, logger)
		defer cleanup()
		if startupInterrupted() {
			return ui.ExitInterrupt
		}
		if ok {
			lspSummary = summary
		}
	}
	// Agent mcp_tools policy controls automatic exposure for discovered external
	// tools: disabled, read_only, or all. LSP tools are first-class but still use
	// this read-only exposure gate so whitelist agents stay locked down. Capture
	// MCP-exposing agents BEFORE augmenting them, so the refresh hook can
	// re-derive their allowed lists.
	// The name lists are empty when MCP/LSP are disabled, making this a no-op.
	mcpBases := mcpExposingAgentBases(agents)
	mcpNames := make([]string, 0, len(mcpSummary.Names)+len(localSummary.Names)+len(lspSummary.Names))
	mcpNames = append(mcpNames, mcpSummary.Names...)
	mcpNames = append(mcpNames, localSummary.Names...)
	mcpNames = append(mcpNames, lspSummary.Names...)
	mcpReadOnlyNames := make([]string, 0, len(mcpSummary.ReadOnlyNames)+len(localSummary.ReadOnlyNames)+len(lspSummary.ReadOnlyNames))
	mcpReadOnlyNames = append(mcpReadOnlyNames, mcpSummary.ReadOnlyNames...)
	mcpReadOnlyNames = append(mcpReadOnlyNames, localSummary.ReadOnlyNames...)
	mcpReadOnlyNames = append(mcpReadOnlyNames, lspSummary.ReadOnlyNames...)
	augmentAgentsWithMCP(agents, mcpNames, mcpReadOnlyNames)
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
		nextReasoning := compatibleReasoningForModel(modelRegistry, next.RegistryModel, mode, reasoning)
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
		snap.ContextWindow = cfg.ContextWindow
		snap.System = system
		snap.Reasoning = nextReasoning
		snap.ResponsesStateful = responsesStatefulForProvider(cfg, catalog, next.Provider)
		snap.Agent = a.Name
		snap.ToolNames = reg.Names()
		delegateState.Set(snap)
		return ui.AgentSelection{
			Name:              a.Name,
			Tools:             reg,
			System:            system,
			Provider:          next.Provider,
			Model:             next.Model,
			RegistryModel:     next.RegistryModel,
			BaseURL:           proxyClient.URL(),
			Runtime:           runtime,
			ContextWindow:     cfg.ContextWindow,
			Reasoning:         nextReasoning,
			ReasoningSet:      true,
			ResponsesStateful: snap.ResponsesStateful,
		}, nil
	}

	provider := proxyClient.Provider(cfg.Provider)

	switchModel := func(input string, nextReasoning llm.ReasoningConfig) (ui.ModelSelection, error) {
		input = strings.TrimSpace(input)
		if input == "" {
			return ui.ModelSelection{}, fmt.Errorf("model is required")
		}
		next, err := resolveCatalogSelection(catalog, "", input, cfg.Provider)
		if err != nil {
			return ui.ModelSelection{}, err
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
		snap.ContextWindow = cfg.ContextWindow
		snap.Reasoning = nextReasoning
		snap.ResponsesStateful = responsesStatefulForProvider(cfg, catalog, next.Provider)
		delegateState.Set(snap)
		reasoning = nextReasoning
		return ui.ModelSelection{
			Provider:          next.Provider,
			Model:             next.Model,
			RegistryModel:     next.RegistryModel,
			BaseURL:           proxyClient.URL(),
			Runtime:           runtime,
			ContextWindow:     cfg.ContextWindow,
			Reasoning:         nextReasoning,
			ReasoningSet:      true,
			ResponsesStateful: snap.ResponsesStateful,
		}, nil
	}

	ag := agent.New(provider, toolRegistry, agent.Options{
		MaxTurns:                  cfg.MaxTurns,
		Model:                     cfg.Model,
		ContextWindow:             cfg.ContextWindow,
		Registry:                  modelRegistry,
		Reasoning:                 reasoning,
		Now:                       now,
		CompactKeepTurns:          cfg.CompactKeepTurns,
		CompactSummaryMaxTokens:   cfg.CompactSummaryMaxTokens,
		CompactToolResultMaxBytes: cfg.CompactToolResultMaxBytes,
		Hooks:                     hookRunner,
		ShowDiffs:                 cfg.ShowDiffs,
		ResponsesStateful:         responsesStatefulForProvider(cfg, catalog, cfg.Provider),
	})
	if env.agentSleep != nil {
		ag.SetSleep(env.agentSleep)
	}

	created := now()
	var totals session.UsageTotals
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
		todoStore.Replace(s.Todos)
		if !s.Created.IsZero() {
			created = s.Created
		}
		totals = s.Usage
		// A resumed session keeps its saved full system prompt unless a static
		// system_prompt override is set.
		if cfg.SystemPrompt == "" && s.System != "" {
			systemPrompt = s.System
		}
		if sessionResponseStateCompatible(cfg, catalog, s, cfg.Provider, cfg.Model) {
			resumeResponseState = s.ResponseState
		}
	}
	ag.SetSystem(systemPrompt)
	activeToolNames := toolRegistry.Names()

	sessionPath := cfg.Session
	if sessionPath == "" {
		if cfg.Resume != "" {
			sessionPath = cfg.Resume
		} else {
			sessionPath = session.DefaultPath(stateDir(getenv), created)
		}
	}
	delegateState.Set(delegate.Runtime{
		Provider:          provider,
		ProviderName:      cfg.Provider,
		Model:             cfg.Model,
		ContextWindow:     cfg.ContextWindow,
		Registry:          modelRegistry,
		Reasoning:         reasoning,
		ResponsesStateful: responsesStatefulForProvider(cfg, catalog, cfg.Provider),
		System:            systemPrompt,
		Agent:             agentName,
		ToolNames:         activeToolNames,
		SessionPath:       sessionPath,
	})
	if hookRunner != nil {
		hookRunner.SetSession(sessionPath)
	}
	// The delegate tool schema reads delegateState.ToolNames. Agent construction
	// cached tool specs before delegateState had the final runtime, so refresh the
	// same registry after the runtime is seeded.
	ag.SetTools(toolRegistry)
	ag.SetResponseState(resumeResponseState)

	color := !cfg.NoColor && env.colorTTY
	renderer := ui.NewRenderer(stdout, stderr, ui.RenderOptions{
		Color:                   color,
		Markdown:                env.colorTTY,
		Verbose:                 cfg.Verbose,
		ToolStream:              cfg.ToolStream,
		Quiet:                   cfg.Quiet,
		SuppressReasoningOutput: suppressReasoningOutput,
		Model:                   registryModel,
		Registry:                modelRegistry,
		Now:                     now,
		TimestampLayout:         timestampLayout(cfg.TimestampMode),
		Width:                   env.terminalCols,
	})

	app := &ui.App{
		Agent:                  ag,
		Renderer:               renderer,
		Out:                    stdout,
		Errw:                   stderr,
		Provider:               cfg.Provider,
		Model:                  cfg.Model,
		RegistryModel:          registryModel,
		BaseURL:                proxyClient.URL(),
		Registry:               modelRegistry,
		System:                 systemPrompt,
		Reasoning:              reasoning,
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
			return config.SaveSelectedModel(writableConfigPath(args, getenv), provider, model, reasoning.Effort, reasoning.Enabled, reasoning.BudgetTokens)
		},
		AgentName:       agentName,
		AvailableAgents: agentSummaries(agents, activeToolNames),
		RefreshAgentSummaries: func() []ui.AgentSummary {
			return agentSummaries(agents, delegateState.Snapshot().ToolNames)
		},
		SwitchAgent: switchAgent,
		Todos:       todoStore,
		SessionPath: sessionPath,
		StateDir:    stateDir(getenv),
		Created:     created,
		Now:         now,
		OnSessionPathChanged: func(path string) {
			snap := delegateState.Snapshot()
			snap.SessionPath = path
			delegateState.Set(snap)
		},
		Prompt:         cfg.ReplPrompt,
		PromptEditMode: cfg.ReplEditMode,
		HistFile:       cfg.HistFile,
		HistFileSize:   cfg.HistFileSize,
		HistSize:       cfg.HistSize,
		Skills:         discoveredSkills,
		SkillDirs:      skillDirs,
		DisabledTools:  disabledTools,
		SummaryWidth:   env.terminalCols,
	}
	// If HistFile was not explicitly configured, derive it from StateDir.
	if app.HistFile == "" {
		app.HistFile = session.HistoryPath(app.StateDir)
	}
	if resumed != nil {
		app.Turn = resumed.Turn
	}
	// Wire the MCP tool-list refresh hook for the interactive REPL only: one-shot
	// runs a single turn with tools discovered before the request, so it needs no hook.
	if mcpConn != nil && !cfg.PromptSet {
		staticSummary := mergeMCPSummaries(localSummary, lspSummary)
		refreshMCP := newMCPRefresher(mcpConn, toolCatalog, agents, mcpBases, mcpSummary, staticSummary, logger, pendingMCP)
		app.RefreshMCP = func(ctx context.Context, agentName string) (*tools.Registry, string) {
			reg, notice := refreshMCP(ctx, agentName)
			if reg != nil {
				snap := delegateState.Snapshot()
				snap.ToolNames = reg.Names()
				delegateState.Set(snap)
			}
			return reg, notice
		}
	}
	ag.SetCompactionArchiver(func(ctx context.Context, archive agent.CompactionArchive) (string, error) {
		return session.SaveCompaction(app.SessionPath, session.Compaction{
			Time:     now(),
			Summary:  archive.Summary,
			Usage:    archive.Usage,
			Messages: archive.Messages,
		})
	})
	app.SetUsage(totals)
	if hookRunner != nil {
		source := "startup"
		if resumed != nil {
			source = "resume"
		}
		app.RunSessionStartHook(source)
	}
	if startupInterrupted() {
		return ui.ExitInterrupt
	}
	stopStartup()

	// SIGINT wiring (design §8.4): a single handler cancels the active turn or,
	// on a second press / at the idle prompt, requests exit.
	exitCh := make(chan struct{}, 1)
	if env.sigCh != nil {
		watcher := agent.NewInterruptWatcher(env.sigCh, now, func() {
			select {
			case exitCh <- struct{}{}:
			default:
			}
		})
		stop := watcher.Start()
		defer stop()
		app.Interrupt = watcher
	}

	// One-shot mode: a single turn, then exit (design §10).
	if cfg.PromptSet {
		prompt, err := ui.BuildPrompt(cfg.Prompt, stdin, env.stdinPiped)
		if err != nil {
			fmt.Fprintf(stderr, "harness: read prompt: %v\n", err)
			return ui.ExitRuntime
		}
		images, err := loadConfiguredImages(cfg.Images)
		if err != nil {
			fmt.Fprintf(stderr, "harness: image: %v\n", err)
			return ui.ExitRuntime
		}
		app.PendingImages = images
		fmt.Fprintf(stderr, "session: %s\n", sessionPath)
		fmt.Fprintln(stderr, ui.ProviderLine(cfg.Provider, cfg.Model, registryModel, reasoning, modelRegistry))
		code := ui.OneShot(app, prompt)
		select {
		case <-exitCh:
			return ui.ExitInterrupt
		default:
		}
		return code
	}

	// Pre-warm the prompt cache in the background so the first real request reads
	// a warm tools+system prefix instead of paying the cold cache-write latency.
	// Gated to an interactive terminal: with piped/scripted stdin (one-shot is
	// already handled above, plus CI and tests) there is no human-perceived
	// first-turn latency to hide, so the extra request would be pure waste. The
	// snapshot is captured synchronously here; only the stream runs in the
	// goroutine, so it never races the loop.
	if env.prewarmCache && !env.stdinPiped {
		if warm, ok := ag.PrewarmFunc(); ok {
			go func() {
				ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
				defer cancel()
				warm(ctx)
			}()
		}
	}

	// Interactive REPL. ui.Run owns the session save in every exit path,
	// including SIGINT, so the exit-save never races an in-flight turn's own save
	// or usage update (design §8.4); main only forwards the exit request.
	fmt.Fprintf(stderr, "session: %s\n", sessionPath)
	fmt.Fprintln(stderr, ui.ProviderLine(cfg.Provider, cfg.Model, registryModel, reasoning, modelRegistry))
	return ui.Run(stdin, app, exitCh)
}

type showConfigOutput struct {
	config.Config
}

func showConfigDefaults(cfg config.Config) config.Config {
	if cfg.ModelProxyURL == "" {
		cfg.ModelProxyURL = protocol.DefaultURL
	}
	if cfg.Agent == "" {
		cfg.Agent = agentdef.Default
	}
	if cfg.MCP.Proxy == "" {
		cfg.MCP.Proxy = resolveMCPProxy("")
	}
	return cfg
}

func resolveConfiguredAgents(cfg config.Config) (map[string]agentdef.Definition, error) {
	agents := agentdef.ResolveWithOptions(fileAgentDefinitions(cfg.Agents), agentdef.Options{SearchTools: cfg.SearchTools})
	if err := agentdef.Validate(agents); err != nil {
		return nil, err
	}
	return agents, nil
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
	Provider     string   `json:"provider,omitempty"`
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
			Provider:     agent.Provider,
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
	total := 0
	for _, provider := range catalog.Providers {
		total += len(provider.Models)
	}
	return total
}

type modelsListOutput struct {
	ProviderCount int
	ModelCount    int
	Models        []modelListEntry
}

type modelListEntry struct {
	ProviderID               string                `json:"provider_id"`
	ProviderName             string                `json:"provider_name,omitempty"`
	ModelID                  string                `json:"model_id"`
	QualifiedID              string                `json:"qualified_id"`
	ModelName                string                `json:"model_name,omitempty"`
	ContextWindow            int                   `json:"context_window,omitempty"`
	PricePerMillionTokensUSD *llm.Price            `json:"price_per_million_tokens_usd,omitempty"`
	Reasoning                modelReasoningDetails `json:"reasoning"`
}

type modelReasoningDetails struct {
	Supported bool                  `json:"supported"`
	Options   []llm.ReasoningOption `json:"options,omitempty"`
}

func buildModelsListOutput(catalog protocol.Catalog) *modelsListOutput {
	models := catalogModelListRows(catalog, modelclient.Registry(catalog))
	return &modelsListOutput{
		ProviderCount: len(catalog.Providers),
		ModelCount:    len(models),
		Models:        models,
	}
}

func sortedModelListEntries(models []modelListEntry) []modelListEntry {
	sorted := append([]modelListEntry(nil), models...)
	sort.SliceStable(sorted, func(i, j int) bool {
		if sorted[i].ProviderID != sorted[j].ProviderID {
			return sorted[i].ProviderID < sorted[j].ProviderID
		}
		return sorted[i].ModelID < sorted[j].ModelID
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
			"[" + agentListModelSummary(agent.Provider, agent.Model) + "]",
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
		fmt.Fprintf(&b, "%s\t%s\t%s\n", row.ProviderID, row.ModelID, modelListReasoningText(row.Reasoning))
	}
	return b.String()
}

func catalogModelListRows(catalog protocol.Catalog, registry *llm.Registry) []modelListEntry {
	var rows []modelListEntry
	for _, provider := range catalog.Providers {
		if provider.ID == "" {
			continue
		}
		for _, model := range provider.Models {
			if model.ID == "" {
				continue
			}
			rows = append(rows, modelListEntry{
				ProviderID:               provider.ID,
				ProviderName:             strings.TrimSpace(provider.Name),
				ModelID:                  model.ID,
				QualifiedID:              provider.ID + ":" + model.ID,
				ModelName:                strings.TrimSpace(model.Name),
				ContextWindow:            model.ContextWindow,
				PricePerMillionTokensUSD: modelListPrice(model.Price),
				Reasoning:                modelListReasoning(registry, provider.ID, model),
			})
		}
	}
	return rows
}

func agentListModelSummary(provider, model string) string {
	provider = strings.TrimSpace(provider)
	model = strings.TrimSpace(model)
	switch {
	case provider == "" && model == "":
		return "default model"
	case provider == "":
		return "default provider/" + model
	case model == "":
		return provider + "/default model"
	default:
		return provider + "/" + model
	}
}

func modelListPrice(price llm.Price) *llm.Price {
	if price.Input == 0 && price.Output == 0 && price.CacheRead == 0 && price.CacheWrite == 0 {
		return nil
	}
	out := price
	return &out
}

func modelListReasoning(registry *llm.Registry, providerID string, model protocol.Model) modelReasoningDetails {
	reasoning := model.Reasoning
	if registry != nil && providerID != "" {
		if info, ok := registry.Lookup(providerID + ":" + model.ID); ok && info.Reasoning != nil {
			reasoning = info.Reasoning
		}
	}
	if reasoning == nil || !reasoning.Supported {
		return modelReasoningDetails{}
	}
	out := modelReasoningDetails{Supported: true}
	if len(reasoning.Options) > 0 {
		out.Options = append([]llm.ReasoningOption(nil), reasoning.Options...)
	}
	return out
}

func modelListReasoningText(reasoning modelReasoningDetails) string {
	if !reasoning.Supported {
		return "-"
	}
	var parts []string
	if values, ok := reasoningEffortValues(reasoning); ok {
		choices := []string{"default"}
		for _, value := range values {
			value = strings.TrimSpace(value)
			if value != "" {
				choices = append(choices, value)
			}
		}
		if len(choices) > 1 {
			parts = append(parts, strings.Join(choices, "/"))
		} else {
			parts = append(parts, "effort=provider-defined")
		}
	}
	if min, max, ok := reasoningBudgetTokenRange(reasoning); ok {
		parts = append(parts, "budget_tokens="+budgetRangeLabel(min, max))
	}
	if reasoningSupportsToggle(reasoning) {
		parts = append(parts, "toggle")
	}
	if len(parts) == 0 && len(reasoning.Options) == 0 {
		return "provider-defined"
	}
	if len(parts) == 0 {
		return "-"
	}
	return strings.Join(parts, ";")
}

func reasoningEffortValues(reasoning modelReasoningDetails) ([]string, bool) {
	for _, opt := range reasoning.Options {
		if opt.Type == "effort" {
			return opt.Values, true
		}
	}
	return nil, false
}

func reasoningBudgetTokenRange(reasoning modelReasoningDetails) (min *int, max *int, ok bool) {
	for _, opt := range reasoning.Options {
		if opt.Type == "budget_tokens" {
			return opt.Min, opt.Max, true
		}
	}
	return nil, nil, false
}

func reasoningSupportsToggle(reasoning modelReasoningDetails) bool {
	if !reasoning.Supported {
		return false
	}
	for _, opt := range reasoning.Options {
		if opt.Type == "toggle" {
			return true
		}
	}
	return len(reasoning.Options) == 0
}

func buildShowConfigOutput(cfg config.Config) (showConfigOutput, error) {
	cfg = showConfigDefaults(cfg)

	agents, err := resolveConfiguredAgents(cfg)
	if err != nil {
		return showConfigOutput{}, err
	}
	agentName, err := resolvedConfigAgentName(cfg, agents)
	if err != nil {
		return showConfigOutput{}, err
	}
	cfg.Agent = agentName

	configuredSystemPrompt, err := resolveAtFile(cfg.SystemPrompt)
	if err != nil {
		return showConfigOutput{}, fmt.Errorf("-system-prompt: %w", err)
	}
	staticSystemPrompt := configuredSystemPrompt
	if staticSystemPrompt == "" {
		staticSystemPrompt = prompts.System()
	}

	for name, a := range agents {
		expanded, err := resolveAtFile(a.Prompt)
		if err != nil {
			return showConfigOutput{}, fmt.Errorf("agent %q prompt: %w", name, err)
		}
		a.Prompt = expanded
		agents[name] = a
	}

	cfg.Agents = configAgentsFromDefinitions(agents)
	cfg.SystemPrompt = staticSystemPrompt

	return showConfigOutput{
		Config: cfg,
	}, nil
}

func fileAgentDefinitions(agents map[string]config.FileAgentConfig) map[string]agentdef.FileDefinition {
	out := make(map[string]agentdef.FileDefinition, len(agents))
	for name, fa := range agents {
		out[name] = agentdef.FileDefinition{
			Description:  fa.Description,
			AllowedTools: fa.AllowedTools,
			MCPTools:     fa.MCPTools,
			Prompt:       fa.Prompt,
			Provider:     fa.Provider,
			Model:        fa.Model,
		}
	}
	return out
}

func configAgentsFromDefinitions(agents map[string]agentdef.Definition) map[string]config.FileAgentConfig {
	out := make(map[string]config.FileAgentConfig, len(agents))
	for name, a := range agents {
		out[name] = config.FileAgentConfig{
			Description:  a.Description,
			AllowedTools: a.AllowedTools,
			MCPTools:     string(a.MCPTools),
			Prompt:       a.Prompt,
			Provider:     a.Provider,
			Model:        a.Model,
		}
	}
	return out
}

func runSessionCommand(args []string, stdout, stderr io.Writer, replayOpts session.ReplayOptions) int {
	if len(args) < 1 {
		fmt.Fprintln(stderr, "usage: harness session <replay|timings> <session-dir>")
		return ui.ExitUsage
	}
	switch args[0] {
	case "replay":
		if len(args) != 2 {
			fmt.Fprintln(stderr, "usage: harness session replay <session-dir>")
			return ui.ExitUsage
		}
		if err := session.Replay(args[1], stdout, replayOpts); err != nil {
			fmt.Fprintf(stderr, "harness: session replay: %v\n", err)
			return ui.ExitRuntime
		}
		return ui.ExitOK
	case "timings":
		if len(args) != 2 {
			fmt.Fprintln(stderr, "usage: harness session timings <session-dir>")
			return ui.ExitUsage
		}
		if err := session.Timings(args[1], stdout); err != nil {
			fmt.Fprintf(stderr, "harness: session timings: %v\n", err)
			return ui.ExitRuntime
		}
		return ui.ExitOK
	default:
		fmt.Fprintf(stderr, "harness: unknown session command %q\n", args[0])
		fmt.Fprintln(stderr, "usage: harness session <replay|timings> <session-dir>")
		return ui.ExitUsage
	}
}

func sessionReplayOptions(env environment, quiet bool) session.ReplayOptions {
	width := markdown.DefaultWidth
	if env.terminalCols != nil {
		if cols := env.terminalCols(); cols > 0 {
			width = cols
		}
	}
	return session.ReplayOptions{
		Markdown: true,
		ANSI:     env.colorTTY && !envColorDisabled(env.getenv),
		Width:    width,
		Quiet:    quiet,
	}
}

// argsQuiet reports whether -q, --quiet, or -quiet appears anywhere in args.
// It is used before config.Load for subcommands dispatched early (e.g. "session").
func argsQuiet(args []string) bool {
	for _, a := range args {
		if a == "-q" || a == "--quiet" || a == "-quiet" {
			return true
		}
	}
	return false
}

func envColorDisabled(getenv func(string) string) bool {
	if getenv == nil {
		return false
	}
	if getenv("NO_COLOR") != "" {
		return true
	}
	switch strings.ToLower(strings.TrimSpace(getenv("HARNESS_NO_COLOR"))) {
	case "1", "true", "yes", "on":
		return true
	default:
		return false
	}
}

func loadConfiguredImages(images []config.ImageAttachment) ([]inputimage.Loaded, error) {
	if len(images) == 0 {
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
	Provider      string
	Model         string
	RegistryModel string
}

func agentModelInputs(def agentdef.Definition, provider, model string) (string, string) {
	if def.Provider != "" {
		provider = def.Provider
	}
	if def.Model != "" {
		model = def.Model
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
	if target != runtime.Agent {
		next, err := resolveAgentCatalogSelection(modelCatalog, def, runtime.ProviderName, runtime.Model)
		if err != nil {
			return delegate.Launch{}, err
		}
		if err := validateReasoningConfig(runtime.Registry, next.RegistryModel, reasoningModeForProvider(modelCatalog, next.Provider), runtime.Reasoning); err != nil {
			return delegate.Launch{}, err
		}
		providerName = next.Provider
		model = next.Model
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
		Provider:          provider,
		ProviderName:      providerName,
		Model:             model,
		ContextWindow:     runtime.ContextWindow,
		Registry:          runtime.Registry,
		Reasoning:         runtime.Reasoning,
		ResponsesStateful: responsesStatefulForProvider(cfg, modelCatalog, providerName),
		System:            system,
		Agent:             target,
		Tools:             reg,
	}, nil
}

func delegateAgentCandidates(agents map[string]agentdef.Definition) []delegate.AgentCandidate {
	names := agentdef.Names(agents)
	out := make([]delegate.AgentCandidate, 0, len(names))
	for _, name := range names {
		a := agents[name]
		out = append(out, delegate.AgentCandidate{Name: name, ToolNames: a.AllowedTools})
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
			Provider:    a.Provider,
			Model:       a.Model,
			Delegatable: delegatable[name],
		})
	}
	return out
}

func resolveCatalogSelection(catalog protocol.Catalog, provider, model, preferredProvider string) (catalogSelection, error) {
	provider = strings.TrimSpace(provider)
	model = strings.TrimSpace(model)
	if p, m, ok := config.SplitProviderModel(model); ok {
		provider = p
		model = m
	}
	if provider != "" && model == "" {
		if p, ok := catalogProvider(catalog, provider); ok && len(p.Models) == 1 {
			model = p.Models[0].ID
		}
	}
	if provider != "" && model != "" {
		p, ok := catalogProvider(catalog, provider)
		if !ok {
			return catalogSelection{}, fmt.Errorf("provider %q is not available from the model proxy", provider)
		}
		if !catalogProviderHasModel(p, model) {
			return catalogSelection{}, fmt.Errorf("provider %q has no model %q", provider, model)
		}
		return catalogSelection{Provider: provider, Model: model, RegistryModel: providerModelKey(provider, model)}, nil
	}
	if provider == "" && model != "" {
		if preferredProvider != "" {
			if p, ok := catalogProvider(catalog, preferredProvider); ok && catalogProviderHasModel(p, model) {
				return catalogSelection{Provider: preferredProvider, Model: model, RegistryModel: providerModelKey(preferredProvider, model)}, nil
			}
		}
		matches := catalogProvidersForModel(catalog, model)
		switch len(matches) {
		case 0:
			return catalogSelection{}, fmt.Errorf("model %q is not available from the model proxy", model)
		case 1:
			return catalogSelection{Provider: matches[0], Model: model, RegistryModel: providerModelKey(matches[0], model)}, nil
		default:
			return catalogSelection{}, fmt.Errorf("model %q is available from multiple providers (%s); use provider:%s", model, strings.Join(matches, ", "), model)
		}
	}
	return catalogSelection{}, fmt.Errorf("a model is required (-model or harness config model)")
}

func catalogProvider(catalog protocol.Catalog, id string) (protocol.Provider, bool) {
	for _, provider := range catalog.Providers {
		if provider.ID == id {
			return provider, true
		}
	}
	return protocol.Provider{}, false
}

func catalogProviderHasModel(provider protocol.Provider, model string) bool {
	for _, entry := range provider.Models {
		if entry.ID == model {
			return true
		}
	}
	return false
}

func catalogProvidersForModel(catalog protocol.Catalog, model string) []string {
	var providers []string
	for _, provider := range catalog.Providers {
		if catalogProviderHasModel(provider, model) {
			providers = append(providers, provider.ID)
		}
	}
	return providers
}

func reasoningModeForProvider(catalog protocol.Catalog, providerID string) string {
	providerID = strings.TrimSpace(providerID)
	if strings.EqualFold(providerID, "openrouter") {
		return "openrouter"
	}
	p, ok := catalogProvider(catalog, providerID)
	apiType := ""
	if ok {
		apiType = strings.ToLower(strings.TrimSpace(p.APIType))
	}
	if apiType == "" {
		apiType = strings.ToLower(providerID)
	}
	switch apiType {
	case "anthropic":
		return "anthropic"
	case "responses":
		return "responses"
	default:
		return "openai"
	}
}

func responsesStatefulForProvider(cfg config.Config, catalog protocol.Catalog, providerID string) bool {
	if !cfg.ResponsesStateful {
		return false
	}
	p, ok := catalogProvider(catalog, providerID)
	return ok && strings.EqualFold(strings.TrimSpace(p.APIType), "responses") && p.ResponsesStateful
}

func sessionResponseStateCompatible(cfg config.Config, catalog protocol.Catalog, s session.Session, provider, model string) bool {
	if s.ResponseState == nil || s.ResponseState.PreviousResponseID == "" {
		return false
	}
	if !responsesStatefulForProvider(cfg, catalog, provider) {
		return false
	}
	if s.Provider != "" && s.Provider != provider {
		return false
	}
	if s.Model != "" && s.Model != model {
		return false
	}
	return s.ResponseState.AnchorMessages <= len(s.Messages)
}

func effectiveReasoningSummary(configured, mode string, interactive, suppressOutput bool) string {
	if suppressOutput {
		return ""
	}
	configured = strings.ToLower(strings.TrimSpace(configured))
	switch configured {
	case "none":
		return ""
	case "auto", "concise", "detailed":
		return configured
	case "":
		if interactive && strings.EqualFold(strings.TrimSpace(mode), "responses") {
			return "auto"
		}
	}
	return ""
}

func providerForReasoningModel(catalog protocol.Catalog, fallbackProvider, model string) string {
	if provider, _, ok := config.SplitProviderModel(model); ok {
		return provider
	}
	model = strings.TrimSpace(model)
	fallbackProvider = strings.TrimSpace(fallbackProvider)
	if fallbackProvider != "" {
		return fallbackProvider
	}
	matches := catalogProvidersForModel(catalog, model)
	if len(matches) == 1 {
		return matches[0]
	}
	return ""
}

func catalogModelPicker(catalog protocol.Catalog) func(ui.PickerIO) (string, error) {
	providerEntries := catalogProviderPickerEntries(catalog)
	if len(providerEntries) == 0 {
		return nil
	}
	return func(pio ui.PickerIO) (string, error) {
		w := pio.Writer
		if w == nil {
			w = io.Discard
		}
		provider, err := ui.Pick(pio.ReadLine, w, ui.PickerOptions[catalogProviderPick]{
			Items:       providerEntries,
			PageSize:    pio.PageSize,
			Prompt:      "Provider (number/id, /search, n/p, q): ",
			Kind:        "provider",
			CancelError: ui.ErrPickerCancelled,
			PrintPage:   ui.PrintProviderPickerPage[catalogProviderPick],
		})
		if err != nil {
			return "", err
		}
		models := catalogModelPickerEntries(provider.provider.Models)
		model, err := ui.Pick(pio.ReadLine, w, ui.PickerOptions[catalogModelPick]{
			Items:       models,
			PageSize:    pio.PageSize,
			Prompt:      "Model (number/id, /search, n/p, q): ",
			Kind:        "model",
			CancelError: ui.ErrPickerCancelled,
			PrintPage: func(w io.Writer, models []catalogModelPick, page, pageSize int, filter string) {
				ui.PrintModelPickerPage(w, provider.provider.ID, models, page, pageSize, filter)
			},
		})
		if err != nil {
			return "", err
		}
		return provider.provider.ID + ":" + model.model.ID, nil
	}
}

func pickStartupModel(readLine func(string) (string, error), w io.Writer, catalog protocol.Catalog, pageSize int) (catalogSelection, error) {
	picker := catalogModelPicker(catalog)
	if picker == nil {
		return catalogSelection{}, fmt.Errorf("model proxy catalog has no selectable models")
	}
	fmt.Fprintln(w, "Select a provider and model to use with harness.")
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

func pickStartupReasoningEffort(readLine func(string) (string, error), w io.Writer, registry *llm.Registry, model string, reasoning llm.ReasoningConfig) (llm.ReasoningConfig, error) {
	info, ok := reasoningInfoForModel(registry, model)
	if !ok || !info.Supported {
		return reasoning, nil
	}
	values, hasEffort := info.EffortValues()
	if !hasEffort || len(values) == 0 {
		return reasoning, nil
	}
	current := strings.TrimSpace(reasoning.Effort)
	currentValid := current == "" || info.SupportsEffort(current)
	for {
		line, err := readLine(fmt.Sprintf("Reasoning effort (default/%s; current: %s): ", strings.Join(values, "/"), effortPromptCurrent(current, currentValid)))
		if err != nil {
			return reasoning, err
		}
		if line == "" {
			if currentValid {
				return reasoning, nil
			}
			reasoning.Effort = ""
			return reasoning, nil
		}
		if strings.EqualFold(line, "q") {
			return reasoning, ui.ErrPickerCancelled
		}
		effort, ok := normalizeEffortInput(line)
		if !ok || (effort != "" && !info.SupportsEffort(effort)) {
			fmt.Fprintf(w, "Invalid reasoning effort %q (supported: default, %s)\n", line, strings.Join(values, ", "))
			continue
		}
		reasoning.Effort = effort
		if effort != "" {
			reasoning.BudgetTokens = nil
			if reasoning.Enabled != nil && !*reasoning.Enabled {
				reasoning.Enabled = nil
			}
		}
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

func effortPromptCurrent(current string, valid bool) string {
	if strings.TrimSpace(current) == "" {
		return "provider default"
	}
	if valid {
		return current
	}
	return current + " (not valid for this model; Enter uses provider default)"
}

func normalizeEffortInput(input string) (string, bool) {
	effort := strings.ToLower(strings.TrimSpace(input))
	switch effort {
	case "":
		return "", false
	case "default", "provider-default":
		return "", true
	default:
		return effort, true
	}
}

func writableConfigPath(args []string, getenv func(string) string) string {
	if p := flagValue(args, "config"); p != "" {
		return p
	}
	return filepath.Join(defaultConfigDir(getenv), "config.json")
}

type catalogProviderPick struct {
	provider protocol.Provider
}

func catalogProviderPickerEntries(catalog protocol.Catalog) []catalogProviderPick {
	seen := make(map[string]bool, len(catalog.Providers))
	entries := make([]catalogProviderPick, 0, len(catalog.Providers))
	for _, provider := range catalog.Providers {
		if provider.ID == "" || len(provider.Models) == 0 || seen[provider.ID] {
			continue
		}
		seen[provider.ID] = true
		entries = append(entries, catalogProviderPick{provider: provider})
	}
	return entries
}

func (p catalogProviderPick) PickerID() string { return p.provider.ID }

func (p catalogProviderPick) PickerName() string {
	if p.provider.Name != "" {
		return p.provider.Name
	}
	return p.provider.ID
}

func (p catalogProviderPick) PickerModelCount() int {
	return len(p.provider.Models)
}

type catalogModelPick struct {
	model protocol.Model
}

func catalogModelPickerEntries(models []protocol.Model) []catalogModelPick {
	entries := make([]catalogModelPick, 0, len(models))
	for _, model := range models {
		if model.ID == "" {
			continue
		}
		entries = append(entries, catalogModelPick{model: model})
	}
	return entries
}

func (m catalogModelPick) PickerID() string { return m.model.ID }
func (m catalogModelPick) PickerName() string {
	if m.model.Name != "" {
		return m.model.Name
	}
	return m.model.ID
}
func (m catalogModelPick) PickerPrice() string   { return formatPickerPrice(m.model.Price) }
func (m catalogModelPick) PickerRelease() string { return "" }

func validateReasoningConfig(registry *llm.Registry, model, mode string, reasoning llm.ReasoningConfig) error {
	reasoning.Effort = strings.ToLower(strings.TrimSpace(reasoning.Effort))
	reasoning.Summary = strings.ToLower(strings.TrimSpace(reasoning.Summary))
	if reasoning.Empty() {
		return nil
	}
	if reasoning.Effort != "" && reasoning.BudgetTokens != nil {
		return fmt.Errorf("reasoning effort and reasoning_budget_tokens cannot both be set")
	}
	if reasoning.Enabled != nil && !*reasoning.Enabled && (reasoning.Effort != "" || reasoning.BudgetTokens != nil || reasoning.Summary != "") {
		return fmt.Errorf("reasoning_enabled=false cannot be combined with reasoning effort, reasoning_budget_tokens, or reasoning_summary")
	}
	if reasoning.BudgetTokens != nil && *reasoning.BudgetTokens < 0 {
		return fmt.Errorf("reasoning_budget_tokens must be non-negative")
	}
	toggleOnly := reasoning.Enabled != nil && reasoning.Effort == "" && reasoning.BudgetTokens == nil && reasoning.Summary == ""
	mode = strings.ToLower(strings.TrimSpace(mode))
	if reasoning.BudgetTokens != nil && !reasoningModeSupportsBudgetTokens(mode) {
		return fmt.Errorf("provider mode %q does not support reasoning_budget_tokens", reasoningModeLabel(mode))
	}
	if reasoning.Summary != "" && mode != "responses" {
		return fmt.Errorf("provider mode %q does not support reasoning_summary", reasoningModeLabel(mode))
	}
	if toggleOnly && !reasoningModeSupportsToggle(mode) {
		return fmt.Errorf("provider mode %q does not support reasoning_enabled", reasoningModeLabel(mode))
	}
	if registry == nil {
		return nil
	}
	info, ok := registry.Lookup(model)
	if !ok || info.Reasoning == nil {
		return nil
	}
	if !info.Reasoning.Supported {
		if reasoning.Effort != "" {
			return fmt.Errorf("model %q does not support reasoning effort", model)
		}
		if reasoning.BudgetTokens != nil {
			return fmt.Errorf("model %q does not support reasoning_budget_tokens", model)
		}
		if reasoning.Summary != "" {
			return fmt.Errorf("model %q does not support reasoning_summary", model)
		}
		if toggleOnly {
			return fmt.Errorf("model %q does not support reasoning_enabled", model)
		}
		return fmt.Errorf("model %q does not support reasoning controls", model)
	}
	if reasoning.Effort != "" && !info.Reasoning.SupportsEffort(reasoning.Effort) {
		if values, ok := info.Reasoning.EffortValues(); ok && len(values) > 0 {
			return fmt.Errorf("model %q does not support reasoning effort %q (supported: %s)", model, reasoning.Effort, strings.Join(values, ", "))
		}
		return fmt.Errorf("model %q does not support reasoning effort", model)
	}
	if reasoning.BudgetTokens != nil && !info.Reasoning.SupportsBudgetTokens(*reasoning.BudgetTokens) {
		if min, max, ok := info.Reasoning.BudgetTokenRange(); ok {
			return fmt.Errorf("model %q does not support reasoning_budget_tokens=%d (supported: %s)", model, *reasoning.BudgetTokens, budgetRangeLabel(min, max))
		}
		return fmt.Errorf("model %q does not support reasoning_budget_tokens", model)
	}
	if toggleOnly && !info.Reasoning.SupportsToggle() {
		return fmt.Errorf("model %q does not support reasoning_enabled", model)
	}
	return nil
}

func compatibleReasoningForModel(registry *llm.Registry, model, mode string, reasoning llm.ReasoningConfig) llm.ReasoningConfig {
	reasoning.Effort = strings.ToLower(strings.TrimSpace(reasoning.Effort))
	reasoning.Summary = strings.ToLower(strings.TrimSpace(reasoning.Summary))
	if reasoning.Empty() {
		return reasoning
	}
	mode = strings.ToLower(strings.TrimSpace(mode))
	info, ok := reasoningInfoForModel(registry, model)
	if ok && !info.Supported {
		return llm.ReasoningConfig{}
	}
	if reasoning.Effort != "" && ok && !info.SupportsEffort(reasoning.Effort) {
		reasoning.Effort = ""
	}
	if reasoning.BudgetTokens != nil {
		if !reasoningModeSupportsBudgetTokens(mode) || (ok && !info.SupportsBudgetTokens(*reasoning.BudgetTokens)) {
			reasoning.BudgetTokens = nil
		}
	}
	if reasoning.Enabled != nil && (!reasoningModeSupportsToggle(mode) || (ok && !info.SupportsToggle())) {
		reasoning.Enabled = nil
	}
	if reasoning.Summary != "" && mode != "responses" {
		reasoning.Summary = ""
	}
	return reasoning
}

func reasoningModeSupportsBudgetTokens(mode string) bool {
	return mode == "openrouter" || mode == "anthropic"
}

func reasoningModeSupportsToggle(mode string) bool {
	return mode == "openrouter"
}

func reasoningModeLabel(mode string) string {
	if mode == "" {
		return "openai"
	}
	return mode
}

func budgetRangeLabel(min, max *int) string {
	switch {
	case min != nil && max != nil:
		return fmt.Sprintf("%d..%d", *min, *max)
	case min != nil:
		return fmt.Sprintf(">=%d", *min)
	case max != nil:
		return fmt.Sprintf("<=%d", *max)
	default:
		return "provider-defined range"
	}
}

// formatPickerPrice formats an llm.Price as "$in/$out" per 1M tokens,
// or "" when no price is configured.
func formatPickerPrice(p llm.Price) string {
	if p.Input == 0 && p.Output == 0 && p.CacheRead == 0 && p.CacheWrite == 0 {
		return ""
	}
	return fmt.Sprintf("$%s/$%s", formatPriceComponent(p.Input), formatPriceComponent(p.Output))
}

func formatPriceComponent(v float64) string {
	if v == float64(int64(v)) {
		return fmt.Sprintf("%.0f", v)
	}
	return strings.TrimRight(strings.TrimRight(fmt.Sprintf("%.2f", v), "0"), ".")
}

func providerModelKey(provider, model string) string {
	if provider == "" || model == "" {
		return model
	}
	return provider + ":" + model
}

func pickerPageSize(env environment) int {
	rows := 0
	if env.terminalRows != nil {
		rows = env.terminalRows()
	}
	return ui.PickerPageSize(rows)
}

// resolveConfigPath determines the config-file path config.Load should read: an
// explicit -config flag, or the implicit ~/.config/harness/config.json only when
// it exists (so an absent default is silently skipped, but a typo'd -config
// surfaces as an error in Load).
func resolveConfigPath(args []string, getenv func(string) string) string {
	if p := flagValue(args, "config"); p != "" {
		return p
	}
	def := filepath.Join(defaultConfigDir(getenv), "config.json")
	if _, err := os.Stat(def); err == nil {
		return def
	}
	return ""
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

// flagValue extracts a string flag's value from raw args, supporting both
// -flag=value and -flag value forms. It returns "" when absent.
func flagValue(args []string, name string) string {
	value, _ := flagValueOK(args, name)
	return value
}

func flagValueOK(args []string, name string) (string, bool) {
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "--" {
			break
		}
		for _, prefix := range []string{"-" + name, "--" + name} {
			if a == prefix {
				if i+1 < len(args) {
					return args[i+1], true
				}
				return "", true
			}
			if strings.HasPrefix(a, prefix+"=") {
				return a[len(prefix)+1:], true
			}
		}
	}
	return "", false
}

func explicitReasoningOutputFlag(args []string) bool {
	value, ok := flagValueOK(args, "reasoning-summary")
	if !ok {
		return false
	}
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "auto", "concise", "detailed", "on", "true", "enabled", "enable":
		return true
	default:
		return false
	}
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

// loadAgentsMD reads AGENTS.md from dir when present. A missing file returns
// an empty string with no error; other read failures (e.g. permissions) are
// returned so the user isn't silently surprised.
func loadAgentsMD(dir string) (string, error) {
	return loadAgentsMDFile(projectAgentsMDPath(dir))
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
