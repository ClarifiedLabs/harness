package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"sort"
	"strings"
	"sync/atomic"
	"time"

	"harness/internal/agentdef"
	"harness/internal/buildinfo"
	"harness/internal/config"
	"harness/internal/logging"
	"harness/internal/mcp"
	"harness/internal/mcpproxy"
	"harness/internal/mcptools"
	"harness/internal/retry"
	"harness/internal/tools"
)

// MCP startup timing budget.
var mcpRegisterTimeout = 5 * time.Second

// setupMCP connects to the already-running HTTP proxy, registers the
// discovered tools into catalog, and returns the live conn plus its initial
// registration summary and a cleanup func. It NEVER fails harness startup: if
// the proxy is unreachable or registration fails it logs a single warning via
// logger and returns ok=false with a nil conn and a no-op cleanup, so the caller
// can defer cleanup unconditionally. The harness does not start the proxy;
// that is the operator's job (run harness-mcp-proxy separately).
//
// The returned conn (when ok) backs tool dispatch; cleanup closes that conn (the
// daemon itself keeps running and serving other sessions).
// dialMCPProxy resolves the configured proxy to a dialable http(s) URL and builds
// a lazily-connecting Conn for it (it does not connect). When the resolved proxy is
// not an http(s) URL it logs a single warning and returns ok=false with a nil conn,
// preserving the "MCP never fails harness startup" contract for both the sync
// (setupMCP) and async (setupMCPAsync) callers.
func dialMCPProxy(mcpCfg config.MCPConfig, logger *slog.Logger) (proxy string, conn *mcptools.Conn, ok bool) {
	proxy = resolveMCPProxy(mcpCfg.Proxy)
	if !isHTTPProxy(proxy) {
		logger.Warn(fmt.Sprintf("mcp: cannot connect to proxy at %s: proxy must be an http(s) URL; MCP tools unavailable", proxy), logging.Category("mcp"))
		return proxy, nil, false
	}
	conn = mcptools.NewConn(mcptools.Options{
		Endpoint: proxy,
		APIKey:   mcpCfg.APIKey,
		Headers:  mcpCfg.Headers,
		Info:     mcp.Implementation{Name: "harness", Version: buildinfo.Version},
		Logger:   logger,
	})
	return proxy, conn, true
}

func setupMCP(ctx context.Context, mcpCfg config.MCPConfig, catalog *tools.Registry, logger *slog.Logger) (conn *mcptools.Conn, summary mcptools.Summary, cleanup func(), ok bool) {
	noop := func() {}
	proxy, c, ok := dialMCPProxy(mcpCfg, logger)
	if !ok {
		return nil, mcptools.Summary{}, noop, false
	}
	regCtx, cancel := context.WithTimeout(ctx, mcpRegisterTimeout)
	defer cancel()
	sum, err := mcptools.RegisterWithOptions(regCtx, catalog, c, mcptools.RegisterOptions{TrustReadOnlyHint: true})
	if err != nil {
		if errors.Is(err, context.Canceled) {
			_ = c.Close()
			return nil, mcptools.Summary{}, noop, false
		}
		logger.Warn(fmt.Sprintf("mcp: cannot connect to proxy at %s: %v; MCP tools unavailable", proxy, err), logging.Category("mcp"))
		_ = c.Close()
		return nil, mcptools.Summary{}, noop, false
	}

	logger.Info(mcpConnectedLine(sum), logging.Category("mcp"))
	for _, name := range sum.Skipped {
		logger.Warn(fmt.Sprintf("mcp: skipping tool %q: name must match [a-zA-Z0-9_-]{1,64}", name), logging.Category("mcp"))
	}
	return c, sum, func() { _ = c.Close() }, true
}

type asyncMCPRegistration struct {
	conn    *mcptools.Conn
	results chan asyncMCPResult
	cancel  context.CancelFunc
	applied atomic.Bool
}

type asyncMCPResult struct {
	registry *tools.Registry
	summary  mcptools.Summary
}

// setupMCPAsync starts remote HTTP MCP discovery in the background for the
// interactive REPL. The goroutine never mutates the shared catalog: it registers
// discovered tools into a private registry, then the prompt-boundary refresh hook
// copies those tools into the real catalog on the main REPL goroutine. This keeps
// startup and prompt reads from blocking on an unreachable proxy while preserving
// the existing "MCP never fails harness startup" behavior.
func setupMCPAsync(mcpCfg config.MCPConfig, logger *slog.Logger) (conn *mcptools.Conn, pending *asyncMCPRegistration, cleanup func(), ok bool) {
	noop := func() {}
	proxy, c, ok := dialMCPProxy(mcpCfg, logger)
	if !ok {
		return nil, nil, noop, false
	}
	ctx, cancel := context.WithCancel(context.Background())
	pending = &asyncMCPRegistration{
		conn:    c,
		results: make(chan asyncMCPResult, 1),
		cancel:  cancel,
	}
	go pending.run(ctx, proxy, logger)
	cleanup = func() {
		cancel()
		_ = c.Close()
	}
	return c, pending, cleanup, true
}

