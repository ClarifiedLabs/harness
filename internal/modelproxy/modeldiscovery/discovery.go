// Package modeldiscovery fetches authenticated provider model catalogs and
// normalizes their heterogeneous wire formats for the model proxy.
package modeldiscovery

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"harness/internal/auth"
	"harness/internal/llm"
	"harness/internal/modelcatalog"
)

type Format string

const (
	FormatOpenAI     Format = "openai"
	FormatOpenRouter Format = "openrouter"
	FormatCodex      Format = "codex"
	FormatAnthropic  Format = "anthropic"
	FormatGemini     Format = "gemini"
	FormatMeta       Format = "meta"
)

const (
	maxPages        = 100
	maxModels       = 100_000
	maxResponseBody = 32 << 20
)

var (
	ErrNoCredentials = errors.New("provider model discovery: no credentials")
	ErrUnsupported   = errors.New("provider model discovery: endpoint unsupported")
)

// Model is one normalized provider model. Pointer and Known fields distinguish
// an absent capability from an explicit zero or false value.
type Model struct {
	ID                        string                `json:"id"`
	Name                      string                `json:"name,omitempty"`
	ContextWindow             *int                  `json:"context_window,omitempty"`
	OutputLimit               *int                  `json:"output_limit,omitempty"`
	InputModalities           []string              `json:"input_modalities,omitempty"`
	InputModalitiesKnown      bool                  `json:"input_modalities_known,omitempty"`
	Reasoning                 *bool                 `json:"reasoning,omitempty"`
	ReasoningSummarySupported *bool                 `json:"reasoning_summary_supported,omitempty"`
	ReasoningOptions          []llm.ReasoningOption `json:"reasoning_options,omitempty"`
	Shape                     *string               `json:"shape,omitempty"`
	Price                     *llm.Price            `json:"price,omitempty"`
	ServiceTiers              []llm.ServiceTier     `json:"service_tiers,omitempty"`
	Eligible                  bool                  `json:"eligible"`
}

// Snapshot is a complete successful response from one provider endpoint.
type Snapshot struct {
	Version              int              `json:"version"`
	Provider             string           `json:"provider"`
	BaseURL              string           `json:"base_url"`
	Endpoint             string           `json:"endpoint"`
	Format               Format           `json:"format"`
	CodexClientVersion   string           `json:"codex_client_version,omitempty"`
	FetchedAt            time.Time        `json:"fetched_at"`
	ETag                 string           `json:"etag,omitempty"`
	Complete             bool             `json:"complete"`
	IncludeUnknownModels bool             `json:"include_unknown_models,omitempty"`
	Models               map[string]Model `json:"models"`
}

// State pairs a snapshot with whether it is fresh enough to determine current
// availability. Stale snapshots remain useful as metadata fallbacks.
type State struct {
	Snapshot      Snapshot
	Authoritative bool
	Unsupported   bool
}

type Spec struct {
	Endpoint             string
	Format               Format
	IncludeUnknownModels bool
	TrustedGenerative    bool
	AutoEndpoint         bool
}

type Fetcher struct {
	Client             *http.Client
	ConfigDir          string
	Getenv             func(string) string
	Now                func() time.Time
	CodexClientVersion string
}

