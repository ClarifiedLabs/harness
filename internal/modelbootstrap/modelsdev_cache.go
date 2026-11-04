package modelbootstrap

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"harness/internal/modelcatalog"
)

const (
	modelsDevCacheFilename              = "models.dev.api.json"
	modelsDevCacheBackupFilename        = "models.dev.api.json.bak"
	DefaultModelsDevTTL                 = 24 * time.Hour
	modelsDevCacheMaxGrowthFactor       = 4
	modelsDevCacheMinProviderCountDelta = 5
	modelsDevCacheMinModelCountDelta    = 50
)

func ModelsDevCachePath(configDir string) string {
	return filepath.Join(configDir, modelsDevCacheFilename)
}

func ModelsDevCacheBackupPath(configDir string) string {
	return filepath.Join(configDir, modelsDevCacheBackupFilename)
}

type cachedModelsDevCatalog struct {
	catalog *modelcatalog.Catalog
	modTime time.Time
}

func readModelsDevCache(path string) (cachedModelsDevCatalog, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return cachedModelsDevCatalog{}, err
	}
	catalog, err := modelcatalog.DecodeModelsDev(bytes.NewReader(data))
	if err != nil {
		return cachedModelsDevCatalog{}, err
	}
	info, err := os.Stat(path)
	if err != nil {
		return cachedModelsDevCatalog{}, err
	}
	return cachedModelsDevCatalog{catalog: catalog, modTime: info.ModTime()}, nil
}

func fetchAndCacheModelsDev(ctx context.Context, env Dependencies, configDir string) (*modelcatalog.Catalog, error) {
	catalog, data, err := fetchModelsDevCatalogData(ctx, env)
	if err != nil {
		return nil, err
	}
	if err := WriteModelsDevCache(configDir, data); err != nil {
		return nil, err
	}
	return catalog, nil
}

