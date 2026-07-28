package ui

import (
	"bytes"
	"strings"
	"testing"

	"harness/internal/llm"
	"harness/internal/session"
)

func recapPrompt(text string) llm.Message {
	return llm.Message{
		Role:    llm.RoleUser,
		Origin:  llm.MessageOriginPrompt,
		Content: []llm.ContentBlock{{Kind: llm.BlockText, Text: text}},
	}
}

func recapAssistant(phase, text string) llm.Message {
	return llm.Message{
		Role:    llm.RoleAssistant,
		Phase:   phase,
		Content: []llm.ContentBlock{{Kind: llm.BlockText, Text: text}},
	}
}

func TestBuildResumeRecapClean(t *testing.T) {
	s := &session.Session{Messages: []llm.Message{
		recapPrompt("fix the bug"),
		recapAssistant(llm.AssistantPhaseFinal, "Done. All tests pass."),
	}}
	recap := buildResumeRecap(s)
	if recap == nil {
		t.Fatal("recap nil, want clean recap")
	}
	if recap.kind != recapClean {
		t.Fatalf("kind = %v, want recapClean", recap.kind)
	}
	if recap.prompt != "fix the bug" || recap.assistant != "Done. All tests pass." {
		t.Fatalf("recap = %+v", recap)
	}
	if recap.trailer != "" {
		t.Fatalf("clean recap trailer = %q, want empty", recap.trailer)
	}
}

// Pre-phase sessions (Phase == "") ended at a final assistant message must
// classify clean: never cry "interrupted" on old data.
func TestBuildResumeRecapPrePhaseTailIsClean(t *testing.T) {
	s := &session.Session{Messages: []llm.Message{
		recapPrompt("old question"),
		recapAssistant("", "old answer"),
	}}
	recap := buildResumeRecap(s)
	if recap == nil {
		t.Fatal("recap nil, want clean recap")
	}
	if recap.kind != recapClean || recap.trailer != "" {
		t.Fatalf("pre-phase tail = %+v, want clean with no trailer", recap)
	}
}

func TestBuildResumeRecapInterruptedStream(t *testing.T) {
	s := &session.Session{Messages: []llm.Message{
		recapPrompt("explain channels"),
		recapAssistant(llm.AssistantPhaseCommentary, "Channels are"),
	}}
	recap := buildResumeRecap(s)
	if recap == nil {
		t.Fatal("recap nil, want interrupted-stream recap")
	}
	if recap.kind != recapInterruptedStream {
		t.Fatalf("kind = %v, want recapInterruptedStream", recap.kind)
	}
	if recap.assistant != "Channels are" {
		t.Fatalf("assistant = %q, want partial text", recap.assistant)
	}
	if recap.trailer != "[turn interrupted mid-reply — the answer above is partial]" {
		t.Fatalf("trailer = %q", recap.trailer)
	}
}

// A mid-turn commentary message that still carries tool_use blocks is not a
// renderable partial; there is no recap for it.
func TestBuildResumeRecapCommentaryWithToolUseNoRecap(t *testing.T) {
	s := &session.Session{Messages: []llm.Message{
		recapPrompt("run it"),
		{Role: llm.RoleAssistant, Phase: llm.AssistantPhaseCommentary, Content: []llm.ContentBlock{
			{Kind: llm.BlockText, Text: "let me check"},
			{Kind: llm.BlockToolUse, ToolUseID: "call-1", ToolName: "read_file"},
		}},
	}}
	if recap := buildResumeRecap(s); recap != nil {
		t.Fatalf("recap = %+v, want nil", recap)
	}
}

func TestBuildResumeRecapInterruptedTools(t *testing.T) {
	s := &session.Session{Messages: []llm.Message{
		recapPrompt("earlier task"),
		{Role: llm.RoleAssistant, Content: []llm.ContentBlock{
			{Kind: llm.BlockToolUse, ToolUseID: "call-1", ToolName: "read_file"},
			{Kind: llm.BlockToolUse, ToolUseID: "call-2", ToolName: "search"},
		}},
		{Role: llm.RoleUser, Content: []llm.ContentBlock{
			{Kind: llm.BlockToolResult, ResultForID: "call-1", ToolName: "read_file", ResultError: true, ResultText: "interrupted"},
			{Kind: llm.BlockToolResult, ResultForID: "call-2", ToolName: "search", ResultError: true, ResultText: "interrupted"},
		}},
	}}
	recap := buildResumeRecap(s)
	if recap == nil {
		t.Fatal("recap nil, want interrupted-tools recap")
	}
	if recap.kind != recapInterruptedTools {
		t.Fatalf("kind = %v, want recapInterruptedTools", recap.kind)
	}
	want := "[turn interrupted during tool execution: read_file, search did not complete]"
	if recap.trailer != want {
		t.Fatalf("trailer = %q, want %q", recap.trailer, want)
	}
}

