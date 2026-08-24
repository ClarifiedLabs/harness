package config

import (
	"strconv"

	"harness/internal/cli"
	"harness/internal/configmeta"
)

var invocationCLIFlags = []cli.Flag{
	{ID: "help", Names: []string{"h", "help"}, Kind: cli.BoolFlag, Description: "show help and exit"},
	{ID: "version", Names: []string{"version"}, Kind: cli.BoolFlag, Description: "print release version and exit"},
	{ID: "config", Names: []string{"config"}, Kind: cli.ValueFlag, ValueName: "path", Description: "alternate config path"},
	{ID: "prompt", Names: []string{"p"}, Kind: cli.ValueFlag, ValueName: "prompt", Description: "one-shot prompt"},
	{ID: "initial_prompt", Names: []string{"i", "initial-prompt"}, Kind: cli.ValueFlag, ValueName: "prompt", Description: "initial interactive prompt"},
	{ID: "image", Names: []string{"image"}, Kind: cli.ValueFlag, ValueName: "detail:path", Description: "attach an image; optionally detail:path", Repeatable: true},
	{ID: "resume", Names: []string{"resume"}, Kind: cli.ValueFlag, ValueName: "path", Description: "load a session transcript and continue"},
	{ID: "session", Names: []string{"session"}, Kind: cli.ValueFlag, ValueName: "path", Description: "explicit session save path"},
	{ID: "quiet", Names: []string{"q", "quiet"}, Kind: cli.BoolFlag, Description: "suppress status messages and reasoning output"},
	{ID: "format", Names: []string{"format"}, Kind: cli.ValueFlag, ValueName: "format", Description: "output format: text or json", Default: "text"},
	{ID: "debug_request", Names: []string{"debug-request"}, Kind: cli.BoolFlag, Description: "dump the first model request and exit"},
	{ID: "show_agents", Names: []string{"agents"}, Kind: cli.BoolFlag, Description: "list configured agents and exit"},
	{ID: "show_models", Names: []string{"models"}, Kind: cli.BoolFlag, Description: "list configured models and exit"},
	{ID: "check_model_proxy", Names: []string{"check-model-proxy"}, Kind: cli.BoolFlag, Description: "check model proxy reachability and exit"},
	{ID: "candidate_lineage", Names: []string{"candidate-lineage"}, Kind: cli.BoolFlag, Description: "preserve strictly improving accepted candidates for this Git session"},
}

// SettingCLIFlags projects every catalog-backed command-line setting exactly
// once. The hooks projection is intentionally a setting flag even though its
// value selects an invocation-time hook file: it overrides the source-resolved
// hooks and hook_configs settings and is accepted by config show.
func SettingCLIFlags() []cli.Flag {
	var flags []cli.Flag
	for _, parameter := range parameterCatalog.Parameters() {
		if len(parameter.Flags) == 0 {
			continue
		}
		kind := cli.ValueFlag
		valueName := parameter.Type
		description := parameter.Description
		defaultValue := configmeta.FormatDefault(parameter.Default)
		if parameter.Type == "boolean" {
			kind = cli.BoolFlag
			valueName = ""
			if value, ok := parameter.Default.Value.(bool); ok {
				defaultValue = strconv.FormatBool(value)
			} else {
				defaultValue = "false"
			}
			if parameter.Default.Note != "" {
				description += " " + parameter.Default.Note + "."
			}
		}
		flags = append(flags, cli.Flag{
			ID:          parameter.Key,
			Names:       append([]string(nil), parameter.Flags...),
			Kind:        kind,
			ValueName:   valueName,
			Description: description,
			Default:     defaultValue,
			Environment: append([]string(nil), parameter.Environment...),
		})
	}
	return flags
}

// CLIFlags returns the complete root harness flag projection: invocation-only
// controls followed by source-resolved setting controls.
func CLIFlags() []cli.Flag {
	flags := cloneCLIFlags(invocationCLIFlags)
	flags = append(flags, SettingCLIFlags()...)
	return flags
}

// LookupCLIFlag looks up one logical root flag by ID.
func LookupCLIFlag(id string) (cli.Flag, bool) {
	for _, flag := range CLIFlags() {
		if flag.ID == id {
			return cloneCLIFlag(flag), true
		}
	}
	return cli.Flag{}, false
}

func cloneCLIFlags(flags []cli.Flag) []cli.Flag {
	out := make([]cli.Flag, len(flags))
	for i, flag := range flags {
		out[i] = cloneCLIFlag(flag)
	}
	return out
}

func cloneCLIFlag(flag cli.Flag) cli.Flag {
	flag.Names = append([]string(nil), flag.Names...)
	flag.Environment = append([]string(nil), flag.Environment...)
	return flag
}
