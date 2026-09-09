package otel

import (
	"bytes"
	"context"
	cryptorand "crypto/rand"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"io"
	"log/slog"
	"maps"
	"math"
	"math/rand"
	"net"
	"net/http"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"harness/internal/buildinfo"
)

// Exporter collects OTLP metrics and pushes them to the collector. It is safe for
// concurrent Record calls; Export is called on a best-effort background path and
// never blocks prompt completion.
type Exporter struct {
	cfg              Config
	client           *http.Client
	endpoint         string
	buildVersion     string
	resourceAttrs    []keyValue
	startNano        string
	mu               sync.Mutex
	metrics          map[string]*aggregatedMetric
	pointCount       int
	dropped          int
	approxBytes      int
	waitRetry        func(context.Context, time.Duration) error
	retryJitter      func(time.Duration) time.Duration
	exportGate       chan struct{}
	health           Health
	regularPoints    int
	overflowReasons  [overflowReasonCount]uint64
	overflowFamilies int
	lifecycleMu      sync.Mutex
	periodicCancel   context.CancelFunc
	periodicDone     chan struct{}
	shutdownDone     chan struct{}
	shutdownErr      error
	closed           bool
	periodicInterval time.Duration
}

type aggregatedMetric struct {
	name          string
	unit          string
	kind          string // sum, gauge, histogram
	monotonic     bool
	temporality   int                     // 2 = cumulative
	points        map[string]*numberPoint // keyed by attribute fingerprint
	histPoints    map[string]*histPoint
	residentBytes int
	regularPoints int
	histBounds    []float64
}

type numberPoint struct {
	attrs      []keyValue
	intValue   int64
	floatValue *float64
	hasFloat   bool
}

type histPoint struct {
	attrs   []keyValue
	count   uint64
	sum     float64
	bounds  []float64
	buckets []uint64
}

const (
	aggTemporalityCumulative = 2
	// Ordinary series share a 16MiB estimated aggregation budget, not equal
	// family partitions. Reserve maximum metadata/overflow storage for ALL
	// possible families up front; never release unused reservations. Otherwise
	// a rejected in-flight identity could become ordinary before its finish.
	// Self metrics bypass admission. No cumulative series is evicted/reset.
	maxQueuePoints         = 8192
	maxMetricFamilies      = 128
	maxSeriesPerMetric     = 256
	maxResidentBytes       = 16 * 1024 * 1024
	maxMetricResidentBytes = 1024 * 1024
	maxFamilyReserveBytes  = 4 * 1024
	// Portable admission estimates, not exact Go heap sizes. Include retained
	// strings/slices separately; changing key encoding must not hide their cost.
	metricOverheadBytes    = 256
	pointOverheadBytes     = 128
	attributeOverheadBytes = 64
	maxHistogramBounds     = 128
	maxMetricAttributes    = 16
	maxResponseBytes       = 8 * 1024
	selfMetricPrefix       = "harness.otel.export."
	overflowKey            = "otel.metric.overflow"
	overflowFingerprint    = "overflow"
	maxPayloadBytes        = 64 * 1024
	// Reserve half the wire budget for metric points, including fixed self metrics.
	// Validate the complete encoded resource/scope envelope, not raw string sizes.
	maxResourceMetadataBytes = maxPayloadBytes / 2
	maxExportAttempts        = 3
	baseRetryDelay           = 100 * time.Millisecond
	maxRetryDelay            = 5 * time.Second
)

