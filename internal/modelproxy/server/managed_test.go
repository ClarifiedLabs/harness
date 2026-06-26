package server

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"harness/internal/llm"
	"harness/internal/llm/factory"
	"harness/internal/llm/llmtest"
	"harness/internal/modelproxy/protocol"
	"harness/internal/modelsdev"
)

// modelsDevCatalogWith builds a single-provider models.dev catalog whose one
// model carries the given price, for exercising managed-price resolution.
func modelsDevCatalogWith(providerID, modelID string, price llm.Price) *modelsdev.Catalog {
	return &modelsdev.Catalog{Providers: map[string]modelsdev.Provider{
		providerID: {
			ID: providerID,
			Models: map[string]modelsdev.Model{
				modelID: {ID: modelID, Cost: price},
			},
		},
	}}
}

func catalogModelPrice(t *testing.T, c protocol.Catalog, providerID, modelID string) llm.Price {
	t.Helper()
	id := providerID + ":" + modelID
	for _, target := range c.Targets {
		if target.ID == id {
			return target.Price
		}
	}
	t.Fatalf("target %s not found in catalog %+v", id, c.Targets)
	return llm.Price{}
}

// streamOnce drives one /v1/stream request returning fixed usage and discards
// the body, so the handler's deferred cost accounting runs.
func streamOnce(t *testing.T, srv *httptest.Server, provider, model string) {
	t.Helper()
	targetID := provider + ":" + model
	body, _ := json.Marshal(protocol.StreamRequest{
		TargetID: targetID,
		Request:  llm.Request{Model: targetID},
	})
	resp, err := srv.Client().Post(srv.URL+"/v1/stream", protocol.ContentTypeNDJSON, bytes.NewReader(body))
	if err != nil {
		t.Fatalf("POST stream %s/%s: %v", provider, model, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("stream %s/%s status = %d", provider, model, resp.StatusCode)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
}

func usageCost(t *testing.T, srv *httptest.Server, model string) (float64, bool) {
	t.Helper()
	resp, err := srv.Client().Get(srv.URL + "/v1/usage")
	if err != nil {
		t.Fatalf("GET usage: %v", err)
	}
	defer resp.Body.Close()
	var report protocol.UsageReport
	if err := json.NewDecoder(resp.Body).Decode(&report); err != nil {
		t.Fatalf("decode usage: %v", err)
	}
	for _, m := range report.Models {
		if strings.HasSuffix(m.TargetID, ":"+model) {
			return m.CostUSD, true
		}
	}
	return 0, false
}

func fixedUsageProvider(usage llm.Usage) func(factory.Options) (llm.Provider, error) {
	return func(factory.Options) (llm.Provider, error) {
		return llmtest.New("fake", llmtest.Step{
			Events: []llm.StreamEvent{{Kind: llm.EventTextDelta, Text: "ok"}},
			Stop:   llm.StopEndTurn,
			Usage:  usage,
		}), nil
	}
}

// TestManagedProviderResolvesPriceFromModelsDevCatalog: a managed config stores
// no per-model price; the served catalog and request cost come from the
// supplied models.dev catalog.
func TestManagedProviderResolvesPriceFromModelsDevCatalog(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "testai.json"), []byte(`{
  "name": "testai",
  "api_type": "openai",
  "base_url": "https://api.test/v1",
  "managed": true,
  "models": [{"name":"alpha","context_window":123000}]
}`), 0o600); err != nil {
		t.Fatalf("write provider config: %v", err)
	}

	md := modelsDevCatalogWith("testai", "alpha", llm.Price{Input: 2, Output: 4})
	usage := llm.Usage{InputTokens: 1000, OutputTokens: 2000}
	handler, err := NewHandler(Options{
		ConfigDir:           dir,
		Config:              Config{ProviderConfigs: []string{"testai.json"}},
		ModelsDevCatalog:    md,
		ModelsDevSourceDate: time.Unix(1_700_000_000, 0),
		New:                 fixedUsageProvider(usage),
	})
	if err != nil {
		t.Fatalf("NewHandler: %v", err)
	}
	srv := httptest.NewServer(handler)
	defer srv.Close()

	if got := catalogModelPrice(t, handler.Catalog(), "testai", "alpha"); got != (llm.Price{Input: 2, Output: 4}) {
		t.Fatalf("served managed price = %+v, want resolved {2,4} from models.dev", got)
	}

	streamOnce(t, srv, "testai", "alpha")
	cost, ok := usageCost(t, srv, "alpha")
	if !ok {
		t.Fatalf("managed model produced no priced usage entry")
	}
	want := 1000.0/1e6*2 + 2000.0/1e6*4
	if diff := cost - want; diff > 1e-9 || diff < -1e-9 {
		t.Fatalf("managed cost = %v, want %v", cost, want)
	}

	if handler.Catalog().Pricing == nil || !handler.Catalog().Pricing.SourceDate.Equal(time.Unix(1_700_000_000, 0)) {
		t.Fatalf("managed pricing source date = %+v, want models.dev source date", handler.Catalog().Pricing)
	}
}

