package otel

import (
	"log/slog"
	"strings"
	"time"

	"harness/internal/logging"
)

// Diagnostic dimensions are fixed enums, never rejected metric names or labels.
type overflowReason uint8

const (
	overflowAttributes overflowReason = iota
	overflowFamilySeries
	overflowGlobalSeries
	overflowFamilyBytes
	overflowGlobalBytes
	overflowReasonCount
	periodicOverflowWarningInterval = 5 * time.Minute
)

var overflowReasonNames = [...]string{"attributes", "family_series", "global_series", "family_bytes", "global_bytes"}

type metricLossSnapshot struct {
	Health
	reasons  [overflowReasonCount]uint64
	families int
	bytes    int
}

func (e *Exporter) lossSnapshot() metricLossSnapshot {
	e.mu.Lock()
	defer e.mu.Unlock()
	return metricLossSnapshot{Health: e.healthLocked(), reasons: e.overflowReasons, families: e.overflowFamilies, bytes: e.approxBytes}
}

// Owned by the periodic worker. Only emitted warnings advance the baseline, so
// suppressed ticks and export failures cannot consume unreported loss counts.
type periodicLossReporter struct {
	last     time.Time
	reported metricLossSnapshot
}

func (p *periodicLossReporter) report(logger *slog.Logger, now time.Time, loss metricLossSnapshot) {
	if logger == nil || (loss.Dropped <= p.reported.Dropped && loss.Overflow <= p.reported.Overflow) {
		return
	}
	// New outright drops warrant a prompt warning even during overflow spam.
	if loss.Dropped == p.reported.Dropped && !p.last.IsZero() && now.Sub(p.last) < periodicOverflowWarningInterval {
		return
	}
	var reasons []string
	for i, count := range loss.reasons {
		if count > p.reported.reasons[i] {
			reasons = append(reasons, overflowReasonNames[i])
		}
	}
	logger.Warn("periodic OTEL export lost metric detail", logging.Category("otel"),
		"dropped", loss.Dropped-p.reported.Dropped, "overflow", loss.Overflow-p.reported.Overflow,
		"overflow_families", loss.families, "active_series", loss.ActiveSeries,
		"resident_bytes", loss.bytes, "limits", strings.Join(reasons, ","))
	p.last, p.reported = now, loss
}