var processInstanceID = func() string {
	var id [16]byte
	// crypto/rand.Read always fills its buffer or terminates on entropy failure.
	_, _ = cryptorand.Read(id[:])
	return hex.EncodeToString(id[:])
}()

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
		cfg.Timeout = DefaultExportTimeout
	}
	// Provider, model, agent, and session identity can change inside a long-lived
	// REPL. Keep the OTLP resource process-stable and put dynamic identity on
	// metric points in Sink.baseAttrs instead of relabeling cumulative data.
	ra := make([]keyValue, 0, len(resourceAttrs)+4)
	for k, v := range resourceAttrs {
		switch k {
		case "service.name", "service.version", "host.name", "service.instance.id":
			continue
		}
		ra = append(ra, stringAttr(k, truncate(v, 128)))
	}
	ra = append(ra,
		stringAttr("service.instance.id", processInstanceID),
		stringAttr("service.name", truncate(cfg.ServiceName, 64)),
		stringAttr("service.version", truncate(build.Version, 64)),
	)
	if cfg.Hostname != "" {
		ra = append(ra, stringAttr("host.name", truncate(cfg.Hostname, 64)))
	}
	ra = sortedAttrs(ra)
	exporter := &Exporter{
		cfg:           cfg,
		client:        &http.Client{},
		endpoint:      normalized,
		buildVersion:  build.Version,
		resourceAttrs: ra,
		startNano:     strconv.FormatInt(time.Now().UnixNano(), 10),
		metrics:       make(map[string]*aggregatedMetric),
		approxBytes:   maxMetricFamilies * maxFamilyReserveBytes,
		exportGate:    make(chan struct{}, 1),
		shutdownDone:  make(chan struct{}),
		waitRetry:     waitForRetry,
		retryJitter:   randomJitter,
	}
	metadata, err := exporter.marshalMetrics(nil)
	if err != nil {
		return nil, err
	}
	if len(metadata) > maxResourceMetadataBytes {
		return nil, &ResourceMetadataError{EncodedBytes: len(metadata), LimitBytes: maxResourceMetadataBytes}
	}
	return exporter, nil
}

// SetPeriodic starts at most one worker. Shutdown cancels and joins it before
// the final export; canceling ctx also stops the worker without closing Export.
func (e *Exporter) SetPeriodic(ctx context.Context, logger *slog.Logger) {
	if e == nil {
		return
	}
	e.lifecycleMu.Lock()
	defer e.lifecycleMu.Unlock()
	if e.closed || e.periodicDone != nil {
		return
	}
	ctx, e.periodicCancel = context.WithCancel(ctx)
	e.periodicDone = make(chan struct{})
	go func() {
		defer close(e.periodicDone)
		interval := e.periodicInterval
		if interval <= 0 {
			interval = PeriodicExportInterval
		}
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		var failure periodicFailureReporter
		var loss periodicLossReporter
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				err := e.Export(ctx)
				if logger != nil && ctx.Err() == nil {
					failure.report(logger, err)
					if err == nil {
						loss.report(logger, time.Now(), e.lossSnapshot())
					}
				}
			}
		}
	}()
}

var ErrExporterShutdown = errors.New("otel exporter is shut down")

// Shutdown stops and joins the periodic worker, then performs one final export.
// The entire operation is bounded by ctx and ShutdownExportTimeout. Concurrent
// callers share the final result; a caller's wait still respects its own budget.
// Record calls should be quiesced by the owner before calling Shutdown.
func (e *Exporter) Shutdown(ctx context.Context) error {
	if e == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, ShutdownExportTimeout)
	defer cancel()
	e.lifecycleMu.Lock()
	if e.closed {
		done := e.shutdownDone
		e.lifecycleMu.Unlock()
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-done:
		}
		e.lifecycleMu.Lock()
		defer e.lifecycleMu.Unlock()
		return e.shutdownErr
	}
	e.closed = true
	if e.periodicCancel != nil {
		e.periodicCancel()
	}
	done := e.periodicDone
	e.lifecycleMu.Unlock()
	var err error
	if done != nil {
		select {
		case <-done:
		case <-ctx.Done():
			err = ctx.Err()
		}
	}
	if err == nil {
		err = e.export(ctx, true)
	}
	e.lifecycleMu.Lock()
	e.shutdownErr = err
	close(e.shutdownDone)
	e.lifecycleMu.Unlock()
	return err
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
	e.mu.Lock()
	defer e.mu.Unlock()
	if hasFloat && (math.IsNaN(*floatVal) || math.IsInf(*floatVal, 0)) {
		e.dropped++
		return
	}
	m := e.metricLocked(name, unit, kind, monotonic, nil)
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
	e.updateNumberPoint(pt, kind, intVal, floatVal, hasFloat)
}

