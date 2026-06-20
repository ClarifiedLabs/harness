// Package mcpproxy implements the MCP proxy daemon: it loads a Claude
// Code-compatible config, supervises downstream MCP servers (stdio children and
// streamable-HTTP endpoints), aggregates their tools under a stable namespace,
// and serves the merged tool surface to harness over HTTP as a single MCP
// server. The binary CLI wrapper (cmd/harness-mcp-proxy) is a thin shell
// around Daemon.Run; all proxy logic lives here so it stays testable without
// a process.
package mcpproxy

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"

	"harness/internal/apikey"
	"harness/internal/auth"
)

// FileConfig is the on-disk config shape. It is Claude Code-compatible
// (camelCase, "mcpServers") with a proxy-level extension block. Decode is
// tolerant (plain json.Unmarshal, unknown keys ignored) per repo convention.
type FileConfig struct {
	MCPServers map[string]ServerConfig `json:"mcpServers"`
	Proxy      ProxySettings           `json:"proxy"`
}

// ServerConfig is one downstream server entry. Type "" or "stdio" selects a
// stdio child (Command/Args/Env); "http" selects a streamable-HTTP endpoint
// (URL/Headers). The two variants are mutually exclusive; validation enforces it.
type ServerConfig struct {
	Type    string            `json:"type"`
	Command string            `json:"command"`
	Args    []string          `json:"args"`
	Env     map[string]string `json:"env"`
	URL     string            `json:"url"`
	Headers map[string]string `json:"headers"`
	Auth    *auth.Config      `json:"auth,omitempty"`
}

// ProxySettings carries proxy-level overrides. Empty fields fall back to
// defaults (listen → DefaultListen, logLevel → info, logFormat → json).
type ProxySettings struct {
	Listen    string         `json:"listen"`
	LogFile   string         `json:"logFile"`
	LogLevel  string         `json:"logLevel"`
	LogFormat string         `json:"logFormat"`
	APIKeys   []apikey.Entry `json:"api_keys,omitempty"`
}

// APIKeyStore returns the proxy-level API-key store. Auth is required as soon as
// the first key is configured.
func (p ProxySettings) APIKeyStore() apikey.Store {
	return apikey.Store{Entries: append([]apikey.Entry(nil), p.APIKeys...)}
}

// Transport selects a resolved server's downstream transport.
type Transport int

const (
	// TransportStdio drives a child process over its stdin/stdout pipes.
	TransportStdio Transport = iota
	// TransportHTTP drives a streamable-HTTP endpoint.
	TransportHTTP
)

// ResolvedServer is a validated, ${VAR}-expanded server ready to supervise.
type ResolvedServer struct {
	Name      string
	Transport Transport
	// stdio
	Command string
	Args    []string
	Env     map[string]string
	// http
	URL     string
	Headers map[string]string
	Auth    *auth.Config

	ConfigDir string
}

// Config is the resolved, validated proxy configuration. Servers is sorted by
// name for stable ordering. Warnings collects non-fatal load problems (unset
// expansion vars, skipped invalid servers); the caller logs them — library code
// never prints.
type Config struct {
	Servers   []ResolvedServer
	APIKeys   []apikey.Entry
	Listen    string
	LogFile   string
	LogLevel  string
	LogFormat string
	Warnings  []string
}

// APIKeyStore returns the proxy-level API-key store. Auth is required as soon as
// the first key is configured.
func (c Config) APIKeyStore() apikey.Store {
	return apikey.Store{Entries: append([]apikey.Entry(nil), c.APIKeys...)}
}

const (
	// DefaultListen is the local HTTP address used when the proxy config and
	// serve flags do not specify one. It intentionally sits next to the model
	// proxy default (127.0.0.1:8765) without sharing the same port.
	DefaultListen = "127.0.0.1:8766"
)

// serverNameRE constrains a server name. It is the qualified-name charset, kept
// tight because the name becomes the middle segment of mcp__<server>__<tool>.
var serverNameRE = regexp.MustCompile(`^[a-zA-Z0-9_-]{1,64}$`)

// LoadConfig reads, expands, and validates the config at path. An empty path or
// a missing file at the DEFAULT location resolves to a valid empty config (zero
// servers). An explicitly-given path that is missing, or any present-but-
// malformed file, is an error. Invalid individual servers are skipped with a
// Warning, never fatal.
func LoadConfig(path string) (Config, error) {
	if path == "" {
		// No config requested: valid empty config with default HTTP listener.
		return resolve(FileConfig{}, ""), nil
	}

	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			// An explicit path that does not exist is a hard error: a typo must not
			// silently degrade to "no servers". (A missing DEFAULT path is handled by
			// the caller passing "" instead.)
			return Config{}, fmt.Errorf("mcpproxy: config %s not found: %w", path, err)
		}
		return Config{}, fmt.Errorf("mcpproxy: read config %s: %w", path, err)
	}

	var fc FileConfig
	if err := json.Unmarshal(data, &fc); err != nil {
		return Config{}, fmt.Errorf("mcpproxy: parse config %s: %w", path, err)
	}
	return resolve(fc, filepath.Dir(path)), nil
}

