package main

const defaultStackGoMod = `module flowbenchstack

go 1.22
`

const defaultStackKVCoreContract = `# Milestone 1: concurrent TTL/LRU store

Repair the existing ` + "`kvstore`" + ` package without changing its public API.

1. ` + "`New`" + ` rejects capacity <= 0 with ` + "`ErrInvalidCapacity`" + ` and substitutes
   ` + "`time.Now`" + ` only for a nil clock.
2. ` + "`Put`" + ` rejects empty keys and negative TTLs without mutation. Zero TTL never
   expires; positive TTL expires at or after its deadline.
3. Stored and returned byte slices are defensive copies. Updating a key refreshes
   its value and TTL and makes it most-recently-used.
4. Capacity uses true least-recently-used eviction. Expired entries are purged
   before capacity enforcement and by ` + "`Len`" + `/` + "`Stats`" + `. Only capacity eviction
   increments ` + "`Evictions`" + `.
5. Every exported Store operation is safe for concurrent callers. Preserve the
   supplied map/list design and keep most-recently-used at the list front.

Run gofmt and ` + "`go test ./...`" + ` from this project directory.
`

const defaultStackKVDurabilityContract = `# Milestone 2: atomic batches and durable store snapshots

Preserve Milestone 1 and complete ` + "`kvstore/batch.go`" + ` and
` + "`kvstore/snapshot.go`" + ` without changing the public API.

1. ` + "`Batch`" + ` validates the complete mutation slice before taking effect. Valid
   kinds are Put and Delete; keys are non-empty; Put TTLs are non-negative;
   Delete requires nil Value and zero TTL. Any invalid item returns
   ` + "`ErrInvalidMutation`" + ` (wrapping the more specific key/TTL error when relevant)
   and changes nothing. A valid batch applies in order under one lock, captures
   the clock once, deep-copies values, and uses normal TTL/LRU/capacity rules.
2. ` + "`Snapshot`" + ` emits deterministic compact JSON with logical shape
   ` + "`{\"version\":1,\"evictions\":N,\"entries\":[{\"key\":\"k\",\"value\":\"base64\",\"ttl_ns\":N}]}`" + `.
   Entries are most- to least-recently-used; expired entries are omitted; TTL is
   zero or positive remaining nanoseconds.
3. ` + "`Restore`" + ` atomically replaces contents, rebases positive TTLs on one clock
   reading, and preserves order and the eviction counter. Reject malformed JSON,
   trailing or unknown data, the wrong version, empty/duplicate keys, negative
   TTLs, or excess capacity with ` + "`ErrInvalidSnapshot`" + ` and no mutation.
4. Snapshotted, restored, batched, stored, and returned buffers never alias.

Run gofmt and ` + "`go test ./...`" + ` from this project directory.
`

const defaultStackPlannerCoreContract = `# Milestone 3: concurrent dependency planner

Preserve both accepted kvstore milestones and repair the ` + "`planner`" + ` package.

1. ` + "`Add`" + ` rejects empty IDs, duplicate IDs, empty/duplicate dependencies, and
   self-dependencies with the documented sentinel errors. Forward dependencies
   are allowed. Task payloads and dependency slices are defensively copied.
2. ` + "`Get`" + ` returns a defensive copy. ` + "`Remove`" + ` returns ` + "`ErrNotFound`" + ` for an
   absent task and ` + "`ErrInUse`" + ` while another task depends on it.
3. ` + "`Validate`" + ` detects missing dependencies and cycles using ` + "`ErrMissingDependency`" + `
   and ` + "`ErrCycle`" + ` through ` + "`errors.Is`" + `.
4. ` + "`Ready(completed)`" + ` first validates the graph and the completed-ID set, then
   returns every incomplete task whose dependencies are complete. Order by
   descending priority, then insertion order, then ID. Unknown completed IDs
   return ` + "`ErrUnknownCompleted`" + `.
5. ` + "`Len`" + `, ` + "`Add`" + `, ` + "`Get`" + `, ` + "`Remove`" + `, ` + "`Validate`" + `, and ` + "`Ready`" + ` are safe for
   concurrent callers.

Run gofmt and ` + "`go test ./...`" + ` from this project directory.
`

