// Package pricing calculates model-proxy request costs. Flat per-token pricing
// remains the default, while tiered pricing (context-length price bands from
// models.dev) is applied generically when present.
package pricing

import (
	"strings"

	"harness/internal/llm"
)

// Input describes one usage snapshot to price.
type Input struct {
	TargetID string
	Provider llm.ProviderConfig
	Model    llm.ModelEntry
	Request  llm.Request
	Usage    llm.Usage
}

// Result is a request cost in USD. Known=false means the pricer cannot price
// this usage accurately.
type Result struct {
	CostUSD float64
	Known   bool
	Handled bool
}

// CatalogPricingResult is a catalog-facing static price schedule. Known=false
// means no static schedule can represent the model's pricing, for example when
// a provider chooses rates dynamically after routing the request.
type CatalogPricingResult struct {
	Price   llm.Price
	Known   bool
	Handled bool
}

// Pricer prices model-proxy usage and exposes static catalog pricing schedules
// when a model has one.
type Pricer interface {
	CatalogPricing(provider llm.ProviderConfig, model llm.ModelEntry) CatalogPricingResult
	PriceUsage(Input) Result
}

// Composite tries provider-specific pricers in order, then falls back to flat
// llm.Price values.
type Composite struct {
	pricers []Pricer
	flat    Flat
}

// NewComposite returns the default pricing chain.
func NewComposite() Composite {
	return Composite{flat: Flat{}}
}

// CatalogPricing returns a static per-million-token pricing schedule when one
// can be shown accurately, including context-length tiers.
func (c Composite) CatalogPricing(provider llm.ProviderConfig, model llm.ModelEntry) CatalogPricingResult {
	for _, p := range c.pricers {
		if res := p.CatalogPricing(provider, model); res.Handled {
			return res
		}
	}
	return c.flat.CatalogPricing(provider, model)
}

// PriceUsage returns a request cost when any pricer can calculate one.
func (c Composite) PriceUsage(in Input) Result {
	for _, p := range c.pricers {
		if res := p.PriceUsage(in); res.Handled {
			return res
		}
	}
	return c.flat.PriceUsage(in)
}

// Flat prices the existing llm.Price shape, including tiered prices.
type Flat struct{}

func (Flat) CatalogPricing(_ llm.ProviderConfig, model llm.ModelEntry) CatalogPricingResult {
	if model.Price.IsZero() {
		return CatalogPricingResult{}
	}
	return CatalogPricingResult{Price: model.Price, Known: true, Handled: true}
}

func (Flat) PriceUsage(in Input) Result {
	basePrice := in.Model.Price
	tiers := llm.ModelServiceTiers(in.Provider, in.Model)
	servedTier := strings.ToLower(strings.TrimSpace(in.Usage.ServiceTier))
	servedSpeed := strings.ToLower(strings.TrimSpace(in.Usage.Speed))
	if servedTier != "" || servedSpeed != "" {
		if standardServiceMode(servedTier) && standardServiceMode(servedSpeed) {
			return priceWith(basePrice, in)
		}
		if tier, ok := llm.MatchServiceTierRequest(tiers, servedTier, servedSpeed); ok {
			return priceModeWith(tier.Price, in)
		}
		return Result{Handled: true}
	}
	requestedTier := strings.ToLower(strings.TrimSpace(in.Request.ServiceTier))
	requestedSpeed := strings.ToLower(strings.TrimSpace(in.Request.Speed))
	if tier, ok := llm.MatchServiceTierRequest(tiers, requestedTier, requestedSpeed); ok {
		return priceModeWith(tier.Price, in)
	}
	if !standardServiceMode(requestedTier) || !standardServiceMode(requestedSpeed) {
		return Result{Handled: true}
	}
	return priceWith(basePrice, in)
}

func priceWith(price llm.Price, in Input) Result {
	if price.IsZero() {
		return Result{}
	}
	usd, known := price.Cost(in.Usage, in.Request.EstimatedInputTokens)
	if !known {
		return Result{Handled: true}
	}
	return Result{CostUSD: usd, Known: true, Handled: true}
}

func priceModeWith(price llm.Price, in Input) Result {
	if price.IsZero() {
		return Result{Handled: true}
	}
	return priceWith(price, in)
}

func standardServiceMode(value string) bool {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "", "default", "standard", "standard_only":
		return true
	default:
		return false
	}
}

// PriceZero reports whether a flat price has no configured components.
// Deprecated: use llm.Price.IsZero.
func PriceZero(p llm.Price) bool {
	return p.IsZero()
}
