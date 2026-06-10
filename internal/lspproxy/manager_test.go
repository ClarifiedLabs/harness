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

func TestManagerListToolsHasSevenReadOnly(t *testing.T) {
	m := NewManager(goConfig(), "lsp", nil)
	res, err := m.ListTools(context.Background(), "")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(res.Tools) != 7 {
		t.Fatalf("tool count = %d, want 7", len(res.Tools))
	}
	names := map[string]bool{}
	for _, tl := range res.Tools {
		names[tl.Name] = true
		if !strings.Contains(string(tl.Annotations), "readOnlyHint") {
			t.Fatalf("tool %s missing readOnlyHint annotation", tl.Name)
		}
	}
	for _, want := range []string{"mcp__lsp__definition", "mcp__lsp__references", "mcp__lsp__hover", "mcp__lsp__document_symbols", "mcp__lsp__workspace_symbols", "mcp__lsp__diagnostics", "mcp__lsp__rename_plan"} {
		if !names[want] {
			t.Fatalf("missing tool %s", want)
		}
	}
}

func TestManagerListToolsAdvertisesAvailableLanguages(t *testing.T) {
	m := NewManager(goConfig(), "lsp", nil)
	// Pretend the gopls binary is present.
	m.lookPath = func(string) (string, error) { return "/usr/bin/gopls", nil }
	m.computeAvailable()

	res, _ := m.ListTools(context.Background(), "")
	if !strings.Contains(res.Tools[0].Description, "go") {
		t.Fatalf("description does not advertise available language: %q", res.Tools[0].Description)
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
