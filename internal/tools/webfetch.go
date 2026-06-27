package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"html"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"harness/internal/httpx"
)

const (
	webFetchDefaultMaxBytes = 1 << 20 // 1 MB
	webFetchMaxBytes        = 5 << 20 // 5 MB
	webFetchDefaultTimeout  = 30
	webFetchMaxRedirects    = 5
)

var webFetchTimeoutUnit = time.Second

const webFetchBackgroundSchema = `{
  "type": "object",
  "properties": {
    "url": {"type": "string", "description": "Absolute http or https URL to fetch."},
    "max_bytes": {"type": "integer", "description": "Maximum response bytes to read (default 1MB, cap 5MB)."},
    "timeout_seconds": {"type": "integer", "description": "Maximum time to wait for the fetch, in seconds (default 30; no maximum)."},
    "background": {"type": "boolean", "description": "When true, start the fetch as a process-local background job and return a job id immediately. Use background_jobs to inspect or cancel it."}
  },
  "required": ["url"]
}`

const webFetchSchema = `{
  "type": "object",
  "properties": {
    "url": {"type": "string", "description": "Absolute http or https URL to fetch."},
    "max_bytes": {"type": "integer", "description": "Maximum response bytes to read (default 1MB, cap 5MB)."},
    "timeout_seconds": {"type": "integer", "description": "Maximum time to wait for the fetch, in seconds (default 30; no maximum)."}
  },
  "required": ["url"]
}`

type webFetch struct {
	background BackgroundJobStarter
}

func (webFetch) Name() string { return "web_fetch" }

func (webFetch) Description() string {
	return "Fetch a URL (http/https) and return its text content. Provide a JSON object with url and optional limits. HTML is reduced to readable text. Returns a background job id immediately when background is true."
}

func (t webFetch) Schema() json.RawMessage {
	if t.background != nil {
		return json.RawMessage(webFetchBackgroundSchema)
	}
	return json.RawMessage(webFetchSchema)
}

// web_fetch issues a GET and mutates no workspace state.
func (webFetch) ReadOnly(json.RawMessage) bool { return true }

type webFetchArgs struct {
	URL            string
	MaxBytes       int
	TimeoutSeconds int
	Background     bool
}

func (t webFetch) Run(ctx context.Context, input json.RawMessage) (string, error) {
	args, err := decodeWebFetchArgs(input)
	if err != nil {
		return "", err
	}
	if args.URL == "" {
		return "", badArgs("url is required")
	}
	if args.MaxBytes < 0 {
		return "", badArgs("max_bytes must be >= 0")
	}
	if args.TimeoutSeconds < 0 {
		return "", badArgs("timeout_seconds must be >= 0")
	}
	if err := validateHTTPURL(args.URL); err != nil {
		return "", err
	}

	if args.Background {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		if t.background == nil {
			return "", fmt.Errorf("background manager is not initialized")
		}
		url := args.URL
		maxBytes := args.MaxBytes
		timeoutSeconds := args.TimeoutSeconds
		info, err := t.background.StartBackgroundJob(BackgroundJobRequest{
			Kind:        "web_fetch",
			Description: url,
			Run: func(ctx context.Context, id string) (BackgroundJobResult, error) {
				out, err := doWebFetch(ctx, url, maxBytes, timeoutSeconds)
				return BackgroundJobResult{Text: out}, err
			},
		})
		if err != nil {
			return "", err
		}
		return fmt.Sprintf("background job %s started", info.ID), nil
	}

	return doWebFetch(ctx, args.URL, args.MaxBytes, args.TimeoutSeconds)
}

