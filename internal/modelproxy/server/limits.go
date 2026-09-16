package server

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"sort"
	"sync"
	"time"

	"harness/internal/llm"
	"harness/internal/modelproxy/protocol"
	"harness/internal/modelproxy/subscription"
	"harness/internal/tracing"
)

// Quota operations are not model requests, but should still be visible at the
// normal log level. Log only fixed route/operation metadata and safe error codes.
func (h *Handler) serveLimits(w http.ResponseWriter, r *http.Request, next http.HandlerFunc) {
	start := time.Now()
	cw := &countingResponseWriter{ResponseWriter: w}
	defer func() {
		attrs := []any{"path", r.URL.Path, "status", cw.statusCode(), "duration", time.Since(start)}
		if tc, ok := tracing.TraceFromHeaders(r.Header); ok {
			attrs = append(attrs, tracing.LogAttrs(tc)...)
		}
		h.logger.Info("subscription quota request completed", attrs...)
	}()
	next(cw, r)
}

func (h *Handler) logLimitsResult(operation, provider, outcome string, err *protocol.LimitsError, warnings []protocol.LimitsError) {
	attrs := []any{"operation", operation, "provider", provider}
	if outcome != "" {
		attrs = append(attrs, "outcome", outcome)
	}
	if err != nil {
		attrs = append(attrs, "error_code", err.Code, "indeterminate", err.Indeterminate)
	}
	if len(warnings) != 0 {
		codes := make([]string, 0, len(warnings))
		for _, w := range warnings {
			codes = append(codes, w.Code)
		}
		attrs = append(attrs, "warning_codes", codes)
	}
	if err != nil || len(warnings) != 0 {
		h.logger.Warn("subscription quota result", attrs...)
	} else {
		h.logger.Info("subscription quota result", attrs...)
	}
}

// providerCredentials is shared with inference: dynamic auth, configured env,
// dialect env fallback, then inline key. Do not create new auth Sources here.
func (h *Handler) providerCredentials(ctx context.Context, pc llm.ProviderConfig) (string, map[string]string, error) {
	if src := h.authSources[pc.Name]; src != nil {
		headers, err := src.Headers(ctx)
		return "", headers, err
	}
	for _, name := range pc.APIKeyEnv {
		if value := h.getenv(name); value != "" {
			return value, nil, nil
		}
	}
	apiType := pc.APIType
	if apiType == "" {
		apiType = pc.Name
	}
	if key := providerAPIKeyEnv(apiType, h.getenv); key != "" {
		return key, nil, nil
	}
	return pc.APIKey, nil, nil
}

func (h *Handler) subscriptionCredentials(ctx context.Context, pc llm.ProviderConfig) (subscription.Credentials, error) {
	key, headers, err := h.providerCredentials(ctx, pc)
	if err != nil { // Auth commands may include secrets in their errors.
		return subscription.Credentials{}, &protocol.LimitsError{Code: "missing_auth", Message: "provider credentials are unavailable"}
	}
	return subscription.Credentials{APIKey: key, Headers: headers}, nil
}

func limitsError(err error) *protocol.LimitsError {
	var safe *protocol.LimitsError
	if errors.As(err, &safe) {
		return safe
	}
	return &protocol.LimitsError{Code: "unavailable", Message: "subscription quota service is unavailable"}
}

func writeLimitsJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeLimitsError(w http.ResponseWriter, status int, err error) {
	writeLimitsJSON(w, status, struct {
		Error *protocol.LimitsError `json:"error"`
	}{limitsError(err)})
}

func limitsMethod(w http.ResponseWriter, r *http.Request, method string) bool {
	if r.Method == method {
		return true
	}
	w.Header().Set("Allow", method)
	writeLimitsError(w, http.StatusMethodNotAllowed, &protocol.LimitsError{Code: "invalid_method", Message: "method not allowed"})
	return false
}

func (h *Handler) limitsProvider(name string) (llm.ProviderConfig, error) {
	if !subscription.Supported(name) {
		return llm.ProviderConfig{}, &protocol.LimitsError{Code: "unsupported_provider", Message: "subscription quota provider is not supported"}
	}
	for _, pc := range h.providers {
		if pc.Name == name {
			return pc, nil
		}
	}
	return llm.ProviderConfig{}, &protocol.LimitsError{Code: "provider_not_configured", Message: "subscription quota provider is not configured"}
}

