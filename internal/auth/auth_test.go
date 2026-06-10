package auth

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestParseCommandTokenPlainAndJSON(t *testing.T) {
	now := time.Date(2026, 6, 15, 12, 0, 0, 0, time.UTC)
	plain, err := parseCommandToken([]byte(" tok \n"), now)
	if err != nil {
		t.Fatalf("plain token: %v", err)
	}
	if plain.AccessToken != "tok" || plain.TokenType != "Bearer" {
		t.Fatalf("plain token = %+v", plain)
	}

	js, err := parseCommandToken([]byte(`{"access_token":"json-token","expires_in":60}`), now)
	if err != nil {
		t.Fatalf("json token: %v", err)
	}
	if js.AccessToken != "json-token" || !js.Expiry.Equal(now.Add(time.Minute)) {
		t.Fatalf("json token = %+v", js)
	}
}

func TestCodexOAuthDeviceLoginStoresToken(t *testing.T) {
	now := time.Date(2026, 6, 15, 12, 0, 0, 0, time.UTC)
	access := testJWT(t, map[string]any{"exp": now.Add(time.Hour).Unix()})
	idToken := testJWT(t, map[string]any{
		"https://api.openai.com/auth": map[string]any{
			"chatgpt_account_id":         "account-123",
			"chatgpt_account_is_fedramp": true,
		},
	})
	var paths []string
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/accounts/deviceauth/usercode":
			var body map[string]string
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Fatalf("decode usercode body: %v", err)
			}
			if body["client_id"] != "client" {
				t.Fatalf("client_id = %q, want client", body["client_id"])
			}
			_, _ = w.Write([]byte(`{"device_auth_id":"device-1","user_code":"ABCD-EFGH","interval":"1"}`))
		case "/api/accounts/deviceauth/token":
			var body map[string]string
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Fatalf("decode token body: %v", err)
			}
			if body["device_auth_id"] != "device-1" || body["user_code"] != "ABCD-EFGH" {
				t.Fatalf("unexpected poll body: %v", body)
			}
			_, _ = w.Write([]byte(`{"authorization_code":"code-1","code_challenge":"challenge","code_verifier":"verifier"}`))
		case "/oauth/token":
			if err := r.ParseForm(); err != nil {
				t.Fatalf("ParseForm: %v", err)
			}
			if r.Form.Get("grant_type") != "authorization_code" ||
				r.Form.Get("code") != "code-1" ||
				r.Form.Get("client_id") != "client" ||
				r.Form.Get("code_verifier") != "verifier" ||
				r.Form.Get("redirect_uri") != srv.URL+"/deviceauth/callback" {
				t.Fatalf("unexpected exchange form: %v", r.Form)
			}
			_, _ = fmt.Fprintf(w, `{"access_token":%q,"id_token":%q,"refresh_token":"refresh-1"}`, access, idToken)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	dir := t.TempDir()
	var out strings.Builder
	cfg := Config{Type: TypeCodexOAuth, Issuer: srv.URL, ClientID: "client", TokenFile: "codex.json"}
	if err := Login(context.Background(), cfg, LoginOptions{
		Name:      "openai-codex",
		ConfigDir: dir,
		Client:    srv.Client(),
		Stdout:    &out,
		Now:       func() time.Time { return now },
	}); err != nil {
		t.Fatalf("Login: %v", err)
	}
	if got := out.String(); !strings.Contains(got, srv.URL+"/codex/device") || !strings.Contains(got, "ABCD-EFGH") {
		t.Fatalf("login prompt = %q", got)
	}
	if got, want := strings.Join(paths, ","), "/api/accounts/deviceauth/usercode,/api/accounts/deviceauth/token,/oauth/token"; got != want {
		t.Fatalf("paths = %s, want %s", got, want)
	}
	stored, err := readTokenFile(filepath.Join(dir, "codex.json"))
	if err != nil {
		t.Fatalf("read stored token: %v", err)
	}
	if stored.AccessToken != access || stored.IDToken != idToken || stored.RefreshToken != "refresh-1" {
		t.Fatalf("stored token = %+v", stored)
	}
	if stored.AccountID != "account-123" || !stored.FedRAMP {
		t.Fatalf("stored account claims = account %q fedramp %v", stored.AccountID, stored.FedRAMP)
	}
	if !stored.Expiry.Equal(now.Add(time.Hour)) {
		t.Fatalf("stored expiry = %s, want %s", stored.Expiry, now.Add(time.Hour))
	}
}

