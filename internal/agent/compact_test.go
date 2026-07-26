package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"harness/internal/hooks"
	"harness/internal/llm"
	"harness/internal/llm/llmtest"
	"harness/internal/tools"
	"harness/prompts"
)

// userText is a genuine user input message (text, not tool results).
func userText(s string) llm.Message {
	return llm.Message{Role: llm.RoleUser, Content: []llm.ContentBlock{{Kind: llm.BlockText, Text: s}}}
}

// asstText is an end-turn assistant message with no tool calls.
func asstText(s string) llm.Message {
	return llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{{Kind: llm.BlockText, Text: s}}}
}

// asstToolUse is an assistant message that issues one tool call.
func asstToolUse(id, name, input string) llm.Message {
	return llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{
		{Kind: llm.BlockToolUse, ToolUseID: id, ToolName: name, ToolInput: []byte(input)},
	}}
}

// toolResult is the user message answering one tool call.
func toolResult(id, text string) llm.Message {
	return llm.Message{Role: llm.RoleUser, Content: []llm.ContentBlock{
		{Kind: llm.BlockToolResult, ResultForID: id, ResultText: text},
	}}
}

// summaryStep scripts a canned summary reply from the FakeProvider.
func summaryStep(summary string, in, out int) llmtest.Step {
	return llmtest.Step{
		Events: []llm.StreamEvent{textDelta(summary)},
		Stop:   llm.StopEndTurn,
		Usage:  llm.Usage{InputTokens: in, OutputTokens: out},
	}
}

// makeTurns builds n whole text turns (user + assistant), labelled by index.
func makeTurns(n int) []llm.Message {
	msgs := make([]llm.Message, 0, 2*n)
	for i := 0; i < n; i++ {
		msgs = append(msgs, userText(turnLabel(i)+" question"), asstText(turnLabel(i)+" answer"))
	}
	return msgs
}

func turnLabel(i int) string {
	return string(rune('A' + i))
}

func TestGenerateSummaryUsesGivenSystemPrompt(t *testing.T) {
	fp := llmtest.New("fake", summaryStep("HANDOFF BRIEF", 100, 20))
	a := newAgent(fp, tools.Default(), Options{Model: "claude-opus-4-8"})
	a.SetSystem("plan agent system prompt")
	a.SetTranscript(makeTurns(3))

	const handoffPrompt = "WRITE A HANDOFF BRIEF FOR A FRESH AGENT"
	summary, usage, err := a.GenerateSummary(context.Background(), handoffPrompt)
	if err != nil {
		t.Fatalf("GenerateSummary: %v", err)
	}
	if summary != "HANDOFF BRIEF" {
		t.Errorf("summary = %q, want canned reply", summary)
	}
	if usage.InputTokens != 100 || usage.OutputTokens != 20 {
		t.Errorf("usage = %+v, want 100/20", usage)
	}
	if len(fp.Requests) != 1 {
		t.Fatalf("want 1 model call, got %d", len(fp.Requests))
	}
	if fp.Requests[0].System != handoffPrompt {
		t.Errorf("summary call System = %q, want the handoff prompt (not compaction)", fp.Requests[0].System)
	}
	if fp.Requests[0].Purpose != llm.RequestPurposeHandoffSummary {
		t.Errorf("summary call purpose = %q, want %q", fp.Requests[0].Purpose, llm.RequestPurposeHandoffSummary)
	}
	if fp.Requests[0].System == prompts.CompactionSummary() {
		t.Error("GenerateSummary must not reuse the compaction prompt")
	}
}

func TestCompactKeepsLastEightTurns(t *testing.T) {
	// Ten whole turns; compaction keeps the latest eight assistant turns and
	// checkpoints the earlier progress and active instructions.
	transcript := makeTurns(10)

	fp := llmtest.New("fake", summaryStep("CANNED SUMMARY", 200, 40))
	a := newAgent(fp, tools.Default(), Options{Model: "claude-opus-4-8"})
	a.SetSystem("system prompt")
	a.SetTranscript(transcript)

	sink := &recordSink{}
	if _, err := a.Compact(context.Background(), sink); err != nil {
		t.Fatalf("Compact: %v", err)
	}

	msgs := a.Transcript()
	mustValid(t, msgs)

	if len(msgs) != 16 {
		t.Fatalf("want checkpoint + 8 kept turns, got %d messages:\n%s", len(msgs), dump(msgs))
	}
	if msgs[0].Role != llm.RoleUser || msgs[0].Origin != llm.MessageOriginCompactionCheckpoint {
		t.Errorf("first message should be a user checkpoint, got role %q origin %q", msgs[0].Role, msgs[0].Origin)
	}
	got := msgs[0].Content[0].Text
	if !strings.HasPrefix(got, checkpointHeader) {
		t.Errorf("checkpoint missing header, got %q", got)
	}
	if !strings.Contains(got, "CANNED SUMMARY") {
		t.Errorf("summary message should carry the model's summary, got %q", got)
	}

	// The kept turns are C..J; C's input survives verbatim in the checkpoint.
	if msgs[1].Content[0].Text != "C answer" || !strings.Contains(got, "C question") {
		t.Errorf("first retained turn/checkpoint mismatch: %q / %q", msgs[1].Content[0].Text, got)
	}
	if msgs[15].Content[0].Text != "J answer" {
		t.Errorf("last kept message = %q, want %q", msgs[15].Content[0].Text, "J answer")
	}

	// The summary request received the older turns but never the system prompt
	// as a message (it lives on Request.System).
	if len(fp.Requests) != 1 {
		t.Fatalf("summary call count = %d, want 1", len(fp.Requests))
	}
	if fp.Requests[0].System != prompts.CompactionSummary() {
		t.Fatalf("summary call system prompt = %q, want embedded compaction prompt", fp.Requests[0].System)
	}
	if fp.Requests[0].Purpose != llm.RequestPurposeCompaction {
		t.Fatalf("summary call purpose = %q, want %q", fp.Requests[0].Purpose, llm.RequestPurposeCompaction)
	}
}

func TestPreferredCompactionBoundaryUsesTokenFloorAndRoundCap(t *testing.T) {
	large := make([]llm.Message, 0, 20)
	for i := 0; i < 10; i++ {
		large = append(large, userText("q"), asstText(strings.Repeat(string(rune('a'+i)), 4_000)))
	}
	a := newAgent(llmtest.New("fake"), tools.Default(), Options{CompactKeepTokens: 2_500, CompactKeepTurns: 8})
	a.SetTranscript(large)
	turns := completedTurnSpans(large)
	boundary := a.preferredCompactionBoundary(turns)
	if got := countCompletedTurns(large[boundary:]); got != 3 {
		t.Fatalf("token-floor suffix kept %d rounds, want 3", got)
	}

	a.SetTranscript(makeTurns(10))
	turns = completedTurnSpans(a.Transcript())
	boundary = a.preferredCompactionBoundary(turns)
	if got := countCompletedTurns(a.Transcript()[boundary:]); got != 8 {
		t.Fatalf("small-round suffix kept %d rounds, want cap 8", got)
	}
}

func TestCompactForContinuationCheckpointsAndArchivesCompleteTranscript(t *testing.T) {
	transcript := makeTurns(3)
	transcript[0].Origin = llm.MessageOriginPrompt
	fp := llmtest.New("fake", summaryStep("CONTINUATION STATE", 120, 18))
	a := newAgent(fp, tools.Default(), Options{Model: "claude-opus-4-8"})
	a.SetSystem("system prompt")
	a.SetTranscript(transcript)
	a.SetProxySessionID("proxy-source")
	a.SetResponseState(&llm.ResponseState{PreviousResponseID: "response-source"})

	var archived CompactionArchive
	a.SetCompactionArchiver(func(_ context.Context, archive CompactionArchive) (string, error) {
		archived = archive
		return "compactions/0001.input.json", nil
	})
	usage, changed, err := a.CompactForContinuation(context.Background(), &recordSink{})
	if err != nil {
		t.Fatalf("CompactForContinuation: %v", err)
	}
	if !changed || usage.InputTokens != 120 || usage.OutputTokens != 18 {
		t.Fatalf("continuation compaction = changed %t usage %+v", changed, usage)
	}
	if len(fp.Requests) != 1 || len(fp.Requests[0].Messages) != len(transcript)+1 ||
		!strings.Contains(dump(fp.Requests[0].Messages), "C answer") {
		t.Fatalf("summary request omitted complete transcript: %s", dump(fp.Requests[0].Messages))
	}
	if !reflect.DeepEqual(archived.Messages, transcript) || archived.Summary != "CONTINUATION STATE" {
		t.Fatalf("archive = %+v, want complete source transcript and summary", archived)
	}

	got := a.Transcript()
	mustValid(t, got)
	if len(got) != 1 || got[0].Origin != llm.MessageOriginCompactionCheckpoint || got[0].Compaction == nil {
		t.Fatalf("continuation transcript = %+v, want one typed checkpoint", got)
	}
	if got[0].Compaction.Summary != "CONTINUATION STATE" ||
		!strings.Contains(got[0].Content[0].Text, "A question") ||
		!strings.Contains(got[0].Content[0].Text, "compactions/0001.input.json") {
		t.Fatalf("continuation checkpoint = %+v", got[0])
	}
	if a.ProxySessionID() == "proxy-source" || a.ProxySessionID() == "" || a.ResponseState() != nil {
		t.Fatalf("continuation compaction retained remote anchor: proxy=%q state=%+v", a.ProxySessionID(), a.ResponseState())
	}
}

