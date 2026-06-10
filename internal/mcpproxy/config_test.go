package mcpproxy

import (
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
)

func writeConfig(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	return path
}

func TestLoadConfigCamelCaseDecode(t *testing.T) {
	path := writeConfig(t, `{
		"mcpServers": {
			"alpha": {"command": "alpha-bin", "args": ["--flag", "x"], "env": {"K": "v"}},
			"beta": {"type": "http", "url": "https://example.com/mcp", "headers": {"Authorization": "Bearer t"}}
		},
		"proxy": {"listen": "127.0.0.1:8089", "socket": "/tmp/ignored.sock", "logFile": "/tmp/g.log", "logLevel": "debug", "logFormat": "text"}
	}`)

	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if cfg.Listen != "127.0.0.1:8089" || cfg.LogFile != "/tmp/g.log" || cfg.LogLevel != "debug" || cfg.LogFormat != "text" {
		t.Fatalf("proxy settings not decoded: %+v", cfg)
	}
	if len(cfg.Servers) != 2 {
		t.Fatalf("want 2 servers, got %d (%+v)", len(cfg.Servers), cfg.Servers)
	}
	// Sorted by name: alpha before beta.
	if cfg.Servers[0].Name != "alpha" || cfg.Servers[1].Name != "beta" {
		t.Fatalf("servers not sorted by name: %+v", cfg.Servers)
	}
	a := cfg.Servers[0]
	if a.Transport != TransportStdio || a.Command != "alpha-bin" || !reflect.DeepEqual(a.Args, []string{"--flag", "x"}) {
		t.Fatalf("alpha decoded wrong: %+v", a)
	}
	if a.Env["K"] != "v" {
		t.Fatalf("alpha env wrong: %+v", a.Env)
	}
	b := cfg.Servers[1]
	if b.Transport != TransportHTTP || b.URL != "https://example.com/mcp" || b.Headers["Authorization"] != "Bearer t" {
		t.Fatalf("beta decoded wrong: %+v", b)
	}
	if len(cfg.Warnings) != 0 {
		t.Fatalf("unexpected warnings: %v", cfg.Warnings)
	}
}

func TestLoadConfigHTTPAuthTokenCommand(t *testing.T) {
	path := writeConfig(t, `{
		"mcpServers": {
			"beta": {
				"type": "http",
				"url": "https://example.com/mcp",
				"headers": {"X-Static": "yes"},
				"auth": {
					"type": "token_command",
					"command": "${TOKEN_CMD}",
					"args": ["print", "${TOKEN_ARG:-fallback}"],
					"cache_ttl_seconds": 60
				}
			}
		}
	}`)
	t.Setenv("TOKEN_CMD", "/bin/token")
	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if len(cfg.Servers) != 1 {
		t.Fatalf("servers = %+v warnings=%v", cfg.Servers, cfg.Warnings)
	}
	s := cfg.Servers[0]
	if s.Auth == nil || s.Auth.Command != "/bin/token" || !reflect.DeepEqual(s.Auth.Args, []string{"print", "fallback"}) {
		t.Fatalf("auth not decoded/expanded: %+v", s.Auth)
	}
	if s.ConfigDir != filepath.Dir(path) {
		t.Fatalf("ConfigDir = %q, want %q", s.ConfigDir, filepath.Dir(path))
	}
}

func TestLoadConfigRejectsInvalidAuth(t *testing.T) {
	path := writeConfig(t, `{
		"mcpServers": {
			"bad": {"type":"http","url":"https://example.com/mcp","auth":{"type":"token_command"}}
		}
	}`)
	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if len(cfg.Servers) != 0 || len(cfg.Warnings) == 0 {
		t.Fatalf("invalid auth should skip server with warning: servers=%+v warnings=%v", cfg.Servers, cfg.Warnings)
	}
}

