// Package pricing calculates model-proxy request costs. Flat per-token pricing
// remains the default, while tiered pricing (context-length price bands from
// models.dev) is applied generically when present.
package pricing

import (
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
	if in.Model.Price.IsZero() {
		return Result{}
	}
	usd, known := in.Model.Price.Cost(in.Usage, in.Request.EstimatedInputTokens)
	if !known {
		return Result{}
	}
	return Result{CostUSD: usd, Known: true, Handled: true}
}

// PriceZero reports whether a flat price has no configured components.
// Deprecated: use llm.Price.IsZero.
func PriceZero(p llm.Price) bool {
	return p.IsZero()
}
