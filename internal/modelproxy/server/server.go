package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
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

	"harness/internal/auth"
	"harness/internal/llm"
	"harness/internal/llm/factory"
	"harness/internal/modelproxy/protocol"
	"harness/internal/modelsdev"
)

const maxStreamRequestBytes = 64 << 20

type Config struct {
	ProviderConfigs      []string `json:"provider_configs"`
	DefaultContextWindow int      `json:"default_context_window"`
	LogLevel             string   `json:"log_level,omitempty"`
	LogFormat            string   `json:"log_format,omitempty"`
	ModelsDevCacheTTL    Duration `json:"models_dev_cache_ttl,omitempty"`
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
}

// usageKey identifies an aggregate usage bucket by provider and model.
type usageKey struct {
	provider string
	model    string
}

// catalogSnapshot is the immutable served state: a registry used to price
// requests and the catalog served at /v1/models. It is swapped atomically when
// the models.dev cache refreshes so managed prices stay fresh without a
// restart. Readers Load() it; the refresher Stores() a freshly built one.
type catalogSnapshot struct {
	registry *llm.Registry
	catalog  protocol.Catalog
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

	usageMu sync.Mutex
	usage   map[usageKey]*protocol.ModelUsage
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
	}
	snapshot, err := h.buildSnapshot(opts.ModelsDevCatalog, opts.ModelsDevSourceDate)
	if err != nil {
		return nil, err
	}
	h.snapshot.Store(snapshot)
	return h, nil
}

