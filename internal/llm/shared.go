package llm

import (
	"encoding/json"
	"net/http"
)

// ErrorResultPrefix marks a failed tool result. OpenAI Chat Completions tool
// messages and Responses function_call_output items have no is_error field, so
// error results carry this prefix in the content/output string (design §4).
const ErrorResultPrefix = "ERROR: "

// EmptyArgs is the canonical serialization for a tool call with no arguments.
// OpenAI requires function.arguments to be a JSON string, never "" (design §4).
const EmptyArgs = "{}"

// RawObjectOrNil returns raw unchanged unless it is empty or the JSON literal
// "null", in which case it returns nil so the field is omitted from the wire
// body.
func RawObjectOrNil(raw json.RawMessage) json.RawMessage {
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	return raw
}

// ImageDataURL renders a content block's image as a base64 data URL.
func ImageDataURL(b ContentBlock) string {
	return "data:" + b.ImageMediaType + ";base64," + b.ImageData
}

// RetryableErrorCode reports whether a provider error code denotes a transient
// condition the retry loop may re-request. Shared by the OpenAI-compatible
// dialects (Chat Completions and Responses).
func RetryableErrorCode(code string) bool {
	switch code {
	case "server_error", "rate_limit_exceeded", "rate_limit_error":
		return true
	}
	return false
}

// ParseErrorResponseByType maps a non-2xx HTTP response onto an *APIError whose
// Code is the envelope's type field. Shared by the Chat Completions and
// Anthropic dialects; the Responses dialect prefers the envelope's code field
// and keeps its own wrapper.
func ParseErrorResponseByType(resp *http.Response) *APIError {
	apiErr, errType, _ := ParseErrorResponse(resp)
	apiErr.Code = errType
	return apiErr
}
