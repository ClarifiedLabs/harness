// Command harness-model-proxy owns provider configuration, API keys, model
// catalog metadata, and concrete provider calls for harness.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"harness/internal/apikey"
	"harness/internal/buildinfo"
	"harness/internal/httpserve"
	"harness/internal/logging"
	"harness/internal/modelproxy/server"
	"harness/internal/modelsdev"
	"harness/internal/term"
)

const (
	exitOK        = 0
	exitRuntime   = 1
	exitUsage     = 2
	exitInterrupt = 130
	defaultListen = "127.0.0.1:8765"
)

type environment struct {
	args              []string
	stdin             io.Reader
	stdout            io.Writer
	stderr            io.Writer
	getenv            func(string) string
	sigCh             chan os.Signal
	modelsDevCatalog  func(context.Context) (*modelsdev.Catalog, error)
	terminalRows      func() int
	modelsDevCacheTTL *time.Duration
	now               func() time.Time
}

func signalCancelContext(sigCh <-chan os.Signal) (context.Context, context.CancelFunc, func() bool) {
	ctx, cancel := context.WithCancel(context.Background())
	var interrupted atomic.Bool
	if sigCh != nil {
		go func() {
			select {
			case _, ok := <-sigCh:
				if ok {
					interrupted.Store(true)
				}
				cancel()
			case <-ctx.Done():
			}
		}()
	}
	return ctx, cancel, interrupted.Load
}

func main() {
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	defer signal.Stop(sigCh)

	os.Exit(run(environment{
		args:             os.Args[1:],
		stdin:            os.Stdin,
		stdout:           os.Stdout,
		stderr:           os.Stderr,
		getenv:           os.Getenv,
		sigCh:            sigCh,
		modelsDevCatalog: defaultModelsDevCatalog,
		terminalRows:     defaultTerminalRows,
	}))
}

// run dispatches on the first non-flag argument (the subcommand) and returns
// the process exit code, mirroring cmd/harness-mcp-proxy's dispatch. With no
// arguments it serves HTTP (the implicit default preserved from the previous
// flag-based CLI). Unknown subcommands and -h/--help are handled here so every
// path prints usage to the right stream with the right exit code.
func run(env environment) int {
	args := env.args
	if len(args) == 0 {
		return runServe(env, nil)
	}
	switch args[0] {
	case "-h", "--help", "help":
		usage(env.stdout)
		return exitOK
	case "--version", "version":
		fmt.Fprintln(env.stdout, buildinfo.Line("harness-model-proxy"))
		return exitOK
	case "serve":
		return runServe(env, args[1:])
	case "setup":
		return runSetupCmd(env, args[1:])
	case "refresh-models":
		return runRefreshModelsCmd(env, args[1:])
	case "auth":
		return runAuth(env, args[1:])
	case "generate-api-key":
		return runGenerateAPIKeyCmd(env, args[1:])
	default:
		fmt.Fprintf(env.stderr, "harness-model-proxy: unknown subcommand %q\n", args[0])
		usage(env.stderr)
		return exitUsage
	}
}

