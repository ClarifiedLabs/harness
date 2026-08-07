package otel

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"harness/internal/buildinfo"
)

// Exporter collects OTLP metrics and pushes them to the collector. It is safe for
// concurrent Record calls; Export is called on a best-effort background path and
// never blocks prompt completion.
type Exporter struct {
	cfg           Config
	client        *http.Client
	endpoint      string
	buildVersion  string
	resourceAttrs []keyValue
	startNano     string
	mu            sync.Mutex
	metrics       map[string]*aggregatedMetric
	dropped       int
	approxBytes   int
}

type aggregatedMetric struct {
	name        string
	unit        string
	kind        string // sum, gauge, histogram
	monotonic   bool
	temporality int // 2 = cumulative
	points      map[string]*numberPoint // keyed by attribute fingerprint
	histPoints  map[string]*histPoint
}

type numberPoint struct {
	attrs     []keyValue
	intValue  int64
	floatValue *float64
	hasFloat  bool
}

type histPoint struct {
	attrs  []keyValue
	count  uint64
	sum    float64
	bounds []float64
	buckets []uint64
}

const (
	aggTemporalityCumulative = 2
	maxQueuePoints           = 1024
	maxPayloadBytes          = 64 * 1024
)

func NewExporter(cfg Config, build buildinfo.Metadata, sessionID, provider, model, agent string, resourceAttrs map[string]string) (*Exporter, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	normalized, err := cfg.NormalizedEndpoint()
	if err != nil {
		return nil, err
	}
	if cfg.ServiceName == "" {
		cfg.ServiceName = "harness"
	}
	if cfg.Timeout == 0 {
		cfg.Timeout = 5 * time.Second
	}
	ra := []keyValue{
		stringAttr("service.name", truncate(cfg.ServiceName, 64)),
		stringAttr("service.version", truncate(build.Version, 64)),
	}
	if sessionID != "" {
		ra = append(ra, stringAttr("service.instance.id", truncate(sessionID, 64)))
		ra = append(ra, stringAttr("harness.session_id", truncate(sessionID, 64)))
	}
	if agent != "" {
		ra = append(ra, stringAttr("harness.agent", truncate(agent, 64)))
	}
	if provider != "" {
		ra = append(ra, stringAttr("harness.provider", truncate(provider, 64)))
	}
	if model != "" {
		ra = append(ra, stringAttr("harness.model", truncate(model, 128)))
	}
	for k, v := range resourceAttrs {
		ra = append(ra, stringAttr(k, truncate(v, 128)))
	}
	ra = sortedAttrs(ra)
	return &Exporter{
		cfg:           cfg,
		client:        &http.Client{Timeout: cfg.Timeout},
		endpoint:      normalized,
		buildVersion:  build.Version,
		resourceAttrs: ra,
		startNano:     strconv.FormatInt(time.Now().UnixNano(), 10),
		metrics:       make(map[string]*aggregatedMetric),
	}, nil
}

// Record helpers -----------------------------------------------------------

func (e *Exporter) RecordSum(name, unit string, value int64, attrs map[string]string) {
	if e == nil {
		return
	}
	e.recordNumber(name, unit, "sum", true, value, nil, false, attrs)
}

func (e *Exporter) RecordSumFloat(name, unit string, value float64, attrs map[string]string) {
	if e == nil {
		return
	}
	e.recordNumber(name, unit, "sum", true, 0, &value, true, attrs)
}

func (e *Exporter) RecordGauge(name, unit string, value int64, attrs map[string]string) {
	if e == nil {
		return
	}
	e.recordNumber(name, unit, "gauge", false, value, nil, false, attrs)
}

func (e *Exporter) RecordGaugeFloat(name, unit string, value float64, attrs map[string]string) {
	if e == nil {
		return
	}
	e.recordNumber(name, unit, "gauge", false, 0, &value, true, attrs)
}

func (e *Exporter) RecordHistogram(name, unit string, value float64, attrs map[string]string, bounds []float64) {
	if e == nil {
		return
	}
	e.recordHistogram(name, unit, value, attrs, bounds)
}

func (e *Exporter) recordNumber(name, unit, kind string, monotonic bool, intVal int64, floatVal *float64, hasFloat bool, attrs map[string]string) {
	kv := attrsFromMap(sanitizeAttrs(attrs))
	fp := fingerprint(kv)
	e.mu.Lock()
	defer e.mu.Unlock()
	e.approxBytes += len(name) + len(fp) + 16
	if e.approxBytes > maxPayloadBytes {
		e.dropped++
		e.approxBytes -= len(name) + len(fp) + 16
		return
	}
	m, ok := e.metrics[name]
	if !ok {
		if len(e.metrics) >= maxQueuePoints {
			e.dropped++
			return
		}
		m = &aggregatedMetric{
			name:        name,
			unit:        unit,
			kind:        kind,
			monotonic:   monotonic,
			temporality: aggTemporalityCumulative,
			points:      make(map[string]*numberPoint),
		}
		e.metrics[name] = m
	}
	// Kind mismatch: keep first kind.
	if m.kind != kind {
		return
	}
	pt, ok := m.points[fp]
	if !ok {
		// Deep copy attrs
		cp := append([]keyValue(nil), kv...)
		pt = &numberPoint{attrs: cp}
		m.points[fp] = pt
	}
	if hasFloat {
		if pt.hasFloat {
			*pt.floatValue += *floatVal
		} else if pt.intValue != 0 {
			f := float64(pt.intValue) + *floatVal
			pt.floatValue = &f
			pt.hasFloat = true
			pt.intValue = 0
		} else {
			v := *floatVal
			pt.floatValue = &v
			pt.hasFloat = true
		}
	} else {
		if pt.hasFloat {
			*pt.floatValue += float64(intVal)
		} else {
			pt.intValue += intVal
		}
	}
}

