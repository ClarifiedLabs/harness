package subscription

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"harness/internal/modelproxy/protocol"
)

type kimiDetail struct {
	Name           *string         `json:"name"`
	Title          *string         `json:"title"`
	Scope          *string         `json:"scope"`
	Limit          *kimiNumber     `json:"limit"`
	Used           *kimiNumber     `json:"used"`
	Remaining      *kimiNumber     `json:"remaining"`
	Duration       *kimiNumber     `json:"duration"`
	TimeUnit       *string         `json:"timeUnit"`
	ResetAt        *string         `json:"reset_at"`
	ResetAtCamel   *string         `json:"resetAt"`
	ResetTime      *string         `json:"reset_time"`
	ResetTimeCamel *string         `json:"resetTime"`
	ResetIn        *kimiNumber     `json:"reset_in"`
	ResetInCamel   *kimiNumber     `json:"resetIn"`
	TTL            *kimiNumber     `json:"ttl"`
	Window         json.RawMessage `json:"window"`
}
type kimiWindow struct {
	Duration *kimiNumber `json:"duration"`
	TimeUnit *string     `json:"timeUnit"`
}
type kimiLimit struct {
	kimiDetail
	Detail *kimiDetail `json:"detail"`
}

func (a *Account) kimi(ctx context.Context) (protocol.ProviderLimits, error) {
	var raw struct {
		Usage  *kimiDetail `json:"usage"`
		Limits []kimiLimit `json:"limits"`
	}
	var out protocol.ProviderLimits
	usagesURL := a.usagesURL
	if usagesURL == "" {
		usagesURL = "https://api.kimi.com/coding/v1/usages"
	}
	if err := a.request(ctx, http.MethodGet, usagesURL, nil, &raw); err != nil {
		return out, err
	}
	pool := protocol.LimitPool{ID: "coding", Name: "Coding Plan"}
	if raw.Usage != nil {
		w, err := parseKimi(*raw.Usage, "weekly", "Weekly limit")
		if err != nil {
			return out, err
		}
		pool.Windows = append(pool.Windows, w)
		if unknownKimiUnit(*raw.Usage) {
			out.Warnings = append(out.Warnings, kimiUnitWarning())
		}
	}
	for i, rawLimit := range raw.Limits {
		detail := rawLimit.kimiDetail
		if rawLimit.Detail != nil {
			detail = *rawLimit.Detail
		}
		w, err := parseKimi(detail, fmt.Sprintf("limit-%d", i+1), fmt.Sprintf("Limit #%d", i+1))
		if err != nil {
			return out, err
		}
		w.Name = label(w.Name, rawLimit.Name, rawLimit.Title, rawLimit.Scope)
		if unknownKimiUnit(detail) || unknownKimiUnit(rawLimit.kimiDetail) {
			out.Warnings = append(out.Warnings, kimiUnitWarning())
		}
		// Nested window metadata belongs to the outer entry, not its quota detail.
		if presentJSON(rawLimit.Window) {
			var window kimiWindow
			if json.Unmarshal(rawLimit.Window, &window) == nil {
				w.DurationSeconds, err = kimiDuration(window.Duration, window.TimeUnit)
				if err != nil {
					return out, err
				}
			} else if rawLimit.Detail != nil {
				return out, invalidPayload()
			}
		} else if rawLimit.Duration != nil {
			w.DurationSeconds, err = kimiDuration(rawLimit.Duration, rawLimit.TimeUnit)
			if err != nil {
				return out, err
			}
		}
		pool.Windows = append(pool.Windows, w)
	}
	if len(pool.Windows) == 0 {
		return out, invalidPayload()
	}
	for _, w := range pool.Windows {
		if inconsistent(w) {
			out.Warnings = append(out.Warnings, countsWarning())
		}
	}
	out.Pools = []protocol.LimitPool{pool}
	return out, nil
}

