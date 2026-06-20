package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"strings"
	"time"

	"harness/internal/mcp"
	"harness/internal/mcpproxy"
	"harness/internal/ui"
)

var toolsCommandTimeout = 10 * time.Second

// runTools connects to a running proxy as an MCP client and prints the
// aggregated tool table. It is a debug/status command: if it connects and
// lists, the proxy is up. It targets the HTTP proxy URL from -proxy,
// config proxy.listen, or the default listener.
func runTools(env environment, args []string) int {
	fs := flag.NewFlagSet("tools", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	// -config defaults to "" so a missing default path is non-fatal (resolved
	// below); an explicit -config typo still surfaces as a load error.
	configPath := fs.String("config", "", "config file path")
	proxy := fs.String("proxy", "", "HTTP proxy URL")
	apiKey := fs.String("api-key", "", "API key for the proxy (also HARNESS_MCP_PROXY_API_KEY)")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			usage(env.stdout, env.getenv)
			return exitOK
		}
		fmt.Fprintf(env.stderr, "harness-mcp-proxy: %v\n", err)
		return exitUsage
	}

	proxyURL, code := resolveToolsProxy(env, fs, *proxy, *configPath)
	if code != exitOK {
		return code
	}
	key := *apiKey
	if key == "" && env.getenv != nil {
		key = env.getenv("HARNESS_MCP_PROXY_API_KEY")
	}
	client := toolsClient(proxyURL, key)
	defer client.Close()

	commandCtx, stop, interrupted := signalCancelContext(env.sigCh)
	defer stop()
	ctx, cancel := context.WithTimeout(commandCtx, toolsCommandTimeout)
	defer cancel()
	if _, err := client.Initialize(ctx); err != nil {
		if interrupted() || errors.Is(err, context.Canceled) {
			return exitInterrupt
		}
		fmt.Fprintf(env.stderr, "harness-mcp-proxy: cannot connect to proxy at %s: %v\n", proxyURL, err)
		return exitRuntime
	}
	tools, err := client.ListTools(ctx)
	if err != nil {
		if interrupted() || errors.Is(err, context.Canceled) {
			return exitInterrupt
		}
		fmt.Fprintf(env.stderr, "harness-mcp-proxy: %v\n", err)
		return exitRuntime
	}
	if interrupted() {
		return exitInterrupt
	}

	printToolTable(env.stdout, tools)
	return exitOK
}

// toolsClient builds the HTTP MCP client for the tools command.
func toolsClient(proxyURL, apiKey string) *mcp.Client {
	info := mcp.Implementation{Name: "harness-mcp-proxy-tools", Version: "1"}
	opts := mcp.HTTPOptions{Endpoint: proxyURL}
	if apiKey != "" {
		opts.Headers = map[string]string{"Authorization": "Bearer " + apiKey}
	}
	tr := mcp.NewHTTPTransport(opts)
	return mcp.NewClientTransport(tr, mcp.ClientOptions{Info: info})
}

// resolveToolsProxy determines which HTTP URL the tools command connects to:
// the -proxy flag, else the config's proxy.listen, else the default URL. A
// config that fails to load is a runtime error (a typo'd -config should not
// silently fall back to the default URL). The returned code is exitOK on success.
func resolveToolsProxy(env environment, fs *flag.FlagSet, proxyFlag, configFlag string) (string, int) {
	if proxyFlag != "" {
		return proxyFlag, exitOK
	}
	configPath := resolveConfigPath(configFlag, flagWasSet(fs, "config"), env.getenv)
	cfg, err := mcpproxy.LoadConfig(configPath)
	if err != nil {
		fmt.Fprintf(env.stderr, "harness-mcp-proxy: %v\n", err)
		return "", exitRuntime
	}
	return mcpproxy.URLForListen(cfg.Listen), exitOK
}

// printToolTable writes an aligned name/description list preceded by a count
// header ("N tools" or "no tools"). Tools arrive sorted by name from the proxy,
// so no local sort is needed.
func printToolTable(w io.Writer, tools []mcp.Tool) {
	if len(tools) == 0 {
		fmt.Fprintln(w, "no tools")
		return
	}
	noun := "tools"
	if len(tools) == 1 {
		noun = "tool"
	}
	fmt.Fprintf(w, "%d %s\n", len(tools), noun)
	rows := make([]ui.NameDescription, 0, len(tools))
	for _, t := range tools {
		rows = append(rows, ui.NameDescription{Name: t.Name, Description: firstLine(t.Description)})
	}
	ui.WriteNameDescriptionList(w, rows, ui.NameDescriptionListOptions{})
}

// firstLine returns the first line of s with surrounding whitespace trimmed, so
// a multi-line description collapses to a single table cell.
func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	return strings.TrimSpace(s)
}
