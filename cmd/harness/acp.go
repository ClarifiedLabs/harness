package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"harness/internal/acp"
	"harness/internal/acpagent"
	"harness/internal/acpclient"
	"harness/internal/acptool"
	"harness/internal/agent"
	"harness/internal/agentdef"
	"harness/internal/agentsession"
	"harness/internal/background"
	"harness/internal/buildinfo"
	"harness/internal/cli"
	"harness/internal/config"
	"harness/internal/delegate"
	"harness/internal/execution"
	"harness/internal/hooks"
	"harness/internal/llm"
	"harness/internal/logging"
	"harness/internal/mcp"
	"harness/internal/mcpchild"
	"harness/internal/mcptools"
	modelclient "harness/internal/modelproxy/client"
	"harness/internal/modelproxy/protocol"
	"harness/internal/otel"
	"harness/internal/plan"
	"harness/internal/session"
	"harness/internal/sessionrec"
	"harness/internal/skills"
	"harness/internal/sysprompt"
	"harness/internal/taskcontext"
	"harness/internal/todo"
	"harness/internal/tools"
	"harness/internal/tracing"
	"harness/internal/ui"
)

const (
	acpRootCloseTimeout    = 10 * time.Second
	maxACPClientMCPServers = 32
)

type acpRootBuilder func(context.Context, environment, acpagent.SessionConfig, *slog.Logger, config.Result) (acpagent.RootSession, error)

type acpRootFactory struct {
	env       environment
	flags     cli.Values
	logger    *slog.Logger
	logLevel  *slog.LevelVar
	launchCWD string
	build     acpRootBuilder

	mu                   sync.Mutex
	boundCWD             string
	telemetry            *rootTelemetry
	telemetryInitialized bool
	roots                []*acpTrackedRoot
	closed               bool
	closeDone            chan struct{}
	construct            chan struct{}
	pending              map[*acpConstruction]struct{}
}

func runACPServe(env environment, invocation cli.Invocation) int {
	// The first root resolves process configuration, including diagnostic level.
	// Keep server/telemetry loggers on the same atomically adjustable stderr sink.
	logLevel := new(slog.LevelVar)
	logger := slog.New(logging.NewPlainHandler(env.stderr, logging.HandlerOptions{Level: logLevel}))
	launchCWD, err := os.Getwd()
	if err != nil {
		return fail(env.stderr, ui.ExitRuntime, "acp serve: resolve launch cwd: %v", err)
	}
	launchCWD, err = filepath.EvalSymlinks(launchCWD)
	if err != nil {
		return fail(env.stderr, ui.ExitRuntime, "acp serve: resolve launch cwd: %v", err)
	}
	factory := acpagent.Factory(&acpRootFactory{env: env, flags: invocation.Flags, logger: logger, logLevel: logLevel, launchCWD: launchCWD})
	if env.acpRootFactory != nil {
		factory = env.acpRootFactory(invocation)
	}
	// The factory joins any root cleanup that outlives Serve's close deadline
	// before the shared process exporter sends its terminal snapshot.
	if closer, ok := factory.(interface{ Close() }); ok {
		defer closer.Close()
	}
	ctx, cancel, interrupted := signalCancelContext(env.sigCh)
	defer cancel()
	conn := &acpCommandConn{Reader: env.stdin, Writer: env.stdout}
	if err := acpagent.Serve(ctx, conn, acpagent.Options{Factory: factory, Logger: logger, CloseTimeout: acpRootCloseTimeout}); err != nil {
		if interrupted() || errors.Is(err, context.Canceled) {
			return ui.ExitInterrupt
		}
		return fail(env.stderr, ui.ExitRuntime, "acp serve: %v", err)
	}
	return ui.ExitOK
}

type acpCommandConn struct {
	io.Reader
	io.Writer
}

func (*acpCommandConn) Close() error { return nil }

