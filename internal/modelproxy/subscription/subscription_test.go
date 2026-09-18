package subscription

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"harness/internal/codexclient"
	"harness/internal/llm"
	"harness/internal/modelproxy/protocol"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

var testNow = time.Date(2026, 9, 16, 0, 0, 0, 0, time.UTC)

func fixture(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile("testdata/" + name + ".json")
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}
func response(status int, body string) *http.Response {
	return &http.Response{StatusCode: status, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body))}
}
func config(name string) llm.ProviderConfig {
	return llm.ProviderConfig{Name: name, BaseURL: map[string]string{"kimi-code-plan-cn": "https://api.kimi.com/coding/v1", "kimi-code-plan-global": "https://api.kimi.ai/coding/v1", "zai-coding-plan": "https://api.z.ai/api/coding/paas/v4", "openai-codex": "https://chatgpt.com/backend-api/codex"}[name], APIKey: "secret-static-key"}
}
func account(t *testing.T, name string, transport roundTripFunc) *Account {
	t.Helper()
	c := New(Options{Client: &http.Client{Transport: transport}, Now: func() time.Time { return testNow }})
	a, err := c.Resolve(context.Background(), config(name))
	if err != nil {
		t.Fatal(err)
	}
	return a
}
func limitsError(t *testing.T, err error, code string) *protocol.LimitsError {
	t.Helper()
	var e *protocol.LimitsError
	if !errors.As(err, &e) || e.Code != code {
		t.Fatalf("error = %v, want LimitsError %q", err, code)
	}
	if strings.Contains(e.Error(), "secret") || strings.Contains(e.Error(), "private") {
		t.Fatalf("unsafe error: %v", e)
	}
	return e
}

