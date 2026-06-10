package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

const writeTmpFileSchema = `{
  "type": "object",
  "properties": {
    "name": {"type": "string", "description": "Relative file name under this run's private temp directory, e.g. \"plan.md\" or \"notes/draft.md\"."},
    "content": {"type": "string", "description": "Full file content (empty allowed)."}
  },
  "required": ["name", "content"]
}`

// writeTmpFile writes scratch files under one per-process temp directory,
// giving restricted agents (e.g. plan) a place to draft notes without write
// access to the project. It is the only stateful tool: the directory is
// created lazily on first use and shared across calls, so a single instance
// must be registered per process (Catalog does this).
type writeTmpFile struct {
	once sync.Once
	dir  string
	err  error
}

func newWriteTmpFile() *writeTmpFile { return &writeTmpFile{} }

func (*writeTmpFile) Name() string { return "write_tmp_file" }

func (*writeTmpFile) Description() string {
	return "Write a scratch file under this run's private temp directory and return its absolute path. Files are kept after exit."
}

func (*writeTmpFile) Schema() json.RawMessage { return json.RawMessage(writeTmpFileSchema) }

func (*writeTmpFile) ReadOnly(json.RawMessage) bool { return false }

func (w *writeTmpFile) Run(ctx context.Context, input json.RawMessage) (string, error) {
	var args struct {
		Name    string `json:"name"`
		Content string `json:"content"`
	}
	if err := json.Unmarshal(input, &args); err != nil {
		return "", err
	}
	rel, err := cleanTmpName(args.Name)
	if err != nil {
		return "", err
	}

	w.once.Do(func() { w.dir, w.err = os.MkdirTemp("", "harness-*") })
	if w.err != nil {
		return "", fmt.Errorf("create temp dir: %w", w.err)
	}

	path := filepath.Join(w.dir, rel)
	overwrote := false
	if info, err := os.Stat(path); err == nil {
		if info.IsDir() {
			return "", fmt.Errorf("%s is a directory", path)
		}
		overwrote = true
	}
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return "", err
	}
	if err := os.WriteFile(path, []byte(args.Content), 0644); err != nil {
		return "", err
	}

	verb := "created"
	if overwrote {
		verb = "overwrote"
	}
	return fmt.Sprintf("%s %s (%d bytes, %d lines)", verb, path, len(args.Content), countLines(args.Content)), nil
}

// cleanTmpName validates name as a relative path that stays inside the temp
// directory: empty names, trailing slashes, absolute paths, and any ".."
// escape remaining after Clean are rejected.
func cleanTmpName(name string) (string, error) {
	if name == "" {
		return "", badArgs("name is required")
	}
	if strings.HasSuffix(name, "/") || strings.HasSuffix(name, string(os.PathSeparator)) {
		return "", badArgs("%s has a trailing slash; provide a file name, not a directory", name)
	}
	if filepath.IsAbs(name) {
		return "", badArgs("name must be relative, not absolute: %s", name)
	}
	clean := filepath.Clean(name)
	if clean == ".." || strings.HasPrefix(clean, ".."+string(os.PathSeparator)) {
		return "", badArgs("name escapes the temp directory: %s", name)
	}
	return clean, nil
}
