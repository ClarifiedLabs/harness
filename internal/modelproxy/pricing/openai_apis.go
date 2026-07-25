package pricing

import (
	"strings"

	"harness/internal/llm"
)

// OpenAIAPIs prices reasoning-token details reported by the Responses and Chat
// Completions wire contracts. Both APIs include reasoning in their aggregate
// output/completion count; the dialects normalize that aggregate into disjoint
// output and reasoning buckets. When a catalog does not publish a separate
// reasoning rate, those reasoning tokens retain the output-token rate.
type OpenAIAPIs struct{}

func (OpenAIAPIs) CatalogPricing(provider llm.ProviderConfig, model llm.ModelEntry) CatalogPricingResult {
	if !isOpenAIAPI(provider) {
		return CatalogPricingResult{}
	}
	model.Price = reasoningAtOutputPrice(model.Price)
	return Flat{}.CatalogPricing(provider, model)
}

func (OpenAIAPIs) PriceUsage(in Input) Result {
	if !isOpenAIAPI(in.Provider) {
		return Result{}
	}
	in.Provider.ServiceTiers = reasoningAtOutputServiceTiers(in.Provider.ServiceTiers)
	in.Model.Price = reasoningAtOutputPrice(in.Model.Price)
	in.Model.ServiceTiers = reasoningAtOutputServiceTiers(in.Model.ServiceTiers)
	return Flat{}.PriceUsage(in)
}

func isOpenAIAPI(provider llm.ProviderConfig) bool {
	switch strings.ToLower(strings.TrimSpace(provider.APIType)) {
	case "openai", "responses":
		return true
	default:
		return false
	}
}

func reasoningAtOutputPrice(price llm.Price) llm.Price {
	if price.Reasoning == 0 {
		price.Reasoning = price.Output
	}
	if len(price.Tiers) == 0 {
		return price
	}
	price.Tiers = append([]llm.PriceTier(nil), price.Tiers...)
	for i := range price.Tiers {
		if price.Tiers[i].Reasoning == 0 {
			price.Tiers[i].Reasoning = price.Tiers[i].Output
		}
	}
	return price
}

func reasoningAtOutputServiceTiers(tiers []llm.ServiceTier) []llm.ServiceTier {
	if len(tiers) == 0 {
		return tiers
	}
	out := append([]llm.ServiceTier(nil), tiers...)
	for i := range out {
		out[i].Price = reasoningAtOutputPrice(out[i].Price)
	}
	return out
}