// resolve expands ${VAR} references, validates each server, and produces a
// sorted Config. Expansion happens before validation so a validated field is
// always the post-expansion value.
func resolve(fc FileConfig, configDir string) Config {
	cfg := Config{
		APIKeys:   append([]apikey.Entry(nil), fc.Proxy.APIKeys...),
		LogFile:   fc.Proxy.LogFile,
		LogLevel:  fc.Proxy.LogLevel,
		LogFormat: fc.Proxy.LogFormat,
	}

	// Expand variables across the whole config first, accumulating one warning
	// per distinct unset var.
	var exp expander
	expand := exp.expand

	// Sort names so validation/warning order is deterministic.
	names := make([]string, 0, len(fc.MCPServers))
	for name := range fc.MCPServers {
		names = append(names, name)
	}
	sort.Strings(names)

	for _, name := range names {
		raw := fc.MCPServers[name]
		rs, warn := resolveServer(name, raw, expand, configDir)
		if warn != "" {
			cfg.Warnings = append(cfg.Warnings, warn)
			continue
		}
		cfg.Servers = append(cfg.Servers, rs)
	}

	// Emit one warning per distinct unset var, in sorted order for determinism.
	unsetNames := exp.unsetNames()
	for _, v := range unsetNames {
		cfg.Warnings = append(cfg.Warnings, fmt.Sprintf("config references unset variable ${%s}; expanded to empty string", v))
	}

	cfg.Listen = fc.Proxy.Listen
	if cfg.Listen == "" {
		cfg.Listen = DefaultListen
	}
	return cfg
}

// DefaultURL is the harness-side URL for the default proxy listener.
func DefaultURL() string {
	return URLForListen(DefaultListen)
}

// URLForListen turns a listen address into the HTTP URL local clients should
// dial. Wildcard listeners are mapped to loopback for client-side convenience.
func URLForListen(addr string) string {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return "http://" + addr
	}
	switch host {
	case "", "0.0.0.0", "::":
		host = "127.0.0.1"
	}
	return "http://" + net.JoinHostPort(host, port)
}

// resolveServer expands and validates one entry. It returns either a resolved
// server (warn == "") or a non-empty warning describing why it was skipped.
func resolveServer(name string, sc ServerConfig, expand func(string) string, configDir string) (ResolvedServer, string) {
	if !serverNameRE.MatchString(name) {
		return ResolvedServer{}, fmt.Sprintf("server %q skipped: name must match [a-zA-Z0-9_-]{1,64}", name)
	}

	command := expand(sc.Command)
	rawURL := expand(sc.URL)
	args := make([]string, len(sc.Args))
	for i, a := range sc.Args {
		args[i] = expand(a)
	}
	env := expandMap(sc.Env, expand)
	headers := expandMap(sc.Headers, expand)
	authCfg := auth.Expand(sc.Auth, expand)
	if authCfg != nil {
		if err := auth.Validate(*authCfg); err != nil {
			return ResolvedServer{}, fmt.Sprintf("server %q skipped: %v", name, err)
		}
	}

	switch sc.Type {
	case "", "stdio":
		if authCfg != nil {
			return ResolvedServer{}, fmt.Sprintf("server %q skipped: stdio server must not set auth", name)
		}
		if command == "" {
			return ResolvedServer{}, fmt.Sprintf("server %q skipped: stdio server requires a command", name)
		}
		if rawURL != "" {
			return ResolvedServer{}, fmt.Sprintf("server %q skipped: stdio server must not set url", name)
		}
		return ResolvedServer{
			Name:      name,
			Transport: TransportStdio,
			Command:   command,
			Args:      args,
			Env:       env,
			ConfigDir: configDir,
		}, ""
	case "http", "streamable-http":
		if rawURL == "" {
			return ResolvedServer{}, fmt.Sprintf("server %q skipped: http server requires a url", name)
		}
		if command != "" {
			return ResolvedServer{}, fmt.Sprintf("server %q skipped: http server must not set command", name)
		}
		u, err := url.Parse(rawURL)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") {
			return ResolvedServer{}, fmt.Sprintf("server %q skipped: url must use http or https scheme", name)
		}
		return ResolvedServer{
			Name:      name,
			Transport: TransportHTTP,
			URL:       rawURL,
			Headers:   headers,
			Auth:      authCfg,
			ConfigDir: configDir,
		}, ""
	default:
		return ResolvedServer{}, fmt.Sprintf("server %q skipped: unknown type %q (want \"\", \"stdio\", \"http\", or \"streamable-http\")", name, sc.Type)
	}
}

