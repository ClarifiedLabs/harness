package llm

import (
	"slices"
	"testing"
)

func TestNormalizeServiceTiers(t *testing.T) {
	tiers := NormalizeServiceTiers([]ServiceTier{
		{ID: " PRIORITY ", Name: "Fast", Aliases: []string{"fast", "FAST"}, Request: ServiceTierRequest{ServiceTier: " PRIORITY "}},
		{ID: "flex"},
		{ID: "priority"},
	})
	if len(tiers) != 2 {
		t.Fatalf("tiers = %+v, want two", tiers)
	}
	if tiers[0].ID != "priority" || tiers[0].Request.ServiceTier != "priority" || !slices.Equal(tiers[0].Aliases, []string{"fast"}) {
		t.Fatalf("priority tier = %+v", tiers[0])
	}
	if tiers[1].Request.ServiceTier != "flex" {
		t.Fatalf("implicit request mapping = %+v, want flex", tiers[1].Request)
	}
}

func TestResolveServiceTierAliasAndModelOverride(t *testing.T) {
	pc := ProviderConfig{ServiceTiers: []ServiceTier{{ID: "flex"}}}
	model := ModelEntry{ServiceTiers: []ServiceTier{{ID: "priority", Name: "Fast", Aliases: []string{"fast"}}}}
	tiers := ModelServiceTiers(pc, model)
	got, ok := ResolveServiceTier("FAST", tiers)
	if !ok || got.ID != "priority" {
		t.Fatalf("ResolveServiceTier(fast) = %+v, %v", got, ok)
	}
	if _, ok := ResolveServiceTier("flex", tiers); ok {
		t.Fatal("provider tier should be replaced by model override")
	}
}

func TestMatchServiceTierRequest(t *testing.T) {
	tiers := []ServiceTier{
		{ID: "priority", Request: ServiceTierRequest{ServiceTier: "priority"}},
		{ID: "fast", Request: ServiceTierRequest{Speed: "fast", Betas: []string{"fast-mode-2026-02-01"}}},
	}
	if got, ok := MatchServiceTierRequest(tiers, "priority", ""); !ok || got.ID != "priority" {
		t.Fatalf("priority match = %+v, %v", got, ok)
	}
	if got, ok := MatchServiceTierRequest(tiers, "", "fast"); !ok || got.ID != "fast" {
		t.Fatalf("fast match = %+v, %v", got, ok)
	}
}
