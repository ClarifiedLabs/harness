// Package tools defines the Tool interface, an ordered registry, and a Dispatch
// entry point that turns every failure mode (unknown tool, invalid arguments,
// tool error, tool panic) into an is_error result and caps oversized output.
// Tools resolve relative paths against the process cwd; there are no path
// restrictions, in keeping with the harness's no-sandbox stance (design §2, §9).
package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"maps"
	"slices"
	"strings"
	"time"

	"harness/internal/llm"
)

// Tool is one model-callable capability. Schema is hand-written JSON Schema for
// the input object; Run decodes input into its own typed struct and self-validates.
type Tool interface {
	Name() string
	Description() string     // model-facing, one line
	Schema() json.RawMessage // JSON Schema for the input object
	// ReadOnly reports whether Run with this input mutates workspace or repo
	// state, so independent read-only calls may dispatch concurrently (spec §8).
	ReadOnly(input json.RawMessage) bool
	Run(ctx context.Context, input json.RawMessage) (string, error)
}

// MeteredResult is returned by tools that consume model tokens internally.
// Dispatch preserves Usage so the agent can include it in turn/session totals.
type MeteredResult struct {
	Text  string
	Usage llm.Usage
}

// MeteredTool is an optional extension for tools whose Run implementation can
// report additional token usage. Tools still implement Tool.Run for ordinary
// callers; Dispatch prefers RunMetered when present.
type MeteredTool interface {
	RunMetered(ctx context.Context, input json.RawMessage) (MeteredResult, error)
}

// FileMutationReporter is implemented by tools that can identify the file paths
// they may mutate from their JSON input. The agent uses this for optional
// user-facing before/after diff display; Dispatch and model-visible results do
// not depend on it.
type FileMutationReporter interface {
	MutatedPaths(input json.RawMessage) ([]string, error)
}

// BackgroundJobRequest is the reusable contract for tools that can hand work to
// the process-local background job manager. The manager owns job ids, status,
// cancellation, notices, and request-context delivery; the tool owns its input
// validation and execution semantics.
type BackgroundJobRequest struct {
	Kind        string
	Description string
	Run         func(context.Context, string) (BackgroundJobResult, error)
}

// BackgroundJobResult is the model-facing outcome of a completed background
// tool job. TranscriptPath is for jobs, such as delegate agents, that persist a
// separate transcript.
type BackgroundJobResult struct {
	Text           string
	TranscriptPath string
}

// BackgroundJobInfo is the minimal start acknowledgement a tool needs to return
// immediately after queueing a background job.
type BackgroundJobInfo struct {
	ID     string
	Status string
}

// BackgroundJobStarter is implemented by the background job manager and injected
// into tools that opt into background execution.
type BackgroundJobStarter interface {
	StartBackgroundJob(BackgroundJobRequest) (BackgroundJobInfo, error)
}

// Registry is an ordered set of tools. Order is preserved so Specs and the
// model-facing tool list are stable across runs.
type Registry struct {
	order           []string
	tools           map[string]Tool
	dispatchTimeout time.Duration // zero = no dispatch-level timeout
	resultLimits    resultLimits
}

// Options configures a tool registry. Zero values keep package defaults.
type Options struct {
	MaxResultBytes       int
	MaxResultLines       int
	ReadFileDefaultLimit int
	Background           BackgroundJobStarter
	SearchTools          string
	// DispatchTimeout is the per-call ceiling applied by Dispatch (zero = none).
	// It backstops tools that ignore ctx (e.g. a hung MCP/web_fetch/lsp call) so
	// one stuck call cannot stall a turn forever. A tool that enforces its own
	// longer deadline (see SelfTimeouter) is never cut below it.
	DispatchTimeout time.Duration
}

// SelfTimeouter is an optional Tool extension. A tool that enforces its own
// per-call deadline reports it here so the Dispatch-level ceiling only ever
// RAISES to that deadline, never lowers it. This preserves run_command's
// documented "no maximum" (its own timeout_seconds stays authoritative) while
// the ceiling still bounds tools that ignore ctx (design §8.2). ok is false when
// the tool has no input-specific deadline.
type SelfTimeouter interface {
	SelfTimeout(input json.RawMessage) (timeout time.Duration, ok bool)
}

