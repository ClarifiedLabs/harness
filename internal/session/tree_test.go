package session

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"harness/internal/llm"
)

func TestTreeBranchesWithoutRewritingOldPath(t *testing.T) {
	created := time.Date(2026, 7, 19, 12, 0, 0, 0, time.UTC)
	messages := []llm.Message{
		treePrompt(created, "first"),
		treeAssistant(created, "first answer"),
		treePrompt(created.Add(time.Minute), "second"),
		treeAssistant(created.Add(time.Minute), "second answer"),
	}
	tree, err := LinearTree(created, "/work", messages)
	if err != nil {
		t.Fatalf("LinearTree: %v", err)
	}
	if len(tree.Entries) != 4 {
		t.Fatalf("entries = %d, want 4", len(tree.Entries))
	}
	oldLeaf := tree.ActiveLeaf
	secondPrompt := tree.Entries[2]
	common, err := tree.CommonAncestor(oldLeaf, secondPrompt.ParentID)
	if err != nil {
		t.Fatalf("CommonAncestor: %v", err)
	}
	leaf, err := tree.AppendBranch(secondPrompt.ParentID, oldLeaf, common, "second path was explored", "")
	if err != nil {
		t.Fatalf("AppendBranch: %v", err)
	}
	active, err := tree.BuildContext()
	if err != nil {
		t.Fatalf("BuildContext: %v", err)
	}
	text := transcriptText(active)
	for _, want := range []string{"first", "first answer", "working directory was not reverted", "second path was explored"} {
		if !strings.Contains(strings.ToLower(text), strings.ToLower(want)) {
			t.Errorf("active context missing %q: %s", want, text)
		}
	}
	if strings.Contains(text, "second answer") {
		t.Fatalf("active context retained abandoned answer: %s", text)
	}
	if _, ok := tree.Entry(oldLeaf); !ok {
		t.Fatalf("old leaf %q was lost", oldLeaf)
	}
	if leaf == oldLeaf {
		t.Fatalf("branch did not create a new leaf")
	}

	dir := t.TempDir()
	state := Session{Created: created, Updated: created, Provider: "test", Model: "test", Messages: active, Tree: tree}
	if err := state.Save(dir); err != nil {
		t.Fatalf("Save: %v", err)
	}
	loaded, err := Load(dir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if loaded.ActiveLeaf != leaf || len(loaded.Tree.Entries) != 5 {
		t.Fatalf("loaded tree leaf/entries = %q/%d, want %q/5", loaded.ActiveLeaf, len(loaded.Tree.Entries), leaf)
	}
}

func TestTreeCompactionKeepsRawPathNavigable(t *testing.T) {
	at := time.Date(2026, 7, 19, 12, 0, 0, 0, time.UTC)
	before := []llm.Message{
		treePrompt(at, "old"), treeAssistant(at, "old answer"),
		treePrompt(at.Add(time.Minute), "kept"), treeAssistant(at.Add(time.Minute), "kept answer"),
	}
	tree, err := LinearTree(at, "", before)
	if err != nil {
		t.Fatalf("LinearTree: %v", err)
	}
	oldPromptID := tree.Entries[0].ID
	checkpoint := llm.Message{
		Role:   llm.RoleUser,
		Time:   at.Add(2 * time.Minute),
		Origin: llm.MessageOriginCompactionCheckpoint,
		Content: []llm.ContentBlock{{
			Kind: llm.BlockText,
			Text: "checkpoint summary",
		}},
	}
	if err := tree.PrepareCompaction(before, 2, "summary", "compactions/0001.input.json", 1200, "", nil, nil); err != nil {
		t.Fatalf("PrepareCompaction: %v", err)
	}
	after := append([]llm.Message{checkpoint}, before[2:]...)
	if err := tree.SyncTranscript(after); err != nil {
		t.Fatalf("SyncTranscript compacted: %v", err)
	}
	active, err := tree.BuildContext()
	if err != nil {
		t.Fatalf("BuildContext: %v", err)
	}
	if got := transcriptText(active); !strings.Contains(got, "checkpoint summary") || !strings.Contains(got, "kept answer") || strings.Contains(got, "old answer") {
		t.Fatalf("compacted active context = %q", got)
	}
	if _, ok := tree.Entry(oldPromptID); !ok {
		t.Fatalf("compaction removed old prompt node")
	}
	var found bool
	for _, entry := range tree.Entries {
		if entry.Type == EntryCompaction {
			found = entry.ArchiveRef == "compactions/0001.input.json" && entry.TokensBefore == 1200
		}
	}
	if !found {
		t.Fatalf("compaction metadata not recorded: %+v", tree.Entries)
	}
}

func TestTreeExtractRecordsParentAndGrowsIndependently(t *testing.T) {
	at := time.Date(2026, 7, 19, 12, 0, 0, 0, time.UTC)
	tree, err := LinearTree(at, "/work", []llm.Message{treePrompt(at, "one"), treeAssistant(at, "answer")})
	if err != nil {
		t.Fatalf("LinearTree: %v", err)
	}
	parentLeaf := tree.ActiveLeaf
	child, err := tree.Extract(parentLeaf, at.Add(time.Minute), "/work")
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	if child.Header.ID == tree.Header.ID || child.Header.ParentSession != tree.Header.ID || child.Header.ParentEntryID != parentLeaf {
		t.Fatalf("child lineage = %+v, parent id/leaf = %s/%s", child.Header, tree.Header.ID, parentLeaf)
	}
	if _, err := child.AppendBranch(parentLeaf, parentLeaf, parentLeaf, "", ""); err != nil {
		t.Fatalf("child AppendBranch: %v", err)
	}
	if len(child.Entries) != len(tree.Entries)+1 || len(tree.Entries) != 2 {
		t.Fatalf("child/parent entries = %d/%d", len(child.Entries), len(tree.Entries))
	}
}

func TestCollectTreeStatsIncludesAbandonedLeaves(t *testing.T) {
	at := time.Date(2026, 7, 19, 12, 0, 0, 0, time.UTC)
	tree, err := LinearTree(at, "", []llm.Message{treePrompt(at, "one"), treeAssistant(at, "answer")})
	if err != nil {
		t.Fatalf("LinearTree: %v", err)
	}
	oldLeaf := tree.ActiveLeaf
	parent := tree.Entries[0].ID
	if _, err := tree.AppendBranch(parent, oldLeaf, parent, "", ""); err != nil {
		t.Fatalf("AppendBranch: %v", err)
	}
	stats := collectTreeStats(tree)
	if stats.entries != 3 || stats.branches != 1 || stats.leaves != 2 || stats.maxDepth != 2 || stats.activeDepth != 2 {
		t.Fatalf("tree stats = %+v", stats)
	}
}

func TestLoadTreeIgnoresInterruptedFinalAppend(t *testing.T) {
	at := time.Date(2026, 7, 19, 12, 0, 0, 0, time.UTC)
	tree, err := LinearTree(at, "", []llm.Message{treePrompt(at, "one")})
	if err != nil {
		t.Fatalf("LinearTree: %v", err)
	}
	dir := t.TempDir()
	if err := tree.Save(dir); err != nil {
		t.Fatalf("Save: %v", err)
	}
	f, err := os.OpenFile(filepath.Join(dir, treeFile), os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		t.Fatalf("OpenFile: %v", err)
	}
	if _, err := f.WriteString("{\"type\":\"segment\",\"id\":\"cut-off\"\n"); err != nil {
		t.Fatalf("append partial: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	loaded, err := LoadTree(dir, tree.ActiveLeaf)
	if err != nil {
		t.Fatalf("LoadTree: %v", err)
	}
	if len(loaded.Entries) != 1 {
		t.Fatalf("entries = %d, want 1", len(loaded.Entries))
	}
	next := append(loaded.activeMsgs, treeAssistant(at.Add(time.Minute), "two"))
	if err := loaded.SyncTranscript(next); err != nil {
		t.Fatalf("SyncTranscript after interrupted append: %v", err)
	}
	if err := loaded.Save(dir); err != nil {
		t.Fatalf("Save after interrupted append: %v", err)
	}
	reloaded, err := LoadTree(dir, loaded.ActiveLeaf)
	if err != nil {
		t.Fatalf("reload repaired tree: %v", err)
	}
	if len(reloaded.Entries) != 2 || !strings.Contains(transcriptText(reloaded.activeMsgs), "two") {
		t.Fatalf("repaired entries/context = %d/%q", len(reloaded.Entries), transcriptText(reloaded.activeMsgs))
	}
}

func TestTreeSaveTracksMultipleDestinations(t *testing.T) {
	at := time.Date(2026, 7, 19, 12, 0, 0, 0, time.UTC)
	tree, err := LinearTree(at, "", []llm.Message{treePrompt(at, "one")})
	if err != nil {
		t.Fatalf("LinearTree: %v", err)
	}
	first, second := filepath.Join(t.TempDir(), "first"), filepath.Join(t.TempDir(), "second")
	if err := tree.Save(first); err != nil {
		t.Fatalf("save first: %v", err)
	}
	withAnswer := append(tree.activeMsgs, treeAssistant(at, "answer"))
	if err := tree.SyncTranscript(withAnswer); err != nil {
		t.Fatalf("SyncTranscript: %v", err)
	}
	if err := tree.Save(second); err != nil {
		t.Fatalf("save second: %v", err)
	}
	withNext := append(tree.activeMsgs, treePrompt(at.Add(time.Minute), "two"))
	if err := tree.SyncTranscript(withNext); err != nil {
		t.Fatalf("SyncTranscript next: %v", err)
	}
	for _, dir := range []string{first, second} {
		if err := tree.Save(dir); err != nil {
			t.Fatalf("save %s: %v", dir, err)
		}
		loaded, err := LoadTree(dir, tree.ActiveLeaf)
		if err != nil {
			t.Fatalf("LoadTree %s: %v", dir, err)
		}
		if len(loaded.Entries) != 3 || transcriptText(loaded.activeMsgs) != "one\nanswer\ntwo" {
			t.Fatalf("tree at %s = %d/%q", dir, len(loaded.Entries), transcriptText(loaded.activeMsgs))
		}
	}
}

func TestPreparedCompactionIsCancelledWhenAgentKeepsTranscript(t *testing.T) {
	at := time.Date(2026, 7, 19, 12, 0, 0, 0, time.UTC)
	messages := []llm.Message{
		treePrompt(at, "old"), treeAssistant(at, "old answer"),
		treePrompt(at.Add(time.Minute), "kept"), treeAssistant(at.Add(time.Minute), "kept answer"),
	}
	tree, err := LinearTree(at, "", messages)
	if err != nil {
		t.Fatalf("LinearTree: %v", err)
	}
	if err := tree.PrepareCompaction(messages, 2, "summary", "archive", 0, "", nil, nil); err != nil {
		t.Fatalf("PrepareCompaction: %v", err)
	}
	if err := tree.SyncTranscript(messages); err != nil {
		t.Fatalf("SyncTranscript unchanged: %v", err)
	}
	if tree.pending != nil || len(tree.Entries) != 4 {
		t.Fatalf("cancelled compaction left pending/entry: pending=%v entries=%d", tree.pending != nil, len(tree.Entries))
	}
}

// Regression: compaction degradation can rewrite blocks in the retained suffix.
// The tree must materialize those messages instead of linking back to the
// original untrimmed entries and resurrecting them after resume.
func TestCompactionMaterializesRewrittenKeptSuffix(t *testing.T) {
	at := time.Date(2026, 7, 19, 12, 0, 0, 0, time.UTC)
	before := []llm.Message{
		treePrompt(at, "old"), treeAssistant(at, "old answer"),
		treePrompt(at.Add(time.Minute), "kept"), treeAssistant(at.Add(time.Minute), "large original payload"),
	}
	tree, err := LinearTree(at, "", before)
	if err != nil {
		t.Fatalf("LinearTree: %v", err)
	}
	checkpoint := llm.Message{Role: llm.RoleUser, Time: at.Add(2 * time.Minute), Origin: llm.MessageOriginCompactionCheckpoint, Content: []llm.ContentBlock{{Kind: llm.BlockText, Text: "checkpoint"}}}
	checkpoint.Compaction = &llm.CompactionMetadata{Summary: "summary"}
	if err := tree.PrepareCompaction(before, 2, "summary", "archive", 100, "focus", []string{"read.go"}, []string{"changed.go"}); err != nil {
		t.Fatalf("PrepareCompaction: %v", err)
	}
	after := append([]llm.Message{checkpoint}, cloneMessagesForTree(before[2:])...)
	after[2].Content[0].Text = "[truncated payload]"
	if err := tree.SyncTranscript(after); err != nil {
		t.Fatalf("SyncTranscript: %v", err)
	}

	dir := t.TempDir()
	if err := tree.Save(dir); err != nil {
		t.Fatalf("Save: %v", err)
	}
	loaded, err := LoadTree(dir, tree.ActiveLeaf)
	if err != nil {
		t.Fatalf("LoadTree: %v", err)
	}
	got, err := loaded.BuildContext()
	if err != nil {
		t.Fatalf("BuildContext: %v", err)
	}
	if text := transcriptText(got); !strings.Contains(text, "[truncated payload]") || strings.Contains(text, "large original payload") {
		t.Fatalf("rewritten kept suffix was not preserved: %q", text)
	}
	if got[0].Compaction == nil || got[0].Compaction.Focus != "focus" || !slices.Equal(got[0].Compaction.ReadFiles, []string{"read.go"}) {
		t.Fatalf("checkpoint metadata was not reconstructed: %+v", got[0].Compaction)
	}
	var materialized bool
	for _, entry := range loaded.Entries {
		if entry.Type == EntryCompaction {
			materialized = entry.MaterializedKept
		}
	}
	if !materialized {
		t.Fatal("rewritten suffix was not marked materialized")
	}
	if err := loaded.PrepareCompaction(got, 2, "again", "archive2", 50, "", nil, nil); err != nil {
		t.Fatalf("materialized suffix lost atomic boundary: %v", err)
	}
}

func treePrompt(at time.Time, text string) llm.Message {
	return llm.Message{Role: llm.RoleUser, Time: at, Origin: llm.MessageOriginPrompt, Content: []llm.ContentBlock{{Kind: llm.BlockText, Text: text}}}
}

func treeAssistant(at time.Time, text string) llm.Message {
	return llm.Message{Role: llm.RoleAssistant, Time: at, Content: []llm.ContentBlock{{Kind: llm.BlockText, Text: text}}}
}

func transcriptText(messages []llm.Message) string {
	var parts []string
	for _, message := range messages {
		for _, block := range message.Content {
			if block.Kind == llm.BlockText {
				parts = append(parts, block.Text)
			}
		}
	}
	return strings.Join(parts, "\n")
}
