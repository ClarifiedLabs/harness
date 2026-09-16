package server

import (
	"bytes"
	"context"
	"log/slog"
	"net/http"
	"strings"
	"testing"
)

func TestLimitsLogsRequestsAndSafeResultsWithoutTracing(t *testing.T) {
	var logs bytes.Buffer
	h := newLimitsHandler(t, []string{"openai-codex"}, func(r *http.Request) (*http.Response, error) {
		if r.Method == "POST" {
			return quotaResponse(200, `{"code":"reset"}`), nil
		}
		return nil, context.DeadlineExceeded
	})
	h.logger = slog.New(slog.NewJSONHandler(&logs, nil))
	for _, tc := range []struct{ method, path, body string }{
		{"GET", "/v1/limits?private=do-not-log-query", ""},
		{"GET", "/v1/limits/reset-credits?provider=openai-codex", ""},
		{"POST", "/v1/limits/reset", `{"provider":"openai-codex","credit_id":"do-not-log-credit","request_id":"do-not-log-request"}`},
		{"POST", "/v1/limits/reset", `{"provider":"do-not-log-invalid-provider"}`},
	} {
		callLimits(t, h, tc.method, tc.path, tc.body)
	}
	text := logs.String()
	if strings.Count(text, `"msg":"subscription quota request completed"`) != 4 {
		t.Fatal(text)
	}
	for _, want := range []string{`"operation":"status"`, `"operation":"reset_credits"`, `"operation":"reset"`, `"provider":"openai-codex"`, `"error_code":"timeout"`, `"outcome":"reset"`, `"warning_codes":["status_refresh_failed","credits_refresh_failed"]`, `"status":400`, `"duration":`} {
		if !strings.Contains(text, want) {
			t.Errorf("missing %s in %s", want, text)
		}
	}
	for _, secret := range []string{"do-not-log", "provider-secret", "Authorization", "credit_id", "request_id"} {
		if strings.Contains(text, secret) {
			t.Fatalf("logged sensitive field %s", secret)
		}
	}
}
