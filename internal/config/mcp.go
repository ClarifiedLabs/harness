package config

import (
	"fmt"
	"maps"
	"path/filepath"
	"slices"
	"sort"
	"strings"

	"harness/internal/auth"
	"harness/internal/mcpproxy"
)

func rejectRemovedEnvironment(lookup func(string) (string, bool)) error {
	for _, name := range []string{"HARNESS_MCP_ENABLE", "HARNESS_MCP_PROXY", "HARNESS_MCP_PROXY_API_KEY", "HARNESS_MCP_LOCAL_ENABLE", "HARNESS_LSP_ENABLE", "HARNESS_LSP_PREWARM", "HARNESS_LSP_SERENA_ENABLE"} {
		if _, ok := lookup(name); ok {
			return fmt.Errorf("environment %s is no longer supported; remove it and migrate to named mcp.servers entries (the lsp configuration has been removed)", name)
		}
	}
	return nil
}

func cloneMCPServers(in map[string]mcpproxy.ServerConfig) map[string]mcpproxy.ServerConfig {
	if in == nil {
		return nil
	}
	out := make(map[string]mcpproxy.ServerConfig, len(in))
	for name, server := range in {
		if server.Enabled != nil {
			enabled := *server.Enabled
			server.Enabled = &enabled
		}
		server.Args = slices.Clone(server.Args)
		server.Env = maps.Clone(server.Env)
		server.Headers = maps.Clone(server.Headers)
		server.ExcludedTools = slices.Clone(server.ExcludedTools)
		server.Auth = auth.Expand(server.Auth, nil)
		out[name] = server
	}
	return out
}

func resolveMCPServers(context *resolveContext) error {
	context.defaultSource("mcp.servers")
	if !context.file.MCP.Set || !context.file.MCP.Value.Servers.Set {
		return nil
	}
	servers := cloneMCPServers(context.file.MCP.Value.Servers.Value)
	dirs := make(map[string]string, len(servers))
	names := make([]string, 0, len(servers))
	for name := range servers {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		key := "mcp.servers." + name
		server := servers[name]
		source := context.fileSourceFor(key)
		var expansionErr error
		expand := func(value string) string {
			expanded, err := expandEnvRefs(value, context.lookup)
			if err != nil && expansionErr == nil {
				expansionErr = err
			}
			return expanded
		}
		if server.IsEnabled() {
			if context.file.MCP.Value.legacy != nil && name == "local" && server.Type == "stdio" && strings.TrimSpace(server.Command) == "" {
				return context.fileError(key, fmt.Errorf("mcp.local.command: enabled legacy connection requires a command"))
			}
			server.Command = expand(server.Command)
			server.URL = expand(server.URL)
			for i, arg := range server.Args {
				server.Args[i] = expand(arg)
			}
			for key, value := range server.Env {
				server.Env[key] = expand(value)
			}
			for key, value := range server.Headers {
				server.Headers[key] = expand(value)
			}
			server.Auth = auth.Expand(server.Auth, expand)
		}
		if expansionErr != nil {
			return context.fileError(key, expansionErr)
		}
		// Transport inference and validation belong to the shared MCP runtime.
		servers[name] = server
		sourcePath, err := filepath.Abs(source.Name)
		if err != nil {
			return context.fileError(key, fmt.Errorf("resolve server config directory: %w", err))
		}
		dirs[name] = filepath.Dir(sourcePath)
		context.result.Sources[key] = source
	}
	context.result.Config.MCP.Servers = servers
	context.result.Config.MCP.ServerConfigDirs = dirs
	context.fileSource("mcp.servers")
	return nil
}

func redactMCPString(value string) string {
	if value == "" {
		return ""
	}
	return redactedValue
}

func redactMCPServers(values map[string]mcpproxy.ServerConfig) any {
	if values == nil {
		return map[string]any(nil)
	}
	out := make(map[string]any, len(values))
	for name, server := range values {
		// These fields may contain expanded environment secrets, not just paths
		// or public endpoint addresses. Preserve shape, never their contents.
		args := slices.Clone(server.Args)
		for i := range args {
			args[i] = redactedValue
		}
		entry := map[string]any{
			"type": server.Type, "command": redactMCPString(server.Command), "args": args,
			"url": redactMCPString(server.URL), "headers": redactStringMap(server.Headers),
			"env": redactStringMap(server.Env), "excludedTools": slices.Clone(server.ExcludedTools),
		}
		if server.Enabled != nil {
			entry["enabled"] = *server.Enabled
		}
		if server.Auth != nil {
			entry["auth"] = redactedValue
		}
		out[name] = entry
	}
	return out
}
