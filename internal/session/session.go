// Package session persists resumable state plus append-only replay/archive
// records. A session path is a directory:
//
//	state.json       compact state used for resume
//	raw.ndjson       user-facing replay events
//	compactions/     raw messages removed from active context
//	artifacts/       full tool outputs omitted from active context
package session

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"harness/internal/llm"
	"harness/internal/markdown"
	"harness/internal/todo"
)

// Version is the on-disk schema version.
const Version = 1

const (
	stateFile = "state.json"
	eventLog  = "raw.ndjson"
)

// Session is the compact, resumable conversation state.
type Session struct {
	Version       int                `json:"version"`
	Provider      string             `json:"provider"`
	Model         string             `json:"model"`
	Created       time.Time          `json:"created"`
	Updated       time.Time          `json:"updated"`
	System        string             `json:"system"`
	Agent         string             `json:"agent,omitempty"`
	Turn          int                `json:"turn,omitempty"`
	Messages      []llm.Message      `json:"messages"`
	ResponseState *llm.ResponseState `json:"response_state,omitempty"`
	Todos         []todo.Item        `json:"todos,omitempty"`
	Usage         UsageTotals        `json:"usage"`
}

// UsageTotals is the cumulative token accounting plus dollar cost for a session.
// CostUSD is 0 when the model has no price entry in the registry.
type UsageTotals struct {
	llm.Usage
	CostUSD float64 `json:"cost_usd"`
}

// ChildMeta is the forensic index for a child-agent run stored under a parent
// session's children/ directory.
type ChildMeta struct {
	ID           string    `json:"id"`
	ParentID     string    `json:"parent_id,omitempty"`
	Kind         string    `json:"kind"`
	Agent        string    `json:"agent,omitempty"`
	Provider     string    `json:"provider,omitempty"`
	Model        string    `json:"model,omitempty"`
	Status       string    `json:"status"`
	TaskPreview  string    `json:"task_preview,omitempty"`
	Transcript   string    `json:"transcript,omitempty"`
	Replay       string    `json:"replay,omitempty"`
	Error        string    `json:"error,omitempty"`
	Created      time.Time `json:"created,omitempty"`
	Updated      time.Time `json:"updated,omitempty"`
	Usage        llm.Usage `json:"usage,omitempty"`
	MessageCount int       `json:"message_count,omitempty"`
}

// Save writes state.json atomically under dir. Parent directories are created,
// and the session directory itself is the stable path printed to the user.
func (s Session) Save(dir string) error {
	if dir == "" {
		return nil
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("session: create dir: %w", err)
	}
	s.Version = Version
	s.Messages = stampMissingMessageTimes(s.Messages, sessionTimestamp(s.Updated, s.Created))

	data, err := json.Marshal(s)
	if err != nil {
		return fmt.Errorf("session: marshal: %w", err)
	}

	target := filepath.Join(dir, stateFile)
	tmp := target + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return fmt.Errorf("session: write temp: %w", err)
	}
	if err := os.Rename(tmp, target); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("session: rename: %w", err)
	}
	return nil
}

// ChildSessionDir returns the directory where a child-agent run should store
// its resumable state and replay log under parentDir.
func ChildSessionDir(parentDir, childID string) string {
	if parentDir == "" || childID == "" {
		return ""
	}
	return filepath.Join(parentDir, "children", safeName(childID))
}

// SaveChildMeta writes children/<id>/meta.json and returns the child directory.
// It is intentionally independent from Session.Save so callers can update
// status before, during, or after the child transcript is available.
func SaveChildMeta(parentDir string, meta ChildMeta) (string, error) {
	if parentDir == "" || meta.ID == "" {
		return "", nil
	}
	dir := ChildSessionDir(parentDir, meta.ID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("session: create child dir: %w", err)
	}
	if meta.Transcript == "" {
		meta.Transcript = filepath.Join("children", safeName(meta.ID), stateFile)
	}
	if meta.Replay == "" {
		meta.Replay = filepath.Join("children", safeName(meta.ID), eventLog)
	}
	if err := writeJSONAtomic(filepath.Join(dir, "meta.json"), meta); err != nil {
		return "", err
	}
	return dir, nil
}

