package modelbootstrap

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"harness/internal/llm"
	"harness/internal/modelcatalog"
	"harness/internal/modelproxy/modeldiscovery"
)

// Initial contains the same on-disk catalog snapshot for local and standalone
// runtimes. Missing caches remain unresolved until background refresh succeeds.
type Initial struct {
	ModelsDevCatalog    *modelcatalog.Catalog
	ModelsDevSourceDate time.Time
	ProviderCatalogs    map[string]modeldiscovery.State
	Providers           []llm.ProviderConfig
}

// Initialize loads configured provider files and catalog caches, without network
// calls or reading proxy access keys. Provider warnings retain loader semantics.
func Initialize(configDir string, providerFiles []string, providerTTL time.Duration, deps Dependencies, logger *slog.Logger) (Initial, error) {
	warn := deps.Warn
	if warn == nil && logger != nil {
		warn = func(message string) { logger.Warn(message) }
	}
	_, providers, err := llm.LoadProviderConfigs(configDir, providerFiles, warn)
	if err != nil {
		return Initial{}, err
	}
	catalog, date := LoadModelsDevCacheForServe(configDir)
	return Initial{ModelsDevCatalog: catalog, ModelsDevSourceDate: date,
		ProviderCatalogs: LoadProviderModelCaches(configDir, providers, CurrentTime(deps), providerTTL, logger), Providers: providers}, nil
}

// RefreshOptions binds shared maintenance to a runtime, not its transport.
type RefreshOptions struct {
	ConfigDir                  string
	Initial                    Initial
	ModelsDevTTL               time.Duration
	ProviderModelsTTL          time.Duration
	SubscriptionInterval       time.Duration
	SubscriptionEnabled        bool
	Dependencies               Dependencies
	Logger                     *slog.Logger
	UpdateModelsDevCatalog     func(*modelcatalog.Catalog, time.Time)
	UpdateProviderCatalogs     func(map[string]modeldiscovery.State)
	RefreshSubscriptionMetrics func(context.Context)
}

// Refresh owns all model-maintenance workers. Close cancels and joins them;
// Cancel permits standalone draining to stop new refresh work before teardown.
type Refresh struct {
	cancel context.CancelFunc
	done   []<-chan struct{}
	once   sync.Once
}

func StartRefresh(ctx context.Context, options RefreshOptions) *Refresh {
	ctx, cancel := context.WithCancel(ctx)
	r := &Refresh{cancel: cancel}
	r.done = append(r.done, StartModelsDevCacheRefresh(ctx, options.Dependencies, options.ConfigDir, options.ModelsDevTTL, options.Logger, options.UpdateModelsDevCatalog))
	r.done = append(r.done, StartProviderModelRefresh(ctx, options.Dependencies, options.ConfigDir, options.Initial.Providers, options.Initial.ProviderCatalogs, options.ProviderModelsTTL, options.Logger, options.UpdateProviderCatalogs))
	r.done = append(r.done, StartSubscriptionPolling(ctx, options.SubscriptionEnabled && options.RefreshSubscriptionMetrics != nil, options.SubscriptionInterval, options.Dependencies.SubscriptionAfter, options.RefreshSubscriptionMetrics))
	return r
}

func (r *Refresh) Cancel() { r.cancel() }
func (r *Refresh) Close() {
	r.once.Do(func() {
		r.cancel()
		for _, done := range r.done {
			<-done
		}
	})
}
