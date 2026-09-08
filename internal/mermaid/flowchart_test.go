package mermaid

import (
	"strings"
	"testing"
)

func parseFlow(t *testing.T, src string) *graph {
	t.Helper()
	lines, _ := preprocess(src)
	g, err := parseFlowchart(lines)
	if err != nil {
		t.Fatalf("parseFlowchart: %v", err)
	}
	return g
}

func TestFlowchartShapes(t *testing.T) {
	src := `flowchart TD
    a[rect]
    b(round)
    c([stadium])
    d[[subroutine]]
    e[(cylinder)]
    f((circle))
    g{diamond}
    h{{hex}}
    i>flag]
    j[/parallelogram/]
    k[\alt parallelogram\]
    l[/trapezoid\]
    m[\alt trapezoid/]`
	g := parseFlow(t, src)
	want := map[string]boxKind{
		"a": boxRect, "b": boxRound, "c": boxStadium, "d": boxSubroutine,
		"e": boxCylinder, "f": boxCircle, "g": boxDiamond, "h": boxHex, "i": boxAsym,
		"j": boxParallelogram, "k": boxParallelogramAlt,
		"l": boxTrapezoid, "m": boxTrapezoidAlt,
	}
	for id, kind := range want {
		n := g.index[id]
		if n == nil {
			t.Fatalf("node %q missing", id)
		}
		if n.kind != kind {
			t.Errorf("node %q kind = %v, want %v", id, n.kind, kind)
		}
	}
	if got := g.index["g"].lines[0]; got != "diamond" {
		t.Errorf("label = %q, want diamond", got)
	}
}

func TestFlowchartLinks(t *testing.T) {
	tests := []struct {
		src    string
		line   lineKind
		sm, em marker
		label  string
	}{
		{"a --> b", lineSolid, mNone, mArrow, ""},
		{"a --- b", lineSolid, mNone, mNone, ""},
		{"a -.-> b", lineDotted, mNone, mArrow, ""},
		{"a -.- b", lineDotted, mNone, mNone, ""},
		{"a ==> b", lineThick, mNone, mArrow, ""},
		{"a === b", lineThick, mNone, mNone, ""},
		{"a --x b", lineSolid, mNone, mCross, ""},
		{"a --o b", lineSolid, mNone, mDiamondOpen, ""},
		{"a <--> b", lineSolid, mArrow, mArrow, ""},
		{"a -->|lbl| b", lineSolid, mNone, mArrow, "lbl"},
		{"a -- lbl --> b", lineSolid, mNone, mArrow, "lbl"},
		{"a -. lbl .-> b", lineDotted, mNone, mArrow, "lbl"},
		{"a == lbl ==> b", lineThick, mNone, mArrow, "lbl"},
		{"a ~~~ b", lineNone, mNone, mNone, ""},
	}
	for _, tt := range tests {
		g := parseFlow(t, "graph TD\n"+tt.src)
		if len(g.edges) != 1 {
			t.Fatalf("%q: %d edges, want 1", tt.src, len(g.edges))
		}
		e := g.edges[0]
		if e.line != tt.line || e.sm != tt.sm || e.em != tt.em || e.label != tt.label {
			t.Errorf("%q: got line=%v sm=%v em=%v label=%q, want line=%v sm=%v em=%v label=%q",
				tt.src, e.line, e.sm, e.em, e.label, tt.line, tt.sm, tt.em, tt.label)
		}
		if e.from.id != "a" || e.to.id != "b" {
			t.Errorf("%q: endpoints %s->%s", tt.src, e.from.id, e.to.id)
		}
	}
}

func TestFlowchartAmpersandAndChain(t *testing.T) {
	g := parseFlow(t, "graph TD\n A & B --> C & D --> E")
	// A->C, A->D, B->C, B->D, C->E, D->E
	if len(g.edges) != 6 {
		t.Fatalf("%d edges, want 6", len(g.edges))
	}
}

func TestFlowchartStatementsPerLine(t *testing.T) {
	g := parseFlow(t, "graph LR; a-->b; b-->c")
	if len(g.edges) != 2 {
		t.Fatalf("%d edges, want 2", len(g.edges))
	}
	if g.dir != "LR" {
		t.Errorf("dir = %q, want LR", g.dir)
	}
}

func TestFlowchartSubgraphIgnored(t *testing.T) {
	src := `flowchart TD
    subgraph one
      a --> b
    end
    b --> c
    style a fill:#f9f
    classDef cls fill:#f96
    click a href "https://example.com"`
	g := parseFlow(t, src)
	if len(g.edges) != 2 {
		t.Fatalf("%d edges, want 2", len(g.edges))
	}
}

