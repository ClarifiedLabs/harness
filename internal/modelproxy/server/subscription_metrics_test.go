package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"harness/internal/metrics"
	"harness/internal/modelproxy/protocol"
	"harness/internal/modelproxy/subscription"
)

func quotaPtr[T any](v T) *T               { return &v }
func quotaText(r *metrics.Registry) string { var b strings.Builder; r.Render(&b); return b.String() }
func requireQuotaSeries(t *testing.T, text string, wants ...string) {
	t.Helper()
	for _, want := range wants {
		if !strings.Contains(text, want+"\n") {
			t.Errorf("missing %s in:\n%s", want, text)
		}
	}
}

func TestSubscriptionMetricsPresenceIdentityAndResetTime(t *testing.T) {
	r := metrics.New()
	m := newSubscriptionMetrics(r)
	fetched := time.Unix(1000, 125000000)
	status := protocol.ProviderLimits{Provider: "openai-codex", FetchedAt: fetched, Plan: "private-plan", ResetCredits: &protocol.ResetCreditSummary{AvailableCount: quotaPtr(int64(0))}, Pools: []protocol.LimitPool{
		{ID: "codex", Name: "private-pool", Allowed: quotaPtr(true), LimitReached: quotaPtr(false), Windows: []protocol.LimitWindow{
			{ID: "primary", Name: "private-window", Unit: "quota units", Used: quotaPtr(0.0), Remaining: quotaPtr(100.0), Limit: quotaPtr(100.0), UsedPercent: quotaPtr(100.0), RemainingPercent: quotaPtr(0.0), DurationSeconds: quotaPtr(int64(18000)), ResetAfterSeconds: quotaPtr(int64(0))},
			{ID: "secondary", UsedPercent: quotaPtr(0.0), ResetAt: quotaPtr(time.Unix(1100, 0)), ResetAfterSeconds: quotaPtr(int64(500))},
		}},
		{ID: "additional-1", Name: "unknown-admission"},
	}}
	m.update(status, time.Unix(1001, 0))
	text := quotaText(r)
	requireQuotaSeries(t, text,
		`model_proxy_subscription_refresh_success{provider="openai-codex"} 1`,
		`model_proxy_subscription_last_refresh_timestamp_seconds{provider="openai-codex"} 1000.125`,
		`model_proxy_subscription_last_success_timestamp_seconds{provider="openai-codex"} 1000.125`,
		`model_proxy_subscription_allowed{pool="codex",provider="openai-codex"} 1`,
		`model_proxy_subscription_limit_reached{pool="codex",provider="openai-codex"} 0`,
		`model_proxy_subscription_used{pool="codex",provider="openai-codex",unit="quota units",window="primary"} 0`,
		`model_proxy_subscription_used_percent{pool="codex",provider="openai-codex",window="primary"} 100`,
		`model_proxy_subscription_used_percent{pool="codex",provider="openai-codex",window="secondary"} 0`,
		`model_proxy_subscription_remaining_percent{pool="codex",provider="openai-codex",window="primary"} 0`,
		`model_proxy_subscription_window_duration_seconds{pool="codex",provider="openai-codex",window="primary"} 18000`,
		`model_proxy_subscription_reset_timestamp_seconds{pool="codex",provider="openai-codex",window="primary"} 1000.125`,
		`model_proxy_subscription_reset_timestamp_seconds{pool="codex",provider="openai-codex",window="secondary"} 1100`,
		`model_proxy_subscription_reset_credits_available{provider="openai-codex"} 0`,
	)
	if strings.Contains(text, "private-") || strings.Contains(text, `pool="additional-1"`) {
		t.Fatal("names or absent data exported", text)
	}
}

func TestSubscriptionMetricsIgnoreOlderCompletedResults(t *testing.T) {
	r := metrics.New()
	m := newSubscriptionMetrics(r)
	latest := protocol.ProviderLimits{Provider: "openai-codex", FetchedAt: time.Unix(1100, 0), Pools: []protocol.LimitPool{{ID: "codex", Allowed: quotaPtr(true)}}}
	m.update(latest, time.Unix(1100, 0))
	before := quotaText(r)
	for _, failure := range []bool{false, true} {
		older := protocol.ProviderLimits{Provider: "openai-codex", FetchedAt: time.Unix(1000, 0)}
		if failure {
			older.Error = &protocol.LimitsError{Code: "timeout"}
		}
		m.update(older, time.Unix(1200, 0))
		if after := quotaText(r); after != before {
			t.Fatal("delayed poll result overwrote newer refresh")
		}
	}
}

