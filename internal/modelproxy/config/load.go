package config

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"harness/internal/cli"
	"harness/internal/configmeta"
	"harness/internal/llm"
	"harness/internal/logging"
	"harness/internal/modelproxy/server"
)

const (
	drainDelayEnvironment      = "HARNESS_MODEL_PROXY_DRAIN_DELAY"
	shutdownTimeoutEnvironment = "HARNESS_MODEL_PROXY_SHUTDOWN_TIMEOUT"
	instanceIDEnvironment      = "HARNESS_MODEL_PROXY_INSTANCE_ID"
)

var instanceIDPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)

// Result is the effective model-proxy configuration and source provenance.
// Explicitness fields retain operational distinctions not representable by
// server.Config's effective values.
type Result struct {
	Config                server.Config
	Path                  string
	PathExplicit          bool
	Sources               map[string]configmeta.Source
	APIKeysFileExplicit   bool
	MetricsListenExplicit bool
	InstanceIDExplicit    bool
}

// FileError reports failure to read or structurally decode the selected config
// file. Commands use this typed boundary to preserve startup's runtime-error
// classification without inspecting error strings.
type FileError struct {
	Path string
	Err  error
}

func (e *FileError) Error() string { return e.Err.Error() }
func (e *FileError) Unwrap() error { return e.Err }

func configFileError(path string, err error) error {
	return &FileError{Path: path, Err: err}
}

// LoadOptions supplies source selection and already-parsed setting flags.
type LoadOptions struct {
	Path         string
	PathExplicit bool
	Getenv       func(string) string
	Flags        cli.Values

	// DeriveInstanceID overrides startup instance generation for deterministic
	// callers and tests. Nil generates the same 32-character lowercase hex shape
	// as the server.
	DeriveInstanceID func() (string, error)
}

// Load resolves model-proxy settings with their package-specific precedence and
// presence semantics. An empty selected path yields defaults; PathExplicit lets
// callers distinguish explicit -config= from failed implicit discovery.
func Load(options LoadOptions) (Result, error) {
	getenv := options.Getenv
	if getenv == nil {
		getenv = os.Getenv
	}
	path, pathExplicit := options.Path, options.PathExplicit
	if value, ok := options.Flags.Last("config"); ok {
		path, pathExplicit = value, true
	}
	path = configPath(path, pathExplicit, getenv)

	result := Result{
		Path:         path,
		PathExplicit: pathExplicit,
		Sources:      make(map[string]configmeta.Source, catalog.Len()),
	}
	file := server.Config{}
	present := map[string]json.RawMessage{}
	metricsPresent := map[string]json.RawMessage{}
	if path != "" {
		data, err := os.ReadFile(path)
		if err != nil {
			return Result{}, configFileError(path, err)
		}
		file, err = server.DecodeConfig(data, path)
		if err != nil {
			return Result{}, configFileError(path, err)
		}
		if err := json.Unmarshal(data, &present); err != nil {
			return Result{}, configFileError(path, err)
		}
		if err := validateIntegerDurationRanges(present); err != nil {
			return Result{}, configFileError(path, err)
		}
		if raw, ok := present["metrics"]; ok && string(raw) != "null" {
			if err := json.Unmarshal(raw, &metricsPresent); err != nil {
				return Result{}, configFileError(path, err)
			}
		}
	}

	resolveFileSettings(&result, file, present)
	if err := resolveStringSettings(&result, file, present, options.Flags); err != nil {
		return Result{}, err
	}
	if err := resolveDurations(&result, file, options.Flags, getenv); err != nil {
		return Result{}, err
	}
	if err := resolveInstance(&result, file, present, options.Flags, getenv, options.DeriveInstanceID); err != nil {
		return Result{}, err
	}
	resolveAPIKeysFile(&result, file, options.Flags)
	if err := resolveMetrics(&result, file, metricsPresent, options.Flags); err != nil {
		return Result{}, err
	}
	return result, nil
}

// CheckLocalReferences decodes configured provider files without contacting any
// provider or reading managed key/token files. Missing and malformed provider
// files retain serving-time warning-and-skip behavior; if none remain, validation
// fails as server startup would.
func CheckLocalReferences(result Result, getenv func(string) string, warn func(string)) error {
	if result.Path == "" {
		return nil
	}
	return server.ValidateConfigReferences(filepath.Dir(result.Path), result.Config, getenv, warn)
}

func validateIntegerDurationRanges(present map[string]json.RawMessage) error {
	for _, key := range []string{"models_dev_cache_ttl", "drain_delay", "shutdown_timeout"} {
		raw, ok := present[key]
		if !ok {
			continue
		}
		var number json.Number
		if err := json.Unmarshal(raw, &number); err != nil {
			continue // Strings and invalid unions are handled by server.DecodeConfig.
		}
		seconds, err := number.Int64()
		if err == nil && seconds > math.MaxInt64/int64(time.Second) {
			return fmt.Errorf("config duration %s overflows time.Duration", key)
		}
	}
	return nil
}

