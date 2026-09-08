package otel

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"unsafe"
)

func TestExporterCompactKeysPreserveIdentity(t *testing.T) {
	e := newTestExporter(t, "http://collector.invalid")
	identities := []map[string]string{
		nil, {"a": ""}, {"a": "b", "c": "d"}, {"a": "b;c=d"},
		{"a\x00b": "c"}, {"a": "b\x00c"}, {"a": "b", "c": ""},
		{"a": "b\x00c\x00"}, {"😀": strings.Repeat("界", 128)},
		{"a": "bc"}, {"ab": "c"},
	}
	for _, attrs := range identities {
		e.RecordSum("counter", "1", 1, attrs)
	}
	// Map order, whitespace, and JSON-equivalent UTF-8 repairs must not create
	// another exported series with the same attribute set.
	e.RecordSum("counter", "1", 1, map[string]string{" c ": " d ", " a ": " b "})
	e.RecordSum("counter", "1", 1, map[string]string{"invalid": "\xff\xfe"})
	e.RecordSum("counter", "1", 1, map[string]string{"invalid": "\ufffd\ufffd"})
	e.RecordSum("counter", "1", 1, map[string]string{"\xff": "value"})
	e.RecordSum("counter", "1", 1, map[string]string{"\ufffd": "value"})
	m := e.metrics["counter"]
	if len(m.points) != len(identities)+2 || e.Health().Overflow != 0 {
		t.Fatalf("points=%d health=%+v", len(m.points), e.Health())
	}
	requireNumber(t, e, "counter", 2, map[string]string{"a": "b", "c": "d"})
	requireNumber(t, e, "counter", 2, map[string]string{"invalid": "\ufffd\ufffd"})
	for fp, p := range m.points {
		if fp == overflowFingerprint || (fp != "" && fp[0] != 0) {
			t.Fatalf("ordinary key is not separated from overflow: %q", fp)
		}
		encoded, err := json.Marshal(p.attrs)
		if err != nil || len(fp) >= len(encoded) {
			t.Fatalf("compact=%d JSON=%d err=%v", len(fp), len(encoded), err)
		}
	}
	// Canonical keys and exported attributes agree, including repaired UTF-8.
	data, err := e.BuildPayloadForTest()
	if err != nil {
		t.Fatal(err)
	}
	for _, metric := range decodeReliabilityMetrics(t, data) {
		if metric.Name != "counter" {
			continue
		}
		seen := make(map[string]bool)
		for _, point := range metric.Sum.DataPoints {
			attrs, _ := json.Marshal(point.Attributes)
			if seen[string(attrs)] {
				t.Fatalf("wire-equivalent identities exported separately: %s", attrs)
			}
			seen[string(attrs)] = true
		}
	}
}

func TestExporterInvalidAttributeIdentitiesUseOverflow(t *testing.T) {
	e := newTestExporter(t, "http://collector.invalid")
	many := make(map[string]string)
	for i := 0; i <= maxMetricAttributes; i++ {
		many[fmt.Sprint(i)] = "value"
	}
	for _, attrs := range []map[string]string{
		many, {strings.Repeat("k", 65): "value"}, {overflowKey: "true"},
		{" " + overflowKey + " ": "true"}, {" a ": "one", "a": "two"},
		{strings.Repeat("\xff", 64): "value"}, // Repair would exceed 64 key bytes.
	} {
		e.RecordSum("counter", "1", 1, attrs)
	}
	if e.Health().Overflow != 6 || e.overflowReasons[overflowAttributes] != 6 || e.overflowFamilies != 1 || e.regularPoints != 0 {
		t.Fatalf("health=%+v reasons=%v families=%d", e.Health(), e.overflowReasons, e.overflowFamilies)
	}
	if p := e.metrics["counter"].points[overflowFingerprint]; p == nil || p.intValue != 6 || len(p.attrs) != 1 || p.attrs[0].Value.BoolValue == nil || !*p.attrs[0].Value.BoolValue {
		t.Fatalf("overflow=%+v", p)
	}
}

