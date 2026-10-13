package main

import (
	"context"
	"encoding/json"
	"reflect"
	"testing"

	"harness/internal/agent"
	"harness/internal/delegate"
	"harness/internal/llm"
	"harness/internal/llm/llmtest"
	"harness/internal/session"
	"harness/internal/tools"
)

type acpToolContextTool struct{ spec llm.ToolSchema }

func (tool acpToolContextTool) Name() string                  { return tool.spec.Name }
func (tool acpToolContextTool) Description() string           { return tool.spec.Description }
func (tool acpToolContextTool) Schema() json.RawMessage       { return tool.spec.Parameters }
func (tool acpToolContextTool) ReadOnly(json.RawMessage) bool { return true }
func (tool acpToolContextTool) Run(context.Context, json.RawMessage) (string, error) {
	return "ok", nil
}

func acpUnsafeToolContextSpec(name string) llm.ToolSchema {
	return llm.ToolSchema{
		Name: name, Description: "\x1b]0;title\x07description",
		Parameters: json.RawMessage(`{"\u001b[31mtype":"object","description":"\u001b]0;title\u0007safe"}`),
	}
}

func TestACPDelegateSanitizesToolContextBeforePersistence(t *testing.T) {
	parentDir := t.TempDir()
	childDir := session.ChildSessionDir(parentDir, "child")
	catalog := &tools.Registry{}
	catalog.Register(acpToolContextTool{acpUnsafeToolContextSpec("read")})
	original := llm.CloneToolSchemas(catalog.Specs())
	assertSaved := func(wantResponseID string) {
		t.Helper()
		stored, err := session.Load(childDir)
		if err != nil {
			t.Fatal(err)
		}
		if err := llm.ValidateTranscript(stored.Messages); err != nil {
			t.Fatal(err)
		}
		if _, changed := sanitizeACPTranscript(stored.Messages); changed {
			t.Error("child persisted unsanitized tool context")
		}
		contexts := 0
		for _, message := range stored.Messages {
			if message.ToolContext != nil {
				contexts++
			}
		}
		if contexts != 1 || stored.Messages[0].ToolContext == nil || !stored.Messages[0].ToolContext.Initial {
			t.Errorf("saved tool contexts = %d, want one initial baseline", contexts)
		}
		if wantResponseID != "" {
			state := stored.ResponseState
			if state == nil || state.PreviousResponseID != wantResponseID || state.AnchorMessages > len(stored.Messages) ||
				!llm.MatchesMessageFingerprint(stored.Messages[:state.AnchorMessages], state.AnchorDigest) {
				t.Errorf("saved continuation does not match sanitized history: %+v", state)
			}
		}
	}
	provider := llmtest.New("fake",
		llmtest.Step{
			Events: []llm.StreamEvent{{Kind: llm.EventToolCallDone, ToolID: "read-1", ToolName: "read", ToolInput: json.RawMessage(`{}`)}},
			Stop:   llm.StopToolUse, ResponseID: "r1",
			// The request-boundary checkpoint is already on disk. Inspect it
			// synchronously before this response can trigger another request.
			Block: func(context.Context) { assertSaved("") },
		},
		llmtest.Step{
			Events: []llm.StreamEvent{{Kind: llm.EventTextDelta, Text: "done"}},
			Stop:   llm.StopEndTurn, ResponseID: "r2",
			Block: func(context.Context) { assertSaved("r1") },
		},
	)
	models := llm.NewRegistry(map[string]llm.ModelInfo{"fake:model": {ContextWindow: 100_000, IncrementalTools: true}})
	state := delegate.NewState(delegate.Runtime{
		Provider: provider, ProviderName: "fake", Model: "fake:model", Registry: models,
		ContextWindow: 100_000, SessionPath: parentDir, CacheAffinityID: "parent-cache",
	})
	runner := delegate.NewRunner(state.Snapshot, func(delegate.Runtime, string) (delegate.Launch, error) {
		return acpDelegateLaunch(delegate.Launch{
			Provider: provider, ProviderName: "fake", Model: "fake:model", Registry: models,
			ContextWindow: 100_000, Agent: "worker", Tools: catalog,
			ReasoningReplayDomain: "test-domain", ResponsesStateful: true,
		}), nil
	}, delegate.Options{MaxTurns: 4, DisableAutoCompaction: true})
	if _, err := runner.Run(context.Background(), delegate.RunRequest{Task: "test", ChildID: "child"}, nil); err != nil {
		t.Fatal(err)
	}
	assertSaved("r2")
	if len(provider.Requests) != 2 || provider.Requests[1].PreviousResponseID != "r1" {
		t.Fatal("sanitizing new tool context discarded the continuation")
	}
	if !reflect.DeepEqual(catalog.Specs(), original) {
		t.Fatal("sanitization mutated the registered tool schemas")
	}
	// Inspect the durable tree as well as the active/consolidated snapshots.
	tree, err := session.LoadTree(childDir, "")
	if err != nil {
		t.Fatal(err)
	}
	messages, err := tree.BuildContext()
	if err != nil {
		t.Fatal(err)
	}
	if _, changed := sanitizeACPTranscript(messages); changed {
		t.Fatal("canonical child tree retained unsafe tool context")
	}
}

