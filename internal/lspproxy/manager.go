package lspproxy

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"harness/internal/mcp"
)

// defaultDiagnosticsTimeout bounds the wait for a server to publish diagnostics.
const defaultDiagnosticsTimeout = 3000 * time.Millisecond

// defaultMaxResults caps bounded list operations by default.
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
	present   map[string]bool // configured server name -> command found on PATH

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
// installed (for a one-time runtime hint). namespace prefixes the exposed
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
		present:   make(map[string]bool),
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
// PATH for the one-time runtime hint.
func (m *Manager) computeAvailable() {
	present := map[string]bool{}
	servers := make(map[string]bool, len(m.cfg.Servers))
	for _, s := range m.cfg.Servers {
		if len(s.Command) == 0 {
			continue
		}
		if _, err := m.lookPath(s.Command[0]); err == nil {
			servers[s.Name] = true
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
	m.mu.Lock()
	m.available = langs
	m.present = servers
	m.mu.Unlock()
}

// RefreshAvailability re-probes configured language-server commands. It lets
// /lsp reflect a binary installed or removed during a long-running session.
func (m *Manager) RefreshAvailability() { m.computeAvailable() }

// AvailableLanguages returns the sorted languages backed by a server binary
// currently on PATH.
func (m *Manager) AvailableLanguages() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return slices.Clone(m.available)
}

// ServerStatus is a user-facing snapshot of one configured server. LoadedRoots
// contains roots whose server process is currently alive; availability only
// means the configured executable can be found on PATH.
type ServerStatus struct {
	Name        string
	Languages   []string
	Command     string
	Available   bool
	LoadedRoots []string
}

// ServerStatuses returns configured servers in stable config order with live
// instance state. It does not launch a server.
func (m *Manager) ServerStatuses() []ServerStatus {
	m.mu.Lock()
	statuses := make([]ServerStatus, 0, len(m.cfg.Servers))
	index := make(map[string]int, len(m.cfg.Servers))
	for _, server := range m.cfg.Servers {
		status := ServerStatus{
			Name: server.Name, Languages: slices.Clone(server.Languages),
			Available: m.present[server.Name],
		}
		if len(server.Command) > 0 {
			status.Command = server.Command[0]
		}
		index[server.Name] = len(statuses)
		statuses = append(statuses, status)
	}
	instances := make([]*serverInstance, 0, len(m.instances))
	for _, inst := range m.instances {
		instances = append(instances, inst)
	}
	m.mu.Unlock()
	for _, inst := range instances {
		name, root, loaded := inst.status()
		if !loaded {
			continue
		}
		if i, ok := index[name]; ok {
			statuses[i].LoadedRoots = append(statuses[i].LoadedRoots, root)
		}
	}
	for i := range statuses {
		sort.Strings(statuses[i].LoadedRoots)
	}
	return statuses
}

// LoadedLanguages returns the sorted languages with at least one live server
// process. It is intentionally distinct from AvailableLanguages, which is only
// a PATH probe and does not mean the server has been initialized.
func (m *Manager) LoadedLanguages() []string {
	set := map[string]bool{}
	for _, status := range m.ServerStatuses() {
		if len(status.LoadedRoots) == 0 {
			continue
		}
		for _, language := range status.Languages {
			set[language] = true
		}
	}
	languages := slices.Sorted(maps.Keys(set))
	return languages
}

// ListTools returns the fixed tool surface. The set fits one page; a non-empty
// cursor returns an empty page.
func (m *Manager) ListTools(ctx context.Context, cursor string) (mcp.ListToolsResult, error) {
	if cursor != "" {
		return mcp.ListToolsResult{}, nil
	}
	tools := make([]mcp.Tool, 0, len(toolSpecs))
	for _, spec := range toolSpecs {
		annotations := mutatingToolAnnotations
		if spec.readOnly {
			annotations = readOnlyToolAnnotations
		}
		tools = append(tools, mcp.Tool{
			Name:        m.publicName(spec.name),
			Description: spec.description,
			InputSchema: json.RawMessage(spec.schema),
			Annotations: json.RawMessage(annotations),
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
	Direction          string `json:"direction"`
	StartLine          int    `json:"start_line"`
	EndLine            int    `json:"end_line"`
	Kind               string `json:"kind"`
	Title              string `json:"title"`
	TabSize            int    `json:"tab_size"`
	InsertSpaces       *bool  `json:"insert_spaces"`
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
	case "declaration":
		return m.handleLocationRequest(ctx, args, "declaration", (*lspClient).Declaration)
	case "definition":
		return m.handleDefinition(ctx, args)
	case "type_definition":
		return m.handleLocationRequest(ctx, args, "type definition", (*lspClient).TypeDefinition)
	case "implementation":
		return m.handleLocationRequest(ctx, args, "implementation", (*lspClient).Implementation)
	case "references":
		return m.handleReferences(ctx, args)
	case "hover":
		return m.handleHover(ctx, args)
	case "signature_help":
		return m.handleSignatureHelp(ctx, args)
	case "completion":
		return m.handleCompletion(ctx, args)
	case "document_highlights":
		return m.handleDocumentHighlights(ctx, args)
	case "document_symbols":
		return m.handleDocumentSymbols(ctx, args)
	case "workspace_symbols":
		return m.handleWorkspaceSymbols(ctx, args)
	case "diagnostics":
		return m.handleDiagnostics(ctx, args)
	case "call_hierarchy":
		return m.handleCallHierarchy(ctx, args)
	case "type_hierarchy":
		return m.handleTypeHierarchy(ctx, args)
	case "inlay_hints":
		return m.handleInlayHints(ctx, args)
	case "code_actions":
		return m.handleCodeActions(ctx, args, false)
	case "code_action":
		return m.handleCodeActions(ctx, args, true)
	case "format_document_plan":
		return m.handleFormatDocument(ctx, args, false)
	case "format_document":
		return m.handleFormatDocument(ctx, args, true)
	case "rename_plan":
		return m.handleRenamePlan(ctx, args)
	case "rename":
		return m.handleRename(ctx, args)
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
	if strings.TrimSpace(path) == "" {
		return nil, errorResult("path is required")
	}
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

type positionTarget struct {
	ft  *fileTarget
	uri string
	pos Position
}

func (m *Manager) positionTarget(ctx context.Context, args toolArgs) (*positionTarget, *mcp.CallToolResult) {
	ft, errRes := m.targetFor(ctx, args.Path)
	if errRes != nil {
		return nil, errRes
	}
	uri, lines, err := m.prepareDoc(ft, uriForPath(ft.abs))
	if err != nil {
		return nil, errorResult("open %s: %v", ft.abs, err)
	}
	pos, err := positionFromArgs(lines, args)
	if err != nil {
		return nil, errorResult("%v", err)
	}
	return &positionTarget{ft: ft, uri: uri, pos: pos}, nil
}

type locationRequest func(*lspClient, context.Context, string, Position) ([]Location, error)

func (m *Manager) handleLocationRequest(ctx context.Context, args toolArgs, label string, request locationRequest) (*mcp.CallToolResult, error) {
	target, errRes := m.positionTarget(ctx, args)
	if errRes != nil {
		return errRes, nil
	}
	locs, err := request(target.ft.cl, ctx, target.uri, target.pos)
	if err != nil {
		return errorResult("%s failed: %v", label, err), nil
	}
	if len(locs) == 0 {
		return textResult("no " + label + " found"), nil
	}
	return textResult(formatLocations(locs, m.snippetFunc(newLineReader()))), nil
}

func (m *Manager) handleDefinition(ctx context.Context, args toolArgs) (*mcp.CallToolResult, error) {
	return m.handleLocationRequest(ctx, args, "definition", (*lspClient).Definition)
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

func (m *Manager) handleSignatureHelp(ctx context.Context, args toolArgs) (*mcp.CallToolResult, error) {
	target, errRes := m.positionTarget(ctx, args)
	if errRes != nil {
		return errRes, nil
	}
	help, err := target.ft.cl.SignatureHelp(ctx, target.uri, target.pos)
	if err != nil {
		return errorResult("signature help failed: %v", err), nil
	}
	if help == nil || len(help.Signatures) == 0 {
		return textResult("no signature help"), nil
	}
	return textResult(formatSignatureHelp(*help)), nil
}

func (m *Manager) handleCompletion(ctx context.Context, args toolArgs) (*mcp.CallToolResult, error) {
	target, errRes := m.positionTarget(ctx, args)
	if errRes != nil {
		return errRes, nil
	}
	// For completion, a symbol/prefix naturally means the cursor after that text;
	// other position tools intentionally address the symbol start.
	if args.Symbol != "" && args.Column <= 0 {
		target.pos.Character += utf16Len(args.Symbol)
	}
	items, err := target.ft.cl.Completion(ctx, target.uri, target.pos)
	if err != nil {
		return errorResult("completion failed: %v", err), nil
	}
	if len(items) == 0 {
		return textResult("no completions"), nil
	}
	max := args.MaxResults
	if max <= 0 {
		max = defaultMaxResults
	}
	return textResult(formatCompletions(items, max)), nil
}

func (m *Manager) handleDocumentHighlights(ctx context.Context, args toolArgs) (*mcp.CallToolResult, error) {
	target, errRes := m.positionTarget(ctx, args)
	if errRes != nil {
		return errRes, nil
	}
	highlights, err := target.ft.cl.DocumentHighlights(ctx, target.uri, target.pos)
	if err != nil {
		return errorResult("document highlights failed: %v", err), nil
	}
	if len(highlights) == 0 {
		return textResult("no document highlights"), nil
	}
	return textResult(formatDocumentHighlights(highlights, target.ft.abs, newLineReader())), nil
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

func (m *Manager) handleCallHierarchy(ctx context.Context, args toolArgs) (*mcp.CallToolResult, error) {
	direction := strings.TrimSpace(args.Direction)
	if direction != "incoming" && direction != "outgoing" {
		return errorResult("direction must be incoming or outgoing"), nil
	}
	target, errRes := m.positionTarget(ctx, args)
	if errRes != nil {
		return errRes, nil
	}
	items, err := target.ft.cl.PrepareCallHierarchy(ctx, target.uri, target.pos)
	if err != nil {
		return errorResult("prepare call hierarchy failed: %v", err), nil
	}
	if len(items) == 0 {
		return textResult("no call hierarchy item found"), nil
	}
	max := args.MaxResults
	if max <= 0 {
		max = defaultMaxResults
	}
	if direction == "incoming" {
		calls, err := target.ft.cl.IncomingCalls(ctx, items[0])
		if err != nil {
			return errorResult("incoming calls failed: %v", err), nil
		}
		return textResult(formatIncomingCalls(calls, max)), nil
	}
	calls, err := target.ft.cl.OutgoingCalls(ctx, items[0])
	if err != nil {
		return errorResult("outgoing calls failed: %v", err), nil
	}
	return textResult(formatOutgoingCalls(calls, max)), nil
}

func (m *Manager) handleTypeHierarchy(ctx context.Context, args toolArgs) (*mcp.CallToolResult, error) {
	direction := strings.TrimSpace(args.Direction)
	if direction != "supertypes" && direction != "subtypes" {
		return errorResult("direction must be supertypes or subtypes"), nil
	}
	target, errRes := m.positionTarget(ctx, args)
	if errRes != nil {
		return errRes, nil
	}
	items, err := target.ft.cl.PrepareTypeHierarchy(ctx, target.uri, target.pos)
	if err != nil {
		return errorResult("prepare type hierarchy failed: %v", err), nil
	}
	if len(items) == 0 {
		return textResult("no type hierarchy item found"), nil
	}
	items, err = target.ft.cl.TypeHierarchy(ctx, direction, items[0])
	if err != nil {
		return errorResult("%s failed: %v", direction, err), nil
	}
	max := args.MaxResults
	if max <= 0 {
		max = defaultMaxResults
	}
	return textResult(formatTypeHierarchy(items, direction, max)), nil
}

func (m *Manager) handleInlayHints(ctx context.Context, args toolArgs) (*mcp.CallToolResult, error) {
	ft, errRes := m.targetFor(ctx, args.Path)
	if errRes != nil {
		return errRes, nil
	}
	uri, lines, err := m.prepareDoc(ft, uriForPath(ft.abs))
	if err != nil {
		return errorResult("open %s: %v", ft.abs, err), nil
	}
	rng, err := rangeFromArgs(lines, args.StartLine, args.EndLine)
	if err != nil {
		return errorResult("%v", err), nil
	}
	hints, err := ft.cl.InlayHints(ctx, uri, rng)
	if err != nil {
		return errorResult("inlay hints failed: %v", err), nil
	}
	max := args.MaxResults
	if max <= 0 {
		max = defaultMaxResults
	}
	return textResult(formatInlayHints(hints, ft.abs, max)), nil
}

func (m *Manager) handleCodeActions(ctx context.Context, args toolArgs, apply bool) (*mcp.CallToolResult, error) {
	ft, errRes := m.targetFor(ctx, args.Path)
	if errRes != nil {
		return errRes, nil
	}
	uri, lines, err := m.prepareDoc(ft, uriForPath(ft.abs))
	if err != nil {
		return errorResult("open %s: %v", ft.abs, err), nil
	}
	rng, err := rangeFromArgs(lines, args.StartLine, args.EndLine)
	if err != nil {
		return errorResult("%v", err), nil
	}
	var only []string
	if strings.TrimSpace(args.Kind) != "" {
		only = []string{strings.TrimSpace(args.Kind)}
	}
	diagnostics := ft.cl.Diagnostics(uri)
	if args.TimeoutMS > 0 {
		fresh, _, waitErr := ft.cl.WaitDiagnostics(ctx, uri, time.Duration(args.TimeoutMS)*time.Millisecond)
		if waitErr != nil {
			return errorResult("code actions diagnostics wait failed: %v", waitErr), nil
		}
		diagnostics = fresh
	}
	actions, err := ft.cl.CodeActions(ctx, uri, rng, diagnosticsInRange(diagnostics, rng), only)
	if err != nil {
		return errorResult("code actions failed: %v", err), nil
	}
	if !apply {
		return textResult(formatCodeActions(actions)), nil
	}
	if strings.TrimSpace(args.Title) == "" {
		return errorResult("title is required"), nil
	}
	var matches []CodeAction
	for _, action := range actions {
		if action.Title == args.Title {
			matches = append(matches, action)
		}
	}
	if len(matches) == 0 {
		return errorResult("code action %q was not offered", args.Title), nil
	}
	if len(matches) > 1 {
		return errorResult("code action title %q is ambiguous (%d matches); narrow with kind", args.Title, len(matches)), nil
	}
	action := matches[0]
	if len(action.Edit) == 0 && len(action.Data) > 0 {
		action, err = ft.cl.ResolveCodeAction(ctx, action)
		if err != nil {
			return errorResult("resolve code action failed: %v", err), nil
		}
	}
	if action.Disabled != nil {
		return errorResult("code action %q is disabled: %s", action.Title, action.Disabled.Reason), nil
	}
	if action.CommandOnly || (len(action.Command) > 0 && string(action.Command) != "null") {
		return errorResult("code action %q includes a server command; harness does not execute language-server commands", action.Title), nil
	}
	if len(action.Edit) == 0 || string(action.Edit) == "null" {
		return errorResult("code action %q supplied no text edits", action.Title), nil
	}
	edits, err := parseWorkspaceEditStrict(action.Edit)
	if err != nil {
		return errorResult("code action failed: %v", err), nil
	}
	result, err := applyWorkspaceTextEdits(edits)
	if err != nil {
		return errorResult("code action failed: %v", err), nil
	}
	return textResult(formatEditsApplied("code action "+strconv.Quote(action.Title), result)), nil
}

func (m *Manager) handleFormatDocument(ctx context.Context, args toolArgs, apply bool) (*mcp.CallToolResult, error) {
	ft, errRes := m.targetFor(ctx, args.Path)
	if errRes != nil {
		return errRes, nil
	}
	uri, lines, err := m.prepareDoc(ft, uriForPath(ft.abs))
	if err != nil {
		return errorResult("open %s: %v", ft.abs, err), nil
	}
	var rng *Range
	if args.StartLine > 0 || args.EndLine > 0 {
		r, err := rangeFromArgs(lines, args.StartLine, args.EndLine)
		if err != nil {
			return errorResult("%v", err), nil
		}
		rng = &r
	}
	tabSize := args.TabSize
	if tabSize <= 0 {
		tabSize = 4
	}
	insertSpaces := true
	if args.InsertSpaces != nil {
		insertSpaces = *args.InsertSpaces
	}
	edits, err := ft.cl.Formatting(ctx, uri, rng, FormattingOptions{TabSize: tabSize, InsertSpaces: insertSpaces})
	if err != nil {
		return errorResult("formatting failed: %v", err), nil
	}
	fileEdits := []FileEdits{{URI: uri, Edits: edits}}
	if !apply {
		lr := newLineReader()
		return textResult(formatEditPlan(fileEdits, func(u string, line int) (string, bool) {
			return lr.line(uriToPath(u), line)
		}, "formatting")), nil
	}
	result, err := applyWorkspaceTextEdits(fileEdits)
	if err != nil {
		return errorResult("formatting failed: %v", err), nil
	}
	return textResult(formatEditsApplied("formatting", result)), nil
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

func (m *Manager) handleRename(ctx context.Context, args toolArgs) (*mcp.CallToolResult, error) {
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
	edits, err := ft.cl.RenameTextEdits(ctx, uri, pos, args.NewName)
	if err != nil {
		return errorResult("rename failed: %v", err), nil
	}
	result, err := applyWorkspaceTextEdits(edits)
	if err != nil {
		return errorResult("rename failed: %v", err), nil
	}
	return textResult(formatRenameApplied(result)), nil
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

// prewarmScanMaxEntries caps the evidence-only workspace scan that decides
// whether a language server is worth prewarming. An inconclusive scan (cap
// reached without a match) skips prewarm; lazy startup remains the fallback.
const prewarmScanMaxEntries = 20000

// Prewarm background-launches language servers for which there is evidence of
// their language files in the workspace root containing the process cwd. For
// each installed configured server it detects the root from the server's root
// markers and scans (bounded) for a matching file extension before acquiring a
// client. Servers with undetectable languages, no detected root, or no file
// evidence are skipped. Failures are logged only. It returns the number of
// servers successfully warmed.
func (m *Manager) Prewarm(ctx context.Context) int {
	cwd, err := os.Getwd()
	if err != nil {
		return 0
	}
	warmed := 0
	for _, s := range m.cfg.Servers {
		m.mu.Lock()
		present := m.present[s.Name]
		m.mu.Unlock()
		if !present {
			continue
		}
		exts := serverExtensions(s)
		if len(exts) == 0 {
			m.logger.Debug("lsp prewarm: skipping server with undetectable languages",
				slog.String("server", s.Name))
			continue
		}
		root, found := detectRoot(cwd, s.RootMarkers)
		if !found {
			continue
		}
		if !hasLanguageFiles(root, exts, prewarmScanMaxEntries) {
			m.logger.Debug("lsp prewarm: no language-file evidence (or scan cap reached); skipping",
				slog.String("server", s.Name), slog.String("root", root))
			continue
		}
		if _, err := m.acquire(ctx, s, root); err != nil {
			m.logger.Warn("lsp prewarm: failed to warm server",
				slog.String("server", s.Name), slog.String("root", root), slog.Any("error", err))
			continue
		}
		warmed++
	}
	return warmed
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

// rangeFromArgs converts optional 1-based inclusive line bounds to an LSP
// range. No bounds means the whole document; one bound means that one line.
func rangeFromArgs(lines []string, startLine, endLine int) (Range, error) {
	if len(lines) == 0 {
		return Range{}, fmt.Errorf("document has no lines")
	}
	if startLine == 0 && endLine == 0 {
		return Range{
			Start: Position{},
			End:   Position{Line: len(lines) - 1, Character: utf16Len(lines[len(lines)-1])},
		}, nil
	}
	if startLine == 0 {
		startLine = endLine
	}
	if endLine == 0 {
		endLine = startLine
	}
	if startLine < 1 || startLine > len(lines) {
		return Range{}, fmt.Errorf("start_line %d is out of range (file has %d lines)", startLine, len(lines))
	}
	if endLine < startLine || endLine > len(lines) {
		return Range{}, fmt.Errorf("end_line %d is out of range or before start_line %d", endLine, startLine)
	}
	return Range{
		Start: Position{Line: startLine - 1},
		End:   Position{Line: endLine - 1, Character: utf16Len(lines[endLine-1])},
	}, nil
}

func diagnosticsInRange(diags []Diagnostic, r Range) []Diagnostic {
	out := make([]Diagnostic, 0, len(diags))
	for _, d := range diags {
		if d.Range.End.Line < r.Start.Line || d.Range.Start.Line > r.End.Line {
			continue
		}
		out = append(out, d)
	}
	return out
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