func (h *Handler) handleLimits(w http.ResponseWriter, r *http.Request) {
	if !limitsMethod(w, r, http.MethodGet) {
		return
	}
	var providers []llm.ProviderConfig
	if name := r.URL.Query().Get("provider"); name != "" {
		pc, err := h.limitsProvider(name)
		if err != nil {
			writeLimitsError(w, http.StatusBadRequest, err)
			return
		}
		providers = append(providers, pc)
	} else {
		for _, pc := range h.providers {
			if subscription.Supported(pc.Name) {
				providers = append(providers, pc)
			}
		}
	}
	sort.Slice(providers, func(i, j int) bool { return providers[i].Name < providers[j].Name })
	report := protocol.LimitsReport{Providers: make([]protocol.ProviderLimits, len(providers))}
	// At most three supported providers; retain an explicit concurrency bound.
	sem := make(chan struct{}, 3)
	var wg sync.WaitGroup
	for i, pc := range providers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			select {
			case sem <- struct{}{}:
				defer func() { <-sem }()
			case <-r.Context().Done():
				report.Providers[i] = protocol.ProviderLimits{Provider: pc.Name, FetchedAt: h.now(), Error: &protocol.LimitsError{Code: "canceled", Message: "quota request canceled"}}
				return
			}
			status := protocol.ProviderLimits{Provider: pc.Name, FetchedAt: h.now()}
			account, err := h.limits.Resolve(r.Context(), pc)
			if err == nil {
				status, err = account.Status(r.Context())
			}
			if err != nil {
				status = protocol.ProviderLimits{Provider: pc.Name, FetchedAt: h.now(), Error: limitsError(err)}
			}
			report.Providers[i] = status
		}()
	}
	wg.Wait()
	for _, p := range report.Providers {
		h.logLimitsResult("status", p.Provider, "", p.Error, p.Warnings)
	}
	writeLimitsJSON(w, http.StatusOK, report)
}

func (h *Handler) handleResetCredits(w http.ResponseWriter, r *http.Request) {
	if !limitsMethod(w, r, http.MethodGet) {
		return
	}
	name := r.URL.Query().Get("provider")
	if name != "openai-codex" {
		writeLimitsError(w, http.StatusBadRequest, &protocol.LimitsError{Code: "unsupported_provider", Message: "reset credits require provider openai-codex"})
		return
	}
	pc, err := h.limitsProvider(name)
	if err != nil {
		writeLimitsError(w, http.StatusBadRequest, err)
		return
	}
	credits := protocol.ResetCredits{Provider: name, FetchedAt: h.now(), Credits: []protocol.ResetCredit{}}
	account, err := h.limits.Resolve(r.Context(), pc)
	if err == nil {
		credits, err = account.ResetCredits(r.Context())
	}
	if err != nil {
		credits = protocol.ResetCredits{Provider: name, FetchedAt: h.now(), Credits: []protocol.ResetCredit{}, Error: limitsError(err)}
	}
	h.logLimitsResult("reset_credits", credits.Provider, "", credits.Error, credits.Warnings)
	writeLimitsJSON(w, http.StatusOK, credits)
}

func (h *Handler) handleLimitsReset(w http.ResponseWriter, r *http.Request) {
	if !limitsMethod(w, r, http.MethodPost) {
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 4096)
	dec := json.NewDecoder(r.Body)
	var req protocol.ResetRequest
	err := dec.Decode(&req)
	if err == nil {
		var extra any
		if dec.Decode(&extra) != io.EOF {
			err = errors.New("trailing body")
		}
	}
	if err != nil {
		writeLimitsError(w, http.StatusBadRequest, &protocol.LimitsError{Code: "invalid_request", Message: "invalid or oversized reset request"})
		return
	}
	if err := req.Validate(); err != nil {
		writeLimitsError(w, http.StatusBadRequest, err)
		return
	}
	pc, err := h.limitsProvider(req.Provider)
	if err != nil {
		writeLimitsError(w, http.StatusBadRequest, err)
		return
	}
	result := protocol.ResetResult{Provider: req.Provider, CreditID: req.CreditID, RequestID: req.RequestID, FetchedAt: h.now()}
	// Capture this provider's auth once, including refreshes. No model retry path.
	account, err := h.limits.Resolve(r.Context(), pc)
	if err == nil {
		result, err = account.Reset(r.Context(), req)
	}
	if err != nil {
		result.Provider, result.CreditID, result.RequestID = req.Provider, req.CreditID, req.RequestID
		result.FetchedAt = h.now()
		result.Error = limitsError(err)
		if result.Error.Indeterminate {
			result.Outcome = "indeterminate"
		}
	} else if result.Outcome == "reset" || result.Outcome == "already_redeemed" {
		// Refresh failures cannot turn a successful redemption into a failed one.
		status, statusErr := account.Status(r.Context())
		if statusErr == nil {
			result.Status = &status
		} else {
			result.Warnings = append(result.Warnings, protocol.LimitsError{Code: "status_refresh_failed", Message: "reset succeeded; quota refresh failed"})
		}
		credits, creditsErr := account.ResetCredits(r.Context())
		if creditsErr == nil {
			result.Credits = &credits
		} else {
			result.Warnings = append(result.Warnings, protocol.LimitsError{Code: "credits_refresh_failed", Message: "reset succeeded; reset credit refresh failed"})
		}
	}
	h.logLimitsResult("reset", result.Provider, result.Outcome, result.Error, result.Warnings)
	writeLimitsJSON(w, http.StatusOK, result)
}
