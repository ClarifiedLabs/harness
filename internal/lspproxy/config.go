package lspproxy

import (
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
)

// defaultsJSON is the built-in default server set (popular languages), embedded
// so the shim works out of the box when the language-server binaries are
// installed. The user config overlays it by server name.
//
//go:embed defaults.json
var defaultsJSON []byte

// FileConfig is the on-disk config shape: a version envelope plus a map of named
// servers. Decode is tolerant (plain json.Unmarshal, unknown keys ignored) per
// repo convention. Keys are snake_case, matching the documented format.
type FileConfig struct {
	Version int                     `json:"version"`
	Servers map[string]ServerConfig `json:"servers"`
}

// ServerConfig is one language-server entry. Command is an argv array (the
// program plus its arguments). Languages lists the LSP language ids this server
// handles; RootMarkers are the filenames whose nearest ancestor directory is the
// workspace root. Extensions optionally augments the built-in extension→language
// table. Env and InitOptions are optional.
type ServerConfig struct {
	Languages   []string          `json:"languages"`
	RootMarkers []string          `json:"root_markers"`
	Command     []string          `json:"command"`
	Extensions  []string          `json:"extensions"`
	Env         map[string]string `json:"env"`
	InitOptions json.RawMessage   `json:"initialization_options"`
}

// ResolvedServer is a validated, ${VAR}-expanded server ready to launch.
type ResolvedServer struct {
	Name        string
	Languages   []string
	RootMarkers []string
	Command     []string
	Extensions  []string
	Env         map[string]string
	InitOptions json.RawMessage
}

// Config is the resolved, validated configuration. Servers is sorted by name for
// stable ordering. Warnings collects non-fatal load problems (missing version,
// skipped invalid servers, unset expansion vars); the caller logs them — library
// code never prints.
type Config struct {
	Servers  []ResolvedServer
	Warnings []string
}

// serverNameRE constrains a server name; it becomes the middle segment of the
// proxy-namespaced tool name, so it is kept to the qualified-name charset.
var serverNameRE = regexp.MustCompile(`^[a-zA-Z0-9_-]{1,64}$`)

// LoadConfig builds the configuration from the embedded defaults overlaid with
// the user file at path (user entries win by server name). An empty path yields
// the defaults alone. An explicitly-given path that is missing, or any present-
// but-malformed file, is an error; an unsupported config version is a
// *ConfigVersionError. Invalid individual servers are skipped with a Warning,
// never fatal.
func LoadConfig(path string) (Config, error) {
	merged := map[string]ServerConfig{}

	var defs FileConfig
	if err := json.Unmarshal(defaultsJSON, &defs); err != nil {
		return Config{}, fmt.Errorf("lspproxy: parse embedded defaults: %w", err)
	}
	maps.Copy(merged, defs.Servers)

	var warnings []string
	if path != "" {
		data, err := os.ReadFile(path)
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				// An explicit path that does not exist is a hard error: a typo must not
				// silently degrade to "defaults only". (No user file is expressed by
				// the caller passing "".)
				return Config{}, fmt.Errorf("lspproxy: config %s not found: %w", path, err)
			}
			return Config{}, fmt.Errorf("lspproxy: read config %s: %w", path, err)
		}
		var fc FileConfig
		if err := json.Unmarshal(data, &fc); err != nil {
			return Config{}, fmt.Errorf("lspproxy: parse config %s: %w", path, err)
		}
		switch {
		case fc.Version == 0:
			warnings = append(warnings, "config has no \"version\"; assuming version 1")
		case !SupportsConfigVersion(fc.Version):
			return Config{}, &ConfigVersionError{Got: fc.Version, Supported: SupportedConfigVersion}
		}
		maps.Copy(merged, fc.Servers)
	}

	return resolve(merged, warnings), nil
}