// Regression: low-water pressure used to drop retained rounds after the summary
// and archive boundary had already been fixed. Every moved round must be included
// in a regenerated summary and in the final raw archive.
func TestLowWaterBoundaryMoveRegeneratesSummaryAndArchive(t *testing.T) {
	transcript := make([]llm.Message, 0, 20)
	for i := 0; i < 10; i++ {
		transcript = append(transcript, userText(turnLabel(i)+" question"), asstText(turnLabel(i)+" "+strings.Repeat("x", 3_500)))
	}
	fp := llmtest.New("fake", summaryStep("first", 10, 1), summaryStep("second", 20, 2))
	a := newAgent(fp, &tools.Registry{}, Options{Model: "local", ContextWindow: 10_000})
	a.SetTranscript(transcript)
	var archived CompactionArchive
	a.SetCompactionArchiver(func(_ context.Context, archive CompactionArchive) (string, error) {
		archived = archive
		return "archive", nil
	})
	usage, err := a.Compact(context.Background(), &recordSink{})
	if err != nil {
		t.Fatalf("Compact: %v", err)
	}
	if len(fp.Requests) != 2 || usage.InputTokens != 30 || usage.OutputTokens != 3 {
		t.Fatalf("summary retries = %d usage=%+v, want 2 and cumulative usage", len(fp.Requests), usage)
	}
	if got := countCompletedTurns(archived.Messages); got != 3 {
		t.Fatalf("archived rounds = %d, want final moved boundary with 3", got)
	}
	if !strings.Contains(dump(fp.Requests[1].Messages), "C ") {
		t.Fatalf("regenerated summary input omitted moved round C: %s", dump(fp.Requests[1].Messages))
	}
}

func TestAutomaticCompactionCanBeDisabledWithoutDisablingManual(t *testing.T) {
	fp := llmtest.New("fake", summaryStep("manual summary", 10, 2))
	a := newAgent(fp, tools.Default(), Options{DisableAutoCompaction: true})
	a.SetTranscript(makeTurns(10))

	if usage, changed, err := a.MaybeCompact(context.Background(), a.window(), &recordSink{}); err != nil || changed || usage != (llm.Usage{}) {
		t.Fatalf("disabled automatic compaction = usage %+v changed=%t err=%v", usage, changed, err)
	}
	if _, err := a.Compact(context.Background(), &recordSink{}); err != nil {
		t.Fatalf("manual compaction: %v", err)
	}
	if fp.RequestCount() != 1 || a.Transcript()[0].Origin != llm.MessageOriginCompactionCheckpoint {
		t.Fatalf("manual compaction did not run with auto disabled: requests=%d", fp.RequestCount())
	}
}

func TestNondefaultCompactionPercentagesMoveTriggerAndBudget(t *testing.T) {
	a := newAgent(llmtest.New("fake"), &tools.Registry{}, Options{
		Model:                 "local",
		ContextWindow:         10_000,
		CompactTriggerPercent: 90,
		CompactTargetPercent:  50,
	})
	if a.overThreshold(8_999) || !a.overThreshold(9_000) {
		t.Fatalf("90%% trigger did not move threshold")
	}
	if got := a.compactBudget(); got != 5_000 {
		t.Fatalf("50%% target budget = %d, want 5000", got)
	}
}

func TestCompactionFileActivityIsCumulativeAndModifiedWins(t *testing.T) {
	reg := tools.Catalog()
	a := newAgent(llmtest.New("fake"), reg, Options{})
	failed := toolResult("r2", "missing")
	failed.Content[0].ResultError = true
	messages := []llm.Message{
		asstToolUse("r1", "read_file", `{"paths":["z.go","./a.go"]}`), toolResult("r1", "ok with one possible inline failure"),
		asstToolUse("r2", "read_file", `{"path":"failed.go"}`), failed,
		asstToolUse("w1", "write_file", `{"path":"a.go","content":"x"}`), toolResult("w1", "ok"),
		asstToolUse("e1", "edit", `{"files":[{"path":"edit.go","edits":[{"oldText":"a","newText":"b"}]}]}`), toolResult("e1", "ok"),
		asstToolUse("p1", "apply_patch", `{"patch":"*** Begin Patch\n*** Add File: patch.go\n+x\n*** End Patch\n"}`), toolResult("p1", "ok"),
		asstToolUse("u1", "run_command", `{"args":["touch","ignored.go"]}`), toolResult("u1", "ok"),
	}
	prior := &llm.CompactionMetadata{ReadFiles: []string{"prior.go"}, ModifiedFiles: []string{"already.go"}}
	reads, modified := a.compactionFileActivity(messages, prior)
	if got, want := strings.Join(reads, ","), "prior.go,z.go"; got != want {
		t.Fatalf("read files = %q, want %q", got, want)
	}
	if got, want := strings.Join(modified, ","), "a.go,already.go,edit.go,patch.go"; got != want {
		t.Fatalf("modified files = %q, want %q", got, want)
	}
}

func TestIdleCompactionAppliesMatchingSnapshot(t *testing.T) {
	fp := llmtest.New("fake", summaryStep("idle summary", 100, 12))
	a := newAgent(fp, tools.Default(), Options{Model: "local", ContextWindow: 10_000})
	a.SetTranscript(makeTurns(10))
	var archives []CompactionArchive
	a.SetCompactionArchiver(func(_ context.Context, archive CompactionArchive) (string, error) {
		archives = append(archives, archive)
		return "compactions/idle.input.json", nil
	})

	work, ok, err := a.PrepareIdleCompaction(1)
	if err != nil || !ok {
		t.Fatalf("PrepareIdleCompaction = ok %t err %v, want work", ok, err)
	}
	result, err := work(context.Background())
	if err != nil {
		t.Fatalf("idle work: %v", err)
	}
	if result.Usage.InputTokens != 100 || result.Usage.OutputTokens != 12 {
		t.Fatalf("idle usage = %+v, want 100/12", result.Usage)
	}
	if len(archives) != 0 {
		t.Fatal("background preparation ran the live archive callback")
	}

	sink := &recordSink{}
	applied, err := a.ApplyIdleCompaction(context.Background(), sink, result)
	if err != nil || !applied {
		t.Fatalf("ApplyIdleCompaction = applied %t err %v", applied, err)
	}
	if len(archives) != 1 || archives[0].Summary != "idle summary" {
		t.Fatalf("applied archives = %+v, want one idle summary", archives)
	}
	got := a.Transcript()
	if len(got) != 16 || got[0].Origin != llm.MessageOriginCompactionCheckpoint {
		t.Fatalf("idle transcript = %d messages, want checkpoint + 8 turns", len(got))
	}
	if !strings.Contains(got[0].Content[0].Text, "compactions/idle.input.json") {
		t.Fatalf("idle checkpoint missing archive reference: %q", got[0].Content[0].Text)
	}
	if len(sink.notices) != 1 || !strings.HasPrefix(sink.notices[0], "[idle compacted:") {
		t.Fatalf("idle notices = %v", sink.notices)
	}
}

func TestIdleCompactionDiscardsStaleSnapshotBeforeArchiving(t *testing.T) {
	for _, mutate := range []struct {
		name string
		run  func(*Agent)
	}{
		{
			name: "transcript",
			run: func(a *Agent) {
				a.SetTranscript(append(cloneMessages(a.Transcript()), userText("new prompt")))
			},
		},
		{
			name: "runtime",
			run: func(a *Agent) {
				a.SetReasoning(llm.ReasoningConfig{Profile: "high"})
			},
		},
	} {
		t.Run(mutate.name, func(t *testing.T) {
			fp := llmtest.New("fake", summaryStep("idle summary", 10, 2))
			a := newAgent(fp, tools.Default(), Options{Model: "local", ContextWindow: 10_000})
			a.SetTranscript(makeTurns(10))
			archived := false
			a.SetCompactionArchiver(func(_ context.Context, _ CompactionArchive) (string, error) {
				archived = true
				return "archive", nil
			})
			work, ok, err := a.PrepareIdleCompaction(1)
			if err != nil || !ok {
				t.Fatalf("PrepareIdleCompaction = ok %t err %v", ok, err)
			}
			result, err := work(context.Background())
			if err != nil {
				t.Fatalf("idle work: %v", err)
			}
			mutate.run(a)
			before := cloneMessages(a.Transcript())

			applied, err := a.ApplyIdleCompaction(context.Background(), &recordSink{}, result)
			if err != nil || applied {
				t.Fatalf("ApplyIdleCompaction = applied %t err %v, want stale discard", applied, err)
			}
			if archived {
				t.Fatal("stale idle candidate ran archive callback")
			}
			if !reflect.DeepEqual(a.Transcript(), before) {
				t.Fatal("stale idle candidate changed the live transcript")
			}
		})
	}
}

