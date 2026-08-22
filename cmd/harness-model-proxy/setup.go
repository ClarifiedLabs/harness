// Setup wizard and provider-config refresh for harness-model-proxy: the
// `harness-model-proxy setup` interactive flow (provider/models.dev-backed
// provider/model pickers) and the `refresh-models` re-sync of provider config
// files. Split from main.go so the entrypoint stays focused on serving HTTP.
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"time"

	"harness/internal/auth"
	"harness/internal/llm"
	"harness/internal/logging"
	"harness/internal/modelcatalog"
	"harness/internal/modelproxy/modeldiscovery"
	"harness/internal/ui"
)

// setupSaveEmptySelectionPrompt is printed when the user saves the model
// selector without enabling any model.
const setupSaveEmptySelectionPrompt = "Select at least one model before continuing."

type setupMainConfig struct {
	ProviderConfigs        []string `json:"provider_configs"`
	DefaultContextWindow   int      `json:"default_context_window"`
	ProviderModelsCacheTTL string   `json:"provider_models_cache_ttl,omitempty"`
	LogLevel               string   `json:"log_level,omitempty"`
	LogFormat              string   `json:"log_format,omitempty"`
}

type setupProviderConfig struct {
	Name    string `json:"name"`
	APIType string `json:"api_type"`
	BaseURL string `json:"base_url"`
	APIKey  string `json:"api_key,omitempty"`
	// Managed is always true for configs written by setup/refresh. For priced
	// providers, static pricing schedules are resolved live from the models.dev
	// cache or a provider-specific pricer, so the per-model entries below carry no
	// price.
	Managed bool `json:"managed,omitempty"`
	// PriceSource names the models.dev provider id whose prices apply to this
	// managed provider when it differs from Name. Empty means price from Name.
	PriceSource string `json:"price_source,omitempty"`
	// OmitMaxOutputTokens suppresses Responses max_output_tokens for compatible
	// backends that reject the standard parameter, such as ChatGPT Codex.
	OmitMaxOutputTokens bool `json:"omit_max_output_tokens,omitempty"`
	// ReasoningReplay controls how much historical reasoning state the dialect
	// replays on the wire; see llm.ReasoningReplay.
	ReasoningReplay llm.ReasoningReplay `json:"reasoning_replay,omitempty"`
	// UsageInputIncludesCache marks Anthropic-compatible endpoints that report
	// input_tokens including cached tokens; see llm.ProviderConfig.
	UsageInputIncludesCache bool                      `json:"usage_input_includes_cache,omitempty"`
	MinOutputTokens         int                       `json:"min_output_tokens,omitempty"`
	PromptCache             llm.PromptCacheConfig     `json:"prompt_cache,omitempty"`
	ResponsesStateful       *bool                     `json:"responses_stateful,omitempty"`
	ResponsesWebSocket      *bool                     `json:"responses_websocket,omitempty"`
	ResponsesCompaction     *bool                     `json:"responses_compaction,omitempty"`
	InteractionsStateful    *bool                     `json:"interactions_stateful,omitempty"`
	ServerTools             []string                  `json:"server_tools,omitempty"`
	ServiceTiers            []llm.ServiceTier         `json:"service_tiers,omitempty"`
	APIKeyEnv               []string                  `json:"api_key_env,omitempty"`
	Auth                    *auth.Config              `json:"auth,omitempty"`
	ModelDiscovery          *llm.ModelDiscoveryConfig `json:"model_discovery,omitempty"`
	Models                  []setupModelConfig        `json:"models"`
}

type setupModelConfig struct {
	Name                      string                `json:"name"`
	ContextWindow             int                   `json:"context_window,omitempty"`
	OutputLimit               int                   `json:"output_limit,omitempty"`
	InputModalities           []string              `json:"input_modalities,omitempty"`
	ServerTools               []string              `json:"server_tools,omitempty"`
	ServiceTiers              []llm.ServiceTier     `json:"service_tiers,omitempty"`
	Price                     *llm.Price            `json:"price,omitempty"`
	Reasoning                 *bool                 `json:"reasoning,omitempty"`
	ReasoningSummarySupported *bool                 `json:"reasoning_summary_supported,omitempty"`
	ReasoningOptions          []llm.ReasoningOption `json:"reasoning_options,omitempty"`
	ReasoningReplayDomain     string                `json:"reasoning_replay_domain,omitempty"`
}

