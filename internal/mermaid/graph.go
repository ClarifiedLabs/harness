package mermaid

import (
	"fmt"
	"sort"
)

type lineKind int

const (
	lineSolid lineKind = iota
	lineDotted
	lineThick
	lineNone // invisible links (~~~): influence layout but are not drawn
)

// marker is a decoration drawn at an edge endpoint.
type marker int

const (
	mNone marker = iota
	mArrow
	mTriangle // UML inheritance / realization
	mDiamondFilled
	mDiamondOpen
	mCircle
	mCross
)

// gnode is a node in a layered graph diagram (flowchart, state or class).
type gnode struct {
	id       string
	lines    []string
	kind     boxKind
	sections [][]string // boxClass only: title / attributes / methods

	w, h    int
	rank    int
	pos     int // order within rank
	cross   int // center coordinate along the cross axis
	x, y    int // top-left corner on the canvas
	virtual bool
	vchar   rune // line char drawn through virtual nodes
}

type gedge struct {
	from, to *gnode
	label    string
	line     lineKind
	sm, em   marker // markers at the from / to ends
	self     bool
	path     []*gnode // from, virtual nodes..., to
}

type graph struct {
	dir   string // TD, BT, LR, RL
	nodes []*gnode
	index map[string]*gnode
	edges []*gedge

	// Drawing metadata is consumed only by responsive flowchart selection.
	labelOverlap bool
	labelClipped bool
}

func newGraph() *graph {
	return &graph{dir: "TD", index: map[string]*gnode{}}
}

// node returns the node with the given id, creating it (labelled with its id)
// if it does not exist yet.
func (g *graph) node(id string) (*gnode, error) {
	if n, ok := g.index[id]; ok {
		return n, nil
	}
	if len(g.nodes) >= maxNodes {
		return nil, limitError("nodes", maxNodes)
	}
	n := &gnode{id: id, lines: []string{id}, kind: boxRect}
	g.nodes = append(g.nodes, n)
	g.index[id] = n
	return n, nil
}

func (g *graph) addEdge(e *gedge) error {
	if len(g.edges) >= maxEdges {
		return limitError("edges", maxEdges)
	}
	if e.from == e.to {
		e.self = true
	}
	g.edges = append(g.edges, e)
	return nil
}

func (g *graph) horizontal() bool { return g.dir == "LR" || g.dir == "RL" }

// render lays the graph out and draws it, returning the ASCII art.
func (g *graph) render() (string, error) {
	if len(g.nodes) == 0 {
		return "", nil
	}
	for _, n := range g.nodes {
		n.w, n.h = nodeSize(n)
		if err := checkCanvasSize(n.w, n.h); err != nil {
			return "", err
		}
	}
	// widen (or heighten) nodes joined by parallel edges so the offset
	// connection points stay inside the box border
	pairs := map[nodePair]int{}
	for _, e := range g.edges {
		if !e.self {
			pairs[pairKey(e.from, e.to)]++
		}
	}
	for _, e := range g.edges {
		if e.self {
			continue
		}
		if k := pairs[pairKey(e.from, e.to)]; k > 1 {
			need := 2*(k/2) + 3
			for _, n := range []*gnode{e.from, e.to} {
				if g.horizontal() {
					if n.h < need {
						n.h = need
					}
				} else if n.w < need {
					n.w = need
				}
			}
		}
	}
	g.breakCycles()
	g.assignRanks()
	if g.dir == "BT" || g.dir == "RL" {
		maxr := 0
		for _, n := range g.nodes {
			if n.rank > maxr {
				maxr = n.rank
			}
		}
		for _, n := range g.nodes {
			n.rank = maxr - n.rank
		}
	}
	if err := g.insertVirtuals(); err != nil {
		return "", err
	}
	ranks := g.orderRanks()
	g.assignCross(ranks)
	if g.horizontal() {
		return g.drawHorizontal(ranks)
	}
	return g.drawVertical(ranks)
}

// breakCycles reverses edges that would otherwise create cycles, so that
// ranks can be assigned. Reversed edges keep their markers attached to the
// correct node, so the rendered arrows still point the right way.
func (g *graph) breakCycles() {
	const (
		white = 0
		gray  = 1
		black = 2
	)
	color := map[*gnode]int{}
	out := map[*gnode][]*gedge{}
	for _, e := range g.edges {
		if !e.self {
			out[e.from] = append(out[e.from], e)
		}
	}
	var reverse []*gedge
	var visit func(n *gnode)
	visit = func(n *gnode) {
		color[n] = gray
		for _, e := range out[n] {
			switch color[e.to] {
			case gray:
				reverse = append(reverse, e)
			case white:
				visit(e.to)
			}
		}
		color[n] = black
	}
	for _, n := range g.nodes {
		if color[n] == white {
			visit(n)
		}
	}
	for _, e := range reverse {
		e.from, e.to = e.to, e.from
		e.sm, e.em = e.em, e.sm
	}
}