func TestPrepareIdleCompactionEligibility(t *testing.T) {
	a := newAgent(llmtest.New("fake"), tools.Default(), Options{Model: "local", ContextWindow: 1_000_000})
	a.SetTranscript(makeTurns(2))
	if _, ok, err := a.PrepareIdleCompaction(40); err != nil || ok {
		t.Fatalf("below-threshold preparation = ok %t err %v", ok, err)
	}
	if _, ok, err := a.PrepareIdleCompaction(100); err == nil || ok {
		t.Fatalf("invalid trigger preparation = ok %t err %v", ok, err)
	}

	disabled := newAgent(llmtest.New("fake"), tools.Default(), Options{
		Model:                 "local",
		ContextWindow:         1,
		DisableAutoCompaction: true,
	})
	disabled.SetTranscript(makeTurns(10))
	if _, ok, err := disabled.PrepareIdleCompaction(1); err != nil || ok {
		t.Fatalf("disabled preparation = ok %t err %v", ok, err)
	}

	hookConfig, err := hooks.DecodeEventMap(json.RawMessage(`{
		"PreCompact": [{"hooks": [{"type": "command", "command": "true"}]}]
	}`))
	if err != nil {
		t.Fatalf("decode hook config: %v", err)
	}
	hooked := newAgent(llmtest.New("fake"), tools.Default(), Options{
		Model:         "local",
		ContextWindow: 10_000,
		Hooks:         &hooks.Runner{Config: hookConfig},
	})
	hooked.SetTranscript(makeTurns(10))
	if _, ok, err := hooked.PrepareIdleCompaction(1); err != nil || ok {
		t.Fatalf("hooked preparation = ok %t err %v", ok, err)
	}
}

func TestCompactWithFocusStoresOnlyCurrentFocus(t *testing.T) {
	fp := llmtest.New("fake", summaryStep("focused summary", 10, 2), summaryStep("later summary", 11, 3))
	a := newAgent(fp, tools.Default(), Options{})
	a.SetTranscript(makeTurns(10))
	var archives []CompactionArchive
	a.SetCompactionArchiver(func(_ context.Context, archive CompactionArchive) (string, error) {
		archives = append(archives, archive)
		return "archive", nil
	})
	if _, err := a.CompactWithFocus(context.Background(), &recordSink{}, "  preserve API names  "); err != nil {
		t.Fatalf("focused compact: %v", err)
	}
	if !strings.Contains(fp.Requests[0].System, "preserve API names") || a.Transcript()[0].Compaction.Focus != "preserve API names" || archives[0].Focus != "preserve API names" {
		t.Fatalf("focus was not propagated: request=%q checkpoint=%+v archive=%+v", fp.Requests[0].System, a.Transcript()[0].Compaction, archives[0])
	}

	a.SetTranscript(append(a.Transcript(), makeTurns(9)...))
	if _, _, err := a.compactTriggered(context.Background(), &recordSink{}, "auto"); err != nil {
		t.Fatalf("later automatic compact: %v", err)
	}
	if fp.Requests[1].System != prompts.CompactionUpdate() {
		t.Fatalf("repeated compaction system = %q, want update prompt", fp.Requests[1].System)
	}
	if a.Transcript()[0].Compaction.Focus != "" || archives[1].Focus != "" || strings.Contains(fp.Requests[1].System, "preserve API names") {
		t.Fatalf("one-shot focus leaked into later compaction")
	}
	metadata := fp.Requests[1].Messages[0].Content[0].Text
	if !strings.Contains(metadata, `"previous_summary":"focused summary"`) {
		t.Fatalf("update metadata missing exact prior summary: %s", metadata)
	}
	for _, message := range fp.Requests[1].Messages[1:] {
		if message.Origin == llm.MessageOriginCompactionCheckpoint || strings.Contains(messageTextForCheckpoint(message), checkpointHeader) {
			t.Fatalf("prior checkpoint was sent as ordinary update history: %+v", message)
		}
	}
}

func TestRepeatedCompactionMapReduceUsesPriorSummaryOnlyInFinalUpdate(t *testing.T) {
	fp := llmtest.New("fake",
		summaryStep("chunk one", 10, 1),
		summaryStep("chunk two", 11, 1),
		summaryStep("chunk three", 12, 1),
		summaryStep("updated", 13, 2),
	)
	a := newAgent(fp, tools.Default(), Options{Model: "local", ContextWindow: 4_000})
	checkpoint := userText("rendered checkpoint")
	checkpoint.Origin = llm.MessageOriginCompactionCheckpoint
	checkpoint.Compaction = &llm.CompactionMetadata{Summary: "EXACT PRIOR SUMMARY"}
	older := []llm.Message{checkpoint}
	for i := 0; i < 3; i++ {
		older = append(older, userText("q"), asstText(strings.Repeat(string(rune('a'+i)), 6_000)))
	}
	got, _, err := a.summarizeCompaction(context.Background(), older, checkpoint.Compaction, []string{"read.go"}, nil, "focus here")
	if err != nil {
		t.Fatalf("summarizeCompaction: %v", err)
	}
	if got != "updated" || len(fp.Requests) != 4 {
		t.Fatalf("summary=%q requests=%d, want updated/4", got, len(fp.Requests))
	}
	for i, request := range fp.Requests[:3] {
		if !strings.HasPrefix(request.System, prompts.CompactionSummary()) || !strings.Contains(request.System, "focus here") {
			t.Fatalf("map request %d system = %q", i+1, request.System)
		}
		for _, message := range request.Messages {
			if strings.Contains(messageTextForCheckpoint(message), "EXACT PRIOR SUMMARY") {
				t.Fatalf("map request %d received prior summary", i+1)
			}
		}
	}
	final := fp.Requests[3]
	if !strings.HasPrefix(final.System, prompts.CompactionUpdate()) || !strings.Contains(final.System, "focus here") {
		t.Fatalf("final update system = %q", final.System)
	}
	if count := strings.Count(final.Messages[0].Content[0].Text, "EXACT PRIOR SUMMARY"); count != 1 {
		t.Fatalf("prior summary count in final metadata = %d: %q", count, final.Messages[0].Content[0].Text)
	}
}

func TestFocusedCompactionIncludesFocusInHooks(t *testing.T) {
	dir := t.TempDir()
	prePath := filepath.Join(dir, "pre.json")
	postPath := filepath.Join(dir, "post.json")
	configBody, err := json.Marshal(map[string]any{
		"PreCompact":  []any{map[string]any{"hooks": []any{map[string]any{"type": "command", "command": "cat > " + prePath}}}},
		"PostCompact": []any{map[string]any{"hooks": []any{map[string]any{"type": "command", "command": "cat > " + postPath}}}},
	})
	if err != nil {
		t.Fatalf("marshal hooks: %v", err)
	}
	cfg, err := hooks.DecodeEventMap(configBody)
	if err != nil {
		t.Fatalf("DecodeEventMap: %v", err)
	}
	fp := llmtest.New("fake", summaryStep("summary", 10, 2))
	a := newAgent(fp, tools.Default(), Options{Hooks: &hooks.Runner{Config: cfg}})
	a.SetTranscript(makeTurns(10))
	if _, err := a.CompactWithFocus(context.Background(), &recordSink{}, "public API"); err != nil {
		t.Fatalf("CompactWithFocus: %v", err)
	}
	for _, path := range []string{prePath, postPath} {
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read hook payload: %v", err)
		}
		if !strings.Contains(string(raw), `"trigger":"manual"`) || !strings.Contains(string(raw), `"focus":"public API"`) {
			t.Fatalf("hook payload missing focus/trigger: %s", raw)
		}
	}
}

func TestCompactRotatesProxySessionID(t *testing.T) {
	fp := llmtest.New("fake", summaryStep("CANNED SUMMARY", 200, 40))
	a := newAgent(fp, tools.Default(), Options{Model: "claude-opus-4-8"})
	a.SetSystem("system prompt")
	a.SetTranscript(makeTurns(10))
	before := a.ProxySessionID()
	cacheBefore := a.CacheAffinityID()

	if _, err := a.Compact(context.Background(), &recordSink{}); err != nil {
		t.Fatalf("Compact: %v", err)
	}

	after := a.ProxySessionID()
	if before == "" || after == "" || before == after {
		t.Fatalf("proxy session id = %q then %q, want rotation after transcript rewrite", before, after)
	}
	if cacheAfter := a.CacheAffinityID(); cacheAfter != cacheBefore {
		t.Fatalf("cache affinity id = %q then %q, want it preserved across compaction", cacheBefore, cacheAfter)
	}
}

