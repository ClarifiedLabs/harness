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

// CatalogResult is a catalog-facing flat price. Handled=true with Known=false
// means the model has dynamic tiered pricing and cannot be represented by a
// single flat catalog price.
type CatalogResult struct {
	Price   llm.Price
	Known   bool
	Handled bool
}

// Pricer prices model-proxy usage and exposes flat catalog prices when a model
// has one.
type Pricer interface {
	CatalogPrice(provider llm.ProviderConfig, model llm.ModelEntry) CatalogResult
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

// CatalogPrice returns a flat per-million-token catalog price when one can be
// shown accurately.
func (c Composite) CatalogPrice(provider llm.ProviderConfig, model llm.ModelEntry) CatalogResult {
	for _, p := range c.pricers {
		if res := p.CatalogPrice(provider, model); res.Handled {
			return res
		}
	}
	return c.flat.CatalogPrice(provider, model)
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

func (Flat) CatalogPrice(_ llm.ProviderConfig, model llm.ModelEntry) CatalogResult {
	if model.Price.IsZero() {
		return CatalogResult{}
	}
	if model.Price.HasTiers() {
		// Tiered models do not have a single accurate flat catalog price.
		return CatalogResult{Handled: true}
	}
	return CatalogResult{Price: model.Price, Known: true, Handled: true}
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