// Resolve determines whether direct discovery is enabled and supported for pc.
func Resolve(pc llm.ProviderConfig) (Spec, bool, error) {
	if pc.ModelDiscovery != nil && pc.ModelDiscovery.Enabled != nil && !*pc.ModelDiscovery.Enabled {
		return Spec{}, false, nil
	}
	base, err := url.Parse(strings.TrimSpace(pc.BaseURL))
	if err != nil || base.Scheme == "" || base.Host == "" {
		if pc.ModelDiscovery != nil && strings.TrimSpace(pc.ModelDiscovery.URL) != "" {
			return Spec{}, false, fmt.Errorf("provider %q model_discovery: invalid base_url %q", pc.Name, pc.BaseURL)
		}
		return Spec{}, false, nil
	}
	host := strings.ToLower(base.Hostname())
	name := strings.ToLower(strings.TrimSpace(pc.Name))
	apiType := strings.ToLower(strings.TrimSpace(pc.APIType))

	format := Format("")
	trusted := false
	switch {
	case name == "openai-codex" || strings.Contains(host, "chatgpt.com") ||
		(pc.Auth != nil && strings.EqualFold(strings.TrimSpace(pc.Auth.Type), auth.TypeCodexOAuth)):
		format, trusted = FormatCodex, true
	case name == "meta" || host == "api.meta.ai":
		format = FormatMeta
	case name == "openrouter" || strings.Contains(host, "openrouter.ai"):
		format = FormatOpenRouter
	case name == "google" || apiType == "interactions" || strings.Contains(host, "generativelanguage.googleapis.com"):
		format, trusted = FormatGemini, true
	case name == "anthropic" || apiType == "anthropic" || strings.Contains(host, "api.anthropic.com"):
		format, trusted = FormatAnthropic, true
	case apiType == "openai" || apiType == "responses":
		format = FormatOpenAI
		trusted = name == "sakana" || strings.Contains(host, "api.sakana.ai")
	}

	if pc.ModelDiscovery != nil && strings.TrimSpace(pc.ModelDiscovery.Format) != "" {
		format = Format(strings.ToLower(strings.TrimSpace(pc.ModelDiscovery.Format)))
		if !validFormat(format) {
			return Spec{}, false, fmt.Errorf("provider %q model_discovery: unsupported format %q", pc.Name, pc.ModelDiscovery.Format)
		}
	}
	if format == "" {
		if pc.ModelDiscovery != nil && (pc.ModelDiscovery.Enabled != nil || pc.ModelDiscovery.URL != "") {
			return Spec{}, false, fmt.Errorf("provider %q model_discovery: format is required for this provider", pc.Name)
		}
		return Spec{}, false, nil
	}

	endpoint := ""
	if pc.ModelDiscovery != nil {
		endpoint = strings.TrimSpace(pc.ModelDiscovery.URL)
	}
	autoEndpoint := endpoint == ""
	if autoEndpoint {
		endpoint = defaultEndpoint(base, format)
	}
	parsed, err := url.Parse(endpoint)
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" || parsed.User != nil || parsed.Fragment != "" {
		return Spec{}, false, fmt.Errorf("provider %q model_discovery: url must be an absolute HTTP(S) URL without userinfo or fragment", pc.Name)
	}
	includeUnknown := trusted
	if pc.ModelDiscovery != nil && pc.ModelDiscovery.IncludeUnknownModels != nil {
		includeUnknown = *pc.ModelDiscovery.IncludeUnknownModels
	}
	return Spec{Endpoint: parsed.String(), Format: format, IncludeUnknownModels: includeUnknown, TrustedGenerative: trusted, AutoEndpoint: autoEndpoint}, true, nil
}

func validFormat(format Format) bool {
	switch format {
	case FormatOpenAI, FormatOpenRouter, FormatCodex, FormatAnthropic, FormatGemini, FormatMeta:
		return true
	default:
		return false
	}
}

func defaultEndpoint(base *url.URL, format Format) string {
	u := *base
	u.RawQuery = ""
	u.Fragment = ""
	u.Path = strings.TrimRight(u.Path, "/") + "/models"
	return u.String()
}

// SnapshotMatches reports whether a cached snapshot was produced with the
// current discovery settings. Codex snapshots are also scoped to the official
// client compatibility version because that query can change catalog results.
func SnapshotMatches(snapshot Snapshot, spec Spec, codexClientVersion string) bool {
	if snapshot.Format != spec.Format || snapshot.Endpoint != spec.Endpoint || snapshot.IncludeUnknownModels != spec.IncludeUnknownModels {
		return false
	}
	return spec.Format != FormatCodex || snapshot.CodexClientVersion == strings.TrimSpace(codexClientVersion)
}

