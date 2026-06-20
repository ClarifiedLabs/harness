package responses

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"harness/internal/llm"
)

func TestCountInputTokens(t *testing.T) {
	var gotPath, gotAuth string
	var gotBody countRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		if err := json.NewDecoder(r.Body).Decode(&gotBody); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		_ = json.NewEncoder(w).Encode(countResponse{InputTokens: 1234})
	}))
	defer srv.Close()

	p := New(Config{APIKey: "sk-test", BaseURL: srv.URL})
	got, err := p.CountInputTokens(context.Background(), llm.Request{
		Model:  "gpt-5.5",
		System: "system",
		Messages: []llm.Message{{
			Role:    llm.RoleUser,
			Content: []llm.ContentBlock{{Kind: llm.BlockText, Text: "hello"}},
		}},
	})
	if err != nil {
		t.Fatalf("CountInputTokens: %v", err)
	}
	if got.InputTokens != 1234 || got.Source != "responses" {
		t.Fatalf("count = %+v, want 1234 responses", got)
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
}