// DisabledTool describes an optional built-in tool that was not registered.
type DisabledTool struct {
	Name   string
	Reason string
}

// Message renders a concise user-facing disabled-tool diagnostic.
func (d DisabledTool) Message() string {
	return fmt.Sprintf("Tool %q is disabled. Reason: %s.", d.Name, d.Reason)
}

func missingBinaryTool(name, binary string) DisabledTool {
	return DisabledTool{Name: name, Reason: fmt.Sprintf("%q binary not found", binary)}
}

// SetDispatchTimeout overrides the optional per-call ceiling applied by Dispatch.
// Non-positive values disable the dispatch-level timeout.
func (r *Registry) SetDispatchTimeout(d time.Duration) { r.dispatchTimeout = d }

// SetResultLimits overrides the central tool-result truncation caps. Non-positive
// fields keep their defaults.
func (r *Registry) SetResultLimits(maxBytes, maxLines int) {
	r.resultLimits = resultLimits{maxBytes: maxBytes, maxLines: maxLines}
}

// RegisterFileTools registers the built-in file tools (read_file, list_dir,
// glob, grep, optional rg, edit, write_file) on r, in that order. It is the only
// exported path to these tools; their types are unexported by design. apply_patch
// is intentionally not here — it ships only in the constructible Catalog (see
// CatalogWithOptions) since edit+write_file subsume it.
func RegisterFileTools(r *Registry) {
	registerFileTools(r, nil, Options{})
}

func registerFileTools(r *Registry, disabled *[]DisabledTool, opts Options) {
	r.Register(readFile{defaultLimit: opts.ReadFileDefaultLimit})
	r.Register(listDir{})
	r.Register(glob{})
	registerSearchTools(r, disabled, opts)
	r.Register(edit{})
	r.Register(writeFile{})
}

const (
	SearchToolsAuto = "auto"
	SearchToolsGrep = "grep"
	SearchToolsRG   = "rg"
	SearchToolsBoth = "both"
)

func registerSearchTools(r *Registry, disabled *[]DisabledTool, opts Options) {
	mode := normalizeSearchTools(opts.SearchTools)
	rg, hasRG := newRipgrep(opts.Background)
	addGrep := mode == SearchToolsGrep || mode == SearchToolsBoth || (mode == SearchToolsAuto && !hasRG) || (mode == SearchToolsRG && !hasRG)
	addRG := hasRG && (mode == SearchToolsRG || mode == SearchToolsBoth || mode == SearchToolsAuto)
	if addGrep {
		// In "both" mode grep and rg ship side by side with near-identical schemas;
		// steer the model to rg so it converges on one tool.
		r.Register(grep{background: opts.Background, preferRG: mode == SearchToolsBoth && addRG})
	}
	if addRG {
		r.Register(rg)
	} else if (mode == SearchToolsRG || mode == SearchToolsBoth) && !hasRG && disabled != nil {
		*disabled = append(*disabled, missingBinaryTool("rg", "rg"))
	}
}

func normalizeSearchTools(mode string) string {
	switch strings.ToLower(strings.TrimSpace(mode)) {
	case "", SearchToolsAuto:
		return SearchToolsAuto
	case SearchToolsGrep:
		return SearchToolsGrep
	case SearchToolsRG, "ripgrep":
		return SearchToolsRG
	case SearchToolsBoth:
		return SearchToolsBoth
	default:
		return SearchToolsAuto
	}
}

// RegisterExecTools registers the process/network tools (run_command, git,
// web_fetch) on r, in that order. It is the only exported path to these tools;
// their types are unexported by design.
func RegisterExecTools(r *Registry) {
	registerExecTools(r, nil, Options{})
}

func registerExecTools(r *Registry, disabled *[]DisabledTool, opts Options) {
	r.Register(runCommand{background: opts.Background})
	if git, ok := newGitTool(); ok {
		r.Register(git)
	} else if disabled != nil {
		*disabled = append(*disabled, missingBinaryTool("git", "git"))
	}
	r.Register(webFetch{background: opts.Background})
}

