package agentdef

import (
	"slices"
	"testing"

	"harness/internal/tools"
)

func TestDefaultIsAuto(t *testing.T) {
	if Default != "auto" {
		t.Errorf("Default = %q, want \"auto\"", Default)
	}
}

func TestBuiltins(t *testing.T) {
	m := Builtins()
	if len(m) != 3 {
		t.Fatalf("want 3 builtin agents, got %d: %v", len(m), Names(m))
	}
	for name, a := range m {
		if a.Name != name {
			t.Errorf("agent %q has Name %q", name, a.Name)
		}
		if a.Description == "" {
			t.Errorf("agent %q has empty description", name)
		}
	}

	auto := m["auto"]
	if auto.Prompt != "" {
		t.Errorf("auto must have no extra prompt (current behavior), got %q", auto.Prompt)
	}
	if !slices.Equal(auto.AllowedTools, defaultTools(Options{})) {
		t.Errorf("auto tools = %v, want default set", auto.AllowedTools)
	}
	if auto.MCPTools != MCPToolsAll {
		t.Errorf("auto MCPTools = %q, want %q", auto.MCPTools, MCPToolsAll)
	}

	ind := m["independent"]
	if ind.Prompt == "" {
		t.Error("independent must carry a prompt")
	}
	if !slices.Equal(ind.AllowedTools, defaultTools(Options{})) {
		t.Errorf("independent tools = %v, want default set", ind.AllowedTools)
	}
	if ind.MCPTools != MCPToolsAll {
		t.Errorf("independent MCPTools = %q, want %q", ind.MCPTools, MCPToolsAll)
	}

	plan := m["plan"]
	if plan.Prompt == "" {
		t.Error("plan must carry a prompt")
	}
	wantPlan := planTools(Options{})
	if !slices.Equal(plan.AllowedTools, wantPlan) {
		t.Errorf("plan tools = %v, want %v", plan.AllowedTools, wantPlan)
	}
	if plan.MCPTools != MCPToolsReadOnly {
		t.Errorf("plan MCPTools = %q, want %q", plan.MCPTools, MCPToolsReadOnly)
	}
}

func TestResolveNilKeepsBuiltins(t *testing.T) {
	m := Resolve(nil)
	if !slices.Equal(Names(m), []string{"auto", "independent", "plan"}) {
		t.Errorf("Names = %v", Names(m))
	}
}

func TestPlanAgentOmitsGitReadonlyWhenGitMissing(t *testing.T) {
	t.Setenv("PATH", t.TempDir())

	plan := Builtins()["plan"]
	if slices.Contains(plan.AllowedTools, "git_readonly") {
		t.Fatalf("plan agent includes unavailable git_readonly: %v", plan.AllowedTools)
	}
}

// Field-level merge: overriding only the prompt keeps the built-in tool list.
func TestResolvePromptOnlyOverrideKeepsTools(t *testing.T) {
	m := Resolve(map[string]FileDefinition{"plan": {Prompt: "custom plan prompt"}})
	plan := m["plan"]
	if plan.Prompt != "custom plan prompt" {
		t.Errorf("Prompt = %q", plan.Prompt)
	}
	if !slices.Equal(plan.AllowedTools, Builtins()["plan"].AllowedTools) {
		t.Errorf("tool list not preserved: %v", plan.AllowedTools)
	}
	if plan.MCPTools != MCPToolsReadOnly {
		t.Errorf("mcp_tools not preserved: %q", plan.MCPTools)
	}
}

// Field-level merge: overriding only the tools keeps the built-in prompt.
func TestResolveToolsOnlyOverrideKeepsPrompt(t *testing.T) {
	m := Resolve(map[string]FileDefinition{"plan": {AllowedTools: []string{"read_file"}}})
	plan := m["plan"]
	if !slices.Equal(plan.AllowedTools, []string{"read_file"}) {
		t.Errorf("tools = %v", plan.AllowedTools)
	}
	if plan.Prompt != Builtins()["plan"].Prompt {
		t.Errorf("prompt not preserved: %q", plan.Prompt)
	}
	if plan.MCPTools != MCPToolsDisabled {
		t.Errorf("explicit allowed_tools should default mcp_tools to disabled, got %q", plan.MCPTools)
	}
}