func parseKimi(d kimiDetail, id, name string) (protocol.LimitWindow, error) {
	w := protocol.LimitWindow{ID: id, Name: label(name, d.Name, d.Title, d.Scope), Unit: "quota units", Limit: number(d.Limit), Used: number(d.Used), Remaining: number(d.Remaining)}
	if !hasQuota(w) {
		return w, invalidPayload()
	}
	// Derive missing counts only when the supplied counts are internally valid.
	if w.Limit != nil {
		if w.Used == nil && w.Remaining != nil && *w.Remaining <= *w.Limit {
			w.Used = pointer(*w.Limit - *w.Remaining)
		}
		if w.Remaining == nil && w.Used != nil && *w.Used <= *w.Limit {
			w.Remaining = pointer(*w.Limit - *w.Used)
		}
		if *w.Limit > 0 && !inconsistent(w) {
			if w.Used != nil {
				w.UsedPercent = pointer(*w.Used / *w.Limit * 100)
			}
			if w.Remaining != nil {
				w.RemainingPercent = pointer(*w.Remaining / *w.Limit * 100)
			}
		}
	}
	var err error
	w.DurationSeconds, err = kimiDuration(d.Duration, d.TimeUnit)
	if err != nil {
		return w, err
	}
	for _, s := range []*string{d.ResetAt, d.ResetAtCamel, d.ResetTime, d.ResetTimeCamel} {
		t, e := parseTime(s)
		if e != nil {
			return w, e
		}
		if t != nil && w.ResetAt == nil {
			w.ResetAt = t
		}
	}
	for _, n := range []*kimiNumber{d.ResetIn, d.ResetInCamel, d.TTL} {
		seconds, e := whole(n)
		if e != nil {
			return w, e
		}
		if seconds != nil && w.ResetAfterSeconds == nil {
			w.ResetAfterSeconds = seconds
		}
	}
	if presentJSON(d.Window) {
		if strings.HasPrefix(strings.TrimSpace(string(d.Window)), "{") {
			var window kimiWindow
			if json.Unmarshal(d.Window, &window) != nil {
				return w, invalidPayload()
			}
			if w.DurationSeconds == nil {
				w.DurationSeconds, err = kimiDuration(window.Duration, window.TimeUnit)
				if err != nil {
					return w, err
				}
			}
		} else {
			var seconds kimiNumber
			if json.Unmarshal(d.Window, &seconds) != nil {
				return w, invalidPayload()
			}
			n, e := whole(&seconds)
			if e != nil {
				return w, e
			}
			if w.ResetAfterSeconds == nil {
				w.ResetAfterSeconds = n
			}
		}
	}
	return w, nil
}

func kimiDuration(n *kimiNumber, unit *string) (*int64, error) {
	count, err := whole(n)
	if err != nil {
		return nil, err
	}
	if unit == nil {
		return nil, nil
	}
	return duration(count, kimiUnitMultiplier(*unit))
}

func kimiUnitMultiplier(unit string) int64 {
	// Kimi also uses protobuf-style names such as TIME_UNIT_MINUTE.
	unit = strings.TrimPrefix(strings.ToUpper(unit), "TIME_UNIT_")
	return map[string]int64{"SECOND": 1, "SECONDS": 1, "MINUTE": 60, "MINUTES": 60, "HOUR": 3600, "HOURS": 3600, "DAY": 86400, "DAYS": 86400, "WEEK": 604800, "WEEKS": 604800}[unit]
}

func unknownKimiUnit(detail kimiDetail) bool {
	if detail.TimeUnit != nil && kimiUnitMultiplier(*detail.TimeUnit) == 0 {
		return true
	}
	var window kimiWindow
	return presentJSON(detail.Window) && json.Unmarshal(detail.Window, &window) == nil && window.TimeUnit != nil && kimiUnitMultiplier(*window.TimeUnit) == 0
}

func kimiUnitWarning() protocol.LimitsError {
	return *safeError("unknown_quota_unit", "provider returned an unrecognized quota duration unit")
}
