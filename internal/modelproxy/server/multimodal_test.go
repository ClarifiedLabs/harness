package server

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"harness/internal/llm"
	"harness/internal/modelproxy/protocol"
)

const serverOnePixelPNG = "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAIAAACQd1PeAAAADElEQVR4nGP4z8AAAAMBAQDJ/pLvAAAAAElFTkSuQmCC"

func richImageRequest(payload string) llm.Request {
	return llm.Request{
		System: "private system prompt",
		Messages: []llm.Message{
			{Role: llm.RoleAssistant, Content: []llm.ContentBlock{{
				Kind:      llm.BlockToolUse,
				ToolUseID: "call-private",
				ToolName:  "view_image",
				ToolInput: []byte(`{"path":"/private/screen.png","nested":"{\"secret\":\"nested-secret\"}"}`),
			}}},
			{Role: llm.RoleUser, Content: []llm.ContentBlock{{
				Kind:        llm.BlockToolResult,
				ResultForID: "call-private",
				ResultText:  "private result text",
				ResultContent: []llm.ContentBlock{{
					Kind:              llm.BlockImage,
					ImageMediaType:    "image/png",
					ImageData:         payload,
					ImageDetail:       "high",
					ImageName:         "/private/screen.png",
					ImageWidth:        640,
					ImageHeight:       480,
					ImageBytes:        6,
					ImageEncodedBytes: len(payload),
				}},
			}}},
		},
	}
}

func TestRichToolResultShapeIsSafeAndDeterministic(t *testing.T) {
	payload := base64.StdEncoding.EncodeToString([]byte("secret"))
	req := richImageRequest(payload)
	got := richToolResultShape(req, "responses")
	if got == nil {
		t.Fatal("shape = nil")
	}
	if got.Strategy != llm.MultimodalStrategyResponsesOutputThenImage || got.ToolResultCount != 1 || got.ImageCount != 1 {
		t.Fatalf("shape = %+v", got)
	}
	if got.EncodedBytes != int64(len(payload)) || got.DecodedBytes != 6 || len(got.MIMETypes) != 1 || got.MIMETypes[0] != "image/png" || len(got.Dimensions) != 1 || got.Dimensions[0].Width != 640 {
		t.Fatalf("shape metadata = %+v", got)
	}
	if len(got.ResultIDsSHA256) != 64 || len(got.ImagePayloadsSHA256) != 64 || strings.Contains(got.ResultIDsSHA256, "call-private") || strings.Contains(got.ImagePayloadsSHA256, payload) {
		t.Fatalf("unsafe fingerprints = %+v", got)
	}
	second := richToolResultShape(req, "responses")
	if second.ResultIDsSHA256 != got.ResultIDsSHA256 || second.ImagePayloadsSHA256 != got.ImagePayloadsSHA256 {
		t.Fatalf("shape fingerprints not deterministic: %+v vs %+v", got, second)
	}
	changed := richToolResultShape(richImageRequest(base64.StdEncoding.EncodeToString([]byte("other!"))), "responses")
	if changed.ImagePayloadsSHA256 == got.ImagePayloadsSHA256 {
		t.Fatal("image fingerprint did not change with payload")
	}
}

