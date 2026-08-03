package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"harness/internal/buildinfo"
	"harness/internal/llm"
	"harness/internal/modelproxy/modeldiscovery"
)

const providerModelRefreshWorkers = 4

func providerModelFetcher(env environment, configDir string) modeldiscovery.Fetcher {
	client := env.providerModelsClient
	if client == nil {
		client = http.DefaultClient
	}
	getenv := env.getenv
	if getenv == nil {
		getenv = func(string) string { return "" }
	}
	return modeldiscovery.Fetcher{
		Client: client, ConfigDir: configDir, Getenv: getenv,
		Now: func() time.Time { return currentTime(env) }, ClientVersion: buildinfo.Version,
	}
}

func directDiscoveryEnabled(pc llm.ProviderConfig) bool {
	if !pc.Managed && pc.ModelDiscovery == nil {
		return false
	}
	_, ok, err := modeldiscovery.Resolve(pc)
	return err == nil && ok
}

func loadProviderModelCaches(configDir string, providers []llm.ProviderConfig, now time.Time, ttl time.Duration, logger *slog.Logger) map[string]modeldiscovery.State {
	states := map[string]modeldiscovery.State{}
	for _, pc := range providers {
		if !directDiscoveryEnabled(pc) {
			continue
		}
		spec, _, _ := modeldiscovery.Resolve(pc)
		snapshot, err := modeldiscovery.ReadProviderCache(configDir, pc.Name, pc.BaseURL)
		if err != nil {
			continue
		}
		if snapshot.Format != spec.Format || snapshot.Endpoint != spec.Endpoint || snapshot.IncludeUnknownModels != spec.IncludeUnknownModels {
			if logger != nil {
				logger.Warn("provider model cache does not match discovery settings; ignoring it", "provider", pc.Name)
			}
			continue
		}
		states[pc.Name] = modeldiscovery.StateFromCache(snapshot, now, ttl)
	}
	return states
}

func fetchProviderModelCatalog(ctx context.Context, env environment, configDir string, pc llm.ProviderConfig, previous *modeldiscovery.Snapshot) (modeldiscovery.Snapshot, error) {
	snapshot, err := providerModelFetcher(env, configDir).Fetch(ctx, pc, previous)
	if err != nil {
		return modeldiscovery.Snapshot{}, err
	}
	if err := modeldiscovery.WriteCache(configDir, snapshot); err != nil {
		return modeldiscovery.Snapshot{}, err
	}
	return snapshot, nil
}

// refreshProviderModels fetches every supported configured provider with a
// bounded standard-library worker pool. Failures are isolated per provider and
// omitted from updates so the handler retains its prior state.
func refreshProviderModels(ctx context.Context, env environment, configDir string, providers []llm.ProviderConfig, previous map[string]modeldiscovery.State, logger *slog.Logger) map[string]modeldiscovery.State {
	type result struct {
		provider    string
		snapshot    modeldiscovery.Snapshot
		unsupported bool
		cacheErr    error
		err         error
	}
	jobs := make(chan llm.ProviderConfig)
	results := make(chan result)
	workers := min(providerModelRefreshWorkers, len(providers))
	if workers == 0 {
		return nil
	}
	var wg sync.WaitGroup
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for pc := range jobs {
				var cached *modeldiscovery.Snapshot
				if state, ok := previous[pc.Name]; ok {
					copy := state.Snapshot
					cached = &copy
				}
				snapshot, err := providerModelFetcher(env, configDir).Fetch(ctx, pc, cached)
				var cacheErr error
				if err == nil {
					cacheErr = modeldiscovery.WriteCache(configDir, snapshot)
				}
				results <- result{provider: pc.Name, snapshot: snapshot, unsupported: errors.Is(err, modeldiscovery.ErrUnsupported), cacheErr: cacheErr, err: err}
			}
		}()
	}
	go func() {
		defer close(jobs)
		for _, pc := range providers {
			if directDiscoveryEnabled(pc) {
				select {
				case jobs <- pc:
				case <-ctx.Done():
					return
				}
			}
		}
	}()
	go func() {
		wg.Wait()
		close(results)
	}()

	updates := map[string]modeldiscovery.State{}
	for result := range results {
		if result.err != nil {
			if result.unsupported {
				updates[result.provider] = modeldiscovery.State{Unsupported: true}
			}
			if logger != nil {
				switch {
				case errors.Is(result.err, modeldiscovery.ErrNoCredentials):
					logger.Debug("provider model discovery skipped; credentials unavailable", "provider", result.provider)
				case errors.Is(result.err, modeldiscovery.ErrUnsupported):
					logger.Debug("provider model discovery endpoint unsupported", "provider", result.provider)
				default:
					logger.Warn("provider model discovery refresh failed", "provider", result.provider, "err", result.err)
				}
			}
			continue
		}
		updates[result.provider] = modeldiscovery.State{Snapshot: result.snapshot, Authoritative: true}
		if logger != nil {
			if result.cacheErr != nil {
				logger.Warn("provider model cache write failed; using in-memory catalog", "provider", result.provider, "err", result.cacheErr)
			}
			logger.Info("provider model catalog refreshed", "provider", result.provider, "models", len(result.snapshot.Models))
		}
	}
	return updates
}

func startProviderModelRefresh(ctx context.Context, env environment, configDir string, providers []llm.ProviderConfig, initial map[string]modeldiscovery.State, ttl time.Duration, logger *slog.Logger, onRefresh func(map[string]modeldiscovery.State)) {
	if ttl <= 0 {
		return
	}
	go func() {
		states := make(map[string]modeldiscovery.State, len(initial))
		for provider, state := range initial {
			states[provider] = state
		}
		refresh := func() {
			updates := map[string]modeldiscovery.State{}
			for provider, state := range states {
				if state.Unsupported || state.Snapshot.FetchedAt.IsZero() {
					continue
				}
				aged := modeldiscovery.StateFromCache(state.Snapshot, currentTime(env), ttl)
				if aged.Authoritative != state.Authoritative {
					states[provider] = aged
					updates[provider] = aged
				}
			}
			fetched := refreshProviderModels(ctx, env, configDir, providers, states, logger)
			for provider, state := range fetched {
				states[provider] = state
				updates[provider] = state
			}
			if onRefresh != nil && len(updates) > 0 {
				onRefresh(updates)
			}
		}
		refresh()
		tick := env.providerModelsTicks
		var ticker *time.Ticker
		if tick == nil {
			ticker = time.NewTicker(ttl)
			tick = ticker.C
			defer ticker.Stop()
		}
		for {
			select {
			case <-tick:
				refresh()
			case <-ctx.Done():
				return
			}
		}
	}()
}