func TestLoadConfigListenField(t *testing.T) {
	withListen := writeConfig(t, `{
		"proxy": {"listen": "127.0.0.1:8089"}
	}`)
	cfg, err := LoadConfig(withListen)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if cfg.Listen != "127.0.0.1:8089" {
		t.Fatalf("Listen = %q, want 127.0.0.1:8089", cfg.Listen)
	}

	// Absent listen defaults to the local HTTP listener.
	noListen := writeConfig(t, `{"proxy": {}}`)
	cfg, err = LoadConfig(noListen)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if cfg.Listen != DefaultListen {
		t.Fatalf("Listen = %q, want %q when unset", cfg.Listen, DefaultListen)
	}
}

func TestLoadConfigVarExpansionSet(t *testing.T) {
	t.Setenv("MY_TOKEN", "secret")
	t.Setenv("MY_DIR", "/srv")
	path := writeConfig(t, `{
		"mcpServers": {
			"s": {"command": "${MY_DIR}/bin", "args": ["--token=${MY_TOKEN}"], "env": {"T": "${MY_TOKEN}"}}
		}
	}`)
	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	s := cfg.Servers[0]
	if s.Command != "/srv/bin" {
		t.Fatalf("command expansion: %q", s.Command)
	}
	if s.Args[0] != "--token=secret" {
		t.Fatalf("args expansion: %q", s.Args[0])
	}
	if s.Env["T"] != "secret" {
		t.Fatalf("env expansion: %q", s.Env["T"])
	}
	if len(cfg.Warnings) != 0 {
		t.Fatalf("unexpected warnings: %v", cfg.Warnings)
	}
}

func TestLoadConfigVarExpansionUnset(t *testing.T) {
	path := writeConfig(t, `{
		"mcpServers": {
			"s": {"command": "/bin/x", "args": ["--a=${UNSET_ONE}", "--b=${UNSET_ONE}"], "env": {"E": "${UNSET_TWO}"}}
		}
	}`)
	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	s := cfg.Servers[0]
	if s.Args[0] != "--a=" || s.Args[1] != "--b=" {
		t.Fatalf("unset vars should expand to empty: %+v", s.Args)
	}
	if s.Env["E"] != "" {
		t.Fatalf("unset env should expand to empty: %q", s.Env["E"])
	}
	// One warning per DISTINCT var, regardless of how many references.
	gotVars := 0
	for _, w := range cfg.Warnings {
		if strings.Contains(w, "UNSET_ONE") {
			gotVars++
		}
	}
	if gotVars != 1 {
		t.Fatalf("want exactly one UNSET_ONE warning, got %d (%v)", gotVars, cfg.Warnings)
	}
	if !slices.ContainsFunc(cfg.Warnings, func(w string) bool { return strings.Contains(w, "UNSET_TWO") }) {
		t.Fatalf("missing UNSET_TWO warning: %v", cfg.Warnings)
	}
}

func TestExpanderStrictSemantics(t *testing.T) {
	t.Setenv("CFG_SET", "value")
	t.Setenv("HOME_LIKE", "/home/u")
	cases := []struct {
		name      string
		in        string
		want      string
		wantUnset []string
	}{
		{"literal_dollar_digit", "price$5", "price$5", nil},
		{"bare_dollar_word", "$NOPE not a ref", "$NOPE not a ref", nil},
		{"double_dollar", "$$", "$$", nil},
		{"double_dollar_word", "a$$b", "a$$b", nil},
		{"trailing_dollar", "cost$", "cost$", nil},
		{"dollar_in_token", "tok$en$here", "tok$en$here", nil},
		{"set_ref", "${CFG_SET}", "value", nil},
		{"home_style", "${HOME_LIKE}/bin", "/home/u/bin", nil},
		{"ref_amid_literals", "p=$5;${CFG_SET};$x", "p=$5;value;$x", nil},
		{"unset_ref", "${CFG_MISSING}", "", []string{"CFG_MISSING"}},
		{"default_ref_unset", "${CFG_MISSING:-fallback}", "fallback", nil},
		{"default_ref_set", "${CFG_SET:-fallback}", "value", nil},
		{"unterminated_brace", "${CFG_SET", "${CFG_SET", nil},
		{"empty_braces", "${}", "${}", nil},
		{"invalid_name_leading_digit", "${1ABC}", "${1ABC}", nil},
		{"invalid_name_punct", "${A-B}", "${A-B}", nil},
		{"dollar_brace_literal_then_ref", "$ {CFG_SET} ${CFG_SET}", "$ {CFG_SET} value", nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var e expander
			got := e.expand(tc.in)
			if got != tc.want {
				t.Fatalf("expand(%q) = %q, want %q", tc.in, got, tc.want)
			}
			if !slices.Equal(e.unsetNames(), tc.wantUnset) {
				t.Fatalf("expand(%q) unset = %v, want %v", tc.in, e.unsetNames(), tc.wantUnset)
			}
		})
	}
}

