// Package protocol defines the small HTTP wire contract between harness and
// harness-model-proxy.
package protocol

import (
	"errors"
	"time"

	"harness/internal/llm"
)

const (
	DefaultURL = "http://127.0.0.1:8765"

	ContentTypeNDJSON = "application/x-ndjson"
)

type Catalog struct {
	Providers []Provider `json:"providers"`
	// Pricing dates the catalog's prices against the models.dev data that backed
	// the most recent setup/refresh, so clients can warn when prices are older
	// than the proxy's refresh interval. Nil when the proxy cannot date them.
	Pricing *PricingInfo `json:"pricing,omitempty"`
}

// PricingInfo describes how fresh a catalog's prices are. The proxy derives its
// catalog from provider config files at startup and never rebuilds it, so the
// served prices are only as current as the last setup/refresh.
type PricingInfo struct {
	// SourceDate is when the price data behind the catalog was last written
	// (the newest provider config modification time).
	SourceDate time.Time `json:"source_date"`
	// MaxAgeSeconds is the proxy's configured models.dev refresh interval in
	// seconds. Prices older than this are stale. Zero when no TTL is configured.
	MaxAgeSeconds int64 `json:"max_age_seconds,omitempty"`
}

// Stale reports whether the catalog's pricing is older than its refresh
// interval as of now. It returns false when the source date or max age is
// unknown, so callers never warn without a basis.
func (p *PricingInfo) Stale(now time.Time) bool {
	if p == nil || p.SourceDate.IsZero() || p.MaxAgeSeconds <= 0 {
		return false
	}
	return now.Sub(p.SourceDate) > time.Duration(p.MaxAgeSeconds)*time.Second
}

// UsageReport is the aggregate token and cost usage served by GET /v1/usage. It
// captures every priced request the proxy has handled this run, including
// delegated child-agent spend that is otherwise invisible to a parent session.
type UsageReport struct {
	Models []ModelUsage `json:"models"`
}

// ModelUsage is the accumulated usage for a single provider:model pair.
type ModelUsage struct {
	Provider         string  `json:"provider"`
	Model            string  `json:"model"`
	Requests         int64   `json:"requests"`
	InputTokens      int64   `json:"input_tokens"`
	OutputTokens     int64   `json:"output_tokens"`
	CacheReadTokens  int64   `json:"cache_read_tokens"`
	CacheWriteTokens int64   `json:"cache_write_tokens"`
	ReasoningTokens  int64   `json:"reasoning_tokens"`
	CostUSD          float64 `json:"cost_usd"`
}

type Provider struct {
	ID                string  `json:"id"`
	Name              string  `json:"name,omitempty"`
	APIType           string  `json:"api_type,omitempty"`
	ResponsesStateful bool    `json:"responses_stateful,omitempty"`
	Models            []Model `json:"models"`
}

type Model struct {
	ID            string             `json:"id"`
	Name          string             `json:"name,omitempty"`
	ContextWindow int                `json:"context_window,omitempty"`
	Price         llm.Price          `json:"price,omitempty"`
	Reasoning     *llm.ReasoningInfo `json:"reasoning,omitempty"`
}

type StreamRequest struct {
	Provider string      `json:"provider"`
	Request  llm.Request `json:"request"`
}

type StreamEnvelope struct {
	Event *llm.StreamEvent `json:"event,omitempty"`
	Error *Error           `json:"error,omitempty"`
}

type Error struct {
	StatusCode   int    `json:"status_code,omitempty"`
	Code         string `json:"code,omitempty"`
	Message      string `json:"message,omitempty"`
	Retryable    bool   `json:"retryable,omitempty"`
	RetryAfterMS int64  `json:"retry_after_ms,omitempty"`
}

func ErrorFrom(err error) *Error {
	if err == nil {
		return nil
	}
	var apiErr *llm.APIError
	if errors.As(err, &apiErr) {
		return &Error{
			StatusCode:   apiErr.StatusCode,
			Code:         apiErr.Code,
			Message:      apiErr.Message,
			Retryable:    apiErr.Retryable,
			RetryAfterMS: apiErr.RetryAfter.Milliseconds(),
		}
	}
	return &Error{Message: err.Error(), Retryable: true}
}

func (e *Error) APIError() *llm.APIError {
	if e == nil {
		return nil
	}
	return &llm.APIError{
		StatusCode: e.StatusCode,
		Code:       e.Code,
		Message:    e.Message,
		Retryable:  e.Retryable,
		RetryAfter: time.Duration(e.RetryAfterMS) * time.Millisecond,
	}
}
