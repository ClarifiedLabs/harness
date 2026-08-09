package agentdef

import (
	"slices"
	"strings"
	"testing"
)

func TestDefaultIsAuto(t *testing.T) {
	if Default != "auto" {
		t.Errorf("Default = %q, want \"auto\"", Default)
	}
}

func TestBuiltins(t *testing.T) {
	m := Builtins()
	if len(m) != 5 {
		t.Fatalf("want 5 builtin agents, got %d: %v", len(m), Names(m))
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
	if !slices.Equal(auto.AllowedTools, defaultTools()) {
		t.Errorf("auto tools = %v, want default set", auto.AllowedTools)
	}
	if auto.MCPTools != MCPToolsAll {
		t.Errorf("auto MCPTools = %q, want %q", auto.MCPTools, MCPToolsAll)
	}
	if auto.WorkspaceAccess != WorkspaceAccessExclusive {
		t.Errorf("auto WorkspaceAccess = %q, want exclusive", auto.WorkspaceAccess)
	}

	explore := m["explore"]
	if explore.Prompt == "" {
		t.Error("explore must carry a prompt")
	}
	if !slices.Equal(explore.AllowedTools, inspectionTools()) {
		t.Errorf("explore tools = %v, want inspection set", explore.AllowedTools)
	}
	if explore.MCPTools != MCPToolsReadOnly {
		t.Errorf("explore MCPTools = %q, want %q", explore.MCPTools, MCPToolsReadOnly)
	}
	if explore.WorkspaceAccess != WorkspaceAccessReadOnly {
		t.Errorf("explore WorkspaceAccess = %q, want read_only", explore.WorkspaceAccess)
	}
	if !slices.Contains(explore.AllowedTools, "shell") {
		t.Errorf("explore tools missing shell: %v", explore.AllowedTools)
	}
	for _, forbidden := range []string{"write", "edit", "record_plan", "handoff", "create_goal", "update_goal", "delegate", "background_jobs"} {
		if slices.Contains(explore.AllowedTools, forbidden) {
			t.Errorf("explore tools unexpectedly include %q: %v", forbidden, explore.AllowedTools)
		}
	}
	for name, def := range m {
		if name == "plan" {
			if !slices.Contains(def.AllowedTools, "record_plan") || slices.Contains(def.AllowedTools, "update_todos") {
				t.Errorf("plan coordination tools = %v; want record_plan without update_todos", def.AllowedTools)
			}
			continue
		}
		if !slices.Contains(def.AllowedTools, "update_todos") || slices.Contains(def.AllowedTools, "record_plan") {
			t.Errorf("built-in %s coordination tools = %v; want update_todos without record_plan", name, def.AllowedTools)
		}
	}

	ind := m["independent"]
	if ind.Prompt == "" {
		t.Error("independent must carry a prompt")
	}
	if !slices.Equal(ind.AllowedTools, defaultTools()) {
		t.Errorf("independent tools = %v, want default set", ind.AllowedTools)
	}
	if ind.MCPTools != MCPToolsAll {
		t.Errorf("independent MCPTools = %q, want %q", ind.MCPTools, MCPToolsAll)
	}
	if ind.WorkspaceAccess != WorkspaceAccessExclusive {
		t.Errorf("independent WorkspaceAccess = %q, want exclusive", ind.WorkspaceAccess)
	}

	plan := m["plan"]
	if plan.Prompt == "" {
		t.Error("plan must carry a prompt")
	}
	wantPlan := planTools()
	if !slices.Equal(plan.AllowedTools, wantPlan) {
		t.Errorf("plan tools = %v, want %v", plan.AllowedTools, wantPlan)
	}
	if plan.MCPTools != MCPToolsReadOnly {
		t.Errorf("plan MCPTools = %q, want %q", plan.MCPTools, MCPToolsReadOnly)
	}
	if plan.WorkspaceAccess != WorkspaceAccessReadOnly {
		t.Errorf("plan WorkspaceAccess = %q, want read_only", plan.WorkspaceAccess)
	}
	if !slices.Contains(plan.AllowedTools, "shell") {
		t.Errorf("plan tools missing shell: %v", plan.AllowedTools)
	}
	for _, forbidden := range []string{"edit", "write"} {
		if slices.Contains(plan.AllowedTools, forbidden) {
			t.Errorf("plan tools unexpectedly include file-mutation tool %q: %v", forbidden, plan.AllowedTools)
		}
	}

	review := m["review"]
	if review.Prompt == "" {
		t.Error("review must carry a prompt")
	}
	if !slices.Equal(review.AllowedTools, inspectionTools()) {
		t.Errorf("review tools = %v, want inspection set", review.AllowedTools)
	}
	if review.MCPTools != MCPToolsReadOnly {
		t.Errorf("review MCPTools = %q, want %q", review.MCPTools, MCPToolsReadOnly)
	}
	if review.WorkspaceAccess != WorkspaceAccessReadOnly {
		t.Errorf("review WorkspaceAccess = %q, want read_only", review.WorkspaceAccess)
	}
	if review.Model != "" || review.Reasoning != "" {
		t.Errorf("review should inherit model/reasoning, got %q/%q", review.Model, review.Reasoning)
	}
	for _, forbidden := range []string{"write", "edit", "record_plan", "handoff", "create_goal", "update_goal", "delegate", "background_jobs"} {
		if slices.Contains(review.AllowedTools, forbidden) {
			t.Errorf("review tools unexpectedly include %q: %v", forbidden, review.AllowedTools)
		}
	}
}

func TestResolveNilKeepsBuiltins(t *testing.T) {
	m := Resolve(nil)
	if !slices.Equal(Names(m), []string{"auto", "explore", "independent", "plan", "review"}) {
		t.Errorf("Names = %v", Names(m))
	}
}

func TestBuiltinsUseOnlyCurrentInspectionTools(t *testing.T) {
	for name, agent := range Builtins() {
		if !slices.Contains(agent.AllowedTools, "read") {
			t.Fatalf("%s agent tools missing read: %v", name, agent.AllowedTools)
		}
		for _, forbidden := range []string{"read_file", "write_file", "apply_patch", "list_dir", "glob", "search", "git", "git_readonly", "grep", "rg"} {
			if slices.Contains(agent.AllowedTools, forbidden) {
				t.Fatalf("%s agent tools unexpectedly include %s: %v", name, forbidden, agent.AllowedTools)
			}
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
	m := Resolve(map[string]FileDefinition{"plan": {AllowedTools: []string{"read"}}})
	plan := m["plan"]
	if !slices.Equal(plan.AllowedTools, []string{"read"}) {
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
		"ro":   {AllowedTools: []string{"read"}, MCPTools: "read-only"},
	})
	if m["plan"].MCPTools != MCPToolsAll {
		t.Errorf("plan MCPTools = %q, want all", m["plan"].MCPTools)
	}
	if m["ro"].MCPTools != MCPToolsReadOnly {
		t.Errorf("ro MCPTools = %q, want read_only", m["ro"].MCPTools)
	}
}

func TestResolveWorkspaceAccessOverride(t *testing.T) {
	m := Resolve(map[string]FileDefinition{
		"plan": {WorkspaceAccess: "exclusive"},
		"ro":   {Description: "read-only worker", WorkspaceAccess: "readonly"},
	})
	if m["plan"].WorkspaceAccess != WorkspaceAccessExclusive {
		t.Fatalf("plan workspace access = %q", m["plan"].WorkspaceAccess)
	}
	if m["ro"].WorkspaceAccess != WorkspaceAccessReadOnly {
		t.Fatalf("ro workspace access = %q", m["ro"].WorkspaceAccess)
	}
	if err := Validate(Resolve(map[string]FileDefinition{
		"plan": {WorkspaceAccess: "shared_write"},
	})); err == nil || !strings.Contains(err.Error(), "workspace_access") {
		t.Fatalf("invalid workspace access error = %v", err)
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
	m := Resolve(map[string]FileDefinition{"custom_review": {Prompt: "review the diff"}})
	rev, ok := m["custom_review"]
	if !ok {
		t.Fatal("new agent not resolved")
	}
	if rev.Name != "custom_review" || rev.Prompt != "review the diff" {
		t.Errorf("rev = %+v", rev)
	}
	if !slices.Equal(rev.AllowedTools, defaultTools()) {
		t.Errorf("tools = %v, want default set", rev.AllowedTools)
	}
	if rev.MCPTools != MCPToolsAll {
		t.Errorf("MCPTools = %q, want all", rev.MCPTools)
	}
}

func TestResolvePromptOnlyReviewOverrideKeepsReadOnlyDefaults(t *testing.T) {
	m := Resolve(map[string]FileDefinition{"review": {Prompt: "custom review"}})
	review := m["review"]
	if review.Prompt != "custom review" {
		t.Fatalf("review prompt = %q", review.Prompt)
	}
	builtin := Builtins()["review"]
	if !slices.Equal(review.AllowedTools, builtin.AllowedTools) ||
		review.MCPTools != MCPToolsReadOnly ||
		review.WorkspaceAccess != WorkspaceAccessReadOnly {
		t.Fatalf("prompt-only review override changed defaults: %+v", review)
	}
}

func TestResolveNewAgentWithExplicitTools(t *testing.T) {
	m := Resolve(map[string]FileDefinition{"ro": {AllowedTools: []string{"read", "shell"}}})
	ro := m["ro"]
	if !slices.Equal(ro.AllowedTools, []string{"read", "shell"}) {
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
		m := Resolve(map[string]FileDefinition{"custom_review": {Description: description}})
		err := Validate(m)
		if err == nil || !strings.Contains(err.Error(), `agent "custom_review"`) || !strings.Contains(err.Error(), "description must state when the parent should use it") {
			t.Fatalf("Validate description %q error = %v", description, err)
		}
	}

	m := Resolve(map[string]FileDefinition{"custom_review": {Description: "Use after implementation for an independent correctness review."}})
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

func TestDefaultToolsIncludeTodosButOmitPlanGoalAndHandoffTools(t *testing.T) {
	def := defaultTools()
	if !slices.Contains(def, "update_todos") {
		t.Errorf("default tools missing update_todos: %v", def)
	}
	for _, name := range []string{"record_plan", "create_goal", "update_goal", "handoff"} {
		if slices.Contains(def, name) {
			t.Errorf("default tools unexpectedly include removed %s: %v", name, def)
		}
	}
}

func TestPlanToolsOnlyIncludePlanCoordinationTool(t *testing.T) {
	pt := planTools()
	if !slices.Contains(pt, "record_plan") {
		t.Errorf("plan tools missing record_plan: %v", pt)
	}
	for _, name := range []string{"update_todos", "handoff", "create_goal", "update_goal"} {
		if slices.Contains(pt, name) {
			t.Errorf("plan tools unexpectedly include %q: %v", name, pt)
		}
	}
}

// plan (and explore) gain shell via the shared inspection set so they can
// explore via external tools (gh, builds, screenshots, live apps) but keep no
// first-class file-mutation tools, so "don't modify the project" remains a
// prompt-level contract.
func TestPlanToolsAllowShellButNotFileMutation(t *testing.T) {
	pt := planTools()
	if !slices.Contains(pt, "shell") {
		t.Errorf("plan tools missing shell: %v", pt)
	}
	for _, forbidden := range []string{"edit", "write"} {
		if slices.Contains(pt, forbidden) {
			t.Errorf("plan tools unexpectedly include %q: %v", forbidden, pt)
		}
	}
}

func TestNamesSorted(t *testing.T) {
	m := Resolve(map[string]FileDefinition{"zz": {}, "aa": {}})
	if got := Names(m); !slices.Equal(got, []string{"aa", "auto", "explore", "independent", "plan", "review", "zz"}) {
		t.Errorf("Names = %v", got)
	}
}

func TestImplementationAgentNamesExcludesReadOnly(t *testing.T) {
	m := Builtins()
	if got := ImplementationAgentNames(m); !slices.Equal(got, []string{"auto", "independent"}) {
		t.Fatalf("Builtins exclusive = %v, want [auto independent]", got)
	}
	// custom exclusive (defaults to exclusive) is included
	m2 := Resolve(map[string]FileDefinition{
		"my-impl": {Description: "custom impl", Prompt: "impl"},
		"my-read": {Description: "custom read", WorkspaceAccess: "read_only", Prompt: "read"},
	})
	got := ImplementationAgentNames(m2)
	if !slices.Contains(got, "my-impl") {
		t.Fatalf("my-impl missing from exclusive names: %v", got)
	}
	if slices.Contains(got, "my-read") {
		t.Fatalf("my-read (read_only) leaked into exclusive names: %v", got)
	}
	if !slices.Contains(got, "auto") || !slices.Contains(got, "independent") {
		t.Fatalf("exclusive names missing builtins: %v", got)
	}
	for _, name := range []string{"explore", "plan", "review"} {
		if slices.Contains(got, name) {
			t.Fatalf("read-only builtin %q leaked into exclusive names: %v", name, got)
		}
	}
	wantSorted := slices.Clone(got)
	slices.Sort(wantSorted)
	if !slices.Equal(got, wantSorted) {
		t.Fatalf("ImplementationAgentNames not sorted: %v", got)
	}
	if len(ImplementationAgentNames(map[string]Definition{})) != 0 {
		t.Fatal("empty input should yield empty slice")
	}
}