func decodeWebFetchArgs(input json.RawMessage) (webFetchArgs, error) {
	var raw struct {
		URL            string `json:"url"`
		MaxBytes       int    `json:"max_bytes"`
		TimeoutSeconds *int   `json:"timeout_seconds"`
		TimeoutAlias   *int   `json:"timeout"`
		Background     bool   `json:"background"`
	}
	if err := json.Unmarshal(input, &raw); err != nil {
		return webFetchArgs{}, err
	}
	timeoutSeconds := 0
	if raw.TimeoutSeconds != nil {
		timeoutSeconds = *raw.TimeoutSeconds
	}
	if raw.TimeoutAlias != nil {
		if raw.TimeoutSeconds != nil && *raw.TimeoutAlias != *raw.TimeoutSeconds {
			return webFetchArgs{}, badArgs("timeout and timeout_seconds must match when both are set")
		}
		timeoutSeconds = *raw.TimeoutAlias
	}
	return webFetchArgs{
		URL:            raw.URL,
		MaxBytes:       raw.MaxBytes,
		TimeoutSeconds: timeoutSeconds,
		Background:     raw.Background,
	}, nil
}

// doWebFetch performs the actual HTTP fetch. It is extracted so both the
// foreground and background paths share one implementation.
func doWebFetch(ctx context.Context, rawURL string, maxBytes int, timeoutSeconds int) (string, error) {
	if maxBytes == 0 {
		maxBytes = webFetchDefaultMaxBytes
	}
	if maxBytes > webFetchMaxBytes {
		maxBytes = webFetchMaxBytes
	}

	client := &http.Client{
		Timeout: time.Duration(resolveWebFetchTimeoutSeconds(timeoutSeconds)) * webFetchTimeoutUnit,
		// Re-validate every hop as http/https; cap redirect depth.
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= webFetchMaxRedirects {
				return fmt.Errorf("stopped after %d redirects", webFetchMaxRedirects)
			}
			return validateHTTPURL(req.URL.String())
		},
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return "", err
	}
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	contentType := resp.Header.Get("Content-Type")
	mediaType := httpx.MediaType(contentType)
	if !isTextual(mediaType) {
		return "", fmt.Errorf("unsupported content type %q (binary content is not fetched as text)", contentType)
	}

	// Read one extra byte so the cap can be reported without a Content-Length.
	raw, err := io.ReadAll(io.LimitReader(resp.Body, int64(maxBytes)))
	if err != nil {
		return "", err
	}

	body := string(raw)
	if mediaType == "text/html" {
		body = reduceHTML(body)
	}

	header := fmt.Sprintf("# %s (%s, %s)", resp.Request.URL.String(), resp.Status, contentType)
	return header + "\n" + body, nil
}

func resolveWebFetchTimeoutSeconds(timeoutSeconds int) int {
	if timeoutSeconds == 0 {
		return webFetchDefaultTimeout
	}
	return timeoutSeconds
}

// validateHTTPURL rejects anything that is not an absolute http/https URL.
// Fetching arbitrary http/https URLs is web_fetch's documented purpose
// (design §2, §9.10); there is no private-IP/SSRF blocking by design.
func validateHTTPURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("invalid url: %v", err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("unsupported url scheme %q; only http and https are allowed", u.Scheme)
	}
	if u.Host == "" {
		return fmt.Errorf("url %q has no host", raw)
	}
	return nil
}

// reduceHTML turns an HTML document into readable-ish text (design §9.10): it
// drops <script>/<style> element contents, renders <a> as "text (url)" so
// citation links survive, inserts newlines at block boundaries so headings,
// paragraphs, list items and table rows stay separated, strips all remaining
// tags, unescapes HTML entities, and collapses whitespace per line. It is a
// heuristic reducer for docs and articles, not a renderer; the preserved line
// structure also lets the central line-cap truncate cleanly instead of chopping
// one giant collapsed paragraph mid-word.
func reduceHTML(s string) string {
	s = stripElement(s, "script")
	s = stripElement(s, "style")
	s = renderAnchors(s)
	s = stripTagsWithBreaks(s)
	s = html.UnescapeString(s)
	return collapseLines(s)
}

