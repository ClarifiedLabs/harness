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

// series is one observed label set and its accumulated value. The value is a
// float64 because that is Prometheus's native counter/gauge type: integer token
// counts are represented exactly below 2^53 (far beyond any realistic volume),
// and accumulated cost carries only negligible floating-point rounding.
type series struct {
	labels map[string]string
	value  float64
}

// seriesTable stores the observed series for one collector, indexed by
// labelSeriesKey for O(1) upsert. It is not safe for concurrent use; the owning
// collector serializes access with its mutex.
type seriesTable struct {
	serieses []series
	index    map[string]int // labelSeriesKey -> position in serieses
}

// upsert adds value to (accumulate) or replaces (!accumulate) the series for the
// given label set, creating it if absent. This is the single shared accumulation
// path behind Counter.Add, Gauge.Set, and Gauge.Add.
func (t *seriesTable) upsert(labels map[string]string, value float64, accumulate bool) {
	key := labelSeriesKey(labels)
	if i, ok := t.index[key]; ok {
		if accumulate {
			t.serieses[i].value += value
		} else {
			t.serieses[i].value = value
		}
		return
	}
	if t.index == nil {
		t.index = make(map[string]int)
	}
	t.index[key] = len(t.serieses)
	t.serieses = append(t.serieses, series{labels: copyLabels(labels), value: value})
}

// snapshot returns a copy of the stored series, safe to sort and render without
// holding the collector lock.
func (t *seriesTable) snapshot() []series {
	return append([]series(nil), t.serieses...)
}

// labelSeriesKey returns a deterministic join of labels (sorted by name) used as
// the dedup/accumulation key. Empty-valued labels are skipped: Prometheus treats
// an empty-valued label as identical to a missing one, so `{a:"x",b:""}` and
// `{a:"x"}` produce the same key and accumulate into one series.
func labelSeriesKey(labels map[string]string) string {
	names := make([]string, 0, len(labels))
	for k, v := range labels {
		if v == "" {
			continue
		}
		names = append(names, k)
	}
	if len(names) == 0 {
		return ""
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

// writeCollector renders one collector's HELP/TYPE preamble followed by its
// series, sorted by label value column-by-column so output is stable across
// runs. snapshot must be a private copy the caller owns; it is sorted in place.
func writeCollector(w io.Writer, m meta, snapshot []series) {
	writeMeta(w, m)
	names := observedLabelNames(snapshot)
	sort.SliceStable(snapshot, func(i, j int) bool {
		return seriesLess(names, snapshot[i], snapshot[j])
	})
	for _, s := range snapshot {
		writeSeries(w, m.metricName, s)
	}
}

// Counter is a monotonically increasing counter metric.
type Counter struct {
	meta
	mu    sync.Mutex
	table seriesTable
}

// Inc adds 1 for the given label set.
func (c *Counter) Inc(labels map[string]string) { c.Add(1, labels) }

// Add adds value to the counter for the given label set. It is safe for
// concurrent use.
func (c *Counter) Add(value float64, labels map[string]string) {
	c.mu.Lock()
	c.table.upsert(labels, value, true)
	c.mu.Unlock()
}

func (c *Counter) write(w io.Writer) {
	c.mu.Lock()
	snapshot := c.table.snapshot()
	c.mu.Unlock()
	writeCollector(w, c.meta, snapshot)
}

// Gauge is a metric that can go up and down.
type Gauge struct {
	meta
	mu    sync.Mutex
	table seriesTable
}

// Set replaces the gauge value for the given label set.
func (g *Gauge) Set(value float64, labels map[string]string) {
	g.mu.Lock()
	g.table.upsert(labels, value, false)
	g.mu.Unlock()
}

// Add adjusts the gauge value for the given label set, creating it at zero if
// absent.
func (g *Gauge) Add(value float64, labels map[string]string) {
	g.mu.Lock()
	g.table.upsert(labels, value, true)
	g.mu.Unlock()
}

func (g *Gauge) write(w io.Writer) {
	g.mu.Lock()
	snapshot := g.table.snapshot()
	g.mu.Unlock()
	writeCollector(w, g.meta, snapshot)
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
// not change a stored series. Empty-valued labels are dropped so a stored series
// never carries one (an empty value is equivalent to a missing label), which
// makes it render bare and coalesce with the missing-label series.
func copyLabels(labels map[string]string) map[string]string {
	out := make(map[string]string, len(labels))
	for k, v := range labels {
		if v == "" {
			continue
		}
		out[k] = v
	}
	if len(out) == 0 {
		return nil
	}
	return out
}
