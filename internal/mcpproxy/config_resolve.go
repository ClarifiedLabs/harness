package mcpproxy

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"

	"harness/internal/cli"
	"harness/internal/configmeta"
	"harness/internal/logging"
)

// ConfigResult is a fully resolved MCP proxy configuration and its provenance.
// Explicitness fields preserve runtime policies that cannot be represented by
// effective values alone.
type ConfigResult struct {
	Config  Config
	Path    string
	Sources map[string]configmeta.Source

	ConfigLoaded          bool
	ConfigBypassed        bool
	APIKeysFileExplicit   bool
	MetricsListenExplicit bool
	APIKeysActive         bool
	MetricsActive         bool
	Stdio                 bool
}

// ResolveOptions controls MCP proxy configuration selection and overlays.
type ResolveOptions struct {
	Path         string
	PathExplicit bool
	Flags        cli.Values
	Getenv       func(string) string

	// Getwd supplies the base for relative command-line file paths. It defaults
	// to os.Getwd and is injectable for deterministic tests.
	Getwd func() (string, error)

	// BypassConfig prevents config-file discovery and loading. The tools command
	// uses this when an explicit proxy URL makes configuration irrelevant.
	BypassConfig bool

	// Stdio marks accepted-key and metrics settings operationally inert. Their
	// resolved values and provenance remain available for introspection.
	Stdio bool
}

type configPresence struct {
	mcpServers     bool
	listen         bool
	logFile        bool
	logLevel       bool
	logFormat      bool
	apiKeysFile    bool
	metricsEnabled bool
	metricsListen  bool
}

// ResolveConfig loads the selected file through LoadConfig, applies catalog
// setting flags, records source provenance, and retains runtime explicitness.
func ResolveConfig(options ResolveOptions) (ConfigResult, error) {
	getenv := options.Getenv
	if getenv == nil {
		getenv = os.Getenv
	}

	path, pathExplicit := options.Path, options.PathExplicit
	if occurrence, ok := options.Flags.LastOccurrence("config"); ok {
		path, pathExplicit = occurrence.Value, true
	}
	if !pathExplicit && path == "" {
		path = DefaultConfigPath(getenv)
	}

	loadPath := path
	loaded := false
	if options.BypassConfig {
		loadPath = ""
	} else if !pathExplicit && loadPath != "" {
		if _, err := os.Stat(loadPath); err != nil {
			// Preserve historical discovery behavior: an unusable implicit default
			// is treated as absent; only explicit paths are load errors.
			loadPath = ""
		}
	}

	cfg, err := LoadConfig(loadPath)
	if err != nil {
		return ConfigResult{}, err
	}
	loaded = loadPath != ""

	var present configPresence
	if loaded {
		present, err = readConfigPresence(loadPath)
		if err != nil {
			return ConfigResult{}, err
		}
	}

	result := ConfigResult{
		Config:         cfg,
		Path:           path,
		Sources:        make(map[string]configmeta.Source, parameterCatalog.Len()),
		ConfigLoaded:   loaded,
		ConfigBypassed: options.BypassConfig,
		APIKeysActive:  !options.Stdio,
		Stdio:          options.Stdio,
	}
	for _, parameter := range parameterCatalog.Parameters() {
		result.Sources[parameter.Key] = configmeta.Source{Kind: configmeta.SourceDefault, Name: "built-in"}
	}
	if present.mcpServers {
		result.fileSource("mcp_servers")
	}
	if present.listen {
		result.fileSource("listen")
	}
	if present.logFile {
		result.fileSource("log_file")
	}
	if present.logLevel {
		result.fileSource("log_level")
	}
	if present.logFormat {
		result.fileSource("log_format")
	}
	if present.apiKeysFile {
		result.fileSource("api_keys_file")
	}
	if present.metricsEnabled {
		result.fileSource("metrics_enabled")
	}
	if present.metricsListen {
		result.fileSource("metrics_listen")
	}

	if occurrence, ok := options.Flags.LastOccurrence("listen"); ok && occurrence.Value != "" {
		result.Config.Listen = occurrence.Value
		result.flagSource("listen", occurrence.Name)
	}
	if occurrence, ok := options.Flags.LastOccurrence("log_file"); ok && occurrence.Value != "" {
		value, err := commandLinePath(occurrence.Value, options.Getwd)
		if err != nil {
			return ConfigResult{}, fmt.Errorf("flag --%s for log_file: %w", occurrence.Name, err)
		}
		result.Config.LogFile = value
		result.flagSource("log_file", occurrence.Name)
	}

	level := result.Config.LogLevel
	if occurrence, ok := options.Flags.LastOccurrence("log_level"); ok && occurrence.Value != "" {
		level = occurrence.Value
		result.flagSource("log_level", occurrence.Name)
	}
	result.Config.LogLevel, err = logging.CanonicalLevel(level)
	if err != nil {
		return ConfigResult{}, fmt.Errorf("resolve log_level: %w", err)
	}

	format := result.Config.LogFormat
	if occurrence, ok := options.Flags.LastOccurrence("log_format"); ok && occurrence.Value != "" {
		format = occurrence.Value
		result.flagSource("log_format", occurrence.Name)
	}
	result.Config.LogFormat, err = logging.ParseFormat(format)
	if err != nil {
		return ConfigResult{}, fmt.Errorf("resolve log_format: %w", err)
	}

	configuredKeyFile := result.Config.APIKeysFile
	result.APIKeysFileExplicit = configuredKeyFile != ""
	result.Config.APIKeysFile = ResolveAPIKeysFile(path, configuredKeyFile, "", getenv)
	if !present.apiKeysFile || configuredKeyFile == "" {
		if !present.apiKeysFile {
			result.Sources["api_keys_file"] = configmeta.Source{Kind: configmeta.SourceDerived, Name: "config directory"}
		}
	}
	if occurrence, ok := options.Flags.LastOccurrence("api_keys_file"); ok && occurrence.Value != "" {
		value, err := commandLinePath(occurrence.Value, options.Getwd)
		if err != nil {
			return ConfigResult{}, fmt.Errorf("flag --%s for api_keys_file: %w", occurrence.Name, err)
		}
		result.Config.APIKeysFile = value
		result.APIKeysFileExplicit = true
		result.flagSource("api_keys_file", occurrence.Name)
	}

	metricsEnabled := true
	if result.Config.Metrics.Enabled != nil {
		metricsEnabled = *result.Config.Metrics.Enabled
	}
	if occurrence, ok := options.Flags.LastOccurrence("metrics_enabled"); ok {
		disabled, parseErr := strconv.ParseBool(occurrence.Value)
		if parseErr != nil {
			return ConfigResult{}, fmt.Errorf("flag --%s for metrics_enabled: %w", occurrence.Name, parseErr)
		}
		metricsEnabled = !disabled
		result.flagSource("metrics_enabled", occurrence.Name)
	}
	result.Config.Metrics.Enabled = boolPointer(metricsEnabled)

	metricsListen := result.Config.Metrics.Listen
	if metricsListen == "" {
		metricsListen = DefaultMetricsListen
	}
	result.MetricsListenExplicit = result.Config.Metrics.Listen != ""
	if occurrence, ok := options.Flags.LastOccurrence("metrics_listen"); ok {
		result.MetricsListenExplicit = true
		if occurrence.Value != "" {
			metricsListen = occurrence.Value
			result.flagSource("metrics_listen", occurrence.Name)
		}
	}
	result.Config.Metrics.Listen = metricsListen
	result.MetricsActive = !options.Stdio && metricsEnabled
	return result, nil
}

