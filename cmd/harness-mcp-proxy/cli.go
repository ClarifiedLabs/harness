package main

import (
	"errors"
	"fmt"
	"strconv"

	"harness/internal/cli"
	"harness/internal/mcpproxy"
)

type commandHandler func(environment, cli.Invocation) int

var commandHandlers = map[string]commandHandler{
	"serve":            handleServe,
	"tools":            handleTools,
	"auth.login":       handleAuthAction,
	"auth.logout":      handleAuthAction,
	"auth.status":      handleAuthAction,
	"generate-api-key": handleGenerateAPIKey,
	"config.list":      runConfigList,
	"config.show":      runConfigShow,
	"config.check":     runConfigCheck,
	"version":          runVersion,
}

var commandCatalog = cli.MustCatalog(cli.Command{
	ID:          "root",
	Name:        "harness-mcp-proxy",
	Summary:     "MCP proxy daemon and debug client.",
	Description: "Run and inspect the Harness MCP proxy, manage downstream OAuth credentials, and inspect proxy configuration.",
	Commands: []cli.Command{
		{
			ID:          "serve",
			Name:        "serve",
			Summary:     "Run the MCP proxy daemon.",
			Description: "Load configured downstream MCP servers and expose their merged tools over streamable HTTP or stdio.",
			Flags: appendCLIFlags(mcpproxy.CLIFlags(), cli.Flag{
				ID: "stdio", Names: []string{"stdio"}, Kind: cli.BoolFlag,
				Description: "serve MCP over stdin/stdout instead of HTTP; API keys and metrics are inert",
				Default:     "false",
			}),
			Runnable: true,
		},
		{
			ID:          "tools",
			Name:        "tools",
			Summary:     "List the aggregated tools exposed by a running proxy.",
			Description: "Connect to a running HTTP proxy and print its aggregated tool table.",
			Flags: appendCLIFlags(selectMCPFlags("config"),
				cli.Flag{ID: "proxy", Names: []string{"proxy"}, Kind: cli.ValueFlag, ValueName: "url", Description: "HTTP proxy URL"},
				cli.Flag{ID: "api-key", Names: []string{"api-key"}, Kind: cli.ValueFlag, ValueName: "key", Description: "API key for the proxy", Environment: []string{"HARNESS_MCP_PROXY_API_KEY"}},
			),
			Runnable: true,
		},
		{
			ID:          "auth",
			Name:        "auth",
			Summary:     "Manage OAuth tokens for configured HTTP downstream servers.",
			Description: "Manage oauth2 and codex_oauth credentials for configured HTTP downstream servers.",
			Examples:    []string{"harness-mcp-proxy auth login remote", "harness-mcp-proxy auth status remote"},
			Commands: []cli.Command{
				authCommand("auth.login", "login", "Sign in a configured HTTP downstream server."),
				authCommand("auth.logout", "logout", "Remove a configured server's stored OAuth token."),
				authCommand("auth.status", "status", "Inspect a configured server's OAuth token."),
			},
		},
		{
			ID:          "generate-api-key",
			Name:        "generate-api-key",
			Summary:     "Generate and store a new proxy API key.",
			Description: "Writes the dedicated API-key file with a generated proxy-level API key hash; it does not create or mutate the normal config.",
			Flags: appendCLIFlags(selectMCPFlags("config", "api_keys_file"), cli.Flag{
				ID: "ttl", Names: []string{"ttl"}, Kind: cli.ValueFlag, ValueName: "duration",
				Description: "key TTL as a Go duration; empty or 0 means no expiry",
			}),
			Args:     cli.Args{Usage: "<name>", Min: 1, Max: 1, Check: true},
			Runnable: true,
		},
		{
			ID:          "config",
			Name:        "config",
			Summary:     "Inspect and validate MCP proxy configuration.",
			Description: "Offline, non-mutating configuration reference, resolution, and validation commands.",
			Commands: []cli.Command{
				{
					ID: "config.list", Name: "list", Summary: "List supported configuration settings.",
					Flags: []cli.Flag{formatFlag("text, json, or markdown")}, Runnable: true,
				},
				{
					ID: "config.show", Name: "show", Summary: "Show safely resolved configuration values.",
					Flags: appendCLIFlags(mcpproxy.CLIFlags(),
						formatFlag("text or json"),
						cli.Flag{ID: "sources", Names: []string{"sources"}, Kind: cli.BoolFlag, Description: "include setting provenance", Default: "false"},
					),
					Runnable: true,
				},
				{
					ID: "config.check", Name: "check", Summary: "Load and validate the selected configuration.",
					Flags: selectMCPFlags("config"), Runnable: true,
				},
			},
		},
		{ID: "version", Name: "version", Summary: "Print the release version and MCP protocol revision.", Runnable: true},
	},
})

