package main

import (
	"context"
	"fmt"
	"log/slog"
	"maps"

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
	sum, err := lsptools.Register(ctx, catalog, mgr)
	if err != nil {
		logger.Warn(fmt.Sprintf("lsp: cannot register tools: %v; LSP tools unavailable", err), logging.Category("lsp"))
		mgr.Shutdown(context.Background())
		return mcptools.Summary{}, noop, false
	}
	logger.Info(fmt.Sprintf("lsp: registered %d tools", sum.Total), logging.Category("lsp"))
	return sum, func() { mgr.Shutdown(context.Background()) }, true
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
