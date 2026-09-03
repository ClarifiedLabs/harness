package tools

import (
	"context"
	"encoding/json"
	"time"
)

// MeteredTool is an optional extension for tools whose Run implementation can
// report additional token usage. Tools still implement Tool.Run for ordinary
// callers; Dispatch prefers RunMetered when present.
type MeteredTool interface {
	RunMetered(ctx context.Context, input json.RawMessage) (MeteredResult, error)
}

// ResultTool is an optional extension for tools that proactively summarize
// successful output. Dispatch prefers it over MeteredTool and Tool.Run.
type ResultTool interface {
	RunResult(ctx context.Context, input json.RawMessage) (RunResult, error)
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

// SequentialTool optionally opts specific inputs out of default-parallel
// intra-turn dispatch. Implementations should return true only for concrete
// ordering-sensitive inputs; ordinary tools are parallel-eligible by default.
type SequentialTool interface {
	RequiresSequential(input json.RawMessage) bool
}

// FileMutationReporter is implemented by tools that can identify the file paths
// they may mutate from their JSON input. The agent uses normalized path keys to
// order overlapping mutations and the original paths for optional user-facing
// before/after diff display; Dispatch and model-visible results do not depend on it.
type FileMutationReporter interface {
	MutatedPaths(input json.RawMessage) ([]string, error)
}

// FileReadReporter is implemented by tools that can identify file paths they
// attempt to read from their JSON input. It reports call-level requested paths;
// Dispatch and model-visible results do not depend on it.
type FileReadReporter interface {
	ReadPaths(input json.RawMessage) ([]string, error)
}

// InputTrimmer is an optional capability for tools whose inputs embed whole
// file bodies (write, edit). RetentionInputReceipt returns a compact receipt
// that preserves the fields other capabilities need — MutatedPaths/ReadPaths
// must stay decodable — while dropping the bulky text. The receipt must remain
// a complete JSON object and should embed a stable sentinel so repeated passes
// can detect an already-trimmed input. ok is false when the input has no
// trimmable payload (the tool keeps the original).
type InputTrimmer interface {
	RetentionInputReceipt(input json.RawMessage) (json.RawMessage, bool)
}

// BackgroundJobStarter is implemented by the background job manager and injected
// into tools that opt into background execution.
type BackgroundJobStarter interface {
	StartBackgroundJob(BackgroundJobRequest) (BackgroundJobInfo, error)
}

// BackgroundJobDiagnosticIdentitySetter is the optional diagnostics capability
// exposed by a background starter. Parent and delegate sinks use it to retain
// launch attribution without depending on the concrete manager package.
type BackgroundJobDiagnosticIdentitySetter interface {
	SetDiagnosticIdentity(string, BackgroundDiagnosticIdentity) bool
}

// SelfTimeouter is an optional Tool extension. A tool that enforces its own
// per-call deadline reports it here so the Dispatch-level ceiling only ever
// RAISES to that deadline, never lowers it. This preserves shell's
// documented "no maximum" (its own timeout_seconds stays authoritative) while
// the ceiling still bounds tools that ignore ctx (design §8.2). ok is false when
// the tool has no input-specific deadline.
type SelfTimeouter interface {
	SelfTimeout(input json.RawMessage) (timeout time.Duration, ok bool)
}

// SchemaDescriptionPreserver is an optional tool capability for concise,
// model-facing JSON-schema field guidance. First-party tools opt in; adapters
// for external schemas omit it so Registry.Specs strips unbounded prose.
type SchemaDescriptionPreserver interface {
	PreserveSchemaDescriptions() bool
}
