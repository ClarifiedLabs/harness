package mermaid

import (
	"errors"
	"fmt"
	"strings"
	"testing"
)

func TestResponsiveLabelIntervals(t *testing.T) {
	for _, tt := range []struct {
		name    string
		labels  []labelDraw
		overlap bool
	}{
		{"empty", nil, false},
		{"adjacent", []labelDraw{{2, 0, "abcd"}, {6, 0, "efgh"}}, false},
		{"different rows", []labelDraw{{2, 0, "abcd"}, {2, 1, "abcd"}}, false},
		{"same text", []labelDraw{{2, 0, "abcd"}, {2, 0, "abcd"}}, true},
		{"partial unsorted", []labelDraw{{4, 1, "abcd"}, {2, 1, "abcd"}}, true},
		{"nested", []labelDraw{{5, 0, "0123456789"}, {5, 0, "ab"}}, true},
		{"wide", []labelDraw{{2, 0, "日本"}, {4, 0, "語"}}, true},
		{"zero width", []labelDraw{{0, 0, "\u200b"}, {0, 0, "x"}}, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			before := append([]labelDraw(nil), tt.labels...)
			if got := labelsOverlap(tt.labels); got != tt.overlap {
				t.Fatalf("overlap=%v want %v", got, tt.overlap)
			}
			for i, l := range before {
				if l != tt.labels[i] {
					t.Fatal("overlap check changed paint order")
				}
			}
		})
	}
}

func TestResponsiveDirectionsAndMarkers(t *testing.T) {
	body := "\nA[Alpha] o--x B[Beta]\nB ==> C[Gamma]\nC -.-> D[Delta]"
	for _, tt := range []struct{ dir, vertical, arrow string }{
		{"LR", "TD", "v"}, {"RL", "BT", "^"}, {"TD", "TD", "v"}, {"BT", "BT", "^"},
	} {
		t.Run(tt.dir, func(t *testing.T) {
			src := "flowchart " + tt.dir + body
			out := renderAt(t, src, 40)
			assertFits(t, out, 40)
			// Rotation changes layout, not edge semantics. The existing vertical
			// layout is the oracle for endpoint ownership and every route cell.
			want := mustRender(t, "flowchart "+tt.vertical+body)
			if out != want {
				t.Fatalf("wrong rotation/markers:\ngot:\n%s\nwant:\n%s", out, want)
			}
			for _, mark := range []string{"o", "x", "#", ":", tt.arrow} {
				if !strings.Contains(out, mark) {
					t.Fatalf("lost marker/style %q:\n%s", mark, out)
				}
			}
			if got := renderAt(t, src, 1000); got != mustRender(t, src) {
				t.Fatal("fitting orientation changed")
			}
			if got := renderAt(t, src, 40); got != out {
				t.Fatal("rendering at a different width mutated source state")
			}
		})
	}
	// Cycle breaking reverses layout edges internally but must preserve markers.
	for _, dir := range []string{"LR", "RL", "TD", "BT"} {
		src := "flowchart " + dir + "\nA[Alpha] --> B[Beta]\nB -.-> C[Gamma]\nC ==> A"
		vertical := dir
		if dir == "LR" {
			vertical = "TD"
		}
		if dir == "RL" {
			vertical = "BT"
		}
		out := renderAt(t, src, 20)
		want := mustRender(t, strings.Replace(src, "flowchart "+dir, "flowchart "+vertical, 1))
		if out != want {
			t.Fatalf("cycle direction %s changed:\ngot:\n%s\nwant:\n%s", dir, out, want)
		}
	}
}

