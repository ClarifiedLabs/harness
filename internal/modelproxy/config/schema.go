package config

import (
	"encoding/json"
	"fmt"
	"harness/internal/metrics"
	"os"
	"path/filepath"
)

type MetricsConfig = metrics.Config
type Config struct {
	ProviderConfigs          []string      `json:"provider_configs"`
	DefaultContextWindow     int           `json:"default_context_window"`
	LogLevel                 string        `json:"log_level,omitempty"`
	LogFormat                string        `json:"log_format,omitempty"`
	ModelsDevCacheTTL        Duration      `json:"models_dev_cache_ttl,omitempty"`
	ProviderModelsCacheTTL   Duration      `json:"provider_models_cache_ttl,omitempty"`
	SubscriptionPollInterval Duration      `json:"subscription_poll_interval,omitempty"`
	DrainDelay               Duration      `json:"drain_delay,omitempty"`
	ShutdownTimeout          Duration      `json:"shutdown_timeout,omitempty"`
	InstanceID               string        `json:"instance_id,omitempty"`
	APIKeysFile              string        `json:"api_keys_file,omitempty"`
	Metrics                  MetricsConfig `json:"metrics,omitempty"`
}

func LoadConfig(path string) (Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Config{}, err
	}
	return DecodeConfig(data, path)
}

// DecodeConfig decodes one model-proxy config byte snapshot. path is used only
// to render config-relative migration guidance. LoadConfig remains the ordinary
// file-loading entry point.
func DecodeConfig(data []byte, path string) (Config, error) {
	var cfg Config
	if err := json.Unmarshal(data, &cfg); err != nil {
		return Config{}, err
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return Config{}, err
	}
	if _, ok := raw["api_keys"]; ok {
		return Config{}, fmt.Errorf("api_keys is no longer supported in config; move entries to %s under {\"api_keys\": [...]} and remove api_keys from config", ResolveAPIKeysFile(path, cfg.APIKeysFile, ""))
	}
	if _, ok := raw["cost_budget"]; ok {
		return Config{}, fmt.Errorf("cost_budget is no longer supported in model proxy config; generate model-proxy API keys with -budget-usd and -budget-period instead")
	}
	return cfg, nil
}

// ResolveAPIKeysFile applies flag > config > conventional-default precedence for
// the model proxy's dedicated accepted-key file. Relative config values resolve
// relative to the proxy config directory; a relative flag value is left relative
// to the caller's working directory.
func ResolveAPIKeysFile(configPath, configValue, flagValue string) string {
	if flagValue != "" {
		return flagValue
	}
	configDir := filepath.Dir(configPath)
	if configValue != "" {
		if filepath.IsAbs(configValue) {
			return configValue
		}
		return filepath.Join(configDir, configValue)
	}
	return filepath.Join(configDir, "api_keys.json")
}

func ConfigPath(argsPath string, explicit bool, getenv func(string) string) string {
	if explicit {
		return argsPath
	}
	def := filepath.Join(DefaultConfigDir(getenv), "config.json")
	if _, err := os.Stat(def); err == nil {
		return def
	}
	return ""
}

func DefaultConfigDir(getenv func(string) string) string {
	if getenv == nil {
		getenv = os.Getenv
	}
	if home := getenv("HOME"); home != "" {
		return filepath.Join(home, ".config", "harness-model-proxy")
	}
	return filepath.Join(os.TempDir(), "harness-model-proxy-config")
}
