package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	"harness/internal/buildinfo"
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

func runLSPCommand(env environment, args []string) int {
	signal.Ignore(syscall.SIGHUP)
	if len(args) == 0 {
		lspUsage(env.stderr, env.getenv)
		return ui.ExitUsage
	}
	switch args[0] {
	case "-h", "--help", "help":
		lspUsage(env.stdout, env.getenv)
		return ui.ExitOK
	case "serve":
		return runLSPServe(env, args[1:])
	case "--version", "version":
		fmt.Fprintf(env.stdout, "%s (MCP protocol %s)\n", buildinfo.Line("harness lsp"), mcp.ProtocolVersion)
		return ui.ExitOK
	default:
		fmt.Fprintf(env.stderr, "harness lsp: unknown subcommand %q\n", args[0])
		lspUsage(env.stderr, env.getenv)
		return ui.ExitUsage
	}
}

func runLSPServe(env environment, args []string) int {
	fs := flag.NewFlagSet("lsp serve", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	configPath := fs.String("config", "", "config file path")
	namespace := fs.String("namespace", "lsp", "tool-name namespace: tools are exposed as mcp__<namespace>__<tool>; empty for bare names")
	logPath := fs.String("log", "", "log file path")
	logLevel := fs.String("log-level", "info", "log level: debug|info|warn|error")
	logFormat := fs.String("log-format", "json", "log format: json|text")
	if err := fs.Parse(args); err != nil {
		if err == flag.ErrHelp {
			lspUsage(env.stdout, env.getenv)
			return ui.ExitOK
		}
		fmt.Fprintf(env.stderr, "harness lsp: %v\n", err)
		return ui.ExitUsage
	}

	cfg, err := lspproxy.LoadConfig(resolveLSPConfigPath(*configPath, flagWasSet(fs, "config"), env.getenv))
	if err != nil {
		fmt.Fprintf(env.stderr, "harness lsp: %v\n", err)
		return ui.ExitRuntime
	}

	if _, err := logging.ParseLevel(*logLevel); err != nil {
		fmt.Fprintf(env.stderr, "harness lsp: %v\n", err)
		return ui.ExitUsage
	}
	if _, err := logging.ParseFormat(*logFormat); err != nil {
		fmt.Fprintf(env.stderr, "harness lsp: %v\n", err)
		return ui.ExitUsage
	}

	sink, closeSink, err := openLSPLogSink(*logPath, env.stderr)
	if err != nil {
		fmt.Fprintf(env.stderr, "harness lsp: %v\n", err)
		return ui.ExitRuntime
	}
	defer closeSink()

	logger, err := logging.NewProxyLogger(sink, *logLevel, *logFormat)
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

	mgr := lspproxy.NewManager(cfg, *namespace, logger)
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

func lspUsage(w io.Writer, getenv func(string) string) {
	if getenv == nil {
		getenv = os.Getenv
	}
	fmt.Fprint(w, `harness lsp - generic LSP-to-MCP shim

Usage:
  harness lsp serve   [-config path] [-namespace ns] [-log path] [-log-level level] [-log-format format]
  harness lsp version
  harness lsp --version

Subcommands:
  serve     Run the shim: launch configured language servers on demand and serve
            their navigation tools over MCP on stdin/stdout. Logs go to stderr
            (or -log); stdout carries the MCP protocol.
  version   Print the release version and MCP protocol revision.

serve flags:
  -config path      config file (default: `+lspproxy.DefaultConfigPath(getenv)+`)
  -namespace ns     tools are exposed as mcp__<ns>__<tool> (default: lsp; empty for bare names behind a proxy)
  -log path         log file (default: stderr)
  -log-level level  debug|info|warn|error (default: info)
  -log-format fmt   json|text (default: json)
`)
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

func flagWasSet(fs *flag.FlagSet, name string) bool {
	set := false
	fs.Visit(func(f *flag.Flag) {
		if f.Name == name {
			set = true
		}
	})
	return set
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