func TestClassifyMultimodalToolResultRejectionIsTargeted(t *testing.T) {
	shape := &llm.MultimodalRequestShape{ImageCount: 1, Strategy: llm.MultimodalStrategyOpenAIToolThenUserImage}
	tests := []struct {
		name       string
		err        error
		wantReason string
	}{
		{"explicit unsupported image", &llm.APIError{StatusCode: 400, Code: "invalid_request", Message: "image input is not supported"}, "image_unsupported"},
		{"structured tool result unsupported", &llm.APIError{StatusCode: 400, Message: "tool_result object content is unsupported"}, "structured_tool_result_unsupported"},
		{"text only", &llm.APIError{StatusCode: 422, Message: "tool_result content must be text only"}, "text_only_required"},
		{"invalid data URL", &llm.APIError{StatusCode: 400, Message: "invalid data URL for image_url"}, "invalid_image_encoding"},
		{"invalid media type", &llm.APIError{StatusCode: 415, Message: "unsupported image media type"}, "invalid_image_encoding"},
		{"invalid detail", &llm.APIError{StatusCode: 400, Message: "invalid image detail"}, "invalid_image_encoding"},
		{"payload limit", &llm.APIError{StatusCode: 413, Message: "image payload too large"}, "payload_limit"},
		{"status zero stream frame", &llm.APIError{Message: "image_url is unsupported"}, "image_unsupported"},
		{"retryable", &llm.APIError{StatusCode: 400, Message: "image is unsupported", Retryable: true}, ""},
		{"rate limit", &llm.APIError{StatusCode: 429, Message: "image rate limit"}, ""},
		{"authentication", &llm.APIError{StatusCode: 401, Message: "invalid API key for image request"}, ""},
		{"max tokens", &llm.APIError{StatusCode: 400, Message: "max_tokens must be at least 16"}, ""},
		{"server", &llm.APIError{StatusCode: 500, Message: "image unsupported"}, ""},
		{"ambiguous bad request", &llm.APIError{StatusCode: 400, Message: "invalid request"}, ""},
		{"non API", errors.New("image unsupported"), ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := classifyMultimodalToolResultRejection(tc.err, shape)
			if tc.wantReason == "" {
				if got != nil {
					t.Fatalf("classification = %+v, want nil", got)
				}
				return
			}
			if got == nil || got.Reason != tc.wantReason {
				t.Fatalf("classification = %+v, want reason %q", got, tc.wantReason)
			}
		})
	}
	if got := classifyMultimodalToolResultRejection(&llm.APIError{StatusCode: 400, Message: "image unsupported"}, nil); got != nil {
		t.Fatalf("text-only classification = %+v", got)
	}
}

func TestRedactImageBearingErrorRemovesRequestAndImageValues(t *testing.T) {
	payload := strings.Repeat("QUJD", 30)
	req := richImageRequest(payload)
	message := "unsupported image data:image/png;base64," + payload + " at /private/screen.png: private result text nested-secret private system prompt"
	got := redactImageBearingError(&llm.APIError{StatusCode: 400, Code: "invalid_" + payload, Message: message}, req)
	var apiErr *llm.APIError
	if !errors.As(got, &apiErr) {
		t.Fatalf("error type = %T", got)
	}
	for _, secret := range []string{payload, "/private/screen.png", "private result text", "nested-secret", "private system prompt"} {
		if strings.Contains(apiErr.Error(), secret) {
			t.Fatalf("redacted error contains %q: %s", secret, apiErr.Error())
		}
	}
	if !strings.Contains(apiErr.Message, "[REDACTED_") {
		t.Fatalf("message = %q, want redaction marker", apiErr.Message)
	}
	if strings.Contains(apiErr.Code, payload) {
		t.Fatalf("code leaked payload: %q", apiErr.Code)
	}
}

func TestRedactModelRequestEventRemovesImageRequestValues(t *testing.T) {
	payload := strings.Repeat("QUJD", 30)
	req := richImageRequest(payload)
	event := llm.ModelRequestEvent{
		State:      llm.ModelRequestUpstreamAttemptFailed,
		StatusCode: 429,
		Code:       "quota_" + payload,
		Message:    "retry image data:image/png;base64," + payload + " from /private/screen.png",
		Retryable:  true,
	}
	got := redactModelRequestEvent(event, req)
	for _, secret := range []string{payload, "/private/screen.png"} {
		if strings.Contains(got.Code+" "+got.Message, secret) {
			t.Fatalf("redacted lifecycle event contains %q: %+v", secret, got)
		}
	}
	if !strings.Contains(got.Message, "[REDACTED_") {
		t.Fatalf("message = %q, want redaction marker", got.Message)
	}
}

