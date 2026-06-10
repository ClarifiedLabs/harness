package auth

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	TypeTokenCommand = "token_command"
	TypeOAuth2       = "oauth2"
	TypeCodexOAuth   = "codex_oauth"

	FlowAuthCodePKCE = "auth_code_pkce"
	FlowDeviceCode   = "device_code"

	defaultCommandTimeout      = 10 * time.Second
	defaultCacheTTL            = 5 * time.Minute
	defaultExpirySkew          = 30 * time.Second
	defaultCodexExpirySkew     = 5 * time.Minute
	defaultCodexIssuer         = "https://auth.openai.com"
	defaultCodexClientID       = "app_EMoamEEZ73f0CkXaXp7hrann"
	defaultCodexDevicePollWait = 15 * time.Minute
)

// Config is the shared auth configuration used by model providers and HTTP MCP
// servers. The zero value means no dynamic auth.
type Config struct {
	Type string `json:"type"`

	// Header controls how the acquired access token is sent. The default is
	// Authorization: Bearer <token>. Set Scheme to "none" to send the raw token.
	Header string `json:"header,omitempty"`
	Scheme string `json:"scheme,omitempty"`

	// token_command
	Command         string   `json:"command,omitempty"`
	Args            []string `json:"args,omitempty"`
	CacheTTLSeconds int      `json:"cache_ttl_seconds,omitempty"`
	TimeoutSeconds  int      `json:"timeout_seconds,omitempty"`

	// oauth2
	Flow            string   `json:"flow,omitempty"`
	ClientID        string   `json:"client_id,omitempty"`
	ClientSecret    string   `json:"client_secret,omitempty"`
	ClientSecretEnv string   `json:"client_secret_env,omitempty"`
	AuthURL         string   `json:"auth_url,omitempty"`
	TokenURL        string   `json:"token_url,omitempty"`
	DeviceURL       string   `json:"device_url,omitempty"`
	Issuer          string   `json:"issuer,omitempty"`
	Scopes          []string `json:"scopes,omitempty"`
	RedirectURI     string   `json:"redirect_uri,omitempty"`
	TokenFile       string   `json:"token_file,omitempty"`
}

// Source resolves request headers from Config, caching short-lived tokens in
// memory. It is safe for concurrent use.
type Source struct {
	cfg       Config
	name      string
	configDir string
	getenv    func(string) string
	client    *http.Client
	now       func() time.Time

	mu     sync.Mutex
	cached token
}

type token struct {
	AccessToken  string    `json:"access_token"`
	RefreshToken string    `json:"refresh_token,omitempty"`
	IDToken      string    `json:"id_token,omitempty"`
	TokenType    string    `json:"token_type,omitempty"`
	Expiry       time.Time `json:"expiry,omitempty"`
	Scope        string    `json:"scope,omitempty"`
	AccountID    string    `json:"account_id,omitempty"`
	FedRAMP      bool      `json:"fedramp,omitempty"`

	RefreshFailed  bool   `json:"refresh_failed,omitempty"`
	RefreshFailure string `json:"refresh_failure,omitempty"`
}

// Options configures a Source.
type Options struct {
	Name      string
	ConfigDir string
	Getenv    func(string) string
	Client    *http.Client
	Now       func() time.Time
}

func NewSource(cfg Config, opts Options) (*Source, error) {
	if err := Validate(cfg); err != nil {
		return nil, err
	}
	getenv := opts.Getenv
	if getenv == nil {
		getenv = os.Getenv
	}
	client := opts.Client
	if client == nil {
		client = http.DefaultClient
	}
	now := opts.Now
	if now == nil {
		now = time.Now
	}
	return &Source{
		cfg:       cfg,
		name:      opts.Name,
		configDir: opts.ConfigDir,
		getenv:    getenv,
		client:    client,
		now:       now,
	}, nil
}

func Validate(cfg Config) error {
	switch normalizeType(cfg.Type) {
	case "":
		return nil
	case TypeTokenCommand:
		if strings.TrimSpace(cfg.Command) == "" {
			return errors.New("auth: token_command requires command")
		}
	case TypeOAuth2:
		if strings.TrimSpace(cfg.Flow) == "" {
			return errors.New("auth: oauth2 requires flow")
		}
		if strings.TrimSpace(cfg.ClientID) == "" {
			return errors.New("auth: oauth2 requires client_id")
		}
		if strings.TrimSpace(cfg.TokenURL) == "" {
			return errors.New("auth: oauth2 requires token_url")
		}
		switch cfg.Flow {
		case FlowAuthCodePKCE:
			if strings.TrimSpace(cfg.AuthURL) == "" {
				return errors.New("auth: oauth2 auth_code_pkce requires auth_url")
			}
		case FlowDeviceCode:
			if strings.TrimSpace(cfg.DeviceURL) == "" {
				return errors.New("auth: oauth2 device_code requires device_url")
			}
		default:
			return fmt.Errorf("auth: unsupported oauth2 flow %q", cfg.Flow)
		}
	case TypeCodexOAuth:
		if flow := strings.TrimSpace(cfg.Flow); flow != "" && flow != FlowDeviceCode {
			return fmt.Errorf("auth: codex_oauth only supports %s flow", FlowDeviceCode)
		}
	default:
		return fmt.Errorf("auth: unsupported type %q", cfg.Type)
	}
	return nil
}