const defaultStackPlannerDurabilityContract = `# Milestone 4: planner transactions and snapshots

Preserve every accepted kvstore and planner-core behavior and complete
` + "`planner/batch.go`" + ` and ` + "`planner/snapshot.go`" + `.

1. ` + "`Batch`" + ` validates and applies Add/Remove mutations atomically in order. Mutation
   shapes must be exact, and the resulting graph must contain no missing
   dependency or cycle. Any error leaves tasks and future insertion ordering
   unchanged.
2. ` + "`Topological`" + ` returns every task exactly once in deterministic dependency
   order. Whenever multiple tasks are ready, use descending priority, insertion
   order, then ID. It returns the same validation sentinels as ` + "`Validate`" + `.
3. ` + "`Snapshot`" + ` emits deterministic compact JSON with version 1 and tasks in
   insertion order, preserving normalized dependency order, priority, payload,
   and insertion ordinal.
4. ` + "`Restore`" + ` atomically replaces the graph. Reject malformed/trailing/unknown
   JSON, wrong versions, empty or duplicate IDs, invalid dependencies or
   ordinals, missing dependencies, and cycles with ` + "`ErrInvalidSnapshot`" + `. A
   restored graph must continue with an insertion ordinal above the maximum.
5. Batch, snapshot, restore, and returned tasks never alias caller buffers.

Run gofmt and ` + "`go test ./...`" + ` from this project directory.
`

const defaultStackFramingCoreContract = `# Milestone 5: checksummed streaming frame codec

Preserve all accepted kvstore and planner behavior and repair the ` + "`framing`" + `
codec without changing its public API.

1. A frame is encoded as: magic bytes 0x48 0x46, version byte 1, flags byte,
   big-endian stream length uint16, sequence uint64, payload length uint32,
   stream bytes, payload bytes, then an IEEE CRC32 uint32 covering every prior
   byte in that frame.
2. Streams are non-empty valid UTF-8 and fit uint16. Only flag bits 0 and 1 are
   valid. Payload length cannot exceed the positive configured maximum.
3. Encoder construction rejects nil writers and non-positive limits. It handles
   legal short writes and reports zero-progress writes with ` + "`io.ErrShortWrite`" + `.
4. Decoder construction rejects nil readers and non-positive limits. ` + "`Next`" + ` uses
   exact reads, returns plain ` + "`io.EOF`" + ` only at a clean frame boundary, wraps
   ` + "`ErrTruncated`" + ` for partial frames, and returns the documented sentinel for
   bad magic/version/flags/length/checksum.
5. Decoded payloads do not alias reusable internal buffers. Encoder and Decoder
   are each safe against their own accidental concurrent calls.

Run gofmt and ` + "`go test ./...`" + ` from this project directory.
`

const defaultStackFramingLogContract = `# Milestone 6: atomic multi-stream framed log

Preserve every earlier milestone and complete ` + "`framing/log.go`" + ` and
` + "`framing/log_snapshot.go`" + `.

1. ` + "`NewLog`" + ` rejects a non-positive payload limit. ` + "`Append`" + ` validates frames,
   deep-copies payloads, and requires each stream's sequence to increase strictly.
   ` + "`Len`" + ` and ` + "`Frames(stream, after)`" + ` are concurrency-safe; Frames preserves
   append order, filters sequence > after, and returns defensive copies.
2. ` + "`Batch`" + ` validates the entire slice, including within-batch sequence ordering,
   then appends atomically. Any invalid frame leaves the log unchanged.
3. ` + "`Snapshot`" + ` emits: ASCII HLOG, version byte 1, big-endian frame count uint32,
   then the exact Milestone-5 encoding of every frame in append order. Output is
   deterministic and does not alias stored data.
4. ` + "`Restore`" + ` atomically replaces the log from that format. Reject bad headers,
   versions, counts, trailing bytes, corrupt/truncated frames, oversized payloads,
   or non-increasing per-stream sequences with ` + "`ErrInvalidLog`" + `, preserving the
   original log on failure.
5. Append, Batch, Frames, Len, Snapshot, and Restore are safe for concurrent
   callers and do not expose internal buffers.

Run gofmt and ` + "`go test ./...`" + ` from this project directory.
`

