package responses

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
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

func TestCompactionRequestOmitsPromptCacheBreakpoints(t *testing.T) {
	enabled := true
	p := New(Config{BaseURL: "https://compatible.test/v1", PromptCache: llm.PromptCacheConfig{ExplicitBreakpoints: &enabled}})
	base, input := p.compactionRequestBase(llm.Request{
		Model: "custom-model",
		Messages: []llm.Message{
			{Role: llm.RoleUser, Content: []llm.ContentBlock{{Kind: llm.BlockText, Text: "stable"}}},
			{Role: llm.RoleUser, Content: []llm.ContentBlock{{Kind: llm.BlockText, Text: "current"}}},
		},
		CachePolicy: llm.CachePolicy{StableMessagePrefix: 1},
	})
	body := mustJSON(t, struct {
		Base  wireRequest     `json:"base"`
		Input []wireInputItem `json:"input"`
	}{base, input})
	if strings.Contains(body, "prompt_cache_breakpoint") || strings.Contains(body, "prompt_cache_options") {
		t.Fatalf("compaction request contains cache write controls: %s", body)
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

func TestCompactContextUsesCompactionV2ForOfficialOpenAI(t *testing.T) {
	var got wireRequest
	var gotURL, gotAuth string
	client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		gotURL = r.URL.String()
		gotAuth = r.Header.Get("Authorization")
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		body := "event: response.output_item.done\n" +
			`data: {"type":"response.output_item.done","item":{"type":"compaction","encrypted_content":"opaque-window"}}` + "\n\n" +
			"event: response.completed\n" +
			`data: {"type":"response.completed","response":{"id":"resp_compact_1","status":"completed","usage":{}}}` + "\n\n"
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     make(http.Header),
			Body:       io.NopCloser(strings.NewReader(body)),
			Request:    r,
		}, nil
	})}

	result, err := New(Config{APIKey: "sk-test", HTTPClient: client}).CompactContext(
		context.Background(),
		llm.Request{
			Model: "gpt-5.5",
			Messages: []llm.Message{{
				Role: llm.RoleUser, Content: []llm.ContentBlock{{Kind: llm.BlockText, Text: "hello"}},
			}},
		},
	)
	if err != nil {
		t.Fatalf("CompactContext: %v", err)
	}
	if gotURL != "https://api.openai.com/v1/responses" || gotAuth != "Bearer sk-test" {
		t.Fatalf("request URL/auth = %q/%q", gotURL, gotAuth)
	}
	if len(got.Input) != 2 || got.Input[1].Type != "compaction_trigger" || !got.Stream {
		t.Fatalf("official OpenAI compaction request = %+v", got)
	}
	if len(result.Items) != 2 || rawInputItemType(result.Items[0]) != "message" || rawInputItemType(result.Items[1]) != "compaction" {
		t.Fatalf("canonical items = %s", mustJSON(t, result.Items))
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) {
	return f(r)
}

