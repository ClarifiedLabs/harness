package mermaid

import (
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"
)

// There are at most four label choices in each of two orientations. Never
// shrink labels to single-letter boxes or wrap completed diagram art.
const minFlowchartWidth = 20

func renderResponsiveFlowchart(lines []string, title string, width int) (string, error) {
	pristine, err := parseFlowchart(lines)
	if err != nil {
		return "", err
	}
	if len(pristine.nodes) == 0 {
		return "", fmt.Errorf("mermaid: diagram has no content")
	}
	forceList := needsFlowList(pristine)
	dirs := []string{pristine.dir}
	if pristine.horizontal() {
		vertical := "TD"
		if pristine.dir == "RL" {
			vertical = "BT"
		}
		dirs = append(dirs, vertical)
	}
	for _, dir := range dirs {
		var previous string
		for _, cap := range []int{0, 32, 24, 16} {
			// render mutates endpoints, ranks and virtual nodes. Every attempt gets a
			// fresh parse; the declaration-ordered pristine graph belongs to the list.
			g, err := parseFlowchart(lines)
			if err != nil {
				return "", err
			}
			g.dir = dir
			var key strings.Builder
			for _, n := range g.nodes {
				if cap > 0 {
					n.lines = wrapFlowText(strings.Join(n.lines, "\n"), cap)
				}
				// Length-delimited lines distinguish explicit breaks and empty labels.
				for _, line := range n.lines {
					fmt.Fprintf(&key, "%d:%s", len(line), line)
				}
				key.WriteByte('\n')
			}
			signature := key.String()
			if cap != 0 && signature == previous {
				continue
			}
			previous = signature
			out, err := g.render()
			// A safety budget failure is not a width miss. Do not hide it with a list
			// or by trying a smaller candidate (also validates the natural layout).
			if err != nil {
				return "", err
			}
			out, err = addResponsiveTitle(title, out, width)
			if err != nil {
				return "", err
			}
			if !g.labelOverlap && !g.labelClipped && !forceList && maxLineLen(strings.Split(out, "\n")) <= width {
				return out, nil
			}
			if width < minFlowchartWidth || forceList {
				return renderFlowList(pristine, title, width)
			}
		}
	}
	return renderFlowList(pristine, title, width)
}

// Self-loop drawing has one fixed, solid, arrowed route per node. It
// cannot faithfully represent other markers/styles or multiple loops. Keep
// that renderer unchanged, but don't accept these as readable adaptive art.
func needsFlowList(g *graph) bool {
	seen := map[*gnode]bool{}
	for _, e := range g.edges {
		if !e.self || e.line == lineNone {
			continue
		}
		if seen[e.from] || e.line != lineSolid || e.sm != mNone || e.em != mArrow {
			return true
		}
		seen[e.from] = true
	}
	return false
}

// wrapFlowText preserves explicit breaks and splits overlong words only at
// UTF-8 boundaries, using the same terminal-column model as the canvas. Zero-
// width runes stay with the preceding rune. A wide rune at width 1 is emitted
// intact, the only unavoidable width exception for textual output.
func wrapFlowText(text string, width int) []string {
	var out []string
	for _, line := range strings.Split(text, "\n") {
		if dispWidth(line) <= width {
			out = append(out, line)
			continue
		}
		for line != "" {
			end, columns, space := 0, 0, -1
			for i, r := range line {
				w := runeWidth(r)
				if unicode.IsSpace(r) {
					space = i
				}
				if w > 0 && columns+w > width && end > 0 {
					break
				}
				columns += w
				_, size := utf8.DecodeRuneInString(line[i:])
				end = i + size
			}
			if end < len(line) && space > 0 {
				end = space
			}
			out = append(out, strings.TrimRightFunc(line[:end], unicode.IsSpace))
			line = strings.TrimLeftFunc(line[end:], unicode.IsSpace)
		}
	}
	return out
}

