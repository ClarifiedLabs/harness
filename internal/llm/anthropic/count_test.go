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
	var gotPath, gotKey, gotVersion string
	var gotBody countRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotKey = r.Header.Get("x-api-key")
		gotVersion = r.Header.Get("anthropic-version")
		if err := json.NewDecoder(r.Body).Decode(&gotBody); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		_ = json.NewEncoder(w).Encode(countResponse{InputTokens: 2345})
	}))
	defer srv.Close()

	p := New(Config{APIKey: "sk-ant", BaseURL: srv.URL})
	got, err := p.CountInputTokens(context.Background(), llm.Request{
		Model:  "claude-sonnet-4-5",
		System: "system",
		Messages: []llm.Message{{
			Role:    llm.RoleUser,
			Content: []llm.ContentBlock{{Kind: llm.BlockText, Text: "hello"}},
		}},
	})
	if err != nil {
		t.Fatalf("CountInputTokens: %v", err)
	}
	if got.InputTokens != 2345 || got.Source != "anthropic" {
		t.Fatalf("count = %+v, want 2345 anthropic", got)
	}
	if gotPath != "/v1/messages/count_tokens" {
		t.Fatalf("path = %q, want /v1/messages/count_tokens", gotPath)
	}
	if gotKey != "sk-ant" || gotVersion == "" {
		t.Fatalf("headers x-api-key=%q anthropic-version=%q", gotKey, gotVersion)
	}
	if gotBody.Model != "claude-sonnet-4-5" || len(gotBody.System) != 1 || len(gotBody.Messages) != 1 {
		t.Fatalf("count request body = %+v", gotBody)
	}
}
