package delegate

import (
	"encoding/json"
	"strings"
	"sync"
	"testing"

	"harness/internal/agent"
	"harness/internal/llm"
)

func TestActivityRegistryTracksLatestNestedDelegateAndRemovesOnce(t *testing.T) {
	registry := NewActivityRegistry(nil)
	parent := registry.Register(ActivityStart{ID: "child-parent", Depth: 1, Agent: "explore", TranscriptPath: "/tmp/parent"})
	nested := registry.Register(ActivityStart{ID: "child-nested", ParentID: "child-parent", Depth: 2, Agent: "plan"})
	parent.MarkTurn(1, 1, agent.ContextEstimate{Total: 10, Window: 100})
	nested.MarkTurn(2, 3, agent.ContextEstimate{Total: 40, Window: 100})
	nested.MarkActivity("tool read_file")

	snapshot := registry.Snapshot()
	if len(snapshot.Active) != 2 {
		t.Fatalf("active delegates = %d, want 2", len(snapshot.Active))
	}
	if snapshot.Active[0].DisplayID != "d1" || snapshot.Active[1].DisplayID != "d2" {
		t.Fatalf("display IDs = %q, %q, want d1, d2", snapshot.Active[0].DisplayID, snapshot.Active[1].DisplayID)
	}
	if snapshot.Recent.ID != "child-nested" || snapshot.Recent.ParentID != "child-parent" || snapshot.Recent.Depth != 2 {
		t.Fatalf("latest nested delegate = %+v", snapshot.Recent)
	}
	if snapshot.Recent.Turn != 2 || snapshot.Recent.Attempt != 3 || snapshot.Recent.Activity != "tool read_file" {
		t.Fatalf("latest nested activity = %+v", snapshot.Recent)
	}

	nested.Finish("completed", 2)
	nested.Finish("completed", 2)
	nested.MarkActivity("must be ignored")
	snapshot = registry.Snapshot()
	if len(snapshot.Active) != 1 || snapshot.Recent.ID != "child-parent" {
		t.Fatalf("after nested removal = %+v", snapshot)
	}
	parent.Finish("completed", 1)
	if got := registry.Snapshot(); len(got.Active) != 0 {
		t.Fatalf("active delegates after close = %+v, want none", got.Active)
	}
}

func TestActivityRegistryLatestTieBreakIsDeterministic(t *testing.T) {
	active := []ActiveDelegate{
		{ID: "z-child", Sequence: 9},
		{ID: "a-child", Sequence: 9},
		{ID: "older", Sequence: 8},
	}
	if got := selectLatestActive(active); got.ID != "a-child" {
		t.Fatalf("latest tie break = %q, want a-child", got.ID)
	}
}

func TestActivityRegistryUsesIndependentRegistrationKeys(t *testing.T) {
	registry := NewActivityRegistry(nil)
	prefix := strings.Repeat("x", maxChildIDRunes)
	first := registry.Register(ActivityStart{ID: prefix + "-first", Agent: "explore"})
	second := registry.Register(ActivityStart{ID: prefix + "-second", Agent: "plan"})
	if got := registry.Snapshot(); len(got.Active) != 2 || got.Active[0].DisplayID != "d1" || got.Active[1].DisplayID != "d2" {
		t.Fatalf("colliding display IDs overwrote registry membership: %+v", got.Active)
	}

	first.Finish("completed", 0)
	first.MarkActivity("stale update")
	got := registry.Snapshot()
	if len(got.Active) != 1 || got.Active[0].DisplayID != "d2" || got.Active[0].Activity == "stale update" {
		t.Fatalf("stale registration affected live entry: %+v", got.Active)
	}
	second.Finish("completed", 0)
}