// assignRanks performs longest-path ranking over the (now acyclic) graph.
func (g *graph) assignRanks() {
	out := map[*gnode][]*gedge{}
	in := map[*gnode]int{}
	for _, e := range g.edges {
		if e.self {
			continue
		}
		out[e.from] = append(out[e.from], e)
		in[e.to]++
	}
	// topological order (Kahn)
	var queue []*gnode
	indeg := map[*gnode]int{}
	for _, n := range g.nodes {
		indeg[n] = in[n]
		if in[n] == 0 {
			queue = append(queue, n)
		}
	}
	var topo []*gnode
	for len(queue) > 0 {
		n := queue[0]
		queue = queue[1:]
		topo = append(topo, n)
		for _, e := range out[n] {
			indeg[e.to]--
			if indeg[e.to] == 0 {
				queue = append(queue, e.to)
			}
		}
	}
	for _, n := range topo {
		for _, e := range out[n] {
			if e.to.rank < n.rank+1 {
				e.to.rank = n.rank + 1
			}
		}
	}
	// pull sources down next to their first child to compact the drawing
	for _, n := range g.nodes {
		if in[n] == 0 && len(out[n]) > 0 {
			minChild := 1 << 30
			for _, e := range out[n] {
				if e.to.rank < minChild {
					minChild = e.to.rank
				}
			}
			if minChild-1 > n.rank {
				n.rank = minChild - 1
			}
		}
	}
}

func lineChars(k lineKind) (horiz, vert rune) {
	switch k {
	case lineDotted:
		return '.', ':'
	case lineThick:
		return '=', '#'
	}
	return '-', '|'
}

// insertVirtuals splits edges spanning more than one rank by inserting
// invisible pass-through nodes on the intermediate ranks.
func (g *graph) insertVirtuals() error {
	// Preflight the entire expansion; never build a partial oversized layout.
	total := 0
	for _, e := range g.edges {
		if e.self {
			continue
		}
		distance := e.to.rank - e.from.rank
		if distance < 0 {
			distance = -distance
		}
		if distance > 0 {
			total += distance - 1
		}
		if total > maxVirtualNodes {
			return limitError("virtual nodes", maxVirtualNodes)
		}
	}
	nv := 0
	for _, e := range g.edges {
		if e.self || e.from.rank == e.to.rank {
			continue
		}
		_, vch := lineChars(e.line)
		if e.line == lineNone {
			vch = ' '
		}
		e.path = []*gnode{e.from}
		r1, r2 := e.from.rank, e.to.rank
		step := 1
		if r2 < r1 {
			step = -1
		}
		for r := r1 + step; r != r2; r += step {
			nv++
			v := &gnode{
				id:      fmt.Sprintf("__virt%d", nv),
				virtual: true,
				kind:    boxBare,
				w:       1, h: 1,
				rank:  r,
				vchar: vch,
			}
			g.nodes = append(g.nodes, v)
			e.path = append(e.path, v)
		}
		e.path = append(e.path, e.to)
	}
	return nil
}

// gseg is one rank-to-rank segment of an edge's path.
type gseg struct {
	u, w        *gnode // consecutive nodes on the path (u comes first)
	e           *gedge
	first, last bool
	labelHere   bool
	off         int  // cross-axis offset separating parallel edges
	parallel    bool // part of a bundle of edges between the same node pair
}

// edgeOffset spreads parallel edges around the node center: 0, -1, 1, -2, 2...
func edgeOffset(i int) int {
	if i == 0 {
		return 0
	}
	k := (i + 1) / 2
	if i%2 == 1 {
		return -k
	}
	return k
}

type nodePair struct {
	from, to *gnode
}

func pairKey(a, b *gnode) nodePair {
	// IDs only canonicalize the unordered pair; keys retain node identity
	// rather than copying potentially long identifiers for every edge.
	if a.id > b.id {
		a, b = b, a
	}
	return nodePair{from: a, to: b}
}