func TestCompactKeepsToolPairsWhole(t *testing.T) {
	// A turn that spans a tool round-trip must be kept whole: no tool_use may be
	// separated from its tool_result by the kept-turns boundary.
	var transcript []llm.Message
	transcript = append(transcript, makeTurns(6)...)
	// Turn G: user, assistant(tool_use), user(tool_result), assistant(text).
	transcript = append(transcript,
		userText("G question"),
		asstToolUse("call_g", "echo", `{}`),
		toolResult("call_g", "tool output"),
		asstText("G answer"),
	)
	transcript = append(transcript, makeTurns(3)...) // H, I, J relabelled below

	fp := llmtest.New("fake", summaryStep("S", 100, 20))
	a := newAgent(fp, tools.Default(), Options{Model: "claude-opus-4-8"})
	a.SetSystem("sys")
	a.SetTranscript(transcript)

	if _, err := a.Compact(context.Background(), &recordSink{}); err != nil {
		t.Fatalf("Compact: %v", err)
	}
	msgs := a.Transcript()
	mustValid(t, msgs)

	// The kept boundary must not split the G tool pair: if turn G is kept, both
	// its tool_use and tool_result survive; the validation above already proves
	// no pair is split, but assert the tool_result is present when its use is.
	var sawUse, sawResult bool
	for _, m := range msgs {
		for _, b := range m.Content {
			if b.Kind == llm.BlockToolUse && b.ToolUseID == "call_g" {
				sawUse = true
			}
			if b.Kind == llm.BlockToolResult && b.ResultForID == "call_g" {
				sawResult = true
			}
		}
	}
	if sawUse != sawResult {
		t.Errorf("tool pair split across the compaction boundary: use=%v result=%v", sawUse, sawResult)
	}
}

func TestCompactBelowThresholdUntouched(t *testing.T) {
	// Only the post-turn trigger should be threshold-gated; a transcript whose
	// last reported input is below 78% of the window is left alone.
	transcript := makeTurns(10)
	fp := llmtest.New("fake") // no summary step scripted
	a := newAgent(fp, tools.Default(), Options{Model: "claude-opus-4-8"})
	a.SetSystem("sys")
	a.SetTranscript(transcript)

	window := a.window()
	below := window / 2 // well under 78%

	sink := &recordSink{}
	if _, _, err := a.MaybeCompact(context.Background(), below, sink); err != nil {
		t.Fatalf("MaybeCompact: %v", err)
	}
	if len(a.Transcript()) != len(transcript) {
		t.Errorf("below-threshold transcript should be untouched, got %d messages", len(a.Transcript()))
	}
	if len(fp.Requests) != 0 {
		t.Errorf("no summary call should happen below threshold, got %d", len(fp.Requests))
	}
	if len(sink.notices) != 0 {
		t.Errorf("no compaction notice below threshold, got %v", sink.notices)
	}
}

func TestMaybeCompactAboveThresholdCompacts(t *testing.T) {
	transcript := makeTurns(10)
	fp := llmtest.New("fake", summaryStep("S", 50, 10))
	a := newAgent(fp, tools.Default(), Options{Model: "claude-opus-4-8"})
	a.SetSystem("sys")
	a.SetTranscript(transcript)

	window := a.window()
	above := window * 80 / 100 // ≥ 78%

	if _, _, err := a.MaybeCompact(context.Background(), above, &recordSink{}); err != nil {
		t.Fatalf("MaybeCompact: %v", err)
	}
	msgs := a.Transcript()
	mustValid(t, msgs)
	if len(msgs) != 16 {
		t.Fatalf("above threshold should compact to checkpoint + 8 turns, got %d", len(msgs))
	}
	if len(fp.Requests) != 1 {
		t.Errorf("summary call count = %d, want 1", len(fp.Requests))
	}
}

// A single prompt can contain many conversational turns. Compaction must count
// each assistant response (plus its tool-result batch) as a turn, preserve the
// latest eight pairs intact, and leave enough headroom that the next small turn
// does not immediately trigger another summary call.
func TestMaybeCompactLongSinglePromptUsesTurnBoundariesAndLowWaterMark(t *testing.T) {
	const window = 20_000
	prompt := userText("implement the complete request exactly")
	prompt.Origin = llm.MessageOriginPrompt
	transcript := []llm.Message{prompt}
	for i := 0; i < 12; i++ {
		id := fmt.Sprintf("call_%02d", i)
		transcript = append(transcript,
			asstToolUse(id, "read_file", fmt.Sprintf(`{"path":"file_%02d.go"}`, i)),
			toolResult(id, fmt.Sprintf("result-%02d-%s", i, strings.Repeat("x", 5_500))),
		)
		if i == 1 {
			steer := userText("also preserve the public API names")
			steer.Origin = llm.MessageOriginSteer
			transcript = append(transcript, steer)
		}
	}

	fp := llmtest.New("fake",
		summaryStep("FOUR EARLY TURNS COMPLETE", 60, 12),
		summaryStep("NEXT FOUR TURNS COMPLETE", 70, 14),
	)
	a := newAgent(fp, &tools.Registry{}, Options{Model: "local", ContextWindow: window, CompactKeepTurns: 8})
	a.SetSystem("sys")
	a.SetTranscript(transcript)
	if got := len(completedTurnSpans(a.Transcript())); got != 12 {
		t.Fatalf("completed turns = %d, want 12", got)
	}
	if before := a.estimateContext(nil).Total; before*100 < window*compactThresholdPct {
		t.Fatalf("test setup context = %d, want at least %d%% of %d", before, compactThresholdPct, window)
	}

	var archived CompactionArchive
	a.SetCompactionArchiver(func(_ context.Context, archive CompactionArchive) (string, error) {
		archived = archive
		return "compactions/0001.input.json", nil
	})
	sink := &recordSink{}
	usage, changed, err := a.MaybeCompact(context.Background(), window*compactThresholdPct/100, sink)
	if err != nil {
		t.Fatalf("MaybeCompact: %v", err)
	}
	if !changed {
		t.Fatal("long single-prompt transcript was not compacted")
	}
	if usage.InputTokens != 60 || usage.OutputTokens != 12 || len(fp.Requests) != 1 {
		t.Fatalf("summary usage/requests = %+v / %d, want 60/12 and one request", usage, len(fp.Requests))
	}
	if got := countCompletedTurns(archived.Messages); got != 4 {
		t.Fatalf("archived completed turns = %d, want 4", got)
	}

	got := a.Transcript()
	mustValid(t, got)
	if len(got) != 17 || got[0].Origin != llm.MessageOriginCompactionCheckpoint {
		t.Fatalf("compacted transcript should be checkpoint + 8 tool turns, got %d messages:\n%s", len(got), dump(got))
	}
	checkpoint := got[0].Content[0].Text
	for _, exact := range []string{"implement the complete request exactly", "also preserve the public API names", "FOUR EARLY TURNS COMPLETE", "compactions/0001.input.json"} {
		if !strings.Contains(checkpoint, exact) {
			t.Fatalf("checkpoint missing %q:\n%s", exact, checkpoint)
		}
	}
	for i := 0; i < 8; i++ {
		wantID := fmt.Sprintf("call_%02d", i+4)
		assistant := got[1+i*2]
		result := got[2+i*2]
		if assistant.Content[0].ToolUseID != wantID || result.Content[0].ResultForID != wantID {
			t.Fatalf("kept turn %d split or reordered: tool_use=%q tool_result=%q want %q", i+1, assistant.Content[0].ToolUseID, result.Content[0].ResultForID, wantID)
		}
	}
	after := a.estimateContext(nil).Total
	if after*100 > window*compactTargetPct {
		t.Fatalf("post-compaction context = %d, want at most %d%% of %d", after, compactTargetPct, window)
	}

	// A small next turn remains below the 78%% trigger, proving the 65%% target
	// provides hysteresis instead of causing per-turn compaction churn.
	withNext := append(a.Transcript(),
		asstToolUse("call_12", "read_file", `{"path":"small.go"}`),
		toolResult("call_12", "small follow-up complete"),
	)
	a.SetTranscript(withNext)
	nextSize := a.estimateContext(nil).Total
	if nextSize*100 >= window*compactThresholdPct {
		t.Fatalf("small next turn unexpectedly crossed trigger: %d", nextSize)
	}
	_, changed, err = a.MaybeCompact(context.Background(), nextSize, sink)
	if err != nil || changed || len(fp.Requests) != 1 {
		t.Fatalf("small next turn caused compaction churn: changed=%v err=%v requests=%d", changed, err, len(fp.Requests))
	}

	// Once enough new large turns accumulate, a second compaction carries the
	// original prompt and steer forward without recursively nesting the prior
	// checkpoint or treating it as a conversational turn.
	withMore := a.Transcript()
	for i := 13; i < 16; i++ {
		id := fmt.Sprintf("call_%02d", i)
		withMore = append(withMore,
			asstToolUse(id, "read_file", fmt.Sprintf(`{"path":"file_%02d.go"}`, i)),
			toolResult(id, fmt.Sprintf("result-%02d-%s", i, strings.Repeat("y", 6_000))),
		)
	}
	a.SetTranscript(withMore)
	secondSize := a.estimateContext(nil).Total
	if secondSize*100 < window*compactThresholdPct {
		t.Fatalf("second compaction setup remained below trigger: %d", secondSize)
	}
	_, changed, err = a.MaybeCompact(context.Background(), secondSize, sink)
	if err != nil || !changed || len(fp.Requests) != 2 {
		t.Fatalf("second compaction = changed=%v err=%v requests=%d", changed, err, len(fp.Requests))
	}
	secondCheckpoint := a.Transcript()[0].Content[0].Text
	for _, exact := range []string{"implement the complete request exactly", "also preserve the public API names", "NEXT FOUR TURNS COMPLETE"} {
		if !strings.Contains(secondCheckpoint, exact) {
			t.Fatalf("second checkpoint lost %q:\n%s", exact, secondCheckpoint)
		}
	}
	if strings.Count(secondCheckpoint, checkpointHeader) != 1 {
		t.Fatalf("second checkpoint nested the prior checkpoint header:\n%s", secondCheckpoint)
	}
	if after := a.estimateContext(nil).Total; after*100 > window*compactTargetPct {
		t.Fatalf("second post-compaction context = %d, want at most %d%% of %d", after, compactTargetPct, window)
	}
}

