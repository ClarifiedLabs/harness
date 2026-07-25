package pricing

import (
	"net/url"
	"strings"

	"harness/internal/llm"
)

// GoogleInteractions accounts for Gemini thought tokens. The Interactions API
// reports them separately from output tokens, while Google bills them at the
// output-token rate. models.dev currently supplies the base Google rates but
// leaves the reasoning component unset. The billing semantics are documented at
// https://ai.google.dev/gemini-api/docs/pricing.
type GoogleInteractions struct{}

func (GoogleInteractions) CatalogPricing(provider llm.ProviderConfig, model llm.ModelEntry) CatalogPricingResult {
	if !isGoogleInteractions(provider) {
		return CatalogPricingResult{}
	}
	model.Price = googleInteractionsPrice(model.Price)
	return Flat{}.CatalogPricing(provider, model)
}

func (GoogleInteractions) PriceUsage(in Input) Result {
	if !isGoogleInteractions(in.Provider) {
		return Result{}
	}
	in.Provider.ServiceTiers = googleInteractionsServiceTiers(in.Provider.ServiceTiers)
	in.Model.Price = googleInteractionsPrice(in.Model.Price)
	in.Model.ServiceTiers = googleInteractionsServiceTiers(in.Model.ServiceTiers)
	return Flat{}.PriceUsage(in)
}

func isGoogleInteractions(provider llm.ProviderConfig) bool {
	if !strings.EqualFold(strings.TrimSpace(provider.APIType), "interactions") {
		return false
	}
	if strings.EqualFold(strings.TrimSpace(provider.Name), "google") ||
		(provider.Managed && strings.EqualFold(strings.TrimSpace(provider.PriceSource), "google")) {
		return true
	}
	baseURL, err := url.Parse(strings.TrimSpace(provider.BaseURL))
	return err == nil && strings.EqualFold(baseURL.Hostname(), "generativelanguage.googleapis.com")
}

func googleInteractionsPrice(price llm.Price) llm.Price {
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

func googleInteractionsServiceTiers(tiers []llm.ServiceTier) []llm.ServiceTier {
	if len(tiers) == 0 {
		return tiers
	}
	out := append([]llm.ServiceTier(nil), tiers...)
	for i := range out {
		out[i].Price = googleInteractionsPrice(out[i].Price)
	}
	return out
}
