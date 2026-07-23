package protocol

import (
	"encoding/json"
	"testing"
	"time"

	"harness/internal/llm"
)

func TestErrorDiagnosticJSONCompatibility(t *testing.T) {
	oldJSON := []byte(`{"status_code":429,"code":"rate_limit","message":"slow down","retryable":true,"retry_after_ms":250}`)
	var old Error
	if err := json.Unmarshal(oldJSON, &old); err != nil {
		t.Fatalf("decode old error: %v", err)
	}
	if old.Diagnostic != nil {
		t.Fatalf("old diagnostic = %+v, want nil", old.Diagnostic)
	}
	apiErr := old.APIError()
	if apiErr.Diagnostic != nil || apiErr.StatusCode != 429 || apiErr.RetryAfter != 250*time.Millisecond {
		t.Fatalf("old reconstructed error = %#v", apiErr)
	}

	want := Error{
		StatusCode: httpStatusBadRequest,
		Code:       "invalid_request",
		Message:    "unsupported image",
		Diagnostic: &llm.APIErrorDiagnostic{
			Stage:          llm.APIErrorStageUpstreamStream,
			ProxyRequestID: 1,
			TraceID:        "trace-1",
			Compatibility: &llm.CompatibilityDiagnostic{
				Category:   llm.CompatibilityCategoryMultimodalToolResultRejected,
				Reason:     "image_unsupported",
				Confidence: llm.CompatibilityConfidenceLikely,
			},
			MultimodalShape: &llm.MultimodalRequestShape{
				Strategy:            llm.MultimodalStrategyOpenAIToolThenUserImage,
				ToolResultCount:     1,
				ImageCount:          1,
				MIMETypes:           []string{"image/png"},
				EncodedBytes:        12,
				ResultIDsSHA256:     "abc",
				ImagePayloadsSHA256: "def",
			},
		},
	}
	data, err := json.Marshal(want)
	if err != nil {
		t.Fatalf("marshal new error: %v", err)
	}
	var got Error
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("decode new error: %v", err)
	}
	if got.Diagnostic == nil || got.Diagnostic.ProxyRequestID != 1 || got.Diagnostic.MultimodalShape == nil || got.Diagnostic.MultimodalShape.ImageCount != 1 {
		t.Fatalf("new diagnostic = %+v", got.Diagnostic)
	}
	gotAPI := got.APIError()
	if gotAPI.Diagnostic == nil || gotAPI.Diagnostic.Compatibility == nil || gotAPI.Diagnostic.Compatibility.Category != llm.CompatibilityCategoryMultimodalToolResultRejected {
		t.Fatalf("new reconstructed error = %#v", gotAPI)
	}
}

const httpStatusBadRequest = 400

func TestPricingInfoStale(t *testing.T) {
	base := time.Date(2026, 6, 19, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		name    string
		pricing *PricingInfo
		now     time.Time
		want    bool
	}{
		{
			name:    "nil never stale",
			pricing: nil,
			now:     base,
			want:    false,
		},
		{
			name:    "no source date never stale",
			pricing: &PricingInfo{MaxAgeSeconds: 3600},
			now:     base,
			want:    false,
		},
		{
			name:    "no max age never stale",
			pricing: &PricingInfo{SourceDate: base.Add(-1000 * time.Hour)},
			now:     base,
			want:    false,
		},
		{
			name:    "within ttl is fresh",
			pricing: &PricingInfo{SourceDate: base, MaxAgeSeconds: int64((24 * time.Hour).Seconds())},
			now:     base.Add(23 * time.Hour),
			want:    false,
		},
		{
			name:    "past ttl is stale",
			pricing: &PricingInfo{SourceDate: base, MaxAgeSeconds: int64((24 * time.Hour).Seconds())},
			now:     base.Add(25 * time.Hour),
			want:    true,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.pricing.Stale(tc.now); got != tc.want {
				t.Fatalf("Stale() = %v, want %v", got, tc.want)
			}
		})
	}
}
