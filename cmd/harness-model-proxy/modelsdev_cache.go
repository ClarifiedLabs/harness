package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"harness/internal/modelsdev"
)

const (
	modelsDevCacheFilename              = "models.dev.api.json"
	modelsDevCacheBackupFilename        = "models.dev.api.json.bak"
	defaultModelsDevTTL                 = 24 * time.Hour
	modelsDevCacheMaxGrowthFactor       = 4
	modelsDevCacheMinProviderCountDelta = 5
	modelsDevCacheMinModelCountDelta    = 50
)

func modelsDevCachePath(configDir string) string {
	return filepath.Join(configDir, modelsDevCacheFilename)
}

func modelsDevCacheBackupPath(configDir string) string {
	return filepath.Join(configDir, modelsDevCacheBackupFilename)
}

type cachedModelsDevCatalog struct {
	catalog *modelsdev.Catalog
	modTime time.Time
}

func readModelsDevCache(path string) (cachedModelsDevCatalog, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return cachedModelsDevCatalog{}, err
	}
	catalog, err := modelsdev.Decode(bytes.NewReader(data))
	if err != nil {
		return cachedModelsDevCatalog{}, err
	}
	info, err := os.Stat(path)
	if err != nil {
		return cachedModelsDevCatalog{}, err
	}
	return cachedModelsDevCatalog{catalog: catalog, modTime: info.ModTime()}, nil
}

func fetchAndCacheModelsDev(ctx context.Context, env environment, configDir string) (*modelsdev.Catalog, error) {
	catalog, data, err := fetchModelsDevCatalogData(ctx, env)
	if err != nil {
		return nil, err
	}
	if err := writeModelsDevCache(configDir, data); err != nil {
		return nil, err
	}
	return catalog, nil
}

func fetchModelsDevCatalogData(ctx context.Context, env environment) (*modelsdev.Catalog, []byte, error) {
	if env.modelsDevCatalog != nil {
		catalog, err := env.modelsDevCatalog(ctx)
		if err != nil {
			return nil, nil, err
		}
		data, err := modelsdev.Encode(catalog)
		if err != nil {
			return nil, nil, err
		}
		data = append(data, '\n')
		return catalog, data, nil
	}
	data, err := modelsdev.FetchData(ctx, http.DefaultClient, modelsdev.DefaultURL)
	if err != nil {
		return nil, nil, err
	}
	catalog, err := modelsdev.Decode(bytes.NewReader(data))
	if err != nil {
		return nil, nil, err
	}
	return catalog, data, nil
}

func writeModelsDevCache(configDir string, data []byte) error {
	catalog, err := modelsdev.Decode(bytes.NewReader(data))
	if err != nil {
		return fmt.Errorf("candidate models.dev cache did not parse: %w", err)
	}
	if err := os.MkdirAll(configDir, 0o700); err != nil {
		return err
	}
	path := modelsDevCachePath(configDir)
	if err := validateModelsDevCacheUpdate(path, catalog); err != nil {
		return err
	}
	if err := backupExistingModelsDevCache(path); err != nil {
		return err
	}
	return writeModelsDevCacheFile(path, data)
}

func backupExistingModelsDevCache(path string) error {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read previous models.dev cache for backup: %w", err)
	}
	backupPath := filepath.Join(filepath.Dir(path), modelsDevCacheBackupFilename)
	if err := writeBytesAtomic(backupPath, data, false); err != nil {
		return fmt.Errorf("write previous models.dev cache backup: %w", err)
	}
	return nil
}

func writeModelsDevCacheFile(path string, data []byte) error {
	return writeBytesAtomic(path, data, true)
}

func writeBytesAtomic(path string, data []byte, ensureNewline bool) error {
	dir := filepath.Dir(path)
	base := filepath.Base(path)
	tmp, err := os.CreateTemp(dir, "."+base+".*.tmp")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)

	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if ensureNewline && (len(data) == 0 || data[len(data)-1] != '\n') {
		if _, err := tmp.Write([]byte("\n")); err != nil {
			_ = tmp.Close()
			return err
		}
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpPath, path)
}

