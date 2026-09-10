package taskcontext

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"harness/internal/llm"
	"harness/internal/session"
)

func TestHistoryLookupIsBoundedAndOmitsOpaqueState(t *testing.T) {
	dir := t.TempDir()
	tree, err := session.LinearTree(time.Now(), "", []llm.Message{
		{Role: llm.RoleUser, Content: []llm.ContentBlock{{Kind: llm.BlockText, Text: "Find the Astra roadmap."}}},
		{Role: llm.RoleAssistant, Content: []llm.ContentBlock{{Kind: llm.BlockReasoning, ReasoningEncrypted: "secret-opaque"}, {Kind: llm.BlockText, Text: strings.Repeat("evidence ", 1000)}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := tree.Save(dir); err != nil {
		t.Fatal(err)
	}
	body, err := search(context.Background(), dir, "ASTRA", 0, 1)
	if err != nil {
		t.Fatal(err)
	}
	var result struct {
		Hits []hit `json:"hits"`
		Next int64 `json:"next_offset"`
	}
	if err := json.Unmarshal([]byte(body), &result); err != nil {
		t.Fatal(err)
	}
	if len(result.Hits) != 1 {
		t.Fatalf("hits: %s", body)
	}
	body, err = readEntry(dir, result.Hits[0].Offset, 0, 12, nil)
	if err != nil {
		t.Fatal(err)
	}
	var entry struct {
		Text string `json:"text"`
		Next int    `json:"next_text_offset"`
	}
	if err := json.Unmarshal([]byte(body), &entry); err != nil {
		t.Fatal(err)
	}
	if len(entry.Text) > 12 || entry.Next != len(entry.Text) {
		t.Fatalf("unbounded entry: %s", body)
	}
	body, err = search(context.Background(), dir, "secret-opaque", 0, 20)
	if err != nil || strings.Contains(body, "secret-opaque") {
		t.Fatalf("opaque state exposed: %s %v", body, err)
	}
	if _, err := readEntry(dir, result.Hits[0].Offset+2, 0, 12, nil); err == nil {
		t.Fatal("accepted offset inside an entry")
	}
}

func TestHistoryWindowsFiltersAndStableIDs(t *testing.T) {
	dir := t.TempDir()
	initial := []llm.Message{
		{Role: llm.RoleUser, Content: []llm.ContentBlock{{Kind: llm.BlockText, Text: "debug cache"}}},
		{Role: llm.RoleAssistant, Content: []llm.ContentBlock{{Kind: llm.BlockToolUse, ToolUseID: "test", ToolName: "shell", ToolInput: json.RawMessage(`{"command":"go test"}`)}}},
		{Role: llm.RoleUser, Content: []llm.ContentBlock{{Kind: llm.BlockToolResult, ResultForID: "test", ResultText: "FAIL: stale cache"}}},
	}
	tree, err := session.LinearTree(time.Now(), "", initial)
	if err != nil {
		t.Fatal(err)
	}
	if err := tree.AppendContextReset([]llm.Message{{Role: llm.RoleUser, Content: []llm.ContentBlock{{Kind: llm.BlockText, Text: "fresh context"}}}}, "notes"); err != nil {
		t.Fatal(err)
	}
	if err := tree.Save(dir); err != nil {
		t.Fatal(err)
	}
	decode := func(in historyInput) []hit {
		t.Helper()
		body, err := lookupHistory(context.Background(), dir, in)
		if err != nil {
			t.Fatal(err)
		}
		var result struct{ Hits []hit }
		if err := json.Unmarshal([]byte(body), &result); err != nil {
			t.Fatal(err)
		}
		return result.Hits
	}
	windows := decode(historyInput{Windows: true, Limit: 20})
	if len(windows) != 2 || windows[0].ID != tree.Header.ID || windows[1].ID != tree.ActiveLeaf {
		t.Fatalf("windows = %+v", windows)
	}
	if got := decode(historyInput{Windows: true, WindowID: tree.ActiveLeaf, Limit: 20}); len(got) != 1 || got[0].ID != tree.ActiveLeaf {
		t.Fatalf("filtered windows = %+v", got)
	}
	if got := decode(historyInput{WindowID: tree.ActiveLeaf, Role: "user", Limit: 20}); len(got) != 1 {
		t.Fatalf("reset role filter = %+v", got)
	}
	entries := decode(historyInput{WindowID: tree.Header.ID, Role: "tool", ToolName: "shell", Limit: 20})
	if len(entries) != 1 || entries[0].WindowID != tree.Header.ID || !strings.Contains(entries[0].Text, "stale cache") {
		t.Fatalf("filtered entries = %+v", entries)
	}
	if got := decode(historyInput{WindowID: tree.ActiveLeaf, Query: "stale cache", Limit: 20}); len(got) != 0 {
		t.Fatal("window filter leaked old entries")
	}
	if err := tree.SyncTranscript(append([]llm.Message{{Role: llm.RoleUser, Content: []llm.ContentBlock{{Kind: llm.BlockText, Text: "fresh context"}}}}, llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{{Kind: llm.BlockText, Text: "new work"}}})); err != nil {
		t.Fatal(err)
	}
	if err := tree.Save(dir); err != nil {
		t.Fatal(err)
	}
	body, err := readHistory(context.Background(), dir, historyInput{ID: entries[0].ID, MaxBytes: 4096})
	if err != nil || !strings.Contains(body, "stale cache") {
		t.Fatalf("stable lookup after save = %s %v", body, err)
	}
}
