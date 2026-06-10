// Package logging provides the CLI's plaintext slog handler. It keeps launch
// diagnostics readable while preserving slog's level and attribute API for
// future call sites.
package logging

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"strconv"
	"strings"
	"sync"
)

const (
	// CategoryKey is the slog attribute key rendered as a second bracketed label,
	// e.g. [warn] [cli_tools] message.
	CategoryKey = "category"

	LevelDebug = "debug"
	LevelInfo  = "info"
	LevelWarn  = "warn"
	LevelError = "error"

	FormatJSON = "json"
	FormatText = "text"
)

// Category returns the standard category label attribute.
func Category(name string) slog.Attr {
	return slog.String(CategoryKey, name)
}

// ParseLevel converts a user-facing log level into slog's level values.
func ParseLevel(name string) (slog.Level, error) {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "", LevelInfo:
		return slog.LevelInfo, nil
	case LevelDebug:
		return slog.LevelDebug, nil
	case LevelWarn, "warning":
		return slog.LevelWarn, nil
	case LevelError:
		return slog.LevelError, nil
	default:
		return slog.LevelInfo, fmt.Errorf("invalid log level %q (valid: debug, info, warn, error)", name)
	}
}

// CanonicalLevel normalizes a user-facing log level for storage on config.
func CanonicalLevel(name string) (string, error) {
	level, err := ParseLevel(name)
	if err != nil {
		return "", err
	}
	switch level {
	case slog.LevelDebug:
		return LevelDebug, nil
	case slog.LevelWarn:
		return LevelWarn, nil
	case slog.LevelError:
		return LevelError, nil
	default:
		return LevelInfo, nil
	}
}

// ParseFormat converts a user-facing log format into its canonical value. The
// empty value defaults proxy daemons to JSON for log-file and collector use.
func ParseFormat(name string) (string, error) {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "", FormatJSON:
		return FormatJSON, nil
	case FormatText:
		return FormatText, nil
	default:
		return "", fmt.Errorf("invalid log format %q (valid: json, text)", name)
	}
}

// HandlerOptions configures PlainHandler.
type HandlerOptions struct {
	Level slog.Leveler
	Quiet bool
}

// NewLogger returns a slog.Logger that writes plaintext records to w. Quiet
// suppresses non-error records handled by this logger.
func NewLogger(w io.Writer, levelName string, quiet bool) (*slog.Logger, error) {
	level, err := ParseLevel(levelName)
	if err != nil {
		return nil, err
	}
	return slog.New(NewPlainHandler(w, HandlerOptions{
		Level: level,
		Quiet: quiet,
	})), nil
}

// NewProxyLogger returns a slog.Logger for long-running proxy daemons. It uses
// the standard library's built-in JSON/Text handlers; unlike the interactive
// CLI logger, proxy logs keep timestamps and structured attributes.
func NewProxyLogger(w io.Writer, levelName, formatName string) (*slog.Logger, error) {
	level, err := ParseLevel(levelName)
	if err != nil {
		return nil, err
	}
	format, err := ParseFormat(formatName)
	if err != nil {
		return nil, err
	}
	if w == nil {
		w = io.Discard
	}
	opts := &slog.HandlerOptions{Level: level}
	switch format {
	case FormatText:
		return slog.New(slog.NewTextHandler(w, opts)), nil
	default:
		return slog.New(slog.NewJSONHandler(w, opts)), nil
	}
}

// PlainHandler renders records as:
//
//	[level] [category] message key=value
//
// It intentionally omits timestamps and source locations because CLI startup
// diagnostics should read like direct user-facing messages, not log files.
type PlainHandler struct {
	w      io.Writer
	level  slog.Leveler
	quiet  bool
	mu     *sync.Mutex
	attrs  []slog.Attr
	groups []string
}

// NewPlainHandler creates a plaintext slog handler.
func NewPlainHandler(w io.Writer, opts HandlerOptions) *PlainHandler {
	if w == nil {
		w = io.Discard
	}
	level := opts.Level
	if level == nil {
		level = slog.LevelInfo
	}
	return &PlainHandler{
		w:     w,
		level: level,
		quiet: opts.Quiet,
		mu:    &sync.Mutex{},
	}
}

func (h *PlainHandler) Enabled(_ context.Context, level slog.Level) bool {
	if h.quiet && level < slog.LevelError {
		return false
	}
	return level >= h.level.Level()
}

func (h *PlainHandler) Handle(_ context.Context, record slog.Record) error {
	var category string
	attrs := make([]slog.Attr, 0, len(h.attrs)+record.NumAttrs())
	collect := func(attr slog.Attr) bool {
		attr.Value = attr.Value.Resolve()
		if attr.Equal(slog.Attr{}) {
			return true
		}
		if attr.Key == CategoryKey {
			category = attr.Value.String()
			return true
		}
		attr.Key = h.qualifiedKey(attr.Key)
		attrs = append(attrs, attr)
		return true
	}
	for _, attr := range h.attrs {
		collect(attr)
	}
	record.Attrs(collect)

	var b strings.Builder
	fmt.Fprintf(&b, "[%s]", levelName(record.Level))
	if category != "" {
		fmt.Fprintf(&b, " [%s]", category)
	}
	if record.Message != "" {
		fmt.Fprintf(&b, " %s", record.Message)
	}
	for _, attr := range attrs {
		fmt.Fprintf(&b, " %s=%s", attr.Key, formatValue(attr.Value))
	}
	b.WriteByte('\n')

	h.mu.Lock()
	defer h.mu.Unlock()
	_, err := io.WriteString(h.w, b.String())
	return err
}

func (h *PlainHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	next := h.clone()
	next.attrs = append(next.attrs, attrs...)
	return next
}

func (h *PlainHandler) WithGroup(name string) slog.Handler {
	if name == "" {
		return h
	}
	next := h.clone()
	next.groups = append(next.groups, name)
	return next
}

func (h *PlainHandler) clone() *PlainHandler {
	next := *h
	next.attrs = append([]slog.Attr(nil), h.attrs...)
	next.groups = append([]string(nil), h.groups...)
	return &next
}

func (h *PlainHandler) qualifiedKey(key string) string {
	if len(h.groups) == 0 {
		return key
	}
	parts := append(append([]string(nil), h.groups...), key)
	return strings.Join(parts, ".")
}

func levelName(level slog.Level) string {
	return strings.ToLower(level.String())
}

func formatValue(v slog.Value) string {
	switch v.Kind() {
	case slog.KindString:
		return strconv.Quote(v.String())
	default:
		return v.String()
	}
}
