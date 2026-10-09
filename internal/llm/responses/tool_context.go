package responses

import (
	"encoding/json"

	"harness/internal/llm"
)

// incrementalToolsEnabled requires a caller-selected window and provider eligibility.
// A full replay must start with an authoritative baseline; a trusted remote
// continuation can contain only changes (or no catalog events at all).
func incrementalToolsEnabled(req llm.Request, opts buildOptions) bool {
	if !req.IncrementalTools || opts.codexBackend ||
		!opts.promptCache.IncrementalToolsEnabled(req.Model, opts.baseURL) ||
		len(req.DeferredToolGroups) != 0 || len(req.ServerTools) != 0 {
		return false
	}
	if req.PreviousResponseID != "" {
		return true
	}
	if len(req.Messages) == 0 || req.Messages[0].ToolContext == nil ||
		!req.Messages[0].ToolContext.Initial || req.Messages[0].ToolContext.After {
		return false
	}
	for _, message := range req.Messages[1:] {
		if message.ToolContext != nil && message.ToolContext.Initial {
			return false
		}
	}
	return true
}

// lowerToolContext inserts events around already-lowered message boundaries.
// It neither infers a catalog from today's req.Tools nor rewrites old events.
func lowerToolContext(input []wireInputItem, ends []int, req llm.Request, opts buildOptions) ([]wireInputItem, []int) {
	out := make([]wireInputItem, 0, len(input)+len(req.Messages)+2)
	messageEnds := make([]int, 0, len(ends))
	async := canonicalOpenAIEndpoint(opts.baseURL) && isAstraModel(req.Model) &&
		(req.Purpose == "" || req.Purpose == llm.RequestPurposeTurn)
	add := func(event *llm.ToolContext) {
		// An anchor-zero continuation still contains the initial event. Normal
		// suffixes do not, so honor the caller's message boundary rather than
		// assuming every remote response already holds the initial prefix.
		if event == nil {
			return
		}
		if event.Initial || len(event.Tools) > 0 {
			tools := make([]wireTool, 0, len(event.Tools))
			for _, tool := range event.Tools {
				tool.Async = tool.Async && async
				tools = append(tools, buildFunctionTool(tool, false))
			}
			out = append(out, wireInputItem{Type: "additional_tools", Role: "developer", Tools: &tools})
		}
		if event.Initial && req.System != "" {
			part := wireContentPart{Type: "input_text", Text: req.System}
			if !opts.disablePromptCacheBreakpoints && promptCacheBreakpointsEnabled(req.Model, opts.baseURL, opts.promptCache) {
				part.PromptCacheBreakpoint = &wirePromptCacheBreakpoint{Mode: "explicit"}
			}
			out = append(out, wireInputItem{Type: "message", Role: "developer", Content: []wireContentPart{part}})
		}
		if len(event.Removed) > 0 {
			// JSON quoting keeps names unambiguous and avoids treating a name as
			// instructions. Removal is advisory; the agent enforces availability.
			names, _ := json.Marshal(event.Removed)
			out = append(out, wireInputItem{Type: "message", Role: "developer", Content: []wireContentPart{{
				Type: "input_text", Text: "These tools are no longer available; do not call them: " + string(names),
			}}})
		}
	}
	start := 0
	for i, message := range req.Messages {
		event := message.ToolContext
		if event != nil && !event.After {
			add(event)
		}
		out = append(out, input[start:ends[i]]...)
		if event != nil && event.After {
			add(event)
		}
		messageEnds = append(messageEnds, len(out))
		start = ends[i]
	}
	return out, messageEnds
}
