package lspproxy

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// This file renders LSP results into compact, LLM-friendly text. Positions are
// shown 1-based (matching editors and grep), paths are the filesystem form of
// the result URI, and a snippet/line reader is injected so formatting stays pure
// and testable.

// formatLocations renders a flat list of locations, one per line, as
// "path:line:col  <snippet>". snippet may return "" to omit the trailing code.
func formatLocations(locs []Location, snippet func(uri string, line int) string) string {
	lines := make([]string, 0, len(locs))
	for _, l := range locs {
		entry := fmt.Sprintf("%s:%d:%d", uriToPath(l.URI), l.Range.Start.Line+1, l.Range.Start.Character+1)
		if s := snippet(l.URI, l.Range.Start.Line); s != "" {
			entry += "  " + s
		}
		lines = append(lines, entry)
	}
	return strings.Join(lines, "\n")
}

// formatReferences renders references as a capped location list with an omitted
// footer. max <= 0 means no cap.
func formatReferences(locs []Location, max int, snippet func(uri string, line int) string) string {
	if len(locs) == 0 {
		return "no references found"
	}
	shown, omitted := locs, 0
	if max > 0 && len(locs) > max {
		shown, omitted = locs[:max], len(locs)-max
	}
	out := formatLocations(shown, snippet)
	if omitted > 0 {
		out += fmt.Sprintf("\n… %d more reference(s) omitted", omitted)
	}
	return out
}

// formatDocumentSymbols renders an indented outline of in-file symbols, all
// sharing path.
func formatDocumentSymbols(syms []Symbol, path string) string {
	var b strings.Builder
	var walk func(items []Symbol, depth int)
	walk = func(items []Symbol, depth int) {
		for _, s := range items {
			fmt.Fprintf(&b, "%s%s %s  %s:%d\n", strings.Repeat("  ", depth), symbolKindName(s.Kind), s.Name, path, s.Line+1)
			walk(s.Children, depth+1)
		}
	}
	walk(syms, 0)
	return strings.TrimRight(b.String(), "\n")
}

// formatWorkspaceSymbols renders flat workspace symbols, each at its own file.
func formatWorkspaceSymbols(syms []Symbol) string {
	lines := make([]string, 0, len(syms))
	for _, s := range syms {
		lines = append(lines, fmt.Sprintf("%s %s  %s:%d", symbolKindName(s.Kind), s.Name, uriToPath(s.URI), s.Line+1))
	}
	return strings.Join(lines, "\n")
}

// formatDiagnostics renders diagnostics sorted by line as
// "severity path:line:col  message [source code]".
func formatDiagnostics(diags []Diagnostic, path string) string {
	if len(diags) == 0 {
		return "no diagnostics"
	}
	sorted := make([]Diagnostic, len(diags))
	copy(sorted, diags)
	sort.SliceStable(sorted, func(i, j int) bool {
		if sorted[i].Range.Start.Line != sorted[j].Range.Start.Line {
			return sorted[i].Range.Start.Line < sorted[j].Range.Start.Line
		}
		return sorted[i].Range.Start.Character < sorted[j].Range.Start.Character
	})
	lines := make([]string, 0, len(sorted))
	for _, d := range sorted {
		entry := fmt.Sprintf("%s %s:%d:%d  %s", severityName(d.Severity), path, d.Range.Start.Line+1, d.Range.Start.Character+1, d.Message)
		if tag := sourceCodeTag(d.Source, d.Code); tag != "" {
			entry += " " + tag
		}
		lines = append(lines, entry)
	}
	return strings.Join(lines, "\n")
}

// formatRenamePlan renders the cross-file rename edits as a per-file before/after
// view plus an explicit apply instruction. The shim never writes files; this is
// the apply-ready plan for the agent's own edit tools. lineFor reads the
// original line so a same-line edit can be shown as a diff.
func formatRenamePlan(edits []FileEdits, lineFor func(uri string, line int) (string, bool)) string {
	if len(edits) == 0 {
		return "no rename edits (the symbol may not be renameable at this position)"
	}
	var b strings.Builder
	total := 0
	for _, fe := range edits {
		fmt.Fprintf(&b, "%s\n", uriToPath(fe.URI))
		for _, e := range fe.Edits {
			total++
			ln := e.Range.Start.Line
			if e.Range.Start.Line == e.Range.End.Line {
				if orig, ok := lineFor(fe.URI, ln); ok {
					fmt.Fprintf(&b, "  L%d  - %s\n        + %s\n", ln+1, orig, applyEditToLine(orig, e))
					continue
				}
			}
			fmt.Fprintf(&b, "  L%d:%d-L%d:%d  → %q\n", e.Range.Start.Line+1, e.Range.Start.Character+1, e.Range.End.Line+1, e.Range.End.Character+1, e.NewText)
		}
	}
	fmt.Fprintf(&b, "\n%d edit(s) across %d file(s). This did NOT modify any files — apply these edits with your file-editing tools to complete the rename.", total, len(edits))
	return b.String()
}

// applyEditToLine returns line with the edit's UTF-16 range replaced by NewText.
func applyEditToLine(line string, e TextEdit) string {
	start := utf16ColToByteOffset(line, e.Range.Start.Character)
	end := utf16ColToByteOffset(line, e.Range.End.Character)
	if start > end {
		start, end = end, start
	}
	return line[:start] + e.NewText + line[end:]
}

// sourceCodeTag renders the "[source code]" suffix for a diagnostic, omitting
// empty parts and the brackets entirely when both are absent.
func sourceCodeTag(source string, code json.RawMessage) string {
	parts := make([]string, 0, 2)
	if source != "" {
		parts = append(parts, source)
	}
	if c := codeString(code); c != "" {
		parts = append(parts, c)
	}
	if len(parts) == 0 {
		return ""
	}
	return "[" + strings.Join(parts, " ") + "]"
}

// codeString renders a diagnostic Code (string or number) as plain text.
func codeString(code json.RawMessage) string {
	code = json.RawMessage(strings.TrimSpace(string(code)))
	if len(code) == 0 || string(code) == "null" {
		return ""
	}
	if code[0] == '"' {
		var s string
		if json.Unmarshal(code, &s) == nil {
			return s
		}
	}
	return string(code)
}

// severityName maps an LSP DiagnosticSeverity to a label.
func severityName(sev int) string {
	switch sev {
	case 1:
		return "error"
	case 2:
		return "warning"
	case 3:
		return "info"
	case 4:
		return "hint"
	default:
		return "diagnostic"
	}
}

// symbolKindName maps an LSP SymbolKind to a label, defaulting to "symbol".
func symbolKindName(kind int) string {
	switch kind {
	case 1:
		return "file"
	case 2:
		return "module"
	case 3:
		return "namespace"
	case 4:
		return "package"
	case 5:
		return "class"
	case 6:
		return "method"
	case 7:
		return "property"
	case 8:
		return "field"
	case 9:
		return "constructor"
	case 10:
		return "enum"
	case 11:
		return "interface"
	case 12:
		return "function"
	case 13:
		return "variable"
	case 14:
		return "constant"
	case 15:
		return "string"
	case 16:
		return "number"
	case 17:
		return "boolean"
	case 18:
		return "array"
	case 19:
		return "object"
	case 20:
		return "key"
	case 21:
		return "null"
	case 22:
		return "enum-member"
	case 23:
		return "struct"
	case 24:
		return "event"
	case 25:
		return "operator"
	case 26:
		return "type-parameter"
	default:
		return "symbol"
	}
}
