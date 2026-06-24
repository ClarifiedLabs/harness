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
		usageAuth(env.stderr)
		return exitUsage
	}
	switch args[0] {
	case "-h", "--help", "help":
		usageAuth(env.stdout)
		return exitOK
	case "login", "logout", "status":
		return runAuthAction(env, args[0], args[1:])
	default:
		fmt.Fprintf(env.stderr, "harness-mcp-proxy: unknown auth command %q\n", args[0])
		usageAuth(env.stderr)
		return exitUsage
	}
}

func runAuthAction(env environment, action string, args []string) int {
	fs := flag.NewFlagSet("auth "+action, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	configPath := fs.String("config", "", "config file path")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			usageAuthAction(env.stdout, action)
			return exitOK
		}
		fmt.Fprintf(env.stderr, "harness-mcp-proxy: %v\n", err)
		return exitUsage
	}
	if fs.NArg() != 1 {
		fmt.Fprintf(env.stderr, "harness-mcp-proxy: auth %s requires exactly one server\n", action)
		usageAuthAction(env.stderr, action)
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

func usageAuth(w io.Writer) {
	fmt.Fprintln(w, "harness-mcp-proxy auth — manage OAuth tokens for configured HTTP downstream servers.")
	fmt.Fprintln(w)
	fmt.Fprintln(w, "Usage:")
	fmt.Fprintln(w, "  harness-mcp-proxy auth <login|logout|status> [-config path] <server>")
	fmt.Fprintln(w, "  harness-mcp-proxy auth login [-config path] <server>")
	fmt.Fprintln(w)
	fmt.Fprintln(w, "Subcommands:")
	fmt.Fprintln(w, "  login   Start OAuth login for a configured HTTP downstream server.")
	fmt.Fprintln(w, "  logout  Remove the stored OAuth token for a server.")
	fmt.Fprintln(w, "  status  Print whether a server has a usable stored OAuth token.")
	fmt.Fprintln(w)
	fmt.Fprintln(w, "Auth types:")
	fmt.Fprintln(w, "  oauth2       Browser PKCE and device-code OAuth flows.")
	fmt.Fprintln(w, "  codex_oauth  OpenAI Codex ChatGPT subscription device-code login.")
	fmt.Fprintln(w)
	fmt.Fprintln(w, "Examples:")
	fmt.Fprintln(w, "  harness-mcp-proxy auth login remote")
	fmt.Fprintln(w, "  harness-mcp-proxy auth status remote")
	fmt.Fprintln(w)
	fmt.Fprintln(w, "Flags:")
	printAuthFlags(w)
}

func usageAuthAction(w io.Writer, action string) {
	switch action {
	case "login":
		fmt.Fprintln(w, "harness-mcp-proxy auth login — sign in a configured HTTP downstream server.")
		fmt.Fprintln(w)
		fmt.Fprintln(w, "Usage:")
		fmt.Fprintln(w, "  harness-mcp-proxy auth login [-config path] <server>")
		fmt.Fprintln(w)
		fmt.Fprintln(w, "Starts the server's configured OAuth flow and stores the token under the")
		fmt.Fprintln(w, "MCP proxy config directory. oauth2 servers may use browser PKCE or")
		fmt.Fprintln(w, "device-code login. codex_oauth servers use OpenAI Codex's ChatGPT")
		fmt.Fprintln(w, "subscription device-code flow.")
		fmt.Fprintln(w)
		fmt.Fprintln(w, "Example:")
		fmt.Fprintln(w, "  harness-mcp-proxy auth login remote")
	case "logout":
		fmt.Fprintln(w, "harness-mcp-proxy auth logout — remove a configured server's OAuth token.")
		fmt.Fprintln(w)
		fmt.Fprintln(w, "Usage:")
		fmt.Fprintln(w, "  harness-mcp-proxy auth logout [-config path] <server>")
	case "status":
		fmt.Fprintln(w, "harness-mcp-proxy auth status — inspect a configured server's OAuth token.")
		fmt.Fprintln(w)
		fmt.Fprintln(w, "Usage:")
		fmt.Fprintln(w, "  harness-mcp-proxy auth status [-config path] <server>")
	}
	fmt.Fprintln(w)
	fmt.Fprintln(w, "Flags:")
	printAuthFlags(w)
}

func printAuthFlags(w io.Writer) {
	fs := flag.NewFlagSet("auth", flag.ContinueOnError)
	fs.SetOutput(w)
	fs.String("config", "", "config file path")
	fs.PrintDefaults()
}
