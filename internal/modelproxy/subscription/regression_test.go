package subscription

import (
	"context"
	"crypto/tls"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"harness/internal/llm"
	"harness/internal/modelproxy/protocol"
)

func TestCredentialResolutionHasBoundedContext(t *testing.T) {
	client := New(Options{Resolve: func(ctx context.Context, pc llm.ProviderConfig) (Credentials, error) {
		deadline, ok := ctx.Deadline()
		if !ok || time.Until(deadline) > upstreamTimeout {
			t.Error("unbounded credential resolution")
		}
		return Credentials{APIKey: "key"}, nil
	}})
	if _, err := client.Resolve(context.Background(), config("openai-codex")); err != nil {
		t.Fatal(err)
	}
}

func TestKimiUnknownDurationUnitsWarnWithoutGuessing(t *testing.T) {
	for _, body := range []string{
		`{"usage":{"limit":100,"duration":1,"timeUnit":"FUTURE"}}`,
		`{"limits":[{"window":{"duration":1,"timeUnit":"FUTURE"},"detail":{"limit":100}}]}`,
		`{"limits":[{"detail":{"limit":100,"window":{"duration":1,"timeUnit":"FUTURE"}}}]}`,
	} {
		a := account(t, "kimi-code-plan-cn", func(*http.Request) (*http.Response, error) { return response(200, body), nil })
		out, err := a.Status(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if out.Pools[0].Windows[0].DurationSeconds != nil || len(out.Warnings) != 1 || out.Warnings[0].Code != "unknown_quota_unit" {
			t.Fatalf("report %+v", out)
		}
	}
}

func TestResetRedirectIsIndeterminate(t *testing.T) {
	calls := 0
	a := account(t, "openai-codex", func(*http.Request) (*http.Response, error) {
		calls++
		r := response(303, "")
		r.Header.Set("Location", "https://chatgpt.com/done")
		return r, nil
	})
	got, err := a.Reset(context.Background(), protocol.ResetRequest{Provider: "openai-codex", CreditID: "selected", RequestID: "same-request"})
	if !limitsError(t, err, "redirect").Indeterminate || got.Outcome != "indeterminate" || calls != 1 {
		t.Fatalf("redirect %+v calls %d", got, calls)
	}
}

func TestStandardTransportCannotReuseOrRetryStaleConnections(t *testing.T) {
	var mu sync.Mutex
	peers := map[string]int{}
	calls := 0
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		calls++
		peers[r.RemoteAddr]++
		n := calls
		mu.Unlock()
		if r.Header.Get("Authorization") != "Bearer secret-static-key" {
			t.Error("missing credentials")
		}
		// A reused connection could trigger Go's automatic GET retry after this
		// response drop. The isolated adapter must instead fail after one attempt.
		if n == 2 {
			conn, _, err := w.(http.Hijacker).Hijack()
			if err != nil {
				t.Error(err)
				return
			}
			_ = conn.Close()
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"plan_type":"plus"}`))
	}))
	defer server.Close()
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.TLSClientConfig = &tls.Config{InsecureSkipVerify: true} // httptest certificate only
	transport.DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, network, strings.TrimPrefix(server.URL, "https://"))
	}
	defer transport.CloseIdleConnections()
	client := New(Options{Client: &http.Client{Transport: transport}})
	isolated := client.http.Transport.(*http.Transport)
	defer isolated.CloseIdleConnections()
	if !isolated.DisableKeepAlives || isolated.ForceAttemptHTTP2 || len(isolated.TLSNextProto) != 0 || transport.DisableKeepAlives {
		t.Fatal("transport isolation")
	}
	a, err := client.Resolve(context.Background(), config("openai-codex"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = a.Status(context.Background()); err != nil {
		t.Fatal(err)
	}
	_, err = a.Status(context.Background())
	limitsError(t, err, "transport_error")
	mu.Lock()
	defer mu.Unlock()
	if calls != 2 || len(peers) != 2 {
		t.Fatalf("attempts=%d connections=%d", calls, len(peers))
	}
}

func TestZaiRequiresConfirmedEnvelope(t *testing.T) {
	for _, prefix := range []string{"", `"success":null,"code":200,`, `"success":true,"code":null,`} {
		a := account(t, "zai-coding-plan", func(*http.Request) (*http.Response, error) {
			return response(200, `{`+prefix+`"data":{"limits":[{"type":"CREDIT_LIMIT","percentage":0}]}}`), nil
		})
		_, err := a.Status(context.Background())
		limitsError(t, err, "invalid_payload")
	}
}

func TestCreditsRequireGrantTimestamp(t *testing.T) {
	for _, grant := range []string{"", `,"granted_at":null`, `,"granted_at":""`, `,"granted_at":"invalid"`} {
		a := account(t, "openai-codex", func(*http.Request) (*http.Response, error) {
			return response(200, `{"credits":[{"id":"credit","status":"available","reset_type":"codex_rate_limits"`+grant+`}],"available_count":1}`), nil
		})
		_, err := a.ResetCredits(context.Background())
		limitsError(t, err, "invalid_payload")
	}
}
