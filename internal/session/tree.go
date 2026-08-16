package session

import (
	"bufio"
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"time"

	"harness/internal/llm"
)

const treeFile = "tree.ndjson"

// EntryType identifies an immutable node in a session conversation tree.
type EntryType string

const (
	EntrySegment      EntryType = "segment"
	EntryCompaction   EntryType = "compaction"
	EntryBranch       EntryType = "branch"
	EntryContextReset EntryType = "context_reset"
)

// TreeHeader is the first record in tree.ndjson.
type TreeHeader struct {
	Type          string    `json:"type"`
	Version       int       `json:"version"`
	ID            string    `json:"id"`
	Created       time.Time `json:"created"`
	CWD           string    `json:"cwd,omitempty"`
	ParentSession string    `json:"parent_session,omitempty"`
	ParentEntryID string    `json:"parent_entry_id,omitempty"`
}

// ContextDelta is the legacy splice representation of a context reset.
// Splice indices always refer to the unmodified parent context.
type ContextDelta struct {
	BaseMessageCount   int             `json:"base_message_count"`
	ResultMessageCount int             `json:"result_message_count"`
	BaseDigest         string          `json:"base_digest"`
	ResultDigest       string          `json:"result_digest"`
	Splices            []ContextSplice `json:"splices"`
}

// ContextSplice deletes and inserts one run at Start in the parent context.
type ContextSplice struct {
	Start    int           `json:"start"`
	Delete   int           `json:"delete"`
	Messages []llm.Message `json:"messages,omitempty"`
}

// Entry is one immutable tree node. Segment nodes contain one transcript-valid
// unit; tool-use assistant messages are grouped with their result message so
// every node is a safe navigation boundary.
type Entry struct {
	Type     EntryType `json:"type"`
	ID       string    `json:"id"`
	ParentID string    `json:"parent_id,omitempty"`
	Time     time.Time `json:"time"`

	Messages     []llm.Message `json:"messages,omitempty"`
	ContextDelta *ContextDelta `json:"context_delta,omitempty"`

	Checkpoint       *llm.Message `json:"checkpoint,omitempty"`
	FirstKeptEntryID string       `json:"first_kept_entry_id,omitempty"`
	ArchiveRef       string       `json:"archive_ref,omitempty"`
	TokensBefore     int          `json:"tokens_before,omitempty"`
	SummarySource    string       `json:"summary_source,omitempty"`
	FallbackReason   string       `json:"fallback_reason,omitempty"`
	ReadFiles        []string     `json:"read_files,omitempty"`
	ModifiedFiles    []string     `json:"modified_files,omitempty"`
	MaterializedKept bool         `json:"materialized_kept,omitempty"`

	FromLeafID         string `json:"from_leaf_id,omitempty"`
	CommonAncestor     string `json:"common_ancestor_id,omitempty"`
	Summary            string `json:"summary,omitempty"`
	WorkspaceUnchanged bool   `json:"workspace_unchanged,omitempty"`
	CustomFocus        string `json:"custom_focus,omitempty"`
	Reason             string `json:"reason,omitempty"`
}

// TreeNode is the read-only hierarchical form used by the terminal picker.
type TreeNode struct {
	Entry    Entry
	Children []*TreeNode
	Depth    int
}

type messageRef struct {
	entryID string
	offset  int
}

type pendingCompaction struct {
	olderCount    int
	summary       string
	archiveRef    string
	tokensBefore  int
	focus         string
	readFiles     []string
	modifiedFiles []string
}

// Tree manages the canonical append-only conversation history in memory.
type Tree struct {
	Header     TreeHeader
	Entries    []Entry
	ActiveLeaf string

	byID         map[string]int
	flushed      int
	flushedPath  string
	needsInspect bool
	activeMsgs   []llm.Message
	activeRefs   []messageRef
	pending      *pendingCompaction
}

// NewTree returns an empty current-schema tree.
func NewTree(created time.Time, cwd, parentSession, parentEntryID string) *Tree {
	if created.IsZero() {
		created = time.Now()
	}
	return &Tree{
		Header: TreeHeader{
			Type:          "session",
			Version:       Version,
			ID:            randomHex(16),
			Created:       created,
			CWD:           cwd,
			ParentSession: parentSession,
			ParentEntryID: parentEntryID,
		},
		byID: make(map[string]int),
	}
}

// LinearTree constructs a single-branch tree from an existing transcript.
func LinearTree(created time.Time, cwd string, messages []llm.Message) (*Tree, error) {
	t := NewTree(created, cwd, "", "")
	if err := t.SyncTranscript(messages); err != nil {
		return nil, err
	}
	return t, nil
}