// runServe parses serve flags and serves HTTP. args may be nil, in which case it
// serves with the resolved default config and listener (the implicit-default-serve
// behavior: running `harness-model-proxy` with no arguments still serves).
func runServe(env environment, args []string) int {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	configPath := fs.String("config", "", "config file path")
	listen := fs.String("listen", "", "HTTP listen address")
	modelsDevCacheTTL := fs.String("models-dev-cache-ttl", "", "models.dev cache refresh interval, e.g. 24h; 0 disables periodic refresh")
	logLevel := fs.String("log-level", "", "log level: debug, info, warn, error")
	logFormat := fs.String("log-format", "", "log format: json, text")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			usageServe(env.stdout)
			return exitOK
		}
		fmt.Fprintf(env.stderr, "harness-model-proxy: %v\n", err)
		return exitUsage
	}

	path := server.ConfigPath(*configPath, flagWasSet(fs, "config"), env.getenv)
	if path == "" {
		fmt.Fprintln(env.stderr, "harness-model-proxy: no config file found; run harness-model-proxy setup")
		return exitUsage
	}
	cfg, err := server.LoadConfig(path)
	if err != nil {
		fmt.Fprintf(env.stderr, "harness-model-proxy: %v\n", err)
		return exitRuntime
	}
	modelsTTL, err := modelsDevCacheTTLFromConfig(cfg, *modelsDevCacheTTL, flagWasSet(fs, "models-dev-cache-ttl"))
	if err != nil {
		fmt.Fprintf(env.stderr, "harness-model-proxy: %v\n", err)
		return exitUsage
	}
	env.modelsDevCacheTTL = &modelsTTL

	level := cfg.LogLevel
	if *logLevel != "" {
		level = *logLevel
	}
	level, err = logging.CanonicalLevel(level)
	if err != nil {
		fmt.Fprintf(env.stderr, "harness-model-proxy: %v\n", err)
		return exitUsage
	}
	format := cfg.LogFormat
	if *logFormat != "" {
		format = *logFormat
	}
	logger, err := logging.NewProxyLogger(env.stderr, level, format)
	if err != nil {
		fmt.Fprintf(env.stderr, "harness-model-proxy: %v\n", err)
		return exitUsage
	}

	configDir := filepath.Dir(path)
	initialCatalog, initialSourceDate := loadModelsDevCacheForServe(configDir)
	handler, err := server.NewHandler(server.Options{
		ConfigDir:           configDir,
		Config:              cfg,
		Getenv:              env.getenv,
		Logger:              logger,
		PricingMaxAge:       modelsTTL,
		ModelsDevCatalog:    initialCatalog,
		ModelsDevSourceDate: initialSourceDate,
		Warn: func(msg string) {
			logger.Warn(msg)
		},
	})
	if err != nil {
		fmt.Fprintf(env.stderr, "harness-model-proxy: %v\n", err)
		return exitRuntime
	}
	addr := defaultListen
	if *listen != "" {
		addr = *listen
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if env.sigCh != nil {
		go func() {
			select {
			case <-env.sigCh:
				cancel()
			case <-ctx.Done():
			}
		}()
	}
	startModelsDevCacheRefresh(ctx, env, configDir, modelsTTL, logger, func(catalog *modelsdev.Catalog, sourceDate time.Time) {
		handler.UpdateModelsDevCatalog(catalog, sourceDate)
	})
	srv := httpserve.New(addr, cfg.APIKeyStore().Middleware(handler))
	logger.Info("model proxy listening", "addr", addr)
	if err := httpserve.Run(ctx, srv); err != nil {
		fmt.Fprintf(env.stderr, "harness-model-proxy: %v\n", err)
		return exitRuntime
	}
	return exitOK
}

// runSetupCmd parses setup flags and runs the interactive provider-config wizard.
func runSetupCmd(env environment, args []string) int {
	fs := flag.NewFlagSet("setup", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	force := fs.Bool("force", false, "overwrite existing provider files")
	modelsDevCacheTTL := fs.String("models-dev-cache-ttl", "", "models.dev cache refresh interval, e.g. 24h; 0 disables periodic refresh")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			usageSetup(env.stdout)
			return exitOK
		}
		fmt.Fprintf(env.stderr, "harness-model-proxy: %v\n", err)
		return exitUsage
	}
	ttl, err := setupModelsDevCacheTTL(env, *modelsDevCacheTTL, flagWasSet(fs, "models-dev-cache-ttl"))
	if err != nil {
		fmt.Fprintf(env.stderr, "harness-model-proxy: %v\n", err)
		return exitUsage
	}
	env.modelsDevCacheTTL = &ttl
	ctx, cancel, interrupted := signalCancelContext(env.sigCh)
	defer cancel()
	if err := runSetup(ctx, env, *force); err != nil {
		if interrupted() || errors.Is(err, context.Canceled) {
			return exitInterrupt
		}
		fmt.Fprintf(env.stderr, "harness-model-proxy: setup: %v\n", err)
		return exitUsage
	}
	return exitOK
}

