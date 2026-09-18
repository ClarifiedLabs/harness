package server

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"harness/internal/apikey"
	"harness/internal/auth"
	"harness/internal/llm"
	"harness/internal/metrics"
	"harness/internal/modelproxy/protocol"
)

type limitsTransport func(*http.Request) (*http.Response, error)

func (f limitsTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
func quotaResponse(status int, body string) *http.Response {
	return &http.Response{StatusCode: status, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body))}
}

func newLimitsHandler(t *testing.T, names []string, transport limitsTransport, registries ...*metrics.Registry) *Handler {
	t.Helper()
	dir := t.TempDir()
	var files []string
	urls := map[string]string{"kimi-code-plan-cn": "https://api.kimi.com/coding/v1", "zai-coding-plan": "https://api.z.ai/api/coding/paas/v4", "openai-codex": "https://chatgpt.com/backend-api/codex"}
	for _, name := range names {
		pc := llm.ProviderConfig{Name: name, APIType: "openai", APIKey: "provider-secret", BaseURL: urls[name], Models: []llm.ModelEntry{{Name: "test-model"}}}
		data, err := json.Marshal(pc)
		if err != nil {
			t.Fatal(err)
		}
		filename := name + ".json"
		files = append(files, filename)
		if err := os.WriteFile(filepath.Join(dir, filename), data, 0600); err != nil {
			t.Fatal(err)
		}
	}
	var registry *metrics.Registry
	if len(registries) != 0 {
		registry = registries[0]
	}
	h, err := NewHandler(Options{Metrics: registry, ConfigDir: dir, Config: Config{ProviderConfigs: files}, Getenv: func(string) string { return "" }, Now: func() time.Time { return time.Date(2026, 9, 16, 0, 0, 0, 0, time.UTC) }, SubscriptionHTTPClient: &http.Client{Transport: transport}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = h.Close() })
	return h
}

func callLimits(t *testing.T, h http.Handler, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(method, path, strings.NewReader(body)))
	if w.Header().Get("Cache-Control") != "no-store" {
		t.Errorf("missing no-store: %s", path)
	}
	return w
}

func decodeLimitsResult(t *testing.T, w *httptest.ResponseRecorder) protocol.ResetResult {
	t.Helper()
	if w.Code != 200 {
		t.Fatalf("status %d: %s", w.Code, w.Body)
	}
	var out protocol.ResetResult
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func TestLimitsAllConcurrentIndependentAndAccountingUntouched(t *testing.T) {
	entered := make(chan string, 3)
	release := make(chan struct{})
	h := newLimitsHandler(t, []string{"zai-coding-plan", "openai-codex", "kimi-code-plan-cn"}, func(r *http.Request) (*http.Response, error) {
		entered <- r.URL.Host
		select {
		case <-release:
		case <-r.Context().Done():
			return nil, r.Context().Err()
		}
		switch r.URL.Host {
		case "api.kimi.com":
			return quotaResponse(200, `{"usage":{"limit":"100","remaining":"60"}}`), nil
		case "chatgpt.com":
			if r.URL.Path != "/backend-api/wham/usage" {
				t.Errorf("status fetched credit details: %s", r.URL.Path)
			}
			return quotaResponse(200, `{"plan_type":"pro","rate_limit":{"allowed":true,"primary_window":{"used_percent":100}}}`), nil
		default:
			return quotaResponse(401, `{"secret":"do-not-echo"}`), nil
		}
	})
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest("GET", "/v1/limits", nil))
		done <- w
	}()
	// All upstream requests must start before any completes; no sleep coordination.
	for range 3 {
		select {
		case <-entered:
		case <-time.After(5 * time.Second):
			close(release)
			t.Fatal("status fetches were not concurrent")
		}
	}
	close(release)
	w := <-done
	var report protocol.LimitsReport
	if err := json.Unmarshal(w.Body.Bytes(), &report); err != nil {
		t.Fatal(err)
	}
	if w.Code != 200 || len(report.Providers) != 3 || w.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("response %d %s", w.Code, w.Body)
	}
	got := []string{}
	for _, p := range report.Providers {
		got = append(got, p.Provider)
	}
	if !reflect.DeepEqual(got, []string{"kimi-code-plan-cn", "openai-codex", "zai-coding-plan"}) {
		t.Fatal(got)
	}
	if report.Providers[0].Error != nil || report.Providers[1].Error != nil || report.Providers[2].Error == nil || report.Providers[2].Error.Code != "unauthorized" {
		t.Fatal(w.Body.String())
	}
	if strings.Contains(w.Body.String(), "do-not-echo") || strings.Contains(w.Body.String(), "provider-secret") {
		t.Fatal("secret leakage")
	}
	usage := h.usageSnapshotForRequest(httptest.NewRequest("GET", "/v1/usage", nil))
	if len(usage.Models) != 0 || usage.Budget != nil || h.nextRequestID.Load() != 0 {
		t.Fatal("quota request changed inference accounting")
	}
}