func runSetup(ctx context.Context, env environment, force bool) error {
	dir := defaultConfigDir(env.getenv)
	configPath := filepath.Join(dir, "config.json")
	configExists, err := pathExists(configPath)
	if err != nil {
		return err
	}

	reader := bufio.NewReader(env.stdin)
	catalog, err := setupCatalog(ctx, env)
	if err != nil {
		return err
	}
	codexProvider, err := setupCodexProvider(env, dir)
	if err != nil {
		return err
	}
	existingProviders, err := loadSetupExistingProviders(configPath, configExists)
	if err != nil {
		return err
	}

	providerMeta, err := promptProviderSelection(reader, env.stdout, catalog, &codexProvider, existingProviders, setupPageSize(env))
	if err != nil {
		return err
	}
	providerName := providerMeta.ID
	providerFile := providerConfigFilename(providerName)
	existingProvider, updatingProvider := existingProviders[providerName]
	if updatingProvider {
		providerFile = existingProvider.File
	}
	providerPath := providerFile
	if !filepath.IsAbs(providerPath) {
		providerPath = filepath.Join(dir, providerFile)
	}
	providerExists, err := pathExists(providerPath)
	if err != nil {
		return err
	}
	if providerExists && !force && !updatingProvider {
		return fmt.Errorf("%s already exists", providerPath)
	}
	if setupProviderAPIType(providerMeta) == "" || setupProviderBaseURL(providerMeta) == "" {
		return fmt.Errorf("provider %q is not supported by harness", providerName)
	}
	authCfg := setupProviderAuth(providerMeta, existingProvider.Config.Auth)
	apiKey := ""
	if authCfg == nil {
		apiKeyLabel := "API key (optional)"
		if len(providerMeta.Env) > 0 {
			apiKeyLabel = fmt.Sprintf("API key (optional; env %s also works)", strings.Join(providerMeta.Env, "/"))
		}
		apiKey, err = promptLine(reader, env.stdout, apiKeyLabel+": ")
		if err != nil {
			return err
		}
		if updatingProvider && apiKey == "" {
			apiKey = existingProvider.Config.APIKey
		}
	}
	discoveryConfig := providerMeta.ProviderConfig(apiKey)
	discoveryConfig.Name = providerMeta.ID
	discoveryConfig.APIType = setupProviderAPIType(providerMeta)
	discoveryConfig.BaseURL = setupProviderBaseURL(providerMeta)
	discoveryConfig.Auth = authCfg
	discoveryConfig.Managed = true
	discoveryConfig.ModelDiscovery = existingProvider.Config.ModelDiscovery
	var previous *modeldiscovery.Snapshot
	if cached, cacheErr := modeldiscovery.ReadProviderCache(dir, discoveryConfig.Name, discoveryConfig.BaseURL); cacheErr == nil {
		previous = &cached
	}
	setupBaseline := providerMeta
	if updatingProvider {
		setupBaseline = modeldiscovery.OverlayProvider(modeldiscovery.ProviderFromConfig(existingProvider.Config), providerMeta)
	}
	if snapshot, discoveryErr := fetchProviderModelCatalog(ctx, env, dir, discoveryConfig, previous); discoveryErr == nil {
		providerMeta = modeldiscovery.MergeProvider(setupBaseline, snapshot)
	} else if !errors.Is(discoveryErr, modeldiscovery.ErrNoCredentials) && !errors.Is(discoveryErr, modeldiscovery.ErrUnsupported) {
		fmt.Fprintf(env.stderr, "harness-model-proxy: setup: warning: provider model discovery failed for %q: %v; using catalog metadata\n", providerMeta.ID, discoveryErr)
	}
	models, err := promptModelSelection(reader, env.stdout, providerMeta, setupConfiguredModelSet(existingProvider.Config.Models), setupPageSize(env))
	if err != nil {
		return err
	}

	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}

	provider := setupProviderFromCatalog(providerMeta, apiKey, authCfg, models)
	provider.ModelDiscovery = existingProvider.Config.ModelDiscovery
	preserveReasoningReplayDomains(existingProvider.Config.Models, provider.Models)
	if existingProvider.Config.OmitMaxOutputTokens {
		provider.OmitMaxOutputTokens = true
	}
	if existingProvider.Config.ReasoningReplay != "" {
		provider.ReasoningReplay = existingProvider.Config.ReasoningReplay
	}
	if existingProvider.Config.UsageInputIncludesCache {
		provider.UsageInputIncludesCache = true
	}
	if existingProvider.Config.MinOutputTokens > 0 {
		provider.MinOutputTokens = existingProvider.Config.MinOutputTokens
	}
	if existingProvider.Config.ResponsesStateful != nil {
		provider.ResponsesStateful = existingProvider.Config.ResponsesStateful
	}
	if existingProvider.Config.ResponsesWebSocket != nil {
		provider.ResponsesWebSocket = existingProvider.Config.ResponsesWebSocket
	}
	if existingProvider.Config.ResponsesCompaction != nil {
		provider.ResponsesCompaction = existingProvider.Config.ResponsesCompaction
	}
	if existingProvider.Config.InteractionsStateful != nil {
		provider.InteractionsStateful = existingProvider.Config.InteractionsStateful
	}
	if len(existingProvider.Config.ServiceTiers) > 0 {
		provider.ServiceTiers = llm.NormalizeServiceTiers(existingProvider.Config.ServiceTiers)
	}
	applySyntheticProviderDefaults(providerMeta, &provider)

	providerModelsTTL := time.Hour
	if env.providerModelsCacheTTL != nil {
		providerModelsTTL = *env.providerModelsCacheTTL
	}
	mainConfig := setupMainConfig{
		ProviderConfigs:        []string{providerFile},
		DefaultContextWindow:   llm.DefaultContextWindow,
		ProviderModelsCacheTTL: providerModelsTTL.String(),
		LogLevel:               logging.LevelInfo,
		LogFormat:              logging.FormatJSON,
	}

	var configBody any = mainConfig
	if configExists {
		updated, err := updatedSetupConfig(configPath, providerFile, force, updatingProvider)
		if err != nil {
			return err
		}
		configBody = updated
	}

	if err := writeSetupProviderConfig(providerPath, provider, force || updatingProvider); err != nil {
		return err
	}

	writeConfig := writeJSONFileExclusive
	configVerb := "Wrote"
	if configExists {
		writeConfig = writeJSONFileAtomic
		configVerb = "Updated"
	}
	if err := writeConfig(configPath, configBody); err != nil {
		if !providerExists {
			_ = os.Remove(providerPath)
		}
		return err
	}

	providerVerb := "Wrote"
	if providerExists {
		providerVerb = "Updated"
	}
	fmt.Fprintf(env.stdout, "%s %s\n", configVerb, configPath)
	fmt.Fprintf(env.stdout, "%s %s\n", providerVerb, providerPath)
	return nil
}

