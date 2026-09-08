package mermaid

import (
	"strings"
	"testing"
)

func parseSt(t *testing.T, src string) *graph {
	t.Helper()
	lines, _ := preprocess(src)
	g, err := parseState(lines)
	if err != nil {
		t.Fatalf("parseState: %v", err)
	}
	return g
}

func TestStateBasics(t *testing.T) {
	g := parseSt(t, `stateDiagram-v2
    [*] --> Still
    Still --> [*]
    Still --> Moving : go
    Moving --> Still`)
	if g.index["__start"] == nil || g.index["__end"] == nil {
		t.Fatalf("start/end pseudo states missing")
	}
	if len(g.edges) != 4 {
		t.Fatalf("%d edges, want 4", len(g.edges))
	}
	var found bool
	for _, e := range g.edges {
		if e.label == "go" && e.from.id == "Still" && e.to.id == "Moving" {
			found = true
		}
	}
	if !found {
		t.Errorf("labelled transition missing")
	}
	if g.index["Still"].kind != boxRound {
		t.Errorf("state should be rounded, got %v", g.index["Still"].kind)
	}
}

// Regression: every `[*]` used to resolve to one global start node, so the
// initial transitions of nested composites all merged into a single marker.
func TestStateCompositeScopedPseudoStates(t *testing.T) {
	g := parseSt(t, `stateDiagram-v2
    [*] --> Outer
    state Outer {
        [*] --> A
        state "Inner region" as Inner {
            [*] --> C
            --
            [*] --> D
        }
        A --> Inner
    }
    Outer --> [*]`)
	for _, id := range []string{
		"__start",               // top level
		"__start:Outer",         // composite Outer
		"__start:Outer/Inner",   // Inner, first region
		"__start:Outer/Inner#1", // Inner, second region
		"__end",                 // top level
	} {
		if g.index[id] == nil {
			t.Errorf("pseudo-state %q missing; have %v", id, nodeIDs(g))
		}
	}
	// each start drives exactly one transition
	for _, tc := range []struct{ from, to string }{
		{"__start", "Outer"},
		{"__start:Outer", "A"},
		{"__start:Outer/Inner", "C"},
		{"__start:Outer/Inner#1", "D"},
	} {
		var found bool
		for _, e := range g.edges {
			if e.from.id == tc.from && e.to.id == tc.to {
				found = true
			}
		}
		if !found {
			t.Errorf("missing transition %s --> %s", tc.from, tc.to)
		}
	}
}

// A `}` that closes a composite must not pop past the top level.
func TestStateUnbalancedBracesAreSafe(t *testing.T) {
	g := parseSt(t, "stateDiagram-v2\n}\n}\n[*] --> A\nA --> [*]")
	if g.index["__start"] == nil || g.index["__end"] == nil {
		t.Errorf("top-level pseudo states missing; have %v", nodeIDs(g))
	}
}

func nodeIDs(g *graph) []string {
	out := make([]string, 0, len(g.nodes))
	for _, n := range g.nodes {
		out = append(out, n.id)
	}
	return out
}

func TestStateDescriptionsAndAnnotations(t *testing.T) {
	g := parseSt(t, `stateDiagram-v2
    state "Reading input" as r
    r : from stdin
    state c <<choice>>
    state f <<fork>>
    r --> c`)
	if got := g.index["r"].lines; got[0] != "Reading input" || len(got) != 2 || got[1] != "from stdin" {
		t.Errorf("description lines = %v", got)
	}
	if g.index["c"].kind != boxDiamond {
		t.Errorf("choice kind = %v", g.index["c"].kind)
	}
	if g.index["f"].kind != boxBar {
		t.Errorf("fork kind = %v", g.index["f"].kind)
	}
}

// Fork and join annotations clear the label before a description is parsed.
func TestStateForkJoinDescriptions(t *testing.T) {
	for _, annotation := range []string{"fork", "join"} {
		t.Run(annotation, func(t *testing.T) {
			src := "stateDiagram-v2\nstate f <<" + annotation + ">>\nf: description"
			g := parseSt(t, src)
			n := g.index["f"]
			if n == nil {
				t.Fatal("annotated state missing")
			}
			if n.kind != boxBar {
				t.Errorf("state kind = %v, want %v", n.kind, boxBar)
			}
			if len(n.lines) != 1 || n.lines[0] != "description" {
				t.Errorf("description lines = %v, want [description]", n.lines)
			}
			if out := mustRender(t, src); !strings.Contains(out, "#########") {
				t.Errorf("rendered state missing bar:\n%s", out)
			}
		})
	}
}

func TestStateComposite(t *testing.T) {
	g := parseSt(t, `stateDiagram-v2
    [*] --> Active
    state Active {
        [*] --> Idle
        Idle --> Busy
        --
        Busy --> Idle
    }
    Active --> Done`)
	for _, id := range []string{"Active", "Idle", "Busy", "Done"} {
		if g.index[id] == nil {
			t.Errorf("flattened state %q missing", id)
		}
	}
}

func TestRenderState(t *testing.T) {
	got := mustRender(t, "stateDiagram-v2\n  [*] --> On\n  On --> Off\n  Off --> On\n  On --> [*]")
	want := strings.Join([]string{
		"     (*)",
		"      |",
		"      v",
		"   .----.",
		"   | On |",
		"   '----'",
		"      ^",
		"   +--+",
		"   +--+",
		"   |  +------+",
		"   v         v",
		".-----.     (O)",
		"| Off |",
		"'-----'",
		"",
	}, "\n")
	if got != want {
		t.Errorf("got:\n%s\nwant:\n%s", got, want)
	}
}

func TestRenderStateLR(t *testing.T) {
	out := mustRender(t, `stateDiagram-v2
    direction LR
    [*] --> Idle
    Idle --> Running : start
    Running --> Idle : stop
    Running --> [*]`)
	for _, want := range []string{"(*)", "(O)", "Idle", "Running", "start", "stop"} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}
	if strings.Count(strings.Split(out, "\n")[0], "\n") != 0 && len(strings.Split(out, "\n")) > 8 {
		t.Errorf("LR layout unexpectedly tall:\n%s", out)
	}
}
