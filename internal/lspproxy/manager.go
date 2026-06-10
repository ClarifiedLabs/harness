package lspproxy

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"harness/internal/mcp"
)

// defaultDiagnosticsTimeout bounds the wait for a server to publish diagnostics.
const defaultDiagnosticsTimeout = 3000 * time.Millisecond

// defaultMaxResults caps references/workspace-symbol output by default.
const defaultMaxResults = 100

// Manager is the shim's MCP ToolProvider. It selects a language server per file,
// lazily launches one instance per (server, workspace-root), manages open
// documents, and renders LSP results as compact text. It is safe for concurrent
// use.
type Manager struct {
	cfg       Config
	namespace string // tools are exposed as mcp__<namespace>__<tool>; empty = bare names
	logger    *slog.Logger

	mu        sync.Mutex
	instances map[string]*serverInstance
	docs      map[openDocKey]*docState
	available []string

	// Test/production seams.
	spawn     func() *exec.Cmd             // injected into instances
	clock     func() time.Time             // injected into instances
	lookPath  func(string) (string, error) // availability probe
	acquireFn func(ctx context.Context, s ResolvedServer, root string) (*lspClient, error)
}

// openDocKey scopes document sync state to one live LSP client. A relaunched
// server gets a fresh client pointer and therefore a fresh didOpen.
type openDocKey struct {
	instKey string
	client  *lspClient
	uri     string
}

// docState tracks a successfully-opened document's version and the mtime we
// last synced to the current LSP client.
type docState struct {
	version int
	mtime   time.Time
}

// NewManager builds a Manager over cfg and probes which configured servers are
// installed (for the dynamic tool descriptions). namespace prefixes the exposed
// tool names as mcp__<namespace>__<tool> (so harness can register them directly);
// an empty namespace exposes bare names (for hosting behind a proxy that
// namespaces them itself).
func NewManager(cfg Config, namespace string, logger *slog.Logger) *Manager {
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	m := &Manager{
		cfg:       cfg,
		namespace: namespace,
		logger:    logger,
		instances: make(map[string]*serverInstance),
		docs:      make(map[openDocKey]*docState),
		lookPath:  exec.LookPath,
	}
	m.computeAvailable()
	return m
}

// publicName maps a bare tool name to its exposed form (mcp__<namespace>__<bare>
// when a namespace is set).
func (m *Manager) publicName(bare string) string {
	if m.namespace == "" {
		return bare
	}
	return "mcp__" + m.namespace + "__" + bare
}

// bareName strips the namespace prefix from an incoming tool-call name, so the
// dispatch switch always matches on the bare name.
func (m *Manager) bareName(name string) string {
	if m.namespace == "" {
		return name
	}
	return strings.TrimPrefix(name, "mcp__"+m.namespace+"__")
}

// computeAvailable records the sorted set of languages whose server binary is on
// PATH, so ListTools can advertise what actually works here.
func (m *Manager) computeAvailable() {
	present := map[string]bool{}
	for _, s := range m.cfg.Servers {
		if len(s.Command) == 0 {
			continue
		}
		if _, err := m.lookPath(s.Command[0]); err == nil {
			for _, l := range s.Languages {
				present[l] = true
			}
		}
	}
	langs := make([]string, 0, len(present))
	for l := range present {
		langs = append(langs, l)
	}
	sort.Strings(langs)
	m.available = langs
}

// ListTools returns the fixed 7-tool surface with descriptions augmented by the
// available-language list. The set fits one page; a non-empty cursor returns an
// empty page.
func (m *Manager) ListTools(ctx context.Context, cursor string) (mcp.ListToolsResult, error) {
	if cursor != "" {
		return mcp.ListToolsResult{}, nil
	}
	suffix := " No LSP servers on PATH."
	if len(m.available) > 0 {
		suffix = " Langs: " + strings.Join(m.available, ", ") + "."
	}
	tools := make([]mcp.Tool, 0, len(toolSpecs))
	for _, spec := range toolSpecs {
		tools = append(tools, mcp.Tool{
			Name:        m.publicName(spec.name),
			Description: spec.description + suffix,
			InputSchema: json.RawMessage(spec.schema),
			Annotations: json.RawMessage(toolAnnotations),
		})
	}
	return mcp.ListToolsResult{Tools: tools}, nil
}