func (e *Exporter) recordHistogram(name, unit string, value float64, attrs map[string]string, bounds []float64) {
	kv := attrsFromMap(sanitizeAttrs(attrs))
	fp := fingerprint(kv)
	e.mu.Lock()
	defer e.mu.Unlock()
	e.approxBytes += len(name) + len(fp) + 16
	if e.approxBytes > maxPayloadBytes {
		e.dropped++
		e.approxBytes -= len(name) + len(fp) + 16
		return
	}
	m, ok := e.metrics[name]
	if !ok {
		if len(e.metrics) >= maxQueuePoints {
			e.dropped++
			return
		}
		m = &aggregatedMetric{
			name:       name,
			unit:       unit,
			kind:       "histogram",
			histPoints: make(map[string]*histPoint),
		}
		// Store bounds on first point's template
		m.histPoints["_bounds"] = &histPoint{bounds: append([]float64(nil), bounds...)}
		e.metrics[name] = m
	}
	if m.kind != "histogram" {
		return
	}
	// Resolve bounds template
	templateBounds := bounds
	if tmp, ok := m.histPoints["_bounds"]; ok && len(tmp.bounds) > 0 {
		templateBounds = tmp.bounds
		if len(bounds) > 0 && len(bounds) != len(templateBounds) {
			// Mismatched bounds: use template
			templateBounds = tmp.bounds
		}
	}
	pt, ok := m.histPoints[fp]
	if !ok {
		cp := append([]keyValue(nil), kv...)
		pt = &histPoint{
			attrs:   cp,
			bounds:  append([]float64(nil), templateBounds...),
			buckets: make([]uint64, len(templateBounds)+1),
		}
		m.histPoints[fp] = pt
	}
	pt.count++
	pt.sum += value
	idx := bucketIndex(value, pt.bounds)
	if idx >= 0 && idx < len(pt.buckets) {
		pt.buckets[idx]++
	}
}

func bucketIndex(v float64, bounds []float64) int {
	for i, b := range bounds {
		if v <= b {
			return i
		}
	}
	return len(bounds)
}

func fingerprint(attrs []keyValue) string {
	if len(attrs) == 0 {
		return ""
	}
	var b strings.Builder
	for i, kv := range attrs {
		if i > 0 {
			b.WriteString("|")
		}
		b.WriteString(kv.Key)
		b.WriteString("=")
		b.WriteString(kv.Value.StringValue)
		if kv.Value.IntValue != "" {
			b.WriteString(kv.Value.IntValue)
		}
		if kv.Value.DoubleValue != nil {
			b.WriteString(strconv.FormatFloat(*kv.Value.DoubleValue, 'g', -1, 64))
		}
	}
	return b.String()
}

func sanitizeAttrs(in map[string]string) map[string]string {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]string, len(in))
	for k, v := range in {
		k = strings.TrimSpace(k)
		v = strings.TrimSpace(v)
		if k == "" {
			continue
		}
		// Truncate bounded labels: keys as-is, values capped 128
		if len([]rune(v)) > 128 {
			v = truncate(v, 128)
		}
		// Lowercase some known enum keys? Keep original case for values but normalize known labels
		out[k] = v
	}
	return out
}

// Export builds an OTLP payload from aggregated points and POSTs it.
func (e *Exporter) Export(ctx context.Context) error {
	if e == nil {
		return nil
	}
	e.mu.Lock()
	payload, err := e.buildPayloadLocked()
	e.mu.Unlock()
	if err != nil {
		return err
	}
	if len(payload) == 0 {
		return nil
	}
	return e.post(ctx, payload)
}

func (e *Exporter) BuildPayloadForTest() ([]byte, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.buildPayloadLocked()
}

