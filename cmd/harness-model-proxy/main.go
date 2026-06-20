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

func run(env environment) int {
	if len(env.args) > 0 && env.args[0] == "--version" {
		fmt.Fprintln(env.stdout, buildinfo.Line("harness-model-proxy"))
		return exitOK
	}
	if len(env.args) > 0 && env.args[0] == "auth" {
		return runAuth(env, env.args[1:])
	}

	fs := flag.NewFlagSet("harness-model-proxy", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	configPath := fs.String("config", "", "config file path")
	listen := fs.String("listen", "", "HTTP listen address")
	setup := fs.Bool("setup", false, "create or update proxy config")
	force := fs.Bool("force", false, "with --setup, overwrite existing provider files")
	refreshModels := fs.Bool("refresh-models", false, "fetch models.dev and update configured provider model metadata")
	generateAPIKey := fs.String("generate-api-key", "", "generate a new API key with the given name and add it to the config")
	modelsDevCacheTTL := fs.String("models-dev-cache-ttl", "", "models.dev cache refresh interval, e.g. 24h; 0 disables periodic refresh")
	logLevel := fs.String("log-level", "", "log level: debug, info, warn, error")
	logFormat := fs.String("log-format", "", "log format: json, text")
	if err := fs.Parse(env.args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			usage(env.stdout)
			return exitOK
		}
		fmt.Fprintf(env.stderr, "harness-model-proxy: %v\n", err)
		return exitUsage
	}
	if *setup {
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
	if *generateAPIKey != "" {
		return runGenerateAPIKey(env, *configPath, *generateAPIKey)
	}

	path := server.ConfigPath(*configPath, flagWasSet(fs, "config"), env.getenv)
	if *refreshModels {
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
	if path == "" {
		fmt.Fprintln(env.stderr, "harness-model-proxy: no config file found; run harness-model-proxy --setup")
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

func usage(w io.Writer) {
	fmt.Fprintln(w, "harness-model-proxy — provider and model proxy for harness.")
	fmt.Fprintln(w)
	fmt.Fprintln(w, "Usage:")
	fmt.Fprintln(w, "  harness-model-proxy [flags]                    serve HTTP")
	fmt.Fprintln(w, "  harness-model-proxy --version                  print release version")
	fmt.Fprintln(w, "  harness-model-proxy --setup [--force]          configure providers")
	fmt.Fprintln(w, "  harness-model-proxy --generate-api-key <name>  generate and store a new API key")
	fmt.Fprintln(w, "  harness-model-proxy auth <login|logout|status> [flags] <provider>")
	fmt.Fprintln(w)
	fmt.Fprintln(w, "Flags:")
	fs := flag.NewFlagSet("harness-model-proxy", flag.ContinueOnError)
	fs.SetOutput(w)
	fs.String("config", "", "config file path")
	fs.String("listen", "", "HTTP listen address")
	fs.Bool("setup", false, "create or update proxy config")
	fs.Bool("force", false, "with --setup, overwrite existing provider files")
	fs.Bool("refresh-models", false, "fetch models.dev and update configured provider model metadata")
	fs.String("generate-api-key", "", "generate a new API key with the given name and add it to the config")
	fs.String("models-dev-cache-ttl", "", "models.dev cache refresh interval, e.g. 24h; 0 disables periodic refresh")
	fs.String("log-level", logging.LevelInfo, "log level: debug, info, warn, error")
	fs.String("log-format", logging.FormatJSON, "log format: json, text")
	fs.PrintDefaults()
}

func runGenerateAPIKey(env environment, argsConfigPath, name string) int {
	path := server.ConfigPath(argsConfigPath, argsConfigPath != "", env.getenv)
	if path == "" {
		fmt.Fprintln(env.stderr, "harness-model-proxy: no config file found; run harness-model-proxy --setup")
		return exitUsage
	}
	// Load existing api_keys via the typed config and add the new entry, then
	// write back only the api_keys field in the raw JSON. This preserves all other
	// config keys exactly and avoids round-tripping custom types such as Duration.
	cfg, err := server.LoadConfig(path)
	if err != nil {
		fmt.Fprintf(env.stderr, "harness-model-proxy: %v\n", err)
		return exitRuntime
	}
	plaintext, err := apikey.Generate(name, apikey.ModelProxyPrefix)
	if err != nil {
		fmt.Fprintf(env.stderr, "harness-model-proxy: %v\n", err)
		return exitUsage
	}
	store := cfg.APIKeyStore()
	store.Add(name, plaintext, env.now())
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