func TestLimitsRoutesMethodsValidationAndAuth(t *testing.T) {
	calls := 0
	h := newLimitsHandler(t, []string{"openai-codex"}, func(r *http.Request) (*http.Response, error) {
		calls++
		return quotaResponse(200, `{"plan_type":"pro","rate_limit":{"allowed":true}}`), nil
	})
	for _, tc := range []struct {
		method, path, body string
		status             int
	}{
		{"POST", "/v1/limits", "", 405}, {"POST", "/v1/limits/reset-credits", "", 405}, {"GET", "/v1/limits/reset", "", 405},
		{"GET", "/v1/limits?provider=OpenAI-Codex", "", 400}, {"GET", "/v1/limits?provider=kimi-code-plan-cn", "", 400},
		{"GET", "/v1/limits/reset-credits", "", 400}, {"GET", "/v1/limits/reset-credits?provider=kimi-code-plan-cn", "", 400},
		{"POST", "/v1/limits/reset", `{}`, 400}, {"POST", "/v1/limits/reset", `{"provider":"openai-codex","credit_id":"x","request_id":"x"} {}`, 400},
		{"POST", "/v1/limits/reset", `{"provider":"openai-codex","credit_id":"x","request_id":"` + strings.Repeat("x", 5000) + `"}`, 400},
	} {
		w := callLimits(t, h, tc.method, tc.path, tc.body)
		if w.Code != tc.status {
			t.Errorf("%s %s = %d, %s", tc.method, tc.path, w.Code, w.Body)
		}
	}
	if calls != 0 {
		t.Fatal("invalid requests reached upstream")
	}
	var store apikey.Store
	store.Add("trusted", "proxy-key", time.Time{})
	lifecycle := NewLifecycle(ObserveAuth(h, store, store.Middleware(h)))
	for _, path := range []string{"/v1/limits", "/v1/limits/reset-credits?provider=openai-codex", "/v1/limits/reset"} {
		method := "GET"
		if path == "/v1/limits/reset" {
			method = "POST"
		}
		w := httptest.NewRecorder()
		lifecycle.ServeHTTP(w, httptest.NewRequest(method, path, nil))
		if w.Code != 401 {
			t.Fatalf("unauthenticated %s returned %d", path, w.Code)
		}
	}
	if calls != 0 {
		t.Fatal("unauthenticated requests reached upstream")
	}
	req := httptest.NewRequest("GET", "/v1/limits", nil)
	req.Header.Set("Authorization", "Bearer proxy-key")
	w := httptest.NewRecorder()
	lifecycle.ServeHTTP(w, req)
	if w.Code != 200 || calls != 1 {
		t.Fatalf("authorized request %d calls %d", w.Code, calls)
	}
}

