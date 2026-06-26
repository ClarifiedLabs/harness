package main

import (
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"harness/internal/llm"
	"harness/internal/modelsdev"
)

const (
	codexModelsURL           = "https://raw.githubusercontent.com/openai/codex/main/codex-rs/models-manager/models.json"
	codexModelsCacheFilename = "openai-codex.models.json"
)

//go:embed codex_models_fallback.json
var codexModelsFallbackJSON []byte

type codexModelsCatalog struct {
	Models []codexModel `json:"models"`
}

type codexModel struct {
	Slug                     string                 `json:"slug"`
	DisplayName              string                 `json:"display_name"`
	ContextWindow            int                    `json:"context_window"`
	MaxContextWindow         int                    `json:"max_context_window"`
	InputModalities          []string               `json:"input_modalities"`
	SupportedReasoningLevels []codexReasoningPreset `json:"supported_reasoning_levels"`
	Visibility               string                 `json:"visibility"`
	SupportedInAPI           *bool                  `json:"supported_in_api"`
}

type codexReasoningPreset struct {
	Effort string `json:"effort"`
}

func codexModelsCachePath(configDir string) string {
	return filepath.Join(configDir, codexModelsCacheFilename)
}

func setupCodexModelsCatalog(env environment, configDir string) (*codexModelsCatalog, error) {
	path := codexModelsCachePath(configDir)
	catalog, err := readCodexModelsCache(path)
	if err == nil {
		return catalog, nil
	}
	if !errors.Is(err, os.ErrNotExist) && env.stderr != nil {
		fmt.Fprintf(env.stderr, "harness-model-proxy: setup: warning: cached OpenAI Codex model catalog failed: %v; using vendored fallback\n", err)
	}
	return codexModelsFallback()
}

func refreshCodexModelsCatalog(ctx context.Context, env environment, configDir string) (*codexModelsCatalog, error) {
	catalog, err := fetchAndCacheCodexModels(ctx, env, configDir)
	if err == nil {
		return catalog, nil
	}
	if errors.Is(err, context.Canceled) {
		return nil, err
	}
	path := codexModelsCachePath(configDir)
	cached, cacheErr := readCodexModelsCache(path)
	if cacheErr == nil {
		fmt.Fprintf(env.stderr, "harness-model-proxy: refresh-models: warning: OpenAI Codex model catalog refresh failed: %v; using cached catalog\n", err)
		return cached, nil
	}
	fallback, fallbackErr := codexModelsFallback()
	if fallbackErr != nil {
		if errors.Is(cacheErr, os.ErrNotExist) {
			return nil, fmt.Errorf("OpenAI Codex model catalog refresh failed: %v; vendored fallback failed: %w", err, fallbackErr)
		}
		return nil, fmt.Errorf("cached OpenAI Codex model catalog failed: %v; refresh failed: %v; vendored fallback failed: %w", cacheErr, err, fallbackErr)
	}
	if errors.Is(cacheErr, os.ErrNotExist) {
		fmt.Fprintf(env.stderr, "harness-model-proxy: refresh-models: warning: OpenAI Codex model catalog refresh failed: %v; using vendored fallback\n", err)
	} else {
		fmt.Fprintf(env.stderr, "harness-model-proxy: refresh-models: warning: cached OpenAI Codex model catalog failed: %v; refresh failed: %v; using vendored fallback\n", cacheErr, err)
	}
	return fallback, nil
}

func readCodexModelsCache(path string) (*codexModelsCatalog, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return decodeCodexModels(data)
}

func fetchAndCacheCodexModels(ctx context.Context, env environment, configDir string) (*codexModelsCatalog, error) {
	data, err := fetchCodexModelsData(ctx, env)
	if err != nil {
		return nil, err
	}
	catalog, err := decodeCodexModels(data)
	if err != nil {
		return nil, err
	}
	if err := writeCodexModelsCache(configDir, data); err != nil {
		return nil, err
	}
	return catalog, nil
}

func fetchCodexModelsData(ctx context.Context, env environment) ([]byte, error) {
	if env.codexModelsData != nil {
		return env.codexModelsData(ctx)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, codexModelsURL, nil)
	if err != nil {
		return nil, err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("OpenAI Codex model catalog: GET %s: %s", codexModelsURL, resp.Status)
	}
	return io.ReadAll(resp.Body)
}

func writeCodexModelsCache(configDir string, data []byte) error {
	if _, err := decodeCodexModels(data); err != nil {
		return fmt.Errorf("candidate OpenAI Codex model catalog did not parse: %w", err)
	}
	if err := os.MkdirAll(configDir, 0o700); err != nil {
		return err
	}
	return writeBytesAtomic(codexModelsCachePath(configDir), data, true)
}

func codexModelsFallback() (*codexModelsCatalog, error) {
	return decodeCodexModels(codexModelsFallbackJSON)
}

func decodeCodexModels(data []byte) (*codexModelsCatalog, error) {
	var catalog codexModelsCatalog
	if err := json.NewDecoder(bytes.NewReader(data)).Decode(&catalog); err != nil {
		return nil, err
	}
	if _, ok := openAICodexProviderFromCatalog(&catalog); !ok {
		return nil, fmt.Errorf("OpenAI Codex model catalog has no list-visible models")
	}
	return &catalog, nil
}

func openAICodexProviderFromCatalog(catalog *codexModelsCatalog) (modelsdev.Provider, bool) {
	if catalog == nil {
		return modelsdev.Provider{}, false
	}
	models := make(map[string]modelsdev.Model)
	for _, model := range catalog.Models {
		entry, ok := codexModelToModelsDev(model)
		if !ok {
			continue
		}
		models[entry.ID] = entry
	}
	if len(models) == 0 {
		return modelsdev.Provider{}, false
	}
	return modelsdev.Provider{
		ID:     openAICodexProviderID,
		Name:   openAICodexProviderName,
		API:    openAICodexProviderBaseURL,
		Models: models,
	}, true
}

func codexModelToModelsDev(model codexModel) (modelsdev.Model, bool) {
	id := strings.TrimSpace(model.Slug)
	if id == "" || !codexModelVisible(model) {
		return modelsdev.Model{}, false
	}
	contextWindow := model.ContextWindow
	if contextWindow <= 0 {
		contextWindow = model.MaxContextWindow
	}
	if contextWindow <= 0 {
		return modelsdev.Model{}, false
	}
	name := strings.TrimSpace(model.DisplayName)
	if name == "" {
		name = id
	}
	reasoningValues := codexReasoningValues(model.SupportedReasoningLevels)
	entry := modelsdev.Model{
		ID:         id,
		Name:       name,
		Modalities: modelsdev.Modalities{Input: append([]string(nil), model.InputModalities...)},
		Reasoning:  len(reasoningValues) > 0,
		Limit:      modelsdev.Limit{Context: contextWindow},
	}
	if len(reasoningValues) > 0 {
		entry.ReasoningOptions = []llm.ReasoningOption{{Type: "effort", Values: reasoningValues}}
	}
	return entry, true
}

func codexModelVisible(model codexModel) bool {
	if model.SupportedInAPI != nil && !*model.SupportedInAPI {
		return false
	}
	visibility := strings.ToLower(strings.TrimSpace(model.Visibility))
	return visibility == "" || visibility == "list"
}

func codexReasoningValues(presets []codexReasoningPreset) []string {
	values := make([]string, 0, len(presets))
	for _, preset := range presets {
		effort := strings.TrimSpace(preset.Effort)
		if effort != "" {
			values = append(values, effort)
		}
	}
	return values
}