// expandMap returns a copy of m with every value expanded; nil maps stay nil.
func expandMap(m map[string]string, expand func(string) string) map[string]string {
	if m == nil {
		return nil
	}
	out := make(map[string]string, len(m))
	for k, v := range m {
		out[k] = expand(v)
	}
	return out
}

// expander substitutes ${NAME} and ${NAME:-default} references in config
// strings, tracking which strict referenced vars were unset in the process
// environment so the loader can warn once per distinct name. An unset strict
// var expands to the empty string; an unset defaulted var expands to its
// provided default.
//
// Only NAME matching [A-Za-z_][A-Za-z0-9_]* is recognized. Every other '$' is
// passed through literally: a bare "$5", "price$5", "$$", or an unterminated
// "${" are all left exactly as written. This avoids os.Expand's behavior of
// eating "$$" and "$<digit>" tokens, which would corrupt secrets that contain
// '$'.
type expander struct {
	seen  map[string]bool
	names []string
}

func (e *expander) expand(s string) string {
	if !strings.ContainsRune(s, '$') {
		return s
	}
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); {
		c := s[i]
		if c != '$' {
			b.WriteByte(c)
			i++
			continue
		}
		// Try to parse a ${NAME} reference starting at i. Anything that is not a
		// well-formed reference is emitted as a literal '$'.
		ref, ok := parseVarRef(s, i)
		if !ok {
			b.WriteByte('$')
			i++
			continue
		}
		b.WriteString(e.lookup(ref))
		i = ref.end
	}
	return b.String()
}

type varRef struct {
	name       string
	def        string
	hasDefault bool
	end        int
}

// parseVarRef parses a ${NAME} or ${NAME:-default} reference at s[i] (s[i] is
// '$'). A malformed reference (no brace, empty/invalid name, unterminated)
// returns ok=false so the caller emits a literal '$'.
func parseVarRef(s string, i int) (varRef, bool) {
	if i+1 >= len(s) || s[i+1] != '{' {
		return varRef{}, false
	}
	j := i + 2
	start := j
	for j < len(s) && s[j] != '}' {
		j++
	}
	if j >= len(s) {
		return varRef{}, false // unterminated "${..."
	}
	body := s[start:j]
	name, def, hasDefault := strings.Cut(body, ":-")
	if !isVarName(name) {
		return varRef{}, false
	}
	return varRef{name: name, def: def, hasDefault: hasDefault, end: j + 1}, true
}

// isVarName reports whether name matches [A-Za-z_][A-Za-z0-9_]*.
func isVarName(name string) bool {
	if name == "" {
		return false
	}
	for i := 0; i < len(name); i++ {
		c := name[i]
		switch {
		case c == '_':
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z':
		case c >= '0' && c <= '9':
			if i == 0 {
				return false
			}
		default:
			return false
		}
	}
	return true
}

// lookup resolves a variable, recording an unset strict reference once per
// distinct name.
func (e *expander) lookup(ref varRef) string {
	if val, ok := os.LookupEnv(ref.name); ok {
		return val
	}
	if ref.hasDefault {
		return ref.def
	}
	if e.seen == nil {
		e.seen = make(map[string]bool)
	}
	if !e.seen[ref.name] {
		e.seen[ref.name] = true
		e.names = append(e.names, ref.name)
	}
	return ""
}

// unsetNames returns the distinct unset variable names referenced during
// expansion, sorted for deterministic warning order.
func (e *expander) unsetNames() []string {
	out := slices.Clone(e.names)
	sort.Strings(out)
	return out
}

// ChildEnv builds the environment for a stdio child: the full parent
// environment with extra entries appended so they win on conflict (exec uses
// the last assignment of a key). A nil/empty extra map yields the parent
// environment unchanged.
func ChildEnv(extra map[string]string) []string {
	env := os.Environ()
	if len(extra) == 0 {
		return env
	}
	// Append in sorted key order for deterministic output (tests, logs).
	keys := make([]string, 0, len(extra))
	for k := range extra {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		env = append(env, k+"="+extra[k])
	}
	return env
}

// DefaultConfigPath resolves the default config file path:
// $XDG_CONFIG_HOME/harness-mcp-proxy/config.json, else
// ~/.config/harness-mcp-proxy/config.json. getenv injects the environment so
// the resolution is testable.
func DefaultConfigPath(getenv func(string) string) string {
	if xdg := getenv("XDG_CONFIG_HOME"); xdg != "" {
		return filepath.Join(xdg, "harness-mcp-proxy", "config.json")
	}
	home := getenv("HOME")
	return filepath.Join(home, ".config", "harness-mcp-proxy", "config.json")
}
