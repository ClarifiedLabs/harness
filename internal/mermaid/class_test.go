package mermaid

import (
	"strings"
	"testing"
)

func parseCls(t *testing.T, src string) *graph {
	t.Helper()
	lines, _ := preprocess(src)
	g, err := parseClass(lines)
	if err != nil {
		t.Fatalf("parseClass: %v", err)
	}
	return g
}

func TestClassRelations(t *testing.T) {
	tests := []struct {
		src    string
		line   lineKind
		sm, em marker
	}{
		{"A <|-- B", lineSolid, mTriangle, mNone},
		{"A --|> B", lineSolid, mNone, mTriangle},
		{"A <|.. B", lineDotted, mTriangle, mNone},
		{"A ..|> B", lineDotted, mNone, mTriangle},
		{"A *-- B", lineSolid, mDiamondFilled, mNone},
		{"A --* B", lineSolid, mNone, mDiamondFilled},
		{"A o-- B", lineSolid, mDiamondOpen, mNone},
		{"A --o B", lineSolid, mNone, mDiamondOpen},
		{"A --> B", lineSolid, mNone, mArrow},
		{"A <-- B", lineSolid, mArrow, mNone},
		{"A ..> B", lineDotted, mNone, mArrow},
		{"A -- B", lineSolid, mNone, mNone},
		{"A .. B", lineDotted, mNone, mNone},
	}
	for _, tt := range tests {
		g := parseCls(t, "classDiagram\n"+tt.src)
		if len(g.edges) != 1 {
			t.Fatalf("%q: %d edges, want 1", tt.src, len(g.edges))
		}
		e := g.edges[0]
		if e.line != tt.line || e.sm != tt.sm || e.em != tt.em {
			t.Errorf("%q: got line=%v sm=%v em=%v, want line=%v sm=%v em=%v",
				tt.src, e.line, e.sm, e.em, tt.line, tt.sm, tt.em)
		}
	}
}

func TestClassMembersAndLabels(t *testing.T) {
	g := parseCls(t, `classDiagram
    class Animal {
        <<abstract>>
        +int age
        +eat() void
    }
    Animal : +String name
    Animal : +sleep()
    Duck "1" --> "0..*" Feather : has
    class List~T~ {
        +get(i) T
    }`)
	n := g.index["Animal"]
	if n == nil {
		t.Fatal("Animal missing")
	}
	title, attrs, methods := n.sections[0], n.sections[1], n.sections[2]
	if title[0] != "<<abstract>>" || title[1] != "Animal" {
		t.Errorf("title = %v", title)
	}
	if len(attrs) != 2 || attrs[0] != "+int age" || attrs[1] != "+String name" {
		t.Errorf("attrs = %v", attrs)
	}
	if len(methods) != 2 || methods[0] != "+eat() void" || methods[1] != "+sleep()" {
		t.Errorf("methods = %v", methods)
	}
	if len(g.edges) != 1 || g.edges[0].label != "1 has 0..*" {
		t.Errorf("edge label = %q", g.edges[0].label)
	}
	if lst := g.index["List"]; lst == nil || lst.sections[0][0] != "List<T>" {
		t.Errorf("generic class title wrong: %v", lst)
	}
}

// Regression: annotations containing spaces (`<<value object>>`) failed the
// `\w+` match and were filed as attributes instead of title annotations.
func TestClassMultiWordAnnotation(t *testing.T) {
	g := parseCls(t, `classDiagram
    class Money {
        <<value object>>
        +long amount
    }
    <<data transfer object>> Dto
    class Dto`)
	if title := g.index["Money"].sections[0]; len(title) != 2 ||
		title[0] != "<<value object>>" || title[1] != "Money" {
		t.Errorf("Money title = %v", title)
	}
	if attrs := g.index["Money"].sections[1]; len(attrs) != 1 || attrs[0] != "+long amount" {
		t.Errorf("Money attrs = %v", attrs)
	}
	if title := g.index["Dto"].sections[0]; len(title) != 2 ||
		title[0] != "<<data transfer object>>" || title[1] != "Dto" {
		t.Errorf("Dto title = %v", title)
	}
}

// Regression: a relation whose left-hand class shared a name with a directive
// keyword (`Link -- Peer`, `Style --> Theme`) was swallowed by the directive
// check and silently dropped from the diagram.
func TestClassNamesCollidingWithKeywords(t *testing.T) {
	g := parseCls(t, `classDiagram
    Link -- Peer
    Style --> Theme
    Note ..> Anchor
    Click <|-- Tap
    Namespace o-- Scope
    Direction --> Axis`)
	if len(g.edges) != 6 {
		t.Fatalf("%d edges, want 6", len(g.edges))
	}
	for _, id := range []string{
		"Link", "Peer", "Style", "Theme", "Note", "Anchor",
		"Click", "Tap", "Namespace", "Scope", "Direction", "Axis",
	} {
		if g.index[id] == nil {
			t.Errorf("class %q missing", id)
		}
	}
}

// Real directives must still be skipped, not read as relations.
func TestClassDirectivesStillIgnored(t *testing.T) {
	g := parseCls(t, `classDiagram
    direction LR
    class Node
    class Edge
    Edge --> Node
    note "Edges are directed"
    style Node fill:#eef
    cssClass "Node" highlight
    link Node "https://example.com" "docs"
    click Node call cb()
    callback Node cb "tip"`)
	if len(g.edges) != 1 {
		t.Fatalf("%d edges, want 1", len(g.edges))
	}
	if g.dir != "LR" {
		t.Errorf("dir = %q, want LR", g.dir)
	}
	if len(g.nodes) != 2 {
		t.Errorf("%d classes, want 2: %v", len(g.nodes), nodeIDs(g))
	}
}

func TestRenderClass(t *testing.T) {
	got := mustRender(t, "classDiagram\n  Animal <|-- Dog\n  Animal : +int age\n  Dog : +bark()")
	want := strings.Join([]string{
		"+----------+",
		"|  Animal  |",
		"+----------+",
		"| +int age |",
		"+----------+",
		"+----------+",
		"      ^",
		"      |",
		" +---------+",
		" |   Dog   |",
		" +---------+",
		" +---------+",
		" | +bark() |",
		" +---------+",
		"",
	}, "\n")
	if got != want {
		t.Errorf("got:\n%s\nwant:\n%s", got, want)
	}
}

func TestRenderClassFeatures(t *testing.T) {
	out := mustRender(t, `classDiagram
    direction LR
    class Shape {
        <<interface>>
        +area() float
    }
    Shape <|.. Circle
    Circle "1" *-- "1" Point : center`)
	for _, want := range []string{"<<interface>>", "Shape", "+area() float", "Circle", "Point", "1 center 1", "<|"} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}
}