// buildSnapshot resolves managed-provider prices from md, then builds the
// registry and served catalog from the price-filled providers. Manual providers
// keep their own configured prices. The catalog's pricing stamp dates the
// managed prices to the models.dev cache when any provider is managed, and to
// the provider-config mtime otherwise.
func (h *Handler) buildSnapshot(md *modelsdev.Catalog, mdSourceDate time.Time) (*catalogSnapshot, error) {
	priced := pricedProviders(h.providers, md)
	registry := llm.RegistryFromProviderConfigs(priced)
	registry.SetDefaultContextWindow(h.defaultContextWindow)
	catalog, err := catalogFromProviderConfigs(priced)
	if err != nil {
		return nil, err
	}
	catalog.Pricing = h.pricingInfo(md, mdSourceDate)
	return &catalogSnapshot{registry: registry, catalog: catalog}, nil
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

// pricedProviders returns provider configs with prices ready for the registry
// and catalog. Managed providers get a fresh copy whose model prices come from
// the models.dev cache (left zero when the cache lacks the model); manual
// providers are returned unchanged, keeping their own configured prices.
func pricedProviders(providers []llm.ProviderConfig, md *modelsdev.Catalog) []llm.ProviderConfig {
	out := make([]llm.ProviderConfig, len(providers))
	for i, pc := range providers {
		if !pc.Managed {
			out[i] = pc
			continue
		}
		cp := pc
		// Managed prices resolve from PriceSource when set (e.g. openai-codex
		// prices from "openai"), otherwise from the provider's own name.
		priceProvider := pc.PriceSource
		if priceProvider == "" {
			priceProvider = pc.Name
		}
		cp.Models = make([]llm.ModelEntry, len(pc.Models))
		for j, entry := range pc.Models {
			if price, ok := modelsDevPrice(md, priceProvider, entry.Name); ok {
				entry.Price = price
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
	if md == nil {
		return llm.Price{}, false
	}
	provider, ok := md.Provider(providerID)
	if !ok {
		return llm.Price{}, false
	}
	info, ok := provider.ModelInfo(modelID)
	if !ok {
		return llm.Price{}, false
	}
	return info.Price, true
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
	case r.Method == http.MethodPost && r.URL.Path == "/v1/stream":
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
// called only for requests whose model has a known price, so every bucket has a
// meaningful CostUSD.
func (h *Handler) recordUsage(provider, model string, u llm.Usage, cost float64) {
	key := usageKey{provider: provider, model: model}
	h.usageMu.Lock()
	defer h.usageMu.Unlock()
	acc := h.usage[key]
	if acc == nil {
		acc = &protocol.ModelUsage{Provider: provider, Model: model}
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
		if report.Models[i].Provider != report.Models[j].Provider {
			return report.Models[i].Provider < report.Models[j].Provider
		}
		return report.Models[i].Model < report.Models[j].Model
	})
	return report
}

func (h *Handler) handleStream(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	requestID := h.nextRequestID.Add(1)
	cw := &countingResponseWriter{ResponseWriter: w}
	var (
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
		if providerID != "" && model != "" {
			if snapshot := h.snapshot.Load(); snapshot != nil && snapshot.registry != nil {
				if cost, ok := snapshot.registry.Cost(providerID+":"+model, usage); ok {
					attrs = append(attrs, "cost_usd", cost)
					h.recordUsage(providerID, model, usage, cost)
				}
			}
		}
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
	providerID = strings.TrimSpace(req.Provider)
	model = req.Request.Model
	if providerID == "" {
		streamErr = "provider is required"
		writeError(cw, http.StatusBadRequest, &protocol.Error{StatusCode: http.StatusBadRequest, Message: "provider is required"})
		return
	}
	if model == "" {
		streamErr = "model is required"
		writeError(cw, http.StatusBadRequest, &protocol.Error{StatusCode: http.StatusBadRequest, Message: "model is required"})
		return
	}

	opts, err := h.runtimeOptions(r.Context(), providerID, req.Request.Model)
	if err != nil {
		streamErr = err.Error()
		writeError(cw, http.StatusBadRequest, &protocol.Error{StatusCode: http.StatusBadRequest, Message: err.Error()})
		return
	}
	apiType = opts.Provider
	provider, err := h.newProvider(opts)
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

	for ev, err := range provider.Stream(r.Context(), req.Request) {
		if err != nil {
			streamErr = err.Error()
			errAttrs = streamErrorLogAttrs(err)
			_ = enc.Encode(protocol.StreamEnvelope{Error: protocol.ErrorFrom(err)})
			flush()
			return
		}
		events++
		if ev.Usage != nil {
			usage = mergeUsage(usage, *ev.Usage)
		}
		if ev.Kind == llm.EventToolCallDone {
			toolCalls++
		}
		if ev.Kind == llm.EventDone {
			stop = ev.StopReason
		}
		event := ev
		if err := enc.Encode(protocol.StreamEnvelope{Event: &event}); err != nil {
			streamErr = err.Error()
			return
		}
		flush()
	}
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
	return acc
}

func (h *Handler) runtimeOptions(ctx context.Context, providerID, model string) (factory.Options, error) {
	pc, ok := providerConfigByName(h.providers, providerID)
	if !ok {
		return factory.Options{}, fmt.Errorf("provider %q is not configured", providerID)
	}
	entry, ok := providerConfigModel(pc, model)
	if !ok {
		return factory.Options{}, fmt.Errorf("provider %q has no configured model %q", providerID, model)
	}
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
		Model:               model,
		BaseURL:             pc.BaseURL,
		APIKey:              apiKey,
		AuthHeaders:         authHeaders,
		ContextWindow:       contextWindow,
		OutputLimit:         entry.OutputLimit,
		OmitMaxOutputTokens: providerOmitMaxOutputTokens(pc),
	}, nil
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

func catalogFromProviderConfigs(providers []llm.ProviderConfig) (protocol.Catalog, error) {
	out := protocol.Catalog{
		Providers: make([]protocol.Provider, 0, len(providers)),
	}
	for _, pc := range providers {
		if pc.Name == "" {
			continue
		}
		p := protocol.Provider{
			ID:                pc.Name,
			Name:              pc.Name,
			APIType:           pc.APIType,
			ResponsesStateful: providerResponsesStateful(pc),
			Models:            make([]protocol.Model, 0, len(pc.Models)),
		}
		for _, entry := range pc.Models {
			if entry.Name == "" {
				continue
			}
			p.Models = append(p.Models, protocol.Model{
				ID:            entry.Name,
				Name:          entry.Name,
				ContextWindow: entry.ContextWindow,
				OutputLimit:   entry.OutputLimit,
				Price:         entry.Price,
				Reasoning:     modelEntryReasoning(entry),
			})
		}
		if len(p.Models) > 0 {
			out.Providers = append(out.Providers, p)
		}
	}
	if len(out.Providers) == 0 {
		return protocol.Catalog{}, fmt.Errorf("model proxy: no configured models")
	}
	return out, nil
}

func providerResponsesStateful(pc llm.ProviderConfig) bool {
	if !strings.EqualFold(strings.TrimSpace(pc.APIType), "responses") {
		return false
	}
	return pc.Auth == nil || !strings.EqualFold(strings.TrimSpace(pc.Auth.Type), auth.TypeCodexOAuth)
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
