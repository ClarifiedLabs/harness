// Package protocol defines the small HTTP wire contract between harness and
// harness-model-proxy.
package protocol

import (
	"errors"
	"time"

	"harness/internal/llm"
	"harness/internal/modelproxy"
)

const (
	DefaultURL = "http://127.0.0.1:8765"

	ContentTypeNDJSON = "application/x-ndjson"

	ErrCodePreviousResponseUnavailable = "previous_response_unavailable"
	ErrCodeContinuationUnsupported     = "continuation_unsupported"
)

type StreamRequest struct {
	TargetID         string                    `json:"target_id"`
	Request          llm.Request               `json:"request"`
	ReasoningProfile string                    `json:"reasoning_profile,omitempty"`
	CallerAttempt    *modelproxy.CallerAttempt `json:"caller_attempt,omitempty"`
}

type TokenCountRequest struct {
	TargetID string      `json:"target_id"`
	Request  llm.Request `json:"request"`
}

type TokenCountResponse struct {
	InputTokens int                      `json:"input_tokens"`
	Source      string                   `json:"source,omitempty"`
	Scope       llm.InputTokenCountScope `json:"scope,omitempty"`
}

type CompactRequest struct {
	TargetID      string                    `json:"target_id"`
	Request       llm.Request               `json:"request"`
	CallerAttempt *modelproxy.CallerAttempt `json:"caller_attempt,omitempty"`
}

type CompactResponse struct {
	Context  llm.CompactedContext `json:"context"`
	Attempts []llm.AttemptEvent   `json:"attempts,omitempty"`
	Error    *Error               `json:"error,omitempty"`
}

type StreamEnvelope struct {
	Event   *llm.StreamEvent  `json:"event,omitempty"`
	Error   *Error            `json:"error,omitempty"`
	Attempt *llm.AttemptEvent `json:"attempt,omitempty"`
}

type Error struct {
	StatusCode      int                     `json:"status_code,omitempty"`
	Code            string                  `json:"code,omitempty"`
	Message         string                  `json:"message,omitempty"`
	ResponsePayload llm.DiagnosticPayload   `json:"response_payload,omitempty"`
	Retryable       bool                    `json:"retryable,omitempty"`
	RetryAfterMS    int64                   `json:"retry_after_ms,omitempty"`
	Diagnostic      *llm.APIErrorDiagnostic `json:"diagnostic,omitempty"`
}

func ErrorFrom(err error) *Error {
	if err == nil {
		return nil
	}
	var apiErr *llm.APIError
	if errors.As(err, &apiErr) {
		return &Error{
			StatusCode:      apiErr.StatusCode,
			Code:            apiErr.Code,
			Message:         apiErr.Message,
			ResponsePayload: apiErr.ResponsePayload,
			Retryable:       apiErr.Retryable,
			RetryAfterMS:    apiErr.RetryAfter.Milliseconds(),
			Diagnostic:      apiErr.Diagnostic,
		}
	}
	return &Error{Message: err.Error(), Retryable: true}
}

func (e *Error) APIError() *llm.APIError {
	if e == nil {
		return nil
	}
	return &llm.APIError{
		StatusCode:      e.StatusCode,
		Code:            e.Code,
		Message:         e.Message,
		ResponsePayload: e.ResponsePayload,
		Retryable:       e.Retryable,
		RetryAfter:      time.Duration(e.RetryAfterMS) * time.Millisecond,
		Diagnostic:      e.Diagnostic,
	}
}

type SteerRequest struct {
	TargetID   string              `json:"target_id"`
	Submission llm.SteerSubmission `json:"submission"`
}