// Load reads dir/state.json and repairs a dangling trailing tool_use, yielding a
// transcript that can be sent to either provider dialect.
func Load(dir string) (Session, error) {
	data, err := os.ReadFile(filepath.Join(dir, stateFile))
	if err != nil {
		return Session{}, err
	}
	var s Session
	if err := json.Unmarshal(data, &s); err != nil {
		return Session{}, fmt.Errorf("session: decode %s: %w", filepath.Join(dir, stateFile), err)
	}
	s.Messages = repair(s.Messages)
	return s, nil
}

// Event is one append-only replay record. Display carries the exact user-facing
// line for events that the renderer shows as dim one-liners.
type Event struct {
	Time       time.Time        `json:"time,omitempty"`
	Type       string           `json:"type"`
	Turn       int              `json:"turn,omitempty"`
	Attempt    int              `json:"attempt,omitempty"`
	Text       string           `json:"text,omitempty"`
	Phase      string           `json:"phase,omitempty"`
	Display    string           `json:"display,omitempty"`
	ToolID     string           `json:"tool_id,omitempty"`
	Tool       string           `json:"tool,omitempty"`
	Input      json.RawMessage  `json:"input,omitempty"`
	Images     []ImageInfo      `json:"images,omitempty"`
	Usage      *llm.Usage       `json:"usage,omitempty"`
	ModelTurns int              `json:"model_turns,omitempty"`
	Context    *ContextSnapshot `json:"context,omitempty"`
}

// ContextSnapshot is the session-log copy of agent.ContextEstimate. It lives in
// session to avoid importing the agent package into persistence code.
type ContextSnapshot struct {
	Total           int `json:"total,omitempty"`
	Window          int `json:"window,omitempty"`
	System          int `json:"system,omitempty"`
	Tools           int `json:"tools,omitempty"`
	Messages        int `json:"messages,omitempty"`
	PayloadTotal    int `json:"payload_total,omitempty"`
	PayloadSystem   int `json:"payload_system,omitempty"`
	PayloadTools    int `json:"payload_tools,omitempty"`
	PayloadMessages int `json:"payload_messages,omitempty"`
}

// ImageInfo records replay-safe image attachment metadata. It intentionally
// excludes base64 image data.
type ImageInfo struct {
	Name         string `json:"name,omitempty"`
	Path         string `json:"path,omitempty"`
	MediaType    string `json:"media_type,omitempty"`
	Detail       string `json:"detail,omitempty"`
	Bytes        int    `json:"bytes,omitempty"`
	EncodedBytes int    `json:"encoded_bytes,omitempty"`
	Width        int    `json:"width,omitempty"`
	Height       int    `json:"height,omitempty"`
}

const (
	EventUser               = "user"
	EventAssistantDelta     = "assistant_delta"
	EventAssistantPhase     = "assistant_phase"
	EventReasoningSummary   = "reasoning_summary"
	EventToolStart          = "tool_start"
	EventToolResult         = "tool_result"
	EventToolDiff           = "tool_diff"
	EventNotice             = "notice"
	EventModelTurnStart     = "model_turn_start"
	EventModelTurnAbandoned = "model_turn_abandoned"
	EventModelTurnUsage     = "model_turn_usage"
	EventTurnUsage          = "turn_usage"
)

// AppendEvent appends ev as one JSON line to raw.ndjson under dir.
func AppendEvent(dir string, ev Event) error {
	if dir == "" {
		return nil
	}
	if ev.Time.IsZero() {
		ev.Time = time.Now()
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("session: create dir: %w", err)
	}
	f, err := os.OpenFile(filepath.Join(dir, eventLog), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return fmt.Errorf("session: open event log: %w", err)
	}
	defer f.Close()
	enc := json.NewEncoder(f)
	if err := enc.Encode(ev); err != nil {
		return fmt.Errorf("session: append event: %w", err)
	}
	return nil
}