func TestCodexAccountRequestsCarryClientIdentity(t *testing.T) {
	var headers http.Header
	transport := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		headers = r.Header.Clone()
		return response(http.StatusOK, fixture(t, "codex")), nil
	})
	c := New(Options{Client: &http.Client{Transport: transport}, Now: func() time.Time { return testNow }, CodexClientVersion: "0.99.0"})
	a, err := c.Resolve(context.Background(), config("openai-codex"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.Status(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := headers.Get("originator"); got != codexclient.Originator {
		t.Fatalf("originator = %q, want %q", got, codexclient.Originator)
	}
	if values := headers.Values("User-Agent"); len(values) != 1 || !strings.HasPrefix(values[0], codexclient.Originator+"/0.99.0 ") {
		t.Fatalf("User-Agent = %q, want a single codex_cli_rs/0.99.0 value", values)
	}
	if headers.Get("Authorization") != "Bearer secret-static-key" || headers.Get("Accept") != "application/json" {
		t.Fatalf("subscription headers = %+v", headers)
	}
}

func TestNonCodexAccountRequestsOmitClientIdentity(t *testing.T) {
	var headers http.Header
	transport := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		headers = r.Header.Clone()
		return response(http.StatusOK, fixture(t, "kimi")), nil
	})
	c := New(Options{Client: &http.Client{Transport: transport}, Now: func() time.Time { return testNow }, CodexClientVersion: "0.99.0"})
	a, err := c.Resolve(context.Background(), config("kimi-code-plan-cn"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.Status(context.Background()); err != nil {
		t.Fatal(err)
	}
	if headers.Get("originator") != "" || headers.Get("User-Agent") != "" {
		t.Fatalf("non-Codex subscription headers = %+v", headers)
	}
}

func TestOfficialEndpointsBeforeCredentials(t *testing.T) {
	cases := []struct {
		name, url string
		valid     bool
	}{
		{"kimi-code-plan-cn", "https://api.kimi.com/coding/v1/", true},
		{"kimi-code-plan-global", "https://api.kimi.ai/coding/v1", true},
		{"kimi-code-plan-global", "https://api.kimi.ai/custom", false},
		{"zai-coding-plan", "https://api.z.ai/api/coding/paas/v4", true},
		{"zai-coding-plan", "https://api.z.ai/api/paas/v4", true},
		{"zai-coding-plan", "https://api.z.ai/api/anthropic", true},
		{"openai-codex", "https://chatgpt.com/backend-api", true},
		{"openai-codex", "https://CHATGPT.COM:443/backend-api/codex/", true},
		{"openai-codex", "https://chatgpt.com.evil.invalid/backend-api/codex", false},
		{"openai-codex", "http://chatgpt.com/backend-api/codex", false},
		{"openai-codex", "https://secret@chatgpt.com/backend-api/codex", false},
		{"openai-codex", "https://chatgpt.com:444/backend-api/codex", false},
		{"openai-codex", "https://chatgpt.com/backend-api/codex?secret=value", false},
		{"openai-codex", "https://chatgpt.com/backend-api/codex#secret", false},
		{"openai-codex", "https://chatgpt.com/backend-api/%63odex", false},
		{"zai-coding-plan", "https://open.bigmodel.cn/api/coding/paas/v4", false},
		{"kimi-code-plan-cn", "https://api.kimi.com/custom", false},
	}
	for _, tc := range cases {
		t.Run(tc.url, func(t *testing.T) {
			calls := 0
			c := New(Options{Resolve: func(context.Context, llm.ProviderConfig) (Credentials, error) {
				calls++
				return Credentials{APIKey: "key"}, nil
			}})
			_, err := c.Resolve(context.Background(), llm.ProviderConfig{Name: tc.name, BaseURL: tc.url})
			if tc.valid {
				if err != nil || calls != 1 {
					t.Fatalf("resolve: %v, calls=%d", err, calls)
				}
			} else {
				limitsError(t, err, "unsupported_endpoint")
				if calls != 0 {
					t.Fatal("resolved credentials before validation")
				}
			}
		})
	}
	if SupportedProvider(llm.ProviderConfig{Name: "openai"}) || !SupportedProvider(llm.ProviderConfig{Name: "openai-codex"}) {
		t.Fatal("SupportedProvider")
	}
	// A second ChatGPT subscription account under a different provider name
	// resolves to the codex quota integration through its profile.
	if !SupportedProvider(llm.ProviderConfig{Name: "openai-codex-2", Profile: llm.ProfileCodex}) {
		t.Fatal("SupportedProvider: codex profile")
	}
	// Kimi and Z.AI second accounts likewise resolve through their profiles.
	if !SupportedProvider(llm.ProviderConfig{Name: "kimi-work", Profile: llm.ProfileKimiCodePlan}) ||
		!SupportedProvider(llm.ProviderConfig{Name: "zai-work", Profile: llm.ProfileZAICodingPlan}) {
		t.Fatal("SupportedProvider: kimi/zai profiles")
	}
	_, err := New(Options{}).Resolve(context.Background(), llm.ProviderConfig{Name: "openai"})
	limitsError(t, err, "unsupported_provider")
}

func TestCredentialSnapshotAndExactRoutes(t *testing.T) {
	for _, name := range []string{"kimi-code-plan-cn", "kimi-code-plan-global", "zai-coding-plan", "openai-codex"} {
		t.Run(name, func(t *testing.T) {
			auth := "Bearer dynamic-secret"
			if name == "zai-coding-plan" {
				auth = "dynamic-secret"
			}
			headers := map[string]string{"Authorization": auth, "ChatGPT-Account-ID": "account-snapshot", "X-OpenAI-Fedramp": "true", "X-Codex-Luna-Reserve": "true"}
			calls, resolves := 0, 0
			client := &http.Client{Timeout: time.Minute, Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
				calls++
				if r.Method != "GET" || r.URL.String() != map[string]string{"kimi-code-plan-cn": "https://api.kimi.com/coding/v1/usages", "kimi-code-plan-global": "https://api.kimi.ai/coding/v1/usages", "zai-coding-plan": "https://api.z.ai/api/monitor/usage/quota/limit", "openai-codex": codexBase + "/usage"}[name] {
					t.Fatalf("route %s %s", r.Method, r.URL)
				}
				if r.Header.Get("Authorization") != auth || r.Header.Get("ChatGPT-Account-ID") != "account-snapshot" || r.Header.Get("X-OpenAI-Fedramp") != "true" || r.Header.Get("X-Codex-Luna-Reserve") != "" {
					t.Fatalf("incorrect snapshot headers")
				}
				deadline, ok := r.Context().Deadline()
				if !ok || time.Until(deadline) > 10*time.Second {
					t.Fatal("missing bound")
				}
				return response(200, fixture(t, map[string]string{"kimi-code-plan-cn": "kimi", "kimi-code-plan-global": "kimi", "zai-coding-plan": "zai", "openai-codex": "codex"}[name])), nil
			})}
			c := New(Options{Client: client, Now: func() time.Time { return testNow }, Resolve: func(context.Context, llm.ProviderConfig) (Credentials, error) {
				resolves++
				return Credentials{APIKey: "ignored", Headers: headers}, nil
			}})
			a, err := c.Resolve(context.Background(), config(name))
			if err != nil {
				t.Fatal(err)
			}
			headers["Authorization"] = "changed"
			headers["ChatGPT-Account-ID"] = "changed"
			for i := 0; i < 2; i++ {
				out, err := a.Status(context.Background())
				if err != nil {
					t.Fatal(err)
				}
				if out.Provider != name || !out.FetchedAt.Equal(testNow) {
					t.Fatalf("metadata %+v", out)
				}
			}
			if resolves != 1 || calls != 2 || client.Timeout != time.Minute || c.http.Timeout != 10*time.Second {
				t.Fatal("snapshot/client mutation")
			}
		})
	}
}

