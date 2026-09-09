package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"harness/internal/logging"
	"harness/internal/ui"
)

func TestHarnessLoggerDebugDuringActiveStatus(t *testing.T) {
	for _, level := range []string{"info", "debug"} {
		t.Run(level, func(t *testing.T) {
			dir := t.TempDir()
			var stdout, stderr bytes.Buffer
			output := ui.NewOutputCoordinator(&stdout, &stderr)
			logger, _, _, err := newHarnessLogger(output.Stderr(), level, dir, true)
			if err != nil {
				t.Fatal(err)
			}
			status := "\r\x1b[2K[turn: 1 · prompt running]"
			output.SetStatus([]byte(status))
			logger.Debug("periodic OTEL export failed", logging.Category("otel"))
			// Inspect before clearing status or ending the prompt: no deferred flush.
			want := status
			if level == "debug" {
				want += "\r\x1b[2K[debug] [otel] periodic OTEL export failed\n" + status
			}
			if got := stderr.String(); got != want {
				t.Fatalf("live diagnostic=%q want=%q", got, want)
			}
			if stdout.Len() != 0 {
				t.Fatalf("diagnostic wrote stdout: %q", stdout.String())
			}
			data, err := os.ReadFile(filepath.Join(dir, sessionDiagnosticsLog))
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(string(data), `"level":"DEBUG"`) || !strings.Contains(string(data), `"msg":"periodic OTEL export failed"`) || bytes.ContainsRune(data, '\x1b') {
				t.Fatalf("missing debug diagnostic or leaked ANSI: %s", data)
			}
		})
	}
}

func TestHarnessLoggerPersistsWarningWhenDisplayLevelSuppressesIt(t *testing.T) {
	dir := t.TempDir()
	var display bytes.Buffer
	_, diagnosticLogger, sink, err := newHarnessLogger(&display, "error", dir, true)
	if err != nil {
		t.Fatalf("newHarnessLogger: %v", err)
	}
	if sink == nil {
		t.Fatal("diagnostics sink = nil")
	}
	if diagnosticLogger == nil {
		t.Fatal("diagnostic logger = nil")
	}
	diagnosticLogger.Warn("model compatibility diagnostic", "proxy_request_id", 42, "category", "multimodal_tool_result_rejected")
	if display.Len() != 0 {
		t.Fatalf("warn should be hidden at error display level: %q", display.String())
	}
	data, err := os.ReadFile(filepath.Join(dir, sessionDiagnosticsLog))
	if err != nil {
		t.Fatalf("read diagnostics: %v", err)
	}
	text := string(data)
	for _, want := range []string{`"msg":"model compatibility diagnostic"`, `"proxy_request_id":42`, `"category":"multimodal_tool_result_rejected"`} {
		if !strings.Contains(text, want) {
			t.Fatalf("diagnostics %q missing %q", text, want)
		}
	}
}

func TestHarnessLoggerSeparatesDiagnosticDisplayAndRotatesBothLoggers(t *testing.T) {
	first := filepath.Join(t.TempDir(), "first")
	second := filepath.Join(t.TempDir(), "second")
	var display bytes.Buffer
	logger, diagnosticLogger, sink, err := newHarnessLogger(&display, "debug", first, true)
	if err != nil {
		t.Fatalf("newHarnessLogger: %v", err)
	}

	logger.Info("ordinary first")
	diagnosticLogger.Warn("compatibility first")
	if strings.Contains(display.String(), "compatibility first") {
		t.Fatalf("diagnostic logger wrote to terminal: %q", display.String())
	}
	if !strings.Contains(display.String(), "ordinary first") {
		t.Fatalf("ordinary logger did not write to terminal: %q", display.String())
	}

	sink.SetDir(second)
	logger.Info("ordinary second")
	diagnosticLogger.Warn("compatibility second")

	firstData, err := os.ReadFile(filepath.Join(first, sessionDiagnosticsLog))
	if err != nil {
		t.Fatalf("read first diagnostics: %v", err)
	}
	if !strings.Contains(string(firstData), "ordinary first") || !strings.Contains(string(firstData), "compatibility first") ||
		strings.Contains(string(firstData), "ordinary second") || strings.Contains(string(firstData), "compatibility second") {
		t.Fatalf("first diagnostics = %s", firstData)
	}
	secondData, err := os.ReadFile(filepath.Join(second, sessionDiagnosticsLog))
	if err != nil {
		t.Fatalf("read second diagnostics: %v", err)
	}
	if !strings.Contains(string(secondData), "ordinary second") || !strings.Contains(string(secondData), "compatibility second") {
		t.Fatalf("second diagnostics = %s", secondData)
	}
}

func TestHarnessLoggerDisabledHasNilDiagnosticLogger(t *testing.T) {
	var display bytes.Buffer
	logger, diagnosticLogger, sink, err := newHarnessLogger(&display, "info", t.TempDir(), false)
	if err != nil {
		t.Fatalf("newHarnessLogger: %v", err)
	}
	if logger == nil || diagnosticLogger != nil || sink != nil {
		t.Fatalf("disabled loggers = app %v diagnostic %v sink %v", logger, diagnosticLogger, sink)
	}
	logger.Info("ordinary")
	if !strings.Contains(display.String(), "ordinary") {
		t.Fatalf("ordinary logger output = %q", display.String())
	}
}