// LoadTree loads and validates tree.ndjson. A malformed final record is treated
// as an interrupted append; malformed records before the final line are errors.
func LoadTree(dir, activeLeaf string) (*Tree, error) {
	path := filepath.Join(dir, treeFile)
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	var records []json.RawMessage
	scanner := bufio.NewScanner(f)
	buf := make([]byte, 64*1024)
	scanner.Buffer(buf, 64*1024*1024)
	for scanner.Scan() {
		line := bytes.TrimSpace(scanner.Bytes())
		if len(line) == 0 {
			continue
		}
		records = append(records, append(json.RawMessage(nil), line...))
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("session: scan %s: %w", path, err)
	}
	if len(records) == 0 {
		return nil, fmt.Errorf("session: empty tree %s", path)
	}

	var header TreeHeader
	if err := json.Unmarshal(records[0], &header); err != nil {
		return nil, fmt.Errorf("session: decode tree header: %w", err)
	}
	if header.Type != "session" || header.Version != Version || header.ID == "" {
		return nil, fmt.Errorf("session: invalid tree header (type=%q version=%d id=%q)", header.Type, header.Version, header.ID)
	}
	t := &Tree{Header: header, byID: make(map[string]int)}
	for i, raw := range records[1:] {
		var entry Entry
		if err := json.Unmarshal(raw, &entry); err != nil {
			if i == len(records)-2 {
				break
			}
			return nil, fmt.Errorf("session: decode tree entry %d: %w", i+1, err)
		}
		if err := t.addLoaded(entry); err != nil {
			return nil, fmt.Errorf("session: tree entry %d: %w", i+1, err)
		}
	}
	t.flushed = len(t.Entries)
	t.flushedPath = filepath.Clean(path)
	if activeLeaf != "" {
		if _, ok := t.byID[activeLeaf]; !ok {
			return nil, fmt.Errorf("session: active leaf %q not found", activeLeaf)
		}
		t.ActiveLeaf = activeLeaf
	} else if len(t.Entries) > 0 {
		t.ActiveLeaf = t.Entries[len(t.Entries)-1].ID
	}
	msgs, refs, err := t.buildContext(t.ActiveLeaf)
	if err != nil {
		return nil, err
	}
	t.activeMsgs, t.activeRefs = msgs, refs
	return t, nil
}

func (t *Tree) addLoaded(entry Entry) error {
	if err := validateEntry(entry); err != nil {
		return err
	}
	if _, exists := t.byID[entry.ID]; exists {
		return fmt.Errorf("duplicate id %q", entry.ID)
	}
	if entry.ParentID != "" {
		if _, ok := t.byID[entry.ParentID]; !ok {
			return fmt.Errorf("missing parent %q", entry.ParentID)
		}
	}
	if err := t.validateEntryLinks(entry); err != nil {
		return err
	}
	t.byID[entry.ID] = len(t.Entries)
	t.Entries = append(t.Entries, entry)
	return nil
}

func (t *Tree) validateEntryLinks(entry Entry) error {
	if entry.Type != EntryCompaction {
		return nil
	}
	if _, ok := t.byID[entry.FirstKeptEntryID]; !ok {
		return fmt.Errorf("compaction retained entry %q not found", entry.FirstKeptEntryID)
	}
	path, err := t.Path(entry.ParentID)
	if err != nil {
		return err
	}
	for _, ancestor := range path {
		if ancestor.ID == entry.FirstKeptEntryID {
			return nil
		}
	}
	return fmt.Errorf("compaction retained entry %q is not an ancestor", entry.FirstKeptEntryID)
}

func validateEntry(entry Entry) error {
	if entry.ID == "" {
		return errors.New("empty entry id")
	}
	if entry.ID == entry.ParentID {
		return fmt.Errorf("entry %q is its own parent", entry.ID)
	}
	switch entry.Type {
	case EntrySegment:
		if len(entry.Messages) == 0 {
			return errors.New("empty segment")
		}
		if err := llm.ValidateTranscript(entry.Messages); err != nil {
			return fmt.Errorf("invalid segment: %w", err)
		}
	case EntryCompaction:
		if entry.Checkpoint == nil || entry.FirstKeptEntryID == "" {
			return errors.New("incomplete compaction entry")
		}
		if entry.Checkpoint.Role != llm.RoleUser || entry.Checkpoint.Origin != llm.MessageOriginCompactionCheckpoint {
			return errors.New("compaction checkpoint must be a compaction-checkpoint user message")
		}
		if err := llm.ValidateTranscript([]llm.Message{*entry.Checkpoint}); err != nil {
			return fmt.Errorf("invalid compaction checkpoint: %w", err)
		}
	case EntryBranch:
		if !entry.WorkspaceUnchanged {
			return errors.New("branch entry must record unchanged workspace")
		}
	case EntryContextReset:
		if entry.ContextDelta == nil {
			if err := llm.ValidateTranscript(entry.Messages); err != nil {
				return fmt.Errorf("invalid context reset: %w", err)
			}
			break
		}
		if len(entry.Messages) > 0 {
			return errors.New("context reset contains both messages and context_delta")
		}
		if err := validateContextDelta(*entry.ContextDelta); err != nil {
			return fmt.Errorf("invalid context reset delta: %w", err)
		}
	default:
		return fmt.Errorf("unknown entry type %q", entry.Type)
	}
	return nil
}

func (t *Tree) nextID() string {
	for {
		id := randomHex(4)
		if _, exists := t.byID[id]; !exists {
			return id
		}
	}
}

func randomHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err == nil {
		return hex.EncodeToString(b)
	}
	return fmt.Sprintf("%x", time.Now().UnixNano())
}