// A session that stopped after successful tool execution but before the model
// replied has no interrupted markers and gets the generic trailer.
func TestBuildResumeRecapToolsCompletedBeforeReply(t *testing.T) {
	s := &session.Session{Messages: []llm.Message{
		recapPrompt("read x"),
		{Role: llm.RoleAssistant, Content: []llm.ContentBlock{
			{Kind: llm.BlockToolUse, ToolUseID: "call-1", ToolName: "read_file"},
		}},
		{Role: llm.RoleUser, Content: []llm.ContentBlock{
			{Kind: llm.BlockToolResult, ResultForID: "call-1", ResultText: "file contents"},
		}},
	}}
	recap := buildResumeRecap(s)
	if recap == nil {
		t.Fatal("recap nil, want interrupted-tools recap")
	}
	if recap.kind != recapInterruptedTools {
		t.Fatalf("kind = %v, want recapInterruptedTools", recap.kind)
	}
	if recap.trailer != "[turn ended after tool execution, before the model replied]" {
		t.Fatalf("trailer = %q", recap.trailer)
	}
}

func TestBuildResumeRecapUnansweredPrompt(t *testing.T) {
	s := &session.Session{Messages: []llm.Message{
		recapPrompt("first question"),
		recapAssistant(llm.AssistantPhaseFinal, "first answer"),
		recapPrompt("follow-up never answered"),
	}}
	recap := buildResumeRecap(s)
	if recap == nil {
		t.Fatal("recap nil, want unanswered-prompt recap")
	}
	if recap.kind != recapUnansweredPrompt {
		t.Fatalf("kind = %v, want recapUnansweredPrompt", recap.kind)
	}
	if recap.prompt != "follow-up never answered" || recap.assistant != "first answer" {
		t.Fatalf("recap = %+v", recap)
	}
	if recap.trailer != "[turn interrupted before the model replied]" {
		t.Fatalf("trailer = %q", recap.trailer)
	}
}

func TestBuildResumeRecapCompactionCheckpointTail(t *testing.T) {
	s := &session.Session{Messages: []llm.Message{
		recapPrompt("long task"),
		recapAssistant(llm.AssistantPhaseFinal, "summary of work"),
		{Role: llm.RoleUser, Origin: llm.MessageOriginCompactionCheckpoint, Content: []llm.ContentBlock{
			{Kind: llm.BlockText, Text: "[compaction checkpoint]"},
		}},
	}}
	recap := buildResumeRecap(s)
	if recap == nil {
		t.Fatal("recap nil, want clean recap with compaction note")
	}
	if recap.kind != recapClean {
		t.Fatalf("kind = %v, want recapClean", recap.kind)
	}
	if recap.trailer != "[history was compacted after this exchange]" {
		t.Fatalf("trailer = %q", recap.trailer)
	}
}

// A recovery checkpoint marks a mid-turn death regardless of how tidy the
// materialized tail looks.
func TestBuildResumeRecapRecovered(t *testing.T) {
	s := &session.Session{
		Recovery: &session.RecoveryInfo{Phase: "tool_dispatch", Prompt: 1, Turn: 2},
		Messages: []llm.Message{
			recapPrompt("do things"),
			recapAssistant(llm.AssistantPhaseFinal, "working on it"),
		},
	}
	recap := buildResumeRecap(s)
	if recap == nil {
		t.Fatal("recap nil, want recovered recap")
	}
	if recap.kind != recapRecovered {
		t.Fatalf("kind = %v, want recapRecovered", recap.kind)
	}
	if recap.trailer != "[session ended mid-turn; recovered from checkpoint — showing the last durable exchange]" {
		t.Fatalf("trailer = %q", recap.trailer)
	}
}

func TestBuildResumeRecapNoRecap(t *testing.T) {
	if recap := buildResumeRecap(nil); recap != nil {
		t.Fatalf("nil session recap = %+v, want nil", recap)
	}
	if recap := buildResumeRecap(&session.Session{}); recap != nil {
		t.Fatalf("empty messages recap = %+v, want nil", recap)
	}
}

