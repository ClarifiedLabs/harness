package runtime

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"harness/internal/auth"
	"harness/internal/llm"
	"harness/internal/llm/factory"
	"harness/internal/metrics"
	"harness/internal/modelcatalog"
	"harness/internal/modelproxy"
	"harness/internal/modelproxy/config"
	"harness/internal/modelproxy/modeldiscovery"
	"harness/internal/modelproxy/pricing"
	"harness/internal/modelproxy/subscription"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

var instanceIDPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)

type CostBudgetConfig struct {
	LimitUSD       float64         `json:"limit_usd,omitempty"`
	Period         config.Duration `json:"period,omitempty"`
	RejectUnpriced bool            `json:"reject_unpriced,omitempty"`
}

func (c CostBudgetConfig) Enabled() bool {
	return budgetEnabled(c)
}

type Options struct {
	ConfigDir string
	Config    config.Config
	Getenv    func(string) string
	Logger    *slog.Logger
	New       func(factory.Options) (llm.Provider, error)
	Warn      func(string)
	// PricingMaxAge is the effective models.dev refresh interval used to stamp
	// catalog pricing staleness. Zero falls back to Config.ModelsDevCacheTTL so
	// a flag override (which Config does not carry) is still reflected.
	PricingMaxAge time.Duration
	// ModelsDevCatalog is the in-memory models.dev cache the proxy uses to
	// resolve prices for managed providers. Nil leaves managed prices unresolved
	// until UpdateModelsDevCatalog supplies one. The cache loader lives in the
	// proxy command, so main passes the catalog in rather than the server
	// reading it (keeping internal/llm free of an internal/modelcatalog import).
	ModelsDevCatalog *modelcatalog.Catalog
	// ModelsDevSourceDate dates ModelsDevCatalog (its cache file mtime). Used to
	// stamp catalog pricing freshness when any provider is managed.
	ModelsDevSourceDate time.Time
	// ProviderCatalogs contains cached authenticated provider snapshots. Fresh
	// states are authoritative for availability; stale states supply metadata.
	ProviderCatalogs map[string]modeldiscovery.State
	// ProviderModelsMaxAge is the provider snapshot refresh interval.
	ProviderModelsMaxAge time.Duration
	// Now supplies the clock for budget windows. Nil uses time.Now.
	Now func() time.Time
	// Metrics, when non-nil, receives Prometheus-style counters for every
	// /v1/stream request (tokens, cost, requests, errors, duration), broken
	// down by provider, model, request purpose, and authorizing key. Nil disables metrics at
	// the handler level; the command wires a registry in when the metrics
	// endpoint is enabled.
	Metrics *metrics.Registry
	// InstanceID identifies this proxy process in diagnostics and per-process
	// usage reports. Empty generates a random 16-byte hexadecimal identifier.
	InstanceID string
	// SubscriptionHTTPClient is used only for subscription quota requests.
	SubscriptionHTTPClient *http.Client
	wsPool                 wsPoolOptions
}

// usageKey identifies an aggregate usage bucket by provider and model.
type usageKey struct {
	targetID string
}

type resolvedTarget struct {
	targetID     string
	baseTargetID string
	variant      string
	serviceTier  llm.ServiceTier
	pc           llm.ProviderConfig
	entry        llm.ModelEntry
}

// metricsCollectors holds the pre-registered metric families the proxy stamps
// per stream request. They are created once in NewRuntime so HELP/TYPE always
// appear in exposition even with zero traffic.
type metricsCollectors struct {
	requests     *metrics.Counter
	errors       *metrics.Counter
	input        *metrics.Counter
	output       *metrics.Counter
	cacheRead    *metrics.Counter
	promptInput  *metrics.Counter
	cacheWrite   *metrics.Counter
	cacheWrite1h *metrics.Counter
	reasoning    *metrics.Counter
	cost         *metrics.Counter
	duration     *metrics.Counter
	continuation *metrics.Counter
	wsPoolEvents *metrics.Counter
	wsPoolSize   *metrics.Gauge
	wsPoolCap    *metrics.Gauge
}

type Runtime struct {
	liveMu sync.Mutex
	live   map[liveStreamKey]*liveStreamBinding
	// snapshot holds the current registry+catalog. Built once in NewRuntime and
	// replaced wholesale after catalog refreshes; never mutated in place.
	snapshot atomic.Pointer[catalogSnapshot]

	providers            []llm.ProviderConfig
	authSources          map[string]*auth.Source
	limits               *subscription.Client
	defaultContextWindow int
	configDir            string
	configSourceDate     time.Time
	pricingMaxAge        time.Duration
	providerModelsMaxAge time.Duration
	catalogMu            sync.Mutex
	modelsDevCatalog     *modelcatalog.Catalog
	modelsDevSourceDate  time.Time
	providerCatalogs     map[string]modeldiscovery.State
	getenv               func(string) string
	now                  func() time.Time
	logger               *slog.Logger
	newProvider          func(factory.Options) (llm.Provider, error)
	nextRequestID        atomic.Uint64
	instanceID           string
	startedAt            time.Time

	metrics             *metrics.Registry
	metricFams          *metricsCollectors
	subscriptionMetrics *subscriptionMetrics

	keyBudgetMu sync.Mutex
	keyBudgets  map[string]*costBudgetTracker

	usageMu sync.Mutex
	usage   map[usageKey]*modelproxy.ModelUsage

	wsPool *wsPool
}

