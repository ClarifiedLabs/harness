package config

import (
	"bytes"
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"strings"

	"harness/internal/atomicfile"
	"harness/internal/mcpproxy"
)

type legacyLocalMCPConfig struct {
	Enable  optional[bool]              `json:"enable"`
	Command optional[string]            `json:"command"`
	Args    optional[[]string]          `json:"args"`
	Env     optional[map[string]string] `json:"env"`
}

type legacyMCPConfig struct {
	Enable          optional[bool]                             `json:"enable"`
	Proxy           optional[string]                           `json:"proxy"`
	URL             optional[string]                           `json:"url"`
	APIKey          optional[string]                           `json:"api_key"`
	Headers         optional[map[string]string]                `json:"headers"`
	Local           optional[legacyLocalMCPConfig]             `json:"local"`
	Servers         optional[map[string]mcpproxy.ServerConfig] `json:"servers"`
	MaxTools        optional[int]                              `json:"max_tools"`
	DisabledServers optional[[]string]                         `json:"disabled_servers"`
}

// migrateMCPConfig changes only the legacy MCP keys. Keeping the rest as raw
// JSON lets the normal strict decoder reject unknown settings everywhere.
func migrateMCPConfig(data []byte) ([]byte, bool, error) {
	var root map[string]json.RawMessage
	if err := json.Unmarshal(data, &root); err != nil {
		return nil, false, err
	}
	if _, ok := root["lsp"]; ok {
		return nil, false, fmt.Errorf("lsp configuration has been removed; configure external language tools as named mcp.servers entries instead (including Serena); native LSP cannot be migrated automatically")
	}
	raw, ok := root["mcp"]
	if !ok {
		return data, false, nil
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return nil, false, fmt.Errorf("mcp: %w", err)
	}
	changed := false
	for _, key := range []string{"enable", "proxy", "url", "api_key", "headers", "local"} {
		if _, ok := fields[key]; ok {
			changed = true
		}
	}
	if !changed {
		return data, false, nil
	}
	var legacy legacyMCPConfig
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&legacy); err != nil {
		return nil, false, fmt.Errorf("mcp: %w", err)
	}
	servers := fields["servers"]
	var entries map[string]json.RawMessage
	if servers != nil {
		if err := json.Unmarshal(servers, &entries); err != nil {
			return nil, false, fmt.Errorf("mcp.servers: %w", err)
		}
	}
	if entries == nil {
		entries = make(map[string]json.RawMessage)
	}
	add := func(name string, server mcpproxy.ServerConfig) error {
		if _, exists := entries[name]; exists {
			return fmt.Errorf("cannot migrate mcp: named mcp.servers.%s already exists; rename it before migration", name)
		}
		encoded, err := json.Marshal(server)
		if err != nil {
			return err
		}
		entries[name] = encoded
		return nil
	}
	shared := legacy.Enable.Set || legacy.Proxy.Set || legacy.URL.Set || legacy.APIKey.Set || legacy.Headers.Set
	if shared {
		if legacy.Proxy.Set && legacy.URL.Set {
			return nil, false, fmt.Errorf("mcp.proxy and mcp.url cannot both be set")
		}
		if err := add("shared", legacySharedServer(legacy)); err != nil {
			return nil, false, err
		}
	}
	if legacy.Local.Set {
		local := legacy.Local.Value
		enabled := local.Enable.Value
		if err := add("local", mcpproxy.ServerConfig{Enabled: &enabled, Type: "stdio", Command: local.Command.Value, Args: local.Args.Value, Env: local.Env.Value}); err != nil {
			return nil, false, err
		}
	}
	for _, key := range []string{"enable", "proxy", "url", "api_key", "headers", "local"} {
		delete(fields, key)
	}
	fields["servers"], _ = json.Marshal(entries)
	root["mcp"], _ = json.Marshal(fields)
	output, err := json.MarshalIndent(root, "", "  ")
	if err != nil {
		return nil, false, err
	}
	return append(output, '\n'), true, nil
}

