package tracing

import (
	"context"
	"net/http"
	"strings"
	"testing"
)

func TestParseAndFormatTraceparent(t *testing.T) {
	const raw = "00-4BF92F3577B34DA6A3CE929D0E0E4736-00F067AA0BA902B7-01"
	tc, ok := ParseTraceparent(raw)
	if !ok {
		t.Fatalf("ParseTraceparent rejected valid header")
	}
	if tc.TraceID != "4bf92f3577b34da6a3ce929d0e0e4736" || tc.SpanID != "00f067aa0ba902b7" || !tc.Sampled {
		t.Fatalf("parsed context = %+v", tc)
	}
	if got := FormatTraceparent(tc); got != strings.ToLower(raw) {
		t.Fatalf("FormatTraceparent = %q, want %q", got, strings.ToLower(raw))
	}
}

func TestParseTraceparentRejectsInvalid(t *testing.T) {
	cases := []string{
		"",
		"01-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01",
		"00-00000000000000000000000000000000-00f067aa0ba902b7-01",
		"00-4bf92f3577b34da6a3ce929d0e0e4736-0000000000000000-01",
		"00-4bf92f3577b34da6a3ce929d0e0e473x-00f067aa0ba902b7-01",
		"00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-zz",
	}
	for _, c := range cases {
		if tc, ok := ParseTraceparent(c); ok {
			t.Fatalf("ParseTraceparent(%q) = %+v, true; want false", c, tc)
		}
	}
}

func TestNewIDsAreValidAndNonZero(t *testing.T) {
	traceID, err := NewTraceID()
	if err != nil {
		t.Fatalf("NewTraceID: %v", err)
	}
	if !validTraceID(traceID) {
		t.Fatalf("trace id %q is invalid", traceID)
	}
	spanID, err := NewSpanID()
	if err != nil {
		t.Fatalf("NewSpanID: %v", err)
	}
	if !validSpanID(spanID) {
		t.Fatalf("span id %q is invalid", spanID)
	}
}

func TestChildLinksParent(t *testing.T) {
	parent := Context{TraceID: "4bf92f3577b34da6a3ce929d0e0e4736", SpanID: "00f067aa0ba902b7", TraceState: "a=b", Sampled: true}
	child, err := Child(parent)
	if err != nil {
		t.Fatalf("Child: %v", err)
	}
	if child.TraceID != parent.TraceID || child.ParentSpanID != parent.SpanID || child.TraceState != parent.TraceState || !child.Sampled {
		t.Fatalf("child = %+v, parent = %+v", child, parent)
	}
	if child.SpanID == parent.SpanID || !validSpanID(child.SpanID) {
		t.Fatalf("child span id = %q", child.SpanID)
	}
}

func TestHeadersAndContext(t *testing.T) {
	tc := Context{TraceID: "4bf92f3577b34da6a3ce929d0e0e4736", SpanID: "00f067aa0ba902b7", TraceState: "rojo=00", Sampled: true}
	h := http.Header{}
	Inject(h, tc)
	got, ok := TraceFromHeaders(h)
	if !ok {
		t.Fatalf("TraceFromHeaders rejected injected headers")
	}
	if got != tc {
		t.Fatalf("TraceFromHeaders = %+v, want %+v", got, tc)
	}
	ctx := ContextWithTrace(context.Background(), got)
	fromCtx, ok := TraceFromContext(ctx)
	if !ok || fromCtx != got {
		t.Fatalf("TraceFromContext = %+v, %v; want %+v, true", fromCtx, ok, got)
	}
}

func TestTracerStart(t *testing.T) {
	tr, err := NewTracer(true)
	if err != nil {
		t.Fatalf("NewTracer: %v", err)
	}
	ctx, root, err := tr.Start(context.Background())
	if err != nil {
		t.Fatalf("Start root: %v", err)
	}
	if root.TraceID != tr.TraceID() || root.ParentSpanID != "" || !root.Sampled {
		t.Fatalf("root = %+v", root)
	}
	_, child, err := tr.Start(ctx)
	if err != nil {
		t.Fatalf("Start child: %v", err)
	}
	if child.TraceID != root.TraceID || child.ParentSpanID != root.SpanID || child.SpanID == root.SpanID {
		t.Fatalf("child = %+v, root = %+v", child, root)
	}
}
