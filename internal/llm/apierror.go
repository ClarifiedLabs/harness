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

// APIErrorStage identifies where a proxied model request failed. Values are
// intentionally provider-neutral and stable because they cross the model-proxy
// protocol boundary.
type APIErrorStage string

const (
	APIErrorStageProxyDecode     APIErrorStage = "proxy_decode"
	APIErrorStageProxyResolve    APIErrorStage = "proxy_resolve"
	APIErrorStageProxyPrepare    APIErrorStage = "proxy_prepare"
	APIErrorStageProviderRuntime APIErrorStage = "provider_runtime"
	APIErrorStageUpstreamHTTP    APIErrorStage = "upstream_http"
	APIErrorStageUpstreamStream  APIErrorStage = "upstream_stream"

	CompatibilityCategoryMultimodalToolResultRejected = "multimodal_tool_result_rejected"
	CompatibilityConfidenceLikely                     = "likely"

	MultimodalStrategyAnthropicToolResultContent = "anthropic_tool_result_content"
	MultimodalStrategyOpenAIToolThenUserImage    = "openai_tool_then_user_image"
	MultimodalStrategyResponsesOutputThenImage   = "responses_output_then_user_image"
)

// APIErrorDiagnostic carries optional request correlation and compatibility
// evidence. It is safe to persist: MultimodalShape contains only bounded shape
// metadata and aggregate fingerprints, never model-visible text or image data.
type APIErrorDiagnostic struct {
	Stage             APIErrorStage            `json:"stage,omitempty"`
	ProxyRequestID    uint64                   `json:"proxy_request_id,omitempty"`
	TargetID          string                   `json:"target_id,omitempty"`
	Provider          string                   `json:"provider,omitempty"`
	APIType           string                   `json:"api_type,omitempty"`
	Model             string                   `json:"model,omitempty"`
	TraceID           string                   `json:"trace_id,omitempty"`
	SpanID            string                   `json:"span_id,omitempty"`
	UpstreamRequestID string                   `json:"upstream_request_id,omitempty"`
	Compatibility     *CompatibilityDiagnostic `json:"compatibility,omitempty"`
	MultimodalShape   *MultimodalRequestShape  `json:"multimodal_shape,omitempty"`
}

// CompatibilityDiagnostic describes an observed endpoint compatibility
// rejection. Reason and remediation are bounded, proxy-authored values rather
// than raw provider text.
type CompatibilityDiagnostic struct {
	Category    string `json:"category"`
	Reason      string `json:"reason"`
	Confidence  string `json:"confidence"`
	Remediation string `json:"remediation,omitempty"`
	Strategy    string `json:"strategy,omitempty"`
}

// MultimodalRequestShape summarizes image-bearing tool results without carrying
// result IDs, names, paths, text, arguments, or image data.
type MultimodalRequestShape struct {
	Strategy            string           `json:"strategy"`
	ToolResultCount     int              `json:"tool_result_count"`
	ImageCount          int              `json:"image_count"`
	MIMETypes           []string         `json:"mime_types,omitempty"`
	Details             []string         `json:"details,omitempty"`
	EncodedBytes        int64            `json:"encoded_bytes,omitempty"`
	DecodedBytes        int64            `json:"decoded_bytes,omitempty"`
	Dimensions          []ImageDimension `json:"dimensions,omitempty"`
	ResultIDsSHA256     string           `json:"result_ids_sha256,omitempty"`
	ImagePayloadsSHA256 string           `json:"image_payloads_sha256,omitempty"`
}

// ImageDimension is one bounded, distinct dimension pair in a multimodal shape.
type ImageDimension struct {
	Width  int `json:"width"`
	Height int `json:"height"`
	Count  int `json:"count,omitempty"`
}

// APIError is the shared provider error surfaced to the agent loop (design §5.5).
// Both dialects construct it. Retryable marks the transport/status classes the
// provider retry loop may retry; RetryAfter carries a parsed Retry-After header
// (0 when absent) honored as a backoff floor. Diagnostic is optional so errors
// from older providers and model proxies retain their original behavior.
type APIError struct {
	StatusCode int
	Code       string // provider error code/type if parseable
	Message    string
	Retryable  bool
	RetryAfter time.Duration
	Diagnostic *APIErrorDiagnostic
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
	if requestID := upstreamRequestID(resp.Header); requestID != "" {
		apiErr.Diagnostic = &APIErrorDiagnostic{UpstreamRequestID: requestID}
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

func upstreamRequestID(header http.Header) string {
	for _, name := range []string{
		"X-Request-ID",
		"Request-ID",
		"OpenAI-Request-ID",
		"Anthropic-Request-ID",
		"X-Amzn-RequestId",
		"X-Amz-Request-ID",
		"X-Goog-Request-ID",
		"X-MS-Request-ID",
	} {
		if value := NormalizeUpstreamRequestID(header.Get(name)); value != "" {
			return value
		}
	}
	return ""
}

// NormalizeUpstreamRequestID returns a conservative identifier safe for
// diagnostics and logs. Provider-controlled values that resemble content,
// paths, controls, or opaque payloads are dropped.
func NormalizeUpstreamRequestID(value string) string {
	for i := 0; i < len(value); i++ {
		if value[i] < 0x20 || value[i] == 0x7f {
			return ""
		}
	}
	value = strings.TrimSpace(value)
	if value == "" || len(value) > 128 {
		return ""
	}
	for i := 0; i < len(value); i++ {
		c := value[i]
		valid := c >= 'a' && c <= 'z' ||
			c >= 'A' && c <= 'Z' ||
			c >= '0' && c <= '9' ||
			(i > 0 && (c == '.' || c == '_' || c == ':' || c == '-'))
		if !valid {
			return ""
		}
	}
	if len(value) >= 64 && looksLikeOpaqueBase64ID(value) &&
		!strings.HasPrefix(value, "req_") && !strings.HasPrefix(value, "request_") {
		return ""
	}
	return value
}

func looksLikeOpaqueBase64ID(value string) bool {
	for i := 0; i < len(value); i++ {
		c := value[i]
		if c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-' || c == '_' {
			continue
		}
		return false
	}
	return true
}
