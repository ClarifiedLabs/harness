package otel

import (
	"maps"
	"strconv"
	"time"

	"harness/internal/execution"
)

var _ execution.Observer = (*Sink)(nil)

var durationBounds = []float64{.001, .005, .01, .05, .1, .25, .5, 1, 5, 30, 60, 300, 1800}
var tokenBounds = []float64{0, 100, 1000, 10000, 50000, 100000, 500000, 1000000}
var countBounds = []float64{0, 1, 2, 3, 5, 10, 20, 50, 100}
var byteBounds = []float64{0, 256, 1024, 4096, 16384, 65536, 262144, 1048576}

// SetWorkGroup installs the root-owned lifecycle group for future scopes. A
// sink does not allocate its own group: multiple roots may share one group.
// Existing scope snapshots retain their original group until their work ends.
func (s *Sink) SetWorkGroup(group *execution.Group) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.workGroup = group
}

// Scope returns immutable identity and group snapshots. Delayed/background
// work must use this scope, not the live sink after a REPL/root switch.
func (s *Sink) Scope() execution.Scope {
	if s == nil {
		return execution.Scope{}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return execution.Scope{Observer: s, Group: s.workGroup, Identity: execution.Identity{Provider: s.provider, Model: s.model, Agent: s.agentName, Delegate: s.delegateLabel()}}
}

func identityAttrs(id execution.Identity) map[string]string {
	attrs := map[string]string{"delegate": strconv.FormatBool(id.Delegate == "true")}
	for k, v := range map[string]string{"provider": id.Provider, "model": id.Model, "agent": id.Agent} {
		if v != "" {
			limit := 64
			if k == "model" {
				limit = 128
			}
			attrs[k] = truncate(v, limit)
		}
	}
	return attrs
}
func labels(attrs map[string]string, key, value string) map[string]string {
	out := maps.Clone(attrs)
	out[key] = value
	return out
}
func bounded(value, fallback string, allowed ...string) string {
	for _, v := range allowed {
		if value == v {
			return v
		}
	}
	return fallback
}
func (s *Sink) duration(name string, d *time.Duration, attrs map[string]string) {
	if d != nil && *d >= 0 {
		s.exp.RecordHistogram(name, "s", d.Seconds(), attrs, durationBounds)
	}
}

// In-flight is additive even when the exporter routes many identities to its
// reserved overflow series. Keep the value at the exporter's admitted point,
// not in an unbounded second map or last-sample-per-identity overflow gauge.
func (s *Sink) inflight(name string, delta int64, attrs map[string]string) {
	s.adjustGauge(name, delta, attrs, false)
}

func (s *Sink) maximum(name string, value int64, attrs map[string]string) {
	s.adjustGauge(name, value, attrs, true)
}

func (s *Sink) adjustGauge(name string, delta int64, attrs map[string]string, maximum bool) {
	e := s.exp
	e.mu.Lock()
	defer e.mu.Unlock()
	unit := "{operation}"
	if maximum {
		unit = "{call}"
	}
	m := e.metricLocked(name, unit, "gauge", false, nil)
	if m == nil {
		return
	}
	kv, fp := e.pointIdentityLocked(m, attrs)
	pt := m.points[fp]
	if pt == nil {
		pt = &numberPoint{attrs: kv}
		m.points[fp] = pt
		e.chargePointLocked(m, fp)
	}
	if maximum {
		pt.intValue = max(pt.intValue, delta)
	} else {
		pt.intValue = max(0, pt.intValue+delta)
	}
}