type modelsDevCacheStats struct {
	providers int
	models    int
}

func validateModelsDevCacheUpdate(path string, candidate *modelsdev.Catalog) error {
	next := modelsDevCatalogStats(candidate)
	if next.providers == 0 || next.models == 0 {
		return fmt.Errorf("candidate models.dev cache is empty: providers=%d models=%d", next.providers, next.models)
	}
	cached, err := readModelsDevCache(path)
	if err != nil {
		return nil
	}
	current := modelsDevCatalogStats(cached.catalog)
	if err := validateModelsDevCacheCount("provider", current.providers, next.providers, modelsDevCacheMinProviderCountDelta); err != nil {
		return err
	}
	if err := validateModelsDevCacheCount("model", current.models, next.models, modelsDevCacheMinModelCountDelta); err != nil {
		return err
	}
	return nil
}

func modelsDevCatalogStats(catalog *modelsdev.Catalog) modelsDevCacheStats {
	var stats modelsDevCacheStats
	if catalog == nil {
		return stats
	}
	stats.providers = len(catalog.Providers)
	for _, provider := range catalog.Providers {
		stats.models += len(provider.Models)
	}
	return stats
}

func validateModelsDevCacheCount(label string, current, next, minDelta int) error {
	if current <= 0 {
		return nil
	}
	if absInt(next-current) < minDelta {
		return nil
	}
	if next*modelsDevCacheMaxGrowthFactor < current {
		return fmt.Errorf("candidate models.dev cache %s count changed too much: current=%d candidate=%d", label, current, next)
	}
	if next > current*modelsDevCacheMaxGrowthFactor {
		return fmt.Errorf("candidate models.dev cache %s count changed too much: current=%d candidate=%d", label, current, next)
	}
	return nil
}

func absInt(v int) int {
	if v < 0 {
		return -v
	}
	return v
}

func cachedOrFetchedSetupCatalog(ctx context.Context, env environment, configDir string, ttl time.Duration) (*modelsdev.Catalog, error) {
	path := modelsDevCachePath(configDir)
	cached, cacheErr := readModelsDevCache(path)
	now := currentTime(env)
	if cacheErr == nil {
		if ttl > 0 && now.Sub(cached.modTime) > ttl {
			catalog, err := fetchAndCacheModelsDev(ctx, env, configDir)
			if err == nil {
				return catalog, nil
			}
			if errors.Is(err, context.Canceled) {
				return nil, err
			}
			fmt.Fprintf(env.stderr, "harness-model-proxy: setup: warning: models.dev cache refresh failed: %v; using cached catalog\n", err)
		}
		return cached.catalog, nil
	}
	catalog, err := fetchAndCacheModelsDev(ctx, env, configDir)
	if err == nil {
		return catalog, nil
	}
	if errors.Is(err, context.Canceled) {
		return nil, err
	}
	fallback, fallbackErr := modelsdev.Fallback()
	if fallbackErr != nil {
		if os.IsNotExist(cacheErr) {
			return nil, fmt.Errorf("models.dev cache refresh failed: %v; vendored fallback failed: %w", err, fallbackErr)
		}
		return nil, fmt.Errorf("cached models.dev catalog failed: %v; models.dev cache refresh failed: %v; vendored fallback failed: %w", cacheErr, err, fallbackErr)
	}
	if os.IsNotExist(cacheErr) {
		fmt.Fprintf(env.stderr, "harness-model-proxy: setup: warning: models.dev cache refresh failed: %v; using vendored fallback\n", err)
	} else {
		fmt.Fprintf(env.stderr, "harness-model-proxy: setup: warning: cached models.dev catalog failed: %v; models.dev cache refresh failed: %v; using vendored fallback\n", cacheErr, err)
	}
	return fallback, nil
}