// blockBreakCloseTags lists tags whose closing form marks a block/line boundary
// at which reduceHTML inserts a newline (<br> is handled separately as a void
// element).
var blockBreakCloseTags = map[string]bool{
	"p": true, "div": true, "li": true, "tr": true,
	"h1": true, "h2": true, "h3": true, "h4": true, "h5": true, "h6": true,
}

// renderAnchors rewrites each <a ...href="URL"...>text</a> as "text (URL)" so
// the model keeps the links it needs to follow citations. The inner text has its
// own tags stripped; entities are left for the final unescape pass. Anchors with
// no href collapse to their text. It scans a single lowercased copy for tag
// boundaries while emitting from the original (case-preserving) string.
func renderAnchors(s string) string {
	lower := strings.ToLower(s)
	var b strings.Builder
	pos := 0
	for {
		rel := indexAnchorOpen(lower[pos:])
		if rel < 0 {
			b.WriteString(s[pos:])
			break
		}
		i := pos + rel
		b.WriteString(s[pos:i])

		gtRel := strings.IndexByte(s[i:], '>')
		if gtRel < 0 {
			b.WriteString(s[i:]) // malformed opening tag; leave for stripTags
			break
		}
		openEnd := i + gtRel + 1
		href := extractHref(s[i:openEnd])

		var inner string
		if closeRel := strings.Index(lower[openEnd:], "</a>"); closeRel < 0 {
			inner = s[openEnd:]
			pos = len(s)
		} else {
			inner = s[openEnd : openEnd+closeRel]
			pos = openEnd + closeRel + len("</a>")
		}
		inner = strings.TrimSpace(stripTags(inner))

		switch {
		case href != "" && inner != "":
			b.WriteString(inner)
			b.WriteString(" (")
			b.WriteString(href)
			b.WriteByte(')')
		case href != "":
			b.WriteByte('(')
			b.WriteString(href)
			b.WriteByte(')')
		default:
			b.WriteString(inner)
		}
		if pos >= len(s) {
			break
		}
	}
	return b.String()
}

// indexAnchorOpen returns the index of the next anchor opening tag ("<a" with a
// following delimiter), or -1. The delimiter check avoids matching <article>,
// <aside>, and similar.
func indexAnchorOpen(lower string) int {
	from := 0
	for {
		i := strings.Index(lower[from:], "<a")
		if i < 0 {
			return -1
		}
		idx := from + i
		next := idx + 2
		if next < len(lower) {
			switch lower[next] {
			case ' ', '\t', '\n', '\r', '>', '/':
				return idx
			}
		}
		from = idx + 2
	}
}

// extractHref pulls the href attribute value out of an anchor opening tag,
// handling double-quoted, single-quoted, and unquoted forms.
func extractHref(tag string) string {
	lower := strings.ToLower(tag)
	i := strings.Index(lower, "href")
	if i < 0 {
		return ""
	}
	j := i + len("href")
	for j < len(tag) && (tag[j] == ' ' || tag[j] == '\t') {
		j++
	}
	if j >= len(tag) || tag[j] != '=' {
		return ""
	}
	j++
	for j < len(tag) && (tag[j] == ' ' || tag[j] == '\t') {
		j++
	}
	if j >= len(tag) {
		return ""
	}
	if q := tag[j]; q == '"' || q == '\'' {
		j++
		if end := strings.IndexByte(tag[j:], q); end >= 0 {
			return strings.TrimSpace(tag[j : j+end])
		}
		return strings.TrimSpace(tag[j:])
	}
	end := j
	for end < len(tag) {
		switch tag[end] {
		case ' ', '\t', '\n', '\r', '>':
			return strings.TrimSpace(tag[j:end])
		}
		end++
	}
	return strings.TrimSpace(tag[j:end])
}