func (f Fetcher) Fetch(ctx context.Context, pc llm.ProviderConfig, previous *Snapshot) (Snapshot, error) {
	spec, ok, err := Resolve(pc)
	if err != nil || !ok {
		return Snapshot{}, err
	}
	codexClientVersion := ""
	if spec.Format == FormatCodex {
		codexClientVersion = strings.TrimSpace(f.CodexClientVersion)
		if err := modelcatalog.ValidateCodexClientVersion(codexClientVersion); err != nil {
			return Snapshot{}, fmt.Errorf("provider %q model discovery: %w", pc.Name, err)
		}
	}
	headers, err := f.headers(ctx, pc, spec.Format)
	if err != nil {
		return Snapshot{}, err
	}
	now := time.Now
	if f.Now != nil {
		now = f.Now
	}
	client := f.Client
	if client == nil {
		client = http.DefaultClient
	}
	etag := ""
	previousMatches := previous != nil && previous.Provider == pc.Name && previous.BaseURL == pc.BaseURL && SnapshotMatches(*previous, spec, codexClientVersion)
	if previousMatches {
		etag = previous.ETag
	}
	models, responseETag, notModified, err := fetchPages(ctx, client, spec, headers, etag, codexClientVersion)
	if err != nil {
		return Snapshot{}, err
	}
	if notModified {
		if !previousMatches || len(previous.Models) == 0 || !previous.Complete {
			return Snapshot{}, fmt.Errorf("provider %q model discovery returned 304 without a complete cached snapshot", pc.Name)
		}
		out := *previous
		out.Models = cloneModels(previous.Models)
		out.FetchedAt = now()
		return out, nil
	}
	if len(models) == 0 {
		return Snapshot{}, fmt.Errorf("provider %q model discovery returned no generative models", pc.Name)
	}
	return Snapshot{
		Version: 1, Provider: pc.Name, BaseURL: pc.BaseURL, Endpoint: spec.Endpoint,
		Format: spec.Format, CodexClientVersion: codexClientVersion, FetchedAt: now(), ETag: responseETag, Complete: true,
		IncludeUnknownModels: spec.IncludeUnknownModels, Models: models,
	}, nil
}

func (f Fetcher) headers(ctx context.Context, pc llm.ProviderConfig, format Format) (http.Header, error) {
	headers := make(http.Header)
	if pc.Auth != nil {
		src, err := auth.NewSource(*pc.Auth, auth.Options{Name: pc.Name, ConfigDir: f.ConfigDir, Getenv: f.getenv(), Client: f.Client, Now: f.Now})
		if err != nil {
			return nil, err
		}
		resolved, err := src.Headers(ctx)
		if err != nil {
			return nil, err
		}
		for key, value := range resolved {
			headers.Set(key, value)
		}
	} else {
		apiKey := ""
		for _, name := range pc.APIKeyEnv {
			if value := f.getenv()(name); value != "" {
				apiKey = value
				break
			}
		}
		if apiKey == "" {
			for _, name := range fallbackKeyEnvironments(format) {
				if value := f.getenv()(name); value != "" {
					apiKey = value
					break
				}
			}
		}
		if apiKey == "" {
			apiKey = pc.APIKey
		}
		if strings.TrimSpace(apiKey) == "" {
			return nil, ErrNoCredentials
		}
		switch format {
		case FormatAnthropic:
			headers.Set("x-api-key", apiKey)
		case FormatGemini:
			headers.Set("x-goog-api-key", apiKey)
		default:
			headers.Set("authorization", "Bearer "+apiKey)
		}
	}
	if format == FormatAnthropic {
		headers.Set("anthropic-version", "2023-06-01")
	}
	return headers, nil
}

func (f Fetcher) getenv() func(string) string {
	if f.Getenv != nil {
		return f.Getenv
	}
	return os.Getenv
}

func fallbackKeyEnvironments(format Format) []string {
	switch format {
	case FormatAnthropic:
		return []string{"ANTHROPIC_API_KEY"}
	case FormatGemini:
		return []string{"GOOGLE_API_KEY", "GOOGLE_GENERATIVE_AI_API_KEY", "GEMINI_API_KEY"}
	case FormatOpenRouter:
		return []string{"OPENROUTER_API_KEY"}
	case FormatMeta:
		return []string{"META_MODEL_API_KEY"}
	default:
		return []string{"RESPONSES_API_KEY", "OPENAI_API_KEY"}
	}
}

func cloneModels(in map[string]Model) map[string]Model {
	out := make(map[string]Model, len(in))
	for id, model := range in {
		model.InputModalities = append([]string(nil), model.InputModalities...)
		model.ReasoningOptions = append([]llm.ReasoningOption(nil), model.ReasoningOptions...)
		model.ServiceTiers = append([]llm.ServiceTier(nil), model.ServiceTiers...)
		out[id] = model
	}
	return out
}