// Headers returns request headers carrying the current token.
func (s *Source) Headers(ctx context.Context) (map[string]string, error) {
	if s == nil || normalizeType(s.cfg.Type) == "" {
		return nil, nil
	}
	tok, err := s.currentToken(ctx)
	if err != nil {
		return nil, err
	}
	if tok.AccessToken == "" {
		return nil, errors.New("auth: access token is empty")
	}
	if normalizeType(s.cfg.Type) == TypeCodexOAuth {
		return s.codexHeaders(tok)
	}
	return map[string]string{s.headerName(): s.headerValue(tok.AccessToken)}, nil
}

func (s *Source) currentToken(ctx context.Context) (token, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.validForConfig(s.cached) {
		return s.cached, nil
	}

	switch normalizeType(s.cfg.Type) {
	case TypeTokenCommand:
		tok, err := s.runTokenCommand(ctx)
		if err != nil {
			return token{}, err
		}
		s.cached = tok
		return tok, nil
	case TypeOAuth2:
		tok, err := s.loadStoredToken()
		if err != nil {
			return token{}, err
		}
		if s.validForConfig(tok) {
			s.cached = tok
			return tok, nil
		}
		if tok.RefreshToken == "" {
			return token{}, fmt.Errorf("auth: no valid stored token for %s; run auth login", s.name)
		}
		next, err := s.refresh(ctx, tok.RefreshToken)
		if err != nil {
			return token{}, err
		}
		if next.RefreshToken == "" {
			next.RefreshToken = tok.RefreshToken
		}
		if err := writeTokenFile(s.tokenPath(), next); err != nil {
			return token{}, err
		}
		s.cached = next
		return next, nil
	case TypeCodexOAuth:
		tok, err := s.loadStoredToken()
		if err != nil {
			return token{}, err
		}
		if s.validForConfig(tok) {
			s.cached = tok
			return tok, nil
		}
		if tok.RefreshFailed {
			return token{}, codexReauthError(s.name, tok.RefreshFailure)
		}
		if tok.RefreshToken == "" {
			return token{}, fmt.Errorf("auth: no valid OpenAI Codex token for %s; run auth login %s", s.name, s.name)
		}
		next, err := s.refreshCodex(ctx, tok)
		if err != nil {
			return token{}, err
		}
		if err := writeTokenFile(s.tokenPath(), next); err != nil {
			return token{}, err
		}
		s.cached = next
		return next, nil
	default:
		return token{}, fmt.Errorf("auth: unsupported type %q", s.cfg.Type)
	}
}

func (s *Source) runTokenCommand(ctx context.Context) (token, error) {
	timeout := defaultCommandTimeout
	if s.cfg.TimeoutSeconds > 0 {
		timeout = time.Duration(s.cfg.TimeoutSeconds) * time.Second
	}
	cmdCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	cmd := exec.CommandContext(cmdCtx, s.cfg.Command, s.cfg.Args...)
	out, err := cmd.Output()
	if cmdCtx.Err() != nil {
		return token{}, fmt.Errorf("auth: token command timed out after %s", timeout)
	}
	if err != nil {
		return token{}, fmt.Errorf("auth: token command failed: %w", err)
	}
	tok, err := parseCommandToken(out, s.now())
	if err != nil {
		return token{}, err
	}
	if tok.Expiry.IsZero() {
		ttl := defaultCacheTTL
		if s.cfg.CacheTTLSeconds > 0 {
			ttl = time.Duration(s.cfg.CacheTTLSeconds) * time.Second
		}
		tok.Expiry = s.now().Add(ttl)
	}
	return tok, nil
}

func parseCommandToken(out []byte, now time.Time) (token, error) {
	trimmed := bytes.TrimSpace(out)
	if len(trimmed) == 0 {
		return token{}, errors.New("auth: token command returned empty output")
	}
	if trimmed[0] != '{' {
		return token{AccessToken: string(trimmed), TokenType: "Bearer"}, nil
	}

	var raw struct {
		AccessToken string          `json:"access_token"`
		Token       string          `json:"token"`
		TokenType   string          `json:"token_type"`
		ExpiresIn   int             `json:"expires_in"`
		ExpiresAt   json.RawMessage `json:"expires_at"`
	}
	if err := json.Unmarshal(trimmed, &raw); err != nil {
		return token{}, fmt.Errorf("auth: decode token command output: %w", err)
	}
	access := raw.AccessToken
	if access == "" {
		access = raw.Token
	}
	if access == "" {
		return token{}, errors.New("auth: token command JSON missing access_token")
	}
	tok := token{AccessToken: access, TokenType: raw.TokenType}
	if tok.TokenType == "" {
		tok.TokenType = "Bearer"
	}
	if raw.ExpiresIn > 0 {
		tok.Expiry = now.Add(time.Duration(raw.ExpiresIn) * time.Second)
	}
	if len(raw.ExpiresAt) > 0 && string(raw.ExpiresAt) != "null" {
		expiry, err := parseExpiresAt(raw.ExpiresAt)
		if err != nil {
			return token{}, err
		}
		tok.Expiry = expiry
	}
	return tok, nil
}

