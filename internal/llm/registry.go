package llm

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"harness/internal/auth"
)

// Price is the per-1M-token price in USD for each token category. CacheRead and
// CacheWrite are 0 when a provider has no separate cache pricing.
type Price struct {
	Input      float64 `json:"input"`
	Output     float64 `json:"output"`
	CacheRead  float64 `json:"cache_read"`
	CacheWrite float64 `json:"cache_write"`
}

// ModelInfo is the registry entry for one model.
type ModelInfo struct {
	ContextWindow   int            `json:"context_window"`
	OutputLimit     int            `json:"output_limit,omitempty"`
	InputModalities []string       `json:"input_modalities,omitempty"`
	Price           Price          `json:"price"`
	Reasoning       *ReasoningInfo `json:"reasoning,omitempty"`
}

// ProviderConfig is the on-disk schema for a provider JSON file.
type ProviderConfig struct {
	Name    string `json:"name"`
	APIType string `json:"api_type"`
	BaseURL string `json:"base_url"`
	APIKey  string `json:"api_key"`
	// Managed marks a config written by `--setup`/`--refresh-models`. Managed
	// configs omit flat per-model prices; the proxy resolves flat prices live
	// from the in-memory models.dev cache or provider-specific pricers. A config
	// lacking this flag (e.g. a hand-written one) is treated as manual and keeps
	// its own configured prices.
	Managed bool `json:"managed,omitempty"`
	// PriceSource overrides which models.dev provider id a managed config's prices
	// are resolved from. Empty means "this provider's own name". It exists so a
	// provider whose models are billed at another provider's rates can price from
	// that provider instead. Ignored for manual configs.
	PriceSource string `json:"price_source,omitempty"`
	// OmitMaxOutputTokens suppresses Responses max_output_tokens for compatible
	// backends that reject the standard parameter, such as ChatGPT Codex.
	OmitMaxOutputTokens bool              `json:"omit_max_output_tokens,omitempty"`
	PromptCache         PromptCacheConfig `json:"prompt_cache,omitempty"`
	ResponsesStateful   *bool             `json:"responses_stateful,omitempty"`
	ResponsesWebSocket  *bool             `json:"responses_websocket,omitempty"`
	APIKeyEnv           []string          `json:"api_key_env"`
	Auth                *auth.Config      `json:"auth,omitempty"`
	Models              []ModelEntry      `json:"models"`
}

// PromptCacheConfig controls how a provider receives the stable
// Request.PromptCacheKey. Empty/auto keeps provider-specific defaults.
type PromptCacheConfig struct {
	KeyField        string   `json:"key_field,omitempty"`
	AffinityHeaders []string `json:"affinity_headers,omitempty"`
}

// ModelEntry is one model inside a ProviderConfig.
type ModelEntry struct {
	Name             string            `json:"name"`
	ContextWindow    int               `json:"context_window"`
	OutputLimit      int               `json:"output_limit,omitempty"`
	InputModalities  []string          `json:"input_modalities,omitempty"`
	Price            Price             `json:"price"`
	Reasoning        *bool             `json:"reasoning,omitempty"`
	ReasoningOptions []ReasoningOption `json:"reasoning_options,omitempty"`
}

// DefaultContextWindow is used for any model not in the registry — arbitrary
// names on OpenAI-compatible servers. Conservative; configurable via
// -default-context-window and overridable per run via -context-window.
const DefaultContextWindow = 256_000

// Registry holds model info loaded from provider config files.
type Registry struct {
	models               map[string]ModelInfo
	qualified            map[string]ModelInfo
	defaultContextWindow int
}

// NewRegistry builds a Registry from a pre-built map. Tests use this to avoid
// file I/O.
func NewRegistry(models map[string]ModelInfo) *Registry {
	if models == nil {
		models = map[string]ModelInfo{}
	}
	return &Registry{
		models:               models,
		qualified:            map[string]ModelInfo{},
		defaultContextWindow: DefaultContextWindow,
	}
}

