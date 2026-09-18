// Package subscription reads account-wide subscription quotas without changing
// inference credentials, retry policy, or token/cost accounting.
package subscription

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"harness/internal/codexclient"
	"harness/internal/llm"
	"harness/internal/modelproxy/protocol"
)

const (
	maxBody         = 1 << 20
	maxItems        = 256
	maxString       = 4096
	upstreamTimeout = 10 * time.Second
)

type Credentials struct {
	APIKey  string
	Headers map[string]string
}

type Options struct {
	Client  *http.Client
	Now     func() time.Time
	Resolve func(context.Context, llm.ProviderConfig) (Credentials, error)
	// CodexClientVersion is the vendor-compatibility Codex CLI version reported
	// in the Codex identity headers; empty omits the version segment.
	CodexClientVersion string
}

type Client struct {
	http               *http.Client
	now                func() time.Time
	resolve            func(context.Context, llm.ProviderConfig) (Credentials, error)
	codexClientVersion string
}

// Account captures one credential snapshot, shared by an operation and its
// best-effort refreshes. It never resolves or refreshes credentials itself.
type Account struct {
	client   *Client
	provider string
	kind     string
	headers  http.Header
	// usagesURL is the quota endpoint for deployments whose host varies with
	// the configured base URL (Kimi Code Plan CN vs global); empty uses the
	// integration's default endpoint.
	usagesURL string
}

func New(opts Options) *Client {
	h := http.Client{}
	if opts.Client != nil {
		h = *opts.Client
	}
	if h.Timeout <= 0 || h.Timeout > upstreamTimeout {
		h.Timeout = upstreamTimeout
	}
	h.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	// The standard transport retries idempotent requests on reused connections.
	// These infrequent account operations use fresh HTTP/1 connections instead,
	// including when a caller supplies a configured standard transport. Custom
	// injected RoundTrippers own their transport behavior.
	transport := h.Transport
	if transport == nil {
		transport = http.DefaultTransport
	}
	if standard, ok := transport.(*http.Transport); ok {
		isolated := standard.Clone()
		isolated.DisableKeepAlives = true
		isolated.ForceAttemptHTTP2 = false
		isolated.TLSNextProto = make(map[string]func(string, *tls.Conn) http.RoundTripper)
		isolated.Protocols = new(http.Protocols)
		isolated.Protocols.SetHTTP1(true)
		// Clone initializes HTTP/2 on the source and can copy its h2 ALPN offer.
		// Disabling the handler alone would negotiate h2 but send HTTP/1 bytes.
		if isolated.TLSClientConfig != nil {
			isolated.TLSClientConfig.NextProtos = []string{"http/1.1"}
		}
		h.Transport = isolated
	}
	// Do not accept credentials from a caller's cookie jar or store provider cookies.
	h.Jar = nil
	if opts.Now == nil {
		opts.Now = time.Now
	}
	return &Client{http: &h, now: opts.Now, resolve: opts.Resolve, codexClientVersion: opts.CodexClientVersion}
}

// QuotaCodex is the quota integration kind for ChatGPT Codex subscription
// providers. The other kinds equal the kimi-code-plan and zai-coding-plan
// profile names.
const QuotaCodex = "codex"

// QuotaKind returns the subscription quota integration for a provider config.
// An explicit profile wins, so multiple subscription accounts (distinct
// provider names sharing one profile) all resolve to the right quota endpoints
// under their own names; absent a profile, legacy name/URL/auth detection
// applies.
func QuotaKind(pc llm.ProviderConfig) string {
	if profile := llm.NormalizeProfile(pc.Profile); profile != "" {
		switch profile {
		case llm.ProfileCodex:
			return QuotaCodex
		case llm.ProfileKimiCodePlan, llm.ProfileZAICodingPlan:
			return profile
		}
		return ""
	}
	if pc.CodexBackend() {
		return QuotaCodex
	}
	switch pc.Name {
	case llm.KimiCodePlanCNProviderName, llm.KimiCodePlanGlobalProviderName:
		return llm.ProfileKimiCodePlan
	case llm.ProfileZAICodingPlan:
		return pc.Name
	}
	return ""
}

