// Command harness-model-proxy owns provider configuration, API keys, model
// catalog metadata, and concrete provider calls for harness.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"math"
	"net"
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
	"harness/internal/metrics"
	"harness/internal/modelcatalog"
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
	args              []string
	stdin             io.Reader
	stdout            io.Writer
	stderr            io.Writer
	getenv            func(string) string
	sigCh             chan os.Signal
	modelsDevCatalog  func(context.Context) (*modelcatalog.Catalog, error)
	codexModelsData   func(context.Context) ([]byte, error)
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
	apiKeysFile := fs.String("api-keys-file", "", "accepted API keys file path")
	logLevel := fs.String("log-level", "", "log level: debug, info, warn, error")
	logFormat := fs.String("log-format", "", "log format: json, text")
	noMetrics := fs.Bool("no-metrics", false, "disable the Prometheus /metrics endpoint")
	metricsListen := fs.String("metrics-listen", "", "Prometheus /metrics listen address (default: "+defaultMetricsListen+")")
	drainDelayFlag := fs.String("drain-delay", "", "readiness propagation delay before API shutdown (default: 5s)")
	shutdownTimeoutFlag := fs.String("shutdown-timeout", "", "maximum graceful stream drain time (default: 5m)")
	instanceIDFlag := fs.String("instance-id", "", "proxy instance identifier")
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
	keyFile := server.ResolveAPIKeysFile(path, cfg.APIKeysFile, *apiKeysFile)
	keyFileExplicit := *apiKeysFile != "" || cfg.APIKeysFile != ""
	initialKeys, keyFileState, err := apikey.LoadInitialFile(keyFile, keyFileExplicit)
	if err != nil {
		fmt.Fprintf(env.stderr, "harness-model-proxy: api keys file %s: %v\n", keyFile, err)
		return exitRuntime
	}
	authStore := apikey.NewDynamicStore(initialKeys, nil)

	modelsTTL, err := modelsDevCacheTTLFromConfig(cfg, *modelsDevCacheTTL, flagWasSet(fs, "models-dev-cache-ttl"))
	if err != nil {
		fmt.Fprintf(env.stderr, "harness-model-proxy: %v\n", err)
		return exitUsage
	}
	env.modelsDevCacheTTL = &modelsTTL
	drainDelay, err := resolveServeDuration(
		"drain-delay",
		*drainDelayFlag,
		flagWasSet(fs, "drain-delay"),
		env.getenv("HARNESS_MODEL_PROXY_DRAIN_DELAY"),
		cfg.DrainDelay,
		defaultDrainDelay,
	)
	if err != nil {
		fmt.Fprintf(env.stderr, "harness-model-proxy: %v\n", err)
		return exitUsage
	}
	shutdownTimeout, err := resolveServeDuration(
		"shutdown-timeout",
		*shutdownTimeoutFlag,
		flagWasSet(fs, "shutdown-timeout"),
		env.getenv("HARNESS_MODEL_PROXY_SHUTDOWN_TIMEOUT"),
		cfg.ShutdownTimeout,
		defaultShutdownTime,
	)
	if err != nil {
		fmt.Fprintf(env.stderr, "harness-model-proxy: %v\n", err)
		return exitUsage
	}
	instanceID := resolveServeInstanceID(
		*instanceIDFlag,
		flagWasSet(fs, "instance-id"),
		env.getenv("HARNESS_MODEL_PROXY_INSTANCE_ID"),
		cfg.InstanceID,
	)

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
	// Resolve metrics before building the handler: a nil registry disables
	// collection at the handler level, so -no-metrics stops per-request recording
	// rather than only the listener.
	metricsSettings := metrics.Resolve(cfg.Metrics, defaultMetricsListen, metrics.Overrides{
		Disable:    *noMetrics,
		DisableSet: flagWasSet(fs, "no-metrics"),
		Listen:     *metricsListen,
		ListenSet:  flagWasSet(fs, "metrics-listen"),
	})
	reg := newMetricsRegistry(metricsSettings.Enabled)
	handler, err := server.NewHandler(server.Options{
		ConfigDir:           configDir,
		Config:              cfg,
		Getenv:              env.getenv,
		Logger:              logger,
		PricingMaxAge:       modelsTTL,
		ModelsDevCatalog:    initialCatalog,
		ModelsDevSourceDate: initialSourceDate,
		Now:                 env.now,
		Metrics:             reg,
		InstanceID:          instanceID,
		Warn: func(msg string) {
			logger.Warn(msg)
		},
	})
	if err != nil {
		fmt.Fprintf(env.stderr, "harness-model-proxy: %v\n", err)
		return exitRuntime
	}
	logger = logger.With("proxy_instance_id", handler.InstanceID())
	addr := defaultListen
	if *listen != "" {
		addr = *listen
	}
	backgroundCtx, cancelBackground := context.WithCancel(context.Background())
	defer cancelBackground()
	startModelsDevCacheRefresh(backgroundCtx, env, configDir, modelsTTL, logger, func(catalog *modelcatalog.Catalog, sourceDate time.Time) {
		handler.UpdateModelsDevCatalog(catalog, sourceDate)
	})
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
// and appends its hash to the dedicated key file.
func runGenerateAPIKeyCmd(env environment, args []string) int {
	fs := flag.NewFlagSet("generate-api-key", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	configPath := fs.String("config", "", "config file path")
	apiKeysFile := fs.String("api-keys-file", "", "accepted API keys file path")
	ttl := fs.String("ttl", "", "key TTL as a Go duration; empty or 0 means no expiry")
	budgetUSD := fs.Float64("budget-usd", 0, "per-key cost budget in USD; 0 means no budget")
	budgetPeriod := fs.String("budget-period", "", "per-key cost budget period as a Go duration; required when -budget-usd is set")
	budgetRejectUnpriced := fs.Bool("budget-reject-unpriced", false, "reject unpriced targets while this key's budget is enabled")
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
	return runGenerateAPIKey(env, *configPath, *apiKeysFile, *ttl, *budgetUSD, *budgetPeriod, *budgetRejectUnpriced, fs.Arg(0))
}

func usage(w io.Writer) {
	fmt.Fprint(w, `harness-model-proxy - provider and model proxy for harness

Usage:
  harness-model-proxy serve             [-config path] [-api-keys-file path] [-listen addr] [-models-dev-cache-ttl d] [-drain-delay d] [-shutdown-timeout d] [-instance-id id] [-log-level level] [-log-format format] [-no-metrics] [-metrics-listen addr]
  harness-model-proxy setup             [-force] [-models-dev-cache-ttl d]
  harness-model-proxy refresh-models    [-config path] [-models-dev-cache-ttl d]
  harness-model-proxy auth              <login|logout|status> [-config path] <provider>
  harness-model-proxy generate-api-key  [-config path] [-api-keys-file path] [-ttl duration] [-budget-usd amount -budget-period duration] [-budget-reject-unpriced] <name>
  harness-model-proxy version
  harness-model-proxy --version

With no arguments, harness-model-proxy serves HTTP (the default action).

Subcommands:
  serve             Load config and serve the HTTP model proxy (default).
  setup             Create or update proxy and provider config interactively.
  refresh-models    Fetch models.dev and update configured provider model metadata.
  auth              Login, logout, or inspect OAuth tokens for a configured provider.
  generate-api-key  Generate a new API key with the given name and add it to the key file.
  version           Print the release version.

serve flags:
  -config path            config file path
  -api-keys-file path     accepted API keys file path (default: api_keys.json next to config)
  -listen addr            HTTP listen address (default: `+defaultListen+`)
  -models-dev-cache-ttl d models.dev cache refresh interval, e.g. 24h; 0 disables periodic refresh
  -log-level level        debug|info|warn|error (overrides config)
  -log-format format      json|text (overrides config)
  -no-metrics             disable the Prometheus /metrics endpoint
  -metrics-listen addr    Prometheus /metrics listen address (default: `+defaultMetricsListen+`)
  -drain-delay d          readiness propagation delay before API shutdown (default: 5s)
  -shutdown-timeout d     maximum graceful stream drain time (default: 5m)
  -instance-id id         proxy instance identifier (default: random)

setup flags:
  -force                  overwrite existing provider files
  -models-dev-cache-ttl d models.dev cache refresh interval

refresh-models flags:
  -config path            config file path
  -models-dev-cache-ttl d models.dev cache refresh interval

generate-api-key flags:
  -config path            config file path
  -api-keys-file path     accepted API keys file path (default: api_keys.json next to config)
  -ttl duration           key TTL as a Go duration; empty or 0 means no expiry
  -budget-usd amount      per-key cost budget in USD; 0 means no budget
  -budget-period duration per-key cost budget period; required when -budget-usd is set
  -budget-reject-unpriced reject unpriced targets while this key's budget is enabled
`)
}

// usageServe prints serve-specific help.
func usageServe(w io.Writer) {
	fmt.Fprint(w, `harness-model-proxy serve - load config and serve the HTTP model proxy

Usage:
  harness-model-proxy serve [-config path] [-api-keys-file path] [-listen addr] [-models-dev-cache-ttl d] [-drain-delay d] [-shutdown-timeout d] [-instance-id id] [-log-level level] [-log-format format] [-no-metrics] [-metrics-listen addr]

With no arguments, harness-model-proxy serves HTTP (the default action).

Flags:
  -config path            config file path
  -api-keys-file path     accepted API keys file path (default: api_keys.json next to config)
  -listen addr            HTTP listen address (default: `+defaultListen+`)
  -models-dev-cache-ttl d models.dev cache refresh interval, e.g. 24h; 0 disables periodic refresh
  -log-level level        debug|info|warn|error (overrides config)
  -log-format format      json|text (overrides config)
  -no-metrics             disable the Prometheus /metrics endpoint
  -metrics-listen addr    Prometheus /metrics listen address (default: `+defaultMetricsListen+`)
  -drain-delay d          readiness propagation delay before API shutdown (default: 5s)
  -shutdown-timeout d     maximum graceful stream drain time (default: 5m)
  -instance-id id         proxy instance identifier (default: random)
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
  harness-model-proxy generate-api-key [-config path] [-api-keys-file path] [-ttl duration] [-budget-usd amount -budget-period duration] [-budget-reject-unpriced] <name>

Writes the dedicated API-key file; it does not create or mutate the normal config.

Flags:
  -config path            config file path
  -api-keys-file path     accepted API keys file path (default: api_keys.json next to config)
  -ttl duration           key TTL as a Go duration; empty or 0 means no expiry
  -budget-usd amount      per-key cost budget in USD; 0 means no budget
  -budget-period duration per-key cost budget period; required when -budget-usd is set
  -budget-reject-unpriced reject unpriced targets while this key's budget is enabled
`)
}

func runGenerateAPIKey(env environment, argsConfigPath, argsAPIKeysFile, ttlValue string, budgetUSD float64, budgetPeriodValue string, budgetRejectUnpriced bool, name string) int {
	configPath := server.ConfigPath(argsConfigPath, argsConfigPath != "", env.getenv)
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

func flagWasSet(fs *flag.FlagSet, name string) bool {
	set := false
	fs.Visit(func(f *flag.Flag) {
		if f.Name == name {
			set = true
		}
	})
	return set
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
