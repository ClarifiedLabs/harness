package anthropic

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"harness/internal/llm"
)

func TestCountInputTokens(t *testing.T) {
	var gotPath, gotKey, gotVersion, gotBeta string
	var gotBody countRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotKey = r.Header.Get("x-api-key")
		gotVersion = r.Header.Get("anthropic-version")
		gotBeta = r.Header.Get("anthropic-beta")
		if err := json.NewDecoder(r.Body).Decode(&gotBody); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		_ = json.NewEncoder(w).Encode(countResponse{InputTokens: 2345})
	}))
	defer srv.Close()

	p := New(Config{
		BaseURL:     srv.URL + "/anthropic/v1",
		AuthHeaders: map[string]string{"x-api-key": "sk-ant", "anthropic-beta": "existing-beta"},
	})
	got, err := p.CountInputTokens(context.Background(), llm.Request{
		Model:  "claude-sonnet-4-5",
		System: "system",
		Speed:  "fast",
		Betas:  []string{"fast-mode-2026-02-01", "existing-beta"},
		Messages: []llm.Message{{
			Role:    llm.RoleUser,
			Content: []llm.ContentBlock{{Kind: llm.BlockText, Text: "hello"}},
		}},
	})
	if err != nil {
		t.Fatalf("CountInputTokens: %v", err)
	}
	if got.InputTokens != 2345 || got.Source != "anthropic" || got.Scope != llm.InputTokenCountScopeEffectiveContext {
		t.Fatalf("count = %+v, want 2345 anthropic effective-context", got)
	}
	if gotPath != "/anthropic/v1/messages/count_tokens" {
		t.Fatalf("path = %q, want /anthropic/v1/messages/count_tokens", gotPath)
	}
	if gotKey != "sk-ant" || gotVersion == "" {
		t.Fatalf("headers x-api-key=%q anthropic-version=%q", gotKey, gotVersion)
	}
	if gotBeta != "existing-beta,fast-mode-2026-02-01" {
		t.Fatalf("anthropic-beta = %q", gotBeta)
	}
	if gotBody.Model != "claude-sonnet-4-5" || len(gotBody.System) != 1 || len(gotBody.Messages) != 1 {
		t.Fatalf("count request body = %+v", gotBody)
	}
	if gotBody.Speed != "fast" {
		t.Fatalf("count request speed = %q, want fast", gotBody.Speed)
	}
}

func TestCountInputTokensUsesNativeToolSearchShape(t *testing.T) {
	var body countRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		_ = json.NewEncoder(w).Encode(countResponse{InputTokens: 55})
	}))
	defer srv.Close()
	p := New(Config{BaseURL: srv.URL, ToolSearch: llm.AnthropicToolSearchBM25})
	_, err := p.CountInputTokens(context.Background(), llm.Request{
		Model:              "compatible",
		Tools:              []llm.ToolSchema{{Name: "tool_catalog", Parameters: json.RawMessage(`{}`)}},
		DeferredToolGroups: []llm.ToolGroup{{Name: "mcp_demo", Tools: []llm.ToolSchema{{Name: "mcp__demo__search", Parameters: json.RawMessage(`{}`)}}}},
		ToolSearchFallback: "tool_catalog",
		Messages:           []llm.Message{{Role: llm.RoleUser, Content: []llm.ContentBlock{{Kind: llm.BlockText, Text: "hi"}}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(body.Tools) != 2 || body.Tools[0].Name != "mcp__demo__search" || !body.Tools[0].DeferLoading || body.Tools[1].Type != "tool_search_tool_bm25_20251119" {
		t.Fatalf("count tools = %+v", body.Tools)
	}
}
