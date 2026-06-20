package tools

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"
)

func runWebFetch(t *testing.T, args map[string]any) (string, error) {
	return runTool(t, webFetch{}, args)
}

func runWebFetchWithBG(t *testing.T, bg BackgroundJobStarter, args map[string]any) (string, error) {
	return runTool(t, webFetch{background: bg}, args)
}

func TestWebFetchBackgroundStartsJob(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		writeString(w, "background content")
	}))
	defer srv.Close()

	starter := &fakeBackgroundStarter{}
	out, err := runWebFetchWithBG(t, starter, map[string]any{
		"url":        srv.URL,
		"background": true,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if out != "background job bg_test started" {
		t.Fatalf("start output = %q", out)
	}
	if starter.req.Kind != "web_fetch" {
		t.Fatalf("job kind = %q, want web_fetch", starter.req.Kind)
	}
	if starter.req.Description != srv.URL {
		t.Fatalf("job description = %q, want %q", starter.req.Description, srv.URL)
	}
	if starter.req.Run == nil {
		t.Fatal("background job runner missing")
	}

	result, err := starter.req.Run(context.Background(), "bg_test")
	if err != nil {
		t.Fatalf("background run: %v", err)
	}
	if !strings.Contains(result.Text, "background content") {
		t.Fatalf("background result = %q", result.Text)
	}
}

func TestWebFetchBackgroundRequiresStarter(t *testing.T) {
	_, err := runWebFetch(t, map[string]any{
		"url":        "http://example.com",
		"background": true,
	})
	if err == nil {
		t.Fatal("expected error when background manager is unavailable")
	}
	if !strings.Contains(err.Error(), "background manager") {
		t.Fatalf("error = %v", err)
	}
}

func TestWebFetchTextPlainRaw(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		writeString(w, "line one\nline two\n")
	}))
	defer srv.Close()

	out, err := runWebFetch(t, map[string]any{"url": srv.URL})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(out, "line one\nline two") {
		t.Errorf("text/plain should be returned raw: %q", out)
	}
	if !strings.HasPrefix(out, "# "+srv.URL) {
		t.Errorf("missing header prefix with url: %q", out)
	}
	if !strings.Contains(out, "200") || !strings.Contains(out, "text/plain") {
		t.Errorf("header should report status and content-type: %q", out)
	}
}

func TestWebFetchJSONRaw(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		writeString(w, `{"a":1,"b":[2,3]}`)
	}))
	defer srv.Close()

	out, err := runWebFetch(t, map[string]any{"url": srv.URL})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(out, `{"a":1,"b":[2,3]}`) {
		t.Errorf("json should be returned raw: %q", out)
	}
}

func TestWebFetchHTMLReduced(t *testing.T) {
	html := `<html><head><title>T</title>
<style>.x{color:red}</style>
<script>var a = 1 < 2;</script>
</head><body><h1>Hello &amp; welcome</h1><p>Some  text   here.</p></body></html>`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		writeString(w, html)
	}))
	defer srv.Close()

	out, err := runWebFetch(t, map[string]any{"url": srv.URL})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if strings.Contains(out, "color:red") {
		t.Errorf("style contents should be dropped: %q", out)
	}
	if strings.Contains(out, "var a") {
		t.Errorf("script contents should be dropped: %q", out)
	}
	if strings.Contains(out, "<h1>") || strings.Contains(out, "<p>") {
		t.Errorf("tags should be stripped: %q", out)
	}
	if !strings.Contains(out, "Hello & welcome") {
		t.Errorf("entities should be unescaped: %q", out)
	}
	if !strings.Contains(out, "Some text here.") {
		t.Errorf("whitespace should be collapsed: %q", out)
	}
}

// r39: reduceHTML preserves links and block structure.
func TestReduceHTMLStructure(t *testing.T) {
	in := `<h1>Title</h1><p>See <a href="https://example.com/docs">the docs</a> for more.</p><ul><li>one</li><li>two</li></ul>`
	out := reduceHTML(in)
	if !strings.Contains(out, "the docs (https://example.com/docs)") {
		t.Errorf("anchor should render as text (url): %q", out)
	}
	lines := strings.Split(out, "\n")
	want := []string{"Title", "See the docs (https://example.com/docs) for more.", "one", "two"}
	if !slices.Equal(lines, want) {
		t.Errorf("block structure not preserved:\n got %q\nwant %q", lines, want)
	}
}

func TestReduceHTMLBrAndPerLineCollapse(t *testing.T) {
	if got := reduceHTML("a  b<br>c   d"); got != "a b\nc d" {
		t.Errorf("br + per-line collapse wrong: %q", got)
	}
	// </tr> is a block boundary too.
	if got := reduceHTML("<table><tr><td>r1</td></tr><tr><td>r2</td></tr></table>"); got != "r1\nr2" {
		t.Errorf("table rows not separated: %q", got)
	}
}

func TestExtractHref(t *testing.T) {
	cases := map[string]string{
		`<a href="https://x.com">`: "https://x.com",
		`<a href='https://y.com'>`: "https://y.com",
		`<a href=https://z.com>`:   "https://z.com",
		`<a class="c" href="u">`:   "u",
		`<a name="anchor">`:        "",
	}
	for tag, want := range cases {
		if got := extractHref(tag); got != want {
			t.Errorf("extractHref(%q) = %q, want %q", tag, got, want)
		}
	}
}