// toolArgs is the union of arguments across all tools; each handler reads the
// fields it needs.
type toolArgs struct {
	Path               string `json:"path"`
	Line               int    `json:"line"`
	Symbol             string `json:"symbol"`
	Column             int    `json:"column"`
	IncludeDeclaration *bool  `json:"include_declaration"`
	MaxResults         int    `json:"max_results"`
	Query              string `json:"query"`
	TimeoutMS          int    `json:"timeout_ms"`
	NewName            string `json:"new_name"`
}

// CallTool dispatches a tool call. Unknown tool / bad params are protocol errors
// (returned as error); every other failure is a CallToolResult with IsError so
// the model sees a normal tool failure.
func (m *Manager) CallTool(ctx context.Context, name string, raw json.RawMessage) (*mcp.CallToolResult, error) {
	var args toolArgs
	if len(raw) > 0 && string(raw) != "null" {
		if err := json.Unmarshal(raw, &args); err != nil {
			return nil, fmt.Errorf("invalid arguments: %w", err)
		}
	}
	switch m.bareName(name) {
	case "definition":
		return m.handleDefinition(ctx, args)
	case "references":
		return m.handleReferences(ctx, args)
	case "hover":
		return m.handleHover(ctx, args)
	case "document_symbols":
		return m.handleDocumentSymbols(ctx, args)
	case "workspace_symbols":
		return m.handleWorkspaceSymbols(ctx, args)
	case "diagnostics":
		return m.handleDiagnostics(ctx, args)
	case "rename_plan":
		return m.handleRenamePlan(ctx, args)
	default:
		return nil, fmt.Errorf("unknown tool: %s", name)
	}
}

// fileTarget bundles a resolved file with its language server client.
type fileTarget struct {
	abs     string
	lang    string
	instKey string
	cl      *lspClient
}

// targetFor resolves path to a server + workspace root and acquires a live
// client. On a recoverable problem (no server, server down) it returns an
// IsError result instead of an error.
func (m *Manager) targetFor(ctx context.Context, path string) (*fileTarget, *mcp.CallToolResult) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, errorResult("invalid path %q: %v", path, err)
	}
	s, lang, ok := m.selectServer(abs)
	if !ok {
		return nil, errorResult("no language server configured for %s files", extOrName(abs))
	}
	root, _ := detectRoot(filepath.Dir(abs), s.RootMarkers)
	cl, err := m.acquire(ctx, s, root)
	if err != nil {
		return nil, errorResult("%v", err)
	}
	return &fileTarget{abs: abs, lang: lang, instKey: instanceKey(s.Name, root), cl: cl}, nil
}

func (m *Manager) handleDefinition(ctx context.Context, args toolArgs) (*mcp.CallToolResult, error) {
	ft, errRes := m.targetFor(ctx, args.Path)
	if errRes != nil {
		return errRes, nil
	}
	uri, lines, err := m.prepareDoc(ft, uriForPath(ft.abs))
	if err != nil {
		return errorResult("open %s: %v", ft.abs, err), nil
	}
	pos, err := positionFromArgs(lines, args)
	if err != nil {
		return errorResult("%v", err), nil
	}
	locs, err := ft.cl.Definition(ctx, uri, pos)
	if err != nil {
		return errorResult("definition failed: %v", err), nil
	}
	if len(locs) == 0 {
		return textResult("no definition found"), nil
	}
	return textResult(formatLocations(locs, m.snippetFunc(newLineReader()))), nil
}

