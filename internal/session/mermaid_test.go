package session

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"harness/internal/markdown"
	"harness/internal/mermaid"
)

const sessionMermaidFlow = "flowchart TD\n  A[Start] --> B[End]\n"

func appendMermaidEvents(t *testing.T, dir string, events ...Event) {
	t.Helper()
	for _, ev := range events {
		if err := AppendEvent(dir, ev); err != nil {
			t.Fatalf("AppendEvent: %v", err)
		}
	}
}

func sessionMermaidDiagram(t *testing.T) string {
	t.Helper()
	diagram, err := mermaid.Render(sessionMermaidFlow, mermaid.Options{Width: markdown.DefaultWidth})
	if err != nil {
		t.Fatalf("render expected Mermaid diagram: %v", err)
	}
	return diagram
}

func TestMermaidReplayFollowSplitFence(t *testing.T) {
	diagram := sessionMermaidDiagram(t)
	for _, tt := range []struct {
		name    string
		closing string
		after   string
	}{
		{name: "closing newline and following prose", closing: "`\nAfter **diagram**.\n", after: "After diagram.\n"},
		{name: "closing marker without newline", closing: "`"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			writeFollowMeta(t, dir, ChildStatusRunning)
			chunks := []string{"``", "`mer", "maid\nflowchart T", "D\n  A[Start] --", "> B[End]\n", "``", tt.closing}
			appendMermaidEvents(t, dir,
				Event{Type: EventUser, Prompt: 1, Text: "show diagram"},
				Event{Type: EventAssistantDelta, Prompt: 1, Turn: 1, Text: chunks[0]},
			)

			var followed strings.Builder
			prefix := "> show diagram\n" + markdown.HorizontalRule + "\n"
			next := 1
			wait := func(context.Context) error {
				if next < len(chunks) {
					if got := followed.String(); got != prefix {
						t.Fatalf("diagram emitted before closing fence at chunk %d: %q", next, got)
					}
					appendMermaidEvents(t, dir, Event{Type: EventAssistantDelta, Prompt: 1, Turn: 1, Text: chunks[next]})
					next++
					return nil
				}
				if next > len(chunks) {
					return errors.New("unexpected extra wait")
				}
				// A newline-terminated closing marker renders immediately. Without
				// that newline, the marker stays buffered until the turn finishes.
				wantLive := prefix
				if strings.Contains(tt.closing, "\n") {
					wantLive += diagram + tt.after
				}
				if got := followed.String(); got != wantLive {
					t.Fatalf("output before turn completion = %q, want %q", got, wantLive)
				}
				appendMermaidEvents(t, dir, Event{Type: EventTurnComplete, Prompt: 1, Turn: 1, Display: "[turn: 1]"})
				writeFollowMeta(t, dir, ChildStatusCompleted)
				next++
				return nil
			}
			opts := ReplayOptions{Markdown: true}
			if err := followWithWaiter(context.Background(), dir, &followed, opts, wait); err != nil {
				t.Fatalf("Follow: %v", err)
			}
			if next != len(chunks)+1 {
				t.Fatalf("processed %d chunks, want %d", next-1, len(chunks))
			}
			want := prefix + diagram + tt.after + "[turn: 1]\n"
			if got := followed.String(); got != want {
				t.Fatalf("Follow = %q, want %q", got, want)
			}
			var replayed strings.Builder
			if err := Replay(dir, &replayed, opts); err != nil {
				t.Fatalf("Replay: %v", err)
			}
			if got := replayed.String(); got != followed.String() {
				t.Fatalf("Replay = %q, want Follow %q", got, followed.String())
			}
			latest, err := LatestTurnOutput(dir)
			if err != nil {
				t.Fatalf("LatestTurnOutput: %v", err)
			}
			if want := diagram + tt.after + "[turn: 1]"; latest != want {
				t.Fatalf("LatestTurnOutput = %q, want %q", latest, want)
			}
		})
	}
}

