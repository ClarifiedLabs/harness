package mermaid

import (
	"strings"
	"testing"
)

func parseSeq(t *testing.T, src string) *seqDiagram {
	t.Helper()
	lines, _ := preprocess(src)
	d, err := parseSequence(lines)
	if err != nil {
		t.Fatalf("parseSequence: %v", err)
	}
	return d
}

func TestSequenceArrows(t *testing.T) {
	tests := []struct {
		src    string
		dashed bool
		head   byte
	}{
		{"A->B: m", false, 0},
		{"A-->B: m", true, 0},
		{"A->>B: m", false, '>'},
		{"A-->>B: m", true, '>'},
		{"A-xB: m", false, 'x'},
		{"A--xB: m", true, 'x'},
		{"A-)B: m", false, ')'},
		{"A--)B: m", true, ')'},
	}
	for _, tt := range tests {
		d := parseSeq(t, "sequenceDiagram\n"+tt.src)
		if len(d.events) != 1 {
			t.Fatalf("%q: %d events, want 1", tt.src, len(d.events))
		}
		ev := d.events[0]
		if ev.kind != evMsg || ev.dashed != tt.dashed || ev.head != tt.head || ev.label != "m" {
			t.Errorf("%q: got dashed=%v head=%q label=%q", tt.src, ev.dashed, ev.head, ev.label)
		}
	}
}

func TestSequenceParticipants(t *testing.T) {
	d := parseSeq(t, `sequenceDiagram
    participant A as Alice
    actor B
    A->>+B: hi
    B-->>-A: yo
    C->>A: new`)
	if len(d.parts) != 3 {
		t.Fatalf("%d participants, want 3", len(d.parts))
	}
	if d.parts[0].label != "Alice" || d.parts[0].id != "A" {
		t.Errorf("alias not applied: %+v", d.parts[0])
	}
	if d.parts[2].id != "C" {
		t.Errorf("implicit participant order wrong: %+v", d.parts[2])
	}
}

func TestSequenceBlocksAndNotes(t *testing.T) {
	d := parseSeq(t, `sequenceDiagram
    A->>B: hi
    loop every day
        B-->>A: ok
    end
    alt good
        A->>B: yes
    else bad
        A->>B: no
    end
    Note over A,B: spanning
    Note right of B: to the side`)
	kinds := []seqEventKind{evMsg, evBlock, evMsg, evEnd, evBlock, evMsg, evDivider, evMsg, evEnd, evNote, evNote}
	if len(d.events) != len(kinds) {
		t.Fatalf("%d events, want %d", len(d.events), len(kinds))
	}
	for i, k := range kinds {
		if d.events[i].kind != k {
			t.Errorf("event %d kind = %v, want %v", i, d.events[i].kind, k)
		}
	}
	if d.events[1].blockKind != "loop" || d.events[1].label != "every day" {
		t.Errorf("loop block parsed wrong: %+v", d.events[1])
	}
	if d.events[9].side != 0 || len(d.events[9].parts) != 2 {
		t.Errorf("note over parsed wrong: %+v", d.events[9])
	}
	if d.events[10].side != 1 {
		t.Errorf("note right parsed wrong: %+v", d.events[10])
	}
}

func TestSequenceAutonumber(t *testing.T) {
	d := parseSeq(t, "sequenceDiagram\nautonumber\nA->>B: one\nB->>A: two")
	if d.events[0].label != "1. one" || d.events[1].label != "2. two" {
		t.Errorf("autonumber labels: %q, %q", d.events[0].label, d.events[1].label)
	}
}

func TestRenderSequenceBasic(t *testing.T) {
	got := mustRender(t, "sequenceDiagram\n  Alice->>Bob: Hello\n  Bob-->>Alice: Hi")
	want := strings.Join([]string{
		" +-------+    +-----+",
		" | Alice |    | Bob |",
		" +-------+    +-----+",
		"     |           |",
		"     |   Hello   |",
		"     |---------->|",
		"     |           |",
		"     |    Hi     |",
		"     |< - - - - -|",
		"     |           |",
		" +-------+    +-----+",
		" | Alice |    | Bob |",
		" +-------+    +-----+",
		"",
	}, "\n")
	if got != want {
		t.Errorf("got:\n%s\nwant:\n%s", got, want)
	}
}

func TestRenderSequenceFeatures(t *testing.T) {
	out := mustRender(t, `sequenceDiagram
    participant W as Web
    participant S as Server
    W->>S: GET /
    alt hit
        S-->>W: cached
    else miss
        S->>S: compute
        S-->>W: fresh
    end
    Note over W,S: done`)
	for _, want := range []string{"alt: hit", "else: miss", "GET /", "cached", "compute", "fresh", "done"} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}
	// frame must have drawn a border
	if !strings.Contains(out, "+-") {
		t.Errorf("no frame border found:\n%s", out)
	}
}

func TestRenderSequenceSelfAndCross(t *testing.T) {
	out := mustRender(t, `sequenceDiagram
    A->>C: skip over B
    B->>B: self
    C-xA: reject`)
	for _, want := range []string{"skip over B", "self", "reject", "--.", "<-'", "x"} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}
}
