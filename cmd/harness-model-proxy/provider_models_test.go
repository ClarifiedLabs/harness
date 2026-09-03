package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"harness/internal/llm"
	"harness/internal/modelcatalog"
	"harness/internal/modelproxy/modeldiscovery"
)

func TestProviderModelFetcherUsesOfficialCodexClientVersion(t *testing.T) {
	t.Parallel()
	fetcher := providerModelFetcher(environment{}, t.TempDir())
	if got, want := fetcher.CodexClientVersion, modelcatalog.CodexClientVersion(); got != want {
		t.Fatalf("CodexClientVersion = %q, want %q", got, want)
	}
	if fetcher.CodexClientVersion == "dev" {
		t.Fatal("provider discovery leaked the harness development build version")
	}
}

func TestStartProviderModelRefreshRunsImmediatelyAndOnTick(t *testing.T) {
	t.Parallel()
	var requests atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		if r.Header.Get("Authorization") != "Bearer secret" {
			t.Errorf("Authorization = %q", r.Header.Get("Authorization"))
		}
		w.Header().Set("ETag", `"v1"`)
		if r.Header.Get("If-None-Match") == `"v1"` {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		_, _ = w.Write([]byte(`{"data":[{"id":"fugu"}]}`))
	}))
	defer server.Close()
	ticks := make(chan time.Time)
	env := environment{providerModelsClient: server.Client(), providerModelsTicks: ticks, now: time.Now}
	provider := llm.ProviderConfig{Name: "sakana", APIType: "openai", BaseURL: server.URL, APIKey: "secret", Managed: true}
	updates := make(chan map[string]modeldiscovery.State, 2)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	startProviderModelRefresh(ctx, env, t.TempDir(), []llm.ProviderConfig{provider}, nil, time.Hour, nil, func(state map[string]modeldiscovery.State) {
		updates <- state
	})
	first := receiveProviderUpdate(t, updates)
	if !first["sakana"].Authoritative || len(first["sakana"].Snapshot.Models) != 1 {
		t.Fatalf("first update = %+v", first)
	}
	ticks <- time.Now()
	second := receiveProviderUpdate(t, updates)
	if !second["sakana"].Authoritative || requests.Load() != 2 {
		t.Fatalf("second update = %+v requests=%d", second, requests.Load())
	}
}

func TestRefreshProviderModelsReportsUnsupportedButOmitsAuthFailure(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/unsupported/models" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer server.Close()
	providers := []llm.ProviderConfig{
		{Name: "unsupported", APIType: "openai", BaseURL: server.URL + "/unsupported", APIKey: "secret", Managed: true},
		{Name: "unauthorized", APIType: "openai", BaseURL: server.URL + "/unauthorized", APIKey: "secret", Managed: true},
	}
	updates := refreshProviderModels(context.Background(), environment{providerModelsClient: server.Client()}, t.TempDir(), providers, nil, nil)
	if state, ok := updates["unsupported"]; !ok || !state.Unsupported {
		t.Fatalf("unsupported state = %+v, present=%v", state, ok)
	}
	if _, ok := updates["unauthorized"]; ok {
		t.Fatalf("auth failure produced a destructive update: %+v", updates)
	}
}

func TestStartProviderModelRefreshDemotesExpiredSnapshotAfterFailure(t *testing.T) {
	t.Parallel()
	var requests atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if requests.Add(1) == 1 {
			_, _ = w.Write([]byte(`{"data":[{"id":"fugu"}]}`))
			return
		}
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer server.Close()

	var unixNow atomic.Int64
	unixNow.Store(1_000)
	ticks := make(chan time.Time)
	env := environment{
		providerModelsClient: server.Client(),
		providerModelsTicks:  ticks,
		now:                  func() time.Time { return time.Unix(unixNow.Load(), 0) },
	}
	provider := llm.ProviderConfig{Name: "sakana", APIType: "openai", BaseURL: server.URL, APIKey: "secret", Managed: true}
	updates := make(chan map[string]modeldiscovery.State, 2)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	startProviderModelRefresh(ctx, env, t.TempDir(), []llm.ProviderConfig{provider}, nil, time.Hour, nil, func(state map[string]modeldiscovery.State) {
		updates <- state
	})
	first := receiveProviderUpdate(t, updates)
	if !first["sakana"].Authoritative {
		t.Fatalf("first update = %+v, want authoritative", first)
	}

	unixNow.Store(1_000 + int64(2*time.Hour/time.Second))
	ticks <- time.Unix(unixNow.Load(), 0)
	second := receiveProviderUpdate(t, updates)
	if second["sakana"].Authoritative || len(second["sakana"].Snapshot.Models) != 1 {
		t.Fatalf("expired update = %+v, want metadata-only cached snapshot", second)
	}
}

func TestStartProviderModelRefreshPreservesSnapshotAcrossTransientNotFound(t *testing.T) {
	t.Parallel()
	var requests atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		switch requests.Add(1) {
		case 1:
			_, _ = w.Write([]byte(`{"data":[{"id":"fugu"}]}`))
		case 2:
			w.WriteHeader(http.StatusNotFound)
		default:
			_, _ = w.Write([]byte(`{"data":[{"id":"fugu-recovered"}]}`))
		}
	}))
	defer server.Close()

	var unixNow atomic.Int64
	unixNow.Store(1_000)
	ticks := make(chan time.Time)
	env := environment{
		providerModelsClient: server.Client(),
		providerModelsTicks:  ticks,
		now:                  func() time.Time { return time.Unix(unixNow.Load(), 0) },
	}
	provider := llm.ProviderConfig{Name: "sakana", APIType: "openai", BaseURL: server.URL, APIKey: "secret", Managed: true}
	updates := make(chan map[string]modeldiscovery.State, 3)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	startProviderModelRefresh(ctx, env, t.TempDir(), []llm.ProviderConfig{provider}, nil, time.Hour, nil, func(state map[string]modeldiscovery.State) {
		updates <- state
	})

	first := receiveProviderUpdate(t, updates)["sakana"]
	if !first.Authoritative || first.Unsupported || len(first.Snapshot.Models) != 1 {
		t.Fatalf("first update = %+v, want authoritative snapshot", first)
	}

	unixNow.Store(1_000 + int64(2*time.Hour/time.Second))
	ticks <- time.Unix(unixNow.Load(), 0)
	second := receiveProviderUpdate(t, updates)["sakana"]
	if second.Authoritative || second.Unsupported || len(second.Snapshot.Models) != 1 {
		t.Fatalf("not-found update = %+v, want metadata-only cached snapshot", second)
	}
	if _, ok := second.Snapshot.Models["fugu"]; !ok {
		t.Fatalf("not-found update lost previous model: %+v", second.Snapshot.Models)
	}

	ticks <- time.Unix(unixNow.Load(), 0)
	third := receiveProviderUpdate(t, updates)["sakana"]
	if !third.Authoritative || third.Unsupported || len(third.Snapshot.Models) != 1 {
		t.Fatalf("recovered update = %+v, want authoritative snapshot", third)
	}
	if _, ok := third.Snapshot.Models["fugu-recovered"]; !ok {
		t.Fatalf("recovered update models = %+v", third.Snapshot.Models)
	}
	if got := requests.Load(); got != 3 {
		t.Fatalf("provider requests = %d, want 3", got)
	}
}

func receiveProviderUpdate(t *testing.T, updates <-chan map[string]modeldiscovery.State) map[string]modeldiscovery.State {
	t.Helper()
	select {
	case update := <-updates:
		return update
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for provider model refresh")
		return nil
	}
}