func TestLimitsResetOutcomesRefreshAndCredentialSnapshot(t *testing.T) {
	for _, outcome := range []string{"reset", "already_redeemed", "nothing_to_reset", "no_credit", "future"} {
		t.Run(outcome, func(t *testing.T) {
			posts, gets, resolutions := 0, 0, 0
			h := newLimitsHandler(t, []string{"openai-codex"}, func(r *http.Request) (*http.Response, error) {
				if r.Header.Get("Authorization") != "Bearer captured-1" {
					t.Error("credentials re-resolved during operation")
				}
				if r.Method == "POST" {
					posts++
					var body map[string]string
					if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
						t.Fatal(err)
					}
					if !reflect.DeepEqual(body, map[string]string{"credit_id": "credit-1", "redeem_request_id": "request-1"}) || r.URL.Path != "/backend-api/wham/rate-limit-reset-credits/consume" {
						t.Errorf("request %s %v", r.URL, body)
					}
					return quotaResponse(200, `{"code":"`+outcome+`","windows_reset":0}`), nil
				}
				gets++
				return quotaResponse(503, "do-not-echo-refresh"), nil
			})
			h.providers[0].APIKeyEnv = []string{"QUOTA_KEY"}
			h.getenv = func(name string) string { resolutions++; return fmt.Sprintf("captured-%d", resolutions) }
			result := decodeLimitsResult(t, callLimits(t, h, "POST", "/v1/limits/reset", `{"provider":"openai-codex","credit_id":"credit-1","request_id":"request-1","future_key":true}`))
			if posts != 1 || resolutions != 1 {
				t.Fatalf("posts=%d auth resolutions=%d", posts, resolutions)
			}
			switch outcome {
			case "reset", "already_redeemed":
				if result.Outcome != outcome || result.Error != nil || len(result.Warnings) != 2 || gets != 2 || result.WindowsReset == nil || *result.WindowsReset != 0 {
					t.Fatalf("result %+v, gets %d", result, gets)
				}
			case "future":
				if result.Outcome != "indeterminate" || result.Error == nil || !result.Error.Indeterminate || gets != 0 {
					t.Fatalf("result %+v", result)
				}
			default:
				if result.Outcome != outcome || result.Error != nil || gets != 0 {
					t.Fatalf("result %+v", result)
				}
			}
		})
	}
}

func TestLimitsResetLostResponseRetryUsesExactIdentity(t *testing.T) {
	var mu sync.Mutex
	var bodies []string
	consumed := map[string]bool{}
	creditsConsumed := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "GET" {
			if strings.HasSuffix(r.URL.Path, "/usage") {
				fmt.Fprint(w, `{"rate_limit":{"allowed":true}}`)
			} else {
				fmt.Fprint(w, `{"available_count":0,"credits":[]}`)
			}
			return
		}
		body, _ := io.ReadAll(r.Body)
		var req struct {
			Credit string `json:"credit_id"`
			ID     string `json:"redeem_request_id"`
		}
		if err := json.Unmarshal(body, &req); err != nil {
			t.Error(err)
			return
		}
		mu.Lock()
		bodies = append(bodies, string(body))
		already := consumed[req.ID]
		if !already {
			consumed[req.ID] = true
			creditsConsumed++
		}
		mu.Unlock()
		if !already {
			conn, _, err := w.(http.Hijacker).Hijack()
			if err != nil {
				t.Error(err)
				return
			}
			_ = conn.Close()
			return
		}
		fmt.Fprint(w, `{"code":"already_redeemed","windows_reset":2}`)
	}))
	defer upstream.Close()
	destination, _ := url.Parse(upstream.URL)
	transport := upstream.Client().Transport
	h := newLimitsHandler(t, []string{"openai-codex"}, func(r *http.Request) (*http.Response, error) {
		clone := r.Clone(r.Context())
		clone.URL.Scheme = destination.Scheme
		clone.URL.Host = destination.Host
		return transport.RoundTrip(clone)
	})
	body := `{"provider":"openai-codex","credit_id":"selected-credit","request_id":"stable-request"}`
	first := decodeLimitsResult(t, callLimits(t, h, "POST", "/v1/limits/reset", body))
	if first.Outcome != "indeterminate" || first.Error == nil || !first.Error.Indeterminate || first.RequestID != "stable-request" {
		t.Fatalf("first %+v", first)
	}
	mu.Lock()
	n := len(bodies)
	mu.Unlock()
	if n != 1 {
		t.Fatalf("implicit retry: %d", n)
	}
	second := decodeLimitsResult(t, callLimits(t, h, "POST", "/v1/limits/reset", body))
	if second.Outcome != "already_redeemed" || second.Error != nil || second.Credits == nil || len(second.Credits.Credits) != 0 || second.Status == nil {
		t.Fatalf("second %+v", second)
	}
	mu.Lock()
	defer mu.Unlock()
	if creditsConsumed != 1 || len(bodies) != 2 || bodies[0] != bodies[1] {
		t.Fatalf("consumed %d bodies %v", creditsConsumed, bodies)
	}
}