func (t *Tree) appendEntry(entry Entry) error {
	if t.byID == nil {
		t.byID = make(map[string]int)
	}
	if entry.ID == "" {
		entry.ID = t.nextID()
	}
	if entry.Time.IsZero() {
		entry.Time = time.Now()
	}
	if err := validateEntry(entry); err != nil {
		return err
	}
	if entry.ParentID != "" {
		if _, ok := t.byID[entry.ParentID]; !ok {
			return fmt.Errorf("missing parent %q", entry.ParentID)
		}
	}
	if err := t.validateEntryLinks(entry); err != nil {
		return err
	}
	t.byID[entry.ID] = len(t.Entries)
	t.Entries = append(t.Entries, entry)
	t.ActiveLeaf = entry.ID
	return nil
}

// SyncTranscript records transcript additions or a prepared rewrite and makes
// messages the active materialized context.
func (t *Tree) SyncTranscript(messages []llm.Message) error {
	if err := llm.ValidateTranscript(messages); err != nil {
		return fmt.Errorf("session: sync invalid transcript: %w", err)
	}
	if t.byID == nil {
		t.byID = make(map[string]int)
	}

	if t.pending != nil {
		if err := t.commitCompaction(messages); err != nil {
			return err
		}
		return nil
	}

	base := len(t.activeMsgs)
	if base <= len(messages) && transcriptsEqual(t.activeMsgs, messages[:base]) {
		if err := t.appendMessages(messages[base:]); err != nil {
			return err
		}
		t.activeMsgs = cloneMessagesForTree(messages)
		return nil
	}

	return t.appendContextReset(messages, "transcript_rewrite")
}

func transcriptsEqual(a, b []llm.Message) bool {
	if len(a) != len(b) {
		return false
	}
	return len(a) == 0 || reflect.DeepEqual(a, b)
}

func validateContextDelta(delta ContextDelta) error {
	if delta.BaseMessageCount < 0 || delta.ResultMessageCount < 0 {
		return errors.New("negative message count")
	}
	if !validContextDigest(delta.BaseDigest) || !validContextDigest(delta.ResultDigest) {
		return errors.New("malformed message digest")
	}
	cursor := 0
	previousStart := -1
	resultCount := delta.BaseMessageCount
	for i, splice := range delta.Splices {
		if splice.Start < 0 || splice.Delete < 0 || splice.Start > delta.BaseMessageCount || splice.Delete > delta.BaseMessageCount-splice.Start {
			return fmt.Errorf("splice %d is out of bounds", i)
		}
		if splice.Start < cursor || splice.Start == previousStart {
			return fmt.Errorf("splice %d is out of order or overlaps", i)
		}
		cursor = splice.Start + splice.Delete
		previousStart = splice.Start
		resultCount += len(splice.Messages) - splice.Delete
	}
	if resultCount != delta.ResultMessageCount {
		return fmt.Errorf("result message count is %d, want %d", resultCount, delta.ResultMessageCount)
	}
	return nil
}

func validContextDigest(digest string) bool {
	if len(digest) != 64 {
		return false
	}
	decoded, err := hex.DecodeString(digest)
	return err == nil && hex.EncodeToString(decoded) == digest
}

func applyContextDelta(base []llm.Message, baseRefs []messageRef, entry Entry) ([]llm.Message, []messageRef, error) {
	if entry.ContextDelta == nil {
		return nil, nil, errors.New("session: missing context reset delta")
	}
	delta := *entry.ContextDelta
	if err := validateContextDelta(delta); err != nil {
		return nil, nil, fmt.Errorf("session: context reset %q: %w", entry.ID, err)
	}
	if len(base) != delta.BaseMessageCount || len(baseRefs) != len(base) {
		return nil, nil, fmt.Errorf("session: context reset %q base count = %d, want %d", entry.ID, len(base), delta.BaseMessageCount)
	}
	if !llm.MatchesMessageFingerprint(base, delta.BaseDigest) {
		return nil, nil, fmt.Errorf("session: context reset %q base digest mismatch", entry.ID)
	}
	// Replay against the one materialized path buffer. Applying from right to
	// left keeps every splice index relative to the unmodified parent while
	// avoiding a full transcript clone for the common equal-length rewrite.
	msgs := base
	refs := baseRefs
	ownedByDelta := make([]bool, len(base))
	for i := len(delta.Splices) - 1; i >= 0; i-- {
		splice := delta.Splices[i]
		oldLen := len(msgs)
		oldEnd := splice.Start + splice.Delete
		shift := len(splice.Messages) - splice.Delete
		switch {
		case shift > 0:
			msgs = append(msgs, make([]llm.Message, shift)...)
			copy(msgs[splice.Start+len(splice.Messages):], msgs[oldEnd:oldLen])
			refs = append(refs, make([]messageRef, shift)...)
			copy(refs[splice.Start+len(splice.Messages):], refs[oldEnd:oldLen])
			ownedByDelta = append(ownedByDelta, make([]bool, shift)...)
			copy(ownedByDelta[splice.Start+len(splice.Messages):], ownedByDelta[oldEnd:oldLen])
		case shift < 0:
			copy(msgs[splice.Start+len(splice.Messages):], msgs[oldEnd:oldLen])
			copy(refs[splice.Start+len(splice.Messages):], refs[oldEnd:oldLen])
			copy(ownedByDelta[splice.Start+len(splice.Messages):], ownedByDelta[oldEnd:oldLen])
			newLen := oldLen + shift
			clear(msgs[newLen:])
			clear(refs[newLen:])
			clear(ownedByDelta[newLen:])
			msgs = msgs[:newLen]
			refs = refs[:newLen]
			ownedByDelta = ownedByDelta[:newLen]
		}
		replacements := cloneMessagesForTree(splice.Messages)
		copy(msgs[splice.Start:], replacements)
		for j := range replacements {
			ownedByDelta[splice.Start+j] = true
		}
	}
	if len(msgs) != delta.ResultMessageCount {
		return nil, nil, fmt.Errorf("session: context reset %q result count = %d, want %d", entry.ID, len(msgs), delta.ResultMessageCount)
	}
	if !llm.MatchesMessageFingerprint(msgs, delta.ResultDigest) {
		return nil, nil, fmt.Errorf("session: context reset %q result digest mismatch", entry.ID)
	}
	for i, owned := range ownedByDelta {
		if owned {
			refs[i] = messageRef{entryID: entry.ID, offset: i}
		}
	}
	if err := llm.ValidateTranscript(msgs); err != nil {
		return nil, nil, fmt.Errorf("session: context reset %q result invalid: %w", entry.ID, err)
	}
	return msgs, refs, nil
}

