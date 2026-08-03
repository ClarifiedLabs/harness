package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	"harness/internal/auth"
	"harness/internal/cli"
	"harness/internal/mcpproxy"
)

func handleAuthAction(env environment, invocation cli.Invocation) int {
	action := invocation.CommandPath[len(invocation.CommandPath)-1]
	serverName := invocation.Args[0]
	cfgPath, _ := selectedConfigPath(invocation.Flags, env.getenv)
	cfg, err := mcpproxy.LoadConfig(cfgPath)
	if err != nil {
		fmt.Fprintf(env.stderr, "harness-mcp-proxy: %v\n", err)
		return exitRuntime
	}
	rs, ok := authServerConfig(cfg, serverName)
	if !ok {
		fmt.Fprintf(env.stderr, "harness-mcp-proxy: server %q is not configured\n", serverName)
		return exitRuntime
	}
	if rs.Auth == nil {
		fmt.Fprintf(env.stderr, "harness-mcp-proxy: server %q has no auth config\n", serverName)
		return exitUsage
	}

	ctx, cancel, interrupted := signalCancelContext(env.sigCh)
	defer cancel()
	switch action {
	case "login":
		err = auth.Login(ctx, *rs.Auth, auth.LoginOptions{
			Name:      rs.Name,
			ConfigDir: rs.ConfigDir,
			Getenv:    env.getenv,
			Client:    http.DefaultClient,
			Stdout:    env.stdout,
			Stderr:    env.stderr,
		})
	case "logout":
		err = auth.Logout(*rs.Auth, rs.ConfigDir, rs.Name)
		if err == nil {
			fmt.Fprintf(env.stdout, "Removed OAuth token for %s\n", rs.Name)
		}
	case "status":
		err = auth.Status(*rs.Auth, rs.ConfigDir, rs.Name, env.stdout, time.Now())
	}
	if err != nil {
		if interrupted() || errors.Is(err, context.Canceled) {
			return exitInterrupt
		}
		fmt.Fprintf(env.stderr, "harness-mcp-proxy: auth %s: %v\n", action, err)
		return exitRuntime
	}
	return exitOK
}

func authServerConfig(cfg mcpproxy.Config, name string) (mcpproxy.ResolvedServer, bool) {
	for _, rs := range cfg.Servers {
		if rs.Name == name {
			return rs, true
		}
	}
	return mcpproxy.ResolvedServer{}, false
}
