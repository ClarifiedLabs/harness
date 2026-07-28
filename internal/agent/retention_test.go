package agent

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"harness/internal/llm"
	"harness/internal/llm/llmtest"
	"harness/internal/tools"
)

func readOnlyRegistry() *tools.Registry {
	noop := func(context.Context, json.RawMessage) (string, error) { return "", nil }
	reg := &tools.Registry{}
	reg.Register(&recordTool{name: "rd", readOnly: true, run: noop})
	reg.Register(&recordTool{name: "wr", readOnly: false, run: noop})
	return reg
}

func TestRetentionBytesIncludeNestedPlaintextThinking(t *testing.T) {
	const thinking = "private chain of thought"
	messages := []llm.Message{{
		Role: llm.RoleUser,
		Content: []llm.ContentBlock{{
			Kind: llm.BlockToolResult,
			ResultContent: []llm.ContentBlock{{
				Kind:     llm.BlockThinking,
				Thinking: thinking,
			}},
		}},
	}}
	if got := retentionTranscriptBytes(messages); got < len(thinking) {
		t.Fatalf("retention bytes = %d, want at least %d nested thinking bytes", got, len(thinking))
	}
}

func userImage(name, data, text string) llm.Message {
	return llm.Message{Role: llm.RoleUser, Content: []llm.ContentBlock{
		{Kind: llm.BlockImage, ImageName: name, ImageMediaType: "image/png", ImageData: data},
		{Kind: llm.BlockText, Text: text},
	}}
}

// TestRetentionTrimsOldReadOnlyResults verifies the live retention pass shrinks
// a large read-only tool result older than keepTurns while leaving a recent one
// of the same size untouched.
func TestRetentionTrimsOldReadOnlyResults(t *testing.T) {
	big := strings.Repeat("x", 9000)
	var msgs []llm.Message
	msgs = append(msgs, userText("q0"), asstToolUse("t0", "rd", `{}`), toolResult("t0", big), asstText("a0"))
	for i := 1; i <= 8; i++ {
		msgs = append(msgs, userText(fmt.Sprintf("q%d", i)), asstText(fmt.Sprintf("a%d", i)))
	}
	msgs = append(msgs, userText("qR"), asstToolUse("tR", "rd", `{}`), toolResult("tR", big), asstText("aR"))

	a := newAgent(llmtest.New("fake"), readOnlyRegistry(), Options{RetentionPolicy: RetentionPolicyAge})
	a.SetTranscript(msgs)
	a.applyRetention(&recordSink{})
	mustValid(t, a.Transcript())

	old := a.Transcript()[2].Content[0].ResultText
	if len(old) >= len(big) || !strings.Contains(old, retentionTrimMarker) {
		t.Errorf("old read-only result not trimmed: len=%d marker=%v", len(old), strings.Contains(old, retentionTrimMarker))
	}
	recent := a.Transcript()[22].Content[0].ResultText
	if recent != big {
		t.Errorf("recent result should be untouched, len=%d want %d", len(recent), len(big))
	}
}

// TestRetentionKeepsMutatingResults verifies a large result from a non-read-only
// tool is never body-dropped, even when old — it is not re-derivable.
func TestRetentionKeepsMutatingResults(t *testing.T) {
	big := strings.Repeat("x", 9000)
	var msgs []llm.Message
	msgs = append(msgs, userText("q0"), asstToolUse("t0", "wr", `{}`), toolResult("t0", big), asstText("a0"))
	for i := 1; i <= 9; i++ {
		msgs = append(msgs, userText(fmt.Sprintf("q%d", i)), asstText(fmt.Sprintf("a%d", i)))
	}

	a := newAgent(llmtest.New("fake"), readOnlyRegistry(), Options{RetentionPolicy: RetentionPolicyAge})
	a.SetTranscript(msgs)
	a.applyRetention(&recordSink{})

	if got := a.Transcript()[2].Content[0].ResultText; got != big {
		t.Errorf("mutating-tool result should be preserved, len=%d want %d", len(got), len(big))
	}
}

// TestRetentionReplacesAgedImages verifies an image older than the image keep
// window is swapped for a text placeholder while a recent image stays.
func TestRetentionReplacesAgedImages(t *testing.T) {
	raw := make([]byte, 4500)
	copy(raw, []byte("\x89PNG\r\n\x1a\n"))
	data := base64.StdEncoding.EncodeToString(raw)
	msgs := []llm.Message{
		userImage("old.png", data, "q0"), asstText("a0"),
		userText("q1"), asstText("a1"),
		userText("q2"), asstText("a2"),
		userImage("new.png", data, "q3"), asstText("a3"),
	}
	a := newAgent(llmtest.New("fake"), readOnlyRegistry(), Options{RetentionPolicy: RetentionPolicyAge})
	a.SetTranscript(msgs)
	a.applyRetention(&recordSink{})
	mustValid(t, a.Transcript())

	if b := a.Transcript()[0].Content[0]; b.Kind != llm.BlockText || !strings.Contains(b.Text, "image omitted") {
		t.Errorf("aged image not replaced with placeholder: %+v", b)
	}
	if b := a.Transcript()[6].Content[0]; b.Kind != llm.BlockImage {
		t.Errorf("recent image should be kept, got kind %s", b.Kind)
	}
}

