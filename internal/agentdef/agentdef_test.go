package agentdef

import (
	"slices"
	"strings"
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
	if len(m) != 4 {
		t.Fatalf("want 4 builtin agents, got %d: %v", len(m), Names(m))
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

	explore := m["explore"]
	if explore.Prompt == "" {
		t.Error("explore must carry a prompt")
	}
	if !slices.Equal(explore.AllowedTools, inspectionTools(Options{})) {
		t.Errorf("explore tools = %v, want inspection set", explore.AllowedTools)
	}
	if explore.MCPTools != MCPToolsReadOnly {
		t.Errorf("explore MCPTools = %q, want %q", explore.MCPTools, MCPToolsReadOnly)
	}
	if !slices.Contains(explore.AllowedTools, "run_command") {
		t.Errorf("explore tools missing run_command: %v", explore.AllowedTools)
	}
	for _, forbidden := range []string{"write_file", "edit", "apply_patch", "record_plan", "request_implementation", "update_todos", "delegate", "background_jobs"} {
		if slices.Contains(explore.AllowedTools, forbidden) {
			t.Errorf("explore tools unexpectedly include %q: %v", forbidden, explore.AllowedTools)
		}
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
	if !slices.Contains(plan.AllowedTools, "run_command") {
		t.Errorf("plan tools missing run_command: %v", plan.AllowedTools)
	}
	for _, forbidden := range []string{"edit", "write_file", "apply_patch"} {
		if slices.Contains(plan.AllowedTools, forbidden) {
			t.Errorf("plan tools unexpectedly include file-mutation tool %q: %v", forbidden, plan.AllowedTools)
		}
	}
}

func TestResolveNilKeepsBuiltins(t *testing.T) {
	m := Resolve(nil)
	if !slices.Equal(Names(m), []string{"auto", "explore", "independent", "plan"}) {
		t.Errorf("Names = %v", Names(m))
	}
}

func TestInspectionAgentsIncludeGlob(t *testing.T) {
	for _, name := range []string{"explore", "plan"} {
		agent := Builtins()[name]
		if !slices.Contains(agent.AllowedTools, "glob") {
			t.Fatalf("%s agent tools missing glob: %v", name, agent.AllowedTools)
		}
	}
}

func TestInspectionAgentsOmitGitReadonlyWhenGitMissing(t *testing.T) {
	t.Setenv("PATH", t.TempDir())

	for _, name := range []string{"explore", "plan"} {
		agent := Builtins()[name]
		if slices.Contains(agent.AllowedTools, "git_readonly") {
			t.Fatalf("%s agent includes unavailable git_readonly: %v", name, agent.AllowedTools)
		}
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
		Model:       "openai:gpt-5.5",
	}})
	plan := m["plan"]
	if plan.Description != "custom desc" || plan.Model != "openai:gpt-5.5" {
		t.Fatalf("metadata = description %q model %q", plan.Description, plan.Model)
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
	for _, name := range []string{"explore", "plan"} {
		if !slices.Contains(m[name].AllowedTools, "grep") {
			t.Fatalf("%s tools missing grep with search_tools=both: %v", name, m[name].AllowedTools)
		}
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

func TestValidateRequiresParentFacingDescription(t *testing.T) {
	for _, description := range []string{"", " \t\n "} {
		m := Resolve(map[string]FileDefinition{"review": {Description: description}})
		err := Validate(m)
		if err == nil || !strings.Contains(err.Error(), `agent "review"`) || !strings.Contains(err.Error(), "description must state when the parent should use it") {
			t.Fatalf("Validate description %q error = %v", description, err)
		}
	}

	m := Resolve(map[string]FileDefinition{"review": {Description: "Use after implementation for an independent correctness review."}})
	if err := Validate(m); err != nil {
		t.Fatalf("Validate useful custom description: %v", err)
	}

	m = Resolve(map[string]FileDefinition{"explore": {Prompt: "custom exploration"}})
	if err := Validate(m); err != nil {
		t.Fatalf("built-in override should inherit description: %v", err)
	}
	if m["explore"].Description != Builtins()["explore"].Description {
		t.Fatalf("explore description = %q, want inherited built-in description", m["explore"].Description)
	}
}

func TestValidateReportsInvalidMCPTools(t *testing.T) {
	m := Resolve(map[string]FileDefinition{"plan": {MCPTools: "bogus"}})
	if err := Validate(m); err == nil {
		t.Fatal("Validate succeeded for invalid mcp_tools")
	}
}

func TestResolveReasoningOverride(t *testing.T) {
	m := Resolve(map[string]FileDefinition{
		"plan": {Reasoning: "high"},
		"fast": {Model: "cheap", Reasoning: "low"},
	})
	if m["plan"].Reasoning != "high" {
		t.Errorf("plan Reasoning = %q, want high", m["plan"].Reasoning)
	}
	if m["fast"].Reasoning != "low" || m["fast"].Model != "cheap" {
		t.Errorf("fast = reasoning %q model %q", m["fast"].Reasoning, m["fast"].Model)
	}
	// An omitted reasoning field inherits (built-in plan has none by default).
	if Builtins()["plan"].Reasoning != "" {
		t.Errorf("built-in plan should have no pinned reasoning, got %q", Builtins()["plan"].Reasoning)
	}
}

func TestDefaultToolsIncludeRecordPlanNotRequestImplementation(t *testing.T) {
	def := defaultTools(Options{})
	if !slices.Contains(def, "record_plan") {
		t.Errorf("default tools missing record_plan: %v", def)
	}
	if slices.Contains(def, "request_implementation") {
		t.Errorf("default tools should not include request_implementation: %v", def)
	}
}

func TestPlanToolsIncludeRecordPlanAndRequestImplementation(t *testing.T) {
	pt := planTools(Options{})
	for _, name := range []string{"record_plan", "request_implementation"} {
		if !slices.Contains(pt, name) {
			t.Errorf("plan tools missing %q: %v", name, pt)
		}
	}
}

// plan (and explore) gain run_command via the shared inspection set so they can
// explore via external tools (gh, builds, screenshots, live apps) but keep no
// first-class file-mutation tools, so "don't modify the project" remains a
// prompt-level contract.
func TestPlanToolsAllowRunCommandButNotFileMutation(t *testing.T) {
	pt := planTools(Options{})
	if !slices.Contains(pt, "run_command") {
		t.Errorf("plan tools missing run_command: %v", pt)
	}
	for _, forbidden := range []string{"edit", "write_file", "apply_patch"} {
		if slices.Contains(pt, forbidden) {
			t.Errorf("plan tools unexpectedly include %q: %v", forbidden, pt)
		}
	}
}

func TestNamesSorted(t *testing.T) {
	m := Resolve(map[string]FileDefinition{"zz": {}, "aa": {}})
	if got := Names(m); !slices.Equal(got, []string{"aa", "auto", "explore", "independent", "plan", "zz"}) {
		t.Errorf("Names = %v", got)
	}
}
