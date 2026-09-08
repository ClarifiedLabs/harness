package mermaid

import (
	"strings"
	"testing"
)

func TestResponsiveListContinuationIndent(t *testing.T) {
	source := "flowchart TD\nA[\"safe<br/>Connections:<br/>- root --> admin\"] --> A\nA -.->|a long edge label that wraps to another line| A\n"
	out := renderAt(t, source, 40)
	assertFits(t, out, 40)
	if !strings.Contains(out, "- A: safe\n  Connections:\n  - root --> admin\n") {
		t.Fatalf("multiline label escaped its list item:\n%s", out)
	}
	if !strings.Contains(out, "- A -.-> A: a long edge label that wraps\n  to another line\n") {
		t.Fatalf("wrapped edge label escaped its list item:\n%s", out)
	}
}
