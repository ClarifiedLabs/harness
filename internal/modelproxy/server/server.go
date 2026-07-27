package server

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net"
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

	"harness/internal/apikey"
	"harness/internal/auth"
	"harness/internal/inputimage"
	"harness/internal/llm"
	"harness/internal/llm/factory"
	"harness/internal/llm/tokencount"
	"harness/internal/metrics"
	"harness/internal/modelcatalog"
	"harness/internal/modelproxy/pricing"
	"harness/internal/modelproxy/protocol"
	"harness/internal/reasoningprofile"
	"harness/internal/tracing"
)

const maxStreamRequestBytes = 64 << 20

var instanceIDPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)

var reasoningProfileRank = map[string]int{
	"none":    0,
	"minimal": 1,
	"low":     2,
	"medium":  3,
	"high":    4,
	"xhigh":   5,
	"max":     6,
}

type Config struct {
	ProviderConfigs      []string      `json:"provider_configs"`
	DefaultContextWindow int           `json:"default_context_window"`
	LogLevel             string        `json:"log_level,omitempty"`
	LogFormat            string        `json:"log_format,omitempty"`
	ModelsDevCacheTTL    Duration      `json:"models_dev_cache_ttl,omitempty"`
	DrainDelay           Duration      `json:"drain_delay,omitempty"`
	ShutdownTimeout      Duration      `json:"shutdown_timeout,omitempty"`
	InstanceID           string        `json:"instance_id,omitempty"`
	APIKeysFile          string        `json:"api_keys_file,omitempty"`
	Metrics              MetricsConfig `json:"metrics,omitempty"`
}

type CostBudgetConfig struct {
	LimitUSD       float64  `json:"limit_usd,omitempty"`
	Period         Duration `json:"period,omitempty"`
	RejectUnpriced bool     `json:"reject_unpriced,omitempty"`
}

func (c CostBudgetConfig) Enabled() bool {
	return budgetEnabled(c)
}

// MetricsConfig toggles the Prometheus /metrics endpoint on a separate port.
// It remains an alias for source compatibility with existing callers.
type MetricsConfig = metrics.Config

// Duration is a JSON duration setting. Strings use Go duration syntax such as
// "24h"; numeric values are seconds. Set distinguishes an explicit zero so
// each caller can apply its own zero-value semantics.
type Duration struct {
	Duration time.Duration
	Set      bool
}

func (d *Duration) UnmarshalJSON(data []byte) error {
	d.Set = true
	var s string
	if err := json.Unmarshal(data, &s); err == nil {
		return d.setString(s)
	}
	var n json.Number
	if err := json.Unmarshal(data, &n); err != nil {
		return fmt.Errorf("duration must be a string like \"24h\" or a number of seconds")
	}
	seconds, err := strconv.ParseInt(n.String(), 10, 64)
	if err != nil {
		return fmt.Errorf("duration seconds must be an integer: %w", err)
	}
	if seconds < 0 {
		return fmt.Errorf("duration must be non-negative")
	}
	d.Duration = time.Duration(seconds) * time.Second
	return nil
}

func (d Duration) MarshalJSON() ([]byte, error) {
	if !d.Set {
		return []byte("null"), nil
	}
	return json.Marshal(d.Duration.String())
}

func (d Duration) IsZero() bool {
	return !d.Set
}

func (d *Duration) setString(s string) error {
	s = strings.TrimSpace(s)
	if s == "0" {
		d.Duration = 0
		return nil
	}
	v, err := time.ParseDuration(s)
	if err != nil {
		return err
	}
	if v < 0 {
		return fmt.Errorf("duration must be non-negative")
	}
	d.Duration = v
	return nil
}

type Options struct {
	ConfigDir string
	Config    Config
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
	wsPool     wsPoolOptions
}

// usageKey identifies an aggregate usage bucket by provider and model.
type usageKey struct {
	targetID string
}

