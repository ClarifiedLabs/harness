package main

import (
	"fmt"
	"strconv"

	"harness/internal/cli"
	"harness/internal/config"
	"harness/internal/lspproxy"
	"harness/internal/ui"
)

type commandHandler func(environment, cli.Invocation) int

var commandHandlers = map[string]commandHandler{
	"root":            runRoot,
	"config.list":     runConfigList,
	"config.show":     runConfigShow,
	"config.check":    runConfigCheck,
	"session.replay":  runSessionReplay,
	"session.timings": runSessionTimings,
	"session.stats":   runSessionStats,
	"session.errors":  runSessionErrors,
	"session.analyze": runSessionAnalyze,
	"lsp.serve":       runLSPServe,
	"lsp.version":     runLSPVersion,
}

func commandCatalog(env environment) cli.Catalog {
	showFlags := append([]cli.Flag{
		mustConfigCLIFlag("config"),
		mustConfigCLIFlag("format"),
		boolCLIFlag("sources", []string{"sources"}, "include each setting's winning source"),
	}, config.SettingCLIFlags()...)
	getenv := env.getenv
	if getenv == nil {
		getenv = func(string) string { return "" }
	}
	return cli.MustCatalog(cli.Command{
		ID:          "root",
		Name:        "harness",
		Summary:     "A minimal agentic coding harness.",
		Description: "Harness is a terminal-first, provider-neutral tool-using agent loop.",
		Runnable:    true,
		Flags:       config.CLIFlags(),
		Args:        cli.Args{Max: -1, Check: false},
		Commands: []cli.Command{
			{
				ID: "config", Name: "config", Summary: "Inspect and validate Harness configuration.",
				Commands: []cli.Command{
					{ID: "config.list", Name: "list", Summary: "List configuration metadata.", Runnable: true, Args: exactArgs(0, ""), Flags: []cli.Flag{valueCLIFlag("format", []string{"format"}, "format", "output format: text, json, or markdown", "text")}},
					{ID: "config.show", Name: "show", Summary: "Show resolved, safely redacted configuration.", Runnable: true, Args: exactArgs(0, ""), Flags: showFlags},
					{ID: "config.check", Name: "check", Summary: "Strictly validate configuration and local references.", Runnable: true, Args: exactArgs(0, ""), Flags: []cli.Flag{mustConfigCLIFlag("config")}},
				},
			},
			{
				ID: "session", Name: "session", Summary: "Inspect recorded sessions.",
				Commands: []cli.Command{
					{ID: "session.replay", Name: "replay", Summary: "Replay a recorded session.", Runnable: true, Args: exactArgs(1, "<session-dir>"), Flags: []cli.Flag{
						boolCLIFlag("follow", []string{"f", "follow"}, "follow appended replay events"),
						boolCLIFlag("quiet", []string{"q", "quiet"}, "suppress replay status lines"),
						valueCLIFlag("color_theme", []string{"color-theme"}, "theme", "syntax and displayed diff color theme: dark or light", config.ColorThemeDark),
						mustConfigCLIFlag("config"),
					}},
					{ID: "session.timings", Name: "timings", Summary: "Show session timing details.", Runnable: true, Args: exactArgs(1, "<session-dir>")},
					{ID: "session.stats", Name: "stats", Summary: "Show session statistics.", Runnable: true, Args: exactArgs(1, "<session-dir>"), Flags: []cli.Flag{valueCLIFlag("format", []string{"format"}, "format", "output format: text or json", "text")}},
					{ID: "session.errors", Name: "errors", Summary: "Show classified tool and model failures.", Runnable: true, Args: optionalDirArgs(), Flags: sessionErrorCLIFlags()},
					{ID: "session.analyze", Name: "analyze", Summary: "Analyze one session or recent session history.", Runnable: true, Args: optionalDirArgs(), Flags: []cli.Flag{
						valueCLIFlag("since", []string{"since"}, "duration", "when scanning, include sessions created within this duration", "24h"),
						boolCLIFlag("all", []string{"all"}, "scan all sessions, ignoring --since"),
						valueCLIFlag("before", []string{"before"}, "RFC3339", "include only events at or before this timestamp", ""),
						valueCLIFlag("format", []string{"format"}, "format", "output format: text or json", "text"),
					}},
				},
			},
			{
				ID: "lsp", Name: "lsp", Summary: "Run the generic LSP-to-MCP shim.",
				Commands: []cli.Command{
					{ID: "lsp.serve", Name: "serve", Summary: "Serve LSP navigation tools over MCP on stdin/stdout.", Runnable: true, Args: cli.Args{Max: -1, Check: false}, Flags: []cli.Flag{
						valueCLIFlag("config", []string{"config"}, "path", "config file path", lspproxy.DefaultConfigPath(getenv)),
						valueCLIFlag("namespace", []string{"namespace"}, "namespace", "tool-name namespace; empty for bare names", "lsp"),
						valueCLIFlag("log", []string{"log"}, "path", "log file path", ""),
						valueCLIFlag("log_level", []string{"log-level"}, "level", "log level: debug|info|warn|error", "info"),
						valueCLIFlag("log_format", []string{"log-format"}, "format", "log format: json|text", "json"),
					}},
					{ID: "lsp.version", Name: "version", Summary: "Print the release and MCP protocol versions.", Runnable: true, Args: cli.Args{Max: -1, Check: false}},
				},
			},
		},
	})
}