func TestResolveSecondCodexAccountViaProfile(t *testing.T) {
	// A second ChatGPT subscription account under a different provider name
	// resolves to the Codex quota endpoints via its profile, carries the Codex
	// identity headers, and reports limits under its own provider name.
	pc := llm.ProviderConfig{Name: "openai-codex-2", APIType: "responses", Profile: llm.ProfileCodex, BaseURL: "https://chatgpt.com/backend-api/codex", APIKey: "secret-static-key"}
	client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.Method != "GET" || r.URL.String() != codexBase+"/usage" {
			t.Fatalf("route %s %s", r.Method, r.URL)
		}
		if r.Header.Get("originator") == "" {
			t.Fatal("codex identity headers missing on quota request")
		}
		return response(200, fixture(t, "codex")), nil
	})}
	c := New(Options{Client: client, Now: func() time.Time { return testNow }})
	a, err := c.Resolve(context.Background(), pc)
	if err != nil {
		t.Fatal(err)
	}
	out, err := a.Status(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if out.Provider != "openai-codex-2" {
		t.Fatalf("limits reported under %q, want the provider's own name", out.Provider)
	}
}

func TestResolveSecondZaiAccountViaProfile(t *testing.T) {
	// A renamed Z.AI Coding Plan provider keeps the Z.AI quota route and the
	// raw-token Authorization header (no Bearer prefix) through its profile.
	pc := llm.ProviderConfig{Name: "zai-coding-plan-2", Profile: llm.ProfileZAICodingPlan, BaseURL: "https://api.z.ai/api/coding/paas/v4", APIKey: "secret-static-key"}
	client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.Method != "GET" || r.URL.String() != "https://api.z.ai/api/monitor/usage/quota/limit" {
			t.Fatalf("route %s %s", r.Method, r.URL)
		}
		if got := r.Header.Get("Authorization"); got != "secret-static-key" {
			t.Fatalf("Authorization = %q, want the raw token without Bearer for Z.AI", got)
		}
		return response(200, fixture(t, "zai")), nil
	})}
	c := New(Options{Client: client, Now: func() time.Time { return testNow }})
	a, err := c.Resolve(context.Background(), pc)
	if err != nil {
		t.Fatal(err)
	}
	out, err := a.Status(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if out.Provider != "zai-coding-plan-2" {
		t.Fatalf("limits reported under %q, want the provider's own name", out.Provider)
	}
}

