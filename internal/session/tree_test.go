package session

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"harness/internal/llm"
)

func TestCloneMessagesForTreeDeepCopiesRichResultContent(t *testing.T) {
	original := []llm.Message{{Role: llm.RoleUser, Content: []llm.ContentBlock{{
		Kind: llm.BlockToolResult, ResultForID: "call_1", ResultText: "image attached",
		ResultContent: []llm.ContentBlock{{Kind: llm.BlockImage, ImageMediaType: "image/png", ImageData: "original"}},
	}}}}
	cloned := cloneMessagesForTree(original)
	cloned[0].Content[0].ResultContent[0].ImageData = "changed"
	if got := original[0].Content[0].ResultContent[0].ImageData; got != "original" {
		t.Fatalf("nested result content aliased original: %q", got)
	}
}

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

// Regression: retention may rewrite an old tool result before compaction,
// causing SyncTranscript to materialize the whole active transcript in one
// context-reset entry. A later valid compaction boundary inside that entry must
// materialize its kept suffix instead of treating the reset as indivisible.
func TestTreeCompactionMaterializesBoundaryInsideContextReset(t *testing.T) {
	at := time.Date(2026, 7, 19, 12, 0, 0, 0, time.UTC)
	before := []llm.Message{
		treePrompt(at, strings.Repeat("old context ", 1000)),
		{Role: llm.RoleAssistant, Time: at, Content: []llm.ContentBlock{{Kind: llm.BlockToolUse, ToolUseID: "old-call", ToolName: "read_file", ToolInput: []byte(`{}`)}}},
		{Role: llm.RoleUser, Time: at, Content: []llm.ContentBlock{{Kind: llm.BlockToolResult, ResultForID: "old-call", ResultText: "original old result"}}},
		{Role: llm.RoleAssistant, Time: at.Add(time.Minute), Content: []llm.ContentBlock{{Kind: llm.BlockToolUse, ToolUseID: "kept-call", ToolName: "read_file", ToolInput: []byte(`{}`)}}},
		{Role: llm.RoleUser, Time: at.Add(time.Minute), Content: []llm.ContentBlock{{Kind: llm.BlockToolResult, ResultForID: "kept-call", ResultText: "kept result"}}},
	}
	tree, err := LinearTree(at, "", before)
	if err != nil {
		t.Fatalf("LinearTree: %v", err)
	}

	rewritten := cloneMessagesForTree(before)
	rewritten[2].Content[0].ResultText = "[older tool output trimmed]"
	rewritten[3].Content[0].ToolInput = []byte(`{"path":"kept"}`)
	rewritten[4].Content[0].ResultText = "[kept output trimmed]"
	if err := tree.SyncTranscript(rewritten); err != nil {
		t.Fatalf("SyncTranscript retention rewrite: %v", err)
	}
	resetID := tree.ActiveLeaf
	if entry, ok := tree.Entry(resetID); !ok || entry.Type != EntryContextReset || entry.ContextDelta == nil || len(entry.Messages) != 0 {
		t.Fatalf("retention rewrite leaf = %+v, %v; want delta context reset", entry, ok)
	}

	checkpoint := llm.Message{
		Role:       llm.RoleUser,
		Time:       at.Add(2 * time.Minute),
		Origin:     llm.MessageOriginCompactionCheckpoint,
		Content:    []llm.ContentBlock{{Kind: llm.BlockText, Text: "checkpoint summary"}},
		Compaction: &llm.CompactionMetadata{Summary: "summary"},
	}
	if err := tree.PrepareCompaction(rewritten, 3, "summary", "compactions/0001.input.json", 1200, "", nil, nil); err != nil {
		t.Fatalf("PrepareCompaction: %v", err)
	}
	after := append([]llm.Message{checkpoint}, cloneMessagesForTree(rewritten[3:])...)
	if err := tree.SyncTranscript(after); err != nil {
		t.Fatalf("SyncTranscript compacted: %v", err)
	}

	var compaction Entry
	for _, entry := range tree.Entries {
		if entry.Type == EntryCompaction {
			compaction = entry
		}
	}
	if compaction.ID == "" || !compaction.MaterializedKept || compaction.FirstKeptEntryID != resetID {
		t.Fatalf("compaction did not materialize reset suffix: %+v", compaction)
	}

	dir := t.TempDir()
	if err := tree.Save(dir); err != nil {
		t.Fatalf("Save: %v", err)
	}
	loaded, err := LoadTree(dir, tree.ActiveLeaf)
	if err != nil {
		t.Fatalf("LoadTree: %v", err)
	}
	active, err := loaded.BuildContext()
	if err != nil {
		t.Fatalf("BuildContext: %v", err)
	}
	if !transcriptsEqual(active, after) {
		t.Fatalf("reloaded context = %+v, want %+v", active, after)
	}
}

