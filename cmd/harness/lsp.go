package main

import (
	"context"
	"fmt"
	"log/slog"
	"maps"
	"slices"
	"strings"
	"time"

	"harness/internal/agentdef"
	"harness/internal/config"
	"harness/internal/logging"
	"harness/internal/lspproxy"
	"harness/internal/lsptools"
	"harness/internal/mcptools"
	"harness/internal/tools"
	"harness/internal/ui"
)

type lspRuntime struct {
	mgr     *lspproxy.Manager
	summary mcptools.Summary
	enabled bool
	prewarm bool
	logger  *slog.Logger
}

// lspDescriptionSuffix is appended to the navigation tools' descriptions at
// Specs() time so the model learns that lsp_* tools are the better answer for
// symbol questions — without repeating that guidance across every lsp_* tool.
// It is only active while LSP tools are enabled and registered, and tool specs
// are cached at registry rebuild boundaries, so toggles take effect at the
// next rebuild.
const lspPreferSuffix = " Symbols: prefer lsp_*."

func lspDescriptionSuffix(r *lspRuntime) func(name, base string) string {
	return func(name, base string) string {
		if r == nil || !r.enabled || len(r.summary.Names) == 0 {
			return base
		}
		switch name {
		case "search", "grep", "rg", "glob":
			return base + lspPreferSuffix
		}
		return base
	}
}

// newLSPRuntime prepares the static tool surface without launching a language
// server. Keeping the manager available while disabled makes /lsp enable a
// session-local, immediate operation; servers remain lazy until a tool call.
func newLSPRuntime(ctx context.Context, lspCfg config.LSPConfig, catalog *tools.Registry, logger *slog.Logger) (*lspRuntime, error) {
	cfg, err := lspproxy.LoadConfigWithServers(convertLSPServers(lspCfg.Servers))
	if err != nil {
		return nil, err
	}
	for _, warning := range cfg.Warnings {
		logger.Warn(warning, logging.Category("lsp"))
	}
	mgr := lspproxy.NewManager(cfg, "", logger)
	summary, err := lsptools.Register(ctx, catalog, mgr, lspCfg.Tools...)
	if err != nil {
		mgr.Shutdown(context.Background())
		return nil, err
	}
	warnUnknownLSPTools(lspCfg.Tools, summary.Names, logger)
	runtime := &lspRuntime{mgr: mgr, summary: summary, enabled: lspCfg.Enable, prewarm: lspCfg.Prewarm, logger: logger}
	catalog.SetDescriptionSuffix(lspDescriptionSuffix(runtime))
	runtime.startPrewarm()
	return runtime, nil
}

// startPrewarm background-launches the language servers whose languages have
// files in the detected workspace root, so early lsp_* calls are warm. It is a
// best-effort optimization: it never blocks startup and never fails it.
func (r *lspRuntime) startPrewarm() {
	if r == nil || !r.enabled || !r.prewarm {
		return
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		languages := r.mgr.Prewarm(ctx)
		r.logger.Info("lsp: prewarmed language servers", logging.Category("lsp"), slog.Any("languages", languages))
	}()
}

func (r *lspRuntime) ActiveSummary() mcptools.Summary {
	if r == nil || !r.enabled {
		return mcptools.Summary{}
	}
	return r.summary
}

