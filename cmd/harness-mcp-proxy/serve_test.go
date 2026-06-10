package main

import (
	"bytes"
	"context"
	"io"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"harness/internal/mcp"
)

// TestOpenLogSinkPrecedence covers the resolution order flag > config > stderr.
func TestOpenLogSinkPrecedence(t *testing.T) {
	dir := t.TempDir()
	flagPath := filepath.Join(dir, "flag.log")
	configPath := filepath.Join(dir, "config.log")
	stderr := &bytes.Buffer{}

	t.Run("flag wins over config", func(t *testing.T) {
		sink, closeFn, err := openLogSink(logSinkParams{
			flagPath: flagPath, configPath: configPath,
			stderr: stderr,
		})
		if err != nil {
			t.Fatalf("openLogSink: %v", err)
		}
		defer closeFn()
		mustBeFile(t, sink, flagPath)
	})

	t.Run("config wins when no flag", func(t *testing.T) {
		sink, closeFn, err := openLogSink(logSinkParams{
			configPath: configPath, stderr: stderr,
		})
		if err != nil {
			t.Fatalf("openLogSink: %v", err)
		}
		defer closeFn()
		mustBeFile(t, sink, configPath)
	})

	t.Run("stderr when no flag or config", func(t *testing.T) {
		sink, closeFn, err := openLogSink(logSinkParams{
			stderr: stderr,
		})
		if err != nil {
			t.Fatalf("openLogSink: %v", err)
		}
		defer closeFn()
		if sink != stderr {
			t.Fatalf("stderr sink = %T, want the injected stderr buffer", sink)
		}
	})
}

// TestOpenLogFileIsAppend0600 verifies the file sink is append-mode and 0600 so
// repeated serve runs accumulate rather than truncate, and logs are not world-
// readable.
func TestOpenLogFileIsAppend0600(t *testing.T) {
	path := filepath.Join(t.TempDir(), "a.log")
	if err := os.WriteFile(path, []byte("existing\n"), 0o600); err != nil {
		t.Fatalf("seed: %v", err)
	}
	sink, closeFn, err := openLogFile(path)
	if err != nil {
		t.Fatalf("openLogFile: %v", err)
	}
	if _, err := sink.Write([]byte("appended\n")); err != nil {
		t.Fatalf("write: %v", err)
	}
	closeFn()

	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(got) != "existing\nappended\n" {
		t.Errorf("log file = %q, want append (not truncate)", string(got))
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("log file perm = %o, want 0600", perm)
	}
}

// mustBeFile asserts the sink is an *os.File pointing at wantPath.
func mustBeFile(t *testing.T, sink any, wantPath string) {
	t.Helper()
	f, ok := sink.(*os.File)
	if !ok {
		t.Fatalf("sink type = %T, want *os.File", sink)
	}
	if f.Name() != wantPath {
		t.Fatalf("sink path = %q, want %q", f.Name(), wantPath)
	}
}

func TestServeStdioServesOverPipe(t *testing.T) {
	c1, c2 := net.Pipe()
	go runServe(environment{
		stdin:  c2,
		stdout: c2,
		stderr: io.Discard,
		getenv: func(string) string { return "" },
	}, []string{"-stdio"})

	client := mcp.NewClient(c1, mcp.ClientOptions{Info: mcp.Implementation{Name: "t", Version: "0"}})
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if _, err := client.Initialize(ctx); err != nil {
		t.Fatalf("initialize over -stdio: %v", err)
	}
	_ = client.Close()
	_ = c1.Close()
}