var defaultStackInitialFiles = func() map[string]string {
	files := map[string]string{
		"go.mod":                  defaultStackGoMod,
		"planner/types.go":        defaultStackPlannerTypes,
		"planner/graph.go":        defaultStackPlannerGraph,
		"planner/validate.go":     defaultStackPlannerValidate,
		"planner/ready.go":        defaultStackPlannerReady,
		"planner/batch.go":        defaultStackPlannerBatch,
		"planner/snapshot.go":     defaultStackPlannerSnapshot,
		"framing/types.go":        defaultStackFramingTypes,
		"framing/codec.go":        defaultStackFramingCodec,
		"framing/log.go":          defaultStackFramingLog,
		"framing/log_snapshot.go": defaultStackFramingSnapshot,
	}
	for name, body := range kvInitialFiles {
		files["kvstore/"+name] = body
	}
	return files
}()

const defaultStackPlannerTypes = `package planner

import "errors"

var (
	ErrInvalidID        = errors.New("planner: invalid id")
	ErrDuplicate        = errors.New("planner: duplicate task")
	ErrInvalidDependency = errors.New("planner: invalid dependency")
	ErrMissingDependency = errors.New("planner: missing dependency")
	ErrCycle             = errors.New("planner: dependency cycle")
	ErrNotFound          = errors.New("planner: task not found")
	ErrInUse             = errors.New("planner: task is in use")
	ErrUnknownCompleted  = errors.New("planner: unknown completed task")
	ErrInvalidMutation   = errors.New("planner: invalid mutation")
	ErrInvalidSnapshot   = errors.New("planner: invalid snapshot")
	ErrNotImplemented    = errors.New("planner: not implemented")
)

type Task struct {
	ID           string
	Dependencies []string
	Priority     int
	Payload      []byte
}

type MutationKind string

const (
	MutationAdd    MutationKind = "add"
	MutationRemove MutationKind = "remove"
)

type Mutation struct {
	Kind MutationKind
	Task Task
	ID   string
}
`

const defaultStackPlannerGraph = `package planner

import "sync"

type taskRecord struct {
	task  Task
	order uint64
}

type Graph struct {
	mu    sync.RWMutex
	tasks map[string]taskRecord
	next  uint64
}

func New() *Graph { return &Graph{tasks: make(map[string]taskRecord)} }

func (g *Graph) Add(task Task) error {
	// BUG: validation, copies, duplicate handling, and ordering are incomplete.
	g.mu.Lock()
	defer g.mu.Unlock()
	g.tasks[task.ID] = taskRecord{task: task, order: g.next}
	g.next++
	return nil
}

func (g *Graph) Get(id string) (Task, bool) {
	g.mu.RLock()
	defer g.mu.RUnlock()
	record, ok := g.tasks[id]
	return record.task, ok
}

func (g *Graph) Remove(id string) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	delete(g.tasks, id)
	return nil
}

func (g *Graph) Len() int {
	g.mu.RLock()
	defer g.mu.RUnlock()
	return len(g.tasks)
}
`

const defaultStackPlannerValidate = `package planner

func (g *Graph) Validate() error { return nil }
`

const defaultStackPlannerReady = `package planner

func (g *Graph) Ready(completed []string) ([]Task, error) {
	return nil, ErrNotImplemented
}
`

const defaultStackPlannerBatch = `package planner

func (g *Graph) Batch(mutations []Mutation) error { return ErrNotImplemented }

func (g *Graph) Topological() ([]Task, error) { return nil, ErrNotImplemented }
`

const defaultStackPlannerSnapshot = `package planner

func (g *Graph) Snapshot() ([]byte, error) { return nil, ErrNotImplemented }

func (g *Graph) Restore(data []byte) error { return ErrNotImplemented }
`

const defaultStackFramingTypes = `package framing

import "errors"

var (
	ErrInvalidLimit = errors.New("framing: invalid limit")
	ErrInvalidIO    = errors.New("framing: nil reader or writer")
	ErrInvalidFrame = errors.New("framing: invalid frame")
	ErrMagic        = errors.New("framing: bad magic")
	ErrVersion      = errors.New("framing: bad version")
	ErrFlags        = errors.New("framing: invalid flags")
	ErrTooLarge     = errors.New("framing: payload too large")
	ErrTruncated    = errors.New("framing: truncated frame")
	ErrChecksum     = errors.New("framing: checksum mismatch")
	ErrSequence     = errors.New("framing: non-increasing sequence")
	ErrInvalidLog   = errors.New("framing: invalid log")
	ErrNotImplemented = errors.New("framing: not implemented")
)

type Frame struct {
	Stream   string
	Sequence uint64
	Flags    uint8
	Payload  []byte
}
`

