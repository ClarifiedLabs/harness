package llm

import (
	"fmt"
	"strings"
	"time"
)

// ToolContext records a replay-domain-scoped change to the ordinary tool catalog,
// not general instructions (which remain on Request.System). Initial events
// carry the complete current catalog, including an empty catalog. Other events
// add or redefine Tools and remove the names in Removed. After places the event
// after the message's content on the wire; otherwise it precedes the content.
// Events may be attached to either user or assistant messages.
type ToolContext struct {
	ReplayDomain string       `json:"replay_domain"`
	Initial      bool         `json:"initial,omitempty"`
	After        bool         `json:"after,omitempty"`
	Tools        []ToolSchema `json:"tools,omitempty"`
	Removed      []string     `json:"removed,omitempty"`
}

// SupportsIncrementalTools reports support only for known models on the canonical
// OpenAI API. Compatible APIs and Codex are not assumed to support this protocol.
func SupportsIncrementalTools(model, baseURL string) bool {
	baseURL = strings.TrimSuffix(strings.ToLower(strings.TrimSpace(baseURL)), "/")
	if baseURL != "https://api.openai.com/v1" {
		return false
	}
	model = strings.ToLower(strings.TrimSpace(model))
	model = strings.TrimSpace(strings.TrimPrefix(model, "openai:"))
	// Sol currently has only this documented model ID, not dated snapshots.
	if model == "gpt-6.1-sol" {
		return true
	}
	for _, family := range []string{"gpt-6-astra", "gpt-5.6"} {
		if model == family {
			return true
		}
		if suffix, ok := strings.CutPrefix(model, family+"-"); ok && len(suffix) == len("2006-01-02") {
			_, err := time.Parse("2006-01-02", suffix)
			return err == nil
		}
	}
	return false
}

// IncrementalToolsEnabled resolves the provider default without widening the
// supported model/endpoint gate. Callers must also exclude Codex profiles and
// incompatible request shapes.
func (p PromptCacheConfig) IncrementalToolsEnabled(model, baseURL string) bool {
	return (p.IncrementalTools == nil || *p.IncrementalTools) && SupportsIncrementalTools(model, baseURL)
}

// validateToolContext validates one event, not the historical catalog baseline:
// continuation deltas and transcript slices need not contain an initial event.
func validateToolContext(context *ToolContext) error {
	if context == nil {
		return nil
	}
	if strings.TrimSpace(context.ReplayDomain) == "" {
		return fmt.Errorf("replay domain is empty")
	}
	if context.Initial && len(context.Removed) != 0 {
		return fmt.Errorf("initial event cannot remove tools")
	}
	added := make(map[string]bool, len(context.Tools))
	for i, tool := range context.Tools {
		if strings.TrimSpace(tool.Name) == "" {
			return fmt.Errorf("tool %d has an empty name", i)
		}
		if added[tool.Name] {
			return fmt.Errorf("duplicate tool name %q", tool.Name)
		}
		added[tool.Name] = true
		if len(tool.Parameters) > 0 {
			if err := ValidateToolInputObject(tool.Parameters); err != nil {
				return fmt.Errorf("tool %q parameters: %w", tool.Name, err)
			}
		}
	}
	removed := make(map[string]bool, len(context.Removed))
	for i, name := range context.Removed {
		if strings.TrimSpace(name) == "" {
			return fmt.Errorf("removed tool %d has an empty name", i)
		}
		if removed[name] {
			return fmt.Errorf("duplicate removed tool name %q", name)
		}
		if added[name] {
			return fmt.Errorf("tool %q is both added and removed", name)
		}
		removed[name] = true
	}
	return nil
}
