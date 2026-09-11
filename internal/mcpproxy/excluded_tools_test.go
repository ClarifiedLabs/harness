package mcpproxy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"slices"
	"sync"
	"testing"

	"harness/internal/mcp"
	"harness/internal/mcp/jsonrpc"
)

func TestLoadConfigExcludedTools(t *testing.T) {
	for _, transport := range []struct {
		name   string
		fields string
	}{
		{"stdio", `"command":"maestro","args":["mcp"]`},
		{"http", `"type":"http","url":"https://example.com/mcp"`},
		{"streamable-http", `"type":"streamable-http","url":"https://example.com/mcp"`},
	} {
		t.Run(transport.name, func(t *testing.T) {
			for _, tc := range []struct {
				name  string
				field string
				want  []string
			}{
				{"omitted", "", nil},
				{"null", `,"excludedTools":null`, nil},
				{"empty", `,"excludedTools":[]`, nil},
				{"names", `,"excludedTools":["run_on_cloud","describe_cloud_run","run_on_cloud"]`, []string{"run_on_cloud", "describe_cloud_run", "run_on_cloud"}},
				{"literal", `,"excludedTools":["${TOOL:-run_on_cloud}","*cloud*"]`, []string{"${TOOL:-run_on_cloud}", "*cloud*"}},
			} {
				t.Run(tc.name, func(t *testing.T) {
					path := writeConfig(t, `{"mcpServers":{"maestro":{`+transport.fields+tc.field+`}}}`)
					cfg, err := LoadConfig(path)
					if err != nil {
						t.Fatal(err)
					}
					if len(cfg.Servers) != 1 || len(cfg.Warnings) != 0 {
						t.Fatalf("servers=%+v warnings=%v", cfg.Servers, cfg.Warnings)
					}
					if got := cfg.Servers[0].ExcludedTools; !slices.Equal(got, tc.want) {
						t.Fatalf("ExcludedTools = %v, want %v", got, tc.want)
					}
				})
			}
		})
	}
}

func TestRegistryExcludedTools(t *testing.T) {
	for _, tc := range []struct {
		name     string
		excluded []string
		want     []string
	}{
		{"omitted", nil, []string{"run", "run_on_cloud", "run_on_cloud_extra"}},
		{"empty", []string{}, []string{"run", "run_on_cloud", "run_on_cloud_extra"}},
		{"exact", []string{"run_on_cloud"}, []string{"run", "run_on_cloud_extra"}},
		{"multiple", []string{"run_on_cloud", "run_on_cloud_extra"}, []string{"run"}},
		{"all", []string{"run", "run_on_cloud", "run_on_cloud_extra"}, nil},
		{"unknown", []string{"missing"}, []string{"run", "run_on_cloud", "run_on_cloud_extra"}},
		{"case_sensitive", []string{"RUN_ON_CLOUD"}, []string{"run", "run_on_cloud", "run_on_cloud_extra"}},
		{"no_globs", []string{"*cloud*"}, []string{"run", "run_on_cloud", "run_on_cloud_extra"}},
		{"not_qualified", []string{"mcp__maestro__run_on_cloud"}, []string{"run", "run_on_cloud", "run_on_cloud_extra"}},
		{"duplicates", []string{"run_on_cloud", "run_on_cloud"}, []string{"run", "run_on_cloud_extra"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tools := []mcp.Tool{tool("run_on_cloud_extra"), tool("run_on_cloud"), tool("run")}
			s := newFixedSupervisor("maestro", tools)
			s.cfg.ExcludedTools = tc.excluded
			other := newFixedSupervisor("other", []mcp.Tool{tool("run_on_cloud")})
			reg := NewRegistry([]*Supervisor{s, other}, nil)

			res, err := reg.ListTools(t.Context(), "")
			if err != nil {
				t.Fatal(err)
			}
			var got, want []string
			for _, tool := range res.Tools {
				got = append(got, tool.Name)
			}
			for _, name := range tc.want {
				want = append(want, "mcp__maestro__"+name)
			}
			want = append(want, "mcp__other__run_on_cloud")
			if !slices.Equal(got, want) || res.NextCursor != "" {
				t.Fatalf("ListTools = %v (cursor %q), want %v", got, res.NextCursor, want)
			}
			for _, name := range want {
				if _, _, ok := reg.route(name); !ok {
					t.Errorf("listed tool %q has no route", name)
				}
			}
			for _, tool := range tools {
				if slices.Contains(tc.want, tool.Name) {
					continue
				}
				result, err := reg.CallTool(t.Context(), "mcp__maestro__"+tool.Name, json.RawMessage(`{}`))
				var rpcErr *jsonrpc.Error
				if result != nil || !errors.As(err, &rpcErr) || rpcErr.Code != jsonrpc.CodeInvalidParams {
					t.Errorf("excluded call %q = (%+v, %v), want unknown-tool error", tool.Name, result, err)
				}
			}
		})
	}
}

