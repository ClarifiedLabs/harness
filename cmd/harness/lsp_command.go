package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	"harness/internal/buildinfo"
	"harness/internal/cli"
	"harness/internal/logging"
	"harness/internal/lspproxy"
	"harness/internal/mcp"
	"harness/internal/ui"
)

const lspCategory = "lsp"

type lspRWConn struct {
	r io.Reader
	w io.Writer
}

func (c lspRWConn) Read(p []byte) (int, error)  { return c.r.Read(p) }
func (c lspRWConn) Write(p []byte) (int, error) { return c.w.Write(p) }
func (c lspRWConn) Close() error                { return nil }

func runLSPVersion(env environment, _ cli.Invocation) int {
	signal.Ignore(syscall.SIGHUP)
	fmt.Fprintf(env.stdout, "%s (MCP protocol %s)\n", buildinfo.Line("harness lsp"), mcp.ProtocolVersion)
	return ui.ExitOK
}

func runLSPServe(env environment, invocation cli.Invocation) int {
	signal.Ignore(syscall.SIGHUP)
	values := invocation.Flags
	configPath := cliLast(values, "config", "")
	namespace := cliLast(values, "namespace", "lsp")
	logPath := cliLast(values, "log", "")
	logLevel := cliLast(values, "log_level", "info")
	logFormat := cliLast(values, "log_format", "json")

	cfg, err := lspproxy.LoadConfig(resolveLSPConfigPath(configPath, values.Has("config"), env.getenv))
	if err != nil {
		fmt.Fprintf(env.stderr, "harness lsp: %v\n", err)
		return ui.ExitRuntime
	}

	if _, err := logging.ParseLevel(logLevel); err != nil {
		fmt.Fprintf(env.stderr, "harness lsp: %v\n", err)
		return ui.ExitUsage
	}
	if _, err := logging.ParseFormat(logFormat); err != nil {
		fmt.Fprintf(env.stderr, "harness lsp: %v\n", err)
		return ui.ExitUsage
	}

	sink, closeSink, err := openLSPLogSink(logPath, env.stderr)
	if err != nil {
		fmt.Fprintf(env.stderr, "harness lsp: %v\n", err)
		return ui.ExitRuntime
	}
	defer closeSink()

	logger, err := logging.NewProxyLogger(sink, logLevel, logFormat)
	if err != nil {
		fmt.Fprintf(env.stderr, "harness lsp: %v\n", err)
		return ui.ExitUsage
	}
	for _, w := range cfg.Warnings {
		logger.Warn(w, logging.Category(lspCategory))
	}

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

	mgr := lspproxy.NewManager(cfg, namespace, logger)
	defer mgr.Shutdown(context.Background())

	if err := mcp.Serve(ctx, lspRWConn{r: env.stdin, w: env.stdout}, mcp.ServerOptions{
		Info:     mcp.Implementation{Name: "harness lsp", Version: buildinfo.Version},
		Provider: mgr,
		Logger:   logger,
	}); err != nil {
		logger.Error("lsp shim exited", logging.Category(lspCategory), "err", err)
		fmt.Fprintf(env.stderr, "harness lsp: %v\n", err)
		return ui.ExitRuntime
	}
	return ui.ExitOK
}

func resolveLSPConfigPath(flagValue string, explicit bool, getenv func(string) string) string {
	if getenv == nil {
		getenv = os.Getenv
	}
	if explicit {
		return flagValue
	}
	def := lspproxy.DefaultConfigPath(getenv)
	if _, err := os.Stat(def); err == nil {
		return def
	}
	return ""
}

func openLSPLogSink(flagPath string, stderr io.Writer) (io.Writer, func(), error) {
	if flagPath != "" {
		return openLSPLogFile(flagPath)
	}
	return stderr, func() {}, nil
}

func openLSPLogFile(path string) (io.Writer, func(), error) {
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		_ = os.MkdirAll(dir, 0o755)
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return nil, nil, fmt.Errorf("open log file %s: %w", path, err)
	}
	return f, func() { _ = f.Close() }, nil
}