func resolveFileSettings(result *Result, file server.Config, present map[string]json.RawMessage) {
	result.Config.ProviderConfigs = []string{}
	result.Sources["provider_configs"] = defaultSource()
	if _, ok := present["provider_configs"]; ok {
		result.Config.ProviderConfigs = append([]string(nil), file.ProviderConfigs...)
		if result.Config.ProviderConfigs == nil {
			result.Config.ProviderConfigs = []string{}
		}
		result.Sources["provider_configs"] = fileSource(result.Path)
	}

	result.Config.DefaultContextWindow = llm.DefaultContextWindow
	result.Sources["default_context_window"] = defaultSource()
	if _, ok := present["default_context_window"]; ok && file.DefaultContextWindow > 0 {
		result.Config.DefaultContextWindow = file.DefaultContextWindow
		result.Sources["default_context_window"] = fileSource(result.Path)
	}
}

func resolveStringSettings(result *Result, file server.Config, present map[string]json.RawMessage, flags cli.Values) error {
	level, levelSource := "info", defaultSource()
	if _, ok := present["log_level"]; ok {
		level, levelSource = file.LogLevel, fileSource(result.Path)
	}
	if value, ok := flags.Last("log_level"); ok && value != "" {
		level, levelSource = value, flagSource("log-level")
	}
	canonicalLevel, err := logging.CanonicalLevel(level)
	if err != nil {
		return fmt.Errorf("log_level: %w", err)
	}
	result.Config.LogLevel = canonicalLevel
	result.Sources["log_level"] = levelSource

	format, formatSource := "json", defaultSource()
	if _, ok := present["log_format"]; ok {
		format, formatSource = file.LogFormat, fileSource(result.Path)
	}
	if value, ok := flags.Last("log_format"); ok && value != "" {
		format, formatSource = value, flagSource("log-format")
	}
	canonicalFormat, err := logging.ParseFormat(format)
	if err != nil {
		return fmt.Errorf("log_format: %w", err)
	}
	result.Config.LogFormat = canonicalFormat
	result.Sources["log_format"] = formatSource
	return nil
}

func resolveDurations(result *Result, file server.Config, flags cli.Values, getenv func(string) string) error {
	modelsTTL, source, err := resolveDuration("models-dev-cache-ttl", "models_dev_cache_ttl", flags, "", file.ModelsDevCacheTTL, 24*time.Hour)
	if err != nil {
		return err
	}
	result.Config.ModelsDevCacheTTL = server.Duration{Duration: modelsTTL, Set: true}
	result.Sources["models_dev_cache_ttl"] = sourceForPath(source, result.Path)

	drain, source, err := resolveDuration("drain-delay", "drain_delay", flags, getenv(drainDelayEnvironment), file.DrainDelay, 5*time.Second)
	if err != nil {
		return err
	}
	result.Config.DrainDelay = server.Duration{Duration: drain, Set: true}
	result.Sources["drain_delay"] = sourceForPath(source, result.Path)

	shutdown, source, err := resolveDuration("shutdown-timeout", "shutdown_timeout", flags, getenv(shutdownTimeoutEnvironment), file.ShutdownTimeout, 5*time.Minute)
	if err != nil {
		return err
	}
	result.Config.ShutdownTimeout = server.Duration{Duration: shutdown, Set: true}
	result.Sources["shutdown_timeout"] = sourceForPath(source, result.Path)
	return nil
}

type durationSource struct {
	kind configmeta.SourceKind
	name string
}

func resolveDuration(flagName, key string, flags cli.Values, environmentValue string, file server.Duration, fallback time.Duration) (time.Duration, durationSource, error) {
	if value, ok := flags.Last(key); ok {
		parsed, err := parseDuration(flagName, value)
		return parsed, durationSource{kind: configmeta.SourceFlag, name: "--" + flagName}, err
	}
	if strings.TrimSpace(environmentValue) != "" {
		parsed, err := parseDuration(flagName, environmentValue)
		environmentName := drainDelayEnvironment
		if key == "shutdown_timeout" {
			environmentName = shutdownTimeoutEnvironment
		}
		return parsed, durationSource{kind: configmeta.SourceEnvironment, name: environmentName}, err
	}
	if file.Set {
		if file.Duration < 0 {
			return 0, durationSource{}, fmt.Errorf("config duration %s must be non-negative", key)
		}
		return file.Duration, durationSource{kind: configmeta.SourceFile}, nil
	}
	return fallback, durationSource{kind: configmeta.SourceDefault, name: "built-in"}, nil
}

func parseDuration(flagName, value string) (time.Duration, error) {
	trimmed := strings.TrimSpace(value)
	if trimmed == "0" {
		return 0, nil
	}
	parsed, err := time.ParseDuration(trimmed)
	if err != nil {
		return 0, fmt.Errorf("invalid -%s %q: %w", flagName, trimmed, err)
	}
	if parsed < 0 {
		return 0, fmt.Errorf("invalid -%s %q: duration must be non-negative", flagName, trimmed)
	}
	return parsed, nil
}