func TestMermaidReplayFollowUnclosedEOF(t *testing.T) {
	diagram := sessionMermaidDiagram(t)
	for _, tt := range []struct {
		name   string
		source string
		want   string
	}{
		{name: "valid", source: strings.TrimSuffix(sessionMermaidFlow, "\n"), want: diagram},
		{name: "invalid fallback", source: "not a mermaid diagram", want: "  ```mermaid\n  not a mermaid diagram\n"},
	} {
		for _, ending := range []string{"", "\n"} {
			name := tt.name + "/no final newline"
			if ending != "" {
				name = tt.name + "/final newline"
			}
			t.Run(name, func(t *testing.T) {
				dir := t.TempDir()
				writeFollowMeta(t, dir, ChildStatusRunning)
				appendMermaidEvents(t, dir, Event{Type: EventAssistantDelta, Prompt: 1, Turn: 1, Text: "```mermaid\n"})
				var followed strings.Builder
				waitCalls := 0
				wait := func(context.Context) error {
					if got := followed.String(); got != "" {
						t.Fatalf("open diagram emitted at temporary EOF: %q", got)
					}
					switch waitCalls {
					case 0:
						appendMermaidEvents(t, dir, Event{Type: EventAssistantDelta, Prompt: 1, Turn: 1, Text: tt.source + ending})
					case 1:
						// No turn-complete event or closing fence: Follow's final Flush
						// must finalize the diagram only when the child really ends.
						writeFollowMeta(t, dir, ChildStatusCompleted)
					default:
						return errors.New("unexpected extra wait")
					}
					waitCalls++
					return nil
				}
				opts := ReplayOptions{Markdown: true}
				if err := followWithWaiter(context.Background(), dir, &followed, opts, wait); err != nil {
					t.Fatalf("Follow: %v", err)
				}
				if waitCalls != 2 {
					t.Fatalf("wait calls = %d, want 2", waitCalls)
				}
				if got := followed.String(); got != tt.want {
					t.Fatalf("Follow at EOF = %q, want %q", got, tt.want)
				}
				var replayed strings.Builder
				if err := Replay(dir, &replayed, opts); err != nil {
					t.Fatalf("Replay: %v", err)
				}
				if got := replayed.String(); got != followed.String() {
					t.Fatalf("Replay at EOF = %q, want Follow %q", got, followed.String())
				}
			})
		}
	}
}

func TestResponsiveMermaidReplayFollowWidth(t *testing.T) {
	input := "```mermaid\nflowchart LR\nA[Alpha] --> B[Beta] --> C[Gamma] --> D[Delta]\n```\n"
	dir := t.TempDir()
	writeFollowMeta(t, dir, ChildStatusCompleted)
	for _, chunk := range strings.SplitAfter(input, "\n") {
		appendMermaidEvents(t, dir, Event{Type: EventAssistantDelta, Prompt: 1, Turn: 1, Text: chunk})
	}
	for _, width := range []int{8, 40, 120} {
		opts := ReplayOptions{Markdown: true, Width: width}
		want := markdown.Render(input, markdown.Options{Enabled: true, Width: width})
		var replayed, followed strings.Builder
		if err := Replay(dir, &replayed, opts); err != nil {
			t.Fatal(err)
		}
		if err := followWithWaiter(context.Background(), dir, &followed, opts, func(context.Context) error {
			return errors.New("completed child unexpectedly waited")
		}); err != nil {
			t.Fatal(err)
		}
		if replayed.String() != want || followed.String() != want {
			t.Fatalf("width %d: replay=%q follow=%q want=%q", width, replayed.String(), followed.String(), want)
		}
	}
	var raw strings.Builder
	if err := Replay(dir, &raw, ReplayOptions{Width: 8}); err != nil {
		t.Fatal(err)
	}
	if raw.String() != input {
		t.Fatalf("width adapted raw replay: %q", raw.String())
	}
}

