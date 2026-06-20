package main

import (
	"context"
	"fmt"
	"log/slog"
	"maps"
	"strings"

	"harness/internal/config"
	"harness/internal/logging"
	"harness/internal/lspproxy"
	"harness/internal/lsptools"
	"harness/internal/mcptools"
	"harness/internal/tools"
)

// setupLSP registers harness's first-class LSP tools with short lsp_* names and
// returns a cleanup func that stops any language servers launched during the
// session. LSP is optional: on setup failure it logs one warning and leaves the
// rest of harness usable.
func setupLSP(ctx context.Context, lspCfg config.LSPConfig, catalog *tools.Registry, logger *slog.Logger) (summary mcptools.Summary, cleanup func(), ok bool) {
	noop := func() {}
	cfg, err := lspproxy.LoadConfigWithServers(convertLSPServers(lspCfg.Servers))
	if err != nil {
		logger.Warn(fmt.Sprintf("lsp: cannot load configuration: %v; LSP tools unavailable", err), logging.Category("lsp"))
		return mcptools.Summary{}, noop, false
	}
	for _, w := range cfg.Warnings {
		logger.Warn(w, logging.Category("lsp"))
	}

	mgr := lspproxy.NewManager(cfg, "", logger)
	sum, err := lsptools.Register(ctx, catalog, mgr, lspCfg.Tools...)
	if err != nil {
		logger.Warn(fmt.Sprintf("lsp: cannot register tools: %v; LSP tools unavailable", err), logging.Category("lsp"))
		mgr.Shutdown(context.Background())
		return mcptools.Summary{}, noop, false
	}
	warnUnknownLSPTools(lspCfg.Tools, sum.Names, logger)
	logger.Info(fmt.Sprintf("lsp: registered %d tools", sum.Total), logging.Category("lsp"))
	return sum, func() { mgr.Shutdown(context.Background()) }, true
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
		full := w
		if !strings.HasPrefix(full, "lsp_") {
			full = "lsp_" + w
		}
		if !have[full] {
			logger.Warn(fmt.Sprintf("lsp: configured tool %q is not a known LSP tool; ignoring", w), logging.Category("lsp"))
		}
	}
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