func (r *lspRuntime) SetEnabled(enabled bool) {
	if r == nil || r.enabled == enabled {
		return
	}
	r.enabled = enabled
	if enabled {
		r.mgr.RefreshAvailability()
		r.startPrewarm()
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	r.mgr.Shutdown(ctx)
}

func (r *lspRuntime) Shutdown() {
	if r != nil {
		r.mgr.Shutdown(context.Background())
	}
}

func (r *lspRuntime) SystemHint() string {
	if r == nil || !r.enabled {
		return ""
	}
	return lspSystemHint(r.mgr.AvailableLanguages())
}

func (r *lspRuntime) Status() ui.LSPStatus {
	if r == nil {
		return ui.LSPStatus{}
	}
	r.mgr.RefreshAvailability()
	servers := r.mgr.ServerStatuses()
	uiServers := make([]ui.LSPServerStatus, 0, len(servers))
	loaded := map[string]bool{}
	for _, server := range servers {
		if len(server.LoadedRoots) > 0 {
			for _, language := range server.Languages {
				loaded[language] = true
			}
		}
		uiServers = append(uiServers, ui.LSPServerStatus{
			Name: server.Name, Languages: server.Languages, Command: server.Command,
			Available: server.Available, LoadedRoots: server.LoadedRoots,
		})
	}
	return ui.LSPStatus{
		Enabled: r.enabled, Tools: slices.Clone(r.summary.Names),
		AvailableLanguages: r.mgr.AvailableLanguages(),
		LoadedLanguages:    slices.Sorted(maps.Keys(loaded)),
		Servers:            uiServers,
	}
}

func lspSystemHint(languages []string) string {
	if len(languages) == 0 {
		return "lsp_* tools enabled but no language server on PATH."
	}
	return "lsp_* available for: " + strings.Join(languages, ", ") + ". Prefer lsp_* over search for definitions, references, hover, symbols, diagnostics, and rename."
}

// warnUnknownLSPTools logs a warning for each configured lsp.tools entry that did
// not match a registered tool (a typo, or a tool this provider does not expose),
// so an operator's restriction does not silently register nothing.
func warnUnknownLSPTools(want, registered []string, logger *slog.Logger) {
	if len(want) == 0 {
		return
	}
	have := make(map[string]bool, len(registered))
	for _, n := range registered {
		have[n] = true
	}
	for _, w := range want {
		w = strings.TrimSpace(w)
		if w == "" {
			continue
		}
		bare := strings.TrimPrefix(w, "lsp_")
		if strings.EqualFold(bare, "all") {
			continue
		}
		full := w
		if !strings.HasPrefix(full, "lsp_") {
			full = "lsp_" + w
		}
		if !have[full] {
			logger.Warn(fmt.Sprintf("lsp: configured tool %q is not a known LSP tool; ignoring", w), logging.Category("lsp"))
		}
	}
}

type lspExplicitTools map[string][]string

// captureLSPExplicitTools records explicit agent allowlist entries and removes
// them from MCP refresh bases. LSP exposure is then re-applied from one runtime
// gate, so a remote MCP list refresh cannot accidentally re-enable disabled LSP.
func captureLSPExplicitTools(agents map[string]agentdef.Definition, bases mcpAgentBases) lspExplicitTools {
	explicit := make(lspExplicitTools)
	for name, agent := range agents {
		for _, tool := range agent.AllowedTools {
			if strings.HasPrefix(tool, "lsp_") {
				explicit[name] = append(explicit[name], tool)
			}
		}
	}
	for name, base := range bases {
		base.Allowed = withoutLSPTools(base.Allowed)
		bases[name] = base
	}
	return explicit
}

func applyLSPExposure(agents map[string]agentdef.Definition, summary mcptools.Summary, enabled bool, explicit lspExplicitTools) {
	for name, agent := range agents {
		agent.AllowedTools = withoutLSPTools(agent.AllowedTools)
		if enabled {
			agent.AllowedTools = appendMCPNames(agent.AllowedTools, explicit[name])
			agent.AllowedTools = appendMCPNames(agent.AllowedTools, mcpNamesForMode(agent.MCPTools, summary.Names, summary.ReadOnlyNames))
		}
		agents[name] = agent
	}
}

func withoutLSPTools(names []string) []string {
	out := make([]string, 0, len(names))
	for _, name := range names {
		if !strings.HasPrefix(name, "lsp_") {
			out = append(out, name)
		}
	}
	return out
}

func convertLSPServers(in map[string]config.LSPServerConfig) map[string]lspproxy.ServerConfig {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]lspproxy.ServerConfig, len(in))
	for name, s := range in {
		out[name] = lspproxy.ServerConfig{
			Languages:   append([]string(nil), s.Languages...),
			RootMarkers: append([]string(nil), s.RootMarkers...),
			Command:     append([]string(nil), s.Command...),
			Extensions:  append([]string(nil), s.Extensions...),
			Env:         maps.Clone(s.Env),
			InitOptions: append([]byte(nil), s.InitOptions...),
		}
	}
	return out
}

func mergeMCPSummaries(summaries ...mcptools.Summary) mcptools.Summary {
	out := mcptools.Summary{Servers: make(map[string]int)}
	for _, sum := range summaries {
		for server, count := range sum.Servers {
			out.Servers[server] += count
		}
		out.Skipped = append(out.Skipped, sum.Skipped...)
		out.Names = append(out.Names, sum.Names...)
		out.ReadOnlyNames = append(out.ReadOnlyNames, sum.ReadOnlyNames...)
		out.Total += sum.Total
	}
	if len(out.Servers) == 0 {
		out.Servers = nil
	}
	return out
}
