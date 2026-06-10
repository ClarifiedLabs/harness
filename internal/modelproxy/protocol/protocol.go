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
