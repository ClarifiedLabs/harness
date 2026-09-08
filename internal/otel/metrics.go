package otel

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"sort"
	"strconv"
	"time"
)

// PartialSuccessError reports collector partial acceptance, which must not be
// blindly retried. Server-provided errorMessage content is intentionally never
// retained or exposed; HasErrorMessage only indicates that a diagnostic existed.
type PartialSuccessError struct {
	RejectedDataPoints int64
	HasErrorMessage    bool
}

func (e *PartialSuccessError) Error() string {
	return fmt.Sprintf("otel partial acceptance: rejected_data_points=%d collector_message_present=%t", e.RejectedDataPoints, e.HasErrorMessage)
}

// ExportError is a bounded diagnostic: no endpoint, header, body, or transport
// error string appears in Error. Unwrap preserves errors.Is/As for callers.
type ExportError struct {
	Operation  string
	StatusCode int
	cause      error
}

func (e *ExportError) Error() string {
	if e.StatusCode != 0 {
		return fmt.Sprintf("otel export %s failed: HTTP %d", e.Operation, e.StatusCode)
	}
	reason := ""
	var network net.Error
	switch {
	case errors.Is(e.cause, context.Canceled):
		reason = ": canceled"
	case errors.Is(e.cause, context.DeadlineExceeded):
		reason = ": deadline exceeded"
	case errors.As(e.cause, &network) && network.Timeout():
		reason = ": network timeout"
	case errors.As(e.cause, &network):
		reason = ": network error"
	}
	return "otel export " + e.Operation + " failed" + reason
}
func (e *ExportError) Unwrap() error { return e.cause }

type PayloadSizeError struct{ DroppedDataPoints int }

func (e *PayloadSizeError) Error() string {
	return fmt.Sprintf("otel payload limit: omitted_data_points=%d", e.DroppedDataPoints)
}

func parseExportResponse(body io.Reader) error {
	data, err := io.ReadAll(io.LimitReader(body, maxResponseBytes+1))
	if err != nil {
		return &ExportError{Operation: "read response", cause: err}
	}
	if len(data) > maxResponseBytes {
		return &ExportError{Operation: "response size limit"}
	}
	data = bytes.TrimSpace(data)
	if len(data) == 0 {
		return nil
	} // Legacy collectors return an empty 200/204.
	var response struct {
		PartialSuccess *struct {
			RejectedDataPoints json.RawMessage `json:"rejectedDataPoints"`
			ErrorMessage       string          `json:"errorMessage"`
		} `json:"partialSuccess"`
	}
	if data[0] != '{' || json.Unmarshal(data, &response) != nil {
		return &ExportError{Operation: "decode response"}
	}
	if response.PartialSuccess == nil {
		return nil
	}
	p := response.PartialSuccess
	var rejected int64
	if raw := p.RejectedDataPoints; len(raw) > 0 {
		value := string(raw)
		if raw[0] == '"' {
			if json.Unmarshal(raw, &value) != nil {
				return &ExportError{Operation: "decode rejection count"}
			}
		}
		rejected, err = strconv.ParseInt(value, 10, 64)
		if err != nil || rejected < 0 {
			return &ExportError{Operation: "decode rejection count"}
		}
	}
	if rejected == 0 && p.ErrorMessage == "" {
		return nil
	}
	return &PartialSuccessError{RejectedDataPoints: rejected, HasErrorMessage: p.ErrorMessage != ""}
}

// Health is a process-local cumulative snapshot. Attempts/Failures count HTTP
// attempts (including partial acceptance); Retries count extra attempts, and
// PayloadBytes counts attempted wire bytes including retries. Duration includes
// snapshot/chunk/network work, not time queued for serialization. LastSuccess
// advances only after a complete export. Dropped counts discarded measurements
// and oversized wire-point omissions; Overflow counts measurements merged into
// reserved overflow series. ActiveSeries excludes the fixed self metrics.
type Health struct {
	Attempts     uint64
	Failures     uint64
	Retries      uint64
	Duration     time.Duration
	PayloadBytes uint64
	Rejected     uint64
	Dropped      uint64
	Overflow     uint64
	ActiveSeries int
	LastSuccess  time.Time
}

