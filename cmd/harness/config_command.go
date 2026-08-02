package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"strconv"
	"strings"

	"harness/internal/config"
	"harness/internal/configmeta"
	"harness/internal/ui"
)

const (
	configUsage      = "usage: harness config <list|show|check> [flags]"
	configListUsage  = "usage: harness config list [-format text|json|markdown]"
	configShowUsage  = "usage: harness config show [-config path] [-format text|json] [-sources] [config-setting flags]"
	configCheckUsage = "usage: harness config check [-config path]"
)

func runConfigCommand(env environment, args []string) int {
	if len(args) == 0 {
		fmt.Fprintln(env.stderr, configUsage)
		return ui.ExitUsage
	}
	switch args[0] {
	case "list":
		return runConfigList(env, args[1:])
	case "show":
		return runConfigShow(env, args[1:])
	case "check":
		return runConfigCheck(env, args[1:])
	case "-h", "--help", "help":
		fmt.Fprintln(env.stdout, configUsage)
		return ui.ExitOK
	default:
		fmt.Fprintf(env.stderr, "harness: unknown config command %q\n", args[0])
		fmt.Fprintln(env.stderr, configUsage)
		return ui.ExitUsage
	}
}

func runConfigList(env environment, args []string) int {
	fs := flag.NewFlagSet("config list", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	format := fs.String("format", "text", "output format: text, json, or markdown")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			fmt.Fprintln(env.stdout, configListUsage)
			return ui.ExitOK
		}
		fmt.Fprintf(env.stderr, "harness: config list: %v\n", err)
		fmt.Fprintln(env.stderr, configListUsage)
		return ui.ExitUsage
	}
	if fs.NArg() != 0 {
		fmt.Fprintln(env.stderr, configListUsage)
		return ui.ExitUsage
	}

	var err error
	switch strings.ToLower(strings.TrimSpace(*format)) {
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

func runConfigShow(env environment, args []string) int {
	loadArgs, includeSources, help, err := parseConfigShowArgs(args, config.Catalog())
	if help {
		fmt.Fprintln(env.stdout, configShowUsage)
		return ui.ExitOK
	}
	if err != nil {
		fmt.Fprintf(env.stderr, "harness: config show: %v\n", err)
		fmt.Fprintln(env.stderr, configShowUsage)
		return ui.ExitUsage
	}
	result, err := config.Load(harnessLoadOptions(env, loadArgs))
	if err != nil {
		fmt.Fprintf(env.stderr, "harness: config show: %v\n", err)
		return ui.ExitUsage
	}

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

func runConfigCheck(env environment, args []string) int {
	fs := flag.NewFlagSet("config check", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	configPath := fs.String("config", "", "alternate config path")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			fmt.Fprintln(env.stdout, configCheckUsage)
			return ui.ExitOK
		}
		fmt.Fprintf(env.stderr, "harness: config check: %v\n", err)
		fmt.Fprintln(env.stderr, configCheckUsage)
		return ui.ExitUsage
	}
	if fs.NArg() != 0 {
		fmt.Fprintln(env.stderr, configCheckUsage)
		return ui.ExitUsage
	}
	var loadArgs []string
	configPathSet := false
	fs.Visit(func(f *flag.Flag) {
		if f.Name == "config" {
			configPathSet = true
		}
	})
	if configPathSet {
		loadArgs = append(loadArgs, "--config", *configPath)
	}
	result, err := config.Load(harnessLoadOptions(env, loadArgs))
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

// parseConfigShowArgs restricts config show to catalog-backed setting flags and
// its three command controls. The actual setting parsing and precedence remain
// owned by config.Load.
func parseConfigShowArgs(args []string, catalog configmeta.Catalog) (loadArgs []string, includeSources, help bool, err error) {
	type showFlag struct{ boolean bool }
	allowed := map[string]showFlag{
		"config": {boolean: false},
		"format": {boolean: false},
		"hooks":  {boolean: false},
	}
	for _, parameter := range catalog.Parameters() {
		for _, name := range parameter.Flags {
			allowed[name] = showFlag{boolean: parameter.Type == "boolean"}
		}
	}

	for index := 0; index < len(args); index++ {
		argument := args[index]
		if argument == "--" {
			if index+1 < len(args) {
				return nil, false, false, fmt.Errorf("unexpected argument %q", args[index+1])
			}
			break
		}
		if argument == "-h" || argument == "--help" {
			return nil, false, true, nil
		}
		if !strings.HasPrefix(argument, "-") || argument == "-" {
			return nil, false, false, fmt.Errorf("unexpected argument %q", argument)
		}
		nameValue := strings.TrimLeft(argument, "-")
		name, value, hasValue := strings.Cut(nameValue, "=")
		if name == "sources" {
			if !hasValue {
				includeSources = true
				continue
			}
			parsed, parseErr := strconv.ParseBool(value)
			if parseErr != nil {
				return nil, false, false, fmt.Errorf("flag --sources: must be true or false")
			}
			includeSources = parsed
			continue
		}
		definition, ok := allowed[name]
		if !ok {
			return nil, false, false, fmt.Errorf("flag provided but not defined: -%s", name)
		}
		loadArgs = append(loadArgs, argument)
		if !hasValue && !definition.boolean {
			if index+1 >= len(args) {
				return nil, false, false, fmt.Errorf("flag needs an argument: -%s", name)
			}
			index++
			loadArgs = append(loadArgs, args[index])
		}
	}
	return loadArgs, includeSources, false, nil
}
