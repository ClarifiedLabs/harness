package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"harness/internal/config"
	"harness/internal/llm"
	"harness/internal/llm/llmtest"
	"harness/internal/tools"
)

type rootSteeringProvider struct{ *llmtest.FakeProvider }

func (rootSteeringProvider) Steer(context.Context, llm.SteerSubmission) error {
	return llm.ErrSteeringUnavailable
}

func TestRootNativeSteeringDefaultsAndOptOuts(t *testing.T) {
	for _, tt := range []struct {
		name    string
		config  string
		capable bool
		live    bool
		want    bool
	}{
		{name: "capable Astra", config: `{}`, capable: true, live: true, want: true},
		{name: "native opt-out", config: `{"astra_native_steering":false}`, capable: true, live: true},
		{name: "steering disabled", config: `{"no_steer":true}`, capable: true, live: true},
		{name: "stateless", config: `{"responses_stateful":false}`, capable: true, live: true},
		{name: "unsupported target", config: `{}`, live: true},
		{name: "unsupported transport", config: `{}`, capable: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "config.json")
			if err := os.WriteFile(path, []byte(tt.config), 0o600); err != nil {
				t.Fatal(err)
			}
			result, err := config.Load(config.LoadOptions{
				Args:              []string{"--model", "openai:gpt-6-astra"},
				LookupEnv:         func(string) (string, bool) { return "", false },
				DefaultConfigPath: path,
				WorkingDir:        dir,
			})
			if err != nil {
				t.Fatal(err)
			}
			fp := llmtest.New(result.Config.Model)
			var provider llm.Provider = fp
			if tt.live {
				provider = rootSteeringProvider{fp}
			}
			ag := newRootAgent(provider, tools.Default(), rootAgentConfig{
				Config: result.Config,
				Registry: llm.NewRegistry(map[string]llm.ModelInfo{
					result.Config.Model: {NativeSteering: tt.capable, ContextWindow: 100000},
				}),
				ResponsesStateful: result.Config.ResponsesStateful,
				Interactive:       true,
			})
			if req := ag.ContextRequest(); req.NativeSteering != tt.want {
				t.Fatalf("request native steering = %t, want %t", req.NativeSteering, tt.want)
			}
		})
	}
}
