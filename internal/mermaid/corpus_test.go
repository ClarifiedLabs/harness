package mermaid

import (
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// update rewrites the golden files instead of comparing against them:
//
//	go test ./internal/mermaid -run TestCorpus -update
var update = flag.Bool("update", false, "rewrite testdata golden files")

// TestCorpus renders every testdata/*.mmd diagram and compares the result
// with its .golden file. Each source may carry `%% want: <text>` directives,
// which are stripped as comments before rendering and asserted on the output.
func TestCorpus(t *testing.T) {
	srcs, err := filepath.Glob(filepath.Join("testdata", "*.mmd"))
	if err != nil {
		t.Fatalf("glob: %v", err)
	}
	if len(srcs) == 0 {
		t.Fatal("no testdata/*.mmd files found")
	}
	for _, src := range srcs {
		name := strings.TrimSuffix(filepath.Base(src), ".mmd")
		t.Run(name, func(t *testing.T) {
			b, err := os.ReadFile(src)
			if err != nil {
				t.Fatalf("read: %v", err)
			}
			source := string(b)

			got, err := Render(source, Options{Width: maxCanvasDimension})
			if err != nil {
				t.Fatalf("Render: %v", err)
			}
			if strings.TrimSpace(got) == "" {
				t.Fatal("Render produced empty output")
			}

			checkWants(t, source, got)
			checkNoLeakedSyntax(t, got)
			checkCanvasShape(t, got)
			checkLabelsIntact(t, source, got)
			checkDeterministic(t, source, got)

			golden := filepath.Join("testdata", name+".golden")
			if *update {
				if err := os.WriteFile(golden, []byte(got), 0o644); err != nil {
					t.Fatalf("write golden: %v", err)
				}
				return
			}
			want, err := os.ReadFile(golden)
			if err != nil {
				t.Fatalf("read golden (run with -update to create it): %v", err)
			}
			if got != string(want) {
				t.Errorf("output differs from %s\n--- got ---\n%s\n--- want ---\n%s",
					golden, got, want)
			}
		})
	}
}

// checkWants asserts the `%% want: <text>` directives in the source appear in
// the rendered output.
func checkWants(t *testing.T, source, out string) {
	t.Helper()
	n := 0
	for _, line := range strings.Split(source, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "%% want:") {
			continue
		}
		n++
		want := strings.TrimSpace(strings.TrimPrefix(line, "%% want:"))
		if !strings.Contains(out, want) {
			t.Errorf("output is missing %q:\n%s", want, out)
		}
	}
	if n == 0 {
		t.Error("source declares no `%% want:` directives")
	}
}

// checkLabelsIntact asserts every piece of node text the parser produced
// survives into the drawing. Edge routing writes over the canvas, so a label
// clipped or overdrawn by a passing line shows up here.
func checkLabelsIntact(t *testing.T, source, out string) {
	t.Helper()
	lines, _ := preprocess(source)
	var texts []string
	switch detectLines(lines) {
	case KindFlowchart, KindState, KindClass:
		var g *graph
		var err error
		switch detectLines(lines) {
		case KindFlowchart:
			g, err = parseFlowchart(lines)
		case KindState:
			g, err = parseState(lines)
		default:
			g, err = parseClass(lines)
		}
		if err != nil {
			t.Fatalf("parse: %v", err)
		}
		for _, n := range g.nodes {
			if n.virtual {
				continue
			}
			texts = append(texts, n.lines...)
			for _, sec := range n.sections {
				texts = append(texts, sec...)
			}
		}
	case KindSequence:
		d, err := parseSequence(lines)
		if err != nil {
			t.Fatalf("parse: %v", err)
		}
		for _, p := range d.parts {
			texts = append(texts, p.label)
		}
	}
	for _, s := range texts {
		if strings.TrimSpace(s) == "" {
			continue
		}
		if !strings.Contains(out, s) {
			t.Errorf("label %q was clipped or overdrawn:\n%s", s, out)
		}
	}
}

// leaked is markup that must always be consumed by the parser rather than
// drawn: comments, directives, styling statements and HTML escapes.
var leaked = []string{
	"%%{", "%% ", "<br", "&amp;", "&lt;", "&gt;", "&quot;",
	string(wideCont), // the double-width filler must never reach the output
	"classDef ", "linkStyle ", "click ", "accTitle", "accDescr",
	"subgraph ", "cssClass ", "callback ",
}

func checkNoLeakedSyntax(t *testing.T, out string) {
	t.Helper()
	for _, bad := range leaked {
		if strings.Contains(out, bad) {
			t.Errorf("unparsed syntax %q leaked into output:\n%s", bad, out)
		}
	}
}

// checkCanvasShape asserts the drawing is a well-formed block of text: it ends
// with exactly one newline and carries no trailing whitespace.
func checkCanvasShape(t *testing.T, out string) {
	t.Helper()
	if !strings.HasSuffix(out, "\n") {
		t.Error("output does not end with a newline")
	}
	if strings.HasSuffix(out, "\n\n") {
		t.Error("output ends with a blank line")
	}
	for i, line := range strings.Split(strings.TrimSuffix(out, "\n"), "\n") {
		if line != strings.TrimRight(line, " \t") {
			t.Errorf("line %d has trailing whitespace: %q", i+1, line)
		}
	}
}

// checkDeterministic re-renders the source and requires identical output;
// layout uses maps, so ordering bugs surface here.
func checkDeterministic(t *testing.T, source, first string) {
	t.Helper()
	for i := 0; i < 5; i++ {
		got, err := Render(source, Options{Width: maxCanvasDimension})
		if err != nil {
			t.Fatalf("re-render %d: %v", i, err)
		}
		if got != first {
			t.Fatalf("nondeterministic output on run %d:\n--- run %d ---\n%s\n--- run 0 ---\n%s",
				i, i, got, first)
		}
	}
}