func resetMessageRefs(entryID string, count int) []messageRef {
	refs := make([]messageRef, count)
	for i := range refs {
		refs[i] = messageRef{entryID: entryID, offset: i}
	}
	return refs
}

func (t *Tree) appendMessages(messages []llm.Message) error {
	for i := 0; i < len(messages); {
		end := i + 1
		if hasToolUseMessage(messages[i]) {
			if end >= len(messages) {
				return errors.New("session: tool-use message missing result while syncing tree")
			}
			end++
		}
		segment := cloneMessagesForTree(messages[i:end])
		entry := Entry{Type: EntrySegment, ParentID: t.ActiveLeaf, Messages: segment, Time: segment[0].Time}
		if err := t.appendEntry(entry); err != nil {
			return err
		}
		id := t.ActiveLeaf
		for offset := range segment {
			t.activeRefs = append(t.activeRefs, messageRef{entryID: id, offset: offset})
		}
		i = end
	}
	return nil
}

func hasToolUseMessage(m llm.Message) bool {
	if m.Role != llm.RoleAssistant {
		return false
	}
	for _, block := range m.Content {
		if block.Kind == llm.BlockToolUse {
			return true
		}
	}
	return false
}

// PrepareCompaction records the rewrite metadata supplied by the compaction
// archiver. The next SyncTranscript commits the corresponding tree entry.
func (t *Tree) PrepareCompaction(before []llm.Message, olderCount int, summary, archiveRef string, tokensBefore int, focus string, readFiles, modifiedFiles []string) error {
	if t.pending != nil {
		if err := t.SyncTranscript(before); err != nil {
			return err
		}
	}
	if err := t.SyncTranscript(before); err != nil {
		return err
	}
	if olderCount < 0 || olderCount >= len(before) || olderCount >= len(t.activeRefs) {
		return fmt.Errorf("session: invalid compaction boundary %d for %d messages", olderCount, len(before))
	}
	t.pending = &pendingCompaction{
		olderCount:    olderCount,
		summary:       summary,
		archiveRef:    archiveRef,
		tokensBefore:  tokensBefore,
		focus:         strings.TrimSpace(focus),
		readFiles:     append([]string(nil), readFiles...),
		modifiedFiles: append([]string(nil), modifiedFiles...),
	}
	return nil
}

func (t *Tree) commitCompaction(messages []llm.Message) error {
	p := t.pending
	t.pending = nil
	if p == nil {
		return errors.New("session: missing compacted transcript")
	}
	if transcriptsEqual(messages, t.activeMsgs) {
		return nil
	}
	if len(messages) == 0 || messages[0].Origin != llm.MessageOriginCompactionCheckpoint {
		return t.SyncTranscript(messages)
	}
	ref := t.activeRefs[p.olderCount]
	materializeBoundary := false
	if ref.offset != 0 {
		idx, ok := t.byID[ref.entryID]
		if !ok {
			return fmt.Errorf("session: compaction boundary entry %q not found", ref.entryID)
		}
		if t.Entries[idx].Type != EntryContextReset {
			return errors.New("session: compaction boundary splits an atomic segment")
		}
		// A context reset may contain a wholesale transcript rewrite. A valid
		// turn boundary can therefore fall inside its entry, but there is no
		// standalone original-tree node for the retained suffix to link to.
		materializeBoundary = true
	}
	checkpoint := cloneMessagesForTree(messages[:1])[0]
	summarySource := ""
	fallbackReason := ""
	if checkpoint.Compaction != nil {
		summarySource = checkpoint.Compaction.SummarySource
		fallbackReason = checkpoint.Compaction.FallbackReason
	}
	keptCount := len(t.activeMsgs) - p.olderCount
	baseLen := 1 + keptCount
	if baseLen > len(messages) {
		return fmt.Errorf("session: compacted transcript too short: got %d want at least %d", len(messages), baseLen)
	}
	materializedKept := materializeBoundary || !transcriptsEqual(messages[1:baseLen], t.activeMsgs[p.olderCount:])
	entry := Entry{
		Type:             EntryCompaction,
		ParentID:         t.ActiveLeaf,
		Time:             checkpoint.Time,
		Checkpoint:       &checkpoint,
		FirstKeptEntryID: ref.entryID,
		ArchiveRef:       p.archiveRef,
		Summary:          p.summary,
		TokensBefore:     p.tokensBefore,
		SummarySource:    summarySource,
		FallbackReason:   fallbackReason,
		CustomFocus:      p.focus,
		ReadFiles:        append([]string(nil), p.readFiles...),
		ModifiedFiles:    append([]string(nil), p.modifiedFiles...),
		MaterializedKept: materializedKept,
	}
	if err := t.appendEntry(entry); err != nil {
		return err
	}
	compactionID := t.ActiveLeaf
	if materializedKept {
		t.activeRefs = []messageRef{{entryID: compactionID}}
		t.activeMsgs = cloneMessagesForTree(messages[:1])
		if err := t.appendMessages(messages[1:]); err != nil {
			return err
		}
		t.activeMsgs = cloneMessagesForTree(messages)
		return nil
	}
	refs := make([]messageRef, 0, len(messages))
	refs = append(refs, messageRef{entryID: compactionID})
	refs = append(refs, t.activeRefs[p.olderCount:]...)
	t.activeRefs = refs
	t.activeMsgs = cloneMessagesForTree(messages[:baseLen])
	if err := t.appendMessages(messages[baseLen:]); err != nil {
		return err
	}
	t.activeMsgs = cloneMessagesForTree(messages)
	return nil
}

