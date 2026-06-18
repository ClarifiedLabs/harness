package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
)

// listDirCap bounds the number of entries returned (design §9.2).
const listDirCap = 1000

const listDirSchema = `{
  "type": "object",
  "properties": {
    "path": {"type": "string", "description": "Directory to list (default \".\")."},
    "glob": {"type": "string", "description": "Optional path.Match glob filtering entry base names."}
  }
}`

type listDir struct{}

func (listDir) Name() string { return "list_dir" }

func (listDir) Description() string {
	return "List directory entries with type and size. Provide a JSON object; pass path and optional glob. Non-recursive."
}

func (listDir) Schema() json.RawMessage { return json.RawMessage(listDirSchema) }

func (listDir) ReadOnly(json.RawMessage) bool { return true }

func (listDir) Run(ctx context.Context, input json.RawMessage) (string, error) {
	var args struct {
		Path string `json:"path"`
		Glob string `json:"glob"`
	}
	if err := json.Unmarshal(input, &args); err != nil {
		return "", err
	}
	dir := args.Path
	if dir == "" {
		dir = "."
	}
	if args.Glob != "" {
		if _, err := path.Match(args.Glob, ""); err != nil {
			return "", badArgs("invalid glob: %v", err)
		}
	}

	info, err := os.Stat(dir)
	if err != nil {
		return "", err
	}
	if !info.IsDir() {
		return "", fmt.Errorf("%s is not a directory", dir)
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		return "", err
	}

	type row struct {
		name  string
		isDir bool
		text  string
	}
	var rows []row
	for _, e := range entries {
		name := e.Name()
		if args.Glob != "" {
			ok, _ := path.Match(args.Glob, name)
			if !ok {
				continue
			}
		}
		isDir := e.IsDir()
		typeChar := fileTypeChar(e)
		size := entrySize(dir, e, isDir)
		display := name
		if isDir {
			display += "/"
		}
		rows = append(rows, row{
			name:  name,
			isDir: isDir,
			text:  fmt.Sprintf("%s %8s  %s", typeChar, size, display),
		})
	}

	sort.SliceStable(rows, func(i, j int) bool {
		if rows[i].isDir != rows[j].isDir {
			return rows[i].isDir // dirs first
		}
		return rows[i].name < rows[j].name
	})

	total := len(rows)
	truncated := false
	if total > listDirCap {
		rows = rows[:listDirCap]
		truncated = true
	}

	var b strings.Builder
	for i, r := range rows {
		if i > 0 {
			b.WriteByte('\n')
		}
		b.WriteString(r.text)
	}
	if truncated {
		fmt.Fprintf(&b, "\n[truncated: showing first %d of %d entries; pass a glob to filter]", listDirCap, total)
	}
	if total == 0 {
		return "(empty directory)", nil
	}
	return b.String(), nil
}

// entrySize renders the size column for a directory entry. Directories show
// "-"; symlinks are resolved with Stat so a broken link (or any entry that
// cannot be stat'd) renders "?" and the listing continues (design §9.2).
func entrySize(dir string, e os.DirEntry, isDir bool) string {
	if isDir {
		return "-"
	}
	if e.Type()&os.ModeSymlink != 0 {
		fi, err := os.Stat(filepath.Join(dir, e.Name()))
		if err != nil {
			return "?"
		}
		return HumanBytes(int(fi.Size()))
	}
	fi, err := e.Info()
	if err != nil {
		return "?"
	}
	return HumanBytes(int(fi.Size()))
}

// fileTypeChar returns a single character classifying a directory entry, in the
// spirit of ls -l: d directory, l symlink, - regular, ? other/unknown.
func fileTypeChar(e os.DirEntry) string {
	switch {
	case e.IsDir():
		return "d"
	case e.Type()&os.ModeSymlink != 0:
		return "l"
	case e.Type().IsRegular():
		return "-"
	default:
		return "?"
	}
}