func runRefreshModels(ctx context.Context, env environment, cfgPath string) error {
	if cfgPath == "" {
		return fmt.Errorf("no config file found")
	}
	data, err := os.ReadFile(cfgPath)
	if err != nil {
		return err
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	files, err := setupProviderConfigs(raw["provider_configs"])
	if err != nil {
		return err
	}
	if len(files) == 0 {
		return fmt.Errorf("%s has no provider_configs", cfgPath)
	}
	dir := filepath.Dir(cfgPath)
	fmt.Fprintln(env.stderr, "harness-model-proxy: refresh-models: refreshing models.dev catalog...")
	catalog, err := refreshCatalog(ctx, env, dir)
	if err != nil {
		return err
	}
	fmt.Fprintf(env.stderr, "harness-model-proxy: refresh-models: models.dev catalog ready (%d providers)\n", len(catalog.Providers))

	type refreshFile struct {
		file      string
		path      string
		providers []llm.ProviderConfig
		missing   bool
		updated   []setupProviderConfig
	}
	loaded := make([]refreshFile, 0, len(files))
	for _, file := range files {
		path := file
		if !filepath.IsAbs(path) {
			path = filepath.Join(dir, file)
		}
		data, err := os.ReadFile(path)
		if errors.Is(err, os.ErrNotExist) {
			fmt.Fprintf(env.stderr, "harness-model-proxy: refresh-models: warning: provider config %s no longer exists; removing its reference\n", file)
			loaded = append(loaded, refreshFile{file: file, path: path, missing: true})
			continue
		}
		if err != nil {
			return err
		}
		providers, err := llm.DecodeProviderConfigs(data)
		if err != nil {
			return fmt.Errorf("%s: %w", path, err)
		}
		if len(providers) == 0 {
			return fmt.Errorf("%s has no providers", path)
		}
		loaded = append(loaded, refreshFile{file: file, path: path, providers: providers})
	}

	var codexProvider *modelcatalog.Provider
	var fetchedSnapshots []modeldiscovery.Snapshot
	for i := range loaded {
		if loaded[i].missing {
			continue
		}
		loaded[i].updated = make([]setupProviderConfig, 0, len(loaded[i].providers))
		for _, current := range loaded[i].providers {
			if current.Name == "" {
				return fmt.Errorf("%s has provider without name", loaded[i].path)
			}
			if current.Name == modelcatalog.OpenAICodexProviderID && codexProvider == nil {
				cached, err := setupCodexProvider(env, dir)
				if err != nil {
					return err
				}
				codexProvider = &cached
			}

			baseline := modeldiscovery.ProviderFromConfig(current)
			meta, inCatalog := setupCatalogProvider(catalog, codexProvider, current.Name)
			if inCatalog {
				baseline = modeldiscovery.OverlayProvider(baseline, meta)
			}
			effective := baseline
			spec, discoverySupported, resolveErr := modeldiscovery.Resolve(current)
			if resolveErr != nil {
				return resolveErr
			}
			_ = spec
			authoritative := false
			var previous *modeldiscovery.Snapshot
			if cached, cacheErr := modeldiscovery.ReadProviderCache(dir, current.Name, current.BaseURL); cacheErr == nil {
				if modeldiscovery.SnapshotMatches(cached, spec, modelcatalog.CodexClientVersion()) {
					previous = &cached
				}
			}
			if discoverySupported {
				fmt.Fprintf(env.stderr, "harness-model-proxy: refresh-models: querying provider %q...\n", current.Name)
				snapshot, discoveryErr := fetchProviderModelSnapshot(ctx, env, dir, current, previous)
				switch {
				case discoveryErr == nil:
					fmt.Fprintf(env.stderr, "harness-model-proxy: refresh-models: provider %q catalog ready (%d models)\n", current.Name, len(snapshot.Models))
					effective = modeldiscovery.MergeProvider(baseline, snapshot)
					authoritative = true
					fetchedSnapshots = append(fetchedSnapshots, snapshot)
				case errors.Is(discoveryErr, context.Canceled):
					return discoveryErr
				case errors.Is(discoveryErr, modeldiscovery.ErrUnsupported):
					discoverySupported = false
					fmt.Fprintf(env.stderr, "harness-model-proxy: refresh-models: warning: provider %q does not support model discovery; using models.dev availability\n", current.Name)
				default:
					if previous != nil {
						effective = modeldiscovery.OverlaySnapshotMetadata(baseline, *previous)
					}
					fmt.Fprintf(env.stderr, "harness-model-proxy: refresh-models: warning: provider model discovery failed for %q: %v; preserving configured models\n", current.Name, discoveryErr)
				}
			}
			if !authoritative && !discoverySupported {
				if !inCatalog {
					fmt.Fprintf(env.stderr, "harness-model-proxy: refresh-models: warning: provider %q from %s is no longer in the model catalog; removing it\n", current.Name, loaded[i].path)
					continue
				}
				if setupProviderAPIType(meta) == "" || setupProviderBaseURL(meta) == "" {
					fmt.Fprintf(env.stderr, "harness-model-proxy: refresh-models: warning: provider %q from %s is no longer supported by harness; removing it\n", current.Name, loaded[i].path)
					continue
				}
				effective = meta
			}

			updatedModels, missing := refreshConfiguredModels(effective, current.Models)
			for _, name := range missing {
				fmt.Fprintf(env.stderr, "harness-model-proxy: refresh-models: warning: model %q of provider %q from %s is no longer in the model catalog (using authoritative provider availability); removing it\n", name, current.Name, loaded[i].path)
			}
			if len(updatedModels) == 0 {
				fmt.Fprintf(env.stderr, "harness-model-proxy: refresh-models: warning: provider %q from %s has no models remaining after refresh; removing it\n", current.Name, loaded[i].path)
				continue
			}
			var catalogMeta *modelcatalog.Provider
			if inCatalog && setupProviderAPIType(meta) != "" && setupProviderBaseURL(meta) != "" {
				catalogMeta = &meta
			}
			loaded[i].updated = append(loaded[i].updated, setupProviderFromCurrent(current, catalogMeta, updatedModels))
		}
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	for _, snapshot := range fetchedSnapshots {
		if err := modeldiscovery.WriteCache(dir, snapshot); err != nil {
			return err
		}
	}

	remainingFiles := make([]string, 0, len(files))
	removedFiles := false
	for _, file := range loaded {
		if file.missing {
			removedFiles = true
			continue
		}
		if len(file.updated) == 0 {
			// Every provider in this file was removed: delete the now-empty file
			// rather than write an unloadable config, and drop its reference below.
			if err := os.Remove(file.path); err != nil && !errors.Is(err, os.ErrNotExist) {
				return err
			}
			fmt.Fprintf(env.stdout, "Removed %s\n", file.path)
			removedFiles = true
			continue
		}
		remainingFiles = append(remainingFiles, file.file)
		var body any = file.updated
		if len(file.updated) == 1 {
			body = file.updated[0]
		}
		if err := writeJSONFileAtomic(file.path, body); err != nil {
			return err
		}
		fmt.Fprintf(env.stdout, "Updated %s\n", file.path)
	}
	if removedFiles {
		if err := setJSONField(raw, "provider_configs", remainingFiles); err != nil {
			return err
		}
		if err := writeJSONFileAtomic(cfgPath, raw); err != nil {
			return err
		}
		fmt.Fprintf(env.stdout, "Updated %s\n", cfgPath)
	}
	return nil
}

func refreshCatalog(ctx context.Context, env environment, configDir string) (*modelcatalog.Catalog, error) {
	return refreshModelsDevCatalog(ctx, env, configDir, "refresh-models")
}

func refreshProviderAfterLogin(ctx context.Context, env environment, cfgPath string, current llm.ProviderConfig) error {
	dir := filepath.Dir(cfgPath)
	var catalog *modelcatalog.Catalog
	if cached, _ := loadModelsDevCacheForServe(dir); cached != nil {
		catalog = cached
	} else {
		fallback, err := modelcatalog.ModelsDevFallback()
		if err != nil {
			return err
		}
		catalog = fallback
	}
	var codexProvider *modelcatalog.Provider
	if current.Name == modelcatalog.OpenAICodexProviderID {
		provider, err := setupCodexProvider(env, dir)
		if err != nil {
			return err
		}
		codexProvider = &provider
	}
	baseline := modeldiscovery.ProviderFromConfig(current)
	meta, inCatalog := setupCatalogProvider(catalog, codexProvider, current.Name)
	if inCatalog {
		baseline = modeldiscovery.OverlayProvider(baseline, meta)
	}
	var previous *modeldiscovery.Snapshot
	if cached, err := modeldiscovery.ReadProviderCache(dir, current.Name, current.BaseURL); err == nil {
		previous = &cached
	}
	snapshot, err := fetchProviderModelSnapshot(ctx, env, dir, current, previous)
	if err != nil {
		return err
	}
	effective := modeldiscovery.MergeProvider(baseline, snapshot)
	models, missing := refreshConfiguredModels(effective, current.Models)
	if err := modeldiscovery.WriteCache(dir, snapshot); err != nil {
		return err
	}
	if len(models) == 0 {
		return fmt.Errorf("authenticated catalog contains none of the configured models; configuration was preserved")
	}
	var catalogMeta *modelcatalog.Provider
	if inCatalog && setupProviderAPIType(meta) != "" && setupProviderBaseURL(meta) != "" {
		catalogMeta = &meta
	}
	next := setupProviderFromCurrent(current, catalogMeta, models)
	if err := replaceProviderConfig(cfgPath, current.Name, next); err != nil {
		return err
	}
	for _, name := range missing {
		fmt.Fprintf(env.stderr, "harness-model-proxy: auth login: warning: configured model %q is absent from the authenticated catalog; removing it\n", name)
	}
	fmt.Fprintf(env.stdout, "Refreshed configured models for %s; rerun setup to enable newly discovered models.\n", current.Name)
	return nil
}

func replaceProviderConfig(configPath, providerName string, replacement setupProviderConfig) error {
	mainData, err := os.ReadFile(configPath)
	if err != nil {
		return err
	}
	var mainRaw map[string]json.RawMessage
	if err := json.Unmarshal(mainData, &mainRaw); err != nil {
		return err
	}
	files, err := setupProviderConfigs(mainRaw["provider_configs"])
	if err != nil {
		return err
	}
	dir := filepath.Dir(configPath)
	for _, file := range files {
		path := file
		if !filepath.IsAbs(path) {
			path = filepath.Join(dir, file)
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		updated, found, err := replaceProviderConfigData(data, providerName, replacement)
		if err != nil {
			return fmt.Errorf("%s: %w", path, err)
		}
		if found {
			return writeBytesAtomic(path, updated, true)
		}
	}
	return fmt.Errorf("provider %q is not present in its configured provider files", providerName)
}

func replaceProviderConfigData(data []byte, providerName string, replacement setupProviderConfig) ([]byte, bool, error) {
	replacementData, err := json.Marshal(replacement)
	if err != nil {
		return nil, false, err
	}
	var array []json.RawMessage
	if err := json.Unmarshal(data, &array); err == nil {
		found, err := replaceProviderRaw(array, providerName, replacementData)
		if err != nil || !found {
			return nil, found, err
		}
		updated, err := json.MarshalIndent(array, "", "  ")
		return updated, true, err
	}
	var wrapper map[string]json.RawMessage
	if err := json.Unmarshal(data, &wrapper); err == nil && wrapper["providers"] != nil {
		var providers []json.RawMessage
		if err := json.Unmarshal(wrapper["providers"], &providers); err != nil {
			return nil, false, err
		}
		found, err := replaceProviderRaw(providers, providerName, replacementData)
		if err != nil || !found {
			return nil, found, err
		}
		providerData, err := json.Marshal(providers)
		if err != nil {
			return nil, false, err
		}
		wrapper["providers"] = providerData
		updated, err := json.MarshalIndent(wrapper, "", "  ")
		return updated, true, err
	}
	var one llm.ProviderConfig
	if err := json.Unmarshal(data, &one); err != nil {
		return nil, false, err
	}
	if one.Name != providerName {
		return nil, false, nil
	}
	updated, err := json.MarshalIndent(replacement, "", "  ")
	return updated, true, err
}

func replaceProviderRaw(providers []json.RawMessage, providerName string, replacement []byte) (bool, error) {
	for i, raw := range providers {
		var pc llm.ProviderConfig
		if err := json.Unmarshal(raw, &pc); err != nil {
			return false, err
		}
		if pc.Name == providerName {
			providers[i] = append(json.RawMessage(nil), replacement...)
			return true, nil
		}
	}
	return false, nil
}

func updatedSetupConfig(path, providerFile string, force bool, allowExisting bool) (map[string]json.RawMessage, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var cfg map[string]json.RawMessage
	if err := json.Unmarshal(data, &cfg); err != nil {
		return nil, err
	}
	if cfg == nil {
		cfg = map[string]json.RawMessage{}
	}

	configs, err := setupProviderConfigs(cfg["provider_configs"])
	if err != nil {
		return nil, err
	}
	if slices.Contains(configs, providerFile) && !force && !allowExisting {
		return nil, fmt.Errorf("%s already references provider config %s", path, providerFile)
	}
	if !slices.Contains(configs, providerFile) {
		configs = append(configs, providerFile)
	}
	if err := setJSONField(cfg, "provider_configs", configs); err != nil {
		return nil, err
	}

	delete(cfg, "provider")
	delete(cfg, "model")
	if _, ok := cfg["default_context_window"]; !ok || force {
		if err := setJSONField(cfg, "default_context_window", llm.DefaultContextWindow); err != nil {
			return nil, err
		}
	}
	return cfg, nil
}

type setupExistingProvider struct {
	File   string
	Config llm.ProviderConfig
}

func loadSetupExistingProviders(configPath string, configExists bool) (map[string]setupExistingProvider, error) {
	existing := map[string]setupExistingProvider{}
	if !configExists {
		return existing, nil
	}
	data, err := os.ReadFile(configPath)
	if err != nil {
		return nil, err
	}
	var cfg map[string]json.RawMessage
	if err := json.Unmarshal(data, &cfg); err != nil {
		return nil, err
	}
	files, err := setupProviderConfigs(cfg["provider_configs"])
	if err != nil {
		return nil, err
	}
	dir := filepath.Dir(configPath)
	for _, file := range files {
		path := file
		if !filepath.IsAbs(path) {
			path = filepath.Join(dir, file)
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return nil, err
		}
		providers, err := llm.DecodeProviderConfigs(data)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", path, err)
		}
		for _, provider := range providers {
			if provider.Name == "" {
				continue
			}
			existing[provider.Name] = setupExistingProvider{
				File:   file,
				Config: provider,
			}
		}
	}
	return existing, nil
}

func setupProviderConfigs(raw json.RawMessage) ([]string, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	var configs []string
	if err := json.Unmarshal(raw, &configs); err != nil {
		return nil, fmt.Errorf("provider_configs must be an array of strings: %w", err)
	}
	return configs, nil
}

func setupProviderFromCatalog(provider modelcatalog.Provider, apiKey string, authCfg *auth.Config, models []modelcatalog.Model) setupProviderConfig {
	entries := make([]setupModelConfig, 0, len(models))
	for _, model := range models {
		entries = append(entries, setupModelFromCatalog(model))
	}
	if isOpenAICodexProvider(provider) {
		enabled := true
		return setupProviderConfig{
			Name:                modelcatalog.OpenAICodexProviderID,
			APIType:             setupProviderAPIType(provider),
			BaseURL:             setupProviderBaseURL(provider),
			Managed:             true,
			OmitMaxOutputTokens: true,
			ResponsesCompaction: &enabled,
			ServerTools:         setupProviderServerTools(modelcatalog.OpenAICodexProviderID, setupProviderAPIType(provider), setupProviderBaseURL(provider)),
			Auth:                setupProviderAuth(provider, authCfg),
			Models:              entries,
		}
	}
	cfg := provider.ProviderConfig(apiKey)
	apiType := setupProviderAPIType(provider)
	baseURL := setupProviderBaseURL(provider)
	out := setupProviderConfig{
		Name:        cfg.Name,
		APIType:     apiType,
		BaseURL:     baseURL,
		APIKey:      cfg.APIKey,
		Managed:     true,
		ServerTools: setupProviderServerTools(cfg.Name, apiType, baseURL),
		APIKeyEnv:   cfg.APIKeyEnv,
		Auth:        authCfg,
		Models:      entries,
	}
	if cfg.Name == "openai" && strings.EqualFold(apiType, "responses") {
		enabled := true
		out.ResponsesCompaction = &enabled
	}
	return out
}

func setupProviderFromCurrent(current llm.ProviderConfig, meta *modelcatalog.Provider, models []modelcatalog.Model) setupProviderConfig {
	var next setupProviderConfig
	if meta != nil {
		next = setupProviderFromCatalog(*meta, current.APIKey, current.Auth, models)
	} else {
		entries := make([]setupModelConfig, 0, len(models))
		for _, model := range models {
			entries = append(entries, setupModelFromCatalog(model))
		}
		next = setupProviderConfig{
			Name: current.Name, APIType: current.APIType, BaseURL: current.BaseURL,
			APIKey: current.APIKey, Managed: true, APIKeyEnv: append([]string(nil), current.APIKeyEnv...),
			Auth: current.Auth, Models: entries,
		}
	}
	next.PriceSource = current.PriceSource
	if current.Name == modelcatalog.OpenAICodexProviderID {
		next.PriceSource = ""
	}
	if current.OmitMaxOutputTokens {
		next.OmitMaxOutputTokens = true
	}
	if current.ReasoningReplay != "" {
		next.ReasoningReplay = current.ReasoningReplay
	}
	if current.UsageInputIncludesCache {
		next.UsageInputIncludesCache = true
	}
	if current.MinOutputTokens > 0 {
		next.MinOutputTokens = current.MinOutputTokens
	}
	next.PromptCache = current.PromptCache
	if current.ResponsesStateful != nil {
		next.ResponsesStateful = current.ResponsesStateful
	}
	if current.ResponsesWebSocket != nil {
		next.ResponsesWebSocket = current.ResponsesWebSocket
	}
	if current.ResponsesCompaction != nil {
		next.ResponsesCompaction = current.ResponsesCompaction
	}
	if current.InteractionsStateful != nil {
		next.InteractionsStateful = current.InteractionsStateful
	}
	if len(current.ServiceTiers) > 0 {
		next.ServiceTiers = llm.NormalizeServiceTiers(current.ServiceTiers)
	}
	if len(current.ServerTools) > 0 {
		next.ServerTools = llm.NormalizeServerTools(current.ServerTools)
	}
	next.ModelDiscovery = current.ModelDiscovery
	preserveReasoningReplayDomains(current.Models, next.Models)
	if meta != nil {
		applySyntheticProviderDefaults(*meta, &next)
	}
	return next
}

func preserveReasoningReplayDomains(current []llm.ModelEntry, next []setupModelConfig) {
	domains := make(map[string]string, len(current))
	for _, model := range current {
		if model.ReasoningReplayDomain != "" {
			domains[model.Name] = model.ReasoningReplayDomain
		}
	}
	for i := range next {
		next[i].ReasoningReplayDomain = domains[next[i].Name]
	}
}

// setupProviderServerTools advertises hosted web search for the provider when
// the persisted (name, apiType, baseURL) is one harness can resolve to a wire
// shape. It delegates to llm.WebSearchServerToolKind — the same function the
// model proxy uses at request time — so the catalog never advertises a tool the
// proxy would later drop.
func setupProviderServerTools(name, apiType, baseURL string) []string {
	if llm.WebSearchServerToolKind(name, apiType, baseURL) == "" {
		return nil
	}
	return []string{llm.ServerToolWebSearch}
}

func setupProviderAuth(provider modelcatalog.Provider, existing *auth.Config) *auth.Config {
	if !isOpenAICodexProvider(provider) {
		return nil
	}
	if existing != nil {
		cfg := *existing
		cfg.Type = auth.TypeCodexOAuth
		if strings.TrimSpace(cfg.Flow) != "" && strings.TrimSpace(cfg.Flow) != auth.FlowDeviceCode {
			cfg.Flow = ""
		}
		return &cfg
	}
	return &auth.Config{Type: auth.TypeCodexOAuth}
}

func promptProviderSelection(r *bufio.Reader, w io.Writer, catalog *modelcatalog.Catalog, codexProvider *modelcatalog.Provider, existing map[string]setupExistingProvider, pageSize int) (modelcatalog.Provider, error) {
	providers := supportedSetupProviders(catalog, codexProvider)
	if len(providers) == 0 {
		return modelcatalog.Provider{}, fmt.Errorf("models.dev catalog has no harness-supported providers")
	}
	entries := make([]setupProviderPick, 0, len(providers))
	for _, provider := range providers {
		_, configured := existing[provider.ID]
		entries = append(entries, setupProviderPick{Provider: provider, Configured: configured})
	}
	selected, err := ui.Pick(func(label string) (string, error) {
		return promptLine(r, w, label)
	}, w, ui.PickerOptions[setupProviderPick]{
		Items:       entries,
		PageSize:    pageSize,
		Prompt:      "Provider (number/id, /search, n/p, q): ",
		Kind:        "provider",
		CancelError: fmt.Errorf("setup cancelled"),
		PrintPage:   printSetupProviderSelectionPage,
	})
	if err != nil {
		return modelcatalog.Provider{}, err
	}
	return selected.Provider, nil
}

func promptModelSelection(r *bufio.Reader, w io.Writer, provider modelcatalog.Provider, enabled map[string]bool, pageSize int) ([]modelcatalog.Model, error) {
	models := provider.ModelsByReleaseDate()
	if len(models) == 0 {
		return nil, fmt.Errorf("provider %q has no models", provider.ID)
	}
	entries := make([]setupModelPick, 0, len(models))
	for _, model := range models {
		entries = append(entries, setupModelPick{Model: model, Enabled: enabled[model.ID]})
	}
	selected, err := pickSetupModels(func(label string) (string, error) {
		return promptLine(r, w, label)
	}, w, provider.ID, entries, pageSize)
	if err != nil {
		return nil, err
	}
	slices.SortFunc(selected, func(a, b modelcatalog.Model) int {
		return strings.Compare(a.ID, b.ID)
	})
	return selected, nil
}

type setupProviderPick struct {
	modelcatalog.Provider
	Configured bool
}

func (p setupProviderPick) PickerID() string      { return p.ID }
func (p setupProviderPick) PickerName() string    { return p.Name }
func (p setupProviderPick) PickerModelCount() int { return len(p.Models) }

func formatSetupPickerID(id string, width int, highlighted bool) string {
	padded := fmt.Sprintf("%-*s", width, id)
	if !highlighted {
		return padded
	}
	return "\x1b[1m" + id + "\x1b[0m" + strings.TrimPrefix(padded, id)
}

func printSetupProviderSelectionPage(w io.Writer, providers []setupProviderPick, page, pageSize int, filter string) {
	start, end := ui.PickerPageBounds(page, pageSize, len(providers))
	title := fmt.Sprintf("Providers %d-%d of %d", start+1, end, len(providers))
	if filter != "" {
		title += fmt.Sprintf(" matching %q", filter)
	}
	fmt.Fprintln(w, title)
	for i := start; i < end; i++ {
		provider := providers[i]
		marker := " "
		id := formatSetupPickerID(provider.PickerID(), 28, provider.Configured)
		name := provider.PickerName()
		if provider.Configured {
			marker = "*"
			name = "\x1b[1m" + name + "\x1b[0m"
		}
		fmt.Fprintf(w, "%s%4d. %s %5d models  %s\n", marker, i+1, id, provider.PickerModelCount(), name)
	}
}

type setupModelPick struct {
	modelcatalog.Model
	Enabled bool
}

func (m setupModelPick) PickerID() string    { return m.ID }
func (m setupModelPick) PickerName() string  { return m.Name }
func (m setupModelPick) PickerPrice() string { return ui.FormatPickerPrice(m.Cost) }
func (m setupModelPick) PickerRelease() string {
	if m.ReleaseDate != "" {
		return m.ReleaseDate
	}
	return m.LastUpdated
}

func pickSetupModels(readLine func(string) (string, error), w io.Writer, providerID string, items []setupModelPick, pageSize int) ([]modelcatalog.Model, error) {
	if readLine == nil {
		return nil, fmt.Errorf("picker has no input reader")
	}
	filter := ""
	page := 0
	for {
		filteredIndexes := filterSetupModelIndexes(items, filter)
		if len(filteredIndexes) == 0 {
			fmt.Fprintf(w, "No models match %q\n", filter)
			filter = ""
			page = 0
			continue
		}
		page = ui.ClampPickerPage(page, len(filteredIndexes), pageSize)
		printSetupModelSelectionPage(w, providerID, items, filteredIndexes, page, pageSize, filter)
		input, err := readLine("Models (number/id toggles, all, none, save, /search, n/p, cancel): ")
		if err != nil {
			return nil, err
		}
		input = strings.TrimSpace(input)
		switch {
		case input == "" || strings.EqualFold(input, "n"):
			if (page+1)*ui.PickerPageSizeValue(pageSize) < len(filteredIndexes) {
				page++
			}
			continue
		case strings.EqualFold(input, "p"):
			if page > 0 {
				page--
			}
			continue
		case strings.EqualFold(input, "cancel"):
			return nil, fmt.Errorf("setup cancelled")
		case strings.EqualFold(input, "all"):
			for i := range items {
				items[i].Enabled = true
			}
			continue
		case strings.EqualFold(input, "none"):
			for i := range items {
				items[i].Enabled = false
			}
			continue
		case strings.EqualFold(input, "save"):
			selected := selectedSetupModels(items)
			if len(selected) == 0 {
				fmt.Fprintln(w, setupSaveEmptySelectionPrompt)
				continue
			}
			return selected, nil
		case strings.HasPrefix(input, "/"):
			filter = strings.TrimSpace(strings.TrimPrefix(input, "/"))
			page = 0
			continue
		}
		if n, ok := ui.ParsePickerSelectionNumber(input, len(filteredIndexes)); ok {
			idx := filteredIndexes[n-1]
			items[idx].Enabled = !items[idx].Enabled
			continue
		}
		if idx, matches, ok := resolveSetupModelSelection(items, input); ok {
			items[idx].Enabled = !items[idx].Enabled
			continue
		} else if len(matches) > 1 {
			fmt.Fprintf(w, "Matches: %s\n", setupModelMatchSummary(items, matches, 8))
			continue
		}
		filter = input
		page = 0
	}
}

func filterSetupModelIndexes(items []setupModelPick, filter string) []int {
	filter = strings.ToLower(strings.TrimSpace(filter))
	indexes := make([]int, 0, len(items))
	for i, item := range items {
		if filter == "" ||
			strings.Contains(strings.ToLower(item.PickerID()), filter) ||
			strings.Contains(strings.ToLower(item.PickerName()), filter) {
			indexes = append(indexes, i)
		}
	}
	return indexes
}

func printSetupModelSelectionPage(w io.Writer, providerID string, items []setupModelPick, indexes []int, page, pageSize int, filter string) {
	start, end := ui.PickerPageBounds(page, pageSize, len(indexes))
	enabled := len(selectedSetupModels(items))
	title := fmt.Sprintf("Models for %s %d-%d of %d (%d enabled)", providerID, start+1, end, len(indexes), enabled)
	if filter != "" {
		title += fmt.Sprintf(" matching %q", filter)
	}
	fmt.Fprintln(w, title)
	fmt.Fprintln(w, ui.ModelPickerPriceLegend)
	for pos := start; pos < end; pos++ {
		item := items[indexes[pos]]
		price := item.PickerPrice()
		if price == "" {
			price = "-"
		}
		release := item.PickerRelease()
		if release == "" {
			release = "-"
		}
		marker := " "
		id := formatSetupPickerID(ui.ClipPickerText(item.PickerID(), 34), 43, item.Enabled)
		name := item.PickerName()
		if item.Enabled {
			marker = "*"
			name = "\x1b[1m" + name + "\x1b[0m"
		}
		fmt.Fprintf(w, "%s%4d. %s %-12s %10s  %s\n", marker, pos+1, id, price, release, name)
	}
}

func selectedSetupModels(items []setupModelPick) []modelcatalog.Model {
	selected := make([]modelcatalog.Model, 0, len(items))
	for _, item := range items {
		if item.Enabled {
			selected = append(selected, item.Model)
		}
	}
	return selected
}

func setupConfiguredModelSet(models []llm.ModelEntry) map[string]bool {
	enabled := map[string]bool{}
	for _, model := range models {
		if model.Name != "" {
			enabled[model.Name] = true
		}
	}
	return enabled
}

func resolveSetupModelSelection(items []setupModelPick, input string) (selected int, matches []int, ok bool) {
	input = strings.ToLower(strings.TrimSpace(input))
	if input == "" {
		return 0, nil, false
	}
	var prefix []int
	for i, item := range items {
		id := strings.ToLower(item.PickerID())
		name := strings.ToLower(item.PickerName())
		if id == input || name == input {
			return i, nil, true
		}
		if strings.HasPrefix(id, input) || strings.HasPrefix(name, input) {
			prefix = append(prefix, i)
		}
	}
	if len(prefix) == 1 {
		return prefix[0], nil, true
	}
	return 0, prefix, false
}

func setupModelMatchSummary(items []setupModelPick, matches []int, limit int) string {
	if len(matches) > limit {
		matches = matches[:limit]
	}
	parts := make([]string, 0, len(matches))
	for _, idx := range matches {
		item := items[idx]
		parts = append(parts, item.PickerID()+ui.PickerDisplayNameSuffix(item.PickerName(), item.PickerID()))
	}
	return strings.Join(parts, ", ")
}

func supportedSetupProviders(catalog *modelcatalog.Catalog, codexProvider *modelcatalog.Provider) []modelcatalog.Provider {
	var providers []modelcatalog.Provider
	for _, provider := range catalog.ProvidersList() {
		if setupProviderAPIType(provider) == "" || setupProviderBaseURL(provider) == "" || len(provider.Models) == 0 {
			continue
		}
		providers = append(providers, provider)
	}
	if _, hasOpenAI := catalog.Provider("openai"); hasOpenAI && codexProvider != nil && len(codexProvider.Models) > 0 && !setupProviderListContains(providers, modelcatalog.OpenAICodexProviderID) {
		providers = append(providers, *codexProvider)
	}
	sort.Slice(providers, func(i, j int) bool {
		if strings.EqualFold(providers[i].Name, providers[j].Name) {
			return providers[i].ID < providers[j].ID
		}
		return strings.ToLower(providers[i].Name) < strings.ToLower(providers[j].Name)
	})
	return providers
}

func setupProviderListContains(providers []modelcatalog.Provider, id string) bool {
	for _, provider := range providers {
		if provider.ID == id {
			return true
		}
	}
	return false
}

func setupCatalogProvider(catalog *modelcatalog.Catalog, codexProvider *modelcatalog.Provider, id string) (modelcatalog.Provider, bool) {
	if id == modelcatalog.OpenAICodexProviderID {
		if codexProvider == nil || len(codexProvider.Models) == 0 {
			return modelcatalog.Provider{}, false
		}
		return *codexProvider, true
	}
	return catalog.Provider(id)
}

func setupProviderAPIType(provider modelcatalog.Provider) string {
	if isOpenAICodexProvider(provider) {
		return "responses"
	}
	if isSakanaProvider(provider) {
		return "responses"
	}
	return provider.APIType()
}

func setupProviderBaseURL(provider modelcatalog.Provider) string {
	if isOpenAICodexProvider(provider) {
		return modelcatalog.OpenAICodexProviderBaseURL
	}
	return provider.BaseURL()
}

func isOpenAICodexProvider(provider modelcatalog.Provider) bool {
	return provider.ID == modelcatalog.OpenAICodexProviderID
}

func isSakanaProvider(provider modelcatalog.Provider) bool {
	return strings.EqualFold(strings.TrimSpace(provider.ID), "sakana") ||
		strings.Contains(strings.ToLower(provider.BaseURL()), "api.sakana.ai")
}

func applySyntheticProviderDefaults(provider modelcatalog.Provider, cfg *setupProviderConfig) {
	if cfg == nil {
		return
	}
	// Keep the managed ChatGPT Codex config self-describing instead of relying on
	// the generic auto resolver's provider-name/backend-URL recognition.
	if isOpenAICodexProvider(provider) && strings.TrimSpace(cfg.PromptCache.KeyField) == "" {
		cfg.PromptCache.KeyField = llm.PromptCacheKeyFieldPromptCacheKey
	}
	// Sakana's Responses implementation does not accept previous_response_id and
	// requires the full conversation each request, so stateful continuation must
	// stay off.
	if isSakanaProvider(provider) {
		stateful := false
		cfg.ResponsesStateful = &stateful
	}
	if strings.EqualFold(strings.TrimSpace(provider.ID), "google") && cfg.InteractionsStateful == nil {
		stateful := true
		cfg.InteractionsStateful = &stateful
	}
	// Kimi for Coding's OpenAI endpoint requires preserved thinking in
	// multi-turn tool loops (Kimi's own CLI replays reasoning on both
	// protocols); harness replays it as compact reasoning_content.
	if strings.EqualFold(strings.TrimSpace(provider.ID), "kimi-for-coding") && cfg.ReasoningReplay == "" {
		cfg.ReasoningReplay = llm.ReasoningReplayFull
	}
}

func setupPageSize(env environment) int {
	rows := 0
	if env.terminalRows != nil {
		rows = env.terminalRows()
	}
	return ui.PickerPageSize(rows)
}

func setJSONField(cfg map[string]json.RawMessage, key string, value any) error {
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	cfg[key] = data
	return nil
}

// refreshConfiguredModels re-resolves each configured model against the refreshed
// catalog provider. Models that are no longer present in the catalog are dropped
// and their names returned in missing so the caller can warn; the surviving
// models are returned in their original order. It never errors on a missing or
// empty result — the caller decides what to do with a provider left with no
// models (warn and remove it).
func refreshConfiguredModels(provider modelcatalog.Provider, current []llm.ModelEntry) (models []modelcatalog.Model, missing []string) {
	models = make([]modelcatalog.Model, 0, len(current))
	for _, entry := range current {
		if entry.Name == "" {
			continue
		}
		model, ok := setupProviderModel(provider, entry.Name)
		if !ok {
			missing = append(missing, entry.Name)
			continue
		}
		models = append(models, model)
	}
	return models, missing
}

func setupProviderModel(provider modelcatalog.Provider, id string) (modelcatalog.Model, bool) {
	if provider.Models == nil {
		return modelcatalog.Model{}, false
	}
	if model, ok := provider.Models[id]; ok {
		return model, true
	}
	for _, model := range provider.Models {
		if model.ID == id {
			return model, true
		}
	}
	return modelcatalog.Model{}, false
}

func writeSetupProviderConfig(path string, provider setupProviderConfig, force bool) error {
	if force {
		return writeJSONFileAtomic(path, provider)
	}
	return writeJSONFileExclusive(path, provider)
}

// marshalJSONLine renders v as indented JSON with a trailing newline, the
// on-disk form both config writers share.
func marshalJSONLine(v any) ([]byte, error) {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(data, '\n'), nil
}

func writeJSONFileAtomic(path string, v any) error {
	data, err := marshalJSONLine(v)
	if err != nil {
		return err
	}

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
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpPath, path); err != nil {
		return err
	}
	return nil
}

func setupCatalog(ctx context.Context, env environment) (*modelcatalog.Catalog, error) {
	ttl := defaultModelsDevTTL
	if env.modelsDevCacheTTL != nil {
		ttl = *env.modelsDevCacheTTL
	}
	return cachedOrFetchedSetupCatalog(ctx, env, defaultConfigDir(env.getenv), ttl)
}

// setupModelFromCatalog builds the on-disk entry for one selected model.
// Managed configs never store a static pricing schedule: the proxy resolves
// prices live from the models.dev cache or provider-specific pricers, so leaving
// Price nil keeps refreshed prices reaching the running server without another
// setup. The complete models.dev schedule is still shown in the interactive
// picker; it just isn't persisted.
func setupModelFromCatalog(model modelcatalog.Model) setupModelConfig {
	serviceTiers := llm.NormalizeServiceTiers(model.ServiceTiers)
	for i := range serviceTiers {
		serviceTiers[i].Price = llm.Price{}
	}
	cfg := setupModelConfig{
		Name:                      model.ID,
		ContextWindow:             model.Limit.Context,
		OutputLimit:               model.Limit.Output,
		InputModalities:           append([]string(nil), model.Modalities.Input...),
		ReasoningSummarySupported: model.ReasoningSummarySupported,
		ReasoningOptions:          append([]llm.ReasoningOption(nil), model.ReasoningOptions...),
		ServiceTiers:              serviceTiers,
	}
	reasoning := model.Reasoning
	cfg.Reasoning = &reasoning
	return cfg
}

func promptLine(r *bufio.Reader, w io.Writer, label string) (string, error) {
	if _, err := fmt.Fprint(w, label); err != nil {
		return "", err
	}
	line, err := r.ReadString('\n')
	if err != nil && !(errors.Is(err, io.EOF) && line != "") {
		return "", err
	}
	return strings.TrimSpace(line), nil
}

func writeJSONFileExclusive(path string, v any) error {
	data, err := marshalJSONLine(v)
	if err != nil {
		return err
	}

	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
}

func pathExists(path string) (bool, error) {
	_, err := os.Stat(path)
	if err == nil {
		return true, nil
	}
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	return false, err
}

func providerConfigFilename(name string) string {
	var b strings.Builder
	lastDash := false
	for _, r := range strings.ToLower(name) {
		ok := (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '_' || r == '.'
		if ok {
			b.WriteRune(r)
			lastDash = false
			continue
		}
		if !lastDash {
			b.WriteByte('-')
			lastDash = true
		}
	}
	s := strings.Trim(b.String(), ".-")
	if s == "" {
		s = "provider"
	}
	return s + ".json"
}