func TestRetentionReplacesAgedRichResultImages(t *testing.T) {
	const imageData = agentOnePixelPNG
	msgs := []llm.Message{
		userText("q0"),
		asstToolUse("old", "rd", `{}`),
		{Role: llm.RoleUser, Content: []llm.ContentBlock{{
			Kind:        llm.BlockToolResult,
			ResultForID: "old",
			ResultText:  "screenshot attached",
			ResultContent: []llm.ContentBlock{{
				Kind:           llm.BlockImage,
				ImageName:      "old.png",
				ImageMediaType: "image/png",
				ImageData:      imageData,
				ImageDetail:    "high",
				ImageWidth:     640,
				ImageHeight:    480,
			}},
		}}},
		asstText("a0"),
		userText("q1"), asstText("a1"),
		userText("q2"), asstText("a2"),
		userText("q3"), asstText("a3"),
	}
	a := newAgent(llmtest.New("fake"), readOnlyRegistry(), Options{RetentionPolicy: RetentionPolicyAge})
	a.SetTranscript(msgs)
	a.applyRetention(&recordSink{})
	got := a.Transcript()
	mustValid(t, got)

	result := got[2].Content[0]
	if len(result.ResultContent) != 0 {
		t.Fatalf("aged rich result still has image children: %+v", result.ResultContent)
	}
	for _, want := range []string{"image omitted", "old.png", "image/png", "detail high", "640x480"} {
		if !strings.Contains(result.ResultText, want) {
			t.Errorf("degraded result missing %q: %q", want, result.ResultText)
		}
	}
	if strings.Contains(result.ResultText, imageData) {
		t.Fatalf("degraded result leaked base64: %q", result.ResultText)
	}
}

// TestRetentionIdempotent verifies a second pass does not re-trim an
// already-trimmed result.
func TestRetentionIdempotent(t *testing.T) {
	big := strings.Repeat("x", 9000)
	var msgs []llm.Message
	msgs = append(msgs, userText("q0"), asstToolUse("t0", "rd", `{}`), toolResult("t0", big), asstText("a0"))
	for i := 1; i <= 9; i++ {
		msgs = append(msgs, userText(fmt.Sprintf("q%d", i)), asstText(fmt.Sprintf("a%d", i)))
	}
	a := newAgent(llmtest.New("fake"), readOnlyRegistry(), Options{RetentionPolicy: RetentionPolicyAge})
	a.SetTranscript(msgs)

	if changed := a.applyRetention(&recordSink{}); !changed {
		t.Fatal("first retention pass reported unchanged")
	}
	first := a.Transcript()[2].Content[0].ResultText
	if changed := a.applyRetention(&recordSink{}); changed {
		t.Fatal("second retention pass reported a change")
	}
	second := a.Transcript()[2].Content[0].ResultText
	if first != second {
		t.Errorf("retention not idempotent:\nfirst=%q\nsecond=%q", first, second)
	}
}

func TestRetentionPolicyOverridesProviderDefault(t *testing.T) {
	oldImageTranscript := func() []llm.Message {
		return []llm.Message{
			userImage("old.png", agentOnePixelPNG, "q0"), asstText("a0"),
			userText("q1"), asstText("a1"),
			userText("q2"), asstText("a2"),
			userText("q3"), asstText("a3"),
		}
	}
	for _, tc := range []struct {
		name       string
		policy     RetentionPolicy
		stateful   bool
		context    int
		wantChange bool
		wantPolicy string
	}{
		{name: "force age for stateful", policy: RetentionPolicyAge, stateful: true, context: 1, wantChange: true, wantPolicy: "age"},
		{name: "disable stateless", policy: RetentionPolicyDisabled, context: 100_000, wantChange: false},
		{name: "force pressure for stateless", policy: RetentionPolicyPressure, context: 100_000, wantChange: true, wantPolicy: "pressure_epoch"},
		{name: "auto uses pressure when high", context: 100_000, wantChange: true, wantPolicy: "pressure_epoch"},
		{name: "auto bounds replay when low", context: 1, wantChange: true, wantPolicy: "auto_age"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := newAgent(llmtest.New("fake"), readOnlyRegistry(), Options{
				ResponsesStateful: tc.stateful,
				RetentionPolicy:   tc.policy,
				ContextWindow:     100_000,
			})
			a.SetTranscript(oldImageTranscript())
			pass := a.applyRetentionPolicy(&recordSink{}, tc.context)
			if pass.changed != tc.wantChange {
				t.Fatalf("retention changed = %t, want %t (pass %+v)", pass.changed, tc.wantChange, pass)
			}
			if tc.wantPolicy != "" && pass.event.Policy != tc.wantPolicy {
				t.Fatalf("retention policy = %q, want %q (pass %+v)", pass.event.Policy, tc.wantPolicy, pass)
			}
		})
	}
}

