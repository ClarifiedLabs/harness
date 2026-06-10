package session

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"harness/internal/llm"
	"harness/internal/todo"
)

// sampleSession builds a valid session whose transcript contains a complete
// tool_use/tool_result pair, so ValidateTranscript passes before any mutation.
func sampleSession() Session {
	created := time.Date(2026, 6, 9, 10, 0, 0, 0, time.UTC)
	msgTime := created.Add(time.Minute)
	return Session{
		Version:  Version,
		Provider: "anthropic",
		Model:    "claude-opus-4-8",
		Created:  created,
		Updated:  created.Add(2 * time.Minute),
		System:   "be terse",
		Messages: []llm.Message{
			{Role: llm.RoleUser, Time: msgTime, Content: []llm.ContentBlock{
				{Kind: llm.BlockText, Text: "list the dir"},
			}},
			{Role: llm.RoleAssistant, Time: msgTime, Content: []llm.ContentBlock{
				{Kind: llm.BlockText, Text: "sure"},
				{Kind: llm.BlockToolUse, ToolUseID: "call_1", ToolName: "list_dir", ToolInput: json.RawMessage(`{"path":"."}`)},
			}},
			{Role: llm.RoleUser, Time: msgTime, Content: []llm.ContentBlock{
				{Kind: llm.BlockToolResult, ResultForID: "call_1", ResultText: "main.go"},
			}},
			{Role: llm.RoleAssistant, Time: msgTime, Content: []llm.ContentBlock{
				{Kind: llm.BlockText, Text: "done"},
			}},
		},
		Usage: UsageTotals{
			Usage:   llm.Usage{InputTokens: 1200, OutputTokens: 340, CacheReadTokens: 800, CacheWriteTokens: 0},
			CostUSD: 0.0123,
		},
	}
}

func TestSaveBackfillsMissingMessageTimestamps(t *testing.T) {
	s := sampleSession()
	s.Messages[0].Time = time.Time{}
	path := filepath.Join(t.TempDir(), "session")
	if err := s.Save(path); err != nil {
		t.Fatalf("Save: %v", err)
	}
	got, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got.Messages[0].Time.IsZero() {
		t.Fatalf("missing message timestamp was not backfilled")
	}
	if !got.Messages[0].Time.Equal(s.Updated) {
		t.Fatalf("backfilled timestamp = %s, want updated %s", got.Messages[0].Time, s.Updated)
	}
}