func parseExpiresAt(raw json.RawMessage) (time.Time, error) {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		if n, err := strconv.ParseInt(s, 10, 64); err == nil {
			return time.Unix(n, 0), nil
		}
		t, err := time.Parse(time.RFC3339, s)
		if err != nil {
			return time.Time{}, fmt.Errorf("auth: parse expires_at: %w", err)
		}
		return t, nil
	}
	var n int64
	if json.Unmarshal(raw, &n) == nil {
		return time.Unix(n, 0), nil
	}
	return time.Time{}, errors.New("auth: expires_at must be RFC3339 string or unix timestamp")
}

func (s *Source) refresh(ctx context.Context, refreshToken string) (token, error) {
	form := s.clientForm()
	form.Set("grant_type", "refresh_token")
	form.Set("refresh_token", refreshToken)
	return postToken(ctx, s.client, s.cfg.TokenURL, form, s.now())
}

func (s *Source) clientForm() url.Values {
	form := url.Values{}
	form.Set("client_id", s.cfg.ClientID)
	if secret := s.clientSecret(); secret != "" {
		form.Set("client_secret", secret)
	}
	return form
}

func (s *Source) clientSecret() string {
	if s.cfg.ClientSecret != "" {
		return s.cfg.ClientSecret
	}
	if s.cfg.ClientSecretEnv != "" {
		return s.getenv(s.cfg.ClientSecretEnv)
	}
	return ""
}

func (s *Source) loadStoredToken() (token, error) {
	tok, err := readTokenFile(s.tokenPath())
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return token{}, fmt.Errorf("auth: no stored token for %s; run auth login", s.name)
		}
		return token{}, err
	}
	return tok, nil
}

func (s *Source) tokenPath() string {
	return TokenPath(s.cfg, s.configDir, s.name)
}

func (s *Source) valid(tok token) bool {
	if tok.AccessToken == "" {
		return false
	}
	if tok.Expiry.IsZero() {
		return true
	}
	return s.now().Add(defaultExpirySkew).Before(tok.Expiry)
}

func (s *Source) validForConfig(tok token) bool {
	if normalizeType(s.cfg.Type) == TypeCodexOAuth {
		return s.validWithSkew(tok, defaultCodexExpirySkew)
	}
	return s.valid(tok)
}

func (s *Source) validWithSkew(tok token, skew time.Duration) bool {
	if tok.AccessToken == "" {
		return false
	}
	if tok.Expiry.IsZero() {
		return true
	}
	return s.now().Add(skew).Before(tok.Expiry)
}

func (s *Source) codexHeaders(tok token) (map[string]string, error) {
	if tok.AccountID == "" && tok.IDToken != "" {
		claims, err := parseCodexIDToken(tok.IDToken)
		if err != nil {
			return nil, err
		}
		tok.AccountID = claims.AccountID
		tok.FedRAMP = claims.FedRAMP
	}
	if strings.TrimSpace(tok.AccountID) == "" {
		return nil, errors.New("auth: OpenAI Codex token missing ChatGPT account id; run auth login")
	}
	headers := map[string]string{
		"Authorization":      "Bearer " + tok.AccessToken,
		"ChatGPT-Account-ID": tok.AccountID,
	}
	if tok.FedRAMP {
		headers["X-OpenAI-Fedramp"] = "true"
	}
	return headers, nil
}

func (s *Source) headerName() string {
	if strings.TrimSpace(s.cfg.Header) != "" {
		return strings.TrimSpace(s.cfg.Header)
	}
	return "Authorization"
}

func (s *Source) headerValue(accessToken string) string {
	scheme := strings.TrimSpace(s.cfg.Scheme)
	if scheme == "" {
		scheme = "Bearer"
	}
	if strings.EqualFold(scheme, "none") {
		return accessToken
	}
	return scheme + " " + accessToken
}

func normalizeType(t string) string {
	switch strings.ToLower(strings.TrimSpace(t)) {
	case "oauth":
		return TypeOAuth2
	default:
		return strings.ToLower(strings.TrimSpace(t))
	}
}

// Expand returns a deep copy of cfg after expanding string fields through fn.
func Expand(cfg *Config, fn func(string) string) *Config {
	if cfg == nil {
		return nil
	}
	if fn == nil {
		fn = func(s string) string { return s }
	}
	out := *cfg
	out.Type = fn(out.Type)
	out.Header = fn(out.Header)
	out.Scheme = fn(out.Scheme)
	out.Command = fn(out.Command)
	out.Args = append([]string(nil), out.Args...)
	for i := range out.Args {
		out.Args[i] = fn(out.Args[i])
	}
	out.Flow = fn(out.Flow)
	out.ClientID = fn(out.ClientID)
	out.ClientSecret = fn(out.ClientSecret)
	out.ClientSecretEnv = fn(out.ClientSecretEnv)
	out.AuthURL = fn(out.AuthURL)
	out.TokenURL = fn(out.TokenURL)
	out.DeviceURL = fn(out.DeviceURL)
	out.Issuer = fn(out.Issuer)
	out.Scopes = append([]string(nil), out.Scopes...)
	for i := range out.Scopes {
		out.Scopes[i] = fn(out.Scopes[i])
	}
	out.RedirectURI = fn(out.RedirectURI)
	out.TokenFile = fn(out.TokenFile)
	return &out
}

