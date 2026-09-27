package llm

import (
	"strings"
	"testing"
)

func TestFormatCitationSourcesOrderingAndDeduplication(t *testing.T) {
	got := FormatCitationSources([]URLCitation{
		{URL: "javascript:alert(1)", Title: "Ignored"},
		{URL: "https://example.com/b", Title: "First title"},
		{URL: "https://example.com/a"},
		{URL: "https://example.com/b", Title: "Later title"},
		{URL: "https://example.com/a", Title: "Still no title"},
		{URL: "http://example.com/c", Title: "Third"},
	})
	want := "\n\nSources:\n- [1](https://example.com/b) — First title\n- [2](https://example.com/a)\n- [3](http://example.com/c) — Third\n"
	if got != want {
		t.Fatalf("sources = %q, want %q", got, want)
	}
	if got := FormatCitationSources(nil); got != "" {
		t.Fatalf("empty sources = %q", got)
	}
}

func TestFormatCitationSourcesRejectsUnsafeURLs(t *testing.T) {
	for _, raw := range []string{
		"", "relative/path", "//example.com", "https:///path", "https://", "https://:80/path",
		"ftp://example.com", "data:text/plain,hello", "https://user:secret@example.com", "https://@example.com",
		"https://example.com/\x1b[31m", "https://example.com/?q=\n", "https://example.com/?q=%0A",
		"https://example.com/%1b", "https://example.com/%7F", "https://example.com/%C2%85",
		"https://example.com/%E2%80%AE", "https://example.com/\u202e", "https://example.com/%25%0A",
		"https://example.com/\xff", "https://example.com/%FF", "https://exa\u00a0mple.com/",
		"https://ex(ample).com/", "https://ex'ample.com/", "https://ex%28ample%29.com/",
	} {
		t.Run(raw, func(t *testing.T) {
			if got := FormatCitationSources([]URLCitation{{URL: raw, Title: "Ignored"}}); got != "" {
				t.Fatalf("unsafe URL %q produced %q", raw, got)
			}
		})
	}
}

func TestFormatCitationSourcesEscapesDestinationsAndSanitizesTitles(t *testing.T) {
	got := FormatCitationSources([]URLCitation{
		{URL: "https://example.com/a(b)?q=hello world&x=<tag>`\\\"'|{}", Title: "  **Good**\n[read](docs)\t<em>title</em>\x00\u202e  "},
		{URL: "https://example.com/a%28b%29?q=hello%20world&x=%3Ctag%3E%60%5C%22%27%7C%7B%7D", Title: "Duplicate"},
		{URL: "https://[::1]/guide", Title: "Café — 日本語 \u00a0 guide"},
		{URL: "https://example.com/empty", Title: "*_[]\x1b\x07"},
	})
	want := "\n\nSources:\n- [1](https://example.com/a%28b%29?q=hello%20world&x=%3Ctag%3E%60%5C%22%27%7C%7B%7D) — Good readdocs emtitle/em\n- [2](https://[::1]/guide) — Café — 日本語 guide\n- [3](https://example.com/empty)\n"
	if got != want {
		t.Fatalf("sources = %q, want %q", got, want)
	}
	body, sources := SplitCitationSources("Answer." + got)
	if body != "Answer." || sources != got {
		t.Fatalf("split escaped sources = %q, %q", body, sources)
	}
}

// Regression: a literal '%' that is not a valid escape used to drop the whole
// citation; it is now escaped as %25 and survives the canonical round trip.
func TestFormatCitationSourcesEscapesStrayPercent(t *testing.T) {
	for raw, want := range map[string]string{
		"https://example.com/sale?off=50%":  "https://example.com/sale?off=50%25",
		"https://example.com/a%zzb":         "https://example.com/a%25zzb",
		"https://example.com/?q=%zz&r=%4":   "https://example.com/?q=%25zz&r=%254",
		"https://example.com/ok?q=a%20b%25": "https://example.com/ok?q=a%20b%25",
	} {
		t.Run(raw, func(t *testing.T) {
			sources := FormatCitationSources([]URLCitation{{URL: raw, Title: "Sale"}})
			if wantSources := "\n\nSources:\n- [1](" + want + ") — Sale\n"; sources != wantSources {
				t.Fatalf("sources = %q, want %q", sources, wantSources)
			}
			if body, split := SplitCitationSources("Body" + sources); body != "Body" || split != sources {
				t.Fatalf("stray-percent sources do not round trip: %q, %q", body, split)
			}
			if again := FormatCitationSources([]URLCitation{{URL: raw}, {URL: want}}); strings.Count(again, "- [") != 1 {
				t.Fatalf("escaped and raw spellings not deduplicated: %q", again)
			}
		})
	}
}

func TestCitationSourcesRoundTrip(t *testing.T) {
	for _, raw := range []string{
		"https://example.com/a(b)", "https://example.com/%28a%29", "https://example.com/a[b]",
		"https://example.com/?q=[a](b)", "https://example.com/#a(b)", "https://example.com/a b",
		"https://example.com/日本語?q=café", "https://example.com/?q=a\u00a0b", "https://[::1]/",
		"https://ex(ample).com/", "https://example.com/?q=%25", "HTTPS://example.com/",
	} {
		t.Run(raw, func(t *testing.T) {
			sources := FormatCitationSources([]URLCitation{{URL: raw, Title: "A title"}})
			body, split := SplitCitationSources("Body" + sources)
			if body != "Body" || split != sources {
				t.Fatalf("generated sources do not round trip: %q => %q, %q", sources, body, split)
			}
		})
	}
}

func TestSplitCitationSourcesCanonicalOnly(t *testing.T) {
	sources := FormatCitationSources([]URLCitation{{URL: "https://example.com/path", Title: "Title"}, {URL: "http://example.org/"}})
	for _, body := range []string{"", "Answer.", "Answer.\n", "A previous\n\nSources:\nsection in prose."} {
		gotBody, gotSources := SplitCitationSources(body + sources)
		if gotBody != body || gotSources != sources {
			t.Fatalf("split = %q, %q; want %q, %q", gotBody, gotSources, body, sources)
		}
	}
	for name, text := range map[string]string{
		"no sources":            "Answer.",
		"empty sources":         "Answer.\n\nSources:\n",
		"nontrailing":           sources + "extra prose",
		"extra newline":         sources + "\n",
		"missing newline":       strings.TrimSuffix(sources, "\n"),
		"wrong heading":         strings.Replace(sources, "Sources:", "Sources", 1),
		"number gap":            strings.Replace(sources, "[2]", "[3]", 1),
		"wrong bullet":          strings.Replace(sources, "- [1]", "* [1]", 1),
		"unsafe scheme":         strings.Replace(sources, "https://", "ftp://", 1),
		"unescaped destination": strings.Replace(sources, "/path", "/pa(th)", 1),
		"markdown title":        strings.Replace(sources, "Title", "**Title**", 1),
		"multiline title":       strings.Replace(sources, "Title", "Title\nInjected", 1),
		"noncanonical spaces":   strings.Replace(sources, "Title", "A  title", 1),
		"empty title":           strings.Replace(sources, "Title", "", 1),
		"duplicate URL":         strings.Replace(sources, "http://example.org/", "https://example.com/path", 1),
		"encoded control":       strings.Replace(sources, "/path", "/%0A", 1),
	} {
		t.Run(name, func(t *testing.T) {
			body, appendix := SplitCitationSources(text)
			if body != text || appendix != "" {
				t.Fatalf("noncanonical suffix accepted: body=%q, sources=%q", body, appendix)
			}
		})
	}
}
