package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// globScanCap bounds how many matches the walk collects before stopping, so a
// pathological pattern over a huge tree cannot run unbounded. It is well above
// listDirCap (the display cap) so the truncation notice can still report "N+".
const globScanCap = 10000

const globSchema = `{
  "type": "object",
  "properties": {
    "pattern": {"type": "string", "description": "Glob relative to root, e.g. \"**/*config*.go\" or \"internal/*_test.go\". ** matches any number of path segments (including zero); * ? [..] match within a single segment."},
    "root": {"type": "string", "description": "Directory to search from (default \".\")."}
  },
  "required": ["pattern"]
}`

type glob struct{}

func (glob) Name() string { return "glob" }

func (glob) Description() string {
	return `Recursively find files and directories by glob. Provide a JSON object with pattern (e.g. {"pattern":"**/*config*.go"}) and optional root. Read-only; ** matches across directories. Returns matching paths with type and size, one per line, sorted by path.`
}

func (glob) Schema() json.RawMessage { return json.RawMessage(globSchema) }

func (glob) ReadOnly(json.RawMessage) bool { return true }

func (glob) Run(ctx context.Context, input json.RawMessage) (string, error) {
	var args struct {
		Pattern string `json:"pattern"`
		Root    string `json:"root"`
	}
	if err := json.Unmarshal(input, &args); err != nil {
		return "", err
	}
	if strings.TrimSpace(args.Pattern) == "" {
		return "", badArgs("pattern is required")
	}
	if err := validateGlobPattern(args.Pattern); err != nil {
		return "", err
	}

	root := args.Root
	if root == "" {
		root = "."
	}
	info, err := os.Stat(root)
	if err != nil {
		return "", err
	}
	if !info.IsDir() {
		return "", fmt.Errorf("%s is not a directory", root)
	}

	type match struct {
		path string
		text string
	}
	var matches []match
	capped := false
	walkErr := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil // skip unreadable entries, mirroring list_dir's resilience
		}
		rel, rerr := filepath.Rel(root, path)
		if rerr != nil || rel == "." {
			return nil // skip the root itself
		}
		ok, merr := globMatch(args.Pattern, filepath.ToSlash(rel))
		if merr != nil {
			return merr
		}
		if !ok {
			return nil
		}
		matches = append(matches, match{path: path, text: globRowText(path, d)})
		if len(matches) >= globScanCap {
			capped = true
			return filepath.SkipAll
		}
		return nil
	})
	if walkErr != nil {
		return "", walkErr
	}
	if len(matches) == 0 {
		return "(no matches)", nil
	}

	sort.Slice(matches, func(i, j int) bool { return matches[i].path < matches[j].path })

	total := len(matches)
	truncated := false
	if total > listDirCap {
		matches = matches[:listDirCap]
		truncated = true
	}

	var b strings.Builder
	for i, m := range matches {
		if i > 0 {
			b.WriteByte('\n')
		}
		b.WriteString(m.text)
	}
	if truncated {
		more := strconv.Itoa(total)
		if capped {
			more += "+"
		}
		fmt.Fprintf(&b, "\n[truncated: showing first %d of %s matches; narrow the pattern or root]", listDirCap, more)
	}
	return b.String(), nil
}

// globRowText renders one matched entry in list_dir's column format, but with
// the full root-relative path (directly usable with read_file) instead of just a
// base name. Reuses fileTypeChar/entrySize/HumanBytes.
func globRowText(path string, d fs.DirEntry) string {
	isDir := d.IsDir()
	display := path
	if isDir {
		display += "/"
	}
	return fmt.Sprintf("%s %8s  %s", fileTypeChar(d), entrySize(filepath.Dir(path), d, isDir), display)
}

// validateGlobPattern rejects a malformed pattern up front (per non-** segment)
// so a bad pattern fails fast instead of only when a candidate path is reached.
func validateGlobPattern(pattern string) error {
	for _, seg := range strings.Split(pattern, "/") {
		if seg == "**" {
			continue
		}
		if _, err := filepath.Match(seg, ""); err != nil {
			return badArgs("invalid glob pattern: %v", err)
		}
	}
	return nil
}

// globMatch reports whether the slash-separated name matches pattern, where **
// matches any number of path segments (including zero) and each other segment is
// matched with filepath.Match.
func globMatch(pattern, name string) (bool, error) {
	return matchGlobSegments(strings.Split(pattern, "/"), strings.Split(name, "/"))
}

func matchGlobSegments(pat, name []string) (bool, error) {
	for len(pat) > 0 {
		if pat[0] == "**" {
			rest := pat[1:]
			for len(rest) > 0 && rest[0] == "**" {
				rest = rest[1:] // collapse consecutive **
			}
			if len(rest) == 0 {
				return true, nil // trailing ** matches any remainder
			}
			for i := 0; i <= len(name); i++ {
				ok, err := matchGlobSegments(rest, name[i:])
				if err != nil {
					return false, err
				}
				if ok {
					return true, nil
				}
			}
			return false, nil
		}
		if len(name) == 0 {
			return false, nil
		}
		ok, err := filepath.Match(pat[0], name[0])
		if err != nil {
			return false, err
		}
		if !ok {
			return false, nil
		}
		pat, name = pat[1:], name[1:]
	}
	return len(name) == 0, nil
}
