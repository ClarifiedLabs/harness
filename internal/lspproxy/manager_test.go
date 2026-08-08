package lspproxy

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"harness/internal/mcp/jsonrpc"
)

func goConfig() Config {
	return Config{Servers: []ResolvedServer{{
		Name:        "gopls",
		Languages:   []string{"go"},
		RootMarkers: []string{".git"},
		Command:     []string{"gopls"},
	}}}
}

func TestManagerListToolsHasExpectedAnnotations(t *testing.T) {
	m := NewManager(goConfig(), "lsp", nil)
	res, err := m.ListTools(context.Background(), "")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(res.Tools) != 21 {
		t.Fatalf("tool count = %d, want 21", len(res.Tools))
	}
	names := map[string]bool{}
	mutating := map[string]bool{
		"mcp__lsp__code_action":     true,
		"mcp__lsp__format_document": true,
		"mcp__lsp__rename":          true,
	}
	for _, tl := range res.Tools {
		names[tl.Name] = true
		if !json.Valid(tl.InputSchema) {
			t.Fatalf("tool %s has invalid schema: %s", tl.Name, tl.InputSchema)
		}
		if mutating[tl.Name] {
			if !strings.Contains(string(tl.Annotations), `"readOnlyHint":false`) {
				t.Fatalf("tool %s should be mutating, annotations=%s", tl.Name, tl.Annotations)
			}
			continue
		}
		if !strings.Contains(string(tl.Annotations), `"readOnlyHint":true`) {
			t.Fatalf("tool %s missing readOnlyHint=true annotation: %s", tl.Name, tl.Annotations)
		}
	}
	for _, want := range []string{
		"mcp__lsp__declaration", "mcp__lsp__definition", "mcp__lsp__type_definition", "mcp__lsp__implementation",
		"mcp__lsp__references", "mcp__lsp__hover", "mcp__lsp__signature_help", "mcp__lsp__completion",
		"mcp__lsp__document_highlights", "mcp__lsp__document_symbols", "mcp__lsp__workspace_symbols", "mcp__lsp__diagnostics",
		"mcp__lsp__call_hierarchy", "mcp__lsp__type_hierarchy", "mcp__lsp__inlay_hints", "mcp__lsp__code_actions",
		"mcp__lsp__code_action", "mcp__lsp__format_document_plan", "mcp__lsp__format_document", "mcp__lsp__rename_plan", "mcp__lsp__rename",
	} {
		if !names[want] {
			t.Fatalf("missing tool %s", want)
		}
	}
}

func TestManagerReportsAvailableLanguagesOnce(t *testing.T) {
	m := NewManager(goConfig(), "lsp", nil)
	// Pretend the gopls binary is present.
	m.lookPath = func(string) (string, error) { return "/usr/bin/gopls", nil }
	m.computeAvailable()

	if got := m.AvailableLanguages(); len(got) != 1 || got[0] != "go" {
		t.Fatalf("AvailableLanguages() = %v, want [go]", got)
	}
	res, _ := m.ListTools(context.Background(), "")
	if strings.Contains(res.Tools[0].Description, "Langs:") {
		t.Fatalf("per-tool descriptions should not repeat languages: %q", res.Tools[0].Description)
	}
}

func TestManagerStatusDistinguishesAvailableAndLoadedLanguages(t *testing.T) {
	m := NewManager(goConfig(), "lsp", nil)
	m.lookPath = func(string) (string, error) { return "/usr/bin/gopls", nil }
	m.RefreshAvailability()

	status := m.ServerStatuses()
	if len(status) != 1 || !status[0].Available || len(status[0].LoadedRoots) != 0 {
		t.Fatalf("status before load = %+v", status)
	}
	if got := m.LoadedLanguages(); len(got) != 0 {
		t.Fatalf("loaded languages before server acquisition = %v", got)
	}

	cl, _ := didOpenClient(t, "/tmp/project")
	inst := newServerInstance(goConfig().Servers[0], "/tmp/project", nil)
	inst.client = cl
	m.instances[instanceKey("gopls", "/tmp/project")] = inst

	status = m.ServerStatuses()
	if len(status[0].LoadedRoots) != 1 || status[0].LoadedRoots[0] != "/tmp/project" {
		t.Fatalf("status after load = %+v", status)
	}
	if got := m.LoadedLanguages(); len(got) != 1 || got[0] != "go" {
		t.Fatalf("loaded languages = %v, want [go]", got)
	}
}

