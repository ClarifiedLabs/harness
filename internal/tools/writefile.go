package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

const writeFileSchema = `{
  "type": "object",
  "properties": {
    "path": {"type": "string", "description": "File path to create or overwrite."},
    "content": {"type": "string", "description": "Full file content (empty allowed)."}
  },
  "required": ["path", "content"]
}`

type writeFile struct{}

func (writeFile) Name() string { return "write_file" }

func (writeFile) Description() string {
	return "Create or overwrite a file with the given content. Creates parent directories."
}

func (writeFile) Schema() json.RawMessage { return json.RawMessage(writeFileSchema) }

func (writeFile) ReadOnly(json.RawMessage) bool { return false }

func (writeFile) MutatedPaths(input json.RawMessage) ([]string, error) {
	var args struct {
		Path string `json:"path"`
	}
	if err := json.Unmarshal(input, &args); err != nil {
		return nil, err
	}
	if args.Path == "" {
		return nil, badArgs("path is required")
	}
	return []string{args.Path}, nil
}

func (writeFile) Run(ctx context.Context, input json.RawMessage) (string, error) {
	var args struct {
		Path    string `json:"path"`
		Content string `json:"content"`
	}
	if err := json.Unmarshal(input, &args); err != nil {
		return "", err
	}
	if args.Path == "" {
		return "", badArgs("path is required")
	}
	if strings.HasSuffix(args.Path, "/") || strings.HasSuffix(args.Path, string(os.PathSeparator)) {
		return "", fmt.Errorf("%s has a trailing slash; provide a file path, not a directory", args.Path)
	}
	overwrote := false
	if info, err := os.Stat(args.Path); err == nil {
		if info.IsDir() {
			return "", fmt.Errorf("%s is a directory", args.Path)
		}
		overwrote = true
	}

	if parent := filepath.Dir(args.Path); parent != "" {
		if err := os.MkdirAll(parent, 0755); err != nil {
			return "", err
		}
	}
	if err := os.WriteFile(args.Path, []byte(args.Content), 0644); err != nil {
		return "", err
	}

	verb := "created"
	if overwrote {
		verb = "overwrote"
	}
	return fmt.Sprintf("%s %s (%d bytes, %d lines)", verb, args.Path, len(args.Content), countLines(args.Content)), nil
}

// countLines reports the number of logical lines in content: the count of
// newlines, plus one for a non-empty final line without a trailing newline.
func countLines(content string) int {
	if content == "" {
		return 0
	}
	n := strings.Count(content, "\n")
	if !strings.HasSuffix(content, "\n") {
		n++
	}
	return n
}
