package agent

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"testing"

	"harness/internal/llm"
	"harness/internal/llm/llmtest"
	"harness/internal/tools"
)

func reasoningCompactionAgent(t *testing.T, notes bool) (*Agent, *nativeCompactionProvider) {
	t.Helper()
	p := &nativeCompactionProvider{
		FakeProvider: llmtest.New("responses", summaryStep("after reset", 10, 1)),
		result:       llm.CompactedContext{Items: nativeCompactedItems()},
	}
	opts := Options{
		Model: "astra", Registry: llm.NewRegistry(map[string]llm.ModelInfo{"astra": {ReasoningUpdates: true, ContextWindow: 100_000}}),
		ReasoningReplayDomain: "astra", Reasoning: llm.ReasoningConfig{Effort: "high", Summary: "concise"},
		NativeCompaction: true, DisableAutoCompaction: true,
	}
	a := newAgent(p, tools.Default(), opts)
	if notes {
		a, _, _, _ = notesTestAgent(t, p, opts)
	}
	messages := makeTurns(10)
	messages[0].ReasoningState = &llm.ReasoningState{
		ReplayDomain: "astra", Baseline: llm.ReasoningConfig{Effort: "low"}, Active: llm.ReasoningConfig{Effort: "low"},
	}
	a.SetTranscript(messages)
	return a, p
}

func TestCompactionEstablishesSelectedReasoningBaseline(t *testing.T) {
	for _, notes := range []bool{false, true} {
		name := "native"
		if notes {
			name = "notes"
		}
		t.Run(name, func(t *testing.T) {
			a, p := reasoningCompactionAgent(t, notes)
			original := a.Transcript()
			if _, err := a.Compact(context.Background(), &recordSink{}); err != nil {
				t.Fatal(err)
			}
			if !notes {
				if len(p.requests) != 1 || p.requests[0].Reasoning.Effort != "low" || p.requests[0].Reasoning.Summary != "concise" {
					t.Fatalf("compaction must use the old baseline with current summary: %+v", p.requests)
				}
				if !reflect.DeepEqual(a.Transcript()[:len(original)], original) {
					t.Fatal("native reset modified semantic history needed for fallback")
				}
			}
			if got := a.requestReasoning(); got != a.reasoning {
				t.Fatalf("successful reset retained old baseline: %+v, want %+v", got, a.reasoning)
			}
			if err := a.RunPrompt(context.Background(), "continue", &recordSink{}); err != nil {
				t.Fatal(err)
			}
			if got := p.Requests[0].Reasoning; got != a.reasoning {
				t.Fatalf("next request: %+v", got)
			}
			if err := llm.ValidateTranscript(a.Transcript()); err != nil {
				t.Fatal(err)
			}

			// Resume must preserve the new baseline, then represent later effort
			// changes as updates rather than resurrecting the pre-reset baseline.
			data, err := json.Marshal(a.Transcript())
			if err != nil {
				t.Fatal(err)
			}
			var saved []llm.Message
			if err := json.Unmarshal(data, &saved); err != nil {
				t.Fatal(err)
			}
			resumed, resumedProvider := reasoningCompactionAgent(t, notes)
			resumed.SetTranscript(saved)
			resumed.SetReasoning(llm.ReasoningConfig{Effort: "medium", Summary: "concise"})
			state := resumed.newReasoningState()
			if state.Baseline.Effort != "high" || state.Active.Effort != "medium" {
				t.Fatalf("resumed baseline/update: %+v", state)
			}
			if err := resumed.RunPrompt(context.Background(), "resume", &recordSink{}); err != nil {
				t.Fatal(err)
			}
			req := resumedProvider.Requests[0]
			last := req.Messages[len(req.Messages)-1].ReasoningState
			if req.Reasoning.Effort != "high" || last == nil || last.Active.Effort != "medium" || last.Baseline.Effort != "high" {
				t.Fatalf("resumed request baseline/update: %+v / %+v", req.Reasoning, last)
			}
		})
	}
}