func TestResponsiveListPreservesGraph(t *testing.T) {
	body := `\nB["Beta<br/>two"]
A["Alpha with a long node label"]
C["Isolated 日本語"]
B -->|one| A
A -.->|back| B
B <==>|parallel| A
B o--x A
B --- A
B -.- A
B === A
A -.->|self dotted| A
A ==>|self thick| A
B ~~~ C`
	body = strings.TrimPrefix(body, `\n`)
	for _, dir := range []string{"LR", "RL", "TD", "BT"} {
		src := "flowchart " + dir + "\n" + body
		for _, width := range []int{1, 2, 8, 20, 40, 60, 80, 100, 120, 1000} {
			t.Run(fmt.Sprintf("%s/%d", dir, width), func(t *testing.T) {
				out := renderAt(t, src, width)
				assertFits(t, out, width)
				prefix := "- "
				if width < minFlowchartWidth {
					prefix = ""
				}
				want := "Flowchart shown as a list to fit the display.\n\nNodes:\n" +
					prefix + "B: Beta\ntwo\n" + prefix + "A: Alpha with a long node label\n" + prefix + "C: Isolated 日本語\n\nConnections:\n" +
					prefix + "B --> A: one\n" + prefix + "A -.-> B: back\n" + prefix + "B <==> A: parallel\n" +
					prefix + "B o--x A\n" + prefix + "B --- A\n" + prefix + "B -.- A\n" + prefix + "B === A\n" +
					prefix + "A -.-> A: self dotted\n" + prefix + "A ==> A: self thick\n"
				if compactText(out) != compactText(want) {
					t.Fatalf("list lost source ordering/connectivity/labels/styles:\ngot:\n%s\nwant (unwrapped):\n%s", out, want)
				}
				if renderAt(t, src, width) != out {
					t.Fatal("nondeterministic list")
				}
			})
		}
	}
}

func TestResponsiveSelfLoopsAndParallelLabels(t *testing.T) {
	for _, dir := range []string{"LR", "RL", "TD", "BT"} {
		for _, body := range []string{
			"A -->|first| A\nA -->|second| A",
			"A -->|same| A\nA -->|same| A",
			"A -->|same| B\nA -->|same| B",
			"A -->|first| A\nB -->|second| A",
		} {
			src := "flowchart " + dir + "\n" + body
			out := renderAt(t, src, 120)
			assertFits(t, out, 120)
			for _, label := range []string{"same", "first", "second"} {
				if got, want := strings.Count(out, label), strings.Count(body, label); got != want {
					t.Fatalf("%s label %q count %d want %d:\n%s", dir, label, got, want, out)
				}
			}
		}
		src := "flowchart " + dir + "\nA --> A"
		if renderAt(t, src, 120) != mustRender(t, src) {
			t.Fatal("simple self loop changed")
		}
	}
}

func TestResponsiveRejectsClippedLabel(t *testing.T) {
	src := "flowchart TD\nA -->|this label extends left of the canvas| B"
	out := renderAt(t, src, 120)
	if !strings.Contains(out, "Nodes:") || !strings.Contains(out, "this label extends left of the canvas") {
		t.Fatalf("accepted clipped label:\n%s", out)
	}
}

func TestResponsiveListBudgets(t *testing.T) {
	// Huge identifiers can be cheap to draw but expensive to repeat in a list.
	id := strings.Repeat("a", 3000)
	src := "flowchart TD\n" + id + "[x]\n" + strings.Repeat(id+" --> "+id+"\n", 5)
	for _, width := range []int{1, 120, int(^uint(0) >> 1)} {
		out, err := Render(src, Options{Width: width})
		if width == 1 {
			if out != "" || !errors.Is(err, ErrLimitExceeded) {
				t.Fatalf("height budget got %d bytes, %v", len(out), err)
			}
		} else if err != nil {
			t.Fatal(err)
		} else {
			assertFits(t, out, min(width, maxCanvasDimension))
		}
	}
	for _, tt := range []struct {
		name         string
		text         string
		count, width int
	}{
		{"height", "x", maxCanvasDimension + 1, 1},
		{"area", strings.Repeat("x", 1000), 1001, 1000},
		{"bytes", strings.Repeat("\u200b", MaxSourceBytes/3), 64, 120},
	} {
		t.Run(tt.name, func(t *testing.T) {
			w := &listWriter{width: tt.width}
			for i := 0; i < tt.count; i++ {
				w.text(tt.text)
			}
			if !errors.Is(w.err, ErrLimitExceeded) || w.b.Len() > 4*maxCanvasCells {
				t.Fatalf("writer exceeded %s budget: rows %d bytes %d err %v", tt.name, w.rows, w.b.Len(), w.err)
			}
		})
	}
	if out, err := addResponsiveTitle("title", strings.Repeat("x\n", maxCanvasDimension-1), 40); out != "" || !errors.Is(err, ErrLimitExceeded) {
		t.Fatal("title must count toward height budget")
	}
}