func refreshModelsDevCatalog(ctx context.Context, env environment, configDir string, command string) (*modelsdev.Catalog, error) {
	catalog, err := fetchAndCacheModelsDev(ctx, env, configDir)
	if err == nil {
		return catalog, nil
	}
	if errors.Is(err, context.Canceled) {
		return nil, err
	}
	path := modelsDevCachePath(configDir)
	cached, cacheErr := readModelsDevCache(path)
	if cacheErr == nil {
		fmt.Fprintf(env.stderr, "harness-model-proxy: %s: warning: models.dev cache refresh failed: %v; using cached catalog\n", command, err)
		return cached.catalog, nil
	}
	fallback, fallbackErr := modelsdev.Fallback()
	if fallbackErr != nil {
		if os.IsNotExist(cacheErr) {
			return nil, fmt.Errorf("models.dev cache refresh failed: %v; vendored fallback failed: %w", err, fallbackErr)
		}
		return nil, fmt.Errorf("cached models.dev catalog failed: %v; models.dev cache refresh failed: %v; vendored fallback failed: %w", cacheErr, err, fallbackErr)
	}
	if os.IsNotExist(cacheErr) {
		fmt.Fprintf(env.stderr, "harness-model-proxy: %s: warning: models.dev cache refresh failed: %v; using vendored fallback\n", command, err)
	} else {
		fmt.Fprintf(env.stderr, "harness-model-proxy: %s: warning: cached models.dev catalog failed: %v; models.dev cache refresh failed: %v; using vendored fallback\n", command, cacheErr, err)
	}
	return fallback, nil
}

// refreshModelsDevCacheIfStale refreshes the on-disk models.dev cache when it
// is older than ttl. On a successful refresh it returns the new catalog and its
// source date (the rewritten cache file's mtime) so the caller can push fresh
// managed prices into the running server; the bool reports whether a refresh
// happened.
func refreshModelsDevCacheIfStale(ctx context.Context, env environment, configDir string, ttl time.Duration, logger *slog.Logger) (*modelsdev.Catalog, time.Time, bool) {
	if ttl <= 0 {
		return nil, time.Time{}, false
	}
	path := modelsDevCachePath(configDir)
	cached, err := readModelsDevCache(path)
	now := currentTime(env)
	if err == nil && now.Sub(cached.modTime) <= ttl {
		return nil, time.Time{}, false
	}
	catalog, fetchErr := fetchAndCacheModelsDev(ctx, env, configDir)
	if fetchErr != nil {
		if logger != nil {
			logger.Warn("models.dev cache refresh failed", "err", fetchErr)
		}
		return nil, time.Time{}, false
	}
	sourceDate := now
	if refreshed, err := readModelsDevCache(path); err == nil {
		sourceDate = refreshed.modTime
	}
	if logger != nil {
		count := 0
		if catalog != nil {
			count = len(catalog.Providers)
		}
		logger.Info("models.dev cache refreshed", "providers", count, "path", path)
	}
	return catalog, sourceDate, true
}

// startModelsDevCacheRefresh runs the background models.dev cache refresher.
// onRefresh, when non-nil, is invoked with the new catalog and its source date
// after each successful refresh so the serving handler can swap in fresh managed
// prices.
func startModelsDevCacheRefresh(ctx context.Context, env environment, configDir string, ttl time.Duration, logger *slog.Logger, onRefresh func(*modelsdev.Catalog, time.Time)) {
	if ttl <= 0 {
		return
	}
	refresh := func() {
		catalog, sourceDate, ok := refreshModelsDevCacheIfStale(ctx, env, configDir, ttl, logger)
		if ok && onRefresh != nil {
			onRefresh(catalog, sourceDate)
		}
	}
	go func() {
		refresh()
		ticker := time.NewTicker(ttl)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				refresh()
			case <-ctx.Done():
				return
			}
		}
	}()
}

// loadModelsDevCacheForServe reads the cached models.dev catalog for the serving
// handler's initial managed-price snapshot. A missing or unreadable cache yields
// a nil catalog: managed prices stay unresolved until the first refresh writes
// the cache.
func loadModelsDevCacheForServe(configDir string) (*modelsdev.Catalog, time.Time) {
	cached, err := readModelsDevCache(modelsDevCachePath(configDir))
	if err != nil {
		return nil, time.Time{}
	}
	return cached.catalog, cached.modTime
}

func currentTime(env environment) time.Time {
	if env.now != nil {
		return env.now()
	}
	return time.Now()
}
