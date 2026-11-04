package modelproxy

import "harness/internal/llm"

// Registry builds a local model metadata registry from a proxy catalog.
func Registry(catalog Catalog) *llm.Registry {
	models := map[string]llm.ModelInfo{}
	for _, target := range catalog.Targets {
		if target.ID == "" {
			continue
		}
		info := llm.ModelInfo{
			IncrementalTools: target.IncrementalTools,
			ReasoningUpdates: target.ReasoningUpdates,
			AsyncTools:       target.AsyncTools,
			NativeSteering:   target.NativeSteering,
			ContextWindow:    target.ContextWindow,
			OutputLimit:      target.OutputLimit,
			InputModalities:  append([]string(nil), target.InputModalities...),
			ServerTools:      llm.NormalizeServerTools(target.ServerTools),
			Price:            target.Price,
			Reasoning:        proxyTargetReasoning(target),
		}
		models[target.ID] = info
		for _, alias := range target.Aliases {
			if alias != "" {
				models[alias] = info
			}
		}
	}
	return llm.NewRegistry(models)
}

func proxyTargetReasoning(target Target) *llm.ReasoningInfo {
	if !target.Reasoning {
		return &llm.ReasoningInfo{Supported: false}
	}
	return &llm.ReasoningInfo{
		Supported: true,
	}
}