func TestSubscriptionMetricsSlowPollCannotOverwriteManualRefresh(t *testing.T) {
	r := metrics.New()
	var clock atomic.Int64
	clock.Store(1000)
	now := func() time.Time { return time.Unix(clock.Load(), 0) }
	olderFetched, releaseSlow := make(chan struct{}), make(chan struct{})
	var firstFetch sync.Once
	var codexCalls atomic.Int32
	transport := limitsTransport(func(req *http.Request) (*http.Response, error) {
		if req.URL.Host == "api.kimi.com" {
			select {
			case <-releaseSlow:
			case <-req.Context().Done():
				return nil, req.Context().Err()
			}
			return quotaResponse(200, `{"usage":{"limit":100,"used":25}}`), nil
		}
		if codexCalls.Add(1) == 1 {
			return quotaResponse(200, `{"rate_limit":{"allowed":true,"primary_window":{"used_percent":50}}}`), nil
		}
		return quotaResponse(200, `{"rate_limit":{"allowed":true}}`), nil
	})
	h := newLimitsHandler(t, []string{"kimi-for-coding", "openai-codex"}, transport, r)
	h.now = now
	h.limits = subscription.New(subscription.Options{Client: &http.Client{Transport: transport}, Resolve: h.subscriptionCredentials, Now: func() time.Time {
		fetched := now()
		firstFetch.Do(func() { close(olderFetched) })
		return fetched
	}})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() { defer close(done); h.RefreshSubscriptionMetrics(ctx) }()
	select {
	case <-olderFetched:
	case <-time.After(5 * time.Second):
		t.Fatal("poll did not fetch Codex")
	}
	clock.Store(1100)
	w := callLimits(t, h, "GET", "/v1/limits?provider=openai-codex", "")
	if w.Code != 200 {
		t.Fatal(w.Code)
	}
	clock.Store(1200)
	close(releaseSlow)
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("poll did not finish")
	}
	text := quotaText(r)
	requireQuotaSeries(t, text,
		`model_proxy_subscription_last_refresh_timestamp_seconds{provider="openai-codex"} 1100`,
		`model_proxy_subscription_last_success_timestamp_seconds{provider="openai-codex"} 1100`,
		`model_proxy_subscription_allowed{pool="codex",provider="openai-codex"} 1`)
	if strings.Contains(text, `window="primary"`) {
		t.Fatal("old poll resurrected removed window", text)
	}
}

func TestSubscriptionMetricsRetainStaleOnFailureReplaceOnSuccess(t *testing.T) {
	r := metrics.New()
	m := newSubscriptionMetrics(r)
	for _, provider := range []string{"openai-codex", "kimi-for-coding"} {
		m.update(protocol.ProviderLimits{Provider: provider, FetchedAt: time.Unix(1000, 0), Pools: []protocol.LimitPool{{ID: "coding", Windows: []protocol.LimitWindow{{ID: "weekly", UsedPercent: quotaPtr(25.0)}}}}, ResetCredits: &protocol.ResetCreditSummary{AvailableCount: quotaPtr(int64(1))}}, time.Unix(1000, 0))
	}
	m.update(protocol.ProviderLimits{Provider: "openai-codex", Error: &protocol.LimitsError{Code: "timeout", Message: "do-not-export-error"}}, time.Unix(1100, 0))
	text := quotaText(r)
	requireQuotaSeries(t, text, `model_proxy_subscription_refresh_success{provider="openai-codex"} 0`, `model_proxy_subscription_last_refresh_timestamp_seconds{provider="openai-codex"} 1100`, `model_proxy_subscription_last_success_timestamp_seconds{provider="openai-codex"} 1000`, `model_proxy_subscription_used_percent{pool="coding",provider="openai-codex",window="weekly"} 25`)
	if strings.Contains(text, "do-not-export") {
		t.Fatal("error text exposed")
	}
	// A successful response without the old optional window/credits removes them.
	m.update(protocol.ProviderLimits{Provider: "openai-codex", FetchedAt: time.Unix(1200, 0), Pools: []protocol.LimitPool{{ID: "codex", Allowed: quotaPtr(false)}}}, time.Unix(1201, 0))
	text = quotaText(r)
	requireQuotaSeries(t, text, `model_proxy_subscription_allowed{pool="codex",provider="openai-codex"} 0`, `model_proxy_subscription_refresh_success{provider="openai-codex"} 1`, `model_proxy_subscription_last_success_timestamp_seconds{provider="openai-codex"} 1200`, `model_proxy_subscription_used_percent{pool="coding",provider="kimi-for-coding",window="weekly"} 25`)
	for _, line := range strings.Split(text, "\n") {
		if strings.Contains(line, `provider="openai-codex"`) && (strings.HasPrefix(line, "model_proxy_subscription_used_percent{") || strings.HasPrefix(line, "model_proxy_subscription_reset_credits_available{")) {
			t.Fatal("stale series retained:", line)
		}
	}
	// A first-ever failed fetch must not create zero quota or success timestamps.
	m.update(protocol.ProviderLimits{Provider: "zai-coding-plan", Error: &protocol.LimitsError{Code: "unauthorized"}}, time.Unix(1202, 0))
	for _, line := range strings.Split(quotaText(r), "\n") {
		if strings.Contains(line, `provider="zai-coding-plan"`) && !strings.HasPrefix(line, "model_proxy_subscription_refresh_success{") && !strings.HasPrefix(line, "model_proxy_subscription_last_refresh_timestamp_seconds{") {
			t.Fatal("fabricated data:", line)
		}
	}
}

