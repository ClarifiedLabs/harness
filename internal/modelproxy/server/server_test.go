package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"

	"harness/internal/apikey"
	"harness/internal/llm"
	"harness/internal/llm/factory"
	"harness/internal/llm/llmtest"
	"harness/internal/logging"
	"harness/internal/metrics"
	"harness/internal/modelproxy/protocol"
	"harness/internal/tracing"
)

type countingFakeProvider struct {
	*llmtest.FakeProvider
	count int
	err   error
}

func (p *countingFakeProvider) CountInputTokens(context.Context, llm.Request) (llm.InputTokenCount, error) {
	if p.err != nil {
		return llm.InputTokenCount{}, p.err
	}
	return llm.InputTokenCount{InputTokens: p.count, Source: "test"}, nil
}

func TestHandlerCatalogAndStreamResolveProviderConfig(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "openrouter.json"), []byte(`{
  "name": "openrouter",
  "api_type": "openai",
  "base_url": "https://openrouter.ai/api/v1",
  "api_key": "sk-file",
  "api_key_env": ["OPENROUTER_API_KEY"],
  "service_tiers": [
    {"id":"default"},
    {"id":"fast","name":"Fast","request":{"service_tier":"priority"}}
  ],
  "min_output_tokens": 16,
  "prompt_cache": {"key_field":"session_id","affinity_headers":["x-session-id"]},
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
	if len(catalog.Targets) != 2 || catalog.Targets[0].ID != "openrouter:openai/gpt-5.5" {
		t.Fatalf("catalog targets = %+v", catalog.Targets)
	}
	if catalog.Targets[0].OutputLimit != 64_000 || !slices.Equal(catalog.Targets[0].InputModalities, []string{"text", "image"}) {
		t.Fatalf("catalog target = %+v, want output limit 64000", catalog.Targets[0])
	}
	if !slices.Equal(catalog.Targets[0].ServerTools, []string{llm.ServerToolWebSearch}) {
		t.Fatalf("catalog server tools = %+v, want web_search", catalog.Targets[0].ServerTools)
	}
	fastTarget := catalog.Targets[1]
	if fastTarget.ID != "openrouter:openai/gpt-5.5:fast" || fastTarget.BaseTargetID != catalog.Targets[0].ID || fastTarget.Variant != "fast" {
		t.Fatalf("fast catalog target = %+v", fastTarget)
	}

	body, _ := json.Marshal(protocol.StreamRequest{
		TargetID: fastTarget.ID,
		Request: llm.Request{
			Model:       fastTarget.ID,
			ServiceTier: "caller-value-is-ignored",
			ServerTools: []llm.ServerTool{{Name: llm.ServerToolWebSearch}},
		},
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
		captured.ContextWindow != 1_050_000 || captured.OutputLimit != 64_000 ||
		captured.MinOutputTokens != 16 {
		t.Fatalf("captured options = %+v", captured)
	}
	if captured.PromptCache.KeyField != "session_id" || len(captured.PromptCache.AffinityHeaders) != 1 || captured.PromptCache.AffinityHeaders[0] != "x-session-id" {
		t.Fatalf("captured prompt cache = %+v, want session_id/x-session-id", captured.PromptCache)
	}
	if len(fp.Requests) != 1 || fp.Requests[0].Model != "openai/gpt-5.5" {
		t.Fatalf("fake provider requests = %+v", fp.Requests)
	}
	if len(fp.Requests[0].ServerTools) != 1 || fp.Requests[0].ServerTools[0].Kind != llm.ServerToolKindOpenRouterWebSearch {
		t.Fatalf("fake provider server tools = %+v", fp.Requests[0].ServerTools)
	}
	if fp.Requests[0].ServiceTier != "priority" {
		t.Fatalf("fake provider service tier = %q, want priority", fp.Requests[0].ServiceTier)
	}
}

func TestApplyServiceTierForTargetUsesResolvedVariant(t *testing.T) {
	base := resolvedTarget{targetID: "custom:model", baseTargetID: "custom:model", entry: llm.ModelEntry{Name: "model"}}
	req := llm.Request{ServiceTier: "caller-value", Speed: "caller-speed", Betas: []string{"caller-beta"}}
	if err := applyServiceTierForTarget(base, &req); err != nil {
		t.Fatalf("apply base target: %v", err)
	}
	if req.ServiceTier != "" || req.Speed != "" || req.Betas != nil {
		t.Fatalf("base request retained caller tier fields: %+v", req)
	}

	fast := base
	fast.targetID += ":fast"
	fast.variant = "fast"
	fast.serviceTier = llm.ServiceTier{
		ID:      "fast",
		Request: llm.ServiceTierRequest{Speed: "fast", Betas: []string{"fast-mode-2026-02-01"}},
	}
	req = llm.Request{ServiceTier: "caller-value"}
	if err := applyServiceTierForTarget(fast, &req); err != nil {
		t.Fatalf("apply fast target: %v", err)
	}
	if req.ServiceTier != "" || req.Speed != "fast" || !slices.Equal(req.Betas, []string{"fast-mode-2026-02-01"}) {
		t.Fatalf("fast request mapping = %+v", req)
	}

	priority := fast
	priority.serviceTier.Request = llm.ServiceTierRequest{ServiceTier: "priority"}
	req = llm.Request{Speed: "caller-speed"}
	if err := applyServiceTierForTarget(priority, &req); err != nil {
		t.Fatalf("apply priority target: %v", err)
	}
	if req.ServiceTier != "priority" || req.Speed != "" || req.Betas != nil {
		t.Fatalf("priority request mapping = %+v", req)
	}
}

func TestHandlerStreamRetriesWithoutServerToolsOnRejection(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "openrouter.json"), []byte(`{
  "name": "openrouter",
  "api_type": "openai",
  "base_url": "https://openrouter.ai/api/v1",
  "api_key": "sk-file",
  "models": [{"name":"openai/gpt-5.5","context_window":1000000}]
}`), 0o600); err != nil {
		t.Fatalf("write provider config: %v", err)
	}

	// First attempt fails with a provider error about the web_search tool; the
	// proxy must retry once without server tools and stream the second attempt.
	fp := llmtest.New("fake",
		llmtest.Step{Err: &llm.APIError{StatusCode: http.StatusBadRequest, Message: "unsupported tool web_search"}},
		llmtest.Step{Events: []llm.StreamEvent{{Kind: llm.EventTextDelta, Text: "ok"}}, Stop: llm.StopEndTurn},
	)
	handler, err := NewHandler(Options{
		ConfigDir: dir,
		Config:    Config{ProviderConfigs: []string{"openrouter.json"}, DefaultContextWindow: 512000},
		New:       func(opts factory.Options) (llm.Provider, error) { return fp, nil },
	})
	if err != nil {
		t.Fatalf("NewHandler: %v", err)
	}
	srv := httptest.NewServer(handler)
	defer srv.Close()

	body, _ := json.Marshal(protocol.StreamRequest{
		TargetID: "openrouter:openai/gpt-5.5",
		Request: llm.Request{
			Model:       "openrouter:openai/gpt-5.5",
			ServerTools: []llm.ServerTool{{Name: llm.ServerToolWebSearch}},
		},
	})
	resp, err := srv.Client().Post(srv.URL+"/v1/stream", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("POST stream: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}

	var sawText, sawError bool
	dec := json.NewDecoder(resp.Body)
	for {
		var env protocol.StreamEnvelope
		if decErr := dec.Decode(&env); decErr != nil {
			if decErr == io.EOF {
				break
			}
			t.Fatalf("decode envelope: %v", decErr)
		}
		if env.Error != nil {
			sawError = true
		}
		if env.Event != nil && env.Event.Kind == llm.EventTextDelta && env.Event.Text == "ok" {
			sawText = true
		}
	}
	if sawError {
		t.Fatalf("stream surfaced an error envelope, want silent server-tool retry")
	}
	if !sawText {
		t.Fatalf("stream missing post-retry text event")
	}
	if len(fp.Requests) != 2 {
		t.Fatalf("provider requests = %d, want 2 (initial + retry)", len(fp.Requests))
	}
	if len(fp.Requests[0].ServerTools) != 1 || fp.Requests[0].ServerTools[0].Kind != llm.ServerToolKindOpenRouterWebSearch {
		t.Fatalf("first attempt server tools = %+v, want openrouter web_search", fp.Requests[0].ServerTools)
	}
	if len(fp.Requests[1].ServerTools) != 0 {
		t.Fatalf("retry server tools = %+v, want none", fp.Requests[1].ServerTools)
	}
}

func TestResolveServerToolsHonorsExplicitConfigOnUnknownProvider(t *testing.T) {
	requested := []llm.ServerTool{{Name: llm.ServerToolWebSearch}}

	// An unknown OpenAI-compatible endpoint that explicitly advertises web_search
	// must still resolve to a wire shape (the OpenAI default) instead of being
	// silently dropped.
	explicit := resolvedTarget{
		pc:    llm.ProviderConfig{Name: "acme", APIType: "openai", BaseURL: "https://api.acme.test/v1", ServerTools: []string{llm.ServerToolWebSearch}},
		entry: llm.ModelEntry{Name: "m"},
	}
	if got := resolveServerToolsForTarget(explicit, requested); len(got) != 1 || got[0].Kind != llm.ServerToolKindOpenAIWebSearch {
		t.Fatalf("explicit web_search on unknown provider = %+v, want openai fallback kind", got)
	}

	// The same provider without explicit config is not implicitly web-search-capable.
	implicit := resolvedTarget{
		pc:    llm.ProviderConfig{Name: "acme", APIType: "openai", BaseURL: "https://api.acme.test/v1"},
		entry: llm.ModelEntry{Name: "m"},
	}
	if got := resolveServerToolsForTarget(implicit, requested); got != nil {
		t.Fatalf("unknown provider without explicit config = %+v, want nil", got)
	}
}

func TestServerToolRejected(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{name: "api 400 unsupported tool", err: &llm.APIError{StatusCode: 400, Message: "unsupported tool web_search"}, want: true},
		{name: "api 422 invalid web_search", err: &llm.APIError{StatusCode: 422, Message: "invalid web_search parameter"}, want: true},
		{name: "api 500 is not a rejection", err: &llm.APIError{StatusCode: 500, Message: "unsupported tool web_search"}, want: false},
		{name: "api 400 unrelated", err: &llm.APIError{StatusCode: 400, Message: "context length exceeded"}, want: false},
		{name: "non-api error never retries", err: errors.New("dial tcp: connection refused (tool invalid)"), want: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := serverToolRejected(tc.err); got != tc.want {
				t.Fatalf("serverToolRejected = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestHandlerInputTokens(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "openai.json"), []byte(`{
  "name": "openai",
  "api_type": "responses",
  "base_url": "https://api.openai.com/v1",
  "api_key": "sk-file",
  "models": [{"name":"gpt-5.5","context_window":1000000}]
}`), 0o600); err != nil {
		t.Fatalf("write provider config: %v", err)
	}
	var captured factory.Options
	handler, err := NewHandler(Options{
		ConfigDir: dir,
		Config:    Config{ProviderConfigs: []string{"openai.json"}},
		New: func(opts factory.Options) (llm.Provider, error) {
			captured = opts
			return &countingFakeProvider{FakeProvider: llmtest.New("responses"), count: 4321}, nil
		},
	})
	if err != nil {
		t.Fatalf("NewHandler: %v", err)
	}
	srv := httptest.NewServer(handler)
	defer srv.Close()

	body, _ := json.Marshal(protocol.TokenCountRequest{
		TargetID: "openai:gpt-5.5",
		Request:  llm.Request{Model: "openai:gpt-5.5"},
	})
	resp, err := srv.Client().Post(srv.URL+"/v1/input_tokens", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("POST input_tokens: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	var out protocol.TokenCountResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if out.InputTokens != 4321 || out.Source != "test" {
		t.Fatalf("token count = %+v, want 4321 test", out)
	}
	if captured.Provider != "responses" || captured.Model != "gpt-5.5" {
		t.Fatalf("captured options = %+v", captured)
	}
}

func TestHandlerInputTokensUsesLocalEstimateForCodexOAuth(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "openai-codex.json"), []byte(`{
  "name": "openai-codex",
  "api_type": "responses",
  "base_url": "https://chatgpt.com/backend-api/codex",
  "auth": {"type":"codex_oauth"},
  "models": [{"name":"gpt-5.5","context_window":1000000}]
}`), 0o600); err != nil {
		t.Fatalf("write provider config: %v", err)
	}
	constructions := 0
	handler, err := NewHandler(Options{
		ConfigDir: dir,
		Config:    Config{ProviderConfigs: []string{"openai-codex.json"}},
		New: func(factory.Options) (llm.Provider, error) {
			constructions++
			return &countingFakeProvider{FakeProvider: llmtest.New("responses"), count: 4321}, nil
		},
	})
	if err != nil {
		t.Fatalf("NewHandler: %v", err)
	}
	srv := httptest.NewServer(handler)
	defer srv.Close()

	body, _ := json.Marshal(protocol.TokenCountRequest{
		TargetID: "openai-codex:gpt-5.5",
		Request: llm.Request{
			Model:  "openai-codex:gpt-5.5",
			System: "system instructions",
			Messages: []llm.Message{{
				Role: llm.RoleUser,
				Content: []llm.ContentBlock{{
					Kind: llm.BlockText,
					Text: "hello from a codex session",
				}},
			}},
		},
	})
	resp, err := srv.Client().Post(srv.URL+"/v1/input_tokens", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("POST input_tokens: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	var out protocol.TokenCountResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if out.InputTokens <= 0 || out.Source != "o200k_base" {
		t.Fatalf("token count = %+v, want positive o200k_base estimate", out)
	}
	if constructions != 0 {
		t.Fatalf("provider constructions = %d, want 0 for local codex token estimate", constructions)
	}
}

func TestHandlerInputTokensUnsupported(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "openai.json"), []byte(`{
  "name": "openai",
  "api_type": "openai",
  "base_url": "https://api.openai.com/v1",
  "api_key": "sk-file",
  "models": [{"name":"gpt-5.5","context_window":1000000}]
}`), 0o600); err != nil {
		t.Fatalf("write provider config: %v", err)
	}
	handler, err := NewHandler(Options{
		ConfigDir: dir,
		Config:    Config{ProviderConfigs: []string{"openai.json"}},
		New: func(factory.Options) (llm.Provider, error) {
			return llmtest.New("openai"), nil
		},
	})
	if err != nil {
		t.Fatalf("NewHandler: %v", err)
	}
	srv := httptest.NewServer(handler)
	defer srv.Close()

	body, _ := json.Marshal(protocol.TokenCountRequest{
		TargetID: "openai:gpt-5.5",
		Request:  llm.Request{Model: "openai:gpt-5.5"},
	})
	resp, err := srv.Client().Post(srv.URL+"/v1/input_tokens", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("POST input_tokens: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotImplemented {
		t.Fatalf("status = %d, want 501", resp.StatusCode)
	}
	var out protocol.Error
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode error: %v", err)
	}
	if out.Code != "input_token_count_unsupported" {
		t.Fatalf("error = %+v, want unsupported", out)
	}
}

func TestLoadConfigRejectsInlineAPIKeysAndLoadsKeyFilePath(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	if err := os.WriteFile(path, []byte(`{
  "provider_configs": ["p.json"],
  "api_keys": [{"name": "laptop", "hash": "AAAA"}]
}`), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := LoadConfig(path)
	if err == nil {
		t.Fatal("LoadConfig accepted inline api_keys, want migration error")
	}
	if !strings.Contains(err.Error(), "api_keys is no longer supported") || !strings.Contains(err.Error(), filepath.Join(dir, "api_keys.json")) {
		t.Fatalf("migration error = %v", err)
	}

	if err := os.WriteFile(path, []byte(`{
  "provider_configs": ["p.json"],
  "api_keys_file": "keys/api_keys.json"
}`), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig api_keys_file: %v", err)
	}
	if got, want := ResolveAPIKeysFile(path, cfg.APIKeysFile, ""), filepath.Join(dir, "keys", "api_keys.json"); got != want {
		t.Fatalf("resolved api_keys_file = %q, want %q", got, want)
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

func TestHandlerStreamManagesResponseStateFields(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "openai.json"), []byte(`{
  "name": "openai",
  "api_type": "responses",
  "base_url": "https://api.openai.com/v1",
  "api_key": "sk-test",
  "service_tiers": [{"id":"fast","name":"Fast","request":{"service_tier":"priority"}}],
  "models": [{"name":"gpt-5.5","context_window":128000}]
}`), 0o600); err != nil {
		t.Fatalf("write provider config: %v", err)
	}

	fp := llmtest.New("responses",
		llmtest.Step{
			Events:     []llm.StreamEvent{{Kind: llm.EventTextDelta, Text: "ok"}},
			Stop:       llm.StopEndTurn,
			ResponseID: "resp_1",
		},
		llmtest.Step{
			Events:     []llm.StreamEvent{{Kind: llm.EventTextDelta, Text: "again"}},
			Stop:       llm.StopEndTurn,
			ResponseID: "resp_2",
		},
		llmtest.Step{
			Events:     []llm.StreamEvent{{Kind: llm.EventTextDelta, Text: "third"}},
			Stop:       llm.StopEndTurn,
			ResponseID: "resp_3",
		},
	)
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

	firstMessages := []llm.Message{
		{Role: llm.RoleUser, Content: []llm.ContentBlock{{Kind: llm.BlockText, Text: "first"}}},
	}
	body, _ := json.Marshal(protocol.StreamRequest{
		TargetID: "openai:gpt-5.5",
		Request:  llm.Request{Model: "openai:gpt-5.5", PromptCacheKey: "session-a", Messages: firstMessages},
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
	if len(fp.Requests) != 1 || !fp.Requests[0].StoreResponse || fp.Requests[0].PreviousResponseID != "" {
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
	fullMessages := append([]llm.Message(nil), firstMessages...)
	fullMessages = append(fullMessages,
		llm.BuildAssistantMessage(nil, "ok", nil, "", llm.StopEndTurn),
		llm.Message{Role: llm.RoleUser, Content: []llm.ContentBlock{{Kind: llm.BlockText, Text: "again"}}},
	)
	body, _ = json.Marshal(protocol.StreamRequest{
		TargetID: "openai:gpt-5.5:fast",
		Request:  llm.Request{Model: "openai:gpt-5.5:fast", PromptCacheKey: "session-a", Messages: fullMessages},
	})
	resp, err = srv.Client().Post(srv.URL+"/v1/stream", protocol.ContentTypeNDJSON, bytes.NewReader(body))
	if err != nil {
		t.Fatalf("POST second stream: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("second status = %d body=%s", resp.StatusCode, b)
	}
	_, _ = io.ReadAll(resp.Body)
	if len(fp.Requests) != 2 || fp.Requests[1].PreviousResponseID != "resp_1" || len(fp.Requests[1].Messages) != 1 || fp.Requests[1].ServiceTier != "priority" {
		t.Fatalf("second provider request = %+v", fp.Requests)
	}
	if got := fp.Requests[1].Messages[0].Content[0].Text; got != "again" {
		t.Fatalf("second provider request message = %q, want again", got)
	}

	fullMessages = append(fullMessages,
		llm.BuildAssistantMessage(nil, "again", nil, "", llm.StopEndTurn),
		llm.Message{Role: llm.RoleUser, Content: []llm.ContentBlock{{Kind: llm.BlockText, Text: "third"}}},
	)
	body, _ = json.Marshal(protocol.StreamRequest{
		TargetID: "openai:gpt-5.5",
		Request:  llm.Request{Model: "openai:gpt-5.5", PromptCacheKey: "session-a", Messages: fullMessages},
	})
	resp, err = srv.Client().Post(srv.URL+"/v1/stream", protocol.ContentTypeNDJSON, bytes.NewReader(body))
	if err != nil {
		t.Fatalf("POST third stream: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("third status = %d body=%s", resp.StatusCode, b)
	}
	_, _ = io.ReadAll(resp.Body)
	if len(fp.Requests) != 3 || fp.Requests[2].PreviousResponseID != "resp_2" || len(fp.Requests[2].Messages) != 1 || fp.Requests[2].ServiceTier != "" {
		t.Fatalf("third provider request = %+v", fp.Requests)
	}
	if got := fp.Requests[2].Messages[0].Content[0].Text; got != "third" {
		t.Fatalf("third provider request message = %q, want third", got)
	}
}

func TestHandlerStreamSeparatesProxySessionStateFromCacheAffinity(t *testing.T) {
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

	fp := llmtest.New("responses",
		llmtest.Step{Stop: llm.StopEndTurn, ResponseID: "resp_1"},
		llmtest.Step{Stop: llm.StopEndTurn, ResponseID: "resp_2"},
	)
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

	firstMessages := []llm.Message{
		{Role: llm.RoleUser, Content: []llm.ContentBlock{{Kind: llm.BlockText, Text: "first"}}},
	}
	body, _ := json.Marshal(protocol.StreamRequest{
		TargetID: "openai:gpt-5.5",
		Request: llm.Request{
			Model:           "openai:gpt-5.5",
			ProxySessionID:  "harness-session-one",
			CacheAffinityID: "harness-cache-shared",
			PromptCacheKey:  "legacy-key",
			Messages:        firstMessages,
		},
	})
	resp, err := srv.Client().Post(srv.URL+"/v1/stream", protocol.ContentTypeNDJSON, bytes.NewReader(body))
	if err != nil {
		t.Fatalf("POST first stream: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("first status = %d", resp.StatusCode)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	resp.Body.Close()

	secondMessages := append([]llm.Message(nil), firstMessages...)
	secondMessages = append(secondMessages,
		llm.BuildAssistantMessage(nil, "", nil, "", llm.StopEndTurn),
		llm.Message{Role: llm.RoleUser, Content: []llm.ContentBlock{{Kind: llm.BlockText, Text: "second"}}},
	)
	body, _ = json.Marshal(protocol.StreamRequest{
		TargetID: "openai:gpt-5.5",
		Request: llm.Request{
			Model:           "openai:gpt-5.5",
			ProxySessionID:  "harness-session-two",
			CacheAffinityID: "harness-cache-shared",
			PromptCacheKey:  "legacy-key",
			Messages:        secondMessages,
		},
	})
	resp, err = srv.Client().Post(srv.URL+"/v1/stream", protocol.ContentTypeNDJSON, bytes.NewReader(body))
	if err != nil {
		t.Fatalf("POST second stream: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("second status = %d", resp.StatusCode)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	resp.Body.Close()

	if len(fp.Requests) != 2 {
		t.Fatalf("provider requests = %d, want 2: %+v", len(fp.Requests), fp.Requests)
	}
	if fp.Requests[0].ProxySessionID != "" || fp.Requests[1].ProxySessionID != "" {
		t.Fatalf("raw proxy session id reached provider: %+v", fp.Requests)
	}
	if fp.Requests[0].CacheAffinityID != "" || fp.Requests[1].CacheAffinityID != "" {
		t.Fatalf("raw cache affinity id reached provider: %+v", fp.Requests)
	}
	if got, want := fp.Requests[0].PromptCacheKey, providerPromptCacheKey("harness-cache-shared"); got != want {
		t.Fatalf("first provider prompt cache key = %q, want %q", got, want)
	}
	if got, want := fp.Requests[1].PromptCacheKey, providerPromptCacheKey("harness-cache-shared"); got != want {
		t.Fatalf("second provider prompt cache key = %q, want %q", got, want)
	}
	if fp.Requests[1].PreviousResponseID != "" {
		t.Fatalf("second request continued across proxy sessions: prev=%q", fp.Requests[1].PreviousResponseID)
	}
}

func TestHandlerStreamDoesNotContinueShorterTranscriptWithSamePromptCacheKey(t *testing.T) {
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

	fp := llmtest.New("responses",
		llmtest.Step{Stop: llm.StopEndTurn, ResponseID: "resp_1"},
		llmtest.Step{Stop: llm.StopEndTurn, ResponseID: "resp_2"},
	)
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

	firstMessages := []llm.Message{
		{Role: llm.RoleUser, Content: []llm.ContentBlock{{Kind: llm.BlockText, Text: "first"}}},
	}
	body, _ := json.Marshal(protocol.StreamRequest{
		TargetID: "openai:gpt-5.5",
		Request:  llm.Request{Model: "openai:gpt-5.5", PromptCacheKey: "session-a", Messages: firstMessages},
	})
	resp, err := srv.Client().Post(srv.URL+"/v1/stream", protocol.ContentTypeNDJSON, bytes.NewReader(body))
	if err != nil {
		t.Fatalf("POST first stream: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("first status = %d", resp.StatusCode)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	resp.Body.Close()

	freshMessages := []llm.Message{
		{Role: llm.RoleUser, Content: []llm.ContentBlock{{Kind: llm.BlockText, Text: "fresh"}}},
	}
	body, _ = json.Marshal(protocol.StreamRequest{
		TargetID: "openai:gpt-5.5",
		Request:  llm.Request{Model: "openai:gpt-5.5", PromptCacheKey: "session-a", Messages: freshMessages},
	})
	resp, err = srv.Client().Post(srv.URL+"/v1/stream", protocol.ContentTypeNDJSON, bytes.NewReader(body))
	if err != nil {
		t.Fatalf("POST fresh stream: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("fresh status = %d", resp.StatusCode)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	resp.Body.Close()

	if len(fp.Requests) != 2 {
		t.Fatalf("provider requests = %d, want 2: %+v", len(fp.Requests), fp.Requests)
	}
	if fp.Requests[1].PreviousResponseID != "" || len(fp.Requests[1].Messages) != 1 {
		t.Fatalf("fresh provider request = prev %q messages %d, want no previous_response_id and one message", fp.Requests[1].PreviousResponseID, len(fp.Requests[1].Messages))
	}
	if got := fp.Requests[1].Messages[0].Content[0].Text; got != "fresh" {
		t.Fatalf("fresh provider request message = %q, want fresh", got)
	}
}

func TestHandlerStreamRetriesPreviousResponseRejectionWithFullHistory(t *testing.T) {
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

	var logs bytes.Buffer
	logger, err := logging.NewProxyLogger(&logs, logging.LevelInfo, logging.FormatJSON)
	if err != nil {
		t.Fatalf("NewProxyLogger: %v", err)
	}
	fp := llmtest.New("responses",
		llmtest.Step{Stop: llm.StopEndTurn, ResponseID: "resp_1"},
		llmtest.Step{Err: &llm.APIError{Code: "previous_response_not_found", Message: "previous_response_id is invalid"}},
		llmtest.Step{Stop: llm.StopEndTurn, ResponseID: "resp_2"},
	)
	handler, err := NewHandler(Options{
		ConfigDir: dir,
		Config:    Config{ProviderConfigs: []string{"openai.json"}},
		Logger:    logger,
		New: func(factory.Options) (llm.Provider, error) {
			return fp, nil
		},
	})
	if err != nil {
		t.Fatalf("NewHandler: %v", err)
	}
	srv := httptest.NewServer(handler)
	defer srv.Close()

	firstMessages := []llm.Message{
		{Role: llm.RoleUser, Content: []llm.ContentBlock{{Kind: llm.BlockText, Text: "first"}}},
	}
	body, _ := json.Marshal(protocol.StreamRequest{
		TargetID: "openai:gpt-5.5",
		Request:  llm.Request{Model: "openai:gpt-5.5", PromptCacheKey: "session-a", Messages: firstMessages},
	})
	resp, err := srv.Client().Post(srv.URL+"/v1/stream", protocol.ContentTypeNDJSON, bytes.NewReader(body))
	if err != nil {
		t.Fatalf("POST first stream: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("first status = %d", resp.StatusCode)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	resp.Body.Close()

	fullMessages := append([]llm.Message(nil), firstMessages...)
	fullMessages = append(fullMessages,
		llm.BuildAssistantMessage(nil, "", nil, "", llm.StopEndTurn),
		llm.Message{Role: llm.RoleUser, Content: []llm.ContentBlock{{Kind: llm.BlockText, Text: "second"}}},
	)
	body, _ = json.Marshal(protocol.StreamRequest{
		TargetID: "openai:gpt-5.5",
		Request:  llm.Request{Model: "openai:gpt-5.5", PromptCacheKey: "session-a", Messages: fullMessages},
	})
	resp, err = srv.Client().Post(srv.URL+"/v1/stream", protocol.ContentTypeNDJSON, bytes.NewReader(body))
	if err != nil {
		t.Fatalf("POST second stream: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("second status = %d", resp.StatusCode)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	resp.Body.Close()

	if len(fp.Requests) != 3 {
		t.Fatalf("provider requests = %d, want 3: %+v", len(fp.Requests), fp.Requests)
	}
	if fp.Requests[1].PreviousResponseID != "resp_1" || len(fp.Requests[1].Messages) != 1 {
		t.Fatalf("continued request = prev %q messages %d, want resp_1 and trimmed tail", fp.Requests[1].PreviousResponseID, len(fp.Requests[1].Messages))
	}
	if fp.Requests[2].PreviousResponseID != "" || fp.Requests[2].StoreResponse || len(fp.Requests[2].Messages) != len(fullMessages) {
		t.Fatalf("stateless retry = prev %q store=%v messages=%d, want full history without previous response", fp.Requests[2].PreviousResponseID, fp.Requests[2].StoreResponse, len(fp.Requests[2].Messages))
	}

	records := strings.Split(strings.TrimSpace(logs.String()), "\n")
	if len(records) < 2 {
		t.Fatalf("logs = %q, want at least two records", logs.String())
	}
	var last map[string]any
	if err := json.Unmarshal([]byte(records[len(records)-1]), &last); err != nil {
		t.Fatalf("decode last log %q: %v", records[len(records)-1], err)
	}
	if _, ok := last["err"]; ok {
		t.Fatalf("successful retry should not log stale err: %+v", last)
	}
}

func TestReasoningProfilesMapToEffortAndBudgetControls(t *testing.T) {
	enabled := true
	h := &Handler{}
	effortOnly := resolvedTarget{pc: llm.ProviderConfig{Name: "openrouter", APIType: "openai"}, entry: llm.ModelEntry{
		Name:      "effort",
		Reasoning: &enabled,
		ReasoningOptions: []llm.ReasoningOption{{
			Type:   "effort",
			Values: []string{"high"},
		}},
	}}
	if got := h.reasoningForTarget(effortOnly, "low", llm.ReasoningConfig{}); got.Effort != "high" {
		t.Fatalf("low mapped to effort %q, want high", got.Effort)
	}

	multiEffort := effortOnly
	multiEffort.entry.ReasoningOptions = []llm.ReasoningOption{{
		Type:   "effort",
		Values: []string{"minimal", "low", "medium", "high", "xhigh"},
	}}
	if got := h.reasoningForTarget(multiEffort, "minimal", llm.ReasoningConfig{}); got.Effort != "minimal" {
		t.Fatalf("minimal mapped to effort %q, want minimal", got.Effort)
	}
	if got := h.reasoningForTarget(multiEffort, "max", llm.ReasoningConfig{}); got.Effort != "xhigh" {
		t.Fatalf("max mapped to effort %q, want xhigh", got.Effort)
	}

	minBudget, maxBudget := 128, 32768
	budgetOnly := resolvedTarget{pc: llm.ProviderConfig{Name: "google", APIType: "openai"}, entry: llm.ModelEntry{
		Name:      "budget",
		Reasoning: &enabled,
		ReasoningOptions: []llm.ReasoningOption{{
			Type: "budget_tokens",
			Min:  &minBudget,
			Max:  &maxBudget,
		}},
	}}
	if got := h.reasoningForTarget(budgetOnly, "medium", llm.ReasoningConfig{}); got.BudgetTokens == nil || *got.BudgetTokens != 16384 || got.Effort != "" {
		t.Fatalf("medium budget mapping = %+v, want budget_tokens 16384 only", got)
	}
	if got := h.reasoningForTarget(budgetOnly, "minimal", llm.ReasoningConfig{}); got.BudgetTokens == nil || *got.BudgetTokens != 1639 {
		t.Fatalf("minimal budget mapping = %+v, want budget_tokens 1639", got)
	}
	if got := h.reasoningForTarget(budgetOnly, "none", llm.ReasoningConfig{}); !got.Empty() {
		t.Fatalf("none without toggle = %+v, want provider default/no controls", got)
	}

	minZero := 0
	budgetToggle := budgetOnly
	budgetToggle.entry.ReasoningOptions = []llm.ReasoningOption{
		{Type: "toggle"},
		{Type: "budget_tokens", Min: &minZero, Max: &maxBudget},
	}
	if got := h.reasoningForTarget(budgetToggle, "none", llm.ReasoningConfig{}); got.Enabled == nil || *got.Enabled {
		t.Fatalf("none with toggle = %+v, want explicit disabled", got)
	}
}

func TestHandlerCatalogExposesTargetsOnly(t *testing.T) {
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
    "name": "codex-compatible",
    "api_type": "responses",
    "base_url": "https://example.test/responses",
    "auth": {"type":"codex_oauth"},
    "responses_stateful": true,
    "models": [{"name":"gpt-5.5","context_window":128000}]
  },
  {
    "name": "stateless-compatible",
    "api_type": "responses",
    "base_url": "https://example.test/responses",
    "responses_stateful": false,
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

	targets := map[string]protocol.Target{}
	for _, target := range handler.Catalog().Targets {
		targets[target.ID] = target
	}
	for _, id := range []string{
		"openai:gpt-5.5",
		"openai-codex:gpt-5.5",
		"codex-compatible:gpt-5.5",
		"stateless-compatible:gpt-5.5",
		"openrouter:openai/gpt-5.5",
	} {
		if _, ok := targets[id]; !ok {
			t.Fatalf("target %q missing from catalog: %+v", id, handler.Catalog().Targets)
		}
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
	constructions := 0
	fp := llmtest.New("fake",
		llmtest.Step{Events: []llm.StreamEvent{{Kind: llm.EventTextDelta, Text: "ok"}}, Stop: llm.StopEndTurn},
		llmtest.Step{Events: []llm.StreamEvent{{Kind: llm.EventTextDelta, Text: "again"}}, Stop: llm.StopEndTurn},
	)
	handler, err := NewHandler(Options{
		ConfigDir: dir,
		Config:    Config{ProviderConfigs: []string{"openai-codex.json"}},
		New: func(opts factory.Options) (llm.Provider, error) {
			captured = opts
			constructions++
			return fp, nil
		},
	})
	if err != nil {
		t.Fatalf("NewHandler: %v", err)
	}
	srv := httptest.NewServer(handler)
	defer srv.Close()

	body, _ := json.Marshal(protocol.StreamRequest{
		TargetID: "openai-codex:gpt-5.5",
		Request:  llm.Request{Model: "openai-codex:gpt-5.5", ProxySessionID: "harness-session-a", CacheAffinityID: "harness-cache-a"},
	})
	resp, err := srv.Client().Post(srv.URL+"/v1/stream", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("POST stream: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	_, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	resp, err = srv.Client().Post(srv.URL+"/v1/stream", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("second POST stream: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("second status = %d", resp.StatusCode)
	}
	_, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	bodyB, _ := json.Marshal(protocol.StreamRequest{
		TargetID: "openai-codex:gpt-5.5",
		Request:  llm.Request{Model: "openai-codex:gpt-5.5", ProxySessionID: "harness-session-b", CacheAffinityID: "harness-cache-a"},
	})
	resp, err = srv.Client().Post(srv.URL+"/v1/stream", "application/json", bytes.NewReader(bodyB))
	if err != nil {
		t.Fatalf("third POST stream: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("third status = %d", resp.StatusCode)
	}
	_, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if captured.Provider != "responses" || !captured.OmitMaxOutputTokens || !captured.ResponsesWebSocket {
		t.Fatalf("captured options = %+v, want responses with OmitMaxOutputTokens and ResponsesWebSocket", captured)
	}
	if constructions != 2 {
		t.Fatalf("provider constructions = %d, want 2 for two proxy sessions", constructions)
	}
	if len(fp.Requests) != 3 {
		t.Fatalf("fake provider requests = %d, want 3", len(fp.Requests))
	}
	if fp.Requests[0].PromptCacheKey != providerPromptCacheKey("harness-cache-a") ||
		fp.Requests[1].PromptCacheKey != providerPromptCacheKey("harness-cache-a") ||
		fp.Requests[2].PromptCacheKey != providerPromptCacheKey("harness-cache-a") {
		t.Fatalf("provider prompt cache keys = %q, %q, %q", fp.Requests[0].PromptCacheKey, fp.Requests[1].PromptCacheKey, fp.Requests[2].PromptCacheKey)
	}
	if fp.Requests[0].ProxySessionID != "" || fp.Requests[1].ProxySessionID != "" || fp.Requests[2].ProxySessionID != "" {
		t.Fatalf("raw proxy session id reached provider: %+v", fp.Requests)
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
		TargetID: "oauth:model",
		Request:  llm.Request{Model: "oauth:model"},
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
		TargetID: "openai:missing",
		Request:  llm.Request{Model: "openai:missing"},
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

	body, _ = json.Marshal(protocol.StreamRequest{TargetID: ""})
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
		TargetID: "openai:priced",
		Request:  llm.Request{Model: "openai:priced", Purpose: llm.RequestPurposeTurn},
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
		"purpose":          string(llm.RequestPurposeTurn),
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
			TargetID: "openai:" + model,
			Request:  llm.Request{Model: "openai:" + model},
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
		TargetID:         "openai:priced",
		Requests:         2,
		InputTokens:      2000,
		OutputTokens:     4000,
		CacheReadTokens:  6000,
		CacheWriteTokens: 8000,
		ReasoningTokens:  1000,
	}
	if got.TargetID != want.TargetID || got.Requests != want.Requests ||
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

func TestHandlerPricesUsageSnapshotsBeforeStreamError(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "openai.json"), []byte(`{
  "name": "openai",
  "api_type": "openai",
  "base_url": "http://localhost:11434/v1",
  "models": [{"name":"priced","context_window":128000,"price":{"input":2,"output":4}}]
}`), 0o600); err != nil {
		t.Fatalf("write provider config: %v", err)
	}

	usage := llm.Usage{InputTokens: 1000, OutputTokens: 2000}
	handler, err := NewHandler(Options{
		ConfigDir: dir,
		Config:    Config{ProviderConfigs: []string{"openai.json"}},
		New: func(factory.Options) (llm.Provider, error) {
			return llmtest.New("fake", llmtest.Step{
				Events: []llm.StreamEvent{{Kind: llm.EventUsage, Usage: &usage}},
				Err:    &llm.APIError{Code: "server_error", Message: "upstream failed", Retryable: true},
			}), nil
		},
	})
	if err != nil {
		t.Fatalf("NewHandler: %v", err)
	}
	srv := httptest.NewServer(handler)
	defer srv.Close()

	body, _ := json.Marshal(protocol.StreamRequest{
		TargetID: "openai:priced",
		Request:  llm.Request{Model: "openai:priced"},
	})
	resp, err := srv.Client().Post(srv.URL+"/v1/stream", protocol.ContentTypeNDJSON, bytes.NewReader(body))
	if err != nil {
		t.Fatalf("POST stream: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("stream status = %d, want 200", resp.StatusCode)
	}
	dec := json.NewDecoder(resp.Body)
	var first protocol.StreamEnvelope
	if err := dec.Decode(&first); err != nil {
		t.Fatalf("decode first envelope: %v", err)
	}
	if first.Event == nil || first.Event.Usage == nil || !first.Event.Usage.CostKnown {
		t.Fatalf("first usage event not priced: %+v", first.Event)
	}
	if diff := first.Event.Usage.CostUSD - 0.01; diff > 1e-9 || diff < -1e-9 {
		t.Fatalf("usage event cost = %v, want 0.01", first.Event.Usage.CostUSD)
	}
	_, _ = io.Copy(io.Discard, resp.Body)

	reportResp, err := srv.Client().Get(srv.URL + "/v1/usage")
	if err != nil {
		t.Fatalf("GET usage: %v", err)
	}
	defer reportResp.Body.Close()
	var report protocol.UsageReport
	if err := json.NewDecoder(reportResp.Body).Decode(&report); err != nil {
		t.Fatalf("decode usage report: %v", err)
	}
	if len(report.Models) != 1 {
		t.Fatalf("usage report = %+v, want one priced failed request", report.Models)
	}
	got := report.Models[0]
	if got.TargetID != "openai:priced" || got.Requests != 1 || got.InputTokens != 1000 || got.OutputTokens != 2000 {
		t.Fatalf("usage report entry = %+v, want priced failed request", got)
	}
	if diff := got.CostUSD - 0.01; diff > 1e-9 || diff < -1e-9 {
		t.Fatalf("usage report cost = %v, want 0.01", got.CostUSD)
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
		TargetID: "openai:known",
		Request:  llm.Request{Model: "openai:known"},
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

func TestHandlerRetriesInferredOutputTokenFloor(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "testai.json"), []byte(`{
  "name": "testai",
  "api_type": "openai",
  "base_url": "https://api.test/v1",
  "models": [{"name":"model","context_window":1000000}]
}`), 0o600); err != nil {
		t.Fatalf("write provider config: %v", err)
	}

	var logs bytes.Buffer
	logger, err := logging.NewProxyLogger(&logs, logging.LevelInfo, logging.FormatJSON)
	if err != nil {
		t.Fatalf("NewProxyLogger: %v", err)
	}
	fp := llmtest.New("fake",
		llmtest.Step{Err: &llm.APIError{
			StatusCode: http.StatusBadRequest,
			Code:       "invalid_request_error",
			Message:    "Invalid 'max_tokens': must be greater than or equal to 16.",
		}},
		llmtest.Step{Stop: llm.StopEndTurn},
	)
	handler, err := NewHandler(Options{
		ConfigDir: dir,
		Config:    Config{ProviderConfigs: []string{"testai.json"}},
		Logger:    logger,
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
		TargetID: "testai:model",
		Request:  llm.Request{Model: "testai:model", MaxTokens: 1},
	})
	resp, err := srv.Client().Post(srv.URL+"/v1/stream", protocol.ContentTypeNDJSON, bytes.NewReader(body))
	if err != nil {
		t.Fatalf("POST stream: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var sawDone bool
	dec := json.NewDecoder(resp.Body)
	for {
		var env protocol.StreamEnvelope
		if err := dec.Decode(&env); err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			t.Fatalf("decode stream envelope: %v", err)
		}
		if env.Error != nil {
			t.Fatalf("unexpected stream error: %+v", env.Error)
		}
		if env.Event != nil && env.Event.Kind == llm.EventDone {
			sawDone = true
		}
	}
	if !sawDone {
		t.Fatalf("stream did not complete after retry")
	}
	if len(fp.Requests) != 2 {
		t.Fatalf("provider requests = %d, want 2", len(fp.Requests))
	}
	if fp.Requests[0].MaxTokens != 1 || fp.Requests[1].MaxTokens != 16 {
		t.Fatalf("request max tokens = [%d, %d], want [1, 16]", fp.Requests[0].MaxTokens, fp.Requests[1].MaxTokens)
	}

	var retryLog map[string]any
	for _, line := range strings.Split(strings.TrimSpace(logs.String()), "\n") {
		var record map[string]any
		if err := json.Unmarshal([]byte(line), &record); err != nil {
			t.Fatalf("decode log %q: %v", line, err)
		}
		if record["msg"] == "retrying model request with higher output token floor" {
			retryLog = record
			break
		}
	}
	if retryLog == nil {
		t.Fatalf("retry log not found in %s", logs.String())
	}
	for k, want := range map[string]any{
		"provider":                     "testai",
		"api_type":                     "openai",
		"model":                        "model",
		"configured_min_output_tokens": float64(0),
		"inferred_min_output_tokens":   float64(16),
		"original_max_tokens":          float64(1),
		"retry_max_tokens":             float64(16),
	} {
		if got := retryLog[k]; got != want {
			t.Fatalf("retry log[%s] = %v (%T), want %v", k, got, got, want)
		}
	}
}

func TestHandlerAPIKeyAuthRejectsAndAccepts(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "openai.json"), []byte(`{
  "name": "openai",
  "api_type": "openai",
  "base_url": "http://localhost:11434/v1",
  "models": [{"name":"known","context_window":128000}]
}`), 0o600); err != nil {
		t.Fatalf("write provider config: %v", err)
	}
	cfg := Config{ProviderConfigs: []string{"openai.json"}}
	store := apikey.Store{Entries: []apikey.Entry{{Name: "laptop", Hash: apikey.Hash("hmp_secret")}}}
	handler, err := NewHandler(Options{
		ConfigDir: dir,
		Config:    cfg,
		New: func(factory.Options) (llm.Provider, error) {
			return llmtest.New("fake"), nil
		},
	})
	if err != nil {
		t.Fatalf("NewHandler: %v", err)
	}
	srv := httptest.NewServer(store.Middleware(handler))
	defer srv.Close()

	resp, err := srv.Client().Get(srv.URL + "/v1/models")
	if err != nil {
		t.Fatalf("GET models: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("no key status = %d, want 401", resp.StatusCode)
	}

	req, err := http.NewRequest(http.MethodGet, srv.URL+"/v1/models", nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer hmp_secret")
	resp, err = srv.Client().Do(req)
	if err != nil {
		t.Fatalf("GET models with key: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("with key status = %d, want 200", resp.StatusCode)
	}
}

func TestHandlerNoAPIKeyAllowsWhenUnconfigured(t *testing.T) {
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
		New: func(factory.Options) (llm.Provider, error) {
			return llmtest.New("fake"), nil
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
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
}

func TestHandlerMetricsRecordsPricedAndFreeModels(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "openai.json"), []byte(`{
  "name": "openai",
  "api_type": "openai",
  "base_url": "http://localhost:11434/v1",
  "models": [
    {"name":"priced","context_window":128000,"price":{"input":2,"output":4}},
    {"name":"free","context_window":128000}
  ]
}`), 0o600); err != nil {
		t.Fatalf("write provider config: %v", err)
	}

	reg := metrics.New()
	usage := llm.Usage{InputTokens: 1000, OutputTokens: 2000, CacheReadTokens: 3000, CacheWriteTokens: 4000, ReasoningTokens: 500}
	handler, err := NewHandler(Options{
		ConfigDir: dir,
		Config:    Config{ProviderConfigs: []string{"openai.json"}},
		Metrics:   reg,
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
			TargetID: "openai:" + model,
			Request:  llm.Request{Model: "openai:" + model, Purpose: llm.RequestPurposeTurn},
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
	stream("free")

	var b strings.Builder
	reg.Render(&b)
	out := b.String()

	// Both models get token counters (free model is a deliberate superset of
	// /v1/usage, which records cost only).
	labels := map[string]string{"provider": "openai", "model": "priced", "purpose": "turn", "key": "anonymous"}
	pricedLine := seriesLine("model_proxy_input_tokens_total", labels)
	if !strings.Contains(out, pricedLine+" 1000") {
		t.Errorf("missing priced input tokens series:\n%s", out)
	}
	if !strings.Contains(out, seriesLine("model_proxy_requests_total", labels)+" 1") {
		t.Errorf("missing priced requests series:\n%s", out)
	}
	// Cost only for the priced model.
	if !strings.Contains(out, seriesLine("model_proxy_cost_usd_total", labels)+" ") {
		t.Errorf("missing priced cost series:\n%s", out)
	}
	freeLabels := map[string]string{"provider": "openai", "model": "free", "purpose": "turn", "key": "anonymous"}
	if !strings.Contains(out, seriesLine("model_proxy_output_tokens_total", freeLabels)+" 2000") {
		t.Errorf("missing free output tokens series:\n%s", out)
	}
	// No cost series for the free model.
	if strings.Contains(out, seriesLine("model_proxy_cost_usd_total", freeLabels)) {
		t.Errorf("free model should not have a cost series:\n%s", out)
	}
	// HELP/TYPE present even pre-traffic for all families.
	for _, name := range []string{
		"model_proxy_requests_total",
		"model_proxy_errors_total",
		"model_proxy_cost_usd_total",
		"model_proxy_request_duration_seconds_total",
	} {
		if !strings.Contains(out, "# TYPE "+name+" counter") {
			t.Errorf("missing TYPE for %s:\n%s", name, out)
		}
	}
}

func TestHandlerMetricsKeyLabelReflectsAuth(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "openai.json"), []byte(`{
  "name": "openai",
  "api_type": "openai",
  "base_url": "http://localhost:11434/v1",
  "models": [{"name":"priced","context_window":128000,"price":{"input":2,"output":4}}]
}`), 0o600); err != nil {
		t.Fatalf("write provider config: %v", err)
	}
	reg := metrics.New()
	usage := llm.Usage{InputTokens: 100, OutputTokens: 50}
	cfg := Config{ProviderConfigs: []string{"openai.json"}}
	store := apikey.Store{Entries: []apikey.Entry{{Name: "laptop", Hash: apikey.Hash("hmp_secret")}}}
	handler, err := NewHandler(Options{
		ConfigDir: dir,
		Config:    cfg,
		Metrics:   reg,
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
	srv := httptest.NewServer(store.Middleware(handler))
	defer srv.Close()

	body, _ := json.Marshal(protocol.StreamRequest{
		TargetID: "openai:priced",
		Request:  llm.Request{Model: "openai:priced", Purpose: llm.RequestPurposePrewarm},
	})
	// Authenticated request: key label = configured name.
	req, err := http.NewRequest(http.MethodPost, srv.URL+"/v1/stream", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer hmp_secret")
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatalf("POST stream with key: %v", err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	resp.Body.Close()

	var b strings.Builder
	reg.Render(&b)
	out := b.String()
	namedLabels := map[string]string{"provider": "openai", "model": "priced", "purpose": "prewarm", "key": "laptop"}
	if !strings.Contains(out, seriesLine("model_proxy_requests_total", namedLabels)+" 1") {
		t.Errorf("missing key=laptop series:\n%s", out)
	}
}

func TestHandlerMetricsRecordsErrors(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "openai.json"), []byte(`{
  "name": "openai",
  "api_type": "openai",
  "base_url": "http://localhost:11434/v1",
  "models": [{"name":"known","context_window":128000}]
}`), 0o600); err != nil {
		t.Fatalf("write provider config: %v", err)
	}
	reg := metrics.New()
	handler, err := NewHandler(Options{
		ConfigDir: dir,
		Config:    Config{ProviderConfigs: []string{"openai.json"}},
		Metrics:   reg,
		New: func(factory.Options) (llm.Provider, error) {
			return llmtest.New("fake", llmtest.Step{Err: &llm.APIError{Code: "server_error", Message: "boom"}}), nil
		},
	})
	if err != nil {
		t.Fatalf("NewHandler: %v", err)
	}
	srv := httptest.NewServer(handler)
	defer srv.Close()

	body, _ := json.Marshal(protocol.StreamRequest{
		TargetID: "openai:known",
		Request:  llm.Request{Model: "openai:known", Purpose: llm.RequestPurposeCompaction},
	})
	resp, err := srv.Client().Post(srv.URL+"/v1/stream", protocol.ContentTypeNDJSON, bytes.NewReader(body))
	if err != nil {
		t.Fatalf("POST stream: %v", err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	resp.Body.Close()

	var b strings.Builder
	reg.Render(&b)
	out := b.String()
	labels := map[string]string{"provider": "openai", "model": "known", "purpose": "compaction", "key": "anonymous"}
	if !strings.Contains(out, seriesLine("model_proxy_errors_total", labels)+" 1") {
		t.Errorf("missing errors series:\n%s", out)
	}
	if !strings.Contains(out, seriesLine("model_proxy_requests_total", labels)+" 1") {
		t.Errorf("missing requests series for failed request:\n%s", out)
	}
}

func TestHandlerMetricsRecordsRejectedAuth(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "openai.json"), []byte(`{
  "name": "openai",
  "api_type": "openai",
  "base_url": "http://localhost:11434/v1",
  "models": [{"name":"priced","context_window":128000,"price":{"input":2,"output":4}}]
}`), 0o600); err != nil {
		t.Fatalf("write provider config: %v", err)
	}
	reg := metrics.New()
	cfg := Config{ProviderConfigs: []string{"openai.json"}}
	store := apikey.Store{Entries: []apikey.Entry{{Name: "laptop", Hash: apikey.Hash("hmp_secret")}}}
	handler, err := NewHandler(Options{
		ConfigDir: dir,
		Config:    cfg,
		Metrics:   reg,
		New: func(factory.Options) (llm.Provider, error) {
			return llmtest.New("fake", llmtest.Step{Stop: llm.StopEndTurn}), nil
		},
	})
	if err != nil {
		t.Fatalf("NewHandler: %v", err)
	}
	srv := httptest.NewServer(ObserveAuth(handler, store, store.Middleware(handler)))
	defer srv.Close()

	// An unauthenticated stream request is rejected with 401 before the handler,
	// but must still be metered so an auth-failure flood produces error signal.
	body, _ := json.Marshal(protocol.StreamRequest{
		TargetID: "openai:priced",
		Request:  llm.Request{Model: "openai:priced"},
	})
	resp, err := srv.Client().Post(srv.URL+"/v1/stream", protocol.ContentTypeNDJSON, bytes.NewReader(body))
	if err != nil {
		t.Fatalf("POST stream: %v", err)
	}
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unauthenticated stream status = %d, want 401", resp.StatusCode)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	resp.Body.Close()

	var b strings.Builder
	reg.Render(&b)
	out := b.String()
	labels := map[string]string{"key": "anonymous", "purpose": "unknown"}
	if !strings.Contains(out, seriesLine("model_proxy_requests_total", labels)+" 1") {
		t.Errorf("rejected auth not counted in requests_total:\n%s", out)
	}
	if !strings.Contains(out, seriesLine("model_proxy_errors_total", labels)+" 1") {
		t.Errorf("rejected auth not counted in errors_total:\n%s", out)
	}
}

func TestStreamFailedExcludesClientCancel(t *testing.T) {
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	// A client disconnect cancels the request context and surfaces as a stream
	// error, but it is not a server/provider failure.
	if streamFailed(canceled, "context canceled", http.StatusOK) {
		t.Error("client-canceled stream should not be marked failed")
	}
	// A genuine provider error on a live context still counts.
	if !streamFailed(context.Background(), "boom", http.StatusOK) {
		t.Error("provider error should be marked failed")
	}
	// A 5xx on a live context counts.
	if !streamFailed(context.Background(), "", http.StatusInternalServerError) {
		t.Error("5xx should be marked failed")
	}
	// A clean success does not.
	if streamFailed(context.Background(), "", http.StatusOK) {
		t.Error("clean success should not be marked failed")
	}
}

func TestHandlerMetricsRecordsUnresolvedRequest(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "openai.json"), []byte(`{
  "name": "openai",
  "api_type": "openai",
  "base_url": "http://localhost:11434/v1",
  "models": [{"name":"priced","context_window":128000,"price":{"input":2,"output":4}}]
}`), 0o600); err != nil {
		t.Fatalf("write provider config: %v", err)
	}
	reg := metrics.New()
	handler, err := NewHandler(Options{
		ConfigDir: dir,
		Config:    Config{ProviderConfigs: []string{"openai.json"}},
		Metrics:   reg,
		New: func(factory.Options) (llm.Provider, error) {
			return llmtest.New("fake", llmtest.Step{Stop: llm.StopEndTurn}), nil
		},
	})
	if err != nil {
		t.Fatalf("NewHandler: %v", err)
	}
	srv := httptest.NewServer(handler)
	defer srv.Close()

	// An unknown target_id fails before resolution: no provider/model, but the
	// request and its error must still be metered.
	body, _ := json.Marshal(protocol.StreamRequest{
		TargetID: "openai:nope",
		Request:  llm.Request{Model: "openai:nope", Purpose: llm.RequestPurpose("unbounded-client-value")},
	})
	resp, err := srv.Client().Post(srv.URL+"/v1/stream", protocol.ContentTypeNDJSON, bytes.NewReader(body))
	if err != nil {
		t.Fatalf("POST stream: %v", err)
	}
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("unresolved target status = %d, want 400", resp.StatusCode)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	resp.Body.Close()

	var b strings.Builder
	reg.Render(&b)
	out := b.String()
	// provider/model are empty and so omitted. The arbitrary client purpose is
	// normalized to unknown so it cannot create an unbounded label value.
	labels := map[string]string{"key": "anonymous", "purpose": "unknown"}
	if !strings.Contains(out, seriesLine("model_proxy_requests_total", labels)+" 1") {
		t.Errorf("unresolved request not counted in requests_total:\n%s", out)
	}
	if !strings.Contains(out, seriesLine("model_proxy_errors_total", labels)+" 1") {
		t.Errorf("unresolved request not counted in errors_total:\n%s", out)
	}
}

// seriesLine renders the metric name + sorted label set prefix that the
// exposition writer emits, so tests can assert on the label order
// deterministically.
func seriesLine(name string, labels map[string]string) string {
	names := make([]string, 0, len(labels))
	for k := range labels {
		names = append(names, k)
	}
	sort.Strings(names)
	var b strings.Builder
	b.WriteString(name)
	if len(names) > 0 {
		b.WriteByte('{')
		for i, k := range names {
			if i > 0 {
				b.WriteByte(',')
			}
			b.WriteString(k)
			b.WriteString(`="`)
			b.WriteString(labels[k])
			b.WriteByte('"')
		}
		b.WriteByte('}')
	}
	return b.String()
}