func TestActivityRegistrySanitizesAndBoundsRetainedText(t *testing.T) {
	registry := NewActivityRegistry(nil)
	longAgent := "wide漢字\x1b[31mred\x1b[0m\n" + strings.Repeat("x", 100)
	registration := registry.Register(ActivityStart{
		ID:             strings.Repeat("child", 40),
		ParentID:       "parent\x1b]52;c;secret\x07-id",
		Agent:          longAgent,
		TranscriptPath: "/tmp/a\npath",
	})
	registration.MarkActivity("working\r\n\x1b[2J" + strings.Repeat("界", 200))

	entry := registry.Snapshot().Recent
	for name, value := range map[string]string{
		"agent": entry.Agent, "parent": entry.ParentID, "path": entry.TranscriptPath, "activity": entry.Activity,
	} {
		if strings.ContainsAny(value, "\x1b\r\n") || strings.Contains(value, "[31m") || strings.Contains(value, "secret") {
			t.Fatalf("%s was not sanitized: %q", name, value)
		}
	}
	if got := len([]rune(entry.ID)); got > maxChildIDRunes {
		t.Fatalf("child ID runes = %d, cap %d", got, maxChildIDRunes)
	}
	if got := len([]rune(entry.Agent)); got > maxAgentRunes {
		t.Fatalf("agent runes = %d, cap %d", got, maxAgentRunes)
	}
	if got := len([]rune(entry.Activity)); got > maxActivityRunes {
		t.Fatalf("activity runes = %d, cap %d", got, maxActivityRunes)
	}
	if entry.DisplayID != "d1" {
		t.Fatalf("long child ID display label = %q, want d1", entry.DisplayID)
	}
}

func TestSafeToolActivityUsesArgumentAllowlist(t *testing.T) {
	read := safeToolActivity(llm.ToolCall{
		Name:  "read_file",
		Input: json.RawMessage(`{"path":"internal/delegate/activity.go","credential":"must-not-leak"}`),
	})
	if !strings.Contains(read, "path=\"internal/delegate/activity.go\"") || strings.Contains(read, "credential") {
		t.Fatalf("read_file summary = %q", read)
	}

	credentialPath := safeToolActivity(llm.ToolCall{
		Name:  "read_file",
		Input: json.RawMessage(`{"path":"https://user:password@example.test/file"}`),
	})
	if credentialPath != "tool read_file" {
		t.Fatalf("credential-bearing path summary = %q, want tool name only", credentialPath)
	}

	command := safeToolActivity(llm.ToolCall{
		Name:  "run_command",
		Input: json.RawMessage(`{"command":"curl -H 'Authorization: secret' https://example.test"}`),
	})
	if command != "tool run_command" {
		t.Fatalf("command summary = %q, want redacted tool name only", command)
	}

	resultBody := "raw result secret"
	sinkRegistry := NewActivityRegistry(nil)
	registration := sinkRegistry.Register(ActivityStart{ID: "child", Agent: "explore"})
	sink := newChildSink("", nil, registration)
	sink.ToolStart(llm.ToolCall{ID: "call", Name: "run_command", Input: json.RawMessage(`{"command":"echo secret"}`)})
	sink.ToolResult(llm.ToolResult{ForID: "call", Text: resultBody, IsError: true})
	if activity := sinkRegistry.Snapshot().Recent.Activity; strings.Contains(activity, "secret") || activity != "tool run_command failed" {
		t.Fatalf("tool result activity = %q", activity)
	}
}

func TestChildSinkDoesNotRetainAssistantTextInActivity(t *testing.T) {
	registry := NewActivityRegistry(nil)
	registration := registry.Register(ActivityStart{ID: "child", Agent: "explore"})
	defer registration.Finish("completed", 0)
	sink := newChildSink("", nil, registration)
	sink.TextDelta("Authorization: Bearer secret-token")

	got := registry.Snapshot().Recent.Activity
	if got != "replying" || strings.Contains(got, "secret-token") {
		t.Fatalf("assistant activity = %q, want semantic reply state only", got)
	}
}

func TestActivityRegistryConcurrentPublishSnapshotAndClose(t *testing.T) {
	registry := NewActivityRegistry(nil)
	const workers = 24
	var wg sync.WaitGroup
	wg.Add(workers)
	for i := 0; i < workers; i++ {
		go func(i int) {
			defer wg.Done()
			registration := registry.Register(ActivityStart{ID: "child-" + string(rune('a'+i)), Agent: "explore"})
			for turn := 1; turn <= 50; turn++ {
				registration.MarkTurn(turn, 1, agent.ContextEstimate{Total: turn, Window: 100})
				registration.MarkActivity("thinking")
				_ = registry.Snapshot()
			}
			registration.Finish("completed", 50)
		}(i)
	}
	wg.Wait()
	if got := registry.Snapshot(); len(got.Active) != 0 {
		t.Fatalf("active delegates after concurrent close = %d, want 0", len(got.Active))
	}
}
