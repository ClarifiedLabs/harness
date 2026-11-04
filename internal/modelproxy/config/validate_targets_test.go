package config_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"harness/internal/llm"
	"harness/internal/modelproxy/config"
	"harness/internal/modelproxy/runtime"
)

func TestOfflineTargetValidationMatchesRuntime(t *testing.T) {
	for _, tc := range []struct {
		name      string
		providers string
		invalid   bool
	}{
		{"tier_then_model", `[{"name":"p","api_type":"openai","models":[{"name":"m","service_tiers":[{"id":"fast","request":{"service_tier":"priority"}}]},{"name":"m:fast"}]}]`, true},
		{"model_then_tier", `[{"name":"p","api_type":"openai","models":[{"name":"m:fast"},{"name":"m","service_tiers":[{"id":"fast","request":{"service_tier":"priority"}}]}]}]`, true},
		{"qualified_alias_collision", `[{"name":"p","api_type":"openai","models":[{"name":"m"}]},{"name":"q","api_type":"openai","models":[{"name":"p:m"}]}]`, true},
		{"tier_alias_collision", `[{"name":"p","api_type":"openai","models":[{"name":"m","service_tiers":[{"id":"fast","request":{"service_tier":"priority"}}]}]},{"name":"q","api_type":"openai","models":[{"name":"p:m:fast"}]}]`, true},
		{"duplicate", `[{"name":"p","api_type":"openai","models":[{"name":"m"},{"name":"m"}]}]`, true},
		{"no_models", `[{"name":"p","api_type":"openai","models":[]}]`, true},
		{"ambiguous_short_alias_omitted", `[{"name":"p","api_type":"openai","models":[{"name":"m"}]},{"name":"q","api_type":"openai","models":[{"name":"m"}]}]`, false},
		{"default_tier_not_variant", `[{"name":"p","api_type":"openai","models":[{"name":"m","service_tiers":[{"id":"fast","request":{"service_tier":"standard"}}]},{"name":"m:fast"}]}]`, false},
		{"valid_tiers", `[{"name":"p","api_type":"openai","models":[{"name":"m","service_tiers":[{"id":"fast","request":{"speed":"fast"}}]}]}]`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var providers []llm.ProviderConfig
			if err := json.Unmarshal([]byte(tc.providers), &providers); err != nil {
				t.Fatal(err)
			}
			dir := t.TempDir()
			data, err := json.Marshal(struct {
				Providers []llm.ProviderConfig `json:"providers"`
			}{providers})
			if err != nil {
				t.Fatal(err)
			}
			if err = os.WriteFile(filepath.Join(dir, "providers.json"), data, 0600); err != nil {
				t.Fatal(err)
			}
			cfg := config.Config{ProviderConfigs: []string{"providers.json"}}
			offline := config.ValidateConfigReferences(dir, cfg, func(string) string { return "" }, nil)
			core, online := runtime.New(runtime.Options{ConfigDir: dir, Config: cfg, Getenv: func(string) string { return "" }})
			if core != nil {
				defer core.Close()
				if _, err := core.Catalog(context.Background()); err != nil {
					t.Fatal(err)
				}
			}
			if (offline != nil) != tc.invalid || (online != nil) != tc.invalid {
				t.Fatalf("offline=%v runtime=%v invalid=%v", offline, online, tc.invalid)
			}
			if offline != nil && online != nil && offline.Error() != online.Error() {
				t.Fatalf("offline=%v runtime=%v", offline, online)
			}
		})
	}
}
