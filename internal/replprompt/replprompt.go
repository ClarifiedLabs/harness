// Package replprompt parses and renders the interactive REPL prompt format.
package replprompt

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"
	"unicode/utf8"
)

// DefaultFormat is the default REPL prompt template.
const DefaultFormat = "[{agent}] > "

const gitTimeout = 250 * time.Millisecond

type field string

const (
	fieldAgent       field = "agent"
	fieldCWD         field = "cwd"
	fieldGitBranch   field = "git_branch"
	fieldProvider    field = "provider"
	fieldModel       field = "model"
	fieldModelInfo   field = "model_info"
	fieldViMode      field = "vimode"
	fieldViModeLong  field = "vimode:long"
	fieldViModeShort field = "vimode:short"
)

// Values carries the runtime values available to a REPL prompt template.
type Values struct {
	Agent     string
	CWD       string
	GitBranch string
	Provider  string
	Model     string
	ModelInfo string
	// ViMode is the current raw-prompt vi edit mode: "insert", "normal", or ""
	// (empty outside vi mode, e.g. emacs mode). {vimode} renders a label for it.
	ViMode string
}

// Template is a compiled REPL prompt format string.
type Template struct {
	parts []part
}

type part struct {
	literal string
	field   field
}

// Compile validates and compiles a REPL prompt format string.
func Compile(format string) (*Template, error) {
	if format == "" {
		format = DefaultFormat
	}

	var parts []part
	var literal strings.Builder
	flushLiteral := func() {
		if literal.Len() == 0 {
			return
		}
		parts = append(parts, part{literal: literal.String()})
		literal.Reset()
	}

	for i := 0; i < len(format); {
		switch format[i] {
		case '\\':
			if i+1 >= len(format) {
				return nil, fmt.Errorf("dangling escape")
			}
			next := format[i+1]
			switch next {
			case 'n':
				literal.WriteByte('\n')
			case 't':
				literal.WriteByte('\t')
			case '\\', '{', '}':
				literal.WriteByte(next)
			default:
				return nil, fmt.Errorf("invalid escape \\%c", next)
			}
			i += 2
		case '{':
			end := strings.IndexByte(format[i+1:], '}')
			if end < 0 {
				return nil, fmt.Errorf("unclosed placeholder")
			}
			name := format[i+1 : i+1+end]
			f, ok := parseField(name)
			if !ok {
				if name == "" {
					return nil, fmt.Errorf("empty placeholder")
				}
				return nil, fmt.Errorf("unknown placeholder {%s}", name)
			}
			flushLiteral()
			parts = append(parts, part{field: f})
			i += end + 2
		case '}':
			return nil, fmt.Errorf("unmatched }")
		default:
			r, size := utf8.DecodeRuneInString(format[i:])
			literal.WriteRune(r)
			i += size
		}
	}
	flushLiteral()
	return &Template{parts: parts}, nil
}

// Validate returns whether format is a valid REPL prompt format string.
func Validate(format string) error {
	_, err := Compile(format)
	return err
}

// Render renders the compiled template with values.
func (t *Template) Render(values Values) string {
	if t == nil {
		t, _ = Compile(DefaultFormat)
	}
	var b strings.Builder
	for _, p := range t.parts {
		if p.literal != "" {
			b.WriteString(p.literal)
			continue
		}
		b.WriteString(valueForField(p.field, values))
	}
	return b.String()
}

// ViModeLabel renders the prompt label for a vi edit mode. mode is the raw mode
// name ("insert", "normal", or "" for non-vi/emacs). style is "long" (the
// default for {vimode} and {vimode:long}) yielding INSERT/NORMAL, or "short"
// (for {vimode:short}) yielding I/N. In any non-vi mode (mode == ""), or for an
// unknown style, the label is empty so the placeholder renders nothing.
func ViModeLabel(mode, style string) string {
	switch style {
	case "long", "":
		switch mode {
		case "insert":
			return "INSERT"
		case "normal":
			return "NORMAL"
		default:
			return ""
		}
	case "short":
		switch mode {
		case "insert":
			return "I"
		case "normal":
			return "N"
		default:
			return ""
		}
	default:
		return ""
	}
}

// UsesViMode reports whether the compiled template references any of the
// {vimode} placeholder variants, so callers know to wire the live-update
// callback that flips the label on mode transitions.
func (t *Template) UsesViMode() bool {
	if t == nil {
		return false
	}
	for _, p := range t.parts {
		if p.field == fieldViMode || p.field == fieldViModeLong || p.field == fieldViModeShort {
			return true
		}
	}
	return false
}

// Uses reports whether the compiled template contains the named placeholder.
func (t *Template) Uses(name string) bool {
	if t == nil {
		return false
	}
	f, ok := parseField(name)
	if !ok {
		return false
	}
	for _, p := range t.parts {
		if p.field == f {
			return true
		}
	}
	return false
}

func parseField(name string) (field, bool) {
	switch field(name) {
	case fieldAgent, fieldCWD, fieldGitBranch, fieldProvider, fieldModel, fieldModelInfo:
		return field(name), true
	case fieldViMode, fieldViModeLong, fieldViModeShort:
		return field(name), true
	default:
		return "", false
	}
}

func valueForField(f field, values Values) string {
	switch f {
	case fieldAgent:
		return values.Agent
	case fieldCWD:
		return abbreviateHome(values.CWD)
	case fieldGitBranch:
		return values.GitBranch
	case fieldProvider:
		return values.Provider
	case fieldModel:
		return values.Model
	case fieldModelInfo:
		if values.ModelInfo != "" {
			return values.ModelInfo
		}
		return ModelInfo(values.Provider, values.Model)
	case fieldViMode, fieldViModeLong:
		return ViModeLabel(values.ViMode, "long")
	case fieldViModeShort:
		return ViModeLabel(values.ViMode, "short")
	default:
		return ""
	}
}

// ModelInfo renders the compact provider/model prompt value.
func ModelInfo(provider, model string) string {
	switch {
	case provider != "" && model != "":
		return provider + ":" + model
	case model != "":
		return model
	default:
		return ""
	}
}

// abbreviateHome rewrites the user's home directory prefix in path as "~",
// shortening e.g. "/Users/alice/work" to "~/work". Paths outside the home
// directory (including empty paths and the home directory itself when it is
// the root prefix) are returned unchanged; the bare home directory becomes "~".
func abbreviateHome(path string) string {
	if path == "" {
		return ""
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return path
	}
	if path == home {
		return "~"
	}
	if strings.HasPrefix(path, home+"/") {
		return "~" + path[len(home):]
	}
	return path
}

// CurrentGitBranch returns the current git branch for dir. It returns "" when
// dir is not in a work tree, git is unavailable, or the command times out.
func CurrentGitBranch(dir string) string {
	if dir == "" {
		dir = "."
	}
	ctx, cancel := context.WithTimeout(context.Background(), gitTimeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, "git", "-C", dir, "--no-pager", "branch", "--show-current").Output()
	if err != nil {
		return ""
	}
	branch := strings.TrimSpace(string(out))
	if branch == "" {
		return "(detached)"
	}
	return branch
}