// ReplayOptions controls the plain-text replay renderer.
type ReplayOptions struct {
	IncludeToolOutput bool
	Markdown          bool
	ANSI              bool
	Width             int
	Quiet             bool // suppress bracketed status lines; assistant text and user prompts are unaffected
}

const finalAnswerSeparator = "\n---\n\n"

type assistantDisplay struct {
	w        io.Writer
	markdown *markdown.Stream
	lineOpen bool

	phase                 string
	visiblePreFinalOutput bool
	visibleFinalOutput    bool
	finalSeparatorPrinted bool
}

func newAssistantDisplay(w io.Writer, opts ReplayOptions) *assistantDisplay {
	d := &assistantDisplay{w: w}
	if opts.Markdown {
		d.markdown = markdown.NewStream(markdown.Options{
			Enabled: true,
			ANSI:    opts.ANSI,
			Width:   opts.Width,
		})
	}
	return d
}

func (d *assistantDisplay) Write(text string) {
	if text == "" {
		return
	}
	d.writeFinalSeparatorIfNeeded()
	if d.markdown != nil {
		io.WriteString(d.w, d.markdown.Write(text))
		d.lineOpen = d.markdown.LineOpen()
		d.markAssistantTextVisible()
		return
	}
	io.WriteString(d.w, text)
	d.lineOpen = !strings.HasSuffix(text, "\n")
	d.markAssistantTextVisible()
}

func (d *assistantDisplay) Phase(phase string) {
	if !llm.ValidAssistantPhase(phase) || phase == "" {
		return
	}
	d.phase = phase
}

func (d *assistantDisplay) Finish() {
	if d.markdown != nil {
		io.WriteString(d.w, d.markdown.Flush())
		d.lineOpen = d.markdown.LineOpen()
	}
	if !d.lineOpen {
		return
	}
	fmt.Fprintln(d.w)
	d.lineOpen = false
	if d.markdown != nil {
		d.markdown.CloseLine()
	}
}

func (d *assistantDisplay) MarkPreFinalOutput() {
	d.visiblePreFinalOutput = true
}

func (d *assistantDisplay) writeFinalSeparatorIfNeeded() {
	if d.phase != llm.AssistantPhaseFinal ||
		!d.visiblePreFinalOutput ||
		d.visibleFinalOutput ||
		d.finalSeparatorPrinted {
		return
	}
	d.Finish()
	io.WriteString(d.w, finalAnswerSeparator)
	d.finalSeparatorPrinted = true
}

func (d *assistantDisplay) markAssistantTextVisible() {
	switch d.phase {
	case llm.AssistantPhaseFinal:
		d.visibleFinalOutput = true
	case llm.AssistantPhaseCommentary:
		d.visiblePreFinalOutput = true
	}
}

// Replay prints a user-facing reconstruction of raw.ndjson.
func Replay(dir string, w io.Writer, opts ReplayOptions) error {
	events, err := readEvents(dir)
	if err != nil {
		return err
	}
	events = filterAbandonedAttemptOutput(events)

	assistant := newAssistantDisplay(w, opts)

	for _, ev := range events {
		switch ev.Type {
		case EventUser:
			assistant.Finish()
			assistant = newAssistantDisplay(w, opts)
			fmt.Fprintf(w, "> %s\n", ev.Text)
			for _, img := range ev.Images {
				fmt.Fprintf(w, "[image: %s %s %d bytes detail=%s]\n", img.Name, img.MediaType, img.Bytes, img.Detail)
			}
		case EventAssistantDelta:
			assistant.Write(ev.Text)
		case EventAssistantPhase:
			assistant.Phase(ev.Phase)
		case EventReasoningSummary:
			assistant.Finish()
			lines := ReasoningSummaryLines(ev.Text, ReasoningSummaryFormat{Width: opts.Width})
			if len(lines) != 0 {
				fmt.Fprintln(w, strings.Join(lines, "\n"))
				assistant.MarkPreFinalOutput()
			}
		case EventToolResult, EventToolDiff, EventNotice, EventModelTurnAbandoned, EventModelTurnUsage, EventTurnUsage:
			assistant.Finish()
			if ev.Display != "" && !opts.Quiet {
				fmt.Fprintln(w, ev.Display)
			}
		}
	}
	assistant.Finish()
	return nil
}

