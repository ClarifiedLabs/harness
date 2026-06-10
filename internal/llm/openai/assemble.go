package openai

import (
	"encoding/json"
	"fmt"
	"sort"

	"harness/internal/llm"
)

// toolAssembler accumulates choices[].delta.tool_calls[] fragments for one
// streamed message, keyed by their tool_calls[].index. The first fragment for an
// index carries id + function.name (emit Start); later fragments carry
// function.arguments string fragments (emit Delta). All buffered calls flush as
// Done together when finish_reason "tool_calls" arrives (design §5.3).
type toolAssembler struct {
	pending map[int]*pendingTool
}

type pendingTool struct {
	id   string
	name string
	args []byte
}

func newToolAssembler() *toolAssembler {
	return &toolAssembler{pending: map[int]*pendingTool{}}
}

// observe folds one streamed tool_call fragment into the buffer, emitting a
// Start event the first time an index is seen and a Delta event for any
// arguments fragment. An emit returning false (consumer break) is reported via
// ok=false so the caller stops.
func (a *toolAssembler) observe(frag wireToolCallDelta, yield func(llm.StreamEvent, error) bool) (ok bool) {
	t := a.pending[frag.Index]
	if t == nil {
		t = &pendingTool{}
		a.pending[frag.Index] = t
		t.id = frag.ID
		t.name = frag.Function.Name
		if !yield(llm.StreamEvent{
			Kind:     llm.EventToolCallStart,
			Index:    frag.Index,
			ToolID:   frag.ID,
			ToolName: frag.Function.Name,
		}, nil) {
			return false
		}
	}
	if frag.Function.Arguments != "" {
		t.args = append(t.args, frag.Function.Arguments...)
		if !yield(llm.StreamEvent{
			Kind:      llm.EventToolCallDelta,
			Index:     frag.Index,
			ArgsDelta: frag.Function.Arguments,
		}, nil) {
			return false
		}
	}
	return true
}

// flush finalizes every buffered call in ascending index order, emitting one
// Done per call. An empty buffer flushes as {}. Accumulated JSON that fails
// json.Valid is a retryable stream error — never a garbage Done; flush returns
// the error and stops without emitting further events.
func (a *toolAssembler) flush(yield func(llm.StreamEvent, error) bool) (ok bool, fatal error) {
	indices := make([]int, 0, len(a.pending))
	for i := range a.pending {
		indices = append(indices, i)
	}
	sort.Ints(indices)

	for _, i := range indices {
		t := a.pending[i]
		args := t.args
		if len(args) == 0 {
			args = []byte(emptyArgs)
		}
		if !json.Valid(args) {
			return false, &llm.APIError{
				Message:   fmt.Sprintf("tool %q produced invalid JSON arguments", t.name),
				Retryable: true,
			}
		}
		if !yield(llm.StreamEvent{
			Kind:      llm.EventToolCallDone,
			Index:     i,
			ToolID:    t.id,
			ToolName:  t.name,
			ToolInput: json.RawMessage(args),
		}, nil) {
			return false, nil
		}
	}
	a.pending = map[int]*pendingTool{}
	return true, nil
}

// has reports whether any tool calls have been buffered.
func (a *toolAssembler) has() bool { return len(a.pending) > 0 }