func TokenPath(cfg Config, configDir, name string) string {
	if cfg.TokenFile != "" {
		if filepath.IsAbs(cfg.TokenFile) {
			return cfg.TokenFile
		}
		return filepath.Join(configDir, cfg.TokenFile)
	}
	return filepath.Join(configDir, "tokens", safeName(name)+".json")
}

func safeName(name string) string {
	if name == "" {
		return "default"
	}
	var b strings.Builder
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '_', r == '-', r == '.':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	return b.String()
}

func readTokenFile(path string) (token, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return token{}, err
	}
	var tok token
	if err := json.Unmarshal(data, &tok); err != nil {
		return token{}, fmt.Errorf("auth: read token file %s: %w", path, err)
	}
	return tok, nil
}

func writeTokenFile(path string, tok token) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(tok, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }()
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpName, path); err != nil {
		return err
	}
	return nil
}

type LoginOptions struct {
	Name      string
	ConfigDir string
	Getenv    func(string) string
	Client    *http.Client
	Stdout    io.Writer
	Stderr    io.Writer
	Now       func() time.Time
}

func Login(ctx context.Context, cfg Config, opts LoginOptions) error {
	if err := Validate(cfg); err != nil {
		return err
	}
	switch normalizeType(cfg.Type) {
	case TypeOAuth2, TypeCodexOAuth:
	default:
		return fmt.Errorf("auth: login requires oauth2 or codex_oauth auth, got %q", cfg.Type)
	}
	rt := runtime{
		cfg:       cfg,
		name:      opts.Name,
		configDir: opts.ConfigDir,
		getenv:    opts.Getenv,
		client:    opts.Client,
		stdout:    opts.Stdout,
		now:       opts.Now,
	}
	rt.defaults()
	var tok token
	var err error
	switch normalizeType(cfg.Type) {
	case TypeOAuth2:
		switch cfg.Flow {
		case FlowAuthCodePKCE:
			tok, err = rt.loginAuthCodePKCE(ctx)
		case FlowDeviceCode:
			tok, err = rt.loginDeviceCode(ctx)
		default:
			err = fmt.Errorf("auth: unsupported oauth2 flow %q", cfg.Flow)
		}
	case TypeCodexOAuth:
		tok, err = rt.loginCodexDevice(ctx)
	default:
		err = fmt.Errorf("auth: unsupported type %q", cfg.Type)
	}
	if err != nil {
		return err
	}
	if err := writeTokenFile(TokenPath(cfg, opts.ConfigDir, opts.Name), tok); err != nil {
		return err
	}
	fmt.Fprintf(rt.stdout, "Stored auth token for %s\n", opts.Name)
	return nil
}

func Logout(cfg Config, configDir, name string) error {
	path := TokenPath(cfg, configDir, name)
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

func Status(cfg Config, configDir, name string, w io.Writer, now time.Time) error {
	if w == nil {
		w = io.Discard
	}
	if now.IsZero() {
		now = time.Now()
	}
	path := TokenPath(cfg, configDir, name)
	tok, err := readTokenFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			fmt.Fprintf(w, "%s: not logged in\n", name)
			return nil
		}
		return err
	}
	if tok.AccessToken == "" {
		fmt.Fprintf(w, "%s: token file has no access token\n", name)
		return nil
	}
	if normalizeType(cfg.Type) == TypeCodexOAuth && tok.RefreshFailed {
		if tok.RefreshFailure != "" {
			fmt.Fprintf(w, "%s: refresh failed (%s); run auth login %s\n", name, tok.RefreshFailure, name)
		} else {
			fmt.Fprintf(w, "%s: refresh failed; run auth login %s\n", name, name)
		}
		return nil
	}
	if tok.Expiry.IsZero() {
		fmt.Fprintf(w, "%s: logged in (no expiry)\n", name)
		return nil
	}
	if now.Add(defaultExpirySkew).Before(tok.Expiry) {
		fmt.Fprintf(w, "%s: logged in until %s\n", name, tok.Expiry.Format(time.RFC3339))
		return nil
	}
	fmt.Fprintf(w, "%s: token expired at %s\n", name, tok.Expiry.Format(time.RFC3339))
	return nil
}

type runtime struct {
	cfg       Config
	name      string
	configDir string
	getenv    func(string) string
	client    *http.Client
	stdout    io.Writer
	now       func() time.Time
}

func (r *runtime) defaults() {
	if r.getenv == nil {
		r.getenv = os.Getenv
	}
	if r.client == nil {
		r.client = http.DefaultClient
	}
	if r.stdout == nil {
		r.stdout = io.Discard
	}
	if r.now == nil {
		r.now = time.Now
	}
}

func (r *runtime) clientSecret() string {
	if r.cfg.ClientSecret != "" {
		return r.cfg.ClientSecret
	}
	if r.cfg.ClientSecretEnv != "" {
		return r.getenv(r.cfg.ClientSecretEnv)
	}
	return ""
}

func (r *runtime) clientForm() url.Values {
	form := url.Values{}
	form.Set("client_id", r.cfg.ClientID)
	if secret := r.clientSecret(); secret != "" {
		form.Set("client_secret", secret)
	}
	return form
}

