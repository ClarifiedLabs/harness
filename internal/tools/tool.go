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
	"errors"
	"fmt"
	"log"
	"maps"
	"reflect"
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
// Dispatch preserves Usage so the agent can include it in prompt/session totals.
// Progress, when non-nil, is an opaque closure (func() agent.DelegateProgressSnapshot)
// built by the tool that reports the run's live activity for the parent wait
// ticker; it is consumed via type assertion by the renderer only. Keeping it
// `any` avoids a tools -> agent import cycle.
type MeteredResult struct {
	Text     string
	Usage    llm.Usage
	Progress any
}

// MeteredTool is an optional extension for tools whose Run implementation can
// report additional token usage. Tools still implement Tool.Run for ordinary
// callers; Dispatch prefers RunMetered when present.
type MeteredTool interface {
	RunMetered(ctx context.Context, input json.RawMessage) (MeteredResult, error)
}

// RunResult separates concise model-visible text from a complete original that
// should be archived for targeted recovery. Usage makes this a complete
// alternative dispatch path rather than an ambiguous mix with MeteredTool.
type RunResult struct {
	Text         string
	OriginalText string
	Usage        llm.Usage
	// Metrics is diagnostics-only aggregate telemetry. It is persisted with the
	// tool-result event but never enters model-visible transcript content.
	Metrics map[string]int
}

// ResultTool is an optional extension for tools that proactively summarize
// successful output. Dispatch prefers it over MeteredTool and Tool.Run.
type ResultTool interface {
	RunResult(ctx context.Context, input json.RawMessage) (RunResult, error)
}

// RichResult carries a tool's ordinary text plus shallow supplementary image
// content. Dispatch truncates only Text and preserves Usage.
type RichResult struct {
	Text    string
	Content []llm.ContentBlock
	Usage   llm.Usage
}

// RichTool is an optional extension for tools that can return supplementary
// image content. Dispatch prefers it over every legacy execution path and calls
// exactly one execution method.
type RichTool interface {
	RunRich(ctx context.Context, input json.RawMessage) (RichResult, error)
}

// RequiredInputModality is an optional proactive capability declaration. It is
// intentionally separate from RichTool because dynamically rich tools such as
// MCP may not know their result modality until after execution.
type RequiredInputModality interface {
	RequiredInputModality() string
}

// ProgressStarter is an optional capability for tools whose Run may block for a
// long time behind a child run (such as delegate) and want to surface live
// activity to the parent wait ticker. StartProgress returns an opaque closure
// (func() agent.DelegateProgressSnapshot) the renderer reads while the call is
// outstanding; it is nil if the tool does not support live progress for this
// input. The closure is created before the blocking Run, so it can report live
// state rather than only a final snapshot. Returning `any` keeps this package
// free of an agent import cycle.
type ProgressStarter interface {
	StartProgress(input json.RawMessage) any
}

// FileMutationReporter is implemented by tools that can identify the file paths
// they may mutate from their JSON input. The agent uses this for optional
// user-facing before/after diff display; Dispatch and model-visible results do
// not depend on it.
type FileMutationReporter interface {
	MutatedPaths(input json.RawMessage) ([]string, error)
}

// FileReadReporter is implemented by tools that can identify file paths they
// attempt to read from their JSON input. It reports call-level requested paths;
// Dispatch and model-visible results do not depend on it.
type FileReadReporter interface {
	ReadPaths(input json.RawMessage) ([]string, error)
}

// BackgroundJobRequest is the reusable contract for tools that can hand work to
// the process-local background job manager. The manager owns job ids, status,
// cancellation, notices, and request-context delivery; the tool owns its input
// validation and execution semantics. Progress, when non-nil, is the opaque
// live-progress closure (func() agent.DelegateProgressSnapshot) the job should
// report while running; the manager stores it on the job so the parent wait
// ticker can read it mid-run.
type BackgroundJobRequest struct {
	Kind        string
	Description string
	Agent       string
	ResourceKey string
	Access      string
	// WaitForPrompt marks work whose result must be incorporated before the parent
	// agent may finish its current prompt. Ordinary background commands leave this
	// false; background delegates set it so the parent joins and synthesizes them.
	WaitForPrompt bool
	Progress      any
	Run           func(context.Context, string) (BackgroundJobResult, error)
}

