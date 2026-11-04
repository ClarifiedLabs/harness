package modelproxy

import (
	"fmt"
	"strings"

	"harness/internal/llm"
)

// ConfiguredTargetNames is the canonical naming pass shared by offline config
// validation and runtime catalog construction. It includes service-tier variants
// and only publishes short aliases when those aliases are unambiguous.
func ConfiguredTargetNames(providers []llm.ProviderConfig) (map[string][]string, error) {
	counts := map[string]int{}
	for _, pc := range providers {
		for _, entry := range pc.Models {
			if entry.Name == "" {
				continue
			}
			counts[entry.Name]++
			for _, tier := range llm.ModelServiceTiers(pc, entry) {
				if !DefaultServiceTier(tier) {
					counts[entry.Name+":"+tier.ID]++
				}
			}
		}
	}
	names := map[string][]string{}
	resolved := map[string]string{}
	add := func(id, short string, variant bool) error {
		if _, exists := resolved[id]; exists {
			if variant {
				return fmt.Errorf("model proxy: target variant %q collides with another target", id)
			}
			return fmt.Errorf("model proxy: target %q collides with another target or alias", id)
		}
		aliases := []string{id}
		if counts[short] == 1 {
			aliases = append(aliases, short)
		}
		for _, alias := range aliases {
			if existing, exists := resolved[alias]; exists && existing != id {
				return fmt.Errorf("model proxy: target alias %q collides with target %q", alias, existing)
			}
			resolved[alias] = id
		}
		names[id] = aliases
		return nil
	}
	for _, pc := range providers {
		if pc.Name == "" {
			continue
		}
		for _, entry := range pc.Models {
			if entry.Name == "" {
				continue
			}
			id := pc.Name + ":" + entry.Name
			if err := add(id, entry.Name, false); err != nil {
				return nil, err
			}
			for _, tier := range llm.ModelServiceTiers(pc, entry) {
				if DefaultServiceTier(tier) {
					continue
				}
				if err := add(id+":"+tier.ID, entry.Name+":"+tier.ID, true); err != nil {
					return nil, err
				}
			}
		}
	}
	if len(names) == 0 {
		return nil, fmt.Errorf("model proxy: no configured models")
	}
	return names, nil
}

// DefaultServiceTier reports whether a tier uses the ordinary target rather
// than a distinct scheduling, speed, or beta variant.
func DefaultServiceTier(tier llm.ServiceTier) bool {
	if tier.Request.Speed != "" || len(tier.Request.Betas) > 0 {
		return false
	}
	switch strings.ToLower(strings.TrimSpace(tier.Request.ServiceTier)) {
	case "", "default", "standard", "standard_only":
		return true
	default:
		return false
	}
}