func TestACPToolContextSanitizesDeltasWithoutRepeatingCatalog(t *testing.T) {
	provider := llmtest.New("fake",
		llmtest.Step{Stop: llm.StopEndTurn, ResponseID: "r1"},
		llmtest.Step{Stop: llm.StopEndTurn, ResponseID: "r2"},
		llmtest.Step{Stop: llm.StopEndTurn, ResponseID: "r3"},
	)
	catalog := func(name string) *tools.Registry {
		r := &tools.Registry{}
		r.Register(acpToolContextTool{acpUnsafeToolContextSpec(name)})
		return r
	}
	a := agent.New(provider, catalog("\x1b[31mold\x1b[0m"), agent.Options{
		Model: "fake:model", ReasoningReplayDomain: "test-domain", ResponsesStateful: true,
		DisableAutoCompaction: true,
		Registry:              llm.NewRegistry(map[string]llm.ModelInfo{"fake:model": {ContextWindow: 100_000, IncrementalTools: true}}),
	})
	a.SetTranscriptSanitizer(sanitizeACPTranscript)
	a.SetRequestSanitizer(sanitizeACPRequest)
	for i := range 3 {
		if i == 1 {
			a.SetTools(catalog("\x1b[31mnew\x1b[0m"))
		}
		if err := a.RunPrompt(context.Background(), "continue", &contextDialectSink{}); err != nil {
			t.Fatal(err)
		}
		if _, changed := sanitizeACPTranscript(a.Transcript()); changed {
			t.Errorf("round %d retained unsafe tool context", i)
		}
		if err := llm.ValidateTranscript(a.Transcript()); err != nil {
			t.Fatal(err)
		}
	}
	if len(provider.Requests) != 3 || provider.Requests[1].PreviousResponseID != "r1" || provider.Requests[2].PreviousResponseID != "r2" {
		t.Fatal("tool-context sanitization invalidated continuation")
	}
	second, third := provider.Requests[1], provider.Requests[2]
	if len(second.Messages) != 2 {
		t.Fatalf("delta request has %d messages, want prompt and catalog delta", len(second.Messages))
	}
	delta := second.Messages[1].ToolContext
	if delta == nil || !delta.After || len(delta.Tools) != 1 || delta.Tools[0].Name != "new" || !reflect.DeepEqual(delta.Removed, []string{"old"}) {
		t.Fatalf("sanitized delta = %+v", delta)
	}
	if len(third.Messages) != 1 || third.Messages[0].ToolContext != nil {
		t.Fatal("unchanged sanitized catalog emitted another delta")
	}
}

