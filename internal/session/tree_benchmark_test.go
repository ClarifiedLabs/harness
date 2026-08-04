package session

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"harness/internal/llm"
)

var (
	contextResetBenchmarkBytes    []byte
	contextResetBenchmarkEntry    Entry
	contextResetBenchmarkMessages []llm.Message
	contextResetBenchmarkTree     *Tree
)

type contextResetBenchmarkFixture struct {
	base               []llm.Message
	result             []llm.Message
	deltaTree          *Tree
	snapshotTree       *Tree
	deltaBytes         int
	snapshotBytes      int
	deltaEntryBytes    int
	snapshotEntryBytes int
}

func TestContextResetBenchmarkFixture(t *testing.T) {
	fixture, err := newContextResetBenchmarkFixture(8, 128)
	if err != nil {
		t.Fatal(err)
	}
	if fixture.deltaEntryBytes >= fixture.snapshotEntryBytes || fixture.deltaBytes >= fixture.snapshotBytes {
		t.Fatalf(
			"delta storage entry/tree = %d/%d, want less than snapshot %d/%d",
			fixture.deltaEntryBytes, fixture.deltaBytes, fixture.snapshotEntryBytes, fixture.snapshotBytes,
		)
	}

	tree := NewTree(time.Time{}, "", "", "")
	tree.ActiveLeaf = "parent"
	snapshotEntry := contextResetBenchmarkSnapshotEntry(tree.ActiveLeaf, fixture.result)
	deltaEntry, err := tree.newContextResetEntry(fixture.base, fixture.result, "retention", true)
	if err != nil {
		t.Fatal(err)
	}
	if snapshotEntry.ID != deltaEntry.ID || snapshotEntry.ParentID != deltaEntry.ParentID {
		t.Fatalf(
			"timed entry metadata differs: snapshot id=%q parent=%q, delta id=%q parent=%q",
			snapshotEntry.ID, snapshotEntry.ParentID, deltaEntry.ID, deltaEntry.ParentID,
		)
	}
	snapshotJSON, err := json.Marshal(snapshotEntry)
	if err != nil {
		t.Fatal(err)
	}
	deltaJSON, err := json.Marshal(deltaEntry)
	if err != nil {
		t.Fatal(err)
	}
	if len(deltaJSON) >= len(snapshotJSON) {
		t.Fatalf("timed delta entry bytes = %d, want less than snapshot entry bytes = %d", len(deltaJSON), len(snapshotJSON))
	}
}

func BenchmarkContextResetEncode(b *testing.B) {
	for _, size := range contextResetBenchmarkSizes() {
		fixture, err := newContextResetBenchmarkFixture(size.pairs, size.payloadBytes)
		if err != nil {
			b.Fatal(err)
		}
		tree := NewTree(time.Time{}, "", "", "")
		tree.ActiveLeaf = "parent"
		snapshotEntry := contextResetBenchmarkSnapshotEntry(tree.ActiveLeaf, fixture.result)
		deltaEntry, err := tree.newContextResetEntry(fixture.base, fixture.result, "retention", true)
		if err != nil {
			b.Fatal(err)
		}
		snapshotJSON, err := json.Marshal(snapshotEntry)
		if err != nil {
			b.Fatal(err)
		}
		deltaJSON, err := json.Marshal(deltaEntry)
		if err != nil {
			b.Fatal(err)
		}
		b.Run(size.name+"/snapshot", func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				entry := contextResetBenchmarkSnapshotEntry(tree.ActiveLeaf, fixture.result)
				encoded, err := json.Marshal(entry)
				if err != nil {
					b.Fatal(err)
				}
				contextResetBenchmarkEntry = entry
				contextResetBenchmarkBytes = encoded
			}
			b.ReportMetric(float64(len(snapshotJSON)), "tree-bytes/op")
		})
		b.Run(size.name+"/delta", func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				entry, err := tree.newContextResetEntry(fixture.base, fixture.result, "retention", true)
				if err != nil {
					b.Fatal(err)
				}
				if entry.ContextDelta == nil {
					b.Fatal("benchmark rewrite did not select delta encoding")
				}
				encoded, err := json.Marshal(entry)
				if err != nil {
					b.Fatal(err)
				}
				contextResetBenchmarkEntry = entry
				contextResetBenchmarkBytes = encoded
			}
			b.ReportMetric(float64(len(deltaJSON)), "tree-bytes/op")
			b.ReportMetric(100*(1-float64(len(deltaJSON))/float64(len(snapshotJSON))), "storage-saved-%")
		})
	}
}

