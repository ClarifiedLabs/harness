package ui

import (
	"sort"
	"strconv"
	"strings"

	"harness/internal/llm"
)

// ModelPickerPriceLegend describes the compact price schedules shown by model
// pickers. Each pair is input/output USD per one million tokens.
const ModelPickerPriceLegend = "Price: input/output USD per 1M tokens"

// FormatPickerPrice formats a static model price schedule for a compact picker
// row. Tier thresholds describe model-visible input context: the base rate
// applies through the first threshold, and each tier applies above its threshold.
// An empty string means no static pricing schedule is available.
func FormatPickerPrice(p llm.Price) string {
	if p.IsZero() {
		return ""
	}
	if len(p.Tiers) == 0 {
		return formatPickerPricePair(p.Input, p.Output)
	}

	tiers := append([]llm.PriceTier(nil), p.Tiers...)
	sort.SliceStable(tiers, func(i, j int) bool {
		return tiers[i].Threshold < tiers[j].Threshold
	})

	bands := make([]string, 0, len(tiers)+1)
	bands = append(bands, formatPickerPricePair(p.Input, p.Output)+" ≤"+formatContextThreshold(tiers[0].Threshold))
	for i, tier := range tiers {
		label := " >" + formatContextThreshold(tier.Threshold)
		if i+1 < len(tiers) {
			label += "–≤" + formatContextThreshold(tiers[i+1].Threshold)
		}
		bands = append(bands, formatPickerPricePair(tier.Input, tier.Output)+label)
	}
	return strings.Join(bands, " · ")
}

func formatPickerPricePair(input, output float64) string {
	return "$" + formatPickerPriceComponent(input) + "/$" + formatPickerPriceComponent(output)
}

func formatPickerPriceComponent(v float64) string {
	return strconv.FormatFloat(v, 'f', -1, 64)
}

func formatContextThreshold(tokens int) string {
	switch {
	case tokens != 0 && tokens%1_000_000 == 0:
		return strconv.Itoa(tokens/1_000_000) + "m"
	case tokens != 0 && tokens%1_000 == 0:
		return strconv.Itoa(tokens/1_000) + "k"
	default:
		return strconv.Itoa(tokens)
	}
}
