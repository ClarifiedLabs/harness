package session

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"harness/internal/goal"
	"harness/internal/llm"
	"harness/internal/markdown"
	"harness/internal/plan"
	"harness/internal/term/highlight"
	"harness/internal/todo"
)

const sessionOnePixelPNG = "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAIAAACQd1PeAAAADElEQVR4nGP4z8AAAAMBAQDJ/pLvAAAAAElFTkSuQmCC"

func stripSessionTestANSI(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); {
		if s[i] == '\x1b' && i+1 < len(s) && s[i+1] == '[' {
			i += 2
			for i < len(s) && (s[i] < '@' || s[i] > '~') {
				i++
			}
			if i < len(s) {
				i++
			}
			continue
		}
		b.WriteByte(s[i])
		i++
	}
	return b.String()
}

func TestReplayDimsStatusLinesWithANSI(t *testing.T) {
	dir := t.TempDir()
	events := []Event{
		{Type: EventUser, Prompt: 1, Text: "hello"},
		{Type: EventAssistantDelta, Prompt: 1, Turn: 1, Text: "world"},
		{Type: EventToolResult, Prompt: 1, Turn: 1, Display: "[read_file] path=a.go → 3 lines"},
		{Type: EventTurnComplete, Prompt: 1, Turn: 1, Display: "[turn: 1 · 1.0s]"},
		{Type: EventPromptUsage, Prompt: 1, Display: "[prompt: 1 turn · 1.0k in / 0.1k out · 0.1s]"},
	}
	for _, ev := range events {
		if err := AppendEvent(dir, ev); err != nil {
			t.Fatalf("AppendEvent: %v", err)
		}
	}

	var out strings.Builder
	if err := Replay(dir, &out, ReplayOptions{ANSI: true}); err != nil {
		t.Fatalf("Replay: %v", err)
	}
	got := out.String()
	for _, line := range []string{"[read_file] path=a.go → 3 lines", "[turn: 1 · 1.0s]", "[prompt: 1 turn"} {
		if !strings.Contains(got, ansiDim+line) {
			t.Fatalf("status line %q not dimmed:\n%q", line, got)
		}
	}
	if !strings.Contains(got, "\n"+ansiDim+markdown.HorizontalRule+ansiReset+"\n") {
		t.Fatalf("prompt separator not dimmed:\n%q", got)
	}
	if stripped := stripSessionTestANSI(got); !strings.Contains(stripped, "[turn: 1 · 1.0s]") {
		t.Fatalf("stripped replay lost status text:\n%q", stripped)
	}

	var plain strings.Builder
	if err := Replay(dir, &plain, ReplayOptions{}); err != nil {
		t.Fatalf("plain Replay: %v", err)
	}
	if strings.Contains(plain.String(), ansiDim) {
		t.Fatalf("ANSI-off replay dimmed status lines:\n%q", plain.String())
	}
}

func TestReplayRendersTurnAttemptStartMarkers(t *testing.T) {
	dir := t.TempDir()
	events := []Event{
		{Type: EventUser, Prompt: 1, Text: "hello"},
		{Type: EventTurnAttemptStart, Prompt: 1, Turn: 1, Attempt: 1},
		{Type: EventTurnAttemptStart, Prompt: 1, Turn: 1, Attempt: 2},
		{Type: EventAssistantDelta, Prompt: 1, Turn: 1, Text: "world"},
	}
	for _, ev := range events {
		if err := AppendEvent(dir, ev); err != nil {
			t.Fatalf("AppendEvent: %v", err)
		}
	}

	var out strings.Builder
	if err := Replay(dir, &out, ReplayOptions{}); err != nil {
		t.Fatalf("Replay: %v", err)
	}
	got := out.String()
	for _, want := range []string{"[turn: 1 waiting]", "[turn: 1 attempt 2 waiting]"} {
		if !strings.Contains(got, "\n"+want+"\n") {
			t.Fatalf("replay missing %q:\n%s", want, got)
		}
	}

	var dim strings.Builder
	if err := Replay(dir, &dim, ReplayOptions{ANSI: true}); err != nil {
		t.Fatalf("ANSI Replay: %v", err)
	}
	if !strings.Contains(dim.String(), ansiDim+"[turn: 1 waiting]"+ansiReset) {
		t.Fatalf("turn waiting marker not dimmed:\n%q", dim.String())
	}

	var quiet strings.Builder
	if err := Replay(dir, &quiet, ReplayOptions{Quiet: true}); err != nil {
		t.Fatalf("quiet Replay: %v", err)
	}
	if strings.Contains(quiet.String(), "waiting") {
		t.Fatalf("quiet replay printed turn waiting markers:\n%s", quiet.String())
	}
}

func TestReplayColorizesToolDiffWithPath(t *testing.T) {
	dir := t.TempDir()
	diff := "--- a/main.go\n+++ b/main.go\n@@ -1,1 +1,1 @@\n-old\n+new"
	events := []Event{
		{Type: EventUser, Prompt: 1, Text: "change it"},
		{Type: EventToolDiff, Prompt: 1, Turn: 1, Tool: "edit", Path: "main.go", Display: diff},
	}
	for _, ev := range events {
		if err := AppendEvent(dir, ev); err != nil {
			t.Fatalf("AppendEvent: %v", err)
		}
	}

	var colored strings.Builder
	if err := Replay(dir, &colored, ReplayOptions{ANSI: true}); err != nil {
		t.Fatalf("ANSI Replay: %v", err)
	}
	got := colored.String()
	if !strings.Contains(got, "\x1b[") {
		t.Fatalf("tool diff not colorized:\n%q", got)
	}
	if strings.Contains(got, ansiDim+"--- a/main.go") {
		t.Fatalf("tool diff dimmed like a status line:\n%q", got)
	}
	if stripped := stripSessionTestANSI(got); !strings.Contains(stripped, "-old\n+new") {
		t.Fatalf("stripped diff lost content:\n%q", stripped)
	}

	var plain strings.Builder
	if err := Replay(dir, &plain, ReplayOptions{}); err != nil {
		t.Fatalf("plain Replay: %v", err)
	}
	if strings.Contains(plain.String(), "\x1b[") || !strings.Contains(plain.String(), diff) {
		t.Fatalf("ANSI-off replay mangled diff:\n%q", plain.String())
	}
}

func TestReplayQuietSuppressesStatusLines(t *testing.T) {
	dir := t.TempDir()

	// Write a minimal event log with a user prompt, an assistant delta, a tool
	// result (has a Display line), and a turn-usage line (also has Display).
	events := []Event{
		{Type: EventUser, Prompt: 1, Text: "hello"},
		{Type: EventAssistantDelta, Prompt: 1, Turn: 1, Text: "world"},
		{Type: EventToolResult, Prompt: 1, Turn: 1, Display: "[read_file] path=a.go → 3 lines"},
		{Type: EventPromptUsage, Prompt: 1, Display: "[prompt: 1 turn · 1.0k in / 0.1k out · 0.1s]"},
	}
	for _, ev := range events {
		if err := AppendEvent(dir, ev); err != nil {
			t.Fatalf("AppendEvent: %v", err)
		}
	}

	// Without quiet: all display lines appear.
	var loud bytes.Buffer
	if err := Replay(dir, &loud, ReplayOptions{}); err != nil {
		t.Fatalf("Replay: %v", err)
	}
	if !strings.Contains(loud.String(), "[read_file]") {
		t.Errorf("non-quiet replay should include status lines, got %q", loud.String())
	}

	// With quiet: status lines suppressed; prompt and assistant text preserved.
	var quiet bytes.Buffer
	if err := Replay(dir, &quiet, ReplayOptions{Quiet: true}); err != nil {
		t.Fatalf("Replay quiet: %v", err)
	}
	got := quiet.String()
	if strings.Contains(got, "[read_file]") || strings.Contains(got, "[turn:") {
		t.Errorf("quiet replay should suppress status lines, got %q", got)
	}
	if !strings.Contains(got, "> hello") {
		t.Errorf("quiet replay should still show user prompt, got %q", got)
	}
	if !strings.Contains(got, "world") {
		t.Errorf("quiet replay should still show assistant text, got %q", got)
	}
}

func TestSaveCompactionPersistsFocusAndFileMetadata(t *testing.T) {
	dir := t.TempDir()
	ref, err := SaveCompaction(dir, Compaction{
		Time:          time.Date(2026, 7, 19, 12, 0, 0, 0, time.UTC),
		Summary:       "summary",
		Messages:      []llm.Message{{Role: llm.RoleUser, Content: []llm.ContentBlock{{Kind: llm.BlockText, Text: "raw"}}}},
		Focus:         "API compatibility",
		ReadFiles:     []string{"a.go"},
		ModifiedFiles: []string{"b.go"},
	})
	if err != nil {
		t.Fatalf("SaveCompaction: %v", err)
	}
	metaPath := strings.TrimSuffix(filepath.Join(dir, ref), ".input.json") + ".meta.json"
	raw, err := os.ReadFile(metaPath)
	if err != nil {
		t.Fatalf("read metadata: %v", err)
	}
	var meta struct {
		Focus         string   `json:"focus"`
		ReadFiles     []string `json:"read_files"`
		ModifiedFiles []string `json:"modified_files"`
	}
	if err := json.Unmarshal(raw, &meta); err != nil {
		t.Fatalf("decode metadata: %v", err)
	}
	if meta.Focus != "API compatibility" || !reflect.DeepEqual(meta.ReadFiles, []string{"a.go"}) || !reflect.DeepEqual(meta.ModifiedFiles, []string{"b.go"}) {
		t.Fatalf("saved compaction metadata = %+v", meta)
	}
}

