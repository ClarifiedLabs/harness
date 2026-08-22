package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"harness/internal/llm"
)

const writeFileSchema = `{
  "type": "object",
	"properties": {
		"path": {"type": "string", "description": "Destination file."},
		"content": {"type": "string", "description": "Complete content; empty allowed."},
		"expected_sha256": {"type": "string", "description": "Optional digest from read; reject a stale overwrite."}
  },
  "required": ["path", "content"]
}`

type writeFile struct{}

func (writeFile) Name() string { return "write" }

func (writeFile) Description() string {
	return "Write a whole file, creating parents; edit for partial changes."
}

func (writeFile) Schema() json.RawMessage { return json.RawMessage(writeFileSchema) }

func (writeFile) PreserveSchemaDescriptions() bool { return true }

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
		Path           string `json:"path"`
		Content        string `json:"content"`
		ExpectedSHA256 string `json:"expected_sha256"`
	}
	if err := json.Unmarshal(input, &args); err != nil {
		return "", err
	}
	if args.Path == "" {
		return "", badArgs("path is required")
	}
	expected, err := normalizeSHA256(args.ExpectedSHA256, "expected_sha256")
	if err != nil {
		return "", err
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
		if expected != "" {
			current, err := os.ReadFile(args.Path)
			if err != nil {
				return "", err
			}
			if err := checkExpectedSHA256(args.Path, expected, current); err != nil {
				return "", err
			}
		}
	} else if !os.IsNotExist(err) {
		return "", err
	} else if expected != "" {
		return "", WithKind(fmt.Errorf("%s disappeared since it was read; expected sha256 %s; re-read the path and retry", args.Path, expected), llm.ToolErrorStaleFile)
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

// RetentionInputReceipt replaces a superseded write body with a path-only
// receipt. The path field keeps MutatedPaths decodable so compaction still
// indexes the mutation; the sentinel marks the input as already trimmed.
func (writeFile) RetentionInputReceipt(input json.RawMessage) (json.RawMessage, bool) {
	var args struct {
		Path           string `json:"path"`
		Content        string `json:"content"`
		ExpectedSHA256 string `json:"expected_sha256"`
	}
	if err := json.Unmarshal(input, &args); err != nil || args.Path == "" {
		return nil, false
	}
	receipt, err := json.Marshal(struct {
		Path           string `json:"path"`
		ExpectedSHA256 string `json:"expected_sha256,omitempty"`
		Superseded     string `json:"_superseded"`
		Bytes          int    `json:"original_bytes"`
	}{
		Path:           args.Path,
		ExpectedSHA256: args.ExpectedSHA256,
		Superseded:     "content omitted; later successful write to this path exists; read the file if needed",
		Bytes:          len(args.Content),
	})
	if err != nil {
		return nil, false
	}
	return receipt, true
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
