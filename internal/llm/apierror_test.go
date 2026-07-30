package llm

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
)

func TestParseErrorResponseCapturesUpstreamRequestIDWithoutChangingErrorText(t *testing.T) {
	resp := &http.Response{
		StatusCode: http.StatusBadRequest,
		Header:     http.Header{"X-Request-Id": []string{"upstream-123"}},
		Body:       io.NopCloser(strings.NewReader(`{"error":{"type":"invalid_request","message":"bad input"}}`)),
	}
	apiErr, errType, _ := ParseErrorResponse(resp)
	apiErr.Code = errType
	if apiErr.Diagnostic == nil || apiErr.Diagnostic.UpstreamRequestID != "upstream-123" {
		t.Fatalf("diagnostic = %+v", apiErr.Diagnostic)
	}
	if got := apiErr.Error(); got != "api error 400 (invalid_request): bad input" {
		t.Fatalf("Error() = %q", got)
	}
}

func TestParseErrorResponseBoundsUpstreamRequestID(t *testing.T) {
	resp := &http.Response{
		StatusCode: http.StatusBadRequest,
		Header:     http.Header{"Request-Id": []string{strings.Repeat("x", 300)}},
		Body:       io.NopCloser(strings.NewReader(`bad`)),
	}
	apiErr, _, _ := ParseErrorResponse(resp)
	if apiErr.Diagnostic != nil {
		t.Fatalf("request id diagnostic = %+v, want malicious long id dropped", apiErr.Diagnostic)
	}
}

func TestParseErrorResponseAcceptsNumericCodeAndOpenRouterMetadata(t *testing.T) {
	resp := &http.Response{
		StatusCode: http.StatusBadGateway,
		Header:     http.Header{"X-Generation-Id": []string{"gen-123"}},
		Body: io.NopCloser(strings.NewReader(
			`{"error":{"code":502,"message":"provider failed","metadata":{"error_type":"provider_unavailable","raw":"capacity timeout"}}}`,
		)),
	}
	apiErr, errType, errCode := ParseErrorResponse(resp)
	if errType != "" || errCode != "provider_unavailable" || apiErr.Message != "provider failed" {
		t.Fatalf("parsed error = type %q code %q error %+v", errType, errCode, apiErr)
	}
	if apiErr.Diagnostic == nil || apiErr.Diagnostic.UpstreamRequestID != "gen-123" {
		t.Fatalf("diagnostic = %+v", apiErr.Diagnostic)
	}
	if !strings.Contains(string(apiErr.ResponsePayload), `"code":502`) ||
		!strings.Contains(string(apiErr.ResponsePayload), `"raw":"capacity timeout"`) {
		t.Fatalf("response payload = %s", apiErr.ResponsePayload)
	}
}

func TestSafeResponsePayloadRedactsContentAndKeepsErrorDetails(t *testing.T) {
	raw := []byte(`{
		"error":{"code":502,"message":"provider timed out","metadata":{"raw":"socket timeout"}},
		"choices":[{"delta":{"content":"private output","reasoning":"private thought","tool_calls":[{"function":{"arguments":"{\"secret\":true}"}}]}}],
		"request":{"messages":["private prompt"]},
		"image_url":"data:image/png;base64,private"
	}`)
	payload := SafeResponsePayload(raw)
	text := string(payload)
	for _, secret := range []string{"private output", "private thought", `{\"secret\":true}`, "private prompt", "base64,private"} {
		if strings.Contains(text, secret) {
			t.Fatalf("payload leaked %q: %s", secret, text)
		}
	}
	for _, want := range []string{"provider timed out", "socket timeout", `"code":502`, `"[redacted]"`} {
		if !strings.Contains(text, want) {
			t.Fatalf("payload %s missing %q", text, want)
		}
	}
	if !json.Valid([]byte(payload)) {
		t.Fatalf("payload is not valid JSON: %s", payload)
	}
}

func TestSafeResponsePayloadDoesNotPersistMalformedBody(t *testing.T) {
	const secret = "private malformed model output"
	payload := SafeResponsePayload([]byte(`{"content":"` + secret))
	if strings.Contains(string(payload), secret) {
		t.Fatalf("malformed payload leaked content: %s", payload)
	}
	for _, want := range []string{`"_capture":"invalid_json"`, `"sha256":`, `"bytes":`} {
		if !strings.Contains(string(payload), want) {
			t.Fatalf("payload %s missing %s", payload, want)
		}
	}
}

func TestSafeResponsePayloadIsBounded(t *testing.T) {
	raw, err := json.Marshal(map[string]any{
		"error": map[string]any{
			"message": strings.Repeat("m", 20<<10),
			"metadata": map[string]any{
				"raw":      strings.Repeat("r", 20<<10),
				"detail_1": strings.Repeat("a", 20<<10),
				"detail_2": strings.Repeat("b", 20<<10),
				"detail_3": strings.Repeat("c", 20<<10),
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	payload := SafeResponsePayload(raw)
	if len(payload) > maxSafeResponsePayloadBytes {
		t.Fatalf("payload bytes = %d, want <= %d", len(payload), maxSafeResponsePayloadBytes)
	}
	if !strings.Contains(string(payload), `"_capture":"truncated"`) {
		t.Fatalf("payload = %s, want truncated summary", payload)
	}
}

func TestNormalizeUpstreamRequestID(t *testing.T) {
	valid := []string{
		"550e8400-e29b-41d4-a716-446655440000",
		"req_01J1234567890ABCDEFGHJKLMN",
		"request-123.example:456",
	}
	for _, value := range valid {
		if got := NormalizeUpstreamRequestID(value); got != value {
			t.Errorf("NormalizeUpstreamRequestID(%q) = %q", value, got)
		}
	}
	invalid := []string{
		"prompt text",
		"../../private/path",
		"request\x1b[31m",
		"request\ninjected",
		"request\r\ninjected",
		strings.Repeat("A", 80),
		strings.Repeat("x", 129),
	}
	for _, value := range invalid {
		if got := NormalizeUpstreamRequestID(value); got != "" {
			t.Errorf("NormalizeUpstreamRequestID(%q) = %q, want empty", value, got)
		}
	}
}