func TestAppendEventStampsMissingTime(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "session")
	if err := AppendEvent(dir, Event{Type: EventUser, Turn: 1, Text: "hello"}); err != nil {
		t.Fatalf("AppendEvent: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(dir, "raw.ndjson"))
	if err != nil {
		t.Fatalf("read event log: %v", err)
	}
	var ev Event
	if err := json.Unmarshal(bytes.TrimSpace(data), &ev); err != nil {
		t.Fatalf("decode event: %v", err)
	}
	if ev.Time.IsZero() {
		t.Fatalf("event time was not stamped")
	}
}

func TestSaveLoadRoundTrip(t *testing.T) {
	s := sampleSession()
	s.ResponseState = &llm.ResponseState{PreviousResponseID: "resp_1", AnchorMessages: len(s.Messages)}
	if err := llm.ValidateTranscript(s.Messages); err != nil {
		t.Fatalf("sample transcript invalid: %v", err)
	}

	path := filepath.Join(t.TempDir(), "session")
	if err := s.Save(path); err != nil {
		t.Fatalf("Save: %v", err)
	}

	got, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if err := llm.ValidateTranscript(got.Messages); err != nil {
		t.Fatalf("loaded transcript invalid: %v", err)
	}
	if !reflect.DeepEqual(s, got) {
		t.Fatalf("round-trip mismatch:\n want %+v\n  got %+v", s, got)
	}
}

func TestSaveLoadPreservesImageBlocks(t *testing.T) {
	s := sampleSession()
	s.Messages = []llm.Message{{
		Role: llm.RoleUser,
		Content: []llm.ContentBlock{
			{Kind: llm.BlockImage, ImageMediaType: "image/png", ImageData: "abc123", ImageDetail: "high", ImageName: "screen.png", ImageWidth: 1, ImageHeight: 1},
			{Kind: llm.BlockText, Text: "describe it"},
		},
	}}
	path := filepath.Join(t.TempDir(), "session")
	if err := s.Save(path); err != nil {
		t.Fatalf("Save: %v", err)
	}
	got, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	content := got.Messages[0].Content
	if len(content) != 2 || content[0].Kind != llm.BlockImage {
		t.Fatalf("content = %+v, want image + text", content)
	}
	if content[0].ImageData != "abc123" || content[0].ImageDetail != "high" || content[0].ImageWidth != 1 {
		t.Fatalf("image block = %+v", content[0])
	}
}

// The active agent round-trips so a resumed session can restore its restricted
// tool set, not just its saved system prompt.
func TestSaveLoadPreservesAgent(t *testing.T) {
	s := sampleSession()
	s.Agent = "plan"
	path := filepath.Join(t.TempDir(), "session")
	if err := s.Save(path); err != nil {
		t.Fatalf("Save: %v", err)
	}
	got, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got.Agent != "plan" {
		t.Errorf("Agent = %q, want plan", got.Agent)
	}
}

func TestSaveLoadPreservesTodos(t *testing.T) {
	s := sampleSession()
	s.Todos = []todo.Item{
		{Content: "explore", Status: todo.StatusCompleted},
		{Content: "implement", Status: todo.StatusInProgress, ActiveForm: "Implementing"},
		{Content: "test", Status: todo.StatusPending},
	}
	path := filepath.Join(t.TempDir(), "session")
	if err := s.Save(path); err != nil {
		t.Fatalf("Save: %v", err)
	}
	got, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !reflect.DeepEqual(got.Todos, s.Todos) {
		t.Errorf("Todos = %+v, want %+v", got.Todos, s.Todos)
	}
}

// A second save over the same path (the after-every-turn case) round-trips too.
func TestSaveLoadSaveRoundTrip(t *testing.T) {
	s := sampleSession()
	path := filepath.Join(t.TempDir(), "session")
	if err := s.Save(path); err != nil {
		t.Fatalf("first Save: %v", err)
	}
	loaded, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	loaded.Updated = loaded.Updated.Add(time.Minute)
	if err := loaded.Save(path); err != nil {
		t.Fatalf("second Save: %v", err)
	}
	again, err := Load(path)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if !reflect.DeepEqual(loaded, again) {
		t.Fatalf("save->load->save mismatch:\n want %+v\n  got %+v", loaded, again)
	}
}

func TestSaveLeavesNoTmpFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "session")
	if err := sampleSession().Save(path); err != nil {
		t.Fatalf("Save: %v", err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".tmp") {
			t.Fatalf("temp file left behind: %s", e.Name())
		}
	}
	if len(entries) != 1 || entries[0].Name() != "session" {
		t.Fatalf("expected exactly one file after save, got %d: %v", len(entries), entries)
	}
	stateEntries, err := os.ReadDir(path)
	if err != nil {
		t.Fatalf("ReadDir session: %v", err)
	}
	if len(stateEntries) != 1 || stateEntries[0].Name() != stateFile {
		t.Fatalf("expected only %s after save, got %v", stateFile, stateEntries)
	}
}

// Save creates parent directories so DefaultPath's nested sessions dir works.
func TestSaveCreatesParentDirs(t *testing.T) {
	path := filepath.Join(t.TempDir(), "a", "b", "session")
	if err := sampleSession().Save(path); err != nil {
		t.Fatalf("Save: %v", err)
	}
	if _, err := os.Stat(filepath.Join(path, stateFile)); err != nil {
		t.Fatalf("session not written: %v", err)
	}
}