func (e *Exporter) updateNumberPoint(pt *numberPoint, kind string, intVal int64, floatVal *float64, hasFloat bool) {
	if kind == "sum" {
		if hasFloat || pt.hasFloat {
			old, added := float64(pt.intValue), float64(intVal)
			if pt.hasFloat {
				old = *pt.floatValue
			}
			if hasFloat {
				added = *floatVal
			}
			if math.IsInf(old+added, 0) {
				e.dropped++
				return
			}
		} else if (intVal > 0 && pt.intValue > math.MaxInt64-intVal) || (intVal < 0 && pt.intValue < math.MinInt64-intVal) {
			e.dropped++
			return
		}
	}
	if kind == "gauge" {
		pt.intValue = intVal
		pt.floatValue = nil
		pt.hasFloat = hasFloat
		if hasFloat {
			value := *floatVal
			pt.floatValue = &value
			pt.intValue = 0
		}
		return
	}
	if hasFloat {
		if pt.hasFloat {
			*pt.floatValue += *floatVal
		} else if pt.intValue != 0 {
			value := float64(pt.intValue) + *floatVal
			pt.floatValue = &value
			pt.hasFloat = true
			pt.intValue = 0
		} else {
			value := *floatVal
			pt.floatValue = &value
			pt.hasFloat = true
		}
		return
	}
	if pt.hasFloat {
		*pt.floatValue += float64(intVal)
	} else {
		pt.intValue += intVal
	}
}

func metricApproxBytes(name, unit string) int {
	return len(name) + len(unit) + metricOverheadBytes
}

func (m *aggregatedMetric) pointApproxBytes(fp string, attrs []keyValue) int {
	charge := pointOverheadBytes + len(fp) + attributeOverheadBytes*len(attrs)
	for _, a := range attrs {
		charge += len(a.Key) + len(a.Value.StringValue)
	}
	if m.kind == "histogram" {
		charge += 8 * (len(m.histBounds) + 1)
	}
	return charge
}

func (e *Exporter) recordHistogram(name, unit string, value float64, attrs map[string]string, bounds []float64) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if math.IsNaN(value) || math.IsInf(value, 0) || len(bounds) > maxHistogramBounds {
		e.dropped++
		return
	}
	for i, bound := range bounds {
		if math.IsNaN(bound) || math.IsInf(bound, 0) || (i > 0 && bounds[i-1] >= bound) {
			e.dropped++
			return
		}
	}
	m := e.metricLocked(name, unit, "histogram", false, bounds)
	if m == nil {
		return
	}
	kv, fp := e.pointIdentityLocked(m, attrs)
	pt := m.histPoints[fp]
	if pt == nil {
		pt = &histPoint{attrs: kv, bounds: m.histBounds, buckets: make([]uint64, len(m.histBounds)+1)}
		m.histPoints[fp] = pt
		e.chargePointLocked(m, fp)
	}
	if math.IsInf(pt.sum+value, 0) || pt.count == math.MaxUint64 {
		e.dropped++
		return
	}
	updateHistogramPoint(pt, value)
}

func (e *Exporter) metricLocked(name, unit, kind string, monotonic bool, bounds []float64) *aggregatedMetric {
	if name == "" || len(name) > 256 || len(unit) > 64 || strings.HasPrefix(name, selfMetricPrefix) {
		e.dropped++
		return nil
	}
	if m := e.metrics[name]; m != nil {
		if m.kind != kind || m.unit != unit {
			e.dropped++
			return nil
		}
		if kind == "histogram" {
			if len(bounds) != len(m.histBounds) {
				e.dropped++
				return nil
			}
			for i := range bounds {
				if bounds[i] != m.histBounds[i] {
					e.dropped++
					return nil
				}
			}
		}
		return m
	}
	if len(e.metrics) >= maxMetricFamilies {
		e.dropped++
		return nil
	}
	// Own exactly the admitted strings, not a short substring of caller storage.
	name, unit = strings.Clone(name), strings.Clone(unit)
	m := &aggregatedMetric{name: name, unit: unit, kind: kind, monotonic: monotonic,
		temporality: aggTemporalityCumulative, points: make(map[string]*numberPoint),
		histPoints: make(map[string]*histPoint), histBounds: append([]float64(nil), bounds...)}
	e.metrics[name] = m
	// Metadata/bounds and overflow are already covered by the fixed global
	// reservations; include them in this family's guardrail without recharging.
	m.residentBytes = metricApproxBytes(name, unit) + 8*len(bounds) + m.pointApproxBytes(overflowFingerprint, overflowAttrs())
	return m
}

