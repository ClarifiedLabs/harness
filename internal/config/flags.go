package config

import (
	"flag"
	"fmt"
	"io"
	"strings"

	"harness/internal/configmeta"
)

type flagOccurrence struct{ name, value string }

type flagState struct {
	set        *flag.FlagSet
	settings   map[string][]flagOccurrence
	invocation map[string][]flagOccurrence
}

type trackedFlag struct {
	state   *flagState
	key     string
	name    string
	boolean bool
	setting bool
}

func (value *trackedFlag) String() string   { return "" }
func (value *trackedFlag) IsBoolFlag() bool { return value.boolean }
func (value *trackedFlag) Set(raw string) error {
	occurrence := flagOccurrence{name: value.name, value: raw}
	if value.setting {
		value.state.settings[value.key] = append(value.state.settings[value.key], occurrence)
	} else {
		value.state.invocation[value.key] = append(value.state.invocation[value.key], occurrence)
	}
	return nil
}

func newFlagState() *flagState {
	state := &flagState{settings: make(map[string][]flagOccurrence), invocation: make(map[string][]flagOccurrence)}
	state.set = flag.NewFlagSet("harness", flag.ContinueOnError)
	state.set.SetOutput(io.Discard)
	for _, definition := range allDefinitions {
		definition.register(state)
	}

	state.addInvocationFlag("h", "help", "show help and exit", true)
	state.addInvocationFlag("help", "help", "show help and exit", true)
	state.addInvocationFlag("version", "version", "print release version and exit", true)
	state.addInvocationFlag("config", "config", "alternate config path", false)
	state.addInvocationFlag("p", "prompt", "one-shot prompt", false)
	state.addInvocationFlag("i", "initial_prompt", "initial interactive prompt", false)
	state.addInvocationFlag("initial-prompt", "initial_prompt", "initial interactive prompt", false)
	state.addInvocationFlag("image", "image", "attach an image; repeatable; optionally detail:path", false)
	state.addInvocationFlag("resume", "resume", "load a session transcript and continue", false)
	state.addInvocationFlag("session", "session", "explicit session save path", false)
	state.addInvocationFlag("q", "quiet", "suppress status messages and reasoning output", true)
	state.addInvocationFlag("quiet", "quiet", "suppress status messages and reasoning output", true)
	state.addInvocationFlag("format", "format", "output format: text or json", false)
	state.addInvocationFlag("debug-request", "debug_request", "dump the first model request and exit", true)
	state.addInvocationFlag("agents", "show_agents", "list configured agents and exit", true)
	state.addInvocationFlag("models", "show_models", "list configured models and exit", true)
	state.addInvocationFlag("check-model-proxy", "check_model_proxy", "check model proxy reachability and exit", true)
	state.addInvocationFlag("hooks", "hooks_override", "override hook config file for this run", false)
	annotateSettingFlags(state.set, parameterCatalog)
	return state
}

func (state *flagState) addSettingFlag(name, key, description string, boolean bool) {
	state.set.Var(&trackedFlag{state: state, key: key, name: name, boolean: boolean, setting: true}, name, description)
}

func annotateSettingFlags(set *flag.FlagSet, catalog configmeta.Catalog) {
	for _, parameter := range catalog.Parameters() {
		for _, name := range parameter.Flags {
			settingFlag := set.Lookup(name)
			if settingFlag == nil {
				continue
			}
			if len(parameter.Environment) > 0 {
				settingFlag.Usage += " (env: " + strings.Join(parameter.Environment, ", ") + ")"
			}
			settingFlag.DefValue = configmeta.FormatDefault(parameter.Default)
		}
	}
}
func (state *flagState) addInvocationFlag(name, key, description string, boolean bool) {
	state.set.Var(&trackedFlag{state: state, key: key, name: name, boolean: boolean}, name, description)
}
func (state *flagState) lastInvocation(key string) (flagOccurrence, bool) {
	values := state.invocation[key]
	if len(values) == 0 {
		return flagOccurrence{}, false
	}
	return values[len(values)-1], true
}

func parseInvocationBool(state *flagState, key string) (bool, bool, error) {
	values := state.invocation[key]
	if len(values) == 0 {
		return false, false, nil
	}
	var result bool
	for _, value := range values {
		parsed, err := parseBool(value.value)
		if err != nil {
			return false, true, fmt.Errorf("flag --%s: %w", value.name, err)
		}
		result = parsed
	}
	return result, true, nil
}

func resolveMetaRunOptions(state *flagState) (RunOptions, error) {
	var options RunOptions
	var err error
	if options.Help, _, err = parseInvocationBool(state, "help"); err != nil {
		return RunOptions{}, err
	}
	if options.Version, _, err = parseInvocationBool(state, "version"); err != nil {
		return RunOptions{}, err
	}
	return options, nil
}

func resolveRunOptions(context *resolveContext) error {
	state := context.flags
	if value, ok := state.lastInvocation("resume"); ok {
		context.result.Run.Resume = value.value
	} else if value, present := context.lookup("HARNESS_RESUME"); present {
		context.result.Run.Resume = value
	}
	if value, ok := state.lastInvocation("session"); ok {
		context.result.Run.Session = value.value
	} else if value, present := context.lookup("HARNESS_SESSION"); present {
		context.result.Run.Session = value
	}
	if value, ok := state.lastInvocation("prompt"); ok {
		context.result.Run.Prompt = value.value
		context.result.Run.PromptSet = true
	}
	if value, ok := state.lastInvocation("initial_prompt"); ok {
		context.result.Run.InitialPrompt = value.value
		context.result.Run.InitialPromptSet = true
	}
	if context.result.Run.PromptSet && context.result.Run.InitialPromptSet {
		return fmt.Errorf("-p cannot be combined with -i or -initial-prompt")
	}
	if context.result.Run.InitialPromptSet && context.result.Run.InitialPrompt == "-" {
		return fmt.Errorf("-i does not read from stdin; pass prompt text directly")
	}
	for _, value := range state.invocation["image"] {
		attachment, err := parseImageAttachment(value.value, context.result.Config.ImageDetail)
		if err != nil {
			return err
		}
		context.result.Run.Images = append(context.result.Run.Images, attachment)
	}
	quiet, _, err := parseInvocationBool(state, "quiet")
	if err != nil {
		return err
	}
	context.result.Run.Quiet = quiet
	format := "text"
	if value, ok := state.lastInvocation("format"); ok {
		format = strings.ToLower(strings.TrimSpace(value.value))
	}
	if format != "text" && format != "json" {
		return fmt.Errorf("--format must be text or json")
	}
	context.result.Run.OutputFormat = format
	if context.result.Run.DebugRequest, _, err = parseInvocationBool(state, "debug_request"); err != nil {
		return err
	}
	if context.result.Run.ShowAgents, _, err = parseInvocationBool(state, "show_agents"); err != nil {
		return err
	}
	if context.result.Run.ShowModels, _, err = parseInvocationBool(state, "show_models"); err != nil {
		return err
	}
	if context.result.Run.CheckModelProxy, _, err = parseInvocationBool(state, "check_model_proxy"); err != nil {
		return err
	}
	return nil
}

// Usage renders root help from the same generated flag set used by Load.
func Usage(w io.Writer) {
	fmt.Fprintln(w, "harness — a minimal agentic coding harness.")
	fmt.Fprintln(w, "\nUsage:\n  harness [flags]\n  harness config <list|show|check> [flags]\n\nFlags:")
	state := newFlagState()
	state.set.SetOutput(w)
	state.set.PrintDefaults()
}
