package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"testing"

	"harness/internal/execution"
	"harness/internal/llm"
	"harness/internal/llm/llmtest"
	"harness/internal/tools"
)

type executionTurnRecorder struct {
	*executionRecorder
	turns []execution.TurnEvent
}

func (r *executionTurnRecorder) ObserveTurn(event execution.TurnEvent) {
	r.mu.Lock()
	defer r.mu.Unlock()
	event.ToolNames = append([]string(nil), event.ToolNames...)
	r.turns = append(r.turns, event)
}

// Hiding optional sink methods models callers that only implement EventSink.
// Source diagnostics must not rely on a UI/recorder TurnProgress forwarder.
type executionBasicSink struct{ EventSink }

func executionTurnToolRun(_ context.Context, input json.RawMessage) (string, error) {
	return string(input), nil
}

type executionActivityTool struct{ *recordTool }

func (*executionActivityTool) Activity(json.RawMessage) tools.Activity {
	return tools.Activity{Class: tools.ActivityVerify, OperationCount: 3, Batched: true}
}

func TestExecutionTurnSourceParentAndChildWithoutProgressSink(t *testing.T) {
	for _, identity := range []execution.Identity{
		{Provider: "configured", Model: "m", Agent: "root"},
		{Provider: "configured", Model: "child-model", Agent: "reviewer", Delegate: "reviewer"},
	} {
		t.Run(identity.Agent, func(t *testing.T) {
			r := &executionTurnRecorder{executionRecorder: &executionRecorder{}}
			scope := execution.Scope{Observer: r, Identity: identity}
			reg := &tools.Registry{}
			reg.Register(&recordTool{name: "read", readOnly: true, run: executionTurnToolRun})
			reg.Register(&executionActivityTool{&recordTool{name: "shell", readOnly: true, run: executionTurnToolRun}})
			p := llmtest.New("fake", llmtest.Step{
				Events: []llm.StreamEvent{toolDone(0, "a", "read", `{"path":"private"}`), toolDone(1, "b", "shell", `{"command":"private"}`)},
				Usage:  llm.Usage{InputTokens: 7}, Stop: llm.StopToolUse,
			}, summaryStep("done", 11, 1), summaryStep("no tools", 13, 1))
			a := newAgent(p, reg, Options{Execution: scope})
			sink := executionBasicSink{&recordSink{}}
			if err := a.RunPrompt(context.Background(), "go", sink); err != nil {
				t.Fatal(err)
			}
			// A second, tool-less prompt must not add tools-per-turn diagnostics.
			if err := a.RunPrompt(context.Background(), "answer", sink); err != nil {
				t.Fatal(err)
			}
			want := []execution.TurnEvent{{Identity: identity, ToolNames: []string{"read", "shell"}, ToolCalls: 2, Operations: 4, SingleLookupCount: 1, Activity: "verify"}}
			if !reflect.DeepEqual(r.turns, want) {
				t.Fatalf("turns = %+v, want %+v", r.turns, want)
			}
			if got := r.usage().InputTokens; got != 31 {
				t.Fatalf("turn diagnostics changed billing: input = %d", got)
			}
			mustValid(t, a.Transcript())
		})
	}
}