func TestCompactSummaryFailureKeepsTranscript(t *testing.T) {
	transcript := makeTurns(10)
	// A persistent failure: every retry attempt errors, so the summary call gives
	// up and the transcript is kept intact (r32 retries 1 + streamRetries times).
	errSteps := make([]llmtest.Step, streamRetries+1)
	for i := range errSteps {
		errSteps[i] = llmtest.Step{
			Events: []llm.StreamEvent{{Kind: llm.EventUsage, Usage: &llm.Usage{InputTokens: 7, OutputTokens: 1}}},
			Err:    errors.New("api down"),
		}
	}
	fp := llmtest.New("fake", errSteps...)
	a := newAgent(fp, tools.Default(), Options{Model: "claude-opus-4-8"})
	a.SetSleep(func(time.Duration) {})
	a.SetSystem("sys")
	a.SetTranscript(transcript)

	sink := &recordSink{}
	usage, err := a.Compact(context.Background(), sink)
	if err == nil {
		t.Fatalf("Compact should return the summary-call error")
	}
	if usage.InputTokens != 7*(streamRetries+1) || usage.OutputTokens != streamRetries+1 {
		t.Fatalf("failed summary usage = %+v, want every reported retry attempt", usage)
	}
	// Full transcript intact — a visible context-length failure beats data loss.
	if len(a.Transcript()) != len(transcript) {
		t.Errorf("failed compaction must keep the full transcript, got %d messages", len(a.Transcript()))
	}
	mustValid(t, a.Transcript())
	var warned bool
	for _, n := range sink.notices {
		if strings.Contains(strings.ToLower(n), "compact") {
			warned = true
		}
	}
	if !warned {
		t.Errorf("a summary-call failure should warn via the sink, notices=%v", sink.notices)
	}
}

func TestCompactDegradesToLastTurnWhenOversized(t *testing.T) {
	// The last four turns together exceed the budget but the last turn alone fits;
	// the ladder drops to the last turn. Budget is 78% of the 1M window ≈ 3.12M
	// bytes; four 1M-byte turns overflow, one does not.
	big := strings.Repeat("x", 1_000_000) // ~250k token estimate per kept turn
	var transcript []llm.Message
	transcript = append(transcript, makeTurns(4)...) // older turns, summarized
	for i := 0; i < 4; i++ {
		transcript = append(transcript, userText("Q"+turnLabel(i)), asstText(big))
	}

	fp := llmtest.New("fake", summaryStep("S", 10, 5))
	a := newAgent(fp, tools.Default(), Options{Model: "claude-opus-4-8"})
	a.SetSystem("sys")
	a.SetTranscript(transcript)

	if _, err := a.Compact(context.Background(), &recordSink{}); err != nil {
		t.Fatalf("Compact: %v", err)
	}
	msgs := a.Transcript()
	mustValid(t, msgs)
	// checkpoint + the single last assistant turn = 2 messages.
	if len(msgs) != 2 {
		t.Fatalf("oversized kept turns should drop to checkpoint + last turn, got %d:\n%s", len(msgs), dumpShort(msgs))
	}
	if msgs[1].Content[0].Text != big {
		t.Errorf("the single kept turn should be the most recent one")
	}
}

func TestCompactHardTruncatesWhenSingleTurnOversized(t *testing.T) {
	// Even the last turn alone is over budget: the ladder hard-truncates the
	// largest tool results in place, leaving a marker, and never wedges.
	huge := strings.Repeat("y", 5_000_000) // ~1.25M token estimate, over the window
	var transcript []llm.Message
	transcript = append(transcript, makeTurns(4)...)
	transcript = append(transcript,
		userText("final"),
		asstToolUse("c1", "big", `{}`),
		toolResult("c1", huge),
		asstText("done"),
	)

	fp := llmtest.New("fake", summaryStep("S", 10, 5))
	a := newAgent(fp, tools.Default(), Options{Model: "claude-opus-4-8"})
	a.SetSystem("sys")
	a.SetTranscript(transcript)

	if _, err := a.Compact(context.Background(), &recordSink{}); err != nil {
		t.Fatalf("Compact: %v", err)
	}
	msgs := a.Transcript()
	mustValid(t, msgs)

	var retainedHuge bool
	for _, m := range msgs {
		for _, b := range m.Content {
			if b.Kind == llm.BlockToolResult && len(b.ResultText) >= len(huge) {
				retainedHuge = true
			}
		}
	}
	if retainedHuge {
		t.Errorf("the oversized tool result should not remain in active context")
	}
}

func TestCompactUsageReported(t *testing.T) {
	transcript := makeTurns(10)
	fp := llmtest.New("fake", summaryStep("S", 9100, 400))
	a := newAgent(fp, tools.Default(), Options{Model: "claude-opus-4-8"})
	a.SetSystem("sys")
	a.SetTranscript(transcript)

	sink := &recordSink{}
	if _, err := a.Compact(context.Background(), sink); err != nil {
		t.Fatalf("Compact: %v", err)
	}

	// The compaction report names semantic turns and the context reduction.
	var report string
	for _, n := range sink.notices {
		if strings.Contains(n, "compacted") {
			report = n
		}
	}
	if report == "" {
		t.Fatalf("expected a [compacted: …] notice, got %v", sink.notices)
	}
	if !strings.Contains(report, "turns → checkpoint") || !strings.Contains(report, "ctx ~") {
		t.Errorf("compaction report should show semantic turns and context reduction, got %q", report)
	}
}

func TestCompactionReportShowsTurnsAndContextReduction(t *testing.T) {
	report := compactionReport(3, 9100, 400)
	if report != "[compacted: 3 turns → checkpoint · ctx ~9.1k → ~0.4k]" {
		t.Fatalf("compaction report = %q", report)
	}
}

func TestCompactArchivesRemovedMessages(t *testing.T) {
	transcript := makeTurns(10)
	fp := llmtest.New("fake", summaryStep("S", 100, 10))
	a := newAgent(fp, tools.Default(), Options{Model: "claude-opus-4-8"})
	a.SetTranscript(transcript)
	var archived CompactionArchive
	a.SetCompactionArchiver(func(ctx context.Context, archive CompactionArchive) (string, error) {
		archived = archive
		return "compactions/0001.input.json", nil
	})

	if _, err := a.Compact(context.Background(), &recordSink{}); err != nil {
		t.Fatalf("Compact: %v", err)
	}
	if len(archived.Messages) != 5 {
		t.Fatalf("archived %d messages, want two turns plus the next turn input", len(archived.Messages))
	}
	if archived.Summary != "S" {
		t.Fatalf("archived summary %q, want S", archived.Summary)
	}
	if !strings.Contains(a.Transcript()[0].Content[0].Text, "Raw compacted transcript archive: compactions/0001.input.json") {
		t.Fatalf("active summary missing archive reference: %q", a.Transcript()[0].Content[0].Text)
	}
}

func TestMaybeCompactReturnsUsageForTotals(t *testing.T) {
	// The summary call's tokens are returned so the caller can fold them into the
	// session totals (design §12, §6).
	transcript := makeTurns(10)
	fp := llmtest.New("fake", summaryStep("S", 5000, 100))
	a := newAgent(fp, tools.Default(), Options{Model: "claude-opus-4-8"})
	a.SetSystem("sys")
	a.SetTranscript(transcript)

	window := a.window()
	above := window * 80 / 100

	u, _, err := a.MaybeCompact(context.Background(), above, &recordSink{})
	if err != nil {
		t.Fatalf("MaybeCompact: %v", err)
	}
	if u.InputTokens != 5000 || u.OutputTokens != 100 {
		t.Errorf("returned compaction usage = %+v, want 5000 in / 100 out", u)
	}
}

func TestMaybeCompactBelowThresholdReturnsZeroUsage(t *testing.T) {
	transcript := makeTurns(4)
	fp := llmtest.New("fake")
	a := newAgent(fp, tools.Default(), Options{Model: "claude-opus-4-8"})
	a.SetSystem("sys")
	a.SetTranscript(transcript)

	below := a.window() / 2
	u, _, err := a.MaybeCompact(context.Background(), below, &recordSink{})
	if err != nil {
		t.Fatalf("MaybeCompact: %v", err)
	}
	if u != (llm.Usage{}) {
		t.Errorf("no compaction below threshold should return zero usage, got %+v", u)
	}
}