// LatestTurnOutput returns the user-visible output recorded for the latest turn,
// excluding the user's prompt. Missing replay logs are treated as empty output so
// callers can use it before the first completed turn.
func LatestTurnOutput(dir string) (string, error) {
	if dir == "" {
		return "", nil
	}
	events, err := readEvents(dir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "", nil
		}
		return "", err
	}
	events = filterAbandonedAttemptOutput(events)

	latestTurn := 0
	var b strings.Builder
	assistant := newAssistantDisplay(&b, ReplayOptions{Markdown: true})
	resetForTurn := func(turn int) {
		latestTurn = turn
		b.Reset()
		assistant = newAssistantDisplay(&b, ReplayOptions{Markdown: true})
	}

	for _, ev := range events {
		if ev.Turn == 0 {
			continue
		}
		if ev.Turn > latestTurn || ev.Type == EventUser && ev.Turn == latestTurn {
			resetForTurn(ev.Turn)
		}
		if ev.Turn != latestTurn || ev.Type == EventUser {
			continue
		}
		switch ev.Type {
		case EventAssistantDelta:
			assistant.Write(ev.Text)
		case EventAssistantPhase:
			assistant.Phase(ev.Phase)
		case EventReasoningSummary:
			assistant.Finish()
			lines := ReasoningSummaryLines(ev.Text, ReasoningSummaryFormat{})
			if len(lines) != 0 {
				b.WriteString(strings.Join(lines, "\n"))
				b.WriteByte('\n')
				assistant.MarkPreFinalOutput()
			}
		case EventToolResult, EventToolDiff, EventNotice, EventModelTurnAbandoned, EventModelTurnUsage, EventTurnUsage:
			assistant.Finish()
			if ev.Display != "" {
				b.WriteString(ev.Display)
				b.WriteByte('\n')
			}
		}
	}
	assistant.Finish()
	return strings.TrimRight(b.String(), "\n"), nil
}

func filterAbandonedAttemptOutput(events []Event) []Event {
	abandoned := map[[3]int]bool{}
	for _, ev := range events {
		if ev.Type == EventModelTurnAbandoned && ev.Turn > 0 && ev.ModelTurns > 0 && ev.Attempt > 0 {
			abandoned[[3]int{ev.Turn, ev.ModelTurns, ev.Attempt}] = true
		}
	}
	if len(abandoned) == 0 {
		return events
	}
	out := make([]Event, 0, len(events))
	for _, ev := range events {
		if attemptOutputDiscarded(ev, abandoned) {
			continue
		}
		out = append(out, ev)
	}
	return out
}

func attemptOutputDiscarded(ev Event, abandoned map[[3]int]bool) bool {
	switch ev.Type {
	case EventAssistantDelta, EventAssistantPhase, EventReasoningSummary:
	default:
		return false
	}
	if ev.Turn == 0 || ev.ModelTurns == 0 || ev.Attempt == 0 {
		return false
	}
	return abandoned[[3]int{ev.Turn, ev.ModelTurns, ev.Attempt}]
}

// Timings prints a concise wall-clock report from raw.ndjson timestamps.
func Timings(dir string, w io.Writer) error {
	events, err := readEvents(dir)
	if err != nil {
		return err
	}
	turns := map[int][]Event{}
	var order []int
	for _, ev := range events {
		if ev.Turn == 0 {
			continue
		}
		if _, ok := turns[ev.Turn]; !ok {
			order = append(order, ev.Turn)
		}
		turns[ev.Turn] = append(turns[ev.Turn], ev)
	}
	sort.Ints(order)
	for _, turn := range order {
		writeTurnTimings(w, turn, turns[turn])
	}
	return nil
}

