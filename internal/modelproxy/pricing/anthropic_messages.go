package pricing

import (
	"strings"

	"harness/internal/llm"
)

// AnthropicMessages prices the disjoint reasoning and prompt-cache TTL buckets
// normalized by the Anthropic Messages dialect.
type AnthropicMessages struct{}

func (AnthropicMessages) CatalogPricing(provider llm.ProviderConfig, model llm.ModelEntry) CatalogPricingResult {
	if !isAnthropicMessages(provider) {
		return CatalogPricingResult{}
	}
	model.Price = anthropicPrice(model.Price)
	return Flat{}.CatalogPricing(provider, model)
}

func (AnthropicMessages) PriceUsage(in Input) Result {
	if !isAnthropicMessages(in.Provider) {
		return Result{}
	}
	// A long-TTL request can place stable system/tool anchors in the 1-hour
	// bucket. If an Anthropic-compatible endpoint reports cache writes without
	// the TTL breakdown, do not silently price them at the cheaper 5-minute rate.
	if in.Request.LongCacheTTL &&
		in.Usage.CacheWriteTokens+in.Usage.CacheWrite1hTokens > 0 &&
		!in.Usage.CacheWriteTTLKnown {
		return Result{Handled: true}
	}
	in.Provider.ServiceTiers = anthropicServiceTiers(in.Provider.ServiceTiers)
	in.Model.Price = anthropicPrice(in.Model.Price)
	in.Model.ServiceTiers = anthropicServiceTiers(in.Model.ServiceTiers)
	return Flat{}.PriceUsage(in)
}

func isAnthropicMessages(provider llm.ProviderConfig) bool {
	return strings.EqualFold(strings.TrimSpace(provider.APIType), "anthropic")
}

func anthropicPrice(price llm.Price) llm.Price {
	price = reasoningAtOutputPrice(price)
	if price.CacheWrite1h == 0 {
		price.CacheWrite1h = 2 * price.Input
	}
	if len(price.Tiers) == 0 {
		return price
	}
	price.Tiers = append([]llm.PriceTier(nil), price.Tiers...)
	for i := range price.Tiers {
		if price.Tiers[i].CacheWrite1h == 0 {
			price.Tiers[i].CacheWrite1h = 2 * price.Tiers[i].Input
		}
	}
	return price
}

func anthropicServiceTiers(tiers []llm.ServiceTier) []llm.ServiceTier {
	if len(tiers) == 0 {
		return tiers
	}
	out := append([]llm.ServiceTier(nil), tiers...)
	for i := range out {
		out[i].Price = anthropicPrice(out[i].Price)
	}
	return out
}
