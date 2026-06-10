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
	"strings"
	"sync/atomic"
	"time"

	"harness/internal/auth"
	"harness/internal/llm"
	"harness/internal/llm/factory"
	"harness/internal/modelproxy/protocol"
)

const maxStreamRequestBytes = 64 << 20

type Config struct {
	ProviderConfigs      []string `json:"provider_configs"`
	DefaultContextWindow int      `json:"default_context_window"`
	LogLevel             string   `json:"log_level,omitempty"`
	LogFormat            string   `json:"log_format,omitempty"`
}

type Options struct {
	ConfigDir string
	Config    Config
	Getenv    func(string) string
	Logger    *slog.Logger
	New       func(factory.Options) (llm.Provider, error)
	Warn      func(string)
}

type Handler struct {
	catalog              protocol.Catalog
	registry             *llm.Registry
	providers            []llm.ProviderConfig
	authSources          map[string]*auth.Source
	defaultContextWindow int
	getenv               func(string) string
	logger               *slog.Logger
	newProvider          func(factory.Options) (llm.Provider, error)
	nextRequestID        atomic.Uint64
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
	registry, providers, err := llm.LoadProviderConfigs(opts.ConfigDir, opts.Config.ProviderConfigs, warn)
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
	registry.SetDefaultContextWindow(defaultWindow)
	if len(providers) == 0 {
		return nil, fmt.Errorf("model proxy: no provider configs are configured")
	}
	catalog, err := catalogFromProviderConfigs(providers)
	if err != nil {
		return nil, err
	}
	return &Handler{
		catalog:              catalog,
		registry:             registry,
		providers:            providers,
		authSources:          authSources,
		defaultContextWindow: defaultWindow,
		getenv:               getenv,
		logger:               logger,
		newProvider:          newProvider,
	}, nil
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.Method == http.MethodGet && r.URL.Path == "/v1/models":
		h.handleModels(w, r)
	case r.Method == http.MethodPost && r.URL.Path == "/v1/stream":
		h.handleStream(w, r)
	default:
		http.NotFound(w, r)
	}
}

func (h *Handler) Catalog() protocol.Catalog {
	return h.catalog
}

func (h *Handler) handleModels(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("content-type", "application/json")
	if err := json.NewEncoder(w).Encode(h.catalog); err != nil {
		h.logger.Warn("write model catalog failed", "err", err)
	}
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
		if h.registry != nil && providerID != "" && model != "" {
			if cost, ok := h.registry.Cost(providerID+":"+model, usage); ok {
				attrs = append(attrs, "cost_usd", cost)
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
		Provider:      apiType,
		ProviderName:  pc.Name,
		Model:         model,
		BaseURL:       pc.BaseURL,
		APIKey:        apiKey,
		AuthHeaders:   authHeaders,
		ContextWindow: contextWindow,
	}, nil
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
