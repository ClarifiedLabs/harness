package goal

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
)

const createSchema = `{
  "type": "object",
  "additionalProperties": false,
  "properties": {
    "objective": {
      "type": "string",
      "description": "A single clear, verifiable objective for the agent to pursue autonomously across multiple turns. Example: \"Fix the three typos in README.md and add a regression test.\""
    }
  },
  "required": ["objective"]
}`

type bindingContextKey struct{}

type toolBinding struct {
	mu         sync.Mutex
	store      *Store
	generation uint64
}

// WithGeneration binds goal-tool calls descended from ctx to one goal/session
// generation. The binding is mutable only so a successful create_goal call can
// target that newly created goal from later tool rounds in the same prompt.
func WithGeneration(ctx context.Context, store *Store, generation uint64) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithValue(ctx, bindingContextKey{}, &toolBinding{store: store, generation: generation})
}

func bindingFor(ctx context.Context, store *Store) (*toolBinding, uint64, bool) {
	if ctx == nil {
		return nil, 0, false
	}
	binding, ok := ctx.Value(bindingContextKey{}).(*toolBinding)
	if !ok || binding == nil || binding.store != store {
		return nil, 0, false
	}
	binding.mu.Lock()
	defer binding.mu.Unlock()
	return binding, binding.generation, true
}

// GenerationFromContext returns the current prompt-local goal generation. A
// successful create_goal call advances this value for the rest of the prompt.
func GenerationFromContext(ctx context.Context, store *Store) (uint64, bool) {
	_, generation, ok := bindingFor(ctx, store)
	return generation, ok
}

func (b *toolBinding) advance(generation uint64) {
	b.mu.Lock()
	b.generation = generation
	b.mu.Unlock()
}

const updateSchema = `{
  "type": "object",
  "additionalProperties": false,
  "properties": {
    "status": {
      "type": "string",
      "enum": ["complete", "blocked"],
      "description": "complete only when the objective is verifiably achieved; blocked only when the same blocking condition has recurred for at least three consecutive goal turns."
    }
  },
  "required": ["status"]
}`

// CreateTool is the model-callable create_goal tool.
type CreateTool struct {
	store       *Store
	interactive bool
}

// NewCreateTool returns a create_goal tool backed by store. interactive gates
// whether goals may be created (they are a REPL-only feature).
func NewCreateTool(store *Store, interactive bool) *CreateTool {
	return &CreateTool{store: store, interactive: interactive}
}

func (*CreateTool) Name() string                  { return "create_goal" }
func (*CreateTool) Schema() json.RawMessage       { return json.RawMessage(createSchema) }
func (*CreateTool) ReadOnly(json.RawMessage) bool { return false }

func (t *CreateTool) Description() string {
	return "Create a session goal for autonomous multi-turn pursuit. Only call when the user explicitly asks for a long-running goal. Fails if an unfinished goal already exists; use update_goal to mark the current goal complete or blocked. Goal management belongs to the root conversation, not child agents."
}

func (t *CreateTool) Run(ctx context.Context, input json.RawMessage) (string, error) {
	if !t.interactive {
		return "", fmt.Errorf("goals are only available in interactive sessions")
	}
	var args struct {
		Objective string `json:"objective"`
	}
	if err := json.Unmarshal(input, &args); err != nil {
		return "", err
	}
	if binding, generation, ok := bindingFor(ctx, t.store); ok {
		next, err := t.store.CreateAtGeneration(args.Objective, generation)
		if err != nil {
			return "", err
		}
		binding.advance(next)
	} else if err := t.store.Create(args.Objective); err != nil {
		return "", err
	}
	return "Goal created. It will be pursued automatically at the next prompt boundary.", nil
}

// UpdateTool is the model-callable update_goal tool.
type UpdateTool struct {
	store       *Store
	interactive bool
}

// NewUpdateTool returns an update_goal tool backed by store.
func NewUpdateTool(store *Store, interactive bool) *UpdateTool {
	return &UpdateTool{store: store, interactive: interactive}
}

func (*UpdateTool) Name() string                  { return "update_goal" }
func (*UpdateTool) Schema() json.RawMessage       { return json.RawMessage(updateSchema) }
func (*UpdateTool) ReadOnly(json.RawMessage) bool { return false }

func (t *UpdateTool) Description() string {
	return "Update the active session goal. Call only when the objective is verifiably achieved or genuinely blocked. status 'complete' is only valid when you can cite concrete evidence that every part of the objective is satisfied. status 'blocked' is only valid when the same blocking condition has recurred for at least three consecutive goal turns and remains unresolved despite your best effort."
}

func (t *UpdateTool) Run(ctx context.Context, input json.RawMessage) (string, error) {
	if !t.interactive {
		return "", fmt.Errorf("goals are only available in interactive sessions")
	}
	var args struct {
		Status string `json:"status"`
	}
	if err := json.Unmarshal(input, &args); err != nil {
		return "", err
	}
	status := Status(strings.ToLower(strings.TrimSpace(args.Status)))
	if _, generation, ok := bindingFor(ctx, t.store); ok {
		if err := t.store.MarkStatusAtGeneration(status, generation); err != nil {
			return "", err
		}
	} else if err := t.store.MarkStatus(status); err != nil {
		return "", err
	}
	switch status {
	case StatusComplete:
		return "Goal marked complete. Autonomous continuation will stop.", nil
	case StatusBlocked:
		return "Goal marked blocked. Autonomous continuation will stop; the user can /goal resume to start a fresh audit.", nil
	default:
		return "", fmt.Errorf("invalid goal status %q (want complete or blocked)", args.Status)
	}
}
