package server

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"harness/internal/auth"
	"harness/internal/llm"
	"harness/internal/llm/factory"
	"harness/internal/llm/llmtest"
	"harness/internal/logging"
	"harness/internal/modelproxy/protocol"
)

func TestHandlerCatalogAndStreamResolveProviderConfig(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "openrouter.json"), []byte(`{
  "name": "openrouter",
  "api_type": "openai",
  "base_url": "https://openrouter.ai/api/v1",
  "api_key": "sk-file",
  "api_key_env": ["OPENROUTER_API_KEY"],
  "models": [
    {"name":"openai/gpt-5.5","context_window":1050000,"output_limit":64000,"input_modalities":["text","image"],"price":{"input":5,"output":30},"reasoning":true,"reasoning_options":[{"type":"effort","values":["low","medium","high"]}]}
  ]
}`), 0o600); err != nil {
		t.Fatalf("write provider config: %v", err)
	}

	var captured factory.Options
	fp := llmtest.New("fake", llmtest.Step{
		Events: []llm.StreamEvent{{Kind: llm.EventTextDelta, Text: "ok"}},
		Stop:   llm.StopEndTurn,
	})
	handler, err := NewHandler(Options{
		ConfigDir: dir,
		Config: Config{
			ProviderConfigs:      []string{"openrouter.json"},
			DefaultContextWindow: 512000,
		},
		Getenv: func(k string) string {
			if k == "OPENROUTER_API_KEY" {
				return "sk-env"
			}
			return ""
		},
		New: func(opts factory.Options) (llm.Provider, error) {
			captured = opts
			return fp, nil
		},
	})
	if err != nil {
		t.Fatalf("NewHandler: %v", err)
	}
	srv := httptest.NewServer(handler)
	defer srv.Close()

	resp, err := srv.Client().Get(srv.URL + "/v1/models")
	if err != nil {
		t.Fatalf("GET models: %v", err)
	}
	var catalog protocol.Catalog
	if err := json.NewDecoder(resp.Body).Decode(&catalog); err != nil {
		t.Fatalf("decode catalog: %v", err)
	}
	resp.Body.Close()
	if len(catalog.Providers) != 1 || catalog.Providers[0].ID != "openrouter" {
		t.Fatalf("catalog providers = %+v", catalog.Providers)
	}
	if len(catalog.Providers[0].Models) != 1 || catalog.Providers[0].Models[0].OutputLimit != 64_000 || !slices.Equal(catalog.Providers[0].Models[0].InputModalities, []string{"text", "image"}) {
		t.Fatalf("catalog models = %+v, want output limit 64000", catalog.Providers[0].Models)
	}

	body, _ := json.Marshal(protocol.StreamRequest{
		Provider: "openrouter",
		Request:  llm.Request{Model: "openai/gpt-5.5"},
	})
	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, srv.URL+"/v1/stream", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	resp, err = srv.Client().Do(req)
	if err != nil {
		t.Fatalf("POST stream: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	if captured.Provider != "openai" || captured.ProviderName != "openrouter" ||
		captured.BaseURL != "https://openrouter.ai/api/v1" || captured.APIKey != "sk-env" ||
		captured.ContextWindow != 1_050_000 || captured.OutputLimit != 64_000 {
		t.Fatalf("captured options = %+v", captured)
	}
	if len(fp.Requests) != 1 || fp.Requests[0].Model != "openai/gpt-5.5" {
		t.Fatalf("fake provider requests = %+v", fp.Requests)
	}
}

func TestLoadConfigParsesModelsDevCacheTTL(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	if err := os.WriteFile(path, []byte(`{
  "provider_configs": ["p.json"],
  "models_dev_cache_ttl": "12h"
}`), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig string ttl: %v", err)
	}
	if !cfg.ModelsDevCacheTTL.Set || cfg.ModelsDevCacheTTL.Duration != 12*time.Hour {
		t.Fatalf("string ttl = %+v, want 12h set", cfg.ModelsDevCacheTTL)
	}

	if err := os.WriteFile(path, []byte(`{
  "provider_configs": ["p.json"],
  "models_dev_cache_ttl": 0
}`), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err = LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig zero ttl: %v", err)
	}
	if !cfg.ModelsDevCacheTTL.Set || cfg.ModelsDevCacheTTL.Duration != 0 {
		t.Fatalf("zero ttl = %+v, want 0 set", cfg.ModelsDevCacheTTL)
	}
}

func TestHandlerStreamPassesResponseStateFields(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "openai.json"), []byte(`{
  "name": "openai",
  "api_type": "responses",
  "base_url": "https://api.openai.com/v1",
  "api_key": "sk-test",
  "models": [{"name":"gpt-5.5","context_window":128000}]
}`), 0o600); err != nil {
		t.Fatalf("write provider config: %v", err)
	}

	fp := llmtest.New("responses", llmtest.Step{
		Events:     []llm.StreamEvent{{Kind: llm.EventTextDelta, Text: "ok"}},
		Stop:       llm.StopEndTurn,
		ResponseID: "resp_1",
	})
	handler, err := NewHandler(Options{
		ConfigDir: dir,
		Config:    Config{ProviderConfigs: []string{"openai.json"}},
		New: func(factory.Options) (llm.Provider, error) {
			return fp, nil
		},
	})
	if err != nil {
		t.Fatalf("NewHandler: %v", err)
	}
	srv := httptest.NewServer(handler)
	defer srv.Close()

	body, _ := json.Marshal(protocol.StreamRequest{
		Provider: "openai",
		Request:  llm.Request{Model: "gpt-5.5", StoreResponse: true, PreviousResponseID: "resp_0"},
	})
	resp, err := srv.Client().Post(srv.URL+"/v1/stream", protocol.ContentTypeNDJSON, bytes.NewReader(body))
	if err != nil {
		t.Fatalf("POST stream: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d body=%s", resp.StatusCode, b)
	}
	if len(fp.Requests) != 1 || !fp.Requests[0].StoreResponse || fp.Requests[0].PreviousResponseID != "resp_0" {
		t.Fatalf("provider requests = %+v", fp.Requests)
	}
	var sawResponseID string
	dec := json.NewDecoder(resp.Body)
	for {
		var env protocol.StreamEnvelope
		if err := dec.Decode(&env); err != nil {
			if err == io.EOF {
				break
			}
			t.Fatalf("decode stream: %v", err)
		}
		if env.Event != nil && env.Event.Kind == llm.EventDone {
			sawResponseID = env.Event.ResponseID
		}
	}
	if sawResponseID != "resp_1" {
		t.Fatalf("response id = %q, want resp_1", sawResponseID)
	}
}

