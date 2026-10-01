package main

import (
	"fmt"
	"strings"

	"harness/internal/cli"
	"harness/internal/configmeta"
	modelconfig "harness/internal/modelproxy/config"
)

func runConfigList(env environment, invocation cli.Invocation) int {
	format := strings.ToLower(strings.TrimSpace(cliLast(invocation.Flags, "format", "text")))
	if format != "text" && format != "json" && format != "markdown" {
		fmt.Fprintln(env.stderr, "harness-model-proxy: config list: -format must be text, json, or markdown")
		return exitUsage
	}
	err := configmeta.WriteReference(env.stdout, modelconfig.Catalog(), format)
	if err != nil {
		fmt.Fprintf(env.stderr, "harness-model-proxy: config list: %v\n", err)
		return exitRuntime
	}
	return exitOK
}

func runConfigShow(env environment, invocation cli.Invocation) int {
	result, err := loadModelConfig(env, invocation)
	if err != nil {
		fmt.Fprintf(env.stderr, "harness-model-proxy: config show: %v\n", err)
		return exitUsage
	}
	snapshot := modelconfig.Snapshot(result)
	includeSources := cliBool(invocation.Flags, "sources")
	format := strings.ToLower(strings.TrimSpace(cliLast(invocation.Flags, "format", "text")))
	switch format {
	case "text":
		err = configmeta.WriteSnapshotText(env.stdout, modelconfig.Catalog(), snapshot, includeSources)
	case "json":
		err = configmeta.WriteSnapshotJSON(env.stdout, modelconfig.Catalog(), snapshot, includeSources)
	default:
		fmt.Fprintln(env.stderr, "harness-model-proxy: config show: -format must be text or json")
		return exitUsage
	}
	if err != nil {
		fmt.Fprintf(env.stderr, "harness-model-proxy: config show: %v\n", err)
		return exitRuntime
	}
	return exitOK
}

func runConfigCheck(env environment, invocation cli.Invocation) int {
	result, err := loadModelConfig(env, invocation)
	if err != nil {
		fmt.Fprintf(env.stderr, "harness-model-proxy: config check: %v\n", err)
		return exitUsage
	}
	if err := modelconfig.CheckLocalReferences(result, env.getenv, func(warning string) {
		fmt.Fprintf(env.stderr, "harness-model-proxy: %s\n", warning)
	}); err != nil {
		fmt.Fprintf(env.stderr, "harness-model-proxy: config check: %v\n", err)
		return exitUsage
	}
	if result.Path == "" {
		fmt.Fprintln(env.stdout, "config ok: no config file (resolved defaults and environment)")
	} else {
		fmt.Fprintf(env.stdout, "config ok: %s\n", result.Path)
	}
	return exitOK
}

func loadModelConfig(env environment, invocation cli.Invocation) (modelconfig.Result, error) {
	return modelconfig.Load(modelconfig.LoadOptions{
		Getenv: env.getenv,
		Flags:  invocation.Flags,
	})
}
