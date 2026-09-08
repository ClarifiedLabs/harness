package mermaid

import "sort"

// drawMarkerV draws an edge-end decoration on a vertical approach.
// dir is 'u' (pointing up, into the node above) or 'd' (pointing down).
func drawMarkerV(c *canvas, x, y int, m marker, dir byte) {
	var ch rune
	switch m {
	case mArrow, mTriangle:
		ch = 'v'
		if dir == 'u' {
			ch = '^'
		}
	case mDiamondFilled:
		ch = '*'
	case mDiamondOpen, mCircle:
		ch = 'o'
	case mCross:
		ch = 'x'
	default:
		return
	}
	c.put(x, y, ch)
}

// drawMarkerH draws an edge-end decoration on a horizontal approach.
// dir is 'l' (pointing left) or 'r' (pointing right). x is the cell
// adjacent to the node border.
func drawMarkerH(c *canvas, x, y int, m marker, dir byte) {
	switch m {
	case mArrow:
		if dir == 'l' {
			c.put(x, y, '<')
		} else {
			c.put(x, y, '>')
		}
	case mTriangle:
		if dir == 'l' {
			c.text(x, y, "<|")
		} else {
			c.text(x-1, y, "|>")
		}
	case mDiamondFilled:
		c.put(x, y, '*')
	case mDiamondOpen, mCircle:
		c.put(x, y, 'o')
	case mCross:
		c.put(x, y, 'x')
	}
}

type labelDraw struct {
	cx, y int // cx is the horizontal center of the label
	text  string
}

// labelsOverlap compares occupied intervals, including padding, without
// deduplicating equal labels. Sorting a separate slice preserves paint order.
func labelsOverlap(labels []labelDraw) bool {
	type interval struct{ y, start, end int }
	spans := make([]interval, 0, len(labels))
	for _, l := range labels {
		w := dispWidth(l.text)
		if w > 0 {
			start := l.cx - w/2
			spans = append(spans, interval{l.y, start, start + w})
		}
	}
	sort.Slice(spans, func(i, j int) bool {
		if spans[i].y != spans[j].y {
			return spans[i].y < spans[j].y
		}
		return spans[i].start < spans[j].start
	})
	for i := 1; i < len(spans); i++ {
		if spans[i].y == spans[i-1].y && spans[i].start < spans[i-1].end {
			return true
		}
	}
	return false
}

func (g *graph) recordLabelPlacement(labels []labelDraw) {
	g.labelOverlap = labelsOverlap(labels)
	for _, l := range labels {
		if l.cx-dispWidth(l.text)/2 < 0 || l.y < 0 {
			g.labelClipped = true
		}
	}
}

func drawLabels(c *canvas, labels []labelDraw) {
	for _, l := range labels {
		c.text(l.cx-dispWidth(l.text)/2, l.y, l.text)
	}
}

// jogLabel places an edge label on a horizontal jog between x1 and x2 at row
// y: centered when it fits between the corners, otherwise to the right of
// the jog.
func jogLabel(x1, x2, y int, label string) labelDraw {
	lo, hi := x1, x2
	if lo > hi {
		lo, hi = hi, lo
	}
	text := " " + label + " "
	tl := dispWidth(text)
	if tl <= hi-lo-1 {
		start := (lo+hi)/2 - tl/2
		if start < lo+1 {
			start = lo + 1
		}
		if start+tl > hi {
			start = hi - tl
		}
		return labelDraw{start + tl/2, y, text}
	}
	return labelDraw{hi + 2 + dispWidth(label)/2, y, label}
}

type markerDraw struct {
	x, y int
	m    marker
	dir  byte
}

// routeBundle identifies edge segments that can share a routing track. Edges
// may only join a bundle at the same end of the same real node; keeping the
// endpoint side in the key prevents an incoming and outgoing edge from being
// joined merely because they touch the same node.
type routeBundle struct {
	gap      int
	endpoint *gnode
	first    bool
	line     lineKind
	marker   marker
}

