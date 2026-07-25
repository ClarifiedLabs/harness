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
	price := Flat{}.CatalogPricing(provider, model)
	if !price.Handled || !price.Known || !price.Price.Equal(model.Price) {
		t.Fatalf("CatalogPricing = %+v; want configured price", price)
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

func TestFlatPricerLeavesNonstandardServiceTiersUnpriced(t *testing.T) {
	model := llm.ModelEntry{Name: "alpha", Price: llm.Price{Input: 2, Output: 4}}
	for _, tier := range []string{"auto", "flex", "priority"} {
		got := Flat{}.PriceUsage(Input{
			Model:   model,
			Request: llm.Request{ServiceTier: tier},
			Usage:   llm.Usage{InputTokens: 1000, OutputTokens: 1000},
		})
		if !got.Handled || got.Known {
			t.Fatalf("%s price = %+v, want handled but unknown", tier, got)
		}
	}
	got := Flat{}.PriceUsage(Input{
		Model:   model,
		Request: llm.Request{ServiceTier: "default"},
		Usage:   llm.Usage{InputTokens: 1000, OutputTokens: 1000},
	})
	if !got.Handled || !got.Known {
		t.Fatalf("default price = %+v, want known", got)
	}
}

func TestFlatPricerUsesCatalogModePrice(t *testing.T) {
	model := llm.ModelEntry{
		Name:  "alpha",
		Price: llm.Price{Input: 2, Output: 4},
		ServiceTiers: []llm.ServiceTier{{
			ID:      "priority",
			Aliases: []string{"fast"},
			Request: llm.ServiceTierRequest{ServiceTier: "priority"},
			Price:   llm.Price{Input: 4, Output: 8},
		}},
	}
	in := Input{
		Model:   model,
		Request: llm.Request{ServiceTier: "priority"},
		Usage:   llm.Usage{InputTokens: 1_000_000, OutputTokens: 1_000_000},
	}
	assertKnownCost(t, Flat{}.PriceUsage(in), 12)

	// A provider can accept priority but gracefully serve standard. Response
	// metadata wins over the requested mode so accounting uses the base rate.
	in.Usage.ServiceTier = "default"
	assertKnownCost(t, Flat{}.PriceUsage(in), 6)
}

func TestFlatPricerUsesCatalogSpeedPrice(t *testing.T) {
	model := llm.ModelEntry{
		Name:  "claude",
		Price: llm.Price{Input: 5, Output: 25},
		ServiceTiers: []llm.ServiceTier{{
			ID:      "fast",
			Request: llm.ServiceTierRequest{Speed: "fast"},
			Price:   llm.Price{Input: 30, Output: 150},
		}},
	}
	assertKnownCost(t, Flat{}.PriceUsage(Input{
		Model:   model,
		Request: llm.Request{Speed: "fast"},
		Usage:   llm.Usage{InputTokens: 1_000_000, OutputTokens: 1_000_000, Speed: "fast"},
	}), 180)
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

func TestFlatPricerExposesTieredCatalogPricing(t *testing.T) {
	model := llm.ModelEntry{
		Name: "tiered-model",
		Price: llm.Price{
			Input: 5,
			Tiers: []llm.PriceTier{{Threshold: 100_000, Input: 10}},
		},
	}
	price := NewComposite().CatalogPricing(llm.ProviderConfig{Name: "testai"}, model)
	if !price.Handled || !price.Known || !price.Price.Equal(model.Price) {
		t.Fatalf("tiered catalog pricing = %+v, want complete static schedule", price)
	}
}

func TestGoogleInteractionsPricesThoughtTokensAtOutputRate(t *testing.T) {
	pricer := NewComposite()
	provider := llm.ProviderConfig{Name: "google", APIType: "interactions"}
	model := llm.ModelEntry{
		Name:  "gemini-3.6-flash",
		Price: llm.Price{Input: 1.5, Output: 7.5, CacheRead: 0.15},
	}
	got := pricer.PriceUsage(Input{
		Provider: provider,
		Model:    model,
		Usage: llm.Usage{
			InputTokens:     1_000_000,
			CacheReadTokens: 1_000_000,
			OutputTokens:    1_000_000,
			ReasoningTokens: 1_000_000,
		},
	})
	assertKnownCost(t, got, 16.65)

	catalog := pricer.CatalogPricing(provider, model)
	if !catalog.Handled || !catalog.Known || catalog.Price.Reasoning != model.Price.Output {
		t.Fatalf("catalog pricing = %+v, want reasoning rate %v", catalog, model.Price.Output)
	}
}

func TestGoogleInteractionsPreservesExplicitReasoningRateAndPriceTiers(t *testing.T) {
	pricer := NewComposite()
	provider := llm.ProviderConfig{Name: "google", APIType: "interactions"}
	model := llm.ModelEntry{
		Name: "gemini-tiered",
		Price: llm.Price{
			Input:     1,
			Output:    4,
			Reasoning: 3,
			Tiers: []llm.PriceTier{{
				Threshold: 100_000,
				Input:     2,
				Output:    8,
			}},
		},
	}
	got := pricer.PriceUsage(Input{
		Provider: provider,
		Model:    model,
		Request:  llm.Request{EstimatedInputTokens: 100_001},
		Usage: llm.Usage{
			InputTokens:     1_000_000,
			OutputTokens:    1_000_000,
			ReasoningTokens: 1_000_000,
		},
	})
	assertKnownCost(t, got, 18)

	catalog := pricer.CatalogPricing(provider, model)
	if catalog.Price.Reasoning != 3 || len(catalog.Price.Tiers) != 1 || catalog.Price.Tiers[0].Reasoning != 8 {
		t.Fatalf("catalog pricing = %+v, want explicit base reasoning and inherited tier reasoning", catalog.Price)
	}
}

func TestReasoningRateFallbackIsScopedToGoogleInteractions(t *testing.T) {
	pricer := NewComposite()
	model := llm.ModelEntry{Name: "model", Price: llm.Price{Input: 1, Output: 4}}
	usage := llm.Usage{
		InputTokens:     1_000_000,
		OutputTokens:    1_000_000,
		ReasoningTokens: 1_000_000,
	}
	for _, provider := range []llm.ProviderConfig{
		{Name: "google", APIType: "openai"},
		{Name: "compatible", APIType: "interactions"},
	} {
		got := pricer.PriceUsage(Input{Provider: provider, Model: model, Usage: usage})
		assertKnownCost(t, got, 5)
	}
}

func TestGoogleInteractionsPricesServiceTierThoughtTokens(t *testing.T) {
	pricer := NewComposite()
	provider := llm.ProviderConfig{
		Name:    "google",
		APIType: "interactions",
		ServiceTiers: []llm.ServiceTier{{
			ID:      "priority",
			Request: llm.ServiceTierRequest{ServiceTier: "priority"},
			Price:   llm.Price{Input: 2, Output: 10},
		}},
	}
	got := pricer.PriceUsage(Input{
		Provider: provider,
		Model:    llm.ModelEntry{Name: "gemini", Price: llm.Price{Input: 1, Output: 4}},
		Request:  llm.Request{ServiceTier: "priority"},
		Usage: llm.Usage{
			InputTokens:     1_000_000,
			OutputTokens:    1_000_000,
			ReasoningTokens: 1_000_000,
		},
	})
	assertKnownCost(t, got, 22)
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
