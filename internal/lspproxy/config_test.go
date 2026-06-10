package lspproxy

import (
	"errors"
	"os"
	"path/filepath"
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

func findServer(t *testing.T, cfg Config, name string) ResolvedServer {
	t.Helper()
	for _, s := range cfg.Servers {
		if s.Name == name {
			return s
		}
	}
	t.Fatalf("server %q not found in %v", name, serverNames(cfg))
	return ResolvedServer{}
}

func serverNames(cfg Config) []string {
	names := make([]string, len(cfg.Servers))
	for i, s := range cfg.Servers {
		names[i] = s.Name
	}
	return names
}

func TestLoadConfigDefaultsOnly(t *testing.T) {
	cfg, err := LoadConfig("")
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	for _, want := range []string{"gopls", "rust-analyzer", "pyright", "typescript-language-server", "clangd"} {
		s := findServer(t, cfg, want)
		if len(s.Command) == 0 {
			t.Fatalf("default %q has empty command", want)
		}
		if len(s.Languages) == 0 {
			t.Fatalf("default %q has no languages", want)
		}
	}
	gopls := findServer(t, cfg, "gopls")
	if !slices.Contains(gopls.Languages, "go") {
		t.Fatalf("gopls languages = %v, want to contain go", gopls.Languages)
	}
}

func TestLoadConfigUserOverlayWins(t *testing.T) {
	path := writeConfig(t, `{
		"version": 1,
		"servers": {
			"gopls": {"languages": ["go"], "root_markers": [".git"], "command": ["gopls", "-rpc.trace"]},
			"customlsp": {"languages": ["foo"], "root_markers": [".git"], "command": ["foolsp"]}
		}
	}`)
	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	// User override replaces the default gopls command.
	if got := findServer(t, cfg, "gopls").Command; !slices.Equal(got, []string{"gopls", "-rpc.trace"}) {
		t.Fatalf("gopls command = %v, want overridden", got)
	}
	// A brand-new user server is added.
	findServer(t, cfg, "customlsp")
	// A default the user did not touch survives.
	findServer(t, cfg, "rust-analyzer")
}

func TestLoadConfigWithServersOverlayWins(t *testing.T) {
	cfg, err := LoadConfigWithServers(map[string]ServerConfig{
		"gopls":     {Languages: []string{"go"}, RootMarkers: []string{".git"}, Command: []string{"gopls", "-remote=auto"}},
		"customlsp": {Languages: []string{"foo"}, RootMarkers: []string{".git"}, Command: []string{"foolsp"}},
	})
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if got := findServer(t, cfg, "gopls").Command; !slices.Equal(got, []string{"gopls", "-remote=auto"}) {
		t.Fatalf("gopls command = %v, want inline override", got)
	}
	findServer(t, cfg, "customlsp")
	findServer(t, cfg, "rust-analyzer")
}

func TestLoadConfigVersionGate(t *testing.T) {
	path := writeConfig(t, `{"version": 2, "servers": {}}`)
	_, err := LoadConfig(path)
	var verr *ConfigVersionError
	if !errors.As(err, &verr) {
		t.Fatalf("got err %v, want *ConfigVersionError", err)
	}
	if verr.Got != 2 || verr.Supported != SupportedConfigVersion {
		t.Fatalf("version error = %+v", verr)
	}
}

func TestLoadConfigMissingVersionWarns(t *testing.T) {
	path := writeConfig(t, `{"servers": {"x": {"languages": ["x"], "command": ["x"]}}}`)
	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if !hasWarningContaining(cfg.Warnings, "version") {
		t.Fatalf("warnings = %v, want one mentioning version", cfg.Warnings)
	}
	findServer(t, cfg, "x")
}

func TestLoadConfigExplicitMissingIsError(t *testing.T) {
	if _, err := LoadConfig(filepath.Join(t.TempDir(), "nope.json")); err == nil {
		t.Fatal("expected error for missing explicit path, got nil")
	}
}

func TestLoadConfigMalformedIsError(t *testing.T) {
	path := writeConfig(t, `{not json`)
	if _, err := LoadConfig(path); err == nil {
		t.Fatal("expected parse error, got nil")
	}
}

func TestLoadConfigInvalidServersSkippedWithWarning(t *testing.T) {
	path := writeConfig(t, `{
		"version": 1,
		"servers": {
			"nolang": {"languages": [], "command": ["x"]},
			"nocmd": {"languages": ["go"], "command": []},
			"bad name": {"languages": ["go"], "command": ["x"]}
		}
	}`)
	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	for _, name := range []string{"nolang", "nocmd", "bad name"} {
		if slices.Contains(serverNames(cfg), name) {
			t.Fatalf("invalid server %q was not skipped", name)
		}
	}
	if len(cfg.Warnings) < 3 {
		t.Fatalf("warnings = %v, want at least 3 skip warnings", cfg.Warnings)
	}
}

func TestLoadConfigExpandsVars(t *testing.T) {
	t.Setenv("LSP_TEST_BIN", "/opt/gopls")
	t.Setenv("LSP_TEST_FLAG", "verbose")
	path := writeConfig(t, `{
		"version": 1,
		"servers": {
			"x": {"languages": ["go"], "command": ["${LSP_TEST_BIN}", "--${LSP_TEST_FLAG}"], "env": {"K": "${LSP_TEST_FLAG}"}}
		}
	}`)
	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	x := findServer(t, cfg, "x")
	if !slices.Equal(x.Command, []string{"/opt/gopls", "--verbose"}) {
		t.Fatalf("command = %v, want expanded", x.Command)
	}
	if x.Env["K"] != "verbose" {
		t.Fatalf("env K = %q, want verbose", x.Env["K"])
	}
}

func TestLoadConfigUnsetVarWarns(t *testing.T) {
	path := writeConfig(t, `{
		"version": 1,
		"servers": {"x": {"languages": ["go"], "command": ["bin", "${LSP_UNSET_VAR}"]}}
	}`)
	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if !hasWarningContaining(cfg.Warnings, "LSP_UNSET_VAR") {
		t.Fatalf("warnings = %v, want one mentioning LSP_UNSET_VAR", cfg.Warnings)
	}
}

func TestDefaultConfigPath(t *testing.T) {
	xdg := func(k string) string {
		if k == "XDG_CONFIG_HOME" {
			return "/xdg"
		}
		return ""
	}
	if got := DefaultConfigPath(xdg); got != filepath.Join("/xdg", "harness", "lsp.json") {
		t.Fatalf("xdg path = %q", got)
	}
	home := func(k string) string {
		if k == "HOME" {
			return "/home/u"
		}
		return ""
	}
	if got := DefaultConfigPath(home); got != filepath.Join("/home/u", ".config", "harness", "lsp.json") {
		t.Fatalf("home path = %q", got)
	}
}

func hasWarningContaining(warnings []string, sub string) bool {
	for _, w := range warnings {
		if strings.Contains(w, sub) {
			return true
		}
	}
	return false
}
