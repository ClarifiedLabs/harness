package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"

	"harness/internal/modelcatalog"
)

const codexModelsCacheFilename = "openai-codex.models.json"

func codexModelsCachePath(configDir string) string {
	return filepath.Join(configDir, codexModelsCacheFilename)
}

func setupCodexProvider(env environment, configDir string) (modelcatalog.Provider, error) {
	path := codexModelsCachePath(configDir)
	provider, err := readCodexModelsCache(path)
	if err == nil {
		return provider, nil
	}
	if !errors.Is(err, os.ErrNotExist) && env.stderr != nil {
		fmt.Fprintf(env.stderr, "harness-model-proxy: setup: warning: cached OpenAI Codex model catalog failed: %v; using vendored fallback\n", err)
	}
	return modelcatalog.CodexModelsFallback()
}

func refreshCodexProvider(ctx context.Context, env environment, configDir string) (modelcatalog.Provider, error) {
	provider, err := fetchAndCacheCodexModels(ctx, env, configDir)
	if err == nil {
		return provider, nil
	}
	if errors.Is(err, context.Canceled) {
		return modelcatalog.Provider{}, err
	}
	path := codexModelsCachePath(configDir)
	cached, cacheErr := readCodexModelsCache(path)
	if cacheErr == nil {
		fmt.Fprintf(env.stderr, "harness-model-proxy: refresh-models: warning: OpenAI Codex model catalog refresh failed: %v; using cached catalog\n", err)
		return cached, nil
	}
	fallback, fallbackErr := modelcatalog.CodexModelsFallback()
	if fallbackErr != nil {
		if errors.Is(cacheErr, os.ErrNotExist) {
			return modelcatalog.Provider{}, fmt.Errorf("OpenAI Codex model catalog refresh failed: %v; vendored fallback failed: %w", err, fallbackErr)
		}
		return modelcatalog.Provider{}, fmt.Errorf("cached OpenAI Codex model catalog failed: %v; refresh failed: %v; vendored fallback failed: %w", cacheErr, err, fallbackErr)
	}
	if errors.Is(cacheErr, os.ErrNotExist) {
		fmt.Fprintf(env.stderr, "harness-model-proxy: refresh-models: warning: OpenAI Codex model catalog refresh failed: %v; using vendored fallback\n", err)
	} else {
		fmt.Fprintf(env.stderr, "harness-model-proxy: refresh-models: warning: cached OpenAI Codex model catalog failed: %v; refresh failed: %v; using vendored fallback\n", cacheErr, err)
	}
	return fallback, nil
}

func readCodexModelsCache(path string) (modelcatalog.Provider, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return modelcatalog.Provider{}, err
	}
	return modelcatalog.DecodeCodexModels(data)
}

func fetchAndCacheCodexModels(ctx context.Context, env environment, configDir string) (modelcatalog.Provider, error) {
	data, err := fetchCodexModelsData(ctx, env)
	if err != nil {
		return modelcatalog.Provider{}, err
	}
	provider, err := modelcatalog.DecodeCodexModels(data)
	if err != nil {
		return modelcatalog.Provider{}, err
	}
	if err := writeCodexModelsCache(configDir, data); err != nil {
		return modelcatalog.Provider{}, err
	}
	return provider, nil
}

func fetchCodexModelsData(ctx context.Context, env environment) ([]byte, error) {
	if env.codexModelsData != nil {
		return env.codexModelsData(ctx)
	}
	return modelcatalog.FetchCodexModelsData(ctx, http.DefaultClient, modelcatalog.CodexModelsURL())
}

func writeCodexModelsCache(configDir string, data []byte) error {
	data, err := modelcatalog.PruneCodexModelsData(data)
	if err != nil {
		return fmt.Errorf("candidate OpenAI Codex model catalog did not parse: %w", err)
	}
	if _, err := modelcatalog.DecodeCodexModels(data); err != nil {
		return fmt.Errorf("candidate OpenAI Codex model catalog did not parse: %w", err)
	}
	if err := os.MkdirAll(configDir, 0o700); err != nil {
		return err
	}
	return writeBytesAtomic(codexModelsCachePath(configDir), data, true)
}
