package mermaid

import (
	"errors"
	"sort"
)

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

// renderSequence lays out and draws a sequence diagram.
func renderSequence(d *seqDiagram) (string, error) {
	n := len(d.parts)
	if n == 0 {
		return "", errors.New("sequence diagram has no participants")
	}
	for _, p := range d.parts {
		p.w = maxInt(dispWidth(p.label)+4, 7)
	}

	// frame nesting depth per block event
	maxDepth, depth := 0, 0
	blockDepth := map[*seqEvent]int{}
	for _, ev := range d.events {
		switch ev.kind {
		case evBlock:
			blockDepth[ev] = depth
			depth++
			if depth > maxDepth {
				maxDepth = depth
			}
		case evDivider:
			if depth > 0 {
				blockDepth[ev] = depth - 1
			}
		case evEnd:
			if depth > 0 {
				depth--
			}
		}
	}

	// horizontal spacing
	gaps := make([]int, maxInt(n-1, 0))
	for i := range gaps {
		gaps[i] = maxInt((d.parts[i].w+d.parts[i+1].w)/2+4, 12)
	}
	leftPad, rightPad := 0, 0
	extendLeft := func(idx, amount int) {
		if idx == 0 {
			leftPad = maxInt(leftPad, amount-d.parts[0].w/2)
		} else {
			gaps[idx-1] = maxInt(gaps[idx-1], amount+2)
		}
	}
	extendRight := func(idx, amount int) {
		if idx == n-1 {
			rightPad = maxInt(rightPad, amount-d.parts[n-1].w/2)
		} else {
			gaps[idx] = maxInt(gaps[idx], amount+2)
		}
	}
	type span struct{ i, j, need int }
	var spans []span
	for _, ev := range d.events {
		switch ev.kind {
		case evMsg:
			a, b := ev.from.idx, ev.to.idx
			if a == b {
				extendRight(a, dispWidth(ev.label)+7)
			} else {
				if a > b {
					a, b = b, a
				}
				spans = append(spans, span{a, b, dispWidth(ev.label) + 6})
			}
		case evNote:
			l := dispWidth(ev.label)
			lo, hi := ev.parts[0].idx, ev.parts[0].idx
			for _, p := range ev.parts {
				if p.idx < lo {
					lo = p.idx
				}
				if p.idx > hi {
					hi = p.idx
				}
			}
			switch {
			case ev.side < 0:
				extendLeft(lo, l+6)
			case ev.side > 0:
				extendRight(hi, l+6)
			case lo == hi:
				extendLeft(lo, (l+4)/2+1)
				extendRight(hi, (l+4)/2+1)
			default:
				spans = append(spans, span{lo, hi, l - 1})
				extendLeft(lo, 4)
				extendRight(hi, 4)
			}
		}
	}
	sort.SliceStable(spans, func(a, b int) bool { return spans[a].j-spans[a].i < spans[b].j-spans[b].i })
	for _, sp := range spans {
		cur := 0
		for k := sp.i; k < sp.j; k++ {
			cur += gaps[k]
		}
		if cur < sp.need {
			w := sp.j - sp.i
			add := (sp.need - cur + w - 1) / w
			for k := sp.i; k < sp.j; k++ {
				gaps[k] += add
			}
		}
	}
	margin := 2*maxDepth + 1
	x := margin + leftPad + d.parts[0].w/2
	for i, p := range d.parts {
		if i > 0 {
			x += gaps[i-1]
		}
		p.x = x
	}
	width := d.parts[n-1].x + d.parts[n-1].w/2 + rightPad + margin + 1
	width = maxInt(width, dispWidth(d.title)+2)
	for ev, dep := range blockDepth {
		if ev.kind == evBlock || ev.kind == evDivider {
			label := ev.blockKind
			if ev.label != "" {
				label += ": " + ev.label
			}
			width = maxInt(width, 2*dep+dispWidth(label)+8)
		}
	}

	// vertical layout
	y := 0
	if d.title != "" {
		y = 2
	}
	headerY := y
	y += 4
	for _, ev := range d.events {
		ev.y = y
		switch ev.kind {
		case evMsg:
			switch {
			case ev.from == ev.to:
				y += 4
			case ev.label != "":
				y += 3
			default:
				y += 2
			}
		case evNote:
			y += 4
		case evBlock, evDivider, evEnd:
			y += 2
		}
	}
	footerY := y
	if err := checkCanvasSize(width, footerY+3); err != nil {
		return "", err
	}

	// draw
	c := &canvas{}
	if d.title != "" {
		c.text((width-dispWidth(d.title))/2, 0, d.title)
	}
	partBox := func(p *seqPart, by int) {
		c.rect(p.x-p.w/2, by, p.w, 3)
		c.text(p.x-dispWidth(p.label)/2, by+1, p.label)
	}
	for _, p := range d.parts {
		partBox(p, headerY)
		partBox(p, footerY)
	}
	for _, p := range d.parts {
		c.vline(headerY+3, footerY-1, p.x, '|')
	}

	type frame struct {
		y, depth    int
		kind, label string
		divs        []*seqEvent
	}
	var stack []frame
	for _, ev := range d.events {
		switch ev.kind {
		case evMsg:
			drawSeqMsg(c, ev)
		case evNote:
			drawSeqNote(c, ev)
		case evBlock:
			stack = append(stack, frame{y: ev.y, depth: blockDepth[ev], kind: ev.blockKind, label: ev.label})
		case evDivider:
			if len(stack) > 0 {
				stack[len(stack)-1].divs = append(stack[len(stack)-1].divs, ev)
			}
		case evEnd:
			if len(stack) == 0 {
				continue
			}
			f := stack[len(stack)-1]
			stack = stack[:len(stack)-1]
			xL, xR := 2*f.depth, width-1-2*f.depth
			for xx := xL; xx <= xR; xx++ {
				c.put(xx, f.y, '-')
				c.put(xx, ev.y, '-')
			}
			c.put(xL, f.y, '+')
			c.put(xR, f.y, '+')
			c.put(xL, ev.y, '+')
			c.put(xR, ev.y, '+')
			for yy := f.y + 1; yy < ev.y; yy++ {
				c.put(xL, yy, '|')
				c.put(xR, yy, '|')
			}
			label := f.kind
			if f.label != "" {
				label += ": " + f.label
			}
			c.text(xL+2, f.y, " "+label+" ")
			for _, dv := range f.divs {
				for xx := xL + 1; xx < xR; xx++ {
					if (xx-xL)%2 == 1 {
						c.put(xx, dv.y, '-')
					}
				}
				dlabel := dv.blockKind
				if dv.label != "" {
					dlabel += ": " + dv.label
				}
				c.text(xL+2, dv.y, " "+dlabel+" ")
			}
		}
	}
	return c.result()
}

