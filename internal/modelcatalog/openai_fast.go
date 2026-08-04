package modelcatalog

import (
	"strings"

	"harness/internal/llm"
)

// NormalizeOpenAIFastServiceTiers canonicalizes OpenAI's renamed Fast mode.
// Older OpenAI and Codex catalog records call the same mode priority with a
// Fast display name, but current requests must use service_tier fast.
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
