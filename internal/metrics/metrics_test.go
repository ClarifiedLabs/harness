package metrics

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

func TestCounterAccumulatesPerLabelSet(t *testing.T) {
	r := New()
	c := r.Counter("requests_total", "total requests")
	c.Add(2, map[string]string{"provider": "openai", "model": "gpt"})
	c.Add(3, map[string]string{"provider": "openai", "model": "gpt"})
	c.Add(5, map[string]string{"provider": "anthropic", "model": "claude"})

	var b strings.Builder
	r.Render(&b)
	out := b.String()

	if !strings.Contains(out, "# HELP requests_total total requests") {
		t.Errorf("missing HELP line:\n%s", out)
	}
	if !strings.Contains(out, "# TYPE requests_total counter") {
		t.Errorf("missing TYPE line:\n%s", out)
	}
	// Series sorted by value within model column: claude < gpt, anthropic < openai.
	if !strings.Contains(out, `requests_total{model="claude",provider="anthropic"} 5`) {
		t.Errorf("missing claude series:\n%s", out)
	}
	if !strings.Contains(out, `requests_total{model="gpt",provider="openai"} 5`) {
		t.Errorf("missing/accumulated gpt series:\n%s", out)
	}
	// Labels sorted by name (model before provider).
	if idxModel := strings.Index(out, `model="`); idxModel == -1 {
		t.Fatal("no model label found")
	}
}

func TestCounterInc(t *testing.T) {
	r := New()
	c := r.Counter("hits", "hits")
	c.Inc(map[string]string{"k": "a"})
	c.Inc(map[string]string{"k": "a"})
	var b strings.Builder
	r.Render(&b)
	if !strings.Contains(b.String(), `hits{k="a"} 2`) {
		t.Errorf("Inc did not accumulate:\n%s", b.String())
	}
}

func TestCounterIdenticalLabelsCoalesce(t *testing.T) {
	r := New()
	c := r.Counter("c", "h")
	c.Add(1, map[string]string{"a": "1", "b": "2"})
	// Same labels in a different map instance coalesce.
	c.Add(1, map[string]string{"b": "2", "a": "1"})
	var b strings.Builder
	r.Render(&b)
	out := b.String()
	count := strings.Count(out, "c{")
	if count != 1 {
		t.Errorf("expected 1 series, got %d:\n%s", count, out)
	}
	if !strings.Contains(out, `c{a="1",b="2"} 2`) {
		t.Errorf("coalesced value wrong:\n%s", out)
	}
}

func TestCounterEmptyVsMissingLabel(t *testing.T) {
	r := New()
	c := r.Counter("c", "h")
	c.Add(1, map[string]string{"a": ""}) // present but empty
	c.Add(1, map[string]string{})        // missing label a
	var b strings.Builder
	r.Render(&b)
	out := b.String()
	// Prometheus treats an empty-valued label as identical to a missing label,
	// so the two adds coalesce into a single bare series rendered without braces.
	if strings.Contains(out, "c{") {
		t.Errorf("empty label should render bare (no braces), got:\n%s", out)
	}
	if !strings.Contains(out, "c 2\n") {
		t.Errorf("empty and missing label should coalesce to `c 2`:\n%s", out)
	}
}

func TestGaugeSetAndAdd(t *testing.T) {
	r := New()
	g := r.Gauge("build_info", "build")
	g.Set(1, map[string]string{"version": "1"})
	g.Add(2, map[string]string{"version": "1"})
	var b strings.Builder
	r.Render(&b)
	out := b.String()
	if !strings.Contains(out, "# TYPE build_info gauge") {
		t.Errorf("missing gauge type:\n%s", out)
	}
	if !strings.Contains(out, `build_info{version="1"} 3`) {
		t.Errorf("gauge set+add wrong:\n%s", out)
	}
}

func TestEscapeLabelValue(t *testing.T) {
	r := New()
	c := r.Counter("c", "h")
	c.Add(1, map[string]string{"k": `a\"b` + "\n" + "c"})
	var b strings.Builder
	r.Render(&b)
	out := b.String()
	if !strings.Contains(out, `c{k="a\\\"b\nc"} 1`) {
		t.Errorf("label value not escaped:\n%s", out)
	}
}

func TestHandlerGET(t *testing.T) {
	r := New()
	r.Counter("c", "h").Inc(nil)
	srv := httptest.NewServer(r.Handler())
	defer srv.Close()
	resp, err := http.Get(srv.URL + "/metrics")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	ct := resp.Header.Get("Content-Type")
	if !strings.HasPrefix(ct, "text/plain") || !strings.Contains(ct, "version=0.0.4") {
		t.Fatalf("content-type = %q", ct)
	}
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "# TYPE c counter") {
		t.Errorf("body missing exposition:\n%s", body)
	}
}

func TestHandlerMethodAndPath(t *testing.T) {
	r := New()
	h := r.Handler()

	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/metrics", nil))
	if w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("POST status = %d, want 405", w.Code)
	}

	w = httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/other", nil))
	if w.Code != http.StatusNotFound {
		t.Fatalf("other path status = %d, want 404", w.Code)
	}
}

func TestCounterConcurrentAdd(t *testing.T) {
	r := New()
	c := r.Counter("c", "h")
	var wg sync.WaitGroup
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			c.Add(1, map[string]string{"k": "shared"})
			c.Add(1, map[string]string{"k": "odd"})
		}(i)
	}
	wg.Wait()
	var b strings.Builder
	r.Render(&b)
	out := b.String()
	if !strings.Contains(out, `c{k="shared"} 100`) {
		t.Errorf("shared series wrong:\n%s", out)
	}
	if !strings.Contains(out, `c{k="odd"} 100`) {
		t.Errorf("odd series wrong:\n%s", out)
	}
}
