package mermaid

import (
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"unicode"
	"unicode/utf8"
)

func responsiveFixture(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile("testdata/responsive/" + name + ".mmd")
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func renderAt(t *testing.T, source string, width int) string {
	t.Helper()
	out, err := Render(source, Options{Width: width})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func assertFits(t *testing.T, out string, width int) {
	t.Helper()
	if !utf8.ValidString(out) || !strings.HasSuffix(out, "\n") {
		t.Fatalf("invalid output: %q", out)
	}
	for i, line := range strings.Split(strings.TrimSuffix(out, "\n"), "\n") {
		if w := dispWidth(line); w > width && !(width == 1 && w == 2) {
			t.Fatalf("row %d has %d columns, budget %d: %q\n%s", i+1, w, width, line, out)
		}
	}
}

func compactText(s string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsSpace(r) {
			return -1
		}
		return r
	}, s)
}

// Labels may wrap, but each original node's words and every full edge label
// must survive. At 120 columns these fixtures must remain actual diagrams.
func TestResponsiveFixtures(t *testing.T) {
	for _, name := range []string{"roadmap", "publishing", "processing"} {
		src := responsiveFixture(t, name)
		pristine := parseFlow(t, src)
		// Generic examples must retain their regression roles: an already
		// fitting roadmap, a wide horizontal fan-in, and a wide vertical graph.
		natural := parseFlow(t, src)
		raw, err := natural.render()
		if err != nil {
			t.Fatal(err)
		}
		naturalWidth := maxLineLen(strings.Split(raw, "\n"))
		if (naturalWidth <= 120) != (name == "roadmap") {
			t.Fatalf("%s: natural width %d no longer exercises the intended layout", name, naturalWidth)
		}
		if name == "publishing" && !natural.labelOverlap {
			t.Fatal("publishing example must exercise the labeled fan-in collision")
		}
		for _, width := range []int{120, 100, 80, 60, 40, 1000, 120} {
			t.Run(fmt.Sprintf("%s/%d", name, width), func(t *testing.T) {
				out := renderAt(t, src, width)
				assertFits(t, out, width)
				if out != renderAt(t, src, width) {
					t.Fatal("nondeterministic rendering")
				}
				list := strings.Contains(out, "Nodes:")
				if width == 120 && list {
					t.Fatalf("expected diagram, got list:\n%s", out)
				}
				if name == "roadmap" && width == 120 && out != mustRender(t, src) {
					t.Fatal("first diagram changed")
				}
				if width == 120 {
					// Reviewed snapshots cover arrow positions, shared routes and
					// dotted future links in addition to the text assertions below.
					want, err := os.ReadFile("testdata/responsive/" + name + ".txt")
					if err != nil {
						t.Fatal(err)
					}
					if out != string(want) {
						t.Fatalf("diagram routes changed:\n%s", out)
					}
				}
				t.Logf("display columns: %d", maxLineLen(strings.Split(out, "\n")))
				for _, n := range pristine.nodes {
					label := strings.Join(n.lines, "\n")
					found := false
					if list {
						found = strings.Contains(compactText(out), compactText(n.id+": "+label))
					} else {
						for _, cap := range []int{MaxSourceBytes, 32, 24, 16} {
							all := true
							for _, fragment := range wrapFlowText(label, cap) {
								all = all && strings.Contains(out, fragment)
							}
							found = found || all
						}
					}
					if !found {
						t.Errorf("missing node %s label %q:\n%s", n.id, label, out)
					}
				}
				counts := map[string]int{}
				for _, e := range pristine.edges {
					if e.label != "" {
						counts[e.label]++
					}
				}
				for label, want := range counts {
					hay, needle := out, label
					if list {
						hay, needle = compactText(out), compactText(label)
					}
					if got := strings.Count(hay, needle); got != want {
						t.Errorf("label %q count=%d want=%d:\n%s", label, got, want, out)
					}
				}
				if name == "processing" && !list && !strings.Contains(out, ":") {
					t.Fatal("dotted links lost")
				}
			})
		}
	}
}

func TestResponsiveLabeledFanIn(t *testing.T) {
	src := "flowchart LR\nA -->|merges| C\nB -->|contains selected examples| C"
	g := parseFlow(t, src)
	if _, err := g.render(); err != nil {
		t.Fatal(err)
	}
	if !g.labelOverlap {
		t.Fatal("horizontal labeled fan-in overlap was not detected")
	}
	for _, width := range []int{120, 1000} {
		out := renderAt(t, src, width)
		assertFits(t, out, width)
		for _, label := range []string{"merges", "contains selected examples"} {
			if !strings.Contains(out, label) {
				t.Fatalf("lost %q at %d:\n%s", label, width, out)
			}
		}
		// The tiny boxes can also clip centered vertical labels. A list is
		// preferable to accepting either destructive placement just for width.
	}
}

func TestResponsiveWidthBoundaries(t *testing.T) {
	src := "flowchart LR\nA[Alphabet] --> B[Beta]"
	for _, width := range []int{0, -1, -100} {
		out, err := Render(src, Options{Width: width})
		if out != "" || err == nil || !strings.Contains(err.Error(), "width must be positive") {
			t.Fatalf("width %d: got %q, %v; expected invalid width error", width, out, err)
		}
	}
	natural := mustRender(t, src)
	width := maxLineLen(strings.Split(natural, "\n"))
	if got := renderAt(t, src, width); got != natural {
		t.Fatal("exact fit changed")
	}
	assertFits(t, renderAt(t, src, width-1), width-1)
	if got := renderAt(t, src, int(^uint(0)>>1)); got != natural {
		t.Fatal("huge width changed output")
	}
	for _, src := range []string{"sequenceDiagram\nA->>B: a very long message", "stateDiagram-v2\nA --> B", "classDiagram\nA <|-- B"} {
		if renderAt(t, src, 1) != mustRender(t, src) {
			t.Fatal("non-flowchart adapted")
		}
	}
}

