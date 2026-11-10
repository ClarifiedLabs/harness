package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"slices"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"harness/internal/agentdef"
	"harness/internal/buildinfo"
	"harness/internal/config"
	"harness/internal/logging"
	"harness/internal/mcp"
	"harness/internal/mcpproxy"
	"harness/internal/mcptools"
	"harness/internal/tools"
	"harness/internal/tracing"
)

// MCP startup timing budget.
var mcpRegisterTimeout = 5 * time.Second

// optionalToolSchemaBudget keeps ordinary MCP catalogs native while moving
// unusually large optional surfaces behind tool_catalog. The catalog is a
// prompt optimization, not a dispatch or permission boundary.
const optionalToolSchemaBudget = 32 << 10

// resolveMCPServices validates already-expanded configuration without expanding
// literal dollar expressions a second time. Child processes run in the root workspace.
func resolveMCPServices(cfg config.MCPConfig, workDir string) ([]mcpproxy.ResolvedServer, error) {
	names := make([]string, 0, len(cfg.Servers))
	for name := range cfg.Servers {
		names = append(names, name)
	}
	sort.Strings(names)
	servers := make([]mcpproxy.ResolvedServer, 0, len(names))
	for _, name := range names {
		if !cfg.Servers[name].IsEnabled() {
			continue
		}
		server, err := mcpproxy.ValidateServer(name, cfg.Servers[name], cfg.ServerConfigDirs[name])
		if err != nil {
			return nil, fmt.Errorf("mcp: %w", err)
		}
		server.WorkDir = workDir
		servers = append(servers, server)
	}
	return servers, nil
}

// setupMCPServices owns one in-process aggregator and its downstream services.
// Readiness is bounded, not conditional on a nonempty tool list. A slow or failed
// service can recover later and notify the same connection at a prompt boundary.
func setupMCPServices(ctx context.Context, servers []mcpproxy.ResolvedServer, catalog *tools.Registry, logger *slog.Logger, tracer *tracing.Tracer) (conn *mcptools.Conn, summary mcptools.Summary, cleanup func(), ok bool) {
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	runCtx, cancel := context.WithCancel(ctx)
	clientSide, serverSide := net.Pipe()
	daemon := mcpproxy.NewDaemon(mcpproxy.Config{Servers: servers}, logger)
	done := make(chan struct{})
	go func() {
		defer close(done)
		if err := daemon.RunStdio(runCtx, serverSide); err != nil && runCtx.Err() == nil {
			logger.Warn("mcp: aggregator stopped", logging.Category("mcp"), "err", err)
		}
	}()
	var dialed atomic.Bool
	c := mcptools.NewConn(mcptools.Options{
		Info: mcp.Implementation{Name: "harness", Version: buildinfo.Version}, Logger: logger, Tracer: tracer,
		Dial: func(ctx context.Context) (io.ReadWriteCloser, error) {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			if !dialed.CompareAndSwap(false, true) {
				return nil, mcptools.ErrClosed
			}
			return clientSide, nil
		},
	})
	var once sync.Once
	cleanup = func() {
		once.Do(func() {
			cancel()
			_ = clientSide.Close()
			_ = serverSide.Close()
			_ = c.Close()
			<-done
		})
	}
	readyCtx, readyCancel := context.WithTimeout(ctx, mcpRegisterTimeout)
	err := daemon.WaitReady(readyCtx)
	readyCancel()
	if ctx.Err() != nil {
		cleanup()
		return nil, mcptools.Summary{}, cleanup, false
	}
	if err != nil {
		logger.Warn("mcp: initial discovery timed out; services continue in background", logging.Category("mcp"))
	}
	regCtx, regCancel := context.WithTimeout(ctx, mcpRegisterTimeout)
	defer regCancel()
	sum, err := mcptools.RegisterWithOptions(regCtx, catalog, c, mcptools.RegisterOptions{TrustReadOnlyHint: true})
	if err != nil {
		if !errors.Is(err, context.Canceled) {
			logger.Warn("mcp: cannot register services", logging.Category("mcp"), "err", err)
		}
		cleanup()
		return nil, mcptools.Summary{}, cleanup, false
	}
	logger.Info(mcpConnectedLine(sum), logging.Category("mcp"))
	for _, name := range sum.Skipped {
		logger.Warn(fmt.Sprintf("mcp: skipping tool %q: invalid qualified name", name), logging.Category("mcp"))
	}
	return c, sum, cleanup, true
}