func TestExporterOwnsAdmittedStringsAndSnapshots(t *testing.T) {
	e := newTestExporter(t, "http://collector.invalid")
	backed := func(s string) string { return (s + strings.Repeat("x", 1024*1024))[:len(s)] }
	name, unit, key, value := backed("histogram"), backed("seconds"), backed("label"), backed("value")
	bounds := []float64{1, 5}
	attrs := map[string]string{key: strings.Repeat(" ", 1024*1024) + value + strings.Repeat(" ", 1024*1024)}
	e.RecordHistogram(name, unit, 2, attrs, bounds)
	m := e.metrics[name]
	var point *histPoint
	for _, p := range m.histPoints {
		point = p
	}
	for _, pair := range [][2]string{{name, m.name}, {unit, m.unit}, {key, point.attrs[0].Key}, {strings.TrimSpace(attrs[key]), point.attrs[0].Value.StringValue}} {
		if pair[0] != pair[1] || unsafe.StringData(pair[0]) == unsafe.StringData(pair[1]) {
			t.Fatalf("retained string not independently owned: %q", pair[1])
		}
	}
	e.RecordSum("sum", "1", 1, map[string]string{"id": "old"})
	e.RecordGauge("gauge", "1", 1, nil)
	e.RecordSumFloat("float_sum", "1", 1.5, nil)
	e.RecordGaugeFloat("float_gauge", "1", 2.5, nil)
	before := e.snapshotMetricsLocked()
	encodedBefore, err := json.Marshal(before)
	if err != nil {
		t.Fatal(err)
	}
	charge := e.approxBytes
	attrs[key], bounds[0] = "changed", -1
	e.RecordHistogram(name, unit, 4, map[string]string{key: value}, []float64{1, 5})
	e.RecordSum("sum", "1", 2, map[string]string{"id": "old"})
	e.RecordGauge("gauge", "1", 3, nil)
	e.RecordSumFloat("float_sum", "1", 2.5, nil)
	e.RecordGaugeFloat("float_gauge", "1", 3.5, nil)
	encodedAfter, err := json.Marshal(before)
	if err != nil || string(encodedBefore) != string(encodedAfter) || charge != e.approxBytes {
		t.Fatalf("updates mutated snapshot or admission charge: err=%v charge=%d/%d", err, charge, e.approxBytes)
	}
	for _, m := range before {
		if m.Histogram != nil && !reflect.DeepEqual(m.Histogram.DataPoints[0].ExplicitBounds, []float64{1, 5}) {
			t.Fatal("caller changed retained bounds")
		}
	}
}

func TestExporterResidentAccountingIncludesAttributesAndReserves(t *testing.T) {
	e := newTestExporter(t, "http://collector.invalid")
	if e.approxBytes != maxMetricFamilies*maxFamilyReserveBytes {
		t.Fatalf("initial permanent reservation=%d", e.approxBytes)
	}
	m := &aggregatedMetric{kind: "histogram", histBounds: make([]float64, maxHistogramBounds)}
	worstReserve := metricApproxBytes(strings.Repeat("n", 256), strings.Repeat("u", 64)) + 8*maxHistogramBounds + m.pointApproxBytes(overflowFingerprint, overflowAttrs())
	if worstReserve > maxFamilyReserveBytes {
		t.Fatalf("max metadata/overflow=%d reserve=%d", worstReserve, maxFamilyReserveBytes)
	}
	attrs, valid := sanitizeAttrs(map[string]string{"key": strings.Repeat("界", 128)})
	if !valid {
		t.Fatal("valid Unicode attrs rejected")
	}
	fp := fingerprint(attrs)
	want := pointOverheadBytes + len(fp) + attributeOverheadBytes + len("key") + 3*128 + 8*(maxHistogramBounds+1)
	if got := m.pointApproxBytes(fp, attrs); got != want {
		t.Fatalf("point estimate=%d want=%d (key and retained strings charged separately)", got, want)
	}
	for _, name := range []string{"a", "b", "c"} {
		e.RecordHistogram(name, "1", 1, map[string]string{"key": "value"}, []float64{1, 5})
	}
	checkExporterAdmissionAccounting(t, e)
}

func checkExporterAdmissionAccounting(t *testing.T, e *Exporter) {
	t.Helper()
	global := maxMetricFamilies * maxFamilyReserveBytes
	ordinary, all := 0, 0
	for _, m := range e.metrics {
		family := metricApproxBytes(m.name, m.unit) + 8*len(m.histBounds) + m.pointApproxBytes(overflowFingerprint, overflowAttrs())
		add := func(fp string, attrs []keyValue) {
			all++
			if fp != overflowFingerprint {
				ordinary++
				charge := m.pointApproxBytes(fp, attrs)
				family += charge
				global += charge
			}
		}
		for fp, p := range m.points {
			add(fp, p.attrs)
		}
		for fp, p := range m.histPoints {
			add(fp, p.attrs)
		}
		if family != m.residentBytes || family > maxMetricResidentBytes {
			t.Fatalf("family=%s estimate=%d tracked=%d limit=%d", m.name, family, m.residentBytes, maxMetricResidentBytes)
		}
	}
	if global != e.approxBytes || global > maxResidentBytes || ordinary != e.regularPoints || all != e.pointCount {
		t.Fatalf("global=%d tracked=%d limit=%d ordinary=%d/%d all=%d/%d", global, e.approxBytes, maxResidentBytes, ordinary, e.regularPoints, all, e.pointCount)
	}
}

