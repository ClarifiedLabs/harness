package otel

import (
	"encoding/json"
	"sort"
)

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
