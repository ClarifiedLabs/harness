// Package config owns the source-resolved model-proxy configuration facade.
package config

import (
	"harness/internal/cli"
	"harness/internal/configmeta"
	"harness/internal/llm"
)

const (
	defaultModelsDevCacheTTL      = "24h"
	defaultProviderModelsCacheTTL = "1h"
	defaultDrainDelay             = "5s"
	defaultShutdownTimeout        = "5m"
	defaultMetricsListen          = "127.0.0.1:9090"
)

var catalog = configmeta.MustCatalog(
	configmeta.Parameter{Key: "provider_configs", Type: "array", JSONPath: "provider_configs", Default: literalDefault([]string{}, "[]"), Description: "Ordered provider configuration file paths, resolved relative to the main configuration file."},
	configmeta.Parameter{Key: "default_context_window", Type: "integer", JSONPath: "default_context_window", Default: literalDefault(llm.DefaultContextWindow, "256000"), Description: "Fallback context window for models without an explicit value."},
	configmeta.Parameter{Key: "log_level", Type: "string", Flags: []string{"log-level"}, JSONPath: "log_level", Default: literalDefault("info", "info"), Description: "Proxy log level.", Accepted: []string{"debug", "info", "warn", "error"}},
	configmeta.Parameter{Key: "log_format", Type: "string", Flags: []string{"log-format"}, JSONPath: "log_format", Default: literalDefault("json", "json"), Description: "Proxy log format.", Accepted: []string{"json", "text"}},
	configmeta.Parameter{Key: "models_dev_cache_ttl", Type: "duration", Flags: []string{"models-dev-cache-ttl"}, JSONPath: "models_dev_cache_ttl", Default: literalDefault(defaultModelsDevCacheTTL, defaultModelsDevCacheTTL), Description: "models.dev cache refresh interval; zero disables periodic refresh."},
	configmeta.Parameter{Key: "provider_models_cache_ttl", Type: "duration", Flags: []string{"provider-models-cache-ttl"}, JSONPath: "provider_models_cache_ttl", Default: literalDefault(defaultProviderModelsCacheTTL, defaultProviderModelsCacheTTL), Description: "Authenticated provider model catalog refresh interval; zero disables background refresh."},
	configmeta.Parameter{Key: "drain_delay", Type: "duration", Flags: []string{"drain-delay"}, Environment: []string{"HARNESS_MODEL_PROXY_DRAIN_DELAY"}, JSONPath: "drain_delay", Default: literalDefault(defaultDrainDelay, defaultDrainDelay), Description: "Readiness propagation delay before API shutdown."},
	configmeta.Parameter{Key: "shutdown_timeout", Type: "duration", Flags: []string{"shutdown-timeout"}, Environment: []string{"HARNESS_MODEL_PROXY_SHUTDOWN_TIMEOUT"}, JSONPath: "shutdown_timeout", Default: literalDefault(defaultShutdownTimeout, defaultShutdownTimeout), Description: "Maximum graceful stream drain time."},
	configmeta.Parameter{Key: "instance_id", Type: "string", Flags: []string{"instance-id"}, Environment: []string{"HARNESS_MODEL_PROXY_INSTANCE_ID"}, JSONPath: "instance_id", Default: configmeta.Default{Kind: configmeta.DefaultDerived, Display: "generated at startup"}, Description: "Proxy instance identifier."},
	configmeta.Parameter{Key: "api_keys_file", Type: "string", Flags: []string{"api-keys-file"}, JSONPath: "api_keys_file", Default: configmeta.Default{Kind: configmeta.DefaultDerived, Display: "api_keys.json beside the selected config"}, Description: "Accepted API keys file path; this setting is a path and never exposes file contents."},
	configmeta.Parameter{Key: "metrics_enabled", Type: "boolean", Flags: []string{"no-metrics"}, JSONPath: "metrics.enabled", Default: literalDefault(true, "true"), Description: "Whether the Prometheus metrics endpoint is enabled; the command-line flag is inverse."},
	configmeta.Parameter{Key: "metrics_listen", Type: "string", Flags: []string{"metrics-listen"}, JSONPath: "metrics.listen", Default: literalDefault(defaultMetricsListen, defaultMetricsListen), Description: "Prometheus metrics listen address."},
)

func literalDefault(value any, display string) configmeta.Default {
	return configmeta.Default{Kind: configmeta.DefaultLiteral, Value: value, Display: display}
}

// Catalog returns the immutable model-proxy configuration catalog.
func Catalog() configmeta.Catalog { return catalog }

var sourceCLIFlags = []cli.Flag{
	{ID: "config", Names: []string{"config"}, Kind: cli.ValueFlag, ValueName: "path", Description: "config file path"},
}

// SettingCLIFlags projects all catalog-backed command-line settings.
func SettingCLIFlags() []cli.Flag {
	flags := make([]cli.Flag, 0, catalog.Len())
	for _, parameter := range catalog.Parameters() {
		if len(parameter.Flags) == 0 {
			continue
		}
		flag := cli.Flag{
			ID:          parameter.Key,
			Names:       append([]string(nil), parameter.Flags...),
			Kind:        cli.ValueFlag,
			ValueName:   parameter.Type,
			Description: parameter.Description,
			Default:     configmeta.FormatDefault(parameter.Default),
			Environment: append([]string(nil), parameter.Environment...),
		}
		if parameter.Key == "metrics_enabled" {
			flag.Kind = cli.BoolFlag
			flag.ValueName = ""
			flag.Default = "false"
			flag.Description = "Disable the Prometheus metrics endpoint."
		}
		flags = append(flags, flag)
	}
	return flags
}

// CLIFlags returns source-selection flags followed by source-resolved setting flags.
func CLIFlags() []cli.Flag {
	flags := cloneCLIFlags(sourceCLIFlags)
	return append(flags, SettingCLIFlags()...)
}

func cloneCLIFlags(flags []cli.Flag) []cli.Flag {
	out := make([]cli.Flag, len(flags))
	for i, flag := range flags {
		flag.Names = append([]string(nil), flag.Names...)
		flag.Environment = append([]string(nil), flag.Environment...)
		out[i] = flag
	}
	return out
}