const defaultStackFramingCodec = `package framing

import "io"

type Encoder struct {
	w   io.Writer
	max int
}

func NewEncoder(w io.Writer, maxPayload int) (*Encoder, error) {
	return &Encoder{w: w, max: maxPayload}, nil
}

func (e *Encoder) WriteFrame(frame Frame) error { return ErrNotImplemented }

type Decoder struct {
	r   io.Reader
	max int
}

func NewDecoder(r io.Reader, maxPayload int) (*Decoder, error) {
	return &Decoder{r: r, max: maxPayload}, nil
}

func (d *Decoder) Next() (Frame, error) { return Frame{}, ErrNotImplemented }
`

const defaultStackFramingLog = `package framing

import "sync"

type Log struct {
	mu         sync.RWMutex
	maxPayload int
	frames     []Frame
	last       map[string]uint64
}

func NewLog(maxPayload int) (*Log, error) {
	return &Log{maxPayload: maxPayload, last: make(map[string]uint64)}, nil
}

func (l *Log) Append(frame Frame) error { return ErrNotImplemented }

func (l *Log) Batch(frames []Frame) error { return ErrNotImplemented }

func (l *Log) Frames(stream string, after uint64) []Frame { return nil }

func (l *Log) Len() int { return 0 }
`

const defaultStackFramingSnapshot = `package framing

func (l *Log) Snapshot() ([]byte, error) { return nil, ErrNotImplemented }

func (l *Log) Restore(data []byte) error { return ErrNotImplemented }
`

func defaultStackHiddenTests(milestoneIndex int) map[string]string {
	tests := map[string]string{}
	if milestoneIndex == 0 {
		tests["kvstore/zz_flowbench_hidden_test.go"] = kvCheckpointTests
	} else {
		tests["kvstore/zz_flowbench_hidden_test.go"] = kvFinalTests
	}
	if milestoneIndex >= 2 {
		tests["planner/zz_flowbench_hidden_test.go"] = defaultStackPlannerCoreTests
	}
	if milestoneIndex >= 3 {
		tests["planner/zz_flowbench_hidden_test.go"] = defaultStackPlannerFinalTests
	}
	if milestoneIndex >= 4 {
		tests["framing/zz_flowbench_hidden_test.go"] = defaultStackFramingCoreTests
	}
	if milestoneIndex >= 5 {
		tests["framing/zz_flowbench_hidden_test.go"] = defaultStackFramingFinalTests
	}
	return tests
}

