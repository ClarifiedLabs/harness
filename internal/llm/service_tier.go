package llm

import "strings"

// ServiceTier is one model-advertised inference tier. ID becomes the catalog
// target suffix. Request describes the bounded provider request mutation needed
// to activate it; Price is the tier-specific per-million-token schedule when
// the catalog provides one.
type ServiceTier struct {
	ID          string             `json:"id"`
	Name        string             `json:"name,omitempty"`
	Description string             `json:"description,omitempty"`
	Aliases     []string           `json:"aliases,omitempty"`
	Request     ServiceTierRequest `json:"request,omitzero"`
	Price       Price              `json:"price,omitzero"`
}

// ServiceTierRequest is intentionally bounded to provider controls harness
// knows how to encode safely. Betas are Anthropic beta feature identifiers,
// not arbitrary HTTP headers.
type ServiceTierRequest struct {
	ServiceTier string   `json:"service_tier,omitempty"`
	Speed       string   `json:"speed,omitempty"`
	Betas       []string `json:"betas,omitempty"`
}

// NormalizeServiceTiers trims, lowercases, and de-duplicates service tiers
// while preserving their configured order. A tier without an explicit request
// mapping defaults to sending its ID as service_tier.
func NormalizeServiceTiers(tiers []ServiceTier) []ServiceTier {
	if tiers == nil {
		return nil
	}
	out := make([]ServiceTier, 0, len(tiers))
	seen := make(map[string]bool, len(tiers))
	for _, tier := range tiers {
		tier.ID = normalizeServiceTierValue(tier.ID)
		if tier.ID == "" || seen[tier.ID] {
			continue
		}
		seen[tier.ID] = true
		tier.Name = strings.TrimSpace(tier.Name)
		tier.Description = strings.TrimSpace(tier.Description)
		tier.Aliases = normalizeServiceTierValues(tier.Aliases, tier.ID)
		tier.Request.ServiceTier = normalizeServiceTierValue(tier.Request.ServiceTier)
		tier.Request.Speed = normalizeServiceTierValue(tier.Request.Speed)
		tier.Request.Betas = normalizeServiceTierValues(tier.Request.Betas, "")
		if tier.Request.ServiceTier == "" && tier.Request.Speed == "" {
			tier.Request.ServiceTier = tier.ID
		}
		out = append(out, tier)
	}
	return out
}

// ResolveServiceTier resolves a catalog value by canonical ID, alias, or
// display name. Empty selects provider-default behavior.
func ResolveServiceTier(selector string, supported []ServiceTier) (ServiceTier, bool) {
	selector = normalizeServiceTierValue(selector)
	if selector == "" {
		return ServiceTier{}, true
	}
	for _, tier := range NormalizeServiceTiers(supported) {
		if selector == tier.ID || selector == normalizeServiceTierValue(tier.Name) {
			return tier, true
		}
		for _, alias := range tier.Aliases {
			if selector == alias {
				return tier, true
			}
		}
	}
	return ServiceTier{}, false
}

// ServiceTierSupported reports whether selector resolves against supported.
func ServiceTierSupported(selector string, supported []ServiceTier) bool {
	_, ok := ResolveServiceTier(selector, supported)
	return ok
}

// ServiceTierIDs returns canonical selector IDs in advertised order.
func ServiceTierIDs(tiers []ServiceTier) []string {
	normalized := NormalizeServiceTiers(tiers)
	out := make([]string, 0, len(normalized))
	for _, tier := range normalized {
		out = append(out, tier.ID)
	}
	return out
}

// MatchServiceTierRequest finds the catalog tier whose bounded request mapping
// matches the values sent to (or reported by) a provider.
func MatchServiceTierRequest(tiers []ServiceTier, serviceTier, speed string) (ServiceTier, bool) {
	serviceTier = normalizeServiceTierValue(serviceTier)
	speed = normalizeServiceTierValue(speed)
	if serviceTier == "" && speed == "" {
		return ServiceTier{}, false
	}
	for _, tier := range NormalizeServiceTiers(tiers) {
		request := tier.Request
		if serviceTier != "" && request.ServiceTier == serviceTier && (request.Speed == "" || request.Speed == speed) {
			return tier, true
		}
		if speed != "" && request.Speed == speed && (request.ServiceTier == "" || request.ServiceTier == serviceTier) {
			return tier, true
		}
	}
	return ServiceTier{}, false
}

// ProviderServiceTiers returns a normalized provider-level capability list.
func ProviderServiceTiers(pc ProviderConfig) []ServiceTier {
	return NormalizeServiceTiers(pc.ServiceTiers)
}

// ModelServiceTiers applies a non-empty model-level override to its provider
// capability.
func ModelServiceTiers(pc ProviderConfig, model ModelEntry) []ServiceTier {
	if len(model.ServiceTiers) > 0 {
		return NormalizeServiceTiers(model.ServiceTiers)
	}
	return ProviderServiceTiers(pc)
}

func normalizeServiceTierValue(value string) string {
	return strings.ToLower(strings.TrimSpace(value))
}

func normalizeServiceTierValues(values []string, exclude string) []string {
	if values == nil {
		return nil
	}
	out := make([]string, 0, len(values))
	seen := map[string]bool{}
	if exclude != "" {
		seen[exclude] = true
	}
	for _, value := range values {
		value = normalizeServiceTierValue(value)
		if value == "" || seen[value] {
			continue
		}
		seen[value] = true
		out = append(out, value)
	}
	return out
}
