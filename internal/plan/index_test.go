package plan

import (
	"os"
	"path/filepath"
	"testing"
)

func TestNextIndexUsesLargestPublishedPlan(t *testing.T) {
	for _, tc := range []struct {
		name  string
		files []string
		want  int
	}{
		{name: "empty", want: 1},
		{name: "gaps", files: []string{"0009-last.plan.md", "0001-first.plan.md", "0003-middle.plan.md"}, want: 10},
		{name: "unpublished ignored", files: []string{"9999-draft.tmp", "bad.plan.md", "0002-saved.plan.md"}, want: 3},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			for _, name := range tc.files {
				if err := os.WriteFile(filepath.Join(dir, name), nil, 0600); err != nil {
					t.Fatal(err)
				}
			}
			got, err := nextIndex(dir)
			if err != nil || got != tc.want {
				t.Fatalf("nextIndex = %d, %v; want %d", got, err, tc.want)
			}
		})
	}
}