func TestCodexOAuthRefreshReturnsChatGPTHeaders(t *testing.T) {
	now := time.Date(2026, 6, 15, 12, 0, 0, 0, time.UTC)
	access := testJWT(t, map[string]any{"exp": now.Add(2 * time.Hour).Unix()})
	idToken := testJWT(t, map[string]any{
		"https://api.openai.com/auth": map[string]any{
			"chatgpt_account_id":         "account-new",
			"chatgpt_account_is_fedramp": true,
		},
	})
	var sawRefresh bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/oauth/token" {
			http.NotFound(w, r)
			return
		}
		var body map[string]string
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("decode refresh body: %v", err)
		}
		if body["grant_type"] != "refresh_token" || body["refresh_token"] != "rt-old" || body["client_id"] != "client" {
			t.Fatalf("unexpected refresh body: %v", body)
		}
		sawRefresh = true
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"access_token":%q,"id_token":%q,"refresh_token":"rt-new"}`, access, idToken)
	}))
	defer srv.Close()

	dir := t.TempDir()
	cfg := Config{Type: TypeCodexOAuth, Issuer: srv.URL, ClientID: "client", TokenFile: "codex.json"}
	old := token{AccessToken: "expired", RefreshToken: "rt-old", IDToken: idToken, AccountID: "account-old", Expiry: now.Add(-time.Hour)}
	body, _ := json.Marshal(old)
	if err := os.WriteFile(filepath.Join(dir, "codex.json"), body, 0o600); err != nil {
		t.Fatalf("write token: %v", err)
	}
	src, err := NewSource(cfg, Options{Name: "openai-codex", ConfigDir: dir, Now: func() time.Time { return now }, Client: srv.Client()})
	if err != nil {
		t.Fatalf("NewSource: %v", err)
	}
	headers, err := src.Headers(context.Background())
	if err != nil {
		t.Fatalf("Headers: %v", err)
	}
	if headers["Authorization"] != "Bearer "+access ||
		headers["ChatGPT-Account-ID"] != "account-new" ||
		headers["X-OpenAI-Fedramp"] != "true" ||
		!sawRefresh {
		t.Fatalf("headers=%v sawRefresh=%v", headers, sawRefresh)
	}
	stored, err := readTokenFile(filepath.Join(dir, "codex.json"))
	if err != nil {
		t.Fatalf("read stored token: %v", err)
	}
	if stored.RefreshToken != "rt-new" || stored.AccountID != "account-new" || stored.RefreshFailed {
		t.Fatalf("stored token after refresh = %+v", stored)
	}
}

func TestCodexOAuthTerminalRefreshFailureIsQuarantined(t *testing.T) {
	now := time.Date(2026, 6, 15, 12, 0, 0, 0, time.UTC)
	idToken := testJWT(t, map[string]any{
		"https://api.openai.com/auth": map[string]any{"chatgpt_account_id": "account-123"},
	})
	refreshCalls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		refreshCalls++
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":{"code":"refresh_token_invalidated","message":"revoked"}}`))
	}))
	defer srv.Close()

	dir := t.TempDir()
	cfg := Config{Type: TypeCodexOAuth, Issuer: srv.URL, ClientID: "client", TokenFile: "codex.json"}
	old := token{AccessToken: "expired", RefreshToken: "rt-old", IDToken: idToken, AccountID: "account-123", Expiry: now.Add(-time.Hour)}
	body, _ := json.Marshal(old)
	if err := os.WriteFile(filepath.Join(dir, "codex.json"), body, 0o600); err != nil {
		t.Fatalf("write token: %v", err)
	}
	src, err := NewSource(cfg, Options{Name: "openai-codex", ConfigDir: dir, Now: func() time.Time { return now }, Client: srv.Client()})
	if err != nil {
		t.Fatalf("NewSource: %v", err)
	}
	if _, err := src.Headers(context.Background()); err == nil || !strings.Contains(err.Error(), "cannot be refreshed") {
		t.Fatalf("first Headers error = %v, want reauth error", err)
	}
	stored, err := readTokenFile(filepath.Join(dir, "codex.json"))
	if err != nil {
		t.Fatalf("read stored token: %v", err)
	}
	if !stored.RefreshFailed || !strings.Contains(stored.RefreshFailure, "refresh_token_invalidated") {
		t.Fatalf("stored quarantine = %+v", stored)
	}
	if _, err := src.Headers(context.Background()); err == nil || !strings.Contains(err.Error(), "cannot be refreshed") {
		t.Fatalf("second Headers error = %v, want reauth error", err)
	}
	if refreshCalls != 1 {
		t.Fatalf("refresh calls = %d, want 1", refreshCalls)
	}
}