func TestACPTranscriptSanitizesToolContext(t *testing.T) {
	for _, test := range []struct {
		name    string
		context *llm.ToolContext
		want    *llm.ToolContext
	}{
		{
			name:    "tool name CSI",
			context: &llm.ToolContext{Tools: []llm.ToolSchema{{Name: "\x1b[31mprobe\x1b[0m"}}},
			want:    &llm.ToolContext{Tools: []llm.ToolSchema{{Name: "probe"}}},
		},
		{
			name:    "description OSC",
			context: &llm.ToolContext{Tools: []llm.ToolSchema{{Name: "probe", Description: "\x1b]0;title\x07description"}}},
			want:    &llm.ToolContext{Tools: []llm.ToolSchema{{Name: "probe", Description: "description"}}},
		},
		{
			name:    "schema keys and values",
			context: &llm.ToolContext{Tools: []llm.ToolSchema{{Name: "probe", Parameters: json.RawMessage(`{"\u001b[31mtype":"object","description":"\u001b]0;title\u0007safe"}`)}}},
			want:    &llm.ToolContext{Tools: []llm.ToolSchema{{Name: "probe", Parameters: json.RawMessage(`{"description":"safe","type":"object"}`)}}},
		},
		{
			name:    "removed names CSI and OSC",
			context: &llm.ToolContext{Removed: []string{"\x1b[31mold\x1b[0m", "\x1b]0;title\x1b\\other"}},
			want:    &llm.ToolContext{Removed: []string{"old", "other"}},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			test.context.ReplayDomain = "responses:example"
			test.context.Initial = true
			test.context.After = true
			test.want.ReplayDomain = test.context.ReplayDomain
			test.want.Initial = true
			test.want.After = true
			original := llm.CloneToolContext(test.context)
			messages := []llm.Message{{Role: llm.RoleUser, ToolContext: test.context}}
			clean, changed := sanitizeACPTranscript(messages)
			if !changed {
				t.Fatal("unsafe tool context was not detected")
			}
			if !reflect.DeepEqual(clean[0].ToolContext, test.want) {
				t.Fatalf("sanitized context = %#v, want %#v", clean[0].ToolContext, test.want)
			}
			if !reflect.DeepEqual(test.context, original) {
				t.Fatal("sanitizer mutated original metadata or schema")
			}
			again, changed := sanitizeACPTranscript(clean)
			if changed || !reflect.DeepEqual(again, clean) {
				t.Fatalf("sanitization was not idempotent: changed=%v", changed)
			}
		})
	}
}

func TestACPTranscriptClonesSafeToolContext(t *testing.T) {
	context := &llm.ToolContext{
		ReplayDomain: "responses:example",
		Initial:      true,
		After:        true,
		Tools:        []llm.ToolSchema{{Name: "probe", Description: "safe", Parameters: json.RawMessage(`{"type":"object"}`)}},
		Removed:      []string{"old"},
	}
	original := llm.CloneToolContext(context)
	clean, changed := sanitizeACPTranscript([]llm.Message{{Role: llm.RoleUser, ToolContext: context}})
	if changed || !reflect.DeepEqual(clean[0].ToolContext, original) {
		t.Fatal("safe tool context was changed")
	}
	cloned := clean[0].ToolContext
	cloned.ReplayDomain = "other"
	cloned.Initial = false
	cloned.After = false
	cloned.Tools[0].Name = "other"
	cloned.Tools[0].Description = "other"
	cloned.Tools[0].Parameters[0] = '['
	cloned.Removed[0] = "other"
	if !reflect.DeepEqual(context, original) {
		t.Fatal("sanitized metadata or schema aliases the original")
	}
}

func TestACPTranscriptPreservesToolContextMetadata(t *testing.T) {
	for _, context := range []*llm.ToolContext{
		nil,
		{},
		{ReplayDomain: "\x1b[31m", Initial: true, After: true},
		{Tools: []llm.ToolSchema{}, Removed: []string{}},
	} {
		clean, changed := sanitizeACPTranscript([]llm.Message{{Role: llm.RoleUser, ToolContext: context}})
		if changed || !reflect.DeepEqual(clean[0].ToolContext, context) {
			t.Fatalf("non-model-facing metadata changed: got %#v, want %#v, changed=%v", clean[0].ToolContext, context, changed)
		}
	}
}