// Default returns a Registry preloaded with every built-in tool.
func Default() *Registry {
	r, _ := DefaultWithDiagnostics()
	return r
}

// DefaultWithDiagnostics returns the default tool registry plus diagnostics for
// optional tools that were not registered.
func DefaultWithDiagnostics() (*Registry, []DisabledTool) {
	return DefaultWithOptions(Options{})
}

// DefaultWithOptions returns the default tool registry with configurable result
// and read_file limits.
func DefaultWithOptions(opts Options) (*Registry, []DisabledTool) {
	r := &Registry{}
	r.SetResultLimits(opts.MaxResultBytes, opts.MaxResultLines)
	r.SetDispatchTimeout(opts.DispatchTimeout)
	var disabled []DisabledTool
	registerFileTools(r, &disabled, opts)
	registerExecTools(r, &disabled, opts)
	return r, disabled
}

// DefaultNames returns the names of the Default tool set in registration
// order. Agent definitions use it as the baseline allowed-tool list.
func DefaultNames() []string { return Default().Names() }

func DefaultNamesWithOptions(opts Options) []string {
	r, _ := DefaultWithOptions(opts)
	return r.Names()
}

// Catalog returns a Registry with every constructible tool: the Default set
// plus the agent-oriented tools (apply_patch, git_readonly, write_tmp_file), which
// agent definitions select from by name. Build it once per process — write_tmp_file
// holds the per-run temp directory.
func Catalog() *Registry {
	r, _ := CatalogWithDiagnostics()
	return r
}

// CatalogWithDiagnostics returns the complete constructible tool catalog plus
// diagnostics for optional tools that were not registered.
func CatalogWithDiagnostics() (*Registry, []DisabledTool) {
	return CatalogWithOptions(Options{})
}

// CatalogWithOptions returns the complete constructible tool catalog with
// configurable limits.
func CatalogWithOptions(opts Options) (*Registry, []DisabledTool) {
	r, disabled := DefaultWithOptions(opts)
	// apply_patch overlaps edit+write_file, so it is kept out of the default
	// request and registered only here, where agents may still whitelist it by
	// name. This auto-drops it from auto/independent allowed lists derived from
	// DefaultNamesWithOptions, which is intended.
	r.Register(applyPatch{})
	if git, ok := newGitReadonly(); ok {
		r.Register(git)
	} else {
		disabled = append(disabled, missingBinaryTool("git_readonly", "git"))
	}
	r.Register(newWriteTmpFile())
	return r, disabled
}

// Names returns the registered tool names in registration order.
func (r *Registry) Names() []string {
	return append([]string(nil), r.order...)
}

// Subset returns a new Registry containing exactly the named tools, in this
// registry's order. Unknown names are an error so a config typo fails fast
// instead of silently dropping a tool.
func (r *Registry) Subset(names []string) (*Registry, error) {
	want := make(map[string]bool, len(names))
	for _, name := range names {
		want[name] = true
	}
	sub := &Registry{resultLimits: r.resultLimits, dispatchTimeout: r.dispatchTimeout}
	for _, name := range r.order {
		if want[name] {
			sub.Register(r.tools[name])
			delete(want, name)
		}
	}
	if len(want) > 0 {
		unknown := slices.Sorted(maps.Keys(want))
		return nil, fmt.Errorf("unknown tools: %s (valid tools: %s)",
			strings.Join(unknown, ", "), strings.Join(r.Names(), ", "))
	}
	return sub, nil
}

// Register adds a tool. A later registration with the same name replaces the
// earlier one but keeps its position in the order.
func (r *Registry) Register(t Tool) {
	if r.tools == nil {
		r.tools = make(map[string]Tool)
	}
	name := t.Name()
	if _, ok := r.tools[name]; !ok {
		r.order = append(r.order, name)
	}
	r.tools[name] = t
}

// Lookup returns the registered tool by name. The returned tool is the concrete
// instance stored in the registry; callers must not mutate shared tool state
// unless they own that instance.
func (r *Registry) Lookup(name string) (Tool, bool) {
	if r == nil || r.tools == nil {
		return nil, false
	}
	t, ok := r.tools[name]
	return t, ok
}

