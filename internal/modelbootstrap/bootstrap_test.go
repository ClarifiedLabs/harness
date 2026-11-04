package modelbootstrap

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"harness/internal/modelcatalog"
	"harness/internal/modelproxy/modeldiscovery"
)

func wait(t *testing.T, ch <-chan struct{}) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(5 * time.Second):
		t.Fatal("worker did not advance")
	}
}

func TestInitializeUsesCachesWithoutNetwork(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "provider.json"), []byte(`{"name":"test","api_type":"openai","base_url":"https://example.invalid","managed":true,"models":[{"name":"test"}]}`), 0600); err != nil {
		t.Fatal(err)
	}
	data := []byte(`{"test":{"id":"test","name":"Test","models":{"test":{"id":"test","name":"Test","limit":{"context":1000,"output":100}}}}}`)
	if err := WriteModelsDevCache(dir, data); err != nil {
		t.Fatal(err)
	}
	deps := Dependencies{ModelsDevCatalog: func(context.Context) (*modelcatalog.Catalog, error) {
		t.Error("initialization contacted network")
		return nil, errors.New("unexpected fetch")
	}}
	initial, err := Initialize(dir, []string{"provider.json"}, time.Hour, deps, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(initial.Providers) != 1 || initial.ModelsDevCatalog == nil || initial.ModelsDevSourceDate.IsZero() {
		t.Fatalf("initial = %+v", initial)
	}
}

func TestRefreshCloseCancelsAndJoinsAllWorkers(t *testing.T) {
	dir := t.TempDir()
	modelsStarted, providerStarted, subscriptionStarted := make(chan struct{}), make(chan struct{}), make(chan struct{})
	modelsFinished, providerFinished, subscriptionFinished := make(chan struct{}), make(chan struct{}), make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(providerStarted)
		<-r.Context().Done()
		close(providerFinished)
	}))
	defer server.Close()
	if err := os.WriteFile(filepath.Join(dir, "provider.json"), []byte(`{"name":"test","api_type":"openai","base_url":"`+server.URL+`","api_key":"secret","managed":true,"models":[{"name":"test"}]}`), 0600); err != nil {
		t.Fatal(err)
	}
	deps := Dependencies{ProviderModelsClient: server.Client(), ModelsDevCatalog: func(ctx context.Context) (*modelcatalog.Catalog, error) {
		close(modelsStarted)
		<-ctx.Done()
		close(modelsFinished)
		return nil, ctx.Err()
	}}
	initial, err := Initialize(dir, []string{"provider.json"}, time.Hour, deps, nil)
	if err != nil {
		t.Fatal(err)
	}
	refresh := StartRefresh(context.Background(), RefreshOptions{ConfigDir: dir, Initial: initial, Dependencies: deps, ModelsDevTTL: time.Hour, ProviderModelsTTL: time.Hour, SubscriptionEnabled: true, SubscriptionInterval: time.Minute,
		RefreshSubscriptionMetrics: func(ctx context.Context) { close(subscriptionStarted); <-ctx.Done(); close(subscriptionFinished) }})
	defer refresh.Close()
	wait(t, modelsStarted)
	wait(t, providerStarted)
	wait(t, subscriptionStarted)
	refresh.Close()
	wait(t, modelsFinished)
	wait(t, providerFinished)
	wait(t, subscriptionFinished)
	refresh.Close() // idempotent
}

func TestRefreshClosedTicksStopWorkers(t *testing.T) {
	ticks := make(chan time.Time)
	close(ticks)
	var fetches atomic.Int32
	deps := Dependencies{ModelsDevTicks: ticks, ProviderModelsTicks: ticks, ModelsDevCatalog: func(context.Context) (*modelcatalog.Catalog, error) {
		fetches.Add(1)
		return nil, errors.New("offline")
	}}
	done := StartModelsDevCacheRefresh(context.Background(), deps, t.TempDir(), time.Hour, nil, nil)
	wait(t, done)
	if fetches.Load() != 1 {
		t.Fatalf("fetches=%d", fetches.Load())
	}
	providerDone := StartProviderModelRefresh(context.Background(), deps, t.TempDir(), nil, map[string]modeldiscovery.State{}, time.Hour, nil, nil)
	wait(t, providerDone)
}

func TestFetchModelsDevUsesInjectedHTTPAndPreservesRawData(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"test":{"id":"test","models":{"test":{"id":"test","name":"Test"}}}}`))
	}))
	defer server.Close()
	dir := t.TempDir()
	catalog, err := RefreshModelsDevCatalog(context.Background(), Dependencies{ModelsDevClient: server.Client(), ModelsDevURL: server.URL}, dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(catalog.Providers) != 1 {
		t.Fatalf("catalog=%+v", catalog)
	}
	if _, err := os.Stat(ModelsDevCachePath(dir)); err != nil {
		t.Fatal(err)
	}
}