// SupportedProvider reports whether subscription quota reporting is available
// for the provider config.
func SupportedProvider(pc llm.ProviderConfig) bool {
	return QuotaKind(pc) != ""
}

func (c *Client) Resolve(ctx context.Context, pc llm.ProviderConfig) (*Account, error) {
	ctx, cancel := context.WithTimeout(ctx, upstreamTimeout)
	defer cancel()
	kind := QuotaKind(pc)
	if kind == "" {
		return nil, safeError("unsupported_provider", "subscription limits are not supported for this provider")
	}
	if !officialEndpoint(pc, kind) {
		return nil, safeError("unsupported_endpoint", "subscription limits require an official provider endpoint and known base path")
	}
	if err := ctx.Err(); err != nil {
		return nil, transportError(err)
	}
	cred := Credentials{APIKey: pc.APIKey}
	if c.resolve != nil {
		var err error
		cred, err = c.resolve(ctx, pc)
		if err != nil {
			if ctx.Err() != nil {
				return nil, transportError(ctx.Err())
			}
			return nil, safeError("missing_auth", "subscription credentials could not be resolved")
		}
	}
	h := make(http.Header)
	if cred.APIKey != "" {
		value := cred.APIKey
		if kind != llm.ProfileZAICodingPlan {
			value = "Bearer " + value
		}
		h.Set("Authorization", value)
	}
	if len(cred.Headers) > maxItems {
		return nil, safeError("missing_auth", "invalid subscription credentials")
	}
	for k, v := range cred.Headers {
		// Passive quota readers must not opt into Luna Reserve behavior.
		if strings.Contains(strings.ToLower(k), "luna") {
			continue
		}
		if !validHeaderName(k) || len(v) > 16384 || strings.ContainsAny(v, "\r\n\x00") {
			return nil, safeError("missing_auth", "invalid subscription credentials")
		}
		h.Set(k, v)
	}
	if strings.TrimSpace(h.Get("Authorization")) == "" || len(h.Get("Authorization")) > 16384 || strings.ContainsAny(h.Get("Authorization"), "\r\n\x00") {
		return nil, safeError("missing_auth", "subscription credentials are missing or invalid")
	}
	// The Codex account endpoints sit behind the same default client as
	// inference, so they carry the CLI's identity headers too. Applied after
	// credential headers so it cannot be shadowed by resolved auth values.
	if kind == QuotaCodex {
		codexclient.Apply(h, c.codexClientVersion)
	}
	var usagesURL string
	if kind == llm.ProfileKimiCodePlan {
		// officialEndpoint has already validated scheme/host/path.
		u, _ := url.Parse(pc.BaseURL)
		usagesURL = "https://" + strings.ToLower(u.Hostname()) + "/coding/v1/usages"
	}
	return &Account{client: c, provider: pc.Name, kind: kind, headers: h, usagesURL: usagesURL}, nil
}

func validHeaderName(s string) bool {
	if len(s) == 0 || len(s) > 128 {
		return false
	}
	for _, r := range s {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || strings.ContainsRune("!#$%&'*+-.^_`|~", r)) {
			return false
		}
	}
	return true
}

func officialEndpoint(pc llm.ProviderConfig, kind string) bool {
	u, err := url.Parse(pc.BaseURL)
	if err != nil || u.Scheme != "https" || u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || u.RawPath != "" || (u.Port() != "" && u.Port() != "443") {
		return false
	}
	host, path := strings.ToLower(u.Hostname()), strings.TrimSuffix(u.Path, "/")
	switch kind {
	case llm.ProfileKimiCodePlan:
		return (host == "api.kimi.com" || host == "api.kimi.ai") && (path == "" || path == "/coding/v1")
	case "zai-coding-plan":
		return host == "api.z.ai" && (path == "" || path == "/api/coding/paas/v4" || path == "/api/paas/v4" || path == "/api/anthropic")
	case QuotaCodex:
		return host == "chatgpt.com" && (path == "" || path == "/backend-api" || path == "/backend-api/codex")
	}
	return false
}