func TestResponsiveWrapText(t *testing.T) {
	for _, tt := range []struct {
		text  string
		width int
		want  string
	}{
		{"one two three", 7, "one two\nthree"},
		{"abcdefghijkl", 5, "abcde\nfghij\nkl"},
		{"first\n\nsecond", 5, "first\n\nsecon\nd"},
		{"日本語日本語", 5, "日本\n語日\n本語"},
		{"e\u0301e\u0301e\u0301", 2, "e\u0301e\u0301\ne\u0301"},
		{"a\u200bb", 1, "a\u200b\nb"},
		{"界", 1, "界"},
	} {
		got := strings.Join(wrapFlowText(tt.text, tt.width), "\n")
		if got != tt.want {
			t.Errorf("wrap(%q,%d)=%q want %q", tt.text, tt.width, got, tt.want)
		}
	}
}

func TestResponsiveTitlesAndUnicode(t *testing.T) {
	for _, title := range []string{"Short", "A title wider than its diagram"} {
		src := "---\ntitle: " + title + "\n---\nflowchart TD\nA[Alphabet]"
		if got := renderAt(t, src, 120); got != mustRender(t, src) {
			t.Fatal("fitting title changed with width")
		}
	}
	for _, width := range []int{120, 40, 20, 8, 2, 1} {
		src := "---\ntitle: A wide title 日本語 repeated for the display\n---\nflowchart TD\nA[\"日本語日本語日本語<br/>abcdefghijklmnopqrstuvwxyz0123456789\"]"
		out := renderAt(t, src, width)
		assertFits(t, out, width)
		if !strings.Contains(compactText(out), "Awidetitle日本語repeatedforthedisplay") {
			t.Fatalf("title lost:\n%s", out)
		}
	}
}

func TestResponsiveTitleBudgets(t *testing.T) {
	var chain strings.Builder
	chain.WriteString("flowchart TD\n")
	for i := 0; i < 199; i++ {
		fmt.Fprintf(&chain, "A%d --> A%d\n", i, i+1)
	}
	for _, tt := range []struct {
		name, diagram     string
		titleWidth, width int
		wantErr           bool
	}{
		{"boundary", "flowchart TD\nA", maxCanvasDimension, 40, false},
		{"wrap overwide title", "flowchart TD\nA", maxCanvasDimension + 1, 40, false},
		{"huge width", "flowchart TD\nA", maxCanvasDimension + 1, int(^uint(0) >> 1), false},
		{"wrap title above chain", chain.String(), maxCanvasDimension, 120, false},
		{"wrapped height limit", "flowchart TD\nA", maxCanvasDimension, 1, true},
		{"combined cell limit", chain.String(), maxCanvasDimension, maxCanvasDimension, true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			src := "---\ntitle: " + strings.Repeat("x", tt.titleWidth) + "\n---\n" + tt.diagram
			out, err := Render(src, Options{Width: tt.width})
			if tt.wantErr {
				if out != "" || !errors.Is(err, ErrLimitExceeded) {
					t.Fatalf("got %d bytes, %v; expected wrapped title budget failure", len(out), err)
				}
			} else if err != nil {
				t.Fatal(err)
			} else {
				assertFits(t, out, tt.width)
			}
		})
	}
}

func TestResponsiveWrapProperties(t *testing.T) {
	for _, text := range []string{
		"日本語 e\u0301e\u0301 a\u200bb " + strings.Repeat("abcdefghijkl", 8),
		"\u200b界\u0301a\u0301\u200b語",
		"word  boundary\nexplicit\n\n日本語",
	} {
		for width := 1; width <= 40; width++ {
			out := strings.Join(wrapFlowText(text, width), "\n") + "\n"
			assertFits(t, out, width)
			if compactText(out) != compactText(text) {
				t.Fatalf("wrapping lost runes: %q -> %q", text, out)
			}
		}
	}
}

func TestResponsiveResourceErrors(t *testing.T) {
	for _, src := range []string{
		"flowchart TD\nA\n%%" + strings.Repeat("x", MaxSourceBytes),
		"flowchart TD\n" + nodeList("A", maxNodes+1),
		"flowchart TD\n" + strings.Repeat("A --> B\n", maxEdges+1),
		"flowchart TD\nA[" + strings.Repeat("x", maxCanvasDimension) + "]",
	} {
		for _, width := range []int{1, 40, 120} {
			out, err := Render(src, Options{Width: width})
			if out != "" || !errors.Is(err, ErrLimitExceeded) {
				t.Fatalf("width=%d got %d bytes, %v", width, len(out), err)
			}
		}
	}
	for _, src := range []string{"pie\nA", "flowchart TD\nA[", "flowchart TD"} {
		out, err := Render(src, Options{Width: 40})
		if out != "" || err == nil || errors.Is(err, ErrLimitExceeded) {
			t.Fatalf("invalid source returned %q, %v", out, err)
		}
	}
}
