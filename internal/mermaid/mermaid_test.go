package mermaid

import (
	"strings"
	"testing"
)

func TestDetect(t *testing.T) {
	tests := []struct {
		src  string
		want Kind
	}{
		{"flowchart TD\nA-->B", KindFlowchart},
		{"graph LR\nA-->B", KindFlowchart},
		{"  \n%% comment\nflowchart LR\nA", KindFlowchart},
		{"sequenceDiagram\nA->>B: hi", KindSequence},
		{"stateDiagram\n[*] --> A", KindState},
		{"stateDiagram-v2\n[*] --> A", KindState},
		{"classDiagram\nA <|-- B", KindClass},
		{"pie\n\"a\": 1", KindUnknown},
		{"", KindUnknown},
	}
	for _, tt := range tests {
		if got := Detect(tt.src); got != tt.want {
			t.Errorf("Detect(%q) = %q, want %q", tt.src, got, tt.want)
		}
	}
}

func TestRenderUnknown(t *testing.T) {
	if _, err := Render("gantt\ntitle x", Options{Width: maxCanvasDimension}); err == nil {
		t.Errorf("expected error for unsupported diagram")
	}
	if _, err := Render("", Options{Width: maxCanvasDimension}); err == nil {
		t.Errorf("expected error for empty input")
	}
}

func TestFrontMatterTitle(t *testing.T) {
	out := mustRender(t, `---
title: My Chart
---
flowchart LR
  A --> B`)
	lines := strings.Split(out, "\n")
	if !strings.Contains(lines[0], "My Chart") {
		t.Errorf("title missing from first line: %q", lines[0])
	}
	if !strings.Contains(out, "| A |") {
		t.Errorf("diagram body missing:\n%s", out)
	}
}

func TestCommentsAndDirectivesStripped(t *testing.T) {
	out := mustRender(t, `%%{init: {'theme':'dark'}}%%
flowchart LR
  %% a comment line
  A --> B`)
	if !strings.Contains(out, "| A |") || !strings.Contains(out, "| B |") {
		t.Errorf("unexpected output:\n%s", out)
	}
	if strings.Contains(out, "comment") || strings.Contains(out, "theme") {
		t.Errorf("comment leaked into output:\n%s", out)
	}
}

// TestRenderNoPanic runs Render over a corpus of syntax variations to make
// sure none of them crash or error unexpectedly.
func TestRenderNoPanic(t *testing.T) {
	corpus := []string{
		"flowchart TD\nA",
		"flowchart TD\nA --> B --> C --> A",
		"flowchart BT\nA --> B\nA --> C\nB --> D\nC --> D",
		"flowchart RL\nA --> B -.-> C ==> D",
		"graph TD\nA[a] & B[b] --> C & D\nC ~~~ D",
		"graph TD\nA <--> B\nB o--o C\nC x--x A",
		"flowchart LR\nA -- text --- B",
		"flowchart TD\nlongid-with-dashes --> B{decision<br>time}",
		"sequenceDiagram\nA->>A: self only",
		"sequenceDiagram\nautonumber\nA->B: x\npar one\nA->>B: y\nand two\nB->>A: z\nend",
		"sequenceDiagram\nparticipant X\nNote left of X: hello",
		"sequenceDiagram\nbox Group\nparticipant A\nparticipant B\nend\nA->>B: hi",
		"stateDiagram-v2\n[*] --> A\nA --> A",
		"stateDiagram\nA --> B : label\nB --> A : back",
		"stateDiagram-v2\nstate f <<fork>>\n[*] --> f\nf --> A\nf --> B",
		"classDiagram\nclass Empty",
		"classDiagram\nA -- B\nB -- C\nC -- A",
		"classDiagram\nnamespace N {\n  class X\n}\nX <|-- Y",
		"---\ntitle: t\n---\nsequenceDiagram\nA->>B: hi",
	}
	for _, src := range corpus {
		out, err := Render(src, Options{Width: maxCanvasDimension})
		if err != nil {
			t.Errorf("Render(%q) error: %v", src, err)
			continue
		}
		if strings.TrimSpace(out) == "" {
			t.Errorf("Render(%q) produced empty output", src)
		}
	}
}

func TestRenderIsDeterministic(t *testing.T) {
	src := `flowchart TD
    A --> B & C & D
    B & C --> E
    D --> E
    E -->|loop| A`
	first := mustRender(t, src)
	for i := 0; i < 10; i++ {
		if got := mustRender(t, src); got != first {
			t.Fatalf("nondeterministic output on run %d:\n%s\nvs:\n%s", i, got, first)
		}
	}
}