// MigrateFile explicitly persists legacy MCP migration without expanding env
// references. It validates the entire config before creating a private,
// exclusive byte-for-byte backup and atomically replacing the config (0600).
// New-format files are validated but left untouched. No secrets are printed.
func MigrateFile(path string) (backupPath string, changed bool, err error) {
	return migrateFile(path, nil)
}

// beforeReplace is a test seam for deterministic concurrent-edit coverage.
func migrateFile(path string, beforeReplace func()) (backupPath string, changed bool, err error) {
	identity, err := os.Lstat(path)
	if err != nil {
		return "", false, err
	}
	if !identity.Mode().IsRegular() {
		return "", false, fmt.Errorf("config %q must be a regular file, not a symlink", path)
	}
	original, err := os.ReadFile(path)
	if err != nil {
		return "", false, fmt.Errorf("read config %q: %w", path, err)
	}
	fc, migrated, needsMigration, err := decodeConfigData(original)
	if err != nil {
		return "", false, fmt.Errorf("decode config %q: %w", path, err)
	}
	if err := validateFileConfig(fc, path); err != nil {
		return "", false, err
	}
	if !needsMigration {
		return "", false, nil
	}
	if isProjectMigrationPath(path) {
		return "", false, fmt.Errorf("cannot migrate legacy project config %q independently: inherited MCP fields would change; consolidate global and project MCP settings into named servers manually", path)
	}
	if local := fc.MCP.Value.legacy; local != nil && local.Local.Value.Enable.Value && strings.TrimSpace(local.Local.Value.Command.Value) == "" {
		return "", false, fmt.Errorf("mcp.local.command: enabled legacy connection requires a command")
	}
	backupPath = path + ".pre-mcp-migration.bak"
	backup, err := os.OpenFile(backupPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return "", false, fmt.Errorf("create migration backup: %w", err)
	}
	if _, err := backup.Write(original); err != nil {
		_ = backup.Close()
		_ = os.Remove(backupPath)
		return "", false, fmt.Errorf("write migration backup: %w", err)
	}
	if err := backup.Sync(); err != nil {
		_ = backup.Close()
		_ = os.Remove(backupPath)
		return "", false, fmt.Errorf("sync migration backup: %w", err)
	}
	if err := backup.Close(); err != nil {
		_ = os.Remove(backupPath)
		return "", false, fmt.Errorf("close migration backup: %w", err)
	}
	err = atomicfile.Replace(path, ".config-migration-*.tmp", func(tmp *os.File) error {
		if err := tmp.Chmod(0o600); err != nil {
			return err
		}
		if _, err := tmp.Write(migrated); err != nil {
			return err
		}
		if err := tmp.Sync(); err != nil {
			return err
		}
		if beforeReplace != nil {
			beforeReplace()
		}
		return checkMigrationSource(path, identity, original)
	})
	if err != nil {
		return backupPath, false, fmt.Errorf("replace migrated config: %w", err)
	}
	return backupPath, true, nil
}

// Project files inherit global legacy leaves. Persisting a standalone translation
// would erase that provenance, even when all currently useful fields are present.
func isProjectMigrationPath(path string) bool {
	abs, err := filepath.Abs(path)
	if err != nil || filepath.Base(abs) != "config.json" || filepath.Base(filepath.Dir(abs)) != ".harness" {
		return false
	}
	home, err := os.UserHomeDir()
	return err != nil || filepath.Clean(abs) != filepath.Join(home, ".harness", "config.json")
}

