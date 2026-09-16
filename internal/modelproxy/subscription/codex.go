package subscription

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"

	"harness/internal/modelproxy/protocol"
)

const codexBase = "https://chatgpt.com/backend-api/wham"

type codexWindow struct {
	UsedPercent *float64 `json:"used_percent"`
	Duration    *int64   `json:"limit_window_seconds"`
	ResetAt     *int64   `json:"reset_at"`
	ResetAfter  *int64   `json:"reset_after_seconds"`
}
type codexRateLimit struct {
	Allowed      *bool        `json:"allowed"`
	LimitReached *bool        `json:"limit_reached"`
	Primary      *codexWindow `json:"primary_window"`
	Secondary    *codexWindow `json:"secondary_window"`
}

func (a *Account) codex(ctx context.Context) (protocol.ProviderLimits, error) {
	var raw struct {
		Plan       *string         `json:"plan_type"`
		RateLimit  *codexRateLimit `json:"rate_limit"`
		Additional []struct {
			Name      *string         `json:"limit_name"`
			Feature   *string         `json:"metered_feature"`
			RateLimit *codexRateLimit `json:"rate_limit"`
		} `json:"additional_rate_limits"`
		Credits *struct {
			Available *int64 `json:"available_count"`
		} `json:"rate_limit_reset_credits"`
	}
	var out protocol.ProviderLimits
	if err := a.request(ctx, http.MethodGet, codexBase+"/usage", nil, &raw); err != nil {
		return out, err
	}
	out.Plan = label("", raw.Plan)
	if raw.RateLimit != nil {
		pool, err := parseCodexPool(*raw.RateLimit, "codex", "Codex")
		if err != nil {
			return out, err
		}
		out.Pools = append(out.Pools, pool)
	}
	for i, extra := range raw.Additional {
		if extra.RateLimit == nil {
			return out, invalidPayload()
		}
		pool, err := parseCodexPool(*extra.RateLimit, fmt.Sprintf("additional-%d", i+1), label("Additional quota", extra.Name, extra.Feature))
		if err != nil {
			return out, err
		}
		out.Pools = append(out.Pools, pool)
	}
	if raw.Credits != nil {
		if !validInts(raw.Credits.Available) {
			return out, invalidPayload()
		}
		out.ResetCredits = &protocol.ResetCreditSummary{AvailableCount: raw.Credits.Available}
	}
	if len(out.Pools) == 0 {
		if out.Plan == "" && (out.ResetCredits == nil || out.ResetCredits.AvailableCount == nil) {
			return out, invalidPayload()
		}
		out.Warnings = append(out.Warnings, *safeError("quota_unavailable", "provider did not report quota windows or admission status"))
	}
	return out, nil
}

func parseCodexPool(raw codexRateLimit, id, name string) (protocol.LimitPool, error) {
	pool := protocol.LimitPool{ID: id, Name: name, Allowed: raw.Allowed, LimitReached: raw.LimitReached}
	for i, rawWindow := range []*codexWindow{raw.Primary, raw.Secondary} {
		if rawWindow == nil {
			continue
		}
		if !validNumbers(rawWindow.UsedPercent) || !validInts(rawWindow.Duration, rawWindow.ResetAfter, rawWindow.ResetAt) {
			return pool, invalidPayload()
		}
		w := protocol.LimitWindow{ID: []string{"primary", "secondary"}[i], Name: []string{"Primary window", "Secondary window"}[i], UsedPercent: rawWindow.UsedPercent, DurationSeconds: rawWindow.Duration, ResetAfterSeconds: rawWindow.ResetAfter}
		if w.UsedPercent == nil && w.DurationSeconds == nil && w.ResetAfterSeconds == nil && rawWindow.ResetAt == nil {
			return pool, invalidPayload()
		}
		if w.UsedPercent != nil && *w.UsedPercent <= 100 {
			w.RemainingPercent = pointer(100 - *w.UsedPercent)
		}
		var err error
		w.ResetAt, err = unixTime(rawWindow.ResetAt, false)
		if err != nil {
			return pool, err
		}
		pool.Windows = append(pool.Windows, w)
	}
	// Null windows are normal when admission status is still reported. An empty
	// object with neither quota nor admission data is not a usable quota pool.
	if len(pool.Windows) == 0 && pool.Allowed == nil && pool.LimitReached == nil {
		return pool, invalidPayload()
	}
	return pool, nil
}

