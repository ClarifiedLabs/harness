package agent

import (
	"context"
	"strings"
	"testing"

	"harness/internal/llm"
	"harness/internal/llm/llmtest"
	"harness/internal/tools"
)

func TestOrdinaryCheckpointStillAgesSupersededPrompts(t *testing.T) {
	a := newAgent(llmtest.New("fake"), &tools.Registry{}, Options{})
	var prior []llm.Message
	for _, text := range []string{"first unrelated task", "second unrelated task", "third unrelated task"} {
		prompt := userText(text)
		prompt.Origin = llm.MessageOriginPrompt
		prior = append(prior, prompt, asstText("finished"))
		checkpoint := a.checkpointMessage("earlier work summarized", prior, "", "model", "", "", nil, 0, nil)
		if checkpoint.Compaction.UserInstructions != nil {
			t.Fatal("ordinary checkpoint acquired notes-reset instruction retention")
		}
		got := messageTextForCheckpoint(checkpoint)
		if !strings.Contains(got, text) {
			t.Fatal("lost current prompt")
		}
		if text != "first unrelated task" && strings.Contains(got, "first unrelated task") {
			t.Fatal("superseded prompt pinned forever")
		}
		prior = []llm.Message{checkpoint}
	}
}

func TestOrdinaryCompactionDoesNotResurrectDegradedContinuityImages(t *testing.T) {
	const png = "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mP8/x8AAwMCAO+jRZkAAAAASUVORK5CYII="
	image := llm.ContentBlock{Kind: llm.BlockImage, ImageMediaType: "image/png", ImageData: png}
	originals := []llm.ContentBlock{{Kind: llm.BlockText, Text: "Preserve the original screenshots."}}
	for i := 0; i < 30; i++ {
		originals = append(originals, image)
	}
	checkpoint := userText("Prior notes checkpoint")
	checkpoint.Origin = llm.MessageOriginCompactionCheckpoint
	checkpoint.Content = append(checkpoint.Content, originals[1:]...)
	checkpoint.Compaction = &llm.CompactionMetadata{Summary: "original notes", SummarySource: "task_notes", UserInstructions: originals}
	p := llmtest.New("fake", summaryStep("Older work is complete.", 20, 10))
	a := newAgent(p, &tools.Registry{}, Options{ContextWindow: 2000, CompactKeepTurns: 1, CompactKeepTokens: 1})
	a.SetTranscript([]llm.Message{checkpoint, asstText(strings.Repeat("older evidence ", 1000)), asstText("latest turn")})
	var archived []llm.Message
	a.SetCompactionArchiver(func(_ context.Context, archive CompactionArchive) (string, error) {
		archived = archive.Messages
		return "compactions/0001.input.json", nil
	})
	if _, err := a.Compact(context.Background(), &recordSink{}); err != nil {
		t.Fatal(err)
	}
	if len(archived) == 0 {
		t.Fatal("did not exercise archived compaction")
	}
	got := a.Transcript()
	if err := llm.ValidateTranscript(got); err != nil {
		t.Fatal(err)
	}
	images := 0
	for _, m := range got {
		for _, b := range m.Content {
			if b.Kind == llm.BlockImage {
				images++
			}
		}
	}
	if images >= 30 {
		t.Fatal("archive attachment resurrected images removed to meet the compaction budget")
	}
	if len(got[0].Compaction.UserInstructions) != 31 {
		t.Fatal("degradation destroyed typed recovery originals")
	}
	if !strings.Contains(messageTextForCheckpoint(got[0]), "compactions/0001.input.json") {
		t.Fatal("lost archive reference")
	}
	if a.estimateContextForTranscript(nil, got).Total >= 2000 {
		t.Fatal("compaction installed an oversized checkpoint")
	}
}