func BenchmarkContextResetReplay(b *testing.B) {
	for _, size := range contextResetBenchmarkSizes() {
		fixture, err := newContextResetBenchmarkFixture(size.pairs, size.payloadBytes)
		if err != nil {
			b.Fatal(err)
		}
		for _, tc := range []struct {
			name      string
			tree      *Tree
			treeBytes int
		}{
			{name: "snapshot", tree: fixture.snapshotTree, treeBytes: fixture.snapshotBytes},
			{name: "delta", tree: fixture.deltaTree, treeBytes: fixture.deltaBytes},
		} {
			b.Run(size.name+"/"+tc.name, func(b *testing.B) {
				b.ReportAllocs()
				for b.Loop() {
					messages, err := tc.tree.BuildContext()
					if err != nil {
						b.Fatal(err)
					}
					contextResetBenchmarkMessages = messages
				}
				b.ReportMetric(float64(tc.treeBytes), "tree-bytes/op")
				b.ReportMetric(float64(len(fixture.result)), "messages/op")
			})
		}
	}
}

func BenchmarkContextResetLoad(b *testing.B) {
	for _, size := range contextResetBenchmarkSizes() {
		fixture, err := newContextResetBenchmarkFixture(size.pairs, size.payloadBytes)
		if err != nil {
			b.Fatal(err)
		}
		for _, tc := range []struct {
			name string
			tree *Tree
		}{
			{name: "snapshot", tree: fixture.snapshotTree},
			{name: "delta", tree: fixture.deltaTree},
		} {
			b.Run(size.name+"/"+tc.name, func(b *testing.B) {
				dir := b.TempDir()
				if err := tc.tree.Save(dir); err != nil {
					b.Fatal(err)
				}
				info, err := os.Stat(filepath.Join(dir, treeFile))
				if err != nil {
					b.Fatal(err)
				}
				leaf := tc.tree.ActiveLeaf
				b.ReportAllocs()
				for b.Loop() {
					loaded, err := LoadTree(dir, leaf)
					if err != nil {
						b.Fatal(err)
					}
					contextResetBenchmarkTree = loaded
				}
				b.ReportMetric(float64(info.Size()), "tree-bytes/op")
			})
		}
	}
}

type contextResetBenchmarkSize struct {
	name         string
	pairs        int
	payloadBytes int
}

func contextResetBenchmarkSizes() []contextResetBenchmarkSize {
	return []contextResetBenchmarkSize{
		{name: "64KiB", pairs: 16, payloadBytes: 2 << 10},
		{name: "256KiB", pairs: 64, payloadBytes: 2 << 10},
		{name: "1MiB", pairs: 256, payloadBytes: 2 << 10},
	}
}

