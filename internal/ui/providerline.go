package ui

import (
	"fmt"
	"strings"

	"harness/internal/llm"
)

// ProviderLine formats the active model summary shown at startup and after
// runtime switches.
func ProviderLine(provider, model, registryModel string, reasoning llm.ReasoningConfig, registry *llm.Registry) string {
	_ = provider // Kept for call-site compatibility; model now carries the display target.
	var b strings.Builder
	fmt.Fprintf(&b, "model: %s  reasoning: %s", model, providerLineReasoningLabel(reasoning))
	if price := providerLineModelPricing(registry, registryModel); price != "" {
		fmt.Fprintf(&b, "  pricing: %s", price)
	}
	return b.String()
}

func providerLineReasoningLabel(reasoning llm.ReasoningConfig) string {
	if reasoning.Empty() {
		return "default"
	}
	var parts []string
	if profile := strings.TrimSpace(reasoning.Profile); profile != "" {
		parts = append(parts, "profile="+profile)
	}
	if summary := strings.TrimSpace(reasoning.Summary); summary != "" {
		parts = append(parts, "summary="+summary)
	}
	return strings.Join(parts, ",")
}

func providerLineModelPricing(registry *llm.Registry, model string) string {
	if registry == nil {
		return ""
	}
	info, ok := registry.Lookup(model)
	if !ok {
		return ""
	}
	return formatProviderLinePrice(info.Price)
}

func formatProviderLinePrice(p llm.Price) string {
	var parts []string
	if p.Input != 0 {
		parts = append(parts, "in=$"+formatProviderLinePriceComponent(p.Input)+"/M")
	}
	if p.Output != 0 {
		parts = append(parts, "out=$"+formatProviderLinePriceComponent(p.Output)+"/M")
	}
	if p.CacheRead != 0 {
		parts = append(parts, "cache-read=$"+formatProviderLinePriceComponent(p.CacheRead)+"/M")
	}
	if p.CacheWrite != 0 {
		parts = append(parts, "cache-write=$"+formatProviderLinePriceComponent(p.CacheWrite)+"/M")
	}
	return strings.Join(parts, " ")
}

func formatProviderLinePriceComponent(v float64) string {
	if v == float64(int64(v)) {
		return fmt.Sprintf("%.0f", v)
	}
	return strings.TrimRight(strings.TrimRight(fmt.Sprintf("%.2f", v), "0"), ".")
}
