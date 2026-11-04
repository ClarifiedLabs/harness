package main

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"harness/internal/modelbootstrap"
	"harness/internal/modelcatalog"
)

const defaultModelsDevTTL = modelbootstrap.DefaultModelsDevTTL

func bootstrapDependencies(env environment) modelbootstrap.Dependencies {
	return modelbootstrap.Dependencies{
		Getenv: env.getenv, Now: env.now, ModelsDevCatalog: env.modelsDevCatalog,
		ProviderModelsClient: env.providerModelsClient, ProviderModelsTimeout: env.providerModelsTimeout,
		ProviderModelsTicks: env.providerModelsTicks, SubscriptionAfter: env.subscriptionAfter,
	}
}

func commandBootstrapDependencies(env environment, command string) modelbootstrap.Dependencies {
	deps := bootstrapDependencies(env)
	deps.Warn = func(message string) {
		fmt.Fprintf(env.stderr, "harness-model-proxy: %s: warning: %s\n", command, message)
	}
	return deps
}

func modelsDevCachePath(dir string) string       { return modelbootstrap.ModelsDevCachePath(dir) }
func modelsDevCacheBackupPath(dir string) string { return modelbootstrap.ModelsDevCacheBackupPath(dir) }
func writeModelsDevCache(dir string, data []byte) error {
	return modelbootstrap.WriteModelsDevCache(dir, data)
}
func writeBytesAtomic(path string, data []byte, newline bool) error {
	return modelbootstrap.WriteBytesAtomic(path, data, newline)
}
func cachedOrFetchedSetupCatalog(ctx context.Context, env environment, dir string, ttl time.Duration) (*modelcatalog.Catalog, error) {
	return modelbootstrap.CachedOrFetchedSetupCatalog(ctx, commandBootstrapDependencies(env, "setup"), dir, ttl)
}
func refreshModelsDevCatalog(ctx context.Context, env environment, dir, command string) (*modelcatalog.Catalog, error) {
	return modelbootstrap.RefreshModelsDevCatalog(ctx, commandBootstrapDependencies(env, command), dir)
}
func refreshModelsDevCacheIfStale(ctx context.Context, env environment, dir string, ttl time.Duration, logger *slog.Logger) (*modelcatalog.Catalog, time.Time, bool) {
	return modelbootstrap.RefreshModelsDevCacheIfStale(ctx, bootstrapDependencies(env), dir, ttl, logger)
}
func loadModelsDevCacheForServe(dir string) (*modelcatalog.Catalog, time.Time) {
	return modelbootstrap.LoadModelsDevCacheForServe(dir)
}
func currentTime(env environment) time.Time {
	return modelbootstrap.CurrentTime(bootstrapDependencies(env))
}

type modelsDevCacheStats struct{ providers, models int }

func modelsDevCatalogStats(catalog *modelcatalog.Catalog) modelsDevCacheStats {
	var stats modelsDevCacheStats
	if catalog != nil {
		stats.providers = len(catalog.Providers)
		for _, provider := range catalog.Providers {
			stats.models += len(provider.Models)
		}
	}
	return stats
}