// runRefreshModelsCmd parses refresh-models flags and re-syncs configured
// provider config files from the models.dev catalog.
func runRefreshModelsCmd(env environment, args []string) int {
	fs := flag.NewFlagSet("refresh-models", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	configPath := fs.String("config", "", "config file path")
	modelsDevCacheTTL := fs.String("models-dev-cache-ttl", "", "models.dev cache refresh interval, e.g. 24h; 0 disables periodic refresh")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			usageRefreshModels(env.stdout)
			return exitOK
		}
		fmt.Fprintf(env.stderr, "harness-model-proxy: %v\n", err)
		return exitUsage
	}
	path := server.ConfigPath(*configPath, flagWasSet(fs, "config"), env.getenv)
	if path == "" {
		fmt.Fprintln(env.stderr, "harness-model-proxy: no config file found; run harness-model-proxy setup")
		return exitUsage
	}
	ttl, err := configuredModelsDevCacheTTL(path, env, *modelsDevCacheTTL, flagWasSet(fs, "models-dev-cache-ttl"))
	if err != nil {
		fmt.Fprintf(env.stderr, "harness-model-proxy: %v\n", err)
		return exitUsage
	}
	env.modelsDevCacheTTL = &ttl
	ctx, cancel, interrupted := signalCancelContext(env.sigCh)
	defer cancel()
	if err := runRefreshModels(ctx, env, path); err != nil {
		if interrupted() || errors.Is(err, context.Canceled) {
			return exitInterrupt
		}
		fmt.Fprintf(env.stderr, "harness-model-proxy: refresh-models: %v\n", err)
		return exitUsage
	}
	return exitOK
}

// runGenerateAPIKeyCmd parses generate-api-key flags, generates a new API key,
// and adds it to the config, creating the config at the default path if none
// exists yet.
func runGenerateAPIKeyCmd(env environment, args []string) int {
	fs := flag.NewFlagSet("generate-api-key", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	configPath := fs.String("config", "", "config file path")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			usageGenerateAPIKey(env.stdout)
			return exitOK
		}
		fmt.Fprintf(env.stderr, "harness-model-proxy: %v\n", err)
		return exitUsage
	}
	if fs.NArg() != 1 {
		fmt.Fprintln(env.stderr, "harness-model-proxy: generate-api-key requires exactly one name")
		return exitUsage
	}
	return runGenerateAPIKey(env, *configPath, fs.Arg(0))
}

func usage(w io.Writer) {
	fmt.Fprint(w, `harness-model-proxy - provider and model proxy for harness

Usage:
  harness-model-proxy serve             [-config path] [-listen addr] [-models-dev-cache-ttl d] [-log-level level] [-log-format format]
  harness-model-proxy setup             [-force] [-models-dev-cache-ttl d]
  harness-model-proxy refresh-models    [-config path] [-models-dev-cache-ttl d]
  harness-model-proxy auth              <login|logout|status> [-config path] <provider>
  harness-model-proxy generate-api-key  [-config path] <name>
  harness-model-proxy version
  harness-model-proxy --version

With no arguments, harness-model-proxy serves HTTP (the default action).

Subcommands:
  serve             Load config and serve the HTTP model proxy (default).
  setup             Create or update proxy and provider config interactively.
  refresh-models    Fetch models.dev and update configured provider model metadata.
  auth              Login, logout, or inspect OAuth tokens for a configured provider.
  generate-api-key  Generate a new API key with the given name and add it to config.
  version           Print the release version.

serve flags:
  -config path            config file path
  -listen addr            HTTP listen address (default: `+defaultListen+`)
  -models-dev-cache-ttl d models.dev cache refresh interval, e.g. 24h; 0 disables periodic refresh
  -log-level level        debug|info|warn|error (overrides config)
  -log-format format      json|text (overrides config)

setup flags:
  -force                  overwrite existing provider files
  -models-dev-cache-ttl d models.dev cache refresh interval

refresh-models flags:
  -config path            config file path
  -models-dev-cache-ttl d models.dev cache refresh interval

generate-api-key flags:
  -config path            config file path
`)
}