// BackgroundJobResult is the model-facing outcome of a completed background
// tool job. TranscriptPath is for jobs, such as delegate agents, that persist a
// separate transcript. Usage carries nested model spend back to the parent prompt.
// Progress, when non-nil, is an opaque closure (func() agent.DelegateProgressSnapshot)
// reporting the job's live activity while it runs; it is consumed via type
// assertion by the renderer only. Keeping it `any` avoids a tools -> agent cycle.
type BackgroundJobResult struct {
	Text           string
	OriginalText   string
	TranscriptPath string
	Usage          llm.Usage
	Progress       any
}

// BackgroundJobInfo is the minimal start acknowledgement a tool needs to return
// immediately after queueing a background job.
type BackgroundJobInfo struct {
	ID          string
	Status      string
	ResourceKey string
	Access      string
}

// BackgroundJobStarter is implemented by the background job manager and injected
// into tools that opt into background execution.
type BackgroundJobStarter interface {
	StartBackgroundJob(BackgroundJobRequest) (BackgroundJobInfo, error)
}

// SchemaDescriptionPreserver is an optional tool capability for the rare case
// where JSON-schema field descriptions carry essential dynamic model-facing
// metadata. Most tools omit it so Registry.Specs strips those descriptions.
type SchemaDescriptionPreserver interface {
	PreserveSchemaDescriptions() bool
}

// Registry is an ordered set of tools. Order is preserved so Specs and the
// model-facing tool list are stable across runs.
type Registry struct {
	order            []string
	tools            map[string]Tool
	dispatchGuard    func(llm.ToolCall, Activity) error
	specFilter       func(string) bool
	descSuffix       func(name, baseDesc string) string
	dispatchTimeout  time.Duration // zero = no dispatch-level timeout
	resultLimits     resultLimits
	toolResultLimits map[string]resultLimits
}

// SetDispatchGuard installs a dynamic semantic gate checked immediately before
// a known tool runs. It is intended for workflow-state constraints, not
// sandboxing or permissions; Tool.ReadOnly remains the dispatch authority.
func (r *Registry) SetDispatchGuard(guard func(llm.ToolCall, Activity) error) {
	r.dispatchGuard = guard
}

// SetSpecFilter installs a dynamic model-visibility filter. Dispatch remains
// independently guarded, so hiding a tool is an efficiency hint rather than an
// authorization boundary.
func (r *Registry) SetSpecFilter(filter func(string) bool) {
	r.specFilter = filter
}

// SetDescriptionSuffix installs a hook applied to tool descriptions at Specs()
// time. Tool.Description() stays static; the hook transforms the exposed spec
// description per tool name. Passing nil disables the hook. Because agent tool
// specs are cached from Specs() at registry rebuild boundaries, a hook closure
// reading live runtime state takes effect at the next rebuild without touching
// call sites.
func (r *Registry) SetDescriptionSuffix(f func(name, baseDesc string) string) {
	r.descSuffix = f
}

