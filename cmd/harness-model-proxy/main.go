// Command harness-model-proxy owns provider configuration, API keys, model
// catalog metadata, and concrete provider calls for harness.
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"harness/internal/apikey"
	"harness/internal/buildinfo"
	"harness/internal/cli"
	"harness/internal/httpserve"
	"harness/internal/llm"
	"harness/internal/logging"
	"harness/internal/metrics"
	"harness/internal/modelcatalog"
	modelconfig "harness/internal/modelproxy/config"
	"harness/internal/modelproxy/server"
	"harness/internal/term"
)

const (
	exitOK        = 0
	exitRuntime   = 1
	exitUsage     = 2
	exitInterrupt = 130
	defaultListen = "127.0.0.1:8765"
	// defaultMetricsListen is the separate, unauthenticated port for the
	// Prometheus /metrics endpoint. It stays off the harness CLI's API-key path.
	defaultMetricsListen = "127.0.0.1:9090"
	defaultDrainDelay    = 5 * time.Second
	defaultShutdownTime  = 5 * time.Minute
)

type environment struct {
	args                   []string
	stdin                  io.Reader
	stdout                 io.Writer
	stderr                 io.Writer
	getenv                 func(string) string
	sigCh                  chan os.Signal
	modelsDevCatalog       func(context.Context) (*modelcatalog.Catalog, error)
	codexModelsData        func(context.Context) ([]byte, error)
	terminalRows           func() int
	modelsDevCacheTTL      *time.Duration
	providerModelsCacheTTL *time.Duration
	providerModelsClient   *http.Client
	providerModelsTimeout  *time.Duration
	providerModelsTicks    <-chan time.Time
	now                    func() time.Time
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

// runServe consumes the source-resolved model-proxy configuration and serves HTTP.
func runServe(env environment, invocation cli.Invocation) int {
	resolved, err := loadModelConfig(env, invocation)
	if err != nil {
		fmt.Fprintf(env.stderr, "harness-model-proxy: %v\n", err)
		var fileErr *modelconfig.FileError
		if errors.As(err, &fileErr) {
			return exitRuntime
		}
		return exitUsage
	}
	if resolved.Path == "" {
		fmt.Fprintln(env.stderr, "harness-model-proxy: no config file found; run harness-model-proxy setup")
		return exitUsage
	}
	path := resolved.Path
	cfg := resolved.Config
	keyFile := cfg.APIKeysFile
	initialKeys, keyFileState, err := apikey.LoadInitialFile(keyFile, resolved.APIKeysFileExplicit)
	if err != nil {
		fmt.Fprintf(env.stderr, "harness-model-proxy: api keys file %s: %v\n", keyFile, err)
		return exitRuntime
	}
	authStore := apikey.NewDynamicStore(initialKeys, nil)

	modelsTTL := cfg.ModelsDevCacheTTL.Duration
	env.modelsDevCacheTTL = &modelsTTL
	providerModelsTTL := cfg.ProviderModelsCacheTTL.Duration
	drainDelay := cfg.DrainDelay.Duration
	shutdownTimeout := cfg.ShutdownTimeout.Duration
	logger, err := logging.NewProxyLogger(env.stderr, cfg.LogLevel, cfg.LogFormat)
	if err != nil {
		fmt.Fprintf(env.stderr, "harness-model-proxy: %v\n", err)
		return exitUsage
	}

	configDir := filepath.Dir(path)
	initialCatalog, initialSourceDate := loadModelsDevCacheForServe(configDir)
	_, configuredProviders, err := llm.LoadProviderConfigs(configDir, cfg.ProviderConfigs, func(msg string) { logger.Warn(msg) })
	if err != nil {
		fmt.Fprintf(env.stderr, "harness-model-proxy: %v\n", err)
		return exitRuntime
	}
	initialProviderCatalogs := loadProviderModelCaches(configDir, configuredProviders, currentTime(env), providerModelsTTL, logger)
	metricsEnabled := cfg.Metrics.Enabled == nil || *cfg.Metrics.Enabled
	metricsSettings := metrics.Settings{
		Enabled:        metricsEnabled,
		Listen:         cfg.Metrics.Listen,
		ListenExplicit: resolved.MetricsListenExplicit,
	}
	reg := newMetricsRegistry(metricsSettings.Enabled)
	handler, err := server.NewHandler(server.Options{
		ConfigDir:            configDir,
		Config:               cfg,
		Getenv:               env.getenv,
		Logger:               logger,
		PricingMaxAge:        modelsTTL,
		ModelsDevCatalog:     initialCatalog,
		ModelsDevSourceDate:  initialSourceDate,
		ProviderCatalogs:     initialProviderCatalogs,
		ProviderModelsMaxAge: providerModelsTTL,
		Now:                  env.now,
		Metrics:              reg,
		InstanceID:           cfg.InstanceID,
		Warn: func(msg string) {
			logger.Warn(msg)
		},
	})
	if err != nil {
		fmt.Fprintf(env.stderr, "harness-model-proxy: %v\n", err)
		return exitRuntime
	}
	logger = logger.With("proxy_instance_id", handler.InstanceID())
	addr := cliLast(invocation.Flags, "listen", defaultListen)
	if addr == "" {
		addr = defaultListen
	}
	backgroundCtx, cancelBackground := context.WithCancel(context.Background())
	defer cancelBackground()
	startModelsDevCacheRefresh(backgroundCtx, env, configDir, modelsTTL, logger, func(catalog *modelcatalog.Catalog, sourceDate time.Time) {
		handler.UpdateModelsDevCatalog(catalog, sourceDate)
	})
	startProviderModelRefresh(backgroundCtx, env, configDir, configuredProviders, initialProviderCatalogs, providerModelsTTL, logger, handler.UpdateProviderCatalogs)
	go apikey.WatchFile(backgroundCtx, keyFile, keyFileState, 2*time.Second, authStore, func(err error) {
		logger.Warn("reload api keys failed", "path", keyFile, "err", err)
	})

	ln, err := net.Listen("tcp", addr)
	if err != nil {
		_ = handler.Close()
		fmt.Fprintf(env.stderr, "harness-model-proxy: %v\n", err)
		return exitRuntime
	}
	metricsEndpoint, err := metrics.StartEndpoint(context.Background(), logger, reg, metricsSettings)
	if err != nil {
		ln.Close()
		_ = handler.Close()
		fmt.Fprintf(env.stderr, "harness-model-proxy: %v\n", err)
		return exitRuntime
	}

	protected := server.ObserveAuth(handler, authStore, authStore.Middleware(handler))
	lifecycle := server.NewLifecycle(protected)
	srv := httpserve.New("", lifecycle)
	stopAPI, cancelAPIStop := context.WithCancel(context.Background())
	defer cancelAPIStop()
	workCtx, cancelWork := context.WithCancel(context.Background())
	defer cancelWork()
	apiErr := make(chan error, 1)
	go func() {
		apiErr <- httpserve.ServeWithOptions(srv, ln, httpserve.ServeOptions{
			StopContext:     stopAPI,
			WorkContext:     workCtx,
			ShutdownTimeout: shutdownTimeout,
		})
	}()
	logger.Info("model proxy listening", "addr", addr)

	var serveErr error
	if env.sigCh == nil {
		serveErr = <-apiErr
	} else {
		select {
		case serveErr = <-apiErr:
		case <-env.sigCh:
			lifecycle.BeginDrain()
			cancelBackground()
			apiFinished := false
			if drainDelay > 0 {
				timer := time.NewTimer(drainDelay)
				select {
				case <-timer.C:
				case serveErr = <-apiErr:
					apiFinished = true
				}
				if !timer.Stop() {
					select {
					case <-timer.C:
					default:
					}
				}
			}
			if !apiFinished {
				cancelAPIStop()
				serveErr = <-apiErr
			}
		}
	}
	lifecycle.BeginTeardown()
	cancelBackground()
	cancelWork()
	closeErr := handler.Close()
	metricsCtx, cancelMetrics := context.WithTimeout(context.Background(), httpserve.DefaultShutdownTimeout)
	metricsErr := metricsEndpoint.Shutdown(metricsCtx)
	cancelMetrics()
	if err := errors.Join(serveErr, closeErr, metricsErr); err != nil {
		fmt.Fprintf(env.stderr, "harness-model-proxy: %v\n", err)
		return exitRuntime
	}
	return exitOK
}

// runSetupCmd runs the interactive provider-config wizard from a parsed invocation.
func runSetupCmd(env environment, invocation cli.Invocation) int {
	ttlValue, ttlSet := invocation.Flags.Last("models_dev_cache_ttl")
	ttl, err := setupModelsDevCacheTTL(env, ttlValue, ttlSet)
	if err != nil {
		fmt.Fprintf(env.stderr, "harness-model-proxy: %v\n", err)
		return exitUsage
	}
	env.modelsDevCacheTTL = &ttl
	providerTTLValue, providerTTLSet := invocation.Flags.Last("provider_models_cache_ttl")
	providerTTL, err := setupProviderModelsCacheTTL(env, providerTTLValue, providerTTLSet)
	if err != nil {
		fmt.Fprintf(env.stderr, "harness-model-proxy: %v\n", err)
		return exitUsage
	}
	env.providerModelsCacheTTL = &providerTTL
	ctx, cancel, interrupted := signalCancelContext(env.sigCh)
	defer cancel()
	if err := runSetup(ctx, env, cliBool(invocation.Flags, "force")); err != nil {
		if interrupted() || errors.Is(err, context.Canceled) {
			return exitInterrupt
		}
		fmt.Fprintf(env.stderr, "harness-model-proxy: setup: %v\n", err)
		return exitUsage
	}
	return exitOK
}

// runRefreshModelsCmd re-syncs configured provider files from a parsed invocation.
func runRefreshModelsCmd(env environment, invocation cli.Invocation) int {
	configPath, _ := invocation.Flags.Last("config")
	path := server.ConfigPath(configPath, invocation.Flags.Has("config"), env.getenv)
	if path == "" {
		fmt.Fprintln(env.stderr, "harness-model-proxy: no config file found; run harness-model-proxy setup")
		return exitUsage
	}
	ttlValue, ttlSet := invocation.Flags.Last("models_dev_cache_ttl")
	ttl, err := configuredModelsDevCacheTTL(path, env, ttlValue, ttlSet)
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

// runGenerateAPIKeyCmd generates and stores an API key from a parsed invocation.
func runGenerateAPIKeyCmd(env environment, invocation cli.Invocation) int {
	budgetValue := cliLast(invocation.Flags, "budget_usd", "0")
	budgetUSD, err := strconv.ParseFloat(budgetValue, 64)
	if err != nil {
		fmt.Fprintf(env.stderr, "harness-model-proxy: invalid -budget-usd %q: %v\n", budgetValue, err)
		return exitUsage
	}
	configPath, _ := invocation.Flags.Last("config")
	return runGenerateAPIKey(
		env,
		configPath,
		invocation.Flags.Has("config"),
		cliLast(invocation.Flags, "api_keys_file", ""),
		cliLast(invocation.Flags, "ttl", ""),
		budgetUSD,
		cliLast(invocation.Flags, "budget_period", ""),
		cliBool(invocation.Flags, "budget_reject_unpriced"),
		invocation.Args[0],
	)
}

func runGenerateAPIKey(env environment, argsConfigPath string, configExplicit bool, argsAPIKeysFile, ttlValue string, budgetUSD float64, budgetPeriodValue string, budgetRejectUnpriced bool, name string) int {
	configPath := server.ConfigPath(argsConfigPath, configExplicit, env.getenv)
	if configPath == "" {
		configPath = filepath.Join(defaultConfigDir(env.getenv), "config.json")
	}
	var cfg server.Config
	if _, err := os.Stat(configPath); err == nil {
		var loadErr error
		cfg, loadErr = server.LoadConfig(configPath)
		if loadErr != nil {
			fmt.Fprintf(env.stderr, "harness-model-proxy: %v\n", loadErr)
			return exitRuntime
		}
	} else if err != nil && !errors.Is(err, os.ErrNotExist) {
		fmt.Fprintf(env.stderr, "harness-model-proxy: %v\n", err)
		return exitRuntime
	}
	keyFile := server.ResolveAPIKeysFile(configPath, cfg.APIKeysFile, argsAPIKeysFile)
	ttl, err := parseAPIKeyTTL(ttlValue)
	if err != nil {
		fmt.Fprintf(env.stderr, "harness-model-proxy: %v\n", err)
		return exitUsage
	}
	budget, err := parseAPIKeyBudget(budgetUSD, budgetPeriodValue, budgetRejectUnpriced)
	if err != nil {
		fmt.Fprintf(env.stderr, "harness-model-proxy: %v\n", err)
		return exitUsage
	}
	plaintext, err := apikey.Generate(name, apikey.ModelProxyPrefix)
	if err != nil {
		fmt.Fprintf(env.stderr, "harness-model-proxy: %v\n", err)
		return exitUsage
	}
	entries, err := apikey.LoadFile(keyFile)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			fmt.Fprintf(env.stderr, "harness-model-proxy: api keys file %s: %v\n", keyFile, err)
			return exitRuntime
		}
	}
	store := apikey.Store{Entries: entries}
	now := env.now
	if now == nil {
		now = time.Now
	}
	added := now()
	expiresAt := time.Time{}
	if ttl > 0 {
		expiresAt = added.Add(ttl)
	}
	store.AddWithBudget(name, plaintext, added, expiresAt, budget)
	if err := apikey.WriteFile(keyFile, store.Entries); err != nil {
		fmt.Fprintf(env.stderr, "harness-model-proxy: api keys file %s: %v\n", keyFile, err)
		return exitRuntime
	}
	fmt.Fprintln(env.stdout, plaintext)
	return exitOK
}

