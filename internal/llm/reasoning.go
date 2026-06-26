package llm

import "strings"

// ReasoningConfig is the provider-neutral user request for model reasoning
// controls. Empty fields mean the provider default is used.
type ReasoningConfig struct {
	// Profile is the portable harness -> model-proxy reasoning level. Empty and
	// "default" mean provider default. Concrete providers should receive this
	// only after the model proxy maps it to provider-specific controls.
	Profile      string `json:"profile,omitempty"`
	Effort       string `json:"effort,omitempty"`
	Enabled      *bool  `json:"enabled,omitempty"`
	BudgetTokens *int   `json:"budget_tokens,omitempty"`
	Summary      string `json:"summary,omitempty"`
}

// Empty reports whether no reasoning controls were requested.
func (r ReasoningConfig) Empty() bool {
	return strings.TrimSpace(r.Profile) == "" && strings.TrimSpace(r.Effort) == "" && r.Enabled == nil && r.BudgetTokens == nil && strings.TrimSpace(r.Summary) == ""
}

// ReasoningOption is one models.dev reasoning parameter supported by a model.
// Known Type values include "effort", "budget_tokens", and "toggle"; unknown
// future values are preserved so configs can round-trip catalog data.
type ReasoningOption struct {
	Type   string   `json:"type"`
	Values []string `json:"values,omitempty"`
	Min    *int     `json:"min,omitempty"`
	Max    *int     `json:"max,omitempty"`
}

// ReasoningInfo describes whether a model supports reasoning controls and, when
// available, which parameter shapes and effort values are accepted.
type ReasoningInfo struct {
	Supported bool              `json:"supported"`
	Options   []ReasoningOption `json:"options,omitempty"`
}

// EffortValues returns the configured effort values, if the catalog knows them.
func (r *ReasoningInfo) EffortValues() ([]string, bool) {
	if r == nil {
		return nil, false
	}
	for _, opt := range r.Options {
		if opt.Type == "effort" {
			return opt.Values, true
		}
	}
	return nil, false
}

// SupportsToggle reports whether an explicit reasoning on/off control is
// allowed by known model metadata.
func (r *ReasoningInfo) SupportsToggle() bool {
	if r == nil {
		return true
	}
	if !r.Supported {
		return false
	}
	for _, opt := range r.Options {
		if opt.Type == "toggle" {
			return true
		}
	}
	return len(r.Options) == 0
}

// BudgetTokenRange returns the catalog range for budget_tokens, if one is
// known. A nil min or max means the catalog omitted that side of the range.
func (r *ReasoningInfo) BudgetTokenRange() (min *int, max *int, ok bool) {
	if r == nil {
		return nil, nil, false
	}
	for _, opt := range r.Options {
		if opt.Type == "budget_tokens" {
			return opt.Min, opt.Max, true
		}
	}
	return nil, nil, false
}

// SupportsBudgetTokens reports whether a reasoning budget token count is
// allowed by known model metadata.
func (r *ReasoningInfo) SupportsBudgetTokens(tokens int) bool {
	if r == nil {
		return true
	}
	if !r.Supported {
		return false
	}
	min, max, known := r.BudgetTokenRange()
	if !known {
		return len(r.Options) == 0
	}
	if min != nil && tokens < *min {
		return false
	}
	if max != nil && tokens > *max {
		return false
	}
	return true
}

// SupportsEffort reports whether effort is allowed by known model metadata. An
// empty option list means the catalog knows the model supports reasoning but
// does not enumerate specific parameter details, so effort is treated as
// provider-defined rather than rejected.
func (r *ReasoningInfo) SupportsEffort(effort string) bool {
	if r == nil {
		return true
	}
	if !r.Supported {
		return false
	}
	values, known := r.EffortValues()
	if !known {
		return len(r.Options) == 0
	}
	if len(values) == 0 {
		return true
	}
	effort = strings.ToLower(strings.TrimSpace(effort))
	for _, value := range values {
		if strings.ToLower(value) == effort {
			return true
		}
	}
	return false
}

// Clone returns an independent copy safe to store in the registry.
func (r *ReasoningInfo) Clone() *ReasoningInfo {
	if r == nil {
		return nil
	}
	out := &ReasoningInfo{Supported: r.Supported}
	if len(r.Options) > 0 {
		out.Options = append([]ReasoningOption(nil), r.Options...)
		for i := range out.Options {
			out.Options[i].Values = append([]string(nil), out.Options[i].Values...)
			if out.Options[i].Min != nil {
				v := *out.Options[i].Min
				out.Options[i].Min = &v
			}
			if out.Options[i].Max != nil {
				v := *out.Options[i].Max
				out.Options[i].Max = &v
			}
		}
	}
	return out
}