// liveAgentTools omits temporarily unavailable MCP names without hiding built-in typos.
func liveAgentTools(catalog *tools.Registry, names []string) []string {
	return slices.DeleteFunc(slices.Clone(names), func(name string) bool {
		if strings.HasPrefix(name, "mcp__") {
			_, ok := catalog.Lookup(name)
			return !ok
		}
		return false
	})
}

func subsetForAgentTools(catalog *tools.Registry, names []string) (*tools.Registry, error) {
	names = slices.DeleteFunc(liveAgentTools(catalog, names), func(name string) bool { return name == tools.ToolCatalogName })
	sub, err := catalog.Subset(names)
	if err != nil {
		return nil, err
	}
	enableLazyOptionalToolSpecs(sub)
	return sub, nil
}

// augmentAgentsWithMCP appends discovered MCP tool names according to each
// agent's mcp_tools mode: disabled gets none, read_only gets only trusted
// read-only MCP tools, and all gets the full discovered set. It is a no-op when
// there are no MCP names.
func augmentAgentsWithMCP(agents map[string]agentdef.Definition, allNames, readOnlyNames []string) {
	for name, a := range agents {
		extra := mcpNamesForMode(a.MCPTools, allNames, readOnlyNames)
		next := appendMCPNames(a.AllowedTools, extra)
		if slices.Equal(next, a.AllowedTools) {
			continue
		}
		a.AllowedTools = next
		agents[name] = a
	}
}

func enableLazyOptionalToolSpecs(registry *tools.Registry) {
	var optional []string
	for _, name := range registry.Names() {
		if strings.HasPrefix(name, "mcp__") {
			optional = append(optional, name)
		}
	}
	tools.EnableLazyToolSpecs(registry, optional, optionalToolSchemaBudget)
}

func appendMCPNames(base, extra []string) []string {
	out := slices.Clone(base)
	for _, name := range extra {
		if !slices.Contains(out, name) {
			out = append(out, name)
		}
	}
	return out
}

// mcpLimits bounds the auto-exposed MCP tool surface. The zero value is
// inactive (no cap, no disabled servers).
type mcpLimits struct {
	maxTools int
	disabled map[string]bool
}

// mcpLimitsFromConfig derives the auto-exposure limits from the resolved MCP
// config.
func mcpLimitsFromConfig(cfg config.MCPConfig) mcpLimits {
	var disabled map[string]bool
	if len(cfg.DisabledServers) > 0 {
		disabled = make(map[string]bool, len(cfg.DisabledServers))
		for _, s := range cfg.DisabledServers {
			if s = strings.TrimSpace(s); s != "" {
				disabled[s] = true
			}
		}
	}
	return mcpLimits{maxTools: cfg.MaxTools, disabled: disabled}
}

func (l mcpLimits) active() bool { return l.maxTools > 0 || len(l.disabled) > 0 }

// capRemoteMCPNames applies the configured per-server and max_tools restrictions
// to the discovered MCP tool surface before it is auto-exposed to agents.
// It returns the kept names (in discovery order) and the read-only subset
// filtered to the kept set, logging one warning when it drops anything. It leaves
// the inputs untouched (the catalog still holds every discovered tool, so an
// explicit allowed_tools whitelist can still name a tool the cap excludes from
// auto-exposure). All configured MCP services share these limits.
func capRemoteMCPNames(names, readOnly []string, limits mcpLimits, logger *slog.Logger) ([]string, []string) {
	if !limits.active() || len(names) == 0 {
		return names, readOnly
	}
	kept := names
	droppedServers := 0
	if len(limits.disabled) > 0 {
		kept = make([]string, 0, len(names))
		for _, n := range names {
			if limits.disabled[mcptools.ServerLabel(n)] {
				droppedServers++
				continue
			}
			kept = append(kept, n)
		}
	}
	droppedCap := 0
	if limits.maxTools > 0 && len(kept) > limits.maxTools {
		droppedCap = len(kept) - limits.maxTools
		kept = kept[:limits.maxTools]
	}
	if droppedServers == 0 && droppedCap == 0 {
		return names, readOnly
	}
	keptSet := make(map[string]bool, len(kept))
	for _, n := range kept {
		keptSet[n] = true
	}
	ro := make([]string, 0, len(readOnly))
	for _, n := range readOnly {
		if keptSet[n] {
			ro = append(ro, n)
		}
	}
	if logger != nil {
		logger.Warn(fmt.Sprintf("mcp: restricting tool surface: exposing %d of %d discovered tools (%d over max_tools, %d from disabled servers)",
			len(kept), len(names), droppedCap, droppedServers), logging.Category("mcp"))
	}
	return kept, ro
}