// TestManagedPriceSourceResolvesFromOtherProvider: a managed config whose
// price_source names a different models.dev provider (the openai-codex case,
// whose models are billed at OpenAI rates) resolves its prices from that
// provider rather than its own name.
func TestManagedPriceSourceResolvesFromOtherProvider(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "openai-codex.json"), []byte(`{
  "name": "openai-codex",
  "api_type": "responses",
  "base_url": "https://chatgpt.com/backend-api/codex",
  "managed": true,
  "price_source": "openai",
  "models": [{"name":"gpt-5-codex","context_window":400000}]
}`), 0o600); err != nil {
		t.Fatalf("write provider config: %v", err)
	}

	// The price lives under the "openai" provider, not "openai-codex".
	md := modelsDevCatalogWith("openai", "gpt-5-codex", llm.Price{Input: 1.25, Output: 10})
	handler, err := NewHandler(Options{
		ConfigDir:           dir,
		Config:              Config{ProviderConfigs: []string{"openai-codex.json"}},
		ModelsDevCatalog:    md,
		ModelsDevSourceDate: time.Unix(1_700_000_000, 0),
		New:                 fixedUsageProvider(llm.Usage{InputTokens: 1000, OutputTokens: 2000}),
	})
	if err != nil {
		t.Fatalf("NewHandler: %v", err)
	}
	if got := catalogModelPrice(t, handler.Catalog(), "openai-codex", "gpt-5-codex"); got != (llm.Price{Input: 1.25, Output: 10}) {
		t.Fatalf("codex managed price = %+v, want {1.25,10} resolved from openai via price_source", got)
	}
}

// TestManualProviderKeepsConfigPrice: a config without managed keeps its own
// price even when a models.dev catalog offers a different one for the same id.
func TestManualProviderKeepsConfigPrice(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "testai.json")
	if err := os.WriteFile(cfgPath, []byte(`{
  "name": "testai",
  "api_type": "openai",
  "base_url": "https://api.test/v1",
  "models": [{"name":"alpha","context_window":123000,"price":{"input":2,"output":4}}]
}`), 0o600); err != nil {
		t.Fatalf("write provider config: %v", err)
	}
	info, err := os.Stat(cfgPath)
	if err != nil {
		t.Fatalf("stat config: %v", err)
	}

	// models.dev offers a wildly different price; the manual config must win.
	md := modelsDevCatalogWith("testai", "alpha", llm.Price{Input: 99, Output: 99})
	usage := llm.Usage{InputTokens: 1000, OutputTokens: 2000}
	handler, err := NewHandler(Options{
		ConfigDir:           dir,
		Config:              Config{ProviderConfigs: []string{"testai.json"}},
		ModelsDevCatalog:    md,
		ModelsDevSourceDate: time.Unix(1_700_000_000, 0),
		PricingMaxAge:       24 * time.Hour,
		New:                 fixedUsageProvider(usage),
	})
	if err != nil {
		t.Fatalf("NewHandler: %v", err)
	}
	srv := httptest.NewServer(handler)
	defer srv.Close()

	if got := catalogModelPrice(t, handler.Catalog(), "testai", "alpha"); got != (llm.Price{Input: 2, Output: 4}) {
		t.Fatalf("served manual price = %+v, want config {2,4} (not models.dev {99,99})", got)
	}

	streamOnce(t, srv, "testai", "alpha")
	cost, ok := usageCost(t, srv, "alpha")
	if !ok {
		t.Fatalf("manual model produced no priced usage entry")
	}
	want := 1000.0/1e6*2 + 2000.0/1e6*4
	if diff := cost - want; diff > 1e-9 || diff < -1e-9 {
		t.Fatalf("manual cost = %v, want %v from config price", cost, want)
	}

	// Manual-only catalog dates prices to the provider-config mtime, not the
	// models.dev source date.
	if handler.Catalog().Pricing == nil || !handler.Catalog().Pricing.SourceDate.Equal(info.ModTime()) {
		t.Fatalf("manual pricing source date = %+v, want config mtime %v", handler.Catalog().Pricing, info.ModTime())
	}
}