// sampleSession builds a valid session whose transcript contains a complete
// tool_use/tool_result pair, so ValidateTranscript passes before any mutation.
func sampleSession() Session {
	created := time.Date(2026, 6, 9, 10, 0, 0, 0, time.UTC)
	msgTime := created.Add(time.Minute)
	return Session{
		Version:         Version,
		Provider:        "anthropic",
		Model:           "claude-opus-4-8",
		Created:         created,
		Updated:         created.Add(2 * time.Minute),
		Build:           BuildMetadata{Version: "v1.2.3", Commit: "abc123", Modified: true},
		Runtime:         RuntimeProfile{RetentionPolicy: "auto", ContextWindow: 200_000, SearchBackend: "rg"},
		System:          "be terse",
		ProxySessionID:  "harness-session-test",
		CacheAffinityID: "harness-cache-test",
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
			Usage:       llm.Usage{InputTokens: 1200, OutputTokens: 340, CacheReadTokens: 800, CacheWriteTokens: 0},
			CostUSD:     0.0123,
			Compactions: 2,
		},
	}
}

func TestLoadAndReplayRejectPreV5SessionSchema(t *testing.T) {
	dir := t.TempDir()
	data, err := json.Marshal(Session{Version: 2, Messages: []llm.Message{}})
	if err != nil {
		t.Fatalf("marshal v2 state: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, stateFile), data, 0o644); err != nil {
		t.Fatalf("write v2 state: %v", err)
	}
	if err := AppendEvent(dir, Event{Type: EventUser, Prompt: 1, Text: "old schema"}); err != nil {
		t.Fatalf("AppendEvent: %v", err)
	}

	if _, err := Load(dir); err == nil || !strings.Contains(err.Error(), "unsupported schema version 2 (want 5)") {
		t.Fatalf("Load error = %v, want clear v2 rejection", err)
	}
	var replay strings.Builder
	if err := Replay(dir, &replay, ReplayOptions{}); err == nil || !strings.Contains(err.Error(), "unsupported schema version 2 (want 5)") {
		t.Fatalf("Replay error = %v, want clear v2 rejection", err)
	}
}

func TestSessionRoundTripsPlansAndUsageByModel(t *testing.T) {
	s := sampleSession()
	s.Plans = []plan.Plan{
		{Title: "First plan", Body: "do the thing", Path: "/sess/plans/0001-first-plan.plan.md"},
	}
	s.UsageByModel = map[string]UsageTotals{
		"anthropic/claude-opus-4-8": {Usage: llm.Usage{InputTokens: 1200, OutputTokens: 340}, CostUSD: 0.0123},
		"openai/gpt-5.5":            {Usage: llm.Usage{InputTokens: 30}, CostUSD: 0.0007},
	}
	dir := filepath.Join(t.TempDir(), "session")
	if err := s.Save(dir); err != nil {
		t.Fatalf("Save: %v", err)
	}
	got, err := Load(dir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(got.Plans) != 1 || got.Plans[0].Title != "First plan" || got.Plans[0].Path == "" {
		t.Errorf("plans not round-tripped: %+v", got.Plans)
	}
	if len(got.UsageByModel) != 2 {
		t.Fatalf("usage_by_model not round-tripped: %+v", got.UsageByModel)
	}
	if got.UsageByModel["openai/gpt-5.5"].CostUSD != 0.0007 {
		t.Errorf("per-model cost lost: %+v", got.UsageByModel["openai/gpt-5.5"])
	}
	if got.ProxySessionID != "harness-session-test" {
		t.Errorf("proxy_session_id = %q, want harness-session-test", got.ProxySessionID)
	}
	if got.CacheAffinityID != "harness-cache-test" {
		t.Errorf("cache_affinity_id = %q, want harness-cache-test", got.CacheAffinityID)
	}
	if got.Usage.Compactions != 2 {
		t.Errorf("compactions = %d, want 2", got.Usage.Compactions)
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

func TestAppendEventReportsWriteFailure(t *testing.T) {
	// The event log path occupied by a directory: open fails and AppendEvent
	// must report it (the contract the JSON run stream's fatal-on-raw-error
	// semantics rely on).
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, "raw.ndjson"), 0o755); err != nil {
		t.Fatalf("mkdir blocking event log: %v", err)
	}
	err := AppendEvent(dir, Event{Type: EventUser, Prompt: 1, Text: "hello"})
	if err == nil || !strings.Contains(err.Error(), "session: open event log") {
		t.Fatalf("AppendEvent error = %v, want open event log failure", err)
	}
}

func TestAppendEventStampsMissingTime(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "session")
	if err := AppendEvent(dir, Event{Type: EventUser, Prompt: 1, Text: "hello"}); err != nil {
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

func TestEventAppenderCoalescesAssistantDeltasAndPreservesOrder(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "session")
	appender := NewEventAppender(dir)
	firstTime := time.Date(2026, 7, 27, 1, 2, 3, 0, time.UTC)
	if err := appender.Append(Event{
		Time: firstTime, Type: EventAssistantDelta, Prompt: 2, Turn: 3, Attempt: 1, Text: "hello ",
	}); err != nil {
		t.Fatal(err)
	}
	if err := appender.Append(Event{
		Type: EventAssistantDelta, Prompt: 2, Turn: 3, Attempt: 1, Text: "world",
	}); err != nil {
		t.Fatal(err)
	}
	if err := appender.Append(Event{Type: EventNotice, Prompt: 2, Turn: 3, Display: "[done]"}); err != nil {
		t.Fatal(err)
	}

	events, err := readEvents(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 2 {
		t.Fatalf("events = %+v, want one coalesced delta plus notice", events)
	}
	if events[0].Type != EventAssistantDelta || events[0].Text != "hello world" || !events[0].Time.Equal(firstTime) {
		t.Fatalf("coalesced event = %+v", events[0])
	}
	if events[1].Type != EventNotice {
		t.Fatalf("second event = %+v, want notice", events[1])
	}
}

func TestEventAppenderMirrorReceivesWrittenEvents(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "session")
	appender := NewEventAppender(dir)
	var mirrored []Event
	appender.Mirror = func(ev Event) { mirrored = append(mirrored, ev) }

	user := Event{Type: EventUser, Prompt: 1, Text: "do it"}
	if err := appender.Append(user); err != nil {
		t.Fatal(err)
	}
	if len(mirrored) != 1 || mirrored[0].Type != EventUser {
		t.Fatalf("mirror = %+v, want the user event mirrored immediately", mirrored)
	}
	if mirrored[0].Time.IsZero() {
		t.Fatalf("mirrored event lost its stamped time: %+v", mirrored[0])
	}

	if err := appender.Append(Event{Type: EventAssistantDelta, Prompt: 1, Turn: 1, Attempt: 1, Text: "hello "}); err != nil {
		t.Fatal(err)
	}
	if err := appender.Append(Event{Type: EventAssistantDelta, Prompt: 1, Turn: 1, Attempt: 1, Text: "world"}); err != nil {
		t.Fatal(err)
	}
	if len(mirrored) != 1 {
		t.Fatalf("mirror = %+v, want deltas mirrored only after coalescing", mirrored)
	}
	if err := appender.Flush(); err != nil {
		t.Fatal(err)
	}
	if len(mirrored) != 2 || mirrored[1].Text != "hello world" {
		t.Fatalf("mirror = %+v, want one coalesced delta after flush", mirrored)
	}

	events, err := readEvents(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != len(mirrored) {
		t.Fatalf("raw.ndjson = %+v, mirror = %+v: streams diverged", events, mirrored)
	}
	for i := range events {
		if events[i].Type != mirrored[i].Type || events[i].Text != mirrored[i].Text ||
			!events[i].Time.Equal(mirrored[i].Time) {
			t.Fatalf("event %d: raw = %+v, mirror = %+v", i, events[i], mirrored[i])
		}
	}
}

func TestEventAppenderMirrorSilentWithoutDir(t *testing.T) {
	appender := NewEventAppender("")
	called := false
	appender.Mirror = func(Event) { called = true }
	if err := appender.Append(Event{Type: EventUser, Text: "x"}); err != nil {
		t.Fatal(err)
	}
	if err := appender.Flush(); err != nil {
		t.Fatal(err)
	}
	if called {
		t.Fatal("mirror fired with no session dir; nothing was durably written")
	}
}

func TestEventAppenderBoundsPendingAssistantText(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "session")
	appender := NewEventAppender(dir)
	if err := appender.Append(Event{
		Type: EventAssistantDelta, Prompt: 1, Turn: 1, Attempt: 1,
		Text: strings.Repeat("x", assistantDeltaChunkBytes),
	}); err != nil {
		t.Fatal(err)
	}
	events, err := readEvents(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 || len(events[0].Text) != assistantDeltaChunkBytes {
		t.Fatalf("events = %+v, want one threshold-flushed chunk", events)
	}
}

func TestEventAppenderFlushesAssistantTextForLiveFollowers(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "session")
	appender := NewEventAppender(dir)
	now := time.Date(2026, 7, 27, 1, 2, 3, 0, time.UTC)
	appender.now = func() time.Time { return now }
	if err := appender.Append(Event{
		Type: EventAssistantDelta, Prompt: 1, Turn: 1, Attempt: 1, Text: "hello ",
	}); err != nil {
		t.Fatal(err)
	}
	now = now.Add(assistantDeltaFlushAfter)
	if err := appender.Append(Event{
		Type: EventAssistantDelta, Prompt: 1, Turn: 1, Attempt: 1, Text: "world",
	}); err != nil {
		t.Fatal(err)
	}
	events, err := readEvents(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 || events[0].Text != "hello world" {
		t.Fatalf("events = %+v, want one interval-flushed chunk", events)
	}
}

func TestSaveLoadRoundTrip(t *testing.T) {
	s := sampleSession()
	digest, err := llm.FingerprintMessages(s.Messages)
	if err != nil {
		t.Fatal(err)
	}
	s.ResponseState = &llm.ResponseState{PreviousResponseID: "resp_1", AnchorMessages: len(s.Messages), AnchorDigest: digest}
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
	s.ID = got.ID
	s.CWD = got.CWD
	s.ParentSession = got.ParentSession
	s.ParentEntryID = got.ParentEntryID
	s.ActiveLeaf = got.ActiveLeaf
	s.Tree = got.Tree
	if !reflect.DeepEqual(s, got) {
		t.Fatalf("round-trip mismatch:\n want %+v\n  got %+v", s, got)
	}
}

func TestSaveLoadPreservesParallelToolBatches(t *testing.T) {
	s := sampleSession()
	s.Messages[1].Content = append(s.Messages[1].Content,
		llm.ContentBlock{Kind: llm.BlockToolUse, ToolUseID: "call_2", ToolName: "read_file", ToolInput: json.RawMessage(`{"path":"README.md"}`)},
	)
	s.Messages[2].Content = append(s.Messages[2].Content,
		llm.ContentBlock{Kind: llm.BlockToolResult, ResultForID: "call_2", ResultText: "readme"},
	)
	s.Messages[2].ParallelToolBatches = []llm.ParallelToolBatch{{ToolUseIDs: []string{"call_1", "call_2"}}}

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
	batches := got.Messages[2].ParallelToolBatches
	if len(batches) != 1 || !reflect.DeepEqual(batches[0].ToolUseIDs, []string{"call_1", "call_2"}) {
		t.Fatalf("parallel tool batches = %+v, want [call_1 call_2]", batches)
	}
	data, err := os.ReadFile(filepath.Join(path, treeFile))
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if !bytes.Contains(data, []byte(`"parallel_tool_batches":[{"tool_use_ids":["call_1","call_2"]}]`)) {
		t.Fatalf("saved session missing parallel batch metadata: %s", data)
	}
}

func TestSaveLoadPreservesImageBlocks(t *testing.T) {
	s := sampleSession()
	s.Messages = []llm.Message{{
		Role: llm.RoleUser,
		Content: []llm.ContentBlock{
			{Kind: llm.BlockImage, ImageMediaType: "image/png", ImageData: sessionOnePixelPNG, ImageDetail: "high", ImageName: "screen.png", ImageWidth: 1, ImageHeight: 1},
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
	if content[0].ImageData != sessionOnePixelPNG || content[0].ImageDetail != "high" || content[0].ImageWidth != 1 {
		t.Fatalf("image block = %+v", content[0])
	}
}

func TestSaveToolResultArtifactIgnoresRichImageContent(t *testing.T) {
	dir := t.TempDir()
	result := llm.ToolResult{
		ForID:        "call_image",
		Text:         "truncated summary",
		Truncated:    true,
		OriginalText: "full textual output",
		Content: []llm.ContentBlock{{
			Kind: llm.BlockImage, ImageMediaType: "image/png", ImageData: "SECRET_BASE64_PAYLOAD", ImageName: "private/path.png",
		}},
	}
	rel, err := SaveToolResultArtifact(dir, 1, 2, result)
	if err != nil {
		t.Fatalf("SaveToolResultArtifact: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(dir, rel))
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if got := string(data); got != result.OriginalText {
		t.Fatalf("artifact = %q, want only original text %q", got, result.OriginalText)
	}
	if bytes.Contains(data, []byte("SECRET_BASE64_PAYLOAD")) || bytes.Contains(data, []byte("private/path.png")) {
		t.Fatalf("artifact leaked rich image content: %q", data)
	}
}

func TestSaveLoadPreservesRichToolResult(t *testing.T) {
	s := sampleSession()
	s.Messages = []llm.Message{
		{Role: llm.RoleAssistant, Content: []llm.ContentBlock{{Kind: llm.BlockToolUse, ToolUseID: "call_image", ToolName: "view_image", ToolInput: json.RawMessage(`{"path":"screen.png"}`)}}},
		{Role: llm.RoleUser, Content: []llm.ContentBlock{{
			Kind: llm.BlockToolResult, ResultForID: "call_image", ResultText: "screen.png attached",
			ResultContent: []llm.ContentBlock{{Kind: llm.BlockImage, ImageMediaType: "image/png", ImageData: sessionOnePixelPNG, ImageDetail: "high", ImageWidth: 1, ImageHeight: 1, ImageBytes: 69, ImageEncodedBytes: len(sessionOnePixelPNG)}},
		}}},
	}
	if err := llm.ValidateTranscript(s.Messages); err != nil {
		t.Fatalf("ValidateTranscript before save: %v", err)
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
		t.Fatalf("ValidateTranscript after load: %v", err)
	}
	child := got.Messages[1].Content[0].ResultContent[0]
	if child.ImageData != sessionOnePixelPNG || child.ImageDetail != "high" || child.ImageBytes != 69 || child.ImageEncodedBytes != len(sessionOnePixelPNG) {
		t.Fatalf("rich child = %+v", child)
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
		{Content: "implement", Status: todo.StatusInProgress},
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
func TestSaveLoadPreservesGoal(t *testing.T) {
	s := sampleSession()
	s.Goal = &goal.State{
		Objective:     "refactor the parser",
		Status:        goal.StatusActive,
		Continuations: 3,
		SetAt:         time.Date(2026, 7, 29, 12, 0, 0, 0, time.UTC),
	}
	path := filepath.Join(t.TempDir(), "session")
	if err := s.Save(path); err != nil {
		t.Fatalf("Save: %v", err)
	}
	got, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !reflect.DeepEqual(got.Goal, s.Goal) {
		t.Errorf("Goal = %+v, want %+v", got.Goal, s.Goal)
	}
}

func TestLoadSessionWithoutGoalKey(t *testing.T) {
	s := sampleSession()
	path := filepath.Join(t.TempDir(), "session")
	if err := s.Save(path); err != nil {
		t.Fatalf("Save: %v", err)
	}
	got, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got.Goal != nil {
		t.Fatalf("Goal = %+v, want nil for state without goal", got.Goal)
	}
}

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
	if len(stateEntries) != 2 || stateEntries[0].Name() != stateFile || stateEntries[1].Name() != treeFile {
		t.Fatalf("expected %s and %s after save, got %v", stateFile, treeFile, stateEntries)
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

// A save requested mid-turn may end with an assistant tool_use that has no
// matching result. Save normalizes it before writing immutable tree data.
func TestSaveRepairsDanglingToolUseBeforeTreeStorage(t *testing.T) {
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
	// Validate the pre-save transcript is dangling.
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
	for i, want := range []struct {
		id   string
		name string
	}{
		{id: "call_x", name: "edit"},
		{id: "call_y", name: "edit"},
	} {
		b := last.Content[i]
		if b.Kind != llm.BlockToolResult {
			t.Fatalf("block %d kind %q, want tool_result", i, b.Kind)
		}
		if b.ResultForID != want.id {
			t.Fatalf("block %d result_for_id %q, want %q", i, b.ResultForID, want.id)
		}
		if b.ToolName != want.name {
			t.Fatalf("block %d tool_name %q, want %q", i, b.ToolName, want.name)
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
	data, err := os.ReadFile(filepath.Join(path, treeFile))
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
		{Type: EventUser, Prompt: 1, Text: "fix it"},
		{Type: EventAssistantDelta, Prompt: 1, Turn: 1, Text: "I'll check **now** and use [docs](https://docs.example.com).\n"},
		{Type: EventReasoningSummary, Prompt: 1, Turn: 1, Text: "Checked **the repo**.\nNext [step](https://example.com)."},
		{Type: EventTurnComplete, Prompt: 1, Turn: 1, Display: "[turn: 1 · 1.0s]"},
		{Type: EventToolResult, Prompt: 1, Turn: 1, Display: `[rg pattern="panic" .] → 2 lines, 80B`},
		{Type: EventToolDiff, Prompt: 1, Turn: 1, Display: "--- a/f.txt\n+++ b/f.txt\n@@ -1,1 +1,1 @@\n-old\n+new"},
		{Type: EventNotice, Prompt: 1, Display: "[compacted: 6 messages → summary]"},
		{Type: EventBranch, Prompt: 1, Display: "[tree: old → new; working directory unchanged]"},
		{Type: EventPromptUsage, Prompt: 1, Display: "[prompt: 2 turns · 1.0k in / 100 out]"},
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
	for _, want := range []string{"> fix it", "I'll check now and use docs <https://docs.example.com>.", "[reasoning]\n", "  Checked the repo.", "  Next step <https://example.com>.", "[turn: 1", `[rg pattern="panic" .]`, "--- a/f.txt", "-old\n+new", "[compacted:", "[tree: old", "[prompt:"} {
		if !strings.Contains(got, want) {
			t.Fatalf("replay missing %q:\n%s", want, got)
		}
	}
}

func TestReplayHighlightsTaggedFenceOnlyWithANSI(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "session")
	for _, ev := range []Event{
		{Type: EventUser, Prompt: 1, Text: "show code"},
		{Type: EventAssistantDelta, Prompt: 1, Turn: 1, Text: "```go\nfu"},
		{Type: EventAssistantDelta, Prompt: 1, Turn: 1, Text: "nc main() {\n```\n"},
		{Type: EventTurnComplete, Prompt: 1, Turn: 1},
	} {
		if err := AppendEvent(dir, ev); err != nil {
			t.Fatalf("AppendEvent: %v", err)
		}
	}

	var plain strings.Builder
	if err := Replay(dir, &plain, ReplayOptions{Markdown: true}); err != nil {
		t.Fatalf("plain Replay: %v", err)
	}
	wantPlain := "> show code\n" + markdown.HorizontalRule + "\n  ```go\n  func main() {\n  ```\n"
	if got := plain.String(); got != wantPlain {
		t.Fatalf("plain replay = %q, want %q", got, wantPlain)
	}
	if strings.Contains(plain.String(), "\x1b[") {
		t.Fatalf("ANSI-off replay contains escapes: %q", plain.String())
	}

	var colored strings.Builder
	if err := Replay(dir, &colored, ReplayOptions{Markdown: true, ANSI: true}); err != nil {
		t.Fatalf("colored Replay: %v", err)
	}
	if colored.String() == plain.String() || !strings.Contains(strings.Split(colored.String(), "\n")[3], "\x1b[") {
		t.Fatalf("ANSI replay did not highlight fence body: %q", colored.String())
	}
	if got := stripSessionTestANSI(colored.String()); got != plain.String() {
		t.Fatalf("stripped colored replay = %q, want plain %q", got, plain.String())
	}

	stored, err := readEvents(dir)
	if err != nil {
		t.Fatalf("readEvents: %v", err)
	}
	var storedAssistant strings.Builder
	for _, ev := range stored {
		if ev.Type == EventAssistantDelta {
			storedAssistant.WriteString(ev.Text)
		}
	}
	if got, want := storedAssistant.String(), "```go\nfunc main() {\n```\n"; got != want || strings.Contains(got, "\x1b[") {
		t.Fatalf("stored assistant deltas = %q, want raw %q", got, want)
	}

	latest, err := LatestTurnOutput(dir)
	if err != nil {
		t.Fatalf("LatestTurnOutput: %v", err)
	}
	wantLatest := "  ```go\n  func main() {\n  ```"
	if latest != wantLatest {
		t.Fatalf("latest turn output = %q, want %q", latest, wantLatest)
	}
	if strings.Contains(latest, "\x1b[") {
		t.Fatalf("LatestTurnOutput contains ANSI: %q", latest)
	}
}

func TestReplayPropagatesLightThemeToEveryCodePath(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "session")
	for _, ev := range []Event{
		{Type: EventAssistantDelta, Prompt: 1, Turn: 1, Text: "```go\nfunc assistant() {}\n```\n"},
		{Type: EventReasoningSummary, Prompt: 1, Turn: 1, Text: "```go\nfunc reason() {}\n```"},
		{Type: EventToolDiff, Prompt: 1, Turn: 1, Path: "main.go", Display: "@@ -1 +1 @@\n-func old() {}\n+func new() {}"},
	} {
		if err := AppendEvent(dir, ev); err != nil {
			t.Fatalf("AppendEvent: %v", err)
		}
	}

	var out strings.Builder
	opts := ReplayOptions{Markdown: true, ANSI: true, ColorTheme: highlight.ThemeLight}
	if err := Replay(dir, &out, opts); err != nil {
		t.Fatalf("Replay: %v", err)
	}
	got := out.String()
	if count := strings.Count(got, "\x1b[38;2;0;0;255mfunc"); count != 4 {
		t.Errorf("light keyword count = %d, want assistant/reasoning/old/new (4): %q", count, got)
	}
	for _, want := range []string{"\x1b[48;2;218;251;225m", "\x1b[48;2;255;235;233m"} {
		if !strings.Contains(got, want) {
			t.Errorf("replayed diff missing light background %q: %q", want, got)
		}
	}
	for _, dark := range []string{"\x1b[38;2;101;169;224mfunc", "\x1b[48;2;33;58;43m", "\x1b[48;2;74;34;29m"} {
		if strings.Contains(got, dark) {
			t.Errorf("light replay contains dark palette %q: %q", dark, got)
		}
	}

	var plain strings.Builder
	if err := Replay(dir, &plain, ReplayOptions{Markdown: true, ColorTheme: highlight.ThemeLight}); err != nil {
		t.Fatalf("plain Replay: %v", err)
	}
	if strings.Contains(plain.String(), "\x1b[") {
		t.Errorf("theme enabled ANSI in replay: %q", plain.String())
	}
}

func TestReplayWrapsReasoningSummaryWithWidth(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "session")
	if err := AppendEvent(dir, Event{
		Type: EventReasoningSummary,
		Turn: 1,
		Text: "alpha beta gamma delta epsilon zeta eta theta",
	}); err != nil {
		t.Fatalf("AppendEvent: %v", err)
	}

	var out strings.Builder
	if err := Replay(dir, &out, ReplayOptions{Markdown: true, Width: 24}); err != nil {
		t.Fatalf("Replay: %v", err)
	}
	want := "[reasoning]\n" +
		"  alpha beta gamma delta\n" +
		"  epsilon zeta eta theta\n" +
		"[end reasoning]\n"
	if got := out.String(); got != want {
		t.Fatalf("replay output mismatch:\nwant %q\n got %q", want, got)
	}
}

func TestReplaySeparatesCommentaryAndFinalAnswer(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "session")
	events := []Event{
		{Type: EventUser, Prompt: 1, Text: "answer this"},
		{Type: EventAssistantPhase, Prompt: 1, Turn: 1, Phase: llm.AssistantPhaseCommentary},
		{Type: EventAssistantDelta, Prompt: 1, Turn: 1, Text: "I have enough to answer."},
		{Type: EventAssistantPhase, Prompt: 1, Turn: 1, Phase: llm.AssistantPhaseFinal},
		{Type: EventAssistantDelta, Prompt: 1, Turn: 1, Text: "Yes, with limits."},
		{Type: EventTurnComplete, Prompt: 1, Turn: 1},
	}
	for _, ev := range events {
		if err := AppendEvent(dir, ev); err != nil {
			t.Fatalf("AppendEvent: %v", err)
		}
	}

	var replay strings.Builder
	if err := Replay(dir, &replay, ReplayOptions{Markdown: true, ANSI: true}); err != nil {
		t.Fatalf("Replay: %v", err)
	}
	wantReplay := "> answer this\n" +
		ansiDim + markdown.HorizontalRule + ansiReset +
		"\nI have enough to answer.\n" +
		ansiDim + markdown.HorizontalRule + ansiReset +
		"\nYes, with limits.\n"
	if replay.String() != wantReplay {
		t.Fatalf("replay mismatch:\nwant %q\n got %q", wantReplay, replay.String())
	}

	latest, err := LatestTurnOutput(dir)
	if err != nil {
		t.Fatalf("LatestTurnOutput: %v", err)
	}
	wantLatest := "I have enough to answer.\n" + markdown.HorizontalRule + "\nYes, with limits."
	if latest != wantLatest {
		t.Fatalf("latest output mismatch:\nwant %q\n got %q", wantLatest, latest)
	}
}

func TestReplayResetsPhaseStateBetweenTurns(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "session")
	events := []Event{
		{Type: EventUser, Prompt: 1, Text: "first"},
		{Type: EventAssistantPhase, Prompt: 1, Turn: 1, Phase: llm.AssistantPhaseCommentary},
		{Type: EventAssistantDelta, Prompt: 1, Turn: 1, Text: "Working."},
		{Type: EventUser, Prompt: 2, Text: "second"},
		{Type: EventAssistantPhase, Prompt: 2, Turn: 1, Phase: llm.AssistantPhaseFinal},
		{Type: EventAssistantDelta, Prompt: 2, Turn: 1, Text: "Done."},
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
	got := replay.String()
	// The separator after each user prompt is structural; a separator between
	// the commentary text and the next prompt would mean phase state carried
	// across turns.
	if strings.Contains(got, "Working.\n"+markdown.HorizontalRule) {
		t.Fatalf("replay carried phase state between turns:\n%s", got)
	}
	if !strings.Contains(got, "> second\n"+markdown.HorizontalRule+"\nDone.\n") {
		t.Fatalf("second turn final answer missing:\n%s", got)
	}
}

func TestReplayFiltersAbandonedAttemptOutput(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "session")
	events := []Event{
		{Type: EventUser, Prompt: 1, Text: "fix it"},
		{Type: EventTurnAttemptStart, Prompt: 1, Turn: 1, Attempt: 1},
		{Type: EventAssistantDelta, Prompt: 1, Turn: 1, Attempt: 1, Text: "discarded partial"},
		{Type: EventReasoningSummary, Prompt: 1, Turn: 1, Attempt: 1, Text: "discarded reasoning"},
		{Type: EventTurnAttemptAbandoned, Prompt: 1, Turn: 1, Attempt: 1, Display: "[turn: 1 attempt 1 discarded; retrying]"},
		{Type: EventNotice, Prompt: 1, Turn: 1, Display: "[stream interrupted: retrying]"},
		{Type: EventTurnAttemptStart, Prompt: 1, Turn: 1, Attempt: 2},
		{Type: EventAssistantDelta, Prompt: 1, Turn: 1, Attempt: 2, Text: "final answer"},
		{Type: EventTurnComplete, Prompt: 1, Turn: 1},
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
	if !strings.Contains(gotReplay, "[turn: 1 attempt 1 discarded; retrying]") || !strings.Contains(gotReplay, "final answer") {
		t.Fatalf("replay missing abandoned marker or final answer:\n%s", gotReplay)
	}

	latest, err := LatestTurnOutput(dir)
	if err != nil {
		t.Fatalf("LatestTurnOutput: %v", err)
	}
	if strings.Contains(latest, "discarded partial") || strings.Contains(latest, "discarded reasoning") ||
		latest != "[stream interrupted: retrying]\nfinal answer" {
		t.Fatalf("latest output did not filter abandoned attempt correctly: %q", latest)
	}
}

func writeFollowMeta(t *testing.T, dir, status string) {
	t.Helper()
	data, err := json.Marshal(ChildMeta{ID: "child-1", Kind: "delegate", Status: status})
	if err != nil {
		t.Fatalf("marshal child metadata: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "meta.json"), data, 0o644); err != nil {
		t.Fatalf("write child metadata: %v", err)
	}
}

func appendFollowBytes(t *testing.T, dir string, data []byte) {
	t.Helper()
	f, err := os.OpenFile(filepath.Join(dir, eventLog), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		t.Fatalf("open raw event log: %v", err)
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		t.Fatalf("append raw event log: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("close raw event log: %v", err)
	}
}

func TestFollowInitialAndAppendedEventsMatchReplayExactlyOnce(t *testing.T) {
	dir := t.TempDir()
	writeFollowMeta(t, dir, ChildStatusRunning)
	initial := []Event{
		{Type: EventUser, Prompt: 1, Text: "answer"},
		{Type: EventAssistantPhase, Prompt: 1, Turn: 1, Phase: llm.AssistantPhaseCommentary},
		{Type: EventAssistantDelta, Prompt: 1, Turn: 1, Text: "Working **now**."},
	}
	for _, ev := range initial {
		if err := AppendEvent(dir, ev); err != nil {
			t.Fatalf("AppendEvent initial: %v", err)
		}
	}

	waitCalls := 0
	wait := func(context.Context) error {
		waitCalls++
		if waitCalls > 1 {
			return errors.New("unexpected extra wait")
		}
		for _, ev := range []Event{
			{Type: EventAssistantPhase, Prompt: 1, Turn: 1, Phase: llm.AssistantPhaseFinal},
			{Type: EventAssistantDelta, Prompt: 1, Turn: 1, Text: "Done **once**."},
		} {
			if err := AppendEvent(dir, ev); err != nil {
				t.Fatalf("AppendEvent live: %v", err)
			}
		}
		writeFollowMeta(t, dir, ChildStatusCompleted)
		return nil
	}

	var followed strings.Builder
	if err := followWithWaiter(context.Background(), dir, &followed, ReplayOptions{Markdown: true}, wait); err != nil {
		t.Fatalf("Follow: %v", err)
	}
	var replayed strings.Builder
	if err := Replay(dir, &replayed, ReplayOptions{Markdown: true}); err != nil {
		t.Fatalf("Replay: %v", err)
	}
	if got, want := followed.String(), replayed.String(); got != want {
		t.Fatalf("follow output differs from replay:\nwant %q\n got %q", want, got)
	}
	got := followed.String()
	if strings.Count(got, "Working now.") != 1 || strings.Count(got, "Done once.") != 1 ||
		strings.Index(got, "Working now.") > strings.Index(got, "Done once.") {
		t.Fatalf("follow did not render initial and live records once in order: %q", got)
	}
}

func TestFollowRetainsLightThemeAndMarkdownStateAcrossRecords(t *testing.T) {
	dir := t.TempDir()
	writeFollowMeta(t, dir, ChildStatusRunning)
	if err := AppendEvent(dir, Event{Type: EventAssistantDelta, Prompt: 1, Turn: 1, Text: "```go\nfu"}); err != nil {
		t.Fatalf("AppendEvent initial: %v", err)
	}

	waitCalls := 0
	wait := func(context.Context) error {
		waitCalls++
		if waitCalls > 1 {
			return errors.New("unexpected extra wait")
		}
		if err := AppendEvent(dir, Event{Type: EventAssistantDelta, Prompt: 1, Turn: 1, Text: "nc main() {}\n```\n"}); err != nil {
			t.Fatalf("AppendEvent live: %v", err)
		}
		writeFollowMeta(t, dir, ChildStatusCompleted)
		return nil
	}

	var out strings.Builder
	opts := ReplayOptions{Markdown: true, ANSI: true, ColorTheme: highlight.ThemeLight}
	if err := followWithWaiter(context.Background(), dir, &out, opts, wait); err != nil {
		t.Fatalf("Follow: %v", err)
	}
	got := out.String()
	if !strings.Contains(got, "\x1b[38;2;0;0;255mfunc") || strings.Contains(got, "\x1b[38;2;101;169;224mfunc") {
		t.Fatalf("follow lost light theme or streaming Markdown state: %q", got)
	}
	if stripped, want := stripSessionTestANSI(got), "  ```go\n  func main() {}\n  ```\n"; stripped != want {
		t.Fatalf("follow source = %q, want %q", stripped, want)
	}
}

func TestFollowRetainsSplitRecordUntilNewline(t *testing.T) {
	dir := t.TempDir()
	writeFollowMeta(t, dir, ChildStatusRunning)
	record, err := json.Marshal(Event{Type: EventAssistantDelta, Prompt: 1, Turn: 1, Text: "split output"})
	if err != nil {
		t.Fatalf("marshal event: %v", err)
	}
	cut := len(record) / 2
	var out strings.Builder
	waitCalls := 0
	wait := func(context.Context) error {
		switch waitCalls {
		case 0:
			appendFollowBytes(t, dir, record[:cut])
		case 1:
			if strings.Contains(out.String(), "split output") {
				t.Fatalf("incomplete record rendered early: %q", out.String())
			}
			appendFollowBytes(t, dir, append(record[cut:], '\n'))
			writeFollowMeta(t, dir, ChildStatusCompleted)
		default:
			return errors.New("unexpected extra wait")
		}
		waitCalls++
		return nil
	}
	if err := followWithWaiter(context.Background(), dir, &out, ReplayOptions{}, wait); err != nil {
		t.Fatalf("Follow: %v", err)
	}
	if got, want := out.String(), "split output\n"; got != want {
		t.Fatalf("split record output = %q, want %q", got, want)
	}
}

func TestFollowRejectsOversizedAndCorruptRecords(t *testing.T) {
	tests := []struct {
		name string
		data []byte
		want string
	}{
		{name: "corrupt", data: []byte("{not json}\n"), want: "replay decode"},
		{name: "oversized", data: append(bytes.Repeat([]byte("x"), maxReplayRecordSize+1), '\n'), want: "replay record exceeds 16777216 bytes"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			writeFollowMeta(t, dir, ChildStatusCompleted)
			appendFollowBytes(t, dir, tt.data)
			var out strings.Builder
			err := Follow(context.Background(), dir, &out, ReplayOptions{})
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("Follow error = %v, want containing %q", err, tt.want)
			}
		})
	}
}

func TestFollowAllowsMissingRawLogForTerminalChildren(t *testing.T) {
	for _, status := range []string{ChildStatusCompleted, ChildStatusFailed, ChildStatusCanceled} {
		t.Run(status, func(t *testing.T) {
			dir := t.TempDir()
			writeFollowMeta(t, dir, status)
			var out strings.Builder
			if err := Follow(context.Background(), dir, &out, ReplayOptions{}); err != nil {
				t.Fatalf("Follow terminal child: %v", err)
			}
			if out.Len() != 0 {
				t.Fatalf("output with missing raw log = %q", out.String())
			}
		})
	}
}

func TestFollowFiltersInitialAbandonmentButKeepsLiveOutputAndMarker(t *testing.T) {
	dir := t.TempDir()
	writeFollowMeta(t, dir, ChildStatusRunning)
	initial := []Event{
		{Type: EventAssistantDelta, Prompt: 1, Turn: 1, Attempt: 1, Text: "initial discarded"},
		{Type: EventTurnAttemptAbandoned, Prompt: 1, Turn: 1, Attempt: 1, Display: "[initial discarded marker]"},
		{Type: EventAssistantDelta, Prompt: 1, Turn: 1, Attempt: 2, Text: "initial kept"},
	}
	for _, ev := range initial {
		if err := AppendEvent(dir, ev); err != nil {
			t.Fatalf("AppendEvent initial: %v", err)
		}
	}
	wait := func(context.Context) error {
		for _, ev := range []Event{
			{Type: EventAssistantDelta, Prompt: 1, Turn: 1, Attempt: 3, Text: "live remains"},
			{Type: EventTurnAttemptAbandoned, Prompt: 1, Turn: 1, Attempt: 3, Display: "[live discarded marker]"},
		} {
			if err := AppendEvent(dir, ev); err != nil {
				t.Fatalf("AppendEvent live: %v", err)
			}
		}
		writeFollowMeta(t, dir, ChildStatusFailed)
		return nil
	}
	var out strings.Builder
	if err := followWithWaiter(context.Background(), dir, &out, ReplayOptions{}, wait); err != nil {
		t.Fatalf("Follow: %v", err)
	}
	got := out.String()
	if strings.Contains(got, "initial discarded\n") || !strings.Contains(got, "[initial discarded marker]") ||
		!strings.Contains(got, "initial kept") || !strings.Contains(got, "live remains") ||
		!strings.Contains(got, "[live discarded marker]") {
		t.Fatalf("unexpected abandonment follow output: %q", got)
	}
}

func TestFollowPromptUsageCompletesChildButNotRoot(t *testing.T) {
	t.Run("child fallback", func(t *testing.T) {
		dir := t.TempDir()
		writeFollowMeta(t, dir, ChildStatusRunning)
		if err := AppendEvent(dir, Event{Type: EventPromptUsage, Prompt: 1, Display: "[prompt done]"}); err != nil {
			t.Fatalf("AppendEvent: %v", err)
		}
		wait := func(context.Context) error {
			t.Fatal("child prompt_usage fallback unexpectedly waited")
			return nil
		}
		var out strings.Builder
		if err := followWithWaiter(context.Background(), dir, &out, ReplayOptions{}, wait); err != nil {
			t.Fatalf("Follow: %v", err)
		}
	})

	t.Run("child identity appears after usage", func(t *testing.T) {
		dir := t.TempDir()
		if err := AppendEvent(dir, Event{Type: EventPromptUsage, Prompt: 1, Display: "[prompt done]"}); err != nil {
			t.Fatalf("AppendEvent: %v", err)
		}
		waitCalls := 0
		wait := func(context.Context) error {
			waitCalls++
			if waitCalls > 1 {
				return errors.New("unexpected extra wait")
			}
			writeFollowMeta(t, dir, ChildStatusRunning)
			return nil
		}
		var out strings.Builder
		if err := followWithWaiter(context.Background(), dir, &out, ReplayOptions{}, wait); err != nil {
			t.Fatalf("Follow: %v", err)
		}
		if waitCalls != 1 {
			t.Fatalf("wait calls = %d, want 1", waitCalls)
		}
	})

	t.Run("root cancellation", func(t *testing.T) {
		dir := t.TempDir()
		if err := AppendEvent(dir, Event{Type: EventPromptUsage, Prompt: 1, Display: "[root prompt done]"}); err != nil {
			t.Fatalf("AppendEvent: %v", err)
		}
		waitCalls := 0
		wait := func(context.Context) error {
			waitCalls++
			return context.Canceled
		}
		var out strings.Builder
		err := followWithWaiter(context.Background(), dir, &out, ReplayOptions{}, wait)
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Follow root error = %v, want context cancellation", err)
		}
		if waitCalls != 1 || !strings.Contains(out.String(), "[root prompt done]") {
			t.Fatalf("root prompt_usage completed follow: waits=%d output=%q", waitCalls, out.String())
		}
	})
}

type followCallbackWriter struct {
	bytes.Buffer
	match   string
	onMatch func()
}

func (w *followCallbackWriter) Write(p []byte) (int, error) {
	n, err := w.Buffer.Write(p)
	if w.onMatch != nil && strings.Contains(string(p), w.match) {
		fn := w.onMatch
		w.onMatch = nil
		fn()
	}
	return n, err
}

func TestFollowCompletionPerformsFinalDrain(t *testing.T) {
	dir := t.TempDir()
	writeFollowMeta(t, dir, ChildStatusRunning)
	if err := AppendEvent(dir, Event{Type: EventPromptUsage, Prompt: 1, Display: "[prompt done]"}); err != nil {
		t.Fatalf("AppendEvent: %v", err)
	}
	out := &followCallbackWriter{match: "[prompt done]"}
	out.onMatch = func() {
		if err := AppendEvent(dir, Event{Type: EventNotice, Prompt: 1, Display: "[late final event]"}); err != nil {
			t.Fatalf("AppendEvent from writer: %v", err)
		}
	}
	wait := func(context.Context) error {
		t.Fatal("completion fallback unexpectedly waited")
		return nil
	}
	if err := followWithWaiter(context.Background(), dir, out, ReplayOptions{}, wait); err != nil {
		t.Fatalf("Follow: %v", err)
	}
	got := out.String()
	if strings.Index(got, "[prompt done]") < 0 || strings.Index(got, "[late final event]") < strings.Index(got, "[prompt done]") {
		t.Fatalf("final drain output missing or out of order: %q", got)
	}
}

func TestFollowErrorsOnPartialRecordAtChildCompletion(t *testing.T) {
	dir := t.TempDir()
	writeFollowMeta(t, dir, ChildStatusCompleted)
	appendFollowBytes(t, dir, []byte(`{"type":"notice"}`))
	var out strings.Builder
	err := Follow(context.Background(), dir, &out, ReplayOptions{})
	if err == nil || !strings.Contains(err.Error(), "replay ended with incomplete record") {
		t.Fatalf("Follow error = %v, want incomplete record error", err)
	}
}

func TestFollowValidatesMetadataAndAppearingSchema(t *testing.T) {
	t.Run("metadata", func(t *testing.T) {
		tests := []struct {
			name string
			data string
			want string
		}{
			{name: "malformed", data: "{", want: "decode child metadata"},
			{name: "missing identity", data: `{"status":"running"}`, want: "id and kind are required"},
			{name: "unknown status", data: `{"id":"c","kind":"delegate","status":"paused"}`, want: `unknown status "paused"`},
		}
		for _, tt := range tests {
			t.Run(tt.name, func(t *testing.T) {
				dir := t.TempDir()
				if err := os.WriteFile(filepath.Join(dir, "meta.json"), []byte(tt.data), 0o644); err != nil {
					t.Fatalf("write metadata: %v", err)
				}
				var out strings.Builder
				err := followWithWaiter(context.Background(), dir, &out, ReplayOptions{}, func(context.Context) error {
					return errors.New("unexpected wait")
				})
				if err == nil || !strings.Contains(err.Error(), tt.want) {
					t.Fatalf("Follow error = %v, want containing %q", err, tt.want)
				}
			})
		}
	})

	t.Run("schema appears", func(t *testing.T) {
		dir := t.TempDir()
		wait := func(context.Context) error {
			if err := os.WriteFile(filepath.Join(dir, stateFile), []byte(`{"version":3}`), 0o644); err != nil {
				t.Fatalf("write state: %v", err)
			}
			return nil
		}
		var out strings.Builder
		err := followWithWaiter(context.Background(), dir, &out, ReplayOptions{}, wait)
		if err == nil || !strings.Contains(err.Error(), "unsupported schema version 3 (want 5)") {
			t.Fatalf("Follow error = %v, want appearing schema rejection", err)
		}
	})
}

func TestFollowRequiresExistingDirectory(t *testing.T) {
	var out strings.Builder
	missing := filepath.Join(t.TempDir(), "missing")
	if err := Follow(context.Background(), missing, &out, ReplayOptions{}); err == nil {
		t.Fatal("Follow accepted nonexistent directory")
	}
	path := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(path, []byte("not a directory"), 0o644); err != nil {
		t.Fatalf("write file: %v", err)
	}
	if err := Follow(context.Background(), path, &out, ReplayOptions{}); err == nil || !strings.Contains(err.Error(), "not a directory") {
		t.Fatalf("Follow file error = %v, want non-directory error", err)
	}
}

func TestTimingsPrintsWallClockReport(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "session")
	base := time.Date(2026, 6, 9, 10, 0, 0, 0, time.UTC)
	events := []Event{
		{Time: base, Type: EventUser, Prompt: 1, Text: "fix it"},
		{Time: base.Add(100 * time.Millisecond), Type: EventTurnAttemptStart, Prompt: 1, Turn: 1, Attempt: 1, Context: &ContextSnapshot{Total: 1000, Window: 4000, PayloadTotal: 400, Tools: 120}},
		{Time: base.Add(200 * time.Millisecond), Type: EventModelRequest, Prompt: 1, Turn: 1, Attempt: 1, ModelRequest: &llm.ModelRequestEvent{State: llm.ModelRequestUpstreamAttemptFailed, StatusCode: 429, AttemptDurationMS: 250}},
		{Time: base.Add(200 * time.Millisecond), Type: EventModelRequest, Prompt: 1, Turn: 1, Attempt: 1, ModelRequest: &llm.ModelRequestEvent{State: llm.ModelRequestRetryScheduled, StatusCode: 429, RetryDelayMS: 500}},
		{Time: base.Add(950 * time.Millisecond), Type: EventModelRequest, Prompt: 1, Turn: 1, Attempt: 1, ModelRequest: &llm.ModelRequestEvent{State: llm.ModelRequestFailed, StatusCode: 529, AttemptDurationMS: 125}},
		{Time: base.Add(1200 * time.Millisecond), Type: EventToolStart, Prompt: 1, Turn: 1, ToolID: "call_1", Tool: "read_file"},
		{Time: base.Add(1500 * time.Millisecond), Type: EventToolResult, Prompt: 1, Turn: 1, ToolID: "call_1", Tool: "read_file"},
		{Time: base.Add(1600 * time.Millisecond), Type: EventTurnAttemptUsage, Prompt: 1, Turn: 1, Attempt: 1},
		{Time: base.Add(2 * time.Second), Type: EventPromptUsage, Prompt: 1},
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
	for _, want := range []string{"prompt 1: total 2s", "first visible 1.2s", "turn 1 attempt 1: 1.5s", "payload 400", "model API issues: 2 failed attempts, 375ms provider time, 500ms scheduled retry wait (429×1, 529×1)", "tool read_file: 300ms", "gap 750ms"} {
		if !strings.Contains(got, want) {
			t.Fatalf("timings missing %q:\n%s", want, got)
		}
	}
}

func TestTimingsTreatsReasoningSummaryAsVisible(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "session")
	base := time.Date(2026, 6, 9, 10, 0, 0, 0, time.UTC)
	events := []Event{
		{Time: base, Type: EventUser, Prompt: 1, Text: "fix it"},
		{Time: base.Add(400 * time.Millisecond), Type: EventReasoningSummary, Prompt: 1, Turn: 1, Text: "Checking."},
		{Time: base.Add(2 * time.Second), Type: EventPromptUsage, Prompt: 1},
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

func TestTimingsLabelsInProgressPromptAndUsesLastEvent(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "session")
	base := time.Date(2026, 7, 26, 10, 0, 0, 0, time.UTC)
	events := []Event{
		{Time: base, Type: EventUser, Prompt: 1, Text: "keep working"},
		{Time: base.Add(400 * time.Millisecond), Type: EventAssistantDelta, Prompt: 1, Turn: 1, Text: "Checking."},
		{Time: base.Add(5 * time.Second), Type: EventTurnComplete, Prompt: 1, Turn: 1},
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
	if !strings.Contains(got, "prompt 1 (in progress): total 5s, first visible 400ms") {
		t.Fatalf("in-progress timings = %q", got)
	}
	if strings.Contains(got, "total 0s") {
		t.Fatalf("in-progress timings reported a zero total: %q", got)
	}
}

func TestLatestTurnOutputReturnsLatestVisibleOutputWithoutUserPrompt(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "session")
	events := []Event{
		{Type: EventUser, Prompt: 1, Text: "one long prompt"},
		{Type: EventAssistantDelta, Prompt: 1, Turn: 1, Text: "old answer\n"},
		{Type: EventTurnComplete, Prompt: 1, Turn: 1, Display: "[turn: 1]"},
		{Type: EventAssistantDelta, Prompt: 1, Turn: 2, Text: "new **answer**"},
		{Type: EventReasoningSummary, Prompt: 1, Turn: 2, Text: "Checked state."},
		{Type: EventTurnAttemptUsage, Prompt: 1, Turn: 2, Attempt: 1, Display: "[attempt usage is not editor output]"},
		{Type: EventToolResult, Prompt: 1, Turn: 2, Display: `[read_file path="x"] → 12B`},
		{Type: EventNotice, Prompt: 1, Turn: 2, Display: "[notice]"},
		{Type: EventTurnComplete, Prompt: 1, Turn: 2, Display: "[turn: 2]"},
		{Type: EventNotice, Prompt: 1, Display: "[compacted maintenance notice]"},
		{Type: EventMaintenanceUsage, Prompt: 1, Purpose: "compaction", Display: "[maintenance usage]"},
		{Type: EventPromptUsage, Prompt: 1, Display: "[prompt: 2 turns]"},
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
		`[read_file path="x"] → 12B` + "\n" +
		"[notice]\n" +
		"[turn: 2]"
	if got != want {
		t.Fatalf("latest output mismatch:\nwant %q\n got %q", want, got)
	}
	if strings.Contains(got, "one long prompt") || strings.Contains(got, "old answer") || strings.Contains(got, "prompt: 2 turns") || strings.Contains(got, "maintenance") {
		t.Fatalf("latest output included wrong turn/user text: %q", got)
	}
}

func TestLatestTurnOutputUsesPromptAndTurnPairWhenTurnNumbersReset(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "session")
	events := []Event{
		{Type: EventUser, Prompt: 1, Text: "first prompt"},
		{Type: EventAssistantDelta, Prompt: 1, Turn: 2, Text: "prompt one turn two"},
		{Type: EventTurnComplete, Prompt: 1, Turn: 2, Display: "[turn: 2]"},
		{Type: EventUser, Prompt: 2, Text: "second prompt"},
		{Type: EventAssistantDelta, Prompt: 2, Turn: 1, Text: "prompt two turn one"},
		{Type: EventTurnComplete, Prompt: 2, Turn: 1, Display: "[turn: 1]"},
		{Type: EventPromptUsage, Prompt: 2, Display: "[prompt: 1 turn]"},
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
	if got != "prompt two turn one\n[turn: 1]" {
		t.Fatalf("latest output = %q, want the latest (prompt, turn) pair", got)
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

func TestLoadRecoversActiveToolDispatchWithoutReexecutingTool(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "session")
	at := time.Date(2026, 7, 26, 18, 0, 0, 0, time.UTC)
	state := Session{
		Version:         Version,
		Provider:        "responses",
		Model:           "gpt-test",
		Created:         at,
		Updated:         at.Add(time.Second),
		System:          "test system",
		ProxySessionID:  "proxy-1",
		CacheAffinityID: "cache-1",
		Prompt:          1,
		Messages: []llm.Message{
			{Role: llm.RoleUser, Time: at, Origin: llm.MessageOriginPrompt, Content: []llm.ContentBlock{{Kind: llm.BlockText, Text: "change it"}}},
			{Role: llm.RoleAssistant, Time: at.Add(time.Second), Content: []llm.ContentBlock{{
				Kind:      llm.BlockToolUse,
				ToolUseID: "call-1",
				ToolName:  "write_file",
				ToolInput: json.RawMessage(`{"path":"x"}`),
			}}},
		},
		Todos: []todo.Item{{Content: "change it", Status: "in_progress"}},
		Plans: []plan.Plan{{Title: "Plan 1", Body: "Do it", Path: "plans/1.md"}},
		Usage: UsageTotals{Usage: llm.Usage{InputTokens: 11, OutputTokens: 3}},
	}
	digest, err := llm.FingerprintMessages(state.Messages)
	if err != nil {
		t.Fatal(err)
	}
	state.ResponseState = &llm.ResponseState{PreviousResponseID: "resp-1", AnchorMessages: 2, AnchorDigest: digest}
	if err := SaveActiveTurnCheckpoint(dir, state, "tool_dispatch", 1, 1); err != nil {
		t.Fatalf("SaveActiveTurnCheckpoint: %v", err)
	}

	recovered, err := Load(dir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if recovered.Recovery == nil || recovered.Recovery.Phase != "tool_dispatch" || recovered.Recovery.Turn != 1 {
		t.Fatalf("recovery metadata = %+v", recovered.Recovery)
	}
	if err := llm.ValidateTranscript(recovered.Messages); err != nil {
		t.Fatalf("recovered transcript: %v", err)
	}
	if len(recovered.Messages) != 3 {
		t.Fatalf("recovered messages = %d, want prompt/tool-use/interrupted result", len(recovered.Messages))
	}
	result := recovered.Messages[2].Content[0]
	if result.Kind != llm.BlockToolResult || result.ResultForID != "call-1" || !result.ResultError || result.ResultText != "interrupted" {
		t.Fatalf("recovered tool result = %+v", result)
	}
	if recovered.ResponseState == nil || recovered.ResponseState.PreviousResponseID != "resp-1" || recovered.ResponseState.AnchorMessages != 2 {
		t.Fatalf("response state = %+v", recovered.ResponseState)
	}
	if len(recovered.Todos) != 1 || len(recovered.Plans) != 1 || recovered.Usage.InputTokens != 11 {
		t.Fatalf("recovered durable state = todos %+v plans %+v usage %+v", recovered.Todos, recovered.Plans, recovered.Usage)
	}

	if err := recovered.SaveConsolidated(dir); err != nil {
		t.Fatalf("SaveConsolidated: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, activeTurnFile)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("active checkpoint after consolidation: %v", err)
	}
	reloaded, err := Load(dir)
	if err != nil {
		t.Fatalf("Load consolidated: %v", err)
	}
	if reloaded.Recovery != nil || !reflect.DeepEqual(reloaded.Messages, recovered.Messages) {
		t.Fatalf("consolidated recovery = %+v messages equal = %v", reloaded.Recovery, reflect.DeepEqual(reloaded.Messages, recovered.Messages))
	}
}

func testCheckpointState(at time.Time, text string) Session {
	return Session{
		Version:  Version,
		Provider: "responses",
		Model:    "gpt-test",
		Created:  at,
		Updated:  at,
		System:   "test system",
		Prompt:   1,
		Messages: []llm.Message{
			{Role: llm.RoleUser, Time: at, Origin: llm.MessageOriginPrompt, Content: []llm.ContentBlock{{Kind: llm.BlockText, Text: text}}},
		},
	}
}

func TestLoadToleratesCorruptActiveTurnCheckpoint(t *testing.T) {
	for _, tc := range []struct {
		name  string
		write func(t *testing.T, dir string)
	}{
		{
			name: "garbage bytes",
			write: func(t *testing.T, dir string) {
				t.Helper()
				if err := os.WriteFile(filepath.Join(dir, activeTurnFile), []byte("{not json"), 0o644); err != nil {
					t.Fatalf("write corrupt checkpoint: %v", err)
				}
			},
		},
		{
			name: "unsupported version",
			write: func(t *testing.T, dir string) {
				t.Helper()
				checkpoint := map[string]any{
					"version":  Version + 1,
					"phase":    "closed_turn",
					"saved_at": time.Now(),
					"state":    map[string]any{"version": Version + 1},
					"messages": []any{map[string]any{"role": "user"}},
				}
				data, err := json.Marshal(checkpoint)
				if err != nil {
					t.Fatalf("marshal checkpoint: %v", err)
				}
				if err := os.WriteFile(filepath.Join(dir, activeTurnFile), data, 0o644); err != nil {
					t.Fatalf("write version-skewed checkpoint: %v", err)
				}
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := filepath.Join(t.TempDir(), "session")
			at := time.Date(2026, 7, 26, 18, 0, 0, 0, time.UTC)
			state := testCheckpointState(at, "healthy saved state")
			if err := state.Save(dir); err != nil {
				t.Fatalf("Save: %v", err)
			}
			tc.write(t, dir)

			loaded, err := Load(dir)
			if err != nil {
				t.Fatalf("Load with corrupt checkpoint: %v", err)
			}
			if loaded.RecoveryWarning == "" {
				t.Fatal("Load should report the ignored checkpoint via RecoveryWarning")
			}
			if loaded.Recovery != nil {
				t.Fatalf("Recovery = %+v, want nil for an ignored checkpoint", loaded.Recovery)
			}
			if len(loaded.Messages) != 1 || loaded.Messages[0].Content[0].Text != "healthy saved state" {
				t.Fatalf("loaded messages = %+v, want the saved state", loaded.Messages)
			}
		})
	}
}

func TestLoadDropsStaleCheckpointWithEqualUpdated(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "session")
	at := time.Date(2026, 7, 26, 18, 0, 0, 0, time.UTC)
	state := testCheckpointState(at, "saved turn")
	if err := SaveActiveTurnCheckpoint(dir, state, "closed_turn", 1, 1); err != nil {
		t.Fatalf("SaveActiveTurnCheckpoint: %v", err)
	}
	// A crash between Save and ClearActiveTurnCheckpoint leaves a checkpoint
	// whose Updated equals the consolidated state's.
	if err := state.Save(dir); err != nil {
		t.Fatalf("Save: %v", err)
	}

	loaded, err := Load(dir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if loaded.Recovery != nil {
		t.Fatalf("Recovery = %+v, want nil for an equal-Updated checkpoint", loaded.Recovery)
	}
	if _, err := os.Stat(filepath.Join(dir, activeTurnFile)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("stale checkpoint should be removed: %v", err)
	}
}

func TestSaveAdoptsExistingTreeWhenTreeNil(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "session")
	at := time.Date(2026, 7, 26, 18, 0, 0, 0, time.UTC)
	first := testCheckpointState(at, "first turn")
	if err := first.Save(dir); err != nil {
		t.Fatalf("first Save: %v", err)
	}
	loaded, err := Load(dir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	// A distinct Session value with Tree == nil (e.g. a delegate child checkpoint)
	// must adopt the on-disk tree identity instead of minting a conflicting ID.
	second := testCheckpointState(at.Add(time.Minute), "second turn")
	second.Messages = append(append([]llm.Message{}, loaded.Messages...), llm.Message{
		Role:    llm.RoleAssistant,
		Time:    at.Add(time.Minute),
		Content: []llm.ContentBlock{{Kind: llm.BlockText, Text: "second answer"}},
	})
	if err := second.Save(dir); err != nil {
		t.Fatalf("second Save: %v", err)
	}

	tree, err := LoadTree(dir, "")
	if err != nil {
		t.Fatalf("LoadTree: %v", err)
	}
	if tree.Header.ID != loaded.ID {
		t.Fatalf("tree ID = %q, want the original %q", tree.Header.ID, loaded.ID)
	}
	messages, err := tree.BuildContext()
	if err != nil {
		t.Fatalf("BuildContext: %v", err)
	}
	if len(messages) != 2 || messages[0].Content[0].Text != "first turn" || messages[1].Content[0].Text != "second answer" {
		t.Fatalf("tree messages = %+v, want both turns under one growing tree", messages)
	}
}

func TestLoadRecoveryPreservesCheckpointTreeIdentity(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "session")
	at := time.Date(2026, 7, 26, 18, 0, 0, 0, time.UTC)
	state := testCheckpointState(at, "crashed turn")
	state.ParentSession = "parent-1"
	state.ParentEntryID = "entry-1"
	// Consolidate once so tree.ndjson exists, then reload so state.ID carries
	// the tree identity Save stamped into state.json, as it does in production.
	if err := state.SaveConsolidated(dir); err != nil {
		t.Fatalf("SaveConsolidated: %v", err)
	}
	saved, err := Load(dir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	// The checkpoint snapshots the exact loaded session (same tree identity and
	// message timestamps as on disk), simulating a crash before consolidation.
	state = saved
	if err := SaveActiveTurnCheckpoint(dir, state, "tool_dispatch", 1, 2); err != nil {
		t.Fatalf("SaveActiveTurnCheckpoint: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, stateFile), []byte("{not json"), 0o644); err != nil {
		t.Fatalf("corrupt state.json: %v", err)
	}

	recovered, err := Load(dir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if recovered.Recovery == nil || recovered.Recovery.Phase != "tool_dispatch" {
		t.Fatalf("recovery metadata = %+v", recovered.Recovery)
	}
	if recovered.ID != state.ID {
		t.Fatalf("recovered ID = %q, want checkpoint tree ID %q", recovered.ID, state.ID)
	}
	if recovered.ParentSession != "parent-1" || recovered.ParentEntryID != "entry-1" {
		t.Fatalf("recovered parent linkage = %q/%q", recovered.ParentSession, recovered.ParentEntryID)
	}
	if err := recovered.SaveConsolidated(dir); err != nil {
		t.Fatalf("SaveConsolidated after recovery: %v", err)
	}
	reloaded, err := Load(dir)
	if err != nil {
		t.Fatalf("Load consolidated: %v", err)
	}
	if reloaded.ID != state.ID {
		t.Fatalf("reloaded ID = %q, want %q", reloaded.ID, state.ID)
	}
}

func TestAbandonRunningChildrenMakesInterruptedCheckpointTerminal(t *testing.T) {
	root := filepath.Join(t.TempDir(), "session")
	runningDir, err := SaveChildMeta(root, ChildMeta{
		ID:         "running-child",
		Kind:       "delegate",
		Status:     ChildStatusRunning,
		Created:    time.Date(2026, 7, 26, 18, 0, 0, 0, time.UTC),
		Transcript: filepath.Join("children", "running-child", stateFile),
	})
	if err != nil {
		t.Fatalf("SaveChildMeta running: %v", err)
	}
	_, err = SaveChildMeta(root, ChildMeta{
		ID:      "complete-child",
		Kind:    "delegate",
		Status:  ChildStatusCompleted,
		Created: time.Date(2026, 7, 26, 18, 0, 0, 0, time.UTC),
	})
	if err != nil {
		t.Fatalf("SaveChildMeta complete: %v", err)
	}
	at := time.Date(2026, 7, 26, 19, 0, 0, 0, time.UTC)
	count, skipped, err := AbandonRunningChildren(root, at)
	if err != nil {
		t.Fatalf("AbandonRunningChildren: %v", err)
	}
	if count != 1 || skipped != 0 {
		t.Fatalf("abandoned = %d skipped = %d, want 1/0", count, skipped)
	}
	data, err := os.ReadFile(filepath.Join(runningDir, "meta.json"))
	if err != nil {
		t.Fatalf("read running metadata: %v", err)
	}
	var meta ChildMeta
	if err := json.Unmarshal(data, &meta); err != nil {
		t.Fatalf("decode running metadata: %v", err)
	}
	if meta.Status != ChildStatusAbandoned || !meta.Updated.Equal(at) || meta.TerminationReason != "cancelled" || !strings.Contains(meta.Error, "resumed") {
		t.Fatalf("abandoned metadata = %+v", meta)
	}
	target, err := readFollowTarget(runningDir)
	if err != nil {
		t.Fatalf("readFollowTarget: %v", err)
	}
	if !target.terminal() {
		t.Fatalf("abandoned target = %+v, want terminal", target)
	}
}

func TestAbandonRunningChildrenSkipsUnreadableChildren(t *testing.T) {
	root := filepath.Join(t.TempDir(), "session")
	runningDir, err := SaveChildMeta(root, ChildMeta{
		ID:      "running-child",
		Kind:    "delegate",
		Status:  ChildStatusRunning,
		Created: time.Date(2026, 7, 26, 18, 0, 0, 0, time.UTC),
	})
	if err != nil {
		t.Fatalf("SaveChildMeta running: %v", err)
	}
	// A child directory left before its first metadata write (crash between
	// MkdirAll and SaveChildMeta) plus one with malformed metadata.
	if err := os.MkdirAll(filepath.Join(root, "children", "orphan-child"), 0o755); err != nil {
		t.Fatalf("mkdir orphan child: %v", err)
	}
	corruptDir := filepath.Join(root, "children", "corrupt-child")
	if err := os.MkdirAll(corruptDir, 0o755); err != nil {
		t.Fatalf("mkdir corrupt child: %v", err)
	}
	if err := os.WriteFile(filepath.Join(corruptDir, "meta.json"), []byte("{not json"), 0o644); err != nil {
		t.Fatalf("write corrupt meta: %v", err)
	}

	abandoned, skipped, err := AbandonRunningChildren(root, time.Date(2026, 7, 26, 19, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("AbandonRunningChildren: %v", err)
	}
	if abandoned != 1 || skipped != 2 {
		t.Fatalf("abandoned = %d skipped = %d, want 1/2", abandoned, skipped)
	}
	data, err := os.ReadFile(filepath.Join(runningDir, "meta.json"))
	if err != nil {
		t.Fatalf("read running metadata: %v", err)
	}
	var meta ChildMeta
	if err := json.Unmarshal(data, &meta); err != nil {
		t.Fatalf("decode running metadata: %v", err)
	}
	if meta.Status != ChildStatusAbandoned {
		t.Fatalf("running child status = %q, want abandoned", meta.Status)
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
