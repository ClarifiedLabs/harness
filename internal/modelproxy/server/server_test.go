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
	"strings"
	"testing"
	"time"

	"harness/internal/apikey"
	"harness/internal/llm"
	"harness/internal/llm/factory"
	"harness/internal/llm/llmtest"
	"harness/internal/logging"
	"harness/internal/modelproxy/protocol"
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
	if len(catalog.Targets) != 1 || catalog.Targets[0].ID != "openrouter:openai/gpt-5.5" {
		t.Fatalf("catalog targets = %+v", catalog.Targets)
	}
	if catalog.Targets[0].OutputLimit != 64_000 || !slices.Equal(catalog.Targets[0].InputModalities, []string{"text", "image"}) {
		t.Fatalf("catalog target = %+v, want output limit 64000", catalog.Targets[0])
	}

	body, _ := json.Marshal(protocol.StreamRequest{
		TargetID: "openrouter:openai/gpt-5.5",
		Request:  llm.Request{Model: "openrouter:openai/gpt-5.5"},
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
		llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{{Kind: llm.BlockText, Text: "first answer"}}},
		llm.Message{Role: llm.RoleUser, Content: []llm.ContentBlock{{Kind: llm.BlockText, Text: "again"}}},
	)
	body, _ = json.Marshal(protocol.StreamRequest{
		TargetID: "openai:gpt-5.5",
		Request:  llm.Request{Model: "openai:gpt-5.5", PromptCacheKey: "session-a", Messages: fullMessages},
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
	if len(fp.Requests) != 2 || fp.Requests[1].PreviousResponseID != "resp_1" || len(fp.Requests[1].Messages) != 1 {
		t.Fatalf("second provider request = %+v", fp.Requests)
	}
	if got := fp.Requests[1].Messages[0].Content[0].Text; got != "again" {
		t.Fatalf("second provider request message = %q, want again", got)
	}

	fullMessages = append(fullMessages,
		llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{{Kind: llm.BlockText, Text: "again answer"}}},
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
	if len(fp.Requests) != 3 || fp.Requests[2].PreviousResponseID != "resp_2" || len(fp.Requests[2].Messages) != 1 {
		t.Fatalf("third provider request = %+v", fp.Requests)
	}
	if got := fp.Requests[2].Messages[0].Content[0].Text; got != "third" {
		t.Fatalf("third provider request message = %q, want third", got)
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
		llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{{Kind: llm.BlockText, Text: "first answer"}}},
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
		Request:  llm.Request{Model: "openai-codex:gpt-5.5", PromptCacheKey: "session-a"},
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
	if captured.Provider != "responses" || !captured.OmitMaxOutputTokens || !captured.ResponsesWebSocket {
		t.Fatalf("captured options = %+v, want responses with OmitMaxOutputTokens and ResponsesWebSocket", captured)
	}
	if constructions != 1 {
		t.Fatalf("provider constructions = %d, want 1 for cached websocket provider", constructions)
	}
	if len(fp.Requests) != 2 {
		t.Fatalf("fake provider requests = %d, want 2", len(fp.Requests))
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
		Request:  llm.Request{Model: "openai:priced"},
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
	cfg := Config{
		ProviderConfigs: []string{"openai.json"},
		APIKeys: []apikey.Entry{
			{Name: "laptop", Hash: apikey.Hash("hmp_secret")},
		},
	}
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
	srv := httptest.NewServer(cfg.APIKeyStore().Middleware(handler))
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