func mcpNamesForMode(mode agentdef.MCPToolsMode, allNames, readOnlyNames []string) []string {
	switch mode {
	case agentdef.MCPToolsAll:
		return allNames
	case agentdef.MCPToolsReadOnly:
		return readOnlyNames
	default:
		return nil
	}
}

// mcpAgentBase preserves configured tools independently of discovery. Never mutate
// Allowed: an explicitly named MCP tool can disappear and return in any mode.
type mcpAgentBase struct {
	Allowed []string
	Mode    agentdef.MCPToolsMode
}
type mcpAgentBases map[string]mcpAgentBase

func snapshotMCPAgentBases(agents map[string]agentdef.Definition) mcpAgentBases {
	bases := make(mcpAgentBases, len(agents))
	for name, a := range agents {
		bases[name] = mcpAgentBase{Allowed: slices.Clone(a.AllowedTools), Mode: a.MCPTools}
	}
	return bases
}

func reconcileMCPTools(agents map[string]agentdef.Definition, bases mcpAgentBases, catalog *tools.Registry, names, readOnly []string) {
	for name, base := range bases {
		a := agents[name]
		a.AllowedTools = appendMCPNames(liveAgentTools(catalog, base.Allowed), mcpNamesForMode(base.Mode, names, readOnly))
		agents[name] = a
	}
}

// mcpRefresher owns the aggregator's previous discovery summary.
type mcpRefresher struct {
	conn            *mcptools.Conn
	catalog         *tools.Registry
	agents          map[string]agentdef.Definition
	bases           mcpAgentBases
	prev            mcptools.Summary
	static          func() mcptools.Summary
	logger          *slog.Logger
	limits          mcpLimits
	registerOptions mcptools.RegisterOptions
}

func (r *mcpRefresher) apply(sum mcptools.Summary, agentName string) (*tools.Registry, string) {
	prevNames := r.prev.Names
	reg, notice := applyMCPRegistration(r.catalog, r.agents, r.bases, agentName, &prevNames, sum, r.static(), r.logger, r.limits)
	r.prev = sum
	r.prev.Names = prevNames
	return reg, notice
}

func (r *mcpRefresher) refresh(parent context.Context, agentName string) (*tools.Registry, string) {
	if r.conn == nil || !r.conn.Dirty() {
		return nil, ""
	}

	if _, ok := r.agents[agentName]; !ok {
		return nil, ""
	}

	// Worst case, a proxy that hangs mid-re-list stalls this prompt for up to
	// mcpRegisterTimeout (~5s) before the warn-and-keep path fires, since the
	// re-list runs synchronously at the prompt boundary. Accepted: it only
	// happens on a misbehaving proxy after an explicit list_changed, the
	// bound is finite, and the alternative (background re-list racing the
	// prompt's Specs()/Dispatch reads) is the unsafe mid-prompt swap we avoid.
	ctx, cancel := context.WithTimeout(parent, mcpRegisterTimeout)
	defer cancel()
	// Consume before listing so notifications after the snapshot remain pending.
	r.conn.ClearDirty()
	sum, err := mcptools.RegisterWithOptions(ctx, r.catalog, r.conn, r.registerOptions)
	if err != nil {
		r.conn.MarkDirty()
		if errors.Is(err, context.Canceled) {
			return nil, ""
		}
		r.logger.Warn(fmt.Sprintf("mcp: tool list refresh failed: %v; keeping current tools", err), logging.Category("mcp"))
		return nil, ""
	}
	return r.apply(sum, agentName)
}

