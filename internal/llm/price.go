package llm

import (
	"encoding/json"
	"math"
	"sort"
)

const perMillion = 1_000_000.0

// Price is the per-1M-token price in USD for each token category. Cache fields
// are 0 when a provider has no separate cache pricing. Tiers carries
// context-length price tiers (e.g. "over 272k tokens costs more"), sorted on
// demand. Reasoning and audio prices are stored for completeness.
type Price struct {
	Input        float64     `json:"input"`
	Output       float64     `json:"output"`
	CacheRead    float64     `json:"cache_read,omitempty"`
	CacheWrite   float64     `json:"cache_write,omitempty"`
	CacheWrite1h float64     `json:"cache_write_1h,omitempty"`
	Reasoning    float64     `json:"reasoning,omitempty"`
	InputAudio   float64     `json:"input_audio,omitempty"`
	OutputAudio  float64     `json:"output_audio,omitempty"`
	Tiers        []PriceTier `json:"tiers,omitempty"`
}

// PriceTier is one context-length price step. The threshold is the maximum
// context size that still uses the lower price; the tier price applies when
// context is strictly greater than Threshold (matching models.dev's
// "context_over_N" semantics).
type PriceTier struct {
	Threshold    int     `json:"threshold"`
	Input        float64 `json:"input"`
	Output       float64 `json:"output"`
	CacheRead    float64 `json:"cache_read,omitempty"`
	CacheWrite   float64 `json:"cache_write,omitempty"`
	CacheWrite1h float64 `json:"cache_write_1h,omitempty"`
	Reasoning    float64 `json:"reasoning,omitempty"`
	InputAudio   float64 `json:"input_audio,omitempty"`
	OutputAudio  float64 `json:"output_audio,omitempty"`
}

// IsZero reports whether the price has no configured components, including no
// tiers.
func (p Price) IsZero() bool {
	if p.Input != 0 || p.Output != 0 || p.CacheRead != 0 || p.CacheWrite != 0 || p.CacheWrite1h != 0 ||
		p.Reasoning != 0 || p.InputAudio != 0 || p.OutputAudio != 0 {
		return false
	}
	return len(p.Tiers) == 0
}

// HasTiers reports whether the price uses context-length tiers.
func (p Price) HasTiers() bool {
	return len(p.Tiers) > 0
}

// Equal reports whether two prices have identical components and tiers.
func (p Price) Equal(other Price) bool {
	if p.Input != other.Input || p.Output != other.Output ||
		p.CacheRead != other.CacheRead || p.CacheWrite != other.CacheWrite ||
		p.CacheWrite1h != other.CacheWrite1h ||
		p.Reasoning != other.Reasoning || p.InputAudio != other.InputAudio || p.OutputAudio != other.OutputAudio {
		return false
	}
	if len(p.Tiers) != len(other.Tiers) {
		return false
	}
	for i := range p.Tiers {
		if p.Tiers[i] != other.Tiers[i] {
			return false
		}
	}
	return true
}

// Effective returns the flat price that applies for a context of the given
// size. When no tiers exist it returns the receiver unchanged; otherwise it walks
// the tiers in ascending threshold order and applies the highest tier whose
// threshold is exceeded.
func (p Price) Effective(contextTokens int) Price {
	if !p.HasTiers() {
		return p
	}
	tiers := make([]PriceTier, len(p.Tiers))
	copy(tiers, p.Tiers)
	sort.Slice(tiers, func(i, j int) bool { return tiers[i].Threshold < tiers[j].Threshold })

	effective := Price{
		Input:        p.Input,
		Output:       p.Output,
		CacheRead:    p.CacheRead,
		CacheWrite:   p.CacheWrite,
		CacheWrite1h: p.CacheWrite1h,
		Reasoning:    p.Reasoning,
		InputAudio:   p.InputAudio,
		OutputAudio:  p.OutputAudio,
	}
	for _, t := range tiers {
		if contextTokens > t.Threshold {
			effective.Input = t.Input
			effective.Output = t.Output
			effective.CacheRead = t.CacheRead
			effective.CacheWrite = t.CacheWrite
			effective.CacheWrite1h = t.CacheWrite1h
			effective.Reasoning = t.Reasoning
			effective.InputAudio = t.InputAudio
			effective.OutputAudio = t.OutputAudio
		}
	}
	return effective
}

// Cost returns the USD cost of the given usage under this price. estimatedInputTokens
// is used, when larger than the actual billed input size, to price requests
// before exact usage is known (e.g. a request expected to exceed a context tier).
// The second result is false when the price is zero, indicating no dollar figure
// should be shown.
func (p Price) Cost(u Usage, estimatedInputTokens int) (float64, bool) {
	if p.IsZero() {
		return 0, false
	}
	contextTokens := u.InputTokens + u.CacheReadTokens + u.CacheWriteTokens + u.CacheWrite1hTokens
	if estimatedInputTokens > contextTokens {
		contextTokens = estimatedInputTokens
	}
	rate := p.Effective(contextTokens)
	usd := float64(u.InputTokens)/perMillion*rate.Input +
		float64(u.OutputTokens)/perMillion*rate.Output +
		float64(u.CacheReadTokens)/perMillion*rate.CacheRead +
		float64(u.CacheWriteTokens)/perMillion*rate.CacheWrite +
		float64(u.CacheWrite1hTokens)/perMillion*rate.CacheWrite1h +
		float64(u.ReasoningTokens)/perMillion*rate.Reasoning
	return usd, !math.IsNaN(usd) && !math.IsInf(usd, 0)
}

// UnmarshalJSON accepts the models.dev nested "tier" form, e.g.
// {"input":10,"tier":{"type":"context","size":272000}}, as well as the
// flatter harness form {"input":10,"threshold":272000}.
func (t *PriceTier) UnmarshalJSON(data []byte) error {
	type tierAlias PriceTier
	var raw struct {
		*tierAlias
		Tier struct {
			Type string `json:"type"`
			Size int    `json:"size"`
		} `json:"tier"`
	}
	raw.tierAlias = (*tierAlias)(t)
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	if raw.Tier.Size > 0 {
		t.Threshold = raw.Tier.Size
	}
	return nil
}

// MarshalJSON serializes a tier as the flatter harness form.
func (t PriceTier) MarshalJSON() ([]byte, error) {
	type tierAlias PriceTier
	return json.Marshal((tierAlias)(t))
}
