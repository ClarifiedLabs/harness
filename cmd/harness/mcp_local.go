package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"sort"
	"sync"
	"time"

	"harness/internal/buildinfo"
	"harness/internal/config"
	"harness/internal/logging"
	"harness/internal/mcp"
	"harness/internal/mcpchild"
	"harness/internal/mcptools"
	"harness/internal/tools"
)

// setupLocalMCP spawns the configured local stdio MCP service, registers its
// tools into catalog, and returns the live conn plus its registration summary
// and a cleanup func. Like setupMCP it NEVER fails harness startup: on any error
// it logs one warning and returns ok=false with a no-op cleanup, so the caller
// defers cleanup unconditionally. cleanup closes the conn and reaps the child
// process group.
func setupLocalMCP(ctx context.Context, localCfg config.LocalMCPConfig, explicit bool, catalog *tools.Registry, logger *slog.Logger) (conn *mcptools.Conn, summary mcptools.Summary, cleanup func(), ok bool) {
	noop := func() {}
	command, args, err := resolveLocalCommand(localCfg)
	if err != nil {
		if explicit {
			logger.Warn(fmt.Sprintf("mcp: cannot start local MCP service: %v; local MCP tools unavailable", err), logging.Category("mcp"))
		} else {
			logger.Debug(fmt.Sprintf("mcp: local MCP service unavailable: %v; skipping", err), logging.Category("mcp"))
		}
		return nil, mcptools.Summary{}, noop, false
	}
	env := localChildEnv(localCfg.Env)

	// The dial closure spawns a fresh child per (re)connect and tracks the current
	// one for cleanup. mcptools.Conn calls it lazily on first use and again after a
	// drop; a stale child is reaped in the background so reconnect never blocks.
	var mu sync.Mutex
	var current *mcpchild.Child
	dial := func(ctx context.Context) (io.ReadWriteCloser, error) {
		child, err := mcpchild.Spawn(command, args, env, func(line string) {
			logger.Info(line, logging.Category("mcp"), "stream", "local")
		})
		if err != nil {
			return nil, err
		}
		mu.Lock()
		prev := current
		current = child
		mu.Unlock()
		if prev != nil {
			go prev.Close(context.Background())
		}
		return child.Conn(), nil
	}

	c := mcptools.NewConn(mcptools.Options{
		Info:   mcp.Implementation{Name: "harness", Version: buildinfo.Version},
		Logger: logger,
		Dial:   dial,
	})

	reap := func() {
		mu.Lock()
		child := current
		current = nil
		mu.Unlock()
		if child != nil {
			child.Close(context.Background())
		}
	}

	// A local proxy registers its downstream servers (the shim) asynchronously
	// after start, so the first tools/list can be empty. Poll until tools appear
	// or the budget elapses; Register is idempotent (replace-in-place), so a
	// retry never double-registers. A connection error fails fast.
	regCtx, cancel := context.WithTimeout(ctx, mcpRegisterTimeout)
	defer cancel()
	sum, err := registerLocalWhenReady(regCtx, catalog, c)
	if err != nil {
		if errors.Is(err, context.Canceled) {
			_ = c.Close()
			reap()
			return nil, mcptools.Summary{}, noop, false
		}
		logger.Warn(fmt.Sprintf("mcp: cannot start local MCP service %s: %v; local MCP tools unavailable", command, err), logging.Category("mcp"))
		_ = c.Close()
		reap()
		return nil, mcptools.Summary{}, noop, false
	}
	if sum.Total == 0 {
		logger.Warn(fmt.Sprintf("mcp: local MCP service %s exposed no tools within %s", command, mcpRegisterTimeout), logging.Category("mcp"))
	}

	logger.Info("mcp: local "+mcpConnectedLine(sum), logging.Category("mcp"))
	for _, name := range sum.Skipped {
		logger.Warn(fmt.Sprintf("mcp: skipping local tool %q: name must match [a-zA-Z0-9_-]{1,64}", name), logging.Category("mcp"))
	}
	return c, sum, func() { _ = c.Close(); reap() }, true
}

// localMCPEnabled decides whether to start the local MCP service. Local MCP (and
// the default LSP shim behind it) is disabled by default; env/file configuration
// must opt in with mcp.local.enable=true.
func localMCPEnabled(cfg config.LocalMCPConfig, _ bool) bool {
	return cfg.EnableSet && cfg.Enable
}

// localReadyPoll is how long registerLocalWhenReady waits between retries while
// the local service brings its downstream tools online.
var localReadyPoll = 200 * time.Millisecond

// registerLocalWhenReady registers the local service's tools, retrying while the
// list is empty (the service registers its downstream servers asynchronously)
// until tools appear or ctx is done. A connection/transport error returns
// immediately. On timeout it returns the last (possibly empty) summary with no
// error, so the caller can warn and continue.
func registerLocalWhenReady(ctx context.Context, catalog *tools.Registry, c *mcptools.Conn) (mcptools.Summary, error) {
	var last mcptools.Summary
	for {
		sum, err := mcptools.RegisterWithOptions(ctx, catalog, c, mcptools.RegisterOptions{TrustReadOnlyHint: true})
		if err != nil {
			return mcptools.Summary{}, err
		}
		if sum.Total > 0 {
			return sum, nil
		}
		last = sum
		select {
		case <-ctx.Done():
			return last, nil
		case <-time.After(localReadyPoll):
		}
	}
}

// resolveLocalCommand resolves the command and args for the local MCP service.
// Local MCP is a generic stdio-MCP slot, so an enabled service must configure a
// command explicitly. First-class LSP tools are configured by the top-level lsp
// block instead.
func resolveLocalCommand(cfg config.LocalMCPConfig) (command string, args []string, err error) {
	command = cfg.Command
	if command == "" {
		return "", nil, fmt.Errorf("mcp.local.command is required")
	}
	args = cfg.Args
	return command, args, nil
}

// localChildEnv builds the child environment: nil (inherit parent) when there
// are no overrides, else the parent environment with overrides appended so they
// win.
func localChildEnv(extra map[string]string) []string {
	if len(extra) == 0 {
		return nil
	}
	env := os.Environ()
	keys := make([]string, 0, len(extra))
	for k := range extra {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		env = append(env, k+"="+extra[k])
	}
	return env
}
