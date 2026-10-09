package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"

	"harness/internal/llm"
	"harness/internal/llm/llmtest"
	"harness/internal/session"
	"harness/internal/tools"
)

func incrementalAgentOptions() Options {
	return Options{
		Model: "test-model", ReasoningReplayDomain: "test-domain", ResponsesStateful: true,
		DisableAutoCompaction: true,
		Registry:              llm.NewRegistry(map[string]llm.ModelInfo{"test-model": {IncrementalTools: true, ContextWindow: 100_000}}),
	}
}

func incrementalRegistry(names ...string) *tools.Registry {
	r := &tools.Registry{}
	for _, name := range names {
		r.Register(&recordTool{name: name, run: func(context.Context, json.RawMessage) (string, error) { return "ok", nil }})
	}
	return r
}

func TestIncrementalToolsPreserveContinuationAndRoundTrip(t *testing.T) {
	first := summaryStep("first", 0, 0)
	first.ResponseID = "r1"
	second := summaryStep("second", 0, 0)
	second.ResponseID = "r2"
	p := llmtest.New("responses", first, second, summaryStep("third", 0, 0))
	a := newAgent(p, incrementalRegistry("a", "remove"), incrementalAgentOptions())
	before := llm.CloneMessages(a.Transcript())
	preview := a.DebugRequest(true, "begin", nil, nil).Request
	if !preview.IncrementalTools || preview.Messages[0].ToolContext == nil || !reflect.DeepEqual(before, a.Transcript()) {
		t.Fatalf("debug projection missing or mutated live history: %+v", preview)
	}
	if err := a.RunPrompt(context.Background(), "begin", &recordSink{}); err != nil {
		t.Fatal(err)
	}
	if !p.Requests[0].IncrementalTools || !p.Requests[0].Messages[0].ToolContext.Initial {
		t.Fatalf("first request: %+v", p.Requests[0])
	}
	prefix := llm.CloneMessages(a.Transcript())
	a.SetTools(incrementalRegistry("a", "added"))
	if a.responseState.PreviousResponseID != "r1" {
		t.Fatal("tool changes discarded the compatible continuation")
	}
	if err := a.RunPrompt(context.Background(), "continue", &recordSink{}); err != nil {
		t.Fatal(err)
	}
	req := p.Requests[1]
	if !req.IncrementalTools || req.PreviousResponseID != "r1" || len(req.Messages) != 2 {
		t.Fatalf("delta request: %+v", req)
	}
	delta := req.Messages[1].ToolContext
	if delta == nil || delta.Initial || !delta.After || len(delta.Tools) != 1 || delta.Tools[0].Name != "added" || !reflect.DeepEqual(delta.Removed, []string{"remove"}) {
		t.Fatalf("delta = %+v", delta)
	}
	if !reflect.DeepEqual(a.Transcript()[:len(prefix)], prefix) {
		t.Fatal("catalog change rewrote the earlier prefix")
	}
	if err := llm.ValidateTranscript(a.Transcript()); err != nil {
		t.Fatal(err)
	}

	// The canonical tree, not a separate mutable catalog file, carries the state.
	tree := session.NewTree(time.Now(), "", "", "")
	if err := tree.SyncTranscript(a.Transcript()); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	if err := tree.Save(dir); err != nil {
		t.Fatal(err)
	}
	loaded, err := session.LoadTree(dir, tree.ActiveLeaf)
	if err != nil {
		t.Fatal(err)
	}
	messages, err := loaded.BuildContext()
	if err != nil {
		t.Fatal(err)
	}
	resumed := newAgent(p, incrementalRegistry("a", "added"), incrementalAgentOptions())
	resumed.SetTranscript(messages)
	resumed.SetResponseState(a.ResponseState())
	if err := resumed.RunPrompt(context.Background(), "after resume", &recordSink{}); err != nil {
		t.Fatal(err)
	}
	if req := p.Requests[2]; req.PreviousResponseID != "r2" || !req.IncrementalTools || len(req.Messages) != 1 || req.Messages[0].ToolContext != nil {
		t.Fatalf("unchanged resumed catalog was repeated: %+v", req)
	}
}