func testJWT(t *testing.T, claims map[string]any) string {
	t.Helper()
	encode := func(v any) string {
		data, err := json.Marshal(v)
		if err != nil {
			t.Fatalf("marshal JWT part: %v", err)
		}
		return base64.RawURLEncoding.EncodeToString(data)
	}
	return encode(map[string]string{"alg": "none", "typ": "JWT"}) + "." + encode(claims) + ".sig"
}

func TestSourceTokenCommandCaches(t *testing.T) {
	dir := t.TempDir()
	script := filepath.Join(dir, "token.sh")
	counter := filepath.Join(dir, "count")
	if err := os.WriteFile(script, []byte("#!/bin/sh\nn=$(cat \"$1\" 2>/dev/null || echo 0)\nn=$((n+1))\necho \"$n\" > \"$1\"\nprintf '{\"access_token\":\"tok-%s\",\"expires_in\":120}' \"$n\"\n"), 0o700); err != nil {
		t.Fatalf("write script: %v", err)
	}
	now := time.Date(2026, 6, 15, 12, 0, 0, 0, time.UTC)
	src, err := NewSource(Config{
		Type:    TypeTokenCommand,
		Command: script,
		Args:    []string{counter},
	}, Options{Now: func() time.Time { return now }})
	if err != nil {
		t.Fatalf("NewSource: %v", err)
	}
	h1, err := src.Headers(context.Background())
	if err != nil {
		t.Fatalf("Headers first: %v", err)
	}
	h2, err := src.Headers(context.Background())
	if err != nil {
		t.Fatalf("Headers second: %v", err)
	}
	if h1["Authorization"] != "Bearer tok-1" || h2["Authorization"] != "Bearer tok-1" {
		t.Fatalf("headers not cached: first=%v second=%v", h1, h2)
	}
	data, _ := os.ReadFile(counter)
	if strings.TrimSpace(string(data)) != "1" {
		t.Fatalf("command ran %q times, want 1", strings.TrimSpace(string(data)))
	}
}

func TestOAuthRefreshLoadsAndStoresToken(t *testing.T) {
	now := time.Date(2026, 6, 15, 12, 0, 0, 0, time.UTC)
	var sawRefresh bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			t.Fatalf("ParseForm: %v", err)
		}
		if r.Form.Get("grant_type") != "refresh_token" || r.Form.Get("refresh_token") != "rt-old" {
			t.Fatalf("unexpected form: %v", r.Form)
		}
		sawRefresh = true
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"access_token":"fresh","refresh_token":"rt-new","expires_in":3600}`))
	}))
	defer srv.Close()

	dir := t.TempDir()
	cfg := Config{
		Type:            TypeOAuth2,
		Flow:            FlowDeviceCode,
		ClientID:        "client",
		TokenURL:        srv.URL,
		DeviceURL:       srv.URL + "/device",
		TokenFile:       "tok.json",
		CacheTTLSeconds: 1,
	}
	old := token{AccessToken: "expired", RefreshToken: "rt-old", Expiry: now.Add(-time.Hour)}
	body, _ := json.Marshal(old)
	if err := os.WriteFile(filepath.Join(dir, "tok.json"), body, 0o600); err != nil {
		t.Fatalf("write token: %v", err)
	}
	src, err := NewSource(cfg, Options{Name: "p", ConfigDir: dir, Now: func() time.Time { return now }, Client: srv.Client()})
	if err != nil {
		t.Fatalf("NewSource: %v", err)
	}
	headers, err := src.Headers(context.Background())
	if err != nil {
		t.Fatalf("Headers: %v", err)
	}
	if headers["Authorization"] != "Bearer fresh" || !sawRefresh {
		t.Fatalf("refresh failed: headers=%v saw=%v", headers, sawRefresh)
	}
	stored, err := readTokenFile(filepath.Join(dir, "tok.json"))
	if err != nil {
		t.Fatalf("read stored token: %v", err)
	}
	if stored.AccessToken != "fresh" || stored.RefreshToken != "rt-new" {
		t.Fatalf("stored token = %+v", stored)
	}
}