func TestStaticAuthAndSafeFailures(t *testing.T) {
	for _, name := range []string{"kimi-code-plan-cn", "kimi-code-plan-global", "zai-coding-plan", "openai-codex"} {
		t.Run(name, func(t *testing.T) {
			a := account(t, name, func(r *http.Request) (*http.Response, error) {
				want := "Bearer secret-static-key"
				if name == "zai-coding-plan" {
					want = "secret-static-key"
				}
				if r.Header.Get("Authorization") != want {
					t.Fatal("incorrect static auth")
				}
				return response(401, "secret-private-response"), nil
			})
			_, err := a.Status(context.Background())
			limitsError(t, err, "unauthorized")
		})
	}
	pc := config("openai-codex")
	pc.APIKey = ""
	_, err := New(Options{}).Resolve(context.Background(), pc)
	limitsError(t, err, "missing_auth")
	_, err = New(Options{Resolve: func(context.Context, llm.ProviderConfig) (Credentials, error) {
		return Credentials{}, errors.New("secret-refresh-token")
	}}).Resolve(context.Background(), pc)
	limitsError(t, err, "missing_auth")
	for status, code := range map[int]string{401: "unauthorized", 403: "unauthorized", 404: "unsupported_endpoint", 429: "throttled", 500: "upstream_error", 503: "upstream_error", 302: "redirect"} {
		t.Run(code, func(t *testing.T) {
			calls := 0
			a := account(t, "openai-codex", func(*http.Request) (*http.Response, error) {
				calls++
				return response(status, "secret-private-response"), nil
			})
			_, err := a.Status(context.Background())
			limitsError(t, err, code)
			if calls != 1 {
				t.Fatal("retried")
			}
		})
	}
}

func TestTransportBoundsAndCancellation(t *testing.T) {
	for name, body := range map[string]string{"oversized": strings.Repeat(" ", maxBody+1), "long-string": `{"plan_type":"` + strings.Repeat("x", maxString+1) + `"}`, "long-array": `{"additional_rate_limits":[` + strings.Repeat(`{},`, maxItems) + `{}]}`, "html": "<html>secret</html>", "multiple": `{} {}`, "null": "null", "array": "[]", "malformed": "{"} {
		t.Run(name, func(t *testing.T) {
			a := account(t, "openai-codex", func(*http.Request) (*http.Response, error) { return response(200, body), nil })
			_, err := a.Status(context.Background())
			limitsError(t, err, "invalid_payload")
		})
	}
	for _, cancellation := range []bool{false, true} {
		ctx := context.Background()
		code := "timeout"
		if cancellation {
			var cancel context.CancelFunc
			ctx, cancel = context.WithCancel(ctx)
			cancel()
			code = "canceled"
		}
		a := account(t, "openai-codex", func(r *http.Request) (*http.Response, error) {
			if cancellation {
				return nil, r.Context().Err()
			}
			return nil, context.DeadlineExceeded
		})
		_, err := a.Status(ctx)
		limitsError(t, err, code)
	}
	a := account(t, "openai-codex", func(*http.Request) (*http.Response, error) { return nil, errors.New("secret URL headers") })
	_, err := a.Status(context.Background())
	limitsError(t, err, "transport_error")
}