func TestIncrementalToolsRefreshAtClosedToolBoundary(t *testing.T) {
	r := &tools.Registry{}
	r.Register(&recordTool{name: "activate", run: func(context.Context, json.RawMessage) (string, error) {
		r.Register(&recordTool{name: "new_tool"})
		return "activated", nil
	}})
	p := llmtest.New("responses",
		llmtest.Step{Events: []llm.StreamEvent{toolDone(0, "call", "activate", `{}`)}, Stop: llm.StopToolUse, ResponseID: "r1"},
		summaryStep("done", 0, 0))
	a := newAgent(p, r, incrementalAgentOptions())
	if err := a.RunPrompt(context.Background(), "activate", &recordSink{}); err != nil {
		t.Fatal(err)
	}
	if len(p.Requests) != 2 || p.Requests[1].PreviousResponseID != "r1" || !p.Requests[1].IncrementalTools {
		t.Fatalf("requests: %+v", p.Requests)
	}
	messages := a.Transcript()
	if err := llm.ValidateTranscript(messages); err != nil {
		t.Fatal(err)
	}
	if len(messages) != 5 || messages[2].Content[0].Kind != llm.BlockToolResult || messages[3].ToolContext == nil || messages[3].ToolContext.Tools[0].Name != "new_tool" {
		t.Fatalf("catalog delta broke tool call/result adjacency: %+v", messages)
	}
}

func TestIncrementalToolsDiffsDefinitionsNotOrdering(t *testing.T) {
	a := newAgent(llmtest.New("responses"), incrementalRegistry("a", "b"), incrementalAgentOptions())
	a.AdmitPromptContent("begin", nil)
	a.modelRequest(nil)
	before := llm.CloneMessages(a.Transcript())
	a.toolSpecs[0], a.toolSpecs[1] = a.toolSpecs[1], a.toolSpecs[0]
	var formatted bytes.Buffer
	if err := json.Indent(&formatted, a.toolSpecs[0].Parameters, "", "  "); err != nil {
		t.Fatal(err)
	}
	a.toolSpecs[0].Parameters = formatted.Bytes()
	if _, changed := a.prepareToolContext(a.Transcript()); changed {
		t.Fatal("reordering or formatting unchanged definitions emitted updates")
	}
	a.toolSpecs[0].Description = "new definition"
	messages, changed := a.prepareToolContext(a.Transcript())
	if !changed || !reflect.DeepEqual(a.Transcript(), before) {
		t.Fatal("projection did not detect change or modified live history")
	}
	event := messages[len(messages)-1].ToolContext
	if len(event.Tools) != 1 || event.Tools[0].Name != "b" || event.Tools[0].Description != "new definition" {
		t.Fatalf("redefinition = %+v", event)
	}
	// Returning all tools to empty records a removal, rather than resetting mode.
	a.toolSpecs = nil
	messages, changed = a.prepareToolContext(messages)
	if !changed || len(messages[len(messages)-1].ToolContext.Removed) != 2 || !a.incrementalToolsIn(messages) {
		t.Fatal("empty catalog lost its incremental window")
	}
}

func TestIncrementalToolsFallbackAndLegacyWindows(t *testing.T) {
	for _, kind := range []string{"disabled", "foreign", "legacy", "deferred", "server"} {
		t.Run(kind, func(t *testing.T) {
			a := newAgent(llmtest.New("responses"), incrementalRegistry("a"), incrementalAgentOptions())
			a.AdmitPromptContent("begin", nil)
			a.modelRequest(nil)
			switch kind {
			case "disabled":
				a.registry = llm.NewRegistry(nil)
			case "foreign":
				a.SetReasoningReplayDomain("other")
			case "legacy":
				a.transcript[0].ToolContext = nil
				a.transcript = append(a.transcript, asstText("old response"))
			case "deferred":
				a.deferredToolGroups = []llm.ToolGroup{{Name: "mcp", Tools: a.toolSpecs}}
			case "server":
				a.serverTools = []llm.ServerTool{{Name: "web_search"}}
			}
			req := a.DebugRequest(true, "next", nil, nil).Request
			if req.IncrementalTools || len(req.Tools) != 1 {
				t.Fatalf("did not fall back: %+v", req)
			}
			for _, message := range req.Messages {
				if message.ToolContext != nil {
					t.Fatalf("metadata leaked across incompatible window: %+v", message.ToolContext)
				}
			}
		})
	}
}

