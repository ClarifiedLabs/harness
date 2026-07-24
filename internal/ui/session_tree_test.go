package ui

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"harness/internal/llm"
	"harness/internal/session"
)

func TestTreeEntryPreviewRichToolResultUsesSafeMetadata(t *testing.T) {
	payload := "SECRET_BASE64_PAYLOAD"
	entry := session.Entry{Type: session.EntrySegment, Messages: []llm.Message{{
		Role: llm.RoleUser,
		Content: []llm.ContentBlock{{
			Kind: llm.BlockToolResult, ResultForID: "call_1", ResultText: "private result text",
			ResultContent: []llm.ContentBlock{{
				Kind: llm.BlockImage, ImageMediaType: "image/png", ImageData: payload, ImageName: "/private/screen.png",
			}},
		}},
	}}}
	got := treeEntryPreview(entry)
	if got != "tool result · 1 image (image/png)" {
		t.Fatalf("preview = %q", got)
	}
	for _, secret := range []string{payload, "/private/screen.png", "private result text", "call_1"} {
		if strings.Contains(got, secret) {
			t.Fatalf("preview leaked %q: %q", secret, got)
		}
	}
}

func TestForkTreePageContractsHiddenLinearEntries(t *testing.T) {
	first := treeTestPrompt("18c79f4f", "Review the most recent commit")
	second := treeTestPrompt("899bd104", "write up a plan to address all these findings")
	nodes := treeTestChain(
		first,
		session.Entry{Type: session.EntrySegment, ID: "bc63393c", Messages: []llm.Message{{
			Role: llm.RoleAssistant, Content: []llm.ContentBlock{{Kind: llm.BlockText, Text: "I’ll inspect the latest commit."}},
		}}},
		session.Entry{Type: session.EntrySegment, ID: "99a72f8a", Messages: []llm.Message{
			{Role: llm.RoleAssistant, Content: []llm.ContentBlock{{Kind: llm.BlockToolUse, ToolUseID: "call_1", ToolName: "update_todos"}}},
			{Role: llm.RoleUser, Content: []llm.ContentBlock{{Kind: llm.BlockToolResult, ResultForID: "call_1"}}},
		}},
		second,
	)

	choices := flattenTreeChoices(nodes, second.ID, true)
	if len(choices) != 2 {
		t.Fatalf("prompt choices = %d, want 2", len(choices))
	}
	for _, choice := range choices {
		if choice.graph != "•" {
			t.Fatalf("prompt %s graph = %q, want flat unary path", choice.entry.ID, choice.graph)
		}
	}

	var out bytes.Buffer
	printTreePage(&out, choices, 0, 20, "", 120, treePromptPresentation)
	want := "Conversation prompts 1-2 of 2\n" +
		"   1. • 18c79f4f  Review the most recent commit\n" +
		"   2. • 899bd104  write up a plan to address all these findings\n"
	if got := out.String(); got != want {
		t.Fatalf("fork tree page:\n%s\nwant:\n%s", got, want)
	}
}

func TestFlattenTreeChoicesShowsOnlyActualForkDepth(t *testing.T) {
	root := &session.TreeNode{Entry: treeTestPrompt("root0001", "root")}
	oldHidden := &session.TreeNode{Entry: session.Entry{Type: session.EntrySegment, ID: "oldhide1"}}
	newHidden := &session.TreeNode{Entry: session.Entry{Type: session.EntryBranch, ID: "branch01"}}
	oldPrompt := &session.TreeNode{Entry: treeTestPrompt("old00001", "old prompt")}
	newPrompt := &session.TreeNode{Entry: treeTestPrompt("new00001", "new prompt")}
	oldHidden.Children = []*session.TreeNode{oldPrompt}
	newHidden.Children = []*session.TreeNode{{
		Entry:    session.Entry{Type: session.EntrySegment, ID: "newhide1"},
		Children: []*session.TreeNode{newPrompt},
	}}
	root.Children = []*session.TreeNode{oldHidden, newHidden}

	prompts := flattenTreeChoices([]*session.TreeNode{root}, "", true)
	if len(prompts) != 3 {
		t.Fatalf("prompt choices = %d, want 3", len(prompts))
	}
	wantGraphs := []string{"•", "├─•", "└─•"}
	for i, want := range wantGraphs {
		if got := prompts[i].graph; got != want {
			t.Errorf("prompt %d graph = %q, want %q", i, got, want)
		}
	}

	all := flattenTreeChoices(treeTestChain(
		treeTestPrompt("one00001", "one"),
		session.Entry{Type: session.EntrySegment, ID: "two00002"},
		session.Entry{Type: session.EntrySegment, ID: "three003"},
	), "", false)
	for _, choice := range all {
		if choice.graph != "•" {
			t.Errorf("unary checkpoint %s graph = %q, want flat path", choice.entry.ID, choice.graph)
		}
	}
}