// A transcript saved mid-turn ends with an assistant tool_use that has no
// matching result. Loading must repair it by synthesizing an interrupted result,
// yielding a transcript that passes ValidateTranscript.
func TestLoadRepairsDanglingToolUse(t *testing.T) {
	dangling := Session{
		Version:  Version,
		Provider: "anthropic",
		Model:    "claude-opus-4-8",
		Created:  time.Date(2026, 6, 9, 10, 0, 0, 0, time.UTC),
		Updated:  time.Date(2026, 6, 9, 10, 1, 0, 0, time.UTC),
		Messages: []llm.Message{
			{Role: llm.RoleUser, Content: []llm.ContentBlock{
				{Kind: llm.BlockText, Text: "edit the file"},
			}},
			{Role: llm.RoleAssistant, Content: []llm.ContentBlock{
				{Kind: llm.BlockToolUse, ToolUseID: "call_x", ToolName: "edit", ToolInput: json.RawMessage(`{}`)},
				{Kind: llm.BlockToolUse, ToolUseID: "call_y", ToolName: "edit", ToolInput: json.RawMessage(`{}`)},
			}},
		},
	}
	// Validate the pre-repair transcript IS dangling (the bug we are fixing).
	if err := llm.ValidateTranscript(dangling.Messages); err == nil {
		t.Fatalf("expected dangling transcript to be invalid before repair")
	}

	path := filepath.Join(t.TempDir(), "session")
	if err := dangling.Save(path); err != nil {
		t.Fatalf("Save: %v", err)
	}
	got, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if err := llm.ValidateTranscript(got.Messages); err != nil {
		t.Fatalf("repaired transcript invalid: %v", err)
	}

	// The repair appends one user message carrying interrupted results, in call
	// order, for every dangling tool_use.
	last := got.Messages[len(got.Messages)-1]
	if last.Role != llm.RoleUser {
		t.Fatalf("repair message role %q, want user", last.Role)
	}
	if len(last.Content) != 2 {
		t.Fatalf("repair carried %d results, want 2", len(last.Content))
	}
	for i, want := range []string{"call_x", "call_y"} {
		b := last.Content[i]
		if b.Kind != llm.BlockToolResult {
			t.Fatalf("block %d kind %q, want tool_result", i, b.Kind)
		}
		if b.ResultForID != want {
			t.Fatalf("block %d result_for_id %q, want %q", i, b.ResultForID, want)
		}
		if !b.ResultError {
			t.Fatalf("block %d result_error false, want true", i)
		}
		if b.ResultText != "interrupted" {
			t.Fatalf("block %d result_text %q, want \"interrupted\"", i, b.ResultText)
		}
	}
}

// A complete transcript is loaded unchanged (no spurious repair message).
func TestLoadDoesNotRepairCompleteTranscript(t *testing.T) {
	s := sampleSession()
	path := filepath.Join(t.TempDir(), "session")
	if err := s.Save(path); err != nil {
		t.Fatalf("Save: %v", err)
	}
	got, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(got.Messages) != len(s.Messages) {
		t.Fatalf("message count changed: %d -> %d (spurious repair?)", len(s.Messages), len(got.Messages))
	}
}