func TestMermaidReplayNoticeDoesNotReopenClosingFence(t *testing.T) {
	dir := t.TempDir()
	appendMermaidEvents(t, dir,
		Event{Type: EventAssistantDelta, Prompt: 1, Turn: 1, Text: "```mermaid\n" + sessionMermaidFlow},
		Event{Type: EventNotice, Prompt: 1, Turn: 1, Display: "[native steer applied]"},
		Event{Type: EventAssistantDelta, Prompt: 1, Turn: 1, Text: "B --> C\n```\n**after**\n"},
	)
	var out strings.Builder
	if err := Replay(dir, &out, ReplayOptions{Markdown: true}); err != nil {
		t.Fatal(err)
	}
	want := sessionMermaidDiagram(t) + "[native steer applied]\n  B --> C\n  ```\nafter\n"
	if got := out.String(); got != want {
		t.Fatalf("notice corrupted replayed fence: %q, want %q", got, want)
	}
}

func TestMermaidRawReplayAndStoredRecordsUnchanged(t *testing.T) {
	for _, tt := range []struct {
		name   string
		source string
	}{
		{name: "closed", source: "```mermaid\n" + sessionMermaidFlow + "```\n"},
		{name: "closed without newline", source: "```mermaid\n" + sessionMermaidFlow + "```"},
		{name: "unclosed valid", source: "```mermaid\n" + strings.TrimSuffix(sessionMermaidFlow, "\n")},
		{name: "unclosed invalid", source: "```mermaid\nnot a mermaid diagram"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			writeFollowMeta(t, dir, ChildStatusCompleted)
			appendMermaidEvents(t, dir,
				Event{Type: EventAssistantDelta, Prompt: 1, Turn: 1, Text: tt.source[:5]},
				Event{Type: EventAssistantDelta, Prompt: 1, Turn: 1, Text: tt.source[5:]},
				Event{Type: EventTurnComplete, Prompt: 1, Turn: 1},
			)
			rawPath := filepath.Join(dir, eventLog)
			before, err := os.ReadFile(rawPath)
			if err != nil {
				t.Fatal(err)
			}

			// Exercise every rendered reader before checking that raw replay and
			// the canonical records still contain the original Mermaid source.
			var rendered strings.Builder
			opts := ReplayOptions{Markdown: true, ANSI: true}
			if err := Replay(dir, &rendered, opts); err != nil {
				t.Fatalf("rendered Replay: %v", err)
			}
			var followed strings.Builder
			if err := followWithWaiter(context.Background(), dir, &followed, opts, func(context.Context) error {
				return errors.New("terminal child unexpectedly waited")
			}); err != nil {
				t.Fatalf("Follow: %v", err)
			}
			if followed.String() != rendered.String() {
				t.Fatalf("Follow = %q, want Replay %q", followed.String(), rendered.String())
			}
			latest, err := LatestTurnOutput(dir)
			if err != nil {
				t.Fatalf("LatestTurnOutput: %v", err)
			}
			if want := strings.TrimRight(rendered.String(), "\n"); latest != want {
				t.Fatalf("LatestTurnOutput = %q, want %q", latest, want)
			}
			var raw strings.Builder
			if err := Replay(dir, &raw, ReplayOptions{Markdown: false, ANSI: true}); err != nil {
				t.Fatalf("raw Replay: %v", err)
			}
			// Session display finishes an open line, but must not alter its source.
			wantRaw := tt.source
			if !strings.HasSuffix(wantRaw, "\n") {
				wantRaw += "\n"
			}
			if got := raw.String(); got != wantRaw {
				t.Fatalf("raw Replay = %q, want %q", got, wantRaw)
			}
			after, err := os.ReadFile(rawPath)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(before, after) {
				t.Fatal("replay/follow/latest output changed raw.ndjson")
			}
			events, err := readEvents(dir)
			if err != nil {
				t.Fatalf("readEvents: %v", err)
			}
			if len(events) != 3 || events[0].Text != tt.source[:5] || events[1].Text != tt.source[5:] {
				t.Fatalf("stored records lost original source or event boundaries: %+v", events)
			}
		})
	}
}
