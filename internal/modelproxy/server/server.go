package server

import (
	"context"
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
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"harness/internal/apikey"
	"harness/internal/auth"
	"harness/internal/llm"
	"harness/internal/llm/factory"
	"harness/internal/metrics"
	"harness/internal/modelproxy/pricing"
	"harness/internal/modelproxy/protocol"
	"harness/internal/modelsdev"
	"harness/internal/reasoningprofile"
)

const (
	maxStreamRequestBytes = 64 << 20
	openAICodexProviderID = "openai-codex"
)

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
	ProviderConfigs      []string       `json:"provider_configs"`
	DefaultContextWindow int            `json:"default_context_window"`
	LogLevel             string         `json:"log_level,omitempty"`
	LogFormat            string         `json:"log_format,omitempty"`
	ModelsDevCacheTTL    Duration       `json:"models_dev_cache_ttl,omitempty"`
	APIKeys              []apikey.Entry `json:"api_keys,omitempty"`
	Metrics              MetricsConfig  `json:"metrics,omitempty"`
}

// MetricsConfig toggles the Prometheus /metrics endpoint on a separate port.
// Enabled is a pointer so the JSON omitempty distinction between unset and
// explicitly false survives; nil means the default (enabled) applies.
type MetricsConfig struct {
	Enabled *bool  `json:"enabled,omitempty"`
	Listen  string `json:"listen,omitempty"`
}

// APIKeyStore returns the API-key store for this config. Auth is required as soon
// as the first key is configured.
func (c Config) APIKeyStore() apikey.Store {
	return apikey.Store{Entries: append([]apikey.Entry(nil), c.APIKeys...)}
}

// Duration is a JSON duration setting. Strings use Go duration syntax such as
// "24h"; numeric values are seconds, so 0 disables the setting.
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
	// reading it (keeping internal/llm free of an internal/modelsdev import).
	ModelsDevCatalog *modelsdev.Catalog
	// ModelsDevSourceDate dates ModelsDevCatalog (its cache file mtime). Used to
	// stamp catalog pricing freshness when any provider is managed.
	ModelsDevSourceDate time.Time
	// Metrics, when non-nil, receives Prometheus-style counters for every
	// /v1/stream request (tokens, cost, requests, errors, duration), broken
	// down by provider, model, and authorizing key. Nil disables metrics at
	// the handler level; the command wires a registry in when the metrics
	// endpoint is enabled.
	Metrics *metrics.Registry
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
	targetID string
	pc       llm.ProviderConfig
	entry    llm.ModelEntry
}

// metricsCollectors holds the pre-registered metric families the proxy stamps
// per stream request. They are created once in NewHandler so HELP/TYPE always
// appear in exposition even with zero traffic.
type metricsCollectors struct {
	requests   *metrics.Counter
	errors     *metrics.Counter
	input      *metrics.Counter
	output     *metrics.Counter
	cacheRead  *metrics.Counter
	cacheWrite *metrics.Counter
	reasoning  *metrics.Counter
	cost       *metrics.Counter
	duration   *metrics.Counter
}

