package main

import (
	"fmt"
	"strconv"

	"harness/internal/buildinfo"
	"harness/internal/cli"
	modelconfig "harness/internal/modelproxy/config"
)

type commandHandler func(environment, cli.Invocation) int

var commandHandlers = map[string]commandHandler{
	"root":             runRoot,
	"serve":            runServe,
	"setup":            runSetupCmd,
	"refresh-models":   runRefreshModelsCmd,
	"auth.login":       runAuthAction,
	"auth.logout":      runAuthAction,
	"auth.status":      runAuthAction,
	"generate-api-key": runGenerateAPIKeyCmd,
	"version":          runVersion,
	"config.list":      runConfigList,
	"config.show":      runConfigShow,
	"config.check":     runConfigCheck,
}

var commandCatalog = cli.MustCatalog(cli.Command{
	ID:           "root",
	Name:         "harness-model-proxy",
	Summary:      "Provider and model proxy for Harness.",
	Description:  "harness-model-proxy owns provider configuration, API keys, model metadata, and concrete provider calls for Harness. With no arguments, it serves HTTP.",
	Runnable:     true,
	DefaultChild: "serve",
	Args:         cli.Args{Max: -1, Check: false},
	Commands: []cli.Command{
		{
			ID:          "serve",
			Name:        "serve",
			Summary:     "Load config and serve the HTTP model proxy (default).",
			Description: "Loads the selected top-level configuration and provider files, then serves the model-proxy HTTP API.",
			Runnable:    true,
			Args:        cli.Args{Max: -1, Check: false},
			Flags: append(modelconfig.CLIFlags(),
				valueCLIFlag("listen", []string{"listen"}, "addr", "HTTP listen address", defaultListen)),
		},
		{
			ID:          "setup",
			Name:        "setup",
			Summary:     "Create or update proxy and provider config interactively.",
			Description: "Runs the models.dev-backed provider/model picker and writes proxy and provider config files in the default config directory.",
			Runnable:    true,
			Args:        cli.Args{Max: -1, Check: false},
			Flags: []cli.Flag{
				boolCLIFlag("force", []string{"force"}, "overwrite existing provider files"),
				mustModelConfigCLIFlag("models_dev_cache_ttl"),
			},
		},
		{
			ID:       "refresh-models",
			Name:     "refresh-models",
			Summary:  "Fetch models.dev and update configured provider model metadata.",
			Runnable: true,
			Args:     cli.Args{Max: -1, Check: false},
			Flags: []cli.Flag{
				mustModelConfigCLIFlag("config"),
				mustModelConfigCLIFlag("models_dev_cache_ttl"),
			},
		},
		{
			ID:          "auth",
			Name:        "auth",
			Summary:     "Login, logout, or inspect OAuth tokens for a configured provider.",
			Description: "Manages OAuth tokens for configured providers. Supported provider auth types include oauth2 browser/device flows and codex_oauth for OpenAI Codex ChatGPT subscriptions.",
			Examples: []string{
				"harness-model-proxy auth login openai-codex",
				"harness-model-proxy auth status openai-codex",
			},
			Commands: []cli.Command{
				authCommand("auth.login", "login", "Start OAuth login for a configured provider.", "Signs in with the provider's configured OAuth flow. oauth2 supports browser PKCE and device-code login; codex_oauth uses the OpenAI Codex ChatGPT subscription device-code flow."),
				authCommand("auth.logout", "logout", "Remove the stored OAuth token for a provider.", "Removes the configured provider's stored OAuth token."),
				authCommand("auth.status", "status", "Print whether a provider has a usable stored OAuth token.", "Inspects the configured provider's stored OAuth token without changing it."),
			},
		},
		{
			ID:          "generate-api-key",
			Name:        "generate-api-key",
			Summary:     "Generate a new API key and add it to the key file.",
			Description: "Writes the dedicated API-key file; it does not create or mutate the normal config.",
			Runnable:    true,
			Args:        exactArgs(1, "<name>"),
			Flags: []cli.Flag{
				mustModelConfigCLIFlag("config"),
				mustModelConfigCLIFlag("api_keys_file"),
				valueCLIFlag("ttl", []string{"ttl"}, "duration", "key TTL as a Go duration; empty or 0 means no expiry", ""),
				valueCLIFlag("budget_usd", []string{"budget-usd"}, "amount", "per-key cost budget in USD; 0 means no budget", "0"),
				valueCLIFlag("budget_period", []string{"budget-period"}, "duration", "per-key cost budget period; required when -budget-usd is set", ""),
				boolCLIFlag("budget_reject_unpriced", []string{"budget-reject-unpriced"}, "reject unpriced targets while this key's budget is enabled"),
			},
		},
		{ID: "version", Name: "version", Summary: "Print the release version.", Runnable: true, Args: cli.Args{Max: -1, Check: false}},
		{
			ID: "config", Name: "config", Summary: "Inspect and validate model-proxy configuration.",
			Commands: []cli.Command{
				{ID: "config.list", Name: "list", Summary: "List configuration metadata.", Runnable: true, Args: exactArgs(0, ""), Flags: []cli.Flag{valueCLIFlag("format", []string{"format"}, "format", "output format: text, json, or markdown", "text")}},
				{ID: "config.show", Name: "show", Summary: "Show resolved safe top-level configuration.", Runnable: true, Args: exactArgs(0, ""), Flags: configShowCLIFlags()},
				{ID: "config.check", Name: "check", Summary: "Validate the selected top-level configuration.", Runnable: true, Args: exactArgs(0, ""), Flags: []cli.Flag{mustModelConfigCLIFlag("config")}},
			},
		},
	},
})

