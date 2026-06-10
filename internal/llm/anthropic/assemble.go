package anthropic

import (
	"encoding/json"
	"fmt"

	"harness/internal/llm"
)

// toolAssembler accumulates input_json_delta fragments for the tool_use blocks
// of one streamed message, keyed by their content-block index. Text blocks share
// the index space but bypass the assembler (design §5.3).
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

// start records a new tool_use block at index and returns the Start event.
func (a *toolAssembler) start(index int, id, name string) llm.StreamEvent {
	a.pending[index] = &pendingTool{id: id, name: name}
	return llm.StreamEvent{
		Kind:     llm.EventToolCallStart,
		Index:    index,
		ToolID:   id,
		ToolName: name,
	}
}

// delta appends a partial_json fragment for the tool at index and returns the
// Delta event. A fragment for an unknown index (e.g. a text block) is ignored.
func (a *toolAssembler) delta(index int, fragment string) (llm.StreamEvent, bool) {
	t := a.pending[index]
	if t == nil {
		return llm.StreamEvent{}, false
	}
	t.args = append(t.args, fragment...)
	return llm.StreamEvent{
		Kind:      llm.EventToolCallDelta,
		Index:     index,
		ArgsDelta: fragment,
	}, true
}

// flush finalizes the tool at index. An empty buffer flushes as {}. Accumulated
// JSON that fails json.Valid is a retryable stream error — never a garbage
// Done. A non-tool index (text block) yields ok=false so the caller skips
// emission.
func (a *toolAssembler) flush(index int) (llm.StreamEvent, error, bool) {
	t := a.pending[index]
	if t == nil {
		return llm.StreamEvent{}, nil, false
	}
	delete(a.pending, index)

	args := t.args
	if len(args) == 0 {
		args = []byte("{}")
	}
	if !json.Valid(args) {
		return llm.StreamEvent{}, &llm.APIError{
			Message:   fmt.Sprintf("tool %q produced invalid JSON arguments", t.name),
			Retryable: true,
		}, true
	}
	return llm.StreamEvent{
		Kind:      llm.EventToolCallDone,
		Index:     index,
		ToolID:    t.id,
		ToolName:  t.name,
		ToolInput: json.RawMessage(args),
	}, nil, true
}
