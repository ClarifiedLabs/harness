package mcptools

import (
	"context"
	"slices"
	"strings"
	"testing"

	"encoding/json"

	"harness/internal/llm"
	"harness/internal/mcp"
	"harness/internal/tools"
)

func TestValidName(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want bool
	}{
		{"valid", "mcp__github__create_issue", true},
		{"valid with dash", "mcp__a-b__c-d", true},
		{"missing prefix", "github__create_issue", false},
		{"bare name", "create_issue", false},
		{"illegal char space", "mcp__a__b c", false},
		{"illegal char dot", "mcp__a__b.c", false},
		{"too long 65", "mcp__" + strings.Repeat("a", 60), false},
		{"boundary 64", "mcp__" + strings.Repeat("a", 59), true},
		{"empty", "", false},
		{"prefix only too short ok-charset", "mcp__", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := validName(tt.in); got != tt.want {
				t.Fatalf("validName(%q) = %v, want %v", tt.in, got, tt.want)
			}
		})
	}
}

func TestServerLabel(t *testing.T) {
	tests := []struct {
		in   string
		want string
	}{
		{"mcp__github__create_issue", "github"},
		{"mcp__filesystem__read", "filesystem"},
		{"mcp__a__b__c", "a"}, // first __ wins (display-only best effort)
		{"mcp__noserver", "noserver"},
	}
	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			if got := serverLabel(tt.in); got != tt.want {
				t.Fatalf("serverLabel(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

func TestRegister(t *testing.T) {
	advertised := []mcp.Tool{
		{Name: "mcp__github__create_issue", Description: "Create an issue.\nDetails here.", InputSchema: json.RawMessage(`{"type":"object","properties":{"title":{}}}`)},
		{Name: "mcp__github__list_issues", Description: "List issues."},
		{Name: "mcp__fs__read", Description: "Read a file."},
		{Name: "bare_no_prefix", Description: "should be skipped"},
		{Name: "mcp__bad name", Description: "illegal char skipped"},
	}
	provider := &scriptedProvider{}
	conn, cleanup := newScriptedConn(t, provider, advertised)
	defer cleanup()

	reg := &tools.Registry{}
	sum, err := Register(context.Background(), reg, conn)
	if err != nil {
		t.Fatalf("Register: %v", err)
	}

	if sum.Total != 3 {
		t.Fatalf("Total = %d, want 3", sum.Total)
	}
	wantNames := []string{"mcp__github__create_issue", "mcp__github__list_issues", "mcp__fs__read"}
	if !slices.Equal(sum.Names, wantNames) {
		t.Fatalf("Names = %v, want %v", sum.Names, wantNames)
	}
	if !slices.Equal(reg.Names(), wantNames) {
		t.Fatalf("registry Names = %v, want %v", reg.Names(), wantNames)
	}
	if sum.Servers["github"] != 2 || sum.Servers["fs"] != 1 {
		t.Fatalf("Servers = %v, want github=2 fs=1", sum.Servers)
	}
	wantSkipped := []string{"bare_no_prefix", "mcp__bad name"}
	if !slices.Equal(sum.Skipped, wantSkipped) {
		t.Fatalf("Skipped = %v, want %v", sum.Skipped, wantSkipped)
	}

	// Description is one-lined; schema descriptions are stripped; empty schema falls back.
	specs := reg.Specs()
	if specs[0].Description != "Create an issue." {
		t.Fatalf("create_issue description = %q, want %q", specs[0].Description, "Create an issue.")
	}
	if string(specs[0].Parameters) != `{"properties":{"title":{}},"type":"object"}` {
		t.Fatalf("create_issue schema = %q, want compact schema without descriptions", specs[0].Parameters)
	}
	if string(specs[1].Parameters) != `{"type":"object"}` {
		t.Fatalf("list_issues schema = %q, want empty fallback", specs[1].Parameters)
	}
}

func TestRegisterReadOnlyHintPolicy(t *testing.T) {
	advertised := []mcp.Tool{{
		Name:        "mcp__local__inspect",
		Description: "Inspect.",
		InputSchema: json.RawMessage(`{"type":"object"}`),
		Annotations: json.RawMessage(`{"readOnlyHint":true}`),
	}}
	call := llm.ToolCall{ID: "1", Name: "mcp__local__inspect", Input: json.RawMessage(`{}`)}

	t.Run("default untrusted", func(t *testing.T) {
		provider := &scriptedProvider{}
		conn, cleanup := newScriptedConn(t, provider, advertised)
		defer cleanup()
		reg := &tools.Registry{}
		sum, err := Register(context.Background(), reg, conn)
		if err != nil {
			t.Fatalf("Register: %v", err)
		}
		if reg.CallReadOnly(call) {
			t.Fatal("default registration trusted readOnlyHint")
		}
		if len(sum.ReadOnlyNames) != 0 {
			t.Fatalf("default ReadOnlyNames = %v, want none", sum.ReadOnlyNames)
		}
	})

	t.Run("trusted registration", func(t *testing.T) {
		provider := &scriptedProvider{}
		conn, cleanup := newScriptedConn(t, provider, advertised)
		defer cleanup()
		reg := &tools.Registry{}
		sum, err := RegisterWithOptions(context.Background(), reg, conn, RegisterOptions{TrustReadOnlyHint: true})
		if err != nil {
			t.Fatalf("RegisterWithOptions: %v", err)
		}
		if !reg.CallReadOnly(call) {
			t.Fatal("trusted registration ignored readOnlyHint")
		}
		if !slices.Equal(sum.ReadOnlyNames, []string{"mcp__local__inspect"}) {
			t.Fatalf("trusted ReadOnlyNames = %v, want [mcp__local__inspect]", sum.ReadOnlyNames)
		}
	})
}

func TestRegisterReplacesInPlace(t *testing.T) {
	provider := &scriptedProvider{}
	conn, cleanup := newScriptedConn(t, provider, []mcp.Tool{
		{Name: "mcp__s__a", Description: "first"},
	})
	defer cleanup()

	reg := &tools.Registry{}
	if _, err := Register(context.Background(), reg, conn); err != nil {
		t.Fatalf("Register 1: %v", err)
	}
	// Re-register with updated description; Registry.Register replaces in place.
	provider.tools = []mcp.Tool{{Name: "mcp__s__a", Description: "second"}}
	if _, err := Register(context.Background(), reg, conn); err != nil {
		t.Fatalf("Register 2: %v", err)
	}
	if got := reg.Specs()[0].Description; got != "second" {
		t.Fatalf("after re-register, description = %q, want %q", got, "second")
	}
	if len(reg.Names()) != 1 {
		t.Fatalf("registry has %d tools, want 1", len(reg.Names()))
	}
}
