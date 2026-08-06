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

func formatSignatureHelp(help SignatureHelp) string {
	lines := make([]string, 0, len(help.Signatures))
	for i, sig := range help.Signatures {
		active := help.ActiveParameter
		if sig.ActiveParameter != nil {
			active = *sig.ActiveParameter
		}
		prefix := "  "
		if i == help.ActiveSignature {
			prefix = "* "
		}
		line := fmt.Sprintf("%s%s", prefix, sig.Label)
		if active >= 0 && active < len(sig.Parameters) {
			if label := parameterLabel(sig.Parameters[active].Label); label != "" {
				line += "  [active: " + label + "]"
			}
		}
		if docs := strings.TrimSpace(markupToText(sig.Documentation)); docs != "" {
			line += "\n    " + strings.ReplaceAll(docs, "\n", "\n    ")
		}
		lines = append(lines, line)
	}
	return strings.Join(lines, "\n")
}

func parameterLabel(raw json.RawMessage) string {
	var label string
	if json.Unmarshal(raw, &label) == nil {
		return label
	}
	var offsets []int
	if json.Unmarshal(raw, &offsets) == nil && len(offsets) == 2 {
		return fmt.Sprintf("offsets %d..%d", offsets[0], offsets[1])
	}
	return ""
}

func formatCompletions(items []CompletionItem, max int) string {
	shown, omitted := items, 0
	if max > 0 && len(shown) > max {
		shown, omitted = shown[:max], len(shown)-max
	}
	lines := make([]string, 0, len(shown)+1)
	for _, item := range shown {
		line := item.Label
		if kind := completionKindName(item.Kind); kind != "" {
			line += "  [" + kind + "]"
		}
		if item.Detail != "" {
			line += "  " + strings.TrimSpace(item.Detail)
		}
		if insert := item.InsertText; insert != "" && insert != item.Label {
			line += "  insert=" + fmt.Sprintf("%q", insert)
		}
		lines = append(lines, line)
	}
	if omitted > 0 {
		lines = append(lines, fmt.Sprintf("… %d more completion(s) omitted", omitted))
	}
	return strings.Join(lines, "\n")
}

func formatDocumentHighlights(highlights []DocumentHighlight, path string, lr *lineReader) string {
	lines := make([]string, 0, len(highlights))
	for _, h := range highlights {
		kind := "text"
		switch h.Kind {
		case 2:
			kind = "read"
		case 3:
			kind = "write"
		}
		line := fmt.Sprintf("%s %s:%d:%d", kind, path, h.Range.Start.Line+1, h.Range.Start.Character+1)
		if snippet, ok := lr.line(path, h.Range.Start.Line); ok {
			line += "  " + strings.TrimSpace(snippet)
		}
		lines = append(lines, line)
	}
	return strings.Join(lines, "\n")
}

func formatIncomingCalls(calls []CallHierarchyIncomingCall, max int) string {
	if len(calls) == 0 {
		return "no incoming calls"
	}
	shown, omitted := capCount(calls, max)
	lines := make([]string, 0, len(shown)+1)
	for _, call := range shown {
		lines = append(lines, formatHierarchyItem(call.From))
	}
	if omitted > 0 {
		lines = append(lines, fmt.Sprintf("… %d more call(s) omitted", omitted))
	}
	return strings.Join(lines, "\n")
}

func formatOutgoingCalls(calls []CallHierarchyOutgoingCall, max int) string {
	if len(calls) == 0 {
		return "no outgoing calls"
	}
	shown, omitted := capCount(calls, max)
	lines := make([]string, 0, len(shown)+1)
	for _, call := range shown {
		lines = append(lines, formatHierarchyItem(call.To))
	}
	if omitted > 0 {
		lines = append(lines, fmt.Sprintf("… %d more call(s) omitted", omitted))
	}
	return strings.Join(lines, "\n")
}

func formatHierarchyItem(item CallHierarchyItem) string {
	line := fmt.Sprintf("%s %s  %s:%d", symbolKindName(item.Kind), item.Name, uriToPath(item.URI), item.SelectionRange.Start.Line+1)
	if item.Detail != "" {
		line += "  " + strings.TrimSpace(item.Detail)
	}
	return line
}