// catalogSnapshot is the immutable served state: a registry used for model
// metadata, a pricer used for request costs, and the catalog served at
// /v1/models. It is swapped atomically when the models.dev cache refreshes so
// managed prices stay fresh without a restart. Readers Load() it; the refresher
// Stores() a freshly built one.
type catalogSnapshot struct {
	registry *llm.Registry
	catalog  protocol.Catalog
	targets  map[string]resolvedTarget
	pricer   pricing.Pricer
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
// per stream request. They are created once in NewHandler so HELP/TYPE always
// appear in exposition even with zero traffic.
type metricsCollectors struct {
	requests     *metrics.Counter
	errors       *metrics.Counter
	input        *metrics.Counter
	output       *metrics.Counter
	cacheRead    *metrics.Counter
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

type Handler struct {
	// snapshot holds the current registry+catalog. Built once in NewHandler and
	// replaced wholesale by UpdateModelsDevCatalog; never mutated in place.
	snapshot atomic.Pointer[catalogSnapshot]

	providers            []llm.ProviderConfig
	authSources          map[string]*auth.Source
	defaultContextWindow int
	configDir            string
	configSourceDate     time.Time
	pricingMaxAge        time.Duration
	getenv               func(string) string
	now                  func() time.Time
	logger               *slog.Logger
	newProvider          func(factory.Options) (llm.Provider, error)
	nextRequestID        atomic.Uint64
	instanceID           string
	startedAt            time.Time

	metrics    *metrics.Registry
	metricFams *metricsCollectors

	keyBudgetMu sync.Mutex
	keyBudgets  map[string]*costBudgetTracker

	usageMu sync.Mutex
	usage   map[usageKey]*protocol.ModelUsage

	wsPool *wsPool
}

func NewHandler(opts Options) (*Handler, error) {
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
	h := &Handler{
		providers:            providers,
		authSources:          authSources,
		defaultContextWindow: defaultWindow,
		configDir:            opts.ConfigDir,
		configSourceDate:     providerConfigSourceDate(opts.ConfigDir, opts.Config.ProviderConfigs),
		pricingMaxAge:        maxAge,
		getenv:               getenv,
		now:                  now,
		logger:               logger,
		newProvider:          newProvider,
		instanceID:           instanceID,
		startedAt:            startedAt,
		keyBudgets:           map[string]*costBudgetTracker{},
		usage:                map[usageKey]*protocol.ModelUsage{},
	}
	if opts.Metrics != nil {
		h.metrics = opts.Metrics
		h.metricFams = registerMetricFamilies(opts.Metrics)
	}
	snapshot, err := h.buildSnapshot(opts.ModelsDevCatalog, opts.ModelsDevSourceDate)
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

func (h *Handler) recordWSPoolEvent(event string) {
	if h.metricFams == nil {
		return
	}
	h.metricFams.wsPoolEvents.Inc(map[string]string{"event": normalizeWSPoolEvent(event)})
}

func (h *Handler) recordContinuation(result string) {
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

// streamPath is the route the proxy streams model responses on.
const streamPath = "/v1/stream"

type authAuthorizer interface {
	Authorize(*http.Request) bool
}

// ObserveAuth wraps the authenticated handler so stream requests rejected by
// API-key auth (401) are still metered. It re-checks store.Authorize for the
// stream route only and records a rejected request before delegating to next
// (which writes the actual 401). When auth is not required store.Authorize is
// always true, so nothing is counted. Counting here, rather than in the apikey
// middleware, keeps that package free of a metrics dependency.
func ObserveAuth(h *Handler, store authAuthorizer, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if store != nil && r.Method == http.MethodPost && r.URL.Path == streamPath && !store.Authorize(r) {
			h.RecordRejectedStream(r)
		}
		next.ServeHTTP(w, r)
	})
}

// RecordRejectedStream meters a stream request rejected before it reaches the
// handler (a 401). provider/model are unknown and omitted; purpose is unknown
// because the authenticated handler never decoded the body, and key is the
// authorizing name or "anonymous".
func (h *Handler) RecordRejectedStream(r *http.Request) {
	if h.metrics == nil || h.metricFams == nil {
		return
	}
	key := "anonymous"
	if name, ok := apikey.AuthorizedName(r); ok {
		key = name
	}
	labels := map[string]string{"key": key, "purpose": string(llm.RequestPurposeUnknown)}
	h.metricFams.requests.Inc(labels)
	h.metricFams.errors.Inc(labels)
	h.recordContinuation("not_offered")
}

// streamFailed reports whether a completed stream should count as an error for
// metrics. A client disconnecting mid-stream cancels the request context and
// surfaces as a stream error, but that is not a server/provider failure, so it
// is excluded; genuine errors and 4xx/5xx statuses still count.
func streamFailed(ctx context.Context, streamErr string, status int) bool {
	if errors.Is(ctx.Err(), context.Canceled) {
		return false
	}
	return streamErr != "" || status >= http.StatusBadRequest
}

// recordMetrics stamps one stream request into the metrics registry. It is called
// once per /v1/stream (including bounded retries), regardless of whether the
// target resolved (empty provider/model labels are omitted) or the model is
// priced, so free models and pre-resolution failures still get counters. Cost is
// recorded only when usage.CostKnown. failed is true when the stream errored or
// returned a 4xx/5xx status. Purpose is normalized to the bounded llm contract;
// key is the authorizing API key's name ("anonymous" when auth is disabled or
// absent).
func (h *Handler) recordMetrics(r *http.Request, providerID, model string, purpose llm.RequestPurpose, usage llm.Usage, duration time.Duration, failed bool) {
	if h.metrics == nil || h.metricFams == nil {
		return
	}
	key := "anonymous"
	if name, ok := apikey.AuthorizedName(r); ok {
		key = name
	}
	labels := map[string]string{
		"provider": providerID,
		"model":    model,
		"purpose":  string(llm.NormalizeRequestPurpose(purpose)),
		"key":      key,
	}
	h.metricFams.requests.Inc(labels)
	h.metricFams.duration.Add(duration.Seconds(), labels)
	if failed {
		h.metricFams.errors.Inc(labels)
	}
	if usage.InputTokens != 0 {
		h.metricFams.input.Add(float64(usage.InputTokens), labels)
	}
	if usage.OutputTokens != 0 {
		h.metricFams.output.Add(float64(usage.OutputTokens), labels)
	}
	if usage.CacheReadTokens != 0 {
		h.metricFams.cacheRead.Add(float64(usage.CacheReadTokens), labels)
	}
	if usage.CacheWriteTokens != 0 {
		h.metricFams.cacheWrite.Add(float64(usage.CacheWriteTokens), labels)
	}
	if usage.CacheWrite1hTokens != 0 {
		h.metricFams.cacheWrite1h.Add(float64(usage.CacheWrite1hTokens), labels)
	}
	if usage.ReasoningTokens != 0 {
		h.metricFams.reasoning.Add(float64(usage.ReasoningTokens), labels)
	}
	if usage.CostKnown {
		h.metricFams.cost.Add(usage.CostUSD, labels)
	}
}

// buildSnapshot resolves managed-provider static pricing schedules from md where
// applicable, then builds the registry and served catalog from the provider
// configs. Manual providers keep their own configured prices. The catalog's
// pricing stamp dates the managed prices to the models.dev cache when any
// provider is managed, and
// to the provider-config mtime otherwise.
func (h *Handler) buildSnapshot(md *modelcatalog.Catalog, mdSourceDate time.Time) (*catalogSnapshot, error) {
	priced, pruned := h.pricedProviders(md)
	pricer := pricing.NewComposite()
	registry := llm.RegistryFromProviderConfigs(priced)
	registry.SetDefaultContextWindow(h.defaultContextWindow)
	catalog, targets, err := catalogFromProviderConfigs(priced, pricer)
	if err != nil {
		if pruned && configuredTargetCount(priced) == 0 {
			catalog := protocol.Catalog{Targets: []protocol.Target{}, Pricing: h.pricingInfo(md, mdSourceDate)}
			return &catalogSnapshot{registry: registry, catalog: catalog, targets: map[string]resolvedTarget{}, pricer: pricer}, nil
		}
		return nil, err
	}
	catalog.Pricing = h.pricingInfo(md, mdSourceDate)
	return &catalogSnapshot{registry: registry, catalog: catalog, targets: targets, pricer: pricer}, nil
}

// UpdateModelsDevCatalog rebuilds the served snapshot with prices resolved from
// md (manual providers unchanged) and swaps it in atomically. The serving
// refresher calls this after a successful models.dev cache refresh so live
// prices reach in-flight cost accounting and /v1/models without a restart.
func (h *Handler) UpdateModelsDevCatalog(md *modelcatalog.Catalog, sourceDate time.Time) {
	snapshot, err := h.buildSnapshot(md, sourceDate)
	if err != nil {
		h.logger.Warn("rebuild catalog snapshot failed", "err", err)
		return
	}
	h.snapshot.Store(snapshot)
}

// pricingInfo dates the served catalog's prices. When any provider is managed
// and a models.dev cache is available, the cache's source date (kept fresh by
// the refresher) wins; otherwise the manual prices are only as fresh as the
// newest provider-config file.
func (h *Handler) pricingInfo(md *modelcatalog.Catalog, mdSourceDate time.Time) *protocol.PricingInfo {
	sourceDate := h.configSourceDate
	if md != nil && !mdSourceDate.IsZero() && anyManagedProvider(h.providers) {
		sourceDate = mdSourceDate
	}
	if sourceDate.IsZero() {
		return nil
	}
	return &protocol.PricingInfo{
		SourceDate:    sourceDate,
		MaxAgeSeconds: int64(h.pricingMaxAge / time.Second),
	}
}

// pricedProviders returns provider configs with static pricing schedules ready
// for the registry and catalog. Managed providers get a fresh copy whose model
// prices and input modalities come from the models.dev cache when applicable;
// when a refreshed cache no longer contains a managed provider/model, the stale
// entry is pruned from the live snapshot with a warning. Manual providers are
// returned unchanged, keeping their own configured prices and metadata.
func (h *Handler) pricedProviders(md *modelcatalog.Catalog) ([]llm.ProviderConfig, bool) {
	out := make([]llm.ProviderConfig, 0, len(h.providers))
	pruned := false
	for _, pc := range h.providers {
		if pc.Name == modelcatalog.OpenAICodexProviderID {
			cp := pc
			cp.Models = make([]llm.ModelEntry, len(pc.Models))
			for j, entry := range pc.Models {
				entry.Price = llm.Price{}
				cp.Models[j] = entry
			}
			out = append(out, cp)
			continue
		}
		if !pc.Managed || md == nil {
			out = append(out, pc)
			continue
		}
		// Managed prices resolve from PriceSource when set, otherwise from the
		// provider's own name.
		priceProvider := pc.PriceSource
		if priceProvider == "" {
			priceProvider = pc.Name
		}
		provider, ok := md.Provider(priceProvider)
		if !ok {
			pruned = true
			h.logger.Warn("managed provider no longer exists in models.dev catalog; removing it from live catalog", "provider", pc.Name, "catalog_provider", priceProvider)
			continue
		}
		cp := pc
		cp.Models = make([]llm.ModelEntry, 0, len(pc.Models))
		for _, entry := range pc.Models {
			info, ok := provider.ModelInfo(entry.Name)
			if !ok {
				pruned = true
				h.logger.Warn("managed model no longer exists in models.dev catalog; removing it from live catalog", "provider", pc.Name, "model", entry.Name, "catalog_provider", priceProvider)
				continue
			}
			entry.Price = info.Price
			entry.InputModalities = append([]string(nil), info.InputModalities...)
			entry.ServiceTiers = llm.NormalizeServiceTiers(info.ServiceTiers)
			cp.Models = append(cp.Models, entry)
		}
		if len(cp.Models) == 0 {
			pruned = true
			h.logger.Warn("managed provider has no models remaining after models.dev refresh; removing it from live catalog", "provider", pc.Name, "catalog_provider", priceProvider)
			continue
		}
		out = append(out, cp)
	}
	return out, pruned
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

// modelsDevPrice bridges a models.dev catalog price into an llm.Price for one
// provider/model. This is the single point where the proxy crosses from
// modelsdev to llm pricing, keeping internal/llm free of a modelsdev import.
func modelsDevPrice(md *modelcatalog.Catalog, providerID, modelID string) (llm.Price, bool) {
	info, ok := modelsDevModelInfo(md, providerID, modelID)
	return info.Price, ok
}

func modelsDevModelInfo(md *modelcatalog.Catalog, providerID, modelID string) (llm.ModelInfo, bool) {
	if md == nil {
		return llm.ModelInfo{}, false
	}
	provider, ok := md.Provider(providerID)
	if !ok {
		return llm.ModelInfo{}, false
	}
	info, ok := provider.ModelInfo(modelID)
	if !ok {
		return llm.ModelInfo{}, false
	}
	return info, true
}

func anyManagedProvider(providers []llm.ProviderConfig) bool {
	for _, pc := range providers {
		if pc.Managed {
			return true
		}
	}
	return false
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

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.Method == http.MethodGet && r.URL.Path == "/v1/models":
		h.handleModels(w, r)
	case r.Method == http.MethodGet && r.URL.Path == "/v1/usage":
		h.handleUsage(w, r)
	case r.Method == http.MethodPost && r.URL.Path == "/v1/input_tokens":
		h.handleInputTokens(w, r)
	case r.Method == http.MethodPost && r.URL.Path == streamPath:
		h.handleStream(w, r)
	default:
		http.NotFound(w, r)
	}
}

func (h *Handler) Catalog() protocol.Catalog {
	return h.snapshot.Load().catalog
}

// InstanceID returns the stable identity stamped into this handler's logs,
// diagnostics, events, and per-process usage report.
func (h *Handler) InstanceID() string {
	if h == nil {
		return ""
	}
	return h.instanceID
}

func (h *Handler) logTracedProxyRequest(r *http.Request, cw *countingResponseWriter, start time.Time, requestBytes int, tc tracing.Context) {
	attrs := []any{
		"requester", requesterName(r),
		"remote_addr", r.RemoteAddr,
		"method", r.Method,
		"path", r.URL.Path,
		"status", cw.statusCode(),
		"request_bytes", requestBytes,
		"response_bytes", cw.bytesWritten(),
		"duration", time.Since(start),
	}
	attrs = append(attrs, tracing.LogAttrs(tc)...)
	if cw.statusCode() >= http.StatusBadRequest {
		h.logger.Warn("model proxy request completed", attrs...)
		return
	}
	h.logger.Info("model proxy request completed", attrs...)
}

func (h *Handler) handleModels(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	tc, traceOK := tracing.TraceFromHeaders(r.Header)
	cw := &countingResponseWriter{ResponseWriter: w}
	defer func() {
		if traceOK {
			h.logTracedProxyRequest(r, cw, start, 0, tc)
		}
	}()
	cw.Header().Set("content-type", "application/json")
	if err := json.NewEncoder(cw).Encode(h.snapshot.Load().catalog); err != nil {
		h.logger.Warn("write model catalog failed", "err", err)
	}
}

func (h *Handler) handleUsage(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("content-type", "application/json")
	if err := json.NewEncoder(w).Encode(h.usageSnapshotForRequest(r)); err != nil {
		h.logger.Warn("write usage report failed", "err", err)
	}
}

// recordUsage accumulates one priced request into the per-model usage map. It is
// called only for requests with known cost, so every bucket has a meaningful
// CostUSD.
func (h *Handler) recordUsage(targetID string, u llm.Usage, cost float64) {
	key := usageKey{targetID: targetID}
	h.usageMu.Lock()
	defer h.usageMu.Unlock()
	acc := h.usage[key]
	if acc == nil {
		acc = &protocol.ModelUsage{TargetID: targetID}
		h.usage[key] = acc
	}
	acc.Requests++
	acc.InputTokens += int64(u.InputTokens)
	acc.OutputTokens += int64(u.OutputTokens)
	acc.CacheReadTokens += int64(u.CacheReadTokens)
	acc.CacheWriteTokens += int64(u.CacheWriteTokens)
	acc.CacheWrite1hTokens += int64(u.CacheWrite1hTokens)
	acc.ReasoningTokens += int64(u.ReasoningTokens)
	acc.CostUSD += cost
}

// usageSnapshot returns a copy of the accumulated usage, sorted by
// provider:model for deterministic output. It omits per-key budget state because
// no authenticated request is available.
func (h *Handler) usageSnapshot() protocol.UsageReport {
	return h.usageSnapshotForRequest(nil)
}

func (h *Handler) usageSnapshotForRequest(r *http.Request) protocol.UsageReport {
	h.usageMu.Lock()
	report := protocol.UsageReport{
		Instance: h.instanceID,
		Since:    h.startedAt,
		Models:   make([]protocol.ModelUsage, 0, len(h.usage)),
	}
	for _, acc := range h.usage {
		report.Models = append(report.Models, *acc)
	}
	h.usageMu.Unlock()
	sort.Slice(report.Models, func(i, j int) bool {
		return report.Models[i].TargetID < report.Models[j].TargetID
	})
	budget, err := h.requestCostBudget(r)
	if err != nil {
		h.logger.Warn("load api key cost budget failed", "err", err)
	} else if budget != nil {
		report.Budget = budget.Report()
	}
	return report
}

func (h *Handler) handleInputTokens(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	tc, traceOK := tracing.TraceFromHeaders(r.Header)
	cw := &countingResponseWriter{ResponseWriter: w}
	reqBytes := 0
	defer func() {
		if traceOK {
			h.logTracedProxyRequest(r, cw, start, reqBytes, tc)
		}
	}()
	body, err := io.ReadAll(http.MaxBytesReader(cw, r.Body, maxStreamRequestBytes))
	reqBytes = len(body)
	if err != nil {
		writeError(cw, http.StatusRequestEntityTooLarge, &protocol.Error{StatusCode: http.StatusRequestEntityTooLarge, Message: "request body too large"})
		return
	}
	var req protocol.TokenCountRequest
	if err := json.Unmarshal(body, &req); err != nil {
		writeError(cw, http.StatusBadRequest, &protocol.Error{StatusCode: http.StatusBadRequest, Message: "malformed input token request"})
		return
	}
	if err := validateProxyRequestMessages(req.Request.Messages); err != nil {
		writeError(cw, http.StatusBadRequest, &protocol.Error{
			StatusCode: http.StatusBadRequest,
			Code:       "invalid_request",
			Message:    "invalid model request content",
		})
		return
	}
	targetID := strings.TrimSpace(req.TargetID)
	if targetID == "" {
		writeError(cw, http.StatusBadRequest, &protocol.Error{StatusCode: http.StatusBadRequest, Message: "target_id is required"})
		return
	}
	target, err := h.resolveTarget(targetID)
	if err != nil {
		writeError(cw, http.StatusBadRequest, &protocol.Error{StatusCode: http.StatusBadRequest, Message: err.Error()})
		return
	}
	if err := applyServiceTierForTarget(target, &req.Request); err != nil {
		writeError(cw, http.StatusBadRequest, &protocol.Error{StatusCode: http.StatusBadRequest, Message: err.Error()})
		return
	}
	req.Request.Model = target.entry.Name
	req.Request.ServerTools = resolveServerToolsForTarget(target, req.Request.ServerTools)
	_, req.Request = prepareProviderRequest(req.Request)
	if codexResponsesUsesLocalTokenCount(target.pc) {
		count := tokencount.EstimateOpenAIChat(req.Request)
		if count <= 0 {
			writeError(cw, http.StatusNotImplemented, &protocol.Error{
				StatusCode: http.StatusNotImplemented,
				Code:       "input_token_count_unsupported",
				Message:    llm.ErrInputTokenCountUnsupported.Error(),
			})
			return
		}
		cw.Header().Set("content-type", "application/json")
		_ = json.NewEncoder(cw).Encode(protocol.TokenCountResponse{
			InputTokens: count,
			Source:      "o200k_base",
		})
		return
	}
	opts, err := h.runtimeOptionsForTarget(r.Context(), target)
	if err != nil {
		writeError(cw, http.StatusBadRequest, &protocol.Error{StatusCode: http.StatusBadRequest, Message: err.Error()})
		return
	}
	provider, err := h.newProvider(opts)
	if err != nil {
		writeError(cw, http.StatusBadRequest, protocol.ErrorFrom(err))
		return
	}
	counter, ok := provider.(llm.InputTokenCounter)
	if !ok {
		writeError(cw, http.StatusNotImplemented, &protocol.Error{
			StatusCode: http.StatusNotImplemented,
			Code:       "input_token_count_unsupported",
			Message:    llm.ErrInputTokenCountUnsupported.Error(),
		})
		return
	}
	count, err := counter.CountInputTokens(r.Context(), req.Request)
	if err != nil {
		if errors.Is(err, llm.ErrInputTokenCountUnsupported) {
			writeError(cw, http.StatusNotImplemented, &protocol.Error{
				StatusCode: http.StatusNotImplemented,
				Code:       "input_token_count_unsupported",
				Message:    err.Error(),
			})
			return
		}
		writeError(cw, http.StatusBadRequest, protocol.ErrorFrom(err))
		return
	}
	cw.Header().Set("content-type", "application/json")
	_ = json.NewEncoder(cw).Encode(protocol.TokenCountResponse{
		InputTokens: count.InputTokens,
		Source:      count.Source,
	})
}

func (h *Handler) handleStream(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	traceCtx, traceOK := tracing.TraceFromHeaders(r.Header)
	requestID := h.nextRequestID.Add(1)
	cw := &countingResponseWriter{ResponseWriter: w}
	var (
		targetID           string
		providerID         string
		apiType            string
		model              string
		purpose            = llm.RequestPurposeUnknown
		usage              llm.Usage
		stop               llm.StopReason
		streamErr          string
		events             int
		toolCalls          int
		reqBytes           int
		errAttrs           []any
		budget             *costBudgetTracker
		requestShape       *llm.MultimodalRequestShape
		finalDiagnostic    *llm.APIErrorDiagnostic
		eventSequence      int
		continuationResult = "not_offered"
	)
	diagnosticFor := func(stage llm.APIErrorStage) *llm.APIErrorDiagnostic {
		diagnostic := &llm.APIErrorDiagnostic{
			Stage:           stage,
			ProxyInstanceID: h.instanceID,
			ProxyRequestID:  requestID,
			TargetID:        targetID,
			Provider:        providerID,
			APIType:         apiType,
			Model:           model,
		}
		if traceOK {
			diagnostic.TraceID = traceCtx.TraceID
			diagnostic.SpanID = traceCtx.SpanID
		}
		return diagnostic
	}
	writeFailure := func(status int, stage llm.APIErrorStage, proxyErr *protocol.Error) {
		finalDiagnostic = diagnosticFor(stage)
		proxyErr.Diagnostic = finalDiagnostic
		errAttrs = []any{
			"err_kind", "api",
			"api_status_code", proxyErr.StatusCode,
			"api_code", proxyErr.Code,
			"api_message", proxyErr.Message,
			"api_retryable", proxyErr.Retryable,
			"api_retry_after_ms", proxyErr.RetryAfterMS,
		}
		writeError(cw, status, proxyErr)
	}
	defer func() {
		attrs := []any{
			"request_id", requestID,
			"requester", requesterName(r),
			"remote_addr", r.RemoteAddr,
			"target_id", targetID,
			"provider", providerID,
			"api_type", apiType,
			"model", model,
			"purpose", purpose,
			"status", cw.statusCode(),
			"request_bytes", reqBytes,
			"response_bytes", cw.bytesWritten(),
			"duration", time.Since(start),
			"events", events,
			"tool_calls", toolCalls,
			"stop_reason", string(stop),
			"input_tokens", usage.InputTokens,
			"output_tokens", usage.OutputTokens,
			"cache_read_tokens", usage.CacheReadTokens,
			"cache_write_tokens", usage.CacheWriteTokens,
			"cache_write_1h_tokens", usage.CacheWrite1hTokens,
			"reasoning_tokens", usage.ReasoningTokens,
		}
		failed := streamFailed(r.Context(), streamErr, cw.statusCode())
		if targetID != "" && usage.CostKnown {
			attrs = append(attrs, "cost_usd", usage.CostUSD)
			h.recordUsage(targetID, usage, usage.CostUSD)
			if budget != nil && !failed {
				if err := budget.Add(usage.CostUSD); err != nil {
					h.logger.Warn("persist api key cost budget state failed", "err", err)
				}
			}
		} else if budget != nil && targetID != "" && !failed {
			h.logger.Warn("api key cost budget enabled but request cost was unknown", "target_id", targetID, "provider", providerID, "model", model)
		}
		if traceOK {
			attrs = append(attrs, tracing.LogAttrs(traceCtx)...)
		}
		attrs = append(attrs, shapeLogAttrs(requestShape)...)
		attrs = append(attrs, diagnosticLogAttrs(finalDiagnostic)...)
		// Record every stream request, even one that failed before the target
		// resolved (provider/model empty), so requests_total/errors_total reflect
		// all client-facing failures, not just post-resolution ones.
		h.recordMetrics(r, providerID, model, purpose, usage, time.Since(start), failed)
		h.recordContinuation(continuationResult)
		if streamErr != "" {
			attrs = append(attrs, "err", streamErr)
			attrs = append(attrs, errAttrs...)
			if !failed && errors.Is(r.Context().Err(), context.Canceled) {
				h.logger.Info("model request completed", attrs...)
			} else {
				h.logger.Warn("model request completed", attrs...)
			}
			return
		}
		if cw.statusCode() >= http.StatusBadRequest {
			h.logger.Warn("model request completed", attrs...)
			return
		}
		h.logger.Info("model request completed", attrs...)
	}()

	body, err := io.ReadAll(http.MaxBytesReader(cw, r.Body, maxStreamRequestBytes))
	reqBytes = len(body)
	if err != nil {
		streamErr = "request body too large"
		writeFailure(http.StatusRequestEntityTooLarge, llm.APIErrorStageProxyDecode, &protocol.Error{StatusCode: http.StatusRequestEntityTooLarge, Message: "request body too large"})
		return
	}
	var req protocol.StreamRequest
	if err := json.Unmarshal(body, &req); err != nil {
		streamErr = "malformed stream request"
		writeFailure(http.StatusBadRequest, llm.APIErrorStageProxyDecode, &protocol.Error{StatusCode: http.StatusBadRequest, Message: "malformed stream request"})
		return
	}
	if req.Request.PreviousResponseID != "" {
		continuationResult = "failed"
	}
	if err := validateProxyRequestMessages(req.Request.Messages); err != nil {
		streamErr = "invalid model request content"
		writeFailure(http.StatusBadRequest, llm.APIErrorStageProxyDecode, &protocol.Error{
			StatusCode: http.StatusBadRequest,
			Code:       "invalid_request",
			Message:    "invalid model request content",
		})
		return
	}
	purpose = llm.NormalizeRequestPurpose(req.Request.Purpose)
	req.Request.Purpose = purpose
	targetID = strings.TrimSpace(req.TargetID)
	if targetID == "" {
		streamErr = "target_id is required"
		writeFailure(http.StatusBadRequest, llm.APIErrorStageProxyResolve, &protocol.Error{StatusCode: http.StatusBadRequest, Message: "target_id is required"})
		return
	}
	target, err := h.resolveTarget(targetID)
	if err != nil {
		streamErr = err.Error()
		writeFailure(http.StatusBadRequest, llm.APIErrorStageProxyResolve, &protocol.Error{StatusCode: http.StatusBadRequest, Message: err.Error()})
		return
	}
	targetID = target.targetID
	providerID = target.pc.Name
	apiType = target.pc.APIType
	model = target.entry.Name
	if err := applyServiceTierForTarget(target, &req.Request); err != nil {
		streamErr = err.Error()
		writeFailure(http.StatusBadRequest, llm.APIErrorStageProxyPrepare, &protocol.Error{StatusCode: http.StatusBadRequest, Message: err.Error()})
		return
	}
	req.Request.Model = model
	req.Request.ServerTools = resolveServerToolsForTarget(target, req.Request.ServerTools)
	req.Request.Reasoning = h.reasoningForTarget(target, req.ReasoningProfile, req.Request.Reasoning)
	sessionKey, providerRequest := prepareProviderRequest(req.Request)
	req.Request = providerRequest
	stateful := providerContinuationStateful(target.pc)
	if !stateful && (req.Request.StoreResponse || req.Request.PreviousResponseID != "") {
		streamErr = "target does not support provider continuation"
		writeFailure(http.StatusBadRequest, llm.APIErrorStageProxyPrepare, &protocol.Error{
			StatusCode: http.StatusBadRequest,
			Code:       protocol.ErrCodeContinuationUnsupported,
			Message:    streamErr,
			Retryable:  false,
		})
		return
	}
	if requestBudget, ok, message := h.checkCostBudget(cw, r, target, req.Request, diagnosticFor(llm.APIErrorStageProxyPrepare)); !ok {
		streamErr = message
		finalDiagnostic = diagnosticFor(llm.APIErrorStageProxyPrepare)
		errAttrs = []any{
			"err_kind", "api",
			"api_status_code", cw.statusCode(),
			"api_message", message,
		}
		return
	} else {
		budget = requestBudget
	}
	opts, err := h.runtimeOptionsForTarget(r.Context(), target)
	if err != nil {
		streamErr = err.Error()
		writeFailure(http.StatusBadRequest, llm.APIErrorStageProviderRuntime, &protocol.Error{StatusCode: http.StatusBadRequest, Message: err.Error()})
		return
	}
	apiType = opts.Provider
	semanticRequest := req.Request
	semanticRequest.Messages = append([]llm.Message(nil), req.Request.Messages...)
	provider, releaseProvider, err := h.streamProvider(opts, target.baseTargetID, sessionKey)
	if err != nil {
		streamErr = err.Error()
		writeFailure(http.StatusBadRequest, llm.APIErrorStageProviderRuntime, protocol.ErrorFrom(err))
		return
	}
	defer releaseProvider()
	if req.Request.PreviousResponseID != "" {
		if availability, ok := provider.(responseContinuationAvailability); ok &&
			!availability.CanContinueResponse(req.Request.PreviousResponseID) {
			continuationResult = "unavailable"
			streamErr = "previous response is unavailable on this proxy instance"
			writeFailure(http.StatusConflict, llm.APIErrorStageProxyPrepare, &protocol.Error{
				StatusCode: http.StatusConflict,
				Code:       protocol.ErrCodePreviousResponseUnavailable,
				Message:    streamErr,
				Retryable:  false,
			})
			return
		}
	}

	cw.Header().Set("content-type", protocol.ContentTypeNDJSON)
	cw.WriteHeader(http.StatusOK)
	var flusher http.Flusher = cw
	enc := json.NewEncoder(cw)
	flush := func() {
		if flusher != nil {
			flusher.Flush()
		}
	}
	enrichModelRequestEvent := func(event llm.ModelRequestEvent) llm.ModelRequestEvent {
		eventSequence++
		event.Sequence = eventSequence
		event.ProxyInstanceID = h.instanceID
		event.ProxyRequestID = requestID
		event.TargetID = targetID
		event.Provider = providerID
		event.APIType = apiType
		event.Model = model
		event.Purpose = purpose
		event.ElapsedMS = time.Since(start).Milliseconds()
		if traceOK {
			event.TraceID = traceCtx.TraceID
			event.SpanID = traceCtx.SpanID
		}
		if event.UpstreamRequestID == "" && finalDiagnostic != nil {
			event.UpstreamRequestID = finalDiagnostic.UpstreamRequestID
		}
		return event
	}
	writeModelRequestEvent := func(event llm.ModelRequestEvent) bool {
		event = enrichModelRequestEvent(event)
		switch event.State {
		case llm.ModelRequestUpstreamAttemptFailed, llm.ModelRequestFailed:
			h.logger.Warn("model upstream attempt failed", modelRequestEventLogAttrs(event)...)
		case llm.ModelRequestRetryScheduled:
			h.logger.Info("model upstream retry scheduled", modelRequestEventLogAttrs(event)...)
		case llm.ModelRequestCancelled:
			h.logger.Info("model request cancelled", modelRequestEventLogAttrs(event)...)
		}
		streamEvent := llm.StreamEvent{Kind: llm.EventModelRequest, ModelRequest: &event}
		if err := enc.Encode(protocol.StreamEnvelope{Event: &streamEvent}); err != nil {
			streamErr = err.Error()
			return false
		}
		flush()
		return true
	}

	accepted := enrichModelRequestEvent(llm.ModelRequestEvent{
		State: llm.ModelRequestAccepted,
	})
	h.logger.Info("model request started", modelRequestEventLogAttrs(accepted)...)
	acceptedEvent := llm.StreamEvent{Kind: llm.EventModelRequest, ModelRequest: &accepted}
	if err := enc.Encode(protocol.StreamEnvelope{Event: &acceptedEvent}); err != nil {
		streamErr = err.Error()
		return
	}
	flush()
	stopCancellationLog := context.AfterFunc(r.Context(), func() {
		cancelled := accepted
		cancelled.State = llm.ModelRequestCancelled
		cancelled.ElapsedMS = time.Since(start).Milliseconds()
		h.logger.Info("model request cancellation observed", modelRequestEventLogAttrs(cancelled)...)
	})
	defer stopCancellationLog()

	type streamRetry int
	const (
		streamRetryNone streamRetry = iota
		streamRetryServerTools
		streamRetryMinOutputTokens
	)
	retryMinOutputTokens := 0
	streamAttempt := func(request, semanticRequest llm.Request) streamRetry {
		attemptStart := time.Now()
		semanticRequest.StoreResponse = request.StoreResponse
		semanticRequest.PreviousResponseID = request.PreviousResponseID
		semanticRequest.MaxTokens = request.MaxTokens
		semanticRequest.ServerTools = request.ServerTools
		streamErr = ""
		errAttrs = nil
		requestShape = richToolResultShape(semanticRequest, apiType)
		finalDiagnostic = nil
		sentEvents := false
		attemptToolCalls := 0
		var pendingTerminalEvent *llm.ModelRequestEvent
		cancelEventSent := false
		for ev, err := range provider.Stream(r.Context(), request) {
			if err != nil {
				rawErr := err
				if errors.Is(rawErr, context.Canceled) || errors.Is(rawErr, context.DeadlineExceeded) {
					streamErr = rawErr.Error()
					errAttrs = streamErrorLogAttrs(rawErr)
					if !cancelEventSent {
						writeModelRequestEvent(llm.ModelRequestEvent{
							State:   llm.ModelRequestCancelled,
							Outcome: llm.ModelRequestOutcomeTerminal,
							Message: rawErr.Error(),
						})
					}
					_ = enc.Encode(protocol.StreamEnvelope{Error: protocol.ErrorFrom(rawErr)})
					flush()
					return streamRetryNone
				}
				diagnostic := diagnosticFor(upstreamErrorStage(rawErr))
				diagnostic.UpstreamRequestID = upstreamRequestIDFromError(rawErr)
				diagnostic.MultimodalShape = requestShape
				diagnostic.Compatibility = classifyMultimodalToolResultRejection(rawErr, requestShape)
				finalDiagnostic = diagnostic
				err = redactImageBearingError(rawErr, semanticRequest)
				err = withAPIErrorDiagnostic(err, diagnostic)
				streamErr = err.Error()
				errAttrs = streamErrorLogAttrs(err)
				if !sentEvents && request.PreviousResponseID != "" && previousResponseRejected(err) {
					continuationResult = "rejected_upstream"
				}

				retryKind := streamRetryNone
				if !sentEvents && len(request.ServerTools) > 0 && serverToolRejected(err) {
					retryKind = streamRetryServerTools
				}
				if retryKind == streamRetryNone && !sentEvents {
					if floor, ok := outputTokenFloorRejected(err); ok && floor > request.MaxTokens {
						retryMinOutputTokens = floor
						h.logger.Warn("retrying model request with higher output token floor",
							"request_id", requestID,
							"target_id", targetID,
							"provider", providerID,
							"api_type", apiType,
							"model", model,
							"configured_min_output_tokens", target.pc.MinOutputTokens,
							"inferred_min_output_tokens", floor,
							"original_max_tokens", request.MaxTokens,
							"retry_max_tokens", floor,
							"err", redactImageBearingError(err, semanticRequest).Error(),
						)
						retryKind = streamRetryMinOutputTokens
					}
				}

				issue := modelRequestEventFromError(err, llm.ModelRequestFailed, llm.ModelRequestOutcomeTerminal, time.Since(attemptStart))
				if pendingTerminalEvent != nil {
					issue = mergeModelRequestFailure(*pendingTerminalEvent, issue)
				}
				if retryKind != streamRetryNone {
					issue.State = llm.ModelRequestUpstreamAttemptFailed
					issue.Outcome = llm.ModelRequestOutcomeRetrying
					if !writeModelRequestEvent(issue) {
						return streamRetryNone
					}
					retryEvent := issue
					retryEvent.State = llm.ModelRequestRetryScheduled
					retryEvent.Outcome = ""
					retryEvent.Attempt++
					retryEvent.AttemptDurationMS = 0
					if !writeModelRequestEvent(retryEvent) {
						return streamRetryNone
					}
					return retryKind
				}
				if !writeModelRequestEvent(issue) {
					return streamRetryNone
				}
				_ = enc.Encode(protocol.StreamEnvelope{Error: protocol.ErrorFrom(err)})
				flush()
				return streamRetryNone
			}
			if ev.Kind == llm.EventModelRequest && ev.ModelRequest != nil {
				status := redactModelRequestEvent(*ev.ModelRequest, semanticRequest)
				if status.State == llm.ModelRequestCancelled {
					cancelEventSent = true
				}
				if status.State == llm.ModelRequestUpstreamAttemptFailed && status.Outcome == llm.ModelRequestOutcomeTerminal {
					pendingTerminalEvent = &status
					continue
				}
				if !writeModelRequestEvent(status) {
					return streamRetryNone
				}
				continue
			}
			sentEvents = true
			events++
			if ev.Usage != nil {
				usage = mergeUsage(usage, *ev.Usage)
				usage = h.priceUsage(targetID, request, usage)
				ev.Usage = &usage
			}
			switch ev.Kind {
			case llm.EventToolCallDone:
				toolCalls++
				attemptToolCalls++
			case llm.EventDone:
				if attemptToolCalls > 0 {
					ev.StopReason = llm.StopToolUse
				}
				stop = ev.StopReason
				if ev.Usage != nil {
					usage = mergeUsage(usage, *ev.Usage)
				}
				usage = h.priceUsage(targetID, request, usage)
				ev.Usage = &usage
			}
			event := ev
			if err := enc.Encode(protocol.StreamEnvelope{Event: &event}); err != nil {
				streamErr = err.Error()
				return streamRetryNone
			}
			flush()
		}
		if pendingTerminalEvent != nil {
			if !writeModelRequestEvent(*pendingTerminalEvent) {
				return streamRetryNone
			}
		}
		return streamRetryNone
	}
	attemptRequest := req.Request
	retry := streamAttempt(attemptRequest, semanticRequest)
	for retry != streamRetryNone {
		switch retry {
		case streamRetryServerTools:
			attemptRequest.ServerTools = nil
			semanticRequest.ServerTools = nil
		case streamRetryMinOutputTokens:
			attemptRequest.MaxTokens = retryMinOutputTokens
			semanticRequest.MaxTokens = retryMinOutputTokens
		default:
			retry = streamRetryNone
			continue
		}
		retry = streamAttempt(attemptRequest, semanticRequest)
	}
	if streamErr == "" {
		if req.Request.PreviousResponseID != "" {
			continuationResult = "served"
		}
		writeModelRequestEvent(llm.ModelRequestEvent{
			State:     llm.ModelRequestCompleted,
			ElapsedMS: time.Since(start).Milliseconds(),
		})
	}
}

func validateProxyRequestMessages(messages []llm.Message) error {
	if _, err := inputimage.ValidateMessages(messages); err != nil {
		return err
	}
	return llm.ValidateTranscript(messages)
}

func (h *Handler) streamProvider(opts factory.Options, providerID, promptCacheKey string) (llm.Provider, func(), error) {
	if !opts.ResponsesWebSocket {
		provider, err := h.newProvider(opts)
		return provider, func() {}, err
	}
	key := streamProviderCacheKey(opts, providerID, promptCacheKey)
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
		ProviderID        string `json:"provider_id"`
		Provider          string `json:"provider"`
		ProviderName      string `json:"provider_name"`
		Model             string `json:"model"`
		BaseURL           string `json:"base_url"`
		ContextWindow     int    `json:"context_window"`
		OutputLimit       int    `json:"output_limit"`
		MinOutputTokens   int    `json:"min_output_tokens"`
		OmitMaxOutput     bool   `json:"omit_max_output_tokens"`
		APIKey            string `json:"api_key"`
		AuthHeadersSHA256 string `json:"auth_headers_sha256"`
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
	})
	return wsPoolKey{
		Connection: sha256.Sum256(connectionJSON),
		Session:    sha256.Sum256([]byte(promptCacheKey)),
	}
}