// NewRegistryWithQualified builds a Registry with both unqualified model
// lookups and provider-qualified lookups such as "openrouter:gpt-5.5".
func NewRegistryWithQualified(models, qualified map[string]ModelInfo) *Registry {
	r := NewRegistry(models)
	if qualified != nil {
		r.qualified = qualified
	}
	return r
}

// LoadProviderConfigs reads each provider config file, logs warnings for missing
// or malformed files, and returns a Registry containing all discovered models.
// Paths are resolved relative to configDir.
func LoadProviderConfigs(configDir string, files []string, warn func(string)) (*Registry, []ProviderConfig, error) {
	models := map[string]ModelInfo{}
	qualified := map[string]ModelInfo{}
	var providers []ProviderConfig
	for _, f := range files {
		path := f
		if !filepath.IsAbs(path) {
			path = filepath.Join(configDir, f)
		}
		data, err := os.ReadFile(path)
		if err != nil {
			if warn != nil {
				warn(fmt.Sprintf("warning: skipping provider config %s: %v", f, err))
			}
			continue
		}
		pcs, err := DecodeProviderConfigs(data)
		if err != nil {
			if warn != nil {
				warn(fmt.Sprintf("warning: skipping provider config %s: %v", f, err))
			}
			continue
		}
		for _, pc := range pcs {
			providers = append(providers, pc)
			addProviderModels(models, qualified, pc)
		}
	}
	registry := NewRegistry(models)
	registry.qualified = qualified
	return registry, providers, nil
}

// RegistryFromProviderConfigs builds a Registry from already-decoded provider
// configs, without any file I/O. The proxy uses this to rebuild its registry
// after resolving managed-provider prices from the live models.dev cache.
func RegistryFromProviderConfigs(providers []ProviderConfig) *Registry {
	models := map[string]ModelInfo{}
	qualified := map[string]ModelInfo{}
	for _, pc := range providers {
		addProviderModels(models, qualified, pc)
	}
	return NewRegistryWithQualified(models, qualified)
}

func addProviderModels(models, qualified map[string]ModelInfo, pc ProviderConfig) {
	for _, m := range pc.Models {
		info := ModelInfo{
			ContextWindow:   m.ContextWindow,
			OutputLimit:     m.OutputLimit,
			InputModalities: append([]string(nil), m.InputModalities...),
			Price:           m.Price,
			Reasoning:       modelEntryReasoning(m),
		}
		models[m.Name] = info
		if pc.Name != "" {
			qualified[pc.Name+":"+m.Name] = info
		}
	}
}

// Lookup returns the configured info for model, if any. The returned bool only
// says an entry exists; the entry may still omit context or price fields.
func (r *Registry) Lookup(model string) (ModelInfo, bool) {
	if r == nil {
		return ModelInfo{}, false
	}
	info, ok := r.models[model]
	if ok {
		return info, true
	}
	info, ok = r.qualified[model]
	return info, ok
}

