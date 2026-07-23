package main

import (
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sync"

	"harness/internal/logging"
)

const sessionDiagnosticsLog = "diagnostics.ndjson"

type sessionLogSink struct {
	mu      sync.RWMutex
	dir     string
	writeMu sync.Mutex
}

func newSessionLogSink(dir string) *sessionLogSink {
	return &sessionLogSink{dir: dir}
}

func (s *sessionLogSink) SetDir(dir string) {
	if s == nil {
		return
	}
	s.mu.Lock()
	s.dir = dir
	s.mu.Unlock()
}

func (s *sessionLogSink) Dir() string {
	if s == nil {
		return ""
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.dir
}

func (s *sessionLogSink) Write(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	dir := s.Dir()
	if dir == "" {
		return len(p), nil
	}

	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return 0, err
	}
	f, err := os.OpenFile(filepath.Join(dir, sessionDiagnosticsLog), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return 0, err
	}
	defer f.Close()

	n, err := f.Write(p)
	if err != nil {
		return n, err
	}
	if n != len(p) {
		return n, io.ErrShortWrite
	}
	return n, nil
}

func newHarnessLogger(w io.Writer, levelName, sessionDir string, enableSessionDiagnostics bool) (*slog.Logger, *slog.Logger, *sessionLogSink, error) {
	level, err := logging.ParseLevel(levelName)
	if err != nil {
		return nil, nil, nil, err
	}
	display := logging.NewPlainHandler(w, logging.HandlerOptions{Level: level})
	if !enableSessionDiagnostics {
		return slog.New(display), nil, nil, nil
	}

	sink := newSessionLogSink(sessionDir)
	diagnostics := slog.NewJSONHandler(sink, &slog.HandlerOptions{Level: slog.LevelDebug})
	return slog.New(logging.NewTeeHandler(display, diagnostics)), slog.New(diagnostics), sink, nil
}
