package session

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/ClarifiedLabs/mdcli/markdown"
)

func TestMarkdownDependencyPreservesReplayLayoutBudget(t *testing.T) {
	var source strings.Builder
	source.WriteString("```mermaid\nflowchart TD\n")
	for i := 0; i < 46; i++ {
		fmt.Fprintf(&source, "N%d --> N%d\n", i, i+1)
	}
	for i := 2; i < 47; i++ {
		fmt.Fprintf(&source, "N0 --> N%d\n", i)
	}
	source.WriteString("```\n")
	input := source.String()
	if got := markdown.Render(input, markdown.Options{Enabled: true, Width: 80}); strings.Contains(got, "```mermaid") {
		t.Fatal("fixture must render under mdcli's default budget")
	}
	want := markdown.Render(input, markdown.Options{Enabled: true, Width: 80, MermaidMaxVirtualNodes: 1024})
	if !strings.Contains(want, "```mermaid") {
		t.Fatal("fixture must fall back under Harness's budget")
	}

	dir := t.TempDir()
	writeFollowMeta(t, dir, ChildStatusCompleted)
	appendMermaidEvents(t, dir,
		Event{Type: EventAssistantDelta, Prompt: 1, Turn: 1, Text: input},
		Event{Type: EventTurnComplete, Prompt: 1, Turn: 1},
	)
	var replayed, followed strings.Builder
	if err := Replay(dir, &replayed, ReplayOptions{Markdown: true, Width: 80}); err != nil {
		t.Fatal(err)
	}
	if err := followWithWaiter(context.Background(), dir, &followed, ReplayOptions{Markdown: true, Width: 80}, func(context.Context) error {
		return errors.New("completed child unexpectedly waited")
	}); err != nil {
		t.Fatal(err)
	}
	if replayed.String() != want || followed.String() != testFollowHeader+want {
		t.Fatalf("replay/follow lost layout budget: replay=%q follow=%q", replayed.String(), followed.String())
	}
	latest, err := LatestTurnOutput(dir)
	if err != nil {
		t.Fatal(err)
	}
	if latest != strings.TrimRight(want, "\n") {
		t.Fatalf("latest output lost layout budget: %q", latest)
	}
	lines := ReasoningSummaryLines(input, ReasoningSummaryFormat{Width: 80})
	if got := strings.Join(lines, "\n"); !strings.Contains(got, "    ```mermaid\n") {
		t.Fatalf("reasoning replay lost layout budget or indentation: %q", got)
	}
}