func TestHandlerCatalogLogsTraceAttrsOnlyForValidTraceparent(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "provider.json"), []byte(`{
  "name":"test",
  "api_type":"openai",
  "base_url":"https://example.invalid",
  "api_key":"sk-test",
  "models":[{"name":"model","context_window":1000}]
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
		Config:    Config{ProviderConfigs: []string{"provider.json"}},
		Logger:    logger,
	})
	if err != nil {
		t.Fatalf("NewHandler: %v", err)
	}
	srv := httptest.NewServer(handler)
	defer srv.Close()

	badReq, err := http.NewRequest(http.MethodGet, srv.URL+"/v1/models", nil)
	if err != nil {
		t.Fatalf("bad request: %v", err)
	}
	badReq.Header.Set(tracing.TraceparentHeader, "not-valid")
	badResp, err := srv.Client().Do(badReq)
	if err != nil {
		t.Fatalf("GET bad trace: %v", err)
	}
	badResp.Body.Close()
	if strings.TrimSpace(logs.String()) != "" {
		t.Fatalf("malformed traceparent produced log: %q", logs.String())
	}

	tc := tracing.Context{TraceID: "4bf92f3577b34da6a3ce929d0e0e4736", SpanID: "00f067aa0ba902b7", ParentSpanID: "1111111111111111", Sampled: true}
	req, err := http.NewRequest(http.MethodGet, srv.URL+"/v1/models", nil)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	tracing.Inject(req.Header, tc)
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatalf("GET models: %v", err)
	}
	resp.Body.Close()
	var record map[string]any
	if err := json.Unmarshal(logs.Bytes(), &record); err != nil {
		t.Fatalf("decode log %q: %v", logs.String(), err)
	}
	for k, want := range map[string]any{
		"msg":           "model proxy request completed",
		"method":        http.MethodGet,
		"path":          "/v1/models",
		"status":        float64(http.StatusOK),
		"trace_id":      tc.TraceID,
		"span_id":       tc.SpanID,
		"trace_sampled": true,
	} {
		if got := record[k]; got != want {
			t.Fatalf("log[%s] = %v (%T), want %v", k, got, got, want)
		}
	}
}

func TestLoadConfigRejectsGlobalCostBudget(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(`{"provider_configs":["p.json"],"cost_budget":{"limit_usd":1.5,"period":"24h"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := LoadConfig(path)
	if err == nil {
		t.Fatal("LoadConfig accepted global cost_budget, want migration error")
	}
	if !strings.Contains(err.Error(), "cost_budget is no longer supported") || !strings.Contains(err.Error(), "-budget-usd") {
		t.Fatalf("global budget error = %v", err)
	}
}

func budgetedKeyEntry(name, plaintext string, limitUSD float64, period time.Duration, rejectUnpriced bool) apikey.Entry {
	return apikey.Entry{
		Name:  name,
		Hash:  apikey.Hash(plaintext),
		Added: time.Date(2026, 7, 7, 12, 0, 0, 0, time.UTC),
		CostBudget: &apikey.CostBudget{
			LimitUSD:       limitUSD,
			PeriodSeconds:  int64(period / time.Second),
			RejectUnpriced: rejectUnpriced,
		},
	}
}

func streamOnceWithKey(t *testing.T, srv *httptest.Server, provider, model, key string) {
	t.Helper()
	targetID := provider + ":" + model
	body, _ := json.Marshal(protocol.StreamRequest{TargetID: targetID, Request: llm.Request{Model: targetID}})
	req, err := http.NewRequest(http.MethodPost, srv.URL+"/v1/stream", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("new stream request: %v", err)
	}
	req.Header.Set("content-type", protocol.ContentTypeNDJSON)
	req.Header.Set("Authorization", "Bearer "+key)
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatalf("POST stream %s/%s: %v", provider, model, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("stream %s/%s status = %d", provider, model, resp.StatusCode)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
}

func usageReportWithKey(t *testing.T, srv *httptest.Server, key string) protocol.UsageReport {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, srv.URL+"/v1/usage", nil)
	if err != nil {
		t.Fatalf("new usage request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+key)
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatalf("GET usage: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("usage status = %d, want 200", resp.StatusCode)
	}
	var report protocol.UsageReport
	if err := json.NewDecoder(resp.Body).Decode(&report); err != nil {
		t.Fatalf("decode usage: %v", err)
	}
	return report
}

func TestAPIKeyCostBudgetRejectsAfterWindowSpendPersists(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "openai.json"), []byte(`{
  "name":"openai",
  "api_type":"openai",
  "base_url":"http://localhost:11434/v1",
  "models":[{"name":"priced","context_window":128000,"price":{"input":10}}]
}`), 0o600); err != nil {
		t.Fatalf("write provider config: %v", err)
	}
	now := time.Date(2026, 7, 7, 12, 0, 0, 0, time.UTC)
	clock := func() time.Time { return now }
	cfg := Config{ProviderConfigs: []string{"openai.json"}}
	secret := "hmp_budgeted"
	entry := budgetedKeyEntry("laptop", secret, 0.01, time.Hour, false)
	handler, err := NewHandler(Options{
		ConfigDir: dir,
		Config:    cfg,
		Now:       clock,
		New: fixedUsageProvider(llm.Usage{
			InputTokens: 1000, // $0.01 at $10 / 1M input tokens.
		}),
	})
	if err != nil {
		t.Fatalf("NewHandler: %v", err)
	}
	srv := httptest.NewServer(apikey.Store{Entries: []apikey.Entry{entry}}.Middleware(handler))
	streamOnceWithKey(t, srv, "openai", "priced", secret)

	report := usageReportWithKey(t, srv, secret)
	if report.Budget == nil || report.Budget.SpentUSD != 0.01 || report.Budget.RemainingUSD != 0 {
		t.Fatalf("budget report after first request = %+v", report.Budget)
	}

	body, _ := json.Marshal(protocol.StreamRequest{TargetID: "openai:priced", Request: llm.Request{Model: "openai:priced"}})
	req, err := http.NewRequest(http.MethodPost, srv.URL+"/v1/stream", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("new over-budget request: %v", err)
	}
	req.Header.Set("content-type", protocol.ContentTypeNDJSON)
	req.Header.Set("Authorization", "Bearer "+secret)
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatalf("POST over budget: %v", err)
	}
	if resp.StatusCode != http.StatusTooManyRequests {
		resp.Body.Close()
		t.Fatalf("over-budget status = %d, want 429", resp.StatusCode)
	}
	if got := resp.Header.Get("Retry-After"); got == "" {
		resp.Body.Close()
		t.Fatal("over-budget response missing Retry-After")
	}
	var apiErr protocol.Error
	if err := json.NewDecoder(resp.Body).Decode(&apiErr); err != nil {
		resp.Body.Close()
		t.Fatalf("decode budget error: %v", err)
	}
	resp.Body.Close()
	if apiErr.Code != "cost_budget_exceeded" || !apiErr.Retryable || apiErr.RetryAfterMS <= 0 {
		t.Fatalf("budget error = %+v", apiErr)
	}
	srv.Close()

	restarted, err := NewHandler(Options{
		ConfigDir: dir,
		Config:    cfg,
		Now:       clock,
		New:       fixedUsageProvider(llm.Usage{InputTokens: 1000}),
	})
	if err != nil {
		t.Fatalf("NewHandler restart: %v", err)
	}
	srv = httptest.NewServer(apikey.Store{Entries: []apikey.Entry{entry}}.Middleware(restarted))
	defer srv.Close()
	report = usageReportWithKey(t, srv, secret)
	if report.Budget == nil || report.Budget.SpentUSD != 0.01 {
		t.Fatalf("restarted budget report = %+v", report.Budget)
	}
}

func TestAPIKeyCostBudgetAllowsUnpricedTargetByDefault(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "openai.json"), []byte(`{
  "name":"openai",
  "api_type":"openai",
  "base_url":"http://localhost:11434/v1",
  "models":[{"name":"free","context_window":128000}]
}`), 0o600); err != nil {
		t.Fatalf("write provider config: %v", err)
	}
	providerCalls := 0
	handler, err := NewHandler(Options{
		ConfigDir: dir,
		Config:    Config{ProviderConfigs: []string{"openai.json"}},
		New: func(factory.Options) (llm.Provider, error) {
			providerCalls++
			return llmtest.New("fake", llmtest.Step{Events: []llm.StreamEvent{{Kind: llm.EventTextDelta, Text: "ok"}}, Stop: llm.StopEndTurn}), nil
		},
	})
	if err != nil {
		t.Fatalf("NewHandler: %v", err)
	}
	secret := "hmp_budgeted"
	entry := budgetedKeyEntry("laptop", secret, 1, time.Hour, false)
	srv := httptest.NewServer(apikey.Store{Entries: []apikey.Entry{entry}}.Middleware(handler))
	defer srv.Close()
	streamOnceWithKey(t, srv, "openai", "free", secret)
	if providerCalls != 1 {
		t.Fatalf("provider calls = %d, want 1", providerCalls)
	}
	report := usageReportWithKey(t, srv, secret)
	if report.Budget == nil || report.Budget.SpentUSD != 0 {
		t.Fatalf("budget report for unpriced request = %+v", report.Budget)
	}
}