func fetchModelsDevCatalogData(ctx context.Context, env Dependencies) (*modelcatalog.Catalog, []byte, error) {
	if env.ModelsDevCatalog != nil {
		catalog, err := env.ModelsDevCatalog(ctx)
		if err != nil {
			return nil, nil, err
		}
		data, err := modelcatalog.EncodeModelsDev(catalog)
		if err != nil {
			return nil, nil, err
		}
		data = append(data, '\n')
		return catalog, data, nil
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	data, err := modelcatalog.FetchModelsDevData(ctx, httpClient(env.ModelsDevClient), modelsDevURL(env))
	if err != nil {
		return nil, nil, err
	}
	catalog, err := modelcatalog.DecodeModelsDev(bytes.NewReader(data))
	if err != nil {
		return nil, nil, err
	}
	return catalog, data, nil
}

func WriteModelsDevCache(configDir string, data []byte) error {
	data, err := modelcatalog.PruneModelsDevData(data)
	if err != nil {
		return fmt.Errorf("candidate models.dev cache did not parse: %w", err)
	}
	catalog, err := modelcatalog.DecodeModelsDev(bytes.NewReader(data))
	if err != nil {
		return fmt.Errorf("candidate models.dev cache did not parse: %w", err)
	}
	if err := os.MkdirAll(configDir, 0o700); err != nil {
		return err
	}
	path := ModelsDevCachePath(configDir)
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
	backupPath := ModelsDevCacheBackupPath(filepath.Dir(path))
	if err := WriteBytesAtomic(backupPath, data, false); err != nil {
		return fmt.Errorf("write previous models.dev cache backup: %w", err)
	}
	return nil
}

func writeModelsDevCacheFile(path string, data []byte) error {
	return WriteBytesAtomic(path, data, true)
}

func WriteBytesAtomic(path string, data []byte, ensureNewline bool) error {
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

func validateModelsDevCacheUpdate(path string, candidate *modelcatalog.Catalog) error {
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

func modelsDevCatalogStats(catalog *modelcatalog.Catalog) modelsDevCacheStats {
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

func CachedOrFetchedSetupCatalog(ctx context.Context, env Dependencies, configDir string, ttl time.Duration) (*modelcatalog.Catalog, error) {
	path := ModelsDevCachePath(configDir)
	cached, cacheErr := readModelsDevCache(path)
	now := CurrentTime(env)
	if cacheErr == nil {
		if ttl > 0 && now.Sub(cached.modTime) > ttl {
			catalog, err := fetchAndCacheModelsDev(ctx, env, configDir)
			if err == nil {
				return catalog, nil
			}
			if errors.Is(err, context.Canceled) {
				return nil, err
			}
			warn(env, fmt.Sprintf("models.dev cache refresh failed: %v; using cached catalog", err))
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
	fallback, fallbackErr := modelcatalog.ModelsDevFallback()
	if fallbackErr != nil {
		if os.IsNotExist(cacheErr) {
			return nil, fmt.Errorf("models.dev cache refresh failed: %v; vendored fallback failed: %w", err, fallbackErr)
		}
		return nil, fmt.Errorf("cached models.dev catalog failed: %v; models.dev cache refresh failed: %v; vendored fallback failed: %w", cacheErr, err, fallbackErr)
	}
	if os.IsNotExist(cacheErr) {
		warn(env, fmt.Sprintf("models.dev cache refresh failed: %v; using vendored fallback", err))
	} else {
		warn(env, fmt.Sprintf("cached models.dev catalog failed: %v; models.dev cache refresh failed: %v; using vendored fallback", cacheErr, err))
	}
	return fallback, nil
}

func RefreshModelsDevCatalog(ctx context.Context, env Dependencies, configDir string) (*modelcatalog.Catalog, error) {
	catalog, err := fetchAndCacheModelsDev(ctx, env, configDir)
	if err == nil {
		return catalog, nil
	}
	if errors.Is(err, context.Canceled) {
		return nil, err
	}
	path := ModelsDevCachePath(configDir)
	cached, cacheErr := readModelsDevCache(path)
	if cacheErr == nil {
		warn(env, fmt.Sprintf("models.dev cache refresh failed: %v; using cached catalog", err))
		return cached.catalog, nil
	}
	fallback, fallbackErr := modelcatalog.ModelsDevFallback()
	if fallbackErr != nil {
		if os.IsNotExist(cacheErr) {
			return nil, fmt.Errorf("models.dev cache refresh failed: %v; vendored fallback failed: %w", err, fallbackErr)
		}
		return nil, fmt.Errorf("cached models.dev catalog failed: %v; models.dev cache refresh failed: %v; vendored fallback failed: %w", cacheErr, err, fallbackErr)
	}
	if os.IsNotExist(cacheErr) {
		warn(env, fmt.Sprintf("models.dev cache refresh failed: %v; using vendored fallback", err))
	} else {
		warn(env, fmt.Sprintf("cached models.dev catalog failed: %v; models.dev cache refresh failed: %v; using vendored fallback", cacheErr, err))
	}
	return fallback, nil
}

// RefreshModelsDevCacheIfStale refreshes the on-disk models.dev cache when it
// is older than ttl. On a successful refresh it returns the new catalog and its
// source date (the rewritten cache file's mtime) so the caller can push fresh
// managed prices into the running server; the bool reports whether a refresh
// happened.
func RefreshModelsDevCacheIfStale(ctx context.Context, env Dependencies, configDir string, ttl time.Duration, logger *slog.Logger) (*modelcatalog.Catalog, time.Time, bool) {
	if ttl <= 0 {
		return nil, time.Time{}, false
	}
	path := ModelsDevCachePath(configDir)
	cached, err := readModelsDevCache(path)
	now := CurrentTime(env)
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

// StartModelsDevCacheRefresh runs the background models.dev cache refresher.
// onRefresh, when non-nil, is invoked with the new catalog and its source date
// after each successful refresh so the serving handler can swap in fresh managed
// prices.
func StartModelsDevCacheRefresh(ctx context.Context, env Dependencies, configDir string, ttl time.Duration, logger *slog.Logger, onRefresh func(*modelcatalog.Catalog, time.Time)) <-chan struct{} {
	done := make(chan struct{})
	if ttl <= 0 || ctx.Err() != nil {
		close(done)
		return done
	}
	refresh := func() {
		catalog, sourceDate, ok := RefreshModelsDevCacheIfStale(ctx, env, configDir, ttl, logger)
		if ok && onRefresh != nil {
			onRefresh(catalog, sourceDate)
		}
	}
	go func() {
		defer close(done)
		refresh()
		ticks := env.ModelsDevTicks
		if ticks == nil {
			ticker := time.NewTicker(ttl)
			defer ticker.Stop()
			ticks = ticker.C
		}
		for {
			select {
			case _, ok := <-ticks:
				if !ok {
					return
				}
				refresh()
			case <-ctx.Done():
				return
			}
		}
	}()
	return done
}

// LoadModelsDevCacheForServe reads the cached models.dev catalog for the serving
// handler's initial managed-price snapshot. A missing or unreadable cache yields
// a nil catalog: managed prices stay unresolved until the first refresh writes
// the cache.
func LoadModelsDevCacheForServe(configDir string) (*modelcatalog.Catalog, time.Time) {
	cached, err := readModelsDevCache(ModelsDevCachePath(configDir))
	if err != nil {
		return nil, time.Time{}
	}
	return cached.catalog, cached.modTime
}

func CurrentTime(env Dependencies) time.Time {
	if env.Now != nil {
		return env.Now()
	}
	return time.Now()
}
