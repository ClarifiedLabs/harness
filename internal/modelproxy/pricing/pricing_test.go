package pricing

import (
	"math"
	"testing"

	"harness/internal/llm"
)

func TestFlatPricer(t *testing.T) {
	provider := llm.ProviderConfig{Name: "testai"}
	model := llm.ModelEntry{
		Name:  "alpha",
		Price: llm.Price{Input: 2, Output: 4, CacheRead: 0.5, CacheWrite: 1},
	}
	price := Flat{}.CatalogPrice(provider, model)
	if !price.Handled || !price.Known || !price.Price.Equal(model.Price) {
		t.Fatalf("CatalogPrice = %+v; want configured price", price)
	}

	got := Flat{}.PriceUsage(Input{
		Provider: provider,
		Model:    model,
		Usage: llm.Usage{
			InputTokens:      1_000_000,
			OutputTokens:     1_000_000,
			CacheReadTokens:  1_000_000,
			CacheWriteTokens: 1_000_000,
		},
	})
	if !got.Known {
		t.Fatal("flat price should be known")
	}
	if !got.Handled {
		t.Fatal("flat price handled = false, want true")
	}
	if want := 7.5; math.Abs(got.CostUSD-want) > 1e-12 {
		t.Fatalf("flat cost = %v, want %v", got.CostUSD, want)
	}
}

func TestFlatPricerTieredPricing(t *testing.T) {
	pricer := NewComposite()
	provider := llm.ProviderConfig{Name: "sakana"}
	model := llm.ModelEntry{
		Name: "fugu-ultra",
		Price: llm.Price{
			Input:     5,
			Output:    30,
			CacheRead: 0.5,
			Tiers: []llm.PriceTier{
				{Threshold: 272_000, Input: 10, Output: 45, CacheRead: 1.0},
			},
		},
	}
	usage := llm.Usage{InputTokens: 1000, OutputTokens: 2000, CacheReadTokens: 300}

	// Below the tier threshold uses base prices.
	got := pricer.PriceUsage(Input{
		Provider: provider,
		Model:    model,
		Request:  llm.Request{EstimatedInputTokens: 1000},
		Usage:    usage,
	})
	assertKnownCost(t, got, 1000.0/1e6*5+2000.0/1e6*30+300.0/1e6*0.5)

	// Estimated input above the threshold uses the upper tier.
	got = pricer.PriceUsage(Input{
		Provider: provider,
		Model:    model,
		Request:  llm.Request{EstimatedInputTokens: 272_001},
		Usage:    usage,
	})
	assertKnownCost(t, got, 1000.0/1e6*10+2000.0/1e6*45+300.0/1e6*1.0)

	// Dated model with the same tiered price.
	model.Name = "fugu-ultra-20260615"
	got = pricer.PriceUsage(Input{
		Provider: provider,
		Model:    model,
		Request:  llm.Request{EstimatedInputTokens: 1000},
		Usage:    usage,
	})
	assertKnownCost(t, got, 1000.0/1e6*5+2000.0/1e6*30+300.0/1e6*0.5)
}

func TestFlatPricerTieredCatalogPriceOmitted(t *testing.T) {
	model := llm.ModelEntry{
		Name: "tiered-model",
		Price: llm.Price{
			Input: 5,
			Tiers: []llm.PriceTier{{Threshold: 100_000, Input: 10}},
		},
	}
	price := NewComposite().CatalogPrice(llm.ProviderConfig{Name: "testai"}, model)
	if !price.Handled || price.Known {
		t.Fatalf("tiered catalog price = %+v, want handled but unknown", price)
	}
}

func TestFlatPricerZeroPriceUnknown(t *testing.T) {
	// A model with no configured price (e.g. Sakana's routed fugu) cannot be costed.
	got := NewComposite().PriceUsage(Input{
		Provider: llm.ProviderConfig{Name: "sakana"},
		Model:    llm.ModelEntry{Name: "fugu"},
		Usage:    llm.Usage{InputTokens: 1000, OutputTokens: 2000},
	})
	if got.Handled || got.Known {
		t.Fatalf("zero price cost = %+v, want unknown", got)
	}
}

func assertKnownCost(t *testing.T, got Result, want float64) {
	t.Helper()
	if !got.Known {
		t.Fatal("cost known = false, want true")
	}
	if !got.Handled {
		t.Fatal("cost handled = false, want true")
	}
	if math.Abs(got.CostUSD-want) > 1e-12 {
		t.Fatalf("cost = %v, want %v", got.CostUSD, want)
	}
}