// assignRouteTracks allocates cross-axis tracks to segments that need a jog.
// Compatible fan-in and fan-out segments reuse one track, producing a single
// trunk with branches instead of a row of adjacent parallel lines.
func assignRouteTracks(segs []*gseg, nGaps int, needsTrack func(*gseg) bool) (map[*gseg]int, []int) {
	tracks := make([]int, nGaps+1)
	trackIdx := map[*gseg]int{}

	gapOf := func(s *gseg) int {
		gi := s.u.rank
		if s.w.rank < gi {
			gi = s.w.rank
		}
		return gi
	}
	firstKey := func(s *gseg) routeBundle {
		return routeBundle{
			gap: gapOf(s), endpoint: s.u, first: true,
			line: s.e.line, marker: s.e.sm,
		}
	}
	lastKey := func(s *gseg) routeBundle {
		return routeBundle{
			gap: gapOf(s), endpoint: s.w, first: false,
			line: s.e.line, marker: s.e.em,
		}
	}
	eligible := func(s *gseg) bool {
		// Labels need their own track, and parallel edges between the same
		// nodes intentionally remain visually distinct.
		return needsTrack(s) && s.e.line != lineNone && s.e.label == "" && !s.parallel
	}

	// Count both possible bundles first. A one-segment edge can participate in
	// either its source fan-out or its destination fan-in, so it chooses the
	// larger group below rather than accidentally connecting both groups.
	candidates := map[routeBundle]int{}
	for _, s := range segs {
		if !eligible(s) {
			continue
		}
		if s.first {
			candidates[firstKey(s)]++
		}
		if s.last {
			candidates[lastKey(s)]++
		}
	}

	selected := map[*gseg]routeBundle{}
	selectedCount := map[routeBundle]int{}
	for _, s := range segs {
		if !eligible(s) {
			continue
		}
		var key routeBundle
		switch {
		case s.first && s.last:
			fk, lk := firstKey(s), lastKey(s)
			key = fk
			if candidates[lk] > candidates[fk] {
				key = lk
			}
		case s.first:
			key = firstKey(s)
		case s.last:
			key = lastKey(s)
		default:
			continue
		}
		selected[s] = key
		selectedCount[key]++
	}

	bundleTrack := map[routeBundle]int{}
	for _, s := range segs {
		if s.e.line == lineNone || !needsTrack(s) {
			continue
		}
		gi := gapOf(s)
		if key, ok := selected[s]; ok && selectedCount[key] > 1 {
			if ti, exists := bundleTrack[key]; exists {
				trackIdx[s] = ti
				continue
			}
			bundleTrack[key] = tracks[gi]
		}
		trackIdx[s] = tracks[gi]
		tracks[gi]++
	}
	return trackIdx, tracks
}