type Handler struct {
	// snapshot holds the current registry+catalog. Built once in NewHandler and
	// replaced wholesale by UpdateModelsDevCatalog; never mutated in place.
	snapshot atomic.Pointer[catalogSnapshot]

	providers            []llm.ProviderConfig
	authSources          map[string]*auth.Source
	defaultContextWindow int
	configSourceDate     time.Time
	pricingMaxAge        time.Duration
	getenv               func(string) string
	logger               *slog.Logger
	newProvider          func(factory.Options) (llm.Provider, error)
	nextRequestID        atomic.Uint64

	metrics    *metrics.Registry
	metricFams *metricsCollectors

	usageMu sync.Mutex
	usage   map[usageKey]*protocol.ModelUsage

	providerMu    sync.Mutex
	providerCache map[string]llm.Provider

	continuationMu       sync.Mutex
	continuations        map[string]llm.ResponseState
	disabledContinuation map[string]bool
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
	maxAge := opts.PricingMaxAge
	if maxAge <= 0 {
		maxAge = opts.Config.ModelsDevCacheTTL.Duration
	}
	h := &Handler{
		providers:            providers,
		authSources:          authSources,
		defaultContextWindow: defaultWindow,
		configSourceDate:     providerConfigSourceDate(opts.ConfigDir, opts.Config.ProviderConfigs),
		pricingMaxAge:        maxAge,
		getenv:               getenv,
		logger:               logger,
		newProvider:          newProvider,
		usage:                map[usageKey]*protocol.ModelUsage{},
		providerCache:        map[string]llm.Provider{},
		continuations:        map[string]llm.ResponseState{},
		disabledContinuation: map[string]bool{},
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
	return h, nil
}

// registerMetricFamilies pre-registers the proxy's Prometheus counter
// families so HELP/TYPE are present even with zero traffic. Labels are
// provider, model, and key (the authorizing key's name, or "anonymous").
func registerMetricFamilies(r *metrics.Registry) *metricsCollectors {
	return &metricsCollectors{
		requests:   r.Counter("model_proxy_requests_total", "Number of proxied model requests."),
		errors:     r.Counter("model_proxy_errors_total", "Number of proxied model requests that failed."),
		input:      r.Counter("model_proxy_input_tokens_total", "Input tokens billed at full rate."),
		output:     r.Counter("model_proxy_output_tokens_total", "Generated output tokens."),
		cacheRead:  r.Counter("model_proxy_cache_read_tokens_total", "Prompt-cache read tokens."),
		cacheWrite: r.Counter("model_proxy_cache_write_tokens_total", "Prompt-cache write tokens."),
		reasoning:  r.Counter("model_proxy_reasoning_tokens_total", "Reasoning tokens."),
		cost:       r.Counter("model_proxy_cost_usd_total", "Estimated cost in US dollars."),
		duration:   r.Counter("model_proxy_request_duration_seconds_total", "Total request wall-clock duration in seconds."),
	}
}

// streamPath is the route the proxy streams model responses on.
const streamPath = "/v1/stream"

// ObserveAuth wraps the authenticated handler so stream requests rejected by
// API-key auth (401) are still metered. It re-checks store.Authorize for the
// stream route only and records a rejected request before delegating to next
// (which writes the actual 401). When auth is not required store.Authorize is
// always true, so nothing is counted. Counting here, rather than in the apikey
// middleware, keeps that package free of a metrics dependency.
func ObserveAuth(h *Handler, store apikey.Store, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && r.URL.Path == streamPath && !store.Authorize(r) {
			h.RecordRejectedStream(r)
		}
		next.ServeHTTP(w, r)
	})
}