// addResponsiveTitle never allocates padding based on the requested width.
// Its bounding rectangle includes every wrapped title line and the diagram.
func addResponsiveTitle(title, out string, width int) (string, error) {
	if title == "" {
		return out, nil
	}
	titleLines := wrapFlowText(title, min(width, maxCanvasDimension))
	diagramLines := strings.Split(strings.TrimSuffix(out, "\n"), "\n")
	w := maxInt(maxLineLen(diagramLines), maxLineLen(titleLines))
	if err := checkCanvasSize(w, len(diagramLines)+len(titleLines)+1); err != nil {
		return "", err
	}
	// Overflowing art will be rejected; centering its title must not create
	// needless overwide lines or huge intermediate padding.
	center := min(w, width)
	bytes := len(out) + 1
	for _, line := range titleLines {
		n := maxInt(0, (center-dispWidth(line))/2) + len(line) + 1
		if n > 4*maxCanvasCells-bytes {
			return "", limitError("output bytes", 4*maxCanvasCells)
		}
		bytes += n
	}
	var b strings.Builder
	for _, line := range titleLines {
		b.WriteString(strings.Repeat(" ", maxInt(0, (center-dispWidth(line))/2)))
		b.WriteString(line)
		b.WriteByte('\n')
	}
	b.WriteByte('\n')
	b.WriteString(out)
	return b.String(), nil
}

// listWriter bounds both the bounding rectangle and actual UTF-8 output bytes
// (zero-width characters consume bytes but no cells). Each item is bounded by
// the source budget; don't concatenate all repeated edge IDs/labels up front.
type listWriter struct {
	b                    strings.Builder
	width, rows, columns int
	err                  error
}

func (w *listWriter) text(text string) {
	if w.err != nil {
		return
	}
	for _, line := range wrapFlowText(text, w.width) {
		columns := maxInt(w.columns, dispWidth(line))
		if err := checkCanvasSize(columns, w.rows+1); err != nil {
			w.err = err
			return
		}
		if len(line)+1 > 4*maxCanvasCells-w.b.Len() {
			w.err = limitError("output bytes", 4*maxCanvasCells)
			return
		}
		w.columns = columns
		w.rows++
		w.b.WriteString(line)
		w.b.WriteByte('\n')
	}
}

// item indents explicit and wrapped continuation lines so label text cannot
// masquerade as another node, connection, or section header. Reserve at least
// one content column on tiny displays, where indentation may be impossible.
func (w *listWriter) item(text string) {
	if w.err != nil {
		return
	}
	prefix := ""
	if w.width >= minFlowchartWidth {
		prefix = "- "
	}
	continuation := strings.Repeat(" ", min(2, w.width-1))
	for i, line := range wrapFlowText(text, w.width-len(continuation)) {
		if i > 0 {
			prefix = continuation
		}
		if line == "" {
			w.text("")
		} else {
			w.text(prefix + line)
		}
		if w.err != nil {
			return
		}
	}
}

func renderFlowList(g *graph, title string, width int) (string, error) {
	w := &listWriter{width: min(width, maxCanvasDimension)}
	w.text("Flowchart shown as a list to fit the display.")
	w.text("")
	w.text("Nodes:")
	for _, n := range g.nodes {
		w.item(n.id + ": " + strings.Join(n.lines, "\n"))
		if w.err != nil {
			return "", w.err
		}
	}
	w.text("")
	w.text("Connections:")
	for _, e := range g.edges {
		if e.line == lineNone {
			continue
		}
		text := e.from.id + " " + flowEdgeToken(e) + " " + e.to.id
		if e.label != "" {
			text += ": " + e.label
		}
		w.item(text)
		if w.err != nil {
			return "", w.err
		}
	}
	if w.err != nil {
		return "", w.err
	}
	return addResponsiveTitle(title, w.b.String(), width)
}

func flowEdgeToken(e *gedge) string {
	start, end := flowMarker(e.sm, true), flowMarker(e.em, false)
	middle := "--"
	switch e.line {
	case lineDotted:
		middle = "-.-"
	case lineThick:
		middle = "=="
	}
	if start == "" && end == "" && e.line != lineDotted {
		middle += middle[:1]
	}
	return start + middle + end
}

func flowMarker(m marker, start bool) string {
	switch m {
	case mArrow, mTriangle:
		if start {
			return "<"
		}
		return ">"
	case mDiamondFilled:
		return "*"
	case mDiamondOpen, mCircle:
		return "o"
	case mCross:
		return "x"
	}
	return ""
}
