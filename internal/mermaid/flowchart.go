package mermaid

import (
	"fmt"
	"regexp"
	"strings"
	"unicode/utf8"
)

// parseFlowchart parses `flowchart` / `graph` diagrams into a graph.
func parseFlowchart(lines []string) (*graph, error) {
	g := newGraph()
	first := true
	subgraphDepth := 0
	var stmts []string
	for _, raw := range lines {
		for _, part := range splitStatements(raw) {
			if strings.TrimSpace(part) != "" {
				stmts = append(stmts, part)
			}
		}
	}
	for _, raw := range stmts {
		line := strings.TrimSpace(raw)
		if line == "" {
			continue
		}
		if first {
			first = false
			// header: flowchart TD / graph LR / flowchart-elk TB ...
			fields := strings.Fields(line)
			if len(fields) > 1 {
				g.dir = normalizeDir(fields[1])
			}
			continue
		}
		lower := strings.ToLower(line)
		switch {
		case strings.HasPrefix(lower, "subgraph"):
			subgraphDepth++
			continue
		case lower == "end":
			if subgraphDepth > 0 {
				subgraphDepth--
			}
			continue
		case strings.HasPrefix(lower, "direction "):
			if subgraphDepth == 0 {
				g.dir = normalizeDir(strings.TrimSpace(line[len("direction "):]))
			}
			continue
		case strings.HasPrefix(lower, "classdef "),
			strings.HasPrefix(lower, "class "),
			strings.HasPrefix(lower, "style "),
			strings.HasPrefix(lower, "linkstyle "),
			strings.HasPrefix(lower, "click "),
			strings.HasPrefix(lower, "acctitle"),
			strings.HasPrefix(lower, "accdescr"):
			continue
		}
		if err := parseFlowStmt(g, line); err != nil {
			return nil, fmt.Errorf("flowchart: %w (in %q)", err, line)
		}
	}
	return g, nil
}

// splitStatements splits a line on `;` separators, leaving semicolons that sit
// inside a quoted label or terminate an HTML entity (`&amp;`, `#35;`) alone.
func splitStatements(line string) []string {
	var out []string
	start, quoted := 0, false
	for i := 0; i < len(line); i++ {
		switch line[i] {
		case '"':
			quoted = !quoted
		case ';':
			if quoted || endsEntity(line[:i]) {
				continue
			}
			out = append(out, line[start:i])
			start = i + 1
		}
	}
	return append(out, line[start:])
}

// endsEntity reports whether s ends with the body of an HTML entity
// reference, i.e. whether appending `;` to it would complete one.
func endsEntity(s string) bool {
	i := len(s)
	for i > 0 && isAlnum(s[i-1]) {
		i--
	}
	if i == len(s) || i == 0 {
		return false // no body, or nothing before it to open the entity
	}
	return s[i-1] == '&' || s[i-1] == '#'
}

func isAlnum(b byte) bool {
	return ('a' <= b && b <= 'z') || ('A' <= b && b <= 'Z') || ('0' <= b && b <= '9')
}

func normalizeDir(s string) string {
	switch strings.ToUpper(strings.TrimSpace(s)) {
	case "LR":
		return "LR"
	case "RL":
		return "RL"
	case "BT":
		return "BT"
	}
	return "TD"
}

// parseFlowStmt parses one statement: nodes joined by links, with & lists,
// e.g. `A[Start] --> B & C -->|ok| D`.
func parseFlowStmt(g *graph, stmt string) error {
	p := &flowParser{s: stmt, g: g}
	left, err := p.nodeList()
	if err != nil {
		return err
	}
	if len(left) == 0 {
		return fmt.Errorf("expected node")
	}
	for {
		p.ws()
		if p.eof() {
			return nil
		}
		link, ok := p.link()
		if !ok {
			return fmt.Errorf("expected link at %q", string(p.s[p.i:]))
		}
		right, err := p.nodeList()
		if err != nil {
			return err
		}
		if len(right) == 0 {
			return fmt.Errorf("expected node after link")
		}
		// Check the Cartesian product before allocating even its first edge.
		if len(left) > (maxEdges-len(g.edges))/len(right) {
			return limitError("edges", maxEdges)
		}
		for _, a := range left {
			for _, b := range right {
				if err := g.addEdge(&gedge{
					from: a, to: b,
					label: link.label,
					line:  link.line,
					sm:    link.sm, em: link.em,
				}); err != nil {
					return err
				}
			}
		}
		left = right
	}
}

type linkTok struct {
	line   lineKind
	sm, em marker
	label  string
}

