package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"harness/internal/config"
	"harness/internal/llm"
	"harness/internal/llm/llmtest"
	"harness/internal/modelproxy/protocol"
	"harness/internal/taskcontext"
	"harness/internal/ui"
)

func TestContextManagementDefaultsToResolvedAstra(t *testing.T) {
	catalog := protocol.Catalog{Targets: []protocol.Target{
		{ID: "openai-codex:gpt-6-astra", Aliases: []string{"astra"}, ProviderLabel: "openai-codex", ModelLabel: "gpt-6-astra"},
		{ID: "openai-codex:gpt-6-astra:priority", Aliases: []string{"astra:priority"}, ProviderLabel: "openai-codex", ModelLabel: "gpt-6-astra"},
		{ID: "openai-codex:gpt-6-astra-2026-09-03:priority"}, // Older catalog: labels omitted.
		{ID: "openai-codex:gpt-6-astral"},
		{ID: "openai-codex:gpt-5.6-sol", ProviderLabel: "openai-codex"},
		{ID: "openai:gpt-6-astra", ProviderLabel: "openai"},
		{ID: "anthropic:claude", Aliases: []string{"openai-codex", "gpt-6-astra"}, ProviderLabel: "anthropic"},
		{ID: "legacy-astra-id", ProviderLabel: "openai-codex", ModelLabel: "gpt-5.6-sol"},
	}}
	for _, mode := range []string{"", "auto", "on", "off"} {
		for _, legacy := range []bool{false, true} {
			cfg := config.Config{ContextManagement: mode, CodexExperimentalContextManagement: legacy}
			for _, id := range []string{"astra", "astra:priority", "openai-codex:gpt-6-astra-2026-09-03:priority", "openai-codex:gpt-6-astral", "openai-codex:gpt-5.6-sol", "openai:gpt-6-astra", "openai-codex", "gpt-6-astra", "legacy-astra-id", "missing"} {
				auto := id == "astra" || id == "astra:priority" || id == "openai-codex:gpt-6-astra-2026-09-03:priority"
				want := id != "missing" && (mode == "on" || (mode == "" || mode == "auto") && legacy && auto)
				if got := contextManagementForProvider(cfg, catalog, id); got != want {
					t.Fatalf("mode=%q legacy=%v target=%s got=%v want=%v", mode, legacy, id, got, want)
				}
			}
		}
	}
}

func TestContextToolsFollowREPLModelSwitches(t *testing.T) {
	for _, mode := range []string{"auto", "on", "off"} {
		t.Run(mode, func(t *testing.T) {
			cfg := filepath.Join(t.TempDir(), "config.json")
			if err := os.WriteFile(cfg, []byte(`{"context_management":"`+mode+`"}`), 0600); err != nil {
				t.Fatal(err)
			}
			fp := llmtest.New("fake", okStep(), okStep(), okStep(), okStep())
			targets := []string{"openai-codex:gpt-6-astra", "openai-codex:gpt-5.6-sol", "anthropic:claude-opus-4-8", "openai-codex:gpt-6-astra:priority"}
			input := "first\n/model " + targets[1] + "\nsecond\n/model " + targets[2] + "\nthird\n/model " + targets[3] + "\nfourth\n/exit\n"
			env, _, errw, _, proxy := fakeProviderEnvWithProxy(t, []string{"-config", cfg, "-model", targets[0]}, fp, input)
			env.stdinPiped = true
			for _, id := range []string{targets[0], targets[1], targets[3]} {
				proxy.catalog.Targets = append(proxy.catalog.Targets, protocol.Target{ID: id, ContextWindow: 100000})
			}
			if code := run(env); code != ui.ExitOK {
				t.Fatalf("run = %d: %s", code, errw.String())
			}
			if len(proxy.requests) != len(targets) {
				t.Fatalf("requests=%d: %s", len(proxy.requests), errw.String())
			}
			for i, request := range proxy.requests {
				if request.TargetID != targets[i] {
					t.Fatalf("request %d target=%s", i, request.TargetID)
				}
				for _, name := range taskcontext.Names {
					want := mode != "off" && (name != "new_context" || mode == "on" || i == 0 || i == 3)
					if got := slices.Contains(toolNames(request.Request), name); got != want {
						t.Errorf("request %d %s exposed=%v want=%v", i, name, got, want)
					}
				}
				// Returning to Astra exposes new_context, but must not invite a
				// destructive automatic reset until intervening work is reconciled.
				wantReset := mode == "on" || mode == "auto" && i == 0
				if got := strings.Contains(strings.Join(request.Request.RequestContext, "\n"), "Experimental context management"); got != wantReset {
					t.Errorf("request %d guidance=%v want=%v", i, got, wantReset)
				}
			}
		})
	}
}