// TestContextWindowOverrideMovesTrigger is the regression test for the
// -context-window override never reaching compaction (design §6, §12). An
// unknown local model whose real window is far below the 256k registry default,
// run with a small override, must compact at 78% of the OVERRIDE, not 78% of
// 256k. Before the fix MaybeCompact read the registry default and
// never fired here, wedging the context.
func TestContextWindowOverrideMovesTrigger(t *testing.T) {
	const overrideWindow = 8000
	transcript := makeTurns(10)
	fp := llmtest.New("fake", summaryStep("S", 50, 10))
	a := newAgent(fp, tools.Default(), Options{
		Model:         "local-tiny-8k", // unknown model: registry default would be 256k
		ContextWindow: overrideWindow,
	})
	a.SetSystem("sys")
	a.SetTranscript(transcript)

	// 80% of the 8k override is ≥ 78% of the override but a tiny fraction of the
	// 256k registry default, so it only triggers when the override is honored.
	above := overrideWindow * 80 / 100
	if above*100 >= llm.NewRegistry(nil).ContextWindow("local-tiny-8k")*compactThresholdPct {
		t.Fatalf("test setup: %d should be below the default trigger", above)
	}

	if _, _, err := a.MaybeCompact(context.Background(), above, &recordSink{}); err != nil {
		t.Fatalf("MaybeCompact: %v", err)
	}
	if len(fp.Requests) != 1 {
		t.Fatalf("override window should have triggered compaction (1 summary call), got %d", len(fp.Requests))
	}
	if got := len(a.Transcript()); got != 16 {
		t.Fatalf("override-triggered compaction should collapse to checkpoint + 8 turns, got %d", got)
	}
}

// TestContextWindowOverrideMovesDegradeBudget pins the degradation budget to the
// override too: the same -context-window value that sizes the trigger must size
// the ladder. With a small override the ladder drops to the last turn and
// hard-truncates the big tool result; with the 256k default the same transcript
// sails under budget and is left fully intact. Comparing the two proves degrade
// reads the override, not the registry default (design §12 "never wedge").
func TestContextWindowOverrideMovesDegradeBudget(t *testing.T) {
	big := strings.Repeat("x", 20_000) // ~5000 estimated tokens in one result
	build := func() []llm.Message {
		var transcript []llm.Message
		for i := 0; i < 6; i++ {
			transcript = append(transcript,
				userText(turnLabel(i)+" q"),
				asstToolUse("t"+turnLabel(i), "read_file", `{}`),
				toolResult("t"+turnLabel(i), big),
				asstText(turnLabel(i)+" done"),
			)
		}
		return transcript
	}

	// With the override the ladder must shrink past rung 1 (keep last 8 turns) all
	// the way to a single truncated turn.
	const overrideWindow = 4000
	ov := newAgent(llmtest.New("fake", summaryStep("S", 50, 10)), tools.Default(), Options{
		Model:         "local-tiny",
		ContextWindow: overrideWindow,
	})
	ov.SetSystem("sys")
	ov.SetTranscript(build())
	if _, err := ov.Compact(context.Background(), &recordSink{}); err != nil {
		t.Fatalf("Compact (override): %v", err)
	}

	// Same transcript, no override: the 256k default budget leaves all 4 kept
	// turns verbatim (summary + 16 messages), no truncation.
	def := newAgent(llmtest.New("fake", summaryStep("S", 50, 10)), tools.Default(), Options{
		Model: "local-tiny",
	})
	def.SetSystem("sys")
	def.SetTranscript(build())
	if _, err := def.Compact(context.Background(), &recordSink{}); err != nil {
		t.Fatalf("Compact (default): %v", err)
	}

	ovEst := estimateTokens(ov.Transcript())
	defEst := estimateTokens(def.Transcript())
	if ovEst >= defEst {
		t.Fatalf("override budget should shrink further than the default: override est %d, default est %d", ovEst, defEst)
	}
	// Sanity: the override result must be near its own budget, the default must
	// keep all four turns verbatim (no truncation under 256k).
	if len(def.Transcript()) != 16 {
		t.Fatalf("default budget should keep the last 8 turns, got %d", len(def.Transcript()))
	}
	if budget := overrideWindow * compactThresholdPct / 100; ovEst > budget+minTruncResult/bytesPerToken {
		t.Fatalf("override degrade left estimate %d well above budget %d", ovEst, budget)
	}
}

// A single in-progress turn whose read-only tool result has ballooned over
// budget has nothing older than the kept turns to summarize, but it must still
// be summarized and archived rather than wedged against the context window.
func TestCompactSummarizesOversizedEarlierTurn(t *testing.T) {
	const window = 10_000 // budget = 7800 tokens ≈ 31.2k bytes
	huge := strings.Repeat("x", 200_000)
	transcript := []llm.Message{
		userText("inspect the file"),
		asstToolUse("c1", "read_file", `{"path":"a.go"}`),
		toolResult("c1", huge),
		asstText("looked"),
	}
	fp := llmtest.New("fake", summaryStep("LOOKED SUMMARY", 20, 4))
	a := newAgent(fp, tools.Default(), Options{Model: "local", ContextWindow: window})
	a.SetSystem("sys")
	a.SetTranscript(transcript)

	if estimateTokens(a.Transcript()) <= a.compactBudget() {
		t.Fatal("test setup: transcript should start over budget")
	}
	if len(turnStarts(a.Transcript())) > a.keepTurns() {
		t.Fatal("test setup: transcript should have <= keepTurns turns")
	}

	above := window * 80 / 100
	sink := &recordSink{}
	usage, changed, err := a.MaybeCompact(context.Background(), above, sink)
	if err != nil {
		t.Fatalf("MaybeCompact: %v", err)
	}
	if !changed {
		t.Fatal("an oversized single-turn transcript should be shrunk (changed=true), not a silent no-op")
	}
	if usage.InputTokens != 20 || usage.OutputTokens != 4 {
		t.Errorf("summary usage = %+v", usage)
	}
	if len(fp.Requests) != 1 {
		t.Errorf("expected one summary call, got %d requests", len(fp.Requests))
	}
	if est := estimateTokens(a.Transcript()); est > a.compactBudget() {
		t.Errorf("shrunk transcript still over budget: est %d > budget %d", est, a.compactBudget())
	}
	mustValid(t, a.Transcript())
	var noticed bool
	for _, n := range sink.notices {
		if strings.Contains(n, "compacted") {
			noticed = true
		}
	}
	if !noticed {
		t.Errorf("degrade-only compaction should emit a visible notice, got %v", sink.notices)
	}
}

func TestCompactUnderKeepTurnsSummarizesOlderTurnsWhenOverBudget(t *testing.T) {
	const window = 1000
	currentResult := strings.Repeat("x", 8000)
	transcript := []llm.Message{
		userText("older question"),
		asstText("older answer"),
		userText("current question"),
		asstToolUse("c1", "read_file", `{"path":"a.go"}`),
		toolResult("c1", currentResult),
	}
	fp := llmtest.New("fake", summaryStep("OLDER SUMMARY", 40, 8))
	a := newAgent(fp, tools.Default(), Options{Model: "local", ContextWindow: window})
	a.SetTranscript(transcript)
	if len(turnStarts(a.Transcript())) >= a.keepTurns() {
		t.Fatal("test setup should have fewer turns than the normal keep window")
	}

	sink := &recordSink{}
	usage, changed, err := a.MaybeCompact(context.Background(), window*80/100, sink)
	if err != nil {
		t.Fatalf("MaybeCompact: %v", err)
	}
	if !changed {
		t.Fatal("over-budget transcript with an older turn should summarize it")
	}
	if usage.InputTokens != 40 || usage.OutputTokens != 8 {
		t.Fatalf("summary usage = %+v, want 40 in / 8 out", usage)
	}
	if len(fp.Requests) != 1 {
		t.Fatalf("summary requests = %d, want 1", len(fp.Requests))
	}
	if len(fp.Requests[0].Messages) != 4 {
		t.Fatalf("summary input messages = %d, want metadata plus older turn and current input", len(fp.Requests[0].Messages))
	}
	got := a.Transcript()
	mustValid(t, got)
	if len(got) != 3 {
		t.Fatalf("compacted transcript len = %d, want checkpoint + current tool turn", len(got))
	}
	if !strings.HasPrefix(got[0].Content[0].Text, checkpointHeader) || !strings.Contains(got[0].Content[0].Text, "OLDER SUMMARY") {
		t.Fatalf("first message is not the compaction checkpoint: %#v", got[0].Content[0])
	}
	if !strings.Contains(got[0].Content[0].Text, "current question") || got[1].Role != llm.RoleAssistant {
		t.Fatalf("current input/turn was not preserved by checkpoint: %#v", got)
	}
}

func TestCompactCurrentNoShrinkNoticeThrottled(t *testing.T) {
	const window = 1000
	a := newAgent(llmtest.New("fake"), tools.Default(), Options{Model: "local", ContextWindow: window})
	a.SetTranscript([]llm.Message{
		userText("single turn"),
		asstText(strings.Repeat("x", 20_000)),
	})
	sink := &recordSink{}

	for i := 0; i < 3; i++ {
		_, changed, err := a.MaybeCompact(context.Background(), window*80/100, sink)
		if err != nil {
			t.Fatalf("MaybeCompact %d: %v", i, err)
		}
		if changed {
			t.Fatalf("MaybeCompact %d changed an unshrinkable transcript", i)
		}
	}
	if got := countNoticesContaining(sink.notices, "nothing left to shrink"); got != 1 {
		t.Fatalf("no-shrink notices = %d, want 1; notices=%v", got, sink.notices)
	}
}

