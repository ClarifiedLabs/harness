package lspproxy

import (
	"bytes"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// parseLocations normalizes the polymorphic result of textDocument/definition
// and friends (Location | Location[] | LocationLink[] | null) into a flat
// []Location. linkSupport means a server may answer with LocationLink, whose
// target fields are mapped onto Location.
func parseLocations(raw json.RawMessage) ([]Location, error) {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || string(raw) == "null" {
		return nil, nil
	}
	if raw[0] == '[' {
		var locs []Location
		if err := json.Unmarshal(raw, &locs); err == nil && allHaveURI(locs) {
			return locs, nil
		}
		var links []LocationLink
		if err := json.Unmarshal(raw, &links); err != nil {
			return nil, fmt.Errorf("lspproxy: decode location array: %w", err)
		}
		return linksToLocations(links), nil
	}
	var loc Location
	if err := json.Unmarshal(raw, &loc); err == nil && loc.URI != "" {
		return []Location{loc}, nil
	}
	var link LocationLink
	if err := json.Unmarshal(raw, &link); err == nil && link.TargetURI != "" {
		return linksToLocations([]LocationLink{link}), nil
	}
	return nil, nil
}

// allHaveURI reports whether locs is non-empty and every element has a URI,
// distinguishing a decoded Location[] from a LocationLink[] (whose elements
// decode into Location with an empty URI).
func allHaveURI(locs []Location) bool {
	if len(locs) == 0 {
		return false
	}
	for _, l := range locs {
		if l.URI == "" {
			return false
		}
	}
	return true
}

// linksToLocations maps LocationLink targets onto Location.
func linksToLocations(links []LocationLink) []Location {
	out := make([]Location, 0, len(links))
	for _, l := range links {
		out = append(out, Location{URI: l.TargetURI, Range: l.TargetRange})
	}
	return out
}

// parseHoverContents normalizes a Hover result to plaintext, flattening the
// MarkedString | MarkedString[] | MarkupContent shapes its "contents" field can
// take.
func parseHoverContents(raw json.RawMessage) (string, error) {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || string(raw) == "null" {
		return "", nil
	}
	var h struct {
		Contents json.RawMessage `json:"contents"`
	}
	if err := json.Unmarshal(raw, &h); err != nil {
		return "", fmt.Errorf("lspproxy: decode hover: %w", err)
	}
	return markupToText(h.Contents), nil
}

// parseCompletionItems normalizes CompletionItem[] | CompletionList | null.
func parseCompletionItems(raw json.RawMessage) ([]CompletionItem, error) {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || string(raw) == "null" {
		return nil, nil
	}
	var items []CompletionItem
	if raw[0] == '[' {
		if err := json.Unmarshal(raw, &items); err != nil {
			return nil, fmt.Errorf("lspproxy: decode completion items: %w", err)
		}
		return items, nil
	}
	var list struct {
		Items []CompletionItem `json:"items"`
	}
	if err := json.Unmarshal(raw, &list); err != nil {
		return nil, fmt.Errorf("lspproxy: decode completion list: %w", err)
	}
	return list.Items, nil
}

// parseCodeActions normalizes (Command | CodeAction)[] | null. Direct Command
// entries have a string "command" field; CodeAction.command is an object.
func parseCodeActions(raw json.RawMessage) ([]CodeAction, error) {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || string(raw) == "null" {
		return nil, nil
	}
	var entries []json.RawMessage
	if err := json.Unmarshal(raw, &entries); err != nil {
		return nil, fmt.Errorf("lspproxy: decode code actions: %w", err)
	}
	out := make([]CodeAction, 0, len(entries))
	for _, entry := range entries {
		var action CodeAction
		if err := json.Unmarshal(entry, &action); err != nil {
			return nil, fmt.Errorf("lspproxy: decode code action: %w", err)
		}
		var probe struct {
			Command json.RawMessage `json:"command"`
		}
		_ = json.Unmarshal(entry, &probe)
		command := bytes.TrimSpace(probe.Command)
		if len(command) > 0 && command[0] == '"' {
			action.CommandOnly = true
		}
		out = append(out, action)
	}
	return out, nil
}

func parseTextEdits(raw json.RawMessage) ([]TextEdit, error) {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || string(raw) == "null" {
		return nil, nil
	}
	var edits []TextEdit
	if err := json.Unmarshal(raw, &edits); err != nil {
		return nil, fmt.Errorf("lspproxy: decode text edits: %w", err)
	}
	return edits, nil
}

// markupToText flattens a string, a {language|kind, value} object, or an array
// of either into plaintext. Both MarkedString objects and MarkupContent carry
// the text under "value".
func markupToText(raw json.RawMessage) string {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || string(raw) == "null" {
		return ""
	}
	switch raw[0] {
	case '"':
		var s string
		_ = json.Unmarshal(raw, &s)
		return s
	case '[':
		var arr []json.RawMessage
		_ = json.Unmarshal(raw, &arr)
		parts := make([]string, 0, len(arr))
		for _, e := range arr {
			if s := markupToText(e); s != "" {
				parts = append(parts, s)
			}
		}
		return strings.Join(parts, "\n")
	case '{':
		var m struct {
			Value string `json:"value"`
		}
		_ = json.Unmarshal(raw, &m)
		return m.Value
	default:
		return ""
	}
}

// parseSymbols normalizes a documentSymbol/workspaceSymbol result into []Symbol,
// detecting the flat (SymbolInformation, has "location") vs hierarchical
// (DocumentSymbol, has "selectionRange") shape from the first element.
func parseSymbols(raw json.RawMessage) ([]Symbol, error) {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || string(raw) == "null" {
		return nil, nil
	}
	var probe []map[string]json.RawMessage
	if err := json.Unmarshal(raw, &probe); err != nil {
		return nil, fmt.Errorf("lspproxy: decode symbols: %w", err)
	}
	if len(probe) == 0 {
		return nil, nil
	}
	if _, flat := probe[0]["location"]; flat {
		var sis []SymbolInformation
		if err := json.Unmarshal(raw, &sis); err != nil {
			return nil, fmt.Errorf("lspproxy: decode symbol information: %w", err)
		}
		out := make([]Symbol, 0, len(sis))
		for _, si := range sis {
			out = append(out, Symbol{
				Name: si.Name,
				Kind: si.Kind,
				Line: si.Location.Range.Start.Line,
				URI:  si.Location.URI,
			})
		}
		return out, nil
	}
	var ds []DocumentSymbol
	if err := json.Unmarshal(raw, &ds); err != nil {
		return nil, fmt.Errorf("lspproxy: decode document symbols: %w", err)
	}
	return docSymbolsToSymbols(ds), nil
}

// docSymbolsToSymbols flattens the hierarchical shape into the normalized tree,
// using the selection range's line as the symbol's line.
func docSymbolsToSymbols(ds []DocumentSymbol) []Symbol {
	out := make([]Symbol, 0, len(ds))
	for _, d := range ds {
		out = append(out, Symbol{
			Name:     d.Name,
			Detail:   d.Detail,
			Kind:     d.Kind,
			Line:     d.SelectionRange.Start.Line,
			Children: docSymbolsToSymbols(d.Children),
		})
	}
	return out
}

func parseWorkspaceEditStrict(raw json.RawMessage) ([]FileEdits, error) {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || string(raw) == "null" {
		return nil, nil
	}
	var probe struct {
		DocumentChanges []map[string]json.RawMessage `json:"documentChanges"`
	}
	if err := json.Unmarshal(raw, &probe); err != nil {
		return nil, fmt.Errorf("lspproxy: decode workspace edit: %w", err)
	}
	for _, change := range probe.DocumentChanges {
		if _, ok := change["textDocument"]; !ok {
			return nil, fmt.Errorf("workspace edit contains unsupported file operation")
		}
	}
	return parseWorkspaceEdit(raw)
}

// parseWorkspaceEdit normalizes a rename's WorkspaceEdit into a per-file edit
// list sorted by URI. It prefers documentChanges (the versioned form) and falls
// back to changes; file-operation entries without text edits are skipped.
func parseWorkspaceEdit(raw json.RawMessage) ([]FileEdits, error) {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || string(raw) == "null" {
		return nil, nil
	}
	var we WorkspaceEdit
	if err := json.Unmarshal(raw, &we); err != nil {
		return nil, fmt.Errorf("lspproxy: decode workspace edit: %w", err)
	}
	byURI := map[string][]TextEdit{}
	if len(we.DocumentChanges) > 0 {
		for _, dc := range we.DocumentChanges {
			if dc.TextDocument.URI == "" {
				continue // a file-operation entry (create/rename/delete); skip
			}
			byURI[dc.TextDocument.URI] = append(byURI[dc.TextDocument.URI], dc.Edits...)
		}
	} else {
		for uri, edits := range we.Changes {
			byURI[uri] = append(byURI[uri], edits...)
		}
	}

	uris := make([]string, 0, len(byURI))
	for uri := range byURI {
		uris = append(uris, uri)
	}
	sort.Strings(uris)
	out := make([]FileEdits, 0, len(uris))
	for _, uri := range uris {
		out = append(out, FileEdits{URI: uri, Edits: byURI[uri]})
	}
	return out, nil
}