const defaultStackPlannerCoreTests = `package planner

import (
	"errors"
	"fmt"
	"reflect"
	"sync"
	"testing"
)

func taskIDs(tasks []Task) []string {
	ids := make([]string, len(tasks))
	for i, task := range tasks { ids[i] = task.ID }
	return ids
}

func TestOraclePlannerValidationReadyAndCopies(t *testing.T) {
	g := New()
	payload := []byte("build")
	deps := []string{"fetch"}
	if err := g.Add(Task{ID:"build", Dependencies:deps, Priority:8, Payload:payload}); err != nil { t.Fatal(err) }
	payload[0] = 'X'; deps[0] = "wrong"
	if err := g.Add(Task{ID:"fetch", Priority:2, Payload:[]byte("source")}); err != nil { t.Fatal(err) }
	if err := g.Add(Task{ID:"lint", Dependencies:[]string{"fetch"}, Priority:9}); err != nil { t.Fatal(err) }
	if err := g.Validate(); err != nil { t.Fatal(err) }
	got, ok := g.Get("build")
	if !ok || string(got.Payload) != "build" || !reflect.DeepEqual(got.Dependencies, []string{"fetch"}) { t.Fatalf("Get = %+v, %t", got, ok) }
	got.Payload[0] = 'Y'; got.Dependencies[0] = "mutated"
	again, _ := g.Get("build")
	if string(again.Payload) != "build" || again.Dependencies[0] != "fetch" { t.Fatal("Get aliases graph") }
	ready, err := g.Ready(nil)
	if err != nil || !reflect.DeepEqual(taskIDs(ready), []string{"fetch"}) { t.Fatalf("Ready(nil) = %v, %v", taskIDs(ready), err) }
	ready, err = g.Ready([]string{"fetch"})
	if err != nil || !reflect.DeepEqual(taskIDs(ready), []string{"lint", "build"}) { t.Fatalf("Ready(fetch) = %v, %v", taskIDs(ready), err) }
	if _, err := g.Ready([]string{"unknown"}); !errors.Is(err, ErrUnknownCompleted) { t.Fatalf("unknown completed error = %v", err) }
	if err := g.Remove("fetch"); !errors.Is(err, ErrInUse) { t.Fatalf("Remove(in use) = %v", err) }
	if err := g.Remove("missing"); !errors.Is(err, ErrNotFound) { t.Fatalf("Remove(missing) = %v", err) }
}

func TestOraclePlannerInvalidAndCycles(t *testing.T) {
	g := New()
	for _, task := range []Task{
		{ID:""}, {ID:"self", Dependencies:[]string{"self"}}, {ID:"dupdeps", Dependencies:[]string{"x", "x"}},
	} {
		if err := g.Add(task); err == nil { t.Fatalf("Add(%+v) succeeded", task) }
	}
	if err := g.Add(Task{ID:"a", Dependencies:[]string{"missing"}}); err != nil { t.Fatal(err) }
	if err := g.Validate(); !errors.Is(err, ErrMissingDependency) { t.Fatalf("missing error = %v", err) }
	if err := g.Add(Task{ID:"missing", Dependencies:[]string{"a"}}); err != nil { t.Fatal(err) }
	if err := g.Validate(); !errors.Is(err, ErrCycle) { t.Fatalf("cycle error = %v", err) }
	if err := g.Add(Task{ID:"a"}); !errors.Is(err, ErrDuplicate) { t.Fatalf("duplicate error = %v", err) }
}

func TestOraclePlannerConcurrentAddsAndReads(t *testing.T) {
	g := New()
	var wg sync.WaitGroup
	for i := 0; i < 32; i++ {
		i := i; wg.Add(1)
		go func() { defer wg.Done(); id := fmt.Sprintf("task-%02d", i); if err := g.Add(Task{ID:id, Priority:i}); err != nil { t.Errorf("Add: %v", err); return }; if _, ok := g.Get(id); !ok { t.Errorf("%s missing", id) } }()
	}
	wg.Wait()
	if g.Len() != 32 { t.Fatalf("Len = %d", g.Len()) }
}
`

const defaultStackPlannerFinalTests = defaultStackPlannerCoreTests + `
func TestOraclePlannerBatchAtomicAndTopological(t *testing.T) {
	g := New()
	if err := g.Batch([]Mutation{
		{Kind:MutationAdd, Task:Task{ID:"fetch", Priority:1}},
		{Kind:MutationAdd, Task:Task{ID:"build", Dependencies:[]string{"fetch"}, Priority:5}},
		{Kind:MutationAdd, Task:Task{ID:"lint", Dependencies:[]string{"fetch"}, Priority:9}},
		{Kind:MutationAdd, Task:Task{ID:"package", Dependencies:[]string{"build", "lint"}, Priority:3}},
	}); err != nil { t.Fatal(err) }
	order, err := g.Topological()
	if err != nil || !reflect.DeepEqual(taskIDs(order), []string{"fetch", "lint", "build", "package"}) { t.Fatalf("Topological = %v, %v", taskIDs(order), err) }
	before, err := g.Snapshot(); if err != nil { t.Fatal(err) }
	err = g.Batch([]Mutation{{Kind:MutationRemove, ID:"fetch"}, {Kind:MutationAdd, Task:Task{ID:"bad", Dependencies:[]string{"missing"}}}})
	if err == nil { t.Fatal("invalid batch succeeded") }
	after, _ := g.Snapshot()
	if string(before) != string(after) { t.Fatalf("failed batch mutated graph\nbefore=%s\nafter=%s", before, after) }
}

func TestOraclePlannerSnapshotRestore(t *testing.T) {
	g := New()
	_ = g.Add(Task{ID:"a", Priority:2, Payload:[]byte("alpha")})
	_ = g.Add(Task{ID:"b", Dependencies:[]string{"a"}, Priority:4, Payload:[]byte("beta")})
	one, err := g.Snapshot(); if err != nil { t.Fatal(err) }
	two, _ := g.Snapshot(); if string(one) != string(two) { t.Fatalf("snapshot not deterministic\n%s\n%s", one, two) }
	if !reflect.ValueOf(string(one)).IsValid() || len(one) == 0 { t.Fatal("empty snapshot") }
	restored := New()
	if err := restored.Restore(one); err != nil { t.Fatal(err) }
	one[0] ^= 0xff
	got, ok := restored.Get("b"); if !ok || string(got.Payload) != "beta" { t.Fatalf("restored b = %+v, %t", got, ok) }
	if err := restored.Add(Task{ID:"c"}); err != nil { t.Fatal(err) }
	order, err := restored.Topological(); if err != nil || !reflect.DeepEqual(taskIDs(order), []string{"a", "b", "c"}) { t.Fatalf("restored order = %v, %v", taskIDs(order), err) }
	want, _ := restored.Snapshot()
	for _, invalid := range [][]byte{
		[]byte("{"),
		[]byte(` + "`{\"version\":2,\"tasks\":[]}`" + `),
		[]byte(` + "`{\"version\":1,\"tasks\":[],\"unknown\":true}`" + `),
		[]byte(` + "`{\"version\":1,\"tasks\":[{\"id\":\"x\",\"dependencies\":[\"missing\"],\"priority\":0,\"payload\":null,\"order\":0}]}`" + `),
	} {
		if err := restored.Restore(invalid); !errors.Is(err, ErrInvalidSnapshot) { t.Fatalf("Restore(%s) = %v", invalid, err) }
		after, _ := restored.Snapshot(); if string(after) != string(want) { t.Fatal("invalid restore mutated graph") }
	}
}
`

