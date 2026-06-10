package mcptools

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"unicode/utf8"

	"harness/internal/llm"
	"harness/internal/mcp"
	"harness/internal/tools"
)

func TestRenderContent(t *testing.T) {
	tests := []struct {
		name string
		res  *mcp.CallToolResult
		want string
	}{
		{
			name: "text only",
			res:  &mcp.CallToolResult{Content: []mcp.ContentBlock{{Type: "text", Text: "hello"}}},
			want: "hello",
		},
		{
			name: "multi text join",
			res: &mcp.CallToolResult{Content: []mcp.ContentBlock{
				{Type: "text", Text: "line one"},
				{Type: "text", Text: "line two"},
			}},
			want: "line one\nline two",
		},
		{
			name: "image with mime",
			res:  &mcp.CallToolResult{Content: []mcp.ContentBlock{{Type: "image", MimeType: "image/png"}}},
			want: "[image: image/png]",
		},
		{
			name: "image without mime",
			res:  &mcp.CallToolResult{Content: []mcp.ContentBlock{{Type: "image"}}},
			want: "[image: unknown]",
		},
		{
			name: "audio with mime",
			res:  &mcp.CallToolResult{Content: []mcp.ContentBlock{{Type: "audio", MimeType: "audio/wav"}}},
			want: "[audio: audio/wav]",
		},
		{
			name: "audio without mime",
			res:  &mcp.CallToolResult{Content: []mcp.ContentBlock{{Type: "audio"}}},
			want: "[audio: unknown]",
		},
		{
			name: "resource_link without name",
			res:  &mcp.CallToolResult{Content: []mcp.ContentBlock{{Type: "resource_link", URI: "file:///a"}}},
			want: "[resource_link: file:///a]",
		},
		{
			name: "resource_link with name",
			res:  &mcp.CallToolResult{Content: []mcp.ContentBlock{{Type: "resource_link", URI: "file:///a", Name: "a.txt"}}},
			want: "[resource_link: file:///a (a.txt)]",
		},
		{
			name: "embedded resource with uri",
			res: &mcp.CallToolResult{Content: []mcp.ContentBlock{
				{Type: "resource", Resource: json.RawMessage(`{"uri":"file:///b","text":"x"}`)},
			}},
			want: "[resource: file:///b]",
		},
		{
			name: "embedded resource without uri",
			res: &mcp.CallToolResult{Content: []mcp.ContentBlock{
				{Type: "resource", Resource: json.RawMessage(`{"text":"x"}`)},
			}},
			want: "[resource]",
		},
		{
			name: "embedded resource no raw",
			res:  &mcp.CallToolResult{Content: []mcp.ContentBlock{{Type: "resource"}}},
			want: "[resource]",
		},
		{
			name: "embedded resource malformed json",
			res: &mcp.CallToolResult{Content: []mcp.ContentBlock{
				{Type: "resource", Resource: json.RawMessage(`not json`)},
			}},
			want: "[resource]",
		},
		{
			name: "unknown type",
			res:  &mcp.CallToolResult{Content: []mcp.ContentBlock{{Type: "video"}}},
			want: "[unsupported content block: video]",
		},
		{
			name: "mixed order preserved",
			res: &mcp.CallToolResult{Content: []mcp.ContentBlock{
				{Type: "text", Text: "intro"},
				{Type: "image", MimeType: "image/jpeg"},
				{Type: "text", Text: "outro"},
			}},
			want: "intro\n[image: image/jpeg]\noutro",
		},
		{
			name: "structured content fallback",
			res:  &mcp.CallToolResult{StructuredContent: json.RawMessage(`{"k":1}`)},
			want: `{"k":1}`,
		},
		{
			name: "text wins over structured content",
			res: &mcp.CallToolResult{
				Content:           []mcp.ContentBlock{{Type: "text", Text: "shown"}},
				StructuredContent: json.RawMessage(`{"k":1}`),
			},
			want: "shown",
		},
		{
			name: "empty everything",
			res:  &mcp.CallToolResult{},
			want: "",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := renderContent(tt.res); got != tt.want {
				t.Fatalf("renderContent() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestOneLineDesc(t *testing.T) {
	// A string one byte over the cap: truncates to maxDescBytes + "…".
	long := strings.Repeat("a", maxDescBytes+1)
	wantLong := strings.Repeat("a", maxDescBytes) + "…"

	// A multibyte string straddling the cap: cutting at maxDescBytes lands
	// mid-rune and must back off to the previous complete rune.
	multibyte := "a" + strings.Repeat("é", maxDescBytes/2)
	wantMultibyte := "a" + strings.Repeat("é", maxDescBytes/2-1) + "…"

	tests := []struct {
		name string
		in   string
		want string
	}{
		{"empty", "", ""},
		{"whitespace only", "   \n  ", ""},
		{"paragraph keeps first line", "first line\nsecond line\nthird", "first line"},
		{"trims surrounding space", "  hello  ", "hello"},
		{"first line trailing space trimmed", "hello   \nmore", "hello"},
		{"exact cap no ellipsis", strings.Repeat("a", maxDescBytes), strings.Repeat("a", maxDescBytes)},
		{"over cap truncated", long, wantLong},
		{"multibyte rune boundary safe", multibyte, wantMultibyte},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := oneLineDesc(tt.in)
			if got != tt.want {
				t.Fatalf("oneLineDesc(%q) = %q, want %q", tt.in, got, tt.want)
			}
			if !utf8.ValidString(got) {
				t.Fatalf("oneLineDesc(%q) produced invalid UTF-8: %q", tt.in, got)
			}
		})
	}
}

func TestSchemaPassthrough(t *testing.T) {
	raw := json.RawMessage(`{"type":"object","properties":{"x":{"type":"string"}}}`)
	tool := &Tool{name: "mcp__s__t", schema: raw}
	if got := tool.Schema(); string(got) != string(raw) {
		t.Fatalf("Schema() = %q, want byte-identical %q", got, raw)
	}
}

func TestReadOnlyReflectsRegistrationPolicy(t *testing.T) {
	if (&Tool{name: "mcp__s__t"}).ReadOnly(json.RawMessage(`{}`)) {
		t.Fatal("zero-value Tool should not be read-only")
	}
	if !(&Tool{name: "mcp__s__t", readOnly: true}).ReadOnly(json.RawMessage(`{}`)) {
		t.Fatal("trusted read-only Tool should report read-only")
	}
}

// TestRunMappingThroughDispatch exercises Run's result mapping end-to-end. Each
// case drives a real *Conn via newScriptedConn, whose dial seam stands up a real
// mcp.Serve session over net.Pipe backed by a scriptedProvider; the Tool is then
// dispatched through a real tools.Registry so the assertions cover the full
// Run -> Dispatch path (success text, IsError preservation, transport error).
func TestRunMappingThroughDispatch(t *testing.T) {
	tests := []struct {
		name        string
		provider    *scriptedProvider
		wantText    string
		wantIsError bool
	}{
		{
			name:     "success text",
			provider: &scriptedProvider{result: &mcp.CallToolResult{Content: []mcp.ContentBlock{{Type: "text", Text: "ok result"}}}},
			wantText: "ok result",
		},
		{
			name: "is_error preserves mcp text",
			provider: &scriptedProvider{result: &mcp.CallToolResult{
				IsError: true,
				Content: []mcp.ContentBlock{{Type: "text", Text: "boom happened"}},
			}},
			wantText:    "error: boom happened",
			wantIsError: true,
		},
		{
			name:        "is_error empty content stand-in",
			provider:    &scriptedProvider{result: &mcp.CallToolResult{IsError: true}},
			wantText:    "error: tool reported an error with no content",
			wantIsError: true,
		},
		{
			name:        "transport error",
			provider:    &scriptedProvider{callErr: errors.New("downstream exploded")},
			wantText:    "error: ",
			wantIsError: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			conn, cleanup := newScriptedConn(t, tt.provider, []mcp.Tool{{
				Name: "mcp__test__echo", InputSchema: json.RawMessage(`{"type":"object"}`),
			}})
			defer cleanup()

			reg := &tools.Registry{}
			if _, err := Register(context.Background(), reg, conn); err != nil {
				t.Fatalf("Register: %v", err)
			}

			res := reg.Dispatch(context.Background(), llm.ToolCall{
				ID: "c1", Name: "mcp__test__echo", Input: json.RawMessage(`{}`),
			})
			if res.IsError != tt.wantIsError {
				t.Fatalf("IsError = %v, want %v (text=%q)", res.IsError, tt.wantIsError, res.Text)
			}
			if tt.name == "transport error" {
				if !strings.HasPrefix(res.Text, "error: ") {
					t.Fatalf("transport error text = %q, want prefix %q", res.Text, "error: ")
				}
				return
			}
			if res.Text != tt.wantText {
				t.Fatalf("Text = %q, want %q", res.Text, tt.wantText)
			}
		})
	}
}