func TestResolveMCPToolsOverride(t *testing.T) {
	m := Resolve(map[string]FileDefinition{
		"plan": {MCPTools: "all"},
		"ro":   {AllowedTools: []string{"read_file"}, MCPTools: "read-only"},
	})
	if m["plan"].MCPTools != MCPToolsAll {
		t.Errorf("plan MCPTools = %q, want all", m["plan"].MCPTools)
	}
	if m["ro"].MCPTools != MCPToolsReadOnly {
		t.Errorf("ro MCPTools = %q, want read_only", m["ro"].MCPTools)
	}
}

func TestResolveMetadataOverrideKeepsOtherFields(t *testing.T) {
	m := Resolve(map[string]FileDefinition{"plan": {
		Description: "custom desc",
		Provider:    "openai",
		Model:       "gpt-5.5",
	}})
	plan := m["plan"]
	if plan.Description != "custom desc" || plan.Provider != "openai" || plan.Model != "gpt-5.5" {
		t.Fatalf("metadata = description %q provider %q model %q", plan.Description, plan.Provider, plan.Model)
	}
	if plan.Prompt != Builtins()["plan"].Prompt {
		t.Errorf("prompt not preserved: %q", plan.Prompt)
	}
	if !slices.Equal(plan.AllowedTools, Builtins()["plan"].AllowedTools) {
		t.Errorf("tool list not preserved: %v", plan.AllowedTools)
	}
}

// A new agent with no allowed_tools inherits the default tool set.
func TestResolveNewAgentInheritsDefaultTools(t *testing.T) {
	m := Resolve(map[string]FileDefinition{"review": {Prompt: "review the diff"}})
	rev, ok := m["review"]
	if !ok {
		t.Fatal("new agent not resolved")
	}
	if rev.Name != "review" || rev.Prompt != "review the diff" {
		t.Errorf("rev = %+v", rev)
	}
	if !slices.Equal(rev.AllowedTools, defaultTools(Options{})) {
		t.Errorf("tools = %v, want default set", rev.AllowedTools)
	}
	if rev.MCPTools != MCPToolsAll {
		t.Errorf("MCPTools = %q, want all", rev.MCPTools)
	}
}

func TestBuiltinsWithSearchToolsOption(t *testing.T) {
	m := BuiltinsWithOptions(Options{SearchTools: tools.SearchToolsBoth})
	for _, name := range []string{"auto", "independent"} {
		if !slices.Contains(m[name].AllowedTools, "grep") {
			t.Fatalf("%s tools missing grep with search_tools=both: %v", name, m[name].AllowedTools)
		}
	}
	if !slices.Contains(m["plan"].AllowedTools, "grep") {
		t.Fatalf("plan tools missing grep with search_tools=both: %v", m["plan"].AllowedTools)
	}
}

func TestResolveNewAgentWithExplicitTools(t *testing.T) {
	m := Resolve(map[string]FileDefinition{"ro": {AllowedTools: []string{"read_file", "grep"}}})
	ro := m["ro"]
	if !slices.Equal(ro.AllowedTools, []string{"read_file", "grep"}) {
		t.Errorf("tools = %v", ro.AllowedTools)
	}
	if ro.Prompt != "" {
		t.Errorf("prompt = %q, want empty", ro.Prompt)
	}
	if ro.MCPTools != MCPToolsDisabled {
		t.Errorf("MCPTools = %q, want disabled", ro.MCPTools)
	}
}

func TestParseMCPToolsMode(t *testing.T) {
	for _, in := range []string{"read_only", "read-only", "readonly", " READ_ONLY "} {
		if got, err := ParseMCPToolsMode(in); err != nil || got != MCPToolsReadOnly {
			t.Errorf("ParseMCPToolsMode(%q) = %q, %v; want read_only", in, got, err)
		}
	}
	for _, in := range []string{"disabled", "all"} {
		if _, err := ParseMCPToolsMode(in); err != nil {
			t.Errorf("ParseMCPToolsMode(%q): %v", in, err)
		}
	}
	if _, err := ParseMCPToolsMode("bogus"); err == nil {
		t.Fatal("ParseMCPToolsMode(bogus) succeeded, want error")
	}
}

func TestValidateReportsInvalidMCPTools(t *testing.T) {
	m := Resolve(map[string]FileDefinition{"plan": {MCPTools: "bogus"}})
	if err := Validate(m); err == nil {
		t.Fatal("Validate succeeded for invalid mcp_tools")
	}
}

func TestNamesSorted(t *testing.T) {
	m := Resolve(map[string]FileDefinition{"zz": {}, "aa": {}})
	if got := Names(m); !slices.Equal(got, []string{"aa", "auto", "independent", "plan", "zz"}) {
		t.Errorf("Names = %v", got)
	}
}
