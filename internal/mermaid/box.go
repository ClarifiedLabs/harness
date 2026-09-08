package mermaid

// boxKind selects the border style used to draw a node.
type boxKind int

const (
	boxRect             boxKind = iota // [text]
	boxRound                           // (text)
	boxStadium                         // ([text])
	boxCircle                          // ((text))
	boxDiamond                         // {text}
	boxHex                             // {{text}}
	boxSubroutine                      // [[text]]
	boxCylinder                        // [(text)]
	boxAsym                            // >text]
	boxParallelogram                   // [/text/]
	boxParallelogramAlt                // [\text\]
	boxTrapezoid                       // [/text\]
	boxTrapezoidAlt                    // [\text/]
	boxBare                            // no border (e.g. state start/end markers)
	boxBar                             // fork/join bar in state diagrams
	boxClass                           // class diagram box with compartments
)

// sideWidth is the number of columns used by the left/right border of a box.
func sideWidth(kind boxKind) int {
	switch kind {
	case boxCircle, boxSubroutine:
		return 2
	}
	return 1
}

// nodeSize computes the width and height of a node's box.
func nodeSize(n *gnode) (w, h int) {
	switch n.kind {
	case boxBare:
		w, h = maxLineLen(n.lines), len(n.lines)
		if w < 1 {
			w = 1
		}
		if h < 1 {
			h = 1
		}
		return w, h
	case boxBar:
		w = maxLineLen(n.lines) + 4
		if w < 9 {
			w = 9
		}
		return w, 1
	case boxClass:
		maxw := 0
		total := 0
		for _, sec := range n.sections {
			if l := maxLineLen(sec); l > maxw {
				maxw = l
			}
			total += len(sec)
		}
		w = maxw + 4
		if w < 8 {
			w = 8
		}
		h = 2 + len(n.sections[0])
		if len(n.sections[1]) > 0 || len(n.sections[2]) > 0 {
			h += 2 + len(n.sections[1]) + len(n.sections[2])
		}
		return w, h
	case boxParallelogram, boxParallelogramAlt, boxTrapezoid, boxTrapezoidAlt:
		h = len(n.lines) + 2
		if h < 3 {
			h = 3
		}
		// the sides shift one column per row, so the bounding box has to be
		// wider than the text by the slant: once for a parallelogram, whose
		// edges lean the same way, twice for a trapezoid, whose edges splay
		slant := h - 1
		if n.kind == boxTrapezoid || n.kind == boxTrapezoidAlt {
			slant *= 2
		}
		return maxLineLen(n.lines) + 4 + slant, h
	}
	sw := sideWidth(n.kind)
	w = maxLineLen(n.lines) + 2*sw + 2
	h = len(n.lines) + 2
	if h < 3 {
		h = 3
	}
	return w, h
}

// drawNode renders a node's box (with its text) at the node's x, y position.
func drawNode(c *canvas, n *gnode) {
	x, y, w, h := n.x, n.y, n.w, n.h
	if !c.reserve(x+w-1, y+h-1) {
		return
	}
	switch n.kind {
	case boxBare:
		for i, l := range n.lines {
			c.text(x+(w-dispWidth(l))/2, y+i, l)
		}
		return
	case boxBar:
		for i := 0; i < w; i++ {
			c.put(x+i, y, '#')
		}
		return
	case boxClass:
		drawClassNode(c, n)
		return
	case boxParallelogram, boxParallelogramAlt, boxTrapezoid, boxTrapezoidAlt:
		drawSlantedNode(c, n)
		return
	}

	tl, tr, bl, br := '+', '+', '+', '+'
	switch n.kind {
	case boxRound, boxStadium, boxCylinder, boxDiamond, boxHex, boxCircle:
		tl, tr, bl, br = '.', '.', '\'', '\''
	}
	// top and bottom borders
	for i := 1; i < w-1; i++ {
		c.put(x+i, y, '-')
		c.put(x+i, y+h-1, '-')
	}
	c.put(x, y, tl)
	c.put(x+w-1, y, tr)
	c.put(x, y+h-1, bl)
	c.put(x+w-1, y+h-1, br)

	sw := sideWidth(n.kind)
	mid := y + h/2
	for ry := y + 1; ry < y+h-1; ry++ {
		lch, rch := "|", "|"
		switch n.kind {
		case boxStadium, boxCylinder:
			lch, rch = "(", ")"
		case boxCircle:
			lch, rch = "((", "))"
		case boxSubroutine:
			lch, rch = "||", "||"
		case boxDiamond:
			if ry == mid {
				lch, rch = "<", ">"
			}
		case boxHex:
			if ry == mid {
				lch, rch = "/", "\\"
			}
		case boxAsym:
			lch = "\\"
			if ry > mid {
				lch = "/"
			}
			if ry == mid && h%2 == 1 {
				lch = ">"
			}
		}
		c.text(x, ry, lch)
		c.text(x+w-dispWidth(rch), ry, rch)
	}
	// text lines, vertically centered
	top := y + (h-len(n.lines))/2
	inner := w - 2*sw - 2
	for i, l := range n.lines {
		c.text(x+sw+1+(inner-dispWidth(l))/2, top+i, l)
	}
}

