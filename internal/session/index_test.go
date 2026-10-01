package session

import (
	"os"
	"path/filepath"
	"testing"
)

func TestNextIndexPreservesArtifactNumbering(t *testing.T) {
	for _, tc := range []struct {
		name  string
		files []string
		want  int
	}{
		{name: "empty", want: 1},
		{name: "gaps", files: []string{"9.txt", "1.txt", "3.txt"}, want: 10},
		{name: "other suffix ignored", files: []string{"99.json", "bad.txt", "2.txt"}, want: 3},
		{name: "negative indexes", files: []string{"-3.txt", "-1.txt"}, want: 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			for _, name := range tc.files {
				if err := os.WriteFile(filepath.Join(dir, name), nil, 0600); err != nil {
					t.Fatal(err)
				}
			}
			got, err := nextIndex(dir, ".txt")
			if err != nil || got != tc.want {
				t.Fatalf("nextIndex = %d, %v; want %d", got, err, tc.want)
			}
		})
	}
}