func TestTreePageUsesSemanticKindsAndTerminalWidth(t *testing.T) {
	items := []treeChoice{
		{entry: treeTestPrompt("user0001", "Review 漢字 and keep this deliberately long"), graph: "•"},
		{entry: session.Entry{Type: session.EntrySegment, ID: "note0001", Messages: []llm.Message{{
			Role: llm.RoleAssistant, Phase: llm.AssistantPhaseCommentary,
			Content: []llm.ContentBlock{{Kind: llm.BlockText, Text: "Inspecting the implementation"}},
		}}}, graph: strings.Repeat("│ ", 8) + "└─•"},
		{entry: session.Entry{Type: session.EntrySegment, ID: "final001", Messages: []llm.Message{{
			Role: llm.RoleAssistant, Phase: llm.AssistantPhaseFinal,
			Content: []llm.ContentBlock{{Kind: llm.BlockText, Text: "Found the issue"}},
		}}}, graph: "•", active: true},
	}

	var out bytes.Buffer
	printTreePage(&out, items, 0, 20, "", 48, treeCheckpointPresentation)
	lines := strings.Split(strings.TrimSuffix(out.String(), "\n"), "\n")
	if len(lines) != 4 {
		t.Fatalf("rendered lines = %d:\n%s", len(lines), out.String())
	}
	for _, line := range lines {
		if got := displayWidth(line); got > 47 {
			t.Errorf("line width = %d, want <= 47: %q", got, line)
		}
	}
	got := out.String()
	for _, want := range []string{"you", "assistant", "answer", "* • final001"} {
		if !strings.Contains(got, want) {
			t.Errorf("tree page missing %q:\n%s", want, got)
		}
	}
	if !strings.Contains(got, "…") {
		t.Errorf("tree page should visibly clip narrow content:\n%s", got)
	}
}

func TestTreeEntryPreviewCondensesToolsWithoutLeakingPayloads(t *testing.T) {
	entry := session.Entry{Type: session.EntrySegment, Messages: []llm.Message{
		{
			Role: llm.RoleAssistant,
			Content: []llm.ContentBlock{
				{Kind: llm.BlockText, Text: "Checking the code"},
				{Kind: llm.BlockToolUse, ToolUseID: "call_1", ToolName: "delegate", ToolInput: json.RawMessage(`{"secret":"ONE"}`)},
				{Kind: llm.BlockToolUse, ToolUseID: "call_2", ToolName: "delegate", ToolInput: json.RawMessage(`{"secret":"TWO"}`)},
				{Kind: llm.BlockToolUse, ToolUseID: "call_3", ToolName: "rg", ToolInput: json.RawMessage(`{"secret":"THREE"}`)},
			},
		},
		{
			Role: llm.RoleUser,
			Content: []llm.ContentBlock{
				{Kind: llm.BlockToolResult, ResultForID: "call_1", ResultText: "PRIVATE_ONE"},
				{Kind: llm.BlockToolResult, ResultForID: "call_2", ResultText: "PRIVATE_TWO", ResultError: true},
				{Kind: llm.BlockToolResult, ResultForID: "call_3", ResultText: "PRIVATE_THREE", ResultContent: []llm.ContentBlock{
					{Kind: llm.BlockImage, ImageMediaType: "image/png", ImageData: "BASE64_ONE", ImageName: "/private/one.png"},
					{Kind: llm.BlockImage, ImageMediaType: "image/jpeg", ImageData: "BASE64_TWO", ImageName: "/private/two.jpg"},
				}},
			},
		},
	}}

	got := treeEntryPreview(entry)
	want := "Checking the code · delegate ×2, rg · 1 failed · 2 images (image/png, image/jpeg)"
	if got != want {
		t.Fatalf("preview = %q, want %q", got, want)
	}
	if kind := treeEntryKind(entry); kind != "tools" {
		t.Fatalf("kind = %q, want tools", kind)
	}
	for _, secret := range []string{"ONE", "TWO", "THREE", "PRIVATE", "BASE64", "/private/", "call_"} {
		if strings.Contains(got, secret) {
			t.Errorf("preview leaked %q: %q", secret, got)
		}
	}
}

func TestTreeEntryKinds(t *testing.T) {
	tests := []struct {
		name  string
		entry session.Entry
		want  string
	}{
		{name: "prompt", entry: treeTestPrompt("prompt01", "hello"), want: "you"},
		{name: "assistant", entry: session.Entry{Type: session.EntrySegment, Messages: []llm.Message{{Role: llm.RoleAssistant}}}, want: "assistant"},
		{name: "answer", entry: session.Entry{Type: session.EntrySegment, Messages: []llm.Message{{Role: llm.RoleAssistant, Phase: llm.AssistantPhaseFinal}}}, want: "answer"},
		{name: "compact", entry: session.Entry{Type: session.EntryCompaction}, want: "compact"},
		{name: "branch", entry: session.Entry{Type: session.EntryBranch}, want: "branch"},
		{name: "reset", entry: session.Entry{Type: session.EntryContextReset}, want: "reset"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := treeEntryKind(tt.entry); got != tt.want {
				t.Fatalf("kind = %q, want %q", got, tt.want)
			}
		})
	}
}

func treeTestPrompt(id, text string) session.Entry {
	return session.Entry{Type: session.EntrySegment, ID: id, Messages: []llm.Message{{
		Role: llm.RoleUser, Origin: llm.MessageOriginPrompt,
		Content: []llm.ContentBlock{{Kind: llm.BlockText, Text: text}},
	}}}
}

func treeTestChain(entries ...session.Entry) []*session.TreeNode {
	var child *session.TreeNode
	for i := len(entries) - 1; i >= 0; i-- {
		node := &session.TreeNode{Entry: entries[i]}
		if child != nil {
			node.Children = []*session.TreeNode{child}
		}
		child = node
	}
	if child == nil {
		return nil
	}
	return []*session.TreeNode{child}
}

func TestLoadedTreeImagesUsesPersistedSizes(t *testing.T) {
	images := loadedTreeImages([]llm.ContentBlock{{
		Kind: llm.BlockImage, ImageMediaType: "image/png", ImageData: "", ImageBytes: 69, ImageEncodedBytes: 92,
	}})
	if len(images) != 1 || images[0].Info.Bytes != 69 || images[0].Info.EncodedBytes != 92 {
		t.Fatalf("loaded image metadata = %+v", images)
	}
}