func TestCompactCurrentTinyShrinkNoticeThrottled(t *testing.T) {
	a := newAgent(llmtest.New("fake"), tools.Default(), Options{Model: "local"})
	sink := &recordSink{}

	a.noticeCurrentShrink(sink, "auto", 10_000, 9_500)
	a.noticeCurrentShrink(sink, "auto", 11_000, 10_500)
	a.noticeCurrentShrink(sink, "auto", 20_000, 18_000)
	if got := countNoticesContaining(sink.notices, "archived oversized turn payload"); got != 2 {
		t.Fatalf("shrink notices = %d, want first tiny + material; notices=%v", got, sink.notices)
	}
}

// A transcript with <= keepTurns turns that already fits the budget is a genuine
// no-op: compact must report changed=false so the mid-loop caller does not churn
// its trigger state (reset lastInput/appendBoundary and re-estimate the whole
// transcript) every turn.
func TestCompactNoOpReportsUnchanged(t *testing.T) {
	a := newAgent(llmtest.New("fake"), tools.Default(), Options{Model: "claude-opus-4-8"})
	a.SetSystem("sys")
	a.SetTranscript(makeTurns(2)) // 2 turns <= keepTurns(8), tiny, well under budget

	before := len(a.Transcript())
	sink := &recordSink{}
	usage, changed, err := a.compact(context.Background(), sink, "auto")
	if err != nil {
		t.Fatalf("compact: %v", err)
	}
	if changed {
		t.Error("a no-op compaction must report changed=false")
	}
	if usage != (llm.Usage{}) {
		t.Errorf("no-op compaction usage = %+v, want zero", usage)
	}
	if len(a.Transcript()) != before {
		t.Errorf("no-op compaction must not rewrite the transcript: %d != %d", len(a.Transcript()), before)
	}
	if len(sink.notices) != 0 {
		t.Errorf("no-op compaction should emit no notice, got %v", sink.notices)
	}
}

func countNoticesContaining(notices []string, needle string) int {
	var count int
	for _, notice := range notices {
		if strings.Contains(notice, needle) {
			count++
		}
	}
	return count
}

// degrade/trim mutate on a deep copy, so when ValidateTranscript fails after the
// shrink, the live transcript is left fully intact — the documented rollback
// guarantee. The transcript is oversized but already INVALID (a trailing
// assistant tool_use with no result); the post-shrink validation must fail and
// the live transcript's huge tool input must be untouched.
func TestCompactValidationFailureLeavesTranscriptIntact(t *testing.T) {
	const window = 1000
	hugeInput := `{"path":"` + strings.Repeat("x", 200_000) + `"}`
	transcript := []llm.Message{
		userText("go"),
		asstToolUse("c1", "read_file", hugeInput), // no tool_result follows -> invalid
	}
	if err := llm.ValidateTranscript(transcript); err == nil {
		t.Fatal("test setup: transcript should be invalid (dangling tool_use)")
	}
	a := newAgent(llmtest.New("fake"), tools.Default(), Options{Model: "local", ContextWindow: window})
	a.SetSystem("sys")
	a.SetTranscript(transcript)
	if estimateTokens(a.Transcript()) <= a.compactBudget() {
		t.Fatal("test setup: transcript should start over budget")
	}

	origInputLen := len(a.Transcript()[1].Content[0].ToolInput)
	above := window * 80 / 100
	_, changed, err := a.MaybeCompact(context.Background(), above, &recordSink{})
	if err == nil {
		t.Fatal("compact should surface the post-shrink validation failure")
	}
	if changed {
		t.Error("a failed shrink must report changed=false")
	}
	if got := len(a.Transcript()[1].Content[0].ToolInput); got != origInputLen {
		t.Errorf("live transcript tool input mutated despite validation failure: len %d, want %d", got, origInputLen)
	}
}

func TestTruncateLargestBlockShrinksToolInput(t *testing.T) {
	largeInput := `{"payload":"` + strings.Repeat("x", 2000) + `"}`
	msgs := []llm.Message{{
		Role: llm.RoleAssistant,
		Content: []llm.ContentBlock{{
			Kind:      llm.BlockToolUse,
			ToolUseID: "call_big",
			ToolName:  "run_command",
			ToolInput: json.RawMessage(largeInput),
		}},
	}}
	before := len(msgs[0].Content[0].ToolInput)

	if !truncateLargestBlock(msgs, 1000) {
		t.Fatal("truncateLargestBlock returned false")
	}
	after := len(msgs[0].Content[0].ToolInput)
	if after >= before {
		t.Fatalf("tool input was not shrunk: before %d after %d", before, after)
	}
	if !strings.Contains(string(msgs[0].Content[0].ToolInput), "_truncated") {
		t.Fatalf("tool input missing truncation marker: %s", msgs[0].Content[0].ToolInput)
	}
}

func TestTruncateLargestBlockReplacesImage(t *testing.T) {
	msgs := []llm.Message{{
		Role: llm.RoleUser,
		Content: []llm.ContentBlock{{
			Kind:           llm.BlockImage,
			ImageName:      "screenshot.png",
			ImageMediaType: "image/png",
			ImageData:      strings.Repeat("x", 2000),
		}},
	}}

	if !truncateLargestBlock(msgs, 1000) {
		t.Fatal("truncateLargestBlock returned false")
	}
	block := msgs[0].Content[0]
	if block.Kind != llm.BlockText || !strings.Contains(block.Text, "image omitted from compaction summary") {
		t.Fatalf("image was not replaced with text placeholder: %+v", block)
	}
}

func TestPrepareSummaryMessagesDegradesRichResultImagesOnDeepCopy(t *testing.T) {
	const imageData = "c2Vuc2l0aXZlLWltYWdlLWRhdGE="
	msgs := []llm.Message{{Role: llm.RoleUser, Content: []llm.ContentBlock{{
		Kind:        llm.BlockToolResult,
		ResultForID: "call",
		ResultText:  "attached",
		ResultContent: []llm.ContentBlock{{
			Kind:           llm.BlockImage,
			ImageName:      "capture.png",
			ImageMediaType: "image/png",
			ImageData:      imageData,
			ImageDetail:    "high",
			ImageWidth:     320,
			ImageHeight:    200,
		}},
	}}}}

	got := prepareSummaryMessages(msgs, 8)
	result := got[0].Content[0]
	if len(result.ResultContent) != 0 {
		t.Fatalf("summary input retained nested image: %+v", result.ResultContent)
	}
	for _, want := range []string{"image omitted", "capture.png", "image/png", "detail high", "320x200"} {
		if !strings.Contains(result.ResultText, want) {
			t.Errorf("summary placeholder missing %q: %q", want, result.ResultText)
		}
	}
	if strings.Contains(result.ResultText, imageData) {
		t.Fatalf("summary placeholder leaked base64: %q", result.ResultText)
	}
	if len(msgs[0].Content[0].ResultContent) != 1 || msgs[0].Content[0].ResultContent[0].ImageData != imageData {
		t.Fatalf("summary preparation mutated source: %+v", msgs[0].Content[0])
	}
}

func TestTruncateLargestBlockReplacesRichResultImage(t *testing.T) {
	const imageData = "c2Vuc2l0aXZlLWltYWdlLWRhdGE="
	msgs := []llm.Message{{Role: llm.RoleUser, Content: []llm.ContentBlock{{
		Kind:        llm.BlockToolResult,
		ResultForID: "call",
		ResultText:  "attached",
		ResultContent: []llm.ContentBlock{{
			Kind:           llm.BlockImage,
			ImageMediaType: "image/jpeg",
			ImageData:      imageData,
			ImageWidth:     800,
			ImageHeight:    600,
		}},
	}}}}

	if !truncateLargestBlock(msgs, 1000) {
		t.Fatal("truncateLargestBlock returned false")
	}
	result := msgs[0].Content[0]
	if len(result.ResultContent) != 0 || !strings.Contains(result.ResultText, "800x600") {
		t.Fatalf("nested image was not replaced by a parent description: %+v", result)
	}
	if strings.Contains(result.ResultText, imageData) {
		t.Fatalf("degraded result leaked base64: %q", result.ResultText)
	}
}

func TestCloneMessagesDeepCopiesRichResultContent(t *testing.T) {
	msgs := []llm.Message{{Role: llm.RoleUser, Content: []llm.ContentBlock{{
		Kind:          llm.BlockToolResult,
		ResultForID:   "call",
		ResultContent: []llm.ContentBlock{{Kind: llm.BlockImage, ImageData: "original"}},
	}}}}
	cloned := cloneMessages(msgs)
	cloned[0].Content[0].ResultContent[0].ImageData = "changed"
	cloned[0].Content[0].ResultContent = append(cloned[0].Content[0].ResultContent, llm.ContentBlock{Kind: llm.BlockImage})

	if got := msgs[0].Content[0].ResultContent; len(got) != 1 || got[0].ImageData != "original" {
		t.Fatalf("clone shared nested result content with source: %+v", got)
	}
}

