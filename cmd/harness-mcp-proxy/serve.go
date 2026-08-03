package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"harness/internal/apikey"
	"harness/internal/buildinfo"
	"harness/internal/cli"
	"harness/internal/logging"
	"harness/internal/mcpproxy"
	"harness/internal/metrics"
)

// serveCategory labels serve-level log records (config warnings, lifecycle).
const (
	serveCategory        = "mcp_proxy"
	configCategory       = "mcp_config"
	defaultMetricsListen = "127.0.0.1:9091"
)

// runServe keeps direct in-package callers concise while routing through the
// same catalog parser and handler as the executable.
func runServe(env environment, args []string) int {
	env.args = append([]string{"serve"}, args...)
	return run(env)
}

// handleServe resolves all source-aware settings, opens the selected log sink,
// wires signals into a cancellable context, and runs the daemon.
func handleServe(env environment, invocation cli.Invocation) int {
	stdio := cliBool(invocation.Flags, "stdio")
	result, err := mcpproxy.ResolveConfig(mcpproxy.ResolveOptions{
		Flags: invocation.Flags, Getenv: env.getenv, Stdio: stdio,
	})
	if err != nil {
		fmt.Fprintf(env.stderr, "harness-mcp-proxy: %v\n", err)
		return serveConfigErrorExit(env, invocation, err)
	}
	cfg := result.Config

	// Resolve and open the log sink (flag > config > stderr).
	sink, closeSink, err := openLogSink(logSinkParams{
		configPath: cfg.LogFile,
		stderr:     env.stderr,
	})
	if err != nil {
		fmt.Fprintf(env.stderr, "harness-mcp-proxy: %v\n", err)
		return exitRuntime
	}
	defer closeSink()

	logger, err := logging.NewProxyLogger(sink, cfg.LogLevel, cfg.LogFormat)
	if err != nil {
		fmt.Fprintf(env.stderr, "harness-mcp-proxy: %v\n", err)
		return exitUsage
	}

	// Surface config load warnings (unset ${VAR}, skipped invalid servers) now
	// that the logger exists; library code never prints these itself.
	for _, w := range cfg.Warnings {
		logger.Warn(w, logging.Category(configCategory))
	}

	var authStore *apikey.DynamicStore
	var keyFile string
	var keyFileState apikey.FileState
	if !stdio {
		keyFile = cfg.APIKeysFile
		initialKeys, state, err := apikey.LoadInitialFile(keyFile, result.APIKeysFileExplicit)
		if err != nil {
			fmt.Fprintf(env.stderr, "harness-mcp-proxy: api keys file %s: %v\n", keyFile, err)
			return exitRuntime
		}
		authStore = apikey.NewDynamicStore(initialKeys, nil)
		keyFileState = state
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

	if authStore != nil {
		go apikey.WatchFile(ctx, keyFile, keyFileState, 2*time.Second, authStore, func(err error) {
			logger.Warn("reload api keys failed", "path", keyFile, "err", err)
		})
	}

	if stdio {
		// Metrics configuration is intentionally inert in stdio mode: no registry is
		// created or passed to the daemon, and no endpoint is started.
		d := mcpproxy.NewDaemon(cfg, logger)
		// stdout is the MCP channel in stdio mode; logs already go to the sink
		// (stderr or -log file), never stdout.
		err = d.RunStdio(ctx, stdioRWC{r: env.stdin, w: env.stdout})
	} else {
		metricsSettings := serveMetricsSettings(result)
		reg := newMCPMetricsRegistry(metricsSettings.Enabled)
		d := mcpproxy.NewDaemonWithOptions(cfg, logger, mcpproxy.DaemonOptions{APIKeys: authStore, Metrics: reg})
		if _, err := metrics.StartEndpoint(ctx, logger.With(logging.Category(serveCategory)), reg, metricsSettings); err != nil {
			fmt.Fprintf(env.stderr, "harness-mcp-proxy: %v\n", err)
			return exitRuntime
		}
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

func serveMetricsSettings(result mcpproxy.ConfigResult) metrics.Settings {
	enabled := result.Config.Metrics.Enabled != nil && *result.Config.Metrics.Enabled
	return metrics.Settings{
		Enabled:        enabled,
		Listen:         result.Config.Metrics.Listen,
		ListenExplicit: result.MetricsListenExplicit,
	}
}

func serveConfigErrorExit(env environment, invocation cli.Invocation, resolveErr error) int {
	if value, ok := invocation.Flags.Last("log_level"); ok && value != "" {
		if _, err := logging.ParseLevel(value); err != nil {
			return exitUsage
		}
	}
	if value, ok := invocation.Flags.Last("log_format"); ok && value != "" {
		if _, err := logging.ParseFormat(value); err != nil {
			return exitUsage
		}
	}

	// ResolveConfig intentionally owns validation but returns a single error type.
	// A second, read-only load only classifies legacy file-sourced logging errors as
	// usage errors; all config I/O, path, and other resolution failures stay runtime.
	path, _ := selectedConfigPath(invocation.Flags, env.getenv)
	cfg, err := mcpproxy.LoadConfig(path)
	if err == nil {
		if _, levelErr := logging.ParseLevel(cfg.LogLevel); levelErr != nil {
			return exitUsage
		}
		if _, formatErr := logging.ParseFormat(cfg.LogFormat); formatErr != nil {
			return exitUsage
		}
	}
	_ = resolveErr
	return exitRuntime
}

func newMCPMetricsRegistry(enabled bool) *metrics.Registry {
	if !enabled {
		return nil
	}
	return metrics.NewWithBuildInfo(metrics.BuildInfo{
		Name:    "mcp_proxy_build_info",
		Help:    "MCP proxy build information.",
		Version: buildinfo.Version,
	})
}

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
