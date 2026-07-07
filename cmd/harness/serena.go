package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sync"

	"harness/internal/buildinfo"
	"harness/internal/config"
	"harness/internal/logging"
	"harness/internal/mcp"
	"harness/internal/mcpchild"
	"harness/internal/mcptools"
	"harness/internal/tools"
)

const serenaNamespace = "serena"

const serenaSystemHint = "Serena MCP tools are available under mcp__serena__*. Prefer them for symbol-aware code navigation and semantic refactors. Use harness built-in tools for shell commands, tests, broad text search, and simple file edits. If Serena reports the project is inactive, call its activation/instructions tools first."

func setupSerena(ctx context.Context, serenaCfg config.SerenaConfig, catalog *tools.Registry, logger *slog.Logger) (summary mcptools.Summary, cleanup func(), ok bool) {
	noop := func() {}
	if serenaCfg.Command == "" {
		logger.Warn("lsp: cannot start Serena: lsp.serena.command is required; Serena tools unavailable", logging.Category("lsp"))
		return mcptools.Summary{}, noop, false
	}
	env := localChildEnv(serenaCfg.Env)

	var mu sync.Mutex
	var current *mcpchild.Child
	dial := func(ctx context.Context) (io.ReadWriteCloser, error) {
		child, err := mcpchild.Spawn(serenaCfg.Command, serenaCfg.Args, env, func(line string) {
			logger.Info(line, logging.Category("lsp"), "stream", serenaNamespace)
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

	regCtx, cancel := context.WithTimeout(ctx, mcpRegisterTimeout)
	defer cancel()
	sum, err := registerLocalWhenReady(regCtx, catalog, c, mcptools.RegisterOptions{
		TrustReadOnlyHint: true,
		Namespace:         serenaNamespace,
	})
	if err != nil {
		if errors.Is(err, context.Canceled) {
			_ = c.Close()
			reap()
			return mcptools.Summary{}, noop, false
		}
		logger.Warn(fmt.Sprintf("lsp: cannot start Serena MCP service %s: %v; Serena tools unavailable", serenaCfg.Command, err), logging.Category("lsp"))
		_ = c.Close()
		reap()
		return mcptools.Summary{}, noop, false
	}
	if sum.Total == 0 {
		logger.Warn(fmt.Sprintf("lsp: Serena MCP service %s exposed no tools within %s", serenaCfg.Command, mcpRegisterTimeout), logging.Category("lsp"))
	}

	logger.Info(fmt.Sprintf("lsp: Serena registered %d tools", sum.Total), logging.Category("lsp"))
	for _, name := range sum.Skipped {
		logger.Warn(fmt.Sprintf("lsp: skipping Serena tool %q: qualified name must match [a-zA-Z0-9_-]{1,64}", name), logging.Category("lsp"))
	}
	return sum, func() { _ = c.Close(); reap() }, true
}