// Close stops transport maintenance and releases idle provider connections.
// Active leases retire and close when their request finishes.
func (h *Handler) Close() error {
	if h == nil || h.wsPool == nil {
		return nil
	}
	return h.wsPool.Close()
}

type countingResponseWriter struct {
	http.ResponseWriter
	status int
	bytes  int
}

func (w *countingResponseWriter) WriteHeader(status int) {
	if w.status == 0 {
		w.status = status
		w.ResponseWriter.WriteHeader(status)
	}
}

func (w *countingResponseWriter) Write(b []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	n, err := w.ResponseWriter.Write(b)
	w.bytes += n
	return n, err
}

func (w *countingResponseWriter) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (w *countingResponseWriter) statusCode() int {
	if w.status == 0 {
		return http.StatusOK
	}
	return w.status
}

func (w *countingResponseWriter) bytesWritten() int {
	return w.bytes
}

func requesterName(r *http.Request) string {
	if v := strings.TrimSpace(r.Header.Get("X-Harness-Requester")); v != "" {
		return v
	}
	if v := strings.TrimSpace(r.UserAgent()); v != "" {
		return v
	}
	if host, _, err := net.SplitHostPort(r.RemoteAddr); err == nil && host != "" {
		return host
	}
	return r.RemoteAddr
}