func TestStableMessagePrefix(t *testing.T) {
	big := strings.Repeat("x", defaultSummaryToolResultSize+100)
	trimmed := big[:defaultSummaryToolResultSize] + "\n" + retentionTrimMarker + "]"
	tests := []struct {
		name     string
		messages []llm.Message
		want     int
	}{
		{
			name:     "text only",
			messages: []llm.Message{userText("q"), asstText("a")},
			want:     2,
		},
		{
			name: "recent read only result",
			messages: []llm.Message{
				userText("q"), asstToolUse("call", "rd", `{}`), toolResult("call", big),
			},
			want: 2,
		},
		{
			name: "already trimmed result",
			messages: []llm.Message{
				userText("q"), asstToolUse("call", "rd", `{}`), toolResult("call", trimmed),
			},
			want: 3,
		},
		{
			name: "mutating result",
			messages: []llm.Message{
				userText("q"), asstToolUse("call", "write_file", `{}`), toolResult("call", big),
			},
			want: 3,
		},
		{
			name:     "top level image",
			messages: []llm.Message{userImage("screen.png", agentOnePixelPNG, "inspect"), asstText("a")},
			want:     0,
		},
		{
			name: "nested result image",
			messages: []llm.Message{
				userText("q"),
				asstToolUse("call", "rd", `{}`),
				{
					Role: llm.RoleUser,
					Content: []llm.ContentBlock{{
						Kind:        llm.BlockToolResult,
						ResultForID: "call",
						ResultText:  "attached",
						ResultContent: []llm.ContentBlock{{
							Kind:           llm.BlockImage,
							ImageMediaType: "image/png",
							ImageData:      agentOnePixelPNG,
						}},
					}},
				},
			},
			want: 2,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			a := newAgent(llmtest.New("fake"), readOnlyRegistry(), Options{})
			if got := a.stableMessagePrefixIn(tc.messages); got != tc.want {
				t.Fatalf("stable prefix = %d, want %d", got, tc.want)
			}
		})
	}
}

func TestRetentionLeavesDeclaredStablePrefixByteIdentical(t *testing.T) {
	a := newAgent(llmtest.New("fake"), readOnlyRegistry(), Options{RetentionPolicy: RetentionPolicyAge})
	a.SetTranscript([]llm.Message{
		userText("q0"),
		asstText("a0"),
		userImage("screen.png", agentOnePixelPNG, "inspect"),
		asstText("a1"),
	})
	stable := a.stableMessagePrefixIn(a.Transcript())
	if stable != 2 {
		t.Fatalf("initial stable prefix = %d, want 2", stable)
	}
	before, err := llm.FingerprintMessages(a.Transcript()[:stable])
	if err != nil {
		t.Fatal(err)
	}
	a.transcript = append(a.transcript,
		userText("q2"), asstText("a2"),
		userText("q3"), asstText("a3"),
		userText("q4"), asstText("a4"),
	)
	if pass := a.applyRetentionPolicy(&recordSink{}, 0); !pass.changed {
		t.Fatal("retention did not degrade the now-old image")
	}
	after, err := llm.FingerprintMessages(a.Transcript()[:stable])
	if err != nil {
		t.Fatal(err)
	}
	if before != after {
		t.Fatalf("declared stable prefix changed: before=%s after=%s", before, after)
	}
}

func TestPressureRetentionThresholdAndHysteresis(t *testing.T) {
	oldImageTranscript := func() []llm.Message {
		return []llm.Message{
			userImage("old.png", agentOnePixelPNG, "q0"), asstText("a0"),
			userText("q1"), asstText("a1"),
			userText("q2"), asstText("a2"),
			userText("q3"), asstText("a3"),
		}
	}
	a := newAgent(llmtest.New("fake"), readOnlyRegistry(), Options{
		RetentionPolicy: RetentionPolicyPressure,
		ContextWindow:   100_000,
	})
	a.SetTranscript(oldImageTranscript())
	sink := &recordSink{}

	if pass := a.applyRetentionPolicy(sink, 59_999); pass.changed || pass.observed {
		t.Fatalf("pressure below high-water mark = %+v, want no epoch", pass)
	}
	first := a.applyRetentionPolicy(sink, 60_000)
	if !first.changed || first.event.Policy != "pressure_epoch" {
		t.Fatalf("pressure at high-water mark = %+v, want epoch", first)
	}

	// Restore an eligible block to prove the disarmed epoch cannot immediately
	// run again while context remains above the low-water mark.
	a.SetTranscript(oldImageTranscript())
	a.retentionEpochArmed = false
	if pass := a.applyRetentionPolicy(sink, 59_000); pass.changed || pass.observed {
		t.Fatalf("disarmed pressure above low-water mark = %+v, want no epoch", pass)
	}
	if pass := a.applyRetentionPolicy(sink, 50_000); pass.changed || pass.observed {
		t.Fatalf("low-water rearm pass = %+v, want no epoch", pass)
	}
	if !a.retentionEpochArmed {
		t.Fatal("pressure epoch did not rearm at low-water mark")
	}
	if pass := a.applyRetentionPolicy(sink, 60_000); !pass.changed {
		t.Fatalf("rearmed pressure at high-water mark = %+v, want epoch", pass)
	}
}

