package mermaid

import (
	"errors"
	"fmt"
	"strings"
	"testing"
)

func TestLimitErrorIdentity(t *testing.T) {
	err := limitError("virtual nodes", maxVirtualNodes)
	if !errors.Is(fmt.Errorf("context: %w", err), ErrLimitExceeded) {
		t.Fatal("limit sentinel lost through wrapping:", err)
	}
	for _, source := range []string{"pie\nA", "flowchart TD\nA[", "flowchart TD", "sequenceDiagram", "stateDiagram-v2", "classDiagram"} {
		if out, err := Render(source, Options{Width: 80}); err == nil || errors.Is(err, ErrLimitExceeded) || out != "" {
			t.Errorf("Render(%q) = %q, %v; want empty output and non-limit error", source, out, err)
		}
	}
}

func parallelSpanSource(links int) string {
	var source strings.Builder
	source.WriteString("flowchart TD\n")
	for i := 0; i < 17; i++ {
		fmt.Fprintf(&source, "N%d --> N%d\n", i, i+1)
	}
	source.WriteString(strings.Repeat("N0 --> N17\n", links))
	return source.String()
}

func TestResponsiveVirtualBudget(t *testing.T) {
	// Each long link adds 16 virtual nodes. Exercise the inclusive 1,024 cap
	// through Render, including adaptive/list widths; no candidate may bypass it.
	for _, dir := range []string{"TD", "BT", "LR", "RL"} {
		for _, width := range []int{8, 80, maxCanvasDimension} {
			source := strings.Replace(parallelSpanSource(64), "TD", dir, 1)
			if out, err := Render(source, Options{Width: width}); err != nil || out == "" {
				t.Fatalf("%s/%d boundary: got %d bytes, %v", dir, width, len(out), err)
			}
			out, err := Render(source+"N0 --> N2\n", Options{Width: width})
			if out != "" || !errors.Is(err, ErrLimitExceeded) || !strings.Contains(err.Error(), "virtual nodes") {
				t.Fatalf("%s/%d overflow: got %d bytes, %v", dir, width, len(out), err)
			}
		}
	}
}

func BenchmarkResponsiveVirtualBudget(b *testing.B) {
	for _, dir := range []string{"TD", "LR"} {
		for _, width := range []int{80, maxCanvasDimension} {
			b.Run(fmt.Sprintf("%s/%d", dir, width), func(b *testing.B) {
				source := strings.Replace(parallelSpanSource(64), "TD", dir, 1)
				b.ReportAllocs()
				b.ResetTimer()
				for i := 0; i < b.N; i++ {
					if _, err := Render(source, Options{Width: width}); err != nil {
						b.Fatal(err)
					}
				}
			})
		}
	}
}