// AppendContextReset records a deliberate wholesale context replacement.
func (t *Tree) AppendContextReset(messages []llm.Message, reason string) error {
	if err := llm.ValidateTranscript(messages); err != nil {
		return err
	}
	return t.appendContextReset(messages, reason)
}

func (t *Tree) appendContextReset(messages []llm.Message, reason string) error {
	parentMsgs, _, trustedParent := t.materializedActiveContext()
	if trustedParent && transcriptsEqual(parentMsgs, messages) {
		return nil
	}
	entry := Entry{
		Type: EntryContextReset, ParentID: t.ActiveLeaf,
		Messages: cloneMessagesForTree(messages), Reason: reason,
	}
	if len(messages) > 0 {
		entry.Time = messages[0].Time
	}
	if err := t.appendEntry(entry); err != nil {
		return err
	}
	t.activeMsgs = cloneMessagesForTree(messages)
	t.activeRefs = resetMessageRefs(t.ActiveLeaf, len(messages))
	return nil
}

func (t *Tree) materializedActiveContext() ([]llm.Message, []messageRef, bool) {
	msgs, refs, err := t.buildContext(t.ActiveLeaf)
	if err != nil || !transcriptsEqual(msgs, t.activeMsgs) || !reflect.DeepEqual(refs, t.activeRefs) {
		return nil, nil, false
	}
	return msgs, refs, true
}

// AppendBranch moves to targetParent and creates a persistent branch marker.
func (t *Tree) AppendBranch(targetParent, fromLeaf, common, summary, customFocus string) (string, error) {
	if targetParent != "" {
		if _, ok := t.byID[targetParent]; !ok {
			return "", fmt.Errorf("session: branch target %q not found", targetParent)
		}
	}
	entry := Entry{
		Type:               EntryBranch,
		ParentID:           targetParent,
		FromLeafID:         fromLeaf,
		CommonAncestor:     common,
		Summary:            strings.TrimSpace(summary),
		CustomFocus:        strings.TrimSpace(customFocus),
		WorkspaceUnchanged: true,
	}
	if err := t.appendEntry(entry); err != nil {
		return "", err
	}
	msgs, refs, err := t.buildContext(t.ActiveLeaf)
	if err != nil {
		return "", err
	}
	t.activeMsgs, t.activeRefs = msgs, refs
	return t.ActiveLeaf, nil
}

// SetLeaf selects an existing safe entry without adding a marker. It is used
// when extracting branches into a separate session.
func (t *Tree) SetLeaf(id string) error {
	if id != "" {
		if _, ok := t.byID[id]; !ok {
			return fmt.Errorf("session: leaf %q not found", id)
		}
	}
	msgs, refs, err := t.buildContext(id)
	if err != nil {
		return err
	}
	t.ActiveLeaf, t.activeMsgs, t.activeRefs = id, msgs, refs
	return nil
}

// Extract creates a new session tree containing only the path to leaf. Entry
// IDs are preserved so the child session can name its exact parent point while
// subsequently growing an independent append-only tree.
func (t *Tree) Extract(leaf string, created time.Time, cwd string) (*Tree, error) {
	path, err := t.Path(leaf)
	if err != nil {
		return nil, err
	}
	out := NewTree(created, cwd, t.Header.ID, leaf)
	for _, entry := range path {
		if err := out.addLoaded(cloneEntry(entry)); err != nil {
			return nil, err
		}
	}
	out.ActiveLeaf = leaf
	msgs, refs, err := out.buildContext(leaf)
	if err != nil {
		return nil, err
	}
	out.activeMsgs, out.activeRefs = msgs, refs
	return out, nil
}

// BuildContext returns the provider-neutral transcript for the active leaf.
func (t *Tree) BuildContext() ([]llm.Message, error) {
	msgs, _, err := t.buildContext(t.ActiveLeaf)
	return msgs, err
}

