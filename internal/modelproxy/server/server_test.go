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
    {"name":"openai/gpt-5.5","context_window":1050000,"price":{"input":5,"output":30},"reasoning":true,"reasoning_options":[{"type":"effort","values":["low","medium","high"]}]}
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
		captured.ContextWindow != 1_050_000 {
		t.Fatalf("captured options = %+v", captured)
	}
	if len(fp.Requests) != 1 || fp.Requests[0].Model != "openai/gpt-5.5" {
		t.Fatalf("fake provider requests = %+v", fp.Requests)
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