func streamErrorLogAttrs(err error) []any {
	attrs := []any{"err_go_type", fmt.Sprintf("%T", err)}
	var apiErr *llm.APIError
	if errors.As(err, &apiErr) {
		return append(attrs,
			"err_kind", "api",
			"api_status_code", apiErr.StatusCode,
			"api_code", apiErr.Code,
			"api_message", apiErr.Message,
			"api_retryable", apiErr.Retryable,
			"api_retry_after_ms", apiErr.RetryAfter.Milliseconds(),
		)
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
	return current
}

func redactModelRequestEvent(event llm.ModelRequestEvent, req llm.Request) llm.ModelRequestEvent {
	if event.Message == "" && event.Code == "" {
		return event
	}
	err := redactImageBearingError(&llm.APIError{
		StatusCode: event.StatusCode,
		Code:       event.Code,
		Message:    event.Message,
		Retryable:  event.Retryable,
		RetryAfter: time.Duration(event.RetryAfterMS) * time.Millisecond,
	}, req)
	var apiErr *llm.APIError
	if errors.As(err, &apiErr) {
		event.Code = apiErr.Code
		event.Message = apiErr.Message
		return event
	}
	event.Message = err.Error()
	return event
}

func modelRequestEventLogAttrs(event llm.ModelRequestEvent) []any {
	return []any{
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

func (h *Handler) runtimeOptionsForTarget(ctx context.Context, target resolvedTarget) (factory.Options, error) {
	pc := target.pc
	entry := target.entry
	apiType := pc.APIType
	if apiType == "" {
		apiType = pc.Name
	}
	apiKey := ""
	var authHeaders map[string]string
	if src := h.authSources[pc.Name]; src != nil {
		var err error
		authHeaders, err = src.Headers(ctx)
		if err != nil {
			return factory.Options{}, err
		}
	} else {
		for _, name := range pc.APIKeyEnv {
			if value := h.getenv(name); value != "" {
				apiKey = value
				break
			}
		}
		if apiKey == "" {
			apiKey = providerAPIKeyEnv(apiType, h.getenv)
		}
		if apiKey == "" {
			apiKey = pc.APIKey
		}
	}
	contextWindow := entry.ContextWindow
	if contextWindow <= 0 {
		contextWindow = h.defaultContextWindow
	}
	return factory.Options{
		Provider:            apiType,
		ProviderName:        pc.Name,
		Model:               entry.Name,
		BaseURL:             pc.BaseURL,
		APIKey:              apiKey,
		AuthHeaders:         authHeaders,
		ContextWindow:       contextWindow,
		OutputLimit:         entry.OutputLimit,
		MinOutputTokens:     pc.MinOutputTokens,
		PromptCache:         pc.PromptCache,
		OmitMaxOutputTokens: providerOmitMaxOutputTokens(pc),
		ResponsesWebSocket:  providerResponsesWebSocket(pc),
	}, nil
}

func (h *Handler) resolveTarget(id string) (resolvedTarget, error) {
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

func (h *Handler) checkCostBudget(w http.ResponseWriter, r *http.Request, target resolvedTarget, request llm.Request, diagnostic *llm.APIErrorDiagnostic) (*costBudgetTracker, bool, string) {
	budget, err := h.requestCostBudget(r)
	if err != nil {
		msg := err.Error()
		writeError(w, http.StatusInternalServerError, &protocol.Error{StatusCode: http.StatusInternalServerError, Code: "cost_budget_state_error", Message: msg, Diagnostic: diagnostic})
		return nil, false, msg
	}
	if budget == nil {
		return nil, true, ""
	}
	snapshot := h.snapshot.Load()
	if snapshot == nil || snapshot.pricer == nil {
		msg := "api key cost budget is enabled but pricing is unavailable"
		if budget.RejectUnpriced() {
			writeError(w, http.StatusBadRequest, &protocol.Error{StatusCode: http.StatusBadRequest, Code: "cost_budget_unpriced_target", Message: msg, Diagnostic: diagnostic})
			return budget, false, msg
		}
		return budget, true, ""
	}
	price := snapshot.pricer.PriceUsage(pricing.Input{
		TargetID: target.targetID,
		Provider: target.pc,
		Model:    target.entry,
		Request:  request,
	})
	if !price.Known {
		msg := fmt.Sprintf("api key cost budget is enabled but target %q has no known price", target.targetID)
		if budget.RejectUnpriced() {
			writeError(w, http.StatusBadRequest, &protocol.Error{StatusCode: http.StatusBadRequest, Code: "cost_budget_unpriced_target", Message: msg, Diagnostic: diagnostic})
			return budget, false, msg
		}
		return budget, true, ""
	}
	if ok, retryAfter := budget.Check(); !ok {
		if retryAfter < 0 {
			retryAfter = 0
		}
		retrySeconds := int(math.Ceil(retryAfter.Seconds()))
		w.Header().Set("Retry-After", strconv.Itoa(retrySeconds))
		msg := fmt.Sprintf("api key cost budget exhausted; retry after %s", retryAfter.Round(time.Second))
		writeError(w, http.StatusTooManyRequests, &protocol.Error{
			StatusCode:   http.StatusTooManyRequests,
			Code:         "cost_budget_exceeded",
			Message:      msg,
			Retryable:    true,
			RetryAfterMS: retryAfter.Milliseconds(),
			Diagnostic:   diagnostic,
		})
		return budget, false, msg
	}
	return budget, true, ""
}

func (h *Handler) requestCostBudget(r *http.Request) (*costBudgetTracker, error) {
	entry, ok := apikey.AuthorizedEntry(r)
	if !ok || entry.CostBudget == nil || !entry.CostBudget.Enabled() {
		return nil, nil
	}
	cfg := CostBudgetConfig{
		LimitUSD:       entry.CostBudget.LimitUSD,
		Period:         Duration{Duration: time.Duration(entry.CostBudget.PeriodSeconds) * time.Second, Set: true},
		RejectUnpriced: entry.CostBudget.RejectUnpriced,
	}
	statePath := h.keyBudgetStatePath(entry)
	cacheKey := strings.Join([]string{
		statePath,
		strconv.FormatFloat(cfg.LimitUSD, 'g', -1, 64),
		strconv.FormatInt(entry.CostBudget.PeriodSeconds, 10),
		strconv.FormatBool(cfg.RejectUnpriced),
	}, "|")
	h.keyBudgetMu.Lock()
	defer h.keyBudgetMu.Unlock()
	if h.keyBudgets == nil {
		h.keyBudgets = map[string]*costBudgetTracker{}
	}
	if budget := h.keyBudgets[cacheKey]; budget != nil {
		return budget, nil
	}
	budget, err := newCostBudgetTrackerAtPath(cfg, statePath, h.now)
	if err != nil {
		return nil, err
	}
	h.keyBudgets[cacheKey] = budget
	return budget, nil
}

func (h *Handler) keyBudgetStatePath(entry apikey.Entry) string {
	hash := hex.EncodeToString(entry.Hash)
	if len(hash) > 16 {
		hash = hash[:16]
	}
	name := entry.Name
	if name == "" {
		name = "key"
	}
	filename := name
	if hash != "" {
		filename += "-" + hash
	}
	return filepath.Join(h.configDir, "state", "api_key_budgets", filename+".json")
}

func (h *Handler) priceUsage(targetID string, request llm.Request, usage llm.Usage) llm.Usage {
	snapshot := h.snapshot.Load()
	if snapshot == nil || snapshot.pricer == nil {
		return usage
	}
	target, ok := snapshot.targets[targetID]
	if !ok {
		return usage
	}
	res := snapshot.pricer.PriceUsage(pricing.Input{
		TargetID: targetID,
		Provider: target.pc,
		Model:    target.entry,
		Request:  request,
		Usage:    usage,
	})
	if !res.Known {
		return usage
	}
	usage.CostUSD = res.CostUSD
	usage.CostKnown = true
	return usage
}

func (h *Handler) reasoningForTarget(target resolvedTarget, profile string, requested llm.ReasoningConfig) llm.ReasoningConfig {
	if profile == "" {
		profile = requested.Profile
	}
	profile = normalizeReasoningProfile(profile)
	info := modelEntryReasoning(target.entry)
	summary := requested.Summary
	if info != nil && !info.SupportsSummaries() {
		summary = ""
	}
	if profile == "" {
		return llm.ReasoningConfig{Summary: summary}
	}
	if info == nil || !info.Supported {
		return llm.ReasoningConfig{Summary: summary}
	}
	mode := reasoningModeForProviderConfig(target.pc)
	out := llm.ReasoningConfig{Profile: profile, Summary: summary}
	switch profile {
	case "none":
		if info.SupportsToggle() {
			disabled := false
			out.Enabled = &disabled
		}
	case "minimal", "low", "medium", "high", "xhigh", "max":
		if effort := mappedReasoningEffort(info, profile); effort != "" {
			out.Effort = effort
		} else if budget, ok := mappedReasoningBudget(info, profile); ok {
			out.BudgetTokens = &budget
		}
	}
	if mode == "responses" {
		out.Enabled = nil
	}
	if mode == "openai" && out.Enabled != nil {
		out.Enabled = nil
	}
	if profile == "none" && out.Enabled == nil {
		return llm.ReasoningConfig{}
	}
	return out
}

func normalizeReasoningProfile(profile string) string {
	if normalized, ok := reasoningprofile.Normalize(profile); ok {
		return normalized
	}
	return strings.ToLower(strings.TrimSpace(profile))
}

func mappedReasoningEffort(info *llm.ReasoningInfo, profile string) string {
	values, ok := info.EffortValues()
	if !ok {
		if len(info.Options) > 0 {
			return ""
		}
		if profile == "minimal" {
			return "low"
		}
		if profile == "max" {
			return "high"
		}
		if profile == "xhigh" {
			return "high"
		}
		return profile
	}
	if len(values) == 0 {
		if profile == "minimal" {
			return "low"
		}
		if profile == "max" {
			return "high"
		}
		if profile == "xhigh" {
			return "high"
		}
		return profile
	}
	type candidate struct {
		value string
		rank  int
	}
	var candidates []candidate
	seen := map[string]bool{}
	for _, value := range values {
		clean := strings.ToLower(strings.TrimSpace(value))
		if clean == "" || clean == "none" || seen[clean] {
			continue
		}
		rank, ok := reasoningProfileRank[clean]
		if !ok {
			continue
		}
		candidates = append(candidates, candidate{value: clean, rank: rank})
		seen[clean] = true
	}
	if len(candidates) == 0 {
		return ""
	}
	switch profile {
	case "minimal":
		best := candidates[0]
		for _, c := range candidates[1:] {
			if c.rank < best.rank {
				best = c
			}
		}
		return best.value
	case "max":
		best := candidates[0]
		for _, c := range candidates[1:] {
			if c.rank > best.rank {
				best = c
			}
		}
		return best.value
	}
	targetRank, ok := reasoningProfileRank[profile]
	if !ok {
		return ""
	}
	best := candidates[0]
	bestDistance := absInt(best.rank - targetRank)
	for _, c := range candidates[1:] {
		distance := absInt(c.rank - targetRank)
		if distance < bestDistance || (distance == bestDistance && c.rank < best.rank) {
			best = c
			bestDistance = distance
		}
	}
	return best.value
}

func mappedReasoningBudget(info *llm.ReasoningInfo, profile string) (int, bool) {
	minPtr, maxPtr, ok := info.BudgetTokenRange()
	if !ok {
		return 0, false
	}
	minBudget := 0
	if minPtr != nil {
		minBudget = *minPtr
	}
	if maxPtr == nil {
		if minBudget <= 0 {
			return 0, false
		}
		return minBudget, true
	}
	if *maxPtr <= 0 {
		return 0, false
	}
	maxBudget := *maxPtr
	if minBudget > maxBudget {
		minBudget = maxBudget
	}
	var budget int
	switch profile {
	case "minimal":
		budget = int(math.Ceil(float64(maxBudget) * 0.05))
		if budget < 1 {
			budget = 1
		}
	case "low":
		budget = int(math.Round(float64(maxBudget) * 0.25))
	case "medium":
		budget = int(math.Round(float64(maxBudget) * 0.50))
	case "high":
		budget = int(math.Round(float64(maxBudget) * 0.75))
	case "xhigh":
		budget = int(math.Round(float64(maxBudget) * 0.90))
	case "max":
		budget = maxBudget
	default:
		return 0, false
	}
	if budget < minBudget {
		budget = minBudget
	}
	if budget > maxBudget {
		budget = maxBudget
	}
	return budget, true
}

func absInt(n int) int {
	if n < 0 {
		return -n
	}
	return n
}

func reasoningModeForProviderConfig(pc llm.ProviderConfig) string {
	apiType := strings.ToLower(strings.TrimSpace(pc.APIType))
	if apiType == "" {
		apiType = strings.ToLower(strings.TrimSpace(pc.Name))
	}
	if apiType == "anthropic" || apiType == "responses" {
		return apiType
	}
	if strings.EqualFold(pc.Name, "google") || strings.Contains(strings.ToLower(pc.BaseURL), "generativelanguage.googleapis.com") {
		return "google"
	}
	if strings.EqualFold(pc.Name, "openrouter") || strings.Contains(strings.ToLower(pc.BaseURL), "openrouter.ai") {
		return "openrouter"
	}
	return "openai"
}

func codexResponsesUsesLocalTokenCount(pc llm.ProviderConfig) bool {
	apiType := strings.ToLower(strings.TrimSpace(pc.APIType))
	if apiType == "" {
		apiType = strings.ToLower(strings.TrimSpace(pc.Name))
	}
	if apiType != "responses" {
		return false
	}
	if strings.EqualFold(pc.Name, modelcatalog.OpenAICodexProviderID) {
		return true
	}
	if pc.Auth != nil && strings.EqualFold(pc.Auth.Type, auth.TypeCodexOAuth) {
		return true
	}
	return strings.Contains(strings.ToLower(pc.BaseURL), "chatgpt.com/backend-api/codex")
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
	if pc.Auth == nil || !strings.EqualFold(strings.TrimSpace(pc.Auth.Type), auth.TypeCodexOAuth) {
		return false
	}
	apiType := pc.APIType
	if apiType == "" {
		apiType = pc.Name
	}
	return strings.EqualFold(strings.TrimSpace(apiType), "responses")
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

func writeError(w http.ResponseWriter, status int, e *protocol.Error) {
	if e == nil {
		e = &protocol.Error{StatusCode: status, Message: http.StatusText(status)}
	}
	if e.StatusCode == 0 {
		e.StatusCode = status
	}
	w.Header().Set("content-type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(e)
}

func LoadConfig(path string) (Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Config{}, err
	}
	var cfg Config
	if err := json.Unmarshal(data, &cfg); err != nil {
		return Config{}, err
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return Config{}, err
	}
	if _, ok := raw["api_keys"]; ok {
		return Config{}, fmt.Errorf("api_keys is no longer supported in config; move entries to %s under {\"api_keys\": [...]} and remove api_keys from config", ResolveAPIKeysFile(path, cfg.APIKeysFile, ""))
	}
	if _, ok := raw["cost_budget"]; ok {
		return Config{}, fmt.Errorf("cost_budget is no longer supported in model proxy config; generate model-proxy API keys with -budget-usd and -budget-period instead")
	}
	return cfg, nil
}

// ResolveAPIKeysFile applies flag > config > conventional-default precedence for
// the model proxy's dedicated accepted-key file. Relative config values resolve
// relative to the proxy config directory; a relative flag value is left relative
// to the caller's working directory.
func ResolveAPIKeysFile(configPath, configValue, flagValue string) string {
	if flagValue != "" {
		return flagValue
	}
	configDir := filepath.Dir(configPath)
	if configValue != "" {
		if filepath.IsAbs(configValue) {
			return configValue
		}
		return filepath.Join(configDir, configValue)
	}
	return filepath.Join(configDir, "api_keys.json")
}

func ConfigPath(argsPath string, explicit bool, getenv func(string) string) string {
	if explicit {
		return argsPath
	}
	def := filepath.Join(DefaultConfigDir(getenv), "config.json")
	if _, err := os.Stat(def); err == nil {
		return def
	}
	return ""
}

func DefaultConfigDir(getenv func(string) string) string {
	if getenv == nil {
		getenv = os.Getenv
	}
	if home := getenv("HOME"); home != "" {
		return filepath.Join(home, ".config", "harness-model-proxy")
	}
	return filepath.Join(os.TempDir(), "harness-model-proxy-config")
}

func catalogFromProviderConfigs(providers []llm.ProviderConfig, pricer pricing.Pricer) (protocol.Catalog, map[string]resolvedTarget, error) {
	out := protocol.Catalog{}
	aliasCounts := map[string]int{}
	for _, pc := range providers {
		for _, entry := range pc.Models {
			if entry.Name != "" {
				aliasCounts[entry.Name]++
				for _, tier := range llm.ModelServiceTiers(pc, entry) {
					if !defaultServiceTier(tier) {
						aliasCounts[entry.Name+":"+tier.ID]++
					}
				}
			}
		}
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
			if _, exists := targets[id]; exists {
				return protocol.Catalog{}, nil, fmt.Errorf("model proxy: target %q collides with another target or alias", id)
			}
			aliases := []string{id}
			if aliasCounts[entry.Name] == 1 {
				aliases = append(aliases, entry.Name)
			}
			price := entry.Price
			if pricer != nil {
				if catalogPricing := pricer.CatalogPricing(pc, entry); catalogPricing.Known {
					price = catalogPricing.Price
				} else {
					price = llm.Price{}
				}
			}
			target := protocol.Target{
				ID:                   id,
				Aliases:              aliases,
				DisplayName:          entry.Name,
				ProviderLabel:        pc.Name,
				ModelLabel:           entry.Name,
				ContextWindow:        entry.ContextWindow,
				OutputLimit:          entry.OutputLimit,
				InputModalities:      append([]string(nil), entry.InputModalities...),
				ServerTools:          targetServerTools(pc, entry),
				APIType:              pc.APIType,
				ContinuationStateful: providerContinuationStateful(pc),
				Price:                price,
				Reasoning:            targetReasoningSupported(entry),
			}
			out.Targets = append(out.Targets, target)
			rt := resolvedTarget{targetID: id, baseTargetID: id, pc: pc, entry: entry}
			for _, alias := range aliases {
				if existing, exists := targets[alias]; exists && existing.targetID != rt.targetID {
					return protocol.Catalog{}, nil, fmt.Errorf("model proxy: target alias %q collides with target %q", alias, existing.targetID)
				}
				targets[alias] = rt
			}
			for _, tier := range llm.ModelServiceTiers(pc, entry) {
				if defaultServiceTier(tier) {
					continue
				}
				variantID := id + ":" + tier.ID
				if _, exists := targets[variantID]; exists {
					return protocol.Catalog{}, nil, fmt.Errorf("model proxy: target variant %q collides with another target", variantID)
				}
				variantAliases := []string{variantID}
				if aliasCounts[entry.Name+":"+tier.ID] == 1 {
					variantAliases = append(variantAliases, entry.Name+":"+tier.ID)
				}
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
						return protocol.Catalog{}, nil, fmt.Errorf("model proxy: target alias %q collides with target %q", alias, existing.targetID)
					}
					targets[alias] = variantRT
				}
			}
		}
	}
	if len(out.Targets) == 0 {
		return protocol.Catalog{}, nil, fmt.Errorf("model proxy: no configured models")
	}
	return out, targets, nil
}

func defaultServiceTier(tier llm.ServiceTier) bool {
	if tier.Request.Speed != "" || len(tier.Request.Betas) > 0 {
		return false
	}
	switch strings.ToLower(strings.TrimSpace(tier.Request.ServiceTier)) {
	case "", "default", "standard", "standard_only":
		return true
	default:
		return false
	}
}

func providerResponsesStateful(pc llm.ProviderConfig) bool {
	if !strings.EqualFold(strings.TrimSpace(pc.APIType), "responses") {
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

func providerResponsesWebSocket(pc llm.ProviderConfig) bool {
	if !strings.EqualFold(strings.TrimSpace(pc.APIType), "responses") {
		return false
	}
	if pc.ResponsesWebSocket != nil {
		return *pc.ResponsesWebSocket
	}
	return pc.Auth != nil && strings.EqualFold(strings.TrimSpace(pc.Auth.Type), auth.TypeCodexOAuth)
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