// newMCPRefresherDynamic refreshes the root-owned aggregator at prompt boundaries.
func newMCPRefresherDynamic(conn *mcptools.Conn, catalog *tools.Registry, agents map[string]agentdef.Definition, bases mcpAgentBases, prev mcptools.Summary, static func() mcptools.Summary, logger *slog.Logger, limits ...mcpLimits) func(context.Context, string) (*tools.Registry, string) {
	var lim mcpLimits
	if len(limits) > 0 {
		lim = limits[0]
	}
	r := &mcpRefresher{
		conn: conn, catalog: catalog, agents: agents, bases: bases, prev: prev,
		static: static, logger: logger, limits: lim,
		registerOptions: mcptools.RegisterOptions{TrustReadOnlyHint: true},
	}
	return r.refresh
}

func applyMCPRegistration(catalog *tools.Registry, agents map[string]agentdef.Definition, bases mcpAgentBases, agentName string, prevNames *[]string, sum, static mcptools.Summary, logger *slog.Logger, limits mcpLimits) (*tools.Registry, string) {
	// Drop tools that were registered before but are gone now. Register replaces
	// survivors in place; only departures need explicit removal.
	removed := removedNames(*prevNames, sum.Names)
	for _, name := range removed {
		catalog.Remove(name)
	}
	*prevNames = slices.Clone(sum.Names)

	// Cap only the auto-exposure name lists, not the catalog: the full discovered
	// set above stays registered so explicit whitelists keep working, while agents
	// in read_only/all mode see at most the configured remote surface.
	cappedNames, cappedReadOnly := capRemoteMCPNames(sum.Names, sum.ReadOnlyNames, limits, logger)
	allNames := append(slices.Clone(cappedNames), static.Names...)
	readOnlyNames := append(slices.Clone(cappedReadOnly), static.ReadOnlyNames...)
	// Capture the current agent's pre-refresh allowed list (before the bases loop may
	// rewrite it) to detect whether the refresh changed its effective tools.
	// agentKnown gates only the current-agent subset below; the catalog updates and
	// the bases re-derivation always run.
	current, agentKnown := agents[agentName]
	oldAllowed := slices.Clone(current.AllowedTools)

	// Include whitelist-only agents as well as automatic exposure modes.
	reconcileMCPTools(agents, bases, catalog, allNames, readOnlyNames)
	if !agentKnown {
		return nil, ""
	}
	allowed := agents[agentName].AllowedTools
	if slices.Equal(allowed, oldAllowed) && !anyNameInAgent(allowed, sum.Names) {
		return nil, ""
	}

	sel, err := catalog.Subset(allowed)
	if err != nil {
		logger.Warn(fmt.Sprintf("mcp: tool list refresh subset failed: %v; keeping current tools", err), logging.Category("mcp"))
		return nil, ""
	}
	enableLazyOptionalToolSpecs(sel)
	return sel, fmt.Sprintf("[mcp: tool list updated; %d tools]", len(allNames))
}

func anyNameInAgent(allowed, names []string) bool {
	for _, name := range names {
		if slices.Contains(allowed, name) {
			return true
		}
	}
	return false
}

// removedNames returns the entries of prev that are absent from next, preserving
// prev's order.
func removedNames(prev, next []string) []string {
	keep := make(map[string]bool, len(next))
	for _, n := range next {
		keep[n] = true
	}
	var gone []string
	for _, n := range prev {
		if !keep[n] {
			gone = append(gone, n)
		}
	}
	return gone
}

// mcpConnectedLine renders the one-line success notice, e.g.
// "mcp: connected (2 servers, 5 tools): a=3 b=2" with servers sorted by name.
func mcpConnectedLine(sum mcptools.Summary) string {
	names := make([]string, 0, len(sum.Servers))
	for name := range sum.Servers {
		names = append(names, name)
	}
	sort.Strings(names)
	parts := make([]string, len(names))
	for i, name := range names {
		parts[i] = fmt.Sprintf("%s=%d", name, sum.Servers[name])
	}
	line := fmt.Sprintf("mcp: connected (%d servers, %d tools)", len(names), sum.Total)
	if len(parts) > 0 {
		line += ": " + strings.Join(parts, " ")
	}
	return line
}
