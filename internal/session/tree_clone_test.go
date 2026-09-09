package session

import (
	"testing"
	"time"

	"harness/internal/llm"
)

func TestTreeViewsAreIndependent(t *testing.T) {
	for _, view := range []string{"Entry", "Nodes", "Path", "Extract", "BuildContext", "DivergentMessages"} {
		t.Run(view, func(t *testing.T) {
			messages := []llm.Message{
				treePrompt(time.Time{}, "task"),
				{Role: llm.RoleAssistant, Content: []llm.ContentBlock{{Kind: llm.BlockToolUse, ToolUseID: "call", ToolName: "read", ToolInput: []byte(`{"path":"original"}`)}}},
				{Role: llm.RoleUser, Content: []llm.ContentBlock{{Kind: llm.BlockToolResult, ResultForID: "call", ResultText: "result"}}},
			}
			fingerprint, err := llm.FingerprintMessages(messages)
			if err != nil {
				t.Fatal(err)
			}
			tree := NewTree(time.Time{}, "", "", "")
			if err := tree.AppendContextReset(messages, "test"); err != nil {
				t.Fatal(err)
			}
			messages[1].Content[0].ToolInput[9] = 'x' // Mutating the caller's input must also be safe.
			var got []llm.Message
			switch view {
			case "Entry":
				entry, _ := tree.Entry(tree.ActiveLeaf)
				got = entry.Messages
			case "Nodes":
				got = tree.Nodes()[0].Entry.Messages
			case "Path":
				path, pathErr := tree.Path(tree.ActiveLeaf)
				if pathErr != nil {
					t.Fatal(pathErr)
				}
				got = path[0].Messages
			case "Extract":
				child, extractErr := tree.Extract(tree.ActiveLeaf, time.Time{}, "")
				if extractErr != nil {
					t.Fatal(extractErr)
				}
				got = child.Entries[0].Messages
			case "BuildContext":
				got, err = tree.BuildContext()
			case "DivergentMessages":
				got, err = tree.DivergentMessages(tree.ActiveLeaf, "")
			}
			if err != nil || !llm.MatchesMessageFingerprint(got, fingerprint) {
				t.Fatalf("input mutation changed tree view: %v", err)
			}
			got[1].Content[0].ToolInput[9] = 'y'
			rebuilt, err := tree.BuildContext()
			if err != nil {
				t.Fatal(err)
			}
			if !llm.MatchesMessageFingerprint(rebuilt, fingerprint) || !llm.MatchesMessageFingerprint(tree.Entries[0].Messages, fingerprint) {
				t.Fatal("output mutation changed canonical history")
			}
			if err := llm.ValidateTranscript(rebuilt); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestTreeCheckpointInstructionsSurviveReload(t *testing.T) {
	before := []llm.Message{treePrompt(time.Time{}, "old"), treeAssistant(time.Time{}, "old answer"), treePrompt(time.Time{}, "kept")}
	tree, err := LinearTree(time.Time{}, "", before)
	if err != nil {
		t.Fatal(err)
	}
	checkpoint := llm.Message{
		Role: llm.RoleUser, Origin: llm.MessageOriginCompactionCheckpoint,
		Content: []llm.ContentBlock{{Kind: llm.BlockText, Text: "summary"}},
		Compaction: &llm.CompactionMetadata{
			Summary: "summary", ReadFilesOmitted: 7,
			UserInstructions: []llm.ContentBlock{{Kind: llm.BlockText, Text: "keep these instructions"}},
		},
	}
	if err := tree.PrepareCompaction(before, 2, "summary", "archive", 100, "", nil, nil); err != nil {
		t.Fatal(err)
	}
	after := []llm.Message{checkpoint, before[2]}
	if err := tree.SyncTranscript(after); err != nil {
		t.Fatal(err)
	}
	// Legacy deltas remain readable and their fingerprints must survive cloning.
	after = llm.CloneMessages(after)
	after[1].Content[0].Text = "rewritten"
	appendLegacyContextDeltaForTest(t, tree, after)
	fingerprint, err := llm.FingerprintMessages(after)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	if err := tree.Save(dir); err != nil {
		t.Fatal(err)
	}
	loaded, err := LoadTree(dir, tree.ActiveLeaf)
	if err != nil {
		t.Fatal(err)
	}
	context, err := loaded.BuildContext()
	if err != nil || !llm.MatchesMessageFingerprint(context, fingerprint) {
		t.Fatalf("checkpoint/delta reload changed transcript: %v", err)
	}
	context[0].Compaction.UserInstructions[0].Text = "changed"
	for _, entry := range loaded.Entries {
		view, _ := loaded.Entry(entry.ID)
		if view.Checkpoint != nil {
			view.Checkpoint.Compaction.UserInstructions[0].Text = "changed"
		}
		if view.ContextDelta != nil {
			view.ContextDelta.Splices[0].Messages[0].Content[0].Text = "changed"
		}
	}
	rebuilt, err := loaded.BuildContext()
	if err != nil || !llm.MatchesMessageFingerprint(rebuilt, fingerprint) {
		t.Fatalf("cloned checkpoint/delta aliases canonical history: %v", err)
	}
}