func TestAPIKeyCostBudgetRejectsUnpricedTargetWhenConfigured(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "openai.json"), []byte(`{
  "name":"openai",
  "api_type":"openai",
  "base_url":"http://localhost:11434/v1",
  "models":[{"name":"free","context_window":128000}]
}`), 0o600); err != nil {
		t.Fatalf("write provider config: %v", err)
	}
	providerCalls := 0
	handler, err := NewHandler(Options{
		ConfigDir: dir,
		Config:    Config{ProviderConfigs: []string{"openai.json"}},
		New: func(factory.Options) (llm.Provider, error) {
			providerCalls++
			return llmtest.New("fake"), nil
		},
	})
	if err != nil {
		t.Fatalf("NewHandler: %v", err)
	}
	secret := "hmp_budgeted"
	entry := budgetedKeyEntry("laptop", secret, 1, time.Hour, true)
	srv := httptest.NewServer(apikey.Store{Entries: []apikey.Entry{entry}}.Middleware(handler))
	defer srv.Close()
	body, _ := json.Marshal(protocol.StreamRequest{TargetID: "openai:free", Request: llm.Request{Model: "openai:free"}})
	req, err := http.NewRequest(http.MethodPost, srv.URL+"/v1/stream", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("new unpriced request: %v", err)
	}
	req.Header.Set("content-type", protocol.ContentTypeNDJSON)
	req.Header.Set("Authorization", "Bearer "+secret)
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatalf("POST unpriced: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("unpriced status = %d, want 400", resp.StatusCode)
	}
	var apiErr protocol.Error
	if err := json.NewDecoder(resp.Body).Decode(&apiErr); err != nil {
		t.Fatalf("decode unpriced error: %v", err)
	}
	if apiErr.Code != "cost_budget_unpriced_target" {
		t.Fatalf("unpriced error = %+v", apiErr)
	}
	if providerCalls != 0 {
		t.Fatalf("provider calls = %d, want 0", providerCalls)
	}
}

