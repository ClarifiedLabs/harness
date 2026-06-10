package agent

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"harness/internal/llm"
	"harness/internal/llm/llmtest"
	"harness/internal/tools"
	"harness/prompts"
)

// userText is a genuine user turn-start message (text, not tool results).
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

func TestCompactKeepsLastFourTurns(t *testing.T) {
	// Ten whole turns; compaction keeps the system prompt plus the last four
	// verbatim and collapses the older six into one summary message.
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

	// One summary message + last four turns (8 messages).
	if len(msgs) != 1+8 {
		t.Fatalf("want 1 summary + 8 kept messages, got %d:\n%s", len(msgs), dump(msgs))
	}
	if msgs[0].Role != llm.RoleUser {
		t.Errorf("summary should be a user message, got role %q", msgs[0].Role)
	}
	got := msgs[0].Content[0].Text
	if !strings.HasPrefix(got, "=== Summary of earlier conversation ===\n") {
		t.Errorf("summary message missing header, got %q", got)
	}
	if !strings.Contains(got, "CANNED SUMMARY") {
		t.Errorf("summary message should carry the model's summary, got %q", got)
	}

	// The kept turns are the last four (G..J), verbatim and whole.
	if msgs[1].Content[0].Text != "G question" {
		t.Errorf("first kept turn = %q, want %q", msgs[1].Content[0].Text, "G question")
	}
	if msgs[8].Content[0].Text != "J answer" {
		t.Errorf("last kept message = %q, want %q", msgs[8].Content[0].Text, "J answer")
	}

	// The summary request received the older turns but never the system prompt
	// as a message (it lives on Request.System).
	if len(fp.Requests) != 1 {
		t.Fatalf("summary call count = %d, want 1", len(fp.Requests))
	}
	if fp.Requests[0].System != prompts.CompactionSummary() {
		t.Fatalf("summary call system prompt = %q, want embedded compaction prompt", fp.Requests[0].System)
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
	if _, err := a.MaybeCompact(context.Background(), below, sink); err != nil {
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

	if _, err := a.MaybeCompact(context.Background(), above, &recordSink{}); err != nil {
		t.Fatalf("MaybeCompact: %v", err)
	}
	msgs := a.Transcript()
	mustValid(t, msgs)
	if len(msgs) != 1+8 {
		t.Fatalf("above threshold should compact to summary + 8, got %d", len(msgs))
	}
	if len(fp.Requests) != 1 {
		t.Errorf("summary call count = %d, want 1", len(fp.Requests))
	}
}

func TestCompactSummaryFailureKeepsTranscript(t *testing.T) {
	transcript := makeTurns(10)
	fp := llmtest.New("fake", llmtest.Step{Err: errors.New("api down")})
	a := newAgent(fp, tools.Default(), Options{Model: "claude-opus-4-8"})
	a.SetSystem("sys")
	a.SetTranscript(transcript)

	sink := &recordSink{}
	_, err := a.Compact(context.Background(), sink)
	if err == nil {
		t.Fatalf("Compact should return the summary-call error")
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
	// summary + the single last turn (user + assistant) = 3 messages.
	if len(msgs) != 1+2 {
		t.Fatalf("oversized kept turns should drop to the last turn (summary + 2), got %d:\n%s", len(msgs), dumpShort(msgs))
	}
	if msgs[2].Content[0].Text != big {
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

	var truncated bool
	for _, m := range msgs {
		for _, b := range m.Content {
			if b.Kind == llm.BlockToolResult && len(b.ResultText) < len(huge) && strings.Contains(b.ResultText, "truncated") {
				truncated = true
			}
		}
	}
	if !truncated {
		t.Errorf("the largest tool result should be hard-truncated with a marker")
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

	// The compaction report names the message collapse and the summary-call usage.
	var report string
	for _, n := range sink.notices {
		if strings.Contains(n, "compacted") {
			report = n
		}
	}
	if report == "" {
		t.Fatalf("expected a [compacted: …] notice, got %v", sink.notices)
	}
	if !strings.Contains(report, "9.1k in") || !strings.Contains(report, "0.4k out") {
		t.Errorf("compaction report should show summary-call usage, got %q", report)
	}
	if !strings.Contains(report, "summary") {
		t.Errorf("compaction report should mention the summary, got %q", report)
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
	if len(archived.Messages) != 12 {
		t.Fatalf("archived %d messages, want older six turns (12 messages)", len(archived.Messages))
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

	u, err := a.MaybeCompact(context.Background(), above, &recordSink{})
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
	u, err := a.MaybeCompact(context.Background(), below, &recordSink{})
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

	if _, err := a.MaybeCompact(context.Background(), above, &recordSink{}); err != nil {
		t.Fatalf("MaybeCompact: %v", err)
	}
	if len(fp.Requests) != 1 {
		t.Fatalf("override window should have triggered compaction (1 summary call), got %d", len(fp.Requests))
	}
	if got := len(a.Transcript()); got != 1+8 {
		t.Fatalf("override-triggered compaction should collapse to summary + 8, got %d", got)
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

	// With the override the ladder must shrink past rung 1 (keep last 4 turns) all
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
	if len(def.Transcript()) != 1+16 {
		t.Fatalf("default budget should keep last 4 turns verbatim (summary + 16), got %d", len(def.Transcript()))
	}
	if budget := overrideWindow * compactThresholdPct / 100; ovEst > budget+minTruncResult/bytesPerToken {
		t.Fatalf("override degrade left estimate %d well above budget %d", ovEst, budget)
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

func TestSummarizeUsageSurvivesZeroedDoneFrame(t *testing.T) {
	fp := llmtest.New("fake", llmtest.Step{
		Events: []llm.StreamEvent{
			{Kind: llm.EventUsage, Usage: &llm.Usage{InputTokens: 55, OutputTokens: 5}},
			textDelta("summary"),
		},
		Stop: llm.StopEndTurn,
	})
	a := newAgent(fp, tools.Default(), Options{})
	_, usage, err := a.summarize(context.Background(), []llm.Message{
		{Role: llm.RoleUser, Content: []llm.ContentBlock{{Kind: llm.BlockText, Text: "old"}}},
	})
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
		llmtest.Step{ // the mid-turn summary call
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

	if err := a.RunTurn(context.Background(), "go", sink); err != nil {
		t.Fatalf("RunTurn: %v", err)
	}
	mustValid(t, a.Transcript())

	if len(fp.Requests) != 3 {
		t.Fatalf("provider called %d times, want 3 (step, summary, step)", len(fp.Requests))
	}
	// The post-compaction request starts with the summary message.
	first := fp.Requests[2].Messages[0]
	if !strings.HasPrefix(first.Content[0].Text, summaryHeader) {
		t.Errorf("post-compaction request should start with the summary, got %q", first.Content[0].Text)
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
	// Summary-call usage folds into the turn total (10+50+20 inputs).
	if got := sink.turnUsage[0].Usage.InputTokens; got != 80 {
		t.Errorf("turn input tokens = %d, want 80", got)
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

	if err := a.RunTurn(context.Background(), "go", sink); err != nil {
		t.Fatalf("RunTurn: %v", err)
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
	// count for that model turn is tiny.
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

	if err := a.RunTurn(context.Background(), "go", sink); err != nil {
		t.Fatalf("RunTurn: %v", err)
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
	if got := sink.turnUsage[0].Usage.InputTokens; got != 60 {
		t.Errorf("turn input tokens = %d, want 60 including summary call", got)
	}
}