// LoadConfigWithServers builds the configuration from the embedded defaults
// overlaid with inline user server definitions. User entries win by server name.
// It is used by harness's first-class LSP config, which has no separate version
// envelope because the surrounding harness config already owns compatibility.
func LoadConfigWithServers(servers map[string]ServerConfig) (Config, error) {
	merged := map[string]ServerConfig{}

	var defs FileConfig
	if err := json.Unmarshal(defaultsJSON, &defs); err != nil {
		return Config{}, fmt.Errorf("lspproxy: parse embedded defaults: %w", err)
	}
	maps.Copy(merged, defs.Servers)
	maps.Copy(merged, servers)

	return resolve(merged, nil), nil
}

// resolve expands ${VAR} references, validates each server, and produces a Config
// sorted by name. Expansion happens before validation so a validated field is
// always the post-expansion value.
func resolve(servers map[string]ServerConfig, warnings []string) Config {
	cfg := Config{Warnings: warnings}

	var exp expander
	expand := exp.expand

	names := make([]string, 0, len(servers))
	for name := range servers {
		names = append(names, name)
	}
	sort.Strings(names)

	for _, name := range names {
		rs, warn := resolveServer(name, servers[name], expand)
		if warn != "" {
			cfg.Warnings = append(cfg.Warnings, warn)
			continue
		}
		cfg.Servers = append(cfg.Servers, rs)
	}

	for _, v := range exp.unsetNames() {
		cfg.Warnings = append(cfg.Warnings, fmt.Sprintf("config references unset variable ${%s}; expanded to empty string", v))
	}
	return cfg
}

// resolveServer expands and validates one entry. It returns either a resolved
// server (warn == "") or a non-empty warning describing why it was skipped.
func resolveServer(name string, sc ServerConfig, expand func(string) string) (ResolvedServer, string) {
	if !serverNameRE.MatchString(name) {
		return ResolvedServer{}, fmt.Sprintf("server %q skipped: name must match [a-zA-Z0-9_-]{1,64}", name)
	}
	command := expandSlice(sc.Command, expand)
	if len(command) == 0 || command[0] == "" {
		return ResolvedServer{}, fmt.Sprintf("server %q skipped: requires a non-empty command", name)
	}
	languages := expandSlice(sc.Languages, expand)
	if len(languages) == 0 {
		return ResolvedServer{}, fmt.Sprintf("server %q skipped: requires at least one language", name)
	}
	return ResolvedServer{
		Name:        name,
		Languages:   languages,
		RootMarkers: expandSlice(sc.RootMarkers, expand),
		Command:     command,
		Extensions:  expandSlice(sc.Extensions, expand),
		Env:         expandMap(sc.Env, expand),
		InitOptions: sc.InitOptions,
	}, ""
}

// expandSlice returns a copy of in with every element expanded; nil stays nil.
func expandSlice(in []string, expand func(string) string) []string {
	if in == nil {
		return nil
	}
	out := make([]string, len(in))
	for i, v := range in {
		out[i] = expand(v)
	}
	return out
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

// expander substitutes ${NAME} and ${NAME:-default} references in config strings,
// tracking which strict referenced vars were unset so the loader can warn once
// per distinct name. Only NAME matching [A-Za-z_][A-Za-z0-9_]* is recognized;
// every other '$' is passed through literally, avoiding os.Expand's eating of
// "$$"/"$<digit>" which would corrupt values containing '$'.
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
// '$'). A malformed reference returns ok=false so the caller emits a literal '$'.
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
	name, def, hasDefault := strings.Cut(s[start:j], ":-")
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

// ChildEnv builds the environment for a language-server child: the full parent
// environment with extra entries appended so they win on conflict. A nil/empty
// extra map yields the parent environment unchanged.
func ChildEnv(extra map[string]string) []string {
	env := os.Environ()
	if len(extra) == 0 {
		return env
	}
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
// $XDG_CONFIG_HOME/harness/lsp.json, else ~/.config/harness/lsp.json.
// getenv injects the environment so the resolution is testable.
func DefaultConfigPath(getenv func(string) string) string {
	if xdg := getenv("XDG_CONFIG_HOME"); xdg != "" {
		return filepath.Join(xdg, "harness", "lsp.json")
	}
	return filepath.Join(getenv("HOME"), ".config", "harness", "lsp.json")
}
