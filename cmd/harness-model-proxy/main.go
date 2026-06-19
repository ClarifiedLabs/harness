// Command harness-model-proxy owns provider configuration, API keys, model
// catalog metadata, and concrete provider calls for harness.
package main

import (
	"context"
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

	handler, err := server.NewHandler(server.Options{
		ConfigDir: filepath.Dir(path),
		Config:    cfg,
		Getenv:    env.getenv,
		Logger:    logger,
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
	startModelsDevCacheRefresh(ctx, env, filepath.Dir(path), modelsTTL, logger)
	srv := httpserve.New(addr, handler)
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
	fmt.Fprintln(w, "  harness-model-proxy [flags]           serve HTTP")
	fmt.Fprintln(w, "  harness-model-proxy --version         print release version")
	fmt.Fprintln(w, "  harness-model-proxy --setup [--force] configure providers")
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
	fs.String("models-dev-cache-ttl", "", "models.dev cache refresh interval, e.g. 24h; 0 disables periodic refresh")
	fs.String("log-level", logging.LevelInfo, "log level: debug, info, warn, error")
	fs.String("log-format", logging.FormatJSON, "log format: json, text")
	fs.PrintDefaults()
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
