package ui

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"harness/internal/agent"
	"harness/internal/llm"
	"harness/internal/llm/llmtest"
	"harness/internal/session"
	"harness/internal/sessionrec"
)

// readRawEvents decodes a session raw.ndjson log for parity comparison.
func readRawEvents(t *testing.T, dir string) []session.Event {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(dir, "raw.ndjson"))
	if err != nil {
		t.Fatalf("read raw.ndjson: %v", err)
	}
	var events []session.Event
	for _, line := range splitLines(string(data)) {
		if line == "" {
			continue
		}
		var ev session.Event
		if err := json.Unmarshal([]byte(line), &ev); err != nil {
			t.Fatalf("decode event %q: %v", line, err)
		}
		events = append(events, ev)
	}
	return events
}

func splitLines(s string) []string {
	var out []string
	start := 0
	for i := 0; i < len(s); i++ {
		if s[i] == '\n' {
			out = append(out, s[start:i])
			start = i + 1
		}
	}
	if start < len(s) {
		out = append(out, s[start:])
	}
	return out
}

// TestChildParentRawEventParity drives one scripted run through the parent
// sink path (accumulatingSink) and through a recorder configured exactly like
// a delegate child session, then pins that both raw.ndjson logs are identical.
// Delegate childSink recording is a thin pass-through to the same recorder, so
// this is the property that keeps child and parent session logs at the same
// fidelity by construction.
func TestChildParentRawEventParity(t *testing.T) {
	parentDir := filepath.Join(t.TempDir(), "parent")
	childDir := filepath.Join(t.TempDir(), "child")
	models := map[string]llm.ModelInfo{
		"anthropic:claude-opus-4-8": {Price: llm.Price{Input: 5, Output: 25}},
	}
	// A constant clock makes every recorded timestamp and duration equal.
	now := func() time.Time { return time.Date(2026, 7, 29, 12, 0, 0, 0, time.UTC) }

	var out, errw lockedBuffer
	fp := llmtest.New("fake")
	app := newTestApp(t, &out, &errw, fp)
	app.SessionPath = parentDir
	app.Now = now
	app.Reasoning = llm.ReasoningConfig{Summary: "auto"}
	app.Registry = llm.NewRegistryWithQualified(nil, models)
	app.Renderer.cwd = parentDir

	parent := newAccumulatingSink(app.Renderer, app, 1)

	childRegistry := llm.NewRegistryWithQualified(nil, models)
	child := sessionrec.New(sessionrec.Config{
		Dir:                childDir,
		Prompt:             1,
		Agent:              app.AgentName,
		ModelTarget:        "anthropic:claude-opus-4-8",
		Provider:           "anthropic",
		Model:              "claude-opus-4-8",
		Clock:              now,
		ReasoningSummaries: true,
		CWD:                parentDir,
		PriceTurnUsage: func(u llm.Usage) (float64, bool) {
			return childRegistry.Cost("anthropic:claude-opus-4-8", u)
		},
		PricePromptUsage: func(u llm.Usage) (float64, bool) {
			if u.CostKnown {
				return u.CostUSD, true
			}
			return childRegistry.Cost("anthropic:claude-opus-4-8", u)
		},
	})

	// The scripted run. Every call pair must produce byte-identical events.
	app.recordEvent(session.Event{Type: session.EventUser, Prompt: 1, Text: "task"})
	child.User("task")

	ctx := agent.ContextEstimate{Total: 1200, Window: 128000, System: 500, Tools: 300, Messages: 400}
	parent.TurnAttemptStart(1, 1, ctx)
	child.TurnAttemptStart(1, 1, ctx)

	parent.TextDelta("working on it")
	child.TextDelta("working on it")

	parent.AssistantPhase(llm.AssistantPhaseCommentary)
	child.AssistantPhase(llm.AssistantPhaseCommentary)

	parent.ReasoningSummary("checking the code")
	child.ReasoningSummary("checking the code")

	parent.TurnAttemptComplete(agent.TurnAttemptUsage{Turn: 1, Attempt: 1, Usage: llm.Usage{InputTokens: 900, OutputTokens: 40}})
	child.TurnAttemptComplete(agent.TurnAttemptUsage{Turn: 1, Attempt: 1, Usage: llm.Usage{InputTokens: 900, OutputTokens: 40}})

	// The read input carries an absolute path under the parent cwd; both sides
	// must relativize it identically (path=a.go) in the recorded Display line.
	callInput, err := json.Marshal(map[string]string{"path": filepath.Join(parentDir, "a.go")})
	if err != nil {
		t.Fatal(err)
	}
	call := llm.ToolCall{ID: "call-1", Name: "read", Input: callInput}
	parent.ToolStart(call)
	child.ToolStart(call)

	result := llm.ToolResult{ForID: "call-1", Text: "package a\n\nfunc A() {}\n"}
	parent.ToolResult(result)
	child.ToolResult(result)

	diff := "--- a/a.go\n+++ b/a.go\n@@ -1,1 +1,1 @@\n-old\n+new\n"
	parent.ToolDiff(call, "a.go", diff)
	child.ToolDiff(call, "a.go", diff)

	parent.Notice("[notice: context at 80%]")
	child.Notice("[notice: context at 80%]", 1)

	requestEvent := llm.ModelRequestEvent{
		State:      llm.ModelRequestUpstreamAttemptFailed,
		StatusCode: 500,
		Message:    "upstream exploded",
		Outcome:    llm.ModelRequestOutcomeRetrying,
	}
	parent.ModelRequestEvent(requestEvent)
	child.ModelRequestEvent(requestEvent)

	// Unpriced turn usage: both sides price against the registry.
	turn := agent.TurnUsage{Turn: 1, Context: ctx, Usage: llm.Usage{InputTokens: 900, OutputTokens: 40}}
	parent.TurnComplete(turn)
	child.TurnComplete(turn)

	// Provider-priced prompt usage: both sides keep the streamed cost.
	prompt := agent.PromptUsage{
		Turns:   1,
		Context: ctx,
		Usage:   llm.Usage{InputTokens: 900, OutputTokens: 40, CostUSD: 0.0055, CostKnown: true},
	}
	parent.PromptComplete(prompt)
	child.PromptComplete(prompt)

	parent.FlushEvents()
	child.Flush()

	parentEvents := readRawEvents(t, parentDir)
	childEvents := readRawEvents(t, childDir)
	if len(parentEvents) != len(childEvents) {
		t.Fatalf("event count: parent %d, child %d\nparent: %+v\nchild: %+v", len(parentEvents), len(childEvents), parentEvents, childEvents)
	}
	for i := range parentEvents {
		if !reflect.DeepEqual(parentEvents[i], childEvents[i]) {
			t.Fatalf("event %d (%s) differs:\nparent: %+v\nchild:  %+v", i, parentEvents[i].Type, parentEvents[i], childEvents[i])
		}
	}
	if got := parentEvents[len(parentEvents)-1].Type; got != session.EventPromptUsage {
		t.Fatalf("last event = %q, want prompt_usage", got)
	}
	if got := parentEvents[len(parentEvents)-1].Display; got == "" {
		t.Fatal("prompt_usage display missing from parent-fidelity recording")
	}
}