func (m *Manager) handleReferences(ctx context.Context, args toolArgs) (*mcp.CallToolResult, error) {
	ft, errRes := m.targetFor(ctx, args.Path)
	if errRes != nil {
		return errRes, nil
	}
	uri, lines, err := m.prepareDoc(ft, uriForPath(ft.abs))
	if err != nil {
		return errorResult("open %s: %v", ft.abs, err), nil
	}
	pos, err := positionFromArgs(lines, args)
	if err != nil {
		return errorResult("%v", err), nil
	}
	includeDecl := args.IncludeDeclaration == nil || *args.IncludeDeclaration
	locs, err := ft.cl.References(ctx, uri, pos, includeDecl)
	if err != nil {
		return errorResult("references failed: %v", err), nil
	}
	max := args.MaxResults
	if max <= 0 {
		max = defaultMaxResults
	}
	return textResult(formatReferences(locs, max, m.snippetFunc(newLineReader()))), nil
}

func (m *Manager) handleHover(ctx context.Context, args toolArgs) (*mcp.CallToolResult, error) {
	ft, errRes := m.targetFor(ctx, args.Path)
	if errRes != nil {
		return errRes, nil
	}
	uri, lines, err := m.prepareDoc(ft, uriForPath(ft.abs))
	if err != nil {
		return errorResult("open %s: %v", ft.abs, err), nil
	}
	pos, err := positionFromArgs(lines, args)
	if err != nil {
		return errorResult("%v", err), nil
	}
	hover, err := ft.cl.Hover(ctx, uri, pos)
	if err != nil {
		return errorResult("hover failed: %v", err), nil
	}
	if hover == "" {
		return textResult("no hover information"), nil
	}
	return textResult(hover), nil
}

func (m *Manager) handleDocumentSymbols(ctx context.Context, args toolArgs) (*mcp.CallToolResult, error) {
	ft, errRes := m.targetFor(ctx, args.Path)
	if errRes != nil {
		return errRes, nil
	}
	uri, _, err := m.prepareDoc(ft, uriForPath(ft.abs))
	if err != nil {
		return errorResult("open %s: %v", ft.abs, err), nil
	}
	syms, err := ft.cl.DocumentSymbols(ctx, uri)
	if err != nil {
		return errorResult("document symbols failed: %v", err), nil
	}
	if len(syms) == 0 {
		return textResult("no symbols found"), nil
	}
	return textResult(formatDocumentSymbols(syms, ft.abs)), nil
}

func (m *Manager) handleWorkspaceSymbols(ctx context.Context, args toolArgs) (*mcp.CallToolResult, error) {
	if args.Query == "" {
		return errorResult("query is required"), nil
	}
	var cl *lspClient
	if args.Path != "" {
		ft, errRes := m.targetFor(ctx, args.Path)
		if errRes != nil {
			return errRes, nil
		}
		cl = ft.cl
	} else {
		if len(m.cfg.Servers) != 1 {
			return errorResult("provide 'path' (any file in the target project) to pick the workspace"), nil
		}
		cwd, _ := os.Getwd()
		c, err := m.acquire(ctx, m.cfg.Servers[0], cwd)
		if err != nil {
			return errorResult("%v", err), nil
		}
		cl = c
	}
	syms, err := cl.WorkspaceSymbols(ctx, args.Query)
	if err != nil {
		return errorResult("workspace symbols failed: %v", err), nil
	}
	if len(syms) == 0 {
		return textResult("no symbols found"), nil
	}
	max := args.MaxResults
	if max <= 0 {
		max = defaultMaxResults
	}
	if len(syms) > max {
		syms = syms[:max]
	}
	return textResult(formatWorkspaceSymbols(syms)), nil
}

func (m *Manager) handleDiagnostics(ctx context.Context, args toolArgs) (*mcp.CallToolResult, error) {
	ft, errRes := m.targetFor(ctx, args.Path)
	if errRes != nil {
		return errRes, nil
	}
	uri, _, err := m.prepareDoc(ft, uriForPath(ft.abs))
	if err != nil {
		return errorResult("open %s: %v", ft.abs, err), nil
	}
	timeout := defaultDiagnosticsTimeout
	if args.TimeoutMS > 0 {
		timeout = time.Duration(args.TimeoutMS) * time.Millisecond
	}
	diags, ok, err := ft.cl.WaitDiagnostics(ctx, uri, timeout)
	if err != nil {
		return errorResult("diagnostics failed: %v", err), nil
	}
	out := formatDiagnostics(diags, ft.abs)
	if !ok {
		out += "\n(diagnostics may be incomplete; the server did not finish analysis before the timeout)"
	}
	return textResult(out), nil
}

