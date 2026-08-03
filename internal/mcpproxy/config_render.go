package mcpproxy

import "harness/internal/configmeta"

// Snapshot projects a resolved MCP proxy configuration into secret-safe values.
// Server definitions are represented only by stable shape metadata: command
// arguments, environment/header values, URLs, auth details, and inline secret
// material are never copied into the snapshot.
func Snapshot(result ConfigResult) configmeta.Snapshot {
	values := map[string]any{
		"mcp_servers":     safeServerSummary(result.Config.Servers),
		"listen":          result.Config.Listen,
		"log_file":        result.Config.LogFile,
		"log_level":       result.Config.LogLevel,
		"log_format":      result.Config.LogFormat,
		"api_keys_file":   result.Config.APIKeysFile,
		"metrics_enabled": result.Config.Metrics.Enabled != nil && *result.Config.Metrics.Enabled,
		"metrics_listen":  result.Config.Metrics.Listen,
	}
	return configmeta.NewSnapshot(values, result.Sources)
}

func safeServerSummary(servers []ResolvedServer) map[string]any {
	names := make([]string, 0, len(servers))
	transports := make(map[string]string, len(servers))
	for _, server := range servers {
		names = append(names, server.Name)
		transport := "stdio"
		if server.Transport == TransportHTTP {
			transport = "http"
		}
		transports[server.Name] = transport
	}
	return map[string]any{
		"kind":       "structured",
		"count":      len(servers),
		"names":      names,
		"transports": transports,
	}
}
