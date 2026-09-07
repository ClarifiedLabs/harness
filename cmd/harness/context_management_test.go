package main

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"harness/internal/config"
	"harness/internal/llm/llmtest"
	"harness/internal/modelproxy/protocol"
	"harness/internal/taskcontext"
	"harness/internal/ui"
)

func TestContextManagementRequiresResolvedCodexProvider(t *testing.T) {
	catalog := protocol.Catalog{Targets: []protocol.Target{
		{ID: "openai-codex:gpt-6-astra", Aliases: []string{"astra"}, ProviderLabel: "openai-codex"},
		{ID: "openai-codex:gpt-6-astra:priority", Aliases: []string{"astra:priority"}, ProviderLabel: "openai-codex"},
		{ID: "openai-codex:gpt-5.6-sol", ProviderLabel: "openai-codex"},
		{ID: "openai:gpt-6-astra", ProviderLabel: "openai"},
		{ID: "anthropic:claude", Aliases: []string{"openai-codex"}, ProviderLabel: "anthropic"},
	}}
	for _, enabled := range []bool{false, true} {
		for _, id := range []string{"astra", "astra:priority", "openai-codex:gpt-5.6-sol", "openai:gpt-6-astra", "openai-codex", "missing"} {
			want := enabled && (id == "astra" || id == "astra:priority" || id == "openai-codex:gpt-5.6-sol")
			if got := contextManagementForProvider(config.Config{CodexExperimentalContextManagement: enabled}, catalog, id); got != want {
				t.Fatalf("enabled=%v target=%s got=%v want=%v", enabled, id, got, want)
			}
		}
	}
}

func TestRootContextToolsFollowProviderOnFirstRequest(t *testing.T) {
	for _, tc := range []struct {
		name, provider, config string
		want                   bool
	}{
		{"codex_default", "openai-codex", `{}`, true},
		{"codex_enabled", "openai-codex", `{"codex_experimental_context_management":true}`, true},
		{"codex_disabled", "openai-codex", `{"codex_experimental_context_management":false}`, false},
		{"other_provider_default", "openai", `{}`, false},
		{"other_provider_enabled", "openai", `{"codex_experimental_context_management":true}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := filepath.Join(t.TempDir(), "config.json")
			if err := os.WriteFile(cfg, []byte(tc.config), 0600); err != nil {
				t.Fatal(err)
			}
			fp := llmtest.New("fake", okStepWithUsage(1, 1))
			id := tc.provider + ":gpt-6-astra"
			env, _, errw, _, proxy := fakeProviderEnvWithProxy(t, []string{"-config", cfg, "-model", id, "-p", "hi"}, fp, "")
			proxy.catalog.Targets = append(proxy.catalog.Targets, protocol.Target{ID: id, ProviderLabel: tc.provider, ModelLabel: "gpt-6-astra", ContextWindow: 100000})
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