func (f *acpRootFactory) New(ctx context.Context, request acpagent.SessionConfig) (acpagent.RootSession, error) {
	ctx, finish, err := f.beginConstruction(ctx)
	if err != nil {
		return nil, err
	}
	defer finish()

	cwd, err := filepath.EvalSymlinks(request.CWD)
	if err != nil {
		return nil, fmt.Errorf("resolve ACP cwd %s: %w", request.CWD, err)
	}
	cwd, err = filepath.Abs(cwd)
	if err != nil {
		return nil, fmt.Errorf("resolve ACP cwd %s: %w", request.CWD, err)
	}
	if f.boundCWD != "" && cwd != f.boundCWD {
		return nil, fmt.Errorf("ACP process is bound to cwd %s; a later session cannot use %s", f.boundCWD, cwd)
	}
	if f.launchCWD == "" {
		f.launchCWD, err = os.Getwd()
		if err != nil {
			return nil, fmt.Errorf("resolve ACP launch cwd: %w", err)
		}
	}

	// Resolve an explicit relative --config or HARNESS_CONFIG from the launch
	// directory, not from an untrusted workspace selected later by session/new.
	// ConfigBaseDir avoids a transient process-wide chdir while an older runtime
	// may still be quiescing.
	load := harnessLoadOptions(f.env, nil)
	load.ConfigBaseDir = f.launchCWD
	load.WorkingDir = cwd
	result, err := config.LoadParsed(load, f.flags)
	if err != nil {
		if f.logger != nil {
			f.logger.Error("acp serve: configuration load failed", "err", acp.SanitizeModelFacingText(err.Error()))
		}
		return nil, errors.New("ACP root configuration is invalid; see server stderr")
	}
	telemetry, err := f.telemetryFor(result.Config)
	if err != nil {
		if f.logger != nil {
			f.logger.Error("acp serve: telemetry configuration invalid", "err", err)
		}
		return nil, errors.New("ACP root telemetry configuration is invalid; see server stderr")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := os.Chdir(cwd); err != nil {
		return nil, fmt.Errorf("set ACP cwd %s: %w", cwd, err)
	}
	if f.boundCWD == "" {
		f.boundCWD = cwd
	}
	request.CWD = cwd
	build := f.build
	if build == nil {
		build = func(ctx context.Context, env environment, request acpagent.SessionConfig, logger *slog.Logger, result config.Result) (acpagent.RootSession, error) {
			return newACPRootSession(ctx, env, request, logger, result, telemetry)
		}
	}
	root, err := build(ctx, f.env, request, f.logger, result)
	f.mu.Lock()
	if f.closed || ctx.Err() != nil {
		f.mu.Unlock()
		if root != nil {
			cleanupCtx, cancel := context.WithTimeout(context.Background(), acpRootCloseTimeout)
			defer cancel()
			_ = closeTrackedACPRoot(cleanupCtx, root)
		}
		return nil, errors.Join(err, errors.New("ACP root factory closed during construction"))
	}
	defer f.mu.Unlock()
	if root == nil {
		return nil, err
	}
	tracked := trackACPRoot(root)
	// Completed sessions must not accumulate in a long-lived ACP process.
	pending := f.roots[:0]
	for _, previous := range f.roots {
		select {
		case <-previous.settled:
		default:
			pending = append(pending, previous)
		}
	}
	f.roots = append(pending, tracked)
	return tracked, err
}

func newACPRootSession(ctx context.Context, env environment, request acpagent.SessionConfig, logger *slog.Logger, result config.Result, telemetry *rootTelemetry) (_ *acpRootSession, retErr error) {
	now := env.now
	if now == nil {
		now = time.Now
	}
	cfg := result.Config
	var err error
	var tracer *tracing.Tracer
	if cfg.TraceProxy {
		tracer, err = tracing.NewTracer(true)
		if err != nil {
			return nil, fmt.Errorf("trace proxy: %w", err)
		}
	}
	proxyURL := cfg.ModelProxyURL
	if proxyURL == "" {
		proxyURL = protocol.DefaultURL
	}
	proxyClient, err := modelclient.New(proxyURL, nil, modelclient.WithAPIKey(cfg.ModelProxyAPIKey), modelclient.WithTracer(tracer))
	if err != nil {
		return nil, err
	}
	catalog, err := proxyClient.Catalog(ctx)
	if err != nil {
		return nil, fmt.Errorf("model proxy: %w", err)
	}
	registry := modelclient.Registry(catalog)
	registry.SetDefaultContextWindow(cfg.DefaultContextWindow)
	agents, err := resolveConfiguredAgents(cfg)
	if err != nil {
		return nil, err
	}
	agentName := cfg.Agent
	if agentName == "" {
		agentName = agentdef.Default
	}
	definition, ok := agents[agentName]
	if !ok {
		return nil, fmt.Errorf("unknown agent %q (available: %s)", agentName, strings.Join(agentdef.Names(agents), ", "))
	}
	providerName, modelName := agentModelInputs(definition, cfg.Provider, cfg.Model)
	selection, err := resolveCatalogSelection(catalog, providerName, modelName, cfg.Provider)
	if err != nil {
		return nil, err
	}
	cfg.Provider, cfg.Model = selection.Provider, selection.Model
	reasoning := llm.ReasoningConfig{Profile: cfg.Reasoning}
	if definition.Reasoning != "" {
		reasoning.Profile = definition.Reasoning
	}
	reasoning.Summary = effectiveReasoningSummary(cfg.ReasoningSummary, reasoningModeForProvider(catalog, selection.Provider), true, false)
	if err := validateReasoningConfig(registry, selection.RegistryModel, reasoningModeForProvider(catalog, selection.Provider), reasoning); err != nil {
		return nil, err
	}

	created := now()
	recordingID, err := tracing.NewSpanID()
	if err != nil {
		return nil, fmt.Errorf("allocate ACP recording ID: %w", err)
	}
	sessionPath := session.DefaultPathForID(stateDir(env.lookup), created, recordingID)
	lock, err := session.AcquireLock(sessionPath)
	if err != nil {
		return nil, err
	}
	defer func() {
		if retErr != nil {
			_ = lock.Close()
		}
	}()
	jobs := background.NewManager(background.Options{MaxContextBytes: cfg.ToolResultMaxBytes, Now: now})
	agentSessions := agentsession.NewManager(agentsession.Options{Background: jobs, Canceler: jobs, Now: now})
	toolCatalog := newRootToolCatalog(cfg, jobs)
	var cleanups []func(context.Context)
	cleanupAll := func(cleanupCtx context.Context) {
		for i := len(cleanups) - 1; i >= 0; i-- {
			cleanups[i](cleanupCtx)
		}
	}
	defer func() {
		if retErr != nil {
			closeCtx, cancel := context.WithTimeout(context.Background(), acpRootCloseTimeout)
			defer cancel()
			_ = agentSessions.CloseAll(closeCtx)
			jobs.ShutdownAndWait(time.Second)
			cleanupAll(closeCtx)
		}
	}()

	var summaries []mcptools.Summary
	if cfg.MCP.Enable {
		_, summary, cleanup, connected := setupMCP(ctx, cfg.MCP, toolCatalog, logger, tracer)
		cleanups = append(cleanups, func(context.Context) { cleanup() })
		if connected {
			summaries = append(summaries, summary)
		}
	}
	if localMCPEnabled(cfg.MCP.Local, true) {
		_, summary, cleanup, connected := setupLocalMCP(ctx, cfg.MCP.Local, cfg.MCP.Local.EnableSet, toolCatalog, logger)
		cleanups = append(cleanups, func(context.Context) { cleanup() })
		if connected {
			summaries = append(summaries, summary)
		}
	}
	clientSummary, clientCleanups, err := setupACPClientMCP(ctx, request.CWD, request.MCPServers, toolCatalog, logger)
	if err != nil {
		return nil, err
	}
	cleanups = append(cleanups, clientCleanups...)
	summaries = append(summaries, clientSummary)

	var lspRuntime *lspRuntime
	var lspSummary mcptools.Summary
	var lspHint string
	if cfg.LSP.Enable {
		lspRuntime, err = newLSPRuntime(ctx, cfg.LSP, toolCatalog, logger)
		if err != nil {
			return nil, fmt.Errorf("lsp: %w", err)
		}
		cleanups = append(cleanups, func(context.Context) { lspRuntime.Shutdown() })
		lspSummary, lspHint = lspRuntime.ActiveSummary(), lspRuntime.SystemHint()
		installMutationDiagnostics(toolCatalog, lspRuntime)
	}
	var runtimeHints []string
	if ripgrepAvailable() {
		runtimeHints = append(runtimeHints, rgSystemHint)
	}
	if lspHint != "" {
		runtimeHints = append(runtimeHints, lspHint)
	}
	if cfg.LSP.Serena.Enable {
		summary, cleanup, connected := setupSerena(ctx, cfg.LSP.Serena, toolCatalog, logger)
		cleanups = append(cleanups, func(context.Context) { cleanup() })
		if connected {
			summaries = append(summaries, summary)
			if summary.Total > 0 {
				runtimeHints = append(runtimeHints, serenaSystemHint)
			}
		}
	}
	allMCP := mergeMCPSummaries(summaries...)
	augmentAgentsWithMCP(agents, allMCP.Names, allMCP.ReadOnlyNames)
	if lspRuntime != nil {
		bases := mcpExposingAgentBases(agents)
		applyLSPExposure(agents, lspSummary, true, captureLSPExplicitTools(agents, bases))
	}
	for name, value := range agents {
		value.Prompt, err = resolveAtFile(value.Prompt)
		if err != nil {
			return nil, fmt.Errorf("agent %q prompt: %w", name, err)
		}
		agents[name] = value
	}
	definition = agents[agentName]

	configuredSystem, err := resolveAtFile(cfg.SystemPrompt)
	if err != nil {
		return nil, fmt.Errorf("system prompt: %w", err)
	}
	userPath := userAgentsMDPath(env.lookup)
	userAgents, err := loadAgentsMDFile(userPath)
	if err != nil {
		return nil, err
	}
	projectPath := projectAgentsMDPath(request.CWD)
	projectAgents, err := loadAgentsMDFile(projectPath)
	if err != nil {
		return nil, err
	}
	var skillWarnings skills.Warnings
	skillDirs := skills.AncestorSkillDirs(request.CWD, homeDir(env.lookup))
	discoveredSkills := skills.Discover(skillDirs, &skillWarnings)
	for _, warning := range skillWarnings {
		logger.Warn("skills: " + warning)
	}
	skillCatalog, skillReport := skills.BuildCatalogBudgeted(discoveredSkills, skills.CatalogBudget(llm.EffectiveContextWindow(cfg.ContextWindow, registry.ContextWindow(selection.RegistryModel))))
	buildSystem := func(agentPrompt string) string {
		return acp.SanitizeModelFacingText(sysprompt.Build(sysprompt.Options{StaticPrompt: configuredSystem, NoEnv: cfg.NoEnv, UserAgentsMD: userAgents, ProjectAgentsMD: projectAgents, SkillsCatalog: skillCatalog, RuntimeHints: runtimeHints, AgentPrompt: agentPrompt, Env: sysprompt.EnvOptions{Dir: request.CWD}}))
	}

	var otelSink *otel.Sink
	var scope execution.Scope
	if telemetry != nil {
		otelSink = telemetry.NewSink(toolCatalog, cfg.Provider, cfg.Model, agentName)
		otelSink.SetIdentity(recordingID, cfg.Provider, cfg.Model, agentName)
		otelSink.RecordSkillCatalog(skillReport)
		scope = otelSink.Scope()
	}
	build := buildinfo.Current()
	buildMeta := session.BuildMetadata{Version: build.Version, Commit: build.Commit, Date: build.Date, Modified: build.Modified}
	runtimeProfile := session.RuntimeProfile{RetentionPolicy: cfg.RetentionPolicy, ContextWindow: cfg.ContextWindow, ToolResultMaxBytes: cfg.ToolResultMaxBytes, ToolResultMaxLines: cfg.ToolResultMaxLines, CompactToolResultMaxBytes: cfg.CompactToolResultMaxBytes, CompactTimeoutSeconds: cfg.CompactTimeoutSeconds, ResponsesStateful: responsesStatefulForProvider(cfg, catalog, cfg.Provider), NativeCompaction: nativeCompactionForProvider(catalog, cfg.Provider), DelegateMaxTurns: cfg.DelegateMaxTurns, DelegateMaxActive: cfg.DelegateMaxActive, SearchBackend: searchBackend(), StagnationNudge: cfg.StagnationNudge}
	state := delegate.NewState(delegate.Runtime{Execution: scope, ProviderName: cfg.Provider, Model: cfg.Model, ReasoningReplayDomain: selection.ReasoningReplayDomain, ContextWindow: cfg.ContextWindow, MaxOutputTokens: cfg.MaxOutputTokens, Registry: registry, Reasoning: reasoning, ServerTools: webSearchServerToolsForModel(cfg.Provider, registry, selection.RegistryModel, cfg.WebSearch), ResponsesStateful: responsesStatefulForProvider(cfg, catalog, cfg.Provider), NativeCompaction: nativeCompactionForProvider(catalog, cfg.Provider), Agent: agentName, SessionPath: sessionPath, CWD: request.CWD, MaxPromptTokens: cfg.MaxPromptTokens, MaxPromptCostUSD: cfg.MaxPromptCostUSD, Build: buildMeta, RuntimeProfile: runtimeProfile})
	resolveDelegate := func(runtime delegate.Runtime, name string) (delegate.Launch, error) {
		launch, err := resolveDelegateLaunch(runtime, name, agents, toolCatalog, nil, catalog, proxyClient, buildSystem, cfg)
		if err != nil {
			return launch, err
		}
		return acpDelegateLaunch(launch), nil
	}
	runner := delegate.NewRunner(state.Snapshot, resolveDelegate, delegate.Options{AstraNativeSteering: cfg.AstraNativeSteering, ExperimentalAsyncTools: cfg.ExperimentalAsyncTools, MaxTurns: cfg.DelegateMaxTurns, MaxDepth: cfg.DelegateMaxDepth, MaxActiveDescendants: cfg.DelegateMaxActive, CompactKeepTurns: cfg.CompactKeepTurns, CompactKeepTokens: cfg.CompactKeepTokens, CompactTriggerPercent: cfg.CompactTriggerPercent, CompactTargetPercent: cfg.CompactTargetPercent, CompactInputTokens: cfg.CompactInputTokens, CompactGrowthTokens: cfg.CompactGrowthTokens, DisableAutoCompaction: !cfg.CompactAutoEnabled, CompactSummaryMaxTokens: cfg.CompactSummaryMaxTokens, CompactTimeout: time.Duration(cfg.CompactTimeoutSeconds) * time.Second, CompactToolResultMaxBytes: cfg.CompactToolResultMaxBytes, RetentionKeepTurns: cfg.RetentionKeepTurns, RetentionResultHeadBytes: cfg.RetentionResultHeadBytes, RetentionPolicy: agent.RetentionPolicy(cfg.RetentionPolicy), ShowDiffs: cfg.ShowDiffs, Now: now, AgentCandidates: func(delegate.Runtime) []delegate.AgentCandidate { return delegateAgentCandidates(agents) }})
	todos := todo.NewStore()
	plans := plan.NewStore()
	toolCatalog.Register(delegate.NewToolWithSessions(runner, agentSessions, jobs))
	toolCatalog.Register(background.NewJobsTool(jobs))
	toolCatalog.Register(todo.NewToolWithTextSanitizer(todos, acp.SanitizeModelFacingText))
	toolCatalog.Register(plan.NewToolWithTextSanitizer(plans, func() string { return sessionPath }, acp.SanitizeModelFacingText))
	if cfg.CodexExperimentalContextManagement {
		manager := taskcontext.New(func() string { return sessionPath })
		manager.SetEnabled(func() bool { return contextManagementForProvider(cfg, catalog, state.Snapshot().ProviderName) })
		manager.Register(toolCatalog)
	}
	toolCatalog.Register(acptool.NewTool(agentSessions, cfg.ACP, func(target config.ACPTargetConfig, cwd string) agentsession.Factory {
		return acpclient.NewFactory(acpclient.Options{Argv: append([]string{target.Command}, target.Args...), Env: acpTargetEnvironment(os.Environ(), target.Env), CWD: cwd, ClientInfo: &acp.Implementation{Name: "harness", Title: "Harness", Version: build.Version}, Logger: logger, LogStderr: func(line string) { logger.Warn("acp: child stderr: "+line, logging.Category("acp")) }})
	}))
	toolCatalog.Register(agentsession.NewTool(agentSessions))
	toolRegistry, err := subsetForAgentTools(toolCatalog, definition.AllowedTools, nil)
	if err != nil {
		return nil, fmt.Errorf("agent %q: %w", agentName, err)
	}
	systemPrompt := buildSystem(definition.Prompt)
	var hookRunner *hooks.Runner
	if !cfg.Hooks.Empty() {
		hookRunner = &hooks.Runner{Config: cfg.Hooks, CWD: request.CWD, Model: cfg.Model}
		hookRunner.SetSession(sessionPath)
	}
	rootAgentCfg := rootAgentConfig{Execution: scope, Config: cfg, Registry: registry, Reasoning: reasoning, ReasoningReplayDomain: selection.ReasoningReplayDomain, ServerTools: webSearchServerToolsForModel(cfg.Provider, registry, selection.RegistryModel, cfg.WebSearch), Hooks: hookRunner, Interactive: true, Now: now, Sleep: env.agentSleep, ResponsesStateful: responsesStatefulForProvider(cfg, catalog, cfg.Provider), NativeCompaction: nativeCompactionForProvider(catalog, cfg.Provider)}
	ag := newRootAgent(proxyClient.Provider(cfg.Provider), toolRegistry, rootAgentCfg)
	ag.SetSystem(systemPrompt)
	ag.SetTranscriptSanitizer(sanitizeACPTranscript)
	ag.SetRequestSanitizer(sanitizeACPRequest)
	state.Set(delegate.Runtime{Execution: scope, Provider: proxyClient.Provider(cfg.Provider), ProviderName: cfg.Provider, Model: cfg.Model, ContextWindow: cfg.ContextWindow, MaxOutputTokens: cfg.MaxOutputTokens, Registry: registry, Reasoning: reasoning, ReasoningReplayDomain: selection.ReasoningReplayDomain, ServerTools: rootAgentCfg.ServerTools, ResponsesStateful: rootAgentCfg.ResponsesStateful, NativeCompaction: rootAgentCfg.NativeCompaction, System: systemPrompt, Agent: agentName, ToolNames: toolRegistry.Names(), SessionPath: sessionPath, CWD: request.CWD, CacheAffinityID: ag.CacheAffinityID(), MaxPromptTokens: cfg.MaxPromptTokens, MaxPromptCostUSD: cfg.MaxPromptCostUSD, Build: buildMeta, RuntimeProfile: runtimeProfile})
	ag.SetTools(toolRegistry)
	ag.SetCompactionArchiver(func(ctx context.Context, archive agent.CompactionArchive) (string, error) {
		return session.SaveCompaction(sessionPath, session.Compaction{Time: now(), Messages: archive.Messages, Summary: archive.Summary, SummarySource: archive.SummarySource, FallbackReason: archive.FallbackReason, Usage: archive.Usage, Focus: archive.Focus, ReadFiles: archive.ReadFiles, ReadFilesOmitted: archive.ReadFilesOmitted, ModifiedFiles: archive.ModifiedFiles})
	})

	return &acpRootSession{otel: otelSink, agent: ag, cfg: cfg, registry: registry, registryModel: selection.RegistryModel, agentName: agentName, system: systemPrompt, path: sessionPath, cwd: request.CWD, created: created, now: now, build: buildMeta, runtime: runtimeProfile, todos: todos, plans: plans, jobs: jobs, agentSessions: agentSessions, lock: lock, cleanups: cleanups}, nil
}

// acpDelegateLaunch extends the ACP host-boundary sanitizers to delegate child
// sessions: control sequences from the client must stay out of child
// transcripts and provider-bound requests exactly as at the root.
func acpDelegateLaunch(launch delegate.Launch) delegate.Launch {
	launch.TranscriptSanitizer = sanitizeACPTranscript
	launch.RequestSanitizer = sanitizeACPRequest
	launch.StateTextSanitizer = acp.SanitizeModelFacingText
	return launch
}

func setupACPClientMCP(ctx context.Context, cwd string, servers []acp.MCPServer, catalog *tools.Registry, logger *slog.Logger) (mcptools.Summary, []func(context.Context), error) {
	if err := validateACPClientMCP(servers); err != nil {
		return mcptools.Summary{}, nil, err
	}
	var summaries []mcptools.Summary
	var cleanups []func(context.Context)
	fail := func(err error) (mcptools.Summary, []func(context.Context), error) {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), acpRootCloseTimeout)
		defer cancel()
		for i := len(cleanups) - 1; i >= 0; i-- {
			cleanups[i](cleanupCtx)
		}
		return mcptools.Summary{}, nil, err
	}
	for _, spec := range servers {
		name := spec.Name
		env, err := acpMCPEnvironment(os.Environ(), spec.Env)
		if err != nil {
			return fail(fmt.Errorf("MCP server %q: %w", name, err))
		}
		child, err := mcpchild.SpawnInDir(spec.Command, spec.Args, env, cwd, func(line string) {
			logger.Warn("ACP client MCP child stderr", "server", name, "line", acp.SanitizeModelFacingText(line))
		})
		if err != nil {
			return fail(err)
		}
		var dialMu sync.Mutex
		dialed := false
		conn := mcptools.NewConn(mcptools.Options{Info: mcp.Implementation{Name: "harness", Version: buildinfo.Version}, Logger: logger, Dial: func(context.Context) (io.ReadWriteCloser, error) {
			dialMu.Lock()
			defer dialMu.Unlock()
			if dialed {
				return nil, errors.New("ACP MCP child cannot reconnect")
			}
			dialed = true
			return child.Conn(), nil
		}})
		cleanup := func(closeCtx context.Context) { _ = conn.Close(); child.Close(closeCtx) }
		cleanups = append(cleanups, cleanup)
		summary, err := mcptools.RegisterWithOptions(ctx, catalog, conn, mcptools.RegisterOptions{TrustReadOnlyHint: true, Namespace: name})
		if err != nil {
			return fail(fmt.Errorf("initialize MCP server %q: %w", name, err))
		}
		summaries = append(summaries, summary)
	}
	return mergeMCPSummaries(summaries...), cleanups, nil
}