func TestSubscriptionMetricsPollingReadOnlyIndependentAndNoScrapeRequests(t *testing.T) {
	r := metrics.New()
	var calls atomic.Int32
	h := newLimitsHandler(t, []string{"kimi-for-coding", "openai-codex", "zai-coding-plan"}, func(req *http.Request) (*http.Response, error) {
		calls.Add(1)
		if req.Method != "GET" {
			t.Error("non-read-only poll", req.Method)
		}
		switch req.URL.Host {
		case "api.kimi.com":
			if req.URL.Path != "/coding/v1/usages" {
				t.Error(req.URL.Path)
			}
			return quotaResponse(200, `{"usage":{"limit":100,"used":25}}`), nil
		case "chatgpt.com":
			if req.URL.Path != "/backend-api/wham/usage" {
				t.Error(req.URL.Path)
			}
			return quotaResponse(200, `{"rate_limit":{"allowed":true,"primary_window":{"used_percent":0}},"rate_limit_reset_credits":{"available_count":0}}`), nil
		default:
			return quotaResponse(401, "do-not-export-body"), nil
		}
	}, r)
	h.RefreshSubscriptionMetrics(context.Background())
	if calls.Load() != 3 {
		t.Fatalf("calls %d", calls.Load())
	}
	requireQuotaSeries(t, quotaText(r), `model_proxy_subscription_refresh_success{provider="kimi-for-coding"} 1`, `model_proxy_subscription_refresh_success{provider="openai-codex"} 1`, `model_proxy_subscription_refresh_success{provider="zai-coding-plan"} 0`)
	for range 2 {
		w := httptest.NewRecorder()
		r.Handler().ServeHTTP(w, httptest.NewRequest("GET", "/metrics", nil))
		if w.Code != 200 {
			t.Fatal(w.Code)
		}
	}
	if calls.Load() != 3 {
		t.Fatal("scraping queried upstream")
	}
	// Manual queries still fetch live and update gauges, never serve this snapshot.
	callLimits(t, h, "GET", "/v1/limits?provider=openai-codex", "")
	if calls.Load() != 4 {
		t.Fatal("manual status used the metric cache")
	}
	if len(h.usageSnapshotForRequest(httptest.NewRequest("GET", "/v1/usage", nil)).Models) != 0 || h.nextRequestID.Load() != 0 {
		t.Fatal("polling changed generation usage")
	}
}

func TestSubscriptionMetricsDisabledAndCancellation(t *testing.T) {
	h := newLimitsHandler(t, []string{"openai-codex"}, func(*http.Request) (*http.Response, error) {
		t.Error("metrics-disabled poll reached upstream")
		return nil, context.Canceled
	})
	h.RefreshSubscriptionMetrics(context.Background())
	started := make(chan struct{})
	r := metrics.New()
	h = newLimitsHandler(t, []string{"openai-codex"}, func(req *http.Request) (*http.Response, error) {
		close(started)
		<-req.Context().Done()
		return nil, req.Context().Err()
	}, r)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() { defer close(done); h.RefreshSubscriptionMetrics(ctx) }()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("poll did not start")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("poll did not cancel")
	}
	requireQuotaSeries(t, quotaText(r), `model_proxy_subscription_refresh_success{provider="openai-codex"} 0`)
}

func TestSubscriptionMetricsConcurrentUpdatesAndScrapes(t *testing.T) {
	r := metrics.New()
	m := newSubscriptionMetrics(r)
	var wg sync.WaitGroup
	for _, provider := range []string{"openai-codex", "kimi-for-coding", "zai-coding-plan"} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range 20 {
				m.update(protocol.ProviderLimits{Provider: provider, FetchedAt: time.Unix(int64(i+1000), 0)}, time.Unix(int64(i+1000), 0))
				_ = quotaText(r)
			}
		}()
	}
	wg.Wait()
	for _, provider := range []string{"openai-codex", "kimi-for-coding", "zai-coding-plan"} {
		requireQuotaSeries(t, quotaText(r), `model_proxy_subscription_last_success_timestamp_seconds{provider="`+provider+`"} 1019`)
	}
}