func readEvents(dir string) ([]Event, error) {
	f, err := os.Open(filepath.Join(dir, eventLog))
	if err != nil {
		return nil, err
	}
	defer f.Close()

	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
	var events []Event
	for sc.Scan() {
		var ev Event
		if err := json.Unmarshal(sc.Bytes(), &ev); err != nil {
			return nil, fmt.Errorf("session: replay decode: %w", err)
		}
		events = append(events, ev)
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	return events, nil
}

func writeTurnTimings(w io.Writer, turn int, events []Event) {
	if len(events) == 0 {
		return
	}
	user := firstEventTime(events, EventUser)
	done := lastEventTime(events, EventTurnUsage)
	total := time.Duration(0)
	if !user.IsZero() && !done.IsZero() && !done.Before(user) {
		total = done.Sub(user)
	}
	firstVisible := firstVisibleDuration(events, user)
	if firstVisible > 0 {
		fmt.Fprintf(w, "turn %d: total %s, first visible %s\n", turn, formatDuration(total), formatDuration(firstVisible))
	} else {
		fmt.Fprintf(w, "turn %d: total %s\n", turn, formatDuration(total))
	}
	writeModelTimings(w, events)
	writeToolTimings(w, events)
	writeLargestGaps(w, events)
}

func writeModelTimings(w io.Writer, events []Event) {
	starts := map[[2]int]Event{}
	for _, ev := range events {
		if ev.Type == EventModelTurnStart {
			starts[[2]int{ev.ModelTurns, ev.Attempt}] = ev
			continue
		}
		if ev.Type != EventModelTurnUsage {
			continue
		}
		key := [2]int{ev.ModelTurns, ev.Attempt}
		start, ok := starts[key]
		if !ok || start.Time.IsZero() || ev.Time.IsZero() || ev.Time.Before(start.Time) {
			continue
		}
		fmt.Fprintf(w, "  model turn %d attempt %d: %s", ev.ModelTurns, ev.Attempt, formatDuration(ev.Time.Sub(start.Time)))
		if start.Context != nil {
			fmt.Fprintf(w, " (%s)", formatContextSnapshot(*start.Context))
		}
		fmt.Fprintln(w)
	}
}

func writeToolTimings(w io.Writer, events []Event) {
	starts := map[string]Event{}
	for _, ev := range events {
		switch ev.Type {
		case EventToolStart:
			starts[ev.ToolID] = ev
		case EventToolResult:
			start, ok := starts[ev.ToolID]
			if !ok || start.Time.IsZero() || ev.Time.IsZero() || ev.Time.Before(start.Time) {
				continue
			}
			tool := ev.Tool
			if tool == "" {
				tool = start.Tool
			}
			if tool == "" {
				tool = ev.ToolID
			}
			fmt.Fprintf(w, "  tool %s: %s\n", tool, formatDuration(ev.Time.Sub(start.Time)))
		}
	}
}

func writeLargestGaps(w io.Writer, events []Event) {
	type gap struct {
		duration time.Duration
		from     string
		to       string
	}
	var gaps []gap
	for i := 1; i < len(events); i++ {
		prev, next := events[i-1], events[i]
		if prev.Time.IsZero() || next.Time.IsZero() || next.Time.Before(prev.Time) {
			continue
		}
		gaps = append(gaps, gap{duration: next.Time.Sub(prev.Time), from: prev.Type, to: next.Type})
	}
	sort.Slice(gaps, func(i, j int) bool { return gaps[i].duration > gaps[j].duration })
	if len(gaps) > 3 {
		gaps = gaps[:3]
	}
	for _, g := range gaps {
		if g.duration <= 0 {
			continue
		}
		fmt.Fprintf(w, "  gap %s: %s -> %s\n", formatDuration(g.duration), g.from, g.to)
	}
}

func firstEventTime(events []Event, typ string) time.Time {
	for _, ev := range events {
		if ev.Type == typ {
			return ev.Time
		}
	}
	return time.Time{}
}

func lastEventTime(events []Event, typ string) time.Time {
	for i := len(events) - 1; i >= 0; i-- {
		if events[i].Type == typ {
			return events[i].Time
		}
	}
	return time.Time{}
}

// ReasoningSummaryFormat controls the replay-safe plain-text form for a
// semantic reasoning summary event.
type ReasoningSummaryFormat struct {
	Header string
	Indent string
	Width  int
}

// ReasoningSummaryLines returns the replay-safe plain-text lines for a
// semantic reasoning summary event.
func ReasoningSummaryLines(text string, format ReasoningSummaryFormat) []string {
	text = strings.TrimSpace(text)
	if text == "" {
		return nil
	}

	header := strings.TrimSpace(format.Header)
	if header == "" {
		header = "[reasoning]"
	}
	indent := format.Indent
	if indent == "" {
		indent = "  "
	}

	body := markdown.Render(text, markdown.Options{
		Enabled: true,
		Width:   format.Width,
		Prefix:  indent,
	})

	out := []string{header}
	if body != "" {
		out = append(out, strings.Split(strings.TrimRight(body, "\n"), "\n")...)
	}
	out = append(out, "[end reasoning]")
	return out
}

// ReasoningSummaryDisplay returns the replay-safe plain-text form for a
// semantic reasoning summary event.
func ReasoningSummaryDisplay(text string) string {
	lines := ReasoningSummaryLines(text, ReasoningSummaryFormat{})
	if len(lines) == 0 {
		return ""
	}
	return strings.Join(lines, "\n")
}

func firstVisibleDuration(events []Event, start time.Time) time.Duration {
	if start.IsZero() {
		return 0
	}
	for _, ev := range events {
		switch ev.Type {
		case EventAssistantDelta, EventReasoningSummary, EventToolStart, EventToolDiff, EventNotice:
			if !ev.Time.IsZero() && !ev.Time.Before(start) {
				return ev.Time.Sub(start)
			}
		}
	}
	return 0
}

func formatContextSnapshot(ctx ContextSnapshot) string {
	parts := []string{fmt.Sprintf("ctx %s/%s", formatTokens(ctx.Total), formatTokens(ctx.Window))}
	payload := ctx.PayloadTotal
	if payload == 0 {
		payload = ctx.Total
	}
	parts = append(parts, "payload "+formatTokens(payload))
	if ctx.System > 0 || ctx.Tools > 0 || ctx.Messages > 0 {
		parts = append(parts, fmt.Sprintf("sys %s tools %s msgs %s",
			formatTokens(ctx.System), formatTokens(ctx.Tools), formatTokens(ctx.Messages)))
	}
	return strings.Join(parts, " ")
}

func formatTokens(n int) string {
	if n >= 1000 {
		return fmt.Sprintf("%.1fk", float64(n)/1000)
	}
	return fmt.Sprintf("%d", n)
}

func formatDuration(d time.Duration) string {
	if d < time.Second {
		return d.Round(time.Millisecond).String()
	}
	return d.Round(100 * time.Millisecond).String()
}

// Compaction stores the raw messages removed from active context and the summary
// that replaced them.
type Compaction struct {
	Time     time.Time     `json:"time"`
	Summary  string        `json:"summary"`
	Usage    llm.Usage     `json:"usage"`
	Messages []llm.Message `json:"messages"`
}

// SaveCompaction writes one numbered compaction archive and returns the relative
// path to its input JSON file.
func SaveCompaction(dir string, c Compaction) (string, error) {
	if dir == "" {
		return "", nil
	}
	base := filepath.Join(dir, "compactions")
	if err := os.MkdirAll(base, 0o755); err != nil {
		return "", fmt.Errorf("session: create compactions dir: %w", err)
	}
	idx, err := nextIndex(base, ".input.json")
	if err != nil {
		return "", err
	}
	prefix := fmt.Sprintf("%04d", idx)

	inputRel := filepath.Join("compactions", prefix+".input.json")
	inputPath := filepath.Join(dir, inputRel)
	if err := writeJSONAtomic(inputPath, c.Messages); err != nil {
		return "", err
	}
	if err := os.WriteFile(filepath.Join(base, prefix+".summary.md"), []byte(c.Summary), 0o644); err != nil {
		return "", fmt.Errorf("session: write compaction summary: %w", err)
	}
	meta := struct {
		Time         time.Time `json:"time"`
		Usage        llm.Usage `json:"usage"`
		MessageCount int       `json:"message_count"`
		Input        string    `json:"input"`
		Summary      string    `json:"summary"`
	}{
		Time:         c.Time,
		Usage:        c.Usage,
		MessageCount: len(c.Messages),
		Input:        inputRel,
		Summary:      filepath.Join("compactions", prefix+".summary.md"),
	}
	if err := writeJSONAtomic(filepath.Join(base, prefix+".meta.json"), meta); err != nil {
		return "", err
	}
	return inputRel, nil
}

// SaveToolResultArtifact writes full output omitted from active context.
func SaveToolResultArtifact(dir string, turn int, result llm.ToolResult) (string, error) {
	if dir == "" || !result.Truncated || result.OriginalText == "" {
		return "", nil
	}
	rel := filepath.Join("artifacts", "tool-results", fmt.Sprintf("%04d-%s.txt", turn, safeName(result.ForID)))
	path := filepath.Join(dir, rel)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return "", fmt.Errorf("session: create artifact dir: %w", err)
	}
	if err := os.WriteFile(path, []byte(result.OriginalText), 0o644); err != nil {
		return "", fmt.Errorf("session: write tool artifact: %w", err)
	}
	return rel, nil
}

