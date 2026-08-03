package mcpproxy

import (
	"strconv"

	"harness/internal/cli"
	"harness/internal/configmeta"
)

const (
	// DefaultMetricsListen is the local address used by the MCP proxy's
	// Prometheus endpoint when no file or command-line value selects another one.
	DefaultMetricsListen = "127.0.0.1:9091"
)

var parameterCatalog = configmeta.MustCatalog(
	configmeta.Parameter{
		Key:         "mcp_servers",
		Type:        "object",
		JSONPath:    "mcpServers",
		Default:     configmeta.Default{Kind: configmeta.DefaultLiteral, Value: map[string]any{}},
		Description: "Downstream MCP server definitions.",
		Sensitive:   true,
	},
	configmeta.Parameter{
		Key:         "listen",
		Type:        "string",
		Flags:       []string{"listen"},
		JSONPath:    "proxy.listen",
		Default:     configmeta.Default{Kind: configmeta.DefaultLiteral, Value: DefaultListen},
		Description: "HTTP listen address.",
	},
	configmeta.Parameter{
		Key:         "log_file",
		Type:        "path",
		Flags:       []string{"log"},
		JSONPath:    "proxy.logFile",
		Default:     configmeta.Default{Kind: configmeta.DefaultLiteral, Value: "", Display: "stderr"},
		Description: "Log file path; empty writes to stderr.",
	},
	configmeta.Parameter{
		Key:         "log_level",
		Type:        "string",
		Flags:       []string{"log-level"},
		JSONPath:    "proxy.logLevel",
		Default:     configmeta.Default{Kind: configmeta.DefaultLiteral, Value: "info"},
		Description: "Minimum log level.",
		Accepted:    []string{"debug", "info", "warn", "warning", "error"},
	},
	configmeta.Parameter{
		Key:         "log_format",
		Type:        "string",
		Flags:       []string{"log-format"},
		JSONPath:    "proxy.logFormat",
		Default:     configmeta.Default{Kind: configmeta.DefaultLiteral, Value: "json"},
		Description: "Log output format.",
		Accepted:    []string{"json", "text"},
	},
	configmeta.Parameter{
		Key:         "api_keys_file",
		Type:        "path",
		Flags:       []string{"api-keys-file"},
		JSONPath:    "proxy.api_keys_file",
		Default:     configmeta.Default{Kind: configmeta.DefaultDerived, Display: "api_keys.json beside the selected config"},
		Description: "File containing accepted proxy API-key hashes.",
	},
	configmeta.Parameter{
		Key:         "metrics_enabled",
		Type:        "boolean",
		Flags:       []string{"no-metrics"},
		JSONPath:    "proxy.metrics.enabled",
		Default:     configmeta.Default{Kind: configmeta.DefaultLiteral, Value: true, Note: "The no-metrics flag inversely controls this setting"},
		Description: "Whether the Prometheus metrics endpoint is enabled.",
	},
	configmeta.Parameter{
		Key:         "metrics_listen",
		Type:        "string",
		Flags:       []string{"metrics-listen"},
		JSONPath:    "proxy.metrics.listen",
		Default:     configmeta.Default{Kind: configmeta.DefaultLiteral, Value: DefaultMetricsListen},
		Description: "Prometheus metrics listen address.",
	},
)

var invocationCLIFlags = []cli.Flag{
	{ID: "config", Names: []string{"config"}, Kind: cli.ValueFlag, ValueName: "path", Description: "config file path"},
}

// Catalog returns the MCP proxy's immutable source-resolved setting catalog.
func Catalog() configmeta.Catalog { return parameterCatalog }

// SettingCLIFlags projects the catalog settings that have command-line forms.
func SettingCLIFlags() []cli.Flag {
	var flags []cli.Flag
	for _, parameter := range parameterCatalog.Parameters() {
		if len(parameter.Flags) == 0 {
			continue
		}
		kind := cli.ValueFlag
		valueName := parameter.Type
		defaultValue := configmeta.FormatDefault(parameter.Default)
		if value, ok := parameter.Default.Value.(string); ok {
			defaultValue = value
		}
		description := parameter.Description
		if parameter.Type == "boolean" {
			kind = cli.BoolFlag
			valueName = ""
			defaultValue = strconv.FormatBool(false) // -no-metrics is inverse.
			description = "Disable the Prometheus metrics endpoint."
		}
		flags = append(flags, cli.Flag{
			ID:          parameter.Key,
			Names:       append([]string(nil), parameter.Flags...),
			Kind:        kind,
			ValueName:   valueName,
			Description: description,
			Default:     defaultValue,
		})
	}
	return flags
}

// CLIFlags returns source-selection flags followed by catalog setting flags.
// Invocation-only command flags such as stdio, proxy, and api-key belong to the
// command catalog rather than this configuration catalog.
func CLIFlags() []cli.Flag {
	flags := cloneConfigCLIFlags(invocationCLIFlags)
	flags = append(flags, SettingCLIFlags()...)
	return flags
}

func cloneConfigCLIFlags(in []cli.Flag) []cli.Flag {
	out := make([]cli.Flag, len(in))
	for i, flag := range in {
		flag.Names = append([]string(nil), flag.Names...)
		flag.Environment = append([]string(nil), flag.Environment...)
		out[i] = flag
	}
	return out
}
