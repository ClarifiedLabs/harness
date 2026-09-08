package mermaid

import (
	"errors"
	"fmt"
)

// ErrLimitExceeded identifies diagrams that exceed a fixed rendering budget.
// Callers can use errors.Is to distinguish limit fallbacks from syntax errors.
var ErrLimitExceeded = errors.New("mermaid: rendering limit exceeded")

// MaxSourceBytes bounds a diagram before preprocessing. Streaming callers can
// use the same limit to stop buffering and fall back to displaying source.
const MaxSourceBytes = 64 * 1024

// Rendering budgets are inclusive, per diagram, and deliberately fixed. Source
// bytes are checked before preprocessing; graph nodes include state pseudo-nodes
// and sequence participants (not repeated references). Edges count expanded
// Cartesian links; events count messages, notes, and all block delimiters.
// Virtual nodes have a separate layout budget, checked before edge subdivision.
// Keep this below mdcli's 8192: responsive layout may try several candidates
// synchronously in Harness's streaming display path.
// Node lists, note participant lists (including repeats), and composite-state
// nesting each share the 256-entry node limit. Canvas dimensions include a
// front-matter title: terminal columns/rows, and cells bound the full bounding
// rectangle, including whitespace, rather than just occupied cells. Limits are
// checked before expansion/allocation and reported as ordinary Render errors.
const (
	maxNodes           = 256
	maxEdges           = 1024
	maxEvents          = 1024
	maxVirtualNodes    = 1024
	maxCanvasDimension = 4096
	maxCanvasCells     = 1_000_000
)

func limitError(resource string, limit int) error {
	return fmt.Errorf("%w: %s (maximum %d)", ErrLimitExceeded, resource, limit)
}

func checkCanvasSize(w, h int) error {
	if w > maxCanvasDimension || h > maxCanvasDimension {
		return limitError("canvas dimension", maxCanvasDimension)
	}
	if w > 0 && h > maxCanvasCells/w {
		return limitError("canvas cells", maxCanvasCells)
	}
	return nil
}