func TestManagerDefinition(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "a.go")
	if err := os.WriteFile(src, []byte("package main\n\nfunc Foo() {}\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	m := NewManager(goConfig(), "lsp", nil)
	m.acquireFn = func(ctx context.Context, s ResolvedServer, root string) (*lspClient, error) {
		conn, _ := fakeLSP(t, func(server **jsonrpc.Peer) jsonrpc.PeerOptions {
			return jsonrpc.PeerOptions{
				Handlers: map[string]jsonrpc.Handler{
					"initialize": initOK,
					"textDocument/definition": func(ctx context.Context, p json.RawMessage) (json.RawMessage, *jsonrpc.Error) {
						return json.RawMessage(`{"uri":"` + uriForPath(src) + `","range":{"start":{"line":2,"character":5},"end":{"line":2,"character":8}}}`), nil
					},
				},
				Notifications: map[string]jsonrpc.NotificationHandler{
					"initialized":          func(ctx context.Context, p json.RawMessage) {},
					"textDocument/didOpen": func(ctx context.Context, p json.RawMessage) {},
				},
			}
		})
		cl := newClient(conn, root, nil)
		if _, err := cl.Initialize(ctx, nil); err != nil {
			return nil, err
		}
		return cl, nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	args := json.RawMessage(`{"path":"` + src + `","line":3,"symbol":"Foo"}`)
	res, err := m.CallTool(ctx, "mcp__lsp__definition", args)
	if err != nil {
		t.Fatalf("callTool: %v", err)
	}
	if res.IsError {
		t.Fatalf("unexpected error result: %s", res.Content[0].Text)
	}
	text := res.Content[0].Text
	if !strings.Contains(text, "a.go:3:6") || !strings.Contains(text, "func Foo() {}") {
		t.Fatalf("definition result = %q", text)
	}
}

func TestManagerRenameAppliesEdits(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "a.go")
	if err := os.WriteFile(src, []byte("package main\n\nfunc Foo() {}\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	m := NewManager(goConfig(), "lsp", nil)
	m.acquireFn = func(ctx context.Context, s ResolvedServer, root string) (*lspClient, error) {
		conn, _ := fakeLSP(t, func(server **jsonrpc.Peer) jsonrpc.PeerOptions {
			return jsonrpc.PeerOptions{
				Handlers: map[string]jsonrpc.Handler{
					"initialize": initOK,
					"textDocument/rename": func(ctx context.Context, p json.RawMessage) (json.RawMessage, *jsonrpc.Error) {
						return json.RawMessage(`{"changes":{"` + uriForPath(src) + `":[{"range":{"start":{"line":2,"character":5},"end":{"line":2,"character":8}},"newText":"Bar"}]}}`), nil
					},
				},
				Notifications: map[string]jsonrpc.NotificationHandler{
					"initialized":          func(ctx context.Context, p json.RawMessage) {},
					"textDocument/didOpen": func(ctx context.Context, p json.RawMessage) {},
				},
			}
		})
		cl := newClient(conn, root, nil)
		if _, err := cl.Initialize(ctx, nil); err != nil {
			return nil, err
		}
		return cl, nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	args := json.RawMessage(`{"path":"` + src + `","line":3,"symbol":"Foo","new_name":"Bar"}`)
	res, err := m.CallTool(ctx, "mcp__lsp__rename", args)
	if err != nil {
		t.Fatalf("callTool: %v", err)
	}
	if res.IsError {
		t.Fatalf("unexpected error result: %s", res.Content[0].Text)
	}
	if text := res.Content[0].Text; !strings.Contains(text, "applied 1 edit(s)") || !strings.Contains(text, src) {
		t.Fatalf("rename result = %q", text)
	}
	if got := string(mustReadTestFile(t, src)); got != "package main\n\nfunc Bar() {}\n" {
		t.Fatalf("file content = %q", got)
	}
}

func TestManagerCodeActionAppliesTextEdits(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "a.go")
	if err := os.WriteFile(src, []byte("package main\nvar x = bad\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	m := NewManager(goConfig(), "lsp", nil)
	m.acquireFn = func(ctx context.Context, s ResolvedServer, root string) (*lspClient, error) {
		conn, _ := fakeLSP(t, func(server **jsonrpc.Peer) jsonrpc.PeerOptions {
			return jsonrpc.PeerOptions{
				Handlers: map[string]jsonrpc.Handler{
					"initialize":              initOK,
					"textDocument/codeAction": rawHandler(`[{"title":"Replace bad","kind":"quickfix","edit":{"changes":{"` + uriForPath(src) + `":[{"range":{"start":{"line":1,"character":8},"end":{"line":1,"character":11}},"newText":"good"}]}}}]`),
				},
				Notifications: map[string]jsonrpc.NotificationHandler{
					"initialized":          func(context.Context, json.RawMessage) {},
					"textDocument/didOpen": func(context.Context, json.RawMessage) {},
				},
			}
		})
		cl := newClient(conn, root, nil)
		if _, err := cl.Initialize(ctx, nil); err != nil {
			return nil, err
		}
		return cl, nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	res, err := m.CallTool(ctx, "mcp__lsp__code_action", json.RawMessage(`{"path":"`+src+`","title":"Replace bad","start_line":2}`))
	if err != nil || res.IsError {
		t.Fatalf("code action result = %+v, err %v", res, err)
	}
	data, err := os.ReadFile(src)
	if err != nil {
		t.Fatal(err)
	}
	if got := string(data); got != "package main\nvar x = good\n" {
		t.Fatalf("file after code action = %q", got)
	}
}

func TestManagerNoServerForExtension(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "a.unknownext")
	if err := os.WriteFile(src, []byte("x\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	m := NewManager(goConfig(), "lsp", nil)
	res, err := m.CallTool(context.Background(), "mcp__lsp__definition", json.RawMessage(`{"path":"`+src+`","line":1,"symbol":"x"}`))
	if err != nil {
		t.Fatalf("callTool: %v", err)
	}
	if !res.IsError || !strings.Contains(res.Content[0].Text, "no language server") {
		t.Fatalf("expected no-server error result, got %+v", res)
	}
}

func mustReadTestFile(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func TestPrepareDocReopensAfterClientRestart(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "a.go")
	if err := os.WriteFile(src, []byte("package main\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	m := NewManager(goConfig(), "lsp", nil)
	cl1, opened1 := didOpenClient(t, dir)
	ft := &fileTarget{abs: src, lang: "go", instKey: instanceKey("gopls", dir), cl: cl1}
	if _, _, err := m.prepareDoc(ft, uriForPath(src)); err != nil {
		t.Fatalf("first prepareDoc: %v", err)
	}
	waitDidOpen(t, opened1)

	cl2, opened2 := didOpenClient(t, dir)
	ft.cl = cl2
	if _, _, err := m.prepareDoc(ft, uriForPath(src)); err != nil {
		t.Fatalf("second prepareDoc: %v", err)
	}
	waitDidOpen(t, opened2)
}

func TestPrepareDocDoesNotRecordFailedDidOpen(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "a.go")
	if err := os.WriteFile(src, []byte("package main\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	m := NewManager(goConfig(), "lsp", nil)
	cl, _ := didOpenClient(t, dir)
	_ = cl.Close()
	ft := &fileTarget{abs: src, lang: "go", instKey: instanceKey("gopls", dir), cl: cl}
	if _, _, err := m.prepareDoc(ft, uriForPath(src)); err == nil {
		t.Fatal("prepareDoc should fail when didOpen cannot be sent")
	}
	if len(m.docs) != 0 {
		t.Fatalf("docs state recorded after failed didOpen: %+v", m.docs)
	}
}

func didOpenClient(t *testing.T, root string) (*lspClient, <-chan json.RawMessage) {
	t.Helper()
	opened := make(chan json.RawMessage, 1)
	conn, _ := fakeLSP(t, func(server **jsonrpc.Peer) jsonrpc.PeerOptions {
		return jsonrpc.PeerOptions{
			Notifications: map[string]jsonrpc.NotificationHandler{
				"textDocument/didOpen": func(ctx context.Context, p json.RawMessage) { opened <- p },
			},
		}
	})
	cl := newClient(conn, root, nil)
	t.Cleanup(func() { _ = cl.Close() })
	return cl, opened
}

func waitDidOpen(t *testing.T, opened <-chan json.RawMessage) {
	t.Helper()
	select {
	case <-opened:
	case <-time.After(2 * time.Second):
		t.Fatal("server never received didOpen")
	}
}

func TestManagerUnknownToolIsProtocolError(t *testing.T) {
	m := NewManager(goConfig(), "lsp", nil)
	if _, err := m.CallTool(context.Background(), "mcp__lsp__nope", json.RawMessage(`{}`)); err == nil {
		t.Fatal("expected error for unknown tool")
	}
}

func TestSelectServer(t *testing.T) {
	m := NewManager(goConfig(), "lsp", nil)
	s, lang, ok := m.selectServer("/x/y/a.go")
	if !ok || s.Name != "gopls" || lang != "go" {
		t.Fatalf("selectServer(go) = %+v %q %v", s, lang, ok)
	}
	if _, _, ok := m.selectServer("/x/y/a.rb"); ok {
		t.Fatal("selectServer(rb) should be not found")
	}
}

func TestResolveCharacter(t *testing.T) {
	if got, err := resolveCharacter("func Foo() {", "Foo", 0); err != nil || got != 5 {
		t.Fatalf("symbol: got %d err %v, want 5", got, err)
	}
	if got, err := resolveCharacter("abcd", "", 3); err != nil || got != 2 {
		t.Fatalf("column: got %d err %v, want 2", got, err)
	}
	if got, err := resolveCharacter("foo(foo)", "foo", 5); err != nil || got != 4 {
		t.Fatalf("column override: got %d err %v, want 4", got, err)
	}
	if _, err := resolveCharacter("func Foo()", "Bar", 0); err == nil {
		t.Fatal("missing symbol should error")
	}
}

func TestPrewarmAcquiresServersWithFileEvidence(t *testing.T) {
	root := t.TempDir()
	t.Chdir(root)
	if err := os.MkdirAll(filepath.Join(root, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	mustWrite(t, filepath.Join(root, "main.go"))

	cfg := Config{Servers: []ResolvedServer{{
		Name:        "gopls",
		Languages:   []string{"go"},
		RootMarkers: []string{".git"},
		Command:     []string{"gopls"},
	}}}
	m := NewManager(cfg, "lsp", nil)
	m.lookPath = func(string) (string, error) { return "/usr/bin/gopls", nil }
	m.computeAvailable()

	type acquired struct {
		server string
		root   string
	}
	var calls []acquired
	m.acquireFn = func(ctx context.Context, s ResolvedServer, r string) (*lspClient, error) {
		calls = append(calls, acquired{server: s.Name, root: r})
		return nil, nil
	}

	if got := m.Prewarm(context.Background()); got != 1 {
		t.Fatalf("Prewarm = %d, want 1", got)
	}
	if len(calls) != 1 || calls[0].server != "gopls" || calls[0].root != root {
		t.Fatalf("acquire calls = %+v, want one for (gopls, %s)", calls, root)
	}
}

func TestPrewarmSkipsWithoutEvidence(t *testing.T) {
	root := t.TempDir()
	t.Chdir(root)
	if err := os.MkdirAll(filepath.Join(root, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	// No .go files: the marker is present but there is no language evidence.
	mustWrite(t, filepath.Join(root, "README.md"))

	m := NewManager(goConfig(), "lsp", nil)
	m.lookPath = func(string) (string, error) { return "/usr/bin/gopls", nil }
	m.computeAvailable()
	m.acquireFn = func(ctx context.Context, s ResolvedServer, r string) (*lspClient, error) {
		t.Fatal("acquire must not be called without file evidence")
		return nil, nil
	}
	if got := m.Prewarm(context.Background()); got != 0 {
		t.Fatalf("Prewarm = %d, want 0", got)
	}
}

func TestPrewarmSkipsAbsentBinary(t *testing.T) {
	root := t.TempDir()
	t.Chdir(root)
	if err := os.MkdirAll(filepath.Join(root, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	mustWrite(t, filepath.Join(root, "main.go"))

	m := NewManager(goConfig(), "lsp", nil)
	m.lookPath = func(string) (string, error) { return "", os.ErrNotExist }
	m.computeAvailable()
	m.acquireFn = func(ctx context.Context, s ResolvedServer, r string) (*lspClient, error) {
		t.Fatal("acquire must not be called for an absent binary")
		return nil, nil
	}
	if got := m.Prewarm(context.Background()); got != 0 {
		t.Fatalf("Prewarm = %d, want 0", got)
	}
}

func TestPrewarmSkipsWithoutRootMarker(t *testing.T) {
	root := t.TempDir()
	t.Chdir(root)
	// No .git marker anywhere above cwd (t.TempDir roots are marker-free).
	mustWrite(t, filepath.Join(root, "main.go"))

	cfg := Config{Servers: []ResolvedServer{{
		Name:        "gopls",
		Languages:   []string{"go"},
		RootMarkers: []string{"definitely-not-present.marker"},
		Command:     []string{"gopls"},
	}}}
	m := NewManager(cfg, "lsp", nil)
	m.lookPath = func(string) (string, error) { return "/usr/bin/gopls", nil }
	m.computeAvailable()
	m.acquireFn = func(ctx context.Context, s ResolvedServer, r string) (*lspClient, error) {
		t.Fatal("acquire must not be called without a detected root")
		return nil, nil
	}
	if got := m.Prewarm(context.Background()); got != 0 {
		t.Fatalf("Prewarm = %d, want 0", got)
	}
}

func TestPrewarmOnlyWarmsServersWithEvidence(t *testing.T) {
	// A repo containing only Go files must prewarm gopls and no other
	// installed server.
	root := t.TempDir()
	t.Chdir(root)
	if err := os.MkdirAll(filepath.Join(root, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	mustWrite(t, filepath.Join(root, "main.go"))

	cfg := Config{Servers: []ResolvedServer{
		{Name: "gopls", Languages: []string{"go"}, RootMarkers: []string{".git"}, Command: []string{"gopls"}},
		{Name: "rust-analyzer", Languages: []string{"rust"}, RootMarkers: []string{".git"}, Command: []string{"rust-analyzer"}},
		{Name: "pyright", Languages: []string{"python"}, RootMarkers: []string{".git"}, Command: []string{"pyright"}},
	}}
	m := NewManager(cfg, "lsp", nil)
	m.lookPath = func(string) (string, error) { return "/usr/bin/installed", nil }
	m.computeAvailable()

	var warmed []string
	m.acquireFn = func(ctx context.Context, s ResolvedServer, r string) (*lspClient, error) {
		warmed = append(warmed, s.Name)
		return nil, nil
	}
	if got := m.Prewarm(context.Background()); got != 1 {
		t.Fatalf("Prewarm = %d, want 1", got)
	}
	if len(warmed) != 1 || warmed[0] != "gopls" {
		t.Fatalf("warmed = %v, want [gopls]", warmed)
	}
}

func TestPrewarmUndetectableLanguagesSkipped(t *testing.T) {
	root := t.TempDir()
	t.Chdir(root)
	if err := os.MkdirAll(filepath.Join(root, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	mustWrite(t, filepath.Join(root, "main.go"))

	cfg := Config{Servers: []ResolvedServer{{
		Name:        "mystery",
		Languages:   []string{"cobol"},
		RootMarkers: []string{".git"},
		Command:     []string{"mystery-lsp"},
		// No configured Extensions: cobol has no built-in extension mapping.
	}}}
	m := NewManager(cfg, "lsp", nil)
	m.lookPath = func(string) (string, error) { return "/usr/bin/mystery-lsp", nil }
	m.computeAvailable()
	m.acquireFn = func(ctx context.Context, s ResolvedServer, r string) (*lspClient, error) {
		t.Fatal("acquire must not be called for undetectable languages")
		return nil, nil
	}
	if got := m.Prewarm(context.Background()); got != 0 {
		t.Fatalf("Prewarm = %d, want 0", got)
	}
}
