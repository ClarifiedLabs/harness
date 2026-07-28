package factory

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"harness/internal/llm"
)

func TestInferProvider(t *testing.T) {
	cases := []struct {
		name     string
		opts     Options
		wantName string
	}{
		{"claude prefix infers anthropic", Options{Model: "claude-opus-4-8", APIKey: "k"}, "anthropic"},
		{"claude sonnet infers anthropic", Options{Model: "claude-sonnet-4-6", APIKey: "k"}, "anthropic"},
		{"gpt infers openai", Options{Model: "gpt-5.4", APIKey: "k"}, "openai"},
		{"arbitrary local model infers openai", Options{Model: "llama-3.1-70b", APIKey: "k"}, "openai"},
		{"explicit provider overrides claude inference", Options{Provider: "openai", Model: "claude-weird", APIKey: "k"}, "openai"},
		{"explicit responses provider", Options{Provider: "responses", Model: "gpt-5.4", APIKey: "k"}, "responses"},
		{"explicit interactions provider", Options{Provider: "interactions", Model: "gemini-3.6-flash", APIKey: "k"}, "interactions"},
		{"explicit anthropic overrides non-claude model", Options{Provider: "anthropic", Model: "custom", APIKey: "k"}, "anthropic"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p, err := New(tc.opts)
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			if p.Name() != tc.wantName {
				t.Errorf("provider = %q, want %q", p.Name(), tc.wantName)
			}
		})
	}
}

func TestMissingAPIKeyDefaultBaseURL(t *testing.T) {
	// OpenAI default base URL with no key is an error.
	if _, err := New(Options{Model: "gpt-5.4"}); err == nil {
		t.Error("expected error for missing OpenAI API key with default base URL")
	}
	// Anthropic default base URL with no key is an error.
	if _, err := New(Options{Model: "claude-opus-4-8"}); err == nil {
		t.Error("expected error for missing Anthropic API key with default base URL")
	}
	// Responses default base URL with no key is an error.
	if _, err := New(Options{Provider: "responses", Model: "gpt-5.4"}); err == nil {
		t.Error("expected error for missing Responses API key with default base URL")
	}
	if _, err := New(Options{Provider: "interactions", Model: "gemini-3.6-flash"}); err == nil {
		t.Error("expected error for missing Gemini API key with default base URL")
	}
}

func TestEmptyKeyAllowedWithCustomBaseURL(t *testing.T) {
	// Local OpenAI-compatible servers need no key when a base URL is given.
	p, err := New(Options{Model: "llama-3.1-70b", BaseURL: "http://localhost:11434/v1"})
	if err != nil {
		t.Fatalf("expected empty key allowed with custom base URL: %v", err)
	}
	if p.Name() != "openai" {
		t.Errorf("provider = %q, want openai", p.Name())
	}

	// Same for a custom Anthropic-style endpoint.
	if _, err := New(Options{Provider: "anthropic", Model: "claude-x", BaseURL: "http://localhost:8080/v1"}); err != nil {
		t.Errorf("expected empty key allowed with custom anthropic base URL: %v", err)
	}

	if _, err := New(Options{Provider: "responses", Model: "gpt-5.4", BaseURL: "http://localhost:8080/v1"}); err != nil {
		t.Errorf("expected empty key allowed with custom responses base URL: %v", err)
	}
	if _, err := New(Options{Provider: "interactions", Model: "gemini", BaseURL: "http://localhost:8080/v1beta"}); err != nil {
		t.Errorf("expected empty key allowed with custom Interactions base URL: %v", err)
	}
}

func TestUnknownProviderRejected(t *testing.T) {
	_, err := New(Options{Provider: "cohere", Model: "command-r", APIKey: "k"})
	if err == nil {
		t.Fatal("expected error for unknown provider")
	}
	if !strings.Contains(err.Error(), "cohere") {
		t.Errorf("error %q should name the unknown provider", err.Error())
	}
}

func TestMissingModelRejected(t *testing.T) {
	if _, err := New(Options{APIKey: "k"}); err == nil {
		t.Error("expected error for missing model")
	}
}

