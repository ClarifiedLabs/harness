package modeldiscovery

import (
	"strings"

	"harness/internal/llm"
	"harness/internal/modelcatalog"
)

// ProviderFromConfig converts configured entries into a metadata fallback.
func ProviderFromConfig(pc llm.ProviderConfig) modelcatalog.Provider {
	models := make(map[string]modelcatalog.Model, len(pc.Models))
	for _, entry := range pc.Models {
		if strings.TrimSpace(entry.Name) == "" {
			continue
		}
		reasoning := false
		if entry.Reasoning != nil {
			reasoning = *entry.Reasoning
		}
		models[entry.Name] = modelcatalog.Model{
			ID: entry.Name, Name: entry.Name,
			Modalities: modelcatalog.Modalities{Input: append([]string(nil), entry.InputModalities...)},
			Reasoning:  reasoning, ReasoningSummarySupported: entry.ReasoningSummarySupported,
			ReasoningOptions: append([]llm.ReasoningOption(nil), entry.ReasoningOptions...),
			Limit:            modelcatalog.Limit{Context: entry.ContextWindow, Output: entry.OutputLimit},
			Provider:         modelcatalog.ModelProvider{Shape: entry.Shape}, Cost: entry.Price,
			ServiceTiers: llm.NormalizeServiceTiers(entry.ServiceTiers),
		}
	}
	return modelcatalog.Provider{ID: pc.Name, Name: pc.Name, API: pc.BaseURL, Env: append([]string(nil), pc.APIKeyEnv...), Models: models}
}

// OverlayProvider overlays higher-priority catalog metadata onto fallback.
func OverlayProvider(fallback, higher modelcatalog.Provider) modelcatalog.Provider {
	out := cloneProvider(fallback)
	if higher.ID != "" {
		out.ID = higher.ID
	}
	if higher.Name != "" {
		out.Name = higher.Name
	}
	if higher.API != "" {
		out.API = higher.API
	}
	if higher.Doc != "" {
		out.Doc = higher.Doc
	}
	if higher.NPM != "" {
		out.NPM = higher.NPM
	}
	if len(higher.Env) > 0 {
		out.Env = append([]string(nil), higher.Env...)
	}
	if out.Models == nil {
		out.Models = map[string]modelcatalog.Model{}
	}
	for id, model := range higher.Models {
		if current, ok := catalogModel(out, id); ok {
			out.Models[id] = mergeCatalogModel(current, model)
		} else {
			out.Models[id] = cloneCatalogModel(model)
		}
	}
	return out
}

// MergeProvider applies a complete direct snapshot to baseline. Only models
// returned by the provider remain. Baseline models need no capability proof;
// direct-only models must be eligible under the adapter policy.
func MergeProvider(baseline modelcatalog.Provider, snapshot Snapshot) modelcatalog.Provider {
	out := cloneProvider(baseline)
	out.Models = make(map[string]modelcatalog.Model, len(snapshot.Models))
	for id, direct := range snapshot.Models {
		base, known := catalogModel(baseline, id)
		if !known && !direct.Eligible {
			continue
		}
		if !known {
			base = modelcatalog.Model{ID: id, Name: id}
		}
		out.Models[id] = mergeModel(base, direct)
	}
	return out
}

// OverlaySnapshotMetadata applies direct fields without changing the baseline
// model set. It is used for stale caches and failed refreshes, where cached data
// may enrich configured models but cannot determine current availability.
func OverlaySnapshotMetadata(baseline modelcatalog.Provider, snapshot Snapshot) modelcatalog.Provider {
	out := cloneProvider(baseline)
	for id, direct := range snapshot.Models {
		base, known := catalogModel(out, id)
		if !known {
			continue
		}
		direct.Price = nil
		out.Models[id] = mergeModel(base, direct)
	}
	return out
}