func TestPressureRetentionRearmsAfterCompaction(t *testing.T) {
	big := strings.Repeat("x", 300_000) // ~75k tokens at 4 bytes/token
	a := newAgent(llmtest.New("fake"), readOnlyRegistry(), Options{
		RetentionPolicy: RetentionPolicyPressure,
		ContextWindow:   100_000,
	})
	// The aged image sits two turns back (past retentionImageKeepTurns) so it is
	// an eligible pressure block; the newest turn carries a large read-only tool
	// result that degradeCurrent can trim so the shrink lands under budget.
	a.SetTranscript([]llm.Message{
		userImage("old.png", agentOnePixelPNG, "q1"), asstText("a1"),
		userText("q2"), asstText("a2"),
		userText("q3"), asstToolUse("call_big", "rd", `{}`), toolResult("call_big", big),
	})
	sink := &recordSink{}

	// Fire an epoch at the high-water mark; the epoch disarms itself.
	if pass := a.applyRetentionPolicy(sink, 60_000); !pass.changed {
		t.Fatalf("pressure at high-water mark = %+v, want epoch", pass)
	}
	if a.retentionEpochArmed {
		t.Fatal("epoch should disarm after firing")
	}

	// A transcript-rewriting shrink (degradeCurrent) must re-arm the epoch: a
	// compacted transcript is a new baseline, even when it lands between the
	// watermarks.
	changed, err := a.degradeCurrent(sink, "manual")
	if err != nil || !changed {
		t.Fatalf("degradeCurrent = changed %t err %v", changed, err)
	}
	if !a.retentionEpochArmed {
		t.Fatal("compaction did not re-arm the pressure epoch")
	}

	// The next high-water pass produces an epoch instead of leaving pressure
	// retention disabled for the rest of the session. observed (not changed) is
	// the signal: degradeCurrent already consumed the aged image, so the epoch
	// fires but trims nothing new.
	if pass := a.applyRetentionPolicy(sink, 55_000); pass.changed {
		t.Fatalf("mid-water pass = %+v, want no epoch", pass)
	}
	if pass := a.applyRetentionPolicy(sink, 60_000); !pass.observed {
		t.Fatalf("post-compaction high-water mark = %+v, want epoch", pass)
	}
}

func TestRetentionArchivesExactReadOnlyResultWithStableRecoveryPath(t *testing.T) {
	big := strings.Repeat("full-output-", 1000)
	msgs := []llm.Message{
		userText("q0"), asstToolUse("t0", "rd", `{}`), toolResult("t0", big), asstText("a0"),
	}
	for i := 1; i <= 9; i++ {
		msgs = append(msgs, userText(fmt.Sprintf("q%d", i)), asstText(fmt.Sprintf("a%d", i)))
	}
	a := newAgent(llmtest.New("fake"), readOnlyRegistry(), Options{RetentionPolicy: RetentionPolicyAge})
	a.SetTranscript(msgs)
	sink := &archiveSink{archive: ToolResultArchive{
		DisplayPath: "artifacts/tool-results/result.txt",
		ModelPath:   "/session/artifacts/tool-results/result.txt",
	}}

	if changed := a.applyRetention(sink); !changed {
		t.Fatal("retention pass reported unchanged")
	}
	if len(sink.archived) != 1 {
		t.Fatalf("archived result = %+v", sink.archived)
	}
	archived := sink.archived[0]
	if !archived.Truncated || archived.OriginalText != big ||
		archived.OriginalBytes != len(big) ||
		archived.ShownBytes != defaultSummaryToolResultSize {
		t.Fatalf("archive receipt did not preserve exact original: %+v", archived)
	}
	got := a.Transcript()[2].Content[0].ResultText
	if !strings.Contains(got, "/session/artifacts/tool-results/result.txt") {
		t.Fatalf("trimmed result lacks stable recovery path: %q", got)
	}
	for _, want := range []string{
		retentionTrimMarker + " receipt]",
		"status: success",
		fmt.Sprintf("output: first %d of %d bytes", defaultSummaryToolResultSize, len(big)),
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("trimmed result missing typed field %q: %q", want, got)
		}
	}
}