func (r *runtime) loginAuthCodePKCE(ctx context.Context) (token, error) {
	verifier, err := randomURLString(32)
	if err != nil {
		return token{}, err
	}
	state, err := randomURLString(24)
	if err != nil {
		return token{}, err
	}
	sum := sha256.Sum256([]byte(verifier))
	challenge := base64.RawURLEncoding.EncodeToString(sum[:])

	redirectURI := r.cfg.RedirectURI
	var ln net.Listener
	if redirectURI == "" {
		ln, err = net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			return token{}, err
		}
		defer ln.Close()
		redirectURI = "http://" + ln.Addr().String() + "/callback"
	} else {
		u, err := url.Parse(redirectURI)
		if err != nil {
			return token{}, fmt.Errorf("auth: parse redirect_uri: %w", err)
		}
		if u.Scheme != "http" || u.Hostname() != "127.0.0.1" && u.Hostname() != "localhost" {
			return token{}, errors.New("auth: redirect_uri must be empty or loopback http")
		}
		ln, err = net.Listen("tcp", u.Host)
		if err != nil {
			return token{}, err
		}
		defer ln.Close()
	}

	authURL, err := url.Parse(r.cfg.AuthURL)
	if err != nil {
		return token{}, err
	}
	q := authURL.Query()
	q.Set("response_type", "code")
	q.Set("client_id", r.cfg.ClientID)
	q.Set("redirect_uri", redirectURI)
	q.Set("state", state)
	q.Set("code_challenge", challenge)
	q.Set("code_challenge_method", "S256")
	if len(r.cfg.Scopes) > 0 {
		q.Set("scope", strings.Join(r.cfg.Scopes, " "))
	}
	authURL.RawQuery = q.Encode()

	codeCh := make(chan string, 1)
	errCh := make(chan error, 1)
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, req *http.Request) {
		if req.URL.Query().Get("state") != state {
			http.Error(w, "invalid state", http.StatusBadRequest)
			errCh <- errors.New("auth: callback state did not match")
			return
		}
		if msg := req.URL.Query().Get("error"); msg != "" {
			http.Error(w, msg, http.StatusBadRequest)
			errCh <- fmt.Errorf("auth: authorization failed: %s", msg)
			return
		}
		code := req.URL.Query().Get("code")
		if code == "" {
			http.Error(w, "missing code", http.StatusBadRequest)
			errCh <- errors.New("auth: callback missing code")
			return
		}
		_, _ = io.WriteString(w, "Authentication complete. You can close this tab.\n")
		codeCh <- code
	})
	srv := &http.Server{Handler: mux}
	go func() {
		if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
	}()
	defer srv.Close()

	fmt.Fprintf(r.stdout, "Open this URL to authorize %s:\n%s\n", r.name, authURL.String())
	var code string
	select {
	case <-ctx.Done():
		return token{}, ctx.Err()
	case err := <-errCh:
		return token{}, err
	case code = <-codeCh:
	}

	form := r.clientForm()
	form.Set("grant_type", "authorization_code")
	form.Set("code", code)
	form.Set("redirect_uri", redirectURI)
	form.Set("code_verifier", verifier)
	return postToken(ctx, r.client, r.cfg.TokenURL, form, r.now())
}