func TestFailedNativeCompactionPreservesReasoningBaseline(t *testing.T) {
	for _, tc := range []struct {
		name  string
		err   error
		items []json.RawMessage
	}{
		{name: "unsupported", err: llm.ErrContextCompactionUnsupported},
		{name: "temporary", err: &llm.APIError{StatusCode: 503, Message: "unavailable"}},
		{name: "invalid checkpoint", items: []json.RawMessage{json.RawMessage(`not-json`)}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a, p := reasoningCompactionAgent(t, false)
			before := a.Transcript()
			p.err, p.result.Items = tc.err, tc.items
			_, changed, _, err := a.compactNative(context.Background(), &recordSink{}, "manual", 1000)
			if err != nil || changed {
				t.Fatalf("changed=%v err=%v", changed, err)
			}
			if got := a.requestReasoning(); got.Effort != "low" || got.Summary != "concise" {
				t.Fatalf("failure changed baseline: %+v", got)
			}
			if !reflect.DeepEqual(a.Transcript(), before) {
				t.Fatal("failure changed history")
			}
		})
	}
}

// A request baseline belongs to the history being replayed, not to the selected
// effort. If an old checkpoint is discarded, replay must restore the semantic
// history's baseline and explicitly carry the current effort as an update.
func TestNativeCompactionFallbackPreservesSelectedReasoning(t *testing.T) {
	for _, rejection := range []bool{false, true} {
		name := "repeat compaction and textual fallback fail"
		if rejection {
			name = "sampling rejects checkpoint"
		}
		t.Run(name, func(t *testing.T) {
			a, p := reasoningCompactionAgent(t, false)
			if _, err := a.Compact(context.Background(), &recordSink{}); err != nil {
				t.Fatal(err)
			}
			if got := a.requestReasoning().Effort; got != "high" {
				t.Fatalf("new baseline=%s", got)
			}
			a.SetReasoning(llm.ReasoningConfig{Effort: "medium", Summary: "concise"})
			if rejection {
				p.FakeProvider = llmtest.New("responses",
					llmtest.Step{Err: &llm.APIError{StatusCode: 400, Code: "invalid_encrypted_content", Message: "checkpoint rejected"}},
					summaryStep("semantic recovery", 10, 1))
			} else {
				p.err = &llm.APIError{StatusCode: 503, Message: "unavailable"}
				fallbackErr := &llm.APIError{StatusCode: 400, Code: "invalid_request_error", Message: "summary rejected"}
				p.FakeProvider = llmtest.New("responses", llmtest.Step{Err: fallbackErr}, summaryStep("semantic recovery", 10, 1))
				if _, err := a.Compact(context.Background(), &recordSink{}); !errors.Is(err, fallbackErr) {
					t.Fatalf("fallback error=%v", err)
				}
				if got := p.requests[len(p.requests)-1].Reasoning.Effort; got != "high" {
					t.Fatalf("repeat compaction baseline=%s", got)
				}
			}
			if err := a.RunPrompt(context.Background(), "continue after fallback", &recordSink{}); err != nil {
				t.Fatal(err)
			}
			req := p.Requests[len(p.Requests)-1]
			if hasProviderCompaction(req.Messages) {
				t.Fatal("fallback replayed discarded checkpoint")
			}
			last := req.Messages[len(req.Messages)-1].ReasoningState
			if req.Reasoning.Effort != "low" || a.requestReasoning().Effort != "low" || last == nil || last.Active.Effort != "medium" || a.reasoning.Effort != "medium" {
				t.Fatalf("semantic fallback lost baseline or selected effort: %+v / %+v", req.Reasoning, last)
			}
			if err := llm.ValidateTranscript(a.Transcript()); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestFailedNotesCompactionPreservesReasoningBaseline(t *testing.T) {
	a, _ := reasoningCompactionAgent(t, true)
	want := errors.New("archive unavailable")
	a.SetCompactionArchiver(func(context.Context, CompactionArchive) (string, error) { return "", want })
	if _, err := a.Compact(context.Background(), &recordSink{}); !errors.Is(err, want) {
		t.Fatalf("error=%v", err)
	}
	if got := a.requestReasoning(); got.Effort != "low" {
		t.Fatalf("failure changed baseline: %+v", got)
	}
}
