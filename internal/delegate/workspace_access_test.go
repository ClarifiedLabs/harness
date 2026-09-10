package delegate

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"harness/internal/agentdef"
	"harness/internal/llm/llmtest"
	"harness/internal/session"
	"harness/internal/tools"
)

func TestEffectiveWorkspaceAccessFallback(t *testing.T) {
	for _, access := range []string{"read_only", "exclusive", "", "readonly", "read-only", "READ_ONLY", " read_only ", "shared", "unknown"} {
		t.Run(access, func(t *testing.T) {
			want := tools.BackgroundAccessExclusive
			if access == "read_only" {
				want = tools.BackgroundAccessReadOnly
			}
			if got := effectiveWorkspaceAccess(access); got != want {
				t.Fatalf("effectiveWorkspaceAccess(%q) = %q, want %q", access, got, want)
			}
		})
	}
}

func TestDefaultWorkspaceAccessFallbackAndContinuation(t *testing.T) {
	runtime := Runtime{Agent: "reader"}
	runner := NewRunner(nil, nil, Options{})
	if got := runner.defaultWorkspaceAccess(runtime, RunRequest{}, nil); got != tools.BackgroundAccessExclusive {
		t.Fatalf("no catalog access = %q, want exclusive", got)
	}
	runner.opts.AgentCandidates = func(Runtime) []AgentCandidate {
		return []AgentCandidate{{Name: "reader", WorkspaceAccess: tools.BackgroundAccessReadOnly}}
	}
	if got := runner.defaultWorkspaceAccess(runtime, RunRequest{Agent: "missing"}, nil); got != tools.BackgroundAccessExclusive {
		t.Fatalf("missing candidate access = %q, want exclusive", got)
	}
	for _, access := range []string{"read_only", "exclusive", "", "unknown"} {
		t.Run(access, func(t *testing.T) {
			source := &continuationSource{meta: session.ChildMeta{Access: access}}
			want := tools.BackgroundAccessReadOnly // Missing/invalid saved access still falls back to the current agent.
			if access == tools.BackgroundAccessExclusive {
				want = tools.BackgroundAccessExclusive
			}
			if got := runner.defaultWorkspaceAccess(runtime, RunRequest{}, source); got != want {
				t.Fatalf("continuation access %q resolved to %q, want %q", access, got, want)
			}
		})
	}
}

func TestDelegateSchemaWorkspaceAccessMatchesRuntime(t *testing.T) {
	for _, custom := range []bool{false, true} {
		name := "builtins"
		definitions := agentdef.Builtins()
		wantAccess := map[string]string{
			"auto": "exclusive", "explore": "read_only", "independent": "exclusive", "plan": "read_only", "review": "read_only",
		}
		if custom {
			name = "custom-and-overridden-builtins"
			definitions = agentdef.Resolve(map[string]agentdef.FileDefinition{
				"explore": {WorkspaceAccess: "exclusive"},
				"reader":  {Description: "Custom reader", WorkspaceAccess: "read_only"},
				"writer":  {Description: "Custom writer", WorkspaceAccess: "exclusive"},
				"default": {Description: "Custom default"},
			})
			wantAccess["explore"] = "exclusive"
			wantAccess["reader"] = "read_only"
			wantAccess["writer"] = "exclusive"
			wantAccess["default"] = "exclusive"
		}
		t.Run(name, func(t *testing.T) {
			var candidates []AgentCandidate
			for _, definition := range definitions {
				candidates = append(candidates, AgentCandidate{
					Name: definition.Name, Description: definition.Description,
					ToolNames: definition.AllowedTools, WorkspaceAccess: definition.WorkspaceAccess,
				})
			}
			// Raw callbacks may omit metadata or supply noncanonical values; neither
			// should advertise a shared lease that the runner will not use.
			candidates = append(candidates,
				AgentCandidate{Name: "raw-missing", Description: "Missing access"},
				AgentCandidate{Name: "raw-invalid", Description: "Invalid access", WorkspaceAccess: " read_only "},
			)
			wantAccess["raw-missing"] = "exclusive"
			wantAccess["raw-invalid"] = "exclusive"
			runtime := Runtime{Provider: llmtest.New("fake"), ToolNames: definitions["auto"].AllowedTools}
			runner := NewRunner(func() Runtime { return runtime }, nil, Options{
				AgentCandidates: func(Runtime) []AgentCandidate { return candidates },
			})
			var decoded struct {
				Properties map[string]struct {
					Description string   `json:"description"`
					Enum        []string `json:"enum"`
				} `json:"properties"`
			}
			if err := json.Unmarshal(runner.Schema(), &decoded); err != nil {
				t.Fatal(err)
			}
			var wantNames []string
			for name := range wantAccess {
				wantNames = append(wantNames, name)
			}
			slices.Sort(wantNames)
			if got := decoded.Properties["agent"].Enum; !slices.Equal(got, wantNames) {
				t.Fatalf("agent enum = %v, want %v", got, wantNames)
			}
			for _, candidate := range candidates {
				want := wantAccess[candidate.Name]
				entry := "\n- " + candidate.Name + ": " + candidate.Description + " [background access: " + want + "]"
				if !strings.Contains(decoded.Properties["agent"].Description, entry) {
					t.Errorf("catalog missing %q", entry)
				}
				if got := runner.defaultWorkspaceAccess(runtime, RunRequest{Agent: candidate.Name}, nil); got != want {
					t.Errorf("%s default access = %q, want %q", candidate.Name, got, want)
				}
				runtime.Agent = candidate.Name
				for _, requestedAgent := range []string{candidate.Name, ""} {
					prepared, err := runner.prepareRun(RunRequest{Task: "inspect", Agent: requestedAgent, Background: true})
					if err != nil {
						t.Fatal(err)
					}
					if prepared.req.Access != want {
						t.Errorf("%s requested as %q: runtime access = %q, want %q", candidate.Name, requestedAgent, prepared.req.Access, want)
					}
				}
				prepared, err := runner.prepareRun(RunRequest{Task: "implement", Mode: ModeImplementation, Background: true, Access: "read_only"})
				if err != nil {
					t.Fatal(err)
				}
				if prepared.req.Access != tools.BackgroundAccessExclusive {
					t.Errorf("%s implementation access = %q, want exclusive", candidate.Name, prepared.req.Access)
				}
			}
			if !strings.Contains(decoded.Properties["access"].Description, "mode:implementation forces exclusive.") {
				t.Fatalf("access description omits implementation override: %q", decoded.Properties["access"].Description)
			}
		})
	}
}