func (t *Tree) buildContext(leaf string) ([]llm.Message, []messageRef, error) {
	path, err := t.Path(leaf)
	if err != nil {
		return nil, nil, err
	}
	var msgs []llm.Message
	var refs []messageRef
	start := 0
	// A snapshot reset and a compaction with an explicitly materialized kept
	// suffix are self-contained anchors. Starting at the newest such entry avoids
	// replaying the full path while still applying every later legacy delta once.
	for i := len(path) - 1; i >= 0; i-- {
		entry := path[i]
		switch {
		case entry.Type == EntryContextReset && entry.ContextDelta == nil:
			msgs = cloneMessagesForTree(entry.Messages)
			refs = resetMessageRefs(entry.ID, len(entry.Messages))
			start = i + 1
		case entry.Type == EntryCompaction && entry.MaterializedKept:
			checkpoint, err := contextCheckpoint(entry)
			if err != nil {
				return nil, nil, err
			}
			msgs = []llm.Message{checkpoint}
			refs = []messageRef{{entryID: entry.ID}}
			start = i + 1
		default:
			continue
		}
		break
	}

	for i := start; i < len(path); i++ {
		entry := path[i]
		switch entry.Type {
		case EntrySegment:
			msgs = append(msgs, cloneMessagesForTree(entry.Messages)...)
			for offset := range entry.Messages {
				refs = append(refs, messageRef{entryID: entry.ID, offset: offset})
			}
		case EntryBranch:
			text := "=== Conversation branch ===\nThe conversation moved to an earlier point. The working directory was not reverted; inspect current files before assuming their state."
			if entry.Summary != "" {
				text += "\n\nSummary of the branch that was left:\n" + entry.Summary
			}
			msgs = append(msgs, llm.Message{Role: llm.RoleUser, Time: entry.Time, Origin: llm.MessageOriginInternal, Content: []llm.ContentBlock{{Kind: llm.BlockText, Text: text}}})
			refs = append(refs, messageRef{entryID: entry.ID})
		case EntryContextReset:
			if entry.ContextDelta == nil {
				msgs = cloneMessagesForTree(entry.Messages)
				refs = resetMessageRefs(entry.ID, len(entry.Messages))
				continue
			}
			msgs, refs, err = applyContextDelta(msgs, refs, entry)
			if err != nil {
				return nil, nil, err
			}
		case EntryCompaction:
			checkpoint, err := contextCheckpoint(entry)
			if err != nil {
				return nil, nil, err
			}
			if entry.MaterializedKept {
				msgs = []llm.Message{checkpoint}
				refs = []messageRef{{entryID: entry.ID}}
				continue
			}
			keptAt := -1
			for j, ref := range refs {
				if ref.entryID == entry.FirstKeptEntryID && ref.offset == 0 {
					keptAt = j
					break
				}
			}
			if keptAt < 0 {
				return nil, nil, fmt.Errorf("session: compaction %q retained entry %q is not materialized on its parent path", entry.ID, entry.FirstKeptEntryID)
			}
			msgs = append([]llm.Message{checkpoint}, cloneMessagesForTree(msgs[keptAt:])...)
			refs = append([]messageRef{{entryID: entry.ID}}, refs[keptAt:]...)
		}
	}
	if err := llm.ValidateTranscript(msgs); err != nil {
		return nil, nil, fmt.Errorf("session: active tree context invalid: %w", err)
	}
	return cloneMessagesForTree(msgs), refs, nil
}

func contextCheckpoint(entry Entry) (llm.Message, error) {
	if entry.Checkpoint == nil {
		return llm.Message{}, fmt.Errorf("session: compaction %q has no checkpoint", entry.ID)
	}
	checkpoint := cloneMessagesForTree([]llm.Message{*entry.Checkpoint})[0]
	readFilesOmitted := 0
	if checkpoint.Compaction != nil {
		readFilesOmitted = checkpoint.Compaction.ReadFilesOmitted
	}
	checkpoint.Compaction = &llm.CompactionMetadata{
		Summary:          entry.Summary,
		SummarySource:    entry.SummarySource,
		FallbackReason:   entry.FallbackReason,
		Focus:            entry.CustomFocus,
		ReadFiles:        append([]string(nil), entry.ReadFiles...),
		ReadFilesOmitted: readFilesOmitted,
		ModifiedFiles:    append([]string(nil), entry.ModifiedFiles...),
	}
	return checkpoint, nil
}

// Path returns entries from the root to id. An empty id means an empty path.
func (t *Tree) Path(id string) ([]Entry, error) {
	if id == "" {
		return nil, nil
	}
	idx, ok := t.byID[id]
	if !ok {
		return nil, fmt.Errorf("session: entry %q not found", id)
	}
	var reverse []Entry
	seen := make(map[string]bool)
	for {
		entry := t.Entries[idx]
		if seen[entry.ID] {
			return nil, fmt.Errorf("session: cycle at entry %q", entry.ID)
		}
		seen[entry.ID] = true
		reverse = append(reverse, entry)
		if entry.ParentID == "" {
			break
		}
		var exists bool
		idx, exists = t.byID[entry.ParentID]
		if !exists {
			return nil, fmt.Errorf("session: missing parent %q", entry.ParentID)
		}
	}
	for i, j := 0, len(reverse)-1; i < j; i, j = i+1, j-1 {
		reverse[i], reverse[j] = reverse[j], reverse[i]
	}
	return reverse, nil
}

