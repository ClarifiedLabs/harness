package runtime

import (
	"testing"

	"harness/internal/llm"
	"harness/internal/modelcatalog"
	"harness/internal/modelproxy/modeldiscovery"
)

func TestProviderCatalogCopyOwnsNestedMetadata(t *testing.T) {
	enabled := true
	window, min := 100, 1
	shape, description := "openai", "original"
	price := llm.Price{Input: 2, Tiers: []llm.PriceTier{{Threshold: 10, Input: 3}}}
	m := modeldiscovery.Model{ContextWindow: &window, OutputLimit: &window, InputModalities: []string{"text"}, Reasoning: &enabled, ReasoningSummarySupported: &enabled, ReasoningOptions: []llm.ReasoningOption{{Type: "effort", Values: []string{"high"}, Min: &min, Max: &window}}, Shape: &shape, Price: &price, ServiceTiers: []llm.ServiceTier{{ID: "fast", Aliases: []string{"quick"}, Request: llm.ServiceTierRequest{Betas: []string{"beta"}}, Price: price}}, SupportsExperimentalContext: &enabled, ContextToolDefinitions: map[string]llm.ContextToolDefinition{"search": {Description: &description}}}
	state := modeldiscovery.State{Snapshot: modeldiscovery.Snapshot{Models: map[string]modeldiscovery.Model{"m": m}}}
	owned := cloneProviderCatalogState(state).Snapshot.Models["m"]
	window = 999
	min = 999
	enabled = false
	shape = "changed"
	description = "changed"
	m.InputModalities[0] = "image"
	m.ReasoningOptions[0].Values[0] = "low"
	m.Price.Tiers[0].Input = 999
	m.ServiceTiers[0].Aliases[0] = "changed"
	m.ServiceTiers[0].Request.Betas[0] = "changed"
	if *owned.ContextWindow != 100 || *owned.OutputLimit != 100 || !*owned.Reasoning || !*owned.ReasoningSummarySupported || !*owned.SupportsExperimentalContext || *owned.Shape != "openai" || owned.InputModalities[0] != "text" || owned.ReasoningOptions[0].Values[0] != "high" || *owned.ReasoningOptions[0].Min != 1 || *owned.ReasoningOptions[0].Max != 100 || owned.Price.Tiers[0].Input != 3 || owned.ServiceTiers[0].Price.Tiers[0].Input != 3 || owned.ServiceTiers[0].Aliases[0] != "quick" || owned.ServiceTiers[0].Request.Betas[0] != "beta" || *owned.ContextToolDefinitions["search"].Description != "original" {
		t.Fatalf("provider snapshot retains caller-owned metadata: %+v", owned)
	}
}

func TestModelsDevCopyOwnsNestedMetadata(t *testing.T) {
	enabled := true
	description := "original"
	min := 1
	source := &modelcatalog.Catalog{Providers: map[string]modelcatalog.Provider{"p": {Env: []string{"KEY"}, Models: map[string]modelcatalog.Model{"m": {Modalities: modelcatalog.Modalities{Input: []string{"text"}, Output: []string{"text"}}, ReasoningSummarySupported: &enabled, ReasoningOptions: []llm.ReasoningOption{{Values: []string{"high"}, Min: &min}}, Cost: llm.Price{Tiers: []llm.PriceTier{{Input: 3}}}, ServiceTiers: []llm.ServiceTier{{ID: "fast", Aliases: []string{"quick"}, Price: llm.Price{Tiers: []llm.PriceTier{{Input: 4}}}}}, ContextToolDefinitions: map[string]llm.ContextToolDefinition{"search": {Description: &description}}}}}}}
	owned := cloneModelsDevCatalog(source)
	p := source.Providers["p"]
	p.Env[0] = "changed"
	m := p.Models["m"]
	m.Modalities.Input[0] = "image"
	m.Modalities.Output[0] = "image"
	enabled = false
	description = "changed"
	min = 99
	m.ReasoningOptions[0].Values[0] = "low"
	m.Cost.Tiers[0].Input = 99
	m.ServiceTiers[0].Price.Tiers[0].Input = 99
	m.ServiceTiers[0].Aliases[0] = "changed"
	op := owned.Providers["p"]
	om := op.Models["m"]
	if op.Env[0] != "KEY" || om.Modalities.Input[0] != "text" || om.Modalities.Output[0] != "text" || !*om.ReasoningSummarySupported || om.ReasoningOptions[0].Values[0] != "high" || *om.ReasoningOptions[0].Min != 1 || om.Cost.Tiers[0].Input != 3 || om.ServiceTiers[0].Price.Tiers[0].Input != 4 || om.ServiceTiers[0].Aliases[0] != "quick" || *om.ContextToolDefinitions["search"].Description != "original" {
		t.Fatalf("models.dev copy retains caller metadata: %+v", om)
	}
}