func authCommand(id, name, summary, description string) cli.Command {
	return cli.Command{
		ID: id, Name: name, Summary: summary, Description: description, Runnable: true,
		Args: exactArgs(1, "<provider>"), Flags: []cli.Flag{mustModelConfigCLIFlag("config")},
		Examples: []string{"harness-model-proxy auth " + name + " openai-codex"},
	}
}

func configShowCLIFlags() []cli.Flag {
	flags := append([]cli.Flag(nil), modelconfig.CLIFlags()...)
	return append(flags,
		valueCLIFlag("format", []string{"format"}, "format", "output format: text or json", "text"),
		boolCLIFlag("sources", []string{"sources"}, "include each setting's winning source"),
	)
}

func mustModelConfigCLIFlag(id string) cli.Flag {
	for _, flag := range modelconfig.CLIFlags() {
		if flag.ID == id {
			return flag
		}
	}
	panic("missing model-proxy config CLI flag " + id)
}

func exactArgs(count int, usage string) cli.Args {
	return cli.Args{Usage: usage, Min: count, Max: count, Check: true}
}

func valueCLIFlag(id string, names []string, valueName, description, defaultValue string) cli.Flag {
	return cli.Flag{ID: id, Names: names, Kind: cli.ValueFlag, ValueName: valueName, Description: description, Default: defaultValue}
}

func boolCLIFlag(id string, names []string, description string) cli.Flag {
	return cli.Flag{ID: id, Names: names, Kind: cli.BoolFlag, Description: description}
}

func cliLast(values cli.Values, id, fallback string) string {
	if value, ok := values.Last(id); ok {
		return value
	}
	return fallback
}

func cliBool(values cli.Values, id string) bool {
	value, ok := values.Last(id)
	if !ok {
		return false
	}
	parsed, _ := strconv.ParseBool(value)
	return parsed
}

func normalizeLegacyCommandArgs(args []string) []string {
	if len(args) > 1 && (args[0] == "auth" || args[0] == "config") && args[1] == "help" {
		return append([]string{args[0], "--help"}, args[2:]...)
	}
	return args
}

func run(env environment) int {
	if len(env.args) > 0 {
		switch env.args[0] {
		case "-h", "--help", "help":
			if err := commandCatalog.WriteHelp(env.stdout, "root"); err != nil {
				fmt.Fprintf(env.stderr, "harness-model-proxy: help: %v\n", err)
				return exitRuntime
			}
			return exitOK
		case "--version", "version":
			return runVersion(env, cli.Invocation{})
		}
		if env.args[0] != "" && env.args[0][0] == '-' {
			fmt.Fprintf(env.stderr, "harness-model-proxy: unknown subcommand %q\n", env.args[0])
			_ = commandCatalog.WriteHelp(env.stderr, "root")
			return exitUsage
		}
	}
	invocation, err := commandCatalog.Parse(normalizeLegacyCommandArgs(env.args))
	if err != nil {
		fmt.Fprintf(env.stderr, "harness-model-proxy: %v\n", err)
		if invocation.CommandID != "" {
			_ = commandCatalog.WriteHelp(env.stderr, invocation.CommandID)
		}
		return exitUsage
	}
	if invocation.Action == cli.Help {
		if err := commandCatalog.WriteHelp(env.stdout, invocation.CommandID); err != nil {
			fmt.Fprintf(env.stderr, "harness-model-proxy: help: %v\n", err)
			return exitRuntime
		}
		return exitOK
	}
	handler, ok := commandHandlers[invocation.CommandID]
	if !ok {
		fmt.Fprintf(env.stderr, "harness-model-proxy: command %q has no handler\n", invocation.CommandID)
		return exitRuntime
	}
	return handler(env, invocation)
}

func runRoot(env environment, invocation cli.Invocation) int {
	if len(invocation.Args) > 0 {
		fmt.Fprintf(env.stderr, "harness-model-proxy: unknown subcommand %q\n", invocation.Args[0])
		_ = commandCatalog.WriteHelp(env.stderr, "root")
		return exitUsage
	}
	_ = commandCatalog.WriteHelp(env.stdout, "root")
	return exitOK
}

func runVersion(env environment, _ cli.Invocation) int {
	fmt.Fprintln(env.stdout, buildinfo.Line("harness-model-proxy"))
	return exitOK
}
