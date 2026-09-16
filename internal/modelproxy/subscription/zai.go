package subscription

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"harness/internal/modelproxy/protocol"
)

type zaiLimit struct {
	Type          string   `json:"type"`
	Unit          *int64   `json:"unit"`
	Number        *int64   `json:"number"`
	Percentage    *float64 `json:"percentage"`
	Usage         *float64 `json:"usage"`
	Current       *float64 `json:"currentValue"`
	Remaining     *float64 `json:"remaining"`
	NextResetTime *int64   `json:"nextResetTime"`
	UsageDetails  []struct {
		ModelCode *string  `json:"modelCode"`
		Usage     *float64 `json:"usage"`
	} `json:"usageDetails"`
}

func (a *Account) zai(ctx context.Context) (protocol.ProviderLimits, error) {
	var raw struct {
		Success *bool  `json:"success"`
		Code    *int64 `json:"code"`
		Data    *struct {
			PlanName    *string    `json:"planName"`
			Plan        *string    `json:"plan"`
			PlanType    *string    `json:"plan_type"`
			PackageName *string    `json:"packageName"`
			Level       *string    `json:"level"`
			Limits      []zaiLimit `json:"limits"`
		} `json:"data"`
	}
	var out protocol.ProviderLimits
	if err := a.request(ctx, http.MethodGet, "https://api.z.ai/api/monitor/usage/quota/limit", nil, &raw); err != nil {
		return out, err
	}
	if raw.Success != nil && !*raw.Success || raw.Code != nil && *raw.Code != 200 {
		return out, safeError("upstream_error", "provider rejected the subscription query")
	}
	if raw.Success == nil || raw.Code == nil || raw.Data == nil || len(raw.Data.Limits) == 0 {
		return out, invalidPayload()
	}
	out.Plan = label("", raw.Data.PlanName, raw.Data.Plan, raw.Data.PlanType, raw.Data.PackageName, raw.Data.Level)
	for i, limit := range raw.Data.Limits {
		if limit.Type == "" || !validNumbers(limit.Percentage, limit.Usage, limit.Current, limit.Remaining) || !validInts(limit.Unit, limit.Number, limit.NextResetTime) {
			return out, invalidPayload()
		}
		for _, detail := range limit.UsageDetails {
			if !validNumbers(detail.Usage) {
				return out, invalidPayload()
			}
		}
		name, unit := "Unknown quota", "quota units"
		switch limit.Type {
		case "TOKENS_LIMIT":
			name = "Coding Plan token quota"
		case "CREDIT_LIMIT":
			name, unit = "Coding Plan credit quota", "quota credits"
		case "TIME_LIMIT":
			name, unit = "MCP quota", "calls"
		default:
			out.Warnings = append(out.Warnings, *safeError("unknown_quota_type", "provider returned an unrecognized quota type"))
		}
		w := protocol.LimitWindow{ID: fmt.Sprintf("window-%d", i+1), Name: name, Unit: unit, Limit: limit.Usage, Used: limit.Current, Remaining: limit.Remaining, UsedPercent: limit.Percentage}
		if !hasQuota(w) {
			return out, invalidPayload()
		}
		if w.UsedPercent != nil && *w.UsedPercent <= 100 {
			w.RemainingPercent = pointer(100 - *w.UsedPercent)
		}
		var multiplier int64
		if limit.Unit != nil {
			multiplier = map[int64]int64{1: 86400, 3: 3600, 5: 60, 6: 604800}[*limit.Unit]
			if multiplier == 0 {
				out.Warnings = append(out.Warnings, *safeError("unknown_quota_unit", "provider returned an unrecognized quota duration unit"))
			}
		}
		var err error
		w.DurationSeconds, err = duration(limit.Number, multiplier)
		if err != nil {
			return out, err
		}
		if limit.Type == "TIME_LIMIT" && limit.Unit != nil && *limit.Unit == 5 && limit.Number != nil && *limit.Number == 1 {
			w.Name = "Monthly MCP quota"
			w.DurationSeconds = nil // This marker is not a one-minute or fixed 30-day window.
		}
		w.ResetAt, err = unixTime(limit.NextResetTime, true)
		if err != nil {
			return out, err
		}
		// Preserve the reported absolute timestamp, but explicitly flag implausibility;
		// never silently repair timezone offsets or manufacture a reset deadline.
		if w.ResetAt != nil && w.DurationSeconds != nil && *w.DurationSeconds > 0 {
			seconds := w.ResetAt.Sub(a.client.now()).Seconds()
			if seconds > float64(*w.DurationSeconds)+time.Minute.Seconds() {
				out.Warnings = append(out.Warnings, *safeError("implausible_reset", "provider reset time exceeds the reported quota window; timestamp is unmodified"))
			}
		}
		if inconsistent(w) {
			out.Warnings = append(out.Warnings, countsWarning())
		}
		out.Pools = append(out.Pools, protocol.LimitPool{ID: fmt.Sprintf("quota-%d", i+1), Name: name, Windows: []protocol.LimitWindow{w}})
	}
	return out, nil
}
