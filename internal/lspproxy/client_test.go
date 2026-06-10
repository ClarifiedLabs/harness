package lspproxy

import (
	"context"
	"encoding/json"
	"net"
	"testing"
	"time"

	"harness/internal/mcp/jsonrpc"
)

// fakeLSP wires a scriptable LSP server to a client over an in-memory pipe,
// speaking the real Content-Length framing. The returned conn is the client end;
// server is the fake, already serving. Handlers/Notifications come from opts; the
// fake can push server→client notifications via the returned *jsonrpc.Peer.
func fakeLSP(t *testing.T, build func(server **jsonrpc.Peer) jsonrpc.PeerOptions) (net.Conn, *jsonrpc.Peer) {
	t.Helper()
	c1, c2 := net.Pipe()
	var server *jsonrpc.Peer
	opts := build(&server)
	server = jsonrpc.NewPeerWithCodec(c2, NewDecoder(c2), NewEncoder(c2), opts)
	t.Cleanup(func() { _ = server.Close() })
	return c1, server
}

func TestClientInitializeHandshake(t *testing.T) {
	gotInitialized := make(chan struct{}, 1)
	conn, _ := fakeLSP(t, func(server **jsonrpc.Peer) jsonrpc.PeerOptions {
		return jsonrpc.PeerOptions{
			Handlers: map[string]jsonrpc.Handler{
				"initialize": func(ctx context.Context, params json.RawMessage) (json.RawMessage, *jsonrpc.Error) {
					return json.RawMessage(`{"capabilities":{"positionEncoding":"utf-16"},"serverInfo":{"name":"fake"}}`), nil
				},
			},
			Notifications: map[string]jsonrpc.NotificationHandler{
				"initialized": func(ctx context.Context, params json.RawMessage) { gotInitialized <- struct{}{} },
			},
		}
	})

	cl := newClient(conn, "/tmp/proj", nil)
	t.Cleanup(func() { _ = cl.Close() })

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	res, err := cl.Initialize(ctx, nil)
	if err != nil {
		t.Fatalf("initialize: %v", err)
	}
	if res.Capabilities.PositionEncoding != "utf-16" {
		t.Fatalf("position encoding = %q, want utf-16", res.Capabilities.PositionEncoding)
	}
	select {
	case <-gotInitialized:
	case <-time.After(2 * time.Second):
		t.Fatal("server never received initialized notification")
	}
	if cl.PositionEncoding() != "utf-16" {
		t.Fatalf("client recorded encoding = %q, want utf-16", cl.PositionEncoding())
	}
}

// initOK answers initialize with empty capabilities.
func initOK(ctx context.Context, params json.RawMessage) (json.RawMessage, *jsonrpc.Error) {
	return json.RawMessage(`{"capabilities":{}}`), nil
}

func initClient(t *testing.T, conn net.Conn) *lspClient {
	t.Helper()
	cl := newClient(conn, "/tmp/proj", nil)
	t.Cleanup(func() { _ = cl.Close() })
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if _, err := cl.Initialize(ctx, nil); err != nil {
		t.Fatalf("initialize: %v", err)
	}
	return cl
}