func TestLoadConfigLiteralDollarPreserved(t *testing.T) {
	// A secret containing '$' must round-trip unchanged (no os.Expand corruption).
	path := writeConfig(t, `{
		"mcpServers": {
			"s": {"command": "/bin/x", "args": ["--price=$5"], "env": {"TOKEN": "ab$$cd$9ef"}}
		}
	}`)
	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	s := cfg.Servers[0]
	if s.Args[0] != "--price=$5" {
		t.Fatalf("literal $ in arg corrupted: %q", s.Args[0])
	}
	if s.Env["TOKEN"] != "ab$$cd$9ef" {
		t.Fatalf("literal $ in env corrupted: %q", s.Env["TOKEN"])
	}
	if len(cfg.Warnings) != 0 {
		t.Fatalf("literal $ should produce no unset-var warnings: %v", cfg.Warnings)
	}
}

func TestLoadConfigStdioHTTPExclusivity(t *testing.T) {
	cases := []struct {
		name    string
		body    string
		wantSvr bool
	}{
		{"stdio_ok", `{"mcpServers":{"s":{"command":"x"}}}`, true},
		{"http_ok", `{"mcpServers":{"s":{"type":"http","url":"http://h/mcp"}}}`, true},
		{"streamable_http_ok", `{"mcpServers":{"s":{"type":"streamable-http","url":"http://h/mcp"}}}`, true},
		{"stdio_with_url", `{"mcpServers":{"s":{"command":"x","url":"http://h"}}}`, false},
		{"http_with_command", `{"mcpServers":{"s":{"type":"http","url":"http://h","command":"x"}}}`, false},
		{"stdio_no_command", `{"mcpServers":{"s":{"env":{"A":"b"}}}}`, false},
		{"http_no_url", `{"mcpServers":{"s":{"type":"http"}}}`, false},
		{"unknown_type", `{"mcpServers":{"s":{"type":"sse","url":"http://h"}}}`, false},
		{"bad_scheme", `{"mcpServers":{"s":{"type":"http","url":"ftp://h/mcp"}}}`, false},
		{"explicit_stdio_type", `{"mcpServers":{"s":{"type":"stdio","command":"x"}}}`, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := writeConfig(t, tc.body)
			cfg, err := LoadConfig(path)
			if err != nil {
				t.Fatalf("LoadConfig: %v", err)
			}
			got := len(cfg.Servers) == 1
			if got != tc.wantSvr {
				t.Fatalf("server present=%v want=%v (servers=%+v warnings=%v)", got, tc.wantSvr, cfg.Servers, cfg.Warnings)
			}
			if !tc.wantSvr && len(cfg.Warnings) == 0 {
				t.Fatalf("invalid server should produce a warning")
			}
		})
	}
}

func TestLoadConfigBadServerName(t *testing.T) {
	path := writeConfig(t, `{"mcpServers":{"bad name!":{"command":"x"},"good":{"command":"y"}}}`)
	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if len(cfg.Servers) != 1 || cfg.Servers[0].Name != "good" {
		t.Fatalf("bad-named server should be skipped: %+v", cfg.Servers)
	}
	if !slices.ContainsFunc(cfg.Warnings, func(w string) bool { return strings.Contains(w, "bad name!") }) {
		t.Fatalf("missing warning for bad name: %v", cfg.Warnings)
	}
}