// drawVertical renders a TD/BT layout.
func (g *graph) drawVertical(ranks [][]*gnode) (string, error) {
	segs := g.segments()
	// side labels of parallel edges extend left of their line; shift the
	// whole drawing right if one would fall off the canvas
	minX := 0
	for _, s := range segs {
		if s.labelHere && s.off < 0 && s.u.cross == s.w.cross {
			if start := s.u.cross + s.off - 1 - dispWidth(s.e.label); start < minX {
				minX = start
			}
		}
	}
	if minX < 0 {
		for _, n := range g.nodes {
			n.cross -= minX
		}
	}
	nGaps := len(ranks) - 1
	trackIdx, tracks := assignRouteTracks(segs, nGaps, func(s *gseg) bool {
		return s.u.cross != s.w.cross || s.labelHere
	})
	rowH := make([]int, len(ranks))
	for ri, row := range ranks {
		for _, n := range row {
			if n.h > rowH[ri] {
				rowH[ri] = n.h
			}
		}
	}
	gapH := make([]int, nGaps+1)
	for gi := 0; gi < nGaps; gi++ {
		gapH[gi] = tracks[gi] + 2
		if gapH[gi] < 2 {
			gapH[gi] = 2
		}
	}
	rowY := make([]int, len(ranks))
	y := 0
	for ri := range ranks {
		rowY[ri] = y
		y += rowH[ri]
		if ri < nGaps {
			y += gapH[ri]
		}
	}

	c := &canvas{}
	for ri, row := range ranks {
		for _, n := range row {
			if n.virtual {
				if n.vchar != ' ' {
					c.vline(rowY[ri], rowY[ri]+rowH[ri]-1, n.cross, n.vchar)
				}
				continue
			}
			n.x = n.cross - n.w/2
			n.y = rowY[ri]
			drawNode(c, n)
		}
	}

	var labels []labelDraw
	var markers []markerDraw
	for _, s := range segs {
		if s.e.line == lineNone {
			continue
		}
		top, bot := s.u, s.w
		if top.rank > bot.rank {
			top, bot = bot, top
		}
		gi := top.rank
		hch, vch := lineChars(s.e.line)
		gapStart := rowY[gi] + rowH[gi]
		gapEnd := gapStart + gapH[gi] - 1
		xt, xb := top.cross, bot.cross
		if xt == xb {
			// parallel straight edges are offset into side-by-side lines;
			// jogged parallels are already separated by their tracks
			xt += s.off
			xb += s.off
		}
		ytop := gapStart
		if !top.virtual {
			ytop = top.y + top.h
		}
		if ti, hasTrack := trackIdx[s]; hasTrack {
			ty := gapStart + 1 + ti
			c.vline(ytop, ty, xt, vch)
			if xt != xb {
				c.hline(xt, xb, ty, hch)
				c.put(xt, ty, '+')
				c.put(xb, ty, '+')
			}
			c.vline(ty, gapEnd, xb, vch)
			if s.labelHere {
				l := dispWidth(s.e.label)
				switch {
				case xt != xb:
					labels = append(labels, jogLabel(xt, xb, ty, s.e.label))
				case s.parallel && s.off < 0: // label beside the line
					labels = append(labels, labelDraw{xt - 1 - l + l/2, ty, s.e.label})
				case s.parallel:
					labels = append(labels, labelDraw{xt + 2 + l/2, ty, s.e.label})
				default:
					labels = append(labels, labelDraw{xt, ty, " " + s.e.label + " "})
				}
			}
		} else {
			c.vline(ytop, gapEnd, xt, vch)
		}
		if s.last && !s.w.virtual {
			if s.w == bot {
				markers = append(markers, markerDraw{xb, gapEnd, s.e.em, 'd'})
			} else {
				markers = append(markers, markerDraw{xt, ytop, s.e.em, 'u'})
			}
		}
		if s.first && !s.u.virtual {
			if s.u == top {
				markers = append(markers, markerDraw{xt, ytop, s.e.sm, 'u'})
			} else {
				markers = append(markers, markerDraw{xb, gapEnd, s.e.sm, 'd'})
			}
		}
	}
	g.drawSelfLoops(c, &labels)
	g.recordLabelPlacement(labels)
	drawLabels(c, labels)
	for _, m := range markers {
		drawMarkerV(c, m.x, m.y, m.m, m.dir)
	}
	return c.result()
}

// drawSelfLoops draws a small loop on the east side of each self-edge's node.
func (g *graph) drawSelfLoops(c *canvas, labels *[]labelDraw) {
	for _, e := range g.edges {
		if !e.self || e.line == lineNone {
			continue
		}
		n := e.from
		x2 := n.x + n.w
		m := n.y + n.h/2
		c.text(x2, m, "--.")
		c.text(x2, m+1, "<-'")
		if e.label != "" {
			*labels = append(*labels, labelDraw{x2 + 4 + dispWidth(e.label)/2, m, e.label})
		}
	}
}

