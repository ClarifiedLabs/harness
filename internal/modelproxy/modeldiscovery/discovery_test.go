package modeldiscovery

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"harness/internal/llm"
	"harness/internal/modelcatalog"
)

func TestOpenAIDiscoveryUsesAuthAndCapabilityPolicy(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer secret" {
			t.Errorf("Authorization = %q", got)
		}
		if r.URL.Path != "/v1/models" {
			t.Errorf("path = %q", r.URL.Path)
		}
		w.Header().Set("ETag", `"models-1"`)
		_, _ = w.Write([]byte(`{"data":[{"id":"basic"},{"id":"rich","context_length":131072,"input_modalities":["image","text"],"supports_reasoning":true,"reasoning_efforts":["low","high"]}]}`))
	}))
	defer server.Close()

	pc := llm.ProviderConfig{Name: "compatible", APIType: "openai", BaseURL: server.URL + "/v1", APIKey: "secret", Managed: true}
	snapshot, err := (Fetcher{Client: server.Client(), Now: func() time.Time { return time.Unix(100, 0) }}).Fetch(context.Background(), pc, nil)
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.ETag != `"models-1"` || len(snapshot.Models) != 2 {
		t.Fatalf("snapshot = %+v", snapshot)
	}
	if snapshot.Models["basic"].Eligible {
		t.Fatal("basic ID-only model unexpectedly eligible as a live-only model")
	}
	rich := snapshot.Models["rich"]
	if !rich.Eligible || rich.ContextWindow == nil || *rich.ContextWindow != 131072 || rich.Reasoning == nil || !*rich.Reasoning {
		t.Fatalf("rich model = %+v", rich)
	}
}

func TestResolveDiscoveryOverride(t *testing.T) {
	t.Parallel()
	enabled := true
	includeUnknown := true
	pc := llm.ProviderConfig{
		Name: "custom", APIType: "unknown", BaseURL: "https://api.example/v1",
		ModelDiscovery: &llm.ModelDiscoveryConfig{Enabled: &enabled, URL: "https://catalog.example/models", Format: "openai", IncludeUnknownModels: &includeUnknown},
	}
	spec, ok, err := Resolve(pc)
	if err != nil || !ok || spec.Endpoint != "https://catalog.example/models" || spec.Format != FormatOpenAI || !spec.IncludeUnknownModels {
		t.Fatalf("Resolve = %+v, %v, %v", spec, ok, err)
	}
	disabled := false
	pc.ModelDiscovery.Enabled = &disabled
	if _, ok, err := Resolve(pc); err != nil || ok {
		t.Fatalf("disabled Resolve returned ok=%v err=%v", ok, err)
	}
	pc.ModelDiscovery.Enabled = &enabled
	pc.ModelDiscovery.URL = "https://user:secret@catalog.example/models"
	if _, _, err := Resolve(pc); err == nil {
		t.Fatal("URL with userinfo was accepted")
	}
}

func TestExplicitDiscoveryURL404IsRefreshFailureNotUnsupportedDetection(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.NotFoundHandler())
	defer server.Close()
	enabled := true
	pc := llm.ProviderConfig{
		Name: "custom", APIType: "openai", BaseURL: "https://api.example/v1", APIKey: "secret",
		ModelDiscovery: &llm.ModelDiscoveryConfig{Enabled: &enabled, URL: server.URL + "/catalog", Format: "openai"},
	}
	_, err := (Fetcher{Client: server.Client()}).Fetch(context.Background(), pc, nil)
	if err == nil || errors.Is(err, ErrUnsupported) {
		t.Fatalf("Fetch error = %v, want non-unsupported refresh failure", err)
	}
}

func TestDiscoveryETag304RenewsSnapshot(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("If-None-Match"); got != `"models-1"` {
			t.Errorf("If-None-Match = %q", got)
		}
		w.WriteHeader(http.StatusNotModified)
	}))
	defer server.Close()

	previous := Snapshot{Version: 1, Provider: "sakana", BaseURL: server.URL + "/v1", Endpoint: server.URL + "/v1/models", Format: FormatOpenAI, FetchedAt: time.Unix(10, 0), ETag: `"models-1"`, Complete: true, IncludeUnknownModels: true, Models: map[string]Model{"fugu": {ID: "fugu", Eligible: true}}}
	pc := llm.ProviderConfig{Name: "sakana", APIType: "openai", BaseURL: server.URL + "/v1", APIKey: "secret", Managed: true}
	snapshot, err := (Fetcher{Client: server.Client(), Now: func() time.Time { return time.Unix(20, 0) }}).Fetch(context.Background(), pc, &previous)
	if err != nil {
		t.Fatal(err)
	}
	if !snapshot.FetchedAt.Equal(time.Unix(20, 0)) || len(snapshot.Models) != 1 {
		t.Fatalf("renewed snapshot = %+v", snapshot)
	}
}