type codexCredit struct {
	ID          string  `json:"id"`
	ResetType   string  `json:"reset_type"`
	Status      string  `json:"status"`
	GrantedAt   *string `json:"granted_at"`
	ExpiresAt   *string `json:"expires_at"`
	Title       *string `json:"title"`
	Description *string `json:"description"`
}

func (a *Account) ResetCredits(ctx context.Context) (protocol.ResetCredits, error) {
	out := protocol.ResetCredits{Provider: a.provider, FetchedAt: a.client.now().UTC(), Credits: []protocol.ResetCredit{}}
	if a.provider != "openai-codex" {
		return out, safeError("unsupported_provider", "reset credits are supported only for openai-codex")
	}
	var raw struct {
		Available *int64         `json:"available_count"`
		Credits   *[]codexCredit `json:"credits"`
	}
	if err := a.request(ctx, http.MethodGet, codexBase+"/rate-limit-reset-credits", nil, &raw); err != nil {
		return out, err
	}
	out.FetchedAt = a.client.now().UTC()
	if raw.Credits == nil || !validInts(raw.Available) {
		return out, invalidPayload()
	}
	out.AvailableCount = raw.Available
	seen := make(map[string]bool)
	for _, credit := range *raw.Credits {
		if !protocol.ValidLimitsID(credit.ID) || credit.ResetType == "" || credit.Status == "" || credit.GrantedAt == nil || seen[credit.ID] {
			return out, invalidPayload()
		}
		seen[credit.ID] = true
		granted, err := parseTime(credit.GrantedAt)
		if err != nil {
			return out, err
		}
		expires, err := parseTime(credit.ExpiresAt)
		if err != nil {
			return out, err
		}
		redeemable := credit.ResetType == "codex_rate_limits" && credit.Status == "available" && (expires == nil || expires.After(out.FetchedAt))
		out.Credits = append(out.Credits, protocol.ResetCredit{
			ID: credit.ID, ResetType: protocol.LimitsLabel(credit.ResetType), Status: protocol.LimitsLabel(credit.Status), GrantedAt: granted, ExpiresAt: expires,
			Title: label("", credit.Title), Description: label("", credit.Description), Redeemable: &redeemable,
		})
	}
	return out, nil
}

// Reset sends exactly the selected credit and request identity. No preflight,
// fallback selection, automatic retry, or refresh occurs here: the backend owns
// eligibility and idempotency, even when a previously consumed credit disappears.
func (a *Account) Reset(ctx context.Context, request protocol.ResetRequest) (protocol.ResetResult, error) {
	out := protocol.ResetResult{Provider: a.provider, CreditID: request.CreditID, RequestID: request.RequestID, FetchedAt: a.client.now().UTC()}
	if err := request.Validate(); err != nil {
		return out, err
	}
	if a.provider != request.Provider {
		return out, safeError("invalid_request", "reset provider does not match the resolved account")
	}
	if err := ctx.Err(); err != nil {
		return out, transportError(err)
	}
	body, _ := json.Marshal(struct {
		RequestID string `json:"redeem_request_id"`
		CreditID  string `json:"credit_id"`
	}{request.RequestID, request.CreditID})
	var raw struct {
		Code         string `json:"code"`
		WindowsReset *int64 `json:"windows_reset"`
	}
	err := a.request(ctx, http.MethodPost, codexBase+"/rate-limit-reset-credits/consume", body, &raw)
	out.FetchedAt = a.client.now().UTC()
	if err == nil {
		switch raw.Code {
		case "reset", "nothing_to_reset", "no_credit", "already_redeemed":
			if !validInts(raw.WindowsReset) {
				err = invalidPayload()
			} else {
				out.Outcome = raw.Code
			}
		default:
			err = safeError("unknown_outcome", "provider returned an unknown reset outcome")
		}
	}
	if err != nil {
		var e *protocol.LimitsError
		if !errors.As(err, &e) {
			e = safeError("unknown_outcome", "provider reset outcome could not be determined")
		}
		switch e.Code {
		case "timeout", "canceled", "transport_error", "upstream_error", "invalid_payload", "unknown_outcome", "redirect":
			e.Indeterminate = true
			out.Outcome = "indeterminate"
		}
		out.Error = e
		return out, e
	}
	out.WindowsReset = raw.WindowsReset
	return out, nil
}