func (r *runtime) loginDeviceCode(ctx context.Context) (token, error) {
	form := r.clientForm()
	if len(r.cfg.Scopes) > 0 {
		form.Set("scope", strings.Join(r.cfg.Scopes, " "))
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, r.cfg.DeviceURL, strings.NewReader(form.Encode()))
	if err != nil {
		return token{}, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := r.client.Do(req)
	if err != nil {
		return token{}, err
	}
	defer resp.Body.Close()
	var device struct {
		DeviceCode              string `json:"device_code"`
		UserCode                string `json:"user_code"`
		VerificationURI         string `json:"verification_uri"`
		VerificationURIComplete string `json:"verification_uri_complete"`
		ExpiresIn               int    `json:"expires_in"`
		Interval                int    `json:"interval"`
		Error                   string `json:"error"`
		ErrorDescription        string `json:"error_description"`
	}
	if err := decodeOAuthResponse(resp, &device); err != nil {
		return token{}, err
	}
	if device.Error != "" {
		return token{}, oauthError(device.Error, device.ErrorDescription)
	}
	if device.DeviceCode == "" {
		return token{}, errors.New("auth: device response missing device_code")
	}
	if device.VerificationURIComplete != "" {
		fmt.Fprintf(r.stdout, "Open this URL to authorize %s:\n%s\n", r.name, device.VerificationURIComplete)
	} else {
		fmt.Fprintf(r.stdout, "Open this URL to authorize %s:\n%s\nCode: %s\n", r.name, device.VerificationURI, device.UserCode)
	}
	interval := time.Duration(device.Interval) * time.Second
	if interval <= 0 {
		interval = 5 * time.Second
	}
	deadline := time.Time{}
	if device.ExpiresIn > 0 {
		deadline = r.now().Add(time.Duration(device.ExpiresIn) * time.Second)
	}
	for {
		if !deadline.IsZero() && !r.now().Before(deadline) {
			return token{}, errors.New("auth: device code expired")
		}
		timer := time.NewTimer(interval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return token{}, ctx.Err()
		case <-timer.C:
		}
		form := r.clientForm()
		form.Set("grant_type", "urn:ietf:params:oauth:grant-type:device_code")
		form.Set("device_code", device.DeviceCode)
		tok, err := postToken(ctx, r.client, r.cfg.TokenURL, form, r.now())
		if err == nil {
			return tok, nil
		}
		var oe *oauthStatusError
		if !errors.As(err, &oe) {
			return token{}, err
		}
		switch oe.Code {
		case "authorization_pending":
			continue
		case "slow_down":
			interval += 5 * time.Second
			continue
		default:
			return token{}, err
		}
	}
}

type codexDeviceCode struct {
	VerificationURL string
	UserCode        string
	DeviceAuthID    string
	Interval        time.Duration
}

func (r *runtime) loginCodexDevice(ctx context.Context) (token, error) {
	issuer := codexIssuer(r.cfg)
	clientID := codexClientID(r.cfg)

	device, err := r.requestCodexDeviceCode(ctx, issuer, clientID)
	if err != nil {
		return token{}, err
	}
	fmt.Fprintf(r.stdout, "Open this URL to authorize %s with ChatGPT:\n%s\nCode: %s\n", r.name, device.VerificationURL, device.UserCode)

	code, err := r.pollCodexDeviceCode(ctx, issuer, device)
	if err != nil {
		return token{}, err
	}

	redirectURI := issuer + "/deviceauth/callback"
	form := url.Values{}
	form.Set("grant_type", "authorization_code")
	form.Set("code", code.AuthorizationCode)
	form.Set("redirect_uri", redirectURI)
	form.Set("client_id", clientID)
	form.Set("code_verifier", code.CodeVerifier)
	tok, err := postToken(ctx, r.client, issuer+"/oauth/token", form, r.now())
	if err != nil {
		return token{}, err
	}
	return enrichCodexToken(tok, r.now())
}

func (r *runtime) requestCodexDeviceCode(ctx context.Context, issuer, clientID string) (codexDeviceCode, error) {
	endpoint := issuer + "/api/accounts/deviceauth/usercode"
	status, data, err := postJSON(ctx, r.client, endpoint, map[string]string{"client_id": clientID})
	if err != nil {
		return codexDeviceCode{}, err
	}
	if status < 200 || status >= 300 {
		return codexDeviceCode{}, fmt.Errorf("auth: Codex device code endpoint returned %d", status)
	}
	var wire struct {
		DeviceAuthID string          `json:"device_auth_id"`
		UserCode     string          `json:"user_code"`
		UserCodeAlt  string          `json:"usercode"`
		Interval     json.RawMessage `json:"interval"`
	}
	if err := json.Unmarshal(data, &wire); err != nil {
		return codexDeviceCode{}, fmt.Errorf("auth: decode Codex device code response: %w", err)
	}
	if wire.UserCode == "" {
		wire.UserCode = wire.UserCodeAlt
	}
	if wire.DeviceAuthID == "" {
		return codexDeviceCode{}, errors.New("auth: Codex device code response missing device_auth_id")
	}
	if wire.UserCode == "" {
		return codexDeviceCode{}, errors.New("auth: Codex device code response missing user_code")
	}
	interval, err := parseFlexibleSeconds(wire.Interval)
	if err != nil {
		return codexDeviceCode{}, err
	}
	if interval <= 0 {
		interval = 5 * time.Second
	}
	return codexDeviceCode{
		VerificationURL: issuer + "/codex/device",
		UserCode:        wire.UserCode,
		DeviceAuthID:    wire.DeviceAuthID,
		Interval:        interval,
	}, nil
}

type codexAuthorizationCode struct {
	AuthorizationCode string `json:"authorization_code"`
	CodeChallenge     string `json:"code_challenge"`
	CodeVerifier      string `json:"code_verifier"`
}

func (r *runtime) pollCodexDeviceCode(ctx context.Context, issuer string, device codexDeviceCode) (codexAuthorizationCode, error) {
	endpoint := issuer + "/api/accounts/deviceauth/token"
	deadline := r.now().Add(defaultCodexDevicePollWait)
	for {
		status, data, err := postJSON(ctx, r.client, endpoint, map[string]string{
			"device_auth_id": device.DeviceAuthID,
			"user_code":      device.UserCode,
		})
		if err != nil {
			return codexAuthorizationCode{}, err
		}
		if status >= 200 && status < 300 {
			var code codexAuthorizationCode
			if err := json.Unmarshal(data, &code); err != nil {
				return codexAuthorizationCode{}, fmt.Errorf("auth: decode Codex device auth response: %w", err)
			}
			if code.AuthorizationCode == "" || code.CodeVerifier == "" {
				return codexAuthorizationCode{}, errors.New("auth: Codex device auth response missing authorization code or verifier")
			}
			return code, nil
		}
		if status != http.StatusForbidden && status != http.StatusNotFound {
			return codexAuthorizationCode{}, fmt.Errorf("auth: Codex device auth endpoint returned %d", status)
		}
		if !r.now().Before(deadline) {
			return codexAuthorizationCode{}, errors.New("auth: Codex device auth timed out after 15 minutes")
		}
		wait := device.Interval
		if remaining := deadline.Sub(r.now()); remaining > 0 && wait > remaining {
			wait = remaining
		}
		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			timer.Stop()
			return codexAuthorizationCode{}, ctx.Err()
		case <-timer.C:
		}
	}
}