func TestRetentionKeepsOriginalWhenArtifactWriteFails(t *testing.T) {
	big := strings.Repeat("full-output-", 1000)
	msgs := []llm.Message{
		userText("q0"), asstToolUse("t0", "rd", `{}`), toolResult("t0", big), asstText("a0"),
	}
	for i := 1; i <= 9; i++ {
		msgs = append(msgs, userText(fmt.Sprintf("q%d", i)), asstText(fmt.Sprintf("a%d", i)))
	}
	a := newAgent(llmtest.New("fake"), readOnlyRegistry(), Options{RetentionPolicy: RetentionPolicyAge})
	a.SetTranscript(msgs)
	sink := &archiveSink{archiveErr: errors.New("disk unavailable")}

	if changed := a.applyRetention(sink); changed {
		t.Fatal("retention trimmed a result whose exact artifact could not be written")
	}
	if got := a.Transcript()[2].Content[0].ResultText; got != big {
		t.Fatalf("result changed after artifact failure: got %d bytes, want %d", len(got), len(big))
	}
	if len(sink.archived) != 1 || sink.archived[0].OriginalText != big {
		t.Fatalf("archive attempt = %+v", sink.archived)
	}
}

func TestExplicitPressureRetentionPreservesResponseStateBelowPressure(t *testing.T) {
	msgs := []llm.Message{
		userImage("old.png", agentOnePixelPNG, "q0"), asstText("a0"),
		userText("q1"), asstText("a1"),
		userText("q2"), asstText("a2"),
		userText("q3"), asstText("a3"),
	}
	fp := llmtest.New("responses",
		llmtest.Step{
			Events:     []llm.StreamEvent{textDelta("done")},
			Stop:       llm.StopEndTurn,
			ResponseID: "resp-new",
		},
		llmtest.Step{
			Events:     []llm.StreamEvent{textDelta("done again")},
			Stop:       llm.StopEndTurn,
			ResponseID: "resp-next",
		},
	)
	a := newAgent(fp, readOnlyRegistry(), Options{
		ResponsesStateful: true,
		RetentionPolicy:   RetentionPolicyPressure,
		ContextWindow:     1_000_000,
	})
	a.SetTranscript(msgs)
	digest, err := llm.FingerprintMessages(msgs)
	if err != nil {
		t.Fatal(err)
	}
	a.SetResponseState(&llm.ResponseState{PreviousResponseID: "resp-old", AnchorMessages: len(msgs), AnchorDigest: digest})

	if err := a.RunPrompt(context.Background(), "next", &recordSink{}); err != nil {
		t.Fatalf("RunPrompt: %v", err)
	}
	if len(fp.Requests) != 1 {
		t.Fatalf("provider requests = %d, want 1", len(fp.Requests))
	}
	if fp.Requests[0].PreviousResponseID != "resp-old" || len(fp.Requests[0].Messages) != 1 {
		t.Fatalf("request after retention = prev %q messages %d", fp.Requests[0].PreviousResponseID, len(fp.Requests[0].Messages))
	}
	if a.Transcript()[0].Content[0].Kind != llm.BlockImage {
		t.Fatalf("old image changed below pressure: %+v", a.Transcript()[0].Content[0])
	}
}

func TestStatefulRetentionPressureEpochTrimsEligibleBlocksAndResetsOnce(t *testing.T) {
	big := strings.Repeat("x", 30_000)
	msgs := []llm.Message{
		userText("q0"), asstToolUse("t0", "rd", `{}`), toolResult("t0", big), asstText("a0"),
		userText("q1"), asstToolUse("t1", "rd", `{}`), toolResult("t1", big), asstText("a1"),
	}
	for i := 2; i <= 10; i++ {
		msgs = append(msgs, userText(fmt.Sprintf("q%d", i)), asstText(fmt.Sprintf("a%d", i)))
	}
	fp := llmtest.New("responses", llmtest.Step{
		Events:     []llm.StreamEvent{textDelta("done")},
		Stop:       llm.StopEndTurn,
		ResponseID: "resp-new",
	})
	a := newAgent(fp, readOnlyRegistry(), Options{
		ResponsesStateful:     true,
		ContextWindow:         20_000,
		DisableAutoCompaction: true,
	})
	a.SetTranscript(msgs)
	digest, err := llm.FingerprintMessages(msgs)
	if err != nil {
		t.Fatal(err)
	}
	a.SetResponseState(&llm.ResponseState{PreviousResponseID: "resp-old", AnchorMessages: len(msgs), AnchorDigest: digest})
	sink := &recordSink{}

	if err := a.RunPrompt(context.Background(), "next", sink); err != nil {
		t.Fatalf("RunPrompt: %v", err)
	}
	if err := a.RunPrompt(context.Background(), "next again", sink); err != nil {
		t.Fatalf("second RunPrompt: %v", err)
	}
	if len(fp.Requests) != 2 {
		t.Fatalf("provider requests = %d, want 2", len(fp.Requests))
	}
	if fp.Requests[0].PreviousResponseID != "" || len(fp.Requests[0].Messages) != len(msgs)+1 {
		t.Fatalf("pressure request = prev %q messages %d", fp.Requests[0].PreviousResponseID, len(fp.Requests[0].Messages))
	}
	if fp.Requests[1].PreviousResponseID != "resp-new" || len(fp.Requests[1].Messages) != 1 {
		t.Fatalf("post-epoch request = prev %q messages %d", fp.Requests[1].PreviousResponseID, len(fp.Requests[1].Messages))
	}
	for _, index := range []int{2, 6} {
		if got := a.Transcript()[index].Content[0].ResultText; !strings.Contains(got, retentionTrimMarker) {
			t.Fatalf("eligible result at %d was not trimmed: %q", index, got)
		}
	}
	if len(sink.retention) != 1 {
		t.Fatalf("retention events = %d, want one epoch: %+v", len(sink.retention), sink.retention)
	}
	event := sink.retention[0]
	if event.Policy != "pressure_epoch" || event.Trigger != "context_pressure" || event.BlocksTrimmed != 2 {
		t.Fatalf("retention event = %+v", event)
	}
	if !event.ResponseStateReset || event.NextRequestStateful || event.BytesAfter >= event.BytesBefore || event.ContextTokensAfter >= event.ContextTokensBefore {
		t.Fatalf("retention effect = %+v", event)
	}
}