func (a *Account) Status(ctx context.Context) (protocol.ProviderLimits, error) {
	var out protocol.ProviderLimits
	var err error
	switch a.kind {
	case llm.ProfileKimiCodePlan:
		out, err = a.kimi(ctx)
	case "zai-coding-plan":
		out, err = a.zai(ctx)
	case QuotaCodex:
		out, err = a.codex(ctx)
	default:
		err = safeError("unsupported_provider", "subscription limits are not supported for this provider")
	}
	out.Provider, out.FetchedAt = a.provider, a.client.now().UTC()
	return out, err
}

func safeError(code, message string) *protocol.LimitsError {
	return &protocol.LimitsError{Code: code, Message: message}
}
func invalidPayload() *protocol.LimitsError {
	return safeError("invalid_payload", "provider returned invalid subscription data")
}
func transportError(err error) *protocol.LimitsError {
	if errors.Is(err, context.Canceled) {
		return safeError("canceled", "subscription request was canceled")
	}
	var ne net.Error
	if errors.Is(err, context.DeadlineExceeded) || errors.As(err, &ne) && ne.Timeout() {
		return safeError("timeout", "subscription request timed out")
	}
	return safeError("transport_error", "subscription request failed")
}

func (a *Account) request(ctx context.Context, method, endpoint string, body []byte, out any) error {
	ctx, cancel := context.WithTimeout(ctx, upstreamTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, method, endpoint, bytes.NewReader(body))
	if err != nil {
		return safeError("invalid_request", "invalid subscription request")
	}
	// A non-replayable POST body also prevents net/http's transport retry path.
	if method == http.MethodPost {
		req.GetBody = nil
	}
	req.Header = a.headers.Clone()
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := a.client.http.Do(req)
	if err != nil {
		return transportError(err)
	}
	defer resp.Body.Close()
	switch {
	case resp.StatusCode == 401 || resp.StatusCode == 403:
		return safeError("unauthorized", "provider rejected subscription credentials")
	case resp.StatusCode == 404:
		return safeError("unsupported_endpoint", "provider subscription endpoint is unavailable")
	case resp.StatusCode == 429:
		return safeError("throttled", "provider throttled the subscription request")
	case resp.StatusCode >= 300 && resp.StatusCode < 400:
		return safeError("redirect", "provider subscription redirects are disabled")
	case resp.StatusCode < 200 || resp.StatusCode >= 300:
		return safeError("upstream_error", "provider subscription service is unavailable")
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, maxBody+1))
	if err != nil {
		return transportError(err)
	}
	if len(b) > maxBody || !boundedJSON(b) {
		return invalidPayload()
	}
	if err := json.Unmarshal(b, out); err != nil {
		return invalidPayload()
	}
	return nil
}

// Bound every value, including unknown fields, while typed decoders below
// validate the provider-specific known shapes and tolerate schema additions.
func boundedJSON(b []byte) bool {
	d := json.NewDecoder(bytes.NewReader(b))
	d.UseNumber()
	var value any
	if d.Decode(&value) != nil {
		return false
	}
	if _, ok := value.(map[string]any); !ok {
		return false
	}
	var extra any
	if d.Decode(&extra) != io.EOF {
		return false
	}
	var valid func(any, int) bool
	valid = func(v any, depth int) bool {
		if depth > 32 {
			return false
		}
		switch v := v.(type) {
		case string:
			return len(v) <= maxString
		case []any:
			if len(v) > maxItems {
				return false
			}
			for _, item := range v {
				if !valid(item, depth+1) {
					return false
				}
			}
		case map[string]any:
			if len(v) > maxItems {
				return false
			}
			for key, item := range v {
				if len(key) > maxString || !valid(item, depth+1) {
					return false
				}
			}
		}
		return true
	}
	return valid(value, 0)
}
