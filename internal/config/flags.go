package config

import (
	"fmt"
	"strings"

	"harness/internal/cli"
)

type flagOccurrence struct{ name, value string }

type flagState struct {
	settings   map[string][]flagOccurrence
	invocation map[string][]flagOccurrence
}

func newParsedFlagState(values cli.Values) *flagState {
	state := &flagState{settings: make(map[string][]flagOccurrence), invocation: make(map[string][]flagOccurrence)}
	for _, value := range values.Occurrences() {
		occurrence := flagOccurrence{name: value.Name, value: value.Value}
		if _, setting := parameterCatalog.Lookup(value.ID); setting {
			if value.ID == "hooks" {
				state.invocation["hooks_override"] = append(state.invocation["hooks_override"], occurrence)
			} else {
				state.settings[value.ID] = append(state.settings[value.ID], occurrence)
			}
			continue
		}
		if _, known := LookupCLIFlag(value.ID); known {
			state.invocation[value.ID] = append(state.invocation[value.ID], occurrence)
		}
	}
	return state
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
