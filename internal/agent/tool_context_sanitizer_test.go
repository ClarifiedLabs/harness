package agent

import (
	"context"
	"reflect"
	"testing"

	"harness/internal/llm"
	"harness/internal/llm/llmtest"
)

func TestIncrementalToolContextSanitizedAcrossCompaction(t *testing.T) {
	const unsafeName = "\x1b[31mcurrent\x1b[0m"
	// Model the host sanitizer without importing a transport implementation.
	sanitize := func(messages []llm.Message) ([]llm.Message, bool) {
		out := llm.CloneMessages(messages)
		changed := false
		for i := range out {
			if event := out[i].ToolContext; event != nil {
				for j := range event.Tools {
					if event.Tools[j].Name == unsafeName {
						event.Tools[j].Name = "current"
						changed = true
					}
				}
			}
		}
		return out, changed
	}

	for _, mode := range []string{"text", "native", "notes"} {
		t.Run(mode, func(t *testing.T) {
			p := &nativeCompactionProvider{FakeProvider: llmtest.New("responses", summaryStep("summary", 0, 0)), result: llm.CompactedContext{Items: nativeCompactedItems()}}
			opts := incrementalAgentOptions()
			opts.NativeCompaction = mode == "native"
			registry := incrementalRegistry(unsafeName)
			a := newAgent(p, registry, opts)
			if mode == "notes" {
				a, registry, _, _ = notesTestAgent(t, p, opts)
				registry.Register(&recordTool{name: unsafeName})
				a.SetTools(registry)
			}
			// Install after notesTestAgent, which replaces the agent instance.
			a.SetTranscriptSanitizer(sanitize)
			if a.transcriptSanitizer == nil {
				t.Fatal("transcript sanitizer not installed")
			}
			rawSpecs := llm.CloneToolSchemas(a.toolSpecs)
			wantTools := a.requestToolSpecs()
			for i := range wantTools {
				if wantTools[i].Name == unsafeName {
					wantTools[i].Name = "current"
				}
			}
			messages := makeTurns(10)
			messages[0].ToolContext = &llm.ToolContext{ReplayDomain: "test-domain", Initial: true, Tools: []llm.ToolSchema{{Name: "obsolete"}}}
			messages[len(messages)-1].ToolContext = &llm.ToolContext{ReplayDomain: "test-domain", After: true, Tools: []llm.ToolSchema{{Name: "outdated_suffix"}}}
			a.SetTranscript(messages)
			if _, changed, err := a.compactInternal(context.Background(), &recordSink{}, compactOptions{trigger: "manual"}); err != nil || !changed {
				t.Fatalf("compaction: changed=%v err=%v", changed, err)
			}

			// Inspect committed history before ContextRequest or another request can
			// sanitize it and hide an unsafe replacement baseline.
			transcript := a.Transcript()
			start := a.toolContextStart(transcript) // Native history retains its pre-checkpoint prefix.
			if len(transcript) == 0 || transcript[start].ToolContext == nil {
				t.Fatal("compaction did not install a tool-context baseline")
			}
			baseline := transcript[start].ToolContext
			if !baseline.Initial || baseline.After || baseline.ReplayDomain != "test-domain" || !reflect.DeepEqual(baseline.Tools, wantTools) {
				t.Error("compaction baseline does not match the sanitized initial catalog")
			}
			for _, message := range transcript[start+1:] {
				if message.ToolContext != nil {
					t.Error("old suffix event survived compaction")
				}
			}
			if err := llm.ValidateTranscript(transcript); err != nil {
				t.Fatal(err)
			}

			// Also isolate diffing against a clean baseline, even when the assertion
			// above fails. An unchanged raw catalog must not reintroduce unsafe names.
			before := llm.CloneMessages(transcript)
			clean, _ := sanitize(transcript)
			for i := 0; i < 2; i++ {
				projected, changed := a.prepareToolContext(clean)
				if changed || !reflect.DeepEqual(projected, clean) {
					t.Errorf("prepareToolContext pass %d emitted a delta for an unchanged sanitized catalog", i+1)
				}
			}
			if !reflect.DeepEqual(a.Transcript(), before) {
				t.Error("prepareToolContext mutated committed history")
			}
			if !reflect.DeepEqual(a.toolSpecs, rawSpecs) || !reflect.DeepEqual(a.requestToolSpecs(), rawSpecs) {
				t.Error("sanitizing tool context mutated raw tool specs")
			}
		})
	}
}