func largeAdmissionAttrs(id int) map[string]string {
	attrs := make(map[string]string, maxMetricAttributes)
	for i := 0; i < maxMetricAttributes; i++ {
		attrs[fmt.Sprintf("%02d", i)+strings.Repeat("k", 62)] = strings.Repeat("界", 126) + fmt.Sprintf("%02x", id)
	}
	return attrs
}

func TestExporterSharedByteBudgetPreservesFutureFamiliesAndGaugeBalance(t *testing.T) {
	e := newTestExporter(t, "http://collector.invalid")
	bounds := make([]float64, maxHistogramBounds)
	for i := range bounds {
		bounds[i] = float64(i)
	}
	// Large (but valid) identities hit family bytes, then shared bytes, well
	// before either series-count limit. No counters are artificially prefilled.
fill:
	for family := 0; family < maxMetricFamilies; family++ {
		for series := 0; series < maxSeriesPerMetric; series++ {
			e.RecordHistogram(fmt.Sprintf("hist.%d", family), "1", 2, largeAdmissionAttrs(series), bounds)
			if e.overflowReasons[overflowGlobalBytes] > 0 {
				break fill
			}
		}
	}
	if e.overflowReasons[overflowFamilyBytes] == 0 || e.overflowReasons[overflowGlobalBytes] == 0 || e.regularPoints >= maxQueuePoints {
		t.Fatalf("did not independently reach byte gates: reasons=%v ordinary=%d", e.overflowReasons, e.regularPoints)
	}
	if e.overflowReasons[overflowFamilySeries] != 0 || e.overflowReasons[overflowGlobalSeries] != 0 {
		t.Fatalf("unexpected series exhaustion: %v", e.overflowReasons)
	}
	// The busy first family borrowed far more than a 16MiB/128 partition.
	if e.metrics["hist.0"].residentBytes <= maxResidentBytes/maxMetricFamilies {
		t.Fatal("family cannot borrow from unused shared capacity")
	}
	s := NewSink(e, nil, "", "", "", false)
	attrs := largeAdmissionAttrs(0)
	s.inflight("held", 1, attrs)
	s.maximum("largest", 3, attrs)
	if e.metrics["held"].points[overflowFingerprint] == nil || e.metrics["largest"].points[overflowFingerprint] == nil {
		t.Fatal("expected gauge overflow after shared exhaustion")
	}
	charge := e.approxBytes
	for len(e.metrics) < maxMetricFamilies {
		name := fmt.Sprintf("%03d.", len(e.metrics)) + strings.Repeat("n", 252)
		e.RecordHistogram(name, strings.Repeat("u", 64), 3, attrs, bounds)
		p := e.metrics[name].histPoints[overflowFingerprint]
		if p == nil || p.count != 1 || p.sum != 3 {
			t.Fatalf("new maximum-shape family lost overflow storage: %s", name)
		}
	}
	// Creating families must not release previously unused reserved headroom.
	s.inflight("held", -1, attrs)
	s.maximum("largest", 2, attrs)
	s.maximum("largest", 5, attrs)
	if len(e.metrics["held"].points) != 1 || e.metrics["held"].points[overflowFingerprint].intValue != 0 || e.metrics["largest"].points[overflowFingerprint].intValue != 5 {
		t.Fatal("overflow gauges changed identity or lost additive/maximum semantics")
	}
	e.RecordHistogram("hist.0", "1", 4, attrs, bounds) // Admitted series still updates.
	if n, _ := observerHistogram(t, e, "hist.0", attrs); n != 2 {
		t.Fatalf("existing series stopped updating: count=%d", n)
	}
	if e.approxBytes != charge || e.Dropped() != 0 {
		t.Fatalf("reservation accounting changed on overflow/update: %d/%d health=%+v", charge, e.approxBytes, e.Health())
	}
	e.RecordSum("family.129", "1", 1, nil)
	if e.Dropped() != 1 {
		t.Fatal("family exhaustion no longer bounded")
	}
	checkExporterAdmissionAccounting(t, e)
}