// An overflow point loses labels, not measurements: sums and histograms merge;
// gauges retain the last overflow sample. Its reserved storage is independent
// of ordinary budgets so a hot family cannot starve all later families.
func (e *Exporter) pointIdentityLocked(m *aggregatedMetric, attrs map[string]string) ([]keyValue, string) {
	kv, valid := sanitizeAttrs(attrs)
	reason := overflowAttributes
	if valid {
		fp := fingerprint(kv)
		if m.points[fp] != nil || m.histPoints[fp] != nil {
			return kv, fp
		}
		charge := m.pointApproxBytes(fp, kv)
		switch {
		case m.regularPoints >= maxSeriesPerMetric:
			reason = overflowFamilySeries
		case e.regularPoints >= maxQueuePoints:
			reason = overflowGlobalSeries
		case m.residentBytes+charge > maxMetricResidentBytes:
			reason = overflowFamilyBytes
		case e.approxBytes+charge > maxResidentBytes:
			reason = overflowGlobalBytes
		default:
			for i := range kv {
				kv[i].Key = strings.Clone(kv[i].Key)
				kv[i].Value.StringValue = strings.Clone(kv[i].Value.StringValue)
			}
			return kv, fp
		}
	}
	e.health.Overflow++
	e.overflowReasons[reason]++
	if pt := m.points[overflowFingerprint]; pt != nil {
		return pt.attrs, overflowFingerprint
	}
	if pt := m.histPoints[overflowFingerprint]; pt != nil {
		return pt.attrs, overflowFingerprint
	}
	e.overflowFamilies++
	return overflowAttrs(), overflowFingerprint
}

func overflowAttrs() []keyValue {
	yes := true
	return []keyValue{{Key: overflowKey, Value: anyValue{BoolValue: &yes}}}
}

func (e *Exporter) chargePointLocked(m *aggregatedMetric, fp string) {
	e.pointCount++
	if fp == overflowFingerprint {
		return // Already covered by permanent per-family reservations.
	}
	var attrs []keyValue
	if m.kind == "histogram" {
		attrs = m.histPoints[fp].attrs
	} else {
		attrs = m.points[fp].attrs
	}
	charge := m.pointApproxBytes(fp, attrs)
	e.approxBytes += charge
	e.regularPoints++
	m.regularPoints++
	m.residentBytes += charge
}