func sourceForPath(source durationSource, path string) configmeta.Source {
	if source.kind == configmeta.SourceFile {
		return fileSource(path)
	}
	return configmeta.Source{Kind: source.kind, Name: source.name}
}

func resolveInstance(result *Result, file server.Config, present map[string]json.RawMessage, flags cli.Values, getenv func(string) string, derive func() (string, error)) error {
	value := ""
	source := configmeta.Source{Kind: configmeta.SourceDerived, Name: "generated instance ID"}
	if _, ok := present["instance_id"]; ok {
		value, source = file.InstanceID, fileSource(result.Path)
		result.InstanceIDExplicit = true
	}
	if environmentValue := strings.TrimSpace(getenv(instanceIDEnvironment)); environmentValue != "" {
		value = environmentValue
		source = configmeta.Source{Kind: configmeta.SourceEnvironment, Name: instanceIDEnvironment}
		result.InstanceIDExplicit = true
	}
	if flagValue, ok := flags.Last("instance_id"); ok {
		value = strings.TrimSpace(flagValue)
		source = flagSource("instance-id")
		result.InstanceIDExplicit = true
	}
	value = strings.TrimSpace(value)
	if value == "" {
		if derive == nil {
			derive = generateInstanceID
		}
		var err error
		value, err = derive()
		if err != nil {
			return fmt.Errorf("model proxy: generate instance id: %w", err)
		}
		value = strings.TrimSpace(value)
	}
	if !instanceIDPattern.MatchString(value) {
		return fmt.Errorf("model proxy: invalid instance id %q (want [A-Za-z0-9][A-Za-z0-9._-]{0,127})", value)
	}
	result.Config.InstanceID = value
	result.Sources["instance_id"] = source
	return nil
}

func generateInstanceID() (string, error) {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(raw[:]), nil
}

func resolveAPIKeysFile(result *Result, file server.Config, flags cli.Values) {
	flagValue, flagSet := flags.Last("api_keys_file")
	if flagSet && flagValue != "" {
		result.Config.APIKeysFile = server.ResolveAPIKeysFile(result.Path, file.APIKeysFile, flagValue)
		result.Sources["api_keys_file"] = flagSource("api-keys-file")
		result.APIKeysFileExplicit = true
		return
	}
	result.Config.APIKeysFile = server.ResolveAPIKeysFile(result.Path, file.APIKeysFile, "")
	if file.APIKeysFile != "" {
		result.Sources["api_keys_file"] = fileSource(result.Path)
		result.APIKeysFileExplicit = true
		return
	}
	result.Sources["api_keys_file"] = configmeta.Source{Kind: configmeta.SourceDerived, Name: "config directory"}
}

func resolveMetrics(result *Result, file server.Config, present map[string]json.RawMessage, flags cli.Values) error {
	enabled := true
	enabledSource := defaultSource()
	if file.Metrics.Enabled != nil {
		enabled = *file.Metrics.Enabled
		enabledSource = fileSource(result.Path)
	}
	if value, ok := flags.Last("metrics_enabled"); ok {
		disable, err := strconv.ParseBool(value)
		if err != nil {
			return fmt.Errorf("invalid -no-metrics %q: %w", value, err)
		}
		enabled = !disable
		enabledSource = flagSource("no-metrics")
	}
	result.Config.Metrics.Enabled = boolPointer(enabled)
	result.Sources["metrics_enabled"] = enabledSource

	listen := defaultMetricsListen
	listenSource := defaultSource()
	if file.Metrics.Listen != "" {
		listen = file.Metrics.Listen
		listenSource = fileSource(result.Path)
		result.MetricsListenExplicit = true
	}
	if value, ok := flags.Last("metrics_listen"); ok {
		result.MetricsListenExplicit = true
		if value != "" {
			listen = value
			listenSource = flagSource("metrics-listen")
		}
	}
	_ = present // nested presence is intentionally value-sensitive, matching metrics.Resolve.
	result.Config.Metrics.Listen = listen
	result.Sources["metrics_listen"] = listenSource
	return nil
}

func configPath(path string, explicit bool, getenv func(string) string) string {
	if explicit {
		return path
	}
	var directory string
	if home := getenv("HOME"); home != "" {
		directory = filepath.Join(home, ".config", "harness-model-proxy")
	} else if temporary := getenv("TMPDIR"); temporary != "" {
		directory = filepath.Join(temporary, "harness-model-proxy-config")
	} else {
		directory = filepath.Join(os.TempDir(), "harness-model-proxy-config")
	}
	candidate := filepath.Join(directory, "config.json")
	if _, err := os.Stat(candidate); err == nil {
		return candidate
	}
	return ""
}

func boolPointer(value bool) *bool { return &value }

func defaultSource() configmeta.Source {
	return configmeta.Source{Kind: configmeta.SourceDefault, Name: "built-in"}
}
func fileSource(path string) configmeta.Source {
	return configmeta.Source{Kind: configmeta.SourceFile, Name: path}
}
func flagSource(name string) configmeta.Source {
	return configmeta.Source{Kind: configmeta.SourceFlag, Name: "--" + name}
}
