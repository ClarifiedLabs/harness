package server

import (
	"sync"
	"time"

	"harness/internal/metrics"
	"harness/internal/modelproxy/protocol"
)

const subscriptionMetricPrefix = "model_proxy_subscription_"

type subscriptionMetricFamily struct{ name, help string }

var subscriptionMetricFamilies = []subscriptionMetricFamily{
	{"refresh_success", "Whether the most recent subscription status refresh succeeded (1 or 0)."},
	{"last_refresh_timestamp_seconds", "Unix timestamp of the most recent subscription status refresh attempt."},
	{"last_success_timestamp_seconds", "Unix fetch timestamp of the most recent successful subscription status refresh."},
	{"used_percent", "Last reported subscription quota used percentage, not model token accounting."},
	{"remaining_percent", "Last reported subscription quota remaining percentage."},
	{"used", "Last reported used quota in provider-defined units."},
	{"remaining", "Last reported remaining quota in provider-defined units."},
	{"limit", "Last reported quota limit in provider-defined units."},
	{"window_duration_seconds", "Last reported subscription quota window duration in seconds."},
	{"reset_timestamp_seconds", "Unix timestamp of the last reported automatic quota reset, not a credit redemption."},
	{"allowed", "Last authoritative provider admission flag (1 allowed, 0 denied); absent when unknown."},
	{"limit_reached", "Last reported quota limit-reached flag (1 or 0); absent when unknown."},
	{"reset_credits_available", "Last reported count of available usage-limit reset credits, not spend credits."},
}

type subscriptionMetricSnapshot struct {
	refreshed time.Time
	succeeded time.Time
	success   bool
	values    map[string][]metrics.GaugeSample
}

type subscriptionMetrics struct {
	mu        sync.Mutex
	gauges    map[string]*metrics.Gauge
	providers map[string]subscriptionMetricSnapshot
}

func newSubscriptionMetrics(r *metrics.Registry) *subscriptionMetrics {
	out := &subscriptionMetrics{gauges: make(map[string]*metrics.Gauge), providers: make(map[string]subscriptionMetricSnapshot)}
	for _, family := range subscriptionMetricFamilies {
		out.gauges[family.name] = r.Gauge(subscriptionMetricPrefix+family.name, family.help)
	}
	return out
}

func (h *Handler) publishSubscriptionStatus(status protocol.ProviderLimits) {
	if h.subscriptionMetrics != nil {
		h.subscriptionMetrics.update(status, h.now())
	}
}

// Store only normalized numeric samples. Provider-controlled display names,
// plan/account identifiers, and credit IDs never become metric labels.
func (m *subscriptionMetrics) update(status protocol.ProviderLimits, now time.Time) {
	m.mu.Lock()
	defer m.mu.Unlock()
	previous := m.providers[status.Provider]
	completed := status.FetchedAt
	if completed.IsZero() {
		completed = now
	}
	// A fan-out result can be published after a newer on-demand refresh.
	// Never let that older completion overwrite current data or health.
	if completed.Before(previous.refreshed) {
		return
	}
	previous.refreshed = completed
	previous.success = status.Error == nil
	if previous.success {
		previous.succeeded = completed
		previous.values = subscriptionSamples(status)
	}
	// A failed refresh retains last-good samples; success replaces the complete
	// provider snapshot, deleting absent fields and vanished windows.
	m.providers[status.Provider] = previous
	all := make(map[string][]metrics.GaugeSample, len(m.gauges))
	for provider, snapshot := range m.providers {
		labels := map[string]string{"provider": provider}
		success := 0.0
		if snapshot.success {
			success = 1
		}
		all["refresh_success"] = append(all["refresh_success"], metrics.GaugeSample{Value: success, Labels: labels})
		all["last_refresh_timestamp_seconds"] = append(all["last_refresh_timestamp_seconds"], metrics.GaugeSample{Value: unixSeconds(snapshot.refreshed), Labels: labels})
		if !snapshot.succeeded.IsZero() {
			all["last_success_timestamp_seconds"] = append(all["last_success_timestamp_seconds"], metrics.GaugeSample{Value: unixSeconds(snapshot.succeeded), Labels: labels})
		}
		for name, samples := range snapshot.values {
			all[name] = append(all[name], samples...)
		}
	}
	for name, gauge := range m.gauges {
		gauge.Replace(all[name])
	}
}

func unixSeconds(t time.Time) float64 { return float64(t.Unix()) + float64(t.Nanosecond())/1e9 }

func subscriptionSamples(status protocol.ProviderLimits) map[string][]metrics.GaugeSample {
	out := make(map[string][]metrics.GaugeSample)
	add := func(name string, value float64, labels map[string]string) {
		out[name] = append(out[name], metrics.GaugeSample{Value: value, Labels: labels})
	}
	addBool := func(name string, value *bool, labels map[string]string) {
		if value != nil {
			n := 0.0
			if *value {
				n = 1
			}
			add(name, n, labels)
		}
	}
	for _, pool := range status.Pools {
		poolLabels := map[string]string{"provider": status.Provider, "pool": pool.ID}
		addBool("allowed", pool.Allowed, poolLabels)
		addBool("limit_reached", pool.LimitReached, poolLabels)
		for _, window := range pool.Windows {
			labels := map[string]string{"provider": status.Provider, "pool": pool.ID, "window": window.ID}
			for _, field := range []struct {
				name  string
				value *float64
			}{{"used_percent", window.UsedPercent}, {"remaining_percent", window.RemainingPercent}} {
				if field.value != nil {
					add(field.name, *field.value, labels)
				}
			}
			counts := map[string]string{"provider": status.Provider, "pool": pool.ID, "window": window.ID, "unit": window.Unit}
			for _, field := range []struct {
				name  string
				value *float64
			}{{"used", window.Used}, {"remaining", window.Remaining}, {"limit", window.Limit}} {
				if field.value != nil {
					add(field.name, *field.value, counts)
				}
			}
			if window.DurationSeconds != nil {
				add("window_duration_seconds", float64(*window.DurationSeconds), labels)
			}
			if window.ResetAt != nil {
				add("reset_timestamp_seconds", unixSeconds(*window.ResetAt), labels)
			} else if window.ResetAfterSeconds != nil && !status.FetchedAt.IsZero() {
				add("reset_timestamp_seconds", unixSeconds(status.FetchedAt)+float64(*window.ResetAfterSeconds), labels)
			}
		}
	}
	if status.ResetCredits != nil && status.ResetCredits.AvailableCount != nil {
		add("reset_credits_available", float64(*status.ResetCredits.AvailableCount), map[string]string{"provider": status.Provider})
	}
	return out
}