func TestRenderAnchorsEdgeCases(t *testing.T) {
	// An anchor without href collapses to its text; <article> must not be treated
	// as an anchor; a missing close tag is tolerated.
	out := reduceHTML(`<a>click</a> and <article>body</article> and <a href="u">x`)
	if !strings.Contains(out, "click") {
		t.Errorf("hrefless anchor text lost: %q", out)
	}
	if strings.Contains(out, "<article>") || strings.Contains(out, "<a") {
		t.Errorf("tags should be stripped: %q", out)
	}
	if !strings.Contains(out, "x (u)") {
		t.Errorf("unterminated anchor should still render text (url): %q", out)
	}
}

func TestWebFetchMaxBytes(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		writeString(w, strings.Repeat("x", 100000))
	}))
	defer srv.Close()

	out, err := runWebFetch(t, map[string]any{"url": srv.URL, "max_bytes": 100})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// Body content (the x's) must be capped near max_bytes, not the full 100k.
	body := out
	if i := strings.Index(out, "\n"); i >= 0 {
		body = out[i+1:]
	}
	if strings.Count(body, "x") > 200 {
		t.Errorf("max_bytes did not stop reading: kept %d bytes", strings.Count(body, "x"))
	}
}

func TestWebFetchTimeoutSeconds(t *testing.T) {
	oldUnit := webFetchTimeoutUnit
	webFetchTimeoutUnit = 25 * time.Millisecond
	t.Cleanup(func() { webFetchTimeoutUnit = oldUnit })

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	}))
	defer srv.Close()

	start := time.Now()
	_, err := runWebFetch(t, map[string]any{"url": srv.URL, "timeout_seconds": 1})
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("expected timeout error")
	}
	if elapsed > 5*time.Second {
		t.Fatalf("web_fetch timeout was not honored promptly: took %v", elapsed)
	}
	if !strings.Contains(err.Error(), "timeout") && !strings.Contains(err.Error(), "deadline") {
		t.Fatalf("timeout error = %v", err)
	}
}

func TestDecodeWebFetchArgsTimeoutAlias(t *testing.T) {
	args, err := decodeWebFetchArgs(json.RawMessage(`{"url":"https://example.com","timeout":45}`))
	if err != nil {
		t.Fatalf("decodeWebFetchArgs: %v", err)
	}
	if args.TimeoutSeconds != 45 {
		t.Fatalf("TimeoutSeconds = %d, want 45", args.TimeoutSeconds)
	}

	_, err = decodeWebFetchArgs(json.RawMessage(`{"url":"https://example.com","timeout":45,"timeout_seconds":46}`))
	if err == nil {
		t.Fatal("expected mismatch error when timeout and timeout_seconds differ")
	}
}

func TestWebFetchDefaultTimeoutAndNoMaximumCap(t *testing.T) {
	if got := resolveWebFetchTimeoutSeconds(0); got != webFetchDefaultTimeout {
		t.Fatalf("resolveWebFetchTimeoutSeconds(0) = %d, want %d", got, webFetchDefaultTimeout)
	}
	if got := resolveWebFetchTimeoutSeconds(601); got != 601 {
		t.Fatalf("resolveWebFetchTimeoutSeconds(601) = %d, want 601", got)
	}
	if got := resolveWebFetchTimeoutSeconds(3600); got != 3600 {
		t.Fatalf("resolveWebFetchTimeoutSeconds(3600) = %d, want 3600", got)
	}
}

func TestWebFetchRedirectFinalURL(t *testing.T) {
	var finalURL string
	mux := http.NewServeMux()
	mux.HandleFunc("/final", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		writeString(w, "arrived")
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	finalURL = srv.URL + "/final"

	mux.HandleFunc("/start", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, finalURL, http.StatusFound)
	})

	out, err := runWebFetch(t, map[string]any{"url": srv.URL + "/start"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(out, "arrived") {
		t.Errorf("redirect not followed: %q", out)
	}
	if !strings.Contains(out, finalURL) {
		t.Errorf("final url should appear in header: %q", out)
	}
}

func TestWebFetchNon2xxReturnsBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		w.WriteHeader(http.StatusNotFound)
		writeString(w, "no such page")
	}))
	defer srv.Close()

	out, err := runWebFetch(t, map[string]any{"url": srv.URL})
	if err != nil {
		t.Fatalf("non-2xx must not be a tool error: %v", err)
	}
	if !strings.Contains(out, "404") {
		t.Errorf("status should appear in header: %q", out)
	}
	if !strings.Contains(out, "no such page") {
		t.Errorf("error page body should be returned as content: %q", out)
	}
}

func TestWebFetchNonHTTPSchemeRejected(t *testing.T) {
	for _, u := range []string{"file:///etc/passwd", "ftp://example.com/x", "gopher://x"} {
		_, err := runWebFetch(t, map[string]any{"url": u})
		if err == nil {
			t.Errorf("scheme in %q should be rejected", u)
		}
	}
}

func TestWebFetchBinaryContentTypeRejected(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/octet-stream")
		writeString(w, "\x00\x01\x02binary")
	}))
	defer srv.Close()

	_, err := runWebFetch(t, map[string]any{"url": srv.URL})
	if err == nil {
		t.Fatal("binary content type should be rejected")
	}
	if !strings.Contains(err.Error(), "octet-stream") {
		t.Errorf("error should mention the content type: %v", err)
	}
}

func TestWebFetchMissingURL(t *testing.T) {
	_, err := runWebFetch(t, map[string]any{})
	if err == nil {
		t.Fatal("expected error for missing url")
	}
}

// writeString streams s to w via io.Copy (matching the provider tests'
// precedent) rather than ResponseWriter.Write, keeping the body-writing seam
// uniform across the suite.
func writeString(w http.ResponseWriter, s string) {
	_, _ = io.Copy(w, strings.NewReader(s))
}