func TestContextDeltaCompactionOwnershipBoundaries(t *testing.T) {
	at := time.Date(2026, 7, 19, 12, 0, 0, 0, time.UTC)
	base := largeTreeTranscript(at, 4)
	rewriteMiddle := func() []llm.Message {
		out := cloneMessagesForTree(base)
		out[2].Content[0].Text = "rewritten prompt"
		out[3].Content[0].Text = "rewritten answer"
		return out
	}
	rewriteFirst := func() []llm.Message {
		out := cloneMessagesForTree(base)
		out[0].Content[0].Text = "rewritten first prompt"
		return out
	}
	insertMiddle := func() []llm.Message {
		out := append(cloneMessagesForTree(base[:2]), treePrompt(at.Add(time.Hour), "inserted"), treeAssistant(at.Add(time.Hour), "inserted answer"))
		return append(out, cloneMessagesForTree(base[2:])...)
	}
	deleteMiddle := func() []llm.Message {
		return append(cloneMessagesForTree(base[:2]), cloneMessagesForTree(base[4:])...)
	}
	cases := []struct {
		name                 string
		rewrite              func() []llm.Message
		boundary             int
		wantMaterializedKept bool
	}{
		{name: "before replacement run", rewrite: rewriteMiddle, boundary: 0},
		{name: "at replacement run start", rewrite: rewriteMiddle, boundary: 2, wantMaterializedKept: true},
		{name: "inside replacement run", rewrite: rewriteMiddle, boundary: 3, wantMaterializedKept: true},
		{name: "after replacement run", rewrite: rewriteMiddle, boundary: 4},
		{name: "delta owned offset zero", rewrite: rewriteFirst, boundary: 0},
		{name: "inside insertion", rewrite: insertMiddle, boundary: 2, wantMaterializedKept: true},
		{name: "after insertion", rewrite: insertMiddle, boundary: 4},
		{name: "after deletion", rewrite: deleteMiddle, boundary: 2},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tree, err := LinearTree(at, "", base)
			if err != nil {
				t.Fatalf("LinearTree: %v", err)
			}
			rewritten := tc.rewrite()
			if err := tree.SyncTranscript(rewritten); err != nil {
				t.Fatalf("SyncTranscript rewrite: %v", err)
			}
			resetID := tree.ActiveLeaf
			reset, ok := tree.Entry(resetID)
			if !ok || reset.ContextDelta == nil {
				t.Fatalf("rewrite entry = %+v, %v; want delta", reset, ok)
			}
			wantRef := tree.activeRefs[tc.boundary]
			checkpoint := llm.Message{
				Role:       llm.RoleUser,
				Time:       at.Add(2 * time.Hour),
				Origin:     llm.MessageOriginCompactionCheckpoint,
				Content:    []llm.ContentBlock{{Kind: llm.BlockText, Text: "checkpoint"}},
				Compaction: &llm.CompactionMetadata{Summary: "summary"},
			}
			if err := tree.PrepareCompaction(rewritten, tc.boundary, "summary", "archive", 100, "", nil, nil); err != nil {
				t.Fatalf("PrepareCompaction: %v", err)
			}
			after := append([]llm.Message{checkpoint}, cloneMessagesForTree(rewritten[tc.boundary:])...)
			if err := tree.SyncTranscript(after); err != nil {
				t.Fatalf("SyncTranscript compaction: %v", err)
			}
			var compaction Entry
			for i := len(tree.Entries) - 1; i >= 0; i-- {
				if tree.Entries[i].Type == EntryCompaction {
					compaction = tree.Entries[i]
					break
				}
			}
			if compaction.ID == "" {
				t.Fatal("compaction entry not found")
			}
			if compaction.MaterializedKept != tc.wantMaterializedKept || compaction.FirstKeptEntryID != wantRef.entryID {
				t.Fatalf("compaction = %+v, boundary ref = %+v, want materialized=%v", compaction, wantRef, tc.wantMaterializedKept)
			}
			got, err := tree.BuildContext()
			if err != nil {
				t.Fatalf("BuildContext: %v", err)
			}
			if !transcriptsEqual(got, after) {
				t.Fatalf("compacted context = %+v, want %+v", got, after)
			}
		})
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