func TestFlowchartDashedIDs(t *testing.T) {
	g := parseFlow(t, "graph TD\n node-1 --> node-2")
	if g.index["node-1"] == nil || g.index["node-2"] == nil {
		t.Fatalf("dashed ids not parsed: %v", g.index)
	}
	if len(g.edges) != 1 {
		t.Fatalf("%d edges, want 1", len(g.edges))
	}
}

func TestFlowchartQuotedAndBrLabels(t *testing.T) {
	g := parseFlow(t, "graph TD\n a[\"has [brackets]\"] --> b[\"two<br/>lines\"]")
	if got := g.index["a"].lines[0]; got != "has [brackets]" {
		t.Errorf("quoted label = %q", got)
	}
	if got := g.index["b"].lines; len(got) != 2 || got[0] != "two" || got[1] != "lines" {
		t.Errorf("br label = %v", got)
	}
}

// Regression: `[/text/]` and friends used to fall through to the plain `[`
// opener, leaving the slashes in the label text.
func TestFlowchartSlantedShapeLabels(t *testing.T) {
	g := parseFlow(t, `flowchart TD
    a[/Alert fires/] --> b[/Ramp up\]
    b --> c[\Wind down/] --> d[\Batch\]`)
	for id, want := range map[string]string{
		"a": "Alert fires", "b": "Ramp up", "c": "Wind down", "d": "Batch",
	} {
		if got := g.index[id].lines[0]; got != want {
			t.Errorf("node %q label = %q, want %q", id, got, want)
		}
	}
	if len(g.edges) != 3 {
		t.Errorf("%d edges, want 3", len(g.edges))
	}
}

// The slanted shapes shift one column per row, so they are drawn row by row
// rather than against fixed left and right borders.
func TestRenderSlantedShapes(t *testing.T) {
	tests := []struct {
		src, want string
	}{
		{"flowchart TD\n  a[/In/]", strings.Join([]string{
			"   ____",
			" / In /",
			"/____/",
			"",
		}, "\n")},
		{"flowchart TD\n  b[/Out\\]", strings.Join([]string{
			"   _____",
			" /  Out  \\",
			"/_________\\",
			"",
		}, "\n")},
	}
	for _, tt := range tests {
		if got := mustRender(t, tt.src); got != tt.want {
			t.Errorf("%s\ngot:\n%s\nwant:\n%s", tt.src, got, tt.want)
		}
	}
}

func TestFlowchartUnclosedShape(t *testing.T) {
	lines, _ := preprocess("flowchart TD\n  a[/never closed")
	if _, err := parseFlowchart(lines); err == nil {
		t.Error("expected an error for an unclosed shape")
	}
}

// Regression: the `;` statement separator used to split HTML entities such as
// `&amp;` in two, leaving an unclosed label and failing the whole parse.
func TestFlowchartEntitySemicolons(t *testing.T) {
	g := parseFlow(t, `graph TD
    a["a &amp; b"] --> b["x &lt;tag&gt; y"]
    b --> c["#35; hash"]`)
	if len(g.edges) != 2 {
		t.Fatalf("%d edges, want 2", len(g.edges))
	}
	for id, want := range map[string]string{
		"a": "a & b", "b": "x <tag> y", "c": "# hash",
	} {
		n := g.index[id]
		if n == nil {
			t.Fatalf("node %q missing", id)
		}
		if got := n.lines[0]; got != want {
			t.Errorf("node %q label = %q, want %q", id, got, want)
		}
	}
}

// Semicolons still separate statements, including inside quoted labels where
// they are part of the text rather than a separator.
func TestFlowchartSemicolonSeparators(t *testing.T) {
	g := parseFlow(t, `graph TD; a --> b; b --> c`)
	if len(g.edges) != 2 {
		t.Fatalf("%d edges, want 2", len(g.edges))
	}
	g = parseFlow(t, `graph TD
    a["stop; go"] --> b`)
	if len(g.edges) != 1 {
		t.Fatalf("%d edges, want 1", len(g.edges))
	}
	if got := g.index["a"].lines[0]; got != "stop; go" {
		t.Errorf("label = %q, want %q", got, "stop; go")
	}
}

func TestFlowchartUnicodeLabels(t *testing.T) {
	g := parseFlow(t, "graph TD\n A[Ünïcodé ✓] --> B[日本語]")
	if len(g.edges) != 1 {
		t.Fatalf("%d edges, want 1", len(g.edges))
	}
	if got := g.index["A"].lines[0]; got != "Ünïcodé ✓" {
		t.Errorf("label = %q", got)
	}
}

func mustRender(t *testing.T, src string) string {
	t.Helper()
	out, err := Render(src, Options{Width: maxCanvasDimension})
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	return out
}

func TestRenderFlowchartLR(t *testing.T) {
	got := mustRender(t, "flowchart LR\n  A --> B\n  B --> C")
	want := strings.Join([]string{
		"+---+    +---+    +---+",
		"| A |--->| B |--->| C |",
		"+---+    +---+    +---+",
		"",
	}, "\n")
	if got != want {
		t.Errorf("got:\n%s\nwant:\n%s", got, want)
	}
}

