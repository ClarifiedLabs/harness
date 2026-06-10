package llm

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"harness/internal/retry"
)

// APIError is the shared provider error surfaced to the agent loop (design §5.5).
// Both dialects construct it. Retryable marks the transport/status classes the
// provider retry loop may retry; RetryAfter carries a parsed Retry-After header
// (0 when absent) honored as a backoff floor.
type APIError struct {
	StatusCode int
	Code       string // provider error code/type if parseable
	Message    string
	Retryable  bool
	RetryAfter time.Duration
}

func (e *APIError) Error() string {
	switch {
	case e.Code != "" && e.Message != "":
		return fmt.Sprintf("api error %d (%s): %s", e.StatusCode, e.Code, e.Message)
	case e.Message != "":
		return fmt.Sprintf("api error %d: %s", e.StatusCode, e.Message)
	default:
		return fmt.Sprintf("api error %d", e.StatusCode)
	}
}

// ParseErrorResponse builds an APIError from a non-2xx HTTP response. Every
// dialect shares the same shape: status-class retryability, a Retry-After
// backoff floor, a best-effort decode of the {"error":{type,code,message}}
// envelope, and the trimmed raw body as the message fallback. The envelope's
// type and code are returned separately because picking APIError.Code is the
// one spot the dialects genuinely differ (Anthropic and Chat Completions use
// type; Responses prefers code).
func ParseErrorResponse(resp *http.Response) (apiErr *APIError, errType, errCode string) {
	apiErr = &APIError{
		StatusCode: resp.StatusCode,
		Retryable:  retry.RetryableStatus(resp.StatusCode),
		RetryAfter: retry.ParseRetryAfter(resp.Header.Get("Retry-After")),
	}

	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	var env struct {
		Error *struct {
			Type    string `json:"type"`
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if json.Unmarshal(body, &env) == nil && env.Error != nil {
		apiErr.Message = env.Error.Message
		errType = env.Error.Type
		errCode = env.Error.Code
	}
	if apiErr.Message == "" {
		apiErr.Message = strings.TrimSpace(string(body))
	}
	return apiErr, errType, errCode
}