// Models returns the configured model names in stable order.
func (r *Registry) Models() []string {
	if r == nil {
		return nil
	}
	names := make([]string, 0, len(r.models))
	for name := range r.models {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// HasPrice reports whether model has any non-zero configured price component.
func (r *Registry) HasPrice(model string) bool {
	info, ok := r.Lookup(model)
	return ok && !priceZero(info.Price)
}

// SupportsInputModality reports whether model explicitly advertises an input
// modality such as "image". Missing model metadata or missing modalities are
// treated as unsupported.
func (r *Registry) SupportsInputModality(model, modality string) bool {
	info, ok := r.Lookup(model)
	if !ok {
		return false
	}
	modality = strings.ToLower(strings.TrimSpace(modality))
	for _, m := range info.InputModalities {
		if strings.ToLower(strings.TrimSpace(m)) == modality {
			return true
		}
	}
	return false
}

// MergeModel fills missing registry metadata for model. Explicit provider
// config values win; discovered data is used only where the registry has no
// context window or no price at all.
func (r *Registry) MergeModel(model string, info ModelInfo) {
	if r == nil || model == "" {
		return
	}
	target := r.models
	if _, ok := r.qualified[model]; ok {
		target = r.qualified
	}
	current := target[model]
	if current.ContextWindow <= 0 && info.ContextWindow > 0 {
		current.ContextWindow = info.ContextWindow
	}
	if current.OutputLimit <= 0 && info.OutputLimit > 0 {
		current.OutputLimit = info.OutputLimit
	}
	if priceZero(current.Price) && !priceZero(info.Price) {
		current.Price = info.Price
	}
	if len(current.InputModalities) == 0 && len(info.InputModalities) > 0 {
		current.InputModalities = append([]string(nil), info.InputModalities...)
	}
	if current.Reasoning == nil && info.Reasoning != nil {
		current.Reasoning = info.Reasoning.Clone()
	} else if current.Reasoning != nil && len(current.Reasoning.Options) == 0 && info.Reasoning != nil && len(info.Reasoning.Options) > 0 {
		current.Reasoning.Options = info.Reasoning.Clone().Options
	}
	target[model] = current
}

// SetDefaultContextWindow sets the fallback window used when a model has no
// configured context window. Non-positive values reset to the built-in default.
func (r *Registry) SetDefaultContextWindow(window int) {
	if r == nil {
		return
	}
	if window <= 0 {
		window = DefaultContextWindow
	}
	r.defaultContextWindow = window
}

func DecodeProviderConfigs(data []byte) ([]ProviderConfig, error) {
	var many []ProviderConfig
	if err := json.Unmarshal(data, &many); err == nil {
		return many, nil
	}

	var wrapper struct {
		Providers []ProviderConfig `json:"providers"`
	}
	if err := json.Unmarshal(data, &wrapper); err == nil && wrapper.Providers != nil {
		return wrapper.Providers, nil
	}

	var one ProviderConfig
	if err := json.Unmarshal(data, &one); err != nil {
		return nil, err
	}
	return []ProviderConfig{one}, nil
}

// Cost returns the USD cost of the given usage for the named model, and whether
// the model was found in the registry. Unknown models report (0, false) so the
// UI can show token counts without a dollar figure.
func (r *Registry) Cost(model string, u Usage) (usd float64, known bool) {
	if r == nil {
		return 0, false
	}
	info, ok := r.Lookup(model)
	if !ok {
		return 0, false
	}
	const perMillion = 1_000_000.0
	p := info.Price
	if priceZero(p) {
		return 0, false
	}
	usd = float64(u.InputTokens)/perMillion*p.Input +
		float64(u.OutputTokens)/perMillion*p.Output +
		float64(u.CacheReadTokens)/perMillion*p.CacheRead +
		float64(u.CacheWriteTokens)/perMillion*p.CacheWrite
	return usd, true
}

func priceZero(p Price) bool {
	return p.Input == 0 && p.Output == 0 && p.CacheRead == 0 && p.CacheWrite == 0
}

func modelEntryReasoning(m ModelEntry) *ReasoningInfo {
	if m.Reasoning == nil && len(m.ReasoningOptions) == 0 {
		return nil
	}
	supported := false
	if m.Reasoning != nil {
		supported = *m.Reasoning
	}
	return (&ReasoningInfo{
		Supported: supported,
		Options:   append([]ReasoningOption(nil), m.ReasoningOptions...),
	}).Clone()
}

// ContextWindow returns the model's context window from the registry, or the
// configured default when the registry has no context metadata for it.
func (r *Registry) ContextWindow(model string) int {
	if r == nil {
		return DefaultContextWindow
	}
	if info, ok := r.Lookup(model); ok && info.ContextWindow > 0 {
		return info.ContextWindow
	}
	if r.defaultContextWindow > 0 {
		return r.defaultContextWindow
	}
	return DefaultContextWindow
}

// OutputLimit returns the model's known max-output-token limit from the
// registry, or 0 when unknown. Unlike ContextWindow there is no configured
// default: an unknown output limit leaves the per-request max_tokens cap to the
// shared automatic policy.
func (r *Registry) OutputLimit(model string) int {
	if r == nil {
		return 0
	}
	if info, ok := r.Lookup(model); ok {
		return info.OutputLimit
	}
	return 0
}