func validateACPClientMCP(servers []acp.MCPServer) error {
	if len(servers) > maxACPClientMCPServers {
		return fmt.Errorf("ACP session provides %d MCP servers; maximum is %d", len(servers), maxACPClientMCPServers)
	}
	seen := make(map[string]struct{}, len(servers))
	for _, spec := range servers {
		name := spec.Name
		if !validACPMCPNamespace(name) {
			return fmt.Errorf("MCP server name %q must use 1-56 ASCII letters, digits, underscores, or hyphens", name)
		}
		if _, ok := seen[name]; ok {
			return fmt.Errorf("duplicate MCP server name %q", name)
		}
		seen[name] = struct{}{}
		if spec.Type != acp.MCPTransportStdio {
			return fmt.Errorf("MCP server %q uses unsupported transport %q", name, spec.Type)
		}
		if !filepath.IsAbs(spec.Command) {
			return fmt.Errorf("MCP server %q command must be absolute", name)
		}
		info, err := os.Stat(spec.Command)
		if err != nil || info.IsDir() || info.Mode().Perm()&0o111 == 0 {
			return fmt.Errorf("MCP server %q command is not executable: %s", name, spec.Command)
		}
		if _, err := acpMCPEnvironment(nil, spec.Env); err != nil {
			return fmt.Errorf("MCP server %q: %w", name, err)
		}
	}
	return nil
}

