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

func TestCatalogAndRegistry(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/v1/models" {
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
		}
		_ = json.NewEncoder(w).Encode(protocol.Catalog{
			Providers: []protocol.Provider{{
				ID: "openrouter",
				Models: []protocol.Model{{
					ID:            "openai/gpt-5.5",
					ContextWindow: 1_050_000,
					OutputLimit:   64_000,
					Price:         llm.Price{Input: 5, Output: 30},
				}},
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
	if len(catalog.Providers) != 1 || catalog.Providers[0].ID != "openrouter" {
		t.Fatalf("catalog providers = %+v", catalog.Providers)
	}
	registry := Registry(catalog)
	if got := registry.ContextWindow("openrouter:openai/gpt-5.5"); got != 1_050_000 {
		t.Fatalf("qualified context window = %d, want 1050000", got)
	}
	if got := registry.OutputLimit("openrouter:openai/gpt-5.5"); got != 64_000 {
		t.Fatalf("qualified output limit = %d, want 64000", got)
	}
}

func TestRegistryInfersOpenRouterReasoningControls(t *testing.T) {
	registry := Registry(protocol.Catalog{
		Providers: []protocol.Provider{{
			ID: "openrouter",
			Models: []protocol.Model{{
				ID: "z-ai/glm-5.1",
				Reasoning: &llm.ReasoningInfo{
					Supported: true,
					Options:   []llm.ReasoningOption{},
				},
			}},
		}},
	})

	info, ok := registry.Lookup("openrouter:z-ai/glm-5.1")
	if !ok || info.Reasoning == nil {
		t.Fatalf("reasoning info = %+v, ok=%v", info.Reasoning, ok)
	}
	if !info.Reasoning.SupportsEffort("xhigh") || !info.Reasoning.SupportsEffort("none") {
		t.Fatalf("openrouter inferred efforts = %+v, want xhigh and none", info.Reasoning)
	}
	if info.Reasoning.SupportsEffort("max") {
		t.Fatalf("openrouter inferred efforts should not accept max: %+v", info.Reasoning)
	}
	if !info.Reasoning.SupportsToggle() {
		t.Fatalf("openrouter inferred controls should include toggle: %+v", info.Reasoning)
	}
	if !info.Reasoning.SupportsBudgetTokens(2048) {
		t.Fatalf("openrouter inferred controls should include budget_tokens: %+v", info.Reasoning)
	}
}

func TestProviderStreamEventsAndErrors(t *testing.T) {
	var sawProvider string
	var sawRequest llm.Request
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1/stream" {
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
		}
		var req protocol.StreamRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		sawProvider = req.Provider
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
	req := llm.Request{Model: "gpt-5.5", StoreResponse: true, PreviousResponseID: "resp_0"}
	for ev, err := range c.Provider("openai").Stream(context.Background(), req) {
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
	if sawProvider != "openai" {
		t.Fatalf("provider sent to proxy = %q", sawProvider)
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