// drawHorizontal renders an LR/RL layout.
func (g *graph) drawHorizontal(ranks [][]*gnode) (string, error) {
	segs := g.segments()
	nGaps := len(ranks) - 1
	trackIdx, tracks := assignRouteTracks(segs, nGaps, func(s *gseg) bool {
		return s.u.cross != s.w.cross
	})
	labelNeed := make([]int, nGaps+1)
	for _, s := range segs {
		if s.e.line == lineNone {
			continue
		}
		gi := s.u.rank
		if s.w.rank < gi {
			gi = s.w.rank
		}
		if s.labelHere {
			if need := dispWidth(s.e.label) + 4; need > labelNeed[gi] {
				labelNeed[gi] = need
			}
		}
	}
	colW := make([]int, len(ranks))
	for ri, row := range ranks {
		for _, n := range row {
			if n.w > colW[ri] {
				colW[ri] = n.w
			}
		}
	}
	gapW := make([]int, nGaps+1)
	for gi := 0; gi < nGaps; gi++ {
		extra := labelNeed[gi]
		if extra < 2 {
			extra = 2
		}
		gapW[gi] = tracks[gi] + extra
		if gapW[gi] < 4 {
			gapW[gi] = 4
		}
	}
	colX := make([]int, len(ranks))
	x := 0
	for ri := range ranks {
		colX[ri] = x
		x += colW[ri]
		if ri < nGaps {
			x += gapW[ri]
		}
	}

	c := &canvas{}
	for ri, row := range ranks {
		for _, n := range row {
			if n.virtual {
				if n.vchar != ' ' {
					c.hline(colX[ri], colX[ri]+colW[ri]-1, n.cross, horizOf(n.vchar))
				}
				continue
			}
			n.x = colX[ri]
			n.y = n.cross - n.h/2
			drawNode(c, n)
		}
	}

	var labels []labelDraw
	var markers []markerDraw
	for _, s := range segs {
		if s.e.line == lineNone {
			continue
		}
		lt, rt := s.u, s.w
		if lt.rank > rt.rank {
			lt, rt = rt, lt
		}
		gi := lt.rank
		hch, vch := lineChars(s.e.line)
		gapStart := colX[gi] + colW[gi]
		gapEnd := gapStart + gapW[gi] - 1
		yl, yr := lt.cross, rt.cross
		if yl == yr {
			yl += s.off
			yr += s.off
		}
		xleft := gapStart
		if !lt.virtual {
			xleft = lt.x + lt.w
		}
		if ti, hasTrack := trackIdx[s]; hasTrack {
			tx := gapStart + 1 + ti
			c.hline(xleft, tx, yl, hch)
			c.vline(yl, yr, tx, vch)
			c.put(tx, yl, '+')
			c.put(tx, yr, '+')
			c.hline(tx, gapEnd, yr, hch)
			if s.labelHere {
				labels = append(labels, jogLabel(tx, gapEnd+1, yr, s.e.label))
			}
		} else {
			c.hline(xleft, gapEnd, yl, hch)
			if s.labelHere {
				labels = append(labels, jogLabel(xleft-1, gapEnd+1, yl, s.e.label))
			}
		}
		if s.last && !s.w.virtual {
			if s.w == rt {
				markers = append(markers, markerDraw{gapEnd, yr, s.e.em, 'r'})
			} else {
				markers = append(markers, markerDraw{xleft, yl, s.e.em, 'l'})
			}
		}
		if s.first && !s.u.virtual {
			if s.u == lt {
				markers = append(markers, markerDraw{xleft, yl, s.e.sm, 'l'})
			} else {
				markers = append(markers, markerDraw{gapEnd, yr, s.e.sm, 'r'})
			}
		}
	}
	// self loops: small loop under the node
	for _, e := range g.edges {
		if !e.self || e.line == lineNone {
			continue
		}
		n := e.from
		cx := n.x + n.w/2
		r1 := n.y + n.h
		c.put(cx-1, r1, '^')
		c.put(cx+1, r1, '|')
		c.put(cx-1, r1+1, '\'')
		c.put(cx, r1+1, '-')
		c.put(cx+1, r1+1, '\'')
		if e.label != "" {
			labels = append(labels, labelDraw{cx + 4 + dispWidth(e.label)/2, r1, e.label})
		}
	}
	g.recordLabelPlacement(labels)
	drawLabels(c, labels)
	for _, m := range markers {
		drawMarkerH(c, m.x, m.y, m.m, m.dir)
	}
	return c.result()
}

// horizOf maps a vertical line character to its horizontal counterpart.
func horizOf(v rune) rune {
	switch v {
	case ':':
		return '.'
	case '#':
		return '='
	}
	return '-'
}