// Options configures a tool registry. Zero values keep package defaults.
type Options struct {
	MaxResultBytes       int
	MaxResultLines       int
	ReadFileDefaultLimit int
	ReadFileResultBytes  int
	ReadFileResultLines  int
	RGResultBytes        int
	RGResultLines        int
	GrepResultBytes      int
	GrepResultLines      int
	Background           BackgroundJobStarter
	// DispatchTimeout is the per-call ceiling applied by Dispatch (zero = none).
	// It backstops tools that ignore ctx (e.g. a hung MCP/web_fetch/lsp call) so
	// one stuck call cannot stall a turn forever. A tool that enforces its own
	// longer deadline (see SelfTimeouter) is never cut below it.
	DispatchTimeout                    time.Duration
	RunCommandTimeoutSeconds           int // 0 = tool default (120)
	RunCommandBackgroundTimeoutSeconds int // 0 = tool default (1200)
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

// SetToolResultLimits overrides result truncation caps for one tool. Positive
// fields override the corresponding global cap; non-positive fields inherit it.
func (r *Registry) SetToolResultLimits(toolName string, maxBytes, maxLines int) {
	toolName = strings.TrimSpace(toolName)
	if toolName == "" {
		return
	}
	if r.toolResultLimits == nil {
		r.toolResultLimits = map[string]resultLimits{}
	}
	r.toolResultLimits[toolName] = resultLimits{maxBytes: maxBytes, maxLines: maxLines}
}

func (r *Registry) resultLimitsFor(toolName string) resultLimits {
	limits := r.resultLimits.withDefaults()
	if r.toolResultLimits == nil {
		return limits
	}
	override, ok := r.toolResultLimits[toolName]
	if !ok {
		return limits
	}
	if override.maxBytes > 0 {
		limits.maxBytes = override.maxBytes
	}
	if override.maxLines > 0 {
		limits.maxLines = override.maxLines
	}
	return limits
}

// RegisterFileTools registers the built-in file tools (read_file, list_dir,
// glob, search, edit, write_file) on r, in that order. It is the only
// exported path to these tools; their types are unexported by design. apply_patch
// is intentionally not here — it ships only in the constructible Catalog (see
// CatalogWithOptions) since edit+write_file subsume it.
func RegisterFileTools(r *Registry) {
	registerFileTools(r, nil, Options{})
}

func registerFileTools(r *Registry, disabled *[]DisabledTool, opts Options) {
	r.Register(readFile{defaultLimit: opts.ReadFileDefaultLimit})
	r.SetToolResultLimits("read_file",
		defaultToolResultBytes(opts.ReadFileResultBytes, opts.ReadFileResultLines, opts.MaxResultBytes, opts.MaxResultLines, defaultReadFileResultBytes),
		defaultToolResultLines(opts.ReadFileResultBytes, opts.ReadFileResultLines, opts.MaxResultBytes, opts.MaxResultLines, 0))
	r.Register(viewImage{})
	r.Register(listDir{})
	r.Register(glob{})
	registerSearchTool(r, opts)
	r.Register(edit{})
	r.Register(writeFile{})
}

func registerSearchTool(r *Registry, opts Options) {
	rg, _ := newRipgrep(opts.Background)
	r.Register(searchTool{program: rg.program})
	r.SetToolResultLimits("search",
		defaultToolResultBytes(opts.RGResultBytes, opts.RGResultLines, opts.MaxResultBytes, opts.MaxResultLines, defaultTypedSearchBytes),
		defaultToolResultLines(opts.RGResultBytes, opts.RGResultLines, opts.MaxResultBytes, opts.MaxResultLines, defaultTypedSearchLines))
}

func registerRawSearchTools(r *Registry, opts Options) {
	r.Register(grep{background: opts.Background})
	r.SetToolResultLimits("grep",
		defaultToolResultBytes(opts.GrepResultBytes, opts.GrepResultLines, opts.MaxResultBytes, opts.MaxResultLines, defaultSearchResultBytes),
		defaultToolResultLines(opts.GrepResultBytes, opts.GrepResultLines, opts.MaxResultBytes, opts.MaxResultLines, defaultSearchResultLines))
	if rg, ok := newRipgrep(opts.Background); ok {
		r.Register(rg)
		r.SetToolResultLimits("rg",
			defaultToolResultBytes(opts.RGResultBytes, opts.RGResultLines, opts.MaxResultBytes, opts.MaxResultLines, defaultSearchResultBytes),
			defaultToolResultLines(opts.RGResultBytes, opts.RGResultLines, opts.MaxResultBytes, opts.MaxResultLines, defaultSearchResultLines))
	}
}

func defaultToolResultBytes(configBytes, configLines, globalBytes, globalLines, defaultBytes int) int {
	if configBytes > 0 {
		return configBytes
	}
	if configLines > 0 {
		return 0
	}
	if globalBytes > 0 || globalLines > 0 {
		return 0
	}
	return defaultBytes
}

func defaultToolResultLines(configBytes, configLines, globalBytes, globalLines, defaultLines int) int {
	if configLines > 0 {
		return configLines
	}
	if configBytes > 0 {
		return 0
	}
	if globalBytes > 0 || globalLines > 0 {
		return 0
	}
	return defaultLines
}

func registerExecTools(r *Registry, disabled *[]DisabledTool, opts Options) {
	r.Register(runCommand{
		background:        opts.Background,
		foregroundTimeout: opts.RunCommandTimeoutSeconds,
		backgroundTimeout: opts.RunCommandBackgroundTimeoutSeconds,
	})
	if git, ok := newGitTool(); ok {
		r.Register(git)
	} else if disabled != nil {
		*disabled = append(*disabled, missingBinaryTool("git", "git"))
	}
	r.Register(webFetch{background: opts.Background})
	r.SetToolResultLimits("web_fetch",
		defaultToolResultBytes(0, 0, opts.MaxResultBytes, opts.MaxResultLines, defaultSearchResultBytes),
		defaultToolResultLines(0, 0, opts.MaxResultBytes, opts.MaxResultLines, defaultSearchResultLines))
}

// Default returns a Registry preloaded with every built-in tool.
func Default() *Registry {
	r, _ := DefaultWithOptions(Options{})
	return r
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
	// Raw search commands remain constructible for custom agents that explicitly
	// whitelist them, but the default model surface exposes only typed search.
	registerRawSearchTools(r, opts)
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
	sub := &Registry{resultLimits: r.resultLimits, dispatchTimeout: r.dispatchTimeout, dispatchGuard: r.dispatchGuard, specFilter: r.specFilter, descSuffix: r.descSuffix}
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

// RegisterAfter adds a tool immediately after the already-registered afterName
// tool, so a related tool block can be anchored to a stable neighbor instead of
// trailing the whole registry. Re-registering an existing name replaces the tool
// in place (like Register) and keeps its current position. A missing anchor
// appends the tool at the end as a documented fallback.
func (r *Registry) RegisterAfter(t Tool, afterName string) {
	if r.tools == nil {
		r.tools = make(map[string]Tool)
	}
	name := t.Name()
	if _, ok := r.tools[name]; ok {
		r.tools[name] = t
		return
	}
	r.tools[name] = t
	if i := slices.Index(r.order, afterName); i >= 0 {
		r.order = slices.Insert(r.order, i+1, name)
		return
	}
	r.order = append(r.order, name)
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
		if r.specFilter != nil && !r.specFilter(name) {
			continue
		}
		t := r.tools[name]
		preserveDescriptions := false
		if preserver, ok := t.(SchemaDescriptionPreserver); ok {
			preserveDescriptions = preserver.PreserveSchemaDescriptions()
		}
		parameters := modelSchemaWithPolicy(t.Schema(), preserveDescriptions)
		desc := t.Description()
		if r.descSuffix != nil {
			desc = r.descSuffix(name, desc)
		}
		specs = append(specs, llm.ToolSchema{
			Name:        t.Name(),
			Description: desc,
			Parameters:  parameters,
		})
	}
	return specs
}

func modelSchema(raw json.RawMessage) json.RawMessage {
	return modelSchemaWithPolicy(raw, false)
}

func modelSchemaWithPolicy(raw json.RawMessage, preserveDescriptions bool) json.RawMessage {
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return compactSchema(raw)
	}
	stripSchemaAnnotations(v, preserveDescriptions)
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

var removableSchemaAnnotations = map[string]bool{
	"$comment":    true,
	"description": true,
	"example":     true,
	"examples":    true,
	"title":       true,
}

// stripSchemaAnnotations removes model-facing prose and examples from actual
// schema nodes without mistaking a property named "description" (or another
// annotation keyword) for an annotation. Validation, defaults, formats, and
// reference/dialect keywords remain intact.
func stripSchemaAnnotations(v any, preserveDescriptions bool) {
	schema, ok := v.(map[string]any)
	if !ok {
		return
	}
	for key := range removableSchemaAnnotations {
		if key != "description" || !preserveDescriptions {
			delete(schema, key)
		}
	}

	for keyword, child := range schema {
		switch keyword {
		case "$defs", "definitions", "dependentSchemas", "patternProperties", "properties":
			stripNamedSchemas(child, preserveDescriptions)
		case "allOf", "anyOf", "oneOf", "prefixItems":
			stripSchemaList(child, preserveDescriptions)
		case "additionalItems", "additionalProperties", "contains", "contentSchema", "else", "if", "items", "not", "propertyNames", "then", "unevaluatedItems", "unevaluatedProperties":
			stripSchemaOrList(child, preserveDescriptions)
		case "dependencies":
			// Legacy dependencies values are either property-name arrays or schemas.
			stripNamedSchemas(child, preserveDescriptions)
		}
	}
}

func stripSchemaOrList(v any, preserveDescriptions bool) {
	if _, ok := v.(map[string]any); ok {
		stripSchemaAnnotations(v, preserveDescriptions)
		return
	}
	stripSchemaList(v, preserveDescriptions)
}

func stripSchemaList(v any, preserveDescriptions bool) {
	schemas, ok := v.([]any)
	if !ok {
		return
	}
	for _, schema := range schemas {
		stripSchemaAnnotations(schema, preserveDescriptions)
	}
}

func stripNamedSchemas(v any, preserveDescriptions bool) {
	named, ok := v.(map[string]any)
	if !ok {
		return
	}
	for _, schema := range named {
		stripSchemaAnnotations(schema, preserveDescriptions)
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

// ReadPaths reports the requested file paths for a call when its tool provides
// that metadata. Unknown tools, non-reporting tools, and invalid inputs return
// ok=false so optional observers can silently skip them.
// RequiredModality reports a tool's proactive input-modality requirement.
// Unknown tools and modality-neutral tools return ok=false.
func (r *Registry) RequiredModality(call llm.ToolCall) (modality string, ok bool) {
	t, found := r.tools[call.Name]
	if !found {
		return "", false
	}
	required, ok := t.(RequiredInputModality)
	if !ok {
		return "", false
	}
	modality = strings.TrimSpace(required.RequiredInputModality())
	return modality, modality != ""
}

// ProgressFor reports a tool's live-progress closure when the tool implements
// ProgressStarter. Unknown tools and non-progressing tools return ok=false so
// optional observers can silently skip them. The returned `any` is an opaque
// func() agent.DelegateProgressSnapshot closure for the renderer to read.
func (r *Registry) ProgressFor(call llm.ToolCall) (progress any, ok bool) {
	t, found := r.tools[call.Name]
	if !found {
		return nil, false
	}
	starter, ok := t.(ProgressStarter)
	if !ok {
		return nil, false
	}
	input := call.Input
	if len(input) == 0 {
		input = json.RawMessage("{}")
	}
	progress = starter.StartProgress(input)
	return progress, progress != nil
}

func (r *Registry) ReadPaths(call llm.ToolCall) (paths []string, ok bool) {
	t, found := r.tools[call.Name]
	if !found {
		return nil, false
	}
	reporter, ok := t.(FileReadReporter)
	if !ok {
		return nil, false
	}
	input := call.Input
	if len(input) == 0 {
		input = json.RawMessage("{}")
	}
	paths, err := reporter.ReadPaths(input)
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
		res.Text = fmt.Sprintf("unknown tool %q", call.Name)
		res.IsError = true
		res.ErrorKind = llm.ToolErrorUnknownTool
		return res
	}

	input := call.Input
	if len(input) == 0 {
		input = json.RawMessage("{}")
	}
	if r.dispatchGuard != nil {
		guardCall := call
		guardCall.Input = input
		if err := r.dispatchGuard(guardCall, r.CallActivity(guardCall)); err != nil {
			res.Text = err.Error()
			res.IsError = true
			res.ErrorKind = llm.ToolErrorBlocked
			return res
		}
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
		out      string
		original string
		content  []llm.ContentBlock
		usage    llm.Usage
		metrics  map[string]int
		err      error
	}
	done := make(chan outcome, 1) // buffered: an abandoned Run can still send and exit
	go func() {
		defer func() {
			if rec := recover(); rec != nil {
				log.Printf("tool %q panicked: %v", call.Name, rec)
				done <- outcome{err: WithKind(fmt.Errorf("tool panicked: %v", rec), llm.ToolErrorPanic)}
			}
		}()
		if rich, ok := t.(RichTool); ok {
			result, err := rich.RunRich(ctx, input)
			done <- outcome{out: result.Text, content: result.Content, usage: result.Usage, err: err}
			return
		}
		if rt, ok := t.(ResultTool); ok {
			result, err := rt.RunResult(ctx, input)
			done <- outcome{out: result.Text, original: result.OriginalText, usage: result.Usage, metrics: result.Metrics, err: err}
			return
		}
		if mt, ok := t.(MeteredTool); ok {
			result, err := mt.RunMetered(ctx, input)
			done <- outcome{out: result.Text, usage: result.Usage, err: err}
			return
		}
		out, err := t.Run(ctx, input)
		done <- outcome{out: out, err: err}
	}()

	var out string
	var original string
	var content []llm.ContentBlock
	var usage llm.Usage
	var metrics map[string]int
	var err error
	select {
	case o := <-done:
		out, original, content, usage, metrics, err = o.out, o.original, o.content, o.usage, o.metrics, o.err
	case <-ctx.Done():
		// The Run goroutine is abandoned if it ignores ctx; its eventual send
		// lands in the buffered channel and is dropped. The abandoned Run may
		// still mutate external state (write files, leave a subprocess running)
		// after we return, so built-in long-running tools are expected to honor
		// ctx and apply their own user-configurable timeouts. An outer
		// cancellation is deliberately left unclassified (no ErrorKind).
		if parent.Err() != nil {
			res.Text = parent.Err().Error()
			res.ErrorKind = llm.ToolErrorCancelled
		} else if timeout > 0 {
			res.Text = fmt.Sprintf("tool timed out after %s", timeout)
			res.ErrorKind = llm.ToolErrorTimeout
		} else {
			res.Text = ctx.Err().Error()
		}
		res.IsError = true
		return res
	}

	res.Usage = usage
	res.Metrics = maps.Clone(metrics)
	if err != nil {
		// Report a timeout only when the ceiling itself expired (the derived
		// context's deadline fired) and it was not an outer cancellation. A
		// tool's own internal deadline (e.g. http.Client.Timeout) also yields
		// a DeadlineExceeded error, but with the ceiling unfired it must pass
		// through as a plain tool error — not be relabeled as a dispatch
		// timeout with the wrong duration (spec §6).
		if timeout > 0 && ctx.Err() == context.DeadlineExceeded && parent.Err() == nil {
			res.Text = fmt.Sprintf("tool timed out after %s", timeout)
			res.ErrorKind = llm.ToolErrorTimeout
		} else if errors.Is(err, context.Canceled) {
			res.Text = err.Error()
			res.ErrorKind = llm.ToolErrorCancelled
		} else if detail, invalid := invalidArgumentsDetail(err); invalid {
			res.Text = "invalid arguments: " + detail
			res.ErrorKind = llm.ToolErrorInvalidArgs
		} else {
			res.Text = err.Error()
			res.ErrorKind = KindOf(err)
		}
		res.IsError = true
		return res
	}

	if err := llm.ValidateToolResultContent(content, false); err != nil {
		res.Text = "invalid rich tool result: " + err.Error()
		res.IsError = true
		res.ErrorKind = llm.ToolErrorInvalidResult
		return res
	}
	prepared := r.PrepareResultWithOriginal(call.Name, call.ID, out, original)
	prepared.Content = append([]llm.ContentBlock(nil), content...)
	prepared.Usage = usage
	prepared.Metrics = maps.Clone(metrics)
	return prepared
}

// PrepareResult applies the registry's configured limits and records the full
// output metadata needed for archival. Background jobs use the same method so
// their truncation behavior cannot drift from ordinary dispatched tools.
func (r *Registry) PrepareResult(toolName, resultID, out string) llm.ToolResult {
	res := llm.ToolResult{ForID: resultID}
	var info truncationInfo
	res.Text, info = truncate(out, r.resultLimitsFor(toolName))
	if info.truncated {
		res.Truncated = true
		res.OriginalText = out
		res.OriginalBytes = info.originalBytes
		res.ShownBytes = info.shownBytes
	}
	return res
}

// PrepareResultWithOriginal applies the model-facing cap to out while retaining
// an explicitly supplied full result for the existing artifact pipeline.
func (r *Registry) PrepareResultWithOriginal(toolName, resultID, out, original string) llm.ToolResult {
	res := r.PrepareResult(toolName, resultID, out)
	if original == "" || original == out {
		return res
	}
	res.Truncated = true
	res.OriginalText = original
	res.OriginalBytes = len(original)
	if res.ShownBytes == 0 {
		res.ShownBytes = len(res.Text)
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

// invalidArgumentsDetail classifies validation and encoding/json decode errors
// from any tool. Unmarshal type errors are translated from Go implementation
// terms into concise JSON terms so the model can repair the call directly.
func invalidArgumentsDetail(err error) (string, bool) {
	var bad *invalidArgsError
	if errors.As(err, &bad) {
		return bad.Error(), true
	}

	var typeErr *json.UnmarshalTypeError
	if errors.As(err, &typeErr) {
		expected := jsonTypeDescription(typeErr.Type)
		received := typeErr.Value
		if received == "" {
			received = "an incompatible value"
		}
		if typeErr.Field == "" {
			return fmt.Sprintf("invalid value: expected %s; got %s", expected, received), true
		}
		return fmt.Sprintf("invalid value for %q: expected %s; got %s", typeErr.Field, expected, received), true
	}

	var syntaxErr *json.SyntaxError
	if errors.As(err, &syntaxErr) {
		return fmt.Sprintf("invalid JSON at byte %d: %s", syntaxErr.Offset, syntaxErr.Error()), true
	}
	return "", false
}

func jsonTypeDescription(t reflect.Type) string {
	if t == nil {
		return "a compatible JSON value"
	}
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	switch t.Kind() {
	case reflect.Array, reflect.Slice:
		return "an array of " + jsonTypePlural(t.Elem())
	case reflect.Map, reflect.Struct:
		return "an object"
	case reflect.Bool:
		return "a boolean"
	case reflect.String:
		return "a string"
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return "an integer"
	case reflect.Float32, reflect.Float64:
		return "a number"
	default:
		return "a compatible JSON value"
	}
}

func jsonTypePlural(t reflect.Type) string {
	if t == nil {
		return "JSON values"
	}
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	switch t.Kind() {
	case reflect.Array, reflect.Slice:
		return "arrays of " + jsonTypePlural(t.Elem())
	case reflect.Map, reflect.Struct:
		return "objects"
	case reflect.Bool:
		return "booleans"
	case reflect.String:
		return "strings"
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return "integers"
	case reflect.Float32, reflect.Float64:
		return "numbers"
	default:
		return "JSON values"
	}
}