type flowParser struct {
	s string
	i int
	g *graph
}

func (p *flowParser) eof() bool { return p.i >= len(p.s) }

func (p *flowParser) ws() {
	for !p.eof() && (p.s[p.i] == ' ' || p.s[p.i] == '\t') {
		p.i++
	}
}

func (p *flowParser) peek() rune {
	if p.eof() {
		return 0
	}
	r, _ := utf8.DecodeRuneInString(p.s[p.i:])
	return r
}

func isWordRune(r rune) bool {
	return r == '_' || r == '.' ||
		('a' <= r && r <= 'z') || ('A' <= r && r <= 'Z') || ('0' <= r && r <= '9') ||
		r > 127
}

// nodeList parses `node (& node)*`.
func (p *flowParser) nodeList() ([]*gnode, error) {
	var out []*gnode
	for {
		p.ws()
		n, err := p.nodeRef()
		if err != nil {
			return nil, err
		}
		if n == nil {
			return out, nil
		}
		if len(out) >= maxNodes {
			return nil, limitError("node list entries", maxNodes)
		}
		out = append(out, n)
		p.ws()
		if p.peek() == '&' {
			p.i++
			continue
		}
		return out, nil
	}
}

var shapeOpeners = []struct {
	open, close string
	kind        boxKind
}{
	{"(((", ")))", boxCircle},
	{"((", "))", boxCircle},
	{"([", "])", boxStadium},
	{"[[", "]]", boxSubroutine},
	{"[(", ")]", boxCylinder},
	{"{{", "}}", boxHex},
	// slanted-side shapes: the opener alone is ambiguous, so the closer
	// decides between a parallelogram and a trapezoid
	{"[/", "/]", boxParallelogram},
	{"[/", "\\]", boxTrapezoid},
	{"[\\", "\\]", boxParallelogramAlt},
	{"[\\", "/]", boxTrapezoidAlt},
	{"[", "]", boxRect},
	{"(", ")", boxRound},
	{"{", "}", boxDiamond},
	{">", "]", boxAsym},
}

// nodeRef parses an identifier with an optional shape+label, e.g. `B{Is it?}`.
// Returns nil (no error) if no identifier is present.
func (p *flowParser) nodeRef() (*gnode, error) {
	p.ws()
	start := p.i
	for !p.eof() {
		r, size := utf8.DecodeRuneInString(p.s[p.i:])
		if isWordRune(r) {
			p.i += size
			continue
		}
		// allow single dashes inside ids (node-1) but not link starts (--)
		next, nextSize := utf8.DecodeRuneInString(p.s[p.i+1:])
		if r == '-' && p.i+1 < len(p.s) && isWordRune(next) && p.i > start {
			p.i += 1 + nextSize
			continue
		}
		break
	}
	if p.i == start {
		return nil, nil
	}
	id := string(p.s[start:p.i])
	n, err := p.g.node(id)
	if err != nil {
		return nil, err
	}
	// optional shape: the longest matching opener wins, and among the shapes
	// sharing it (`[/` starts both a parallelogram and a trapezoid) the one
	// that closes soonest does — otherwise `[\a/] --> b[\c\]` would swallow
	// everything up to the far `\]`.
	rest := string(p.s[p.i:])
	for _, sh := range shapeOpeners {
		if !strings.HasPrefix(rest, sh.open) {
			continue
		}
		best := struct {
			kind     boxKind
			body     string
			consumed int
			found    bool
		}{}
		for _, cand := range shapeOpeners {
			if cand.open != sh.open {
				continue
			}
			body, consumed, ok := scanShapeBody(rest, cand.open, cand.close)
			if !ok || (best.found && consumed >= best.consumed) {
				continue
			}
			best.kind, best.body, best.consumed, best.found = cand.kind, body, consumed, true
		}
		if !best.found {
			return nil, fmt.Errorf("unclosed %q after %q", sh.open, id)
		}
		p.i += best.consumed
		n.kind = best.kind
		n.lines = splitLabel(best.body)
		return n, nil
	}
	return n, nil
}

// scanShapeBody extracts the text between open and close, honoring quotes.
func scanShapeBody(s, open, close string) (body string, consumed int, ok bool) {
	i := len(open)
	if i < len(s) && s[i] == '"' {
		j := strings.Index(s[i+1:], "\"")
		if j < 0 {
			return "", 0, false
		}
		body = s[i+1 : i+1+j]
		rest := s[i+1+j+1:]
		if !strings.HasPrefix(rest, close) {
			return "", 0, false
		}
		return body, i + 1 + j + 1 + len(close), true
	}
	j := strings.Index(s[i:], close)
	if j < 0 {
		return "", 0, false
	}
	return strings.TrimSpace(s[i : i+j]), i + j + len(close), true
}