func (m *Manager) handleRenamePlan(ctx context.Context, args toolArgs) (*mcp.CallToolResult, error) {
	if args.NewName == "" {
		return errorResult("new_name is required"), nil
	}
	ft, errRes := m.targetFor(ctx, args.Path)
	if errRes != nil {
		return errRes, nil
	}
	uri, lines, err := m.prepareDoc(ft, uriForPath(ft.abs))
	if err != nil {
		return errorResult("open %s: %v", ft.abs, err), nil
	}
	pos, err := positionFromArgs(lines, args)
	if err != nil {
		return errorResult("%v", err), nil
	}
	edits, err := ft.cl.Rename(ctx, uri, pos, args.NewName)
	if err != nil {
		return errorResult("rename failed: %v", err), nil
	}
	lr := newLineReader()
	lineFor := func(u string, line int) (string, bool) { return lr.line(uriToPath(u), line) }
	return textResult(formatRenamePlan(edits, lineFor)), nil
}

// selectServer picks the configured server for a file: first one claiming the
// extension directly, else the first listing the extension's built-in language.
func (m *Manager) selectServer(absPath string) (ResolvedServer, string, bool) {
	ext := filepath.Ext(absPath)
	for _, s := range m.cfg.Servers {
		if slices.Contains(s.Extensions, ext) {
			return s, s.Languages[0], true
		}
	}
	lang, ok := languageForExt(ext)
	if !ok {
		return ResolvedServer{}, "", false
	}
	for _, s := range m.cfg.Servers {
		if slices.Contains(s.Languages, lang) {
			return s, lang, true
		}
	}
	return ResolvedServer{}, "", false
}

// acquire returns a live client for (s, root), via the test seam if set.
func (m *Manager) acquire(ctx context.Context, s ResolvedServer, root string) (*lspClient, error) {
	if m.acquireFn != nil {
		return m.acquireFn(ctx, s, root)
	}
	key := instanceKey(s.Name, root)
	m.mu.Lock()
	inst := m.instances[key]
	if inst == nil {
		inst = newServerInstance(s, root, m.logger)
		inst.spawn = m.spawn
		if m.clock != nil {
			inst.clock = m.clock
		}
		m.instances[key] = inst
	}
	m.mu.Unlock()
	return inst.ensure(ctx)
}

// prepareDoc reads abs from disk and syncs it with the server: didOpen on first
// use, didChange (full text) when the file changed since the last sync. It marks
// the doc pending first so a following diagnostics wait blocks for the fresh
// publish. It returns the document URI and its lines.
func (m *Manager) prepareDoc(ft *fileTarget, uri string) (string, []string, error) {
	data, err := os.ReadFile(ft.abs)
	if err != nil {
		return "", nil, err
	}
	text := string(data)
	lines := splitLines(text)

	var mtime time.Time
	if info, err := os.Stat(ft.abs); err == nil {
		mtime = info.ModTime()
	}
	docKey := openDocKey{instKey: ft.instKey, client: ft.cl, uri: uri}

	// Decide and commit the sync action under the lock. Keeping the notification
	// send inside the critical section prevents a second concurrent request from
	// observing optimistic state before the server has accepted didOpen/didChange.
	m.mu.Lock()
	defer m.mu.Unlock()

	st := m.docs[docKey]
	switch {
	case st == nil:
		ft.cl.MarkDocPending(uri)
		if err := ft.cl.DidOpen(uri, ft.lang, 1, text); err != nil {
			return "", nil, err
		}
		m.docs[docKey] = &docState{version: 1, mtime: mtime}
	case st.mtime != mtime:
		version := st.version + 1
		ft.cl.MarkDocPending(uri)
		if err := ft.cl.DidChange(uri, version, text); err != nil {
			return "", nil, err
		}
		st.version = version
		st.mtime = mtime
	}
	return uri, lines, nil
}