func TestReasoningModeInference(t *testing.T) {
	cases := []struct {
		name string
		opts Options
		want string
	}{
		{"openrouter provider name", Options{ProviderName: "openrouter", Provider: "openai"}, "openrouter"},
		{"openrouter base url", Options{Provider: "openai", BaseURL: "https://openrouter.ai/api/v1"}, "openrouter"},
		{"google provider name", Options{ProviderName: "google", Provider: "openai"}, "google"},
		{"google base url", Options{Provider: "openai", BaseURL: "https://generativelanguage.googleapis.com/v1beta/openai"}, "google"},
		{"openai default", Options{Provider: "openai"}, "openai"},
		{"anthropic", Options{Provider: "anthropic"}, "anthropic"},
		{"explicit wins", Options{Provider: "openai", ProviderName: "openrouter", ReasoningMode: "openai"}, "openai"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := reasoningMode(tc.opts.ProviderName, tc.opts.Provider, tc.opts.BaseURL, tc.opts.ReasoningMode); got != tc.want {
				t.Fatalf("reasoningMode = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestReasoningReplayReachesOpenAIDialect verifies the provider-config
// reasoning_replay quirk is threaded through the factory into the
// chat-completions wire body.
func TestReasoningReplayReachesOpenAIDialect(t *testing.T) {
	for _, tc := range []struct {
		name string
		mode llm.ReasoningReplay
		want bool
	}{
		{"full replays", llm.ReasoningReplayFull, true},
		{"default omits", "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var captured []byte
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				captured, _ = io.ReadAll(r.Body)
				w.Header().Set("Content-Type", "text/event-stream")
				w.WriteHeader(http.StatusOK)
				fmt.Fprintf(w, "data: {\"choices\":[{\"delta\":{\"content\":\"ok\"},\"finish_reason\":\"stop\"}]}\n\n")
				fmt.Fprintf(w, "data: [DONE]\n\n")
			}))
			defer srv.Close()

			p, err := New(Options{
				Provider:        "openai",
				ProviderName:    "kimi-for-coding",
				Model:           "kimi-k3",
				BaseURL:         srv.URL,
				APIKey:          "k",
				ReasoningReplay: tc.mode,
			})
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			req := llm.Request{
				Model:     "kimi-k3",
				Reasoning: llm.ReasoningConfig{Effort: "max"},
				Messages: []llm.Message{
					{Role: llm.RoleUser, Content: []llm.ContentBlock{{Kind: llm.BlockText, Text: "hi"}}},
					{Role: llm.RoleAssistant, Content: []llm.ContentBlock{
						{Kind: llm.BlockThinking, Thinking: "prior reasoning"},
						{Kind: llm.BlockText, Text: "answer"},
					}},
				},
			}
			for range p.Stream(context.Background(), req) {
			}
			got := bytes.Contains(captured, []byte(`"reasoning_content":"prior reasoning"`))
			if got != tc.want {
				t.Fatalf("reasoning_content present = %v, want %v; body: %s", got, tc.want, captured)
			}
		})
	}
}

// TestUsageInputIncludesCacheReachesAnthropicDialect verifies the
// usage_input_includes_cache provider quirk is threaded through the factory
// into Anthropic usage normalization.
func TestUsageInputIncludesCacheReachesAnthropicDialect(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		fmt.Fprintf(w, "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"usage\":{\"input_tokens\":208979,\"output_tokens\":1,\"cache_read_input_tokens\":208128}}}\n\n")
		fmt.Fprintf(w, "event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\n")
		fmt.Fprintf(w, "event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"ok\"}}\n\n")
		fmt.Fprintf(w, "event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\n")
		fmt.Fprintf(w, "event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"input_tokens\":208979,\"output_tokens\":2,\"cache_read_input_tokens\":208128}}\n\n")
		fmt.Fprintf(w, "event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")
	}))
	defer srv.Close()

	p, err := New(Options{Provider: "anthropic", Model: "kimi-k3", BaseURL: srv.URL, APIKey: "k", UsageInputIncludesCache: true})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	var done llm.StreamEvent
	for ev, err := range p.Stream(context.Background(), llm.Request{Model: "kimi-k3", Messages: []llm.Message{{Role: llm.RoleUser, Content: []llm.ContentBlock{{Kind: llm.BlockText, Text: "hi"}}}}}) {
		if err != nil {
			t.Fatalf("stream: %v", err)
		}
		if ev.Kind == llm.EventDone {
			done = ev
		}
	}
	if done.Usage == nil || done.Usage.InputTokens != 851 || done.Usage.CacheReadTokens != 208128 {
		t.Fatalf("usage = %+v, want uncached input 851 with cache read 208128", done.Usage)
	}
}
