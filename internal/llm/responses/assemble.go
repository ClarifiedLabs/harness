package responses

import (
	"encoding/json"
	"fmt"
	"sort"

	"harness/internal/llm"
)

type toolAssembler struct {
	pending map[int]*pendingTool
}

type pendingTool struct {
	itemID  string
	callID  string
	name    string
	args    []byte
	started bool
}

func newToolAssembler() *toolAssembler {
	return &toolAssembler{pending: map[int]*pendingTool{}}
}

func (a *toolAssembler) ensure(index int) *pendingTool {
	t := a.pending[index]
	if t == nil {
		t = &pendingTool{}
		a.pending[index] = t
	}
	return t
}

func (a *toolAssembler) outputItemAdded(index int, item *wireOutputItem, yield func(llm.StreamEvent, error) bool) bool {
	if item == nil || item.Type != "function_call" {
		return true
	}
	t := a.ensure(index)
	mergeItem(t, item)
	return a.emitStart(index, t, yield)
}

func (a *toolAssembler) argumentsDelta(index int, delta string, yield func(llm.StreamEvent, error) bool) bool {
	if delta == "" {
		return true
	}
	t := a.ensure(index)
	t.args = append(t.args, delta...)
	return yield(llm.StreamEvent{Kind: llm.EventToolCallDelta, Index: index, ArgsDelta: delta}, nil)
}

func (a *toolAssembler) argumentsDone(index int, itemID, name, arguments string) {
	t := a.ensure(index)
	if itemID != "" {
		t.itemID = itemID
	}
	if name != "" {
		t.name = name
	}
	t.args = []byte(arguments)
}

func (a *toolAssembler) outputItemDone(index int, item *wireOutputItem) {
	if item == nil || item.Type != "function_call" {
		return
	}
	t := a.ensure(index)
	mergeItem(t, item)
	t.args = []byte(item.Arguments)
}

func (a *toolAssembler) responseOutput(output []wireOutputItem) {
	for i := range output {
		item := &output[i]
		if item.Type != "function_call" {
			continue
		}
		t := a.ensure(i)
		mergeItem(t, item)
		t.args = []byte(item.Arguments)
	}
}

func (a *toolAssembler) flush(yield func(llm.StreamEvent, error) bool) (ok bool, fatal error) {
	indices := make([]int, 0, len(a.pending))
	for i := range a.pending {
		indices = append(indices, i)
	}
	sort.Ints(indices)

	for _, i := range indices {
		t := a.pending[i]
		if !t.started {
			if !a.emitStart(i, t, yield) {
				return false, nil
			}
		}
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
			ToolID:    toolID(t),
			ToolName:  t.name,
			ToolInput: json.RawMessage(args),
		}, nil) {
			return false, nil
		}
	}
	a.pending = map[int]*pendingTool{}
	return true, nil
}

func (a *toolAssembler) emitStart(index int, t *pendingTool, yield func(llm.StreamEvent, error) bool) bool {
	if t.started {
		return true
	}
	t.started = true
	return yield(llm.StreamEvent{
		Kind:     llm.EventToolCallStart,
		Index:    index,
		ToolID:   toolID(t),
		ToolName: t.name,
	}, nil)
}

func (a *toolAssembler) has() bool { return len(a.pending) > 0 }

func mergeItem(t *pendingTool, item *wireOutputItem) {
	if item.ID != "" {
		t.itemID = item.ID
	}
	if item.CallID != "" {
		t.callID = item.CallID
	}
	if item.Name != "" {
		t.name = item.Name
	}
}

func toolID(t *pendingTool) string {
	if t.callID != "" {
		return t.callID
	}
	return t.itemID
}
