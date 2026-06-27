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
	if !price.Handled || !price.Known || price.Price != model.Price {
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

func TestCompositeSakanaFuguUltraPricingTiers(t *testing.T) {
	pricer := NewComposite()
	provider := llm.ProviderConfig{Name: SakanaProviderID}
	model := llm.ModelEntry{Name: "fugu-ultra"}
	usage := llm.Usage{InputTokens: 1000, OutputTokens: 2000, CacheReadTokens: 300}

	got := pricer.PriceUsage(Input{
		Provider: provider,
		Model:    model,
		Request:  llm.Request{EstimatedInputTokens: 1000},
		Usage:    usage,
	})
	assertKnownCost(t, got, 1000.0/1e6*5+2000.0/1e6*30+300.0/1e6*0.5)

	got = pricer.PriceUsage(Input{
		Provider: provider,
		Model:    model,
		Request:  llm.Request{EstimatedInputTokens: sakanaContextTierThreshold + 1},
		Usage:    usage,
	})
	assertKnownCost(t, got, 1000.0/1e6*10+2000.0/1e6*45+300.0/1e6*1)

	got = pricer.PriceUsage(Input{
		Provider: provider,
		Model:    llm.ModelEntry{Name: "fugu-ultra-20260615"},
		Request:  llm.Request{EstimatedInputTokens: 1000},
		Usage:    usage,
	})
	assertKnownCost(t, got, 1000.0/1e6*5+2000.0/1e6*30+300.0/1e6*0.5)
}

func TestCompositeSakanaFuguRouterUnknown(t *testing.T) {
	got := NewComposite().PriceUsage(Input{
		Provider: llm.ProviderConfig{Name: SakanaProviderID},
		Model:    llm.ModelEntry{Name: "fugu", Price: llm.Price{Input: 99, Output: 99}},
		Usage:    llm.Usage{InputTokens: 1000, OutputTokens: 2000},
	})
	if !got.Handled || got.Known {
		t.Fatalf("fugu cost = %+v, want handled unknown route-dependent price", got)
	}

	price := NewComposite().CatalogPrice(llm.ProviderConfig{Name: SakanaProviderID}, llm.ModelEntry{Name: "fugu-ultra", Price: llm.Price{Input: 99}})
	if !price.Handled || price.Known {
		t.Fatalf("sakana dynamic catalog price = %+v, want handled unknown", price)
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
