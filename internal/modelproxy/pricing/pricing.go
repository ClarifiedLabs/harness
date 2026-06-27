// Package pricing calculates model-proxy request costs. Flat per-token pricing
// remains the default, while provider-specific implementations can price models
// whose billing cannot be represented by llm.Price alone.
package pricing

import (
	"strings"

	"harness/internal/llm"
)

const (
	perMillion = 1_000_000.0

	SakanaProviderID = "sakana"

	sakanaFuguUltraModelID      = "fugu-ultra"
	sakanaFuguUltraDatedModelID = "fugu-ultra-20260615"
	sakanaContextTierThreshold  = 272_000
)

var (
	sakanaFuguUltraStandard = llm.Price{Input: 5, Output: 30, CacheRead: 0.50}
	sakanaFuguUltraLong     = llm.Price{Input: 10, Output: 45, CacheRead: 1.00}
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
// means a provider-specific pricer owns the model, but cannot show a single
// flat catalog price for it.
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
	return Composite{pricers: []Pricer{Sakana{}}, flat: Flat{}}
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

// Flat prices the existing llm.Price shape.
type Flat struct{}

func (Flat) CatalogPrice(_ llm.ProviderConfig, model llm.ModelEntry) CatalogResult {
	if PriceZero(model.Price) {
		return CatalogResult{}
	}
	return CatalogResult{Price: model.Price, Known: true, Handled: true}
}

func (Flat) PriceUsage(in Input) Result {
	if PriceZero(in.Model.Price) {
		return Result{}
	}
	return Result{CostUSD: cost(in.Model.Price, in.Usage), Known: true, Handled: true}
}

// Sakana prices Sakana-specific dynamic billing. The default fugu router cannot
// be priced accurately without response billing metadata, so only Fugu Ultra is
// priced here.
type Sakana struct{}

func (Sakana) CatalogPrice(provider llm.ProviderConfig, model llm.ModelEntry) CatalogResult {
	if isSakanaRouted(provider, model) {
		return CatalogResult{Handled: true}
	}
	if isSakanaFuguUltra(provider, model) {
		return CatalogResult{Handled: true}
	}
	return CatalogResult{}
}

func (Sakana) PriceUsage(in Input) Result {
	if isSakanaRouted(in.Provider, in.Model) {
		return Result{Handled: true}
	}
	if !isSakanaFuguUltra(in.Provider, in.Model) {
		return Result{}
	}
	price := sakanaFuguUltraStandard
	if sakanaContextTokens(in) > sakanaContextTierThreshold {
		price = sakanaFuguUltraLong
	}
	return Result{CostUSD: cost(price, in.Usage), Known: true, Handled: true}
}

func isSakanaProvider(provider llm.ProviderConfig) bool {
	return strings.EqualFold(strings.TrimSpace(provider.Name), SakanaProviderID)
}

func isSakanaRouted(provider llm.ProviderConfig, model llm.ModelEntry) bool {
	return isSakanaProvider(provider) && strings.EqualFold(strings.TrimSpace(model.Name), "fugu")
}

func isSakanaFuguUltra(provider llm.ProviderConfig, model llm.ModelEntry) bool {
	if !isSakanaProvider(provider) {
		return false
	}
	switch strings.ToLower(strings.TrimSpace(model.Name)) {
	case sakanaFuguUltraModelID, sakanaFuguUltraDatedModelID:
		return true
	default:
		return false
	}
}

func sakanaContextTokens(in Input) int {
	fromUsage := in.Usage.InputTokens + in.Usage.CacheReadTokens + in.Usage.CacheWriteTokens
	if in.Request.EstimatedInputTokens > fromUsage {
		return in.Request.EstimatedInputTokens
	}
	return fromUsage
}

func cost(price llm.Price, usage llm.Usage) float64 {
	return float64(usage.InputTokens)/perMillion*price.Input +
		float64(usage.OutputTokens)/perMillion*price.Output +
		float64(usage.CacheReadTokens)/perMillion*price.CacheRead +
		float64(usage.CacheWriteTokens)/perMillion*price.CacheWrite
}

// PriceZero reports whether a flat price has no configured components.
func PriceZero(p llm.Price) bool {
	return p.Input == 0 && p.Output == 0 && p.CacheRead == 0 && p.CacheWrite == 0
}
