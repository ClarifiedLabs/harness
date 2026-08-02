// Package agentdef defines named agent definitions: bundles of an allowed-tool
// set, an optional model target override, and extra system-prompt instructions.
// Five built-ins ship with the harness (auto, explore, plan, review,
// independent); config-file entries field-level merge onto them (an omitted
// field keeps the built-in value) or define new agents. The agent prompt is
// appended to the system prompt as a final section; the tool list is realized
// by main via tools.Registry.Subset, which gates both what the model sees and
// what dispatches.
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
	// WorkspaceAccess is the default lease access for background delegation.
	WorkspaceAccess string
	Prompt          string
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

const (
	WorkspaceAccessReadOnly  = tools.BackgroundAccessReadOnly
	WorkspaceAccessExclusive = tools.BackgroundAccessExclusive
)

// FileDefinition mirrors one entry of the config file's "agents" object. Empty fields
// drive the field-level merge: they inherit from the same-named built-in, or
// for new agents from the defaults (default tool set, no prompt). Description
// is required parent-facing "when to use" metadata for new agents; a built-in
// override may omit it and inherit the built-in description.
type FileDefinition struct {
	Description     string   `json:"description"`
	AllowedTools    []string `json:"allowed_tools"`
	MCPTools        string   `json:"mcp_tools"`
	WorkspaceAccess string   `json:"workspace_access"`
	Prompt          string   `json:"prompt"`
	Model           string   `json:"model"`
	Reasoning       string   `json:"reasoning"`
}

// Builtins returns fresh copies of the five built-in agents keyed by name.
func Builtins() map[string]Definition {
	explorePrompt, _ := prompts.BuiltinAgentPrompt("explore")
	independentPrompt, _ := prompts.BuiltinAgentPrompt("independent")
	planPrompt, _ := prompts.BuiltinAgentPrompt("plan")
	reviewPrompt, _ := prompts.BuiltinAgentPrompt("review")
	return map[string]Definition{
		"auto": {
			Name:            "auto",
			Description:     "General-purpose agent.",
			AllowedTools:    defaultTools(),
			MCPTools:        MCPToolsAll,
			WorkspaceAccess: WorkspaceAccessExclusive,
		},
		"explore": {
			Name:            "explore",
			Description:     "Broad read-only search, tracing, and root-cause analysis; not known-file lookup.",
			AllowedTools:    inspectionTools(),
			MCPTools:        MCPToolsReadOnly,
			WorkspaceAccess: WorkspaceAccessReadOnly,
			Prompt:          explorePrompt,
		},
		"independent": {
			Name:            "independent",
			Description:     "End-to-end work without user input.",
			AllowedTools:    defaultTools(),
			MCPTools:        MCPToolsAll,
			WorkspaceAccess: WorkspaceAccessExclusive,
			Prompt:          independentPrompt,
		},
		"plan": {
			Name:            "plan",
			Description:     "Collaborative implementation planning; explores freely (including running commands) but does not modify the project.",
			AllowedTools:    planTools(),
			MCPTools:        MCPToolsReadOnly,
			WorkspaceAccess: WorkspaceAccessReadOnly,
			Prompt:          planPrompt,
		},
		"review": {
			Name:            "review",
			Description:     "Findings-first review of a concrete code change; read-only.",
			AllowedTools:    inspectionTools(),
			MCPTools:        MCPToolsReadOnly,
			WorkspaceAccess: WorkspaceAccessReadOnly,
			Prompt:          reviewPrompt,
		},
	}
}

func inspectionTools() []string {
	names := []string{"read_file", "view_image", "list_dir", "glob", "search", "inspect", "update_todos"}
	// run_command widens exploration (gh, builds, screenshots, live apps) for the
	// read-only agents (explore, plan, review). None has first-class file-mutation
	// tools (edit, write_file, apply_patch), so "don't modify the project" stays
	// a prompt-level contract, not an enforced gate.
	names = append(names, "run_command", "web_fetch")
	if tools.GitAvailable() {
		names = append(names, "git_readonly")
	}
	return names
}

func planTools() []string {
	// run_command comes from the shared inspection set; plan adds no first-class
	// file-mutation tools (edit, write_file, apply_patch), so "don't modify the
	// project" stays a prompt-level contract (prompts/agents/plan.txt).
	return append(inspectionTools(), "write_tmp_file", "record_plan", "request_implementation", "delegate", "background_jobs")
}

func defaultTools() []string {
	// git already covers every git_readonly operation, so the default set omits
	// git_readonly to avoid advertising duplicate functionality. Read-only
	// agents (explore, plan, review) that require git_readonly remain delegatable
	// from here because delegate.MissingTools treats an available git as
	// satisfying a required git_readonly.
	names := tools.DefaultNames()
	return append(names, "update_todos", "record_plan", "create_goal", "update_goal", "delegate", "background_jobs")
}

// DefaultTools returns the default allowed-tool set that auto/independent and
// any config agent without an explicit allowed_tools list inherit. main uses it
// to detect default-inheriting agents when extending them with discovered MCP
// tools.
func DefaultTools() []string { return defaultTools() }

// Resolve merges config-file agent entries onto the built-ins and returns the
// full agent set. Merge is field-level: a non-empty field replaces, an empty
// field inherits (from the built-in of the same name, or from the defaults for
// a new agent).
func Resolve(file map[string]FileDefinition) map[string]Definition {
	agents := Builtins()
	for name, fm := range file {
		a, ok := agents[name]
		if !ok {
			a = Definition{Name: name, AllowedTools: defaultTools(), MCPTools: MCPToolsAll, WorkspaceAccess: WorkspaceAccessExclusive}
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
		if fm.WorkspaceAccess != "" {
			access, err := ParseWorkspaceAccess(fm.WorkspaceAccess)
			if err == nil {
				a.WorkspaceAccess = access
			} else {
				a.WorkspaceAccess = fm.WorkspaceAccess
			}
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

func ParseWorkspaceAccess(s string) (string, error) {
	s = strings.ToLower(strings.TrimSpace(s))
	switch s {
	case WorkspaceAccessReadOnly, "readonly":
		return WorkspaceAccessReadOnly, nil
	case WorkspaceAccessExclusive:
		return WorkspaceAccessExclusive, nil
	default:
		return "", fmt.Errorf("invalid workspace_access %q (want read_only or exclusive)", s)
	}
}

// Validate reports invalid resolved agent definitions. Resolve keeps invalid
// mcp_tools strings in place so callers that can return contextual errors (main,
// config show/check) can fail fast after all field-level merging is done.
func Validate(agents map[string]Definition) error {
	for _, name := range Names(agents) {
		if strings.TrimSpace(agents[name].Description) == "" {
			return fmt.Errorf("agent %q: description must state when the parent should use it", name)
		}
		if _, err := ParseMCPToolsMode(string(agents[name].MCPTools)); err != nil {
			return fmt.Errorf("agent %q: %w", name, err)
		}
		if _, err := ParseWorkspaceAccess(agents[name].WorkspaceAccess); err != nil {
			return fmt.Errorf("agent %q: %w", name, err)
		}
	}
	return nil
}

// Names returns the agent names in sorted order, for listing and error text.
func Names(agents map[string]Definition) []string {
	return slices.Sorted(maps.Keys(agents))
}