func formatTypeHierarchy(items []TypeHierarchyItem, direction string, max int) string {
	if len(items) == 0 {
		return "no " + direction
	}
	shown, omitted := capCount(items, max)
	lines := make([]string, 0, len(shown)+1)
	for _, item := range shown {
		line := fmt.Sprintf("%s %s  %s:%d", symbolKindName(item.Kind), item.Name, uriToPath(item.URI), item.SelectionRange.Start.Line+1)
		if item.Detail != "" {
			line += "  " + strings.TrimSpace(item.Detail)
		}
		lines = append(lines, line)
	}
	if omitted > 0 {
		lines = append(lines, fmt.Sprintf("… %d more item(s) omitted", omitted))
	}
	return strings.Join(lines, "\n")
}

func formatInlayHints(hints []InlayHint, path string, max int) string {
	if len(hints) == 0 {
		return "no inlay hints"
	}
	shown, omitted := capCount(hints, max)
	lines := make([]string, 0, len(shown)+1)
	for _, hint := range shown {
		label := inlayHintLabel(hint.Label)
		kind := "hint"
		if hint.Kind == 1 {
			kind = "type"
		} else if hint.Kind == 2 {
			kind = "parameter"
		}
		lines = append(lines, fmt.Sprintf("%s %s:%d:%d  %s", kind, path, hint.Position.Line+1, hint.Position.Character+1, label))
	}
	if omitted > 0 {
		lines = append(lines, fmt.Sprintf("… %d more hint(s) omitted", omitted))
	}
	return strings.Join(lines, "\n")
}

func inlayHintLabel(raw json.RawMessage) string {
	var label string
	if json.Unmarshal(raw, &label) == nil {
		return label
	}
	var parts []struct {
		Value string `json:"value"`
	}
	if json.Unmarshal(raw, &parts) == nil {
		var b strings.Builder
		for _, part := range parts {
			b.WriteString(part.Value)
		}
		return b.String()
	}
	return string(raw)
}

func formatCodeActions(actions []CodeAction) string {
	if len(actions) == 0 {
		return "no code actions"
	}
	lines := make([]string, 0, len(actions))
	for _, action := range actions {
		line := action.Title
		if action.Kind != "" {
			line += "  [" + action.Kind + "]"
		}
		switch {
		case action.Disabled != nil:
			line += "  disabled: " + action.Disabled.Reason
		case action.CommandOnly || (len(action.Command) > 0 && string(action.Command) != "null"):
			line += "  includes server command (not executable by harness)"
		case len(action.Edit) > 0 && string(action.Edit) != "null":
			if edits, err := parseWorkspaceEdit(action.Edit); err == nil {
				files, total := editCounts(edits)
				line += fmt.Sprintf("  %d edit(s), %d file(s)", total, files)
			}
		case len(action.Data) > 0:
			line += "  resolvable"
		}
		lines = append(lines, line)
	}
	return strings.Join(lines, "\n")
}

func editCounts(edits []FileEdits) (files, total int) {
	for _, file := range edits {
		if len(file.Edits) > 0 {
			files++
			total += len(file.Edits)
		}
	}
	return files, total
}

func capCount[T any](items []T, max int) ([]T, int) {
	if max > 0 && len(items) > max {
		return items[:max], len(items) - max
	}
	return items, 0
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
	return formatEditPlan(edits, lineFor, "rename")
}

func formatEditPlan(edits []FileEdits, lineFor func(uri string, line int) (string, bool), label string) string {
	if len(edits) == 0 {
		return "no " + label + " edits"
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
	fmt.Fprintf(&b, "\n%d edit(s) across %d file(s). This did NOT modify any files.", total, len(edits))
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

func completionKindName(kind int) string {
	switch kind {
	case 1:
		return "text"
	case 2:
		return "method"
	case 3:
		return "function"
	case 4:
		return "constructor"
	case 5:
		return "field"
	case 6:
		return "variable"
	case 7:
		return "class"
	case 8:
		return "interface"
	case 9:
		return "module"
	case 10:
		return "property"
	case 13:
		return "enum"
	case 14:
		return "keyword"
	case 15:
		return "snippet"
	case 21:
		return "constant"
	case 22:
		return "struct"
	case 25:
		return "type-parameter"
	default:
		return ""
	}
}