// Saved files are provider-neutral: the internal JSON tags (kind, tool_use_id,
// ...) must appear, and no OpenAI wire strings (function, tool_calls) may leak.
func TestSavedFileIsProviderNeutral(t *testing.T) {
	path := filepath.Join(t.TempDir(), "session")
	if err := sampleSession().Save(path); err != nil {
		t.Fatalf("Save: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(path, stateFile))
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	body := string(data)
	for _, forbidden := range []string{"function", "tool_calls", "tool_call_id", "arguments"} {
		if strings.Contains(body, forbidden) {
			t.Fatalf("saved session leaked provider wire string %q:\n%s", forbidden, body)
		}
	}
	for _, want := range []string{"tool_use_id", "result_for_id"} {
		if !strings.Contains(body, want) {
			t.Fatalf("saved session missing provider-neutral tag %q", want)
		}
	}
}

// Cross-provider resume: a session saved under anthropic loads cleanly and its
// transcript is re-sendable; the caller (Phase 10) overrides provider/model from
// flags. Here we assert the loaded transcript is valid and provider field is
// preserved as recorded.
func TestCrossProviderResume(t *testing.T) {
	s := sampleSession()
	path := filepath.Join(t.TempDir(), "session")
	if err := s.Save(path); err != nil {
		t.Fatalf("Save: %v", err)
	}
	got, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got.Provider != "anthropic" {
		t.Fatalf("provider %q not preserved", got.Provider)
	}
	if err := llm.ValidateTranscript(got.Messages); err != nil {
		t.Fatalf("transcript not re-sendable under a different provider: %v", err)
	}
}

func TestLoadMissingFileIsError(t *testing.T) {
	_, err := Load(filepath.Join(t.TempDir(), "nope"))
	if err == nil {
		t.Fatalf("expected error loading missing session file")
	}
}

func TestLoadMalformedFileIsError(t *testing.T) {
	path := filepath.Join(t.TempDir(), "bad")
	if err := os.MkdirAll(path, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(path, stateFile), []byte("{not json"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	_, err := Load(path)
	if err == nil {
		t.Fatalf("expected error loading malformed session file")
	}
}

func TestReplayPrintsUserFacingView(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "session")
	events := []Event{
		{Type: EventUser, Turn: 1, Text: "fix it"},
		{Type: EventAssistantDelta, Turn: 1, Text: "I'll check **now** and use [docs](https://docs.example.com).\n"},
		{Type: EventReasoningSummary, Turn: 1, Text: "Checked **the repo**.\nNext [step](https://example.com)."},
		{Type: EventModelTurnUsage, Turn: 1, Display: "[model: turn 1 cost: $0.0010 · totals: $0.0010 prompt · $0.0010 session]"},
		{Type: EventToolResult, Turn: 1, Display: `[rg pattern="panic" .] → 2 lines, 80B`},
		{Type: EventToolDiff, Turn: 1, Display: "--- a/f.txt\n+++ b/f.txt\n@@ -1,1 +1,1 @@\n-old\n+new"},
		{Type: EventNotice, Turn: 1, Display: "[compacted: 6 messages → summary]"},
		{Type: EventTurnUsage, Turn: 1, Display: "[turn: 2 model turns · 1.0k in / 100 out]"},
	}
	for _, ev := range events {
		if err := AppendEvent(dir, ev); err != nil {
			t.Fatalf("AppendEvent: %v", err)
		}
	}
	var out strings.Builder
	if err := Replay(dir, &out, ReplayOptions{Markdown: true}); err != nil {
		t.Fatalf("Replay: %v", err)
	}
	got := out.String()
	for _, want := range []string{"> fix it", "I'll check now and use docs <https://docs.example.com>.", "[reasoning]\n", "  Checked the repo.", "  Next step <https://example.com>.", "[model: turn 1 cost:", `[rg pattern="panic" .]`, "--- a/f.txt", "-old\n+new", "[compacted:", "[turn:"} {
		if !strings.Contains(got, want) {
			t.Fatalf("replay missing %q:\n%s", want, got)
		}
	}
}

func TestReplayFiltersAbandonedAttemptOutput(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "session")
	events := []Event{
		{Type: EventUser, Turn: 1, Text: "fix it"},
		{Type: EventModelTurnStart, Turn: 1, ModelTurns: 1, Attempt: 1},
		{Type: EventAssistantDelta, Turn: 1, ModelTurns: 1, Attempt: 1, Text: "discarded partial"},
		{Type: EventReasoningSummary, Turn: 1, ModelTurns: 1, Attempt: 1, Text: "discarded reasoning"},
		{Type: EventModelTurnAbandoned, Turn: 1, ModelTurns: 1, Attempt: 1, Display: "[model: turn 1 attempt 1 discarded; retrying]"},
		{Type: EventNotice, Turn: 1, Display: "[stream interrupted: retrying]"},
		{Type: EventModelTurnStart, Turn: 1, ModelTurns: 1, Attempt: 2},
		{Type: EventAssistantDelta, Turn: 1, ModelTurns: 1, Attempt: 2, Text: "final answer"},
	}
	for _, ev := range events {
		if err := AppendEvent(dir, ev); err != nil {
			t.Fatalf("AppendEvent: %v", err)
		}
	}

	var replay strings.Builder
	if err := Replay(dir, &replay, ReplayOptions{}); err != nil {
		t.Fatalf("Replay: %v", err)
	}
	gotReplay := replay.String()
	for _, bad := range []string{"discarded partial", "discarded reasoning"} {
		if strings.Contains(gotReplay, bad) {
			t.Fatalf("replay included abandoned attempt output %q:\n%s", bad, gotReplay)
		}
	}
	if !strings.Contains(gotReplay, "[model: turn 1 attempt 1 discarded; retrying]") || !strings.Contains(gotReplay, "final answer") {
		t.Fatalf("replay missing abandoned marker or final answer:\n%s", gotReplay)
	}

	latest, err := LatestTurnOutput(dir)
	if err != nil {
		t.Fatalf("LatestTurnOutput: %v", err)
	}
	if strings.Contains(latest, "discarded partial") || strings.Contains(latest, "discarded reasoning") ||
		latest != "[model: turn 1 attempt 1 discarded; retrying]\n[stream interrupted: retrying]\nfinal answer" {
		t.Fatalf("latest output did not filter abandoned attempt correctly: %q", latest)
	}
}

func TestTimingsPrintsWallClockReport(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "session")
	base := time.Date(2026, 6, 9, 10, 0, 0, 0, time.UTC)
	events := []Event{
		{Time: base, Type: EventUser, Turn: 1, Text: "fix it"},
		{Time: base.Add(100 * time.Millisecond), Type: EventModelTurnStart, Turn: 1, ModelTurns: 1, Attempt: 1, Context: &ContextSnapshot{Total: 1000, Window: 4000, PayloadTotal: 400, Tools: 120}},
		{Time: base.Add(1200 * time.Millisecond), Type: EventToolStart, Turn: 1, ToolID: "call_1", Tool: "read_file"},
		{Time: base.Add(1500 * time.Millisecond), Type: EventToolResult, Turn: 1, ToolID: "call_1", Tool: "read_file"},
		{Time: base.Add(1600 * time.Millisecond), Type: EventModelTurnUsage, Turn: 1, ModelTurns: 1, Attempt: 1},
		{Time: base.Add(2 * time.Second), Type: EventTurnUsage, Turn: 1},
	}
	for _, ev := range events {
		if err := AppendEvent(dir, ev); err != nil {
			t.Fatalf("AppendEvent: %v", err)
		}
	}

	var out strings.Builder
	if err := Timings(dir, &out); err != nil {
		t.Fatalf("Timings: %v", err)
	}
	got := out.String()
	for _, want := range []string{"turn 1: total 2s", "first visible 1.2s", "model turn 1 attempt 1: 1.5s", "payload 400", "tool read_file: 300ms", "gap 1.1s"} {
		if !strings.Contains(got, want) {
			t.Fatalf("timings missing %q:\n%s", want, got)
		}
	}
}

func TestTimingsTreatsReasoningSummaryAsVisible(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "session")
	base := time.Date(2026, 6, 9, 10, 0, 0, 0, time.UTC)
	events := []Event{
		{Time: base, Type: EventUser, Turn: 1, Text: "fix it"},
		{Time: base.Add(400 * time.Millisecond), Type: EventReasoningSummary, Turn: 1, Text: "Checking."},
		{Time: base.Add(2 * time.Second), Type: EventTurnUsage, Turn: 1},
	}
	for _, ev := range events {
		if err := AppendEvent(dir, ev); err != nil {
			t.Fatalf("AppendEvent: %v", err)
		}
	}

	var out strings.Builder
	if err := Timings(dir, &out); err != nil {
		t.Fatalf("Timings: %v", err)
	}
	if got := out.String(); !strings.Contains(got, "first visible 400ms") {
		t.Fatalf("timings should treat reasoning summaries as visible output:\n%s", got)
	}
}

