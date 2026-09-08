package mermaid

import (
	"regexp"
	"strings"
)

var (
	reClassRel = regexp.MustCompile(
		`^([A-Za-z_][\w.]*(?:~[\w,. ]+~)?)\s*(?:"([^"]*)"\s*)?` +
			`((?:<\||[*ox<])?(?:--|\.\.)(?:\|>|[*ox>])?)\s*` +
			`(?:"([^"]*)"\s*)?([A-Za-z_][\w.]*(?:~[\w,. ]+~)?)\s*(?::\s*(.*))?$`)
	reClassDecl   = regexp.MustCompile(`^class\s+([A-Za-z_][\w.]*(?:~[\w,. ]+~)?)(?:\s*\{)?\s*$`)
	reClassMember = regexp.MustCompile(`^([A-Za-z_][\w.]*(?:~[\w,. ]+~)?)\s*:\s*(.+)$`)
	reClassAnnot  = regexp.MustCompile(`^<<([^<>]+)>>\s*([A-Za-z_][\w.]*)?\s*$`)
	reGenerics    = regexp.MustCompile(`~([^~]*)~`)
)

type classInfo struct {
	name        string
	annotations []string
	attrs       []string
	methods     []string
}

// parseClass parses classDiagram sources into a graph of boxClass nodes.
func parseClass(lines []string) (*graph, error) {
	g := newGraph()
	classes := map[string]*classInfo{}
	getClass := func(name string) (*classInfo, error) {
		base := reGenerics.ReplaceAllString(name, "")
		if ci, ok := classes[base]; ok {
			return ci, nil
		}
		if _, err := g.node(base); err != nil {
			return nil, err
		}
		ci := &classInfo{name: displayGenerics(name)}
		classes[base] = ci
		return ci, nil
	}
	addMember := func(ci *classInfo, m string) {
		m = strings.TrimSpace(displayGenerics(m))
		if m == "" {
			return
		}
		if am := reClassAnnot.FindStringSubmatch(m); am != nil {
			ci.annotations = append(ci.annotations, "<<"+am[1]+">>")
			return
		}
		if strings.Contains(m, "(") {
			ci.methods = append(ci.methods, m)
		} else {
			ci.attrs = append(ci.attrs, m)
		}
	}

	first := true
	var current *classInfo // inside a `class X { ... }` block
	for _, raw := range lines {
		line := strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(raw), ";"))
		if line == "" {
			continue
		}
		if first {
			first = false
			// classDiagram header, may carry a direction on following lines
			continue
		}
		if current != nil {
			if line == "}" {
				current = nil
				continue
			}
			addMember(current, line)
			continue
		}
		// Relations are matched before the directive keywords below, so that a
		// class named after one of them (`Link -- Peer`, `Style --> Theme`) is
		// not mistaken for a directive. A real directive never carries the
		// `--` or `..` this pattern requires.
		if m := reClassRel.FindStringSubmatch(line); m != nil {
			if err := addRelation(g, getClass, m); err != nil {
				return nil, err
			}
			continue
		}
		lower := strings.ToLower(line)
		switch {
		case strings.HasPrefix(lower, "direction "):
			g.dir = normalizeDir(line[len("direction "):])
			continue
		case strings.HasPrefix(lower, "namespace "):
			continue // flatten namespaces
		case line == "}":
			continue
		case lower == "note", strings.HasPrefix(lower, "note "), strings.HasPrefix(lower, `note"`):
			continue
		case strings.HasPrefix(lower, "style "),
			strings.HasPrefix(lower, "classdef "),
			strings.HasPrefix(lower, "cssclass "),
			strings.HasPrefix(lower, "link "),
			strings.HasPrefix(lower, "click "),
			strings.HasPrefix(lower, "callback "),
			strings.HasPrefix(lower, "accdescr"),
			strings.HasPrefix(lower, "acctitle"):
			continue
		}
		if m := reClassDecl.FindStringSubmatch(line); m != nil {
			ci, err := getClass(m[1])
			if err != nil {
				return nil, err
			}
			if strings.HasSuffix(line, "{") {
				current = ci
			}
			continue
		}
		if m := reClassAnnot.FindStringSubmatch(line); m != nil && m[2] != "" {
			ci, err := getClass(m[2])
			if err != nil {
				return nil, err
			}
			ci.annotations = append(ci.annotations, "<<"+m[1]+">>")
			continue
		}
		if m := reClassMember.FindStringSubmatch(line); m != nil {
			ci, err := getClass(m[1])
			if err != nil {
				return nil, err
			}
			addMember(ci, m[2])
			continue
		}
	}
	// build class boxes
	for _, n := range g.nodes {
		ci := classes[n.id]
		if ci == nil {
			ci = &classInfo{name: n.id}
		}
		title := append(append([]string{}, ci.annotations...), ci.name)
		n.kind = boxClass
		n.sections = [][]string{title, ci.attrs, ci.methods}
	}
	return g, nil
}

// addRelation turns a matched relation statement into an edge, decorating each
// end with the marker its side of the arrow calls for.
func addRelation(g *graph, getClass func(string) (*classInfo, error), m []string) error {
	if _, err := getClass(m[1]); err != nil {
		return err
	}
	if _, err := getClass(m[5]); err != nil {
		return err
	}
	rel := m[3]
	e := &gedge{
		from: g.index[reGenerics.ReplaceAllString(m[1], "")],
		to:   g.index[reGenerics.ReplaceAllString(m[5], "")],
	}
	if strings.Contains(rel, "..") {
		e.line = lineDotted
	}
	switch {
	case strings.HasPrefix(rel, "<|"):
		e.sm = mTriangle
	case strings.HasPrefix(rel, "*"):
		e.sm = mDiamondFilled
	case strings.HasPrefix(rel, "o"):
		e.sm = mDiamondOpen
	case strings.HasPrefix(rel, "<"):
		e.sm = mArrow
	case strings.HasPrefix(rel, "x"):
		e.sm = mCross
	}
	switch {
	case strings.HasSuffix(rel, "|>"):
		e.em = mTriangle
	case strings.HasSuffix(rel, "*"):
		e.em = mDiamondFilled
	case strings.HasSuffix(rel, "o"):
		e.em = mDiamondOpen
	case strings.HasSuffix(rel, ">"):
		e.em = mArrow
	case strings.HasSuffix(rel, "x"):
		e.em = mCross
	}
	var parts []string
	for _, p := range []string{m[2], strings.TrimSpace(m[6]), m[4]} {
		if p != "" {
			parts = append(parts, p)
		}
	}
	e.label = strings.Join(parts, " ")
	return g.addEdge(e)
}

// displayGenerics converts mermaid generic syntax (List~T~) to List<T>.
func displayGenerics(s string) string {
	return reGenerics.ReplaceAllString(s, "<$1>")
}
