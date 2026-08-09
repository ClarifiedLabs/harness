package tools

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// renderDirectory renders a bounded, non-recursive directory listing with
// directories first, then other entries, sorted by name within each group.
func renderDirectory(dir string, capEntries int) (string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return "", err
	}

	type row struct {
		name  string
		isDir bool
		text  string
	}
	rows := make([]row, 0, len(entries))
	for _, entry := range entries {
		name := entry.Name()
		isDir := entry.IsDir()
		display := name
		if isDir {
			display += "/"
		}
		rows = append(rows, row{
			name:  name,
			isDir: isDir,
			text:  fmt.Sprintf("%s %8s  %s", fileTypeChar(entry), entrySize(dir, entry, isDir), display),
		})
	}

	sort.SliceStable(rows, func(i, j int) bool {
		if rows[i].isDir != rows[j].isDir {
			return rows[i].isDir
		}
		return rows[i].name < rows[j].name
	})

	total := len(rows)
	if capEntries > 0 && total > capEntries {
		rows = rows[:capEntries]
	}

	var b strings.Builder
	for i, row := range rows {
		if i > 0 {
			b.WriteByte('\n')
		}
		b.WriteString(row.text)
	}
	if len(rows) < total {
		fmt.Fprintf(&b, "\n[truncated: showing first %d of %d entries]", capEntries, total)
	}
	if total == 0 {
		return "(empty directory)", nil
	}
	return b.String(), nil
}

// entrySize renders the size column for a directory entry. Directories show
// "-"; symlinks are resolved with Stat so a broken link (or any entry that
// cannot be stat'd) renders "?" and the listing continues.
func entrySize(dir string, entry os.DirEntry, isDir bool) string {
	if isDir {
		return "-"
	}
	if entry.Type()&os.ModeSymlink != 0 {
		info, err := os.Stat(filepath.Join(dir, entry.Name()))
		if err != nil {
			return "?"
		}
		return HumanBytes(int(info.Size()))
	}
	info, err := entry.Info()
	if err != nil {
		return "?"
	}
	return HumanBytes(int(info.Size()))
}

// fileTypeChar classifies a directory entry in the spirit of ls -l.
func fileTypeChar(entry os.DirEntry) string {
	switch {
	case entry.IsDir():
		return "d"
	case entry.Type()&os.ModeSymlink != 0:
		return "l"
	case entry.Type().IsRegular():
		return "-"
	default:
		return "?"
	}
}
