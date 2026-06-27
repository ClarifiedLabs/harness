// Package metrics is a tiny, dependency-free Prometheus registry and text
// exposition (0.0.4) writer. It exists so harness auxiliary binaries can expose
// /metrics without pulling in a third-party client library, per AGENTS.md.
//
// Only counters and gauges are supported; histogram buckets are out of scope.
// Each collector derives its label-name set from the union of observed label
// sets, and renders series sorted by label name then by value, so output is
// stable across runs.
package metrics

import (
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
)

// metricKind is the Prometheus TYPE of a collector.
type metricKind string

const (
	kindCounter metricKind = "counter"
	kindGauge   metricKind = "gauge"
)

// Registry is an ordered collection of collectors. The zero value is not usable;
// use New.
type Registry struct {
	mu         sync.Mutex
	collectors []collector
}

// New returns an empty Registry.
func New() *Registry { return &Registry{} }

// collector is the shared behavior of Counter and Gauge.
type collector interface {
	name() string
	help() string
	kind() metricKind
	write(w io.Writer)
}

func (r *Registry) add(c collector) collector {
	r.mu.Lock()
	r.collectors = append(r.collectors, c)
	r.mu.Unlock()
	return c
}

// Counter registers a counter metric. Registering the same name twice is not
// detected; callers should register each family once.
func (r *Registry) Counter(name, help string) *Counter {
	c := &Counter{meta: meta{metricName: name, metricHelp: help, metricKind: kindCounter}}
	return r.add(c).(*Counter)
}

// Gauge registers a gauge metric.
func (r *Registry) Gauge(name, help string) *Gauge {
	g := &Gauge{meta: meta{metricName: name, metricHelp: help, metricKind: kindGauge}}
	return r.add(g).(*Gauge)
}

// Render writes the exposition format for all registered collectors to w, in
// registration order. Within a collector, series are sorted by label name then
// value. (Not named WriteTo to avoid colliding with io.WriterTo's signature.)
func (r *Registry) Render(w io.Writer) {
	r.mu.Lock()
	collectors := append([]collector(nil), r.collectors...)
	r.mu.Unlock()
	for _, c := range collectors {
		c.write(w)
	}
}

// Handler returns an http.Handler serving the exposition text at GET /metrics.
// Other methods get 405; other paths get 404. It performs no authentication.
func (r *Registry) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.URL.Path != "/metrics" {
			http.NotFound(w, req)
			return
		}
		if req.Method != http.MethodGet {
			w.Header().Set("Allow", "GET")
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
		r.Render(w)
	})
}

// meta holds the shared identity of a collector.
type meta struct {
	metricName string
	metricHelp string
	metricKind metricKind
}

func (m meta) name() string     { return m.metricName }
func (m meta) help() string     { return m.metricHelp }
func (m meta) kind() metricKind { return m.metricKind }

// series is one observed label set and its accumulated value.
type series struct {
	labels map[string]string
	value  float64
}

// labelSeriesKey returns a deterministic join of labels (sorted by name) used as
// the dedup/accumulation key. A missing label is omitted from the key; an empty
// value contributes `name=""`, which is a distinct key from a missing label.
func labelSeriesKey(labels map[string]string) string {
	if len(labels) == 0 {
		return ""
	}
	names := make([]string, 0, len(labels))
	for k := range labels {
		names = append(names, k)
	}
	sort.Strings(names)
	var b strings.Builder
	for i, k := range names {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(k)
		b.WriteString(`="`)
		b.WriteString(labels[k])
		b.WriteByte('"')
	}
	return b.String()
}

// observedLabelNames returns the sorted union of label names across all series,
// so the column order is stable even as new label sets are added later.
func observedLabelNames(serieses []series) []string {
	seen := map[string]bool{}
	for _, s := range serieses {
		for k := range s.labels {
			seen[k] = true
		}
	}
	names := make([]string, 0, len(seen))
	for k := range seen {
		names = append(names, k)
	}
	sort.Strings(names)
	return names
}

// writeMeta writes the # HELP and # TYPE header lines for a collector.
func writeMeta(w io.Writer, m meta) {
	var b strings.Builder
	b.WriteString("# HELP ")
	b.WriteString(m.metricName)
	b.WriteByte(' ')
	b.WriteString(escapeHelp(m.metricHelp))
	b.WriteByte('\n')
	b.WriteString("# TYPE ")
	b.WriteString(m.metricName)
	b.WriteByte(' ')
	b.WriteString(string(m.metricKind))
	b.WriteByte('\n')
	io.WriteString(w, b.String())
}

// writeSeries writes one metric line, rendering only the labels present in
// the series (so a missing label is omitted, distinct from an empty value).
// Labels are sorted by name. The caller has already sorted the series list.
func writeSeries(w io.Writer, name string, s series) {
	names := make([]string, 0, len(s.labels))
	for k := range s.labels {
		names = append(names, k)
	}
	sort.Strings(names)
	var b strings.Builder
	b.WriteString(name)
	if len(names) > 0 {
		b.WriteByte('{')
		for i, k := range names {
			if i > 0 {
				b.WriteByte(',')
			}
			b.WriteString(k)
			b.WriteString(`="`)
			b.WriteString(escapeLabelValue(s.labels[k]))
			b.WriteByte('"')
		}
		b.WriteByte('}')
	}
	b.WriteByte(' ')
	b.WriteString(formatFloat(s.value))
	b.WriteByte('\n')
	io.WriteString(w, b.String())
}

