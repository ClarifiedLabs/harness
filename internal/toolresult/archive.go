// Package toolresult prepares truncated tool output for archival and model reuse.
package toolresult

import (
	"fmt"
	"strconv"

	"harness/internal/llm"
	"harness/internal/tools"
)

// ArchivedHintMarker is the stable substring included in every model-facing
// archive hint. Callers can use it to avoid archiving the same result twice.
const ArchivedHintMarker = "full output archived at"

// Archive references the persisted full output behind a truncated result.
type Archive struct {
	DisplayPath string
	ModelPath   string
}

// Archiver persists full raw output and returns a path the model can read or
// search. DisplayPath may be relative for concise user-facing notices;
// ModelPath should be directly usable by file tools.
type Archiver interface {
	ArchiveToolResult(llm.ToolResult) (Archive, error)
}

// PrepareTruncated archives a truncated result when possible, adds the shared
// model-facing recovery hint, and returns the shared user-facing notice. An
// unavailable archiver leaves the ordinary truncation marker intact.
func PrepareTruncated(r llm.ToolResult, archiver Archiver) (llm.ToolResult, string) {
	if !r.Truncated {
		return r, ""
	}
	msg := fmt.Sprintf("[tool result truncated: showing %s of %s", tools.HumanBytes(r.ShownBytes), tools.HumanBytes(r.OriginalBytes))
	if archiver == nil {
		return r, msg + "]"
	}
	archive, err := archiver.ArchiveToolResult(r)
	if err != nil {
		return r, fmt.Sprintf("[tool result truncated; full output archive failed: %v]", err)
	}
	if archive.DisplayPath != "" {
		msg += "; full output: " + archive.DisplayPath
	}
	if archive.ModelPath != "" {
		r.Text += "\n" + ArchivedHint(archive.ModelPath)
	}
	return r, msg + "]"
}

// ArchivedHint gives the model a concrete path and targeted inspection
// examples instead of encouraging it to load a potentially large artifact in
// one call.
func ArchivedHint(path string) string {
	quoted := strconv.Quote(path)
	return fmt.Sprintf(`[full output archived at %s; use read_file {"path":%s,"offset":1,"limit":200} or rg {"args":["-n","<pattern>",%s]} to inspect it]`, quoted, quoted, quoted)
}
