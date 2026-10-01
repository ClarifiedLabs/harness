package main

import (
	"fmt"
	"strings"

	"harness/internal/cli"
	"harness/internal/configmeta"
	"harness/internal/mcpproxy"
)

func runConfigList(env environment, invocation cli.Invocation) int {
	format := strings.ToLower(strings.TrimSpace(cliLast(invocation.Flags, "format", "text")))
	if format != "text" && format != "json" && format != "markdown" {
		fmt.Fprintln(env.stderr, "harness-mcp-proxy: config list: -format must be text, json, or markdown")
		return exitUsage
	}
	err := configmeta.WriteReference(env.stdout, mcpproxy.Catalog(), format)
	if err != nil {
		fmt.Fprintf(env.stderr, "harness-mcp-proxy: config list: %v\n", err)
		return exitRuntime
	}
	return exitOK
}

func runConfigShow(env environment, invocation cli.Invocation) int {
	result, err := mcpproxy.ResolveConfig(mcpproxy.ResolveOptions{Flags: invocation.Flags, Getenv: env.getenv})
	if err != nil {
		fmt.Fprintf(env.stderr, "harness-mcp-proxy: config show: %v\n", err)
		return exitUsage
	}
	writeConfigWarnings(env, result.Config.Warnings)
	snapshot := mcpproxy.Snapshot(result)
	includeSources := cliBool(invocation.Flags, "sources")
	format := strings.ToLower(strings.TrimSpace(cliLast(invocation.Flags, "format", "text")))
	switch format {
	case "text":
		err = configmeta.WriteSnapshotText(env.stdout, mcpproxy.Catalog(), snapshot, includeSources)
	case "json":
		err = configmeta.WriteSnapshotJSON(env.stdout, mcpproxy.Catalog(), snapshot, includeSources)
	default:
		fmt.Fprintln(env.stderr, "harness-mcp-proxy: config show: -format must be text or json")
		return exitUsage
	}
	if err != nil {
		fmt.Fprintf(env.stderr, "harness-mcp-proxy: config show: %v\n", err)
		return exitRuntime
	}
	return exitOK
}

func runConfigCheck(env environment, invocation cli.Invocation) int {
	result, err := mcpproxy.ResolveConfig(mcpproxy.ResolveOptions{Flags: invocation.Flags, Getenv: env.getenv})
	if err != nil {
		fmt.Fprintf(env.stderr, "harness-mcp-proxy: config check: %v\n", err)
		return exitUsage
	}
	writeConfigWarnings(env, result.Config.Warnings)
	if result.ConfigLoaded {
		fmt.Fprintf(env.stdout, "config ok: %s\n", result.Path)
	} else {
		fmt.Fprintln(env.stdout, "config ok: no config file (resolved defaults)")
	}
	return exitOK
}

func writeConfigWarnings(env environment, warnings []string) {
	for _, warning := range warnings {
		fmt.Fprintf(env.stderr, "harness-mcp-proxy: config warning: %s\n", warning)
	}
}