func (e *Exporter) buildPayloadLocked() ([]byte, error) {
	if len(e.metrics) == 0 {
		return nil, nil
	}
	nowNano := strconv.FormatInt(time.Now().UnixNano(), 10)
	var metrics []metric
	for _, m := range e.metrics {
		switch m.kind {
		case "sum":
			var dps []numberDataPoint
			for _, pt := range m.points {
				dp := numberDataPoint{
					Attributes:        sortedAttrs(pt.attrs),
					StartTimeUnixNano: e.startNano,
					TimeUnixNano:      nowNano,
				}
				if pt.hasFloat {
					dp.AsDouble = pt.floatValue
				} else {
					dp.AsInt = strconv.FormatInt(pt.intValue, 10)
				}
				dps = append(dps, dp)
			}
			sort.Slice(dps, func(i, j int) bool { return fingerprint(dps[i].Attributes) < fingerprint(dps[j].Attributes) })
			metrics = append(metrics, metric{
				Name: m.name, Unit: m.unit,
				Sum: &sum{
					DataPoints:             dps,
					AggregationTemporality: m.temporality,
					IsMonotonic:            m.monotonic,
				},
			})
		case "gauge":
			var dps []numberDataPoint
			for _, pt := range m.points {
				dp := numberDataPoint{
					Attributes:   sortedAttrs(pt.attrs),
					TimeUnixNano: nowNano,
				}
				if pt.hasFloat {
					dp.AsDouble = pt.floatValue
				} else {
					dp.AsInt = strconv.FormatInt(pt.intValue, 10)
				}
				dps = append(dps, dp)
			}
			sort.Slice(dps, func(i, j int) bool { return fingerprint(dps[i].Attributes) < fingerprint(dps[j].Attributes) })
			metrics = append(metrics, metric{
				Name: m.name, Unit: m.unit,
				Gauge: &gauge{DataPoints: dps},
			})
		case "histogram":
			var dps []histogramDataPoint
			for fp, pt := range m.histPoints {
				if fp == "_bounds" {
					continue
				}
				bucketCounts := make([]string, len(pt.buckets))
				for i, c := range pt.buckets {
					bucketCounts[i] = strconv.FormatUint(c, 10)
				}
				sum := pt.sum
				dps = append(dps, histogramDataPoint{
					Attributes:        sortedAttrs(pt.attrs),
					StartTimeUnixNano: e.startNano,
					TimeUnixNano:      nowNano,
					Count:             strconv.FormatUint(pt.count, 10),
					Sum:               &sum,
					BucketCounts:      bucketCounts,
					ExplicitBounds:    append([]float64(nil), pt.bounds...),
				})
			}
			sort.Slice(dps, func(i, j int) bool { return fingerprint(dps[i].Attributes) < fingerprint(dps[j].Attributes) })
			metrics = append(metrics, metric{
				Name: m.name, Unit: m.unit,
				Histogram: &histogram{
					DataPoints:             dps,
					AggregationTemporality: aggTemporalityCumulative,
				},
			})
		}
	}
	sort.Slice(metrics, func(i, j int) bool { return metrics[i].Name < metrics[j].Name })
	payload, err := buildPayload(e.resourceAttrs, []scopeMetrics{{Scope: scope{Name: "harness", Version: e.buildVersion}, Metrics: metrics}})
	if err != nil {
		return nil, err
	}
	// Validate it is JSON
	var tmp json.RawMessage
	if err := json.Unmarshal(payload, &tmp); err != nil {
		return nil, err
	}
	// Keep metrics for next export (cumulative); do not clear
	return payload, nil
}

func (e *Exporter) post(ctx context.Context, payload []byte) error {
	endpoint := e.endpoint
	headers := e.cfg.Headers
	// Per-request context timeout is already on e.client; honor ctx too.
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(payload))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "harness/"+e.buildVersion)
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := e.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, io.LimitReader(resp.Body, 4<<10))
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return nil
	}
	// Retry once on 429/503 with Retry-After (or immediately if no header but retriable).
	if resp.StatusCode == 429 || resp.StatusCode == 503 {
		delay := retryAfter(resp.Header.Get("Retry-After"))
		if delay <= 5*time.Second {
			select {
			case <-time.After(delay + time.Duration(rand.Intn(200))*time.Millisecond):
			case <-ctx.Done():
				return ctx.Err()
			}
			// Single retry
			req2, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(payload))
			if err != nil {
				return err
			}
			req2.Header.Set("Content-Type", "application/json")
			req2.Header.Set("User-Agent", "harness/"+e.buildVersion)
			for k, v := range headers {
				req2.Header.Set(k, v)
			}
			resp2, err := e.client.Do(req2)
			if err != nil {
				return err
			}
			defer resp2.Body.Close()
			io.Copy(io.Discard, io.LimitReader(resp2.Body, 4<<10))
			if resp2.StatusCode >= 200 && resp2.StatusCode < 300 {
				return nil
			}
			return fmt.Errorf("otel export failed: %s", resp2.Status)
		}
	}
	return fmt.Errorf("otel export failed: %s", resp.Status)
}

func retryAfter(header string) time.Duration {
	header = strings.TrimSpace(header)
	if header == "" {
		return 0
	}
	if secs, err := strconv.Atoi(header); err == nil {
		return time.Duration(secs) * time.Second
	}
	if t, err := http.ParseTime(header); err == nil {
		d := time.Until(t)
		if d < 0 {
			return 0
		}
		return d
	}
	return 0
}

// Flush is a convenience that exports with a 5s timeout in background. Callers
// should use Export directly when they need ctx control.
func (e *Exporter) Flush(ctx context.Context) error {
	if e == nil {
		return nil
	}
	return e.Export(ctx)
}

// Dropped returns the number of points dropped due to queue caps.
func (e *Exporter) Dropped() int {
	if e == nil {
		return 0
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.dropped
}