func TestIncrementalToolContextRebasesAcrossCompaction(t *testing.T) {
	for _, mode := range []string{"text", "native", "notes"} {
		t.Run(mode, func(t *testing.T) {
			p := &nativeCompactionProvider{FakeProvider: llmtest.New("responses", summaryStep("summary", 0, 0)), result: llm.CompactedContext{Items: nativeCompactedItems()}}
			opts := incrementalAgentOptions()
			opts.NativeCompaction = mode == "native"
			a := newAgent(p, incrementalRegistry("current"), opts)
			if mode == "notes" {
				a, _, _, _ = notesTestAgent(t, p, opts)
			}
			messages := makeTurns(10)
			messages[0].ToolContext = &llm.ToolContext{ReplayDomain: "test-domain", Initial: true, Tools: []llm.ToolSchema{{Name: "obsolete"}}}
			messages[len(messages)-1].ToolContext = &llm.ToolContext{ReplayDomain: "test-domain", After: true, Tools: []llm.ToolSchema{{Name: "outdated_suffix"}}}
			a.SetTranscript(messages)
			if _, changed, err := a.compactInternal(context.Background(), &recordSink{}, compactOptions{trigger: "manual"}); err != nil || !changed {
				t.Fatalf("compaction: changed=%v err=%v", changed, err)
			}
			req := a.ContextRequest()
			if !req.IncrementalTools || req.Messages[0].ToolContext == nil || !req.Messages[0].ToolContext.Initial {
				t.Fatalf("replacement missing new baseline: %+v", req)
			}
			if !reflect.DeepEqual(req.Messages[0].ToolContext.Tools, a.requestToolSpecs()) {
				t.Fatal("replacement retained obsolete catalog")
			}
			for _, message := range req.Messages[1:] {
				if message.ToolContext != nil {
					t.Fatal("old suffix event could override rebuilt catalog")
				}
			}
			if mode == "native" && !p.requests[0].IncrementalTools {
				t.Fatal("native maintenance did not use the chronological input")
			}
			if err := llm.ValidateTranscript(a.Transcript()); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestIncrementalToolsSkipPrewarm(t *testing.T) {
	for _, resumed := range []bool{false, true} {
		a := newAgent(llmtest.New("responses"), incrementalRegistry("a"), incrementalAgentOptions())
		if resumed {
			a.AdmitPromptContent("begin", nil)
			a.modelRequest(nil)
		}
		if _, ok := a.PrewarmRequest(); ok {
			t.Fatal("chronological target allowed prefix-only prewarm")
		}
		digest, err := llm.FingerprintMessages(nil)
		if err != nil {
			t.Fatal(err)
		}
		if a.ApplyPrewarmResult(PrewarmResult{
			ResponseState:      &llm.ResponseState{PreviousResponseID: "warm", AnchorDigest: digest},
			ResponseStateEpoch: a.responseStateEpoch, ProxySessionID: a.proxySessionID,
			TranscriptMessages: len(a.transcript),
		}) {
			t.Fatal("installed stale zero-message prewarm anchor")
		}
	}
}

func TestIncrementalToolsFinalizerDisablesHistoricalCalls(t *testing.T) {
	for _, ignoredCall := range []bool{false, true} {
		step := summaryStep("summary", 0, 0)
		step.ResponseID = "final"
		if ignoredCall {
			step.Events = append(step.Events, toolDone(0, "ignored", "a", `{}`))
		}
		p := llmtest.New("responses", step)
		a := newAgent(p, incrementalRegistry("a"), incrementalAgentOptions())
		a.AdmitPromptContent("begin", nil)
		a.modelRequest(nil)
		if _, _, _, ok := a.finalizeWithSummary(context.Background(), &recordSink{}, nil, 1); !ok {
			t.Fatal("finalizer failed")
		}
		req := p.Requests[0]
		if !req.DisableTools || !req.IncrementalTools || len(req.Tools) != 0 {
			t.Fatalf("finalizer request: %+v", req)
		}
		if ignoredCall && a.responseState.PreviousResponseID != "" {
			t.Fatal("anchored discarded remote tool calls")
		}
		if !ignoredCall && a.responseState.PreviousResponseID != "final" {
			t.Fatal("lost valid summary anchor")
		}
		if err := llm.ValidateTranscript(a.Transcript()); err != nil {
			t.Fatal(err)
		}
	}
}

func TestIncrementalToolContextCompactionTreeRetainsMarkerSlots(t *testing.T) {
	p := llmtest.New("responses", summaryStep("summary", 0, 0))
	a := newAgent(p, incrementalRegistry("current"), incrementalAgentOptions())
	messages := makeTurns(10)
	messages[0].ToolContext = a.initialToolContext()
	// A standalone catalog update in the retained suffix must not shorten the
	// suffix expected by the tree's pending compaction transaction.
	last := messages[len(messages)-1]
	messages[len(messages)-1] = llm.Message{Role: llm.RoleUser, Origin: llm.MessageOriginInternal,
		ToolContext: &llm.ToolContext{ReplayDomain: "test-domain", After: true, Removed: []string{"old"}}}
	messages = append(messages, last)
	a.SetTranscript(messages)
	tree := session.NewTree(time.Now(), "", "", "")
	dir := t.TempDir()
	a.SetCompactionArchiver(func(_ context.Context, archive CompactionArchive) (string, error) {
		ref, err := session.SaveCompaction(dir, session.Compaction{Messages: archive.Messages, Summary: archive.Summary})
		if err != nil {
			return "", err
		}
		return ref, tree.PrepareCompaction(a.Transcript(), len(archive.Messages), archive.Summary, ref, archive.TokensBefore, "", nil, nil)
	})
	if _, changed, err := a.compactInternal(context.Background(), &recordSink{}, compactOptions{trigger: "manual"}); err != nil || !changed {
		t.Fatalf("compaction: %v, %v", changed, err)
	}
	found := false
	for _, message := range a.Transcript() {
		if len(message.Content) == 0 && message.ToolContext == nil {
			found = true
		}
	}
	if !found {
		t.Fatal("test did not retain a catalog marker slot")
	}
	if err := tree.SyncTranscript(a.Transcript()); err != nil {
		t.Fatal(err)
	}
	if err := tree.Save(dir); err != nil {
		t.Fatal(err)
	}
	loaded, err := session.LoadTree(dir, tree.ActiveLeaf)
	if err != nil {
		t.Fatal(err)
	}
	context, err := loaded.BuildContext()
	if err != nil {
		t.Fatal(err)
	}
	if dump(context) != dump(a.Transcript()) {
		t.Fatalf("compacted tree context did not round trip: got %s want %s", dump(context), dump(a.Transcript()))
	}
	if err := llm.ValidateTranscript(context); err != nil {
		t.Fatal(err)
	}
	for _, message := range a.ContextRequest().Messages {
		if len(message.Content) == 0 && message.ToolContext == nil {
			t.Fatal("empty marker reached provider")
		}
	}
}

func TestIncrementalToolContextEstimatesCatalogOnce(t *testing.T) {
	a := newAgent(llmtest.New("responses"), incrementalRegistry("a"), incrementalAgentOptions())
	a.AdmitPromptContent("begin", nil)
	mr := a.modelRequest(nil)
	want := estimateRequest(llm.Request{System: a.system, Messages: []llm.Message{userText("begin")}, Tools: a.toolSpecs}, a.window())
	if mr.estimate.Total != want.Total {
		t.Fatalf("initial catalog counted twice: %+v vs %+v", mr.estimate, want)
	}
	large := llm.ToolSchema{Name: "large", Description: strings.Repeat("description ", 1000)}
	a.toolSpecs = append(a.toolSpecs, large)
	a.transcript, _ = a.prepareToolContext(a.transcript)
	est := a.estimateContext(nil)
	if est.Total <= want.Total+2000 {
		t.Fatal("appended definitions not counted")
	}
	// Removing a tool does not erase its earlier cached definition.
	a.toolSpecs = nil
	a.transcript, _ = a.prepareToolContext(a.transcript)
	if after := a.estimateContext(nil); after.Total <= est.Total {
		t.Fatalf("removal incorrectly reduced historical token footprint: %d <= %d", after.Total, est.Total)
	}
}