const defaultStackFramingCoreTests = `package framing

import (
	"bytes"
	"errors"
	"io"
	"reflect"
	"testing"
)

type chunkWriter struct { dst *bytes.Buffer; n int }
func (w chunkWriter) Write(p []byte) (int, error) { if len(p) > w.n { p = p[:w.n] }; return w.dst.Write(p) }

type zeroWriter struct{}
func (zeroWriter) Write([]byte) (int, error) { return 0, nil }

type chunkReader struct { src *bytes.Reader; n int }
func (r *chunkReader) Read(p []byte) (int, error) { if len(p) > r.n { p = p[:r.n] }; return r.src.Read(p) }

func TestOracleFrameRoundTripAndShortIO(t *testing.T) {
	var raw bytes.Buffer
	enc, err := NewEncoder(chunkWriter{dst:&raw, n:3}, 1024); if err != nil { t.Fatal(err) }
	frames := []Frame{{Stream:"alpha", Sequence:1, Flags:1, Payload:[]byte("hello")}, {Stream:"βeta", Sequence:9, Flags:2, Payload:[]byte("world")}}
	for _, frame := range frames { if err := enc.WriteFrame(frame); err != nil { t.Fatal(err) } }
	reader := &chunkReader{src:bytes.NewReader(raw.Bytes()), n:2}
	dec, err := NewDecoder(reader, 1024); if err != nil { t.Fatal(err) }
	for i, want := range frames {
		got, err := dec.Next(); if err != nil { t.Fatal(err) }
		if !reflect.DeepEqual(got, want) { t.Fatalf("frame %d = %+v, want %+v", i, got, want) }
		got.Payload[0] ^= 0xff
	}
	if _, err := dec.Next(); !errors.Is(err, io.EOF) || errors.Is(err, ErrTruncated) { t.Fatalf("clean EOF = %v", err) }
}

func TestOracleFrameValidationAndCorruption(t *testing.T) {
	if _, err := NewEncoder(nil, 1); !errors.Is(err, ErrInvalidIO) { t.Fatalf("nil writer = %v", err) }
	if _, err := NewDecoder(nil, 1); !errors.Is(err, ErrInvalidIO) { t.Fatalf("nil reader = %v", err) }
	if _, err := NewEncoder(io.Discard, 0); !errors.Is(err, ErrInvalidLimit) { t.Fatalf("zero max = %v", err) }
	enc, _ := NewEncoder(io.Discard, 4)
	for _, frame := range []Frame{{Stream:""}, {Stream:"x", Flags:4}, {Stream:"x", Payload:[]byte("12345")}} {
		if err := enc.WriteFrame(frame); !errors.Is(err, ErrInvalidFrame) && !errors.Is(err, ErrFlags) && !errors.Is(err, ErrTooLarge) { t.Fatalf("WriteFrame(%+v) = %v", frame, err) }
	}
	badEnc, _ := NewEncoder(zeroWriter{}, 10)
	if err := badEnc.WriteFrame(Frame{Stream:"x"}); !errors.Is(err, io.ErrShortWrite) { t.Fatalf("zero writer = %v", err) }
	var raw bytes.Buffer; good, _ := NewEncoder(&raw, 100); _ = good.WriteFrame(Frame{Stream:"x", Sequence:1, Payload:[]byte("payload")})
	truncated := raw.Bytes()[:raw.Len()-1]
	dec, _ := NewDecoder(bytes.NewReader(truncated), 100)
	if _, err := dec.Next(); !errors.Is(err, ErrTruncated) { t.Fatalf("truncated = %v", err) }
	corrupt := append([]byte(nil), raw.Bytes()...); corrupt[len(corrupt)-5] ^= 1
	dec, _ = NewDecoder(bytes.NewReader(corrupt), 100)
	if _, err := dec.Next(); !errors.Is(err, ErrChecksum) { t.Fatalf("checksum = %v", err) }
}
`