func validACPMCPNamespace(name string) bool {
	if len(name) == 0 || len(name) > 56 {
		return false
	}
	for i := 0; i < len(name); i++ {
		c := name[i]
		if (c < 'a' || c > 'z') && (c < 'A' || c > 'Z') && (c < '0' || c > '9') && c != '_' && c != '-' {
			return false
		}
	}
	return true
}

func acpMCPEnvironment(parent []string, entries []acp.EnvVariable) ([]string, error) {
	values := make(map[string]string, len(parent)+len(entries))
	for _, item := range parent {
		if name, value, ok := strings.Cut(item, "="); ok && name != "" {
			values[name] = value
		}
	}
	seen := make(map[string]struct{}, len(entries))
	for _, entry := range entries {
		if _, ok := seen[entry.Name]; ok {
			return nil, fmt.Errorf("duplicate environment variable %q", entry.Name)
		}
		seen[entry.Name] = struct{}{}
		values[entry.Name] = entry.Value
	}
	names := make([]string, 0, len(values))
	for name := range values {
		names = append(names, name)
	}
	sort.Strings(names)
	out := make([]string, 0, len(names))
	for _, name := range names {
		out = append(out, name+"="+values[name])
	}
	return out, nil
}

type acpRootSession struct {
	mu sync.Mutex

	agent         *agent.Agent
	otel          *otel.Sink
	cfg           config.Config
	registry      *llm.Registry
	registryModel string
	agentName     string
	system        string
	path          string
	cwd           string
	created       time.Time
	now           func() time.Time
	build         session.BuildMetadata
	runtime       session.RuntimeProfile
	todos         *todo.Store
	plans         *plan.Store
	jobs          *background.Manager
	agentSessions *agentsession.Manager
	lock          *session.Lock
	cleanups      []func(context.Context)
	prompt        int
	usage         session.UsageTotals
	closed        bool
}