func updateHistogramPoint(pt *histPoint, value float64) {
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

// fingerprint encodes canonical ordinary STRING attributes in key order. Length
// framing handles arbitrary delimiters/NULs; the prefix separates nonempty
// ordinary identities from the overflow sentinel. Empty attributes remain "".
func fingerprint(attrs []keyValue) string {
	if len(attrs) == 0 {
		return ""
	}
	size := 1
	for _, a := range attrs {
		size += 8 + len(a.Key) + len(a.Value.StringValue)
	}
	var out strings.Builder
	out.Grow(size)
	out.WriteByte(0)
	var length [4]byte
	for _, a := range attrs {
		binary.BigEndian.PutUint32(length[:], uint32(len(a.Key)))
		out.Write(length[:])
		out.WriteString(a.Key)
		binary.BigEndian.PutUint32(length[:], uint32(len(a.Value.StringValue)))
		out.Write(length[:])
		out.WriteString(a.Value.StringValue)
	}
	return out.String()
}

func sanitizeAttrs(in map[string]string) ([]keyValue, bool) {
	if len(in) > maxMetricAttributes {
		return nil, false
	}
	out := make([]keyValue, 0, len(in))
	for k, v := range in {
		if len(k) > 64 {
			return nil, false
		}
		k = strings.TrimSpace(k)
		if !utf8.ValidString(k) {
			k = string([]rune(k)) // Match JSON's repair of invalid UTF-8.
		}
		if len(k) > 64 || k == overflowKey {
			return nil, false
		}
		if k == "" {
			continue
		}
		v = truncate(strings.TrimSpace(v), 128)
		if !utf8.ValidString(v) {
			v = string([]rune(v))
		}
		out = append(out, stringAttr(k, v))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	for i := 1; i < len(out); i++ {
		if out[i-1].Key == out[i].Key {
			return nil, false // Never choose a normalized duplicate by map order.
		}
	}
	return out, true
}

// Export serializes the entire cumulative snapshot/retry sequence. Waiting for
// another exporter consumes the same context budget as the network operation.
func (e *Exporter) Export(ctx context.Context) error { return e.export(ctx, false) }

func (e *Exporter) export(ctx context.Context, final bool) (result error) {
	if e == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, e.cfg.Timeout)
	defer cancel()
	select {
	case e.exportGate <- struct{}{}:
	case <-ctx.Done():
		return ctx.Err()
	}
	defer func() { <-e.exportGate }()
	if err := ctx.Err(); err != nil {
		return err
	}
	e.lifecycleMu.Lock()
	closed := e.closed
	e.lifecycleMu.Unlock()
	if closed && !final {
		return ErrExporterShutdown
	}
	started := time.Now()
	defer func() {
		e.mu.Lock()
		e.health.Duration += time.Since(started)
		if result == nil {
			e.health.LastSuccess = time.Now()
		}
		e.mu.Unlock()
	}()
	e.mu.Lock()
	metrics := e.snapshotMetricsLocked()
	e.mu.Unlock()
	payloads, skipped, err := e.buildPayloads(ctx, metrics)
	e.mu.Lock()
	e.dropped += skipped
	e.mu.Unlock()
	if err != nil {
		return err
	}
	// Bound the final diagnostic independently of the number of chunks. Preserve
	// total partial rejections, one representative failure, and local wire loss.
	var partial *PartialSuccessError
	var firstErr error
	for _, payload := range payloads {
		if err := e.post(ctx, payload); err != nil {
			var p *PartialSuccessError
			if errors.As(err, &p) {
				if partial == nil {
					partial = &PartialSuccessError{}
				}
				partial.RejectedDataPoints = int64(addRejected(uint64(partial.RejectedDataPoints), uint64(p.RejectedDataPoints)))
				partial.HasErrorMessage = partial.HasErrorMessage || p.HasErrorMessage
			} else if firstErr == nil {
				firstErr = err
			}
			if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				// Context identity matters even when another chunk failed earlier.
				firstErr = err
				break
			}
		}
	}
	var errs []error
	if skipped > 0 {
		errs = append(errs, &PayloadSizeError{DroppedDataPoints: skipped})
	}
	if partial != nil {
		errs = append(errs, partial)
	}
	if firstErr != nil {
		errs = append(errs, firstErr)
	}
	return errors.Join(errs...)
}

func (e *Exporter) BuildPayloadForTest() ([]byte, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.buildPayloadLocked()
}

func (e *Exporter) MetricsForTest() map[string]*aggregatedMetric {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := make(map[string]*aggregatedMetric, len(e.metrics))
	for k, v := range e.metrics {
		out[k] = v
	}
	return out
}