func TestAgentEstimatesCountRichResultImagesAtFlatWeight(t *testing.T) {
	plain := []llm.Message{{Role: llm.RoleUser, Content: []llm.ContentBlock{{Kind: llm.BlockToolResult, ResultForID: "call", ResultText: "attached"}}}}
	withImage := cloneMessages(plain)
	withImage[0].Content[0].ResultContent = []llm.ContentBlock{{
		Kind:           llm.BlockImage,
		ImageMediaType: "image/png",
		ImageDetail:    "high",
		ImageName:      "capture.png",
		ImageData:      "YWJj",
	}}
	withHugeImage := cloneMessages(withImage)
	withHugeImage[0].Content[0].ResultContent[0].ImageData = strings.Repeat("A", 1<<20)

	if delta := estimateTokens(withImage) - estimateTokens(plain); delta < imageTokenEstimate {
		t.Fatalf("nested image token delta = %d, want at least flat weight %d", delta, imageTokenEstimate)
	}
	if small, huge := estimateTokens(withImage), estimateTokens(withHugeImage); small != huge {
		t.Fatalf("base64 length changed flat image estimate: small=%d huge=%d", small, huge)
	}
	requestSmall := estimateRequest(llm.Request{Messages: withImage}, 10_000).Messages
	requestHuge := estimateRequest(llm.Request{Messages: withHugeImage}, 10_000).Messages
	requestPlain := estimateRequest(llm.Request{Messages: plain}, 10_000).Messages
	if requestSmall-requestPlain < imageTokenEstimate || requestSmall != requestHuge {
		t.Fatalf("request estimates plain=%d small=%d huge=%d", requestPlain, requestSmall, requestHuge)
	}
}

func TestSummarizeUsageSurvivesZeroedDoneFrame(t *testing.T) {
	fp := llmtest.New("fake", llmtest.Step{
		Events: []llm.StreamEvent{
			{Kind: llm.EventUsage, Usage: &llm.Usage{InputTokens: 55, OutputTokens: 5}},
			textDelta("summary"),
		},
		Stop: llm.StopEndTurn,
	})
	a := newAgent(fp, tools.Default(), Options{})
	_, usage, err := a.summarize(context.Background(), prompts.CompactionSummary(), []llm.Message{
		{Role: llm.RoleUser, Content: []llm.ContentBlock{{Kind: llm.BlockText, Text: "old"}}},
	}, llm.RequestPurposeCompaction)
	if err != nil {
		t.Fatalf("summarize: %v", err)
	}
	if usage.InputTokens != 55 || usage.OutputTokens != 5 {
		t.Errorf("usage = %+v, want 55 in / 5 out preserved", usage)
	}
}

func dumpShort(msgs []llm.Message) string {
	var b strings.Builder
	for i, m := range msgs {
		b.WriteString(string(rune('0'+i%10)) + ":" + string(m.Role) + " ")
	}
	return b.String()
}

// seedTurns returns n complete small turns so compaction has history to fold.
// The 1-byte "q"/"a" bodies are deliberate: TestProactiveCompactionMidTurn does
// byte-estimate threshold math against them.
func seedTurns(n int) []llm.Message {
	var msgs []llm.Message
	for range n {
		msgs = append(msgs, userText("q"), asstText("a"))
	}
	return msgs
}

func TestProactiveCompactionMidTurn(t *testing.T) {
	// Window 1000 tokens -> trigger at 780 tokens (3120 bytes estimated).
	// The tool result is 8000 bytes, so the estimate crosses the threshold
	// before step 2's request is built.
	big := strings.Repeat("x", 8000)
	tool := &recordTool{name: "blob", run: func(_ context.Context, _ json.RawMessage) (string, error) {
		return big, nil
	}}
	reg := &tools.Registry{}
	reg.Register(tool)

	fp := llmtest.New("fake",
		llmtest.Step{ // step 1: ask for the ballooning tool
			Events: []llm.StreamEvent{toolDone(0, "c1", "blob", `{}`)},
			Stop:   llm.StopToolUse,
			Usage:  llm.Usage{InputTokens: 10, OutputTokens: 2},
		},
		llmtest.Step{ // the mid-prompt maintenance summary call
			Events: []llm.StreamEvent{textDelta("the summary")},
			Stop:   llm.StopEndTurn,
			Usage:  llm.Usage{InputTokens: 50, OutputTokens: 5},
		},
		llmtest.Step{ // step 2 proper, against the compacted transcript
			Events: []llm.StreamEvent{textDelta("done")},
			Stop:   llm.StopEndTurn,
			Usage:  llm.Usage{InputTokens: 20, OutputTokens: 3},
		},
	)
	a := newAgent(fp, reg, Options{ContextWindow: 1000})
	a.SetTranscript(seedTurns(5))
	sink := &recordSink{}

	if err := a.RunPrompt(context.Background(), "go", sink); err != nil {
		t.Fatalf("RunPrompt: %v", err)
	}
	mustValid(t, a.Transcript())

	if len(fp.Requests) != 3 {
		t.Fatalf("provider called %d times, want 3 (step, summary, step)", len(fp.Requests))
	}
	// The post-compaction request starts with the user checkpoint.
	first := fp.Requests[2].Messages[0]
	if first.Role != llm.RoleUser {
		t.Fatalf("post-compaction checkpoint role = %q, want user", first.Role)
	}
	if !strings.HasPrefix(first.Content[0].Text, checkpointHeader) {
		t.Errorf("post-compaction request should start with the checkpoint, got %q", first.Content[0].Text)
	}
	var compacted bool
	for _, n := range sink.notices {
		if strings.Contains(n, "compacted:") {
			compacted = true
		}
	}
	if !compacted {
		t.Errorf("no compaction notice, notices=%v", sink.notices)
	}
	// Summary-call usage folds into the prompt total (10+50+20 inputs).
	if got := sink.promptUsage[0].Usage.InputTokens; got != 80 {
		t.Errorf("prompt input tokens = %d, want 80", got)
	}
}

func TestNoMidTurnCompactionUnderThreshold(t *testing.T) {
	tool := &recordTool{name: "small", run: func(_ context.Context, _ json.RawMessage) (string, error) {
		return "tiny", nil
	}}
	reg := &tools.Registry{}
	reg.Register(tool)

	fp := llmtest.New("fake",
		llmtest.Step{
			Events: []llm.StreamEvent{toolDone(0, "c1", "small", `{}`)},
			Stop:   llm.StopToolUse,
		},
		llmtest.Step{Events: []llm.StreamEvent{textDelta("done")}, Stop: llm.StopEndTurn},
	)
	a := newAgent(fp, reg, Options{ContextWindow: 1_000_000})
	a.SetTranscript(seedTurns(5))
	sink := &recordSink{}

	if err := a.RunPrompt(context.Background(), "go", sink); err != nil {
		t.Fatalf("RunPrompt: %v", err)
	}
	if len(fp.Requests) != 2 {
		t.Errorf("provider called %d times, want 2 (no summary call)", len(fp.Requests))
	}
	for _, n := range sink.notices {
		if strings.Contains(n, "compacted:") {
			t.Errorf("unexpected compaction: %v", sink.notices)
		}
	}
}

func TestPostTurnCompactionUsesFreshTranscriptEstimate(t *testing.T) {
	// Window 1000 tokens -> trigger at 780 tokens. The final assistant message is
	// large enough to cross the threshold even though the provider-reported input
	// count for that turn is tiny.
	final := strings.Repeat("z", 8000)
	fp := llmtest.New("fake",
		llmtest.Step{
			Events: []llm.StreamEvent{textDelta(final)},
			Stop:   llm.StopEndTurn,
			Usage:  llm.Usage{InputTokens: 10, OutputTokens: 2},
		},
		llmtest.Step{
			Events: []llm.StreamEvent{textDelta("summary after final")},
			Stop:   llm.StopEndTurn,
			Usage:  llm.Usage{InputTokens: 50, OutputTokens: 5},
		},
	)
	a := newAgent(fp, tools.Default(), Options{ContextWindow: 1000})
	a.SetTranscript(seedTurns(5))
	sink := &recordSink{}

	if err := a.RunPrompt(context.Background(), "go", sink); err != nil {
		t.Fatalf("RunPrompt: %v", err)
	}
	mustValid(t, a.Transcript())
	if len(fp.Requests) != 2 {
		t.Fatalf("provider called %d times, want 2 (final response + summary)", len(fp.Requests))
	}
	var compacted bool
	for _, n := range sink.notices {
		if strings.Contains(n, "compacted:") {
			compacted = true
		}
	}
	if !compacted {
		t.Fatalf("post-turn compaction did not run, notices=%v", sink.notices)
	}
	if got := sink.promptUsage[0].Usage.InputTokens; got != 60 {
		t.Errorf("prompt input tokens = %d, want 60 including summary call", got)
	}
}