// drawSlantedNode renders the parallelogram and trapezoid shapes. Their sides
// shift one column per row, so each row is drawn at its own offset rather than
// against fixed left and right borders:
//
//	  ______        ______
//	 /      /      /      \
//	/______/      /________\
func drawSlantedNode(c *canvas, n *gnode) {
	x, y, w, h := n.x, n.y, n.w, n.h
	slant := h - 1
	// left and right edge columns on row i, counted from the top
	var left, right func(i int) int
	var lch, rch rune
	switch n.kind {
	case boxParallelogram: // both edges lean right
		left = func(i int) int { return x + slant - i }
		right = func(i int) int { return x + w - 1 - i }
		lch, rch = '/', '/'
	case boxParallelogramAlt: // both edges lean left
		left = func(i int) int { return x + i }
		right = func(i int) int { return x + w - 1 - slant + i }
		lch, rch = '\\', '\\'
	case boxTrapezoid: // narrow on top, splaying outwards
		left = func(i int) int { return x + slant - i }
		right = func(i int) int { return x + w - 1 - slant + i }
		lch, rch = '/', '\\'
	default: // boxTrapezoidAlt: wide on top, tapering inwards
		left = func(i int) int { return x + i }
		right = func(i int) int { return x + w - 1 - i }
		lch, rch = '\\', '/'
	}

	// both edges span the interior only: the corner cells are implied by the
	// slashes on the row below, so the top and bottom runs read as equal
	for cx := left(0) + 1; cx < right(0); cx++ {
		c.put(cx, y, '_')
	}
	for i := 1; i < h; i++ {
		c.put(left(i), y+i, lch)
		c.put(right(i), y+i, rch)
	}
	for cx := left(h-1) + 1; cx < right(h-1); cx++ {
		c.put(cx, y+h-1, '_')
	}
	// text rows sit between the top and bottom edges, centered on each row
	for i, l := range n.lines {
		row := i + 1
		lo, hi := left(row)+1, right(row)-1
		c.text(lo+(hi-lo+1-dispWidth(l))/2, y+row, l)
	}
}

// drawClassNode renders a UML-style box with name/attribute/method compartments.
func drawClassNode(c *canvas, n *gnode) {
	x, y, w := n.x, n.y, n.w
	c.rect(x, y, w, n.h)
	ry := y + 1
	// title compartment: centered
	for _, l := range n.sections[0] {
		c.text(x+(w-dispWidth(l))/2, ry, l)
		ry++
	}
	if len(n.sections[1]) == 0 && len(n.sections[2]) == 0 {
		return
	}
	sep := func() {
		c.put(x, ry, '+')
		for i := 1; i < w-1; i++ {
			c.put(x+i, ry, '-')
		}
		c.put(x+w-1, ry, '+')
		ry++
	}
	sep()
	for _, l := range n.sections[1] {
		c.put(x, ry, '|')
		c.text(x+2, ry, l)
		c.put(x+w-1, ry, '|')
		ry++
	}
	sep()
	for _, l := range n.sections[2] {
		c.put(x, ry, '|')
		c.text(x+2, ry, l)
		c.put(x+w-1, ry, '|')
		ry++
	}
}