// RecordRejectedStream meters a stream request rejected before it reaches the
// handler (a 401). provider/model are unknown and omitted; the key label is the
// authorizing name or "anonymous".
func (h *Handler) RecordRejectedStream(r *http.Request) {
	if h.metrics == nil || h.metricFams == nil {
		return
	}
	key := "anonymous"
	if name, ok := apikey.AuthorizedName(r); ok {
		key = name
	}
	labels := map[string]string{"key": key}
	h.metricFams.requests.Inc(labels)
	h.metricFams.errors.Inc(labels)
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
// returned a 4xx/5xx status. The key label is the authorizing API key's name
// ("anonymous" when auth is disabled or absent).
func (h *Handler) recordMetrics(r *http.Request, providerID, model string, usage llm.Usage, duration time.Duration, failed bool) {
	if h.metrics == nil || h.metricFams == nil {
		return
	}
	key := "anonymous"
	if name, ok := apikey.AuthorizedName(r); ok {
		key = name
	}
	labels := map[string]string{"provider": providerID, "model": model, "key": key}
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
	if usage.ReasoningTokens != 0 {
		h.metricFams.reasoning.Add(float64(usage.ReasoningTokens), labels)
	}
	if usage.CostKnown {
		h.metricFams.cost.Add(usage.CostUSD, labels)
	}
}

// buildSnapshot resolves managed-provider flat prices from md where applicable,
// then builds the registry and served catalog from the provider configs. Manual
// providers keep their own configured prices. The catalog's pricing stamp dates
// the managed prices to the models.dev cache when any provider is managed, and
// to the provider-config mtime otherwise.
func (h *Handler) buildSnapshot(md *modelsdev.Catalog, mdSourceDate time.Time) (*catalogSnapshot, error) {
	priced := pricedProviders(h.providers, md)
	pricer := pricing.NewComposite()
	registry := llm.RegistryFromProviderConfigs(priced)
	registry.SetDefaultContextWindow(h.defaultContextWindow)
	catalog, targets, err := catalogFromProviderConfigs(priced, pricer)
	if err != nil {
		return nil, err
	}
	catalog.Pricing = h.pricingInfo(md, mdSourceDate)
	return &catalogSnapshot{registry: registry, catalog: catalog, targets: targets, pricer: pricer}, nil
}

// UpdateModelsDevCatalog rebuilds the served snapshot with prices resolved from
// md (manual providers unchanged) and swaps it in atomically. The serving
// refresher calls this after a successful models.dev cache refresh so live
// prices reach in-flight cost accounting and /v1/models without a restart.
func (h *Handler) UpdateModelsDevCatalog(md *modelsdev.Catalog, sourceDate time.Time) {
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
func (h *Handler) pricingInfo(md *modelsdev.Catalog, mdSourceDate time.Time) *protocol.PricingInfo {
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

// pricedProviders returns provider configs with flat prices ready for the
// registry and catalog. Managed providers get a fresh copy whose flat model
// prices come from the models.dev cache when applicable (left zero when the
// cache lacks the model or a provider-specific pricer owns the cost); manual
// providers are returned unchanged, keeping their own configured prices.
func pricedProviders(providers []llm.ProviderConfig, md *modelsdev.Catalog) []llm.ProviderConfig {
	out := make([]llm.ProviderConfig, len(providers))
	for i, pc := range providers {
		if pc.Name == openAICodexProviderID || (pc.Managed && pc.Name == pricing.SakanaProviderID) {
			cp := pc
			cp.Models = make([]llm.ModelEntry, len(pc.Models))
			for j, entry := range pc.Models {
				entry.Price = llm.Price{}
				cp.Models[j] = entry
			}
			out[i] = cp
			continue
		}
		if !pc.Managed {
			out[i] = pc
			continue
		}
		cp := pc
		// Managed prices resolve from PriceSource when set, otherwise from the
		// provider's own name.
		priceProvider := pc.PriceSource
		if priceProvider == "" {
			priceProvider = pc.Name
		}
		cp.Models = make([]llm.ModelEntry, len(pc.Models))
		for j, entry := range pc.Models {
			if info, ok := modelsDevModelInfo(md, priceProvider, entry.Name); ok {
				entry.Price = info.Price
				entry.InputModalities = append([]string(nil), info.InputModalities...)
			}
			cp.Models[j] = entry
		}
		out[i] = cp
	}
	return out
}

// modelsDevPrice bridges a models.dev catalog price into an llm.Price for one
// provider/model. This is the single point where the proxy crosses from
// modelsdev to llm pricing, keeping internal/llm free of a modelsdev import.
func modelsDevPrice(md *modelsdev.Catalog, providerID, modelID string) (llm.Price, bool) {
	info, ok := modelsDevModelInfo(md, providerID, modelID)
	return info.Price, ok
}

func modelsDevModelInfo(md *modelsdev.Catalog, providerID, modelID string) (llm.ModelInfo, bool) {
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

func (h *Handler) handleModels(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("content-type", "application/json")
	if err := json.NewEncoder(w).Encode(h.snapshot.Load().catalog); err != nil {
		h.logger.Warn("write model catalog failed", "err", err)
	}
}

func (h *Handler) handleUsage(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("content-type", "application/json")
	if err := json.NewEncoder(w).Encode(h.usageSnapshot()); err != nil {
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
	acc.ReasoningTokens += int64(u.ReasoningTokens)
	acc.CostUSD += cost
}

// usageSnapshot returns a copy of the accumulated usage, sorted by
// provider:model for deterministic output.
func (h *Handler) usageSnapshot() protocol.UsageReport {
	h.usageMu.Lock()
	report := protocol.UsageReport{Models: make([]protocol.ModelUsage, 0, len(h.usage))}
	for _, acc := range h.usage {
		report.Models = append(report.Models, *acc)
	}
	h.usageMu.Unlock()
	sort.Slice(report.Models, func(i, j int) bool {
		return report.Models[i].TargetID < report.Models[j].TargetID
	})
	return report
}

func (h *Handler) handleInputTokens(w http.ResponseWriter, r *http.Request) {
	cw := &countingResponseWriter{ResponseWriter: w}
	body, err := io.ReadAll(http.MaxBytesReader(cw, r.Body, maxStreamRequestBytes))
	if err != nil {
		writeError(cw, http.StatusRequestEntityTooLarge, &protocol.Error{StatusCode: http.StatusRequestEntityTooLarge, Message: "request body too large"})
		return
	}
	var req protocol.TokenCountRequest
	if err := json.Unmarshal(body, &req); err != nil {
		writeError(cw, http.StatusBadRequest, &protocol.Error{StatusCode: http.StatusBadRequest, Message: "malformed input token request"})
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
	req.Request.Model = target.entry.Name
	req.Request.ServerTools = resolveServerToolsForTarget(target, req.Request.ServerTools)
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
	requestID := h.nextRequestID.Add(1)
	cw := &countingResponseWriter{ResponseWriter: w}
	var (
		targetID   string
		providerID string
		apiType    string
		model      string
		usage      llm.Usage
		stop       llm.StopReason
		streamErr  string
		events     int
		toolCalls  int
		reqBytes   int
		errAttrs   []any
	)
	defer func() {
		attrs := []any{
			"request_id", requestID,
			"requester", requesterName(r),
			"remote_addr", r.RemoteAddr,
			"target_id", targetID,
			"provider", providerID,
			"api_type", apiType,
			"model", model,
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
			"reasoning_tokens", usage.ReasoningTokens,
		}
		if targetID != "" && usage.CostKnown {
			attrs = append(attrs, "cost_usd", usage.CostUSD)
			h.recordUsage(targetID, usage, usage.CostUSD)
		}
		// Record every stream request, even one that failed before the target
		// resolved (provider/model empty), so requests_total/errors_total reflect
		// all client-facing failures, not just post-resolution ones.
		h.recordMetrics(r, providerID, model, usage, time.Since(start), streamFailed(r.Context(), streamErr, cw.statusCode()))
		if streamErr != "" {
			attrs = append(attrs, "err", streamErr)
			attrs = append(attrs, errAttrs...)
			h.logger.Warn("model request completed", attrs...)
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
		writeError(cw, http.StatusRequestEntityTooLarge, &protocol.Error{StatusCode: http.StatusRequestEntityTooLarge, Message: "request body too large"})
		return
	}
	var req protocol.StreamRequest
	if err := json.Unmarshal(body, &req); err != nil {
		streamErr = "malformed stream request"
		writeError(cw, http.StatusBadRequest, &protocol.Error{StatusCode: http.StatusBadRequest, Message: "malformed stream request"})
		return
	}
	targetID = strings.TrimSpace(req.TargetID)
	if targetID == "" {
		streamErr = "target_id is required"
		writeError(cw, http.StatusBadRequest, &protocol.Error{StatusCode: http.StatusBadRequest, Message: "target_id is required"})
		return
	}
	target, err := h.resolveTarget(targetID)
	if err != nil {
		streamErr = err.Error()
		writeError(cw, http.StatusBadRequest, &protocol.Error{StatusCode: http.StatusBadRequest, Message: err.Error()})
		return
	}
	targetID = target.targetID
	providerID = target.pc.Name
	model = target.entry.Name
	req.Request.Model = model
	req.Request.ServerTools = resolveServerToolsForTarget(target, req.Request.ServerTools)
	req.Request.Reasoning = h.reasoningForTarget(target, req.ReasoningProfile, req.Request.Reasoning)
	opts, err := h.runtimeOptionsForTarget(r.Context(), target)
	if err != nil {
		streamErr = err.Error()
		writeError(cw, http.StatusBadRequest, &protocol.Error{StatusCode: http.StatusBadRequest, Message: err.Error()})
		return
	}
	apiType = opts.Provider
	stateful := providerResponsesStateful(target.pc)
	cacheKey := h.continuationKey(targetID, req.Request.PromptCacheKey)
	fullRequest := req.Request
	fullRequest.Messages = append([]llm.Message(nil), req.Request.Messages...)
	req.Request = h.applyContinuation(cacheKey, stateful, req.Request)
	provider, err := h.streamProvider(opts, targetID, req.Request.PromptCacheKey)
	if err != nil {
		streamErr = err.Error()
		writeError(cw, http.StatusBadRequest, protocol.ErrorFrom(err))
		return
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

	type streamRetry int
	const (
		streamRetryNone streamRetry = iota
		streamRetryContinuation
		streamRetryServerTools
	)
	streamAttempt := func(request llm.Request, anchorMessageCount int) (streamRetry, llm.ResponseState) {
		streamErr = ""
		errAttrs = nil
		sentEvents := false
		var finalState llm.ResponseState
		for ev, err := range provider.Stream(r.Context(), request) {
			if err != nil {
				streamErr = err.Error()
				errAttrs = streamErrorLogAttrs(err)
				if !sentEvents && request.PreviousResponseID != "" && previousResponseRejected(err) {
					h.resetContinuation(cacheKey)
					return streamRetryContinuation, llm.ResponseState{}
				}
				if !sentEvents && request.StoreResponse && storeResponseRejected(err) {
					h.disableContinuation(cacheKey)
					return streamRetryContinuation, llm.ResponseState{}
				}
				if !sentEvents && len(request.ServerTools) > 0 && serverToolRejected(err) {
					return streamRetryServerTools, llm.ResponseState{}
				}
				_ = enc.Encode(protocol.StreamEnvelope{Error: protocol.ErrorFrom(err)})
				flush()
				return streamRetryNone, llm.ResponseState{}
			}
			sentEvents = true
			events++
			if ev.Usage != nil {
				usage = mergeUsage(usage, *ev.Usage)
				usage = h.priceUsage(targetID, request, usage)
				ev.Usage = &usage
			}
			if ev.Kind == llm.EventToolCallDone {
				toolCalls++
			}
			if ev.Kind == llm.EventDone {
				stop = ev.StopReason
				if ev.Usage != nil {
					usage = mergeUsage(usage, *ev.Usage)
				}
				usage = h.priceUsage(targetID, request, usage)
				ev.Usage = &usage
				if ev.ResponseID != "" && request.StoreResponse {
					// The caller appends the assistant message from this response
					// after the proxy sees the full request, so the next delta
					// starts after that future transcript item. request.Messages
					// may be a trimmed continuation delta, so anchor against the
					// caller's full message count instead.
					finalState = llm.ResponseState{
						PreviousResponseID: ev.ResponseID,
						AnchorMessages:     anchorMessageCount + 1,
					}
				}
			}
			event := ev
			if err := enc.Encode(protocol.StreamEnvelope{Event: &event}); err != nil {
				streamErr = err.Error()
				return streamRetryNone, llm.ResponseState{}
			}
			flush()
		}
		return streamRetryNone, finalState
	}
	attemptRequest := req.Request
	retry, state := streamAttempt(attemptRequest, len(fullRequest.Messages))
	for retry != streamRetryNone {
		switch retry {
		case streamRetryContinuation:
			fullRequest.StoreResponse = false
			fullRequest.PreviousResponseID = ""
			attemptRequest = fullRequest
		case streamRetryServerTools:
			attemptRequest.ServerTools = nil
			fullRequest.ServerTools = nil
		default:
			retry = streamRetryNone
			continue
		}
		retry, state = streamAttempt(attemptRequest, len(fullRequest.Messages))
	}
	if state.PreviousResponseID != "" {
		h.saveContinuation(cacheKey, state)
	}
}

func (h *Handler) streamProvider(opts factory.Options, providerID, promptCacheKey string) (llm.Provider, error) {
	if !opts.ResponsesWebSocket {
		return h.newProvider(opts)
	}
	key := streamProviderCacheKey(opts, providerID, promptCacheKey)
	h.providerMu.Lock()
	defer h.providerMu.Unlock()
	if provider := h.providerCache[key]; provider != nil {
		return provider, nil
	}
	provider, err := h.newProvider(opts)
	if err != nil {
		return nil, err
	}
	h.providerCache[key] = provider
	return provider, nil
}

func streamProviderCacheKey(opts factory.Options, providerID, promptCacheKey string) string {
	auth := sha256.New()
	auth.Write([]byte(opts.APIKey))
	keys := make([]string, 0, len(opts.AuthHeaders))
	for key := range opts.AuthHeaders {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		auth.Write([]byte{0})
		auth.Write([]byte(strings.ToLower(key)))
		auth.Write([]byte{0})
		auth.Write([]byte(opts.AuthHeaders[key]))
	}
	return strings.Join([]string{
		providerID,
		opts.Provider,
		opts.ProviderName,
		opts.Model,
		opts.BaseURL,
		strconv.Itoa(opts.ContextWindow),
		strconv.Itoa(opts.OutputLimit),
		strconv.FormatBool(opts.OmitMaxOutputTokens),
		promptCacheKey,
		hex.EncodeToString(auth.Sum(nil)),
	}, "\x00")
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

func mergeUsage(acc, in llm.Usage) llm.Usage {
	acc.InputTokens = max(acc.InputTokens, in.InputTokens)
	acc.OutputTokens = max(acc.OutputTokens, in.OutputTokens)
	acc.CacheReadTokens = max(acc.CacheReadTokens, in.CacheReadTokens)
	acc.CacheWriteTokens = max(acc.CacheWriteTokens, in.CacheWriteTokens)
	acc.ReasoningTokens = max(acc.ReasoningTokens, in.ReasoningTokens)
	if in.CostKnown {
		acc.CostUSD = in.CostUSD
		acc.CostKnown = true
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
	if profile == "" {
		return llm.ReasoningConfig{Summary: requested.Summary}
	}
	info := modelEntryReasoning(target.entry)
	if info == nil || !info.Supported {
		return llm.ReasoningConfig{Summary: requested.Summary}
	}
	mode := reasoningModeForProviderConfig(target.pc)
	out := llm.ReasoningConfig{Profile: profile, Summary: requested.Summary}
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
	if !ok || maxPtr == nil || *maxPtr <= 0 {
		return 0, false
	}
	minBudget := 0
	if minPtr != nil {
		minBudget = *minPtr
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
// for ServerToolKindOpenAIWebSearch, so it is the safe default for everything
// except Anthropic.
func defaultWebSearchKindForAPIType(apiType string) string {
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

func stringSliceContains(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

func (h *Handler) continuationKey(targetID, promptCacheKey string) string {
	if strings.TrimSpace(promptCacheKey) == "" {
		return ""
	}
	return targetID + "\x00" + promptCacheKey
}

func (h *Handler) applyContinuation(key string, stateful bool, req llm.Request) llm.Request {
	if key == "" || !stateful {
		req.StoreResponse = false
		req.PreviousResponseID = ""
		return req
	}
	h.continuationMu.Lock()
	defer h.continuationMu.Unlock()
	if h.disabledContinuation[key] {
		req.StoreResponse = false
		req.PreviousResponseID = ""
		return req
	}
	req.StoreResponse = true
	state := h.continuations[key]
	if state.PreviousResponseID != "" && state.AnchorMessages >= 0 && state.AnchorMessages < len(req.Messages) {
		req.PreviousResponseID = state.PreviousResponseID
		req.Messages = req.Messages[state.AnchorMessages:]
	}
	return req
}

func (h *Handler) saveContinuation(key string, state llm.ResponseState) {
	if key == "" || state.PreviousResponseID == "" {
		return
	}
	h.continuationMu.Lock()
	h.continuations[key] = state
	h.continuationMu.Unlock()
}

func (h *Handler) resetContinuation(key string) {
	if key == "" {
		return
	}
	h.continuationMu.Lock()
	delete(h.continuations, key)
	h.continuationMu.Unlock()
}

func (h *Handler) disableContinuation(key string) {
	if key == "" {
		return
	}
	h.continuationMu.Lock()
	h.disabledContinuation[key] = true
	delete(h.continuations, key)
	h.continuationMu.Unlock()
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
	msg := strings.ToLower(apiErr.Message)
	return strings.Contains(msg, "previous_response_id") || strings.Contains(msg, "previous response")
}

func storeResponseRejected(err error) bool {
	var apiErr *llm.APIError
	if !errors.As(err, &apiErr) {
		return false
	}
	code := strings.ToLower(apiErr.Code)
	if strings.Contains(code, "store") {
		return true
	}
	msg := strings.ToLower(apiErr.Message)
	return strings.Contains(msg, "store") &&
		(strings.Contains(msg, "false") || strings.Contains(msg, "unsupported") || strings.Contains(msg, "not supported"))
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
	// Reject keys with an empty/invalid name: such a key authenticates but its
	// per-key metrics would be misattributed to the "anonymous" bucket. KeyNameRE
	// is otherwise only enforced at key generation.
	for i, e := range cfg.APIKeys {
		if !apikey.KeyNameRE.MatchString(e.Name) {
			return Config{}, fmt.Errorf("api_keys[%d]: name %q must match %s", i, e.Name, apikey.KeyNameRE.String())
		}
	}
	return cfg, nil
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
	modelCounts := map[string]int{}
	for _, pc := range providers {
		for _, entry := range pc.Models {
			if entry.Name != "" {
				modelCounts[entry.Name]++
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
			aliases := []string{id}
			if modelCounts[entry.Name] == 1 {
				aliases = append(aliases, entry.Name)
			}
			price := entry.Price
			if pricer != nil {
				if catalogPrice := pricer.CatalogPrice(pc, entry); catalogPrice.Known {
					price = catalogPrice.Price
				} else {
					price = llm.Price{}
				}
			}
			target := protocol.Target{
				ID:              id,
				Aliases:         aliases,
				DisplayName:     entry.Name,
				ProviderLabel:   pc.Name,
				ModelLabel:      entry.Name,
				ContextWindow:   entry.ContextWindow,
				OutputLimit:     entry.OutputLimit,
				InputModalities: append([]string(nil), entry.InputModalities...),
				ServerTools:     targetServerTools(pc, entry),
				Price:           price,
				Reasoning:       targetReasoningSupported(entry),
			}
			out.Targets = append(out.Targets, target)
			rt := resolvedTarget{targetID: id, pc: pc, entry: entry}
			targets[id] = rt
			for _, alias := range aliases {
				targets[alias] = rt
			}
		}
	}
	if len(out.Targets) == 0 {
		return protocol.Catalog{}, nil, fmt.Errorf("model proxy: no configured models")
	}
	return out, targets, nil
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

func providerResponsesWebSocket(pc llm.ProviderConfig) bool {
	if !strings.EqualFold(strings.TrimSpace(pc.APIType), "responses") {
		return false
	}
	if pc.ResponsesWebSocket != nil {
		return *pc.ResponsesWebSocket
	}
	return pc.Auth != nil && strings.EqualFold(strings.TrimSpace(pc.Auth.Type), auth.TypeCodexOAuth)
}

func providerConfigByName(providers []llm.ProviderConfig, name string) (llm.ProviderConfig, bool) {
	for _, pc := range providers {
		if pc.Name == name {
			return pc, true
		}
	}
	return llm.ProviderConfig{}, false
}

func providerConfigModel(pc llm.ProviderConfig, model string) (llm.ModelEntry, bool) {
	for _, entry := range pc.Models {
		if entry.Name == model {
			return entry, true
		}
	}
	return llm.ModelEntry{}, false
}

func modelEntryReasoning(m llm.ModelEntry) *llm.ReasoningInfo {
	if m.Reasoning == nil && len(m.ReasoningOptions) == 0 {
		return nil
	}
	supported := false
	if m.Reasoning != nil {
		supported = *m.Reasoning
	}
	return (&llm.ReasoningInfo{
		Supported: supported,
		Options:   append([]llm.ReasoningOption(nil), m.ReasoningOptions...),
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
	case "responses":
		if v := getenv("RESPONSES_API_KEY"); v != "" {
			return v
		}
		return getenv("OPENAI_API_KEY")
	default:
		return getenv("OPENAI_API_KEY")
	}
}