func TestExecutionTurnEveryGuardOutcomeOnce(t *testing.T) {
	for _, test := range []struct {
		name        string
		turns       int
		termination TerminationReason
		steer       GuardSteerReason
	}{
		{"normal", 2, TerminationModelCompleted, ""},
		{"turn_budget", 2, TerminationTurnLimit, ""},
		{"token_budget", 2, TerminationTokenLimit, ""},
		{"cost_budget", 2, TerminationCostLimit, ""},
		{"repeat", repeatBreak, TerminationRepeatGuard, GuardSteerRepeat},
		{"error", errorStormBreak, TerminationErrorGuard, GuardSteerErrorStorm},
		{"command", commandRepeatBreak, TerminationRepeatGuard, GuardSteerCommandRepeat},
		{"inspection", semanticSteerThreshold, TerminationModelCompleted, GuardSteerPhaseTransition},
	} {
		t.Run(test.name, func(t *testing.T) {
			r := &executionTurnRecorder{executionRecorder: &executionRecorder{}}
			identity := execution.Identity{Provider: "configured", Model: "claude-opus-4-8", Agent: "child", Delegate: "child"}
			opts := Options{Execution: execution.Scope{Observer: r, Identity: identity}, Model: identity.Model, DisableAutoCompaction: true}
			switch test.name {
			case "turn_budget":
				opts.MaxTurns = test.turns
			case "token_budget":
				opts.MaxPromptTokens = 20
			case "cost_budget":
				opts.MaxPromptCostUSD = 0.00009
			}
			name := "read"
			if test.name == "error" {
				name = "probe"
			} else if test.name == "command" {
				name = "shell"
			}
			reg := &tools.Registry{}
			reg.Register(&recordTool{name: name, readOnly: true, run: func(_ context.Context, input json.RawMessage) (string, error) {
				if test.name == "error" {
					return "", fmt.Errorf("failed %s", input)
				}
				return string(input), nil
			}})
			var steps []llmtest.Step
			for i := 0; i < test.turns; i++ {
				input := fmt.Sprintf(`{"path":"file-%d"}`, i)
				if test.name == "repeat" {
					input = `{"path":"same"}`
				} else if test.name == "command" {
					input = fmt.Sprintf(`{"command":"go test ./pkg | head -%d"}`, i+1)
				}
				steps = append(steps, llmtest.Step{Events: []llm.StreamEvent{toolDone(0, fmt.Sprintf("call-%d", i), name, input)}, Usage: llm.Usage{InputTokens: 10, CostUSD: 0.00005, CostKnown: true}, Stop: llm.StopToolUse})
			}
			steps = append(steps, summaryStep("done", 10, 0))
			p := llmtest.New("fake", steps...)
			a := newAgent(p, reg, opts)
			sink := &recordSink{}
			if err := a.RunPrompt(context.Background(), "go", sink); err != nil {
				t.Fatal(err)
			}
			if len(r.turns) != test.turns || len(sink.progress) != test.turns {
				t.Fatalf("source/recorder turns = %d/%d, want %d", len(r.turns), len(sink.progress), test.turns)
			}
			steers := 0
			for i, progress := range sink.progress {
				want := execution.TurnEvent{Identity: identity, ToolNames: []string{name}, ToolCalls: progress.ToolCalls, Operations: progress.Operations, SingleLookupCount: progress.SingleLookupCount, InspectionNoProgressRun: progress.InspectionNoProgressRun, Activity: dominantActivity(progress.Activity), SteerReason: string(progress.SteerReason)}
				if !reflect.DeepEqual(r.turns[i], want) {
					t.Fatalf("turn %d source = %+v, recorder = %+v", i, r.turns[i], progress)
				}
				if test.steer != "" && r.turns[i].SteerReason == string(test.steer) {
					steers++
				}
			}
			if test.steer != "" && steers != 1 {
				t.Fatalf("%s steers = %d, want one", test.steer, steers)
			}
			if len(r.prompts) != 1 || r.prompts[0].Termination != string(test.termination) {
				t.Fatalf("prompt = %+v, want %s", r.prompts, test.termination)
			}
			if got := r.usage().InputTokens; got != p.RequestCount()*10 {
				t.Fatalf("turn diagnostics changed billing: %d across %d requests", got, p.RequestCount())
			}
			mustValid(t, a.Transcript())
		})
	}
}

func TestExecutionTurnReportedOnceAcrossBackgroundJoin(t *testing.T) {
	for _, waitErr := range []error{nil, context.Canceled} {
		t.Run(fmt.Sprint(waitErr), func(t *testing.T) {
			r := &executionTurnRecorder{executionRecorder: &executionRecorder{}}
			reg := &tools.Registry{}
			reg.Register(&recordTool{name: "read", readOnly: true, run: executionTurnToolRun})
			p := llmtest.New("fake", llmtest.Step{Events: []llm.StreamEvent{toolDone(0, "r", "read", `{"path":"file"}`)}, Stop: llm.StopToolUse}, textStep("joined"))
			a := newAgent(p, reg, Options{Execution: execution.Scope{Observer: r}})
			sink := &promptWorkSink{pending: true, waitErr: waitErr}
			if err := a.RunPrompt(context.Background(), "go", sink); !errors.Is(err, waitErr) {
				t.Fatalf("run = %v, want %v", err, waitErr)
			}
			if len(r.turns) != 1 || len(sink.recordSink.progress) != 1 || r.turns[0].ToolCalls != 1 || r.turns[0].Activity != "inspect" {
				t.Fatalf("turns = %+v, recorder = %+v", r.turns, sink.recordSink.progress)
			}
			mustValid(t, a.Transcript())
		})
	}
}

func TestExecutionTurnObserverOptionalAndActivityTies(t *testing.T) {
	for _, test := range []struct {
		counts ToolActivityCounts
		want   string
	}{
		{ToolActivityCounts{}, "inspect"},
		{ToolActivityCounts{Inspect: 2, Mutate: 2}, "inspect"},
		{ToolActivityCounts{Inspect: 1, Mutate: 2, Verify: 2}, "mutate"},
		{ToolActivityCounts{Verify: 3, Wait: 3}, "verify"},
		{ToolActivityCounts{Wait: 4, Coordinate: 4}, "wait"},
		{ToolActivityCounts{Coordinate: 5, Other: 5}, "coordinate"},
		{ToolActivityCounts{Inspect: 1, Other: 6}, "other"},
	} {
		if got := dominantActivity(test.counts); got != test.want {
			t.Errorf("dominantActivity(%+v) = %q, want %q", test.counts, got, test.want)
		}
	}
	// The pre-existing observer intentionally does not implement TurnObserver.
	r := &executionRecorder{}
	sink := &recordSink{}
	progress := TurnProgress{ToolCalls: 1, Operations: 1}
	reportTurnProgress(r.scope("m"), sink, progress, []llm.ToolCall{{Name: "read"}})
	if len(sink.progress) != 1 || !reflect.DeepEqual(sink.progress[0], progress) || len(r.models) != 0 {
		t.Fatalf("optional observer altered diagnostics/billing: %+v", sink.progress)
	}
}