func TestRetentionRewriteResetsCompactionMeasurement(t *testing.T) {
	old := strings.Repeat("o", 12_000)
	fresh := strings.Repeat("n", 16_000)
	msgs := []llm.Message{
		userText("q0"), asstToolUse("old", "rd", `{}`), toolResult("old", old), asstText("a0"),
	}
	for i := 1; i <= 9; i++ {
		msgs = append(msgs, userText(fmt.Sprintf("q%d", i)), asstText(fmt.Sprintf("a%d", i)))
	}
	reg := &tools.Registry{}
	reg.Register(&recordTool{
		name:     "rd",
		readOnly: true,
		run: func(context.Context, json.RawMessage) (string, error) {
			return fresh, nil
		},
	})
	fp := llmtest.New("fake",
		llmtest.Step{
			Events: []llm.StreamEvent{toolDone(0, "fresh", "rd", `{}`)},
			Stop:   llm.StopToolUse,
			// Deliberately larger than the local estimate. Once retention
			// rewrites the prefix, this measurement must not be reused.
			Usage: llm.Usage{InputTokens: 7_600},
		},
		llmtest.Step{
			Events: []llm.StreamEvent{textDelta("done")},
			Stop:   llm.StopEndTurn,
			Usage:  llm.Usage{InputTokens: 6_500},
		},
	)
	a := newAgent(fp, reg, Options{
		ContextWindow:   10_000,
		RetentionPolicy: RetentionPolicyPressure,
	})
	a.SetTranscript(msgs)
	sink := &recordSink{}

	if err := a.RunPrompt(context.Background(), "next", sink); err != nil {
		t.Fatalf("RunPrompt: %v", err)
	}
	if len(sink.retention) != 1 || sink.retention[0].BlocksTrimmed == 0 {
		t.Fatalf("retention events = %+v, want one changed pressure epoch", sink.retention)
	}
	if len(fp.Requests) != 2 {
		t.Fatalf("provider requests = %d, want two conversational requests without a stale-trigger compaction", len(fp.Requests))
	}
	if len(sink.maintenance) != 0 {
		t.Fatalf("maintenance calls = %+v, want none", sink.maintenance)
	}
}

// TestTruncateLargestBlockDownranksImages pins r22: when a text result and an
// image coexist, the text is truncated first even though the image's base64
// byte length is larger.
func TestTruncateLargestBlockDownranksImages(t *testing.T) {
	msgs := []llm.Message{{
		Role: llm.RoleUser,
		Content: []llm.ContentBlock{
			{Kind: llm.BlockImage, ImageMediaType: "image/png", ImageData: strings.Repeat("x", 50000)},
			{Kind: llm.BlockToolResult, ResultForID: "t", ResultText: strings.Repeat("y", 20000)},
		},
	}}
	if !truncateLargestBlock(msgs, 5000) {
		t.Fatal("truncateLargestBlock returned false")
	}
	if msgs[0].Content[0].Kind != llm.BlockImage {
		t.Errorf("image should not be dropped before the larger text result, got %s", msgs[0].Content[0].Kind)
	}
	if got := msgs[0].Content[1].ResultText; len(got) >= 20000 || !strings.Contains(got, "truncated") {
		t.Errorf("text result should have been truncated first, len=%d", len(got))
	}
}

