package runtime

import (
	"maps"
	"slices"

	"harness/internal/llm"
	"harness/internal/modelcatalog"
	"harness/internal/modelproxy/modeldiscovery"
)

func clonePointer[T any](p *T) *T {
	if p == nil {
		return nil
	}
	v := *p
	return &v
}
func cloneReasoningOptions(options []llm.ReasoningOption) []llm.ReasoningOption {
	options = slices.Clone(options)
	for i := range options {
		o := &options[i]
		o.Values = slices.Clone(o.Values)
		o.Min = clonePointer(o.Min)
		o.Max = clonePointer(o.Max)
	}
	return options
}
func cloneServiceTiers(tiers []llm.ServiceTier) []llm.ServiceTier {
	tiers = slices.Clone(tiers)
	for i := range tiers {
		t := &tiers[i]
		t.Aliases = slices.Clone(t.Aliases)
		t.Request.Betas = slices.Clone(t.Request.Betas)
		t.Price.Tiers = slices.Clone(t.Price.Tiers)
	}
	return tiers
}
func cloneProviderCatalogState(state modeldiscovery.State) modeldiscovery.State {
	state.Snapshot.Models = maps.Clone(state.Snapshot.Models)
	for id, m := range state.Snapshot.Models {
		m.ContextWindow = clonePointer(m.ContextWindow)
		m.OutputLimit = clonePointer(m.OutputLimit)
		m.InputModalities = slices.Clone(m.InputModalities)
		m.Reasoning = clonePointer(m.Reasoning)
		m.ReasoningSummarySupported = clonePointer(m.ReasoningSummarySupported)
		m.ReasoningOptions = cloneReasoningOptions(m.ReasoningOptions)
		m.Shape = clonePointer(m.Shape)
		m.Price = clonePointer(m.Price)
		if m.Price != nil {
			m.Price.Tiers = slices.Clone(m.Price.Tiers)
		}
		m.ServiceTiers = cloneServiceTiers(m.ServiceTiers)
		m.SupportsExperimentalContext = clonePointer(m.SupportsExperimentalContext)
		m.ContextToolDefinitions = modelcatalog.CloneContextToolDefinitions(m.ContextToolDefinitions)
		state.Snapshot.Models[id] = m
	}
	return state
}
func cloneModelsDevCatalog(c *modelcatalog.Catalog) *modelcatalog.Catalog {
	if c == nil {
		return nil
	}
	out := &modelcatalog.Catalog{Providers: maps.Clone(c.Providers)}
	for id, p := range out.Providers {
		p.Env = slices.Clone(p.Env)
		p.Models = maps.Clone(p.Models)
		for name, m := range p.Models {
			m.Modalities.Input = slices.Clone(m.Modalities.Input)
			m.Modalities.Output = slices.Clone(m.Modalities.Output)
			m.ReasoningSummarySupported = clonePointer(m.ReasoningSummarySupported)
			m.ReasoningOptions = cloneReasoningOptions(m.ReasoningOptions)
			m.Cost.Tiers = slices.Clone(m.Cost.Tiers)
			m.ServiceTiers = cloneServiceTiers(m.ServiceTiers)
			m.ContextToolDefinitions = modelcatalog.CloneContextToolDefinitions(m.ContextToolDefinitions)
			m.Experimental.Modes = maps.Clone(m.Experimental.Modes)
			for modeID, mode := range m.Experimental.Modes {
				mode.Cost.Tiers = slices.Clone(mode.Cost.Tiers)
				mode.Provider.Headers = maps.Clone(mode.Provider.Headers)
				m.Experimental.Modes[modeID] = mode
			}
			p.Models[name] = m
		}
		out.Providers[id] = p
	}
	return out
}