func (a *asyncMCPRegistration) run(ctx context.Context, proxy string, logger *slog.Logger) {
	var warned bool
	for attempt := 0; ; attempt++ {
		regCtx, cancel := context.WithTimeout(ctx, mcpRegisterTimeout)
		reg := &tools.Registry{}
		sum, err := mcptools.RegisterWithOptions(regCtx, reg, a.conn, mcptools.RegisterOptions{TrustReadOnlyHint: true})
		cancel()
		if err == nil {
			select {
			case a.results <- asyncMCPResult{registry: reg, summary: sum}:
			case <-ctx.Done():
			}
			return
		}
		if errors.Is(err, context.Canceled) {
			return
		}
		if !warned {
			logger.Warn(fmt.Sprintf("mcp: cannot connect to proxy at %s: %v; retrying in background", proxy, err), logging.Category("mcp"))
			warned = true
		}
		select {
		case <-time.After(mcpRetryDelay(attempt)):
		case <-ctx.Done():
			return
		}
	}
}

// mcpBackgroundRetryFloor is the minimum delay between background reconnect
// attempts. retry.Next applies full jitter from zero, so without a floor a run of
// small draws could spin the reconnect loop against a fast-failing proxy; the floor
// bounds the cadence while the ceiling still grows with the attempt count.
const mcpBackgroundRetryFloor = time.Second

// mcpRetryDelay is the backoff before the given background reconnect attempt
// (0-based), never below mcpBackgroundRetryFloor.
func mcpRetryDelay(attempt int) time.Duration {
	return retry.Next(attempt, mcpBackgroundRetryFloor)
}

func (a *asyncMCPRegistration) take() (asyncMCPResult, bool) {
	if a == nil {
		return asyncMCPResult{}, false
	}
	select {
	case res := <-a.results:
		a.applied.Store(true)
		return res, true
	default:
		return asyncMCPResult{}, false
	}
}

