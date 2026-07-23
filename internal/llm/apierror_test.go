package llm

import (
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
