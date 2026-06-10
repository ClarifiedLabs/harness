package mcpproxy

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"harness/internal/logging"
	"harness/internal/mcp"
	"harness/internal/mcp/jsonrpc"
)

// newFixedSupervisor builds a Supervisor whose tools and call behavior are fixed
// in place (no run loop). It uses the real Supervisor type so the registry sees
// the production API.
func newFixedSupervisor(name string, tools []mcp.Tool) *Supervisor {
	rs := ResolvedServer{Name: name, Transport: TransportStdio, Command: "x"}
	s := NewSupervisor(rs, slog.New(slog.DiscardHandler))
	s.mu.Lock()
	s.tools = tools
	s.state = StateReady
	s.mu.Unlock()
	return s
}

func tool(name string) mcp.Tool {
	return mcp.Tool{Name: name, Description: "d-" + name, InputSchema: json.RawMessage(`{"type":"object"}`)}
}

func TestRegistryNamespacingAndPassthrough(t *testing.T) {
	in := mcp.Tool{
		Name:         "search",
		Title:        "Search",
		Description:  "find things",
		InputSchema:  json.RawMessage(`{"type":"object","properties":{"q":{"type":"string"}}}`),
		OutputSchema: json.RawMessage(`{"type":"object"}`),
		Annotations:  json.RawMessage(`{"readOnly":true}`),
	}
	s := newFixedSupervisor("web", []mcp.Tool{in})
	reg := NewRegistry([]*Supervisor{s}, slog.New(slog.DiscardHandler))

	res, err := reg.ListTools(context.Background(), "")
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	if len(res.Tools) != 1 {
		t.Fatalf("want 1 tool, got %d", len(res.Tools))
	}
	got := res.Tools[0]
	if got.Name != "mcp__web__search" {
		t.Fatalf("name not namespaced: %q", got.Name)
	}
	// All passthrough fields must be untouched.
	if got.Title != in.Title || got.Description != in.Description {
		t.Fatalf("title/description changed: %+v", got)
	}
	if string(got.InputSchema) != string(in.InputSchema) ||
		string(got.OutputSchema) != string(in.OutputSchema) ||
		string(got.Annotations) != string(in.Annotations) {
		t.Fatalf("schema/annotations changed: %+v", got)
	}
}

func TestRegistryInvalidQualifiedDropped(t *testing.T) {
	long := strings.Repeat("z", 70) // qualified name will exceed 64
	s := newFixedSupervisor("srv", []mcp.Tool{tool("ok"), tool(long), tool("bad name")})
	reg := NewRegistry([]*Supervisor{s}, slog.New(slog.DiscardHandler))

	res, err := reg.ListTools(context.Background(), "")
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	if len(res.Tools) != 1 || res.Tools[0].Name != "mcp__srv__ok" {
		t.Fatalf("only the valid tool should survive: %+v", res.Tools)
	}
}

func TestRegistryRoutingWithUnderscoreServerNames(t *testing.T) {
	// Server names containing _ and __ must route correctly via the reverse map,
	// not by string-splitting on __.
	s1 := newFixedSupervisor("a_b", []mcp.Tool{tool("t")})
	s2 := newFixedSupervisor("a__b", []mcp.Tool{tool("t")})
	reg := NewRegistry([]*Supervisor{s1, s2}, slog.New(slog.DiscardHandler))

	cases := []struct {
		qualified string
		wantSrv   string
	}{
		{"mcp__a_b__t", "a_b"},
		{"mcp__a__b__t", "a__b"},
	}
	for _, tc := range cases {
		sup, bare, ok := reg.route(tc.qualified)
		if !ok {
			t.Fatalf("route(%q) not found", tc.qualified)
		}
		if sup.Name() != tc.wantSrv || bare != "t" {
			t.Fatalf("route(%q) = (%s,%s), want (%s,t)", tc.qualified, sup.Name(), bare, tc.wantSrv)
		}
	}
}

func TestRegistryLogsToolCallStats(t *testing.T) {
	var logs bytes.Buffer
	logger, err := logging.NewProxyLogger(&logs, logging.LevelInfo, logging.FormatJSON)
	if err != nil {
		t.Fatalf("NewProxyLogger: %v", err)
	}
	s := newFixedSupervisor("web", []mcp.Tool{tool("search")})
	reg := NewRegistry([]*Supervisor{s}, logger)
	ctx := mcp.ContextWithRequestInfo(context.Background(), mcp.RequestInfo{
		Requester:        "harness",
		RequesterVersion: "test",
		RemoteAddr:       "127.0.0.1:1234",
	})

	res, err := reg.CallTool(ctx, "mcp__web__search", json.RawMessage(`{"q":"x"}`))
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	if res == nil || !res.IsError {
		t.Fatalf("CallTool result = %+v, want unavailable isError result", res)
	}

	var record map[string]any
	if err := json.Unmarshal(logs.Bytes(), &record); err != nil {
		t.Fatalf("decode log %q: %v", logs.String(), err)
	}
	for k, want := range map[string]any{
		"msg":               "mcp tool request completed",
		"requester":         "harness",
		"requester_version": "test",
		"remote_addr":       "127.0.0.1:1234",
		"mcp":               "web",
		"tool":              "search",
		"qualified_tool":    "mcp__web__search",
		"request_bytes":     float64(len(`{"q":"x"}`)),
		"is_error":          true,
	} {
		if got := record[k]; got != want {
			t.Fatalf("log[%s] = %v (%T), want %v", k, got, got, want)
		}
	}
	if record["response_bytes"].(float64) <= 0 {
		t.Fatalf("log response_bytes not populated: %+v", record)
	}
}