func mergeCatalogModel(base, higher modelcatalog.Model) modelcatalog.Model {
	out := cloneCatalogModel(base)
	if higher.ID != "" {
		out.ID = higher.ID
	}
	if higher.Name != "" {
		out.Name = higher.Name
	}
	if higher.ReleaseDate != "" {
		out.ReleaseDate = higher.ReleaseDate
	}
	if higher.LastUpdated != "" {
		out.LastUpdated = higher.LastUpdated
	}
	if higher.Modalities.Input != nil {
		out.Modalities.Input = append([]string(nil), higher.Modalities.Input...)
	}
	if higher.Modalities.Output != nil {
		out.Modalities.Output = append([]string(nil), higher.Modalities.Output...)
	}
	out.Reasoning = higher.Reasoning
	if higher.ReasoningSummarySupported != nil {
		value := *higher.ReasoningSummarySupported
		out.ReasoningSummarySupported = &value
	}
	if higher.ReasoningOptions != nil {
		out.ReasoningOptions = append([]llm.ReasoningOption(nil), higher.ReasoningOptions...)
	}
	if higher.Limit.Context > 0 {
		out.Limit.Context = higher.Limit.Context
	}
	if higher.Limit.Output > 0 {
		out.Limit.Output = higher.Limit.Output
	}
	if higher.Provider.Shape != "" {
		out.Provider = higher.Provider
	}
	if !higher.Cost.IsZero() {
		out.Cost = higher.Cost
	}
	if higher.ServiceTiers != nil {
		out.ServiceTiers = llm.NormalizeServiceTiers(higher.ServiceTiers)
	}
	return out
}

func mergeModel(base modelcatalog.Model, direct Model) modelcatalog.Model {
	out := cloneCatalogModel(base)
	out.ID = direct.ID
	if direct.Name != "" {
		out.Name = direct.Name
	}
	if direct.ContextWindow != nil {
		out.Limit.Context = *direct.ContextWindow
	}
	if direct.OutputLimit != nil {
		out.Limit.Output = *direct.OutputLimit
	}
	if direct.InputModalitiesKnown {
		out.Modalities.Input = append([]string(nil), direct.InputModalities...)
	}
	if direct.Reasoning != nil {
		out.Reasoning = *direct.Reasoning
	}
	if direct.ReasoningSummarySupported != nil {
		value := *direct.ReasoningSummarySupported
		out.ReasoningSummarySupported = &value
	}
	if direct.ReasoningOptions != nil {
		out.ReasoningOptions = append([]llm.ReasoningOption(nil), direct.ReasoningOptions...)
	}
	if direct.Shape != nil {
		out.Provider.Shape = *direct.Shape
	}
	if direct.Price != nil {
		out.Cost = *direct.Price
	}
	if direct.ServiceTiers != nil {
		out.ServiceTiers = llm.NormalizeServiceTiers(direct.ServiceTiers)
	}
	return out
}

func catalogModel(provider modelcatalog.Provider, id string) (modelcatalog.Model, bool) {
	if model, ok := provider.Models[id]; ok {
		return model, true
	}
	for _, model := range provider.Models {
		if model.ID == id {
			return model, true
		}
	}
	return modelcatalog.Model{}, false
}

func cloneProvider(provider modelcatalog.Provider) modelcatalog.Provider {
	out := provider
	out.Env = append([]string(nil), provider.Env...)
	out.Models = make(map[string]modelcatalog.Model, len(provider.Models))
	for id, model := range provider.Models {
		out.Models[id] = cloneCatalogModel(model)
	}
	return out
}

func cloneCatalogModel(model modelcatalog.Model) modelcatalog.Model {
	model.Modalities.Input = append([]string(nil), model.Modalities.Input...)
	model.Modalities.Output = append([]string(nil), model.Modalities.Output...)
	model.ReasoningOptions = append([]llm.ReasoningOption(nil), model.ReasoningOptions...)
	model.ServiceTiers = append([]llm.ServiceTier(nil), model.ServiceTiers...)
	if model.ReasoningSummarySupported != nil {
		value := *model.ReasoningSummarySupported
		model.ReasoningSummarySupported = &value
	}
	return model
}