func TestRegistryExcludedToolsNameCollision(t *testing.T) {
	for _, excludedServer := range []string{"a", "a__b"} {
		for _, reverse := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/reverse=%t", excludedServer, reverse), func(t *testing.T) {
				a := newFixedSupervisor("a", []mcp.Tool{tool("b__danger")})
				b := newFixedSupervisor("a__b", []mcp.Tool{tool("danger")})
				if excludedServer == "a" {
					a.cfg.ExcludedTools = []string{"b__danger"}
				} else {
					b.cfg.ExcludedTools = []string{"danger"}
				}
				servers := []*Supervisor{a, b}
				if reverse {
					slices.Reverse(servers)
				}
				reg := NewRegistry(servers, nil)
				res, err := reg.ListTools(t.Context(), "")
				if err != nil || len(res.Tools) != 0 {
					t.Fatalf("ambiguous excluded name still listed: %+v, %v", res, err)
				}
				result, err := reg.CallTool(t.Context(), "mcp__a__b__danger", json.RawMessage(`{}`))
				var rpcErr *jsonrpc.Error
				if result != nil || !errors.As(err, &rpcErr) || rpcErr.Code != jsonrpc.CodeInvalidParams {
					t.Fatalf("ambiguous excluded call = (%+v, %v), want unknown-tool error", result, err)
				}
			})
		}
	}

	// Unknown exclusions must not reserve names that only another server advertises.
	a := newFixedSupervisor("a", nil)
	a.cfg.ExcludedTools = []string{"b__danger"}
	b := newFixedSupervisor("a__b", []mcp.Tool{tool("danger")})
	reg := NewRegistry([]*Supervisor{a, b}, nil)
	if sup, bare, ok := reg.route("mcp__a__b__danger"); !ok || sup != b || bare != "danger" {
		t.Fatalf("unadvertised exclusion blocked another server: (%v, %q, %v)", sup, bare, ok)
	}
}

type rebuildPauseHandler struct {
	slog.Handler
	once    sync.Once
	entered chan struct{}
	release chan struct{}
}

func (h *rebuildPauseHandler) Handle(context.Context, slog.Record) error {
	h.once.Do(func() {
		close(h.entered)
		<-h.release
	})
	return nil
}

func TestRegistryExcludedToolsConcurrentRebuild(t *testing.T) {
	a := newFixedSupervisor("a", nil)
	a.cfg.ExcludedTools = []string{"b__danger"}
	b := newFixedSupervisor("a__b", []mcp.Tool{tool("danger")})
	h := &rebuildPauseHandler{
		Handler: slog.NewTextHandler(io.Discard, nil),
		entered: make(chan struct{}),
		release: make(chan struct{}),
	}
	reg := NewRegistry([]*Supervisor{a, b}, slog.New(h))

	// Pause the first rebuild after it snapshots a without the excluded tool.
	// A warning from b gives us a deterministic barrier inside computation.
	b.mu.Lock()
	b.tools = append(b.tools, tool("bad name"))
	b.mu.Unlock()
	done := make(chan struct{}, 2)
	go func() {
		reg.onSupervisorToolsChanged()
		done <- struct{}{}
	}()
	<-h.entered
	if reg.mu.TryRLock() {
		reg.mu.RUnlock()
		t.Error("rebuild computation is not serialized with publication")
	}

	// A newer snapshot must be published after, never before, the paused one.
	a.mu.Lock()
	a.tools = []mcp.Tool{tool("b__danger")}
	a.mu.Unlock()
	go func() {
		reg.onSupervisorToolsChanged()
		done <- struct{}{}
	}()
	close(h.release)
	<-done
	<-done
	if _, _, ok := reg.route("mcp__a__b__danger"); ok {
		t.Fatal("concurrent rebuild restored the excluded identity")
	}
}

func TestRegistryExcludedToolsRebuild(t *testing.T) {
	s := newFixedSupervisor("maestro", nil)
	s.cfg.ExcludedTools = []string{"run_on_cloud", "describe_cloud_run"}
	reg := NewRegistry([]*Supervisor{s}, nil)
	for _, tools := range [][]mcp.Tool{
		{tool("run"), tool("run_on_cloud")},
		{tool("run"), tool("run_on_cloud"), tool("describe_cloud_run")},
		nil, // disconnect, followed by rediscovery
		{tool("run"), tool("run_on_cloud"), tool("describe_cloud_run")},
	} {
		s.mu.Lock()
		s.tools = tools
		s.mu.Unlock()
		s.onToolsChanged()

		res, err := reg.ListTools(t.Context(), "")
		if err != nil {
			t.Fatal(err)
		}
		if tools == nil {
			if len(res.Tools) != 0 {
				t.Fatalf("disconnected tools = %+v", res.Tools)
			}
		} else if len(res.Tools) != 1 || res.Tools[0].Name != "mcp__maestro__run" {
			t.Fatalf("refreshed tools = %+v, want only run", res.Tools)
		}
		for _, name := range s.cfg.ExcludedTools {
			if _, _, ok := reg.route("mcp__maestro__" + name); ok {
				t.Fatalf("excluded tool %q gained a route after rebuild", name)
			}
		}
	}
}

func TestRegistryExcludedToolsBeforePagination(t *testing.T) {
	var tools []mcp.Tool
	var excluded []string
	for i := range pageSize + 1 {
		name := fmt.Sprintf("t%03d", i)
		tools = append(tools, tool(name))
		if i == 0 {
			excluded = append(excluded, name)
		}
	}
	s := newFixedSupervisor("s", tools)
	s.cfg.ExcludedTools = excluded
	reg := NewRegistry([]*Supervisor{s}, nil)
	res, err := reg.ListTools(t.Context(), "")
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Tools) != pageSize || res.NextCursor != "" || res.Tools[0].Name != "mcp__s__t001" {
		t.Fatalf("filtered page = %+v, want one full page starting at t001", res)
	}
}