func TestRegistryPagination(t *testing.T) {
	var tools []mcp.Tool
	for i := range 107 {
		tools = append(tools, tool(fmt.Sprintf("t%03d", i)))
	}
	s := newFixedSupervisor("s", tools)
	reg := NewRegistry([]*Supervisor{s}, slog.New(slog.DiscardHandler))

	page1, err := reg.ListTools(context.Background(), "")
	if err != nil {
		t.Fatalf("page1: %v", err)
	}
	if len(page1.Tools) != 100 {
		t.Fatalf("page1 size = %d, want 100", len(page1.Tools))
	}
	if page1.NextCursor == "" {
		t.Fatalf("page1 should have a nextCursor")
	}

	page2, err := reg.ListTools(context.Background(), page1.NextCursor)
	if err != nil {
		t.Fatalf("page2: %v", err)
	}
	if len(page2.Tools) != 7 {
		t.Fatalf("page2 size = %d, want 7", len(page2.Tools))
	}
	if page2.NextCursor != "" {
		t.Fatalf("page2 should be the last page")
	}

	// All 107 unique, in sorted order across both pages.
	seen := map[string]bool{}
	var all []string
	for _, tl := range append(append([]mcp.Tool{}, page1.Tools...), page2.Tools...) {
		if seen[tl.Name] {
			t.Fatalf("duplicate tool across pages: %s", tl.Name)
		}
		seen[tl.Name] = true
		all = append(all, tl.Name)
	}
	if len(all) != 107 {
		t.Fatalf("total tools = %d, want 107", len(all))
	}
	for i := 1; i < len(all); i++ {
		if all[i-1] >= all[i] {
			t.Fatalf("tools not sorted: %q >= %q", all[i-1], all[i])
		}
	}
}

func TestRegistryInvalidCursor(t *testing.T) {
	s := newFixedSupervisor("s", []mcp.Tool{tool("a")})
	reg := NewRegistry([]*Supervisor{s}, slog.New(slog.DiscardHandler))

	for _, bad := range []string{"not-base64!!!", base64.StdEncoding.EncodeToString([]byte("notnum")), base64.StdEncoding.EncodeToString([]byte("-5"))} {
		_, err := reg.ListTools(context.Background(), bad)
		if err == nil {
			t.Fatalf("invalid cursor %q should error", bad)
		}
		var je *jsonrpc.Error
		if !errors.As(err, &je) || je.Code != jsonrpc.CodeInvalidParams {
			t.Fatalf("invalid cursor %q: want InvalidParams, got %v", bad, err)
		}
	}
}

func TestRegistryUnknownToolError(t *testing.T) {
	s := newFixedSupervisor("s", []mcp.Tool{tool("a")})
	reg := NewRegistry([]*Supervisor{s}, slog.New(slog.DiscardHandler))

	_, err := reg.CallTool(context.Background(), "mcp__s__does_not_exist", json.RawMessage(`{}`))
	if err == nil {
		t.Fatalf("unknown tool should error")
	}
	var je *jsonrpc.Error
	if !errors.As(err, &je) || je.Code != jsonrpc.CodeInvalidParams {
		t.Fatalf("unknown tool: want InvalidParams error, got %v", err)
	}
}

func TestRegistryCallRoutesVerbatim(t *testing.T) {
	// Use a real supervisor backed by the helper to verify verbatim passthrough.
	rs := ResolvedServer{Name: "h", Transport: TransportStdio, Command: "helper"}
	sup := NewSupervisor(rs, slog.New(slog.DiscardHandler))
	sup.spawn = helperSpawn(t, map[string]string{"HELPER_TOOLS": "echo"})
	sup.sleep = func(context.Context, time.Duration) {}
	reg := NewRegistry([]*Supervisor{sup}, slog.New(slog.DiscardHandler))
	ctx := t.Context()
	sup.Start(ctx)
	defer sup.Shutdown(context.Background())
	waitFor(t, 5*time.Second, func() bool { return sup.State() == StateReady })

	res, err := reg.CallTool(context.Background(), "mcp__h__echo", json.RawMessage(`{"k":"v"}`))
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	if res.IsError || res.Content[0].Text != `{"k":"v"}` {
		t.Fatalf("verbatim passthrough failed: %+v", res)
	}
}