func authCommand(id, name, summary string) cli.Command {
	description := summary + " OAuth token files are managed beneath the MCP proxy config directory."
	if name == "login" {
		description += " oauth2 supports browser PKCE and device-code login; codex_oauth uses the OpenAI Codex ChatGPT subscription device-code flow."
	}
	return cli.Command{
		ID: id, Name: name, Summary: summary,
		Description: description,
		Flags:       selectMCPFlags("config"),
		Args:        cli.Args{Usage: "<server>", Min: 1, Max: 1, Check: true},
		Examples:    []string{"harness-mcp-proxy auth " + name + " remote"},
		Runnable:    true,
	}
}

func formatFlag(description string) cli.Flag {
	return cli.Flag{ID: "format", Names: []string{"format"}, Kind: cli.ValueFlag, ValueName: "format", Description: description, Default: "text"}
}

func selectMCPFlags(ids ...string) []cli.Flag {
	wanted := make(map[string]struct{}, len(ids))
	for _, id := range ids {
		wanted[id] = struct{}{}
	}
	var selected []cli.Flag
	for _, declaration := range mcpproxy.CLIFlags() {
		if _, ok := wanted[declaration.ID]; ok {
			selected = append(selected, declaration)
		}
	}
	return selected
}

func appendCLIFlags(base []cli.Flag, additional ...cli.Flag) []cli.Flag {
	out := append([]cli.Flag(nil), base...)
	return append(out, additional...)
}

func normalizeCLIArgs(args []string) []string {
	if len(args) == 0 {
		return nil
	}
	if len(args) >= 2 && args[0] == "auth" && args[1] == "help" {
		return []string{"auth", "--help"}
	}
	return append([]string(nil), args...)
}

func commandCatalogForEnvironment(getenv func(string) string) cli.Catalog {
	if getenv == nil {
		getenv = func(string) string { return "" }
	}
	root := commandCatalog.Root()
	var applyDefaults func(*cli.Command)
	applyDefaults = func(command *cli.Command) {
		for i := range command.Flags {
			switch command.Flags[i].ID {
			case "config":
				command.Flags[i].Default = mcpproxy.DefaultConfigPath(getenv)
			case "proxy":
				command.Flags[i].Default = mcpproxy.DefaultURL()
			}
		}
		for i := range command.Commands {
			applyDefaults(&command.Commands[i])
		}
	}
	applyDefaults(&root)
	return cli.MustCatalog(root)
}

func writeCommandHelp(env environment, commandID string, stdout bool) int {
	writer := env.stderr
	if stdout {
		writer = env.stdout
	}
	if err := commandCatalogForEnvironment(env.getenv).WriteHelp(writer, commandID); err != nil {
		fmt.Fprintf(env.stderr, "harness-mcp-proxy: %v\n", err)
		return exitRuntime
	}
	return exitOK
}

func handleCLIError(env environment, args []string, invocation cli.Invocation, err error) int {
	var commandErr *cli.CommandError
	if errors.As(err, &commandErr) {
		switch commandErr.CommandID {
		case "root":
			if commandErr.Token != "" {
				fmt.Fprintf(env.stderr, "harness-mcp-proxy: unknown subcommand %q\n", commandErr.Token)
			}
			writeCommandHelp(env, "root", false)
		case "auth":
			if commandErr.Token == "" {
				fmt.Fprintln(env.stderr, "harness-mcp-proxy: auth requires login, logout, or status")
			} else {
				fmt.Fprintf(env.stderr, "harness-mcp-proxy: unknown auth command %q\n", commandErr.Token)
			}
			writeCommandHelp(env, "auth", false)
		case "config":
			if commandErr.Token == "" {
				fmt.Fprintln(env.stderr, "harness-mcp-proxy: config requires list, show, or check")
			} else {
				fmt.Fprintf(env.stderr, "harness-mcp-proxy: unknown config command %q\n", commandErr.Token)
			}
			writeCommandHelp(env, "config", false)
		default:
			fmt.Fprintf(env.stderr, "harness-mcp-proxy: %v\n", err)
		}
		return exitUsage
	}

	var argsErr *cli.ArgsError
	if errors.As(err, &argsErr) {
		switch argsErr.CommandID {
		case "generate-api-key":
			fmt.Fprintln(env.stderr, "harness-mcp-proxy: generate-api-key requires exactly one name")
		case "auth.login", "auth.logout", "auth.status":
			action := invocation.CommandPath[len(invocation.CommandPath)-1]
			fmt.Fprintf(env.stderr, "harness-mcp-proxy: auth %s requires exactly one server\n", action)
			writeCommandHelp(env, argsErr.CommandID, false)
		default:
			fmt.Fprintf(env.stderr, "harness-mcp-proxy: %v\n", err)
		}
		return exitUsage
	}

	var parseErr *cli.ParseError
	if errors.As(err, &parseErr) {
		fmt.Fprintf(env.stderr, "harness-mcp-proxy: %v\n", parseErr.Err)
		return exitUsage
	}
	fmt.Fprintf(env.stderr, "harness-mcp-proxy: %v\n", err)
	return exitUsage
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