func New(opts Options) (*Runtime, error) {
	getenv := opts.Getenv
	if getenv == nil {
		getenv = os.Getenv
	}
	logger := opts.Logger
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	newProvider := opts.New
	if newProvider == nil {
		newProvider = factory.New
	}
	warn := opts.Warn
	_, providers, err := llm.LoadProviderConfigs(opts.ConfigDir, opts.Config.ProviderConfigs, warn)
	if err != nil {
		return nil, err
	}
	for _, pc := range providers {
		if err := llm.ValidateProfile(pc); err != nil {
			return nil, fmt.Errorf("provider %q: %w", pc.Name, err)
		}
	}
	authSources, err := buildAuthSources(providers, opts.ConfigDir, getenv)
	if err != nil {
		return nil, err
	}
	defaultWindow := opts.Config.DefaultContextWindow
	if defaultWindow <= 0 {
		defaultWindow = llm.DefaultContextWindow
	}
	if len(providers) == 0 {
		return nil, fmt.Errorf("model proxy: no provider configs are configured")
	}
	now := opts.Now
	if now == nil {
		now = time.Now
	}
	maxAge := opts.PricingMaxAge
	if maxAge <= 0 {
		maxAge = opts.Config.ModelsDevCacheTTL.Duration
	}
	configuredInstanceID := opts.InstanceID
	if strings.TrimSpace(configuredInstanceID) == "" {
		configuredInstanceID = opts.Config.InstanceID
	}
	instanceID, err := resolveInstanceID(configuredInstanceID)
	if err != nil {
		return nil, err
	}
	logger = logger.With("proxy_instance_id", instanceID)
	startedAt := now()
	h := &Runtime{
		providers:            providers,
		authSources:          authSources,
		defaultContextWindow: defaultWindow,
		configDir:            opts.ConfigDir,
		configSourceDate:     providerConfigSourceDate(opts.ConfigDir, opts.Config.ProviderConfigs),
		pricingMaxAge:        maxAge,
		providerModelsMaxAge: opts.ProviderModelsMaxAge,
		modelsDevCatalog:     cloneModelsDevCatalog(opts.ModelsDevCatalog),
		modelsDevSourceDate:  opts.ModelsDevSourceDate,
		providerCatalogs:     cloneProviderCatalogStates(opts.ProviderCatalogs),
		getenv:               getenv,
		now:                  now,
		logger:               logger,
		newProvider:          newProvider,
		instanceID:           instanceID,
		startedAt:            startedAt,
		keyBudgets:           map[string]*costBudgetTracker{},
		usage:                map[usageKey]*modelproxy.ModelUsage{},
	}
	h.limits = subscription.New(subscription.Options{Client: opts.SubscriptionHTTPClient, Now: now, Resolve: h.subscriptionCredentials, CodexClientVersion: modelcatalog.CodexClientVersion()})
	if opts.Metrics != nil {
		h.metrics = opts.Metrics
		h.metricFams = registerMetricFamilies(opts.Metrics)
		h.subscriptionMetrics = newSubscriptionMetrics(opts.Metrics)
	}
	for _, pc := range providers {
		if _, _, err := modeldiscovery.Resolve(pc); err != nil {
			return nil, err
		}
	}
	snapshot, err := h.buildSnapshot(h.modelsDevCatalog, h.modelsDevSourceDate, h.providerCatalogs)
	if err != nil {
		return nil, err
	}
	h.snapshot.Store(snapshot)
	poolOpts := opts.wsPool
	externalPoolEvent := poolOpts.onEvent
	poolOpts.onEvent = func(event string) {
		if externalPoolEvent != nil {
			externalPoolEvent(event)
		}
		h.recordWSPoolEvent(event)
	}
	externalPoolSize := poolOpts.onSize
	poolOpts.onSize = func(current, capacity int) {
		if externalPoolSize != nil {
			externalPoolSize(current, capacity)
		}
		if h.metricFams != nil {
			h.metricFams.wsPoolSize.Set(float64(current), nil)
			h.metricFams.wsPoolCap.Set(float64(capacity), nil)
		}
	}
	h.wsPool = newWSPool(poolOpts)
	return h, nil
}

func resolveInstanceID(configured string) (string, error) {
	id := strings.TrimSpace(configured)
	if id == "" {
		var raw [16]byte
		if _, err := rand.Read(raw[:]); err != nil {
			return "", fmt.Errorf("model proxy: generate instance id: %w", err)
		}
		return hex.EncodeToString(raw[:]), nil
	}
	if !instanceIDPattern.MatchString(id) {
		return "", fmt.Errorf("model proxy: invalid instance id %q (want [A-Za-z0-9][A-Za-z0-9._-]{0,127})", configured)
	}
	return id, nil
}

// registerMetricFamilies pre-registers the proxy's Prometheus counter
// families so HELP/TYPE are present even with zero traffic. Labels are
// provider, model, purpose, and key (the authorizing key's name, or "anonymous").
func registerMetricFamilies(r *metrics.Registry) *metricsCollectors {
	return &metricsCollectors{
		requests:     r.Counter("model_proxy_requests_total", "Number of proxied model requests."),
		errors:       r.Counter("model_proxy_errors_total", "Number of proxied model requests that failed."),
		input:        r.Counter("model_proxy_input_tokens_total", "Input tokens billed at full rate."),
		output:       r.Counter("model_proxy_output_tokens_total", "Generated output tokens."),
		cacheRead:    r.Counter("model_proxy_cache_read_tokens_total", "Prompt-cache read tokens."),
		promptInput:  r.Counter("model_proxy_prompt_input_tokens_total", "Total prompt input tokens across uncached input, cache reads, and cache writes."),
		cacheWrite:   r.Counter("model_proxy_cache_write_tokens_total", "Default-rate prompt-cache write tokens."),
		cacheWrite1h: r.Counter("model_proxy_cache_write_1h_tokens_total", "One-hour prompt-cache write tokens."),
		reasoning:    r.Counter("model_proxy_reasoning_tokens_total", "Reasoning tokens."),
		cost:         r.Counter("model_proxy_cost_usd_total", "Estimated cost in US dollars."),
		duration:     r.Counter("model_proxy_request_duration_seconds_total", "Total request wall-clock duration in seconds."),
		continuation: r.Counter("model_proxy_continuation_total", "Number of stream requests by provider-continuation result."),
		wsPoolEvents: r.Counter("model_proxy_ws_pool_events_total", "Number of Responses WebSocket pool events."),
		wsPoolSize:   r.Gauge("model_proxy_ws_pool_connections", "Current number of pooled Responses WebSocket connections."),
		wsPoolCap:    r.Gauge("model_proxy_ws_pool_capacity", "Configured maximum Responses WebSocket pool size."),
	}
}