// CommonAncestor returns the deepest shared entry of two paths.
func (t *Tree) CommonAncestor(a, b string) (string, error) {
	pa, err := t.Path(a)
	if err != nil {
		return "", err
	}
	pb, err := t.Path(b)
	if err != nil {
		return "", err
	}
	var common string
	for i := 0; i < len(pa) && i < len(pb) && pa[i].ID == pb[i].ID; i++ {
		common = pa[i].ID
	}
	return common, nil
}

// DivergentMessages returns model-visible material after common on the old path.
func (t *Tree) DivergentMessages(oldLeaf, common string) ([]llm.Message, error) {
	path, err := t.Path(oldLeaf)
	if err != nil {
		return nil, err
	}
	positions := make(map[string]int, len(path))
	commonPosition := -1
	for i, entry := range path {
		positions[entry.ID] = i
		if entry.ID == common {
			commonPosition = i
		}
	}
	messages, refs, err := t.buildContext(oldLeaf)
	if err != nil {
		return nil, err
	}
	var out []llm.Message
	for i, message := range messages {
		if positions[refs[i].entryID] > commonPosition {
			out = append(out, message)
		}
	}
	return cloneMessagesForTree(out), nil
}

// Entry returns a copy of one entry.
func (t *Tree) Entry(id string) (Entry, bool) {
	idx, ok := t.byID[id]
	if !ok {
		return Entry{}, false
	}
	return t.Entries[idx], true
}

// Nodes returns all entries as a deterministic hierarchy.
func (t *Tree) Nodes() []*TreeNode {
	nodes := make(map[string]*TreeNode, len(t.Entries))
	var roots []*TreeNode
	for _, entry := range t.Entries {
		nodes[entry.ID] = &TreeNode{Entry: entry}
	}
	for _, entry := range t.Entries {
		node := nodes[entry.ID]
		if parent := nodes[entry.ParentID]; parent != nil {
			parent.Children = append(parent.Children, node)
		} else {
			roots = append(roots, node)
		}
	}
	var walk func([]*TreeNode, int)
	walk = func(items []*TreeNode, depth int) {
		sort.SliceStable(items, func(i, j int) bool { return items[i].Entry.Time.Before(items[j].Entry.Time) })
		for _, node := range items {
			node.Depth = depth
			walk(node.Children, depth+1)
		}
	}
	walk(roots, 0)
	return roots
}

// HumanPromptText returns editable text for a user-authored segment.
func HumanPromptText(entry Entry) (string, []llm.ContentBlock, bool) {
	if entry.Type != EntrySegment || len(entry.Messages) != 1 {
		return "", nil, false
	}
	m := entry.Messages[0]
	if m.Role != llm.RoleUser || m.Origin != llm.MessageOriginPrompt && m.Origin != llm.MessageOriginSteer {
		return "", nil, false
	}
	var textParts []string
	var images []llm.ContentBlock
	for _, block := range m.Content {
		switch block.Kind {
		case llm.BlockText:
			textParts = append(textParts, block.Text)
		case llm.BlockImage:
			images = append(images, block)
		}
	}
	return strings.Join(textParts, ""), images, true
}

// Save appends new entries to tree.ndjson. The initial write uses temp+rename;
// later writes append immutable records.
func (t *Tree) Save(dir string) error {
	if dir == "" {
		return nil
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("session: create dir: %w", err)
	}
	path := filepath.Join(dir, treeFile)
	if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
		var b bytes.Buffer
		enc := json.NewEncoder(&b)
		if err := enc.Encode(t.Header); err != nil {
			return err
		}
		for _, entry := range t.Entries {
			if err := enc.Encode(entry); err != nil {
				return err
			}
		}
		tmp := path + ".tmp"
		if err := os.WriteFile(tmp, b.Bytes(), 0o644); err != nil {
			return fmt.Errorf("session: write tree temp: %w", err)
		}
		if err := os.Rename(tmp, path); err != nil {
			_ = os.Remove(tmp)
			return fmt.Errorf("session: rename tree: %w", err)
		}
		t.flushed = len(t.Entries)
		t.flushedPath = filepath.Clean(path)
		return nil
	} else if err != nil {
		return err
	}
	if err := repairTreeTail(path); err != nil {
		return err
	}
	diskCount := t.flushed
	if t.flushedPath != filepath.Clean(path) || t.needsInspect {
		disk, err := LoadTree(dir, "")
		if err != nil {
			return fmt.Errorf("session: inspect existing tree: %w", err)
		}
		if disk.Header.ID != t.Header.ID {
			return fmt.Errorf("session: tree id mismatch at %s (%q != %q)", path, disk.Header.ID, t.Header.ID)
		}
		if len(disk.Entries) > len(t.Entries) {
			return fmt.Errorf("session: existing tree has %d entries, in-memory tree has %d", len(disk.Entries), len(t.Entries))
		}
		for i := range disk.Entries {
			if !entriesEqualOnDisk(disk.Entries[i], t.Entries[i]) {
				return fmt.Errorf("session: existing tree diverges at entry %d", i+1)
			}
		}
		diskCount = len(disk.Entries)
		t.needsInspect = false
	}
	if diskCount >= len(t.Entries) {
		t.flushed = diskCount
		t.flushedPath = filepath.Clean(path)
		return nil
	}
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	for _, entry := range t.Entries[diskCount:] {
		if err := enc.Encode(entry); err != nil {
			return err
		}
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return fmt.Errorf("session: open tree append: %w", err)
	}
	_, writeErr := io.Copy(f, &b)
	if writeErr == nil {
		writeErr = f.Sync()
	}
	closeErr := f.Close()
	if writeErr != nil {
		t.needsInspect = true
		return fmt.Errorf("session: append tree: %w", writeErr)
	}
	if closeErr != nil {
		t.needsInspect = true
		return fmt.Errorf("session: close tree: %w", closeErr)
	}
	t.flushed = len(t.Entries)
	t.flushedPath = filepath.Clean(path)
	return nil
}