func TestLimitsCancellationAndCredentialPrecedence(t *testing.T) {
	h := newLimitsHandler(t, []string{"openai-codex"}, func(*http.Request) (*http.Response, error) {
		t.Error("canceled request reached upstream")
		return nil, context.Canceled
	})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	req := httptest.NewRequest("GET", "/v1/limits", nil).WithContext(ctx)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if !strings.Contains(w.Body.String(), `"code":"canceled"`) {
		t.Fatal(w.Body.String())
	}
	pc := h.providers[0]
	pc.APIKeyEnv = []string{"CONFIGURED"}
	pc.APIKey = "inline"
	for _, tc := range []struct {
		env  map[string]string
		want string
	}{
		{map[string]string{"CONFIGURED": "configured", "OPENAI_API_KEY": "fallback"}, "configured"},
		{map[string]string{"OPENAI_API_KEY": "fallback"}, "fallback"},
		{map[string]string{}, "inline"},
	} {
		h.getenv = func(k string) string { return tc.env[k] }
		cred, err := h.subscriptionCredentials(context.Background(), pc)
		if err != nil {
			t.Fatal(err)
		}
		opts, err := h.runtimeOptionsForTarget(context.Background(), resolvedTarget{pc: pc})
		if err != nil {
			t.Fatal(err)
		}
		if cred.APIKey != tc.want || opts.APIKey != tc.want {
			t.Fatalf("precedence quota=%s inference=%s want=%s", cred.APIKey, opts.APIKey, tc.want)
		}
	}
	src, err := auth.NewSource(auth.Config{Type: auth.TypeTokenCommand, Command: "/bin/sh", Args: []string{"-c", "printf dynamic"}}, auth.Options{Name: pc.Name, ConfigDir: t.TempDir(), Getenv: func(string) string { return "" }})
	if err != nil {
		t.Fatal(err)
	}
	h.authSources[pc.Name] = src
	cred, err := h.subscriptionCredentials(context.Background(), pc)
	if err != nil {
		t.Fatal(err)
	}
	opts, err := h.runtimeOptionsForTarget(context.Background(), resolvedTarget{pc: pc})
	if err != nil {
		t.Fatal(err)
	}
	if cred.APIKey != "" || opts.APIKey != "" || cred.Headers["Authorization"] != "Bearer dynamic" || !reflect.DeepEqual(cred.Headers, opts.AuthHeaders) {
		t.Fatal("dynamic auth did not win for both paths")
	}
}

func TestLimitsStatusAndCreditDetailsAreIndependent(t *testing.T) {
	h := newLimitsHandler(t, []string{"openai-codex"}, func(r *http.Request) (*http.Response, error) {
		if strings.HasSuffix(r.URL.Path, "/usage") {
			return quotaResponse(200, `{"rate_limit":{"allowed":true},"rate_limit_reset_credits":{"available_count":0}}`), nil
		}
		return quotaResponse(404, "secret-error"), nil
	})
	status := callLimits(t, h, "GET", "/v1/limits?provider=openai-codex", "")
	if bytes.Contains(status.Body.Bytes(), []byte(`"error"`)) {
		t.Fatal(status.Body.String())
	}
	credits := callLimits(t, h, "GET", "/v1/limits/reset-credits?provider=openai-codex", "")
	if !strings.Contains(credits.Body.String(), `"code":"unsupported_endpoint"`) || strings.Contains(credits.Body.String(), "secret-error") {
		t.Fatal(credits.Body.String())
	}
}
