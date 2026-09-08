package otel

import (
	"encoding/json"
	"errors"
	"math"
	"strconv"
	"strings"
	"testing"

	"harness/internal/buildinfo"
)

func TestExporterWireAnyValueExclusiveArm(t *testing.T) {
	yes, no, zero, fraction := true, false, 0.0, 1.25
	for _, tc := range []struct {
		name  string
		value anyValue
		want  string
	}{
		{"empty-string", anyValue{StringValue: ""}, `{"stringValue":""}`},
		{"string", anyValue{StringValue: "value"}, `{"stringValue":"value"}`},
		{"bool-overflow", anyValue{BoolValue: &yes}, `{"boolValue":true}`},
		{"false-bool", anyValue{BoolValue: &no}, `{"boolValue":false}`},
		{"zero-integer", anyValue{IntValue: "0"}, `{"intValue":"0"}`},
		{"integer", anyValue{IntValue: "12"}, `{"intValue":"12"}`},
		{"zero-double", anyValue{DoubleValue: &zero}, `{"doubleValue":0}`},
		{"double", anyValue{DoubleValue: &fraction}, `{"doubleValue":1.25}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			data, err := json.Marshal(tc.value)
			if err != nil {
				t.Fatal(err)
			}
			if string(data) != tc.want {
				t.Fatalf("wire=%s want=%s", data, tc.want)
			}
			var arms map[string]json.RawMessage
			if err := json.Unmarshal(data, &arms); err != nil {
				t.Fatal(err)
			}
			if len(arms) != 1 {
				t.Fatalf("wire has %d arms: %s", len(arms), data)
			}
			var decoded anyValue
			if err := json.Unmarshal(data, &decoded); err != nil {
				t.Fatal(err)
			}
			roundtrip, err := json.Marshal(decoded)
			if err != nil || string(roundtrip) != tc.want {
				t.Fatalf("roundtrip=%s err=%v", roundtrip, err)
			}
		})
	}
	for _, value := range []anyValue{
		{StringValue: "SECRET", BoolValue: &yes},
		{StringValue: "SECRET", IntValue: "1"},
		{BoolValue: &yes, DoubleValue: &zero},
		{IntValue: "1", DoubleValue: &zero},
	} {
		_, err := json.Marshal(value)
		if !errors.Is(err, errAnyValueMultipleValues) || strings.Contains(err.Error(), "SECRET") {
			t.Fatalf("conflicting-arm error=%v", err)
		}
	}
}

func TestExporterWireEmptyAttributeAndBooleanOverflow(t *testing.T) {
	e, err := NewExporter(Config{Endpoint: "http://collector.invalid"}, buildinfo.Metadata{}, "", "", "", "", map[string]string{"empty.resource": ""})
	if err != nil {
		t.Fatal(err)
	}
	e.RecordSum("counter", "1", 1, map[string]string{"empty": ""})
	for i := 0; i < maxSeriesPerMetric; i++ {
		e.RecordSum("counter", "1", 1, map[string]string{"id": strconv.Itoa(i)})
	}
	data, err := e.BuildPayloadForTest()
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		`{"key":"empty.resource","value":{"stringValue":""}}`,
		`{"key":"empty","value":{"stringValue":""}}`,
		`{"key":"otel.metric.overflow","value":{"boolValue":true}}`,
	} {
		if !strings.Contains(string(data), want) {
			t.Fatalf("payload missing exact attribute %s", want)
		}
	}
	if strings.Contains(string(data), `"value":{}`) {
		t.Fatal("payload contains an unset attribute value arm")
	}
}

func TestExporterWireRecordAndMarshalFailuresStaySafe(t *testing.T) {
	e, err := NewExporter(Config{Endpoint: "http://collector.invalid"}, buildinfo.Metadata{}, "", "", "", "", map[string]string{"SECRET-resource": "SECRET-value"})
	if err != nil {
		t.Fatal(err)
	}
	e.RecordSumFloat("SECRET-invalid-number", "1", math.NaN(), nil)
	e.RecordHistogram("SECRET-invalid-histogram", "1", 1, nil, []float64{math.Inf(1)})
	if e.Health().Dropped != 2 || len(e.metrics) != 0 {
		t.Fatalf("invalid records were retained: health=%+v", e.Health())
	}
	if _, err := e.BuildPayloadForTest(); err != nil {
		t.Fatalf("invalid records poisoned aggregation: %v", err)
	}

	nan, yes := math.NaN(), true
	for _, point := range []numberDataPoint{
		{AsDouble: &nan},
		{AsInt: "1", Attributes: []keyValue{{Key: "SECRET-key", Value: anyValue{StringValue: "SECRET-string", BoolValue: &yes}}}},
	} {
		payload, err := e.marshalMetrics([]metric{{Name: "SECRET-metric", Gauge: &gauge{DataPoints: []numberDataPoint{point}}}})
		var exportErr *ExportError
		if len(payload) != 0 || !errors.As(err, &exportErr) || exportErr.Operation != "encode payload" {
			t.Fatalf("payload=%d error=%v", len(payload), err)
		}
		if err.Error() != "otel export encode payload failed" || errors.Unwrap(err) != nil {
			t.Fatalf("unsafe marshal diagnostic: %v", err)
		}
	}
}
