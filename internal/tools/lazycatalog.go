package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"sync"
)

// ToolCatalogName is the explicit discovery tool installed when optional MCP
// and LSP schemas exceed the prompt budget.
const ToolCatalogName = "tool_catalog"

const lazyCatalogSchema = `{
  "type": "object",
  "properties": {
    "action": {"type": "string", "enum": ["list", "describe", "activate"], "description": "Default list. Activate makes tools native on the next model turn."},
    "query": {"type": "string", "description": "Case-insensitive name/description filter for list."},
    "names": {"type": "array", "items": {"type": "string"}, "minItems": 1, "maxItems": 16, "uniqueItems": true, "description": "Optional-tool names for describe or activate."},
    "limit": {"type": "integer", "minimum": 1, "maximum": 100, "description": "List cap; default 50."}
  },
  "additionalProperties": false
}`

type lazyCatalogState struct {
	mu       sync.Mutex
	registry *Registry
	order    []string
	allowed  map[string]bool
	active   map[string]bool
	previous func(string) bool
}

// EnableLazyToolSpecs hides candidate schemas behind tool_catalog when their
// aggregate model-facing declaration size exceeds thresholdBytes. Dispatch is
// unchanged: this is prompt-size optimization, not an authorization boundary.
// Activation is local to r and persists for its lifetime. It returns true when
// virtualization was installed.
func EnableLazyToolSpecs(r *Registry, candidates []string, thresholdBytes int) bool {
	if r == nil || thresholdBytes <= 0 || len(candidates) == 0 {
		return false
	}
	if _, exists := r.Lookup(ToolCatalogName); exists {
		return false
	}
	candidateSet := make(map[string]bool, len(candidates))
	for _, name := range candidates {
		if name != ToolCatalogName {
			candidateSet[name] = true
		}
	}
	var order []string
	total := 0
	for _, spec := range r.Specs() {
		if !candidateSet[spec.Name] {
			continue
		}
		order = append(order, spec.Name)
		total += len(spec.Name) + len(spec.Description) + len(spec.Parameters)
	}
	if len(order) == 0 || total <= thresholdBytes {
		return false
	}
	state := &lazyCatalogState{
		registry: r,
		order:    order,
		allowed:  make(map[string]bool, len(order)),
		active:   make(map[string]bool),
		previous: r.specFilter,
	}
	for _, name := range order {
		state.allowed[name] = true
	}
	r.Register(&lazyCatalogTool{state: state})
	r.SetSpecFilter(func(name string) bool {
		if state.previous != nil && !state.previous(name) {
			return false
		}
		state.mu.Lock()
		defer state.mu.Unlock()
		return !state.allowed[name] || state.active[name]
	})
	return true
}

type lazyCatalogTool struct{ state *lazyCatalogState }

func (*lazyCatalogTool) Name() string { return ToolCatalogName }

func (*lazyCatalogTool) Description() string {
	return "List, inspect, or activate optional MCP/LSP tools before calling them directly."
}

func (*lazyCatalogTool) Schema() json.RawMessage { return json.RawMessage(lazyCatalogSchema) }

func (*lazyCatalogTool) PreserveSchemaDescriptions() bool { return true }

func (*lazyCatalogTool) ReadOnly(json.RawMessage) bool { return true }

func (*lazyCatalogTool) RequiresSequential(input json.RawMessage) bool {
	var args struct {
		Action string `json:"action"`
	}
	return json.Unmarshal(input, &args) == nil && strings.TrimSpace(args.Action) == "activate"
}

func (t *lazyCatalogTool) Run(_ context.Context, input json.RawMessage) (string, error) {
	var args struct {
		Action string   `json:"action"`
		Query  string   `json:"query"`
		Names  []string `json:"names"`
		Limit  int      `json:"limit"`
	}
	if err := json.Unmarshal(input, &args); err != nil {
		return "", err
	}
	action := strings.TrimSpace(args.Action)
	if action == "" {
		action = "list"
	}
	switch action {
	case "list":
		if len(args.Names) > 0 {
			return "", badArgs("names is only valid for describe or activate")
		}
		return t.list(args.Query, args.Limit), nil
	case "describe":
		return t.describe(args.Names)
	case "activate":
		return t.activate(args.Names)
	default:
		return "", badArgs("unknown action %q", action)
	}
}

func (t *lazyCatalogTool) list(query string, limit int) string {
	if limit <= 0 {
		limit = 50
	}
	query = strings.ToLower(strings.TrimSpace(query))
	var lines []string
	for _, name := range t.state.order {
		tool, ok := t.state.registry.Lookup(name)
		if !ok {
			continue
		}
		description := tool.Description()
		if query != "" && !strings.Contains(strings.ToLower(name+" "+description), query) {
			continue
		}
		state := "available"
		t.state.mu.Lock()
		if t.state.active[name] {
			state = "active"
		}
		t.state.mu.Unlock()
		lines = append(lines, fmt.Sprintf("%s\t%s\t%s", name, state, description))
		if len(lines) == limit {
			break
		}
	}
	if len(lines) == 0 {
		return "No optional tools matched."
	}
	return strings.Join(lines, "\n") + "\nUse action=activate with names before calling a listed tool directly."
}

func (t *lazyCatalogTool) checkedNames(names []string) ([]string, error) {
	if len(names) == 0 {
		return nil, badArgs("names is required")
	}
	if len(names) > 16 {
		return nil, badArgs("names must contain at most 16 tools")
	}
	seen := make(map[string]bool, len(names))
	out := make([]string, 0, len(names))
	for _, name := range names {
		name = strings.TrimSpace(name)
		if !t.state.allowed[name] {
			return nil, badArgs("unknown optional tool %q", name)
		}
		if !seen[name] {
			seen[name] = true
			out = append(out, name)
		}
	}
	return out, nil
}

func (t *lazyCatalogTool) describe(names []string) (string, error) {
	names, err := t.checkedNames(names)
	if err != nil {
		return "", err
	}
	type description struct {
		Name        string          `json:"name"`
		Description string          `json:"description,omitempty"`
		Parameters  json.RawMessage `json:"parameters"`
	}
	out := make([]description, 0, len(names))
	for _, name := range names {
		tool, _ := t.state.registry.Lookup(name)
		preserve := false
		if p, ok := tool.(SchemaDescriptionPreserver); ok {
			preserve = p.PreserveSchemaDescriptions()
		}
		out = append(out, description{
			Name: name, Description: tool.Description(),
			Parameters: modelSchemaWithExecutionMetadata(modelSchemaWithPolicy(tool.Schema(), preserve)),
		})
	}
	encoded, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		return "", err
	}
	return string(encoded), nil
}

func (t *lazyCatalogTool) activate(names []string) (string, error) {
	names, err := t.checkedNames(names)
	if err != nil {
		return "", err
	}
	t.state.mu.Lock()
	for _, name := range names {
		t.state.active[name] = true
	}
	t.state.mu.Unlock()
	slices.Sort(names)
	return "Activated optional tools for the next model turn: " + strings.Join(names, ", "), nil
}

var _ Tool = (*lazyCatalogTool)(nil)
var _ SequentialTool = (*lazyCatalogTool)(nil)
var _ SchemaDescriptionPreserver = (*lazyCatalogTool)(nil)