// Remove deletes the named tool from the registry, dropping it from both the
// lookup map and the order slice. It reports whether a tool was removed; an
// absent name is a no-op returning false. The MCP prompt-boundary refresh uses
// it to drop tools that vanish from the proxy between list_changed events.
func (r *Registry) Remove(name string) bool {
	if _, ok := r.tools[name]; !ok {
		return false
	}
	delete(r.tools, name)
	if i := slices.Index(r.order, name); i >= 0 {
		r.order = slices.Delete(r.order, i, i+1)
	}
	return true
}

// Specs returns the registered tools' schemas in registration order.
func (r *Registry) Specs() []llm.ToolSchema {
	specs := make([]llm.ToolSchema, 0, len(r.order))
	for _, name := range r.order {
		t := r.tools[name]
		specs = append(specs, llm.ToolSchema{
			Name:        t.Name(),
			Description: t.Description(),
			Parameters:  modelSchema(t.Schema()),
		})
	}
	return specs
}

func modelSchema(raw json.RawMessage) json.RawMessage {
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return compactSchema(raw)
	}
	stripSchemaDescriptions(v)
	b, err := json.Marshal(v)
	if err != nil {
		return compactSchema(raw)
	}
	return json.RawMessage(b)
}

func compactSchema(raw json.RawMessage) json.RawMessage {
	var b bytes.Buffer
	if err := json.Compact(&b, raw); err != nil {
		return raw
	}
	return json.RawMessage(b.Bytes())
}

func stripSchemaDescriptions(v any) {
	switch x := v.(type) {
	case map[string]any:
		delete(x, "description")
		for _, child := range x {
			stripSchemaDescriptions(child)
		}
	case []any:
		for _, child := range x {
			stripSchemaDescriptions(child)
		}
	}
}

// CallReadOnly reports whether one call resolves to a read-only tool invocation.
// Unknown names count as not read-only: they dispatch to an error result, and
// serializing them is the conservative choice.
func (r *Registry) CallReadOnly(call llm.ToolCall) bool {
	t, ok := r.tools[call.Name]
	if !ok {
		return false
	}
	input := call.Input
	if len(input) == 0 {
		input = json.RawMessage("{}")
	}
	return t.ReadOnly(input)
}

// AllReadOnly reports whether every call resolves to a read-only invocation.
func (r *Registry) AllReadOnly(calls []llm.ToolCall) bool {
	for _, c := range calls {
		if !r.CallReadOnly(c) {
			return false
		}
	}
	return true
}

// MutatedPaths reports the file paths a call may mutate when its tool provides
// that metadata. Unknown tools, non-reporting tools, and invalid inputs return
// ok=false so callers can silently skip optional observers.
func (r *Registry) MutatedPaths(call llm.ToolCall) (paths []string, ok bool) {
	t, found := r.tools[call.Name]
	if !found {
		return nil, false
	}
	reporter, ok := t.(FileMutationReporter)
	if !ok {
		return nil, false
	}
	input := call.Input
	if len(input) == 0 {
		input = json.RawMessage("{}")
	}
	paths, err := reporter.MutatedPaths(input)
	if err != nil || len(paths) == 0 {
		return nil, false
	}
	return uniqueMutationPaths(paths), true
}

func uniqueMutationPaths(paths []string) []string {
	out := make([]string, 0, len(paths))
	seen := make(map[string]bool, len(paths))
	for _, path := range paths {
		if strings.TrimSpace(path) == "" {
			continue
		}
		key := duplicatePathKey(path)
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, path)
	}
	return out
}