func checkMigrationSource(path string, identity os.FileInfo, original []byte) error {
	current, err := os.Lstat(path)
	if err != nil {
		return fmt.Errorf("check migration source: %w", err)
	}
	if !current.Mode().IsRegular() || !os.SameFile(identity, current) {
		return fmt.Errorf("config changed during migration; refusing replacement")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	if !bytes.Equal(data, original) {
		return fmt.Errorf("config changed during migration; refusing replacement")
	}
	return nil
}

func hasLegacyShared(l *legacyMCPConfig) bool {
	return l != nil && (l.Enable.Set || l.Proxy.Set || l.URL.Set || l.APIKey.Set || l.Headers.Set)
}

// Merge legacy leaves before translating; named server entries still replace
// entire entries, while legacy maps/slices retain their old leaf replacement.
func mergeLegacyMCP(g, p fileMCPConfig, globalPath, projectPath string, sources map[string]string) (fileMCPConfig, fileMCPConfig) {
	if g.legacy == nil && p.legacy == nil {
		return g, p
	}
	var gl, pl legacyMCPConfig
	if g.legacy != nil {
		gl = *g.legacy
	}
	if p.legacy != nil {
		pl = *p.legacy
	}
	g.Servers.Value = cloneMCPServers(g.Servers.Value)
	p.Servers.Value = cloneMCPServers(p.Servers.Value)
	if hasLegacyShared(g.legacy) {
		delete(g.Servers.Value, "shared")
	}
	if hasLegacyShared(p.legacy) {
		delete(p.Servers.Value, "shared")
	}
	if gl.Local.Set {
		delete(g.Servers.Value, "local")
	}
	if pl.Local.Set {
		delete(p.Servers.Value, "local")
	}
	proxy := func(l legacyMCPConfig) optional[string] {
		if l.URL.Set {
			return l.URL
		}
		return l.Proxy
	}
	l := legacyMCPConfig{
		Enable:  mergeOpt(gl.Enable, pl.Enable, globalPath, projectPath, "mcp.enable", sources),
		Proxy:   mergeOpt(proxy(gl), proxy(pl), globalPath, projectPath, "mcp.proxy", sources),
		APIKey:  mergeOpt(gl.APIKey, pl.APIKey, globalPath, projectPath, "mcp.api_key", sources),
		Headers: mergeOpt(gl.Headers, pl.Headers, globalPath, projectPath, "mcp.headers", sources),
	}
	local := legacyLocalMCPConfig{
		Enable:  mergeOpt(gl.Local.Value.Enable, pl.Local.Value.Enable, globalPath, projectPath, "mcp.local.enable", sources),
		Command: mergeOpt(gl.Local.Value.Command, pl.Local.Value.Command, globalPath, projectPath, "mcp.local.command", sources),
		Args:    mergeOpt(gl.Local.Value.Args, pl.Local.Value.Args, globalPath, projectPath, "mcp.local.args", sources),
		Env:     mergeOpt(gl.Local.Value.Env, pl.Local.Value.Env, globalPath, projectPath, "mcp.local.env", sources),
	}
	servers := make(map[string]mcpproxy.ServerConfig)
	if hasLegacyShared(g.legacy) || hasLegacyShared(p.legacy) {
		servers["shared"] = legacySharedServer(l)
	}
	if gl.Local.Set || pl.Local.Set {
		servers["local"] = mcpproxy.ServerConfig{Enabled: &local.Enable.Value, Type: "stdio", Command: local.Command.Value, Args: local.Args.Value, Env: local.Env.Value}
	}
	for name, server := range servers {
		target := &g
		if name == "shared" && hasLegacyShared(p.legacy) || name == "local" && pl.Local.Set {
			target = &p
		}
		if target.Servers.Value == nil {
			target.Servers.Value = make(map[string]mcpproxy.ServerConfig)
		}
		target.Servers.Set = true
		target.Servers.Value[name] = server
	}
	return g, p
}

func legacySharedServer(legacy legacyMCPConfig) mcpproxy.ServerConfig {
	url := legacy.Proxy.Value
	if legacy.URL.Set {
		url = legacy.URL.Value
	}
	if !legacy.Proxy.Set && !legacy.URL.Set {
		url = mcpproxy.DefaultURL()
	}
	headers := maps.Clone(legacy.Headers.Value)
	if legacy.APIKey.Value != "" {
		if headers == nil {
			headers = make(map[string]string)
		}
		for key := range headers {
			if strings.EqualFold(key, "Authorization") {
				delete(headers, key)
			}
		}
		headers["Authorization"] = "Bearer " + legacy.APIKey.Value
	}
	return mcpproxy.ServerConfig{Enabled: &legacy.Enable.Value, Type: "http", URL: url, Headers: headers}
}