// The most recent assistant text wins; tool_use-only assistant messages are
// skipped when collecting the excerpt.
func TestBuildResumeRecapSkipsToolUseOnlyAssistant(t *testing.T) {
	s := &session.Session{Messages: []llm.Message{
		recapPrompt("check then answer"),
		recapAssistant(llm.AssistantPhaseCommentary, "checking the file"),
		{Role: llm.RoleAssistant, Content: []llm.ContentBlock{
			{Kind: llm.BlockToolUse, ToolUseID: "call-1", ToolName: "read_file"},
		}},
		{Role: llm.RoleUser, Content: []llm.ContentBlock{
			{Kind: llm.BlockToolResult, ResultForID: "call-1", ResultText: "data"},
		}},
		recapAssistant(llm.AssistantPhaseFinal, "the file says data"),
	}}
	recap := buildResumeRecap(s)
	if recap == nil {
		t.Fatal("recap nil, want clean recap")
	}
	if recap.assistant != "the file says data" {
		t.Fatalf("assistant = %q, want most recent text", recap.assistant)
	}
}

func TestPrintResumeRecapWritesErrwOnly(t *testing.T) {
	var out, errw bytes.Buffer
	r := NewRenderer(&out, &errw, RenderOptions{Markdown: true, Width: func() int { return 72 }})
	app := &App{Renderer: r, Errw: &errw}
	s := &session.Session{Messages: []llm.Message{
		recapPrompt("fix the bug"),
		recapAssistant(llm.AssistantPhaseFinal, "Done. All tests pass."),
	}}

	PrintResumeRecap(app, s)

	if out.Len() != 0 {
		t.Fatalf("recap must not touch stdout, got %q", out.String())
	}
	got := stripRenderTestANSI(errw.String())
	for _, want := range []string{
		"--- resuming session: last exchange ---\n",
		"> fix the bug\n",
		"Done. All tests pass.",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("stderr missing %q, got %q", want, got)
		}
	}
	if !strings.HasSuffix(got, "---\n") {
		t.Errorf("stderr should end with the closing marker, got %q", got)
	}
	if strings.Contains(got, "[turn") || strings.Contains(got, "[session ended") {
		t.Errorf("clean recap should have no trailer, got %q", got)
	}
}

func TestPrintResumeRecapCollapsesPrompt(t *testing.T) {
	var errw bytes.Buffer
	app := &App{Errw: &errw}
	s := &session.Session{Messages: []llm.Message{
		recapPrompt("line one\nline   two\n\nline three"),
	}}

	PrintResumeRecap(app, s)

	got := errw.String()
	if !strings.Contains(got, "> line one line two line three\n") {
		t.Errorf("multiline prompt should collapse to one line, got %q", got)
	}
	if !strings.Contains(got, "[turn interrupted before the model replied]\n") {
		t.Errorf("unanswered-prompt trailer missing, got %q", got)
	}
}

func TestPrintResumeRecapInterruptedStream(t *testing.T) {
	var errw bytes.Buffer
	app := &App{Errw: &errw}
	s := &session.Session{Messages: []llm.Message{
		recapPrompt("explain channels"),
		recapAssistant(llm.AssistantPhaseCommentary, "Channels are"),
	}}

	PrintResumeRecap(app, s)

	got := errw.String()
	if !strings.Contains(got, "Channels are\n") {
		t.Errorf("partial answer missing, got %q", got)
	}
	if !strings.Contains(got, "[turn interrupted mid-reply — the answer above is partial]\n") {
		t.Errorf("partial trailer missing, got %q", got)
	}
}

// A clean tail with neither a prompt nor assistant text prints nothing.
func TestPrintResumeRecapSkipsEmptyClean(t *testing.T) {
	var errw bytes.Buffer
	app := &App{Errw: &errw}
	s := &session.Session{Messages: []llm.Message{
		{Role: llm.RoleAssistant, Phase: llm.AssistantPhaseFinal, Content: []llm.ContentBlock{
			{Kind: llm.BlockText, Text: ""},
		}},
	}}

	PrintResumeRecap(app, s)

	if errw.Len() != 0 {
		t.Fatalf("empty clean recap should print nothing, got %q", errw.String())
	}
}

// A nil session (no resume) prints nothing.
func TestPrintResumeRecapNilSession(t *testing.T) {
	var errw bytes.Buffer
	app := &App{Errw: &errw}
	PrintResumeRecap(app, nil)
	if errw.Len() != 0 {
		t.Fatalf("nil session should print nothing, got %q", errw.String())
	}
}
