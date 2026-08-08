package modelcatalog

import (
	"strings"

	"harness/internal/llm"
)

// NormalizeOpenAIFastServiceTiers canonicalizes OpenAI's renamed Fast mode.
// Older OpenAI catalog records call the same mode priority with a Fast display
// name, but current first-party OpenAI requests must use service_tier fast.
func NormalizeOpenAIFastServiceTiers(tiers []llm.ServiceTier) []llm.ServiceTier {
	out := append([]llm.ServiceTier(nil), tiers...)
	for i := range out {
		id := strings.ToLower(strings.TrimSpace(out[i].ID))
		name := strings.ToLower(strings.TrimSpace(out[i].Name))
		if id != "fast" && (id != "priority" || name != "fast") {
			continue
		}
		out[i].ID = "fast"
		out[i].Request.ServiceTier = "fast"
	}
	return llm.NormalizeServiceTiers(out)
}

// NormalizeCodexFastServiceTiers canonicalizes the Codex Fast mode while
// preserving the wire value expected by the ChatGPT Codex backend. The Codex
// catalog advertises the mode as priority with display name Fast; harness
// exposes it as :fast but must send service_tier priority.
func NormalizeCodexFastServiceTiers(tiers []llm.ServiceTier) []llm.ServiceTier {
	out := append([]llm.ServiceTier(nil), tiers...)
	for i := range out {
		id := strings.ToLower(strings.TrimSpace(out[i].ID))
		name := strings.ToLower(strings.TrimSpace(out[i].Name))
		if id != "fast" && (id != "priority" || name != "fast") {
			continue
		}
		out[i].ID = "fast"
		out[i].Request.ServiceTier = "priority"
	}
	return llm.NormalizeServiceTiers(out)
}