func (g *graph) segments() []*gseg {
	var out []*gseg
	for _, e := range g.edges {
		if e.self || len(e.path) < 2 {
			continue
		}
		n := len(e.path) - 1
		li := (n - 1) / 2
		for i := 0; i < n; i++ {
			out = append(out, &gseg{
				u: e.path[i], w: e.path[i+1], e: e,
				first:     i == 0,
				last:      i == n-1,
				labelHere: i == li && e.label != "",
			})
		}
	}
	// separate parallel edges between the same pair of real nodes
	cnt := map[nodePair]int{}
	total := map[nodePair]int{}
	for _, s := range out {
		if !s.u.virtual && !s.w.virtual {
			total[pairKey(s.u, s.w)]++
		}
	}
	for _, s := range out {
		if s.u.virtual || s.w.virtual {
			continue
		}
		k := pairKey(s.u, s.w)
		if total[k] > 1 {
			s.off = edgeOffset(cnt[k])
			s.parallel = true
			cnt[k]++
		}
	}
	return out
}

// orderRanks groups nodes by rank and reduces edge crossings with a few
// barycenter passes.
func (g *graph) orderRanks() [][]*gnode {
	maxr := 0
	for _, n := range g.nodes {
		if n.rank > maxr {
			maxr = n.rank
		}
	}
	ranks := make([][]*gnode, maxr+1)
	for _, n := range g.nodes {
		ranks[n.rank] = append(ranks[n.rank], n)
	}
	for _, row := range ranks {
		for i, n := range row {
			n.pos = i
		}
	}
	nb := map[*gnode][]*gnode{}
	for _, s := range g.segments() {
		nb[s.u] = append(nb[s.u], s.w)
		nb[s.w] = append(nb[s.w], s.u)
	}
	sortRow := func(row []*gnode, targetRank int) {
		weights := map[*gnode]float64{}
		for _, n := range row {
			sum, cnt := 0.0, 0
			for _, m := range nb[n] {
				if m.rank == targetRank {
					sum += float64(m.pos)
					cnt++
				}
			}
			if cnt > 0 {
				weights[n] = sum / float64(cnt)
			} else {
				weights[n] = float64(n.pos)
			}
		}
		sort.SliceStable(row, func(i, j int) bool { return weights[row[i]] < weights[row[j]] })
		for i, n := range row {
			n.pos = i
		}
	}
	for iter := 0; iter < 4; iter++ {
		for ri := 1; ri <= maxr; ri++ {
			sortRow(ranks[ri], ri-1)
		}
		for ri := maxr - 1; ri >= 0; ri-- {
			sortRow(ranks[ri], ri+1)
		}
	}
	return ranks
}

// selfExtra returns extra cross-axis clearance needed to the right of a node
// for its self-loops (and their labels).
func (g *graph) selfExtra(n *gnode) int {
	extra := 0
	for _, e := range g.edges {
		if e.self && e.from == n {
			need := 4
			if e.label != "" {
				need += dispWidth(e.label) + 2
			}
			if need > extra {
				extra = need
			}
		}
	}
	return extra
}

// assignCross assigns center coordinates along the cross axis (x for
// vertical layouts, y for horizontal ones), aligning nodes under their
// neighbors where possible.
func (g *graph) assignCross(ranks [][]*gnode) {
	horizontal := g.horizontal()
	size := func(n *gnode) int {
		if horizontal {
			return n.h
		}
		return n.w + g.selfExtra(n)
	}
	sep := 5
	if horizontal {
		sep = 2
	}
	for _, row := range ranks {
		c := 0
		for _, n := range row {
			n.cross = c + size(n)/2
			c += size(n) + sep
		}
	}
	up := map[*gnode][]*gnode{}
	down := map[*gnode][]*gnode{}
	for _, s := range g.segments() {
		a, b := s.u, s.w
		if a.rank > b.rank {
			a, b = b, a
		}
		down[a] = append(down[a], b)
		up[b] = append(up[b], a)
	}
	adjust := func(row []*gnode, nbmap map[*gnode][]*gnode) {
		for i, n := range row {
			desired := n.cross
			if ms := nbmap[n]; len(ms) > 0 {
				sum := 0
				for _, m := range ms {
					sum += m.cross
				}
				desired = sum / len(ms)
			}
			minc := 0
			if i > 0 {
				p := row[i-1]
				minc = p.cross + (size(p)+1)/2 + sep + size(n)/2
			}
			if desired < minc {
				desired = minc
			}
			n.cross = desired
		}
	}
	for iter := 0; iter < 6; iter++ {
		for ri := 1; ri < len(ranks); ri++ {
			adjust(ranks[ri], up)
		}
		for ri := len(ranks) - 2; ri >= 0; ri-- {
			adjust(ranks[ri], down)
		}
	}
	// normalize left edge to 0
	minLeft := 1 << 30
	for _, n := range g.nodes {
		if l := n.cross - size(n)/2; l < minLeft {
			minLeft = l
		}
	}
	for _, n := range g.nodes {
		n.cross -= minLeft
	}
}