func (e *Exporter) Health() Health {
	if e == nil {
		return Health{}
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.healthLocked()
}
func (e *Exporter) healthLocked() Health {
	h := e.health
	h.Dropped = uint64(e.dropped)
	h.ActiveSeries = e.pointCount
	return h
}

// Direct snapshot construction avoids recursively recording exporter activity.
// These fixed, unlabeled series do not consume ordinary cardinality budgets.
func (e *Exporter) healthMetricsLocked(now string) []metric {
	h := e.healthLocked()
	counters := []struct {
		name, unit string
		value      uint64
	}{
		{"attempts", "{attempt}", h.Attempts}, {"failures", "{attempt}", h.Failures},
		{"retries", "{retry}", h.Retries}, {"payload_bytes", "By", h.PayloadBytes},
		{"rejected_data_points", "{point}", h.Rejected}, {"dropped_data_points", "{point}", h.Dropped},
		{"overflow_measurements", "{measurement}", h.Overflow},
	}
	out := make([]metric, 0, len(counters)+3)
	for _, c := range counters {
		out = append(out, metric{Name: selfMetricPrefix + c.name, Unit: c.unit, Sum: &sum{
			AggregationTemporality: aggTemporalityCumulative, IsMonotonic: true,
			DataPoints: []numberDataPoint{{StartTimeUnixNano: e.startNano, TimeUnixNano: now, AsInt: strconv.FormatUint(c.value, 10)}},
		}})
	}
	duration := h.Duration.Seconds()
	out = append(out, metric{Name: selfMetricPrefix + "duration", Unit: "s", Sum: &sum{
		AggregationTemporality: aggTemporalityCumulative, IsMonotonic: true,
		DataPoints: []numberDataPoint{{StartTimeUnixNano: e.startNano, TimeUnixNano: now, AsDouble: &duration}},
	}})
	out = append(out, metric{Name: selfMetricPrefix + "active_series", Unit: "{series}", Gauge: &gauge{
		DataPoints: []numberDataPoint{{TimeUnixNano: now, AsInt: strconv.Itoa(h.ActiveSeries)}},
	}})
	last := int64(0)
	if !h.LastSuccess.IsZero() {
		last = h.LastSuccess.Unix()
	}
	out = append(out, metric{Name: selfMetricPrefix + "last_success", Unit: "s", Gauge: &gauge{
		DataPoints: []numberDataPoint{{TimeUnixNano: now, AsInt: strconv.FormatInt(last, 10)}},
	}})
	return out
}

func metricPointCount(m metric) int {
	if m.Sum != nil {
		return len(m.Sum.DataPoints)
	}
	if m.Gauge != nil {
		return len(m.Gauge.DataPoints)
	}
	return len(m.Histogram.DataPoints)
}
func metricPart(m metric, start, end int) metric {
	if m.Sum != nil {
		value := *m.Sum
		value.DataPoints = value.DataPoints[start:end]
		m.Sum = &value
	}
	if m.Gauge != nil {
		value := *m.Gauge
		value.DataPoints = value.DataPoints[start:end]
		m.Gauge = &value
	}
	if m.Histogram != nil {
		value := *m.Histogram
		value.DataPoints = value.DataPoints[start:end]
		m.Histogram = &value
	}
	return m
}

// OTLP JSON structures for metrics (subset used by harness)

type exportMetricsServiceRequest struct {
	ResourceMetrics []resourceMetrics `json:"resourceMetrics"`
}

type resourceMetrics struct {
	Resource     resource       `json:"resource"`
	ScopeMetrics []scopeMetrics `json:"scopeMetrics"`
}

type resource struct {
	Attributes []keyValue `json:"attributes"`
}

type scopeMetrics struct {
	Scope   scope    `json:"scope"`
	Metrics []metric `json:"metrics"`
}

type scope struct {
	Name    string `json:"name"`
	Version string `json:"version,omitempty"`
}

type metric struct {
	Name        string     `json:"name"`
	Description string     `json:"description,omitempty"`
	Unit        string     `json:"unit,omitempty"`
	Sum         *sum       `json:"sum,omitempty"`
	Gauge       *gauge     `json:"gauge,omitempty"`
	Histogram   *histogram `json:"histogram,omitempty"`
}

type sum struct {
	DataPoints             []numberDataPoint `json:"dataPoints"`
	AggregationTemporality int               `json:"aggregationTemporality"`
	IsMonotonic            bool              `json:"isMonotonic"`
}

type gauge struct {
	DataPoints []numberDataPoint `json:"dataPoints"`
}

type histogram struct {
	DataPoints             []histogramDataPoint `json:"dataPoints"`
	AggregationTemporality int                  `json:"aggregationTemporality"`
}

type numberDataPoint struct {
	Attributes        []keyValue `json:"attributes,omitempty"`
	StartTimeUnixNano string     `json:"startTimeUnixNano,omitempty"`
	TimeUnixNano      string     `json:"timeUnixNano"`
	AsInt             string     `json:"asInt,omitempty"`
	AsDouble          *float64   `json:"asDouble,omitempty"`
}

type histogramDataPoint struct {
	Attributes        []keyValue `json:"attributes,omitempty"`
	StartTimeUnixNano string     `json:"startTimeUnixNano,omitempty"`
	TimeUnixNano      string     `json:"timeUnixNano"`
	Count             string     `json:"count"`
	Sum               *float64   `json:"sum,omitempty"`
	BucketCounts      []string   `json:"bucketCounts"`
	ExplicitBounds    []float64  `json:"explicitBounds"`
}

type keyValue struct {
	Key   string   `json:"key"`
	Value anyValue `json:"value"`
}

type anyValue struct {
	StringValue string   `json:"stringValue,omitempty"`
	IntValue    string   `json:"intValue,omitempty"`
	DoubleValue *float64 `json:"doubleValue,omitempty"`
	BoolValue   *bool    `json:"boolValue,omitempty"`
}

var errAnyValueMultipleValues = errors.New("otel AnyValue has multiple value fields")

// MarshalJSON preserves OTLP's exclusive value arm, including an empty string.
// StringValue remains a string for callers; its zero value selects the string
// arm only when no explicit int/double/bool arm is present.
func (v anyValue) MarshalJSON() ([]byte, error) {
	wire := struct {
		StringValue *string  `json:"stringValue,omitempty"`
		IntValue    *string  `json:"intValue,omitempty"`
		DoubleValue *float64 `json:"doubleValue,omitempty"`
		BoolValue   *bool    `json:"boolValue,omitempty"`
	}{DoubleValue: v.DoubleValue, BoolValue: v.BoolValue}
	arms := 0
	if v.IntValue != "" {
		wire.IntValue = &v.IntValue
		arms++
	}
	if v.DoubleValue != nil {
		arms++
	}
	if v.BoolValue != nil {
		arms++
	}
	if v.StringValue != "" || arms == 0 {
		wire.StringValue = &v.StringValue
		arms++
	}
	if arms != 1 {
		return nil, errAnyValueMultipleValues
	}
	return json.Marshal(wire)
}

func stringAttr(key, value string) keyValue {
	return keyValue{Key: key, Value: anyValue{StringValue: value}}
}

func sortedAttrs(attrs []keyValue) []keyValue {
	if len(attrs) == 0 {
		return attrs
	}
	out := append([]keyValue(nil), attrs...)
	sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	return out
}

func buildPayload(resourceAttrs []keyValue, scopeMetrics []scopeMetrics) ([]byte, error) {
	req := exportMetricsServiceRequest{
		ResourceMetrics: []resourceMetrics{
			{
				Resource:     resource{Attributes: sortedAttrs(resourceAttrs)},
				ScopeMetrics: scopeMetrics,
			},
		},
	}
	return json.Marshal(req)
}

// Helpers for metric construction keep truncation and sorting consistent.

func attrsFromMap(m map[string]string) []keyValue {
	if len(m) == 0 {
		return nil
	}
	out := make([]keyValue, 0, len(m))
	for k, v := range m {
		out = append(out, stringAttr(k, v))
	}
	return sortedAttrs(out)
}