func drawSeqMsg(c *canvas, ev *seqEvent) {
	if ev.from == ev.to {
		x, y := ev.from.x, ev.y
		c.text(x+1, y, "--.")
		c.put(x+3, y+1, '|')
		if ev.label != "" {
			c.text(x+5, y+1, ev.label)
		}
		c.text(x+1, y+2, "<-'")
		return
	}
	xa, xb := ev.from.x, ev.to.x
	rightward := xb > xa
	lo, hi := xa, xb
	if lo > hi {
		lo, hi = hi, lo
	}
	y := ev.y
	if ev.label != "" {
		c.text((lo+hi)/2-(dispWidth(ev.label)+2)/2, y, " "+ev.label+" ")
		y++
	}
	for x := lo + 1; x <= hi-1; x++ {
		if ev.dashed && (x-lo)%2 == 0 {
			continue
		}
		c.put(x, y, '-')
	}
	headRight := map[byte]rune{'>': '>', 'x': 'x', ')': ')'}
	headLeft := map[byte]rune{'>': '<', 'x': 'x', ')': '('}
	if ev.head != 0 {
		if rightward || ev.bidir {
			c.put(hi-1, y, headRight[ev.head])
		}
		if !rightward || ev.bidir {
			c.put(lo+1, y, headLeft[ev.head])
		}
	}
}

func drawSeqNote(c *canvas, ev *seqEvent) {
	l := dispWidth(ev.label)
	lo, hi := ev.parts[0], ev.parts[0]
	for _, p := range ev.parts {
		if p.idx < lo.idx {
			lo = p
		}
		if p.idx > hi.idx {
			hi = p
		}
	}
	var xL, w int
	switch {
	case ev.side > 0:
		xL, w = hi.x+2, l+4
	case ev.side < 0:
		w = l + 4
		xL = lo.x - 2 - w + 1
	default:
		cx := (lo.x + hi.x) / 2
		w = maxInt(l+4, hi.x-lo.x+5)
		xL = cx - w/2
	}
	c.clear(xL, ev.y, w, 3)
	c.rect(xL, ev.y, w, 3)
	c.text(xL+(w-l)/2, ev.y+1, ev.label)
}