func (h *Runtime) recordWSPoolEvent(event string) {
	if h.metricFams == nil {
		return
	}
	h.metricFams.wsPoolEvents.Inc(map[string]string{"event": normalizeWSPoolEvent(event)})
}

func (h *Runtime) recordContinuation(result string) {
	if h.metricFams == nil {
		return
	}
	h.metricFams.continuation.Inc(map[string]string{"result": normalizeContinuationResult(result)})
}

func normalizeWSPoolEvent(event string) string {
	switch event {
	case "hit", "miss", "create", "evict_lru", "evict_idle", "evict_age", "overflow":
		return event
	default:
		return "unknown"
	}
}

func normalizeContinuationResult(result string) string {
	switch result {
	case "not_offered", "served", "unavailable", "rejected_upstream", "failed":
		return result
	default:
		return "failed"
	}
}

func streamFailed(ctx context.Context, streamErr string, status int) bool {
	if errors.Is(ctx.Err(), context.Canceled) {
		return false
	}
	return streamErr != "" || status >= http.StatusBadRequest
}

// buildSnapshot resolves provider-direct availability and metadata together
// with models.dev and configured fallbacks, then builds the registry and served
// catalog. Manual providers keep their configured availability and prices
// unless they explicitly opt into discovery.
func (h *Runtime) buildSnapshot(md *modelcatalog.Catalog, mdSourceDate time.Time, providerCatalogs map[string]modeldiscovery.State) (*catalogSnapshot, error) {
	priced, pruned := h.effectiveProviders(md, providerCatalogs)
	pricer := pricing.NewComposite()
	registry := llm.RegistryFromProviderConfigs(priced)
	registry.SetDefaultContextWindow(h.defaultContextWindow)
	catalog, targets, err := catalogFromProviderConfigs(priced, pricer)
	if err != nil {
		if pruned && configuredTargetCount(priced) == 0 {
			catalog := modelproxy.Catalog{Targets: []modelproxy.Target{}, Pricing: h.pricingInfo(md, mdSourceDate, providerCatalogs)}
			return &catalogSnapshot{registry: registry, catalog: catalog, targets: map[string]resolvedTarget{}, pricer: pricer}, nil
		}
		return nil, err
	}
	catalog.Pricing = h.pricingInfo(md, mdSourceDate, providerCatalogs)
	return &catalogSnapshot{registry: registry, catalog: catalog, targets: targets, pricer: pricer}, nil
}

// UpdateModelsDevCatalog rebuilds the served snapshot with prices resolved from
// md (manual providers unchanged) and swaps it in atomically. The serving
// refresher calls this after a successful models.dev cache refresh so live
// prices reach in-flight cost accounting and /v1/models without a restart.
func (h *Runtime) UpdateModelsDevCatalog(md *modelcatalog.Catalog, sourceDate time.Time) {
	h.catalogMu.Lock()
	defer h.catalogMu.Unlock()
	h.modelsDevCatalog = cloneModelsDevCatalog(md)
	h.modelsDevSourceDate = sourceDate
	snapshot, err := h.buildSnapshot(h.modelsDevCatalog, h.modelsDevSourceDate, h.providerCatalogs)
	if err != nil {
		h.logger.Warn("rebuild catalog snapshot failed", "err", err)
		return
	}
	h.snapshot.Store(snapshot)
}

// UpdateProviderCatalogs merges newly fetched provider snapshots and atomically
// rebuilds the served catalog once for the completed refresh cycle.
func (h *Runtime) UpdateProviderCatalogs(updates map[string]modeldiscovery.State) {
	if len(updates) == 0 {
		return
	}
	h.catalogMu.Lock()
	defer h.catalogMu.Unlock()
	if h.providerCatalogs == nil {
		h.providerCatalogs = map[string]modeldiscovery.State{}
	}
	for provider, state := range updates {
		h.providerCatalogs[provider] = cloneProviderCatalogState(state)
	}
	snapshot, err := h.buildSnapshot(h.modelsDevCatalog, h.modelsDevSourceDate, h.providerCatalogs)
	if err != nil {
		h.logger.Warn("rebuild catalog snapshot failed", "err", err)
		return
	}
	h.snapshot.Store(snapshot)
}

func snapshotHasPrice(snapshot modeldiscovery.Snapshot) bool {
	for _, model := range snapshot.Models {
		if model.Price != nil {
			return true
		}
	}
	return false
}

func modelsDevContributesPrice(providers []llm.ProviderConfig, md *modelcatalog.Catalog, providerCatalogs map[string]modeldiscovery.State) bool {
	for _, pc := range providers {
		if !pc.Managed || pc.CodexBackend() {
			continue
		}
		_, discoverySupported, _ := modeldiscovery.Resolve(pc)
		state, hasState := providerCatalogs[pc.Name]
		if hasState && state.Unsupported {
			discoverySupported = false
		}
		// When direct discovery is disabled or unsupported, models.dev remains
		// the authoritative catalog source even if a refresh removes the whole
		// provider. Keep dating that catalog decision to the refresh itself.
		if !discoverySupported {
			return true
		}
		priceProvider := pc.Name
		if strings.TrimSpace(pc.PriceSource) != "" {
			priceProvider = pc.PriceSource
		}
		provider, ok := md.Provider(priceProvider)
		if !ok {
			continue
		}
		for _, entry := range pc.Models {
			info, pricedByModelsDev := provider.ModelInfo(entry.Name)
			if !pricedByModelsDev || info.Price.IsZero() {
				continue
			}
			if strings.TrimSpace(pc.PriceSource) != "" || !hasState || !state.Authoritative || state.Unsupported {
				return true
			}
			direct, present := state.Snapshot.Models[entry.Name]
			if present && direct.Price == nil {
				return true
			}
		}
	}
	return false
}

func cloneProviderCatalogStates(in map[string]modeldiscovery.State) map[string]modeldiscovery.State {
	out := make(map[string]modeldiscovery.State, len(in))
	for provider, state := range in {
		out[provider] = cloneProviderCatalogState(state)
	}
	return out
}

