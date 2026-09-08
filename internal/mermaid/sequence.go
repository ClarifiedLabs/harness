package mermaid

import (
	"regexp"
	"strings"
)

type seqPart struct {
	id, label string
	w         int // header box width
	x         int // lifeline center column
	idx       int
}

type seqEventKind int

const (
	evMsg seqEventKind = iota
	evNote
	evBlock
	evDivider
	evEnd
)

type seqEvent struct {
	kind      seqEventKind
	from, to  *seqPart // evMsg
	label     string
	dashed    bool
	head      byte // '>', 'x', ')' or 0 for a plain line
	bidir     bool
	side      int        // evNote: -1 left, 0 over, 1 right
	parts     []*seqPart // evNote
	blockKind string     // evBlock: loop, alt, opt, par, critical, break, rect

	y int // assigned during layout
}

type seqDiagram struct {
	parts  []*seqPart
	byName map[string]*seqPart
	events []*seqEvent
	title  string
}

func (d *seqDiagram) part(name string) (*seqPart, error) {
	name = strings.TrimSpace(name)
	name = strings.TrimLeft(name, "+-")
	name = strings.TrimSpace(name)
	if p, ok := d.byName[name]; ok {
		return p, nil
	}
	if len(d.parts) >= maxNodes {
		return nil, limitError("participants", maxNodes)
	}
	p := &seqPart{id: name, label: name, idx: len(d.parts)}
	d.parts = append(d.parts, p)
	d.byName[name] = p
	return p, nil
}

var (
	reSeqPart = regexp.MustCompile(`(?i)^(?:create\s+)?(?:participant|actor)\s+(\S+)(?:\s+as\s+(.+))?$`)
	reSeqMsg  = regexp.MustCompile(`^(.+?)\s*(<<-->>|<<->>|-->>|->>|--[x)]|-[x)]|-->|->)\s*(.+?)\s*(?::\s*(.*))?$`)
	reSeqNote = regexp.MustCompile(`(?i)^note\s+(right of|left of|over)\s+([^:]+?)\s*:\s*(.*)$`)
)

// parseSequence parses sequenceDiagram sources.
func parseSequence(lines []string) (*seqDiagram, error) {
	d := &seqDiagram{byName: map[string]*seqPart{}}
	first := true
	autonumber := false
	msgNum := 0
	boxDepth := 0 // participant `box ... end` groups (ignored)
	for _, raw := range lines {
		line := strings.TrimSpace(raw)
		if line == "" {
			continue
		}
		if first {
			first = false
			continue // sequenceDiagram header
		}
		lower := strings.ToLower(line)
		switch {
		case strings.HasPrefix(lower, "autonumber"):
			autonumber = !strings.Contains(lower, "off")
			continue
		case strings.HasPrefix(lower, "title:"):
			d.title = strings.TrimSpace(line[len("title:"):])
			continue
		case strings.HasPrefix(lower, "title "):
			d.title = strings.TrimSpace(line[len("title "):])
			continue
		case strings.HasPrefix(lower, "activate "), strings.HasPrefix(lower, "deactivate "),
			strings.HasPrefix(lower, "destroy "),
			strings.HasPrefix(lower, "link "), strings.HasPrefix(lower, "links "),
			strings.HasPrefix(lower, "accdescr"), strings.HasPrefix(lower, "acctitle"):
			continue
		case strings.HasPrefix(lower, "box"):
			boxDepth++
			continue
		case lower == "end":
			if boxDepth > 0 {
				boxDepth--
			} else {
				if err := d.addEvent(&seqEvent{kind: evEnd}); err != nil {
					return nil, err
				}
			}
			continue
		}
		if m := reSeqPart.FindStringSubmatch(line); m != nil {
			p, err := d.part(m[1])
			if err != nil {
				return nil, err
			}
			if m[2] != "" {
				p.label = strings.TrimSpace(m[2])
			}
			continue
		}
		if m := reSeqNote.FindStringSubmatch(line); m != nil {
			ev := &seqEvent{kind: evNote, label: strings.TrimSpace(m[3])}
			switch strings.ToLower(m[1]) {
			case "left of":
				ev.side = -1
			case "right of":
				ev.side = 1
			}
			for _, name := range strings.Split(m[2], ",") {
				p, err := d.part(name)
				if err != nil {
					return nil, err
				}
				if len(ev.parts) >= maxNodes {
					return nil, limitError("note participants", maxNodes)
				}
				ev.parts = append(ev.parts, p)
			}
			if err := d.addEvent(ev); err != nil {
				return nil, err
			}
			continue
		}
		if kind, rest, ok := seqBlockStart(line); ok {
			if err := d.addEvent(&seqEvent{kind: evBlock, blockKind: kind, label: rest}); err != nil {
				return nil, err
			}
			continue
		}
		if kind, rest, ok := seqDividerStart(line); ok {
			if err := d.addEvent(&seqEvent{kind: evDivider, blockKind: kind, label: rest}); err != nil {
				return nil, err
			}
			continue
		}
		if m := reSeqMsg.FindStringSubmatch(line); m != nil {
			arrow := m[2]
			from, err := d.part(m[1])
			if err != nil {
				return nil, err
			}
			to, err := d.part(m[3])
			if err != nil {
				return nil, err
			}
			ev := &seqEvent{
				kind:  evMsg,
				from:  from,
				to:    to,
				label: strings.TrimSpace(m[4]),
			}
			ev.dashed = strings.Contains(arrow, "--")
			switch {
			case strings.Contains(arrow, "x"):
				ev.head = 'x'
			case strings.Contains(arrow, ")"):
				ev.head = ')'
			case strings.Contains(arrow, ">>"):
				ev.head = '>'
			}
			if strings.HasPrefix(arrow, "<<") {
				ev.bidir = true
				ev.head = '>'
			}
			if autonumber {
				msgNum++
				num := itoa(msgNum)
				if ev.label == "" {
					ev.label = num
				} else {
					ev.label = num + ". " + ev.label
				}
			}
			if err := d.addEvent(ev); err != nil {
				return nil, err
			}
			continue
		}
	}
	return d, nil
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}

func seqBlockStart(line string) (kind, rest string, ok bool) {
	for _, k := range []string{"loop", "opt", "alt", "par", "critical", "break", "rect"} {
		if strings.EqualFold(line, k) {
			return k, "", true
		}
		if len(line) > len(k) && strings.EqualFold(line[:len(k)], k) && line[len(k)] == ' ' {
			rest = strings.TrimSpace(line[len(k):])
			if k == "rect" {
				rest = "" // rect takes a color, not a label
			}
			return k, rest, true
		}
	}
	return "", "", false
}

func seqDividerStart(line string) (kind, rest string, ok bool) {
	for _, k := range []string{"else", "and", "option"} {
		if strings.EqualFold(line, k) {
			return k, "", true
		}
		if len(line) > len(k) && strings.EqualFold(line[:len(k)], k) && line[len(k)] == ' ' {
			return k, strings.TrimSpace(line[len(k):]), true
		}
	}
	return "", "", false
}

func (d *seqDiagram) addEvent(ev *seqEvent) error {
	if len(d.events) >= maxEvents {
		return limitError("events", maxEvents)
	}
	d.events = append(d.events, ev)
	return nil
}
