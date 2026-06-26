// Package factory selects and constructs a concrete llm.Provider from
// user-facing options (design §7). It lives in its own package, importing both
// dialects directly, rather than as internal/llm/factory.go: a file inside
// internal/llm cannot import internal/llm/openai or internal/llm/anthropic
// (those packages import internal/llm, which would be an import cycle). A
// self-contained package avoids the alternative — init()-time registration that
// only fires if some other package blank-imports the dialects — and keeps
// internal/llm free of any dialect dependency.
package factory

import (
	"fmt"
	"strings"

	"harness/internal/llm"
	"harness/internal/llm/anthropic"
	"harness/internal/llm/openai"
	"harness/internal/llm/responses"
)

// Options is the resolved, provider-neutral configuration handed to the factory
// (design §7). API keys are passed in (resolved from the environment by the
// config layer), never read here.
type Options struct {
	Provider      string // api type: "openai" | "responses" | "anthropic"; empty = infer from Model
	ProviderName  string // configured provider name, e.g. "openrouter"
	Model         string
	BaseURL       string
	APIKey        string
	AuthHeaders   map[string]string
	MaxTokens     int
	Temperature   *float64
	ContextWindow int
	OutputLimit   int // model's real max-output-token limit; 0 = unknown
	// OmitMaxOutputTokens suppresses Responses max_output_tokens for providers
	// that reject the standard parameter.
	OmitMaxOutputTokens bool
	ResponsesWebSocket  bool
	ReasoningMode       string // "openai" | "openrouter" | "anthropic"; empty = infer
}

// New constructs the provider selected by opts. The provider is inferred from
// the model name (claude* -> anthropic, else openai) unless Provider is set
// explicitly, in which case it wins (design §7). An empty API key is allowed
// only when a custom (non-default) base URL is supplied — local servers need
// none.
func New(opts Options) (llm.Provider, error) {
	if opts.Model == "" {
		return nil, fmt.Errorf("llm: a model is required")
	}

	provider := opts.Provider
	if provider == "" {
		provider = inferProvider(opts.Model)
	}

	switch provider {
	case "anthropic":
		if opts.APIKey == "" && opts.BaseURL == "" && len(opts.AuthHeaders) == 0 {
			return nil, fmt.Errorf("llm: ANTHROPIC_API_KEY is required (or set a custom base URL for a local server)")
		}
		return anthropic.New(anthropic.Config{
			APIKey:        opts.APIKey,
			AuthHeaders:   opts.AuthHeaders,
			BaseURL:       opts.BaseURL,
			ContextWindow: opts.ContextWindow,
			OutputLimit:   opts.OutputLimit,
		}), nil
	case "openai":
		if opts.APIKey == "" && opts.BaseURL == "" && len(opts.AuthHeaders) == 0 {
			return nil, fmt.Errorf("llm: OPENAI_API_KEY is required (or set a custom base URL for a local server)")
		}
		return openai.New(openai.Config{
			APIKey:        opts.APIKey,
			AuthHeaders:   opts.AuthHeaders,
			BaseURL:       opts.BaseURL,
			ContextWindow: opts.ContextWindow,
			OutputLimit:   opts.OutputLimit,
			ReasoningMode: reasoningMode(opts.ProviderName, provider, opts.BaseURL, opts.ReasoningMode),
		}), nil
	case "responses":
		if opts.APIKey == "" && opts.BaseURL == "" && len(opts.AuthHeaders) == 0 {
			return nil, fmt.Errorf("llm: OPENAI_API_KEY is required (or set a custom base URL for a local server)")
		}
		return responses.New(responses.Config{
			APIKey:              opts.APIKey,
			AuthHeaders:         opts.AuthHeaders,
			BaseURL:             opts.BaseURL,
			ContextWindow:       opts.ContextWindow,
			OutputLimit:         opts.OutputLimit,
			OmitMaxOutputTokens: opts.OmitMaxOutputTokens,
			UseWebSocket:        opts.ResponsesWebSocket,
		}), nil
	default:
		return nil, fmt.Errorf("llm: unknown provider %q (want openai, responses, or anthropic)", provider)
	}
}

// inferProvider applies the §7 selection rule: model names starting with
// "claude" are Anthropic; everything else is OpenAI-compatible (the right
// fallback for arbitrary local model names).
func inferProvider(model string) string {
	if strings.HasPrefix(model, "claude") {
		return "anthropic"
	}
	return "openai"
}

func reasoningMode(providerName, apiType, baseURL, explicit string) string {
	if explicit != "" {
		return explicit
	}
	if apiType == "anthropic" {
		return "anthropic"
	}
	if strings.EqualFold(providerName, "openrouter") || strings.Contains(strings.ToLower(baseURL), "openrouter.ai") {
		return "openrouter"
	}
	return "openai"
}