func subsetForAgentTools(catalog *tools.Registry, names []string, pending *asyncMCPRegistration) (*tools.Registry, error) {
	if pending == nil || pending.applied.Load() {
		return catalog.Subset(names)
	}
	filtered := make([]string, 0, len(names))
	for _, name := range names {
		if strings.HasPrefix(name, "mcp__") {
			if _, ok := catalog.Lookup(name); !ok {
				continue
			}
		}
		filtered = append(filtered, name)
	}
	return catalog.Subset(filtered)
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

func appendMCPNames(base, extra []string) []string {
	out := slices.Clone(base)
	for _, name := range extra {
		if !slices.Contains(out, name) {
			out = append(out, name)
		}
	}
	return out
}

// mcpLimits bounds the auto-exposed remote MCP tool surface. The zero value is
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
// to the discovered remote MCP tool surface before it is auto-exposed to agents.
// It returns the kept names (in discovery order) and the read-only subset
// filtered to the kept set, logging one warning when it drops anything. It leaves
// the inputs untouched (the catalog still holds every discovered tool, so an
// explicit allowed_tools whitelist can still name a tool the cap excludes from
// auto-exposure). Only the remote HTTP-proxy surface is capped; local MCP and LSP
// tools pass through their own gating.
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

// resolveMCPProxy turns the configured proxy value into a dialable HTTP URL.
// An empty value resolves to the shared default proxy URL.
func resolveMCPProxy(proxy string) string {
	if proxy == "" {
		return mcpproxy.DefaultURL()
	}
	return proxy
}

// isHTTPProxy reports whether proxy is an http(s) URL (case-insensitive
// scheme).
func isHTTPProxy(proxy string) bool {
	lower := strings.ToLower(proxy)
	return strings.HasPrefix(lower, "http://") || strings.HasPrefix(lower, "https://")
}

// mcpAgentBase is the pre-augmentation allowed-tool list and MCP exposure mode
// for one agent that automatically exposes MCP tools. Built once at startup, it
// lets a refresh re-derive the full allowed list (base ∪ live MCP names) without
// re-classifying.
type mcpAgentBase struct {
	Allowed []string
	Mode    agentdef.MCPToolsMode
}

// mcpAgentBases is keyed by agents whose mcp_tools mode is not disabled. Agents
// absent from the map do not automatically expose MCP tools, though they may
// still explicitly whitelist mcp__ tool names.
type mcpAgentBases map[string]mcpAgentBase

// mcpExposingAgentBases returns the base allowed-tool list and exposure mode for
// every agent that automatically exposes MCP tools. It must be called on the
// agents BEFORE augmentAgentsWithMCP mutates them.
func mcpExposingAgentBases(agents map[string]agentdef.Definition) mcpAgentBases {
	bases := make(mcpAgentBases)
	for name, a := range agents {
		if a.MCPTools == agentdef.MCPToolsAll || a.MCPTools == agentdef.MCPToolsReadOnly {
			bases[name] = mcpAgentBase{Allowed: slices.Clone(a.AllowedTools), Mode: a.MCPTools}
		}
	}
	return bases
}

// newMCPRefresher returns the prompt-boundary refresh hook for ui.App. It owns
// the conn, the tool catalog, the resolved agents, and the previous
// registration's tool names so it can compute which tools vanished. On a
// list_changed it re-lists, removes departed tools from the catalog, re-derives
// every MCP-exposing agent's allowed list (so a later /agent switch stays valid),
// and returns the current agent's subset. It returns a nil registry ("no
// change") fast when nothing changed, and on a re-discovery error keeps the
// current tools. Not safe for concurrent use: the REPL calls it only at the
// idle prompt boundary, between turns.
func newMCPRefresher(conn *mcptools.Conn, catalog *tools.Registry, agents map[string]agentdef.Definition, bases mcpAgentBases, prev, static mcptools.Summary, logger *slog.Logger, pending *asyncMCPRegistration, limits ...mcpLimits) func(context.Context, string) (*tools.Registry, string) {
	prevNames := prev.Names
	var lim mcpLimits
	if len(limits) > 0 {
		lim = limits[0]
	}
	return func(parent context.Context, agentName string) (*tools.Registry, string) {
		if res, ok := pending.take(); ok {
			return applyMCPRegistration(catalog, res.registry, agents, bases, agentName, &prevNames, res.summary, static, logger, lim)
		}

		if !conn.Dirty() {
			return nil, ""
		}

		if _, ok := agents[agentName]; !ok {
			return nil, ""
		}

		// Worst case, a proxy that hangs mid-re-list stalls this prompt for up to
		// mcpRegisterTimeout (~5s) before the warn-and-keep path fires, since the
		// re-list runs synchronously at the prompt boundary. Accepted: it only
		// happens on a misbehaving proxy after an explicit list_changed, the
		// bound is finite, and the alternative (background re-list racing the
		// turn's Specs()/Dispatch reads) is the unsafe mid-turn swap we avoid.
		ctx, cancel := context.WithTimeout(parent, mcpRegisterTimeout)
		defer cancel()
		sum, err := mcptools.RegisterWithOptions(ctx, catalog, conn, mcptools.RegisterOptions{TrustReadOnlyHint: true})
		if err != nil {
			if errors.Is(err, context.Canceled) {
				return nil, ""
			}
			logger.Warn(fmt.Sprintf("mcp: tool list refresh failed: %v; keeping current tools", err), logging.Category("mcp"))
			return nil, ""
		}
		conn.ClearDirty()
		return applyMCPRegistration(catalog, nil, agents, bases, agentName, &prevNames, sum, static, logger, lim)
	}
}

func applyMCPRegistration(catalog *tools.Registry, discovered *tools.Registry, agents map[string]agentdef.Definition, bases mcpAgentBases, agentName string, prevNames *[]string, sum, static mcptools.Summary, logger *slog.Logger, limits mcpLimits) (*tools.Registry, string) {
	// Async first-apply: copy the privately-discovered tools into the shared catalog,
	// log the connection, warn on skipped names, and drop any explicit-whitelist
	// mcp__ name the proxy never exposed. This runs BEFORE the current-agent handling
	// below (and unconditionally on agentName) so a result already consumed by take()
	// is never stranded: the catalog and every MCP-exposing agent stay correct even
	// if this refresh fired for an agent not in the map.
	if discovered != nil {
		for _, name := range sum.Names {
			if tool, ok := discovered.Lookup(name); ok {
				catalog.Register(tool)
			}
		}
		logger.Info(mcpConnectedLine(sum), logging.Category("mcp"))
		for _, name := range sum.Skipped {
			logger.Warn(fmt.Sprintf("mcp: skipping tool %q: name must match [a-zA-Z0-9_-]{1,64}", name), logging.Category("mcp"))
		}
		pruneUndiscoveredMCPTools(agents, bases, catalog, logger)
	}

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

	// Re-derive every MCP-exposing agent's allowed list against the live tool set,
	// so /agent switches after a tool vanishes never reference a name the catalog no
	// longer has.
	for name, base := range bases {
		cleanBase := withoutNames(base.Allowed, removed)
		if !slices.Equal(cleanBase, base.Allowed) {
			base.Allowed = cleanBase
			bases[name] = base
		}
		a := agents[name]
		a.AllowedTools = appendMCPNames(cleanBase, mcpNamesForMode(base.Mode, allNames, readOnlyNames))
		agents[name] = a
	}

	if !agentKnown {
		return nil, ""
	}

	// An explicit-whitelist agent (one not in bases) exposes no MCP tools, so a
	// refresh leaves its subset unchanged — unless it explicitly whitelisted a tool
	// that was just removed. In the unchanged case, skip the swap and the "tool list
	// updated" notice, which would otherwise mislead (the agent's tools did not
	// change). The catalog/agent re-derivation above still ran so a later /agent
	// switch to an MCP-exposing agent is correct.
	allowed := agents[agentName].AllowedTools
	if _, exposesMCP := bases[agentName]; !exposesMCP {
		if !anyRemovedInAgent(allowed, removed) && !(discovered != nil && anyNameInAgent(allowed, sum.Names)) {
			return nil, ""
		}
		// The whitelist named a removed tool: drop the gone names so Subset does not
		// error on a name the catalog no longer has.
		allowed = withoutNames(allowed, removed)
		a := agents[agentName]
		a.AllowedTools = allowed
		agents[agentName] = a
	} else if slices.Equal(allowed, oldAllowed) && !anyNameInAgent(allowed, sum.Names) {
		// The refreshed remote MCP list did not affect this agent's effective tools
		// (for example, a read_only agent when only non-read-only remote tools
		// changed). Avoid a misleading swap/notice. If the agent does use a remote
		// MCP name, still swap so description/schema updates take effect.
		return nil, ""
	}

	sel, err := catalog.Subset(allowed)
	if err != nil {
		logger.Warn(fmt.Sprintf("mcp: tool list refresh subset failed: %v; keeping current tools", err), logging.Category("mcp"))
		return nil, ""
	}
	return sel, fmt.Sprintf("[mcp: tool list updated; %d tools]", len(allNames))
}

// pruneUndiscoveredMCPTools removes mcp__ tool names from explicit-whitelist agents
// (those not in bases) that the catalog does not have after discovery, logging a
// warning for each. setupMCPAsync filters such names from the running subset before
// discovery so an unreachable proxy never blocks startup; once discovery is applied,
// a still-missing mcp__ name a whitelist agent referenced is a typo or a tool the
// proxy never exposed, so dropping it both surfaces the mistake and keeps a later
// /agent switch from failing Subset on a name the catalog will never have. Bases
// agents derive their mcp__ names from the live catalog and are re-derived
// separately, so they are skipped here.
func pruneUndiscoveredMCPTools(agents map[string]agentdef.Definition, bases mcpAgentBases, catalog *tools.Registry, logger *slog.Logger) {
	for name, a := range agents {
		if _, exposesMCP := bases[name]; exposesMCP {
			continue
		}
		kept := make([]string, 0, len(a.AllowedTools))
		for _, tool := range a.AllowedTools {
			if strings.HasPrefix(tool, "mcp__") {
				if _, ok := catalog.Lookup(tool); !ok {
					logger.Warn(fmt.Sprintf("mcp: agent %q references unknown MCP tool %q; the proxy did not expose it", name, tool), logging.Category("mcp"))
					continue
				}
			}
			kept = append(kept, tool)
		}
		if len(kept) != len(a.AllowedTools) {
			a.AllowedTools = kept
			agents[name] = a
		}
	}
}

// anyRemovedInAgent reports whether allowed references any of the removed tool
// names, i.e. whether the refresh shrank an agent's effective tool set.
func anyRemovedInAgent(allowed, removed []string) bool {
	return anyNameInAgent(allowed, removed)
}

func anyNameInAgent(allowed, names []string) bool {
	for _, name := range names {
		if slices.Contains(allowed, name) {
			return true
		}
	}
	return false
}

// withoutNames returns allowed with every entry in drop removed, preserving
// order. It is used to drop just-removed MCP tool names from a whitelist agent's
// allowed list so Subset does not error on a name the catalog no longer has.
func withoutNames(allowed, drop []string) []string {
	out := make([]string, 0, len(allowed))
	for _, name := range allowed {
		if !slices.Contains(drop, name) {
			out = append(out, name)
		}
	}
	return out
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
