package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"time"

	"harness/internal/auth"
	"harness/internal/mcpproxy"
)

func runAuth(env environment, args []string) int {
	if len(args) == 0 {
		fmt.Fprintln(env.stderr, "harness-mcp-proxy: auth requires login, logout, or status")
		return exitUsage
	}
	switch args[0] {
	case "login", "logout", "status":
		return runAuthAction(env, args[0], args[1:])
	default:
		fmt.Fprintf(env.stderr, "harness-mcp-proxy: unknown auth command %q\n", args[0])
		return exitUsage
	}
}

func runAuthAction(env environment, action string, args []string) int {
	fs := flag.NewFlagSet("auth "+action, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	configPath := fs.String("config", "", "config file path")
	if err := fs.Parse(args); err != nil {
		fmt.Fprintf(env.stderr, "harness-mcp-proxy: %v\n", err)
		return exitUsage
	}
	if fs.NArg() != 1 {
		fmt.Fprintf(env.stderr, "harness-mcp-proxy: auth %s requires exactly one server\n", action)
		return exitUsage
	}
	serverName := fs.Arg(0)
	cfgPath := resolveConfigPath(*configPath, flagWasSet(fs, "config"), env.getenv)
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