func TestContextToolsUseResolvedChildModel(t *testing.T) {
	for _, tc := range []struct {
		name, parent, child, mode   string
		parentEnabled, childEnabled bool
	}{
		{"astra_to_sol", "openai-codex:gpt-6-astra", "openai-codex:gpt-5.6-sol", "auto", true, false},
		{"sol_to_astra", "openai-codex:gpt-5.6-sol", "openai-codex:gpt-6-astra", "auto", false, true},
		{"claude_to_astra", "anthropic:claude-opus-4-8", "openai-codex:gpt-6-astra", "auto", false, true},
		{"astra_to_claude_opt_in", "openai-codex:gpt-6-astra", "anthropic:claude-opus-4-8", "on", true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := filepath.Join(t.TempDir(), "config.json")
			body := `{"context_management":"` + tc.mode + `","agents":{"context-child":{"description":"Context policy test","model":"` + tc.child + `","allowed_tools":["read"],"prompt":"Inspect the task."}}}`
			if err := os.WriteFile(cfg, []byte(body), 0600); err != nil {
				t.Fatal(err)
			}
			fp := llmtest.New("fake", llmtest.Step{Events: []llm.StreamEvent{{Kind: llm.EventToolCallDone, ToolID: "child", ToolName: "delegate", ToolInput: json.RawMessage(`{"task":"inspect","agent":"context-child"}`)}}, Stop: llm.StopToolUse}, okStep(), okStep())
			env, _, errw, _, proxy := fakeProviderEnvWithProxy(t, []string{"-config", cfg, "-model", tc.parent, "-p", "inspect"}, fp, "")
			for _, id := range []string{"openai-codex:gpt-6-astra", "openai-codex:gpt-5.6-sol"} {
				proxy.catalog.Targets = append(proxy.catalog.Targets, protocol.Target{ID: id, ContextWindow: 100000})
			}
			if code := run(env); code != ui.ExitOK {
				t.Fatalf("run = %d: %s", code, errw.String())
			}
			if len(proxy.requests) != 3 {
				t.Fatalf("requests=%d: %s", len(proxy.requests), errw.String())
			}
			for i, want := range []bool{tc.parentEnabled, tc.childEnabled, tc.parentEnabled} {
				request := proxy.requests[i]
				wantTarget := tc.parent
				if i == 1 {
					wantTarget = tc.child
				}
				if request.TargetID != wantTarget {
					t.Fatalf("request %d target=%s want=%s", i, request.TargetID, wantTarget)
				}
				for _, name := range taskcontext.Names {
					if got := slices.Contains(toolNames(request.Request), name); got != want {
						t.Errorf("request %d %s exposed=%v want=%v", i, name, got, want)
					}
				}
			}
		})
	}
}

func TestRootContextToolsFollowProviderOnFirstRequest(t *testing.T) {
	for _, tc := range []struct {
		name, provider, model, config string
		want                          bool
	}{
		{"astra_default", "openai-codex", "gpt-6-astra", `{}`, true},
		{"codex_legacy_enabled", "openai-codex", "gpt-6-astra", `{"codex_experimental_context_management":true}`, true},
		{"codex_legacy_disabled", "openai-codex", "gpt-6-astra", `{"codex_experimental_context_management":false}`, false},
		{"sol_default", "openai-codex", "gpt-5.6-sol", `{}`, false},
		{"api_astra_default", "openai", "gpt-6-astra", `{}`, false},
		{"api_legacy_enabled", "openai", "gpt-6-astra", `{"codex_experimental_context_management":true}`, false},
		{"claude_default", "anthropic", "claude-test", `{}`, false},
		{"claude_opt_in", "anthropic", "claude-test", `{"context_management":"on"}`, true},
		{"gemini_opt_in", "google", "gemini-test", `{"context_management":"on"}`, true},
		{"sol_opt_in", "openai-codex", "gpt-5.6-sol", `{"context_management":"on"}`, true},
		{"explicit_overrides_legacy", "anthropic", "claude-test", `{"context_management":"on","codex_experimental_context_management":false}`, true},
		{"astra_off", "openai-codex", "gpt-6-astra", `{"context_management":"off"}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := filepath.Join(t.TempDir(), "config.json")
			if err := os.WriteFile(cfg, []byte(tc.config), 0600); err != nil {
				t.Fatal(err)
			}
			fp := llmtest.New("fake", okStepWithUsage(1, 1))
			id := tc.provider + ":" + tc.model
			env, _, errw, _, proxy := fakeProviderEnvWithProxy(t, []string{"-config", cfg, "-model", id, "-p", "hi"}, fp, "")
			proxy.catalog.Targets = append(proxy.catalog.Targets, protocol.Target{ID: id, ProviderLabel: tc.provider, ModelLabel: tc.model, ContextWindow: 100000})
			if code := run(env); code != ui.ExitOK {
				t.Fatalf("run = %d: %s", code, errw.String())
			}
			if len(fp.Requests) == 0 {
				t.Fatal("no model request")
			}
			req := fp.Requests[0]
			for _, name := range taskcontext.Names {
				if got := slices.Contains(toolNames(req), name); got != tc.want {
					t.Fatalf("%s exposed=%v on %s", name, got, tc.provider)
				}
			}
			if strings.Contains(strings.Join(req.RequestContext, "\n"), "Experimental context management") != tc.want {
				t.Fatal("guidance did not follow provider")
			}
		})
	}
}
