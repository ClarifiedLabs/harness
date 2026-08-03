package main

import (
	"fmt"
	"strings"

	"harness/internal/cli"
	"harness/internal/config"
	"harness/internal/configmeta"
	"harness/internal/ui"
)

func runConfigList(env environment, invocation cli.Invocation) int {
	format := strings.ToLower(strings.TrimSpace(cliLast(invocation.Flags, "format", "text")))
	var err error
	switch format {
	case "text":
		err = configmeta.WriteText(env.stdout, config.Catalog())
	case "json":
		err = configmeta.WriteJSON(env.stdout, config.Catalog())
	case "markdown":
		err = configmeta.WriteMarkdown(env.stdout, config.Catalog())
	default:
		fmt.Fprintln(env.stderr, "harness: config list: -format must be text, json, or markdown")
		return ui.ExitUsage
	}
	if err != nil {
		fmt.Fprintf(env.stderr, "harness: config list: %v\n", err)
		return ui.ExitRuntime
	}
	return ui.ExitOK
}

func runConfigShow(env environment, invocation cli.Invocation) int {
	result, err := config.LoadParsed(harnessLoadOptions(env, nil), invocation.Flags)
	if err != nil {
		fmt.Fprintf(env.stderr, "harness: config show: %v\n", err)
		return ui.ExitUsage
	}
	includeSources := cliBool(invocation.Flags, "sources")
	switch result.Run.OutputFormat {
	case "text":
		err = config.WriteProjectionText(env.stdout, result, includeSources)
	case "json":
		err = config.WriteProjectionJSON(env.stdout, result, includeSources)
	default:
		err = fmt.Errorf("unsupported format %q", result.Run.OutputFormat)
	}
	if err != nil {
		fmt.Fprintf(env.stderr, "harness: config show: %v\n", err)
		return ui.ExitRuntime
	}
	return ui.ExitOK
}

func runConfigCheck(env environment, invocation cli.Invocation) int {
	result, err := config.LoadParsed(harnessLoadOptions(env, nil), invocation.Flags)
	if err != nil {
		fmt.Fprintf(env.stderr, "harness: config check: %v\n", err)
		return ui.ExitUsage
	}
	if err := validateLocalConfig(result); err != nil {
		fmt.Fprintf(env.stderr, "harness: config check: %v\n", err)
		return ui.ExitUsage
	}
	if result.ConfigPath == "" {
		fmt.Fprintln(env.stdout, "config ok: no config file (resolved defaults and environment)")
	} else {
		fmt.Fprintf(env.stdout, "config ok: %s\n", result.ConfigPath)
	}
	return ui.ExitOK
}

func validateLocalConfig(result config.Result) error {
	if err := result.ValidateFileReferences(func(value string) error {
		_, err := resolveAtFile(value)
		return err
	}); err != nil {
		return err
	}
	cfg := result.Config
	if _, err := resolveAtFile(cfg.SystemPrompt); err != nil {
		return fmt.Errorf("system_prompt: %w", err)
	}
	agents, err := resolveConfiguredAgents(cfg)
	if err != nil {
		return err
	}
	for name, definition := range agents {
		if _, err := resolveAtFile(definition.Prompt); err != nil {
			return fmt.Errorf("agent %q prompt: %w", name, err)
		}
	}
	return nil
}
