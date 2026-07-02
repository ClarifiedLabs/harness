// Setup wizard and provider-config refresh for harness-model-proxy: the
// `harness-model-proxy setup` interactive flow (models.dev-backed
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

	"harness/internal/auth"
	"harness/internal/llm"
	"harness/internal/logging"
	"harness/internal/modelsdev"
	"harness/internal/ui"
)

type setupMainConfig struct {
	ProviderConfigs      []string `json:"provider_configs"`
	DefaultContextWindow int      `json:"default_context_window"`
	LogLevel             string   `json:"log_level,omitempty"`
	LogFormat            string   `json:"log_format,omitempty"`
}

type setupProviderConfig struct {
	Name    string `json:"name"`
	APIType string `json:"api_type"`
	BaseURL string `json:"base_url"`
	APIKey  string `json:"api_key,omitempty"`
	// Managed is always true for configs written by setup/refresh. For priced
	// providers, flat prices are resolved live from the models.dev cache or a
	// provider-specific pricer, so the per-model entries below carry no price.
	Managed bool `json:"managed,omitempty"`
	// PriceSource names the models.dev provider id whose prices apply to this
	// managed provider when it differs from Name. Empty means price from Name.
	PriceSource string `json:"price_source,omitempty"`
	// OmitMaxOutputTokens suppresses Responses max_output_tokens for compatible
	// backends that reject the standard parameter, such as ChatGPT Codex.
	OmitMaxOutputTokens bool                  `json:"omit_max_output_tokens,omitempty"`
	PromptCache         llm.PromptCacheConfig `json:"prompt_cache,omitempty"`
	ResponsesStateful   *bool                 `json:"responses_stateful,omitempty"`
	ResponsesWebSocket  *bool                 `json:"responses_websocket,omitempty"`
	ServerTools         []string              `json:"server_tools,omitempty"`
	APIKeyEnv           []string              `json:"api_key_env,omitempty"`
	Auth                *auth.Config          `json:"auth,omitempty"`
	Models              []setupModelConfig    `json:"models"`
}

type setupModelConfig struct {
	Name             string                `json:"name"`
	ContextWindow    int                   `json:"context_window,omitempty"`
	OutputLimit      int                   `json:"output_limit,omitempty"`
	InputModalities  []string              `json:"input_modalities,omitempty"`
	ServerTools      []string              `json:"server_tools,omitempty"`
	Price            *llm.Price            `json:"price,omitempty"`
	Reasoning        *bool                 `json:"reasoning,omitempty"`
	ReasoningOptions []llm.ReasoningOption `json:"reasoning_options,omitempty"`
}

