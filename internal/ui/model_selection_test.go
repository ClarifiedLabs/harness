package ui

import (
	"bytes"
	"reflect"
	"testing"

	"harness/internal/llm"
	"harness/internal/llm/llmtest"
	"harness/internal/tools"
)

func TestAgentSelectionPartialOverrides(t *testing.T) {
	for _, reasoningSet := range []bool{false, true} {
		name := "preserve reasoning"
		if reasoningSet {
			name = "explicit default reasoning"
		}
		t.Run(name, func(t *testing.T) {
			var out, errw bytes.Buffer
			fp := llmtest.New("fake")
			app := newTestApp(t, &out, &errw, fp)
			app.Reasoning = llm.ReasoningConfig{Profile: "high"}
			app.Agent.SetReasoning(app.Reasoning)
			app.Agent.SetModel(app.Model, 12345)
			app.Agent.SetServerTools([]llm.ServerTool{{Name: llm.ServerToolWebSearch}})
			app.BaseTargetID, app.ReasoningReplayDomain = "old-base", "old-domain"
			oldProvider, oldModel, oldURL := app.Provider, app.Model, app.BaseURL
			oldProxySession := app.Agent.ProxySessionID()
			app.SwitchAgent = func(string) (AgentSelection, error) {
				return AgentSelection{Name: "plan", Tools: tools.Default(), System: "plan prompt", ModelSelection: ModelSelection{ReasoningSet: reasoningSet}}, nil
			}
			if err := app.applyAgentSwitch("plan"); err != nil {
				t.Fatal(err)
			}
			req := app.Agent.ContextRequest()
			wantReasoning := llm.ReasoningConfig{Profile: "high"}
			if reasoningSet {
				wantReasoning = llm.ReasoningConfig{}
			}
			if app.Provider != oldProvider || app.Model != oldModel || app.BaseURL != oldURL || req.Model != oldModel {
				t.Fatalf("partial selection replaced omitted target fields: provider=%s model=%s url=%s request model=%s", app.Provider, app.Model, app.BaseURL, req.Model)
			}
			if got := app.Agent.EstimateContext().Window; got != 12345 {
				t.Fatalf("partial selection changed context window to %d", got)
			}
			if !reflect.DeepEqual(app.Reasoning, wantReasoning) || !reflect.DeepEqual(req.Reasoning, wantReasoning) {
				t.Fatalf("reasoning=%+v request=%+v, want %+v", app.Reasoning, req.Reasoning, wantReasoning)
			}
			if app.RegistryModel != oldModel || app.Renderer.model != oldModel || app.BaseTargetID != "" || app.ReasoningReplayDomain != "" || len(req.ServerTools) != 0 {
				t.Fatal("partial selection did not apply target defaults/clear replace-only fields")
			}
			if app.Agent.ProxySessionID() == oldProxySession || app.AgentName != "plan" || req.System != "plan prompt" {
				t.Fatal("partial agent switch did not replace agent state and reset continuation")
			}
		})
	}
}

func TestModelSelectionReasoningFallbackAndDefaults(t *testing.T) {
	for _, test := range []struct {
		name         string
		reasoning    llm.ReasoningConfig
		reasoningSet bool
		want         llm.ReasoningConfig
	}{
		{name: "requested fallback", want: llm.ReasoningConfig{Profile: "high"}},
		{name: "explicit default", reasoningSet: true},
		{name: "returned reasoning", reasoning: llm.ReasoningConfig{Profile: "low"}, want: llm.ReasoningConfig{Profile: "low"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			var out, errw bytes.Buffer
			fp := llmtest.New("fake")
			app := newTestApp(t, &out, &errw, fp)
			oldProvider := app.Provider
			app.SwitchModel = func(string, llm.ReasoningConfig) (ModelSelection, error) {
				return ModelSelection{Runtime: fp, Reasoning: test.reasoning, ReasoningSet: test.reasoningSet}, nil
			}
			if !app.switchModel("selected-model", llm.ReasoningConfig{Profile: "high"}) {
				t.Fatalf("switch failed: %s", errw.String())
			}
			req := app.Agent.ContextRequest()
			if !reflect.DeepEqual(app.Reasoning, test.want) || !reflect.DeepEqual(req.Reasoning, test.want) {
				t.Fatalf("reasoning=%+v request=%+v, want %+v", app.Reasoning, req.Reasoning, test.want)
			}
			if app.Provider != oldProvider || app.Model != "selected-model" || req.Model != app.Model || app.RegistryModel != app.Model || app.Renderer.model != app.Model {
				t.Fatal("model/provider/registry fallback not applied")
			}
			if app.BaseTargetID != app.Model || app.ReasoningReplayDomain != app.Model || app.BaseURL != "" {
				t.Fatalf("target defaults/base URL = %q/%q/%q", app.BaseTargetID, app.ReasoningReplayDomain, app.BaseURL)
			}
		})
	}
}