func TestCompactContextUsesCompactionV2ForChatGPTCodex(t *testing.T) {
	var got wireRequest
	var gotPath string
	var headers http.Header
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		headers = r.Header.Clone()
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		_, _ = w.Write([]byte("event: response.output_item.done\n" +
			`data: {"type":"response.output_item.done","output_index":0,"item":{"type":"compaction","encrypted_content":"opaque-window","created_by":"server"}}` + "\n\n" +
			"event: response.completed\n" +
			`data: {"type":"response.completed","response":{"id":"resp_compact_1","status":"completed","output":[{"type":"compaction","encrypted_content":"opaque-window","created_by":"server"}],"usage":{"input_tokens":100,"output_tokens":20,"input_tokens_details":{"cached_tokens":30}}}}` + "\n\n"))
	}))
	t.Cleanup(srv.Close)

	p := New(Config{
		AuthHeaders: map[string]string{
			"Authorization":         "Bearer chatgpt-token",
			"ChatGPT-Account-ID":    "account-123",
			"x-codex-beta-features": "existing_feature",
		},
		BaseURL:      srv.URL,
		ProviderName: "openai-codex",
	})
	result, err := p.CompactContext(context.Background(), llm.Request{
		Model:  "gpt-5.5",
		System: "system prompt",
		Messages: []llm.Message{
			{Role: llm.RoleUser, Content: []llm.ContentBlock{{Kind: llm.BlockText, Text: "hello"}}},
			{Role: llm.RoleAssistant, Content: []llm.ContentBlock{{Kind: llm.BlockText, Text: "response"}}},
		},
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
	if gotPath != "/responses" {
		t.Fatalf("Codex compaction path = %q, want /responses", gotPath)
	}
	if len(got.Input) != 3 || got.Input[2].Type != "compaction_trigger" {
		t.Fatalf("Codex compaction input = %s", mustJSON(t, got.Input))
	}
	if !got.Stream || got.Store || got.MaxOutputTokens != nil || len(got.Include) != 1 || got.Include[0] != reasoningInclude {
		t.Fatalf("Codex compaction controls = %+v", got)
	}
	if len(result.Items) != 2 || rawInputItemType(result.Items[0]) != "message" || rawInputItemType(result.Items[1]) != "compaction" {
		t.Fatalf("canonical items = %s", mustJSON(t, result.Items))
	}
	if !strings.Contains(string(result.Items[1]), `"created_by":"server"`) {
		t.Fatalf("compaction item was not preserved verbatim: %s", result.Items[1])
	}
	wantUsage := llm.Usage{InputTokens: 70, OutputTokens: 20, CacheReadTokens: 30}
	if result.Usage != wantUsage {
		t.Fatalf("usage = %+v, want %+v", result.Usage, wantUsage)
	}
	if got.PromptCacheKey != "harness-session" || !got.ParallelTools || len(got.Tools) != 2 ||
		got.Reasoning == nil || got.Reasoning.Effort != "high" || got.Reasoning.Summary != "concise" {
		t.Fatalf("Codex compact request = %+v", got)
	}
	if headers.Get("Authorization") != "Bearer chatgpt-token" || headers.Get("ChatGPT-Account-ID") != "account-123" {
		t.Fatalf("Codex auth headers = %+v", headers)
	}
	if got := headers.Get("x-codex-beta-features"); got != "existing_feature,remote_compaction_v2" {
		t.Fatalf("x-codex-beta-features = %q", got)
	}
	for _, name := range []string{"x-codex-installation-id", "session-id", "thread-id", "x-codex-window-id"} {
		if headers.Get(name) == "" {
			t.Fatalf("Codex compact request missing %s: %+v", name, headers)
		}
	}
}

func TestCompactContextV2RequiresCompletedStreamAndOneCompactionItem(t *testing.T) {
	for _, tt := range []struct {
		name string
		body string
	}{
		{name: "truncated", body: "event: response.output_item.done\n" +
			`data: {"type":"response.output_item.done","item":{"id":"cmp_1","type":"compaction","encrypted_content":"opaque"}}` + "\n\n"},
		{name: "missing compaction", body: "event: response.completed\n" +
			`data: {"type":"response.completed","response":{"id":"resp_1","status":"completed","output":[]}}` + "\n\n"},
		{name: "multiple compactions", body: "event: response.output_item.done\n" +
			`data: {"type":"response.output_item.done","item":{"id":"cmp_1","type":"compaction","encrypted_content":"one"}}` + "\n\n" +
			"event: response.output_item.done\n" +
			`data: {"type":"response.output_item.done","item":{"id":"cmp_2","type":"compaction","encrypted_content":"two"}}` + "\n\n" +
			"event: response.completed\n" +
			`data: {"type":"response.completed","response":{"id":"resp_1","status":"completed","output":[]}}` + "\n\n"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				_, _ = w.Write([]byte(tt.body))
			}))
			defer srv.Close()
			_, err := New(Config{BaseURL: srv.URL, ProviderName: "openai-codex"}).CompactContext(
				context.Background(), llm.Request{Model: "gpt-5.5", Messages: []llm.Message{{Role: llm.RoleUser, Content: []llm.ContentBlock{{Kind: llm.BlockText, Text: "hello"}}}}},
			)
			if err == nil {
				t.Fatal("CompactContext succeeded for malformed compaction v2 stream")
			}
		})
	}
}

