package llm

import (
	"fmt"
	"net/url"
	"strings"
	"unicode"
	"unicode/utf8"
)

// URLCitation attributes a response to a web source, not an exact text span.
type URLCitation struct {
	URL   string
	Title string
}

const (
	citationSourcesHeader          = "\n\nSources:\n"
	unsafeCitationDestinationChars = "()<>\\\"'`{}|"
)

// FormatCitationSources returns a Markdown appendix with safe HTTP(S) links.
// Invalid URLs are omitted. Duplicate URLs retain their first occurrence and
// title, in input order. An empty source list produces no appendix.
func FormatCitationSources(citations []URLCitation) string {
	var out strings.Builder
	seen := make(map[string]bool)
	for _, citation := range citations {
		destination := citationDestination(citation.URL)
		if destination == "" || seen[destination] {
			continue
		}
		seen[destination] = true
		if len(seen) == 1 {
			out.WriteString(citationSourcesHeader)
		}
		fmt.Fprintf(&out, "- [%d](%s)", len(seen), destination)
		if title := citationTitle(citation.Title); title != "" {
			out.WriteString(" — ")
			out.WriteString(title)
		}
		out.WriteByte('\n')
	}
	return out.String()
}

// SplitCitationSources separates only an exact, trailing appendix produced by
// FormatCitationSources. Other text is returned unchanged with empty sources.
// The returned sources include the leading blank lines and final newline.
func SplitCitationSources(text string) (body, sources string) {
	start := strings.LastIndex(text, citationSourcesHeader)
	if start < 0 || !strings.HasSuffix(text, "\n") {
		return text, ""
	}
	suffix := text[start:]
	lines := strings.Split(strings.TrimSuffix(suffix[len(citationSourcesHeader):], "\n"), "\n")
	citations := make([]URLCitation, 0, len(lines))
	for i, line := range lines {
		prefix := fmt.Sprintf("- [%d](", i+1)
		if !strings.HasPrefix(line, prefix) {
			return text, ""
		}
		link := line[len(prefix):]
		end := strings.IndexByte(link, ')')
		if end < 0 {
			return text, ""
		}
		citation := URLCitation{URL: link[:end]}
		if tail := link[end+1:]; tail != "" {
			if !strings.HasPrefix(tail, " — ") {
				return text, ""
			}
			citation.Title = strings.TrimPrefix(tail, " — ")
		}
		citations = append(citations, citation)
	}
	if FormatCitationSources(citations) != suffix {
		return text, ""
	}
	return text[:start], suffix
}

func citationDestination(raw string) string {
	if !utf8.ValidString(raw) {
		return ""
	}
	// Providers may cite URLs with a literal '%' that is not a valid escape (for
	// example "?off=50%"). Escape only those stray signs so the URL parses and
	// stays a stable canonical destination instead of being dropped.
	raw = escapeStrayPercent(raw)
	// Check the decoded spelling as well: encoded terminal controls must not
	// survive in model-facing text, even though they are not literal escapes.
	decoded, err := url.PathUnescape(raw)
	if err != nil || !utf8.ValidString(raw) || !utf8.ValidString(decoded) {
		return ""
	}
	for _, r := range decoded {
		if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) {
			return ""
		}
	}
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" || u.User != nil || u.Opaque != "" {
		return ""
	}
	// net/url does not accept percent-escaped ASCII delimiters in hosts, so
	// omit hosts that would need Markdown escaping rather than emit a broken
	// link that cannot be parsed back as a canonical appendix.
	for _, r := range u.Host {
		if unicode.IsSpace(r) || strings.ContainsRune(unsafeCitationDestinationChars, r) {
			return ""
		}
	}
	// net/url handles path escaping but leaves parentheses and some query
	// characters intact. Escape those too so our limited Markdown renderer
	// cannot end the destination early or interpret injected markup.
	var out strings.Builder
	for _, r := range u.String() {
		if unicode.IsSpace(r) || strings.ContainsRune(unsafeCitationDestinationChars, r) {
			for _, b := range []byte(string(r)) {
				fmt.Fprintf(&out, "%%%02X", b)
			}
		} else {
			out.WriteRune(r)
		}
	}
	return out.String()
}

// escapeStrayPercent rewrites each '%' not followed by two hex digits as "%25".
// Valid escapes are unchanged, so the result is idempotent.
func escapeStrayPercent(raw string) string {
	if !strings.Contains(raw, "%") {
		return raw
	}
	var out strings.Builder
	for i := 0; i < len(raw); i++ {
		if raw[i] == '%' && (i+2 >= len(raw) || !isHexDigit(raw[i+1]) || !isHexDigit(raw[i+2])) {
			out.WriteString("%25")
			continue
		}
		out.WriteByte(raw[i])
	}
	return out.String()
}

func isHexDigit(c byte) bool {
	return '0' <= c && c <= '9' || 'a' <= c && c <= 'f' || 'A' <= c && c <= 'F'
}

func citationTitle(title string) string {
	title = strings.Map(func(r rune) rune {
		switch {
		case unicode.IsSpace(r):
			return ' '
		case unicode.IsControl(r), unicode.Is(unicode.Cf, r), r == utf8.RuneError:
			return -1
		case strings.ContainsRune("\\`*_{}[]()<>#!|~", r):
			return -1
		default:
			return r
		}
	}, title)
	return strings.Join(strings.Fields(title), " ")
}