func TestDiscoveryRejects304ForMismatchedSnapshot(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("If-None-Match"); got != "" {
			t.Errorf("If-None-Match = %q, want empty for mismatched cache", got)
		}
		w.WriteHeader(http.StatusNotModified)
	}))
	defer server.Close()

	previous := Snapshot{Version: 1, Provider: "sakana", BaseURL: server.URL + "/v1", Endpoint: server.URL + "/old/models", Format: FormatOpenAI, ETag: `"old"`, Complete: true, IncludeUnknownModels: true, Models: map[string]Model{"old": {ID: "old", Eligible: true}}}
	pc := llm.ProviderConfig{Name: "sakana", APIType: "openai", BaseURL: server.URL + "/v1", APIKey: "secret", Managed: true}
	if _, err := (Fetcher{Client: server.Client()}).Fetch(context.Background(), pc, &previous); err == nil {
		t.Fatal("Fetch accepted 304 for mismatched snapshot")
	}
}

func TestOpenRouterDiscoveryMapsRichMetadataAndPricing(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.URL.Query().Get("limit"); got != "" {
			t.Errorf("undocumented limit query = %q", got)
		}
		_, _ = w.Write([]byte(`{"data":[
          {"id":"vendor/text","name":"Text","context_length":200000,"architecture":{"input_modalities":["text","image"],"output_modalities":["text"]},"top_provider":{"max_completion_tokens":8192},"supported_parameters":["reasoning"],"reasoning":{"supported_efforts":["low","high"]},"pricing":{"prompt":"0.000002","completion":"0.000006","input_cache_read":"0.0000005"}},
          {"id":"vendor/image","architecture":{"output_modalities":["image"]},"pricing":{"prompt":"0.1"}}
        ]}`))
	}))
	defer server.Close()
	pc := llm.ProviderConfig{Name: "openrouter", APIType: "openai", BaseURL: server.URL, APIKey: "secret", Managed: true}
	snapshot, err := (Fetcher{Client: server.Client()}).Fetch(context.Background(), pc, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshot.Models) != 1 {
		t.Fatalf("models = %+v", snapshot.Models)
	}
	model := snapshot.Models["vendor/text"]
	if model.Price == nil || model.Price.Input != 2 || model.Price.Output != 6 || model.Price.CacheRead != .5 {
		t.Fatalf("price = %+v", model.Price)
	}
	if model.OutputLimit == nil || *model.OutputLimit != 8192 || model.Reasoning == nil || !*model.Reasoning {
		t.Fatalf("model = %+v", model)
	}
}

func TestAnthropicDiscoveryPaginatesWithProviderHeaders(t *testing.T) {
	t.Parallel()
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		if r.Header.Get("x-api-key") != "secret" || r.Header.Get("anthropic-version") == "" {
			t.Errorf("headers = %+v", r.Header)
		}
		if requests == 1 {
			_, _ = w.Write([]byte(`{"data":[{"id":"claude-a","display_name":"A"}],"has_more":true,"last_id":"cursor-a"}`))
			return
		}
		if got := r.URL.Query().Get("after_id"); got != "cursor-a" {
			t.Errorf("after_id = %q", got)
		}
		_, _ = w.Write([]byte(`{"data":[{"id":"claude-b","display_name":"B"}],"has_more":false}`))
	}))
	defer server.Close()
	pc := llm.ProviderConfig{Name: "anthropic", APIType: "anthropic", BaseURL: server.URL, APIKey: "secret", Managed: true}
	snapshot, err := (Fetcher{Client: server.Client()}).Fetch(context.Background(), pc, nil)
	if err != nil {
		t.Fatal(err)
	}
	if requests != 2 || len(snapshot.Models) != 2 {
		t.Fatalf("requests=%d models=%+v", requests, snapshot.Models)
	}
}

func TestGeminiDiscoveryFiltersGenerateContentAndPaginates(t *testing.T) {
	t.Parallel()
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		if r.Header.Get("x-goog-api-key") != "secret" {
			t.Errorf("x-goog-api-key = %q", r.Header.Get("x-goog-api-key"))
		}
		if requests == 1 {
			_, _ = w.Write([]byte(`{"models":[{"name":"models/gemini-a","displayName":"A","inputTokenLimit":1000,"outputTokenLimit":100,"supportedGenerationMethods":["generateContent"]},{"name":"models/embed","supportedGenerationMethods":["embedContent"]}],"nextPageToken":"next"}`))
			return
		}
		if got := r.URL.Query().Get("pageToken"); got != "next" {
			t.Errorf("pageToken = %q", got)
		}
		_, _ = w.Write([]byte(`{"models":[{"name":"models/gemini-b","supportedGenerationMethods":["generateContent"]}]}`))
	}))
	defer server.Close()
	pc := llm.ProviderConfig{Name: "google", APIType: "interactions", BaseURL: server.URL, APIKey: "secret", Managed: true}
	snapshot, err := (Fetcher{Client: server.Client()}).Fetch(context.Background(), pc, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshot.Models) != 2 || snapshot.Models["gemini-a"].ContextWindow == nil {
		t.Fatalf("models = %+v", snapshot.Models)
	}
}