func newContextResetBenchmarkFixture(pairs, payloadBytes int) (contextResetBenchmarkFixture, error) {
	base := makeContextResetBenchmarkTranscript(pairs, payloadBytes)
	if err := llm.ValidateTranscript(base); err != nil {
		return contextResetBenchmarkFixture{}, fmt.Errorf("validate base transcript: %w", err)
	}
	result := cloneMessagesForTree(base)
	for pair := 0; pair < pairs; pair += 8 {
		index := pair*2 + 1
		result[index].Content[0].Text = fmt.Sprintf("[older result %d trimmed by retention]", pair)
	}
	if err := llm.ValidateTranscript(result); err != nil {
		return contextResetBenchmarkFixture{}, fmt.Errorf("validate rewritten transcript: %w", err)
	}

	created := time.Date(2026, 8, 4, 12, 0, 0, 0, time.UTC)
	deltaTree, err := LinearTree(created, "/benchmark", base)
	if err != nil {
		return contextResetBenchmarkFixture{}, fmt.Errorf("build delta tree: %w", err)
	}
	if err := deltaTree.SyncTranscript(result); err != nil {
		return contextResetBenchmarkFixture{}, fmt.Errorf("append delta reset: %w", err)
	}
	deltaEntry := deltaTree.Entries[len(deltaTree.Entries)-1]
	if deltaEntry.Type != EntryContextReset || deltaEntry.ContextDelta == nil {
		return contextResetBenchmarkFixture{}, fmt.Errorf("context reset benchmark: rewrite did not select delta encoding")
	}

	snapshotTree, err := LinearTree(created, "/benchmark", base)
	if err != nil {
		return contextResetBenchmarkFixture{}, fmt.Errorf("build snapshot tree: %w", err)
	}
	snapshotEntry := contextResetBenchmarkSnapshotEntry(snapshotTree.ActiveLeaf, result)
	if err := snapshotTree.appendEntry(snapshotEntry); err != nil {
		return contextResetBenchmarkFixture{}, fmt.Errorf("append snapshot reset: %w", err)
	}

	deltaBytes, err := marshalContextResetBenchmarkTree(deltaTree)
	if err != nil {
		return contextResetBenchmarkFixture{}, err
	}
	snapshotBytes, err := marshalContextResetBenchmarkTree(snapshotTree)
	if err != nil {
		return contextResetBenchmarkFixture{}, err
	}
	deltaEntryJSON, err := json.Marshal(deltaEntry)
	if err != nil {
		return contextResetBenchmarkFixture{}, err
	}
	snapshotEntry = snapshotTree.Entries[len(snapshotTree.Entries)-1]
	snapshotEntryJSON, err := json.Marshal(snapshotEntry)
	if err != nil {
		return contextResetBenchmarkFixture{}, err
	}
	if len(deltaEntryJSON) >= len(snapshotEntryJSON) || len(deltaBytes) >= len(snapshotBytes) {
		return contextResetBenchmarkFixture{}, fmt.Errorf(
			"delta did not reduce storage: entry %d >= %d or tree %d >= %d",
			len(deltaEntryJSON), len(snapshotEntryJSON), len(deltaBytes), len(snapshotBytes),
		)
	}
	for name, tree := range map[string]*Tree{"delta": deltaTree, "snapshot": snapshotTree} {
		messages, err := tree.BuildContext()
		if err != nil {
			return contextResetBenchmarkFixture{}, fmt.Errorf("replay %s tree: %w", name, err)
		}
		if !transcriptsEqual(messages, result) {
			return contextResetBenchmarkFixture{}, fmt.Errorf("replay %s tree produced the wrong transcript", name)
		}
	}
	return contextResetBenchmarkFixture{
		base: base, result: result, deltaTree: deltaTree, snapshotTree: snapshotTree,
		deltaBytes: len(deltaBytes), snapshotBytes: len(snapshotBytes),
		deltaEntryBytes: len(deltaEntryJSON), snapshotEntryBytes: len(snapshotEntryJSON),
	}, nil
}

func contextResetBenchmarkSnapshotEntry(parentID string, messages []llm.Message) Entry {
	entry := Entry{
		Type: EntryContextReset, ParentID: parentID,
		Messages: cloneMessagesForTree(messages), Reason: "retention",
	}
	if len(messages) > 0 {
		entry.Time = messages[0].Time
	}
	return entry
}

func makeContextResetBenchmarkTranscript(pairs, payloadBytes int) []llm.Message {
	at := time.Date(2026, 8, 4, 12, 0, 0, 0, time.UTC)
	messages := make([]llm.Message, 0, pairs*2)
	for pair := 0; pair < pairs; pair++ {
		stamp := at.Add(time.Duration(pair) * time.Minute)
		messages = append(messages,
			llm.Message{
				Role: llm.RoleUser, Time: stamp, Origin: llm.MessageOriginPrompt,
				Content: []llm.ContentBlock{{Kind: llm.BlockText, Text: fmt.Sprintf("prompt %04d %s", pair, strings.Repeat("p", payloadBytes))}},
			},
			llm.Message{
				Role: llm.RoleAssistant, Time: stamp,
				Content: []llm.ContentBlock{{Kind: llm.BlockText, Text: fmt.Sprintf("result %04d %s", pair, strings.Repeat("r", payloadBytes))}},
			},
		)
	}
	return messages
}

func marshalContextResetBenchmarkTree(tree *Tree) ([]byte, error) {
	var out bytes.Buffer
	encoder := json.NewEncoder(&out)
	if err := encoder.Encode(tree.Header); err != nil {
		return nil, err
	}
	for _, entry := range tree.Entries {
		if err := encoder.Encode(entry); err != nil {
			return nil, err
		}
	}
	return out.Bytes(), nil
}