func configuredTargetCount(providers []llm.ProviderConfig) int {
	count := 0
	for _, pc := range providers {
		if pc.Name == "" {
			continue
		}
		for _, entry := range pc.Models {
			if entry.Name != "" {
				count++
			}
		}
	}
	return count
}

// providerConfigSourceDate returns the newest modification time among the
// configured provider files. It dates manual prices (which live in those files)
// and returns the zero time when no file can be stat'd. Managed prices are dated
// separately by the models.dev cache source date, which the refresher keeps
// fresh; see pricingInfo.
func providerConfigSourceDate(configDir string, files []string) time.Time {
	var newest time.Time
	for _, f := range files {
		path := f
		if !filepath.IsAbs(path) {
			path = filepath.Join(configDir, f)
		}
		info, err := os.Stat(path)
		if err != nil {
			continue
		}
		if mt := info.ModTime(); mt.After(newest) {
			newest = mt
		}
	}
	return newest
}

func (h *Runtime) streamProvider(opts factory.Options, providerID, promptCacheKey string, principal []byte) (llm.Provider, func(), error) {
	if !opts.ResponsesWebSocket {
		provider, err := h.newProvider(opts)
		return provider, func() {}, err
	}
	key := streamProviderCacheKey(opts, providerID, promptCacheKey)
	key.Principal = sha256.Sum256(principal)
	return h.wsPool.Acquire(key, func() (llm.Provider, error) {
		return h.newProvider(opts)
	})
}

