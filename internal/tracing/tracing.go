// Package tracing provides lightweight W3C Trace Context helpers used to
// correlate harness requests through local proxy processes. It is deliberately
// stdlib-only and does not implement a full telemetry SDK.
package tracing

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net/http"
	"strconv"
	"strings"
)

const (
	TraceparentHeader = "traceparent"
	TracestateHeader  = "tracestate"
)

const (
	traceIDBytes = 16
	spanIDBytes  = 8
)

// Context is the trace metadata carried in W3C Trace Context headers.
type Context struct {
	TraceID      string
	SpanID       string
	ParentSpanID string
	TraceState   string
	Sampled      bool
}

type contextKey struct{}

// ContextWithTrace attaches trace metadata to ctx.
func ContextWithTrace(ctx context.Context, tc Context) context.Context {
	return context.WithValue(ctx, contextKey{}, tc)
}

// TraceFromContext returns trace metadata attached to ctx.
func TraceFromContext(ctx context.Context) (Context, bool) {
	tc, ok := ctx.Value(contextKey{}).(Context)
	if !ok || !validTraceID(tc.TraceID) || !validSpanID(tc.SpanID) {
		return Context{}, false
	}
	return tc, true
}

// TraceFromHeaders parses W3C Trace Context headers. Invalid traceparent values
// are ignored and never fail a request.
func TraceFromHeaders(h http.Header) (Context, bool) {
	tc, ok := ParseTraceparent(h.Get(TraceparentHeader))
	if !ok {
		return Context{}, false
	}
	tc.TraceState = h.Get(TracestateHeader)
	return tc, true
}

// Inject writes W3C Trace Context headers for tc. Invalid trace IDs/spans are not
// injected.
func Inject(h http.Header, tc Context) {
	if !validTraceID(tc.TraceID) || !validSpanID(tc.SpanID) {
		return
	}
	h.Set(TraceparentHeader, FormatTraceparent(tc))
	if tc.TraceState != "" {
		h.Set(TracestateHeader, tc.TraceState)
	}
}

// ParseTraceparent parses a version 00 W3C traceparent header.
func ParseTraceparent(s string) (Context, bool) {
	parts := strings.Split(strings.TrimSpace(s), "-")
	if len(parts) != 4 {
		return Context{}, false
	}
	if parts[0] != "00" {
		return Context{}, false
	}
	traceID := strings.ToLower(parts[1])
	spanID := strings.ToLower(parts[2])
	flagsText := parts[3]
	if !validTraceID(traceID) || !validSpanID(spanID) || len(flagsText) != 2 || !isHex(flagsText) {
		return Context{}, false
	}
	flags, err := strconv.ParseUint(flagsText, 16, 8)
	if err != nil {
		return Context{}, false
	}
	return Context{TraceID: traceID, SpanID: spanID, Sampled: flags&0x01 == 0x01}, true
}

// FormatTraceparent formats tc as a version 00 W3C traceparent header. Callers
// should validate before sending when accepting arbitrary input.
func FormatTraceparent(tc Context) string {
	flags := "00"
	if tc.Sampled {
		flags = "01"
	}
	return "00-" + strings.ToLower(tc.TraceID) + "-" + strings.ToLower(tc.SpanID) + "-" + flags
}

// NewTraceID returns a fresh non-zero 16-byte hex trace ID.
func NewTraceID() (string, error) { return newNonZeroHex(traceIDBytes) }

// NewSpanID returns a fresh non-zero 8-byte hex span ID.
func NewSpanID() (string, error) { return newNonZeroHex(spanIDBytes) }

func newNonZeroHex(n int) (string, error) {
	buf := make([]byte, n)
	for {
		if _, err := rand.Read(buf); err != nil {
			return "", err
		}
		if !allZeroBytes(buf) {
			return hex.EncodeToString(buf), nil
		}
	}
}

// Child returns a child span context under parent.
func Child(parent Context) (Context, error) {
	if !validTraceID(parent.TraceID) || !validSpanID(parent.SpanID) {
		return Context{}, fmt.Errorf("invalid parent trace context")
	}
	spanID, err := NewSpanID()
	if err != nil {
		return Context{}, err
	}
	return Context{
		TraceID:      strings.ToLower(parent.TraceID),
		SpanID:       spanID,
		ParentSpanID: strings.ToLower(parent.SpanID),
		TraceState:   parent.TraceState,
		Sampled:      parent.Sampled,
	}, nil
}

// Tracer creates child spans under one root trace ID for a harness process/run.
type Tracer struct {
	traceID string
	sampled bool
}

// NewTracer creates a tracer with a new root trace ID.
func NewTracer(sampled bool) (*Tracer, error) {
	traceID, err := NewTraceID()
	if err != nil {
		return nil, err
	}
	return &Tracer{traceID: traceID, sampled: sampled}, nil
}

// TraceID returns the tracer's root trace ID.
func (t *Tracer) TraceID() string {
	if t == nil {
		return ""
	}
	return t.traceID
}

// Start returns ctx with a new span context. If ctx already carries a trace, the
// new span is a child of that context; otherwise it uses the tracer's root trace
// ID and has no parent span.
func (t *Tracer) Start(ctx context.Context) (context.Context, Context, error) {
	if parent, ok := TraceFromContext(ctx); ok {
		child, err := Child(parent)
		if err != nil {
			return ctx, Context{}, err
		}
		return ContextWithTrace(ctx, child), child, nil
	}
	if t == nil || !validTraceID(t.traceID) {
		return ctx, Context{}, fmt.Errorf("nil or invalid tracer")
	}
	spanID, err := NewSpanID()
	if err != nil {
		return ctx, Context{}, err
	}
	tc := Context{TraceID: t.traceID, SpanID: spanID, Sampled: t.sampled}
	return ContextWithTrace(ctx, tc), tc, nil
}

// LogAttrs returns slog key/value attrs for tc. The keys are stable so proxy logs
// can be correlated across processes.
func LogAttrs(tc Context) []any {
	attrs := []any{
		"trace_id", strings.ToLower(tc.TraceID),
		"span_id", strings.ToLower(tc.SpanID),
		"parent_span_id", strings.ToLower(tc.ParentSpanID),
		"trace_sampled", tc.Sampled,
	}
	if tc.TraceState != "" {
		attrs = append(attrs, "tracestate", tc.TraceState)
	}
	return attrs
}

func validTraceID(s string) bool { return len(s) == 32 && isHex(s) && !allZeroHex(s) }
func validSpanID(s string) bool  { return len(s) == 16 && isHex(s) && !allZeroHex(s) }

func isHex(s string) bool {
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= '0' && c <= '9':
		case c >= 'a' && c <= 'f':
		case c >= 'A' && c <= 'F':
		default:
			return false
		}
	}
	return true
}

func allZeroHex(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] != '0' {
			return false
		}
	}
	return true
}

func allZeroBytes(b []byte) bool {
	for _, v := range b {
		if v != 0 {
			return false
		}
	}
	return true
}