func TestRedirectDoesNotForwardCredentials(t *testing.T) {
	destinationCalls := 0
	destination := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		destinationCalls++
		t.Error("credentials forwarded to redirect destination")
	}))
	defer destination.Close()
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") == "" {
			t.Error("missing auth")
		}
		http.Redirect(w, r, destination.URL, http.StatusTemporaryRedirect)
	}))
	defer origin.Close()
	// Map only the fixed official request onto httptest; net/http still handles redirects.
	tr := http.DefaultTransport.(*http.Transport).Clone()
	defer tr.CloseIdleConnections()
	a := account(t, "openai-codex", func(r *http.Request) (*http.Response, error) {
		clone := r.Clone(r.Context())
		u := *r.URL
		clone.URL = &u
		clone.URL.Scheme = "http"
		clone.URL.Host = strings.TrimPrefix(origin.URL, "http://")
		return tr.RoundTrip(clone)
	})
	_, err := a.Status(context.Background())
	limitsError(t, err, "redirect")
	if destinationCalls != 0 {
		t.Fatal("followed redirect")
	}
}

func TestResetIdentityRetryAndOutcomes(t *testing.T) {
	req := protocol.ResetRequest{Provider: "openai-codex", CreditID: "selected-credit", RequestID: "stable-request"}
	for _, outcome := range []string{"reset", "nothing_to_reset", "no_credit", "already_redeemed"} {
		t.Run(outcome, func(t *testing.T) {
			calls := 0
			a := account(t, "openai-codex", func(r *http.Request) (*http.Response, error) {
				calls++
				if r.Method != "POST" || r.URL.String() != codexBase+"/rate-limit-reset-credits/consume" || r.GetBody != nil {
					t.Fatal("wrong redemption request")
				}
				b, _ := io.ReadAll(r.Body)
				var got map[string]string
				if json.Unmarshal(b, &got) != nil || !reflect.DeepEqual(got, map[string]string{"credit_id": req.CreditID, "redeem_request_id": req.RequestID}) {
					t.Fatalf("body %s", b)
				}
				return response(200, `{"code":"`+outcome+`","windows_reset":2}`), nil
			})
			result, err := a.Reset(context.Background(), req)
			if err != nil || result.Outcome != outcome || result.CreditID != req.CreditID || result.RequestID != req.RequestID || calls != 1 || result.Status != nil || result.Credits != nil || result.WindowsReset == nil || *result.WindowsReset != 2 {
				t.Fatalf("result %+v err %v calls %d", result, err, calls)
			}
		})
	}
	calls := 0
	var bodies []string
	a := account(t, "openai-codex", func(r *http.Request) (*http.Response, error) {
		calls++
		if r.Method != "POST" {
			t.Fatal("preflight or refresh")
		}
		b, _ := io.ReadAll(r.Body)
		bodies = append(bodies, string(b))
		if calls == 1 {
			return nil, io.ErrUnexpectedEOF
		}
		return response(200, `{"code":"already_redeemed"}`), nil
	})
	first, err := a.Reset(context.Background(), req)
	e := limitsError(t, err, "transport_error")
	if !e.Indeterminate || first.Outcome != "indeterminate" || calls != 1 {
		t.Fatalf("ambiguous outcome %+v", first)
	}
	// Backend has consumed and removed the credit. Retry must still reach it with
	// identical identity and expose already_redeemed, not locally claim no_credit.
	second, err := a.Reset(context.Background(), req)
	if err != nil || second.Outcome != "already_redeemed" || len(bodies) != 2 || bodies[0] != bodies[1] {
		t.Fatalf("retry %+v %v bodies %v", second, err, bodies)
	}
	a = account(t, "openai-codex", func(*http.Request) (*http.Response, error) { return response(200, `{"code":"future_outcome"}`), nil })
	result, err := a.Reset(context.Background(), req)
	if !limitsError(t, err, "unknown_outcome").Indeterminate || result.Outcome != "indeterminate" {
		t.Fatal("unknown outcome not ambiguous")
	}
	req.RequestID = ""
	_, err = a.Reset(context.Background(), req)
	limitsError(t, err, "invalid_request")
}