func streamProviderCacheKey(opts factory.Options, providerID, promptCacheKey string) wsPoolKey {
	type authHeader struct {
		Name  string `json:"name"`
		Value string `json:"value"`
	}
	type connectionConfig struct {
		ProviderID        string                `json:"provider_id"`
		Provider          string                `json:"provider"`
		ProviderName      string                `json:"provider_name"`
		Model             string                `json:"model"`
		BaseURL           string                `json:"base_url"`
		ContextWindow     int                   `json:"context_window"`
		OutputLimit       int                   `json:"output_limit"`
		MinOutputTokens   int                   `json:"min_output_tokens"`
		OmitMaxOutput     bool                  `json:"omit_max_output_tokens"`
		APIKey            string                `json:"api_key"`
		AuthHeadersSHA256 string                `json:"auth_headers_sha256"`
		PromptCache       llm.PromptCacheConfig `json:"prompt_cache"`
	}
	keys := make([]string, 0, len(opts.AuthHeaders))
	for key := range opts.AuthHeaders {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	headers := make([]authHeader, 0, len(keys))
	for _, key := range keys {
		headers = append(headers, authHeader{Name: strings.ToLower(key), Value: opts.AuthHeaders[key]})
	}
	headerJSON, _ := json.Marshal(headers)
	headerDigest := sha256.Sum256(headerJSON)
	connectionJSON, _ := json.Marshal(connectionConfig{
		ProviderID:        providerID,
		Provider:          opts.Provider,
		ProviderName:      opts.ProviderName,
		Model:             opts.Model,
		BaseURL:           opts.BaseURL,
		ContextWindow:     opts.ContextWindow,
		OutputLimit:       opts.OutputLimit,
		MinOutputTokens:   opts.MinOutputTokens,
		OmitMaxOutput:     opts.OmitMaxOutputTokens,
		APIKey:            opts.APIKey,
		AuthHeadersSHA256: hex.EncodeToString(headerDigest[:]),
		PromptCache:       opts.PromptCache,
	})
	return wsPoolKey{
		Connection: sha256.Sum256(connectionJSON),
		Session:    sha256.Sum256([]byte(promptCacheKey)),
	}
}

// Close stops transport maintenance and releases idle provider connections.
// Active leases retire and close when their request finishes.
func (h *Runtime) Close() error {
	if h == nil || h.wsPool == nil {
		return nil
	}
	return h.wsPool.Close()
}

func streamErrorLogAttrs(err error) []any {
	attrs := []any{"err_go_type", fmt.Sprintf("%T", err)}
	var apiErr *llm.APIError
	if errors.As(err, &apiErr) {
		attrs = append(attrs,
			"err_kind", "api",
			"api_status_code", apiErr.StatusCode,
			"api_code", apiErr.Code,
			"api_message", apiErr.Message,
			"api_retryable", apiErr.Retryable,
			"api_retry_after_ms", apiErr.RetryAfter.Milliseconds(),
		)
		if len(apiErr.ResponsePayload) > 0 {
			attrs = append(attrs, "api_response_payload", json.RawMessage(apiErr.ResponsePayload))
		}
		return attrs
	}
	switch {
	case errors.Is(err, context.Canceled):
		return append(attrs, "err_kind", "context_canceled")
	case errors.Is(err, context.DeadlineExceeded):
		return append(attrs, "err_kind", "context_deadline")
	default:
		return append(attrs, "err_kind", "other")
	}
}

func modelRequestEventFromError(err error, state llm.ModelRequestState, outcome llm.ModelRequestOutcome, duration time.Duration) llm.ModelRequestEvent {
	event := llm.ModelRequestEvent{
		State:             state,
		Outcome:           outcome,
		AttemptDurationMS: duration.Milliseconds(),
		Stage:             upstreamErrorStage(err),
	}
	var apiErr *llm.APIError
	if errors.As(err, &apiErr) {
		event.StatusCode = apiErr.StatusCode
		event.Code = apiErr.Code
		event.Message = apiErr.Message
		event.ResponsePayload = apiErr.ResponsePayload
		event.Retryable = apiErr.Retryable
		event.RetryAfterMS = apiErr.RetryAfter.Milliseconds()
		if apiErr.Diagnostic != nil {
			event.UpstreamRequestID = apiErr.Diagnostic.UpstreamRequestID
			if apiErr.Diagnostic.Stage != "" {
				event.Stage = apiErr.Diagnostic.Stage
			}
		}
		return event
	}
	event.Message = err.Error()
	return event
}

func mergeModelRequestFailure(base, current llm.ModelRequestEvent) llm.ModelRequestEvent {
	current.Attempt = base.Attempt
	current.MaxAttempts = base.MaxAttempts
	if base.AttemptDurationMS > 0 {
		current.AttemptDurationMS = base.AttemptDurationMS
	}
	if base.UpstreamRequestID != "" {
		current.UpstreamRequestID = base.UpstreamRequestID
	}
	if len(current.ResponsePayload) == 0 && len(base.ResponsePayload) > 0 {
		current.ResponsePayload = base.ResponsePayload
	}
	return current
}

func redactModelRequestEvent(event llm.ModelRequestEvent, req llm.Request) llm.ModelRequestEvent {
	if event.Message == "" && event.Code == "" {
		return event
	}
	err := redactImageBearingError(&llm.APIError{
		StatusCode:      event.StatusCode,
		Code:            event.Code,
		Message:         event.Message,
		ResponsePayload: event.ResponsePayload,
		Retryable:       event.Retryable,
		RetryAfter:      time.Duration(event.RetryAfterMS) * time.Millisecond,
	}, req)
	var apiErr *llm.APIError
	if errors.As(err, &apiErr) {
		event.Code = apiErr.Code
		event.Message = apiErr.Message
		event.ResponsePayload = apiErr.ResponsePayload
		return event
	}
	event.Message = err.Error()
	return event
}

func modelRequestEventLogAttrs(event llm.ModelRequestEvent) []any {
	attrs := []any{
		"request_id", event.ProxyRequestID,
		"sequence", event.Sequence,
		"state", string(event.State),
		"outcome", string(event.Outcome),
		"target_id", event.TargetID,
		"provider", event.Provider,
		"api_type", event.APIType,
		"model", event.Model,
		"purpose", event.Purpose,
		"upstream_request_id", event.UpstreamRequestID,
		"trace_id", event.TraceID,
		"span_id", event.SpanID,
		"upstream_attempt", event.Attempt,
		"upstream_max_attempts", event.MaxAttempts,
		"api_status_code", event.StatusCode,
		"api_code", event.Code,
		"api_message", event.Message,
		"api_retryable", event.Retryable,
		"api_retry_after_ms", event.RetryAfterMS,
		"retry_delay_ms", event.RetryDelayMS,
		"attempt_duration_ms", event.AttemptDurationMS,
		"elapsed_ms", event.ElapsedMS,
		"error_stage", string(event.Stage),
	}
	if len(event.ResponsePayload) > 0 {
		attrs = append(attrs, "api_response_payload", json.RawMessage(event.ResponsePayload))
	}
	return attrs
}

func mergeUsage(acc, in llm.Usage) llm.Usage {
	acc.InputTokens = max(acc.InputTokens, in.InputTokens)
	acc.OutputTokens = max(acc.OutputTokens, in.OutputTokens)
	acc.CacheReadTokens = max(acc.CacheReadTokens, in.CacheReadTokens)
	acc.CacheWriteTokens = max(acc.CacheWriteTokens, in.CacheWriteTokens)
	acc.CacheWrite1hTokens = max(acc.CacheWrite1hTokens, in.CacheWrite1hTokens)
	acc.CacheWriteTTLKnown = acc.CacheWriteTTLKnown || in.CacheWriteTTLKnown
	acc.ReasoningTokens = max(acc.ReasoningTokens, in.ReasoningTokens)
	if in.CostKnown {
		acc.CostUSD = in.CostUSD
		acc.CostKnown = true
	}
	if in.ServiceTier != "" {
		acc.ServiceTier = in.ServiceTier
	}
	if in.Speed != "" {
		acc.Speed = in.Speed
	}
	return acc
}

func (h *Runtime) runtimeOptionsForTarget(ctx context.Context, target resolvedTarget) (factory.Options, error) {
	if err := ctx.Err(); err != nil {
		return factory.Options{}, err
	}
	pc := target.pc
	entry := target.entry
	apiType := pc.APIType
	if apiType == "" {
		apiType = pc.Name
	}
	apiKey, authHeaders, err := h.providerCredentials(ctx, pc)
	if err != nil {
		return factory.Options{}, credentialResolutionError(ctx, err)
	}
	contextWindow := entry.ContextWindow
	if contextWindow <= 0 {
		contextWindow = h.defaultContextWindow
	}
	return factory.Options{
		Provider:                apiType,
		ProviderName:            pc.Name,
		Profile:                 pc.Profile,
		Model:                   entry.Name,
		BaseURL:                 pc.BaseURL,
		APIKey:                  apiKey,
		AuthHeaders:             authHeaders,
		ContextWindow:           contextWindow,
		OutputLimit:             entry.OutputLimit,
		MinOutputTokens:         pc.MinOutputTokens,
		PromptCache:             pc.PromptCache,
		ReasoningReplay:         pc.ReasoningReplay,
		OmitMaxOutputTokens:     providerOmitMaxOutputTokens(pc),
		ResponsesWebSocket:      targetResponsesWebSocket(pc, entry),
		ResponsesToolSearch:     pc.ResponsesToolSearch,
		AnthropicToolSearch:     pc.AnthropicToolSearch,
		UsageInputIncludesCache: pc.UsageInputIncludesCache,
	}, nil
}

func (h *Runtime) resolveTarget(id string) (resolvedTarget, error) {
	id = strings.TrimSpace(id)
	if id == "" {
		return resolvedTarget{}, fmt.Errorf("target_id is required")
	}
	snapshot := h.snapshot.Load()
	if snapshot == nil {
		return resolvedTarget{}, fmt.Errorf("model proxy catalog is unavailable")
	}
	if target, ok := snapshot.targets[id]; ok {
		return target, nil
	}
	return resolvedTarget{}, fmt.Errorf("target %q is not available from the model proxy", id)
}

func codexResponsesUsesLocalTokenCount(pc llm.ProviderConfig) bool {
	apiType := strings.ToLower(strings.TrimSpace(pc.APIType))
	if apiType == "" {
		apiType = strings.ToLower(strings.TrimSpace(pc.Name))
	}
	if apiType != "responses" {
		return false
	}
	return pc.CodexBackend()
}

func resolveServerToolsForTarget(target resolvedTarget, requested []llm.ServerTool) []llm.ServerTool {
	if len(requested) == 0 {
		return nil
	}
	supported := targetServerTools(target.pc, target.entry)
	if !stringSliceContains(supported, llm.ServerToolWebSearch) {
		return nil
	}
	kind := serverToolKindForProviderConfig(target.pc)
	if kind == "" {
		// The tool is supported (configured explicitly on the provider/model) but
		// the endpoint isn't one harness recognizes by name. Fall back to a wire
		// shape that matches the configured dialect so explicit config is honored
		// instead of being silently dropped.
		kind = defaultWebSearchKindForAPIType(target.pc.APIType)
	}
	if kind == "" {
		return nil
	}
	seen := map[string]bool{}
	var out []llm.ServerTool
	for _, tool := range requested {
		name := strings.ToLower(strings.TrimSpace(tool.Name))
		if name != llm.ServerToolWebSearch || seen[name] {
			continue
		}
		seen[name] = true
		tool.Name = name
		tool.Kind = kind
		out = append(out, tool)
	}
	return out
}

func applyServiceTierForTarget(target resolvedTarget, req *llm.Request) error {
	if req == nil {
		return nil
	}
	req.ServiceTier = ""
	req.Speed = ""
	req.Betas = nil
	if target.variant == "" {
		return nil
	}
	tier := target.serviceTier
	req.ServiceTier = tier.Request.ServiceTier
	req.Speed = tier.Request.Speed
	req.Betas = append([]string(nil), tier.Request.Betas...)
	return nil
}

func targetServerTools(pc llm.ProviderConfig, entry llm.ModelEntry) []string {
	tools := make([]string, 0, len(pc.ServerTools)+len(entry.ServerTools)+1)
	tools = append(tools, pc.ServerTools...)
	tools = append(tools, entry.ServerTools...)
	if providerImplicitWebSearch(pc) {
		tools = append(tools, llm.ServerToolWebSearch)
	}
	return llm.NormalizeServerTools(tools)
}

func providerImplicitWebSearch(pc llm.ProviderConfig) bool {
	return serverToolKindForProviderConfig(pc) != ""
}

func serverToolKindForProviderConfig(pc llm.ProviderConfig) string {
	return llm.WebSearchServerToolKind(pc.Name, pc.APIType, pc.BaseURL)
}

// defaultWebSearchKindForAPIType picks a hosted web-search wire shape for a
// provider harness doesn't recognize by name, based on its configured dialect.
// Both the OpenAI Chat and Responses dialects emit the OpenAI `web_search` tool
// for ServerToolKindOpenAIWebSearch; Anthropic and Gemini Interactions use
// their native declarations.
func defaultWebSearchKindForAPIType(apiType string) string {
	if strings.EqualFold(strings.TrimSpace(apiType), "interactions") {
		return llm.ServerToolKindGoogleSearch
	}
	if strings.EqualFold(strings.TrimSpace(apiType), "anthropic") {
		return llm.ServerToolKindAnthropicWebSearch
	}
	return llm.ServerToolKindOpenAIWebSearch
}

func serverToolRejected(err error) bool {
	// Only treat genuine provider API errors as server-tool rejections; a
	// transport/network failure must surface, not silently retry without tools.
	var apiErr *llm.APIError
	if !errors.As(err, &apiErr) {
		return false
	}
	if apiErr.StatusCode != 0 && apiErr.StatusCode != http.StatusBadRequest && apiErr.StatusCode != http.StatusUnprocessableEntity {
		return false
	}
	text := strings.ToLower(err.Error())
	if !strings.Contains(text, "tool") && !strings.Contains(text, "web_search") && !strings.Contains(text, "web search") {
		return false
	}
	for _, marker := range []string{"unsupported", "invalid", "unknown", "unrecognized", "not supported", "not available", "parameter", "schema"} {
		if strings.Contains(text, marker) {
			return true
		}
	}
	return false
}

var minTokenErrorRe = regexp.MustCompile(`(?i)(?:greater than or equal to|at least|minimum(?: value)?(?: is)?|must be >=)\s+(\d+)`)

func outputTokenFloorRejected(err error) (int, bool) {
	var apiErr *llm.APIError
	if !errors.As(err, &apiErr) {
		return 0, false
	}
	if apiErr.StatusCode != 0 && apiErr.StatusCode != http.StatusBadRequest && apiErr.StatusCode != http.StatusUnprocessableEntity {
		return 0, false
	}
	text := strings.ToLower(apiErr.Code + " " + apiErr.Message)
	if !strings.Contains(text, "max_tokens") &&
		!strings.Contains(text, "max output") &&
		!strings.Contains(text, "max_output_tokens") {
		return 0, false
	}
	match := minTokenErrorRe.FindStringSubmatch(apiErr.Message)
	if len(match) != 2 {
		match = minTokenErrorRe.FindStringSubmatch(apiErr.Code)
	}
	if len(match) != 2 {
		return 0, false
	}
	floor, err := strconv.Atoi(match[1])
	if err != nil || floor <= 0 {
		return 0, false
	}
	return floor, true
}

func stringSliceContains(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

func prepareProviderRequest(req llm.Request) (sessionKey string, providerReq llm.Request) {
	providerReq = req
	proxySessionID := strings.TrimSpace(req.ProxySessionID)
	cacheAffinityID := strings.TrimSpace(req.CacheAffinityID)
	providerReq.ProxySessionID = ""
	providerReq.CacheAffinityID = ""
	if cacheAffinityID != "" {
		providerReq.PromptCacheKey = providerPromptCacheKey(cacheAffinityID)
	}
	if proxySessionID == "" {
		return strings.TrimSpace(req.PromptCacheKey), providerReq
	}
	return proxySessionID, providerReq
}

func providerPromptCacheKey(sessionID string) string {
	sum := sha256.Sum256([]byte(sessionID))
	return hex.EncodeToString(sum[:])
}

func webSocketConnectionLimitReached(err error) bool {
	var apiErr *llm.APIError
	return errors.As(err, &apiErr) && strings.EqualFold(strings.TrimSpace(apiErr.Code), "websocket_connection_limit_reached")
}

func webSocketContinuationUnavailableError(err error) error {
	var apiErr *llm.APIError
	if !errors.As(err, &apiErr) {
		return err
	}
	copyError := *apiErr
	copyError.StatusCode = http.StatusConflict
	copyError.Code = modelproxy.ErrCodePreviousResponseUnavailable
	copyError.Message = "Responses websocket connection expired; previous response is unavailable on a new connection; resend full context."
	copyError.Retryable = false
	copyError.RetryAfter = 0
	return &copyError
}

func previousResponseRejected(err error) bool {
	var apiErr *llm.APIError
	if !errors.As(err, &apiErr) {
		return false
	}
	code := strings.ToLower(apiErr.Code)
	if strings.Contains(code, "previous_response") {
		return true
	}
	if strings.Contains(code, "previous_interaction") {
		return true
	}
	msg := strings.ToLower(apiErr.Message)
	return strings.Contains(msg, "previous_response_id") ||
		strings.Contains(msg, "previous response") ||
		strings.Contains(msg, "previous_interaction_id") ||
		strings.Contains(msg, "previous interaction")
}

func providerOmitMaxOutputTokens(pc llm.ProviderConfig) bool {
	if pc.OmitMaxOutputTokens {
		return true
	}
	apiType := pc.APIType
	if apiType == "" {
		apiType = pc.Name
	}
	if !strings.EqualFold(strings.TrimSpace(apiType), "responses") {
		return false
	}
	return pc.CodexBackend()
}

func buildAuthSources(providers []llm.ProviderConfig, configDir string, getenv func(string) string) (map[string]*auth.Source, error) {
	out := map[string]*auth.Source{}
	for _, pc := range providers {
		if pc.Name == "" || pc.Auth == nil {
			continue
		}
		src, err := auth.NewSource(*pc.Auth, auth.Options{
			Name:      pc.Name,
			ConfigDir: configDir,
			Getenv:    getenv,
		})
		if err != nil {
			return nil, fmt.Errorf("provider %q: %w", pc.Name, err)
		}
		out[pc.Name] = src
	}
	return out, nil
}

func catalogFromProviderConfigs(providers []llm.ProviderConfig, pricer pricing.Pricer) (modelproxy.Catalog, map[string]resolvedTarget, error) {
	out := modelproxy.Catalog{}
	names, err := modelproxy.ConfiguredTargetNames(providers)
	if err != nil {
		return modelproxy.Catalog{}, nil, err
	}
	targets := map[string]resolvedTarget{}
	for _, pc := range providers {
		if pc.Name == "" {
			continue
		}
		for _, entry := range pc.Models {
			if entry.Name == "" {
				continue
			}
			id := pc.Name + ":" + entry.Name
			aliases := names[id]
			price := entry.Price
			if pricer != nil {
				if catalogPricing := pricer.CatalogPricing(pc, entry); catalogPricing.Known {
					price = catalogPricing.Price
				} else {
					price = llm.Price{}
				}
			}
			target := modelproxy.Target{
				IncrementalTools:            targetIncrementalTools(pc, entry),
				ReasoningUpdates:            targetReasoningUpdates(pc, entry),
				AsyncTools:                  targetReasoningUpdates(pc, entry),
				NativeSteering:              targetReasoningUpdates(pc, entry) && targetResponsesWebSocket(pc, entry),
				ID:                          id,
				Aliases:                     aliases,
				DisplayName:                 entry.Name,
				ProviderLabel:               pc.Name,
				Profile:                     llm.NormalizeProfile(pc.Profile),
				ModelLabel:                  entry.Name,
				ContextWindow:               entry.ContextWindow,
				OutputLimit:                 entry.OutputLimit,
				InputModalities:             append([]string(nil), entry.InputModalities...),
				ServerTools:                 targetServerTools(pc, entry),
				APIType:                     pc.APIType,
				ContinuationStateful:        providerContinuationStateful(pc),
				NativeCompaction:            providerResponsesCompaction(pc),
				Prewarm:                     targetResponsesWebSocket(pc, entry),
				Price:                       price,
				Reasoning:                   targetReasoningSupported(entry),
				ReasoningReplayDomain:       reasoningReplayDomain(pc.Name, entry, id),
				SupportsExperimentalContext: entry.SupportsExperimentalContext,
				ContextToolDefinitions:      modelcatalog.CloneContextToolDefinitions(entry.ContextToolDefinitions),
			}
			out.Targets = append(out.Targets, target)
			rt := resolvedTarget{targetID: id, baseTargetID: id, pc: pc, entry: entry}
			for _, alias := range aliases {
				if existing, exists := targets[alias]; exists && existing.targetID != rt.targetID {
					return modelproxy.Catalog{}, nil, fmt.Errorf("model proxy: target alias %q collides with target %q", alias, existing.targetID)
				}
				targets[alias] = rt
			}
			for _, tier := range llm.ModelServiceTiers(pc, entry) {
				if modelproxy.DefaultServiceTier(tier) {
					continue
				}
				variantID := id + ":" + tier.ID
				variantAliases := names[variantID]
				label := tier.Name
				if label == "" {
					label = tier.ID
				}
				variantTarget := target
				variantTarget.ID = variantID
				variantTarget.Aliases = variantAliases
				variantTarget.DisplayName = entry.Name + " (" + label + ")"
				variantTarget.ModelLabel = variantTarget.DisplayName
				variantTarget.BaseTargetID = id
				variantTarget.Variant = tier.ID
				variantTarget.Price = tier.Price
				if pricer != nil {
					variantEntry := entry
					variantEntry.Price = tier.Price
					variantEntry.ServiceTiers = nil
					if catalogPricing := pricer.CatalogPricing(pc, variantEntry); catalogPricing.Known {
						variantTarget.Price = catalogPricing.Price
					} else {
						variantTarget.Price = llm.Price{}
					}
				}
				out.Targets = append(out.Targets, variantTarget)
				variantRT := resolvedTarget{targetID: variantID, baseTargetID: id, variant: tier.ID, serviceTier: tier, pc: pc, entry: entry}
				for _, alias := range variantAliases {
					if existing, exists := targets[alias]; exists && existing.targetID != variantRT.targetID {
						return modelproxy.Catalog{}, nil, fmt.Errorf("model proxy: target alias %q collides with target %q", alias, existing.targetID)
					}
					targets[alias] = variantRT
				}
			}
		}
	}
	if len(out.Targets) == 0 {
		return modelproxy.Catalog{}, nil, fmt.Errorf("model proxy: no configured models")
	}
	return out, targets, nil
}

func reasoningReplayDomain(provider string, entry llm.ModelEntry, targetID string) string {
	if domain := strings.TrimSpace(entry.ReasoningReplayDomain); domain != "" {
		return provider + ":" + domain
	}
	return targetID
}

func providerResponsesStateful(pc llm.ProviderConfig) bool {
	if !strings.EqualFold(strings.TrimSpace(pc.APIType), "responses") {
		return false
	}
	// Meta cannot return encrypted reasoning while continuing a previous response,
	// so an unavailable continuation could not be rebuilt from local history.
	if llm.IsMetaResponsesProvider(pc.Name, pc.APIType, pc.BaseURL) {
		return false
	}
	if pc.ResponsesStateful != nil {
		return *pc.ResponsesStateful
	}
	return true
}

func providerContinuationStateful(pc llm.ProviderConfig) bool {
	apiType := strings.ToLower(strings.TrimSpace(pc.APIType))
	switch apiType {
	case "responses":
		return providerResponsesStateful(pc)
	case "interactions":
		if pc.InteractionsStateful != nil {
			return *pc.InteractionsStateful
		}
		return true
	default:
		return false
	}
}

// Astra's public Responses targets use WebSockets by default for native
// steering. Explicit transport settings still take precedence for every model.
func targetResponsesWebSocket(pc llm.ProviderConfig, entry llm.ModelEntry) bool {
	if !strings.EqualFold(strings.TrimSpace(pc.APIType), "responses") {
		return false
	}
	if pc.ResponsesWebSocket != nil {
		return *pc.ResponsesWebSocket
	}
	return targetReasoningUpdates(pc, entry) || pc.CodexBackend()
}

func providerResponsesCompaction(pc llm.ProviderConfig) bool {
	if !strings.EqualFold(strings.TrimSpace(pc.APIType), "responses") {
		return false
	}
	if pc.ResponsesCompaction != nil {
		return *pc.ResponsesCompaction
	}
	// An explicit profile declares the backend regardless of managed status.
	if profile := llm.NormalizeProfile(pc.Profile); profile != "" {
		return profile == llm.ProfileCodex
	}
	if !pc.Managed {
		return false
	}
	name := strings.ToLower(strings.TrimSpace(pc.Name))
	baseURL := strings.TrimRight(strings.ToLower(strings.TrimSpace(pc.BaseURL)), "/")
	return (name == "openai" && baseURL == "https://api.openai.com/v1") ||
		(name == modelcatalog.OpenAICodexProviderID && baseURL == modelcatalog.OpenAICodexProviderBaseURL)
}

func modelEntryReasoning(m llm.ModelEntry) *llm.ReasoningInfo {
	if m.Reasoning == nil && m.ReasoningSummarySupported == nil && len(m.ReasoningOptions) == 0 {
		return nil
	}
	supported := false
	if m.Reasoning != nil {
		supported = *m.Reasoning
	}
	return (&llm.ReasoningInfo{
		Supported:        supported,
		SummarySupported: m.ReasoningSummarySupported,
		Options:          append([]llm.ReasoningOption(nil), m.ReasoningOptions...),
	}).Clone()
}

func targetReasoningSupported(m llm.ModelEntry) bool {
	info := modelEntryReasoning(m)
	if info == nil || !info.Supported {
		return false
	}
	return true
}

func providerAPIKeyEnv(provider string, getenv func(string) string) string {
	switch provider {
	case "anthropic":
		return getenv("ANTHROPIC_API_KEY")
	case "interactions":
		return getenv("GEMINI_API_KEY")
	case "responses":
		if v := getenv("RESPONSES_API_KEY"); v != "" {
			return v
		}
		return getenv("OPENAI_API_KEY")
	default:
		return getenv("OPENAI_API_KEY")
	}
}

func (h *Runtime) InstanceID() string {
	if h == nil {
		return ""
	}
	return h.instanceID
}
func (h *Runtime) Logger() *slog.Logger { return h.logger }
func (h *Runtime) Catalog(ctx context.Context) (modelproxy.Catalog, error) {
	if err := ctx.Err(); err != nil {
		return modelproxy.Catalog{}, err
	}
	return modelproxy.CloneCatalog(h.snapshot.Load().catalog), nil
}
func (h *Runtime) Usage(ctx context.Context) modelproxy.UsageReport {
	h.usageMu.Lock()
	report := modelproxy.UsageReport{Instance: h.instanceID, Since: h.startedAt, Models: make([]modelproxy.ModelUsage, 0, len(h.usage))}
	for _, u := range h.usage {
		report.Models = append(report.Models, *u)
	}
	h.usageMu.Unlock()
	sort.Slice(report.Models, func(i, j int) bool { return report.Models[i].TargetID < report.Models[j].TargetID })
	if b, e := h.requestCostBudget(ctx); e == nil && b != nil {
		report.Budget = b.Report()
	}
	return report
}
func executionErrorFrom(err error) *llm.APIError {
	if err == nil {
		return nil
	}
	var e *llm.APIError
	if errors.As(err, &e) {
		c := *e
		return &c
	}
	return &llm.APIError{Message: err.Error(), Retryable: true}
}