func (r *acpRootSession) Prompt(ctx context.Context, text string, updates acpagent.UpdateSink) (acp.StopReason, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return acp.StopReasonRefusal, errors.New("ACP root session is closed")
	}
	r.prompt++
	sink := newACPEventSink(r, r.prompt, updates)
	sink.rec.User(acp.SanitizeModelFacingText(text))
	text = acp.SanitizeModelFacingText(text)
	runErr := r.agent.RunPromptContentWithContext(ctx, text, nil, nil, r.prompt, sink)
	sink.rec.Flush()
	if transcript, changed := sanitizeACPTranscript(r.agent.Transcript()); changed {
		// Sanitizing a provider response changes the continuation fingerprint. Reset
		// remote response state rather than ever persisting or replaying terminal
		// controls through a later UI session.
		r.agent.SetTranscript(transcript)
	}
	if state := r.agent.ResponseState(); state != nil && sanitizeACPResponseState(state) == nil {
		r.agent.SetResponseState(nil)
	}
	if validationErr := llm.ValidateTranscript(r.agent.Transcript()); validationErr != nil {
		runErr = errors.Join(runErr, fmt.Errorf("validate transcript after prompt: %w", validationErr))
	}
	saveErr := r.save(nil)
	if recErr := sink.rec.Err(); recErr != nil {
		saveErr = errors.Join(saveErr, recErr)
	}
	reason := mapACPStopReason(sink.termination)
	if ctx.Err() != nil || errors.Is(runErr, context.Canceled) || errors.Is(runErr, context.DeadlineExceeded) {
		reason = acp.StopReasonCancelled
	}
	return reason, errors.Join(runErr, saveErr)
}

func (r *acpRootSession) Close(ctx context.Context) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return nil
	}
	r.closed = true
	var errs []error
	if err := r.agentSessions.CloseAll(ctx); err != nil {
		errs = append(errs, err)
	}
	r.jobs.ShutdownAndWait(time.Second)
	if r.otel != nil {
		r.otel.RecordSession(r.usage.CostUSD, llm.PromptInputTokens(r.usage.Usage)+r.usage.OutputTokens+r.usage.ReasoningTokens)
	}
	for i := len(r.cleanups) - 1; i >= 0; i-- {
		r.cleanups[i](ctx)
	}
	if err := r.save(nil); err != nil {
		errs = append(errs, err)
	}
	if r.lock != nil {
		errs = append(errs, r.lock.Close())
		r.lock = nil
	}
	return errors.Join(errs...)
}

func (r *acpRootSession) snapshot(current *agent.PromptUsage) session.Session {
	usage := r.usage
	if current != nil {
		addACPUsage(&usage, current.Usage, current.Compactions)
	}
	var latestPlan *plan.Plan
	if value, ok := r.plans.Latest(); ok {
		value.Title = acp.SanitizeModelFacingText(value.Title)
		value.Body = acp.SanitizeModelFacingText(value.Body)
		latestPlan = &value
	}
	todos := r.todos.Snapshot()
	for i := range todos {
		todos[i].Step = acp.SanitizeModelFacingText(todos[i].Step)
	}
	messages, _ := sanitizeACPTranscript(r.agent.Transcript())
	usageByModel := map[string]session.UsageTotals{r.cfg.Provider + "/" + r.cfg.Model: usage}
	return session.Session{Version: session.Version, CWD: r.cwd, Provider: r.cfg.Provider, Model: r.cfg.Model, Created: r.created, Updated: r.now(), Build: r.build, Runtime: r.runtime, System: acp.SanitizeModelFacingText(r.system), Agent: r.agentName, ProxySessionID: r.agent.ProxySessionID(), CacheAffinityID: r.agent.CacheAffinityID(), Prompt: r.prompt, Messages: messages, ResponseState: sanitizeACPResponseState(r.agent.ResponseState()), Plan: latestPlan, Todos: todos, Usage: usage, UsageByModel: usageByModel}
}

func (r *acpRootSession) save(current *agent.PromptUsage) error {
	return r.snapshot(current).SaveConsolidated(r.path)
}

// sanitizeACPTranscript cleans text, not validated base64 image payloads. Applying
// text limits to ImageData would corrupt otherwise valid tool-result images.
func sanitizeACPTranscript(messages []llm.Message) ([]llm.Message, bool) {
	out := append([]llm.Message(nil), messages...)
	changed := false
	for i := range out {
		if sanitizeACPString(&out[i].Phase) {
			changed = true
		}
		if len(messages[i].ParallelToolBatches) > 0 {
			out[i].ParallelToolBatches = make([]llm.ParallelToolBatch, len(messages[i].ParallelToolBatches))
			for batchIndex, batch := range messages[i].ParallelToolBatches {
				ids := make([]string, len(batch.ToolUseIDs))
				for idIndex, id := range batch.ToolUseIDs {
					ids[idIndex] = sanitizeACPReplayIdentifier(id)
					changed = changed || ids[idIndex] != id
				}
				out[i].ParallelToolBatches[batchIndex].ToolUseIDs = ids
			}
		}
		if messages[i].Compaction != nil {
			compaction := *messages[i].Compaction
			for _, field := range []*string{&compaction.Summary, &compaction.SummarySource, &compaction.FallbackReason, &compaction.Focus} {
				changed = sanitizeACPString(field) || changed
			}
			compaction.ReadFiles = sanitizeACPStrings(compaction.ReadFiles)
			compaction.ModifiedFiles = sanitizeACPStrings(compaction.ModifiedFiles)
			for index, value := range messages[i].Compaction.ReadFiles {
				changed = changed || compaction.ReadFiles[index] != value
			}
			for index, value := range messages[i].Compaction.ModifiedFiles {
				changed = changed || compaction.ModifiedFiles[index] != value
			}
			out[i].Compaction = &compaction
		}
		content := make([]llm.ContentBlock, 0, len(messages[i].Content))
		for _, original := range messages[i].Content {
			block := original
			for _, field := range []*string{
				&block.Text, &block.ResultText, &block.ToolName, &block.ToolNamespace,
				&block.ReasoningReplayDomain, &block.ImageMediaType,
				&block.ImageDetail, &block.ImageName,
			} {
				if sanitizeACPString(field) {
					changed = true
				}
			}
			cleanID := sanitizeACPReplayIdentifier(block.ToolUseID)
			if cleanID != block.ToolUseID {
				block.ToolUseID = cleanID
				changed = true
			}
			cleanID = sanitizeACPReplayIdentifier(block.ResultForID)
			if cleanID != block.ResultForID {
				block.ResultForID = cleanID
				changed = true
			}
			if input, inputChanged := sanitizeACPJSON(block.ToolInput); inputChanged {
				block.ToolInput = input
				changed = true
			}
			if len(block.ResultContent) > 0 {
				block.ResultContent = append([]llm.ContentBlock(nil), block.ResultContent...)
				for nestedIndex := range block.ResultContent {
					if sanitizeACPLooseContentBlock(&block.ResultContent[nestedIndex]) {
						changed = true
					}
				}
			}

			drop := false
			switch block.Kind {
			case llm.BlockThinking:
				thinking := acp.SanitizeModelFacingText(block.Thinking)
				signature := acp.SanitizeModelFacingText(block.ThinkingSignature)
				if block.ThinkingSignature != "" && (thinking != block.Thinking || signature != block.ThinkingSignature) {
					drop = true
				} else if thinking != block.Thinking {
					block.Thinking = thinking
					changed = true
				}
			case llm.BlockInteractionThought:
				summary := acp.SanitizeModelFacingText(block.InteractionThoughtSummary)
				signature := acp.SanitizeModelFacingText(block.InteractionThoughtSignature)
				if summary != block.InteractionThoughtSummary || signature != block.InteractionThoughtSignature {
					drop = true
				}
			case llm.BlockRedactedThinking:
				drop = acp.SanitizeModelFacingText(block.RedactedData) != block.RedactedData
			case llm.BlockReasoning:
				drop = acp.SanitizeModelFacingText(block.ReasoningID) != block.ReasoningID ||
					acp.SanitizeModelFacingText(block.ReasoningEncrypted) != block.ReasoningEncrypted
			case llm.BlockInteractionStep:
				_, drop = sanitizeACPJSON(block.InteractionStep)
			case llm.BlockResponsesToolSearch:
				_, drop = sanitizeACPJSON(block.ResponsesToolSearch)
			case llm.BlockAnthropicToolSearch:
				_, drop = sanitizeACPJSON(block.AnthropicToolSearch)
			case llm.BlockProviderCompaction:
				for _, item := range block.ProviderCompaction {
					if _, unsafe := sanitizeACPJSON(item); unsafe {
						drop = true
						break
					}
				}
			}
			if drop {
				changed = true
				continue
			}
			content = append(content, block)
		}
		if len(content) == 0 && len(messages[i].Content) > 0 {
			content = append(content, llm.ContentBlock{Kind: llm.BlockText, Text: "[unsafe provider-owned content omitted]"})
			changed = true
		}
		out[i].Content = content
	}
	return out, changed
}

