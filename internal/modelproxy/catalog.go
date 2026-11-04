package modelproxy

import (
	"harness/internal/llm"
	"time"
)

type Catalog struct {
	Targets []Target `json:"targets"`
	// Pricing dates the catalog's prices against the provider-direct, models.dev,
	// or configured data that supplied them. Nil when the proxy cannot date them.
	Pricing *PricingInfo `json:"pricing,omitempty"`
}

// PricingInfo describes how fresh a catalog's prices are across all sources.
type PricingInfo struct {
	// SourceDate is the oldest update time among sources that supplied prices.
	SourceDate time.Time `json:"source_date"`
	// MaxAgeSeconds is the proxy's configured models.dev refresh interval in
	// seconds. Prices older than this are stale. Zero when no TTL is configured.
	MaxAgeSeconds int64 `json:"max_age_seconds,omitempty"`
	// ExpiresAt is the earliest expiry among the catalog sources that supplied
	// prices. It permits mixed provider and models.dev refresh intervals.
	ExpiresAt time.Time `json:"expires_at,omitempty"`
}

// Stale reports whether the catalog's pricing is older than its refresh
// interval as of now. ExpiresAt takes precedence for mixed source intervals. It
// returns false when no expiry basis is known.
func (p *PricingInfo) Stale(now time.Time) bool {
	if p != nil && !p.ExpiresAt.IsZero() {
		return now.After(p.ExpiresAt)
	}
	if p == nil || p.SourceDate.IsZero() || p.MaxAgeSeconds <= 0 {
		return false
	}
	return now.Sub(p.SourceDate) > time.Duration(p.MaxAgeSeconds)*time.Second
}

// UsageReport is the aggregate token and cost usage served by GET /v1/usage. It
// captures every priced request the proxy has handled this run, including
// delegated child-agent spend that is otherwise invisible to a parent session.
type UsageReport struct {
	Instance string        `json:"instance"`
	Since    time.Time     `json:"since"`
	Models   []ModelUsage  `json:"models"`
	Budget   *BudgetReport `json:"budget,omitempty"`
}

// BudgetReport describes the current authenticated API key's cost-budget window.
type BudgetReport struct {
	LimitUSD      float64   `json:"limit_usd"`
	PeriodSeconds int64     `json:"period_seconds"`
	WindowStart   time.Time `json:"window_start"`
	WindowEnd     time.Time `json:"window_end"`
	SpentUSD      float64   `json:"spent_usd"`
	RemainingUSD  float64   `json:"remaining_usd"`
}

// ModelUsage is the accumulated usage for a single provider:model pair.
type ModelUsage struct {
	TargetID           string  `json:"target_id"`
	TargetName         string  `json:"target_name,omitempty"`
	Requests           int64   `json:"requests"`
	InputTokens        int64   `json:"input_tokens"`
	OutputTokens       int64   `json:"output_tokens"`
	CacheReadTokens    int64   `json:"cache_read_tokens"`
	CacheWriteTokens   int64   `json:"cache_write_tokens"`
	CacheWrite1hTokens int64   `json:"cache_write_1h_tokens"`
	ReasoningTokens    int64   `json:"reasoning_tokens"`
	CostUSD            float64 `json:"cost_usd"`
}

type Target struct {
	IncrementalTools bool     `json:"incremental_tools,omitempty"`
	NativeSteering   bool     `json:"native_steering,omitempty"`
	AsyncTools       bool     `json:"async_tools,omitempty"`
	ReasoningUpdates bool     `json:"reasoning_updates,omitempty"`
	ID               string   `json:"id"`
	Aliases          []string `json:"aliases,omitempty"`
	DisplayName      string   `json:"display_name,omitempty"`
	ProviderLabel    string   `json:"provider_label,omitempty"`
	// Profile carries the provider config's profile (llm.ProviderConfig.Profile)
	// so clients can apply profile-level defaults without name matching.
	Profile                     string                               `json:"profile,omitempty"`
	ModelLabel                  string                               `json:"model_label,omitempty"`
	ContextWindow               int                                  `json:"context_window,omitempty"`
	OutputLimit                 int                                  `json:"output_limit,omitempty"`
	InputModalities             []string                             `json:"input_modalities,omitempty"`
	ServerTools                 []string                             `json:"server_tools,omitempty"`
	BaseTargetID                string                               `json:"base_target_id,omitempty"`
	Variant                     string                               `json:"variant,omitempty"`
	APIType                     string                               `json:"api_type,omitempty"`
	ContinuationStateful        bool                                 `json:"continuation_stateful,omitempty"`
	NativeCompaction            bool                                 `json:"native_compaction,omitempty"`
	Prewarm                     bool                                 `json:"prewarm,omitempty"`
	Price                       llm.Price                            `json:"price,omitempty"`
	Reasoning                   bool                                 `json:"reasoning,omitempty"`
	ReasoningReplayDomain       string                               `json:"reasoning_replay_domain,omitempty"`
	SupportsExperimentalContext bool                                 `json:"supports_experimental_context,omitempty"`
	ContextToolDefinitions      map[string]llm.ContextToolDefinition `json:"context_tool_definitions,omitempty"`
}

// CallerAttempt carries only the caller's inherited retry/continuation origin.
// Target identity and purpose remain server-resolved. Dialect and proxy retries
// may replace this origin; it is not an override applied to each emitted fact.
type CallerAttempt struct {
	Cause      llm.AttemptCause `json:"cause"`
	RetryLayer llm.RetryLayer   `json:"retry_layer"`
}

func (a CallerAttempt) Normalized() CallerAttempt {
	event := llm.NormalizeAttemptEvent(llm.AttemptEvent{AttemptMetadata: llm.AttemptMetadata{
		Cause: a.Cause, RetryLayer: a.RetryLayer,
	}})
	return CallerAttempt{Cause: event.Cause, RetryLayer: event.RetryLayer}
}
