package llm

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"errors"
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
	APIErrorStageUpstreamConnect APIErrorStage = "upstream_connect"
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
	ProxyInstanceID   string                   `json:"proxy_instance_id,omitempty"`
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
	StatusCode      int
	Code            string // provider error code/type if parseable
	Message         string
	ResponsePayload DiagnosticPayload // bounded, redacted upstream response fragment
	Retryable       bool
	RetryAfter      time.Duration
	// Stage marks where the request failed. It is set by the connect loop for
	// transport failures (APIErrorStageUpstreamConnect); HTTP/stream failures
	// are classified at event-emission time instead.
	Stage      APIErrorStage
	Diagnostic *APIErrorDiagnostic
}

// DiagnosticPayload is valid, compact JSON stored as a comparable string so
// model-request lifecycle events remain comparable. It marshals as the embedded
// JSON value rather than as an escaped JSON string.
type DiagnosticPayload string

func (p DiagnosticPayload) MarshalJSON() ([]byte, error) {
	if p == "" {
		return []byte("null"), nil
	}
	data := []byte(p)
	if !json.Valid(data) {
		return nil, fmt.Errorf("invalid diagnostic payload")
	}
	return data, nil
}

func (p *DiagnosticPayload) UnmarshalJSON(data []byte) error {
	if p == nil {
		return fmt.Errorf("nil diagnostic payload")
	}
	if bytes.Equal(bytes.TrimSpace(data), []byte("null")) {
		*p = ""
		return nil
	}
	if !json.Valid(data) {
		return fmt.Errorf("invalid diagnostic payload")
	}
	*p = DiagnosticPayload(string(data))
	return nil
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
	apiErr.ResponsePayload = SafeResponsePayload(body)
	var env struct {
		Error *struct {
			Type     json.RawMessage `json:"type"`
			Code     json.RawMessage `json:"code"`
			Status   string          `json:"status"`
			Message  string          `json:"message"`
			Metadata *struct {
				ErrorType string `json:"error_type"`
			} `json:"metadata"`
		} `json:"error"`
	}
	if json.Unmarshal(body, &env) == nil && env.Error != nil {
		apiErr.Message = env.Error.Message
		errType = JSONScalarString(env.Error.Type)
		if errType == "" {
			errType = env.Error.Status
		}
		errCode = JSONScalarString(env.Error.Code)
		if env.Error.Metadata != nil && env.Error.Metadata.ErrorType != "" {
			errCode = env.Error.Metadata.ErrorType
		}
	}
	if apiErr.Message == "" {
		apiErr.Message = strings.TrimSpace(string(body))
	}
	return apiErr, errType, errCode
}

