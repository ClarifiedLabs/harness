package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"harness/internal/logging"
	"harness/internal/mcpproxy"
)

// serveCategory labels serve-level log records (config warnings, lifecycle).
const (
	serveCategory  = "mcp_proxy"
	configCategory = "mcp_config"
)

// runServe parses serve flags, loads config, resolves the log sink, wires
// signals into a cancellable context, and runs the daemon.
func runServe(env environment, args []string) int {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	fs.SetOutput(io.Discard) // errors are returned, printed once below (cmd/harness convention)
	// -config defaults to "" so we can distinguish "unset" (a missing default
	// path is non-fatal) from an explicit value (a typo is a hard error).
	configPath := fs.String("config", "", "config file path")
	listen := fs.String("listen", "", "HTTP listen address (overrides config and default)")
	stdio := fs.Bool("stdio", false, "serve MCP over stdin/stdout instead of HTTP")
	logPath := fs.String("log", "", "log file path (overrides config logFile)")
	logLevel := fs.String("log-level", "", "log level: debug|info|warn|error (overrides config)")
	logFormat := fs.String("log-format", "", "log format: json|text (overrides config)")
	if err := fs.Parse(args); err != nil {
		if err == flag.ErrHelp {
			usage(env.stdout, env.getenv)
			return exitOK
		}
		fmt.Fprintf(env.stderr, "harness-mcp-proxy: %v\n", err)
		return exitUsage
	}

	cfg, err := mcpproxy.LoadConfig(resolveConfigPath(*configPath, flagWasSet(fs, "config"), env.getenv))
	if err != nil {
		fmt.Fprintf(env.stderr, "harness-mcp-proxy: %v\n", err)
		return exitRuntime
	}

	// Flags override config; LoadConfig fills the default listener.
	if *listen != "" {
		cfg.Listen = *listen
	}

	// Resolve the effective log level (flag > config > info), validating early so
	// a bad level surfaces as a usage error before we open any sink.
	level := cfg.LogLevel
	if *logLevel != "" {
		level = *logLevel
	}
	if _, err := logging.ParseLevel(level); err != nil {
		fmt.Fprintf(env.stderr, "harness-mcp-proxy: %v\n", err)
		return exitUsage
	}
	format := cfg.LogFormat
	if *logFormat != "" {
		format = *logFormat
	}
	if _, err := logging.ParseFormat(format); err != nil {
		fmt.Fprintf(env.stderr, "harness-mcp-proxy: %v\n", err)
		return exitUsage
	}

	// Resolve and open the log sink (flag > config > stderr-if-TTY > file).
	sink, closeSink, err := openLogSink(logSinkParams{
		flagPath:   *logPath,
		configPath: cfg.LogFile,
		stderr:     env.stderr,
	})
	if err != nil {
		fmt.Fprintf(env.stderr, "harness-mcp-proxy: %v\n", err)
		return exitRuntime
	}
	defer closeSink()

	logger, err := logging.NewProxyLogger(sink, level, format)
	if err != nil {
		fmt.Fprintf(env.stderr, "harness-mcp-proxy: %v\n", err)
		return exitUsage
	}

	// Surface config load warnings (unset ${VAR}, skipped invalid servers) now
	// that the logger exists; library code never prints these itself.
	for _, w := range cfg.Warnings {
		logger.Warn(w, logging.Category(configCategory))
	}

	// Wire SIGINT/SIGTERM into ctx cancellation. The signal channel is injected
	// so tests can drive a clean shutdown without sending real process signals.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if env.sigCh != nil {
		go func() {
			select {
			case <-env.sigCh:
				cancel()
			case <-ctx.Done():
			}
		}()
	}

	d := mcpproxy.NewDaemon(cfg, logger)
	if *stdio {
		// stdout is the MCP channel in stdio mode; logs already go to the sink
		// (stderr or -log file), never stdout.
		err = d.RunStdio(ctx, stdioRWC{r: env.stdin, w: env.stdout})
	} else {
		err = d.Run(ctx)
	}
	if err != nil {
		logger.Error("proxy exited", logging.Category(serveCategory), "err", err)
		fmt.Fprintf(env.stderr, "harness-mcp-proxy: %v\n", err)
		return exitRuntime
	}
	return exitOK
}

// stdioRWC adapts a reader and writer into the io.ReadWriteCloser RunStdio drives
// over stdin/stdout. Close is a no-op so a final flush is never cut off.
type stdioRWC struct {
	r io.Reader
	w io.Writer
}

func (c stdioRWC) Read(p []byte) (int, error)  { return c.r.Read(p) }
func (c stdioRWC) Write(p []byte) (int, error) { return c.w.Write(p) }
func (c stdioRWC) Close() error                { return nil }

// logSinkParams carries the inputs to log-sink resolution so the precedence
// rules are unit-testable without opening real files or process state.
type logSinkParams struct {
	flagPath   string
	configPath string
	stderr     io.Writer
}

// openLogSink resolves and opens the log sink in precedence order:
//
//	-log flag > config logFile > stderr
//
// File sinks open append-only; the returned close func is a no-op for the stderr
// sink (we must not close the process's stderr).
func openLogSink(p logSinkParams) (sink io.Writer, closeFn func(), err error) {
	switch {
	case p.flagPath != "":
		return openLogFile(p.flagPath)
	case p.configPath != "":
		return openLogFile(p.configPath)
	default:
		return p.stderr, func() {}, nil
	}
}

// openLogFile opens path append-only, creating it if absent. Parent directories
// are created best-effort first so an explicit nested log path works.
func openLogFile(path string) (io.Writer, func(), error) {
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		// Best-effort: a creation failure is reported by the OpenFile below with a
		// clearer path-specific error.
		_ = os.MkdirAll(dir, 0o755)
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return nil, nil, fmt.Errorf("open log file %s: %w", path, err)
	}
	return f, func() { _ = f.Close() }, nil
}
