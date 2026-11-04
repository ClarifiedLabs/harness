package modelproxy

import (
	"harness/internal/llm"
	"slices"
)

// CloneRequest restores the ownership boundary provided by the remote codec,
// without serializing or reducing provider-owned opaque content.
func CloneRequest(r llm.Request) llm.Request {
	r.Messages = llm.CloneMessages(r.Messages)
	r.Tools = llm.CloneToolSchemas(r.Tools)
	r.DeferredToolGroups = llm.CloneToolGroups(r.DeferredToolGroups)
	r.ServerTools = llm.CloneServerTools(r.ServerTools)
	r.StopSeqs = slices.Clone(r.StopSeqs)
	r.Betas = slices.Clone(r.Betas)
	r.RequestContext = slices.Clone(r.RequestContext)
	if r.Temperature != nil {
		v := *r.Temperature
		r.Temperature = &v
	}
	if r.Reasoning.Enabled != nil {
		v := *r.Reasoning.Enabled
		r.Reasoning.Enabled = &v
	}
	if r.Reasoning.BudgetTokens != nil {
		v := *r.Reasoning.BudgetTokens
		r.Reasoning.BudgetTokens = &v
	}
	return r
}
func CloneCatalog(c Catalog) Catalog {
	c.Targets = slices.Clone(c.Targets)
	if c.Pricing != nil {
		p := *c.Pricing
		c.Pricing = &p
	}
	for i := range c.Targets {
		t := &c.Targets[i]
		t.Price.Tiers = slices.Clone(t.Price.Tiers)
		t.Aliases = slices.Clone(t.Aliases)
		t.InputModalities = slices.Clone(t.InputModalities)
		t.ServerTools = slices.Clone(t.ServerTools)
		if t.ContextToolDefinitions != nil {
			m := make(map[string]llm.ContextToolDefinition, len(t.ContextToolDefinitions))
			for k, v := range t.ContextToolDefinitions {
				if v.Description != nil {
					s := *v.Description
					v.Description = &s
				}
				if v.Parameters != nil {
					s := *v.Parameters
					v.Parameters = &s
				}
				m[k] = v
			}
			t.ContextToolDefinitions = m
		}
	}
	return c
}
