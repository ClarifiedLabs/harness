package llm

import (
	"fmt"
	"net/url"
	"strings"

	"harness/internal/auth"
)

// ProfileCodex selects the bundle of ChatGPT Codex subscription-backend
// behaviors: Codex client identity headers, ChatGPT account auth headers,
// WebSocket transport with turn-state continuation, store:false, omitted
// max_output_tokens, local o200k preflight counting, trigger-based native
// compaction, no dollar pricing, and the account quota/reset-credit endpoints.
const ProfileCodex = "codex"

// ProfileKimiCodePlan and ProfileZAICodingPlan select the Kimi Code Plan and
// Z.AI Coding Plan subscription quota integrations. Unlike ProfileCodex they
// carry no dialect behavior — the wire shape stays the configured api_type —
// but they let renamed providers and second accounts keep subscription quota
// reporting under their own names.
const (
	ProfileKimiCodePlan = "kimi-code-plan"
	ProfileZAICodingPlan = "zai-coding-plan"
)

// Kimi Code Plan provider IDs published by the models.dev catalog: the China
// deployment (api.kimi.com) and the global deployment (api.kimi.ai). Both run
// the same subscription quota integration under ProfileKimiCodePlan.
const (
	KimiCodePlanCNProviderName     = "kimi-code-plan-cn"
	KimiCodePlanGlobalProviderName = "kimi-code-plan-global"
)

// CodexProviderName is the canonical provider ID managed setup writes for the
// ChatGPT Codex subscription provider. modelcatalog.OpenAICodexProviderID
// aliases it (the catalog imports llm; llm must not import the catalog).
const CodexProviderName = "openai-codex"

// CodexCanonicalBaseURL is the canonical ChatGPT Codex backend endpoint.
// modelcatalog.OpenAICodexProviderBaseURL aliases it.
const CodexCanonicalBaseURL = "https://chatgpt.com/backend-api/codex"

// NormalizeProfile canonicalizes a provider profile string for comparison.
func NormalizeProfile(profile string) string {
	return strings.ToLower(strings.TrimSpace(profile))
}

// ValidProfile reports whether profile is empty or a known provider profile.
func ValidProfile(profile string) bool {
	switch NormalizeProfile(profile) {
	case "", ProfileCodex, ProfileKimiCodePlan, ProfileZAICodingPlan:
		return true
	default:
		return false
	}
}

// ValidateProfile rejects unknown profiles and profiles incompatible with the
// provider's configured dialect.
func ValidateProfile(pc ProviderConfig) error {
	profile := NormalizeProfile(pc.Profile)
	if profile == "" {
		return nil
	}
	if !ValidProfile(profile) {
		return fmt.Errorf("unknown profile %q (want %q, %q, or %q)", pc.Profile, ProfileCodex, ProfileKimiCodePlan, ProfileZAICodingPlan)
	}
	if profile == ProfileCodex {
		// The proxy resolves an empty api_type from the provider name
		// (runtimeOptionsForTarget); validate the dialect the config will run
		// as, not just the literal field.
		apiType := strings.ToLower(strings.TrimSpace(pc.APIType))
		if apiType == "" {
			apiType = strings.ToLower(strings.TrimSpace(pc.Name))
		}
		if apiType != "responses" {
			return fmt.Errorf("profile %q requires api_type %q, got %q", ProfileCodex, "responses", apiType)
		}
	}
	return nil
}

// CodexBackend reports whether the provider targets the ChatGPT Codex
// subscription backend. An explicit profile wins; absent a profile, legacy
// detection matches the canonical provider name, codex_oauth auth, or the
// canonical Codex base URL so older configs keep working.
func (pc ProviderConfig) CodexBackend() bool {
	if profile := NormalizeProfile(pc.Profile); profile != "" {
		return profile == ProfileCodex
	}
	if strings.EqualFold(strings.TrimSpace(pc.Name), CodexProviderName) {
		return true
	}
	if pc.Auth != nil && strings.EqualFold(strings.TrimSpace(pc.Auth.Type), auth.TypeCodexOAuth) {
		return true
	}
	return CanonicalCodexBaseURL(pc.BaseURL)
}

// CanonicalCodexBaseURL reports whether baseURL is the canonical ChatGPT Codex
// backend endpoint.
func CanonicalCodexBaseURL(baseURL string) bool {
	u, err := url.Parse(strings.TrimSpace(baseURL))
	if err != nil {
		return false
	}
	return strings.EqualFold(u.Hostname(), "chatgpt.com") &&
		strings.TrimRight(u.Path, "/") == "/backend-api/codex"
}
