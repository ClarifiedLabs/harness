// Command harness-mcp-proxy is a thin CLI over internal/mcpproxy. It has
// three subcommands: serve (run the proxy daemon, started manually by an
// operator), tools (a debug client that lists the aggregated tool surface), and
// version. All proxy logic lives in internal/mcpproxy and internal/mcp; this
// binary only parses flags, resolves the log sink, wires signals, and dispatches.
//
// It mirrors cmd/harness conventions: flag.ContinueOnError with discarded flag
// output (errors are returned, not printed by the flag package), errors printed
// once at the entry point as "harness-mcp-proxy: %v" to stderr, exit codes
// 0 ok / 1 runtime / 2 usage, and an injectable environment so the run-
// equivalent code is testable in-process.
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"harness/internal/apikey"
	"harness/internal/buildinfo"
	"harness/internal/cli"
	"harness/internal/mcp"
	"harness/internal/mcpproxy"
)

// Exit codes mirror cmd/harness (design §1): 0 ok, 1 runtime, 2 usage.
const (
	exitOK        = 0
	exitRuntime   = 1
	exitUsage     = 2
	exitInterrupt = 130
)

func main() {
	// SIGINT/SIGTERM are forwarded into the serve loop via this channel so a
	// signal cancels the daemon's context (mirrors cmd/harness's injected sigCh).
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	defer signal.Stop(sigCh)

	// SIGHUP is ignored process-wide so a detached proxy survives its
	// controlling terminal closing. This is process-lifetime signal policy, set
	// once here alongside the other signal setup rather than inside runServe, so
	// the run-equivalent code stays free of un-restored process-global mutation
	// (in-process tests call runServe directly).
	signal.Ignore(syscall.SIGHUP)

	os.Exit(run(environment{
		args:   os.Args[1:],
		stdin:  os.Stdin,
		stdout: os.Stdout,
		stderr: os.Stderr,
		getenv: os.Getenv,
		sigCh:  sigCh,
		now:    time.Now,
	}))
}

// environment carries everything run depends on, so the dispatch is testable
// with injected writers, env, and signal channel. A nil sigCh disables
// signal-driven cancellation (tests drive ctx directly or inject their own
// channel).
type environment struct {
	args   []string
	stdin  io.Reader
	stdout io.Writer
	stderr io.Writer
	getenv func(string) string
	sigCh  chan os.Signal
	now    func() time.Time
}

func signalCancelContext(sigCh <-chan os.Signal) (context.Context, context.CancelFunc, func() bool) {
	ctx, cancel := context.WithCancel(context.Background())
	var interrupted atomic.Bool
	if sigCh != nil {
		go func() {
			select {
			case _, ok := <-sigCh:
				if ok {
					interrupted.Store(true)
				}
				cancel()
			case <-ctx.Done():
			}
		}()
	}
	return ctx, cancel, interrupted.Load
}

// run normalizes the two legacy root spellings, parses one selected command
// scope, renders generated help, and dispatches by stable command ID.
func run(env environment) int {
	if len(env.args) > 0 {
		switch env.args[0] {
		case "-h", "--help", "help":
			return writeCommandHelp(env, "root", true)
		case "--version", "version":
			return runVersion(env, cli.Invocation{})
		}
		if env.args[0] != "" && env.args[0][0] == '-' {
			fmt.Fprintf(env.stderr, "harness-mcp-proxy: unknown subcommand %q\n", env.args[0])
			writeCommandHelp(env, "root", false)
			return exitUsage
		}
	}
	args := normalizeCLIArgs(env.args)
	catalog := commandCatalogForEnvironment(env.getenv)
	invocation, err := catalog.Parse(args)
	if err != nil {
		return handleCLIError(env, args, invocation, err)
	}
	if invocation.Action == cli.Help {
		return writeCommandHelp(env, invocation.CommandID, true)
	}
	handler, ok := commandHandlers[invocation.CommandID]
	if !ok {
		fmt.Fprintf(env.stderr, "harness-mcp-proxy: no handler for command %q\n", invocation.CommandID)
		return exitRuntime
	}
	return handler(env, invocation)
}

func runVersion(env environment, _ cli.Invocation) int {
	fmt.Fprintf(env.stdout, "%s (MCP protocol %s)\n", buildinfo.Line("harness-mcp-proxy"), mcp.ProtocolVersion)
	return exitOK
}

// selectedConfigPath preserves legacy path selection for commands that do not
// use ResolveConfig directly. Explicit values are returned verbatim; a missing
// implicit default becomes an empty load path.
func selectedConfigPath(values cli.Values, getenv func(string) string) (string, bool) {
	if value, ok := values.Last("config"); ok {
		return value, true
	}
	path := mcpproxy.DefaultConfigPath(getenv)
	if _, err := os.Stat(path); err == nil {
		return path, false
	}
	return "", false
}

func handleGenerateAPIKey(env environment, invocation cli.Invocation) int {
	name := invocation.Args[0]
	path, _ := selectedConfigPath(invocation.Flags, env.getenv)
	if path == "" {
		path = mcpproxy.DefaultConfigPath(env.getenv)
	}
	var cfg mcpproxy.Config
	if _, err := os.Stat(path); err == nil {
		var loadErr error
		cfg, loadErr = mcpproxy.LoadConfig(path)
		if loadErr != nil {
			fmt.Fprintf(env.stderr, "harness-mcp-proxy: %v\n", loadErr)
			return exitRuntime
		}
	} else if err != nil && !errors.Is(err, os.ErrNotExist) {
		fmt.Fprintf(env.stderr, "harness-mcp-proxy: %v\n", err)
		return exitRuntime
	}
	apiKeysFile := cliLast(invocation.Flags, "api_keys_file", "")
	keyFile := mcpproxy.ResolveAPIKeysFile(path, cfg.APIKeysFile, apiKeysFile, env.getenv)
	ttl, err := parseAPIKeyTTL(cliLast(invocation.Flags, "ttl", ""))
	if err != nil {
		fmt.Fprintf(env.stderr, "harness-mcp-proxy: %v\n", err)
		return exitUsage
	}
	plaintext, err := apikey.Generate(name, apikey.MCPProxyPrefix)
	if err != nil {
		fmt.Fprintf(env.stderr, "harness-mcp-proxy: %v\n", err)
		return exitUsage
	}
	entries, err := apikey.LoadFile(keyFile)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			fmt.Fprintf(env.stderr, "harness-mcp-proxy: api keys file %s: %v\n", keyFile, err)
			return exitRuntime
		}
	}
	store := apikey.Store{Entries: entries}
	now := env.now
	if now == nil {
		now = time.Now
	}
	added := now()
	expiresAt := time.Time{}
	if ttl > 0 {
		expiresAt = added.Add(ttl)
	}
	store.AddWithExpiry(name, plaintext, added, expiresAt)
	if err := apikey.WriteFile(keyFile, store.Entries); err != nil {
		fmt.Fprintf(env.stderr, "harness-mcp-proxy: api keys file %s: %v\n", keyFile, err)
		return exitRuntime
	}
	fmt.Fprintln(env.stdout, plaintext)
	return exitOK
}

func parseAPIKeyTTL(value string) (time.Duration, error) {
	value = strings.TrimSpace(value)
	if value == "" || value == "0" {
		return 0, nil
	}
	ttl, err := time.ParseDuration(value)
	if err != nil {
		return 0, fmt.Errorf("invalid -ttl %q: %w", value, err)
	}
	if ttl < 0 {
		return 0, fmt.Errorf("invalid -ttl %q: duration must be non-negative", value)
	}
	return ttl, nil
}
