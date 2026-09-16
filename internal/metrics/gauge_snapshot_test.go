package metrics

import (
	"strings"
	"sync"
	"testing"
)

func TestGaugeReplaceRemovesAbsentAndCopiesLabels(t *testing.T) {
	r := New()
	g := r.Gauge("quota", "quota snapshot")
	labels := map[string]string{"window": "first"}
	g.Replace([]GaugeSample{{Value: 1, Labels: labels}, {Value: 0, Labels: map[string]string{"window": "zero"}}})
	labels["window"] = "mutated"
	var out strings.Builder
	r.Render(&out)
	if !strings.Contains(out.String(), `quota{window="first"} 1`) || !strings.Contains(out.String(), `quota{window="zero"} 0`) || strings.Contains(out.String(), "mutated") {
		t.Fatal(out.String())
	}
	g.Replace([]GaugeSample{{Value: 2, Labels: nil}, {Value: 3, Labels: nil}})
	out.Reset()
	r.Render(&out)
	if strings.Contains(out.String(), "quota{") || !strings.Contains(out.String(), "quota 3\n") {
		t.Fatal(out.String())
	}
	g.Replace(nil)
	out.Reset()
	r.Render(&out)
	if out.String() != "# HELP quota quota snapshot\n# TYPE quota gauge\n" {
		t.Fatal(out.String())
	}
}

func TestGaugeReplaceConcurrentScrapesSeeCompleteFamily(t *testing.T) {
	r := New()
	g := r.Gauge("quota", "snapshot")
	a := []GaugeSample{{Value: 1, Labels: map[string]string{"set": "a", "window": "first"}}, {Value: 2, Labels: map[string]string{"set": "a", "window": "second"}}}
	b := []GaugeSample{{Value: 3, Labels: map[string]string{"set": "b", "window": "first"}}, {Value: 4, Labels: map[string]string{"set": "b", "window": "second"}}}
	g.Replace(a)
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for range 200 {
			g.Replace(a)
			g.Replace(b)
		}
	}()
	for range 200 {
		var out strings.Builder
		r.Render(&out)
		text := out.String()
		if strings.Count(text, "quota{") != 2 || strings.Contains(text, `set="a"`) && strings.Contains(text, `set="b"`) {
			t.Error(text)
			break
		}
	}
	wg.Wait()
}
