package ui

import (
	"strings"
	"unicode"
)

func promptFileReferences(text string) []string {
	runes := []rune(text)
	var refs []string
	for i := 0; i < len(runes); i++ {
		if runes[i] != '@' || !isPromptRefBoundary(runes, i) {
			continue
		}
		if i+1 < len(runes) && runes[i+1] == '@' {
			continue
		}
		if i+1 < len(runes) && runes[i+1] == '"' {
			ref, end, ok := parseQuotedPromptRef(runes, i+2)
			if ok {
				if ref != "" {
					refs = append(refs, ref)
				}
				i = end - 1
			}
			continue
		}
		start := i + 1
		end := start
		for end < len(runes) && !unicode.IsSpace(runes[end]) {
			end++
		}
		if end > start {
			refs = append(refs, string(runes[start:end]))
			i = end - 1
		}
	}
	return refs
}

func parseQuotedPromptRef(runes []rune, start int) (string, int, bool) {
	var b strings.Builder
	for i := start; i < len(runes); i++ {
		r := runes[i]
		if r == '"' {
			return b.String(), i + 1, true
		}
		if r == '\\' && i+1 < len(runes) {
			next := runes[i+1]
			if next == '"' || next == '\\' {
				b.WriteRune(next)
				i++
				continue
			}
		}
		b.WriteRune(r)
	}
	return "", start, false
}

func unescapePromptRefPath(text string) string {
	runes := []rune(text)
	var b strings.Builder
	for i := 0; i < len(runes); i++ {
		r := runes[i]
		if r == '\\' && i+1 < len(runes) {
			next := runes[i+1]
			if next == '"' || next == '\\' {
				b.WriteRune(next)
				i++
				continue
			}
		}
		b.WriteRune(r)
	}
	return b.String()
}

func escapePromptRefPath(path string) string {
	var b strings.Builder
	for _, r := range path {
		if r == '\\' || r == '"' {
			b.WriteRune('\\')
		}
		b.WriteRune(r)
	}
	return b.String()
}

func needsPromptRefQuotes(path string) bool {
	for _, r := range path {
		if unicode.IsSpace(r) || r == '"' || r == '\\' {
			return true
		}
	}
	return false
}

func isPromptRefBoundary(buf []rune, at int) bool {
	if at < 0 || at >= len(buf) {
		return false
	}
	if at == 0 {
		return true
	}
	return unicode.IsSpace(buf[at-1])
}
