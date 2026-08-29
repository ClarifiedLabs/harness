package main

import (
	"errors"
	"fmt"
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

func readCodexModelsCache(path string) (modelcatalog.Provider, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return modelcatalog.Provider{}, err
	}
	return modelcatalog.DecodeCodexModels(data)
}
