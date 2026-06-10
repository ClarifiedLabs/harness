package ui

import (
	"fmt"
	"io"
	"strings"
)

// NameDescription is one row in a terminal name/description list.
type NameDescription struct {
	Name        string
	Description string
}

// NameDescriptionListOptions controls terminal rendering for name/description
// lists. Width is the full terminal width; zero disables forced wrapping.
type NameDescriptionListOptions struct {
	Indent     string
	NamePrefix string
	Separator  string
	Width      int
}

// WriteNameDescriptionList writes a padded name column followed by descriptions.
// Wrapped description lines align under the first description column.
func WriteNameDescriptionList(w io.Writer, rows []NameDescription, opts NameDescriptionListOptions) {
	if opts.Separator == "" {
		opts.Separator = "  "
	}
	nameWidth := 0
	for _, row := range rows {
		if n := len(opts.NamePrefix + row.Name); n > nameWidth {
			nameWidth = n
		}
	}
	for _, row := range rows {
		displayName := opts.NamePrefix + row.Name
		if strings.TrimSpace(row.Description) == "" {
			fmt.Fprintf(w, "%s%s\n", opts.Indent, displayName)
			continue
		}
		descIndent := opts.Indent + strings.Repeat(" ", nameWidth) + strings.Repeat(" ", len(opts.Separator))
		descWidth := 0
		if opts.Width > len(descIndent) {
			descWidth = opts.Width - len(descIndent)
		}
		lines := wrapWords(row.Description, descWidth)
		if len(lines) == 0 {
			fmt.Fprintf(w, "%s%-*s%s\n", opts.Indent, nameWidth, displayName, opts.Separator)
			continue
		}
		fmt.Fprintf(w, "%s%-*s%s%s\n", opts.Indent, nameWidth, displayName, opts.Separator, lines[0])
		for _, line := range lines[1:] {
			fmt.Fprintf(w, "%s%s\n", descIndent, line)
		}
	}
}

func wrapWords(s string, width int) []string {
	words := strings.Fields(s)
	if len(words) == 0 {
		return nil
	}
	if width <= 0 {
		return []string{strings.Join(words, " ")}
	}
	var lines []string
	var line strings.Builder
	for _, word := range words {
		if line.Len() == 0 {
			line.WriteString(word)
			continue
		}
		if line.Len()+1+len(word) > width {
			lines = append(lines, line.String())
			line.Reset()
			line.WriteString(word)
			continue
		}
		line.WriteByte(' ')
		line.WriteString(word)
	}
	if line.Len() > 0 {
		lines = append(lines, line.String())
	}
	return lines
}