func TestLatestTurnOutputReturnsLatestVisibleOutputWithoutUserPrompt(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "session")
	events := []Event{
		{Type: EventUser, Turn: 1, Text: "first prompt"},
		{Type: EventAssistantDelta, Turn: 1, Text: "old answer\n"},
		{Type: EventTurnUsage, Turn: 1, Display: "[turn: 1 model turn]"},
		{Type: EventUser, Turn: 2, Text: "second prompt"},
		{Type: EventAssistantDelta, Turn: 2, Text: "new **answer**"},
		{Type: EventReasoningSummary, Turn: 2, Text: "Checked state."},
		{Type: EventModelTurnUsage, Turn: 2, Display: "[model: turn 1 cost: $0.0010 · totals: $0.0010 prompt · $0.0010 session]"},
		{Type: EventToolResult, Turn: 2, Display: `[read_file path="x"] → 12B`},
		{Type: EventNotice, Turn: 2, Display: "[notice]"},
		{Type: EventTurnUsage, Turn: 2, Display: "[turn: 2 model turns]"},
	}
	for _, ev := range events {
		if err := AppendEvent(dir, ev); err != nil {
			t.Fatalf("AppendEvent: %v", err)
		}
	}

	got, err := LatestTurnOutput(dir)
	if err != nil {
		t.Fatalf("LatestTurnOutput: %v", err)
	}
	want := "new answer\n" +
		"[reasoning]\n" +
		"  Checked state.\n" +
		"[end reasoning]\n" +
		"[model: turn 1 cost: $0.0010 · totals: $0.0010 prompt · $0.0010 session]\n" +
		`[read_file path="x"] → 12B` + "\n" +
		"[notice]\n" +
		"[turn: 2 model turns]"
	if got != want {
		t.Fatalf("latest output mismatch:\nwant %q\n got %q", want, got)
	}
	if strings.Contains(got, "second prompt") || strings.Contains(got, "old answer") {
		t.Fatalf("latest output included wrong turn/user text: %q", got)
	}
}

func TestLatestTurnOutputMissingLogIsEmpty(t *testing.T) {
	got, err := LatestTurnOutput(filepath.Join(t.TempDir(), "missing"))
	if err != nil {
		t.Fatalf("LatestTurnOutput missing log: %v", err)
	}
	if got != "" {
		t.Fatalf("missing log output = %q, want empty", got)
	}
}

// DefaultPath builds a timestamped directory path under an injectable state dir.
func TestDefaultPath(t *testing.T) {
	stateDir := t.TempDir()
	at := time.Date(2026, 6, 9, 14, 30, 15, 0, time.UTC)
	p := DefaultPath(stateDir, at)
	if filepath.Dir(p) != filepath.Join(stateDir, "harness", "sessions") {
		t.Fatalf("DefaultPath dir %q unexpected", filepath.Dir(p))
	}
	if strings.HasSuffix(p, ".json") {
		t.Fatalf("DefaultPath %q should be a directory path, not a .json file", p)
	}
	// The timestamp must round to a path that does not collide minute-to-minute.
	p2 := DefaultPath(stateDir, at.Add(time.Second))
	if p == p2 {
		t.Fatalf("DefaultPath collides one second apart: %q", p)
	}
}