var (
	reSolidClose  = regexp.MustCompile(`^([^-]*?)-{2,}([>xo]?)`)
	reDottedClose = regexp.MustCompile(`^([^.]*?)\.+-*([>xo]?)`)
	reThickClose  = regexp.MustCompile(`^([^=]*?)={2,}([>xo]?)`)
)

// link parses a link token such as `-->`, `---`, `-.->`, `==>`, `--x`,
// `<-->`, `~~~`, `-- label -->` or `-->|label|`.
func (p *flowParser) link() (linkTok, bool) {
	p.ws()
	save := p.i
	var lt linkTok

	// invisible link
	if p.peek() == '~' {
		for p.peek() == '~' {
			p.i++
		}
		lt.line = lineNone
		return lt, true
	}

	isLinkChar := func(r rune) bool { return r == '-' || r == '=' || r == '.' }

	// start marker
	if !p.eof() && p.i+1 < len(p.s) && isLinkChar(rune(p.s[p.i+1])) {
		switch p.s[p.i] {
		case '<':
			lt.sm = mArrow
			p.i++
		case 'x':
			lt.sm = mCross
			p.i++
		case 'o':
			lt.sm = mDiamondOpen
			p.i++
		}
	}
	runStart := p.i
	hasDot, hasEq := false, false
	for !p.eof() && isLinkChar(rune(p.s[p.i])) {
		if p.s[p.i] == '.' {
			hasDot = true
		}
		if p.s[p.i] == '=' {
			hasEq = true
		}
		p.i++
	}
	if p.i-runStart < 2 {
		p.i = save
		return lt, false
	}
	switch {
	case hasDot:
		lt.line = lineDotted
	case hasEq:
		lt.line = lineThick
	default:
		lt.line = lineSolid
	}
	// end marker
	if !p.eof() {
		switch p.s[p.i] {
		case '>':
			lt.em = mArrow
			p.i++
		case 'x', 'o':
			// only a marker at a word boundary (`--x B`, not `--x1`)
			if p.i+1 >= len(p.s) || !isWordRune(rune(p.s[p.i+1])) {
				if p.s[p.i] == 'x' {
					lt.em = mCross
				} else {
					lt.em = mDiamondOpen
				}
				p.i++
			}
		}
	}
	// inline label: `-- label -->`, `-. label .->`, `== label ==>`
	if lt.em == mNone && lt.sm == mNone {
		rest := string(p.s[p.i:])
		var re *regexp.Regexp
		switch lt.line {
		case lineDotted:
			re = reDottedClose
		case lineThick:
			re = reThickClose
		default:
			re = reSolidClose
		}
		if m := re.FindStringSubmatch(rest); m != nil && strings.TrimSpace(m[1]) != "" {
			lt.label = strings.TrimSpace(m[1])
			switch m[2] {
			case ">":
				lt.em = mArrow
			case "x":
				lt.em = mCross
			case "o":
				lt.em = mDiamondOpen
			}
			p.i += len(m[0])
		}
	}
	// pipe label: `-->|label|`
	p.ws()
	if p.peek() == '|' {
		p.i++
		start := p.i
		for !p.eof() && p.s[p.i] != '|' {
			p.i++
		}
		if !p.eof() {
			lt.label = strings.TrimSpace(stripQuotes(string(p.s[start:p.i])))
			p.i++
		}
	}
	return lt, true
}

func stripQuotes(s string) string {
	if len(s) >= 2 && s[0] == '"' && s[len(s)-1] == '"' {
		return s[1 : len(s)-1]
	}
	return s
}

var reBr = regexp.MustCompile(`(?i)<br\s*/?>`)

var entityReplacer = strings.NewReplacer(
	"&amp;", "&", "&lt;", "<", "&gt;", ">", "&quot;", "\"",
	"&#35;", "#", "#35;", "#", "&#59;", ";", "#59;", ";",
)

// splitLabel normalizes a node/edge label into display lines.
func splitLabel(s string) []string {
	s = stripQuotes(strings.TrimSpace(s))
	s = strings.TrimSpace(strings.Trim(s, "`"))
	s = entityReplacer.Replace(s)
	s = reBr.ReplaceAllString(s, "\n")
	parts := strings.Split(s, "\n")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		out = append(out, strings.TrimSpace(p))
	}
	if len(out) == 0 {
		out = []string{""}
	}
	return out
}