// usageServe prints serve-specific help.
func usageServe(w io.Writer) {
	fmt.Fprint(w, `harness-model-proxy serve - load config and serve the HTTP model proxy

Usage:
  harness-model-proxy serve [-config path] [-listen addr] [-models-dev-cache-ttl d] [-log-level level] [-log-format format]

With no arguments, harness-model-proxy serves HTTP (the default action).

Flags:
  -config path            config file path
  -listen addr            HTTP listen address (default: `+defaultListen+`)
  -models-dev-cache-ttl d models.dev cache refresh interval, e.g. 24h; 0 disables periodic refresh
  -log-level level        debug|info|warn|error (overrides config)
  -log-format format      json|text (overrides config)
`)
}

// usageSetup prints setup-specific help.
func usageSetup(w io.Writer) {
	fmt.Fprint(w, `harness-model-proxy setup - create or update proxy and provider config interactively

Usage:
  harness-model-proxy setup [-force] [-models-dev-cache-ttl d]

Runs the models.dev-backed provider/model picker and writes proxy and provider
config files in the default config directory.

Flags:
  -force                  overwrite existing provider files
  -models-dev-cache-ttl d models.dev cache refresh interval
`)
}

// usageRefreshModels prints refresh-models-specific help.
func usageRefreshModels(w io.Writer) {
	fmt.Fprint(w, `harness-model-proxy refresh-models - fetch models.dev and update configured provider model metadata

Usage:
  harness-model-proxy refresh-models [-config path] [-models-dev-cache-ttl d]

Flags:
  -config path            config file path
  -models-dev-cache-ttl d models.dev cache refresh interval
`)
}

// usageGenerateAPIKey prints generate-api-key-specific help.
func usageGenerateAPIKey(w io.Writer) {
	fmt.Fprint(w, `harness-model-proxy generate-api-key - generate and store a new API key

Usage:
  harness-model-proxy generate-api-key [-config path] <name>

Creates config at the default path if none exists yet.

Flags:
  -config path            config file path
`)
}

func runGenerateAPIKey(env environment, argsConfigPath, name string) int {
	path := server.ConfigPath(argsConfigPath, argsConfigPath != "", env.getenv)
	if path == "" {
		// No config file exists yet; create one at the default path so a key can
		// be generated on a fresh install (mirrors harness-mcp-proxy).
		path = filepath.Join(defaultConfigDir(env.getenv), "config.json")
	}
	// Load existing api_keys via the typed config and add the new entry, then
	// write back only the api_keys field in the raw JSON. This preserves all other
	// config keys exactly and avoids round-tripping custom types such as Duration.
	cfg, err := server.LoadConfig(path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		fmt.Fprintf(env.stderr, "harness-model-proxy: %v\n", err)
		return exitRuntime
	}
	plaintext, err := apikey.Generate(name, apikey.ModelProxyPrefix)
	if err != nil {
		fmt.Fprintf(env.stderr, "harness-model-proxy: %v\n", err)
		return exitUsage
	}
	store := cfg.APIKeyStore()
	now := env.now
	if now == nil {
		now = time.Now
	}
	store.Add(name, plaintext, now())
	if err := updateConfigAPIKeys(path, store.Entries); err != nil {
		fmt.Fprintf(env.stderr, "harness-model-proxy: %v\n", err)
		return exitRuntime
	}
	fmt.Fprintln(env.stdout, plaintext)
	return exitOK
}