func TestWithAPIErrorDiagnosticPreservesRetryClassification(t *testing.T) {
	diagnostic := &llm.APIErrorDiagnostic{Stage: llm.APIErrorStageUpstreamStream}
	plain := withAPIErrorDiagnostic(errors.New("truncated stream"), diagnostic)
	if wire := protocol.ErrorFrom(plain); !wire.Retryable || wire.Diagnostic != diagnostic {
		t.Fatalf("plain wire error = %+v", wire)
	}

	retryAfter := 3 * time.Second
	retryable := &llm.APIError{
		StatusCode: 503,
		Code:       "server_error",
		Message:    "retry",
		Retryable:  true,
		RetryAfter: retryAfter,
	}
	var got *llm.APIError
	if !errors.As(withAPIErrorDiagnostic(retryable, diagnostic), &got) {
		t.Fatal("retryable error did not remain APIError")
	}
	if got.StatusCode != 503 || got.Code != "server_error" || !got.Retryable || got.RetryAfter != retryAfter || got.Diagnostic != diagnostic {
		t.Fatalf("retryable error changed: %+v", got)
	}
	if retryable.Diagnostic != nil {
		t.Fatal("withAPIErrorDiagnostic mutated original APIError")
	}

	final := &llm.APIError{StatusCode: 400, Code: "invalid_request", Message: "final", Retryable: false}
	if !errors.As(withAPIErrorDiagnostic(final, diagnostic), &got) || got.Retryable {
		t.Fatalf("non-retryable error changed: %+v", got)
	}
}

func TestRedactImageBearingErrorCoversEveryRequestField(t *testing.T) {
	req := richImageRequest(serverOnePixelPNG)
	req.Model = "private-model"
	req.RequestContext = []string{"private-context"}
	req.StopSeqs = []string{"private-stop"}
	req.Reasoning = llm.ReasoningConfig{Profile: "private-profile", Effort: "private-effort", Summary: "private-summary"}
	req.ServiceTier = "private-tier"
	req.Speed = "private-speed"
	req.Betas = []string{"private-beta"}
	req.PreviousResponseID = "private-response-id"
	req.PromptCacheKey = "private-cache-key"
	req.Tools = []llm.ToolSchema{{
		Name:        "private-function-name",
		Description: "private-function-description",
		Parameters:  json.RawMessage(`{"private-property":{"description":"private-schema-string"}}`),
	}}
	req.ServerTools = []llm.ServerTool{{
		Name:       "private-server-name",
		Kind:       "private-server-kind",
		Parameters: json.RawMessage(`{"private-server-key":"private-server-value"}`),
	}}
	req.Messages = append(req.Messages, llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{{
		Kind:               llm.BlockReasoning,
		ReasoningID:        "private-reasoning-id",
		ReasoningEncrypted: "private-reasoning-blob",
	}}})

	secrets := []string{
		"private-model", "private system prompt", "private-context", "private-stop",
		"private-profile", "private-effort", "private-summary", "private-tier", "private-speed", "private-beta",
		"private-response-id", "private-cache-key", "private-function-name", "private-function-description",
		"private-property", "private-schema-string", "private-server-name", "private-server-kind",
		"private-server-key", "private-server-value", "private result text", "nested-secret",
		"private-reasoning-id", "private-reasoning-blob",
	}
	message := "provider echoed " + strings.Join(secrets, " | ")
	got := redactImageBearingError(&llm.APIError{StatusCode: 400, Code: "invalid_request", Message: message}, req)
	var apiErr *llm.APIError
	if !errors.As(got, &apiErr) {
		t.Fatalf("error type = %T", got)
	}
	for _, secret := range secrets {
		if strings.Contains(apiErr.Error(), secret) {
			t.Fatalf("redacted error contains %q: %s", secret, apiErr.Error())
		}
	}
	if len(apiErr.Message) > maxSanitizedErrorBytes+len("…") {
		t.Fatalf("sanitized message length = %d", len(apiErr.Message))
	}
}

func TestSanitizeImageErrorFailsClosedOnUnsafeOutput(t *testing.T) {
	req := richImageRequest(serverOnePixelPNG)
	for _, message := range []string{
		"provider error\x1b[31m",
		"provider error\x00",
		string([]byte{0xff, 0xfe}),
	} {
		got := redactImageBearingError(errors.New(message), req)
		if got.Error() != genericImageErrorMessage {
			t.Fatalf("unsafe error %q sanitized to %q", message, got.Error())
		}
	}
}