// TestUpdateModelsDevCatalogSwapsManagedPrices: refreshing the models.dev
// catalog re-prices managed models in the served catalog and in cost accounting
// without a restart, and restamps the pricing source date.
func TestUpdateModelsDevCatalogSwapsManagedPrices(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "testai.json"), []byte(`{
  "name": "testai",
  "api_type": "openai",
  "base_url": "https://api.test/v1",
  "managed": true,
  "models": [{"name":"alpha","context_window":123000}]
}`), 0o600); err != nil {
		t.Fatalf("write provider config: %v", err)
	}

	usage := llm.Usage{InputTokens: 1000, OutputTokens: 2000}
	firstDate := time.Unix(1_700_000_000, 0)
	handler, err := NewHandler(Options{
		ConfigDir:           dir,
		Config:              Config{ProviderConfigs: []string{"testai.json"}},
		ModelsDevCatalog:    modelsDevCatalogWith("testai", "alpha", llm.Price{Input: 2, Output: 4}),
		ModelsDevSourceDate: firstDate,
		New:                 fixedUsageProvider(usage),
	})
	if err != nil {
		t.Fatalf("NewHandler: %v", err)
	}
	srv := httptest.NewServer(handler)
	defer srv.Close()

	if got := catalogModelPrice(t, handler.Catalog(), "testai", "alpha"); got != (llm.Price{Input: 2, Output: 4}) {
		t.Fatalf("initial managed price = %+v, want {2,4}", got)
	}

	// Live refresh: new prices arrive without restarting the server.
	secondDate := time.Unix(1_700_086_400, 0)
	handler.UpdateModelsDevCatalog(modelsDevCatalogWith("testai", "alpha", llm.Price{Input: 6, Output: 8}), secondDate)

	if got := catalogModelPrice(t, handler.Catalog(), "testai", "alpha"); got != (llm.Price{Input: 6, Output: 8}) {
		t.Fatalf("post-refresh managed price = %+v, want {6,8}", got)
	}
	if handler.Catalog().Pricing == nil || !handler.Catalog().Pricing.SourceDate.Equal(secondDate) {
		t.Fatalf("post-refresh source date = %+v, want %v", handler.Catalog().Pricing, secondDate)
	}

	streamOnce(t, srv, "testai", "alpha")
	cost, ok := usageCost(t, srv, "alpha")
	if !ok {
		t.Fatalf("refreshed managed model produced no priced usage entry")
	}
	want := 1000.0/1e6*6 + 2000.0/1e6*8
	if diff := cost - want; diff > 1e-9 || diff < -1e-9 {
		t.Fatalf("refreshed cost = %v, want %v from new managed price", cost, want)
	}
}

// TestUpdateModelsDevCatalogConcurrentWithRequests exercises the atomic
// snapshot swap under -race: a writer keeps swapping catalogs while readers hit
// /v1/models, /v1/stream, and Catalog().
func TestUpdateModelsDevCatalogConcurrentWithRequests(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "testai.json"), []byte(`{
  "name": "testai",
  "api_type": "openai",
  "base_url": "https://api.test/v1",
  "managed": true,
  "models": [{"name":"alpha","context_window":123000}]
}`), 0o600); err != nil {
		t.Fatalf("write provider config: %v", err)
	}
	handler, err := NewHandler(Options{
		ConfigDir:           dir,
		Config:              Config{ProviderConfigs: []string{"testai.json"}},
		ModelsDevCatalog:    modelsDevCatalogWith("testai", "alpha", llm.Price{Input: 1, Output: 1}),
		ModelsDevSourceDate: time.Unix(1_700_000_000, 0),
		New:                 fixedUsageProvider(llm.Usage{InputTokens: 100, OutputTokens: 200}),
	})
	if err != nil {
		t.Fatalf("NewHandler: %v", err)
	}
	srv := httptest.NewServer(handler)
	defer srv.Close()

	done := make(chan struct{})
	go func() {
		for i := 0; ; i++ {
			select {
			case <-done:
				return
			default:
			}
			price := llm.Price{Input: float64(i%5) + 1, Output: float64(i%7) + 1}
			handler.UpdateModelsDevCatalog(modelsDevCatalogWith("testai", "alpha", price), time.Unix(int64(1_700_000_000+i), 0))
		}
	}()

	for range 30 {
		resp, err := srv.Client().Get(srv.URL + "/v1/models")
		if err != nil {
			t.Fatalf("GET models: %v", err)
		}
		var catalog protocol.Catalog
		_ = json.NewDecoder(resp.Body).Decode(&catalog)
		resp.Body.Close()
		_ = handler.Catalog()
		streamOnce(t, srv, "testai", "alpha")
	}
	close(done)
}

func TestModelsDevPriceLookup(t *testing.T) {
	md := modelsDevCatalogWith("testai", "alpha", llm.Price{Input: 3, Output: 5})
	if price, ok := modelsDevPrice(md, "testai", "alpha"); !ok || price != (llm.Price{Input: 3, Output: 5}) {
		t.Fatalf("modelsDevPrice(testai/alpha) = %+v, %v; want {3,5}, true", price, ok)
	}
	if _, ok := modelsDevPrice(md, "testai", "missing"); ok {
		t.Fatalf("modelsDevPrice for unknown model = ok, want not ok")
	}
	if _, ok := modelsDevPrice(md, "other", "alpha"); ok {
		t.Fatalf("modelsDevPrice for unknown provider = ok, want not ok")
	}
	if _, ok := modelsDevPrice(nil, "testai", "alpha"); ok {
		t.Fatalf("modelsDevPrice with nil catalog = ok, want not ok")
	}
}