func TestCostBudgetWindowResetAndUsageOmitWhenDisabled(t *testing.T) {
	now := time.Date(2026, 7, 7, 12, 0, 0, 0, time.UTC)
	clock := func() time.Time { return now }
	cfg := CostBudgetConfig{LimitUSD: 1, Period: Duration{Duration: time.Hour, Set: true}}
	tracker, err := newCostBudgetTrackerAtPath(cfg, filepath.Join(t.TempDir(), "budget.json"), clock)
	if err != nil {
		t.Fatalf("newCostBudgetTracker: %v", err)
	}
	if err := tracker.Add(0.75); err != nil {
		t.Fatalf("Add: %v", err)
	}
	if report := tracker.Report(); report.SpentUSD != 0.75 || report.RemainingUSD != 0.25 {
		t.Fatalf("report before reset = %+v", report)
	}
	now = now.Add(time.Hour)
	if ok, retry := tracker.Check(); !ok || retry != 0 {
		t.Fatalf("Check after reset = (%v,%v), want true,0", ok, retry)
	}
	if report := tracker.Report(); report.SpentUSD != 0 || !report.WindowStart.Equal(now) {
		t.Fatalf("report after reset = %+v, now=%v", report, now)
	}

	h := &Handler{}
	if report := h.usageSnapshot(); report.Budget != nil {
		t.Fatalf("disabled budget report = %+v, want nil", report.Budget)
	}
}