// TestCompactionRecoversTransientSummaryError pins r32: a retryable mid-stream
// failure on the summary call is retried, so compaction completes.
func TestCompactionRecoversTransientSummaryError(t *testing.T) {
	fp := llmtest.New("fake",
		llmtest.Step{Err: errors.New("transient blip")},
		summaryStep("recovered summary", 50, 10),
	)
	a := newAgent(fp, tools.Default(), Options{Model: "claude-opus-4-8"})
	a.SetSleep(func(time.Duration) {})
	a.SetSystem("sys")
	a.SetTranscript(makeTurns(10))
	sink := &recordSink{}

	if _, err := a.Compact(context.Background(), sink); err != nil {
		t.Fatalf("Compact should recover from a transient error: %v", err)
	}
	if got := len(a.Transcript()); got != 16 {
		t.Fatalf("compaction should have collapsed to checkpoint + 8 turns, got %d", got)
	}
	if !strings.Contains(a.Transcript()[0].Content[0].Text, "recovered summary") {
		t.Errorf("summary message = %q, want the recovered text", a.Transcript()[0].Content[0].Text)
	}
}

// TestCompactionBumpsBudgetOnTruncatedSummary pins r33: a max-tokens-truncated
// summary call is retried once with a doubled budget.
func TestCompactionBumpsBudgetOnTruncatedSummary(t *testing.T) {
	truncated := llmtest.Step{
		Events: []llm.StreamEvent{textDelta("partial")},
		Stop:   llm.StopMaxTokens,
		Usage:  llm.Usage{InputTokens: 40, OutputTokens: 2048},
	}
	full := summaryStep("complete summary", 40, 50)
	fp := llmtest.New("fake", truncated, full)
	a := newAgent(fp, tools.Default(), Options{Model: "claude-opus-4-8"})
	a.SetSleep(func(time.Duration) {})
	a.SetSystem("sys")
	a.SetTranscript(makeTurns(10))

	if _, err := a.Compact(context.Background(), &recordSink{}); err != nil {
		t.Fatalf("Compact: %v", err)
	}
	if len(fp.Requests) != 2 {
		t.Fatalf("summary call should have retried once, got %d requests", len(fp.Requests))
	}
	if first, second := fp.Requests[0].MaxTokens, fp.Requests[1].MaxTokens; second != first*2 {
		t.Errorf("budget not doubled on retry: first=%d second=%d", first, second)
	}
	if !strings.Contains(a.Transcript()[0].Content[0].Text, "complete summary") {
		t.Errorf("the un-truncated summary should win, got %q", a.Transcript()[0].Content[0].Text)
	}
}

// TestSummaryCallDisablesReasoning pins r13: the summary request carries no
// reasoning budget even when the agent is configured for reasoning.
func TestSummaryCallDisablesReasoning(t *testing.T) {
	fp := llmtest.New("fake", summaryStep("S", 50, 10))
	a := newAgent(fp, tools.Default(), Options{Model: "claude-opus-4-8"})
	a.SetReasoning(llm.ReasoningConfig{Effort: "high"})
	a.SetSystem("sys")
	a.SetTranscript(makeTurns(10))

	if _, err := a.Compact(context.Background(), &recordSink{}); err != nil {
		t.Fatalf("Compact: %v", err)
	}
	if got := fp.Requests[0].Reasoning; got != (llm.ReasoningConfig{}) {
		t.Errorf("summary request should disable reasoning, got %+v", got)
	}
}

// TestCompactionTrimsKeptTurnsWhenReclaimLow pins r54: when collapsing the older
// turns reclaims little, the kept turns' large read-only results are trimmed in
// place.
func TestCompactionTrimsKeptTurnsWhenReclaimLow(t *testing.T) {
	big := strings.Repeat("x", 9000)
	var msgs []llm.Message
	// Two tiny old turns -> summarize to almost nothing.
	msgs = append(msgs, userText("q0"), asstText("a0"), userText("q1"), asstText("a1"))
	// Four big kept turns dominate the context.
	for i := 0; i < 4; i++ {
		id := fmt.Sprintf("t%d", i)
		msgs = append(msgs, userText(fmt.Sprintf("Q%d", i)), asstToolUse(id, "rd", `{}`), toolResult(id, big), asstText(fmt.Sprintf("A%d", i)))
	}
	a := newAgent(llmtest.New("fake", summaryStep("S", 20, 5)), readOnlyRegistry(), Options{Model: "claude-opus-4-8"})
	a.SetSleep(func(time.Duration) {})
	a.SetSystem("sys")
	a.SetTranscript(msgs)

	if _, err := a.Compact(context.Background(), &recordSink{}); err != nil {
		t.Fatalf("Compact: %v", err)
	}
	mustValid(t, a.Transcript())

	trimmed := 0
	for _, m := range a.Transcript() {
		for _, b := range m.Content {
			if b.Kind == llm.BlockToolResult && strings.Contains(b.ResultText, retentionTrimMarker) {
				trimmed++
			}
		}
	}
	if trimmed != 4 {
		t.Errorf("expected all 4 kept read-only results trimmed, got %d:\n%s", trimmed, dump(a.Transcript()))
	}
}

