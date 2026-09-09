package llm

import "testing"

func TestAddUsageSumsEveryBucketWithoutPricingMetadata(t *testing.T) {
	a := Usage{InputTokens: 1, OutputTokens: 2, CacheReadTokens: 3, CacheWriteTokens: 4, CacheWrite1hTokens: 5, ReasoningTokens: 6, CostUSD: .25, CostKnown: true, CacheWriteTTLKnown: true, ServiceTier: "priority", Speed: "fast"}
	b := Usage{InputTokens: 10, OutputTokens: 20, CacheReadTokens: 30, CacheWriteTokens: 40, CacheWrite1hTokens: 50, ReasoningTokens: 60, CostUSD: .5, CostKnown: true, ServiceTier: "standard"}
	want := Usage{InputTokens: 11, OutputTokens: 22, CacheReadTokens: 33, CacheWriteTokens: 44, CacheWrite1hTokens: 55, ReasoningTokens: 66, CostUSD: .75, CostKnown: true}
	for _, pair := range [][2]Usage{{a, b}, {b, a}} {
		if got := AddUsage(pair[0], pair[1]); got != want {
			t.Fatalf("AddUsage(%+v, %+v) = %+v, want %+v", pair[0], pair[1], got, want)
		}
	}
}

func TestAddUsageCostKnown(t *testing.T) {
	priced := Usage{InputTokens: 10, CostUSD: .25, CostKnown: true}
	tests := []struct {
		name      string
		a, b      Usage
		wantKnown bool
		wantCost  float64
	}{
		{name: "empty"},
		{name: "empty and priced", b: priced, wantKnown: true, wantCost: .25},
		{name: "empty and known zero", b: Usage{CostKnown: true}, wantKnown: true},
		{name: "both priced", a: priced, b: priced, wantKnown: true, wantCost: .5},
		{name: "priced and known zero", a: priced, b: Usage{OutputTokens: 2, CostKnown: true}, wantKnown: true, wantCost: .25},
		{name: "both unknown", a: Usage{InputTokens: 1}, b: Usage{OutputTokens: 2}},
		{name: "empty and unknown", b: Usage{InputTokens: 1}},
		{name: "unknown nonzero cost", a: priced, b: Usage{OutputTokens: 2, CostUSD: .5}, wantCost: .75},
		{name: "cost alone is not known", a: Usage{CostUSD: .5}, wantCost: .5},
		{name: "cost without tokens does not poison", a: priced, b: Usage{CostUSD: .5}, wantKnown: true, wantCost: .75},
		{name: "metadata does not poison", a: priced, b: Usage{CacheWriteTTLKnown: true, ServiceTier: "priority", Speed: "fast"}, wantKnown: true, wantCost: .25},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			for _, pair := range [][2]Usage{{tt.a, tt.b}, {tt.b, tt.a}} {
				got := AddUsage(pair[0], pair[1])
				if got.CostKnown != tt.wantKnown || got.CostUSD != tt.wantCost {
					t.Fatalf("AddUsage(%+v, %+v) = %+v, want cost %v known %t", pair[0], pair[1], got, tt.wantCost, tt.wantKnown)
				}
			}
		})
	}
}

func TestAddUsageUnknownCostPoisonsEveryTokenBucket(t *testing.T) {
	priced := Usage{InputTokens: 10, CostUSD: .25, CostKnown: true}
	tests := []struct {
		name  string
		usage Usage
	}{
		{"input", Usage{InputTokens: 1}},
		{"output", Usage{OutputTokens: 1}},
		{"cache read", Usage{CacheReadTokens: 1}},
		{"cache write", Usage{CacheWriteTokens: 1}},
		{"cache write 1h", Usage{CacheWrite1hTokens: 1}},
		{"reasoning", Usage{ReasoningTokens: 1}},
		{"negative is nonzero", Usage{ReasoningTokens: -1}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			for _, known := range []Usage{priced, {CostKnown: true}} {
				for _, pair := range [][2]Usage{{known, tt.usage}, {tt.usage, known}} {
					got := AddUsage(pair[0], pair[1])
					if got.CostKnown || got.CostUSD != known.CostUSD {
						t.Fatalf("AddUsage(%+v, %+v) = %+v", pair[0], pair[1], got)
					}
					if AddUsage(got, priced).CostKnown || AddUsage(priced, got).CostKnown {
						t.Fatal("later priced usage erased unknown cost")
					}
				}
			}
		})
	}
}
