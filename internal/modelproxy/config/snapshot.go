package config

import "harness/internal/configmeta"

// Snapshot projects only safe top-level resolved values. Provider configuration
// files are represented by their path list and are never opened here; API-key
// files are represented by path only.
func Snapshot(result Result) configmeta.Snapshot {
	providers := append([]string(nil), result.Config.ProviderConfigs...)
	if providers == nil {
		providers = []string{}
	}
	values := map[string]any{
		"provider_configs":          providers,
		"default_context_window":    result.Config.DefaultContextWindow,
		"log_level":                 result.Config.LogLevel,
		"log_format":                result.Config.LogFormat,
		"models_dev_cache_ttl":      result.Config.ModelsDevCacheTTL.Duration.String(),
		"provider_models_cache_ttl": result.Config.ProviderModelsCacheTTL.Duration.String(),
		"drain_delay":               result.Config.DrainDelay.Duration.String(),
		"shutdown_timeout":          result.Config.ShutdownTimeout.Duration.String(),
		"instance_id":               result.Config.InstanceID,
		"api_keys_file":             result.Config.APIKeysFile,
		"metrics_enabled":           result.Config.Metrics.Enabled == nil || *result.Config.Metrics.Enabled,
		"metrics_listen":            result.Config.Metrics.Listen,
	}
	return configmeta.NewSnapshot(values, result.Sources)
}