func TestHandlerCatalogMarksResponsesStatefulCapability(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "providers.json"), []byte(`[
  {
    "name": "openai",
    "api_type": "responses",
    "base_url": "https://api.openai.com/v1",
    "api_key": "sk-test",
    "models": [{"name":"gpt-5.5","context_window":128000}]
  },
  {
    "name": "openai-codex",
    "api_type": "responses",
    "base_url": "https://chatgpt.com/backend-api/codex",
    "auth": {"type":"codex_oauth"},
    "models": [{"name":"gpt-5.5","context_window":128000}]
  },
  {
    "name": "openrouter",
    "api_type": "openai",
    "base_url": "https://openrouter.ai/api/v1",
    "api_key": "sk-test",
    "models": [{"name":"openai/gpt-5.5","context_window":128000}]
  }
]`), 0o600); err != nil {
		t.Fatalf("write provider config: %v", err)
	}
	handler, err := NewHandler(Options{
		ConfigDir: dir,
		Config:    Config{ProviderConfigs: []string{"providers.json"}},
	})
	if err != nil {
		t.Fatalf("NewHandler: %v", err)
	}

	providers := map[string]protocol.Provider{}
	for _, p := range handler.Catalog().Providers {
		providers[p.ID] = p
	}
	if !providers["openai"].ResponsesStateful {
		t.Fatalf("openai ResponsesStateful = false, want true")
	}
	if providers["openai-codex"].ResponsesStateful {
		t.Fatalf("openai-codex ResponsesStateful = true, want false for %s", auth.TypeCodexOAuth)
	}
	if providers["openrouter"].ResponsesStateful {
		t.Fatalf("openrouter ResponsesStateful = true, want false")
	}
}