func parseAPIKeyTTL(value string) (time.Duration, error) {
	value = strings.TrimSpace(value)
	if value == "" || value == "0" {
		return 0, nil
	}
	ttl, err := time.ParseDuration(value)
	if err != nil {
		return 0, fmt.Errorf("invalid -ttl %q: %w", value, err)
	}
	if ttl < 0 {
		return 0, fmt.Errorf("invalid -ttl %q: duration must be non-negative", value)
	}
	return ttl, nil
}

func parseAPIKeyBudget(limitUSD float64, periodValue string, rejectUnpriced bool) (*apikey.CostBudget, error) {
	periodValue = strings.TrimSpace(periodValue)
	if limitUSD < 0 || math.IsNaN(limitUSD) || math.IsInf(limitUSD, 0) {
		return nil, fmt.Errorf("invalid -budget-usd %v: must be finite and non-negative", limitUSD)
	}
	if limitUSD == 0 {
		if periodValue != "" && periodValue != "0" {
			return nil, fmt.Errorf("-budget-period requires -budget-usd")
		}
		if rejectUnpriced {
			return nil, fmt.Errorf("-budget-reject-unpriced requires -budget-usd")
		}
		return nil, nil
	}
	if periodValue == "" || periodValue == "0" {
		return nil, fmt.Errorf("-budget-period is required when -budget-usd is set")
	}
	period, err := time.ParseDuration(periodValue)
	if err != nil {
		return nil, fmt.Errorf("invalid -budget-period %q: %w", periodValue, err)
	}
	if period <= 0 {
		return nil, fmt.Errorf("invalid -budget-period %q: duration must be positive", periodValue)
	}
	budget := apikey.CostBudget{
		LimitUSD:       limitUSD,
		PeriodSeconds:  int64(period / time.Second),
		RejectUnpriced: rejectUnpriced,
	}
	if time.Duration(budget.PeriodSeconds)*time.Second != period {
		return nil, fmt.Errorf("invalid -budget-period %q: duration must be a whole number of seconds", periodValue)
	}
	if err := apikey.ValidateCostBudget(budget); err != nil {
		return nil, fmt.Errorf("invalid budget: %w", err)
	}
	return &budget, nil
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

func setupProviderModelsCacheTTL(env environment, flagValue string, flagSet bool) (time.Duration, error) {
	ttl := time.Hour
	configPath := filepath.Join(defaultConfigDir(env.getenv), "config.json")
	if cfg, err := server.LoadConfig(configPath); err == nil && cfg.ProviderModelsCacheTTL.Set {
		ttl = cfg.ProviderModelsCacheTTL.Duration
	} else if err != nil && !errors.Is(err, os.ErrNotExist) {
		return 0, err
	}
	if !flagSet {
		return ttl, nil
	}
	value := strings.TrimSpace(flagValue)
	if value == "0" {
		return 0, nil
	}
	parsed, err := time.ParseDuration(value)
	if err != nil || parsed < 0 {
		if err == nil {
			err = fmt.Errorf("duration must be non-negative")
		}
		return 0, fmt.Errorf("invalid -provider-models-cache-ttl %q: %w", value, err)
	}
	return parsed, nil
}

func resolveServeDuration(name, flagValue string, flagSet bool, envValue string, configured server.Duration, fallback time.Duration) (time.Duration, error) {
	if flagSet {
		return parseServeDuration(name, flagValue)
	}
	if strings.TrimSpace(envValue) != "" {
		return parseServeDuration(name, envValue)
	}
	if configured.Set {
		return configured.Duration, nil
	}
	return fallback, nil
}

func parseServeDuration(name, value string) (time.Duration, error) {
	value = strings.TrimSpace(value)
	if value == "0" {
		return 0, nil
	}
	duration, err := time.ParseDuration(value)
	if err != nil {
		return 0, fmt.Errorf("invalid -%s %q: %w", name, value, err)
	}
	if duration < 0 {
		return 0, fmt.Errorf("invalid -%s %q: duration must be non-negative", name, value)
	}
	return duration, nil
}

func resolveServeInstanceID(flagValue string, flagSet bool, envValue, configured string) string {
	if flagSet {
		return strings.TrimSpace(flagValue)
	}
	if value := strings.TrimSpace(envValue); value != "" {
		return value
	}
	return strings.TrimSpace(configured)
}

// newMetricsRegistry returns a registry seeded with the build-info gauge when
// metrics are enabled, or nil when disabled. A nil registry disables collection
// at the handler level (Options.Metrics), so -no-metrics stops the per-request
// recording cost, not just the HTTP listener.
func newMetricsRegistry(enabled bool) *metrics.Registry {
	if !enabled {
		return nil
	}
	return metrics.NewWithBuildInfo(metrics.BuildInfo{
		Name:    "model_proxy_build_info",
		Help:    "Model proxy build information.",
		Version: buildinfo.Version,
	})
}

func defaultConfigDir(getenv func(string) string) string {
	return server.DefaultConfigDir(getenv)
}

func defaultModelsDevCatalog(ctx context.Context) (*modelcatalog.Catalog, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	return modelcatalog.FetchModelsDev(ctx, http.DefaultClient, modelcatalog.ModelsDevURL)
}

func defaultTerminalRows() int {
	rows, _, ok := term.Size()
	if !ok {
		return 0
	}
	return rows
}