func (result *ConfigResult) fileSource(key string) {
	result.Sources[key] = configmeta.Source{Kind: configmeta.SourceFile, Name: result.Path}
}

func (result *ConfigResult) flagSource(key, name string) {
	result.Sources[key] = configmeta.Source{Kind: configmeta.SourceFlag, Name: "--" + name}
}

func boolPointer(value bool) *bool { return &value }

func commandLinePath(path string, getwd func() (string, error)) (string, error) {
	if filepath.IsAbs(path) {
		return filepath.Clean(path), nil
	}
	if getwd == nil {
		getwd = os.Getwd
	}
	workingDir, err := getwd()
	if err != nil {
		return "", fmt.Errorf("resolve working directory: %w", err)
	}
	return filepath.Join(workingDir, path), nil
}

func readConfigPresence(path string) (configPresence, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return configPresence{}, fmt.Errorf("mcpproxy: read config presence %s: %w", path, err)
	}
	var root map[string]json.RawMessage
	if err := json.Unmarshal(data, &root); err != nil {
		return configPresence{}, fmt.Errorf("mcpproxy: parse config presence %s: %w", path, err)
	}
	presence := configPresence{}
	presence.mcpServers = hasNonNullValue(root, "mcpServers")
	proxyRaw, ok := root["proxy"]
	if !ok || bytes.Equal(bytes.TrimSpace(proxyRaw), []byte("null")) {
		return presence, nil
	}
	var proxy map[string]json.RawMessage
	if err := json.Unmarshal(proxyRaw, &proxy); err != nil {
		return configPresence{}, fmt.Errorf("mcpproxy: parse proxy presence %s: %w", path, err)
	}
	presence.listen = hasNonEmptyString(proxy, "listen")
	_, presence.logFile = proxy["logFile"] // Empty explicitly selects stderr.
	presence.logLevel = hasNonEmptyString(proxy, "logLevel")
	presence.logFormat = hasNonEmptyString(proxy, "logFormat")
	presence.apiKeysFile = hasNonEmptyString(proxy, "api_keys_file")
	metricsRaw, ok := proxy["metrics"]
	if !ok || bytes.Equal(bytes.TrimSpace(metricsRaw), []byte("null")) {
		return presence, nil
	}
	var metrics map[string]json.RawMessage
	if err := json.Unmarshal(metricsRaw, &metrics); err != nil {
		return configPresence{}, fmt.Errorf("mcpproxy: parse metrics presence %s: %w", path, err)
	}
	presence.metricsEnabled = hasNonNullValue(metrics, "enabled")
	presence.metricsListen = hasNonEmptyString(metrics, "listen")
	return presence, nil
}

func hasNonEmptyString(values map[string]json.RawMessage, key string) bool {
	raw, ok := values[key]
	if !ok {
		return false
	}
	var value string
	return json.Unmarshal(raw, &value) == nil && value != ""
}

func hasNonNullValue(values map[string]json.RawMessage, key string) bool {
	raw, ok := values[key]
	return ok && !bytes.Equal(bytes.TrimSpace(raw), []byte("null"))
}
