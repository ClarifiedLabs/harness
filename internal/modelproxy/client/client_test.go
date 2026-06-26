package client

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"harness/internal/llm"
	"harness/internal/modelproxy/protocol"
)

func TestCatalogSendsAuthorizationHeader(t *testing.T) {
	var auth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth = r.Header.Get("Authorization")
		_ = json.NewEncoder(w).Encode(protocol.Catalog{})
	}))
	defer srv.Close()

	c, err := New(srv.URL, srv.Client(), WithAPIKey("hmp_secret"))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := c.Catalog(context.Background()); err != nil {
		t.Fatalf("Catalog: %v", err)
	}
	if auth != "Bearer hmp_secret" {
		t.Fatalf("Authorization header = %q, want Bearer hmp_secret", auth)
	}
}

func TestCatalogAndRegistry(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/v1/models" {
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
		}
		_ = json.NewEncoder(w).Encode(protocol.Catalog{
			Targets: []protocol.Target{{
				ID:              "openrouter:openai/gpt-5.5",
				Aliases:         []string{"openai/gpt-5.5"},
				ContextWindow:   1_050_000,
				OutputLimit:     64_000,
				InputModalities: []string{"text", "image"},
				Price:           llm.Price{Input: 5, Output: 30},
			}},
		})
	}))
	defer srv.Close()

	c, err := New(srv.URL, srv.Client())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	catalog, err := c.Catalog(context.Background())
	if err != nil {
		t.Fatalf("Catalog: %v", err)
	}
	if len(catalog.Targets) != 1 || catalog.Targets[0].ID != "openrouter:openai/gpt-5.5" {
		t.Fatalf("catalog targets = %+v", catalog.Targets)
	}
	registry := Registry(catalog)
	if got := registry.ContextWindow("openrouter:openai/gpt-5.5"); got != 1_050_000 {
		t.Fatalf("qualified context window = %d, want 1050000", got)
	}
	if got := registry.OutputLimit("openrouter:openai/gpt-5.5"); got != 64_000 {
		t.Fatalf("qualified output limit = %d, want 64000", got)
	}
	if !registry.SupportsInputModality("openrouter:openai/gpt-5.5", "image") {
		t.Fatalf("qualified model should support image input")
	}
}

func TestRegistryUsesTargetReasoningProfiles(t *testing.T) {
	registry := Registry(protocol.Catalog{
		Targets: []protocol.Target{{
			ID: "openrouter:z-ai/glm-5.1",
			Reasoning: &protocol.ReasoningProfiles{
				Supported: true,
				Profiles:  []string{"none", "low", "medium", "high", "xhigh", "max"},
			},
		}},
	})

	info, ok := registry.Lookup("openrouter:z-ai/glm-5.1")
	if !ok || info.Reasoning == nil {
		t.Fatalf("reasoning info = %+v, ok=%v", info.Reasoning, ok)
	}
	if !info.Reasoning.SupportsEffort("xhigh") || !info.Reasoning.SupportsEffort("none") {
		t.Fatalf("target reasoning efforts = %+v, want xhigh and none", info.Reasoning)
	}
}

func TestProviderStreamEventsAndErrors(t *testing.T) {
	var sawTarget string
	var sawProfile string
	var sawRequest llm.Request
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1/stream" {
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
		}
		var req protocol.StreamRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		sawTarget = req.TargetID
		sawProfile = req.ReasoningProfile
		sawRequest = req.Request
		w.Header().Set("content-type", protocol.ContentTypeNDJSON)
		enc := json.NewEncoder(w)
		text := llm.StreamEvent{Kind: llm.EventTextDelta, Text: "hello"}
		_ = enc.Encode(protocol.StreamEnvelope{Event: &text})
		done := llm.StreamEvent{Kind: llm.EventDone, ResponseID: "resp_1"}
		_ = enc.Encode(protocol.StreamEnvelope{Event: &done})
		_ = enc.Encode(protocol.StreamEnvelope{Error: &protocol.Error{
			StatusCode:   http.StatusTooManyRequests,
			Code:         "rate_limit",
			Message:      "slow down",
			Retryable:    true,
			RetryAfterMS: 250,
		}})
	}))
	defer srv.Close()

	c, err := New(srv.URL, srv.Client())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	var texts []string
	var responseID string
	var gotErr error
	req := llm.Request{Model: "gpt-5.5", Reasoning: llm.ReasoningConfig{Effort: "xhigh"}, StoreResponse: true, PreviousResponseID: "resp_0"}
	for ev, err := range c.Provider("openai:gpt-5.5").Stream(context.Background(), req) {
		if err != nil {
			gotErr = err
			break
		}
		if ev.Kind == llm.EventTextDelta {
			texts = append(texts, ev.Text)
		}
		if ev.Kind == llm.EventDone {
			responseID = ev.ResponseID
		}
	}
	if sawTarget != "openai:gpt-5.5" || sawProfile != "xhigh" {
		t.Fatalf("target/profile sent to proxy = %q/%q", sawTarget, sawProfile)
	}
	if len(texts) != 1 || texts[0] != "hello" {
		t.Fatalf("texts = %v", texts)
	}
	if !sawRequest.StoreResponse || sawRequest.PreviousResponseID != "resp_0" {
		t.Fatalf("request passthrough = %+v", sawRequest)
	}
	if responseID != "resp_1" {
		t.Fatalf("response id = %q, want resp_1", responseID)
	}
	var apiErr *llm.APIError
	if !errors.As(gotErr, &apiErr) || apiErr.StatusCode != http.StatusTooManyRequests || !apiErr.Retryable {
		t.Fatalf("error = %v, want retryable APIError 429", gotErr)
	}
	if apiErr.RetryAfter != 250*time.Millisecond {
		t.Fatalf("retry after = %v, want 250ms", apiErr.RetryAfter)
	}
}

func TestProviderCountInputTokens(t *testing.T) {
	var sawTarget string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1/input_tokens" {
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
		}
		var req protocol.TokenCountRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		sawTarget = req.TargetID
		_ = json.NewEncoder(w).Encode(protocol.TokenCountResponse{InputTokens: 3456, Source: "proxy"})
	}))
	defer srv.Close()

	c, err := New(srv.URL, srv.Client())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	got, err := c.Provider("openai:gpt-5.5").(llm.InputTokenCounter).CountInputTokens(context.Background(), llm.Request{Model: "gpt-5.5"})
	if err != nil {
		t.Fatalf("CountInputTokens: %v", err)
	}
	if sawTarget != "openai:gpt-5.5" || got.InputTokens != 3456 || got.Source != "proxy" {
		t.Fatalf("target/count = %q/%+v", sawTarget, got)
	}
}

func TestProviderCountInputTokensUnsupported(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotImplemented)
		_ = json.NewEncoder(w).Encode(protocol.Error{
			StatusCode: http.StatusNotImplemented,
			Code:       "input_token_count_unsupported",
			Message:    llm.ErrInputTokenCountUnsupported.Error(),
		})
	}))
	defer srv.Close()

	c, err := New(srv.URL, srv.Client())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	_, err = c.Provider("openai").(llm.InputTokenCounter).CountInputTokens(context.Background(), llm.Request{Model: "gpt-5.5"})
	if !errors.Is(err, llm.ErrInputTokenCountUnsupported) {
		t.Fatalf("err = %v, want ErrInputTokenCountUnsupported", err)
	}
}