func writeJSONAtomic(path string, v any) error {
	data, err := json.Marshal(v)
	if err != nil {
		return fmt.Errorf("session: marshal %s: %w", path, err)
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return fmt.Errorf("session: write temp %s: %w", path, err)
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("session: rename %s: %w", path, err)
	}
	return nil
}

func nextIndex(dir, suffix string) (int, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return 0, err
	}
	var nums []int
	for _, e := range entries {
		name := e.Name()
		if !strings.HasSuffix(name, suffix) {
			continue
		}
		var n int
		if _, err := fmt.Sscanf(strings.TrimSuffix(name, suffix), "%d", &n); err == nil {
			nums = append(nums, n)
		}
	}
	sort.Ints(nums)
	if len(nums) == 0 {
		return 1, nil
	}
	return nums[len(nums)-1] + 1, nil
}

func safeName(s string) string {
	if s == "" {
		return "result"
	}
	var b strings.Builder
	for _, r := range s {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_' {
			b.WriteRune(r)
		} else {
			b.WriteByte('_')
		}
	}
	return b.String()
}

// repair applies the dangling-tool_use rule. It is a no-op for a complete
// transcript.
func repair(msgs []llm.Message) []llm.Message {
	if len(msgs) == 0 {
		return msgs
	}
	last := msgs[len(msgs)-1]
	if last.Role != llm.RoleAssistant {
		return msgs
	}

	var results []llm.ContentBlock
	for _, b := range last.Content {
		if b.Kind == llm.BlockToolUse {
			results = append(results, llm.ContentBlock{
				Kind:        llm.BlockToolResult,
				ResultForID: b.ToolUseID,
				ResultText:  "interrupted",
				ResultError: true,
			})
		}
	}
	if len(results) == 0 {
		return msgs
	}
	return append(msgs, llm.Message{Role: llm.RoleUser, Time: time.Now(), Content: results})
}

func stampMissingMessageTimes(msgs []llm.Message, at time.Time) []llm.Message {
	if at.IsZero() {
		at = time.Now()
	}
	out := make([]llm.Message, len(msgs))
	copy(out, msgs)
	for i := range out {
		if out[i].Time.IsZero() {
			out[i].Time = at
		}
	}
	return out
}

func sessionTimestamp(updated, created time.Time) time.Time {
	if !updated.IsZero() {
		return updated
	}
	return created
}

// DefaultPath returns <stateDir>/harness/sessions/<timestamp>/.
func DefaultPath(stateDir string, at time.Time) string {
	name := at.UTC().Format("20060102T150405Z")
	return filepath.Join(stateDir, "harness", "sessions", name)
}
