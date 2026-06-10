package logging

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"
)

func TestPlainHandlerRendersLevelCategoryAndMessage(t *testing.T) {
	var b bytes.Buffer
	logger := slog.New(NewPlainHandler(&b, HandlerOptions{Level: slog.LevelInfo}))

	logger.Warn(`Tool "rg" is disabled. Reason: "rg" binary not found.`, Category("cli_tools"))

	got := b.String()
	want := "[warn] [cli_tools] Tool \"rg\" is disabled. Reason: \"rg\" binary not found.\n"
	if got != want {
		t.Fatalf("log output = %q, want %q", got, want)
	}
}

func TestPlainHandlerFiltersByLevel(t *testing.T) {
	var b bytes.Buffer
	logger := slog.New(NewPlainHandler(&b, HandlerOptions{Level: slog.LevelWarn}))

	logger.Info("hidden")
	logger.Warn("shown")

	got := b.String()
	if got != "[warn] shown\n" {
		t.Fatalf("log output = %q", got)
	}
}

func TestPlainHandlerQuietSuppressesNonErrorRecords(t *testing.T) {
	var b bytes.Buffer
	logger := slog.New(NewPlainHandler(&b, HandlerOptions{
		Level: slog.LevelDebug,
		Quiet: true,
	}))

	logger.Warn("hidden")
	logger.Error("shown")

	if got := b.String(); got != "[error] shown\n" {
		t.Fatalf("quiet log output = %q", got)
	}
}

func TestPlainHandlerRendersExtraAttrsAsPlainText(t *testing.T) {
	var b bytes.Buffer
	logger := slog.New(NewPlainHandler(&b, HandlerOptions{Level: slog.LevelInfo})).
		With("tool", "git").
		WithGroup("detail")

	logger.Info("disabled", slog.String("reason", "missing"))

	got := b.String()
	for _, want := range []string{"[info] disabled", `tool="git"`, `detail.reason="missing"`} {
		if !strings.Contains(got, want) {
			t.Fatalf("log output %q missing %q", got, want)
		}
	}
}

func TestParseLevel(t *testing.T) {
	tests := map[string]slog.Level{
		"":        slog.LevelInfo,
		"DEBUG":   slog.LevelDebug,
		"info":    slog.LevelInfo,
		"warn":    slog.LevelWarn,
		"warning": slog.LevelWarn,
		"error":   slog.LevelError,
	}
	for input, want := range tests {
		got, err := ParseLevel(input)
		if err != nil {
			t.Fatalf("ParseLevel(%q): %v", input, err)
		}
		if got != want {
			t.Fatalf("ParseLevel(%q) = %v, want %v", input, got, want)
		}
	}
	if _, err := ParseLevel("verbose"); err == nil {
		t.Fatal("ParseLevel(\"verbose\") should fail")
	}
}

func TestParseFormat(t *testing.T) {
	for input, want := range map[string]string{
		"":     FormatJSON,
		"JSON": FormatJSON,
		"text": FormatText,
	} {
		got, err := ParseFormat(input)
		if err != nil {
			t.Fatalf("ParseFormat(%q): %v", input, err)
		}
		if got != want {
			t.Fatalf("ParseFormat(%q) = %q, want %q", input, got, want)
		}
	}
	if _, err := ParseFormat("plain"); err == nil {
		t.Fatal("ParseFormat(\"plain\") should fail")
	}
}

func TestNewProxyLoggerUsesBuiltInFormats(t *testing.T) {
	var jsonBuf bytes.Buffer
	jsonLogger, err := NewProxyLogger(&jsonBuf, LevelInfo, "")
	if err != nil {
		t.Fatalf("NewProxyLogger json: %v", err)
	}
	jsonLogger.Info("served", "requester", "harness")
	var record map[string]any
	if err := json.Unmarshal(jsonBuf.Bytes(), &record); err != nil {
		t.Fatalf("json log did not decode: %q: %v", jsonBuf.String(), err)
	}
	if record["msg"] != "served" || record["requester"] != "harness" {
		t.Fatalf("json record = %+v", record)
	}

	var textBuf bytes.Buffer
	textLogger, err := NewProxyLogger(&textBuf, LevelInfo, FormatText)
	if err != nil {
		t.Fatalf("NewProxyLogger text: %v", err)
	}
	textLogger.Info("served", "requester", "harness")
	got := textBuf.String()
	for _, want := range []string{`level=INFO`, `msg=served`, `requester=harness`} {
		if !strings.Contains(got, want) {
			t.Fatalf("text log %q missing %q", got, want)
		}
	}
}