func TestHandlerStreamOmitsMaxOutputTokensForCodexOAuth(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "tokens"), 0o700); err != nil {
		t.Fatalf("mkdir tokens: %v", err)
	}
	token, err := json.Marshal(map[string]any{
		"access_token": "access-token",
		"account_id":   "account-123",
		"expiry":       time.Now().Add(time.Hour),
	})
	if err != nil {
		t.Fatalf("marshal token: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "tokens", "codex.json"), token, 0o600); err != nil {
		t.Fatalf("write token: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "openai-codex.json"), []byte(`{
  "name": "openai-codex",
  "api_type": "responses",
  "base_url": "https://chatgpt.com/backend-api/codex",
  "auth": {"type":"codex_oauth","token_file":"tokens/codex.json"},
  "models": [{"name":"gpt-5.5","context_window":1050000}]
}`), 0o600); err != nil {
		t.Fatalf("write provider config: %v", err)
	}

	var captured factory.Options
	handler, err := NewHandler(Options{
		ConfigDir: dir,
		Config:    Config{ProviderConfigs: []string{"openai-codex.json"}},
		New: func(opts factory.Options) (llm.Provider, error) {
			captured = opts
			return llmtest.New("fake", llmtest.Step{
				Events: []llm.StreamEvent{{Kind: llm.EventTextDelta, Text: "ok"}},
				Stop:   llm.StopEndTurn,
			}), nil
		},
	})
	if err != nil {
		t.Fatalf("NewHandler: %v", err)
	}
	srv := httptest.NewServer(handler)
	defer srv.Close()

	body, _ := json.Marshal(protocol.StreamRequest{
		Provider: "openai-codex",
		Request:  llm.Request{Model: "gpt-5.5"},
	})
	resp, err := srv.Client().Post(srv.URL+"/v1/stream", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("POST stream: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	if captured.Provider != "responses" || !captured.OmitMaxOutputTokens {
		t.Fatalf("captured options = %+v, want responses with OmitMaxOutputTokens", captured)
	}
}

func TestHandlerStreamResolvesProviderAuth(t *testing.T) {
	dir := t.TempDir()
	script := filepath.Join(dir, "token.sh")
	if err := os.WriteFile(script, []byte("#!/bin/sh\nprintf oauth-token\n"), 0o700); err != nil {
		t.Fatalf("write token script: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "oauth.json"), []byte(`{
  "name": "oauth",
  "api_type": "openai",
  "base_url": "https://oauth.example/v1",
  "api_key": "sk-file-should-not-win",
  "auth": {"type":"token_command","command":"`+script+`"},
  "models": [
    {"name":"model","context_window":128000}
  ]
}`), 0o600); err != nil {
		t.Fatalf("write provider config: %v", err)
	}

	var captured factory.Options
	handler, err := NewHandler(Options{
		ConfigDir: dir,
		Config: Config{
			ProviderConfigs: []string{"oauth.json"},
		},
		New: func(opts factory.Options) (llm.Provider, error) {
			captured = opts
			return llmtest.New("fake", llmtest.Step{
				Events: []llm.StreamEvent{{Kind: llm.EventTextDelta, Text: "ok"}},
				Stop:   llm.StopEndTurn,
			}), nil
		},
	})
	if err != nil {
		t.Fatalf("NewHandler: %v", err)
	}
	srv := httptest.NewServer(handler)
	defer srv.Close()

	body, _ := json.Marshal(protocol.StreamRequest{
		Provider: "oauth",
		Request:  llm.Request{Model: "model"},
	})
	resp, err := srv.Client().Post(srv.URL+"/v1/stream", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("POST stream: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	if captured.APIKey != "" {
		t.Fatalf("APIKey = %q, want empty when auth is configured", captured.APIKey)
	}
	if got := captured.AuthHeaders["Authorization"]; got != "Bearer oauth-token" {
		t.Fatalf("Authorization auth header = %q, want Bearer oauth-token; options=%+v", got, captured)
	}
}

func TestHandlerRejectsUnknownModel(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "openai.json"), []byte(`{
  "name": "openai",
  "api_type": "openai",
  "base_url": "http://localhost:11434/v1",
  "models": [{"name":"known","context_window":128000}]
}`), 0o600); err != nil {
		t.Fatalf("write provider config: %v", err)
	}
	handler, err := NewHandler(Options{
		ConfigDir: dir,
		Config:    Config{ProviderConfigs: []string{"openai.json"}},
	})
	if err != nil {
		t.Fatalf("NewHandler: %v", err)
	}
	srv := httptest.NewServer(handler)
	defer srv.Close()

	body, _ := json.Marshal(protocol.StreamRequest{
		Provider: "openai",
		Request:  llm.Request{Model: "missing"},
	})
	resp, err := srv.Client().Post(srv.URL+"/v1/stream", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("POST stream: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
	var wireErr protocol.Error
	if err := json.NewDecoder(resp.Body).Decode(&wireErr); err != nil {
		t.Fatalf("decode error: %v", err)
	}
	if wireErr.Message == "" {
		t.Fatalf("expected error message")
	}
}

func TestHandlerRequiresExplicitProviderAndModel(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "openai.json"), []byte(`{
  "name": "openai",
  "api_type": "openai",
  "base_url": "http://localhost:11434/v1",
  "models": [{"name":"known","context_window":128000}]
}`), 0o600); err != nil {
		t.Fatalf("write provider config: %v", err)
	}
	handler, err := NewHandler(Options{
		ConfigDir: dir,
		Config:    Config{ProviderConfigs: []string{"openai.json"}},
	})
	if err != nil {
		t.Fatalf("NewHandler: %v", err)
	}
	srv := httptest.NewServer(handler)
	defer srv.Close()

	body, _ := json.Marshal(protocol.StreamRequest{Request: llm.Request{Model: "known"}})
	resp, err := srv.Client().Post(srv.URL+"/v1/stream", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("POST stream: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("missing provider status = %d, want 400", resp.StatusCode)
	}

	body, _ = json.Marshal(protocol.StreamRequest{Provider: "openai"})
	resp, err = srv.Client().Post(srv.URL+"/v1/stream", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("POST stream: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("missing model status = %d, want 400", resp.StatusCode)
	}
}

func TestHandlerLogsStreamStats(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "openai.json"), []byte(`{
  "name": "openai",
  "api_type": "openai",
  "base_url": "http://localhost:11434/v1",
  "models": [{"name":"priced","context_window":128000,"price":{"input":2,"output":4,"cache_read":0.5,"cache_write":1}}]
}`), 0o600); err != nil {
		t.Fatalf("write provider config: %v", err)
	}

	var logs bytes.Buffer
	logger, err := logging.NewProxyLogger(&logs, logging.LevelInfo, logging.FormatJSON)
	if err != nil {
		t.Fatalf("NewProxyLogger: %v", err)
	}
	handler, err := NewHandler(Options{
		ConfigDir: dir,
		Config:    Config{ProviderConfigs: []string{"openai.json"}},
		Logger:    logger,
		New: func(factory.Options) (llm.Provider, error) {
			return llmtest.New("fake", llmtest.Step{
				Events: []llm.StreamEvent{
					{Kind: llm.EventTextDelta, Text: "ok"},
					{Kind: llm.EventToolCallDone, ToolName: "x"},
				},
				Stop:  llm.StopToolUse,
				Usage: llm.Usage{InputTokens: 1000, OutputTokens: 2000, CacheReadTokens: 3000, CacheWriteTokens: 4000, ReasoningTokens: 500},
			}), nil
		},
	})
	if err != nil {
		t.Fatalf("NewHandler: %v", err)
	}
	srv := httptest.NewServer(handler)
	defer srv.Close()

	body, _ := json.Marshal(protocol.StreamRequest{
		Provider: "openai",
		Request:  llm.Request{Model: "priced"},
	})
	req, err := http.NewRequest(http.MethodPost, srv.URL+"/v1/stream", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("X-Harness-Requester", "test-client")
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatalf("POST stream: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	_, _ = io.Copy(io.Discard, resp.Body)

	var record map[string]any
	if err := json.Unmarshal(logs.Bytes(), &record); err != nil {
		t.Fatalf("decode log %q: %v", logs.String(), err)
	}
	for k, want := range map[string]any{
		"msg":              "model request completed",
		"requester":        "test-client",
		"provider":         "openai",
		"api_type":         "openai",
		"model":            "priced",
		"status":           float64(http.StatusOK),
		"input_tokens":     float64(1000),
		"output_tokens":    float64(2000),
		"reasoning_tokens": float64(500),
		"tool_calls":       float64(1),
		"stop_reason":      string(llm.StopToolUse),
	} {
		if got := record[k]; got != want {
			t.Fatalf("log[%s] = %v (%T), want %v", k, got, got, want)
		}
	}
	if record["cost_usd"] == nil {
		t.Fatalf("log missing cost_usd: %+v", record)
	}
	if record["request_bytes"].(float64) <= 0 || record["response_bytes"].(float64) <= 0 {
		t.Fatalf("log sizes not populated: %+v", record)
	}
}

func TestHandlerUsageAggregatesKnownCostRequests(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "openai.json"), []byte(`{
  "name": "openai",
  "api_type": "openai",
  "base_url": "http://localhost:11434/v1",
  "models": [
    {"name":"priced","context_window":128000,"price":{"input":2,"output":4,"cache_read":0.5,"cache_write":1}},
    {"name":"free","context_window":128000}
  ]
}`), 0o600); err != nil {
		t.Fatalf("write provider config: %v", err)
	}

	usage := llm.Usage{InputTokens: 1000, OutputTokens: 2000, CacheReadTokens: 3000, CacheWriteTokens: 4000, ReasoningTokens: 500}
	handler, err := NewHandler(Options{
		ConfigDir: dir,
		Config:    Config{ProviderConfigs: []string{"openai.json"}},
		New: func(factory.Options) (llm.Provider, error) {
			return llmtest.New("fake", llmtest.Step{
				Events: []llm.StreamEvent{{Kind: llm.EventTextDelta, Text: "ok"}},
				Stop:   llm.StopEndTurn,
				Usage:  usage,
			}), nil
		},
	})
	if err != nil {
		t.Fatalf("NewHandler: %v", err)
	}
	srv := httptest.NewServer(handler)
	defer srv.Close()

	stream := func(model string) {
		body, _ := json.Marshal(protocol.StreamRequest{
			Provider: "openai",
			Request:  llm.Request{Model: model},
		})
		resp, err := srv.Client().Post(srv.URL+"/v1/stream", protocol.ContentTypeNDJSON, bytes.NewReader(body))
		if err != nil {
			t.Fatalf("POST stream %s: %v", model, err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("stream %s status = %d", model, resp.StatusCode)
		}
		_, _ = io.Copy(io.Discard, resp.Body)
	}
	stream("priced")
	stream("priced")
	stream("free") // unknown price: must not appear in the aggregate

	resp, err := srv.Client().Get(srv.URL + "/v1/usage")
	if err != nil {
		t.Fatalf("GET usage: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("usage status = %d, want 200", resp.StatusCode)
	}
	var report protocol.UsageReport
	if err := json.NewDecoder(resp.Body).Decode(&report); err != nil {
		t.Fatalf("decode usage report: %v", err)
	}
	if len(report.Models) != 1 {
		t.Fatalf("usage models = %+v, want exactly the priced model", report.Models)
	}
	got := report.Models[0]
	want := protocol.ModelUsage{
		Provider:         "openai",
		Model:            "priced",
		Requests:         2,
		InputTokens:      2000,
		OutputTokens:     4000,
		CacheReadTokens:  6000,
		CacheWriteTokens: 8000,
		ReasoningTokens:  1000,
	}
	if got.Provider != want.Provider || got.Model != want.Model || got.Requests != want.Requests ||
		got.InputTokens != want.InputTokens || got.OutputTokens != want.OutputTokens ||
		got.CacheReadTokens != want.CacheReadTokens || got.CacheWriteTokens != want.CacheWriteTokens ||
		got.ReasoningTokens != want.ReasoningTokens {
		t.Fatalf("usage entry = %+v, want %+v (cost aside)", got, want)
	}
	// Two priced requests at the configured prices.
	perReq := 1000.0/1e6*2 + 2000.0/1e6*4 + 3000.0/1e6*0.5 + 4000.0/1e6*1
	wantCost := 2 * perReq
	if diff := got.CostUSD - wantCost; diff > 1e-9 || diff < -1e-9 {
		t.Fatalf("usage cost = %v, want %v", got.CostUSD, wantCost)
	}
}

func TestHandlerUsageEmptyBeforeAnyRequest(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "openai.json"), []byte(`{
  "name": "openai",
  "api_type": "openai",
  "base_url": "http://localhost:11434/v1",
  "models": [{"name":"priced","context_window":128000,"price":{"input":2,"output":4}}]
}`), 0o600); err != nil {
		t.Fatalf("write provider config: %v", err)
	}
	handler, err := NewHandler(Options{
		ConfigDir: dir,
		Config:    Config{ProviderConfigs: []string{"openai.json"}},
	})
	if err != nil {
		t.Fatalf("NewHandler: %v", err)
	}
	srv := httptest.NewServer(handler)
	defer srv.Close()

	resp, err := srv.Client().Get(srv.URL + "/v1/usage")
	if err != nil {
		t.Fatalf("GET usage: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("usage status = %d, want 200", resp.StatusCode)
	}
	var report protocol.UsageReport
	if err := json.NewDecoder(resp.Body).Decode(&report); err != nil {
		t.Fatalf("decode usage report: %v", err)
	}
	if len(report.Models) != 0 {
		t.Fatalf("usage models = %+v, want empty", report.Models)
	}
}

func TestHandlerCatalogStampsPricingStaleness(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "openai.json")
	if err := os.WriteFile(cfgPath, []byte(`{
  "name": "openai",
  "api_type": "openai",
  "base_url": "http://localhost:11434/v1",
  "models": [{"name":"priced","context_window":128000,"price":{"input":2,"output":4}}]
}`), 0o600); err != nil {
		t.Fatalf("write provider config: %v", err)
	}
	info, err := os.Stat(cfgPath)
	if err != nil {
		t.Fatalf("stat config: %v", err)
	}
	wantModTime := info.ModTime()

	handler, err := NewHandler(Options{
		ConfigDir:     dir,
		Config:        Config{ProviderConfigs: []string{"openai.json"}},
		PricingMaxAge: 24 * time.Hour,
	})
	if err != nil {
		t.Fatalf("NewHandler: %v", err)
	}
	srv := httptest.NewServer(handler)
	defer srv.Close()

	resp, err := srv.Client().Get(srv.URL + "/v1/models")
	if err != nil {
		t.Fatalf("GET models: %v", err)
	}
	var catalog protocol.Catalog
	if err := json.NewDecoder(resp.Body).Decode(&catalog); err != nil {
		t.Fatalf("decode catalog: %v", err)
	}
	resp.Body.Close()

	if catalog.Pricing == nil {
		t.Fatalf("catalog.Pricing = nil, want a stamped source date")
	}
	if !catalog.Pricing.SourceDate.Equal(wantModTime) {
		t.Fatalf("pricing source date = %v, want config mtime %v", catalog.Pricing.SourceDate, wantModTime)
	}
	if catalog.Pricing.MaxAgeSeconds != int64((24 * time.Hour).Seconds()) {
		t.Fatalf("pricing max age = %d, want 86400", catalog.Pricing.MaxAgeSeconds)
	}
	if catalog.Pricing.Stale(wantModTime.Add(23 * time.Hour)) {
		t.Fatalf("pricing reported stale within the TTL window")
	}
	if !catalog.Pricing.Stale(wantModTime.Add(25 * time.Hour)) {
		t.Fatalf("pricing not reported stale past the TTL window")
	}
}

func TestNewHandlerPricingMaxAgeFallsBackToConfigTTL(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "openai.json"), []byte(`{
  "name": "openai",
  "api_type": "openai",
  "base_url": "http://localhost:11434/v1",
  "models": [{"name":"priced","context_window":128000,"price":{"input":2,"output":4}}]
}`), 0o600); err != nil {
		t.Fatalf("write provider config: %v", err)
	}
	handler, err := NewHandler(Options{
		ConfigDir: dir,
		Config: Config{
			ProviderConfigs:   []string{"openai.json"},
			ModelsDevCacheTTL: Duration{Duration: 12 * time.Hour, Set: true},
		},
	})
	if err != nil {
		t.Fatalf("NewHandler: %v", err)
	}
	pricing := handler.Catalog().Pricing
	if pricing == nil {
		t.Fatalf("catalog.Pricing = nil, want config TTL fallback")
	}
	if pricing.MaxAgeSeconds != int64((12 * time.Hour).Seconds()) {
		t.Fatalf("pricing max age = %d, want 43200 from config TTL", pricing.MaxAgeSeconds)
	}
}

func TestHandlerLogsStreamErrorDetails(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "openai.json"), []byte(`{
  "name": "openai",
  "api_type": "openai",
  "base_url": "http://localhost:11434/v1",
  "models": [{"name":"known","context_window":128000}]
}`), 0o600); err != nil {
		t.Fatalf("write provider config: %v", err)
	}

	var logs bytes.Buffer
	logger, err := logging.NewProxyLogger(&logs, logging.LevelInfo, logging.FormatJSON)
	if err != nil {
		t.Fatalf("NewProxyLogger: %v", err)
	}
	providerErr := &llm.APIError{
		Code:       "server_error",
		Message:    "upstream exploded",
		Retryable:  true,
		RetryAfter: 250 * time.Millisecond,
	}
	handler, err := NewHandler(Options{
		ConfigDir: dir,
		Config:    Config{ProviderConfigs: []string{"openai.json"}},
		Logger:    logger,
		New: func(factory.Options) (llm.Provider, error) {
			return llmtest.New("fake", llmtest.Step{Err: providerErr}), nil
		},
	})
	if err != nil {
		t.Fatalf("NewHandler: %v", err)
	}
	srv := httptest.NewServer(handler)
	defer srv.Close()

	body, _ := json.Marshal(protocol.StreamRequest{
		Provider: "openai",
		Request:  llm.Request{Model: "known"},
	})
	resp, err := srv.Client().Post(srv.URL+"/v1/stream", protocol.ContentTypeNDJSON, bytes.NewReader(body))
	if err != nil {
		t.Fatalf("POST stream: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var env protocol.StreamEnvelope
	if err := json.NewDecoder(resp.Body).Decode(&env); err != nil {
		t.Fatalf("decode stream envelope: %v", err)
	}
	if env.Error == nil || env.Error.RetryAfterMS != 250 {
		t.Fatalf("stream error = %+v, want retry_after_ms 250", env.Error)
	}
	if _, err := io.Copy(io.Discard, resp.Body); err != nil {
		t.Fatalf("drain response body: %v", err)
	}

	var record map[string]any
	if err := json.Unmarshal(logs.Bytes(), &record); err != nil {
		t.Fatalf("decode log %q: %v", logs.String(), err)
	}
	for k, want := range map[string]any{
		"level":              "WARN",
		"msg":                "model request completed",
		"provider":           "openai",
		"api_type":           "openai",
		"model":              "known",
		"status":             float64(http.StatusOK),
		"err":                "api error 0 (server_error): upstream exploded",
		"err_kind":           "api",
		"err_go_type":        "*llm.APIError",
		"api_status_code":    float64(0),
		"api_code":           "server_error",
		"api_retryable":      true,
		"api_retry_after_ms": float64(250),
		"events":             float64(0),
		"tool_calls":         float64(0),
	} {
		if got := record[k]; got != want {
			t.Fatalf("log[%s] = %v (%T), want %v", k, got, got, want)
		}
	}
	if record["request_id"].(float64) <= 0 {
		t.Fatalf("request_id not populated: %+v", record)
	}
}
