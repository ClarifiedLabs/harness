package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

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
