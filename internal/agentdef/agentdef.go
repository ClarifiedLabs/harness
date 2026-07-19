// Package agentdef defines named agent definitions: bundles of an allowed-tool
// set, an optional model target override, and extra system-prompt instructions.
// Four built-ins ship with the harness (auto, explore, plan, independent); config-file
// entries field-level merge onto them (an omitted field keeps the built-in
// value) or define new agents. The agent prompt is appended to the system prompt
// as a final section; the tool list is realized by main via tools.Registry.Subset,
// which gates both what the model sees and what dispatches.
package agentdef

import (
	"fmt"
	"maps"
	"slices"
	"strings"

	"harness/internal/tools"
	"harness/prompts"
)

// Default is the agent used when none is specified anywhere.
const Default = "auto"

// Definition is one resolved agent definition. AllowedTools is always explicit after
// Builtins/Resolve (never empty), so callers need no nil special case. The
// struct is deliberately small; future per-agent knobs (e.g. max_turns) can be
// added alongside without changing the merge contract.
type Definition struct {
	Name string
	// Description is required parent-facing selection metadata explaining when
	// this agent should be used.
	Description  string
	AllowedTools []string
	MCPTools     MCPToolsMode
	Prompt       string
	// Model is a complete provider-qualified model-proxy target id. Empty
	// inherits the current session target.
	Model string
	// Reasoning pins this agent's thinking effort (provider/model dependent,
	// e.g. low/medium/high). Empty inherits the session default. The value is
	// validated against the live model at switch time, not here.
	Reasoning string
}

// MCPToolsMode controls which discovered MCP tools are exposed to an agent.
// It affects automatic MCP tool exposure; explicit allowed_tools entries are
// still validated against the runtime catalog like any other tool name.
type MCPToolsMode string

const (
	MCPToolsDisabled MCPToolsMode = "disabled"
	MCPToolsReadOnly MCPToolsMode = "read_only"
	MCPToolsAll      MCPToolsMode = "all"
)

// FileDefinition mirrors one entry of the config file's "agents" object. Empty fields
// drive the field-level merge: they inherit from the same-named built-in, or
// for new agents from the defaults (default tool set, no prompt). Description
// is required parent-facing "when to use" metadata for new agents; a built-in
// override may omit it and inherit the built-in description.
type FileDefinition struct {
	Description  string   `json:"description"`
	AllowedTools []string `json:"allowed_tools"`
	MCPTools     string   `json:"mcp_tools"`
	Prompt       string   `json:"prompt"`
	Model        string   `json:"model"`
	Reasoning    string   `json:"reasoning"`
}

type Options struct {
	SearchTools string
}

// Builtins returns fresh copies of the four built-in agents keyed by name.
func Builtins() map[string]Definition {
	return BuiltinsWithOptions(Options{})
}

func BuiltinsWithOptions(opts Options) map[string]Definition {
	explorePrompt, _ := prompts.BuiltinAgentPrompt("explore")
	independentPrompt, _ := prompts.BuiltinAgentPrompt("independent")
	planPrompt, _ := prompts.BuiltinAgentPrompt("plan")
	return map[string]Definition{
		"auto": {
			Name:         "auto",
			Description:  "Default agent; the model decides what to do.",
			AllowedTools: defaultTools(opts),
			MCPTools:     MCPToolsAll,
		},
		"explore": {
			Name:         "explore",
			Description:  "Use proactively for broad search, architecture or dependency tracing, root-cause investigation, and questions spanning many files; not for a single known-file lookup.",
			AllowedTools: inspectionTools(opts),
			MCPTools:     MCPToolsReadOnly,
			Prompt:       explorePrompt,
		},
		"independent": {
			Name:         "independent",
			Description:  "Complete the task end to end without pausing for input.",
			AllowedTools: defaultTools(opts),
			MCPTools:     MCPToolsAll,
			Prompt:       independentPrompt,
		},
		"plan": {
			Name:         "plan",
			Description:  "Collaborate on an implementation plan without modifying the project.",
			AllowedTools: planTools(opts),
			MCPTools:     MCPToolsReadOnly,
			Prompt:       planPrompt,
		},
	}
}

func inspectionTools(opts Options) []string {
	names := []string{"read_file", "list_dir", "glob"}
	names = append(names, searchToolNames(opts.SearchTools)...)
	names = append(names, "web_fetch")
	if tools.GitAvailable() {
		names = append(names, "git_readonly")
	}
	return names
}

