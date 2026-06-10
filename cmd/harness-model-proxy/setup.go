// Setup wizard and provider-config refresh for harness-model-proxy: the
// `harness-model-proxy --setup` interactive flow (models.dev-backed
// provider/model pickers) and the `--refresh-models` re-sync of provider config
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
	Name      string             `json:"name"`
	APIType   string             `json:"api_type"`
	BaseURL   string             `json:"base_url"`
	APIKey    string             `json:"api_key,omitempty"`
	APIKeyEnv []string           `json:"api_key_env,omitempty"`
	Auth      *auth.Config       `json:"auth,omitempty"`
	Models    []setupModelConfig `json:"models"`
}

type setupModelConfig struct {
	Name             string                `json:"name"`
	ContextWindow    int                   `json:"context_window,omitempty"`
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
	existingProviders, err := loadSetupExistingProviders(configPath, configExists)
	if err != nil {
		return err
	}

	providerMeta, err := promptProviderSelection(reader, env.stdout, catalog, existingProviders, setupPageSize(env))
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
	catalog, err := refreshCatalog(ctx, env)
	if err != nil {
		return err
	}

	dir := filepath.Dir(cfgPath)
	for _, file := range files {
		path := file
		if !filepath.IsAbs(path) {
			path = filepath.Join(dir, file)
		}
		data, err := os.ReadFile(path)
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
			meta, ok := setupCatalogProvider(catalog, current.Name)
			if !ok {
				return fmt.Errorf("provider %q from %s was not found in models.dev", current.Name, path)
			}
			if setupProviderAPIType(meta) == "" || setupProviderBaseURL(meta) == "" {
				return fmt.Errorf("provider %q from %s is not supported by harness", current.Name, path)
			}
			updatedModels, err := refreshConfiguredModels(meta, current.Models)
			if err != nil {
				return fmt.Errorf("%s: provider %q: %w", path, current.Name, err)
			}
			updated = append(updated, setupProviderFromModelsDev(meta, current.APIKey, current.Auth, updatedModels))
		}
		var body any = updated
		if len(updated) == 1 {
			body = updated[0]
		}
		if err := writeJSONFileAtomic(path, body); err != nil {
			return err
		}
		fmt.Fprintf(env.stdout, "Updated %s\n", path)
	}
	return nil
}

func refreshCatalog(ctx context.Context, env environment) (*modelsdev.Catalog, error) {
	if env.modelsDevCatalog != nil {
		return env.modelsDevCatalog(ctx)
	}
	return defaultModelsDevCatalog(ctx)
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
			Name:    openAICodexProviderID,
			APIType: setupProviderAPIType(provider),
			BaseURL: setupProviderBaseURL(provider),
			Auth:    setupProviderAuth(provider, authCfg),
			Models:  entries,
		}
	}
	cfg := provider.ProviderConfig(apiKey)
	return setupProviderConfig{
		Name:      cfg.Name,
		APIType:   cfg.APIType,
		BaseURL:   cfg.BaseURL,
		APIKey:    cfg.APIKey,
		APIKeyEnv: cfg.APIKeyEnv,
		Auth:      authCfg,
		Models:    entries,
	}
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

func promptProviderSelection(r *bufio.Reader, w io.Writer, catalog *modelsdev.Catalog, existing map[string]setupExistingProvider, pageSize int) (modelsdev.Provider, error) {
	providers := supportedSetupProviders(catalog)
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

func supportedSetupProviders(catalog *modelsdev.Catalog) []modelsdev.Provider {
	var providers []modelsdev.Provider
	for _, provider := range catalog.ProvidersList() {
		if setupProviderAPIType(provider) == "" || setupProviderBaseURL(provider) == "" || len(provider.Models) == 0 {
			continue
		}
		providers = append(providers, provider)
	}
	if codex, ok := openAICodexProvider(catalog); ok && !setupProviderListContains(providers, openAICodexProviderID) {
		providers = append(providers, codex)
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

func setupCatalogProvider(catalog *modelsdev.Catalog, id string) (modelsdev.Provider, bool) {
	if id == openAICodexProviderID {
		return openAICodexProvider(catalog)
	}
	return catalog.Provider(id)
}

func openAICodexProvider(catalog *modelsdev.Catalog) (modelsdev.Provider, bool) {
	openai, ok := catalog.Provider("openai")
	if !ok || len(openai.Models) == 0 {
		return modelsdev.Provider{}, false
	}
	models := make(map[string]modelsdev.Model, len(openai.Models))
	for id, model := range openai.Models {
		models[id] = model
	}
	return modelsdev.Provider{
		ID:     openAICodexProviderID,
		Name:   openAICodexProviderName,
		API:    openAICodexProviderBaseURL,
		Models: models,
	}, true
}

func setupProviderAPIType(provider modelsdev.Provider) string {
	if isOpenAICodexProvider(provider) {
		return "responses"
	}
	return provider.APIType()
}

func setupProviderBaseURL(provider modelsdev.Provider) string {
	if isOpenAICodexProvider(provider) {
		return openAICodexProviderBaseURL
	}
	return provider.BaseURL()
}

func isOpenAICodexProvider(provider modelsdev.Provider) bool {
	return provider.ID == openAICodexProviderID
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

func refreshConfiguredModels(provider modelsdev.Provider, current []llm.ModelEntry) ([]modelsdev.Model, error) {
	models := make([]modelsdev.Model, 0, len(current))
	for _, entry := range current {
		if entry.Name == "" {
			continue
		}
		model, ok := setupProviderModel(provider, entry.Name)
		if !ok {
			return nil, fmt.Errorf("configured model %q was not found in models.dev", entry.Name)
		}
		models = append(models, model)
	}
	if len(models) == 0 {
		return nil, fmt.Errorf("no configured models")
	}
	return models, nil
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
	if env.modelsDevCatalog != nil {
		catalog, err := env.modelsDevCatalog(ctx)
		if err == nil {
			return catalog, nil
		}
		if errors.Is(err, context.Canceled) {
			return nil, err
		}
		fallback, fallbackErr := modelsdev.Fallback()
		if fallbackErr != nil {
			return nil, fmt.Errorf("models.dev lookup failed: %v; vendored fallback failed: %w", err, fallbackErr)
		}
		fmt.Fprintf(env.stderr, "harness-model-proxy: setup: warning: models.dev lookup failed: %v; using vendored fallback\n", err)
		return fallback, nil
	}
	return modelsdev.Fallback()
}

func setupModelFromModelsDev(model modelsdev.Model) setupModelConfig {
	cfg := setupModelConfig{
		Name:             model.ID,
		ContextWindow:    model.Limit.Context,
		ReasoningOptions: append([]llm.ReasoningOption(nil), model.ReasoningOptions...),
	}
	reasoning := model.Reasoning
	cfg.Reasoning = &reasoning
	if setupPriceKnown(model.Cost) {
		price := model.Cost
		cfg.Price = &price
	}
	return cfg
}

func setupPriceKnown(p llm.Price) bool {
	return p.Input != 0 || p.Output != 0 || p.CacheRead != 0 || p.CacheWrite != 0
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
