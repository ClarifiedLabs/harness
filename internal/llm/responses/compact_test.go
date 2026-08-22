package responses

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"harness/internal/llm"
)

func TestCompactContextReturnsCanonicalWindow(t *testing.T) {
	var gotPath, gotAuth string
	var got compactRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		_, _ = w.Write([]byte(`{
  "id":"resp_compact_1",
  "object":"response.compaction",
  "output":[
    {"type":"message","role":"user","content":[{"type":"input_text","text":"retained"}]},
    {"id":"cmp_1","type":"compaction","encrypted_content":"opaque-window"}
  ],
  "usage":{"input_tokens":100,"output_tokens":20,"input_tokens_details":{"cached_tokens":30},"output_tokens_details":{"reasoning_tokens":5}}
}`))
	}))
	t.Cleanup(srv.Close)

	p := New(Config{APIKey: "sk-test", BaseURL: srv.URL})
	result, err := p.CompactContext(context.Background(), llm.Request{
		Model:  "gpt-5.5",
		System: "system prompt",
		Messages: []llm.Message{{
			Role: llm.RoleUser, Content: []llm.ContentBlock{{Kind: llm.BlockText, Text: "hello"}},
		}},
	})
	if err != nil {
		t.Fatalf("CompactContext: %v", err)
	}
	if gotPath != "/responses/compact" || gotAuth != "Bearer sk-test" {
		t.Fatalf("request path/auth = %q/%q", gotPath, gotAuth)
	}
	if got.Model != "gpt-5.5" || got.Instructions != "system prompt" || len(got.Input) != 1 {
		t.Fatalf("compact request = %+v", got)
	}
	if len(result.Items) != 2 || rawInputItemType(result.Items[1]) != "compaction" {
		t.Fatalf("canonical items = %s", mustJSON(t, result.Items))
	}
	wantUsage := llm.Usage{InputTokens: 70, OutputTokens: 15, CacheReadTokens: 30, ReasoningTokens: 5}
	if result.Usage != wantUsage {
		t.Fatalf("usage = %+v, want %+v", result.Usage, wantUsage)
	}
}

func TestCompactContextRejectsWindowWithoutCompactionItem(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"id":"x","object":"response.compaction","output":[{"type":"message","role":"user","content":[]}],"usage":{}}`))
	}))
	t.Cleanup(srv.Close)

	_, err := New(Config{BaseURL: srv.URL}).CompactContext(context.Background(), llm.Request{Model: "gpt-5.5"})
	if err == nil {
		t.Fatal("CompactContext succeeded without a compaction item")
	}
}

func TestCompactContextRejectsMissingObjectOutsideCodex(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"output":[{"id":"cmp_1","type":"compaction","encrypted_content":"opaque"}]}`))
	}))
	t.Cleanup(srv.Close)

	_, err := New(Config{BaseURL: srv.URL, ProviderName: "compatible"}).CompactContext(
		context.Background(), llm.Request{Model: "gpt-5.5"},
	)
	if err == nil {
		t.Fatal("CompactContext accepted a missing object discriminator outside ChatGPT Codex")
	}
}

func TestCompactContextSupportsChatGPTCodexContract(t *testing.T) {
	var got compactRequest
	var headers http.Header
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		headers = r.Header.Clone()
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		// Codex's standalone client treats output as the required response field
		// and does not require the public API's object discriminator.
		_, _ = w.Write([]byte(`{"output":[{"id":"cmp_1","type":"compaction","encrypted_content":"opaque-window"}]}`))
	}))
	t.Cleanup(srv.Close)

	p := New(Config{
		AuthHeaders: map[string]string{
			"Authorization":      "Bearer chatgpt-token",
			"ChatGPT-Account-ID": "account-123",
		},
		BaseURL:      srv.URL,
		ProviderName: "openai-codex",
	})
	result, err := p.CompactContext(context.Background(), llm.Request{
		Model:  "gpt-5.5",
		System: "system prompt",
		Messages: []llm.Message{{
			Role: llm.RoleUser, Content: []llm.ContentBlock{{Kind: llm.BlockText, Text: "hello"}},
		}},
		Tools: []llm.ToolSchema{{
			Name: "read", Description: "Read a file", Parameters: json.RawMessage(`{"type":"object"}`),
		}},
		ServerTools:    []llm.ServerTool{{Name: llm.ServerToolWebSearch}},
		Reasoning:      llm.ReasoningConfig{Effort: "high", Summary: "concise"},
		PromptCacheKey: "harness-session",
	})
	if err != nil {
		t.Fatalf("CompactContext: %v", err)
	}
	if len(result.Items) != 1 || rawInputItemType(result.Items[0]) != "compaction" {
		t.Fatalf("canonical items = %s", mustJSON(t, result.Items))
	}
	if got.PromptCacheKey != "harness-session" || !got.ParallelTools || len(got.Tools) != 2 ||
		got.Reasoning == nil || got.Reasoning.Effort != "high" || got.Reasoning.Summary != "concise" {
		t.Fatalf("Codex compact request = %+v", got)
	}
	if headers.Get("Authorization") != "Bearer chatgpt-token" || headers.Get("ChatGPT-Account-ID") != "account-123" {
		t.Fatalf("Codex auth headers = %+v", headers)
	}
	for _, name := range []string{"x-codex-installation-id", "session-id", "thread-id", "x-codex-window-id"} {
		if headers.Get(name) == "" {
			t.Fatalf("Codex compact request missing %s: %+v", name, headers)
		}
	}
}

func TestBuildRequestReplaysCompactedItemsWithoutPruning(t *testing.T) {
	retained := json.RawMessage(`{"type":"message","role":"assistant","content":[{"type":"output_text","text":"kept"}],"provider_extension":{"x":1}}`)
	compaction := json.RawMessage(`{"id":"cmp_1","type":"compaction","encrypted_content":"opaque","created_by":"server"}`)
	w := buildRequest(llm.Request{
		Model: "gpt-5.5",
		Messages: []llm.Message{{
			Role: llm.RoleUser, Origin: llm.MessageOriginProviderCompaction,
			Content: []llm.ContentBlock{{Kind: llm.BlockProviderCompaction, ReasoningReplayDomain: "openai:gpt", ProviderCompaction: []json.RawMessage{retained, compaction}}},
		}},
	}, 0, 0)
	if len(w.Input) != 2 {
		t.Fatalf("input items = %d, want 2", len(w.Input))
	}
	got, err := json.Marshal(w.Input)
	if err != nil {
		t.Fatalf("marshal input: %v", err)
	}
	want, _ := json.Marshal([]json.RawMessage{retained, compaction})
	if !jsonEqual(got, want) {
		t.Fatalf("replayed input = %s, want %s", got, want)
	}
}

func mustJSON(t *testing.T, value any) string {
	t.Helper()
	b, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func jsonEqual(left, right []byte) bool {
	var a, b any
	return json.Unmarshal(left, &a) == nil && json.Unmarshal(right, &b) == nil &&
		mustCanonical(a) == mustCanonical(b)
}

func mustCanonical(value any) string {
	b, _ := json.Marshal(value)
	return string(b)
}