// updateConfigAPIKeys writes entries into the api_keys field of the JSON file at
// path, preserving every other top-level key exactly as it was written. It
// creates parent directories as needed and writes atomically (temp file + rename).
func updateConfigAPIKeys(path string, entries []apikey.Entry) error {
	raw := map[string]json.RawMessage{}
	data, err := os.ReadFile(path)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("read config: %w", err)
		}
	} else if len(strings.TrimSpace(string(data))) > 0 {
		if err := json.Unmarshal(data, &raw); err != nil {
			return fmt.Errorf("parse config: %w", err)
		}
	}
	apiKeysJSON, err := json.Marshal(entries)
	if err != nil {
		return fmt.Errorf("marshal api_keys: %w", err)
	}
	raw["api_keys"] = apiKeysJSON
	data, err = json.MarshalIndent(raw, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal config: %w", err)
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("create config dir: %w", err)
	}
	tmp, err := os.CreateTemp(dir, "config.json.*")
	if err != nil {
		return fmt.Errorf("create temp config: %w", err)
	}
	tmpName := tmp.Name()
	cleanup := true
	defer func() {
		if cleanup {
			_ = os.Remove(tmpName)
		}
	}()
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("write temp config: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close temp config: %w", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("rename temp config: %w", err)
	}
	cleanup = false
	return nil
}

func setupModelsDevCacheTTL(env environment, flagValue string, flagSet bool) (time.Duration, error) {
	configPath := filepath.Join(defaultConfigDir(env.getenv), "config.json")
	if _, err := os.Stat(configPath); err == nil {
		return configuredModelsDevCacheTTL(configPath, env, flagValue, flagSet)
	} else if err != nil && !errors.Is(err, os.ErrNotExist) {
		return 0, err
	}
	return modelsDevCacheTTLFromConfig(server.Config{}, flagValue, flagSet)
}

func configuredModelsDevCacheTTL(path string, env environment, flagValue string, flagSet bool) (time.Duration, error) {
	if path == "" {
		return modelsDevCacheTTLFromConfig(server.Config{}, flagValue, flagSet)
	}
	cfg, err := server.LoadConfig(path)
	if err != nil {
		return 0, err
	}
	return modelsDevCacheTTLFromConfig(cfg, flagValue, flagSet)
}

func modelsDevCacheTTLFromConfig(cfg server.Config, flagValue string, flagSet bool) (time.Duration, error) {
	ttl := defaultModelsDevTTL
	if cfg.ModelsDevCacheTTL.Set {
		ttl = cfg.ModelsDevCacheTTL.Duration
	}
	if flagSet {
		parsed, err := parseModelsDevCacheTTLFlag(flagValue)
		if err != nil {
			return 0, err
		}
		ttl = parsed
	}
	return ttl, nil
}

func parseModelsDevCacheTTLFlag(value string) (time.Duration, error) {
	value = strings.TrimSpace(value)
	if value == "0" {
		return 0, nil
	}
	ttl, err := time.ParseDuration(value)
	if err != nil {
		return 0, fmt.Errorf("invalid -models-dev-cache-ttl %q: %w", value, err)
	}
	if ttl < 0 {
		return 0, fmt.Errorf("invalid -models-dev-cache-ttl %q: duration must be non-negative", value)
	}
	return ttl, nil
}

func flagWasSet(fs *flag.FlagSet, name string) bool {
	set := false
	fs.Visit(func(f *flag.Flag) {
		if f.Name == name {
			set = true
		}
	})
	return set
}

func defaultConfigDir(getenv func(string) string) string {
	return server.DefaultConfigDir(getenv)
}

func defaultModelsDevCatalog(ctx context.Context) (*modelsdev.Catalog, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	return modelsdev.Fetch(ctx, http.DefaultClient, modelsdev.DefaultURL)
}

func defaultTerminalRows() int {
	rows, _, ok := term.Size()
	if !ok {
		return 0
	}
	return rows
}