func planTools(opts Options) []string {
	return append(inspectionTools(opts), "write_tmp_file", "record_plan", "request_implementation", "update_todos", "delegate", "background_jobs")
}

func searchToolNames(mode string) []string {
	names := tools.DefaultNamesWithOptions(tools.Options{SearchTools: mode})
	var out []string
	for _, name := range names {
		if name == "grep" || name == "rg" {
			out = append(out, name)
		}
	}
	return out
}

func defaultTools(opts Options) []string {
	names := tools.DefaultNamesWithOptions(tools.Options{SearchTools: opts.SearchTools})
	if tools.GitAvailable() && !slices.Contains(names, "git_readonly") {
		names = append(names, "git_readonly")
	}
	return append(names, "record_plan", "update_todos", "delegate", "background_jobs")
}

// DefaultTools returns the default allowed-tool set (the built-in tool names
// plus delegate) that auto/independent and any config agent without an explicit
// allowed_tools list inherit. main uses it to detect default-inheriting agents
// when extending them with discovered MCP tools.
func DefaultTools() []string { return defaultTools(Options{}) }

// Resolve merges config-file agent entries onto the built-ins and returns the
// full agent set. Merge is field-level: a non-empty field replaces, an empty
// field inherits (from the built-in of the same name, or from the defaults for
// a new agent).
func Resolve(file map[string]FileDefinition) map[string]Definition {
	return ResolveWithOptions(file, Options{})
}

func ResolveWithOptions(file map[string]FileDefinition, opts Options) map[string]Definition {
	agents := BuiltinsWithOptions(opts)
	for name, fm := range file {
		a, ok := agents[name]
		if !ok {
			a = Definition{Name: name, AllowedTools: defaultTools(opts), MCPTools: MCPToolsAll}
		}
		allowedOverride := len(fm.AllowedTools) > 0
		if fm.Description != "" {
			a.Description = fm.Description
		}
		if allowedOverride {
			a.AllowedTools = slices.Clone(fm.AllowedTools)
		}
		if fm.MCPTools != "" {
			mode, err := ParseMCPToolsMode(fm.MCPTools)
			if err == nil {
				a.MCPTools = mode
			} else {
				a.MCPTools = MCPToolsMode(fm.MCPTools)
			}
		} else if allowedOverride {
			// An explicit allowed_tools list is a whitelist. Preserve the historical
			// behavior that whitelists opt out of automatic MCP tools unless the
			// agent also opts back in with mcp_tools.
			a.MCPTools = MCPToolsDisabled
		}
		if fm.Prompt != "" {
			a.Prompt = fm.Prompt
		}
		if fm.Model != "" {
			a.Model = fm.Model
		}
		if fm.Reasoning != "" {
			a.Reasoning = fm.Reasoning
		}
		agents[name] = a
	}
	return agents
}

// ParseMCPToolsMode canonicalizes a config string. The documented values are
// disabled, read_only, and all; read-only/readonly are accepted as ergonomic
// aliases for read_only.
func ParseMCPToolsMode(s string) (MCPToolsMode, error) {
	value := strings.ToLower(strings.TrimSpace(s))
	value = strings.ReplaceAll(value, "-", "_")
	switch value {
	case string(MCPToolsDisabled):
		return MCPToolsDisabled, nil
	case string(MCPToolsReadOnly), "readonly":
		return MCPToolsReadOnly, nil
	case string(MCPToolsAll):
		return MCPToolsAll, nil
	default:
		return "", fmt.Errorf("invalid mcp_tools %q (want disabled, read_only, or all)", s)
	}
}

// Validate reports invalid resolved agent definitions. Resolve keeps invalid
// mcp_tools strings in place so callers that can return contextual errors (main,
// --show-config) can fail fast after all field-level merging is done.
func Validate(agents map[string]Definition) error {
	for _, name := range Names(agents) {
		if strings.TrimSpace(agents[name].Description) == "" {
			return fmt.Errorf("agent %q: description must state when the parent should use it", name)
		}
		if _, err := ParseMCPToolsMode(string(agents[name].MCPTools)); err != nil {
			return fmt.Errorf("agent %q: %w", name, err)
		}
	}
	return nil
}

// Names returns the agent names in sorted order, for listing and error text.
func Names(agents map[string]Definition) []string {
	return slices.Sorted(maps.Keys(agents))
}