// TestPressureRetentionFloorTriggersBelowWindowPercentage pins the
// retention_floor_tokens fallback: on a 1M window the 60% high-water mark is
// 600K, so without a floor nothing trims at 200K; with the floor configured,
// the same hysteretic epoch fires at the floor instead.
func TestPressureRetentionFloorTriggersBelowWindowPercentage(t *testing.T) {
	oldImageTranscript := func() []llm.Message {
		return []llm.Message{
			userImage("old.png", agentOnePixelPNG, "q0"), asstText("a0"),
			userText("q1"), asstText("a1"),
			userText("q2"), asstText("a2"),
			userText("q3"), asstText("a3"),
		}
	}
	a := newAgent(llmtest.New("fake"), readOnlyRegistry(), Options{
		RetentionPolicy:      RetentionPolicyPressure,
		ContextWindow:        1_000_000,
		RetentionFloorTokens: 200_000,
	})
	a.SetTranscript(oldImageTranscript())
	sink := &recordSink{}

	if pass := a.applyRetentionPolicy(sink, 199_999); pass.changed || pass.observed {
		t.Fatalf("context below floor = %+v, want no epoch", pass)
	}
	// Above the floor but far below the 60% window high-water mark (600K).
	first := a.applyRetentionPolicy(sink, 200_000)
	if !first.changed || first.event.Policy != "pressure_epoch" {
		t.Fatalf("context at floor = %+v, want epoch", first)
	}

	// Hysteresis uses the floor-scaled low-water mark (200K * 50/60 ≈ 166.7K).
	a.SetTranscript(oldImageTranscript())
	a.retentionEpochArmed = false
	if pass := a.applyRetentionPolicy(sink, 190_000); pass.changed || pass.observed {
		t.Fatalf("disarmed above scaled low-water = %+v, want no epoch", pass)
	}
	if pass := a.applyRetentionPolicy(sink, 166_000); pass.changed || pass.observed {
		t.Fatalf("scaled low-water rearm pass = %+v, want no epoch", pass)
	}
	if !a.retentionEpochArmed {
		t.Fatal("floor epoch did not rearm at the scaled low-water mark")
	}
	second := a.applyRetentionPolicy(sink, 200_000)
	if !second.changed {
		t.Fatalf("rearmed floor pass = %+v, want epoch", second)
	}

	// After the floor trim consumed the aged image, the retention-stable prefix
	// spans the whole transcript: nothing remains a future pass could rewrite.
	if stable := a.stableMessagePrefixIn(a.Transcript()); stable != len(a.Transcript()) {
		t.Fatalf("stable prefix after floor trim = %d, want %d (whole transcript)", stable, len(a.Transcript()))
	}
}

func TestPressureRetentionFloorDisabledByDefault(t *testing.T) {
	a := newAgent(llmtest.New("fake"), readOnlyRegistry(), Options{
		RetentionPolicy: RetentionPolicyPressure,
		ContextWindow:   1_000_000,
	})
	a.SetTranscript([]llm.Message{
		userImage("old.png", agentOnePixelPNG, "q0"), asstText("a0"),
		userText("q1"), asstText("a1"),
		userText("q2"), asstText("a2"),
		userText("q3"), asstText("a3"),
	})
	if pass := a.applyRetentionPolicy(&recordSink{}, 200_000); pass.changed || pass.observed {
		t.Fatalf("20%% of window without a floor = %+v, want no epoch", pass)
	}
}

func TestPressureRetentionFloorAboveWindowPercentageIsNoOp(t *testing.T) {
	// A floor above the 60% window high-water mark must not RAISE the trigger:
	// the window percentage still governs.
	a := newAgent(llmtest.New("fake"), readOnlyRegistry(), Options{
		RetentionPolicy:      RetentionPolicyPressure,
		ContextWindow:        1_000_000,
		RetentionFloorTokens: 900_000,
	})
	a.SetTranscript([]llm.Message{
		userImage("old.png", agentOnePixelPNG, "q0"), asstText("a0"),
		userText("q1"), asstText("a1"),
		userText("q2"), asstText("a2"),
		userText("q3"), asstText("a3"),
	})
	if pass := a.applyRetentionPolicy(&recordSink{}, 599_999); pass.changed || pass.observed {
		t.Fatalf("just below 60%% with high floor = %+v, want no epoch", pass)
	}
	if pass := a.applyRetentionPolicy(&recordSink{}, 600_000); !pass.changed {
		t.Fatalf("at 60%% with high floor = %+v, want epoch (window governs)", pass)
	}
}

func TestPressureRetentionFloorNothingOldEnough(t *testing.T) {
	// A transcript within the keep-turns boundary has nothing old enough to
	// trim; the floor pass is a no-op (not even observed).
	a := newAgent(llmtest.New("fake"), readOnlyRegistry(), Options{
		RetentionPolicy:      RetentionPolicyPressure,
		ContextWindow:        1_000_000,
		RetentionFloorTokens: 100_000,
	})
	a.SetTranscript([]llm.Message{
		userText("q1"), asstText("a1"),
	})
	if pass := a.applyRetentionPolicy(&recordSink{}, 900_000); pass.changed || pass.observed {
		t.Fatalf("floor pass with nothing old enough = %+v, want no epoch", pass)
	}
}