func upstreamRequestID(header http.Header) string {
	for _, name := range []string{
		"X-Generation-Id",
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

// WithUpstreamRequestID attaches a safe provider response identifier to an API
// error without mutating the original. It is used for errors that arrive after
// an HTTP 2xx response has already opened a stream.
func WithUpstreamRequestID(err error, header http.Header) error {
	requestID := upstreamRequestID(header)
	if err == nil || requestID == "" {
		return err
	}
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		return err
	}
	copyError := *apiErr
	if apiErr.Diagnostic != nil {
		copyDiagnostic := *apiErr.Diagnostic
		copyError.Diagnostic = &copyDiagnostic
	} else {
		copyError.Diagnostic = &APIErrorDiagnostic{}
	}
	copyError.Diagnostic.UpstreamRequestID = requestID
	return &copyError
}

const (
	maxSafeResponsePayloadBytes = 16 << 10
	maxSafeResponsePreviewBytes = 4 << 10
	maxSafeResponseStringBytes  = 4 << 10
)

// NewResponseDecodeError preserves a bounded, redacted copy of a provider
// response fragment alongside its decode failure. The payload is diagnostics
// only and never becomes model-visible content.
func NewResponseDecodeError(prefix string, cause error, raw []byte) *APIError {
	message := strings.TrimSpace(prefix)
	if cause != nil {
		if message != "" {
			message += ": "
		}
		message += cause.Error()
	}
	return &APIError{
		Message:         message,
		ResponsePayload: SafeResponsePayload(raw),
	}
}

// JSONScalarString returns the text form of a JSON string or number. Provider
// error codes commonly vary between those two forms; null and composite values
// return an empty string.
func JSONScalarString(raw json.RawMessage) string {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || bytes.Equal(raw, []byte("null")) {
		return ""
	}
	var value string
	if json.Unmarshal(raw, &value) == nil {
		return value
	}
	var number json.Number
	if json.Unmarshal(raw, &number) == nil {
		return number.String()
	}
	return ""
}

// SafeResponsePayload returns a compact JSON diagnostic for an upstream
// response fragment. Common prompt, generated-content, tool-argument, and
// binary fields are redacted recursively. Invalid JSON is represented by
// length and digest because it cannot be inspected safely by field name.
func SafeResponsePayload(raw []byte) DiagnosticPayload {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 {
		return ""
	}
	digest := fmt.Sprintf("%x", sha256.Sum256(raw))

	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var value any
	if err := dec.Decode(&value); err != nil {
		return marshalResponsePayloadSummary("invalid_json", len(raw), digest, "")
	}
	var extra any
	if err := dec.Decode(&extra); err != io.EOF {
		return marshalResponsePayloadSummary("multiple_json_values", len(raw), digest, "")
	}

	safe := redactResponsePayloadValue(value, false)
	data, err := json.Marshal(safe)
	if err != nil {
		return marshalResponsePayloadSummary("marshal_failed", len(raw), digest, "")
	}
	if len(data) <= maxSafeResponsePayloadBytes {
		return DiagnosticPayload(data)
	}
	preview := string(data[:maxSafeResponsePreviewBytes])
	return marshalResponsePayloadSummary("truncated", len(raw), digest, preview)
}

func marshalResponsePayloadSummary(kind string, size int, digest, preview string) DiagnosticPayload {
	summary := map[string]any{
		"_capture": kind,
		"bytes":    size,
		"sha256":   digest,
	}
	if preview != "" {
		summary["preview"] = preview
	}
	data, _ := json.Marshal(summary)
	return DiagnosticPayload(data)
}

func redactResponsePayloadValue(value any, inError bool) any {
	switch value := value.(type) {
	case map[string]any:
		out := make(map[string]any, len(value))
		for key, child := range value {
			normalized := strings.ToLower(strings.ReplaceAll(key, "-", "_"))
			childInError := inError || normalized == "error"
			if redactResponsePayloadKey(normalized, inError) {
				out[key] = "[redacted]"
				continue
			}
			out[key] = redactResponsePayloadValue(child, childInError)
		}
		return out
	case []any:
		out := make([]any, len(value))
		for i, child := range value {
			out[i] = redactResponsePayloadValue(child, inError)
		}
		return out
	case string:
		if strings.HasPrefix(strings.ToLower(strings.TrimSpace(value)), "data:") {
			return "[redacted data URL]"
		}
		if len(value) > maxSafeResponseStringBytes {
			return value[:maxSafeResponseStringBytes] + fmt.Sprintf("… [truncated %d bytes]", len(value)-maxSafeResponseStringBytes)
		}
		return value
	default:
		return value
	}
}

func redactResponsePayloadKey(key string, inError bool) bool {
	switch key {
	case "api_key", "authorization", "credentials", "password", "secret", "token",
		"access_token", "refresh_token", "id_token",
		"prompt", "messages", "request", "request_body", "body",
		"arguments", "partial_arguments", "input", "input_text",
		"image", "image_url", "audio", "data":
		return true
	case "completion", "content", "generated_text", "output", "output_text",
		"reasoning", "reasoning_content", "refusal", "summary", "text", "thinking":
		return !inError
	default:
		return false
	}
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