func (s *Source) refreshCodex(ctx context.Context, old token) (token, error) {
	endpoint := codexIssuer(s.cfg) + "/oauth/token"
	status, data, err := postJSON(ctx, s.client, endpoint, map[string]string{
		"client_id":     codexClientID(s.cfg),
		"grant_type":    "refresh_token",
		"refresh_token": old.RefreshToken,
	})
	if err != nil {
		return token{}, err
	}
	if status < 200 || status >= 300 {
		code, desc := parseOAuthErrorBody(data)
		message := codexRefreshFailureMessage(status, code, desc)
		if codexTerminalRefreshFailure(status, code) {
			old.RefreshFailed = true
			old.RefreshFailure = message
			if err := writeTokenFile(s.tokenPath(), old); err != nil {
				return token{}, err
			}
			return token{}, codexReauthError(s.name, message)
		}
		return token{}, fmt.Errorf("auth: OpenAI Codex token refresh failed: %s", message)
	}
	var wire struct {
		IDToken      string `json:"id_token"`
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		TokenType    string `json:"token_type"`
		ExpiresIn    int    `json:"expires_in"`
		Scope        string `json:"scope"`
	}
	if err := json.Unmarshal(data, &wire); err != nil {
		return token{}, fmt.Errorf("auth: decode OpenAI Codex refresh response: %w", err)
	}
	if wire.AccessToken == "" {
		return token{}, errors.New("auth: OpenAI Codex refresh response missing access_token")
	}
	next := old
	next.AccessToken = wire.AccessToken
	next.RefreshFailed = false
	next.RefreshFailure = ""
	if wire.IDToken != "" {
		next.IDToken = wire.IDToken
	}
	if wire.RefreshToken != "" {
		next.RefreshToken = wire.RefreshToken
	}
	if wire.TokenType != "" {
		next.TokenType = wire.TokenType
	}
	if wire.Scope != "" {
		next.Scope = wire.Scope
	}
	next.Expiry = time.Time{}
	if wire.ExpiresIn > 0 {
		next.Expiry = s.now().Add(time.Duration(wire.ExpiresIn) * time.Second)
	}
	return enrichCodexToken(next, s.now())
}

func postJSON(ctx context.Context, client *http.Client, endpoint string, body any) (int, []byte, error) {
	data, err := json.Marshal(body)
	if err != nil {
		return 0, nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(data))
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	respBody, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return 0, nil, err
	}
	return resp.StatusCode, respBody, nil
}

func parseFlexibleSeconds(raw json.RawMessage) (time.Duration, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return 0, nil
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		if strings.TrimSpace(s) == "" {
			return 0, nil
		}
		n, err := strconv.ParseInt(strings.TrimSpace(s), 10, 64)
		if err != nil {
			return 0, fmt.Errorf("auth: parse interval: %w", err)
		}
		return time.Duration(n) * time.Second, nil
	}
	var n int64
	if json.Unmarshal(raw, &n) == nil {
		return time.Duration(n) * time.Second, nil
	}
	return 0, errors.New("auth: interval must be seconds as string or number")
}

func codexIssuer(cfg Config) string {
	issuer := strings.TrimRight(strings.TrimSpace(cfg.Issuer), "/")
	if issuer == "" {
		issuer = defaultCodexIssuer
	}
	return issuer
}

func codexClientID(cfg Config) string {
	clientID := strings.TrimSpace(cfg.ClientID)
	if clientID == "" {
		clientID = defaultCodexClientID
	}
	return clientID
}

func enrichCodexToken(tok token, now time.Time) (token, error) {
	if tok.TokenType == "" {
		tok.TokenType = "Bearer"
	}
	if tok.IDToken != "" {
		claims, err := parseCodexIDToken(tok.IDToken)
		if err != nil {
			return token{}, err
		}
		if claims.AccountID != "" {
			tok.AccountID = claims.AccountID
		}
		tok.FedRAMP = claims.FedRAMP
	}
	if tok.Expiry.IsZero() {
		if expiry, err := parseJWTExpiration(tok.AccessToken); err == nil && !expiry.IsZero() {
			tok.Expiry = expiry
		}
	}
	if strings.TrimSpace(tok.AccountID) == "" {
		return token{}, errors.New("auth: OpenAI Codex token missing chatgpt_account_id claim")
	}
	return tok, nil
}

type codexIDTokenClaims struct {
	AccountID string
	FedRAMP   bool
}

func parseCodexIDToken(jwt string) (codexIDTokenClaims, error) {
	var wire struct {
		Auth struct {
			AccountID string `json:"chatgpt_account_id"`
			FedRAMP   bool   `json:"chatgpt_account_is_fedramp"`
		} `json:"https://api.openai.com/auth"`
	}
	if err := decodeJWTPayload(jwt, &wire); err != nil {
		return codexIDTokenClaims{}, fmt.Errorf("auth: decode OpenAI Codex id_token: %w", err)
	}
	return codexIDTokenClaims{
		AccountID: wire.Auth.AccountID,
		FedRAMP:   wire.Auth.FedRAMP,
	}, nil
}

