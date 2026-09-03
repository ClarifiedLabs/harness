package server

import (
	"math"
	"strings"

	"harness/internal/llm"
	"harness/internal/reasoningprofile"
)

var reasoningProfileRank = map[string]int{
	"none":    0,
	"minimal": 1,
	"low":     2,
	"medium":  3,
	"high":    4,
	"xhigh":   5,
	"max":     6,
}

func (h *Handler) reasoningForTarget(target resolvedTarget, profile string, requested llm.ReasoningConfig) llm.ReasoningConfig {
	if profile == "" {
		profile = requested.Profile
	}
	profile = normalizeReasoningProfile(profile)
	info := modelEntryReasoning(target.entry)
	summary := requested.Summary
	if info != nil && !info.SupportsSummaries() {
		summary = ""
	}
	if profile == "" {
		return llm.ReasoningConfig{Summary: summary}
	}
	if info == nil || !info.Supported {
		return llm.ReasoningConfig{Summary: summary}
	}
	mode := reasoningModeForProviderConfig(target.pc)
	out := llm.ReasoningConfig{Profile: profile, Summary: summary}
	switch profile {
	case "none":
		if info.SupportsToggle() {
			disabled := false
			out.Enabled = &disabled
		}
	case "minimal", "low", "medium", "high", "xhigh", "max":
		if effort := mappedReasoningEffort(info, profile); effort != "" {
			out.Effort = effort
		} else if budget, ok := mappedReasoningBudget(info, profile); ok {
			out.BudgetTokens = &budget
		}
	}
	if mode == "responses" {
		out.Enabled = nil
	}
	if mode == "openai" && out.Enabled != nil {
		out.Enabled = nil
	}
	if profile == "none" && out.Enabled == nil {
		return llm.ReasoningConfig{}
	}
	return out
}

func normalizeReasoningProfile(profile string) string {
	if normalized, ok := reasoningprofile.Normalize(profile); ok {
		return normalized
	}
	return strings.ToLower(strings.TrimSpace(profile))
}

func mappedReasoningEffort(info *llm.ReasoningInfo, profile string) string {
	values, ok := info.EffortValues()
	if !ok {
		if len(info.Options) > 0 {
			return ""
		}
		if profile == "minimal" {
			return "low"
		}
		if profile == "max" {
			return "high"
		}
		if profile == "xhigh" {
			return "high"
		}
		return profile
	}
	if len(values) == 0 {
		if profile == "minimal" {
			return "low"
		}
		if profile == "max" {
			return "high"
		}
		if profile == "xhigh" {
			return "high"
		}
		return profile
	}
	type candidate struct {
		value string
		rank  int
	}
	var candidates []candidate
	seen := map[string]bool{}
	for _, value := range values {
		clean := strings.ToLower(strings.TrimSpace(value))
		if clean == "" || clean == "none" || seen[clean] {
			continue
		}
		rank, ok := reasoningProfileRank[clean]
		if !ok {
			continue
		}
		candidates = append(candidates, candidate{value: clean, rank: rank})
		seen[clean] = true
	}
	if len(candidates) == 0 {
		return ""
	}
	switch profile {
	case "minimal":
		best := candidates[0]
		for _, c := range candidates[1:] {
			if c.rank < best.rank {
				best = c
			}
		}
		return best.value
	case "max":
		best := candidates[0]
		for _, c := range candidates[1:] {
			if c.rank > best.rank {
				best = c
			}
		}
		return best.value
	}
	targetRank, ok := reasoningProfileRank[profile]
	if !ok {
		return ""
	}
	best := candidates[0]
	bestDistance := absInt(best.rank - targetRank)
	for _, c := range candidates[1:] {
		distance := absInt(c.rank - targetRank)
		if distance < bestDistance || (distance == bestDistance && c.rank < best.rank) {
			best = c
			bestDistance = distance
		}
	}
	return best.value
}

func mappedReasoningBudget(info *llm.ReasoningInfo, profile string) (int, bool) {
	minPtr, maxPtr, ok := info.BudgetTokenRange()
	if !ok {
		return 0, false
	}
	minBudget := 0
	if minPtr != nil {
		minBudget = *minPtr
	}
	if maxPtr == nil {
		if minBudget <= 0 {
			return 0, false
		}
		return minBudget, true
	}
	if *maxPtr <= 0 {
		return 0, false
	}
	maxBudget := *maxPtr
	if minBudget > maxBudget {
		minBudget = maxBudget
	}
	var budget int
	switch profile {
	case "minimal":
		budget = int(math.Ceil(float64(maxBudget) * 0.05))
		if budget < 1 {
			budget = 1
		}
	case "low":
		budget = int(math.Round(float64(maxBudget) * 0.25))
	case "medium":
		budget = int(math.Round(float64(maxBudget) * 0.50))
	case "high":
		budget = int(math.Round(float64(maxBudget) * 0.75))
	case "xhigh":
		budget = int(math.Round(float64(maxBudget) * 0.90))
	case "max":
		budget = maxBudget
	default:
		return 0, false
	}
	if budget < minBudget {
		budget = minBudget
	}
	if budget > maxBudget {
		budget = maxBudget
	}
	return budget, true
}

func absInt(n int) int {
	if n < 0 {
		return -n
	}
	return n
}

func reasoningModeForProviderConfig(pc llm.ProviderConfig) string {
	apiType := strings.ToLower(strings.TrimSpace(pc.APIType))
	if apiType == "" {
		apiType = strings.ToLower(strings.TrimSpace(pc.Name))
	}
	if apiType == "anthropic" || apiType == "responses" {
		return apiType
	}
	if strings.EqualFold(pc.Name, "google") || strings.Contains(strings.ToLower(pc.BaseURL), "generativelanguage.googleapis.com") {
		return "google"
	}
	if strings.EqualFold(pc.Name, "openrouter") || strings.Contains(strings.ToLower(pc.BaseURL), "openrouter.ai") {
		return "openrouter"
	}
	return "openai"
}
