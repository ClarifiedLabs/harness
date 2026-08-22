package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"maps"
	"slices"
	"strings"
	"sync"
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

const maxPostMutationDiagnosticFiles = 8

// PostMutationDiagnostics returns bounded, path-ordered diagnostics for files
// successfully changed through Harness's built-in write/edit tools. Unsupported
// paths and unavailable servers are silent so LSP remains optional.
func (r *lspRuntime) PostMutationDiagnostics(ctx context.Context, paths []string) string {
	if r == nil || !r.enabled || len(paths) == 0 {
		return ""
	}
	unique := make([]string, 0, min(len(paths), maxPostMutationDiagnosticFiles))
	seen := make(map[string]bool, len(paths))
	for _, path := range paths {
		path = strings.TrimSpace(path)
		if path == "" || seen[path] {
			continue
		}
		seen[path] = true
		unique = append(unique, path)
		if len(unique) == maxPostMutationDiagnosticFiles {
			break
		}
	}
	results := make([]string, len(unique))
	var wg sync.WaitGroup
	for i, path := range unique {
		wg.Add(1)
		go func() {
			defer wg.Done()
			text, applicable, err := r.mgr.DiagnosticsAfterWrite(ctx, path, 3*time.Second)
			if !applicable {
				return
			}
			if err != nil {
				results[i] = fmt.Sprintf("%s: diagnostics unavailable: %v", path, err)
				return
			}
			results[i] = text
		}()
	}
	wg.Wait()
	results = slices.DeleteFunc(results, func(value string) bool { return value == "" })
	return strings.Join(results, "\n")
}

type mutationDiagnosticsTool struct {
	base     tools.Tool
	diagnose func(context.Context, []string) string
}

func installMutationDiagnostics(catalog *tools.Registry, runtime *lspRuntime) {
	if catalog == nil || runtime == nil {
		return
	}
	for _, name := range []string{"edit", "write"} {
		base, ok := catalog.Lookup(name)
		if !ok {
			continue
		}
		if _, ok := base.(tools.FileMutationReporter); !ok {
			continue
		}
		catalog.Register(&mutationDiagnosticsTool{base: base, diagnose: runtime.PostMutationDiagnostics})
	}
}

func (t *mutationDiagnosticsTool) Name() string                        { return t.base.Name() }
func (t *mutationDiagnosticsTool) Description() string                 { return t.base.Description() }
func (t *mutationDiagnosticsTool) Schema() json.RawMessage             { return t.base.Schema() }
func (t *mutationDiagnosticsTool) ReadOnly(input json.RawMessage) bool { return t.base.ReadOnly(input) }

func (t *mutationDiagnosticsTool) PreserveSchemaDescriptions() bool {
	preserver, ok := t.base.(tools.SchemaDescriptionPreserver)
	return ok && preserver.PreserveSchemaDescriptions()
}

func (t *mutationDiagnosticsTool) MutatedPaths(input json.RawMessage) ([]string, error) {
	return t.base.(tools.FileMutationReporter).MutatedPaths(input)
}

func (t *mutationDiagnosticsTool) RetentionInputReceipt(input json.RawMessage) (json.RawMessage, bool) {
	trimmer, ok := t.base.(tools.InputTrimmer)
	if !ok {
		return nil, false
	}
	return trimmer.RetentionInputReceipt(input)
}

func (t *mutationDiagnosticsTool) RequiresSequential(input json.RawMessage) bool {
	sequential, ok := t.base.(tools.SequentialTool)
	return ok && sequential.RequiresSequential(input)
}

func (t *mutationDiagnosticsTool) Run(ctx context.Context, input json.RawMessage) (string, error) {
	result, err := t.RunResult(ctx, input)
	return result.Text, err
}

func (t *mutationDiagnosticsTool) RunResult(ctx context.Context, input json.RawMessage) (tools.RunResult, error) {
	var result tools.RunResult
	var err error
	if resultTool, ok := t.base.(tools.ResultTool); ok {
		result, err = resultTool.RunResult(ctx, input)
	} else {
		result.Text, err = t.base.Run(ctx, input)
	}
	if err != nil || t.diagnose == nil {
		return result, err
	}
	paths, pathErr := t.MutatedPaths(input)
	if pathErr != nil {
		return result, nil
	}
	diagnostics := strings.TrimSpace(t.diagnose(ctx, paths))
	if diagnostics == "" {
		return result, nil
	}
	suffix := "\n\nLSP diagnostics after mutation:\n" + diagnostics
	result.Text += suffix
	if result.OriginalText != "" {
		result.OriginalText += suffix
	}
	return result, nil
}

var _ tools.Tool = (*mutationDiagnosticsTool)(nil)
var _ tools.ResultTool = (*mutationDiagnosticsTool)(nil)
var _ tools.FileMutationReporter = (*mutationDiagnosticsTool)(nil)
var _ tools.InputTrimmer = (*mutationDiagnosticsTool)(nil)
var _ tools.SequentialTool = (*mutationDiagnosticsTool)(nil)
var _ tools.SchemaDescriptionPreserver = (*mutationDiagnosticsTool)(nil)

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
	return "lsp_* available for: " + strings.Join(languages, ", ") + ". Prefer lsp_* over text search for definitions, references, hover, symbols, diagnostics, and rename."
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