func TestClientDidOpenAndDefinition(t *testing.T) {
	gotDidOpen := make(chan json.RawMessage, 1)
	conn, _ := fakeLSP(t, func(server **jsonrpc.Peer) jsonrpc.PeerOptions {
		return jsonrpc.PeerOptions{
			Handlers: map[string]jsonrpc.Handler{
				"initialize": initOK,
				"textDocument/definition": func(ctx context.Context, params json.RawMessage) (json.RawMessage, *jsonrpc.Error) {
					return json.RawMessage(`{"uri":"file:///tmp/proj/b.go","range":{"start":{"line":9,"character":5},"end":{"line":9,"character":8}}}`), nil
				},
			},
			Notifications: map[string]jsonrpc.NotificationHandler{
				"initialized":          func(ctx context.Context, p json.RawMessage) {},
				"textDocument/didOpen": func(ctx context.Context, p json.RawMessage) { gotDidOpen <- p },
			},
		}
	})
	cl := initClient(t, conn)

	if err := cl.DidOpen("file:///tmp/proj/a.go", "go", 1, "package main\n"); err != nil {
		t.Fatalf("didOpen: %v", err)
	}
	select {
	case p := <-gotDidOpen:
		var got DidOpenParams
		if err := json.Unmarshal(p, &got); err != nil {
			t.Fatalf("decode didOpen: %v", err)
		}
		if got.TextDocument.URI != "file:///tmp/proj/a.go" || got.TextDocument.LanguageID != "go" || got.TextDocument.Text != "package main\n" {
			t.Fatalf("didOpen params = %+v", got.TextDocument)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("server never received didOpen")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	locs, err := cl.Definition(ctx, "file:///tmp/proj/a.go", Position{Line: 0, Character: 8})
	if err != nil {
		t.Fatalf("definition: %v", err)
	}
	if len(locs) != 1 || locs[0].URI != "file:///tmp/proj/b.go" || locs[0].Range.Start.Line != 9 {
		t.Fatalf("definition = %+v", locs)
	}
}

func TestParseLocations(t *testing.T) {
	single := json.RawMessage(`{"uri":"file:///a","range":{"start":{"line":1,"character":0},"end":{"line":1,"character":3}}}`)
	if locs, _ := parseLocations(single); len(locs) != 1 || locs[0].URI != "file:///a" {
		t.Fatalf("single: %+v", locs)
	}
	array := json.RawMessage(`[{"uri":"file:///a","range":{"start":{"line":0,"character":0},"end":{"line":0,"character":1}}},{"uri":"file:///b","range":{"start":{"line":2,"character":0},"end":{"line":2,"character":1}}}]`)
	if locs, _ := parseLocations(array); len(locs) != 2 || locs[1].URI != "file:///b" {
		t.Fatalf("array: %+v", locs)
	}
	links := json.RawMessage(`[{"targetUri":"file:///c","targetRange":{"start":{"line":4,"character":0},"end":{"line":4,"character":2}},"targetSelectionRange":{"start":{"line":4,"character":0},"end":{"line":4,"character":2}}}]`)
	if locs, _ := parseLocations(links); len(locs) != 1 || locs[0].URI != "file:///c" || locs[0].Range.Start.Line != 4 {
		t.Fatalf("links: %+v", locs)
	}
	if locs, _ := parseLocations(json.RawMessage(`null`)); len(locs) != 0 {
		t.Fatalf("null: %+v", locs)
	}
}

func TestClientReferencesHoverSymbols(t *testing.T) {
	conn, _ := fakeLSP(t, func(server **jsonrpc.Peer) jsonrpc.PeerOptions {
		return jsonrpc.PeerOptions{
			Handlers: map[string]jsonrpc.Handler{
				"initialize": initOK,
				"textDocument/references": func(ctx context.Context, p json.RawMessage) (json.RawMessage, *jsonrpc.Error) {
					return json.RawMessage(`[{"uri":"file:///a","range":{"start":{"line":1,"character":0},"end":{"line":1,"character":3}}},{"uri":"file:///b","range":{"start":{"line":5,"character":2},"end":{"line":5,"character":5}}}]`), nil
				},
				"textDocument/hover": func(ctx context.Context, p json.RawMessage) (json.RawMessage, *jsonrpc.Error) {
					return json.RawMessage(`{"contents":{"kind":"markdown","value":"func Foo() int"}}`), nil
				},
				"textDocument/documentSymbol": func(ctx context.Context, p json.RawMessage) (json.RawMessage, *jsonrpc.Error) {
					return json.RawMessage(`[{"name":"Foo","kind":12,"range":{"start":{"line":3,"character":0},"end":{"line":8,"character":0}},"selectionRange":{"start":{"line":3,"character":5},"end":{"line":3,"character":8}},"children":[{"name":"x","kind":13,"range":{"start":{"line":4,"character":1},"end":{"line":4,"character":2}},"selectionRange":{"start":{"line":4,"character":1},"end":{"line":4,"character":2}}}]}]`), nil
				},
				"workspace/symbol": func(ctx context.Context, p json.RawMessage) (json.RawMessage, *jsonrpc.Error) {
					return json.RawMessage(`[{"name":"Bar","kind":12,"location":{"uri":"file:///tmp/proj/c.go","range":{"start":{"line":7,"character":0},"end":{"line":7,"character":3}}}}]`), nil
				},
			},
			Notifications: map[string]jsonrpc.NotificationHandler{"initialized": func(ctx context.Context, p json.RawMessage) {}},
		}
	})
	cl := initClient(t, conn)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	refs, err := cl.References(ctx, "file:///tmp/proj/a.go", Position{Line: 1, Character: 0}, true)
	if err != nil || len(refs) != 2 || refs[1].URI != "file:///b" {
		t.Fatalf("references = %+v, err %v", refs, err)
	}

	hover, err := cl.Hover(ctx, "file:///tmp/proj/a.go", Position{Line: 1, Character: 0})
	if err != nil || hover != "func Foo() int" {
		t.Fatalf("hover = %q, err %v", hover, err)
	}

	syms, err := cl.DocumentSymbols(ctx, "file:///tmp/proj/a.go")
	if err != nil || len(syms) != 1 || syms[0].Name != "Foo" || syms[0].Line != 3 || len(syms[0].Children) != 1 || syms[0].Children[0].Name != "x" {
		t.Fatalf("documentSymbols = %+v, err %v", syms, err)
	}

	wsyms, err := cl.WorkspaceSymbols(ctx, "Bar")
	if err != nil || len(wsyms) != 1 || wsyms[0].Name != "Bar" || wsyms[0].URI != "file:///tmp/proj/c.go" || wsyms[0].Line != 7 {
		t.Fatalf("workspaceSymbols = %+v, err %v", wsyms, err)
	}
}

func TestClientDiagnostics(t *testing.T) {
	conn, _ := fakeLSP(t, func(server **jsonrpc.Peer) jsonrpc.PeerOptions {
		return jsonrpc.PeerOptions{
			Handlers: map[string]jsonrpc.Handler{"initialize": initOK},
			Notifications: map[string]jsonrpc.NotificationHandler{
				"initialized": func(ctx context.Context, p json.RawMessage) {},
				"textDocument/didOpen": func(ctx context.Context, p json.RawMessage) {
					var op DidOpenParams
					_ = json.Unmarshal(p, &op)
					_ = (*server).Notify("textDocument/publishDiagnostics", json.RawMessage(`{"uri":"`+op.TextDocument.URI+`","version":1,"diagnostics":[{"range":{"start":{"line":2,"character":0},"end":{"line":2,"character":5}},"severity":1,"message":"undefined: x","source":"compiler"}]}`))
				},
			},
		}
	})
	cl := initClient(t, conn)

	cl.MarkDocPending("file:///tmp/proj/a.go")
	if err := cl.DidOpen("file:///tmp/proj/a.go", "go", 1, "package main\n"); err != nil {
		t.Fatalf("didOpen: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	diags, ok, err := cl.WaitDiagnostics(ctx, "file:///tmp/proj/a.go", 2*time.Second)
	if err != nil || !ok {
		t.Fatalf("waitDiagnostics ok=%v err=%v", ok, err)
	}
	if len(diags) != 1 || diags[0].Severity != 1 || diags[0].Message != "undefined: x" || diags[0].Range.Start.Line != 2 {
		t.Fatalf("diagnostics = %+v", diags)
	}
}

func TestClientDiagnosticsTimeout(t *testing.T) {
	conn, _ := fakeLSP(t, func(server **jsonrpc.Peer) jsonrpc.PeerOptions {
		return jsonrpc.PeerOptions{
			Handlers:      map[string]jsonrpc.Handler{"initialize": initOK},
			Notifications: map[string]jsonrpc.NotificationHandler{"initialized": func(ctx context.Context, p json.RawMessage) {}},
		}
	})
	cl := initClient(t, conn)
	cl.MarkDocPending("file:///tmp/proj/silent.go")

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_, ok, err := cl.WaitDiagnostics(ctx, "file:///tmp/proj/silent.go", 50*time.Millisecond)
	if err != nil {
		t.Fatalf("err = %v, want nil on timeout", err)
	}
	if ok {
		t.Fatal("ok = true, want false when no publish arrives before timeout")
	}
}

func TestClientAnswersConfiguration(t *testing.T) {
	replied := make(chan json.RawMessage, 1)
	conn, _ := fakeLSP(t, func(server **jsonrpc.Peer) jsonrpc.PeerOptions {
		return jsonrpc.PeerOptions{
			Handlers: map[string]jsonrpc.Handler{"initialize": initOK},
			Notifications: map[string]jsonrpc.NotificationHandler{
				"initialized": func(ctx context.Context, p json.RawMessage) {
					res, err := (*server).Call(context.Background(), "workspace/configuration", json.RawMessage(`{"items":[{"section":"gopls"},{"section":"foo"}]}`))
					if err == nil {
						replied <- res
					}
				},
			},
		}
	})
	cl := initClient(t, conn)
	_ = cl

	select {
	case res := <-replied:
		var arr []json.RawMessage
		if err := json.Unmarshal(res, &arr); err != nil || len(arr) != 2 {
			t.Fatalf("configuration reply = %s, want array of 2", res)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("server's workspace/configuration was never answered (would hang a real server)")
	}
}

func TestClientRename(t *testing.T) {
	conn, _ := fakeLSP(t, func(server **jsonrpc.Peer) jsonrpc.PeerOptions {
		return jsonrpc.PeerOptions{
			Handlers: map[string]jsonrpc.Handler{
				"initialize": initOK,
				"textDocument/rename": func(ctx context.Context, p json.RawMessage) (json.RawMessage, *jsonrpc.Error) {
					return json.RawMessage(`{"changes":{"file:///tmp/proj/a.go":[{"range":{"start":{"line":1,"character":5},"end":{"line":1,"character":8}},"newText":"Bar"}],"file:///tmp/proj/b.go":[{"range":{"start":{"line":3,"character":0},"end":{"line":3,"character":3}},"newText":"Bar"}]}}`), nil
				},
			},
			Notifications: map[string]jsonrpc.NotificationHandler{"initialized": func(ctx context.Context, p json.RawMessage) {}},
		}
	})
	cl := initClient(t, conn)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	edits, err := cl.Rename(ctx, "file:///tmp/proj/a.go", Position{Line: 1, Character: 5}, "Bar")
	if err != nil {
		t.Fatalf("rename: %v", err)
	}
	if len(edits) != 2 || edits[0].URI != "file:///tmp/proj/a.go" || edits[1].URI != "file:///tmp/proj/b.go" {
		t.Fatalf("rename edits = %+v", edits)
	}
	if len(edits[0].Edits) != 1 || edits[0].Edits[0].NewText != "Bar" {
		t.Fatalf("a.go edits = %+v", edits[0].Edits)
	}
}

func TestParseWorkspaceEditDocumentChanges(t *testing.T) {
	raw := json.RawMessage(`{"documentChanges":[{"textDocument":{"uri":"file:///z","version":2},"edits":[{"range":{"start":{"line":0,"character":0},"end":{"line":0,"character":1}},"newText":"Q"}]}]}`)
	edits, err := parseWorkspaceEdit(raw)
	if err != nil || len(edits) != 1 || edits[0].URI != "file:///z" || edits[0].Edits[0].NewText != "Q" {
		t.Fatalf("documentChanges parse = %+v, err %v", edits, err)
	}
}

func TestParseHoverContents(t *testing.T) {
	cases := map[string]string{
		`{"contents":"plain text"}`:                            "plain text",
		`{"contents":{"kind":"markdown","value":"# Heading"}}`: "# Heading",
		`{"contents":{"language":"go","value":"func Foo()"}}`:  "func Foo()",
		`{"contents":["line one","line two"]}`:                 "line one\nline two",
		`null`:                                                 "",
	}
	for raw, want := range cases {
		got, err := parseHoverContents(json.RawMessage(raw))
		if err != nil || got != want {
			t.Errorf("parseHoverContents(%s) = %q (err %v), want %q", raw, got, err, want)
		}
	}
}