func TestHandlerRichImageRejectionReturnsSafeCorrelatedDiagnostic(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "openai.json"), []byte(`{
  "name":"openai",
  "api_type":"openai",
  "base_url":"https://api.test/v1",
  "models":[{"name":"vision","context_window":128000,"input_modalities":["text","image"]}]
}`), 0o600); err != nil {
		t.Fatalf("write provider config: %v", err)
	}

	payload := serverOnePixelPNG
	providerErr := &llm.APIError{
		StatusCode: http.StatusBadRequest,
		Code:       "invalid_image_" + payload,
		Message:    "tool message image content is unsupported at /private/screen.png: private result text " + payload,
		Diagnostic: &llm.APIErrorDiagnostic{UpstreamRequestID: "upstream-123"},
	}
	var logs bytes.Buffer
	logger, err := logging.NewProxyLogger(&logs, logging.LevelInfo, logging.FormatJSON)
	if err != nil {
		t.Fatalf("NewProxyLogger: %v", err)
	}
	fp := llmtest.New("fake", llmtest.Step{Err: providerErr})
	handler, err := NewHandler(Options{
		ConfigDir: dir,
		Config:    Config{ProviderConfigs: []string{"openai.json"}},
		Logger:    logger,
		New: func(factory.Options) (llm.Provider, error) {
			return fp, nil
		},
	})
	if err != nil {
		t.Fatalf("NewHandler: %v", err)
	}
	srv := httptest.NewServer(handler)
	defer srv.Close()

	request := richImageRequest(payload)
	request.Model = "openai:vision"
	body, err := json.Marshal(protocol.StreamRequest{TargetID: "openai:vision", Request: request})
	if err != nil {
		t.Fatalf("marshal request: %v", err)
	}
	req, err := http.NewRequest(http.MethodPost, srv.URL+"/v1/stream", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("content-type", protocol.ContentTypeNDJSON)
	const traceID = "0123456789abcdef0123456789abcdef"
	const spanID = "0123456789abcdef"
	req.Header.Set(tracing.TraceparentHeader, "00-"+traceID+"-"+spanID+"-01")
	resp, err := srv.Client().Do(req)
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
	if _, err := io.Copy(io.Discard, resp.Body); err != nil {
		t.Fatalf("drain response body: %v", err)
	}
	if env.Error == nil || env.Error.Diagnostic == nil {
		t.Fatalf("stream error = %+v", env.Error)
	}
	diagnostic := env.Error.Diagnostic
	if diagnostic.Stage != llm.APIErrorStageUpstreamHTTP || diagnostic.ProxyRequestID == 0 || diagnostic.UpstreamRequestID != "upstream-123" || diagnostic.TraceID != traceID || diagnostic.SpanID != spanID {
		t.Fatalf("diagnostic correlation = %+v", diagnostic)
	}
	if diagnostic.TargetID != "openai:vision" || diagnostic.Provider != "openai" || diagnostic.APIType != "openai" || diagnostic.Model != "vision" {
		t.Fatalf("diagnostic target = %+v", diagnostic)
	}
	if diagnostic.Compatibility == nil || diagnostic.Compatibility.Category != llm.CompatibilityCategoryMultimodalToolResultRejected {
		t.Fatalf("compatibility = %+v", diagnostic.Compatibility)
	}
	shape := diagnostic.MultimodalShape
	if shape == nil || shape.Strategy != llm.MultimodalStrategyOpenAIToolThenUserImage || shape.ImageCount != 1 || shape.ToolResultCount != 1 || shape.EncodedBytes != int64(len(payload)) {
		t.Fatalf("shape = %+v", shape)
	}
	for _, text := range []string{env.Error.Message, env.Error.Code, logs.String()} {
		for _, secret := range []string{payload, "/private/screen.png", "private result text", "private system prompt", "nested-secret"} {
			if strings.Contains(text, secret) {
				t.Fatalf("output contains %q: %s", secret, text)
			}
		}
	}

	var record map[string]any
	if err := json.Unmarshal(logs.Bytes(), &record); err != nil {
		t.Fatalf("decode log %q: %v", logs.String(), err)
	}
	if record["error_stage"] != string(llm.APIErrorStageUpstreamHTTP) || record["category"] != llm.CompatibilityCategoryMultimodalToolResultRejected || record["trace_id"] != traceID {
		t.Fatalf("completion log = %+v", record)
	}
	if got, ok := record["proxy_request_id"].(float64); !ok || uint64(got) != diagnostic.ProxyRequestID {
		t.Fatalf("proxy request ids do not correlate: log=%v diagnostic=%d", record["proxy_request_id"], diagnostic.ProxyRequestID)
	}
}