func TestLoadConfigMissingDefaultIsEmpty(t *testing.T) {
	// Empty path => valid empty config.
	cfg, err := LoadConfig("")
	if err != nil {
		t.Fatalf("empty path should be valid empty config: %v", err)
	}
	if len(cfg.Servers) != 0 {
		t.Fatalf("want zero servers, got %+v", cfg.Servers)
	}
}

func TestLoadConfigExplicitMissingIsError(t *testing.T) {
	path := filepath.Join(t.TempDir(), "does-not-exist.json")
	if _, err := LoadConfig(path); err == nil {
		t.Fatalf("explicit missing path should error")
	}
}

func TestLoadConfigMalformedIsError(t *testing.T) {
	path := writeConfig(t, `{not valid json`)
	if _, err := LoadConfig(path); err == nil {
		t.Fatalf("malformed config should error")
	}
}

func TestLoadConfigListenFallback(t *testing.T) {
	path := writeConfig(t, `{"mcpServers":{}}`)
	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if cfg.Listen != DefaultListen {
		t.Fatalf("listen fallback = %q, want %q", cfg.Listen, DefaultListen)
	}
}

func TestChildEnvMergeLastWins(t *testing.T) {
	t.Setenv("MERGE_KEY", "parent")
	t.Setenv("KEEP_KEY", "kept")
	merged := ChildEnv(map[string]string{"MERGE_KEY": "child", "NEW_KEY": "new"})
	// Build a lookup of the LAST value per key (exec uses last-wins).
	last := map[string]string{}
	for _, kv := range merged {
		k, v, ok := strings.Cut(kv, "=")
		if !ok {
			continue
		}
		last[k] = v
	}
	if last["MERGE_KEY"] != "child" {
		t.Fatalf("MERGE_KEY last-wins failed: %q", last["MERGE_KEY"])
	}
	if last["KEEP_KEY"] != "kept" {
		t.Fatalf("KEEP_KEY should be inherited: %q", last["KEEP_KEY"])
	}
	if last["NEW_KEY"] != "new" {
		t.Fatalf("NEW_KEY should be added: %q", last["NEW_KEY"])
	}
}

func TestDefaultConfigPath(t *testing.T) {
	// XDG_CONFIG_HOME takes precedence.
	got := DefaultConfigPath(func(k string) string {
		if k == "XDG_CONFIG_HOME" {
			return "/xdg"
		}
		return ""
	})
	if got != "/xdg/harness-mcp-proxy/config.json" {
		t.Fatalf("XDG path = %q", got)
	}
	// Falls back to ~/.config.
	got = DefaultConfigPath(func(k string) string {
		if k == "HOME" {
			return "/home/u"
		}
		return ""
	})
	if got != "/home/u/.config/harness-mcp-proxy/config.json" {
		t.Fatalf("HOME path = %q", got)
	}
}

func TestURLForListen(t *testing.T) {
	tests := []struct {
		addr string
		want string
	}{
		{"127.0.0.1:8766", "http://127.0.0.1:8766"},
		{":8766", "http://127.0.0.1:8766"},
		{"0.0.0.0:8766", "http://127.0.0.1:8766"},
		{"[::]:8766", "http://127.0.0.1:8766"},
		{"[::1]:8766", "http://[::1]:8766"},
	}
	for _, tt := range tests {
		if got := URLForListen(tt.addr); got != tt.want {
			t.Errorf("URLForListen(%q) = %q, want %q", tt.addr, got, tt.want)
		}
	}
	if got := DefaultURL(); got != "http://"+DefaultListen {
		t.Errorf("DefaultURL() = %q, want http://%s", got, DefaultListen)
	}
}