// Dispatch runs one tool call and always returns a result (design §8.2). It
// runs Tool.Run in a goroutine, recovers
// panics (inside that goroutine), maps unknown tools and decode/run errors to
// is_error result strings, and applies the central output cap (design §8.3).
// When SetDispatchTimeout has configured a positive ceiling, expiry returns a
// timeout is_error result even for a tool that ignores its context; an outer
// cancellation is reported as cancellation, not a dispatch timeout.
func (r *Registry) Dispatch(parent context.Context, call llm.ToolCall) (res llm.ToolResult) {
	res.ForID = call.ID

	t, ok := r.tools[call.Name]
	if !ok {
		res.Text = fmt.Sprintf("error: unknown tool %q", call.Name)
		res.IsError = true
		return res
	}

	input := call.Input
	if len(input) == 0 {
		input = json.RawMessage("{}")
	}

	timeout := r.dispatchTimeout
	if timeout > 0 {
		// A tool with its own (possibly longer) deadline must not be cut below it;
		// the ceiling only raises, never lowers.
		if st, ok := t.(SelfTimeouter); ok {
			if d, has := st.SelfTimeout(input); has && d > timeout {
				timeout = d
			}
		}
	}
	var ctx context.Context
	var cancel context.CancelFunc
	if timeout > 0 {
		ctx, cancel = context.WithTimeout(parent, timeout)
	} else {
		ctx, cancel = context.WithCancel(parent)
	}
	defer cancel()

	type outcome struct {
		out   string
		usage llm.Usage
		err   error
	}
	done := make(chan outcome, 1) // buffered: an abandoned Run can still send and exit
	go func() {
		defer func() {
			if rec := recover(); rec != nil {
				log.Printf("tool %q panicked: %v", call.Name, rec)
				done <- outcome{err: fmt.Errorf("tool panicked: %v", rec)}
			}
		}()
		if mt, ok := t.(MeteredTool); ok {
			result, err := mt.RunMetered(ctx, input)
			done <- outcome{out: result.Text, usage: result.Usage, err: err}
			return
		}
		out, err := t.Run(ctx, input)
		done <- outcome{out: out, err: err}
	}()

	var out string
	var usage llm.Usage
	var err error
	select {
	case o := <-done:
		out, usage, err = o.out, o.usage, o.err
	case <-ctx.Done():
		// The Run goroutine is abandoned if it ignores ctx; its eventual send
		// lands in the buffered channel and is dropped. The abandoned Run may
		// still mutate external state (write files, leave a subprocess running)
		// after we return, so built-in long-running tools are expected to honor
		// ctx and apply their own user-configurable timeouts.
		if parent.Err() != nil {
			res.Text = "error: " + parent.Err().Error()
		} else if timeout > 0 {
			res.Text = fmt.Sprintf("error: tool timed out after %s", timeout)
		} else {
			res.Text = "error: " + ctx.Err().Error()
		}
		res.IsError = true
		return res
	}

	res.Usage = usage
	if err != nil {
		// Report a timeout only when the ceiling itself expired (the derived
		// context's deadline fired) and it was not an outer cancellation. A
		// tool's own internal deadline (e.g. http.Client.Timeout) also yields
		// a DeadlineExceeded error, but with the ceiling unfired it must pass
		// through as a plain tool error — not be relabeled as a dispatch
		// timeout with the wrong duration (spec §6).
		if timeout > 0 && ctx.Err() == context.DeadlineExceeded && parent.Err() == nil {
			res.Text = fmt.Sprintf("error: tool timed out after %s", timeout)
		} else if _, bad := err.(*invalidArgsError); bad || isJSONError(err) {
			res.Text = "error: invalid arguments: " + err.Error()
		} else {
			res.Text = "error: " + err.Error()
		}
		res.IsError = true
		return res
	}

	var info truncationInfo
	res.Text, info = truncate(out, r.resultLimits)
	if info.truncated {
		res.Truncated = true
		res.OriginalText = out
		res.OriginalBytes = info.originalBytes
		res.ShownBytes = info.shownBytes
	}
	return res
}

// invalidArgsError marks a validation failure a tool raises after decoding;
// Dispatch renders it under the "invalid arguments" prefix.
type invalidArgsError struct{ msg string }

func (e *invalidArgsError) Error() string { return e.msg }

func badArgs(format string, a ...any) error {
	return &invalidArgsError{msg: fmt.Sprintf(format, a...)}
}

// isJSONError reports whether err originates from encoding/json decoding, so a
// tool's failed json.Unmarshal surfaces as an "invalid arguments" result.
func isJSONError(err error) bool {
	switch err.(type) {
	case *json.SyntaxError, *json.UnmarshalTypeError:
		return true
	}
	return false
}