// snippetFunc returns a snippet provider for the formatters that reads (and
// caches) the trimmed source line at a result location.
func (m *Manager) snippetFunc(lr *lineReader) func(uri string, line int) string {
	return func(uri string, line int) string {
		s, ok := lr.line(uriToPath(uri), line)
		if !ok {
			return ""
		}
		return strings.TrimSpace(s)
	}
}

// Shutdown gracefully stops all launched language servers.
func (m *Manager) Shutdown(ctx context.Context) {
	m.mu.Lock()
	insts := make([]*serverInstance, 0, len(m.instances))
	for _, inst := range m.instances {
		insts = append(insts, inst)
	}
	m.instances = make(map[string]*serverInstance)
	m.docs = make(map[openDocKey]*docState)
	m.mu.Unlock()
	for _, inst := range insts {
		inst.shutdown(ctx)
	}
}

// instanceKey keys an instance by server name and workspace root.
func instanceKey(name, root string) string {
	return name + "\x00" + root
}

// resolveCharacter computes the UTF-16 column for a position request: an
// explicit 1-based rune column wins so repeated symbols can be disambiguated,
// otherwise the named symbol's first location is used, else 0.
func resolveCharacter(lineText, symbol string, oneBasedColumn int) (int, error) {
	if oneBasedColumn > 0 {
		return runeColToUTF16(lineText, oneBasedColumn), nil
	}
	if symbol != "" {
		col, ok := symbolColumnUTF16(lineText, symbol)
		if !ok {
			return 0, fmt.Errorf("symbol %q not found on the line", symbol)
		}
		return col, nil
	}
	return 0, nil
}

// positionFromArgs turns the 1-based line + symbol/column into an LSP Position.
func positionFromArgs(lines []string, args toolArgs) (Position, error) {
	if args.Line < 1 || args.Line > len(lines) {
		return Position{}, fmt.Errorf("line %d is out of range (file has %d lines)", args.Line, len(lines))
	}
	ch, err := resolveCharacter(lines[args.Line-1], args.Symbol, args.Column)
	if err != nil {
		return Position{}, err
	}
	return Position{Line: args.Line - 1, Character: ch}, nil
}

// lineReader caches file lines read while formatting one tool call's results.
type lineReader struct {
	cache map[string][]string
}

func newLineReader() *lineReader {
	return &lineReader{cache: make(map[string][]string)}
}

func (lr *lineReader) line(path string, line0 int) (string, bool) {
	lines, ok := lr.cache[path]
	if !ok {
		if data, err := os.ReadFile(path); err == nil {
			lines = splitLines(string(data))
		}
		lr.cache[path] = lines
	}
	if line0 < 0 || line0 >= len(lines) {
		return "", false
	}
	return lines[line0], true
}

// splitLines splits text into lines, stripping a trailing CR so CRLF files read
// cleanly.
func splitLines(text string) []string {
	lines := strings.Split(text, "\n")
	for i := range lines {
		lines[i] = strings.TrimSuffix(lines[i], "\r")
	}
	return lines
}

// extOrName returns the file extension for an error message, or the base name
// when there is no extension.
func extOrName(path string) string {
	if ext := filepath.Ext(path); ext != "" {
		return ext
	}
	return filepath.Base(path)
}

// textResult builds a successful single-text-block result.
func textResult(text string) *mcp.CallToolResult {
	return &mcp.CallToolResult{Content: []mcp.ContentBlock{{Type: "text", Text: text}}}
}

// errorResult builds an IsError single-text-block result (a normal tool failure
// the model can read, not a protocol error).
func errorResult(format string, a ...any) *mcp.CallToolResult {
	return &mcp.CallToolResult{
		IsError: true,
		Content: []mcp.ContentBlock{{Type: "text", Text: fmt.Sprintf(format, a...)}},
	}
}