func sanitizeACPResponseState(state *llm.ResponseState) *llm.ResponseState {
	if state == nil || acp.SanitizeModelFacingText(state.PreviousResponseID) != state.PreviousResponseID {
		return nil
	}
	copyState := *state
	return &copyState
}

func sanitizeACPRequest(request llm.Request) llm.Request {
	sanitizeACPString(&request.System)
	request.Messages, _ = sanitizeACPTranscript(request.Messages)
	for i := range request.Tools {
		sanitizeACPString(&request.Tools[i].Name)
		sanitizeACPString(&request.Tools[i].Description)
		if parameters, changed := sanitizeACPJSON(request.Tools[i].Parameters); changed {
			request.Tools[i].Parameters = parameters
		}
	}
	for i := range request.DeferredToolGroups {
		sanitizeACPString(&request.DeferredToolGroups[i].Name)
		sanitizeACPString(&request.DeferredToolGroups[i].Description)
		for j := range request.DeferredToolGroups[i].Tools {
			sanitizeACPString(&request.DeferredToolGroups[i].Tools[j].Name)
			sanitizeACPString(&request.DeferredToolGroups[i].Tools[j].Description)
			if parameters, changed := sanitizeACPJSON(request.DeferredToolGroups[i].Tools[j].Parameters); changed {
				request.DeferredToolGroups[i].Tools[j].Parameters = parameters
			}
		}
	}
	for i := range request.ServerTools {
		sanitizeACPString(&request.ServerTools[i].Name)
		sanitizeACPString(&request.ServerTools[i].Kind)
		if parameters, changed := sanitizeACPJSON(request.ServerTools[i].Parameters); changed {
			request.ServerTools[i].Parameters = parameters
		}
	}
	request.RequestContext = sanitizeACPStrings(request.RequestContext)
	request.StopSeqs = sanitizeACPStrings(request.StopSeqs)
	if acp.SanitizeModelFacingText(request.PreviousResponseID) != request.PreviousResponseID {
		request.PreviousResponseID = ""
	}
	return request
}

func sanitizeACPStrings(values []string) []string {
	if values == nil {
		return nil
	}
	out := make([]string, len(values))
	for i, value := range values {
		out[i] = acp.SanitizeModelFacingText(value)
	}
	return out
}

func sanitizeACPLooseContentBlock(block *llm.ContentBlock) bool {
	changed := false
	for _, field := range []*string{
		&block.Text, &block.ImageMediaType, &block.ImageDetail, &block.ImageName,
		&block.ToolName, &block.ToolNamespace, &block.ResultText, &block.Thinking,
		&block.ThinkingSignature, &block.RedactedData, &block.ReasoningID, &block.ReasoningEncrypted,
		&block.InteractionThoughtSummary, &block.InteractionThoughtSignature, &block.ReasoningReplayDomain,
	} {
		changed = sanitizeACPString(field) || changed
	}
	cleanID := sanitizeACPReplayIdentifier(block.ToolUseID)
	changed = changed || cleanID != block.ToolUseID
	block.ToolUseID = cleanID
	cleanID = sanitizeACPReplayIdentifier(block.ResultForID)
	changed = changed || cleanID != block.ResultForID
	block.ResultForID = cleanID
	for _, raw := range []*json.RawMessage{&block.ToolInput, &block.InteractionStep, &block.ResponsesToolSearch, &block.AnthropicToolSearch} {
		if clean, cleanChanged := sanitizeACPJSON(*raw); cleanChanged {
			*raw = clean
			changed = true
		}
	}
	return changed
}

func sanitizeACPReplayIdentifier(value string) string {
	clean := acp.SanitizeModelFacingText(value)
	if clean == value {
		return value
	}
	clean = strings.TrimSpace(clean)
	if clean == "" {
		clean = "sanitized"
	}
	sum := sha256.Sum256([]byte(value))
	return fmt.Sprintf("%s_safe_%x", clean, sum[:4])
}

func sanitizeACPIdentifier(value string) string {
	clean := acp.SanitizeModelFacingText(value)
	if clean == value && len(value) <= acp.MaxIdentifierBytes {
		return value
	}
	clean = strings.TrimSpace(clean)
	limit := acp.MaxIdentifierBytes - len("_safe_") - 8
	if len(clean) > limit {
		cut := limit
		for cut > 0 && !utf8.RuneStart(clean[cut]) {
			cut--
		}
		clean = clean[:cut]
	}
	sum := sha256.Sum256([]byte(value))
	if clean == "" {
		clean = "sanitized"
	}
	return fmt.Sprintf("%s_safe_%x", clean, sum[:4])
}

func sanitizeACPString(value *string) bool {
	clean := acp.SanitizeModelFacingText(*value)
	if clean == *value {
		return false
	}
	*value = clean
	return true
}

func sanitizeACPJSON(raw json.RawMessage) (json.RawMessage, bool) {
	if len(raw) == 0 {
		return raw, false
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		return raw, false
	}
	value, changed := sanitizeACPJSONValue(value)
	if !changed {
		return raw, false
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		return raw, false
	}
	return encoded, true
}

