package modelbootstrap

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync"

	"harness/internal/metrics"
	"harness/internal/modelproxy"
	modelconfig "harness/internal/modelproxy/config"
	modelruntime "harness/internal/modelproxy/runtime"
)

// Options selects a local runtime. A nonempty ConfigPath is explicit; otherwise
// ordinary model configuration discovery and environment precedence apply.
type Options struct {
	ConfigPath   string
	Getenv       func(string) string
	Logger       *slog.Logger
	Dependencies Dependencies
}

// Service owns a local backend and all its maintenance jobs. Delegates and
// sessions borrow Backend and Accounts; the application alone calls Close.
type Service struct {
	Backend  modelproxy.Backend
	Accounts modelproxy.Accounts
	refresh  *Refresh
	runtime  *modelruntime.Runtime
	once     sync.Once
	closeErr error
}

// Open initializes local execution directly, without HTTP listeners, metrics
// endpoints, access-key loading, or API-key polling. ctx controls startup only;
// successful services own their maintenance lifetime until Close.
func Open(ctx context.Context, options Options) (*Service, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	getenv := options.Getenv
	if getenv == nil {
		getenv = options.Dependencies.Getenv
	}
	if getenv == nil {
		getenv = os.Getenv
	}
	resolved, err := modelconfig.Load(modelconfig.LoadOptions{Path: options.ConfigPath, PathExplicit: options.ConfigPath != "", Getenv: getenv})
	if err != nil {
		return nil, fmt.Errorf("open local model runtime: %w", err)
	}
	if resolved.Path == "" {
		return nil, fmt.Errorf("no model config file found; run harness-model-proxy setup or set the local model config path")
	}
	logger := options.Logger
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	deps := options.Dependencies
	deps.Getenv = getenv
	cfg := resolved.Config
	dir := filepath.Dir(resolved.Path)
	initial, err := Initialize(dir, cfg.ProviderConfigs, cfg.ProviderModelsCacheTTL.Duration, deps, logger)
	if err != nil {
		return nil, fmt.Errorf("open local model runtime: %w", err)
	}
	// Collect subscription state in-process when enabled, without starting a
	// metrics endpoint. The same config toggle governs standalone polling.
	metricsEnabled := cfg.Metrics.Enabled == nil || *cfg.Metrics.Enabled
	var registry *metrics.Registry
	if metricsEnabled {
		registry = metrics.New()
	}
	runtime, err := modelruntime.New(modelruntime.Options{ConfigDir: dir, Config: cfg, Getenv: getenv, Logger: logger, Now: deps.Now,
		PricingMaxAge: cfg.ModelsDevCacheTTL.Duration, ModelsDevCatalog: initial.ModelsDevCatalog, ModelsDevSourceDate: initial.ModelsDevSourceDate,
		ProviderCatalogs: initial.ProviderCatalogs, ProviderModelsMaxAge: cfg.ProviderModelsCacheTTL.Duration,
		Warn:    func(message string) { logger.Warn(message) },
		Metrics: registry, SubscriptionHTTPClient: deps.SubscriptionHTTPClient,
	})
	if err != nil {
		return nil, fmt.Errorf("open local model runtime: %w", err)
	}
	refresh := StartRefresh(context.Background(), RefreshOptions{ConfigDir: dir, Initial: initial, ModelsDevTTL: cfg.ModelsDevCacheTTL.Duration,
		ProviderModelsTTL: cfg.ProviderModelsCacheTTL.Duration, Dependencies: deps, Logger: logger,
		SubscriptionEnabled: metricsEnabled, SubscriptionInterval: cfg.SubscriptionPollInterval.Duration,
		UpdateModelsDevCatalog: runtime.UpdateModelsDevCatalog, UpdateProviderCatalogs: runtime.UpdateProviderCatalogs, RefreshSubscriptionMetrics: runtime.RefreshSubscriptionMetrics})
	service := &Service{Backend: runtime, Accounts: runtime, refresh: refresh, runtime: runtime}
	if err := ctx.Err(); err != nil {
		return nil, errors.Join(err, service.Close())
	}
	return service, nil
}

func (s *Service) Close() error {
	s.once.Do(func() { s.refresh.Close(); s.closeErr = s.runtime.Close() })
	return s.closeErr
}