func TestRegistryFanOutAndUnsubscribe(t *testing.T) {
	s := newFixedSupervisor("s", []mcp.Tool{tool("a")})
	reg := NewRegistry([]*Supervisor{s}, slog.New(slog.DiscardHandler))

	// Build two real sessions via mcp.Serve + Client over net.Pipe so
	// NotifyToolsListChanged exercises the real notification path.
	type clientHook struct {
		changed atomic.Int32
	}
	mkSession := func() (*mcp.Client, *clientHook, func()) {
		cc, sc := net.Pipe()
		hook := &clientHook{}
		sessionCh := make(chan *mcp.ServerSession, 1)
		go func() {
			_ = mcp.Serve(context.Background(), sc, mcp.ServerOptions{
				Info:        mcp.Implementation{Name: "gw", Version: "1"},
				Provider:    reg,
				ListChanged: true,
				OnSession: func(ss *mcp.ServerSession) {
					reg.Subscribe(ss)
					sessionCh <- ss
				},
			})
		}()
		client := mcp.NewClient(cc, mcp.ClientOptions{
			Info:           mcp.Implementation{Name: "c", Version: "1"},
			OnToolsChanged: func() { hook.changed.Add(1) },
		})
		if _, err := client.Initialize(context.Background()); err != nil {
			t.Fatalf("client init: %v", err)
		}
		ss := <-sessionCh
		cleanup := func() { reg.Unsubscribe(ss); client.Close() }
		return client, hook, cleanup
	}

	c1, h1, clean1 := mkSession()
	defer clean1()
	c2, h2, clean2 := mkSession()
	defer clean2()
	_ = c1
	_ = c2

	// Broadcast reaches both subscribed sessions.
	reg.BroadcastListChanged()
	waitFor(t, 5*time.Second, func() bool { return h1.changed.Load() >= 1 && h2.changed.Load() >= 1 })

	// Unsubscribe c2; a second broadcast reaches only c1.
	clean2()
	before2 := h2.changed.Load()
	reg.BroadcastListChanged()
	waitFor(t, 5*time.Second, func() bool { return h1.changed.Load() >= 2 })
	if h2.changed.Load() != before2 {
		t.Fatalf("unsubscribed session still received notifications: %d -> %d", before2, h2.changed.Load())
	}
}

func TestRegistryBroadcastDropsBlockedSession(t *testing.T) {
	s := newFixedSupervisor("s", []mcp.Tool{tool("a")})
	reg := NewRegistry([]*Supervisor{s}, slog.New(slog.DiscardHandler))

	blockClient, blockServer := net.Pipe()
	blockSessionCh := make(chan *mcp.ServerSession, 1)
	go func() {
		_ = mcp.Serve(context.Background(), blockServer, mcp.ServerOptions{
			Info:        mcp.Implementation{Name: "gw", Version: "1"},
			Provider:    reg,
			ListChanged: true,
			OnSession: func(ss *mcp.ServerSession) {
				reg.Subscribe(ss)
				blockSessionCh <- ss
			},
		})
	}()
	blocked := <-blockSessionCh
	defer blockClient.Close()
	defer blocked.Close()

	changed := make(chan struct{}, 1)
	activeClient, activeServer := net.Pipe()
	activeSessionCh := make(chan *mcp.ServerSession, 1)
	go func() {
		_ = mcp.Serve(context.Background(), activeServer, mcp.ServerOptions{
			Info:        mcp.Implementation{Name: "gw", Version: "1"},
			Provider:    reg,
			ListChanged: true,
			OnSession: func(ss *mcp.ServerSession) {
				reg.Subscribe(ss)
				activeSessionCh <- ss
			},
		})
	}()
	client := mcp.NewClient(activeClient, mcp.ClientOptions{
		Info:           mcp.Implementation{Name: "c", Version: "1"},
		OnToolsChanged: func() { changed <- struct{}{} },
	})
	defer client.Close()
	active := <-activeSessionCh
	defer reg.Unsubscribe(active)
	if _, err := client.Initialize(context.Background()); err != nil {
		t.Fatalf("active client init: %v", err)
	}

	fillBlocked := func() {
		t.Helper()
		for range 200 {
			err := blocked.TryNotifyToolsListChanged()
			if errors.Is(err, jsonrpc.ErrPeerBlocked) {
				return
			}
			if err != nil {
				t.Fatalf("fill blocked session: %v", err)
			}
		}
		t.Fatal("blocked session outbound queue did not fill")
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		for range 5 {
			fillBlocked()
			reg.BroadcastListChanged()
			reg.mu.RLock()
			_, stillSubscribed := reg.sessions[blocked]
			reg.mu.RUnlock()
			if !stillSubscribed {
				return
			}
		}
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("BroadcastListChanged blocked on full session")
	}
	select {
	case <-changed:
	case <-time.After(5 * time.Second):
		t.Fatal("active session did not receive broadcast")
	}

	reg.mu.RLock()
	_, stillSubscribed := reg.sessions[blocked]
	reg.mu.RUnlock()
	if stillSubscribed {
		t.Fatal("blocked session should be unsubscribed after broadcast")
	}
}