func TestRetainedCompactionV2ItemsKeepsNewestUserMessagesWithinBudget(t *testing.T) {
	input := []wireInputItem{
		{Type: "message", Role: "user", Content: []wireContentPart{{Type: "input_text", Text: "old"}}, RetainOnCompaction: true},
		{Type: "message", Role: "assistant", Content: []wireContentPart{{Type: "output_text", Text: "drop"}}},
		{Type: "message", Role: "user", Content: []wireContentPart{{Type: "input_text", Text: strings.Repeat("x", compactV2RetainedMessageTokenBudget*4)}}, RetainOnCompaction: true},
		{Type: "message", Role: "user", Content: []wireContentPart{{Type: "input_text", Text: "new"}}, RetainOnCompaction: true},
	}

	got, err := retainedCompactionV2Items(input)
	if err != nil {
		t.Fatalf("retainedCompactionV2Items: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("retained items = %d, want truncated boundary and newest message", len(got))
	}
	var first, second struct {
		Role    string `json:"role"`
		Content []struct {
			Text string `json:"text"`
		} `json:"content"`
	}
	if err := json.Unmarshal(got[0], &first); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(got[1], &second); err != nil {
		t.Fatal(err)
	}
	if first.Role != "user" || len(first.Content) != 1 || len(first.Content[0].Text) != (compactV2RetainedMessageTokenBudget-1)*4 {
		t.Fatalf("truncated boundary message length = %d", len(first.Content[0].Text))
	}
	if second.Role != "user" || len(second.Content) != 1 || second.Content[0].Text != "new" {
		t.Fatalf("newest retained message = %s", got[1])
	}
}

func TestRetainedCompactionV2ItemsExcludesToolResultImageProjection(t *testing.T) {
	input := buildInput([]llm.Message{
		{Role: llm.RoleUser, Content: []llm.ContentBlock{{Kind: llm.BlockImage, ImageMediaType: "image/png", ImageData: "dXNlcg=="}}},
		{Role: llm.RoleUser, Content: []llm.ContentBlock{{
			Kind: llm.BlockToolResult, ResultForID: "call_1", ResultText: "attached",
			ResultContent: []llm.ContentBlock{{Kind: llm.BlockImage, ImageMediaType: "image/png", ImageData: "dG9vbA=="}},
		}}},
		{Role: llm.RoleUser, Origin: llm.MessageOriginProviderCompaction, Content: []llm.ContentBlock{{
			Kind: llm.BlockProviderCompaction, ReasoningReplayDomain: "openai:gpt",
			ProviderCompaction: []json.RawMessage{
				json.RawMessage(`{"type":"message","role":"user","content":[{"type":"input_text","text":"older retained"}]}`),
				json.RawMessage(`{"type":"compaction","encrypted_content":"old"}`),
			},
		}}},
	}, true)

	got, err := retainedCompactionV2Items(input)
	if err != nil {
		t.Fatalf("retainedCompactionV2Items: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("retained items = %s, want genuine user image and older retained user message", mustJSON(t, got))
	}
	encoded := string(mustJSON(t, got))
	if !strings.Contains(encoded, "dXNlcg==") || !strings.Contains(encoded, "older retained") || strings.Contains(encoded, "dG9vbA==") {
		t.Fatalf("retained items have wrong image provenance: %s", encoded)
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
