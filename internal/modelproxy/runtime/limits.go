package runtime

import (
	"context"
	"errors"
	"harness/internal/llm"
	"harness/internal/modelproxy"
	"harness/internal/modelproxy/subscription"
	"sort"
	"sync"
)

func (h *Runtime) logLimitsResult(operation, provider, outcome string, err *modelproxy.LimitsError, warnings []modelproxy.LimitsError) {
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
func (h *Runtime) providerCredentials(ctx context.Context, pc llm.ProviderConfig) (string, map[string]string, error) {
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

func (h *Runtime) subscriptionCredentials(ctx context.Context, pc llm.ProviderConfig) (subscription.Credentials, error) {
	key, headers, err := h.providerCredentials(ctx, pc)
	if err != nil { // Auth commands may include secrets in their errors.
		return subscription.Credentials{}, &modelproxy.LimitsError{Code: "missing_auth", Message: "provider credentials are unavailable"}
	}
	return subscription.Credentials{APIKey: key, Headers: headers}, nil
}

func limitsError(err error) *modelproxy.LimitsError {
	var safe *modelproxy.LimitsError
	if errors.As(err, &safe) {
		return safe
	}
	return &modelproxy.LimitsError{Code: "unavailable", Message: "subscription quota service is unavailable"}
}

func (h *Runtime) limitsProvider(name string) (llm.ProviderConfig, error) {
	for _, pc := range h.providers {
		if pc.Name == name {
			if !subscription.SupportedProvider(pc) {
				return llm.ProviderConfig{}, &modelproxy.LimitsError{Code: "unsupported_provider", Message: "subscription quota provider is not supported"}
			}
			return pc, nil
		}
	}
	return llm.ProviderConfig{}, &modelproxy.LimitsError{Code: "provider_not_configured", Message: "subscription quota provider is not configured"}
}

func (h *Runtime) subscriptionProviders() []llm.ProviderConfig {
	var providers []llm.ProviderConfig
	for _, pc := range h.providers {
		if subscription.SupportedProvider(pc) {
			providers = append(providers, pc)
		}
	}
	sort.Slice(providers, func(i, j int) bool { return providers[i].Name < providers[j].Name })
	return providers
}

// RefreshSubscriptionMetrics performs read-only status queries; it never lists
// or consumes credits, and is a no-op when metrics are disabled.
func (h *Runtime) RefreshSubscriptionMetrics(ctx context.Context) {
	if h.subscriptionMetrics == nil || ctx.Err() != nil {
		return
	}
	h.fetchLimits(ctx, h.subscriptionProviders(), "poll")
}

func (h *Runtime) fetchLimits(ctx context.Context, providers []llm.ProviderConfig, operation string) modelproxy.LimitsReport {
	report := modelproxy.LimitsReport{Providers: make([]modelproxy.ProviderLimits, len(providers))}
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
			case <-ctx.Done():
				report.Providers[i] = modelproxy.ProviderLimits{Provider: pc.Name, FetchedAt: h.now(), Error: &modelproxy.LimitsError{Code: "canceled", Message: "quota request canceled"}}
				return
			}
			status := modelproxy.ProviderLimits{Provider: pc.Name, FetchedAt: h.now()}
			account, err := h.limits.Resolve(ctx, pc)
			if err == nil {
				status, err = account.Status(ctx)
			}
			if err != nil {
				status = modelproxy.ProviderLimits{Provider: pc.Name, FetchedAt: h.now(), Error: limitsError(err)}
			}
			report.Providers[i] = status
		}()
	}
	wg.Wait()
	for _, p := range report.Providers {
		h.logLimitsResult(operation, p.Provider, "", p.Error, p.Warnings)
		h.publishSubscriptionStatus(p)
	}
	return report
}

func (h *Runtime) Limits(ctx context.Context, name string) (modelproxy.LimitsReport, error) {
	var providers []llm.ProviderConfig
	if name != "" {
		if !modelproxy.ValidLimitsID(name) {
			return modelproxy.LimitsReport{}, &modelproxy.LimitsError{Code: "invalid_request", Message: "provider must be a safe identifier of at most 128 characters"}
		}
		pc, err := h.limitsProvider(name)
		if err != nil {
			return modelproxy.LimitsReport{}, err
		}
		providers = append(providers, pc)
	} else {
		providers = h.subscriptionProviders()
	}
	report := h.fetchLimits(ctx, providers, "status")
	return report, modelproxy.LimitsReportError(report)
}
func (h *Runtime) ResetCredits(ctx context.Context, name string) (modelproxy.ResetCredits, error) {
	pc, err := h.limitsProvider(name)
	if err != nil {
		return modelproxy.ResetCredits{}, err
	}
	if subscription.QuotaKind(pc) != subscription.QuotaCodex {
		return modelproxy.ResetCredits{}, &modelproxy.LimitsError{Code: "unsupported_provider", Message: "reset credits require a codex-profile provider"}
	}
	credits := modelproxy.ResetCredits{Provider: name, FetchedAt: h.now(), Credits: []modelproxy.ResetCredit{}}
	account, err := h.limits.Resolve(ctx, pc)
	if err == nil {
		credits, err = account.ResetCredits(ctx)
	}
	if err != nil {
		credits = modelproxy.ResetCredits{Provider: name, FetchedAt: h.now(), Credits: []modelproxy.ResetCredit{}, Error: limitsError(err)}
	}
	h.logLimitsResult("reset_credits", credits.Provider, "", credits.Error, credits.Warnings)
	if credits.Error != nil {
		return credits, credits.Error
	}
	return credits, nil
}

func (h *Runtime) ResetLimits(ctx context.Context, req modelproxy.ResetRequest) (modelproxy.ResetResult, error) {
	if err := req.Validate(); err != nil {
		return modelproxy.ResetResult{Provider: req.Provider, CreditID: req.CreditID, RequestID: req.RequestID}, err
	}
	pc, err := h.limitsProvider(req.Provider)
	if err != nil {
		return modelproxy.ResetResult{Provider: req.Provider, CreditID: req.CreditID, RequestID: req.RequestID}, err
	}
	result := modelproxy.ResetResult{Provider: req.Provider, CreditID: req.CreditID, RequestID: req.RequestID, FetchedAt: h.now()}
	// Capture this provider's auth once, including refreshes. No model retry path.
	account, err := h.limits.Resolve(ctx, pc)
	if err == nil {
		result, err = account.Reset(ctx, req)
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
		status, statusErr := account.Status(ctx)
		if statusErr == nil {
			result.Status = &status
		} else {
			status = modelproxy.ProviderLimits{Provider: req.Provider, FetchedAt: h.now(), Error: limitsError(statusErr)}
			result.Warnings = append(result.Warnings, modelproxy.LimitsError{Code: "status_refresh_failed", Message: "reset succeeded; quota refresh failed"})
		}
		h.publishSubscriptionStatus(status)
		credits, creditsErr := account.ResetCredits(ctx)
		if creditsErr == nil {
			result.Credits = &credits
		} else {
			result.Warnings = append(result.Warnings, modelproxy.LimitsError{Code: "credits_refresh_failed", Message: "reset succeeded; reset credit refresh failed"})
		}
	}
	h.logLimitsResult("reset", result.Provider, result.Outcome, result.Error, result.Warnings)
	if result.Error != nil {
		return result, result.Error
	}
	return result, nil
}