func TestLoadTreeLegacyAndDeltaFixture(t *testing.T) {
	dir := filepath.Join("testdata", "tree-delta-mixed")
	for _, tc := range []struct {
		leaf string
		want string
	}{{leaf: "snapshot", want: "legacy answer"}, {leaf: "delta", want: "delta answer"}} {
		t.Run(tc.leaf, func(t *testing.T) {
			tree, err := LoadTree(dir, tc.leaf)
			if err != nil {
				t.Fatalf("LoadTree: %v", err)
			}
			messages, err := tree.BuildContext()
			if err != nil {
				t.Fatalf("BuildContext: %v", err)
			}
			if got := transcriptText(messages); !strings.Contains(got, tc.want) {
				t.Fatalf("fixture context = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestTreeContextDeltaMixedReplayAndStorage(t *testing.T) {
	at := time.Date(2026, 7, 19, 12, 0, 0, 0, time.UTC)
	base := largeTreeTranscript(at, 16)
	base[0].Content = append(base[0].Content, llm.ContentBlock{
		Kind: llm.BlockImage, ImageMediaType: "image/png", ImageData: sessionOnePixelPNG,
	})
	tree := NewTree(at, "", "", "")
	if err := tree.AppendContextReset(base, "legacy_fixture"); err != nil {
		t.Fatalf("AppendContextReset legacy snapshot: %v", err)
	}
	legacyLeaf := tree.ActiveLeaf
	legacy, _ := tree.Entry(legacyLeaf)
	if legacy.ContextDelta != nil || len(legacy.Messages) != len(base) {
		t.Fatalf("initial reset encoding = %+v, want legacy snapshot", legacy)
	}

	result := cloneMessagesForTree(base)
	for _, index := range []int{3, 11, 23} {
		result[index].Content[0].Text = fmt.Sprintf("rewritten assistant %d", index)
	}
	if err := tree.SyncTranscript(result); err != nil {
		t.Fatalf("SyncTranscript delta: %v", err)
	}
	deltaLeaf := tree.ActiveLeaf
	delta, _ := tree.Entry(deltaLeaf)
	if delta.ContextDelta == nil || len(delta.Messages) != 0 || len(delta.ContextDelta.Splices) != 3 {
		t.Fatalf("delta reset = %+v, want three disjoint splices", delta)
	}
	if got, err := tree.BuildContext(); err != nil || !transcriptsEqual(got, result) {
		t.Fatalf("BuildContext delta = %+v, %v; want rewritten transcript", got, err)
	}

	stats := collectTreeStats(tree)
	if stats.contextResets != 2 || stats.snapshotResetEntries != 1 || stats.deltaResetEntries != 1 || stats.snapshotResetBytes == 0 || stats.deltaResetBytes == 0 || stats.deltaResetBytes >= stats.snapshotResetBytes {
		t.Fatalf("tree reset stats = %+v", stats)
	}

	dir := t.TempDir()
	if err := tree.Save(dir); err != nil {
		t.Fatalf("Save: %v", err)
	}
	branchLeaf, err := tree.AppendBranch(deltaLeaf, deltaLeaf, deltaLeaf, "delta branch", "")
	if err != nil {
		t.Fatalf("AppendBranch: %v", err)
	}
	if err := tree.Save(dir); err != nil {
		t.Fatalf("Save branch: %v", err)
	}
	branched, err := LoadTree(dir, branchLeaf)
	if err != nil {
		t.Fatalf("LoadTree branch: %v", err)
	}
	if got, err := branched.BuildContext(); err != nil || !strings.Contains(transcriptText(got), "rewritten assistant 23") || !strings.Contains(transcriptText(got), "delta branch") {
		t.Fatalf("branched delta context = %q, %v", transcriptText(got), err)
	}
	for _, tc := range []struct {
		name string
		leaf string
		want []llm.Message
	}{{"legacy", legacyLeaf, base}, {"delta", deltaLeaf, result}} {
		t.Run(tc.name, func(t *testing.T) {
			loaded, err := LoadTree(dir, tc.leaf)
			if err != nil {
				t.Fatalf("LoadTree: %v", err)
			}
			got, err := loaded.BuildContext()
			if err != nil || !transcriptsEqual(got, tc.want) {
				t.Fatalf("reloaded context = %+v, %v; want %+v", got, err, tc.want)
			}
		})
	}
	component, resets, err := analyzeTreeStorage(filepath.Join(dir, treeFile), false)
	if err != nil || component.Status != "complete" || resets.snapshotResetEntries != 1 || resets.deltaResetEntries != 1 || resets.snapshotPayloadBytes == 0 || resets.deltaPayloadBytes == 0 {
		t.Fatalf("analyzeTreeStorage = %+v/%+v, %v", component, resets, err)
	}
}

func TestContextDeltaSpliceShapes(t *testing.T) {
	at := time.Date(2026, 7, 19, 12, 0, 0, 0, time.UTC)
	base := largeTreeTranscript(at, 4)
	inserted := append(cloneMessagesForTree(base[:4]), treePrompt(at.Add(time.Hour), "inserted"), treeAssistant(at.Add(time.Hour), "inserted answer"))
	inserted = append(inserted, cloneMessagesForTree(base[4:])...)
	deleted := append(cloneMessagesForTree(base[:2]), cloneMessagesForTree(base[4:])...)
	replaced := cloneMessagesForTree(base)
	replaced[1].Content[0].Text = "first rewrite"
	replaced[5].Content[0].Text = "second rewrite"
	timestampOnly := cloneMessagesForTree(base)
	timestampOnly[1].Time = timestampOnly[1].Time.Add(time.Second)
	complete := largeTreeTranscript(at.Add(2*time.Hour), 2)
	cases := []struct {
		name         string
		base, result []llm.Message
		wantSplices  int
	}{
		{name: "empty", base: []llm.Message{}, result: []llm.Message{}, wantSplices: 0},
		{name: "complete replacement", base: base, result: complete, wantSplices: 1},
		{name: "insertion", base: base, result: inserted, wantSplices: 1},
		{name: "deletion", base: base, result: deleted, wantSplices: 1},
		{name: "append suffix", base: base, result: append(cloneMessagesForTree(base), treePrompt(at.Add(time.Hour), "tail"), treeAssistant(at.Add(time.Hour), "tail answer")), wantSplices: 1},
		{name: "disjoint equal length", base: base, result: replaced, wantSplices: 2},
		{name: "timestamp only", base: base, result: timestampOnly, wantSplices: 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			delta, err := buildContextDelta(tc.base, tc.result)
			if err != nil {
				t.Fatalf("buildContextDelta: %v", err)
			}
			if len(delta.Splices) != tc.wantSplices {
				t.Fatalf("splices = %+v, want %d", delta.Splices, tc.wantSplices)
			}
			applyBase := cloneMessagesForTree(tc.base)
			got, refs, err := applyContextDelta(applyBase, resetMessageRefs("base", len(tc.base)), Entry{ID: "delta", ContextDelta: delta})
			if err != nil {
				t.Fatalf("applyContextDelta: %v", err)
			}
			if !transcriptsEqual(got, tc.result) || len(refs) != len(tc.result) {
				t.Fatalf("applied delta = %+v/%+v, want %+v", got, refs, tc.result)
			}
			owned := make([]bool, len(tc.result))
			shift := 0
			for _, splice := range delta.Splices {
				start := splice.Start + shift
				for i := range splice.Messages {
					owned[start+i] = true
				}
				shift += len(splice.Messages) - splice.Delete
			}
			for i, ref := range refs {
				if owned[i] && (ref.entryID != "delta" || ref.offset != i) {
					t.Fatalf("replacement ref[%d] = %+v, want delta ownership at final offset", i, ref)
				}
				if !owned[i] && ref.entryID != "base" {
					t.Fatalf("unchanged ref[%d] = %+v, want base ownership", i, ref)
				}
			}
		})
	}
}

func TestApplyContextDeltaMultipleLengthChangingSplices(t *testing.T) {
	at := time.Date(2026, 7, 19, 12, 0, 0, 0, time.UTC)
	base := largeTreeTranscript(at, 4)
	inserted := []llm.Message{
		treePrompt(at.Add(time.Hour), "inserted prompt"),
		treeAssistant(at.Add(time.Hour), "inserted answer"),
	}
	result := append(cloneMessagesForTree(base[:2]), cloneMessagesForTree(inserted)...)
	result = append(result, cloneMessagesForTree(base[2:6])...)
	baseDigest, err := llm.FingerprintMessages(base)
	if err != nil {
		t.Fatal(err)
	}
	resultDigest, err := llm.FingerprintMessages(result)
	if err != nil {
		t.Fatal(err)
	}
	delta := &ContextDelta{
		BaseMessageCount: len(base), ResultMessageCount: len(result),
		BaseDigest: baseDigest, ResultDigest: resultDigest,
		Splices: []ContextSplice{
			{Start: 2, Messages: inserted},
			{Start: 6, Delete: 2},
		},
	}
	got, refs, err := applyContextDelta(cloneMessagesForTree(base), resetMessageRefs("base", len(base)), Entry{ID: "delta", ContextDelta: delta})
	if err != nil {
		t.Fatal(err)
	}
	if !transcriptsEqual(got, result) {
		t.Fatalf("applied delta = %+v, want %+v", got, result)
	}
	for i, ref := range refs {
		switch {
		case i == 2 || i == 3:
			if ref.entryID != "delta" || ref.offset != i {
				t.Fatalf("inserted ref[%d] = %+v", i, ref)
			}
		case i < 2:
			if ref.entryID != "base" || ref.offset != i {
				t.Fatalf("prefix ref[%d] = %+v", i, ref)
			}
		default:
			if ref.entryID != "base" || ref.offset != i-2 {
				t.Fatalf("shifted suffix ref[%d] = %+v", i, ref)
			}
		}
	}
}

func TestApplyContextDeltaReusesEqualLengthReplayBuffer(t *testing.T) {
	at := time.Date(2026, 7, 19, 12, 0, 0, 0, time.UTC)
	base := largeTreeTranscript(at, 4)
	result := cloneMessagesForTree(base)
	result[3].Content[0].Text = "small rewrite"
	delta, err := buildContextDelta(base, result)
	if err != nil {
		t.Fatal(err)
	}
	before := &base[0]
	got, _, err := applyContextDelta(base, resetMessageRefs("base", len(base)), Entry{ID: "delta", ContextDelta: delta})
	if err != nil {
		t.Fatal(err)
	}
	if &got[0] != before {
		t.Fatal("equal-length delta allocated a replacement replay buffer")
	}
	if !transcriptsEqual(got, result) {
		t.Fatalf("applied delta = %+v, want %+v", got, result)
	}
}

func TestTreeContextDeltaRejectsCorruption(t *testing.T) {
	at := time.Date(2026, 7, 19, 12, 0, 0, 0, time.UTC)
	base := largeTreeTranscript(at, 16)
	tree := NewTree(at, "", "", "")
	if err := tree.AppendContextReset(base, "legacy"); err != nil {
		t.Fatal(err)
	}
	result := cloneMessagesForTree(base)
	result[3].Content[0].Text = "rewrite one"
	result[11].Content[0].Text = "rewrite two"
	if err := tree.SyncTranscript(result); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name   string
		mutate func(*ContextDelta)
		want   string
	}{
		{name: "base count", mutate: func(d *ContextDelta) { d.BaseMessageCount++ }, want: "result message count"},
		{name: "malformed digest", mutate: func(d *ContextDelta) { d.BaseDigest = strings.ToUpper(d.BaseDigest) }, want: "malformed message digest"},
		{name: "base digest", mutate: func(d *ContextDelta) { d.BaseDigest = strings.Repeat("0", 64) }, want: "base digest mismatch"},
		{name: "result digest", mutate: func(d *ContextDelta) { d.ResultDigest = strings.Repeat("0", 64) }, want: "result digest mismatch"},
		{name: "out of order", mutate: func(d *ContextDelta) { d.Splices[1].Start = d.Splices[0].Start }, want: "out of order or overlaps"},
		{name: "out of bounds", mutate: func(d *ContextDelta) { d.Splices[0].Delete = d.BaseMessageCount + 1 }, want: "out of bounds"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			bad := cloneTreeForDeltaTest(tree)
			entry := &bad.Entries[len(bad.Entries)-1]
			tc.mutate(entry.ContextDelta)
			_, err := bad.BuildContext()
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("BuildContext error = %v, want %q", err, tc.want)
			}
		})
	}

	both := cloneEntry(tree.Entries[len(tree.Entries)-1])
	both.Messages = []llm.Message{treePrompt(at, "also snapshot")}
	if err := validateEntry(both); err == nil || !strings.Contains(err.Error(), "both messages") {
		t.Fatalf("validateEntry both encodings = %v", err)
	}
	invalid := cloneMessagesForTree(base)
	invalid[0].Role = "invalid"
	invalidDelta, err := buildContextDelta(base, invalid)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := applyContextDelta(base, resetMessageRefs("base", len(base)), Entry{ID: "invalid", ContextDelta: invalidDelta}); err == nil || !strings.Contains(err.Error(), "result invalid") {
		t.Fatalf("invalid result error = %v", err)
	}
}

func TestTreeContextDeltaRepeatedGrowthAndSnapshotFallback(t *testing.T) {
	at := time.Date(2026, 7, 19, 12, 0, 0, 0, time.UTC)
	current := largeTreeTranscript(at, 24)
	tree := NewTree(at, "", "", "")
	if err := tree.AppendContextReset(current, "initial"); err != nil {
		t.Fatal(err)
	}
	const rewrites = 20
	for i := 0; i < rewrites; i++ {
		next := cloneMessagesForTree(current)
		next[1+i*2].Content[0].Text = fmt.Sprintf("small rewrite %d", i)
		if err := tree.SyncTranscript(next); err != nil {
			t.Fatalf("rewrite %d: %v", i, err)
		}
		entry, _ := tree.Entry(tree.ActiveLeaf)
		if entry.ContextDelta == nil {
			t.Fatalf("rewrite %d used snapshot: %+v", i, entry)
		}
		current = next
	}
	stats := collectTreeStats(tree)
	if stats.snapshotResetEntries != 1 || stats.deltaResetEntries != rewrites || stats.deltaResetBytes >= stats.snapshotResetBytes {
		t.Fatalf("repeated reset storage = %+v, want deltas smaller than one snapshot", stats)
	}

	legacy := cloneTreeForDeltaTest(tree)
	for i, entry := range legacy.Entries {
		if entry.ContextDelta == nil {
			continue
		}
		messages, _, err := tree.buildContext(entry.ID)
		if err != nil {
			t.Fatalf("materialize legacy snapshot %s: %v", entry.ID, err)
		}
		legacy.Entries[i].Messages = messages
		legacy.Entries[i].ContextDelta = nil
	}
	newDir, legacyDir := t.TempDir(), t.TempDir()
	if err := tree.Save(newDir); err != nil {
		t.Fatalf("Save delta tree: %v", err)
	}
	if err := legacy.Save(legacyDir); err != nil {
		t.Fatalf("Save legacy tree: %v", err)
	}
	newInfo, err := os.Stat(filepath.Join(newDir, treeFile))
	if err != nil {
		t.Fatal(err)
	}
	legacyInfo, err := os.Stat(filepath.Join(legacyDir, treeFile))
	if err != nil {
		t.Fatal(err)
	}
	reduction := 100 * (1 - float64(newInfo.Size())/float64(legacyInfo.Size()))
	t.Logf("reset-heavy tree reduction: legacy=%d delta=%d reduction=%.2f%%", legacyInfo.Size(), newInfo.Size(), reduction)
	if reduction < 70 {
		t.Fatalf("reset-heavy tree reduction = %.2f%%, want at least 70%%", reduction)
	}

	entryCount := len(tree.Entries)
	if err := tree.SyncTranscript(cloneMessagesForTree(current)); err != nil {
		t.Fatalf("unchanged SyncTranscript: %v", err)
	}
	if err := tree.AppendContextReset(cloneMessagesForTree(current), "unchanged"); err != nil {
		t.Fatalf("unchanged AppendContextReset: %v", err)
	}
	if len(tree.Entries) != entryCount {
		t.Fatalf("unchanged reset paths appended %d entries", len(tree.Entries)-entryCount)
	}

	small, err := LinearTree(at, "", []llm.Message{treePrompt(at, "a")})
	if err != nil {
		t.Fatal(err)
	}
	if err := small.AppendContextReset([]llm.Message{treePrompt(at, "b")}, "complete_replacement"); err != nil {
		t.Fatal(err)
	}
	entry, _ := small.Entry(small.ActiveLeaf)
	if entry.ContextDelta != nil || len(entry.Messages) != 1 {
		t.Fatalf("non-compact delta did not fall back to snapshot: %+v", entry)
	}
	snapshotPayload, _ := json.Marshal(entry.Messages)
	delta, _ := buildContextDelta([]llm.Message{treePrompt(at, "a")}, entry.Messages)
	deltaPayload, _ := json.Marshal(delta)
	if len(snapshotPayload) >= len(deltaPayload) {
		t.Fatalf("test precondition failed: snapshot=%d delta=%d", len(snapshotPayload), len(deltaPayload))
	}
}

func largeTreeTranscript(at time.Time, pairs int) []llm.Message {
	messages := make([]llm.Message, 0, pairs*2)
	for i := 0; i < pairs; i++ {
		stamp := at.Add(time.Duration(i) * time.Minute)
		messages = append(messages,
			treePrompt(stamp, fmt.Sprintf("prompt %d %s", i, strings.Repeat("p", 800))),
			treeAssistant(stamp, fmt.Sprintf("answer %d %s", i, strings.Repeat("a", 800))),
		)
	}
	return messages
}

func cloneTreeForDeltaTest(tree *Tree) *Tree {
	cloned := &Tree{Header: tree.Header, ActiveLeaf: tree.ActiveLeaf, byID: make(map[string]int)}
	for _, entry := range tree.Entries {
		cloned.byID[entry.ID] = len(cloned.Entries)
		cloned.Entries = append(cloned.Entries, cloneEntry(entry))
	}
	return cloned
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