func TestHandlerSuccessfulRichImageLogsSafeShapeAndCorrelationOnly(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "openai.json"), []byte(`{
  "name":"openai",
  "api_type":"openai",
  "base_url":"https://api.test/v1",
  "models":[{"name":"vision","context_window":128000,"input_modalities":["text","image"]}]
}`), 0o600); err != nil {
		t.Fatalf("write provider config: %v", err)
	}

	payload := serverOnePixelPNG
	var logs bytes.Buffer
	logger, err := logging.NewProxyLogger(&logs, logging.LevelInfo, logging.FormatJSON)
	if err != nil {
		t.Fatalf("NewProxyLogger: %v", err)
	}
	fp := llmtest.New("fake", llmtest.Step{
		Events: []llm.StreamEvent{{Kind: llm.EventTextDelta, Text: "ok"}},
		Stop:   llm.StopEndTurn,
	})
	handler, err := NewHandler(Options{
		ConfigDir: dir,
		Config:    Config{ProviderConfigs: []string{"openai.json"}},
		Logger:    logger,
		New: func(factory.Options) (llm.Provider, error) {
			return fp, nil
		},
	})
	if err != nil {
		t.Fatalf("NewHandler: %v", err)
	}
	srv := httptest.NewServer(handler)
	defer srv.Close()

	request := richImageRequest(payload)
	request.Model = "openai:vision"
	body, err := json.Marshal(protocol.StreamRequest{TargetID: "openai:vision", Request: request})
	if err != nil {
		t.Fatalf("marshal request: %v", err)
	}
	req, err := http.NewRequest(http.MethodPost, srv.URL+"/v1/stream", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("content-type", protocol.ContentTypeNDJSON)
	const traceID = "1123456789abcdef0123456789abcdef"
	const spanID = "1123456789abcdef"
	req.Header.Set(tracing.TraceparentHeader, "00-"+traceID+"-"+spanID+"-01")
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatalf("POST stream: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if _, err := io.Copy(io.Discard, resp.Body); err != nil {
		t.Fatalf("drain response body: %v", err)
	}

	var record map[string]any
	if err := json.Unmarshal(logs.Bytes(), &record); err != nil {
		t.Fatalf("decode log %q: %v", logs.String(), err)
	}
	if record["msg"] != "model request completed" || record["status"] != float64(http.StatusOK) || record["trace_id"] != traceID || record["span_id"] != spanID {
		t.Fatalf("completion correlation = %+v", record)
	}
	if requestID, ok := record["request_id"].(float64); !ok || requestID <= 0 {
		t.Fatalf("request_id = %v, want positive correlation id", record["request_id"])
	}
	shape, ok := record["multimodal_shape"].(map[string]any)
	if !ok {
		t.Fatalf("multimodal_shape = %v (%T), want object", record["multimodal_shape"], record["multimodal_shape"])
	}
	safeShapeKeys := map[string]bool{
		"strategy": true, "tool_result_count": true, "image_count": true,
		"mime_types": true, "details": true, "encoded_bytes": true,
		"decoded_bytes": true, "dimensions": true, "result_ids_sha256": true,
		"image_payloads_sha256": true,
	}
	for key := range shape {
		if !safeShapeKeys[key] {
			t.Fatalf("multimodal_shape contains unsafe field %q: %+v", key, shape)
		}
	}
	if shape["strategy"] != llm.MultimodalStrategyOpenAIToolThenUserImage || shape["tool_result_count"] != float64(1) || shape["image_count"] != float64(1) || shape["encoded_bytes"] != float64(len(payload)) {
		t.Fatalf("multimodal_shape = %+v", shape)
	}
	for _, secret := range []string{
		"private system prompt",
		"view_image",
		"call-private",
		"/private/screen.png",
		"nested-secret",
		"private result text",
		"data:image/png;base64,",
		payload,
	} {
		if strings.Contains(logs.String(), secret) {
			t.Fatalf("completion log contains sensitive request value %q: %s", secret, logs.String())
		}
	}
}