func TestRenderFlowchartSharedTracks(t *testing.T) {
	tests := []struct {
		name, src, want string
	}{
		{
			name: "left to right",
			src: `flowchart LR
  A --> Hub
  B --> Hub
  C --> Hub
  Hub --> X
  Hub --> Y
  Hub --> Z`,
			want: strings.Join([]string{
				"                    +---+",
				"                 +->| X |",
				"                 |  +---+",
				"                 |",
				"                 |",
				"+---+    +-----+ |  +---+",
				"| A |-+->| Hub |-+->| Y |",
				"+---+ |  +-----+ |  +---+",
				"      |          |",
				"      |          |",
				"+---+ |          |  +---+",
				"| B |-+          +->| Z |",
				"+---+ |             +---+",
				"      |",
				"      |",
				"+---+ |",
				"| C |-+",
				"+---+",
				"",
			}, "\n"),
		},
		{
			name: "top down",
			src: `flowchart TD
  A --> Hub
  B --> Hub
  C --> Hub
  Hub --> X
  Hub --> Y
  Hub --> Z`,
			want: strings.Join([]string{
				"          +---+     +---+     +---+",
				"          | A |     | B |     | C |",
				"          +---+     +---+     +---+",
				"            |         |         |",
				"            +---------+---------+",
				"            v",
				"         +-----+",
				"         | Hub |",
				"         +-----+",
				"            |",
				"  +---------+---------+",
				"  v         v         v",
				"+---+     +---+     +---+",
				"| X |     | Y |     | Z |",
				"+---+     +---+     +---+",
				"",
			}, "\n"),
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := mustRender(t, tt.src); got != tt.want {
				t.Errorf("got:\n%s\nwant:\n%s", got, tt.want)
			}
		})
	}
}

func TestRouteTracksKeepDestinationsSeparate(t *testing.T) {
	a := &gnode{id: "a", rank: 0, cross: 0}
	b := &gnode{id: "b", rank: 0, cross: 10}
	c := &gnode{id: "c", rank: 0, cross: 20}
	d := &gnode{id: "d", rank: 0, cross: 30}
	x := &gnode{id: "x", rank: 1, cross: 5}
	y := &gnode{id: "y", rank: 1, cross: 25}

	var segs []*gseg
	for _, ends := range [][2]*gnode{{a, x}, {b, x}, {c, y}, {d, y}} {
		e := &gedge{from: ends[0], to: ends[1], line: lineSolid, em: mArrow}
		segs = append(segs, &gseg{
			u: ends[0], w: ends[1], e: e, first: true, last: true,
		})
	}
	idx, tracks := assignRouteTracks(segs, 1, func(s *gseg) bool {
		return s.u.cross != s.w.cross
	})
	if tracks[0] != 2 {
		t.Fatalf("allocated %d tracks, want one for each destination", tracks[0])
	}
	if idx[segs[0]] != idx[segs[1]] {
		t.Error("edges into x did not share a track")
	}
	if idx[segs[2]] != idx[segs[3]] {
		t.Error("edges into y did not share a track")
	}
	if idx[segs[0]] == idx[segs[2]] {
		t.Error("independent destinations shared a track")
	}
}

func TestRenderFlowchartBranchAndCycle(t *testing.T) {
	got := mustRender(t, "flowchart TD\n  A[Start] --> B{OK?}\n  B -->|yes| C[Done]\n  B -->|no| A")
	want := strings.Join([]string{
		"+-------+",
		"| Start |",
		"+-------+",
		"   ^|",
		"no ||",
		"   |v",
		" .-----.",
		" < OK? >",
		" '-----'",
		"    |",
		"   yes",
		"    v",
		"+------+",
		"| Done |",
		"+------+",
		"",
	}, "\n")
	if got != want {
		t.Errorf("got:\n%s\nwant:\n%s", got, want)
	}
}

func TestRenderFlowchartSelfLoop(t *testing.T) {
	got := mustRender(t, "graph TD\n  A --> A")
	want := strings.Join([]string{
		"  +---+",
		"  | A |--.",
		"  +---+<-'",
		"",
	}, "\n")
	if got != want {
		t.Errorf("got:\n%s\nwant:\n%s", got, want)
	}
}

func TestRenderContainsAllNodes(t *testing.T) {
	src := `flowchart TD
    A[Alpha] --> B[Beta] & C[Gamma]
    B --> D{Delta}
    C --> D
    D -->|maybe| E((Epsilon))
    E -.-> A`
	out := mustRender(t, src)
	for _, want := range []string{"Alpha", "Beta", "Gamma", "Delta", "Epsilon", "maybe"} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}
}