// stripTagsWithBreaks removes every <...> tag like stripTags, but emits a
// newline in place of a block-boundary tag so document structure survives.
func stripTagsWithBreaks(s string) string {
	var b strings.Builder
	for {
		lt := strings.IndexByte(s, '<')
		if lt < 0 {
			b.WriteString(s)
			break
		}
		b.WriteString(s[:lt])
		gt := strings.IndexByte(s[lt:], '>')
		if gt < 0 {
			break // unterminated '<': drop remainder, matching stripTags
		}
		if isBlockBreakTag(s[lt : lt+gt+1]) {
			b.WriteByte('\n')
		}
		s = s[lt+gt+1:]
	}
	return b.String()
}

// isBlockBreakTag reports whether a "<...>" tag is a line/block boundary: any
// <br> (void) or a closing tag for a block element in blockBreakCloseTags.
func isBlockBreakTag(tag string) bool {
	if len(tag) < 3 {
		return false
	}
	name, closing := tagNameOf(tag[1 : len(tag)-1])
	if name == "br" {
		return true
	}
	return closing && blockBreakCloseTags[name]
}

// tagNameOf extracts the lowercased element name from a tag's interior (the text
// between '<' and '>') and reports whether it is a closing tag.
func tagNameOf(inner string) (name string, closing bool) {
	inner = strings.TrimSpace(inner)
	if strings.HasPrefix(inner, "/") {
		closing = true
		inner = strings.TrimSpace(inner[1:])
	}
	end := 0
	for end < len(inner) {
		switch inner[end] {
		case ' ', '\t', '\n', '\r', '/':
			return strings.ToLower(inner[:end]), closing
		}
		end++
	}
	return strings.ToLower(inner[:end]), closing
}

// collapseLines collapses whitespace within each line and drops lines that
// become empty, preserving the newline structure reduceHTML inserted instead of
// flattening the whole document to one line.
func collapseLines(s string) string {
	lines := strings.Split(s, "\n")
	out := make([]string, 0, len(lines))
	for _, ln := range lines {
		if collapsed := strings.Join(strings.Fields(ln), " "); collapsed != "" {
			out = append(out, collapsed)
		}
	}
	return strings.Join(out, "\n")
}

// stripElement removes every <name ...>...</name> block (contents included),
// case-insensitively. An unterminated opening tag drops the rest of the input.
func stripElement(s, name string) string {
	openTag := "<" + name
	closeTag := "</" + name
	lower := strings.ToLower(s)
	var b strings.Builder
	for {
		start := strings.Index(lower, openTag)
		if start < 0 {
			b.WriteString(s)
			break
		}
		b.WriteString(s[:start])
		endClose := strings.Index(lower[start:], closeTag)
		if endClose < 0 {
			break // unterminated: discard remainder
		}
		// Advance past the closing tag's '>'.
		rest := lower[start+endClose:]
		gt := strings.IndexByte(rest, '>')
		if gt < 0 {
			break
		}
		cut := start + endClose + gt + 1
		s = s[cut:]
		lower = lower[cut:]
	}
	return b.String()
}

// stripTags removes everything from '<' to the matching '>'. Text outside tags
// is preserved; a '<' with no '>' drops the remainder.
func stripTags(s string) string {
	var b strings.Builder
	for {
		lt := strings.IndexByte(s, '<')
		if lt < 0 {
			b.WriteString(s)
			break
		}
		b.WriteString(s[:lt])
		gt := strings.IndexByte(s[lt:], '>')
		if gt < 0 {
			break
		}
		s = s[lt+gt+1:]
	}
	return b.String()
}

// isTextual reports whether a media type carries text the model can read:
// any text/*, application/json, application/xml, or +json/+xml suffixes. An
// absent type is treated as text (servers often omit it for plain responses).
func isTextual(mediaType string) bool {
	switch {
	case mediaType == "":
		return true
	case strings.HasPrefix(mediaType, "text/"):
		return true
	case mediaType == "application/json" || mediaType == "application/xml":
		return true
	case strings.HasSuffix(mediaType, "+json") || strings.HasSuffix(mediaType, "+xml"):
		return true
	}
	return false
}
