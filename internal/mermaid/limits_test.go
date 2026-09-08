package mermaid

import (
	"errors"
	"fmt"
	"runtime"
	"strings"
	"testing"
)

func nodeList(prefix string, count int) string {
	names := make([]string, count)
	for i := range names {
		names[i] = fmt.Sprintf("%s%d", prefix, i)
	}
	return strings.Join(names, " & ")
}

func TestRenderResourceLimits(t *testing.T) {
	var participants, nodes, classes strings.Builder
	for i := 0; i < 500; i++ {
		fmt.Fprintf(&participants, "participant P%d\n", i)
		fmt.Fprintf(&nodes, "state S%d\n", i)
		fmt.Fprintf(&classes, "class C%d\n", i)
	}
	var sequenceArea strings.Builder
	sequenceArea.WriteString("sequenceDiagram\n")
	for i := 0; i < 100; i++ {
		fmt.Fprintf(&sequenceArea, "participant P%d\n", i)
	}
	sequenceArea.WriteString(strings.Repeat("P0->>P99: msg\n", 300))
	for _, tt := range []struct{ name, source string }{
		{"source", "flowchart TD\nA\n%%" + strings.Repeat("x", MaxSourceBytes)},
		{"cartesian review reproducer", "flowchart TD\n" + nodeList("A", 500) + " --> " + nodeList("B", 500)},
		{"cartesian below node limit", "flowchart TD\n" + nodeList("A", 33) + " --> " + nodeList("B", 33)},
		{"duplicate node list", "flowchart TD\n" + strings.Repeat("A & ", maxNodes) + "A --> B"},
		{"flow nodes", "flowchart TD\n" + nodeList("A", maxNodes+1)},
		{"flow chain", "flowchart TD\n" + strings.Repeat("A --> ", maxEdges+1) + "A"},
		{"state nodes", "stateDiagram-v2\n" + nodes.String()},
		{"class nodes", "classDiagram\n" + classes.String()},
		{"state edges", "stateDiagram-v2\n" + strings.Repeat("A --> B\n", maxEdges+1)},
		{"class edges", "classDiagram\n" + strings.Repeat("A --> B\n", maxEdges+1)},
		{"trapezoid review reproducer", "flowchart TD\nA[/" + strings.Repeat("x<br>", 3000) + "x\\]"},
		{"trapezoid area", "flowchart TD\nA[/" + strings.Repeat("x<br>", 800) + "x\\]"},
		{"sequence review reproducer", "sequenceDiagram\n" + participants.String()},
		{"sequence area", sequenceArea.String()},
		{"sequence events", "sequenceDiagram\n" + strings.Repeat("A->>B\n", maxEvents+1)},
		{"sequence block events", "sequenceDiagram\nparticipant A\n" + strings.Repeat("alt\n", maxEvents+1)},
		{"sequence note list", "sequenceDiagram\nnote over " + strings.Repeat("A,", maxNodes) + "A: note"},
		{"state nesting", "stateDiagram-v2\n" + strings.Repeat("state A {\n", maxNodes+1)},
	} {
		t.Run(tt.name, func(t *testing.T) {
			out, err := Render(tt.source, Options{Width: maxCanvasDimension})
			if !errors.Is(err, ErrLimitExceeded) || out != "" {
				t.Fatalf("Render returned %d bytes, error %v; want empty output and limit error", len(out), err)
			}
		})
	}
}

func TestSourceLimitBoundary(t *testing.T) {
	source := "flowchart TD\nA\n%%"
	source += strings.Repeat("x", MaxSourceBytes-len(source))
	if _, err := Render(source, Options{Width: maxCanvasDimension}); err != nil {
		t.Fatal(err)
	}
	if Detect(source) != KindFlowchart || Detect(source+"x") != KindUnknown {
		t.Fatal("Detect must share the source preprocessing budget")
	}
}

// Non-flowchart diagrams use unwrapped titles.
func TestUnwrappedTitleBudget(t *testing.T) {
	var chain strings.Builder
	chain.WriteString("stateDiagram-v2\n")
	for i := 0; i < 199; i++ {
		fmt.Fprintf(&chain, "A%d --> A%d\n", i, i+1)
	}
	for _, tt := range []struct {
		name, diagram string
		width         int
		wantErr       bool
	}{
		{"width boundary", "stateDiagram-v2\nstate A", maxCanvasDimension, false},
		{"width overflow", "stateDiagram-v2\nstate A", maxCanvasDimension + 1, true},
		{"combined area", chain.String(), maxCanvasDimension, true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			source := "---\ntitle: " + strings.Repeat("x", tt.width) + "\n---\n" + tt.diagram
			out, err := Render(source, Options{Width: maxCanvasDimension})
			if (err != nil) != tt.wantErr || (err != nil && out != "") {
				t.Fatalf("Render returned %d bytes, error %v; wantErr=%v", len(out), err, tt.wantErr)
			}
		})
	}
}