const (
	openAICodexProviderID      = "openai-codex"
	openAICodexProviderName    = "OpenAI Codex (ChatGPT subscription)"
	openAICodexProviderBaseURL = "https://chatgpt.com/backend-api/codex"
)

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
	codexCatalog, err := setupCodexModelsCatalog(env, dir)
	if err != nil {
		return err
	}
	existingProviders, err := loadSetupExistingProviders(configPath, configExists)
	if err != nil {
		return err
	}

	providerMeta, err := promptProviderSelection(reader, env.stdout, catalog, codexCatalog, existingProviders, setupPageSize(env))
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
	models, err := promptModelSelection(reader, env.stdout, providerMeta, setupConfiguredModelSet(existingProvider.Config.Models), setupPageSize(env))
	if err != nil {
		return err
	}

	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}

	provider := setupProviderFromModelsDev(providerMeta, apiKey, authCfg, models)
	if existingProvider.Config.OmitMaxOutputTokens {
		provider.OmitMaxOutputTokens = true
	}
	if existingProvider.Config.ResponsesStateful != nil {
		provider.ResponsesStateful = existingProvider.Config.ResponsesStateful
	}
	if existingProvider.Config.ResponsesWebSocket != nil {
		provider.ResponsesWebSocket = existingProvider.Config.ResponsesWebSocket
	}
	applySyntheticProviderDefaults(providerMeta, &provider)

	mainConfig := setupMainConfig{
		ProviderConfigs:      []string{providerFile},
		DefaultContextWindow: llm.DefaultContextWindow,
		LogLevel:             logging.LevelInfo,
		LogFormat:            logging.FormatJSON,
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
	catalog, err := refreshCatalog(ctx, env, filepath.Dir(cfgPath))
	if err != nil {
		return err
	}

	dir := filepath.Dir(cfgPath)
	var codexCatalog *codexModelsCatalog
	// Files kept after refresh, in original order. Provider files whose every
	// provider disappeared from the catalog are deleted and dropped from this
	// list so a stale reference does not fail the next refresh; when any file is
	// dropped the main config's provider_configs is rewritten to match.
	remainingFiles := make([]string, 0, len(files))
	removedFiles := false
	for _, file := range files {
		path := file
		if !filepath.IsAbs(path) {
			path = filepath.Join(dir, file)
		}
		data, err := os.ReadFile(path)
		if errors.Is(err, os.ErrNotExist) {
			fmt.Fprintf(env.stderr, "harness-model-proxy: refresh-models: warning: provider config %s no longer exists; removing its reference\n", file)
			removedFiles = true
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
		updated := make([]setupProviderConfig, 0, len(providers))
		for _, current := range providers {
			if current.Name == "" {
				return fmt.Errorf("%s has provider without name", path)
			}
			if current.Name == openAICodexProviderID && codexCatalog == nil {
				codexCatalog, err = refreshCodexModelsCatalog(ctx, env, dir)
				if err != nil {
					return err
				}
			}
			meta, ok := setupCatalogProvider(catalog, codexCatalog, current.Name)
			if !ok {
				fmt.Fprintf(env.stderr, "harness-model-proxy: refresh-models: warning: provider %q from %s is no longer in the model catalog; removing it\n", current.Name, path)
				continue
			}
			if setupProviderAPIType(meta) == "" || setupProviderBaseURL(meta) == "" {
				fmt.Fprintf(env.stderr, "harness-model-proxy: refresh-models: warning: provider %q from %s is no longer supported by harness; removing it\n", current.Name, path)
				continue
			}
			updatedModels, missing := refreshConfiguredModels(meta, current.Models)
			for _, name := range missing {
				fmt.Fprintf(env.stderr, "harness-model-proxy: refresh-models: warning: model %q of provider %q from %s is no longer in the model catalog; removing it\n", name, current.Name, path)
			}
			if len(updatedModels) == 0 {
				fmt.Fprintf(env.stderr, "harness-model-proxy: refresh-models: warning: provider %q from %s has no models remaining after refresh; removing it\n", current.Name, path)
				continue
			}
			next := setupProviderFromModelsDev(meta, current.APIKey, current.Auth, updatedModels)
			if current.OmitMaxOutputTokens {
				next.OmitMaxOutputTokens = true
			}
			next.PromptCache = current.PromptCache
			if current.ResponsesStateful != nil {
				next.ResponsesStateful = current.ResponsesStateful
			}
			if current.ResponsesWebSocket != nil {
				next.ResponsesWebSocket = current.ResponsesWebSocket
			}
			applySyntheticProviderDefaults(meta, &next)
			updated = append(updated, next)
		}
		if len(updated) == 0 {
			// Every provider in this file was removed: delete the now-empty file
			// rather than write an unloadable config, and drop its reference below.
			if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
				return err
			}
			fmt.Fprintf(env.stdout, "Removed %s\n", path)
			removedFiles = true
			continue
		}
		remainingFiles = append(remainingFiles, file)
		var body any = updated
		if len(updated) == 1 {
			body = updated[0]
		}
		if err := writeJSONFileAtomic(path, body); err != nil {
			return err
		}
		fmt.Fprintf(env.stdout, "Updated %s\n", path)
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

func refreshCatalog(ctx context.Context, env environment, configDir string) (*modelsdev.Catalog, error) {
	return refreshModelsDevCatalog(ctx, env, configDir, "refresh-models")
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

func setupProviderFromModelsDev(provider modelsdev.Provider, apiKey string, authCfg *auth.Config, models []modelsdev.Model) setupProviderConfig {
	entries := make([]setupModelConfig, 0, len(models))
	for _, model := range models {
		entries = append(entries, setupModelFromModelsDev(model))
	}
	if isOpenAICodexProvider(provider) {
		return setupProviderConfig{
			Name:                openAICodexProviderID,
			APIType:             setupProviderAPIType(provider),
			BaseURL:             setupProviderBaseURL(provider),
			Managed:             true,
			OmitMaxOutputTokens: true,
			ServerTools:         setupProviderServerTools(openAICodexProviderID, setupProviderAPIType(provider), setupProviderBaseURL(provider)),
			Auth:                setupProviderAuth(provider, authCfg),
			Models:              entries,
		}
	}
	if isSakanaProvider(provider) {
		stateful := false
		cfg := provider.ProviderConfig(apiKey)
		return setupProviderConfig{
			Name:              cfg.Name,
			APIType:           setupProviderAPIType(provider),
			BaseURL:           setupProviderBaseURL(provider),
			APIKey:            cfg.APIKey,
			Managed:           true,
			ResponsesStateful: &stateful,
			ServerTools:       setupProviderServerTools(cfg.Name, setupProviderAPIType(provider), setupProviderBaseURL(provider)),
			APIKeyEnv:         cfg.APIKeyEnv,
			Models:            entries,
		}
	}
	cfg := provider.ProviderConfig(apiKey)
	return setupProviderConfig{
		Name:        cfg.Name,
		APIType:     cfg.APIType,
		BaseURL:     cfg.BaseURL,
		APIKey:      cfg.APIKey,
		Managed:     true,
		ServerTools: setupProviderServerTools(cfg.Name, cfg.APIType, cfg.BaseURL),
		APIKeyEnv:   cfg.APIKeyEnv,
		Auth:        authCfg,
		Models:      entries,
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

func setupProviderAuth(provider modelsdev.Provider, existing *auth.Config) *auth.Config {
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

func promptProviderSelection(r *bufio.Reader, w io.Writer, catalog *modelsdev.Catalog, codexCatalog *codexModelsCatalog, existing map[string]setupExistingProvider, pageSize int) (modelsdev.Provider, error) {
	providers := supportedSetupProviders(catalog, codexCatalog)
	if len(providers) == 0 {
		return modelsdev.Provider{}, fmt.Errorf("models.dev catalog has no harness-supported providers")
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
		return modelsdev.Provider{}, err
	}
	return selected.Provider, nil
}

func promptModelSelection(r *bufio.Reader, w io.Writer, provider modelsdev.Provider, enabled map[string]bool, pageSize int) ([]modelsdev.Model, error) {
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
	slices.SortFunc(selected, func(a, b modelsdev.Model) int {
		return strings.Compare(a.ID, b.ID)
	})
	return selected, nil
}

type setupProviderPick struct {
	modelsdev.Provider
	Configured bool
}

func (p setupProviderPick) PickerID() string      { return p.ID }
func (p setupProviderPick) PickerName() string    { return p.Name }
func (p setupProviderPick) PickerModelCount() int { return len(p.Models) }

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
		id := provider.PickerID()
		name := provider.PickerName()
		if provider.Configured {
			marker = "*"
			id = "\x1b[1m" + id + "\x1b[0m"
			name = "\x1b[1m" + name + "\x1b[0m"
		}
		fmt.Fprintf(w, "%s%4d. %-28s %5d models  %s\n", marker, i+1, id, provider.PickerModelCount(), name)
	}
}

type setupModelPick struct {
	modelsdev.Model
	Enabled bool
}

func (m setupModelPick) PickerID() string    { return m.ID }
func (m setupModelPick) PickerName() string  { return m.Name }
func (m setupModelPick) PickerPrice() string { return formatPickerPrice(m.Cost) }
func (m setupModelPick) PickerRelease() string {
	if m.ReleaseDate != "" {
		return m.ReleaseDate
	}
	return m.LastUpdated
}

func pickSetupModels(readLine func(string) (string, error), w io.Writer, providerID string, items []setupModelPick, pageSize int) ([]modelsdev.Model, error) {
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
				fmt.Fprintln(w, "Select at least one model before continuing.")
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
		id := ui.ClipPickerText(item.PickerID(), 34)
		name := item.PickerName()
		if item.Enabled {
			marker = "*"
			id = "\x1b[1m" + id + "\x1b[0m"
			name = "\x1b[1m" + name + "\x1b[0m"
		}
		fmt.Fprintf(w, "%s%4d. %-43s %12s %10s  %s\n", marker, pos+1, id, price, release, name)
	}
}

func selectedSetupModels(items []setupModelPick) []modelsdev.Model {
	selected := make([]modelsdev.Model, 0, len(items))
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

func supportedSetupProviders(catalog *modelsdev.Catalog, codexCatalog *codexModelsCatalog) []modelsdev.Provider {
	var providers []modelsdev.Provider
	for _, provider := range catalog.ProvidersList() {
		if setupProviderAPIType(provider) == "" || setupProviderBaseURL(provider) == "" || len(provider.Models) == 0 {
			continue
		}
		providers = append(providers, provider)
	}
	if _, hasOpenAI := catalog.Provider("openai"); hasOpenAI && !setupProviderListContains(providers, openAICodexProviderID) {
		codex, ok := openAICodexProvider(codexCatalog)
		if ok {
			providers = append(providers, codex)
		}
	}
	if !setupProviderListContains(providers, sakanaProviderID) {
		sakana, ok := sakanaProvider()
		if ok {
			providers = append(providers, sakana)
		}
	}
	sort.Slice(providers, func(i, j int) bool {
		if strings.EqualFold(providers[i].Name, providers[j].Name) {
			return providers[i].ID < providers[j].ID
		}
		return strings.ToLower(providers[i].Name) < strings.ToLower(providers[j].Name)
	})
	return providers
}

func setupProviderListContains(providers []modelsdev.Provider, id string) bool {
	for _, provider := range providers {
		if provider.ID == id {
			return true
		}
	}
	return false
}

func setupCatalogProvider(catalog *modelsdev.Catalog, codexCatalog *codexModelsCatalog, id string) (modelsdev.Provider, bool) {
	if id == openAICodexProviderID {
		return openAICodexProvider(codexCatalog)
	}
	if id == sakanaProviderID {
		return sakanaProvider()
	}
	return catalog.Provider(id)
}

func openAICodexProvider(catalog *codexModelsCatalog) (modelsdev.Provider, bool) {
	return openAICodexProviderFromCatalog(catalog)
}

func setupProviderAPIType(provider modelsdev.Provider) string {
	if isOpenAICodexProvider(provider) {
		return "responses"
	}
	if isSakanaProvider(provider) {
		return "responses"
	}
	return provider.APIType()
}

func setupProviderBaseURL(provider modelsdev.Provider) string {
	if isOpenAICodexProvider(provider) {
		return openAICodexProviderBaseURL
	}
	if isSakanaProvider(provider) {
		return sakanaProviderBaseURL
	}
	return provider.BaseURL()
}

func isOpenAICodexProvider(provider modelsdev.Provider) bool {
	return provider.ID == openAICodexProviderID
}

func isSakanaProvider(provider modelsdev.Provider) bool {
	return provider.ID == sakanaProviderID
}

func applySyntheticProviderDefaults(provider modelsdev.Provider, cfg *setupProviderConfig) {
	if cfg == nil {
		return
	}
	if isSakanaProvider(provider) {
		stateful := false
		cfg.ResponsesStateful = &stateful
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
func refreshConfiguredModels(provider modelsdev.Provider, current []llm.ModelEntry) (models []modelsdev.Model, missing []string) {
	models = make([]modelsdev.Model, 0, len(current))
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

func setupProviderModel(provider modelsdev.Provider, id string) (modelsdev.Model, bool) {
	if provider.Models == nil {
		return modelsdev.Model{}, false
	}
	if model, ok := provider.Models[id]; ok {
		return model, true
	}
	for _, model := range provider.Models {
		if model.ID == id {
			return model, true
		}
	}
	return modelsdev.Model{}, false
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

func setupCatalog(ctx context.Context, env environment) (*modelsdev.Catalog, error) {
	ttl := defaultModelsDevTTL
	if env.modelsDevCacheTTL != nil {
		ttl = *env.modelsDevCacheTTL
	}
	return cachedOrFetchedSetupCatalog(ctx, env, defaultConfigDir(env.getenv), ttl)
}

// setupModelFromModelsDev builds the on-disk entry for one selected model.
// Managed configs never store a flat price: the proxy resolves flat prices live
// from the models.dev cache or provider-specific pricers, so leaving Price nil
// keeps refreshed prices reaching the running server without another setup. The
// models.dev price is still shown in the interactive picker (formatPickerPrice),
// it just isn't persisted.
func setupModelFromModelsDev(model modelsdev.Model) setupModelConfig {
	cfg := setupModelConfig{
		Name:             model.ID,
		ContextWindow:    model.Limit.Context,
		OutputLimit:      model.Limit.Output,
		InputModalities:  append([]string(nil), model.Modalities.Input...),
		ReasoningOptions: append([]llm.ReasoningOption(nil), model.ReasoningOptions...),
	}
	reasoning := model.Reasoning
	cfg.Reasoning = &reasoning
	return cfg
}

func formatPickerPrice(p llm.Price) string {
	if p.Input == 0 && p.Output == 0 && p.CacheRead == 0 && p.CacheWrite == 0 {
		return ""
	}
	return fmt.Sprintf("$%s/$%s", formatPriceComponent(p.Input), formatPriceComponent(p.Output))
}

func formatPriceComponent(v float64) string {
	if v == float64(int64(v)) {
		return fmt.Sprintf("%.0f", v)
	}
	return strings.TrimRight(strings.TrimRight(fmt.Sprintf("%.2f", v), "0"), ".")
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