// Copy mutable values, but share admitted immutable attributes and bounds.
// Sorting retained keys avoids rebuilding serialized identities in comparisons.
func (e *Exporter) snapshotMetricsLocked() []metric {
	nowNano := strconv.FormatInt(time.Now().UnixNano(), 10)
	var metrics []metric
	for _, m := range e.metrics {
		switch m.kind {
		case "sum":
			var dps []numberDataPoint
			for _, fp := range slices.Sorted(maps.Keys(m.points)) {
				pt := m.points[fp]
				dp := numberDataPoint{
					Attributes:        pt.attrs,
					StartTimeUnixNano: e.startNano,
					TimeUnixNano:      nowNano,
				}
				if pt.hasFloat {
					value := *pt.floatValue
					dp.AsDouble = &value
				} else {
					dp.AsInt = strconv.FormatInt(pt.intValue, 10)
				}
				dps = append(dps, dp)
			}
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
			for _, fp := range slices.Sorted(maps.Keys(m.points)) {
				pt := m.points[fp]
				dp := numberDataPoint{
					Attributes:   pt.attrs,
					TimeUnixNano: nowNano,
				}
				if pt.hasFloat {
					value := *pt.floatValue
					dp.AsDouble = &value
				} else {
					dp.AsInt = strconv.FormatInt(pt.intValue, 10)
				}
				dps = append(dps, dp)
			}
			metrics = append(metrics, metric{
				Name: m.name, Unit: m.unit,
				Gauge: &gauge{DataPoints: dps},
			})
		case "histogram":
			var dps []histogramDataPoint
			for _, fp := range slices.Sorted(maps.Keys(m.histPoints)) {
				pt := m.histPoints[fp]
				bucketCounts := make([]string, len(pt.buckets))
				for i, c := range pt.buckets {
					bucketCounts[i] = strconv.FormatUint(c, 10)
				}
				sum := pt.sum
				dps = append(dps, histogramDataPoint{
					Attributes:        pt.attrs,
					StartTimeUnixNano: e.startNano,
					TimeUnixNano:      nowNano,
					Count:             strconv.FormatUint(pt.count, 10),
					Sum:               &sum,
					BucketCounts:      bucketCounts,
					ExplicitBounds:    pt.bounds,
				})
			}
			metrics = append(metrics, metric{
				Name: m.name, Unit: m.unit,
				Histogram: &histogram{
					DataPoints:             dps,
					AggregationTemporality: aggTemporalityCumulative,
				},
			})
		}
	}
	metrics = append(metrics, e.healthMetricsLocked(nowNano)...)
	sort.Slice(metrics, func(i, j int) bool { return metrics[i].Name < metrics[j].Name })
	return metrics
}

func (e *Exporter) marshalMetrics(metrics []metric) ([]byte, error) {
	payload, err := buildPayload(e.resourceAttrs, []scopeMetrics{{Scope: scope{Name: "harness", Version: e.buildVersion}, Metrics: metrics}})
	if err != nil {
		return nil, &ExportError{Operation: "encode payload"}
	}
	return payload, nil
}

func (e *Exporter) buildPayloadLocked() ([]byte, error) {
	return e.marshalMetrics(e.snapshotMetricsLocked())
}

// Chunk complete data points, preserving all families and cumulative start times.
// Wire limits are measured on the actual JSON, not resident-size estimates.
func (e *Exporter) buildPayloadsLocked() ([][]byte, int, error) {
	return e.buildPayloads(context.Background(), e.snapshotMetricsLocked())
}

func (e *Exporter) buildPayloads(ctx context.Context, metrics []metric) ([][]byte, int, error) {
	var payloads [][]byte
	var current []metric
	var encoded []byte
	skipped := 0
	for _, m := range metrics {
		for offset := 0; offset < metricPointCount(m); {
			remaining := metricPointCount(m) - offset
			best := 0
			var bestPayload []byte
			for low, high := 1, remaining; low <= high; {
				if err := ctx.Err(); err != nil {
					return nil, skipped, err
				}
				n := low + (high-low)/2
				candidate := append(current, metricPart(m, offset, offset+n))
				data, err := e.marshalMetrics(candidate)
				if err != nil {
					return nil, skipped, err
				}
				if len(data) <= maxPayloadBytes {
					best, bestPayload, low = n, data, n+1
				} else {
					high = n - 1
				}
			}
			if best > 0 {
				current = append(current, metricPart(m, offset, offset+best))
				encoded = bestPayload
				offset += best
			} else if len(current) == 0 {
				skipped++ // Keep resident cumulative data; count this omitted wire point.
				offset++
			}
			if best < remaining && len(current) > 0 {
				payloads = append(payloads, encoded)
				current, encoded = nil, nil
			}
		}
	}
	if len(current) > 0 {
		payloads = append(payloads, encoded)
	}
	return payloads, skipped, nil
}