func sanitizeACPJSONValue(value any) (any, bool) {
	switch value := value.(type) {
	case string:
		clean := acp.SanitizeModelFacingText(value)
		return clean, clean != value
	case []any:
		changed := false
		for i := range value {
			var itemChanged bool
			value[i], itemChanged = sanitizeACPJSONValue(value[i])
			changed = changed || itemChanged
		}
		return value, changed
	case map[string]any:
		changed := false
		keys := make([]string, 0, len(value))
		for key := range value {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		cleaned := make(map[string]any, len(value))
		for _, key := range keys {
			cleanKey := acp.SanitizeModelFacingText(key)
			if cleanKey == "" {
				cleanKey = "sanitized_key"
			}
			base := cleanKey
			for suffix := 2; ; suffix++ {
				if _, exists := cleaned[cleanKey]; !exists {
					break
				}
				cleanKey = fmt.Sprintf("%s_%d", base, suffix)
			}
			clean, itemChanged := sanitizeACPJSONValue(value[key])
			cleaned[cleanKey] = clean
			changed = changed || itemChanged || cleanKey != key
		}
		return cleaned, changed
	default:
		return value, false
	}
}

func sanitizeACPToolCall(call llm.ToolCall) llm.ToolCall {
	call.ID = sanitizeACPIdentifier(call.ID)
	sanitizeACPString(&call.Name)
	sanitizeACPString(&call.Namespace)
	sanitizeACPString(&call.InvalidInputError)
	if input, changed := sanitizeACPJSON(call.Input); changed {
		call.Input = input
	}
	call.Stage = llm.CloneToolStage(call.Stage)
	if call.Stage != nil {
		if emitted, changed := sanitizeACPJSON(call.Stage.Emitted); changed {
			call.Stage.Emitted = emitted
		}
	}
	return call
}

func sanitizeACPToolResult(result llm.ToolResult) llm.ToolResult {
	result.ForID = sanitizeACPIdentifier(result.ForID)
	sanitizeACPString(&result.Text)
	sanitizeACPString(&result.OriginalText)
	result.BackgroundJobID = sanitizeACPIdentifier(result.BackgroundJobID)
	result.ErrorDetails = llm.CloneToolErrorDetails(result.ErrorDetails)
	if result.ErrorDetails != nil && result.ErrorDetails.LeaseConflict != nil {
		conflict := result.ErrorDetails.LeaseConflict
		conflict.BlockingJobID = sanitizeACPIdentifier(conflict.BlockingJobID)
		for _, field := range []*string{
			&conflict.BlockingAgent, &conflict.BlockingStatus, &conflict.ResourceKey,
			&conflict.RequestedAccess, &conflict.ActiveAccess, &conflict.Guidance,
		} {
			sanitizeACPString(field)
		}
	}
	if len(result.Content) > 0 {
		messages, _ := sanitizeACPTranscript([]llm.Message{{Content: result.Content}})
		result.Content = messages[0].Content
	}
	return result
}

func sanitizeACPModelRequestEvent(event llm.ModelRequestEvent) llm.ModelRequestEvent {
	switch event.State {
	case llm.ModelRequestAccepted, llm.ModelRequestUpstreamAttemptFailed, llm.ModelRequestRetryScheduled,
		llm.ModelRequestCompleted, llm.ModelRequestFailed, llm.ModelRequestCancelled:
	default:
		event.State = ""
	}
	switch event.Outcome {
	case "", llm.ModelRequestOutcomeRetrying, llm.ModelRequestOutcomeTerminal:
	default:
		event.Outcome = ""
	}
	event.Purpose = llm.NormalizeRequestPurpose(event.Purpose)
	switch event.Stage {
	case "", llm.APIErrorStageProxyDecode, llm.APIErrorStageProxyResolve, llm.APIErrorStageProxyPrepare,
		llm.APIErrorStageProviderRuntime, llm.APIErrorStageUpstreamConnect, llm.APIErrorStageUpstreamHTTP,
		llm.APIErrorStageUpstreamStream:
	default:
		event.Stage = ""
	}
	for _, field := range []*string{
		&event.ProxyInstanceID, &event.UpstreamRequestID, &event.TraceID, &event.SpanID,
		&event.TargetID, &event.Provider, &event.APIType, &event.Model, &event.Code,
		&event.Message,
	} {
		sanitizeACPString(field)
	}
	payload := string(event.ResponsePayload)
	if sanitizeACPString(&payload) {
		event.ResponsePayload = llm.DiagnosticPayload(payload)
	}
	return event
}

func addACPUsage(total *session.UsageTotals, usage llm.Usage, compactions int) {
	total.InputTokens += usage.InputTokens
	total.OutputTokens += usage.OutputTokens
	total.CacheReadTokens += usage.CacheReadTokens
	total.CacheWriteTokens += usage.CacheWriteTokens
	total.CacheWrite1hTokens += usage.CacheWrite1hTokens
	total.ReasoningTokens += usage.ReasoningTokens
	total.CostUSD += usage.CostUSD
	total.Compactions += compactions
}

func mapACPStopReason(reason agent.TerminationReason) acp.StopReason {
	switch reason {
	case agent.TerminationTokenLimit:
		return acp.StopReasonMaxTokens
	case agent.TerminationTurnLimit, agent.TerminationCostLimit, agent.TerminationRepeatGuard, agent.TerminationErrorGuard:
		return acp.StopReasonMaxTurnRequests
	case agent.TerminationCancelled:
		return acp.StopReasonCancelled
	case agent.TerminationError:
		return acp.StopReasonRefusal
	default:
		return acp.StopReasonEndTurn
	}
}

type acpEventSink struct {
	root        *acpRootSession
	wire        acpagent.UpdateSink
	rec         *sessionrec.Recorder
	prompt      int
	turn        int
	termination agent.TerminationReason
	pending     map[string]string
	hookContext []string
}

func newACPEventSink(root *acpRootSession, prompt int, wire acpagent.UpdateSink) *acpEventSink {
	rec := sessionrec.New(sessionrec.Config{
		Dir: root.path, Prompt: prompt, Agent: root.agentName, ModelTarget: root.registryModel,
		Provider: root.cfg.Provider, Model: root.cfg.Model, Clock: root.now,
		ReasoningSummaries: root.cfg.ReasoningSummary != "none", CWD: root.cwd,
		PriceTurnUsage:   func(u llm.Usage) (float64, bool) { return root.registry.Cost(root.registryModel, u) },
		PricePromptUsage: func(u llm.Usage) (float64, bool) { return root.registry.Cost(root.registryModel, u) },
		PromptUsageLine: func(u agent.PromptUsage, elapsed time.Duration, cost float64, known bool) string {
			return sessionrec.UsageLine(u, elapsed, cost, known, root.usage.InputTokens, root.usage.OutputTokens, root.usage.CostUSD, root.usage.Compactions)
		},
	})
	return &acpEventSink{root: root, wire: wire, rec: rec, prompt: prompt, pending: make(map[string]string)}
}

func (s *acpEventSink) TextDelta(text string) {
	text = acp.SanitizeModelFacingText(text)
	s.rec.TextDelta(text)
	s.wire.Text(text)
}
func (s *acpEventSink) ReasoningSummary(text string) {
	s.rec.ReasoningSummary(acp.SanitizeModelFacingText(text))
}
func (s *acpEventSink) TurnAttemptStart(turn, attempt int, estimate agent.ContextEstimate) {
	s.turn = turn
	s.root.todos.CommitModelRound(turn > 0)
	s.rec.TurnAttemptStart(turn, attempt, estimate)
}
func (s *acpEventSink) TurnAttemptComplete(usage agent.TurnAttemptUsage) {
	s.rec.TurnAttemptComplete(usage)
}
func (s *acpEventSink) TurnAttemptAbandoned(turn, attempt int) {
	s.rec.TurnAttemptAbandoned(turn, attempt)
}
func (*acpEventSink) ToolUseStart(llm.ToolCall) {}
func (*acpEventSink) ToolUseDelta(int, string)  {}
func (s *acpEventSink) ToolStart(call llm.ToolCall) {
	call = sanitizeACPToolCall(call)
	s.pending[call.ID] = call.Name
	s.rec.ToolStart(call)
	s.wire.ToolCall(acp.ToolCall{ToolCallID: acp.ToolCallID(call.ID), Title: call.Name, Kind: acpToolKind(call.Name), Status: acp.ToolCallPending, RawInput: append(json.RawMessage(nil), call.Input...)})
	s.wire.ToolStatus(acp.ToolCallID(call.ID), acp.ToolCallInProgress)
}
func (s *acpEventSink) ToolResult(result llm.ToolResult) {
	result = sanitizeACPToolResult(result)
	name := s.pending[result.ForID]
	delete(s.pending, result.ForID)
	s.rec.ToolResult(result)
	s.wire.ToolResult(acp.ToolCallID(result.ForID), result.Text, result.IsError)
	if !result.IsError && name == "update_todos" {
		items := s.root.todos.Snapshot()
		entries := make([]acp.PlanEntry, 0, len(items))
		for _, item := range items {
			status := acp.PlanEntryStatus(item.Status)
			entries = append(entries, acp.PlanEntry{Content: acp.SanitizeModelFacingText(item.Step), Priority: acp.PlanPriorityMedium, Status: status})
		}
		s.wire.Plan(entries)
	}
}
func (s *acpEventSink) Notice(text string) {
	text = acp.SanitizeModelFacingText(text)
	s.rec.Notice(text, s.turn)
	s.wire.Notice(text)
}
func (s *acpEventSink) TurnComplete(usage agent.TurnUsage) { s.rec.TurnComplete(usage) }
func (s *acpEventSink) PromptComplete(usage agent.PromptUsage) {
	if !usage.Usage.CostKnown {
		usage.Usage.CostUSD, usage.Usage.CostKnown = s.root.registry.Cost(s.root.registryModel, usage.Usage)
	}
	s.termination = usage.TerminationReason
	addACPUsage(&s.root.usage, usage.Usage, usage.Compactions)
	s.rec.PromptComplete(usage)
	used, size := usage.Context.Total, usage.Context.Window
	if used < 0 {
		used = 0
	}
	if size < used {
		size = used
	}
	update := acp.UsageUpdate{Used: uint64(used), Size: uint64(size)}
	if usage.Usage.CostKnown {
		update.Cost = &acp.Cost{Amount: usage.Usage.CostUSD, Currency: "USD"}
	}
	s.wire.Usage(update)
}
func (s *acpEventSink) AssistantPhase(phase string) {
	s.rec.AssistantPhase(acp.SanitizeModelFacingText(phase))
}
func (s *acpEventSink) ModelRequestEvent(event llm.ModelRequestEvent) {
	s.rec.ModelRequestEvent(sanitizeACPModelRequestEvent(event))
}
func (s *acpEventSink) MaintenanceComplete(usage agent.MaintenanceUsage) {
	s.rec.MaintenanceComplete(usage)
}
func (s *acpEventSink) ClosureStarted(event agent.ClosureEvent)  { s.rec.ClosureStarted(event) }
func (s *acpEventSink) TurnProgress(progress agent.TurnProgress) { s.rec.TurnProgress(progress) }
func (s *acpEventSink) RetentionApplied(event agent.RetentionEvent) {
	s.rec.Append(session.Event{Type: session.EventRetention, Prompt: s.prompt, Turn: s.turn, Retention: sessionrec.RetentionSnapshot(event)})
}
func (s *acpEventSink) ToolDiff(call llm.ToolCall, path, text string) {
	s.rec.ToolDiff(sanitizeACPToolCall(call), acp.SanitizeModelFacingText(path), acp.SanitizeModelFacingText(text))
}
func (s *acpEventSink) ToolMutation(call llm.ToolCall, paths []string) {
	cleanPaths := make([]string, len(paths))
	for i, path := range paths {
		cleanPaths[i] = acp.SanitizeModelFacingText(path)
	}
	s.rec.ToolMutation(sanitizeACPToolCall(call), cleanPaths)
}
func (s *acpEventSink) ArchiveToolResult(result llm.ToolResult) (agent.ToolResultArchive, error) {
	result = sanitizeACPToolResult(result)
	ref, err := session.SaveToolResultArtifact(s.root.path, s.prompt, s.turn, result)
	return agent.ToolResultArchive{DisplayPath: ref, ModelPath: filepath.Join(s.root.path, ref)}, err
}
func (s *acpEventSink) PromptCheckpoint(checkpoint agent.PromptCheckpoint) {
	state := s.root.snapshot(&checkpoint.Usage)
	started := time.Now()
	var err error
	if checkpoint.Kind == agent.PromptCheckpointClosedTurn {
		err = session.SaveClosedTurnCheckpoint(s.root.path, state, s.prompt, checkpoint.Turn)
	} else {
		err = session.SaveActiveTurnCheckpoint(s.root.path, state, string(checkpoint.Kind), s.prompt, checkpoint.Turn)
	}
	if err != nil {
		s.Notice("[checkpoint failed: " + err.Error() + "]")
		return
	}
	s.rec.Append(session.Event{
		Type: session.EventCheckpoint, Prompt: s.prompt, Turn: checkpoint.Turn,
		Purpose: string(checkpoint.Kind), DurationMS: time.Since(started).Milliseconds(),
		MessageCount: len(state.Messages), ClosureTrigger: string(checkpoint.Usage.ClosureTrigger),
		ClosureTurn: checkpoint.Usage.ClosureTurn, TurnBudgetExhausted: checkpoint.Usage.TurnBudgetExhausted,
		WorkflowStatus: sessionrec.WorkflowStatusSnapshot(checkpoint.Usage.WorkflowStatus),
	})
}
func (s *acpEventSink) AddHookContext(values []string) {
	for _, value := range values {
		s.hookContext = append(s.hookContext, acp.SanitizeModelFacingText(value))
	}
}
func (s *acpEventSink) RequestContext() []string {
	out := append([]string(nil), s.hookContext...)
	s.hookContext = nil
	if text := s.root.todos.PendingRequestContext(); text != "" {
		out = append(out, text)
	}
	out = append(out, s.root.jobs.DrainCompletedContext(s)...)
	return out
}
func (s *acpEventSink) PeekRequestContext() []string {
	out := append([]string(nil), s.hookContext...)
	if text := s.root.todos.PendingRequestContext(); text != "" {
		out = append(out, text)
	}
	return append(out, s.root.jobs.PeekCompletedContext()...)
}
func (s *acpEventSink) TranscriptRewritten()    { s.root.todos.RequireRequestContext() }
func (s *acpEventSink) PendingPromptWork() bool { return s.root.jobs.PendingPromptWork() }
func (s *acpEventSink) WaitForPromptWork(ctx context.Context) (llm.Usage, error) {
	return s.root.jobs.WaitForPromptWork(ctx)
}
func (s *acpEventSink) DrainPromptWorkUsage() llm.Usage { return s.root.jobs.DrainPromptWorkUsage() }
func (s *acpEventSink) HookDiagnostic(diagnostic hooks.Diagnostic) {
	s.rec.HookDiagnostic(diagnostic)
}
func (s *acpEventSink) EvaluatorResult(result hooks.EvaluatorResult) {
	s.rec.EvaluatorResult(result)
}
func (s *acpEventSink) TryStagnationNudge(threshold int) bool {
	return s.rec.TryStagnationNudge(threshold)
}

func acpToolKind(name string) acp.ToolKind {
	switch name {
	case "read", "view_image":
		return acp.ToolKindRead
	case "edit", "write":
		return acp.ToolKindEdit
	case "shell":
		return acp.ToolKindExecute
	case "web_fetch":
		return acp.ToolKindFetch
	case "record_plan", "update_todos":
		return acp.ToolKindThink
	default:
		if strings.Contains(name, "search") || strings.HasPrefix(name, "lsp_") {
			return acp.ToolKindSearch
		}
		return acp.ToolKindOther
	}
}
