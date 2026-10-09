package llm

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

func TestSupportsIncrementalTools(t *testing.T) {
	for _, model := range []string{
		"gpt-6-astra", "gpt-5.6", "gpt-6-astra-2026-10-08", "gpt-5.6-2026-02-28",
		" OPENAI: GPT-6-ASTRA ", " OpenAI:gpt-5.6-2026-10-08 ",
		"gpt-6.1-sol", " OPENAI:GPT-6.1-SOL ",
	} {
		for _, baseURL := range []string{"https://api.openai.com/v1", "https://api.openai.com/v1/", " HTTPS://API.OPENAI.COM/v1/ "} {
			if !SupportsIncrementalTools(model, baseURL) {
				t.Errorf("rejected supported target %q at %q", model, baseURL)
			}
		}
	}
	for _, model := range []string{
		"", "gpt-5.5", "gpt-5.7", "gpt-6", "gpt-6-astra-mini", "gpt-6-astra-codex", "gpt-5.6-codex",
		"gpt-5.6-latest", "gpt-5.60", "gpt-5.6-2026-1-01", "gpt-5.6-2026-01-1", "gpt-5.6-2026-02-30",
		"gpt-5.6-2026-10-08-extra", "gpt-6-astra-20261008", "gpt-6-astra-2026-00-01", "gpt-6-astra-2026-10-08-codex",
		"custom:gpt-5.6", "codex:gpt-6-astra", "openai:openai:gpt-5.6", "gpt- 5.6",
		"gpt-6.1", "gpt-6.1-sol-mini", "gpt-6.1-sol-codex", "gpt-6.1-sol-2026-10-08",
	} {
		if SupportsIncrementalTools(model, "https://api.openai.com/v1") {
			t.Errorf("accepted unsupported model %q", model)
		}
	}
	for _, baseURL := range []string{
		"", "http://api.openai.com/v1", "https://api.openai.com", "https://api.openai.com/v1//",
		"https://api.openai.com:443/v1", "https://api.openai.com/v1/responses", "https://api.openai.com/custom/v1",
		"https://api.openai.com/v1?", "https://api.openai.com/v1?x=1", "https://api.openai.com/v1#", "https://api.openai.com/v1#fragment",
		"https://user@api.openai.com/v1", "https://user:pass@api.openai.com/v1", "https://api.openai.com.evil/v1",
		"https://chatgpt.com/backend-api/codex", "https://localhost/v1", "https://api.openai.com/%76%31",
	} {
		for _, model := range []string{"gpt-6-astra", "gpt-6.1-sol"} {
			if SupportsIncrementalTools(model, baseURL) {
				t.Errorf("accepted unsupported endpoint %q for %s", baseURL, model)
			}
		}
	}
}

func TestValidateToolContext(t *testing.T) {
	for _, tc := range []struct {
		name      string
		context   *ToolContext
		wantError string
	}{
		{name: "absent"},
		{name: "empty initial", context: &ToolContext{ReplayDomain: "domain", Initial: true}},
		{name: "complete initial", context: &ToolContext{ReplayDomain: "domain", Initial: true, Tools: []ToolSchema{{Name: "read", Parameters: json.RawMessage(`{"type":"object"}`)}}}},
		{name: "delta without baseline", context: &ToolContext{ReplayDomain: "domain", After: true, Tools: []ToolSchema{{Name: "read"}}, Removed: []string{"write"}}},
		{name: "empty delta", context: &ToolContext{ReplayDomain: "domain"}},
		{name: "missing domain", context: &ToolContext{}, wantError: "replay domain"},
		{name: "blank domain", context: &ToolContext{ReplayDomain: " \t"}, wantError: "replay domain"},
		{name: "initial removal", context: &ToolContext{ReplayDomain: "domain", Initial: true, Removed: []string{"read"}}, wantError: "initial event"},
		{name: "empty tool name", context: &ToolContext{ReplayDomain: "domain", Tools: []ToolSchema{{}}}, wantError: "empty name"},
		{name: "blank tool name", context: &ToolContext{ReplayDomain: "domain", Tools: []ToolSchema{{Name: " "}}}, wantError: "empty name"},
		{name: "duplicate tool", context: &ToolContext{ReplayDomain: "domain", Tools: []ToolSchema{{Name: "read"}, {Name: "read"}}}, wantError: "duplicate tool"},
		{name: "empty removal", context: &ToolContext{ReplayDomain: "domain", Removed: []string{""}}, wantError: "empty name"},
		{name: "blank removal", context: &ToolContext{ReplayDomain: "domain", Removed: []string{" "}}, wantError: "empty name"},
		{name: "duplicate removal", context: &ToolContext{ReplayDomain: "domain", Removed: []string{"read", "read"}}, wantError: "duplicate removed"},
		{name: "overlap", context: &ToolContext{ReplayDomain: "domain", Tools: []ToolSchema{{Name: "read"}}, Removed: []string{"read"}}, wantError: "both added and removed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, role := range []Role{RoleUser, RoleAssistant} {
				messages := []Message{{Role: role, ToolContext: tc.context}}
				for name, validate := range map[string]func([]Message) error{"content": ValidateMessageContent, "transcript": ValidateTranscript} {
					err := validate(messages)
					if tc.wantError == "" {
						if err != nil {
							t.Errorf("%s %s: %v", name, role, err)
						}
					} else if err == nil || !strings.Contains(err.Error(), tc.wantError) {
						t.Errorf("%s %s: error = %v; want %q", name, role, err, tc.wantError)
					}
				}
			}
		})
	}
	for _, raw := range []string{"null", "[]", "true", "1", `"object"`, "{", "{} {}", " "} {
		context := &ToolContext{ReplayDomain: "domain", Tools: []ToolSchema{{Name: "read", Parameters: json.RawMessage(raw)}}}
		if err := ValidateMessageContent([]Message{{Role: RoleUser, ToolContext: context}}); err == nil {
			t.Errorf("accepted non-object schema %q", raw)
		}
	}
}

