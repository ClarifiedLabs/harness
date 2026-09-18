package client

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"harness/internal/modelproxy/protocol"
	"harness/internal/tracing"
)

func TestLimitsRejectsUnboundedProviderBeforeRequest(t *testing.T) {
	c, _ := New("http://127.0.0.1:1", nil)
	for _, provider := range []string{strings.Repeat("p", 129), "bad/provider"} {
		_, err := c.Limits(context.Background(), provider)
		var detail *protocol.LimitsError
		if !errors.As(err, &detail) || detail.Code != "invalid_request" {
			t.Fatalf("err=%v", err)
		}
		_, err = c.ResetCredits(context.Background(), provider)
		if !errors.As(err, &detail) || detail.Code != "invalid_request" {
			t.Fatalf("err=%v", err)
		}
	}
}

func TestLimitsRequests(t *testing.T) {
	tracer, _ := tracing.NewTracer(true)
	request := protocol.ResetRequest{Provider: "openai-codex", CreditID: "credit-1", RequestID: "request-1"}
	for _, tc := range []struct{ name, method, path, query, body string }{
		{"status", "GET", "/v1/limits", "provider=kimi-code-plan-cn", `{"providers":[{"provider":"kimi-code-plan-cn"}]}`},
		{"all", "GET", "/v1/limits", "", `{"providers":[]}`},
		{"credits", "GET", "/v1/limits/reset-credits", "provider=openai-codex", `{"provider":"openai-codex","credits":[]}`},
		{"reset", "POST", "/v1/limits/reset", "", `{"provider":"openai-codex","credit_id":"credit-1","request_id":"request-1","outcome":"reset"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				if r.Method != tc.method || r.URL.Path != tc.path || r.URL.RawQuery != tc.query {
					t.Errorf("request: %s %s", r.Method, r.URL)
				}
				if r.Header.Get("Authorization") != "Bearer secret" || r.Header.Get(requesterHeader) != "harness" || r.Header.Get("traceparent") == "" {
					t.Errorf("headers: %v", r.Header)
				}
				if tc.name == "reset" {
					var got protocol.ResetRequest
					_ = json.NewDecoder(r.Body).Decode(&got)
					if got != request {
						t.Errorf("body: %+v", got)
					}
				}
				fmt.Fprint(w, tc.body)
			}))
			defer srv.Close()
			c, _ := New(srv.URL, srv.Client(), WithAPIKey("secret"), WithTracer(tracer))
			var err error
			switch tc.name {
			case "status":
				_, err = c.Limits(context.Background(), "kimi-code-plan-cn")
			case "all":
				_, err = c.Limits(context.Background(), "")
			case "credits":
				_, err = c.ResetCredits(context.Background(), "openai-codex")
			case "reset":
				_, err = c.ResetLimits(context.Background(), request)
			}
			if err != nil || calls != 1 {
				t.Fatalf("calls=%d err=%v", calls, err)
			}
		})
	}
}

func TestLimitsQueryErrorSummaryKeepsProviderDetails(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"providers":[{"provider":"kimi-code-plan-cn","error":{"code":"transport_error","message":"subscription request failed"}},{"provider":"openai-codex","error":{"code":"transport_error","message":"subscription request failed"}},{"provider":"zai-coding-plan","error":{"code":"transport_error","message":"subscription request failed"}}]}`)
	}))
	defer srv.Close()
	c, _ := New(srv.URL, srv.Client())
	report, err := c.Limits(context.Background(), "")
	if err == nil || err.Error() != "3 of 3 subscription quota queries failed; see provider results" {
		t.Fatalf("summary = %v", err)
	}
	var detail *protocol.LimitsError
	if !errors.As(err, &detail) || detail.Code != "transport_error" || len(report.Providers) != 3 {
		t.Fatalf("report=%+v err=%v", report, err)
	}
}

func TestLimitsPartialAndStructuredErrors(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/limits":
			fmt.Fprint(w, `{"providers":[{"provider":"kimi-code-plan-cn"},{"provider":"openai-codex","error":{"code":"unauthorized","message":"unavailable"}}]}`)
		case "/v1/limits/reset-credits":
			fmt.Fprint(w, `{"provider":"openai-codex","error":{"code":"unauthorized","message":"unavailable"}}`)
		default:
			w.WriteHeader(400)
			fmt.Fprint(w, `{"error":{"code":"invalid_request","message":"bad credit"}}`)
		}
	}))
	defer srv.Close()
	c, _ := New(srv.URL, srv.Client())
	report, err := c.Limits(context.Background(), "")
	if len(report.Providers) != 2 || err == nil {
		t.Fatalf("%+v %v", report, err)
	}
	credits, err := c.ResetCredits(context.Background(), "openai-codex")
	if credits.Error == nil || err == nil {
		t.Fatalf("%+v %v", credits, err)
	}
	result, err := c.ResetLimits(context.Background(), protocol.ResetRequest{Provider: "openai-codex", CreditID: "c", RequestID: "r"})
	if err == nil || result.Error == nil || result.Error.Indeterminate || result.RequestID != "r" {
		t.Fatalf("%+v %v", result, err)
	}
}