func TestTitleHeightBudget(t *testing.T) {
	if _, err := addTitle("title", strings.Repeat("x\n", maxCanvasDimension-2)); err != nil {
		t.Fatal(err)
	}
	if out, err := addTitle("title", strings.Repeat("x\n", maxCanvasDimension-1)); err == nil || out != "" {
		t.Fatal("title rows must count toward the height budget")
	}
}

func TestStateScopeCacheInvalidation(t *testing.T) {
	lines, _ := preprocess(`stateDiagram-v2
state A {
[*] --> X
[*] --> X
--
[*] --> Y
state "nested" as B {
[*] --> Z
}
Z --> [*]
}
[*] --> A
`)
	g, err := parseState(lines)
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"__start:A", "__start:A#1", "__start:A#1/B", "__end:A#1", "__start"} {
		if g.index[id] == nil {
			t.Errorf("missing pseudo-state %q after scope change", id)
		}
	}
	if g.edges[0].from != g.edges[1].from {
		t.Fatal("same-scope transitions must reuse their pseudo-state")
	}
}

func TestParserBudgetBoundaries(t *testing.T) {
	g := newGraph()
	for i := 0; i < maxNodes; i++ {
		if _, err := g.node(fmt.Sprint(i)); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := g.node("0"); err != nil {
		t.Fatal("existing node must not consume budget:", err)
	}
	if _, err := g.node("overflow"); err == nil || len(g.nodes) != maxNodes {
		t.Fatal("node limit must reject before allocation")
	}
	for i := 0; i < maxEdges; i++ {
		if err := g.addEdge(&gedge{from: g.nodes[0], to: g.nodes[1]}); err != nil {
			t.Fatal(err)
		}
	}
	if err := g.addEdge(&gedge{}); err == nil || len(g.edges) != maxEdges {
		t.Fatal("edge limit must reject before insertion")
	}
	d := &seqDiagram{byName: map[string]*seqPart{}}
	for i := 0; i < maxNodes; i++ {
		if _, err := d.part(fmt.Sprint(i)); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := d.part("0"); err != nil {
		t.Fatal(err)
	}
	if _, err := d.part("overflow"); err == nil || len(d.parts) != maxNodes {
		t.Fatal("participant limit")
	}
	for i := 0; i < maxEvents; i++ {
		if err := d.addEvent(&seqEvent{}); err != nil {
			t.Fatal(err)
		}
	}
	if err := d.addEvent(&seqEvent{}); err == nil || len(d.events) != maxEvents {
		t.Fatal("event limit")
	}
}

func TestCartesianBudgetBeforeExpansion(t *testing.T) {
	g := newGraph()
	if err := parseFlowStmt(g, nodeList("A", 32)+" --> "+nodeList("B", 32)); err != nil {
		t.Fatal(err)
	}
	if len(g.edges) != maxEdges {
		t.Fatalf("got %d edges", len(g.edges))
	}
	if err := parseFlowStmt(g, "A0 --> B0"); err == nil || len(g.edges) != maxEdges {
		t.Fatal("remaining edge budget")
	}
	g = newGraph()
	if err := parseFlowStmt(g, nodeList("A", 33)+" --> "+nodeList("B", 33)); err == nil {
		t.Fatal("expected Cartesian limit")
	}
	if len(g.edges) != 0 {
		t.Fatalf("allocated %d edges before rejecting expansion", len(g.edges))
	}
}

func TestVirtualBudgetBeforeExpansion(t *testing.T) {
	for _, excess := range []int{0, 1} {
		g := newGraph()
		a, _ := g.node("a")
		b, _ := g.node("b")
		b.rank = maxVirtualNodes + 1 + excess
		if err := g.addEdge(&gedge{from: a, to: b}); err != nil {
			t.Fatal(err)
		}
		err := g.insertVirtuals()
		if excess == 0 {
			if err != nil || len(g.nodes) != maxVirtualNodes+2 {
				t.Fatalf("boundary: nodes=%d err=%v", len(g.nodes), err)
			}
		} else if err == nil || len(g.nodes) != 2 || len(g.edges[0].path) != 0 {
			t.Fatalf("oversized virtual expansion was not atomic: nodes=%d err=%v", len(g.nodes), err)
		}
	}
	// A legal-sized real graph can still require too many virtual nodes.
	var source strings.Builder
	source.WriteString("flowchart TD\n")
	for i := 0; i < 100; i++ {
		fmt.Fprintf(&source, "N%d --> N%d\n", i, i+1)
	}
	for i := 2; i <= 100; i++ {
		fmt.Fprintf(&source, "N0 --> N%d\n", i)
	}
	if out, err := Render(source.String(), Options{Width: maxCanvasDimension}); err == nil || out != "" {
		t.Fatal("Render must propagate virtual node limit")
	}
}

func TestCanvasBudgetBeforeAllocation(t *testing.T) {
	for _, draw := range []struct {
		name string
		f    func(*canvas)
	}{
		{"width", func(c *canvas) { c.put(maxCanvasDimension, 0, 'x') }},
		{"height", func(c *canvas) { c.put(0, maxCanvasDimension, 'x') }},
		{"area", func(c *canvas) { c.put(1000, 999, 'x') }},
		{"horizontal line", func(c *canvas) { c.hline(0, maxCanvasDimension, 0, '-') }},
		{"vertical line", func(c *canvas) { c.vline(0, maxCanvasDimension, 0, '|') }},
		{"clear", func(c *canvas) { c.clear(0, 0, 1001, 1000) }},
		{"wide text", func(c *canvas) { c.text(0, 0, strings.Repeat("界", maxCanvasDimension/2+1)) }},
	} {
		t.Run(draw.name, func(t *testing.T) {
			c := &canvas{}
			draw.f(c)
			c.put(0, 0, 'x') // failure must remain sticky
			if out, err := c.result(); err == nil || out != "" || len(c.rows) != 0 {
				t.Fatalf("allocated before rejecting: rows=%d err=%v", len(c.rows), err)
			}
		})
	}
	for _, size := range [][2]int{{maxCanvasDimension, 1}, {1, maxCanvasDimension}, {1000, 1000}} {
		c := &canvas{}
		c.put(size[0]-1, size[1]-1, 'x')
		if _, err := c.result(); err != nil {
			t.Fatal(err)
		}
	}
	c := &canvas{}
	c.put(999, 999, 'x')
	c.put(1000, 0, 'x')
	if c.err == nil || len(c.rows[0]) != 0 {
		t.Fatal("combined bounding rectangle must be checked before growing a different row")
	}
}

func TestFlowByteCursorUnicode(t *testing.T) {
	g := newGraph()
	if err := parseFlowStmt(g, "日本-語[開始] -->|経路| 終了((完了)) --> A"); err != nil {
		t.Fatal(err)
	}
	if len(g.nodes) != 3 || len(g.edges) != 2 || g.nodes[0].id != "日本-語" || g.edges[0].label != "経路" || g.nodes[1].lines[0] != "完了" {
		t.Fatal("byte cursor lost UTF-8 boundaries")
	}
}

func longStateIdentifierSource(id string) string {
	return "stateDiagram-v2\nstate \"x\" as " + id + " {\n" + strings.Repeat("[*]-->X\n", 1024)
}

func TestGraphLongIdentifierAllocations(t *testing.T) {
	want, err := Render(longStateIdentifierSource("A"), Options{Width: maxCanvasDimension})
	if err != nil {
		t.Fatal(err)
	}
	lines, _ := preprocess(longStateIdentifierSource(strings.Repeat("A", 48*1024)))
	g, err := parseState(lines)
	if err != nil {
		t.Fatal(err)
	}
	// Measure layout alone: pair bookkeeping must not copy identifiers for
	// each edge or repeated segment pass. Leave ample headroom for the canvas.
	runtime.GC()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	got, err := g.render()
	runtime.ReadMemStats(&after)
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatal("identifier length changed the rendered diagram")
	}
	const maxLayoutBytes = 16 << 20
	if allocated := after.TotalAlloc - before.TotalAlloc; allocated > maxLayoutBytes {
		t.Fatalf("layout allocated %d bytes; budget is %d", allocated, maxLayoutBytes)
	}
}

func BenchmarkRenderLongStateIdentifier(b *testing.B) {
	source := longStateIdentifierSource(strings.Repeat("A", 48*1024))
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := Render(source, Options{Width: maxCanvasDimension}); err != nil {
			b.Fatal(err)
		}
	}
}

// Benchmark parsing rather than layout; repeated references keep the node count
// fixed. Suffixes must remain string views, not fresh rune-to-string conversions.
func BenchmarkFlowChain(b *testing.B) {
	for _, edges := range []int{128, 512, maxEdges} {
		b.Run(fmt.Sprint(edges), func(b *testing.B) {
			source := strings.Repeat("A --> ", edges) + "A"
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				if err := parseFlowStmt(newGraph(), source); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
