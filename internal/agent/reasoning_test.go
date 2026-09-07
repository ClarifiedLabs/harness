package agent

import (
	"context"
	"encoding/json"
	"harness/internal/llm"
	"harness/internal/llm/llmtest"
	"harness/internal/tools"
	"testing"
)

func TestReasoningChangesPersistBaselineThroughResume(t *testing.T) {
	registry := llm.NewRegistry(map[string]llm.ModelInfo{"astra": {ReasoningUpdates: true, ContextWindow: 100000}})
	fp := llmtest.New("responses", summaryStep("first", 10, 1), summaryStep("second", 10, 1))
	opts := Options{Model: "astra", Registry: registry, ReasoningReplayDomain: "astra", Reasoning: llm.ReasoningConfig{Profile: "low"}}
	a := newAgent(fp, tools.Default(), opts)
	if err := a.RunPrompt(context.Background(), "start", &recordSink{}); err != nil {
		t.Fatal(err)
	}
	a.SetReasoning(llm.ReasoningConfig{Profile: "high"})
	if err := a.RunPrompt(context.Background(), "continue", &recordSink{}); err != nil {
		t.Fatal(err)
	}
	req := fp.Requests[1]
	if req.Reasoning.Profile != "low" || req.Messages[2].ReasoningState.Active.Profile != "high" {
		t.Fatalf("baseline/update: %+v", req)
	}
	data, err := json.Marshal(a.Transcript())
	if err != nil {
		t.Fatal(err)
	}
	var messages []llm.Message
	if err := json.Unmarshal(data, &messages); err != nil {
		t.Fatal(err)
	}
	resumed := newAgent(fp, tools.Default(), opts)
	resumed.SetTranscript(messages)
	resumed.SetReasoning(llm.ReasoningConfig{Profile: "medium"})
	snapshot := resumed.DebugRequest(true, "resume", nil, nil).Request
	state := snapshot.Messages[len(snapshot.Messages)-1].ReasoningState
	if state.Baseline.Profile != "low" || state.Active.Profile != "medium" {
		t.Fatalf("resumed state %+v", state)
	}
	resumed.SetReasoningReplayDomain("other")
	for _, m := range resumed.ContextRequest().Messages {
		if m.ReasoningState != nil {
			t.Fatal("state crossed replay domains")
		}
	}
}
