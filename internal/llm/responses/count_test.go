package responses

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"harness/internal/llm"
)

func TestCountInputTokens(t *testing.T) {
	var gotPath, gotAuth string
	var gotBody countRequest
	var gotFields map[string]json.RawMessage
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		if err := json.NewDecoder(r.Body).Decode(&gotFields); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		body, err := json.Marshal(gotFields)
		if err != nil {
			t.Fatalf("remarshal request: %v", err)
		}
		if err := json.Unmarshal(body, &gotBody); err != nil {
			t.Fatalf("decode typed request: %v", err)
		}
		_ = json.NewEncoder(w).Encode(countResponse{InputTokens: 1234})
	}))
	defer srv.Close()

	p := New(Config{APIKey: "sk-test", BaseURL: srv.URL})
	got, err := p.CountInputTokens(context.Background(), llm.Request{
		Model:         "gpt-5.5",
		System:        "system",
		StoreResponse: true,
		Messages: []llm.Message{{
			Role:    llm.RoleUser,
			Content: []llm.ContentBlock{{Kind: llm.BlockText, Text: "hello"}},
		}},
	})
	if err != nil {
		t.Fatalf("CountInputTokens: %v", err)
	}
	if got.InputTokens != 1234 || got.Source != "responses" || got.Scope != llm.InputTokenCountScopeEffectiveContext {
		t.Fatalf("count = %+v, want 1234 responses effective-context", got)
	}
	if gotPath != "/responses/input_tokens" {
		t.Fatalf("path = %q, want /responses/input_tokens", gotPath)
	}
	if gotAuth != "Bearer sk-test" {
		t.Fatalf("Authorization = %q", gotAuth)
	}
	if gotBody.Model != "gpt-5.5" || gotBody.Instructions != "system" || len(gotBody.Input) != 1 {
		t.Fatalf("count request body = %+v", gotBody)
	}
	if _, ok := gotFields["store"]; ok {
		t.Fatalf("count request fields = %v, store is not part of the input-token count contract", gotFields)
	}
}

func TestCountInputTokensUsesNativeToolSearchShape(t *testing.T) {
	var got countRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Fatal(err)
		}
		_ = json.NewEncoder(w).Encode(countResponse{InputTokens: 55})
	}))
	defer srv.Close()
	enabled := true
	_, err := New(Config{BaseURL: srv.URL, ToolSearch: &enabled}).CountInputTokens(context.Background(), llm.Request{
		Model: "compatible",
		Tools: []llm.ToolSchema{
			{Name: "read", Parameters: json.RawMessage(`{"type":"object"}`)},
			{Name: "tool_catalog", Parameters: json.RawMessage(`{"type":"object"}`)},
		},
		DeferredToolGroups: []llm.ToolGroup{{
			Name:  "mcp_demo",
			Tools: []llm.ToolSchema{{Name: "mcp__demo__search", Parameters: json.RawMessage(`{"type":"object"}`)}},
		}},
		ToolSearchFallback: "tool_catalog",
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Tools) != 3 || got.Tools[0].Name != "read" || got.Tools[1].Type != "namespace" || got.Tools[2].Type != "tool_search" {
		t.Fatalf("count tools = %+v, want read + namespace + tool_search", got.Tools)
	}
}

func TestCountInputTokensOmitsPromptCacheBreakpoints(t *testing.T) {
	var body []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var err error
		body, err = io.ReadAll(r.Body)
		if err != nil {
			t.Fatal(err)
		}
		_ = json.NewEncoder(w).Encode(countResponse{InputTokens: 55})
	}))
	defer srv.Close()
	enabled := true
	_, err := New(Config{BaseURL: srv.URL, PromptCache: llm.PromptCacheConfig{ExplicitBreakpoints: &enabled}}).CountInputTokens(context.Background(), llm.Request{
		Model: "gpt-5.6",
		Messages: []llm.Message{
			{Role: llm.RoleUser, Content: []llm.ContentBlock{{Kind: llm.BlockText, Text: "stable"}}},
			{Role: llm.RoleUser, Content: []llm.ContentBlock{{Kind: llm.BlockText, Text: "current"}}},
		},
		CachePolicy: llm.CachePolicy{StableMessagePrefix: 1},
	})
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(body, []byte("prompt_cache_breakpoint")) || bytes.Contains(body, []byte("prompt_cache_options")) {
		t.Fatalf("count request contains cache write controls: %s", body)
	}
}

func TestCountInputTokensLeavesContinuationScopeUnknown(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(countResponse{InputTokens: 55})
	}))
	defer srv.Close()

	got, err := New(Config{BaseURL: srv.URL}).CountInputTokens(context.Background(), llm.Request{
		Model: "gpt-5.5", PreviousResponseID: "resp_1",
	})
	if err != nil {
		t.Fatalf("CountInputTokens: %v", err)
	}
	if got.Scope != llm.InputTokenCountScopeUnknown {
		t.Fatalf("scope = %q, want unknown", got.Scope)
	}
}
