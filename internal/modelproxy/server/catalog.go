package server

import (
	"strings"
	"time"

	"harness/internal/llm"
	"harness/internal/modelcatalog"
	"harness/internal/modelproxy/modeldiscovery"
	"harness/internal/modelproxy/pricing"
	"harness/internal/modelproxy/protocol"
)

// catalogSnapshot is the immutable served state: a registry used for model
// metadata, a pricer used for request costs, and the catalog served at
// /v1/models. It is swapped atomically when either catalog source refreshes so
// availability, metadata, and prices stay fresh without a restart. Readers
// Load() it; refreshers Store() a freshly built one.
type catalogSnapshot struct {
	registry *llm.Registry
	catalog  protocol.Catalog
	targets  map[string]resolvedTarget
	pricer   pricing.Pricer
}

// pricingInfo dates the served catalog's prices from every source that actually
// contributed price data. Provider-direct catalogs and models.dev may expire on
// different schedules; manual prices are dated by the provider-config files.
func (h *Handler) pricingInfo(md *modelcatalog.Catalog, mdSourceDate time.Time, providerCatalogs map[string]modeldiscovery.State) *protocol.PricingInfo {
	sourceDate := h.configSourceDate
	var expiresAt time.Time
	if md != nil && !mdSourceDate.IsZero() && modelsDevContributesPrice(h.providers, md, providerCatalogs) {
		sourceDate = mdSourceDate
		if h.pricingMaxAge > 0 {
			expiresAt = mdSourceDate.Add(h.pricingMaxAge)
		}
	}
	if h.providerModelsMaxAge > 0 {
		for _, state := range providerCatalogs {
			if !state.Authoritative || state.Snapshot.FetchedAt.IsZero() || !snapshotHasPrice(state.Snapshot) {
				continue
			}
			if sourceDate.IsZero() || state.Snapshot.FetchedAt.Before(sourceDate) {
				sourceDate = state.Snapshot.FetchedAt
			}
			candidate := state.Snapshot.FetchedAt.Add(h.providerModelsMaxAge)
			if expiresAt.IsZero() || candidate.Before(expiresAt) {
				expiresAt = candidate
			}
		}
	}
	if sourceDate.IsZero() {
		return nil
	}
	return &protocol.PricingInfo{
		SourceDate:    sourceDate,
		MaxAgeSeconds: int64(h.pricingMaxAge / time.Second),
		ExpiresAt:     expiresAt,
	}
}

// effectiveProviders returns provider configs ready for the registry and
// catalog. Fresh complete provider snapshots are authoritative for managed
// availability; stale snapshots contribute metadata only. models.dev and the
// configured entries remain fallbacks. Manual providers are unchanged unless
// they explicitly opt into discovery.
func (h *Handler) effectiveProviders(md *modelcatalog.Catalog, providerCatalogs map[string]modeldiscovery.State) ([]llm.ProviderConfig, bool) {
	out := make([]llm.ProviderConfig, 0, len(h.providers))
	pruned := false
	for _, pc := range h.providers {
		spec, discoverySupported, resolveErr := modeldiscovery.Resolve(pc)
		if resolveErr != nil {
			h.logger.Warn("provider model discovery configuration invalid", "provider", pc.Name, "err", resolveErr)
			discoverySupported = false
		}
		_ = spec
		state, hasState := providerCatalogs[pc.Name]
		if hasState && state.Unsupported {
			discoverySupported = false
			hasState = false
		}
		explicitDiscovery := pc.ModelDiscovery != nil &&
			(pc.ModelDiscovery.Enabled == nil || *pc.ModelDiscovery.Enabled)
		if !pc.Managed && !explicitDiscovery {
			out = append(out, pc)
			continue
		}

		baseline := modeldiscovery.ProviderFromConfig(pc)
		if provider, ok := md.Provider(pc.Name); ok {
			baseline = modeldiscovery.OverlayProvider(baseline, provider)
		}
		effective := baseline
		if hasState {
			if state.Authoritative && state.Snapshot.Complete {
				effective = modeldiscovery.MergeProvider(baseline, state.Snapshot)
			} else {
				effective = modeldiscovery.OverlaySnapshotMetadata(baseline, state.Snapshot)
			}
		} else if pc.Managed && !discoverySupported {
			provider, ok := md.Provider(pc.Name)
			if !ok {
				pruned = true
				h.logger.Warn("managed provider no longer exists in models.dev catalog; removing it from live catalog", "provider", pc.Name)
				continue
			}
			effective = provider
		}

		cp := pc
		cp.Models = make([]llm.ModelEntry, 0, len(pc.Models))
		for _, entry := range pc.Models {
			info, ok := effective.ModelInfo(entry.Name)
			if !ok {
				pruned = true
				h.logger.Warn("configured model is absent from authoritative model catalog; removing it from live catalog", "provider", pc.Name, "model", entry.Name)
				continue
			}
			configuredPrice := entry.Price
			if info.ContextWindow > 0 {
				entry.ContextWindow = info.ContextWindow
			}
			if info.OutputLimit > 0 {
				entry.OutputLimit = info.OutputLimit
			}
			if len(info.InputModalities) > 0 {
				entry.InputModalities = append([]string(nil), info.InputModalities...)
			}
			if hasState {
				if direct, ok := state.Snapshot.Models[entry.Name]; ok && direct.InputModalitiesKnown {
					entry.InputModalities = append([]string(nil), direct.InputModalities...)
				}
			}
			if len(info.ServiceTiers) > 0 {
				entry.ServiceTiers = llm.NormalizeServiceTiers(info.ServiceTiers)
			}
			if info.Reasoning != nil {
				reasoning := info.Reasoning.Supported
				entry.Reasoning = &reasoning
				entry.ReasoningSummarySupported = info.Reasoning.SummarySupported
				entry.ReasoningOptions = append([]llm.ReasoningOption(nil), info.Reasoning.Options...)
			}
			entry.Shape = info.Shape
			switch {
			case !pc.Managed:
				entry.Price = configuredPrice
			case pc.Name == modelcatalog.OpenAICodexProviderID:
				entry.Price = llm.Price{}
			case strings.TrimSpace(pc.PriceSource) != "":
				entry.Price = llm.Price{}
				if priceProvider, ok := md.Provider(pc.PriceSource); ok {
					if priceInfo, ok := priceProvider.ModelInfo(entry.Name); ok {
						entry.Price = priceInfo.Price
					}
				}
			default:
				entry.Price = info.Price
			}
			cp.Models = append(cp.Models, entry)
		}
		if len(cp.Models) == 0 {
			pruned = true
			h.logger.Warn("provider has no models remaining in authoritative model catalog; removing it from live catalog", "provider", pc.Name)
			continue
		}
		out = append(out, cp)
	}
	return out, pruned
}