func TestToolContextJSONAndFingerprint(t *testing.T) {
	base := []Message{{Role: RoleUser, ToolContext: &ToolContext{
		ReplayDomain: "domain", Initial: true, After: true,
		Tools: []ToolSchema{{Name: "read", Description: "read file", Parameters: json.RawMessage(`{"type":"object"}`)}},
	}}}
	encoded, err := json.Marshal(base)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(encoded), `"tool_context":{"replay_domain":"domain","initial":true,"after":true,"tools":`) {
		t.Fatalf("unexpected JSON contract: %s", encoded)
	}
	var decoded []Message
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(base, decoded) {
		t.Fatal("metadata did not round trip")
	}
	fingerprint, err := FingerprintMessages(base)
	if err != nil {
		t.Fatal(err)
	}
	if !MatchesMessageFingerprint(CloneMessages(base), fingerprint) {
		t.Fatal("clone fingerprint changed")
	}
	for name, mutate := range map[string]func(*Message){
		"presence":    func(m *Message) { m.ToolContext = nil },
		"domain":      func(m *Message) { m.ToolContext.ReplayDomain = "other" },
		"initial":     func(m *Message) { m.ToolContext.Initial = false },
		"placement":   func(m *Message) { m.ToolContext.After = false },
		"name":        func(m *Message) { m.ToolContext.Tools[0].Name = "write" },
		"description": func(m *Message) { m.ToolContext.Tools[0].Description = "changed" },
		"parameters":  func(m *Message) { m.ToolContext.Tools[0].Parameters = json.RawMessage(`{}`) },
		"async":       func(m *Message) { m.ToolContext.Tools[0].Async = true },
		"removed":     func(m *Message) { m.ToolContext.Removed = []string{"write"} },
	} {
		t.Run(name, func(t *testing.T) {
			changed := CloneMessages(base)
			mutate(&changed[0])
			if MatchesMessageFingerprint(changed, fingerprint) {
				t.Fatal("metadata change did not affect fingerprint")
			}
		})
	}
}

func TestIncrementalToolsConfigDefaultsAndOverride(t *testing.T) {
	for _, tc := range []struct {
		input string
		want  bool
	}{
		{`{}`, true},
		{`{"incremental_tools":null}`, true},
		{`{"incremental_tools":true}`, true},
		{`{"incremental_tools":false}`, false},
	} {
		t.Run(tc.input, func(t *testing.T) {
			var cache PromptCacheConfig
			if err := json.Unmarshal([]byte(tc.input), &cache); err != nil {
				t.Fatal(err)
			}
			for _, model := range []string{"gpt-5.6", "gpt-6-astra", "gpt-6.1-sol", "gpt-5.6-2026-10-01", "gpt-6-astra-2026-10-01"} {
				if got := cache.IncrementalToolsEnabled(model, "https://api.openai.com/v1"); got != tc.want {
					t.Fatalf("%s enabled=%v, want %v", model, got, tc.want)
				}
			}
			if cache.IncrementalToolsEnabled("gpt-5.5", "https://api.openai.com/v1") || cache.IncrementalToolsEnabled("gpt-5.6", "https://custom.example/v1") {
				t.Fatal("override widened support gate")
			}
			encoded, err := json.Marshal(cache)
			if err != nil {
				t.Fatal(err)
			}
			var restored PromptCacheConfig
			if err := json.Unmarshal(encoded, &restored); err != nil {
				t.Fatal(err)
			}
			if restored.IncrementalToolsEnabled("gpt-5.6", "https://api.openai.com/v1") != tc.want {
				t.Fatalf("round trip lost setting: %s", encoded)
			}
			if !tc.want && !strings.Contains(string(encoded), `"incremental_tools":false`) {
				t.Fatalf("explicit opt-out omitted: %s", encoded)
			}
		})
	}
}