// formatFloat renders a metric value so integers have no trailing decimals and
// large totals stay exact (strconv 'g' with -1 precision).
func formatFloat(v float64) string {
	return strconv.FormatFloat(v, 'g', -1, 64)
}

// escapeLabelValue escapes a label value per the exposition format: backslash,
// double-quote, and newline are escaped.
func escapeLabelValue(v string) string {
	if !strings.ContainsAny(v, "\\\"\n") {
		return v
	}
	var b strings.Builder
	b.Grow(len(v))
	for _, r := range v {
		switch r {
		case '\\':
			b.WriteString(`\\`)
		case '"':
			b.WriteString(`\"`)
		case '\n':
			b.WriteString(`\n`)
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

// escapeHelp escapes backslash and newline in a # HELP line.
func escapeHelp(v string) string {
	if !strings.ContainsAny(v, "\\\n") {
		return v
	}
	var b strings.Builder
	b.Grow(len(v))
	for _, r := range v {
		switch r {
		case '\\':
			b.WriteString(`\\`)
		case '\n':
			b.WriteString(`\n`)
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

// Counter is a monotonically increasing counter metric.
type Counter struct {
	meta
	mu      sync.Mutex
	serieses []series
	order    []string // observed label names, in first-seen order
}

// Inc adds 1 for the given label set.
func (c *Counter) Inc(labels map[string]string) { c.Add(1, labels) }

// Add adds value to the counter for the given label set. It is safe for
// concurrent use.
func (c *Counter) Add(value float64, labels map[string]string) {
	key := labelSeriesKey(labels)
	c.mu.Lock()
	defer c.mu.Unlock()
	for i := range c.serieses {
		if labelSeriesKey(c.serieses[i].labels) == key {
			c.serieses[i].value += value
			return
		}
	}
	c.serieses = append(c.serieses, series{labels: copyLabels(labels), value: value})
}

func (c *Counter) write(w io.Writer) {
	c.mu.Lock()
	serieses := append([]series(nil), c.serieses...)
	c.mu.Unlock()
	writeMeta(w, c.meta)
	names := observedLabelNames(serieses)
	sorted := append([]series(nil), serieses...)
	sort.SliceStable(sorted, func(i, j int) bool {
		return seriesLess(names, sorted[i], sorted[j])
	})
	for _, s := range sorted {
		writeSeries(w, c.metricName, s)
	}
}

// Gauge is a metric that can go up and down.
type Gauge struct {
	meta
	mu      sync.Mutex
	serieses []series
}

// Set replaces the gauge value for the given label set.
func (g *Gauge) Set(value float64, labels map[string]string) {
	key := labelSeriesKey(labels)
	g.mu.Lock()
	defer g.mu.Unlock()
	for i := range g.serieses {
		if labelSeriesKey(g.serieses[i].labels) == key {
			g.serieses[i].value = value
			return
		}
	}
	g.serieses = append(g.serieses, series{labels: copyLabels(labels), value: value})
}

// Add adjusts the gauge value for the given label set, creating it at zero if
// absent.
func (g *Gauge) Add(value float64, labels map[string]string) {
	key := labelSeriesKey(labels)
	g.mu.Lock()
	defer g.mu.Unlock()
	for i := range g.serieses {
		if labelSeriesKey(g.serieses[i].labels) == key {
			g.serieses[i].value += value
			return
		}
	}
	g.serieses = append(g.serieses, series{labels: copyLabels(labels), value: value})
}

func (g *Gauge) write(w io.Writer) {
	g.mu.Lock()
	serieses := append([]series(nil), g.serieses...)
	g.mu.Unlock()
	writeMeta(w, g.meta)
	names := observedLabelNames(serieses)
	sorted := append([]series(nil), serieses...)
	sort.SliceStable(sorted, func(i, j int) bool {
		return seriesLess(names, sorted[i], sorted[j])
	})
	for _, s := range sorted {
		writeSeries(w, g.metricName, s)
	}
}

// seriesLess orders two series by label value, column by column, in the given
// label-name order. A missing value sorts before an empty one.
func seriesLess(names []string, a, b series) bool {
	for _, k := range names {
		av, aok := a.labels[k]
		bv, bok := b.labels[k]
		if aok != bok {
			return aok // present before absent
		}
		if av != bv {
			return av < bv
		}
	}
	return false
}

// copyLabels returns a defensive copy so later mutation of the caller's map does
// not change a stored series.
func copyLabels(labels map[string]string) map[string]string {
	if len(labels) == 0 {
		return nil
	}
	out := make(map[string]string, len(labels))
	for k, v := range labels {
		out[k] = v
	}
	return out
}