func run(env environment) int {
	catalog := commandCatalog(env)
	args := normalizeLegacyCommandArgs(env.args)
	invocation, err := catalog.Parse(args)
	if err != nil {
		fmt.Fprintf(env.stderr, "harness: %v\n", err)
		if invocation.CommandID != "" {
			_ = catalog.WriteHelp(env.stderr, invocation.CommandID)
		}
		return ui.ExitUsage
	}
	if invocation.Action == cli.Help {
		if err := catalog.WriteHelp(env.stdout, invocation.CommandID); err != nil {
			fmt.Fprintf(env.stderr, "harness: help: %v\n", err)
			return ui.ExitRuntime
		}
		return ui.ExitOK
	}
	handler, ok := commandHandlers[invocation.CommandID]
	if !ok {
		fmt.Fprintf(env.stderr, "harness: command %q has no handler\n", invocation.CommandID)
		return ui.ExitRuntime
	}
	return handler(env, invocation)
}

func normalizeLegacyCommandArgs(args []string) []string {
	if len(args) >= 2 && (args[0] == "config" || args[0] == "lsp") && args[1] == "help" {
		return append([]string{args[0], "--help"}, args[2:]...)
	}
	if len(args) >= 2 && args[0] == "lsp" && args[1] == "--version" {
		out := append([]string{"lsp", "version"}, args[2:]...)
		return out
	}
	return args
}

func exactArgs(count int, usage string) cli.Args {
	return cli.Args{Usage: usage, Min: count, Max: count, Check: true}
}

func optionalDirArgs() cli.Args {
	return cli.Args{Usage: "[dir]", Min: 0, Max: 1, Check: true}
}

func valueCLIFlag(id string, names []string, valueName, description, defaultValue string) cli.Flag {
	return cli.Flag{ID: id, Names: names, Kind: cli.ValueFlag, ValueName: valueName, Description: description, Default: defaultValue}
}

func boolCLIFlag(id string, names []string, description string) cli.Flag {
	return cli.Flag{ID: id, Names: names, Kind: cli.BoolFlag, Description: description}
}

func mustConfigCLIFlag(id string) cli.Flag {
	flag, ok := config.LookupCLIFlag(id)
	if !ok {
		panic("missing config CLI flag " + id)
	}
	return flag
}

func sessionErrorCLIFlags() []cli.Flag {
	return []cli.Flag{
		valueCLIFlag("tool", []string{"tool"}, "tool", "only failures from this tool", ""),
		valueCLIFlag("kind", []string{"kind"}, "kind", "only failures of this error kind", ""),
		valueCLIFlag("model", []string{"model"}, "model", "only failures attributed to this model", ""),
		valueCLIFlag("agent", []string{"agent"}, "agent", "only failures attributed to this agent", ""),
		valueCLIFlag("since", []string{"since"}, "duration", "when scanning, include sessions created within this duration", "24h"),
		boolCLIFlag("all", []string{"all"}, "scan all sessions, ignoring --since"),
		valueCLIFlag("before", []string{"before"}, "RFC3339", "include only events at or before this timestamp", ""),
		valueCLIFlag("format", []string{"format"}, "format", "output format: text or json", "text"),
	}
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
