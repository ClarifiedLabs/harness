// Package mermaid renders Mermaid diagrams as ASCII text using only the Go
// standard library.
//
// Supported diagram types: flowcharts (flowchart/graph), sequence diagrams,
// state diagrams (stateDiagram / stateDiagram-v2) and class diagrams.
//
//	out, err := mermaid.Render("flowchart TD\n  A[Start] --> B{OK?}\n  B -->|yes| C[Done]", mermaid.Options{Width: 80})
package mermaid

import (
	"fmt"
	"regexp"
	"strings"
)

// Kind identifies a Mermaid diagram type.
type Kind string

const (
	KindUnknown   Kind = ""
	KindFlowchart Kind = "flowchart"
	KindSequence  Kind = "sequence"
	KindState     Kind = "state"
	KindClass     Kind = "class"
)

// Detect returns the diagram type declared by the source, or KindUnknown if the
// source exceeds the rendering byte budget.
func Detect(source string) Kind {
	if len(source) > MaxSourceBytes {
		return KindUnknown
	}
	lines, _ := preprocess(source)
	return detectLines(lines)
}

func detectLines(lines []string) Kind {
	for _, l := range lines {
		l = strings.TrimSpace(l)
		if l == "" {
			continue
		}
		word := strings.ToLower(strings.Fields(l)[0])
		switch {
		case word == "flowchart" || word == "graph" || word == "flowchart-elk" ||
			strings.HasPrefix(word, "flowchart"):
			return KindFlowchart
		case word == "sequencediagram":
			return KindSequence
		case strings.HasPrefix(word, "statediagram"):
			return KindState
		case strings.HasPrefix(word, "classdiagram"):
			return KindClass
		}
		return KindUnknown
	}
	return KindUnknown
}

// DisplayWidth measures unstyled text in the renderer's terminal-column model.
// Callers reserving space for a prefix should strip ANSI styling first.
func DisplayWidth(text string) int { return dispWidth(text) }

// Options controls diagram rendering.
type Options struct {
	// Width is the available terminal columns; it must be positive for flowcharts.
	// Node labels may wrap, horizontal layouts may rotate, and complex diagrams
	// may become a Nodes / Connections list. Other diagram kinds ignore Width.
	Width int
}

// Render parses Mermaid and renders ASCII art, adapting flowcharts to Width.
// Successful output ends with a newline. Sources or diagrams that exceed the
// fixed budgets documented in limits.go return an error, not partial output.
func Render(source string, opts Options) (string, error) {
	if len(source) > MaxSourceBytes {
		return "", limitError("source bytes", MaxSourceBytes)
	}
	lines, title := preprocess(source)
	kind := detectLines(lines)
	var out string
	var err error
	switch kind {
	case KindFlowchart:
		if opts.Width <= 0 {
			return "", fmt.Errorf("mermaid: flowchart width must be positive")
		}
		return renderResponsiveFlowchart(lines, title, opts.Width)
	case KindSequence:
		var d *seqDiagram
		d, err = parseSequence(lines)
		if err == nil {
			out, err = renderSequence(d)
		}
	case KindState:
		var g *graph
		g, err = parseState(lines)
		if err == nil {
			out, err = g.render()
		}
	case KindClass:
		var g *graph
		g, err = parseClass(lines)
		if err == nil {
			out, err = g.render()
		}
	default:
		return "", fmt.Errorf("mermaid: unrecognized or unsupported diagram type")
	}
	if err != nil {
		return "", err
	}
	if strings.TrimSpace(out) == "" {
		return "", fmt.Errorf("mermaid: diagram has no content")
	}
	if title != "" {
		out, err = addTitle(title, out)
		if err != nil {
			return "", err
		}
	}
	return out, nil
}

var reDirective = regexp.MustCompile(`%%\{[^}]*\}%%`)

// preprocess splits the source into lines, stripping comments, init
// directives and YAML front matter (whose title, if any, is returned).
func preprocess(source string) (lines []string, title string) {
	source = strings.ReplaceAll(source, "\r\n", "\n")
	source = reDirective.ReplaceAllString(source, "")
	raw := strings.Split(source, "\n")
	// front matter
	start := 0
	for start < len(raw) && strings.TrimSpace(raw[start]) == "" {
		start++
	}
	if start < len(raw) && strings.TrimSpace(raw[start]) == "---" {
		for i := start + 1; i < len(raw); i++ {
			t := strings.TrimSpace(raw[i])
			if t == "---" {
				start = i + 1
				break
			}
			if strings.HasPrefix(t, "title:") {
				title = strings.TrimSpace(strings.TrimPrefix(t, "title:"))
			}
		}
	}
	for _, l := range raw[start:] {
		t := strings.TrimSpace(l)
		if strings.HasPrefix(t, "%%") {
			continue
		}
		lines = append(lines, l)
	}
	return lines, title
}

// addTitle centers the title above the rendered diagram.
func addTitle(title, out string) (string, error) {
	w := 0
	lines := strings.Split(strings.TrimSuffix(out, "\n"), "\n")
	for _, l := range lines {
		if dispWidth(l) > w {
			w = dispWidth(l)
		}
	}
	if err := checkCanvasSize(maxInt(w, dispWidth(title)), len(lines)+2); err != nil {
		return "", err
	}
	pad := (w - dispWidth(title)) / 2
	if pad < 0 {
		pad = 0
	}
	return strings.Repeat(" ", pad) + title + "\n\n" + out, nil
}