func TestCodexDiscoveryUsesAccountCatalogVisibility(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("client_version") != "0.99.0" {
			t.Errorf("client_version = %q", r.URL.Query().Get("client_version"))
		}
		_, _ = w.Write([]byte(`{"models":[{"slug":"spark","display_name":"Spark","context_window":100000,"visibility":"list","supported_in_api":false},{"slug":"hidden","context_window":100000,"visibility":"hide"}]}`))
	}))
	defer server.Close()
	pc := llm.ProviderConfig{Name: "openai-codex", APIType: "responses", BaseURL: server.URL, APIKey: "secret", Managed: true}
	snapshot, err := (Fetcher{Client: server.Client(), CodexClientVersion: "0.99.0"}).Fetch(context.Background(), pc, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshot.Models) != 1 || snapshot.Models["spark"].Name != "Spark" || snapshot.CodexClientVersion != "0.99.0" {
		t.Fatalf("models = %+v", snapshot.Models)
	}
}

func TestCodexDiscoveryRejectsInvalidClientVersionBeforeRequest(t *testing.T) {
	t.Parallel()
	var requests atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer server.Close()

	pc := llm.ProviderConfig{Name: "openai-codex", APIType: "responses", BaseURL: server.URL, APIKey: "secret", Managed: true}
	_, err := (Fetcher{Client: server.Client(), CodexClientVersion: "dev"}).Fetch(context.Background(), pc, nil)
	if err == nil || !strings.Contains(err.Error(), "invalid Codex client version") {
		t.Fatalf("Fetch error = %v, want invalid Codex client version", err)
	}
	if requests.Load() != 0 {
		t.Fatalf("invalid client version made %d requests", requests.Load())
	}
}

func TestCodexDiscoveryDoesNotReuseETagAcrossClientVersions(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.URL.Query().Get("client_version"); got != "0.99.0" {
			t.Errorf("client_version = %q", got)
		}
		if got := r.Header.Get("If-None-Match"); got != "" {
			t.Errorf("If-None-Match = %q, want empty after client version change", got)
		}
		_, _ = w.Write([]byte(`{"models":[{"slug":"spark","context_window":100000,"visibility":"list"}]}`))
	}))
	defer server.Close()

	pc := llm.ProviderConfig{Name: "openai-codex", APIType: "responses", BaseURL: server.URL, APIKey: "secret", Managed: true}
	previous := Snapshot{
		Version: 1, Provider: pc.Name, BaseURL: pc.BaseURL, Endpoint: server.URL + "/models",
		Format: FormatCodex, CodexClientVersion: "0.98.0", ETag: `"old"`, Complete: true,
		IncludeUnknownModels: true, Models: map[string]Model{"old": {ID: "old", Eligible: true}},
	}
	snapshot, err := (Fetcher{Client: server.Client(), CodexClientVersion: "0.99.0"}).Fetch(context.Background(), pc, &previous)
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.CodexClientVersion != "0.99.0" || len(snapshot.Models) != 1 {
		t.Fatalf("snapshot = %+v", snapshot)
	}
}

func TestMergeProviderKeepsKnownBasicIDsAndEligibleLiveOnlyModels(t *testing.T) {
	t.Parallel()
	baseline := modelcatalog.Provider{ID: "p", Models: map[string]modelcatalog.Model{"known": {ID: "known", Limit: modelcatalog.Limit{Context: 100}}}}
	contextWindow := 200
	snapshot := Snapshot{Models: map[string]Model{
		"known":      {ID: "known", Eligible: false, ContextWindow: &contextWindow},
		"live-rich":  {ID: "live-rich", Eligible: true, ContextWindow: &contextWindow},
		"live-basic": {ID: "live-basic", Eligible: false},
	}}
	merged := MergeProvider(baseline, snapshot)
	if len(merged.Models) != 2 || merged.Models["known"].Limit.Context != 200 {
		t.Fatalf("merged = %+v", merged.Models)
	}
}

func TestProviderCacheRoundTripAndFreshness(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	now := time.Unix(1000, 0)
	snapshot := Snapshot{Version: 1, Provider: "p/name", BaseURL: "https://example.test/v1", Endpoint: "https://example.test/v1/models", Format: FormatOpenAI, FetchedAt: now, Complete: true, Models: map[string]Model{"m": {ID: "m", Eligible: true}}}
	if err := WriteCache(dir, snapshot); err != nil {
		t.Fatal(err)
	}
	loaded, err := ReadProviderCache(dir, snapshot.Provider, snapshot.BaseURL)
	if err != nil {
		t.Fatal(err)
	}
	if len(loaded.Models) != 1 || !StateFromCache(loaded, now.Add(30*time.Minute), time.Hour).Authoritative || StateFromCache(loaded, now.Add(2*time.Hour), time.Hour).Authoritative {
		t.Fatalf("loaded = %+v", loaded)
	}
	info, err := os.Stat(CachePath(dir, snapshot.Provider))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("cache mode = %o", info.Mode().Perm())
	}
	if filepath.Dir(CachePath(dir, snapshot.Provider)) != filepath.Join(dir, cacheDirectory) {
		t.Fatal("unexpected cache directory")
	}
	data, _ := os.ReadFile(CachePath(dir, snapshot.Provider))
	var decoded Snapshot
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatal(err)
	}
}