func (e *Exporter) post(ctx context.Context, payload []byte) error {
	var lastErr error
	for attempt := 0; attempt < maxExportAttempts; attempt++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, e.endpoint, bytes.NewReader(payload))
		if err != nil {
			return &ExportError{Operation: "create request", cause: err}
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("User-Agent", "harness/"+e.buildVersion)
		for k, v := range e.cfg.Headers {
			req.Header.Set(k, v)
		}
		e.mu.Lock()
		e.health.Attempts++
		e.health.PayloadBytes += uint64(len(payload))
		if attempt > 0 {
			e.health.Retries++
		}
		e.mu.Unlock()
		resp, err := e.client.Do(req)
		var retryHeader string
		retryable := false
		if err == nil {
			retryHeader = resp.Header.Get("Retry-After")
			if resp.StatusCode >= 200 && resp.StatusCode < 300 {
				lastErr = parseExportResponse(resp.Body)
				// A truncated response is a transport failure, not confirmed acceptance.
				// Parse/partial-success errors have no transient cause and remain final.
				retryable = isTransientTransportError(lastErr)
			} else {
				_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxResponseBytes))
				lastErr = &ExportError{Operation: "HTTP response", StatusCode: resp.StatusCode}
				retryable = isRetryableStatus(resp.StatusCode)
			}
			_ = resp.Body.Close()
		} else {
			lastErr = &ExportError{Operation: "transport", cause: err}
			retryable = isTransientTransportError(err)
		}
		if lastErr == nil {
			return nil
		}
		e.mu.Lock()
		e.health.Failures++
		var partial *PartialSuccessError
		if errors.As(lastErr, &partial) {
			e.health.Rejected = addRejected(e.health.Rejected, uint64(partial.RejectedDataPoints))
		}
		e.mu.Unlock()
		if !retryable || attempt == maxExportAttempts-1 {
			return lastErr
		}
		delay := retryDelay(attempt, retryAfter(retryHeader), e.retryJitter)
		// Never retry ahead of the collector's requested delay just to fit a budget.
		if deadline, ok := ctx.Deadline(); ok && time.Until(deadline) <= delay {
			return errors.Join(lastErr, context.DeadlineExceeded)
		}
		wait := e.waitRetry
		if wait == nil {
			wait = waitForRetry
		}
		if err := wait(ctx, delay); err != nil {
			return err
		}
	}
	return lastErr
}

// OTLP integer sums are signed 64-bit. Saturate untrusted rejection counters
// rather than wrap a cumulative health series or create an invalid wire value.
func addRejected(current, added uint64) uint64 {
	if added > math.MaxInt64-current {
		return math.MaxInt64
	}
	return current + added
}

func isRetryableStatus(status int) bool {
	switch status {
	case http.StatusTooManyRequests, http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout:
		return true
	default:
		return false
	}
}

func isTransientTransportError(err error) bool {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	var netErr net.Error
	return errors.As(err, &netErr) || errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF)
}

func retryDelay(attempt int, retryAfterDelay time.Duration, jitter func(time.Duration) time.Duration) time.Duration {
	delay := baseRetryDelay << attempt
	if delay > maxRetryDelay {
		delay = maxRetryDelay
	}
	if jitter != nil {
		delay += jitter(delay / 2)
		if delay > maxRetryDelay {
			delay = maxRetryDelay
		}
	}
	// Retry-After is a server minimum, not exponential backoff subject to our cap.
	if retryAfterDelay > delay {
		delay = retryAfterDelay
	}
	return delay
}

func randomJitter(max time.Duration) time.Duration {
	if max <= 0 {
		return 0
	}
	return time.Duration(rand.Int63n(int64(max) + 1))
}

func waitForRetry(ctx context.Context, delay time.Duration) error {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-timer.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func retryAfter(header string) time.Duration {
	header = strings.TrimSpace(header)
	if header == "" {
		return 0
	}
	allDigits := true
	for _, r := range header {
		if r < '0' || r > '9' {
			allDigits = false
			break
		}
	}
	if allDigits {
		secs, err := strconv.ParseUint(header, 10, 64)
		if err != nil || secs > uint64(math.MaxInt64/int64(time.Second)) {
			return time.Duration(math.MaxInt64)
		}
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

// Flush is an alias for Export; use Shutdown to stop periodic work and flush.
func (e *Exporter) Flush(ctx context.Context) error {
	if e == nil {
		return nil
	}
	return e.Export(ctx)
}

// Dropped returns discarded measurements plus oversized wire-point omissions.
// Overflow measurements retain their values and are counted separately.
func (e *Exporter) Dropped() int {
	if e == nil {
		return 0
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.dropped
}
