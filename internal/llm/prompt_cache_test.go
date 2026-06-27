package llm

import (
	"net/http"
	"strings"
	"testing"
)

func TestPromptCacheSessionIDHashesOverlongValues(t *testing.T) {
	got := PromptCacheSessionID(strings.Repeat("x", 300))
	if len(got) > 256 {
		t.Fatalf("session id length = %d, want <= 256", len(got))
	}
	if !strings.HasPrefix(got, "harness-sha256-") {
		t.Fatalf("session id = %q, want hashed harness prefix", got)
	}
	if got != PromptCacheSessionID(strings.Repeat("x", 300)) {
		t.Fatal("hashed session id is not stable")
	}
}

func TestApplyPromptCacheAffinityHeadersSkipsAuthHeaders(t *testing.T) {
	h := http.Header{}
	h.Set("Authorization", "Bearer token")
	ApplyPromptCacheAffinityHeaders(h, []string{"x-session-id", "authorization", "x-api-key"}, "harness-session")
	if got := h.Get("x-session-id"); got != "harness-session" {
		t.Fatalf("x-session-id = %q, want harness-session", got)
	}
	if got := h.Get("Authorization"); got != "Bearer token" {
		t.Fatalf("Authorization = %q, want original auth", got)
	}
	if got := h.Get("x-api-key"); got != "" {
		t.Fatalf("x-api-key = %q, want omitted", got)
	}
}