func TestLimitsInvalidResponsesAndOldProxy(t *testing.T) {
	for _, tc := range []struct {
		name       string
		status     int
		body, code string
		ambiguous  bool
	}{
		{"old", 404, "404 page not found", "unsupported_feature", false},
		{"structured404", 404, `{"error":{"code":"provider_not_found","message":"not configured"}}`, "provider_not_found", false},
		{"html", 502, "<html>secret</html>", "proxy_http_error", true},
		{"malformed", 200, "{secret", "invalid_response", true},
		{"empty", 200, "{}", "invalid_response", true},
		{"oversize", 200, strings.Repeat("x", maxErrorBodyBytes+1), "invalid_response", true},
		{"unknown", 200, `{"provider":"openai-codex","credit_id":"c","request_id":"r","outcome":"future_outcome"}`, "indeterminate", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(tc.status); fmt.Fprint(w, tc.body) }))
			defer srv.Close()
			c, _ := New(srv.URL, srv.Client())
			result, err := c.ResetLimits(context.Background(), protocol.ResetRequest{Provider: "openai-codex", CreditID: "c", RequestID: "r"})
			var detail *protocol.LimitsError
			if !errors.As(err, &detail) || detail.Code != tc.code || result.Error.Indeterminate != tc.ambiguous {
				t.Fatalf("result=%+v err=%v detail=%+v", result, err, detail)
			}
			if strings.Contains(err.Error(), "secret") {
				t.Fatal("leaked body")
			}
		})
	}
}

func TestResetLostResponseExplicitRetryReusesTuple(t *testing.T) {
	var requests []protocol.ResetRequest
	var mu sync.Mutex
	snapshot := func() []protocol.ResetRequest {
		mu.Lock()
		defer mu.Unlock()
		return append([]protocol.ResetRequest(nil), requests...)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request protocol.ResetRequest
		_ = json.NewDecoder(r.Body).Decode(&request)
		mu.Lock()
		requests = append(requests, request)
		first := len(requests) == 1
		mu.Unlock()
		if first {
			conn, _, _ := w.(http.Hijacker).Hijack()
			_ = conn.Close()
			return
		}
		_ = json.NewEncoder(w).Encode(protocol.ResetResult{Provider: request.Provider, CreditID: request.CreditID, RequestID: request.RequestID, Outcome: "already_redeemed"})
	}))
	defer srv.Close()
	c, _ := New(srv.URL, srv.Client())
	request := protocol.ResetRequest{Provider: "openai-codex", CreditID: "credit", RequestID: "stable"}
	first, err := c.ResetLimits(context.Background(), request)
	if got := snapshot(); err == nil || first.Error == nil || !first.Error.Indeterminate || len(got) != 1 {
		t.Fatalf("first=%+v err=%v requests=%v", first, err, got)
	}
	retry := protocol.ResetRequest{Provider: first.Provider, CreditID: first.CreditID, RequestID: first.RequestID}
	second, err := c.ResetLimits(context.Background(), retry)
	if got := snapshot(); err != nil || second.Outcome != "already_redeemed" || !reflect.DeepEqual(got, []protocol.ResetRequest{request, request}) {
		t.Fatalf("second=%+v err=%v requests=%v", second, err, got)
	}
}

func TestResetRejectsMissingOrDifferentIdentity(t *testing.T) {
	for _, body := range []string{`{"outcome":"reset"}`, `{"provider":"openai-codex","credit_id":"other","request_id":"r","outcome":"reset","windows_reset":2,"status":{"provider":"foreign"},"credits":{"provider":"foreign"}}`} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, body) }))
		c, _ := New(srv.URL, srv.Client())
		result, err := c.ResetLimits(context.Background(), protocol.ResetRequest{Provider: "openai-codex", CreditID: "c", RequestID: "r"})
		srv.Close()
		if err == nil || result.Outcome != "indeterminate" || result.CreditID != "c" || result.RequestID != "r" || result.Error == nil || !result.Error.Indeterminate || result.Status != nil || result.Credits != nil || result.WindowsReset != nil {
			t.Fatalf("result=%+v err=%v", result, err)
		}
	}
}

func TestResetPreservesEmbeddedIndeterminateError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"provider":"openai-codex","credit_id":"c","request_id":"r","outcome":"indeterminate","error":{"code":"timeout","message":"upstream timeout","indeterminate":true},"warnings":[{"code":"refresh","message":"unavailable"}]}`)
	}))
	defer srv.Close()
	c, _ := New(srv.URL, srv.Client())
	result, err := c.ResetLimits(context.Background(), protocol.ResetRequest{Provider: "openai-codex", CreditID: "c", RequestID: "r"})
	if err == nil || result.Error == nil || result.Error.Code != "timeout" || !result.Error.Indeterminate || len(result.Warnings) != 1 || result.RequestID != "r" {
		t.Fatalf("result=%+v err=%v", result, err)
	}
}

func TestLimitsCancellationAndRedirectNoResend(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Location", "/other")
		w.WriteHeader(307)
	}))
	defer srv.Close()
	c, _ := New(srv.URL, srv.Client())
	request := protocol.ResetRequest{Provider: "openai-codex", CreditID: "c", RequestID: "r"}
	result, err := c.ResetLimits(context.Background(), request)
	if err == nil || calls.Load() != 1 || !result.Error.Indeterminate {
		t.Fatalf("calls=%d result=%+v err=%v", calls.Load(), result, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = c.Limits(ctx, "")
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err=%v", err)
	}
	if calls.Load() != 1 {
		t.Fatal("canceled request was sent")
	}
}