func entriesEqualOnDisk(a, b Entry) bool {
	aJSON, aErr := json.Marshal(a)
	bJSON, bErr := json.Marshal(b)
	return aErr == nil && bErr == nil && bytes.Equal(aJSON, bJSON)
}

// repairTreeTail makes a subsequent append safe after a crash. Harness writes
// newline-terminated records, so a non-terminated valid record gets its missing
// newline and a non-terminated invalid record is truncated to the previous
// complete boundary.
func repairTreeTail(path string) error {
	f, err := os.OpenFile(path, os.O_RDWR, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return err
	}
	size := info.Size()
	if size == 0 {
		return errors.New("session: empty tree file")
	}
	recordEnd := size
	var one [1]byte
	for recordEnd > 0 {
		if _, err := f.ReadAt(one[:], recordEnd-1); err != nil {
			return err
		}
		if one[0] != '\n' && one[0] != '\r' {
			break
		}
		recordEnd--
	}
	if recordEnd == 0 {
		return errors.New("session: tree file contains no records")
	}
	start := int64(0)
	for cursor := recordEnd; cursor > 0; {
		chunkStart := max(cursor-4096, int64(0))
		buf := make([]byte, cursor-chunkStart)
		if _, err := f.ReadAt(buf, chunkStart); err != nil {
			return err
		}
		if idx := bytes.LastIndexByte(buf, '\n'); idx >= 0 {
			start = chunkStart + int64(idx) + 1
			break
		}
		cursor = chunkStart
	}
	record := make([]byte, recordEnd-start)
	if _, err := f.ReadAt(record, start); err != nil {
		return err
	}
	var raw json.RawMessage
	if json.Unmarshal(bytes.TrimSpace(record), &raw) == nil {
		if recordEnd < size {
			return nil
		}
		if _, err := f.WriteAt([]byte{'\n'}, size); err != nil {
			return fmt.Errorf("session: terminate tree record: %w", err)
		}
		return f.Sync()
	}
	if start == 0 {
		return errors.New("session: invalid tree header")
	}
	if err := f.Truncate(start); err != nil {
		return fmt.Errorf("session: truncate interrupted tree record: %w", err)
	}
	if err := f.Sync(); err != nil {
		return fmt.Errorf("session: sync repaired tree: %w", err)
	}
	return nil
}

func cloneMessagesForTree(messages []llm.Message) []llm.Message {
	out := make([]llm.Message, len(messages))
	for i := range messages {
		out[i] = messages[i]
		out[i].Content = append([]llm.ContentBlock(nil), messages[i].Content...)
		for j := range out[i].Content {
			out[i].Content[j].ResultContent = append([]llm.ContentBlock(nil), messages[i].Content[j].ResultContent...)
		}
		out[i].ParallelToolBatches = append([]llm.ParallelToolBatch(nil), messages[i].ParallelToolBatches...)
		for j := range out[i].ParallelToolBatches {
			out[i].ParallelToolBatches[j].ToolUseIDs = append([]string(nil), messages[i].ParallelToolBatches[j].ToolUseIDs...)
		}
		if messages[i].Compaction != nil {
			meta := *messages[i].Compaction
			meta.ReadFiles = append([]string(nil), messages[i].Compaction.ReadFiles...)
			meta.ModifiedFiles = append([]string(nil), messages[i].Compaction.ModifiedFiles...)
			out[i].Compaction = &meta
		}
	}
	return out
}

func cloneEntry(entry Entry) Entry {
	entry.Messages = cloneMessagesForTree(entry.Messages)
	if entry.ContextDelta != nil {
		delta := *entry.ContextDelta
		delta.Splices = append([]ContextSplice(nil), entry.ContextDelta.Splices...)
		for i := range delta.Splices {
			delta.Splices[i].Messages = cloneMessagesForTree(entry.ContextDelta.Splices[i].Messages)
		}
		entry.ContextDelta = &delta
	}
	entry.ReadFiles = append([]string(nil), entry.ReadFiles...)
	entry.ModifiedFiles = append([]string(nil), entry.ModifiedFiles...)
	if entry.Checkpoint != nil {
		checkpoint := cloneMessagesForTree([]llm.Message{*entry.Checkpoint})[0]
		entry.Checkpoint = &checkpoint
	}
	return entry
}
