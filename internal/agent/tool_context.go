package agent

import (
	"bytes"
	"encoding/json"
	"reflect"
	"sort"

	"harness/internal/llm"
)

// The first version handles ordinary local tools only. Hosted/deferred tools
// retain their existing protocol and continuation rules.
func (a *Agent) incrementalToolsEnabled() bool {
	if a.provider == nil || a.reasoningReplayDomain == "" || len(a.deferredToolGroups) != 0 || len(a.serverTools) != 0 {
		return false
	}
	info, ok := a.registry.Lookup(a.provider.Name())
	if !ok {
		info, _ = a.registry.Lookup(a.model)
	}
	return info.IncrementalTools
}

func (a *Agent) initialToolContext() *llm.ToolContext {
	if !a.incrementalToolsEnabled() {
		return nil
	}
	return &llm.ToolContext{ReplayDomain: a.reasoningReplayDomain, Initial: true, Tools: a.requestToolSpecs()}
}

func (a *Agent) toolContextStart(messages []llm.Message) int {
	if a.nativeCompaction && !a.disabledNativeCompaction[a.reasoningReplayDomain] {
		for i := len(messages) - 1; i >= 0; i-- {
			for _, block := range messages[i].Content {
				if block.Kind == llm.BlockProviderCompaction && block.ReasoningReplayDomain == a.reasoningReplayDomain {
					return i
				}
			}
		}
	}
	return 0
}

func (a *Agent) incrementalToolsIn(messages []llm.Message) bool {
	if !a.incrementalToolsEnabled() || len(messages) == 0 {
		return false
	}
	start := a.toolContextStart(messages)
	event := messages[start].ToolContext
	if event == nil || !event.Initial || event.After || event.ReplayDomain != a.reasoningReplayDomain {
		return false
	}
	for _, message := range messages[start+1:] {
		if event := message.ToolContext; event != nil && event.Initial && event.ReplayDomain == a.reasoningReplayDomain {
			return false
		}
	}
	return true
}

// prepareToolContext is a pure projection until the caller commits its result.
// It never retrofits an already-sampled message: changes are standalone internal
// events at a closed tool boundary. Debug/count projections use the same code
// without changing live history. Legacy windows keep their ordinary layout.
func (a *Agent) prepareToolContext(messages []llm.Message) ([]llm.Message, bool) {
	if !a.incrementalToolsEnabled() || len(messages) == 0 {
		return messages, false
	}
	if !a.incrementalToolsIn(messages) {
		// A genuinely fresh first prompt or a newly replaced checkpoint can start
		// a window. A foreign baseline or a legacy conversation must not migrate
		// merely because the provider configuration changed on resume.
		if messages[0].ToolContext == nil && len(messages) == 1 && messages[0].Role == llm.RoleUser && !hasToolResult(messages[0]) {
			out := append([]llm.Message(nil), messages...)
			out[0].ToolContext = a.initialToolContext()
			return out, true
		}
		return messages, false
	}
	current := make(map[string]llm.ToolSchema)
	for _, message := range messages[a.toolContextStart(messages):] {
		event := message.ToolContext
		if event == nil || event.ReplayDomain != a.reasoningReplayDomain {
			continue
		}
		for _, tool := range event.Tools {
			current[tool.Name] = tool
		}
		for _, name := range event.Removed {
			delete(current, name)
		}
	}
	event := &llm.ToolContext{ReplayDomain: a.reasoningReplayDomain, After: true}
	for _, tool := range a.requestToolSpecs() {
		previous, exists := current[tool.Name]
		if !exists || !sameToolDefinition(previous, tool) {
			event.Tools = append(event.Tools, tool)
		}
		delete(current, tool.Name)
	}
	for name := range current {
		event.Removed = append(event.Removed, name)
	}
	sort.Strings(event.Removed)
	if len(event.Tools) == 0 && len(event.Removed) == 0 {
		return messages, false
	}
	out := append([]llm.Message(nil), messages...)
	out = append(out, llm.Message{Role: llm.RoleUser, Origin: llm.MessageOriginInternal, Time: a.now(), ToolContext: event})
	return out, true
}

func sameToolDefinition(a, b llm.ToolSchema) bool {
	if a.Name != b.Name || a.Description != b.Description || a.Async != b.Async {
		return false
	}
	if bytes.Equal(a.Parameters, b.Parameters) {
		return true
	}
	var x, y any
	return json.Unmarshal(a.Parameters, &x) == nil && json.Unmarshal(b.Parameters, &y) == nil && reflect.DeepEqual(x, y)
}

// rebaseToolContext belongs only to a replacement context. Retained history must
// not replay old changes over today's complete catalog. The canonical archive
// keeps the original events; the replacement records a new initial snapshot.
func (a *Agent) rebaseToolContext(messages []llm.Message) []llm.Message {
	out := make([]llm.Message, 0, len(messages))
	for _, message := range messages {
		if message.ToolContext != nil {
			message.ToolContext = nil
			// Keep the retained suffix cardinality for the session tree's
			// compaction transaction. Empty markers are omitted on the wire.
		}
		out = append(out, message)
	}
	if len(out) > 0 {
		out[0].ToolContext = a.initialToolContext()
	}
	return out
}

func toolContextBytes(event *llm.ToolContext) int {
	if event == nil {
		return 0
	}
	n := 0
	for _, tool := range event.Tools {
		n += len(tool.Name) + len(tool.Description) + len(tool.Parameters)
	}
	if len(event.Removed) > 0 {
		n += len("The following tools are no longer available. Do not call them:")
		for _, name := range event.Removed {
			n += len(name) + 3
		}
	}
	return n
}