const defaultStackFramingFinalTests = defaultStackFramingCoreTests + `
func TestOracleLogBatchQueriesAndCopies(t *testing.T) {
	if _, err := NewLog(0); !errors.Is(err, ErrInvalidLimit) { t.Fatalf("NewLog(0) = %v", err) }
	log, err := NewLog(32); if err != nil { t.Fatal(err) }
	payload := []byte("one")
	if err := log.Append(Frame{Stream:"a", Sequence:1, Payload:payload}); err != nil { t.Fatal(err) }
	payload[0] = 'X'
	if err := log.Batch([]Frame{{Stream:"b", Sequence:2, Payload:[]byte("b2")}, {Stream:"a", Sequence:3, Payload:[]byte("a3")}}); err != nil { t.Fatal(err) }
	if log.Len() != 3 { t.Fatalf("Len = %d", log.Len()) }
	got := log.Frames("a", 0)
	if len(got) != 2 || string(got[0].Payload) != "one" || got[1].Sequence != 3 { t.Fatalf("Frames = %+v", got) }
	got[0].Payload[0] = 'Y'; if again := log.Frames("a", 0); string(again[0].Payload) != "one" { t.Fatal("Frames aliases log") }
	before, _ := log.Snapshot()
	if err := log.Batch([]Frame{{Stream:"a", Sequence:4}, {Stream:"a", Sequence:2}}); !errors.Is(err, ErrSequence) { t.Fatalf("bad batch = %v", err) }
	after, _ := log.Snapshot(); if string(before) != string(after) { t.Fatal("bad batch mutated log") }
}

func TestOracleLogSnapshotRestoreAtomic(t *testing.T) {
	log, _ := NewLog(64)
	_ = log.Batch([]Frame{{Stream:"a", Sequence:1, Flags:1, Payload:[]byte("alpha")}, {Stream:"b", Sequence:7, Flags:2, Payload:[]byte("beta")}, {Stream:"a", Sequence:3, Payload:[]byte("gamma")}})
	one, err := log.Snapshot(); if err != nil { t.Fatal(err) }
	two, _ := log.Snapshot(); if string(one) != string(two) { t.Fatal("snapshot not deterministic") }
	restored, _ := NewLog(64); if err := restored.Restore(one); err != nil { t.Fatal(err) }
	one[0] ^= 0xff
	if got := restored.Frames("a", 0); len(got) != 2 || string(got[1].Payload) != "gamma" { t.Fatalf("restored = %+v", got) }
	want, _ := restored.Snapshot()
	invalids := [][]byte{[]byte("HLO"), append(append([]byte(nil), want...), 0), append([]byte(nil), want[:len(want)-1]...)}
	corrupt := append([]byte(nil), want...); corrupt[len(corrupt)-5] ^= 1; invalids = append(invalids, corrupt)
	for _, invalid := range invalids {
		if err := restored.Restore(invalid); !errors.Is(err, ErrInvalidLog) { t.Fatalf("Restore invalid = %v", err) }
		after, _ := restored.Snapshot(); if string(after) != string(want) { t.Fatal("invalid restore mutated log") }
	}
}
`