func parseJWTExpiration(jwt string) (time.Time, error) {
	var wire struct {
		Exp int64 `json:"exp"`
	}
	if err := decodeJWTPayload(jwt, &wire); err != nil {
		return time.Time{}, err
	}
	if wire.Exp <= 0 {
		return time.Time{}, nil
	}
	return time.Unix(wire.Exp, 0), nil
}

func decodeJWTPayload(jwt string, v any) error {
	parts := strings.Split(jwt, ".")
	if len(parts) < 3 || parts[1] == "" {
		return errors.New("invalid JWT format")
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		payload, err = base64.URLEncoding.DecodeString(parts[1])
	}
	if err != nil {
		return err
	}
	if err := json.Unmarshal(payload, v); err != nil {
		return err
	}
	return nil
}

func parseOAuthErrorBody(data []byte) (code, desc string) {
	var root map[string]json.RawMessage
	if err := json.Unmarshal(data, &root); err != nil {
		return "", strings.TrimSpace(string(data))
	}
	if raw, ok := root["error"]; ok {
		if json.Unmarshal(raw, &code) == nil && code != "" {
			if rawDesc, ok := root["error_description"]; ok {
				_ = json.Unmarshal(rawDesc, &desc)
			}
			return code, desc
		}
		var nested map[string]json.RawMessage
		if json.Unmarshal(raw, &nested) == nil {
			if rawCode, ok := nested["code"]; ok {
				_ = json.Unmarshal(rawCode, &code)
			}
			if rawDesc, ok := nested["message"]; ok {
				_ = json.Unmarshal(rawDesc, &desc)
			}
			if desc == "" {
				if rawDesc, ok := nested["error_description"]; ok {
					_ = json.Unmarshal(rawDesc, &desc)
				}
			}
			return code, desc
		}
	}
	if raw, ok := root["code"]; ok {
		_ = json.Unmarshal(raw, &code)
	}
	if raw, ok := root["message"]; ok {
		_ = json.Unmarshal(raw, &desc)
	}
	return code, desc
}

func codexRefreshFailureMessage(status int, code, desc string) string {
	parts := []string{fmt.Sprintf("HTTP %d", status)}
	if code != "" {
		parts = append(parts, code)
	}
	if desc != "" {
		parts = append(parts, desc)
	}
	return strings.Join(parts, ": ")
}

func codexTerminalRefreshFailure(status int, code string) bool {
	if status >= 400 && status < 500 {
		return true
	}
	switch strings.ToLower(strings.TrimSpace(code)) {
	case "invalid_grant", "revoked", "refresh_token_expired", "refresh_token_reused", "refresh_token_invalidated", "refresh_token_invalid", "invalid_refresh_token":
		return true
	default:
		return false
	}
}

func codexReauthError(name, detail string) error {
	if detail != "" {
		return fmt.Errorf("auth: OpenAI Codex token for %s cannot be refreshed (%s); run auth login %s", name, detail, name)
	}
	return fmt.Errorf("auth: OpenAI Codex token for %s cannot be refreshed; run auth login %s", name, name)
}

func postToken(ctx context.Context, client *http.Client, endpoint string, form url.Values, now time.Time) (token, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return token{}, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := client.Do(req)
	if err != nil {
		return token{}, err
	}
	defer resp.Body.Close()
	var wire struct {
		AccessToken      string `json:"access_token"`
		RefreshToken     string `json:"refresh_token"`
		IDToken          string `json:"id_token"`
		TokenType        string `json:"token_type"`
		ExpiresIn        int    `json:"expires_in"`
		Scope            string `json:"scope"`
		Error            string `json:"error"`
		ErrorDescription string `json:"error_description"`
	}
	if err := decodeOAuthResponse(resp, &wire); err != nil {
		return token{}, err
	}
	if wire.Error != "" {
		return token{}, oauthError(wire.Error, wire.ErrorDescription)
	}
	if wire.AccessToken == "" {
		return token{}, errors.New("auth: token response missing access_token")
	}
	tok := token{
		AccessToken:  wire.AccessToken,
		RefreshToken: wire.RefreshToken,
		IDToken:      wire.IDToken,
		TokenType:    wire.TokenType,
		Scope:        wire.Scope,
	}
	if tok.TokenType == "" {
		tok.TokenType = "Bearer"
	}
	if wire.ExpiresIn > 0 {
		tok.Expiry = now.Add(time.Duration(wire.ExpiresIn) * time.Second)
	}
	return tok, nil
}

func decodeOAuthResponse(resp *http.Response, v any) error {
	data, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return err
	}
	if err := json.Unmarshal(data, v); err != nil {
		return fmt.Errorf("auth: decode OAuth response: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		var wire struct {
			Error            string `json:"error"`
			ErrorDescription string `json:"error_description"`
		}
		_ = json.Unmarshal(data, &wire)
		if wire.Error != "" {
			return oauthError(wire.Error, wire.ErrorDescription)
		}
		return fmt.Errorf("auth: OAuth endpoint returned %d", resp.StatusCode)
	}
	return nil
}

type oauthStatusError struct {
	Code        string
	Description string
}

func (e *oauthStatusError) Error() string {
	if e.Description != "" {
		return "auth: OAuth error " + e.Code + ": " + e.Description
	}
	return "auth: OAuth error " + e.Code
}

func oauthError(code, desc string) error {
	return &oauthStatusError{Code: code, Description: desc}
}

func randomURLString(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}
