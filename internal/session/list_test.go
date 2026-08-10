package session

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

func writeListTestState(t *testing.T, root, name, id, cwd string, created, updated time.Time) string {
	t.Helper()
	dir := filepath.Join(root, name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(struct {
		Version int       `json:"version"`
		ID      string    `json:"id"`
		CWD     string    `json:"cwd,omitempty"`
		Created time.Time `json:"created,omitempty"`
		Updated time.Time `json:"updated,omitempty"`
	}{Version: Version, ID: id, CWD: cwd, Created: created, Updated: updated})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, stateFile), data, 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestListMissingAndEmptyRoot(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "missing")
	summaries, skipped, err := List(missing, ListOptions{All: true})
	if err != nil || len(summaries) != 0 || len(skipped) != 0 {
		t.Fatalf("List missing = %v, %v, %v; want empty", summaries, skipped, err)
	}

	empty := t.TempDir()
	summaries, skipped, err = List(empty, ListOptions{All: true})
	if err != nil || len(summaries) != 0 || len(skipped) != 0 {
		t.Fatalf("List empty = %v, %v, %v; want empty", summaries, skipped, err)
	}
}

func TestListIncludesOnlyImmediateRealSessionDirectories(t *testing.T) {
	root := t.TempDir()
	valid := writeListTestState(t, root, "valid", "valid-id", "/work", time.Now(), time.Now())
	writeListTestState(t, filepath.Join(root, "container"), "nested", "nested-id", "/work", time.Now(), time.Now())
	if err := os.WriteFile(filepath.Join(root, "ordinary-file"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	if runtime.GOOS != "windows" {
		if err := os.Symlink(valid, filepath.Join(root, "linked-session")); err != nil {
			t.Fatal(err)
		}
		linkedStateDir := filepath.Join(root, "linked-state")
		if err := os.Mkdir(linkedStateDir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(filepath.Join(valid, stateFile), filepath.Join(linkedStateDir, stateFile)); err != nil {
			t.Fatal(err)
		}
	}

	summaries, skipped, err := List(root, ListOptions{All: true})
	if err != nil || len(skipped) != 0 {
		t.Fatalf("List = skipped %v, err %v", skipped, err)
	}
	if len(summaries) != 1 || summaries[0].Path != valid {
		t.Fatalf("List summaries = %+v, want only %s", summaries, valid)
	}
}

func TestListFiltersByCleanAbsoluteCWD(t *testing.T) {
	root := t.TempDir()
	project := filepath.Join(t.TempDir(), "project")
	other := filepath.Join(t.TempDir(), "other")
	writeListTestState(t, root, "same", "same", filepath.Join(project, "."), time.Now(), time.Now())
	writeListTestState(t, root, "other", "other", other, time.Now(), time.Now())
	writeListTestState(t, root, "empty", "empty", "", time.Now(), time.Now())

	summaries, skipped, err := List(root, ListOptions{CWD: filepath.Join(project, "child", "..")})
	if err != nil || len(skipped) != 0 {
		t.Fatalf("List filtered = skipped %v, err %v", skipped, err)
	}
	if len(summaries) != 1 || summaries[0].ID != "same" {
		t.Fatalf("List filtered = %+v, want same", summaries)
	}

	summaries, skipped, err = List(root, ListOptions{})
	if err != nil || len(skipped) != 0 || len(summaries) != 0 {
		t.Fatalf("List empty CWD = %+v, %v, %v; want no matches", summaries, skipped, err)
	}

	summaries, skipped, err = List(root, ListOptions{All: true})
	if err != nil || len(skipped) != 0 || len(summaries) != 3 {
		t.Fatalf("List all = %+v, %v, %v; want three", summaries, skipped, err)
	}
}

func TestListSortsNewestFirstWithPathTieBreak(t *testing.T) {
	root := t.TempDir()
	base := time.Date(2026, 8, 10, 12, 0, 0, 0, time.UTC)
	writeListTestState(t, root, "z-old", "old", "/work", base, base)
	a := writeListTestState(t, root, "a-new", "a", "/work", base.Add(time.Hour), base.Add(time.Hour))
	b := writeListTestState(t, root, "b-new", "b", "/work", base.Add(time.Hour), base.Add(time.Hour))

	summaries, _, err := List(root, ListOptions{All: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(summaries) != 3 || summaries[0].Path != a || summaries[1].Path != b || summaries[2].ID != "old" {
		t.Fatalf("List order = %+v", summaries)
	}
}

func TestListExtractsAndBoundsFirstUserPrompt(t *testing.T) {
	root := t.TempDir()
	now := time.Now()
	first := writeListTestState(t, root, "first", "first", "/work", now, now)
	if err := AppendEvent(first, Event{Type: EventNotice, Text: "before"}); err != nil {
		t.Fatal(err)
	}
	if err := AppendEvent(first, Event{Type: EventUser, Purpose: "internal", Text: "first\n\tprompt"}); err != nil {
		t.Fatal(err)
	}
	if err := AppendEvent(first, Event{Type: EventUser, Text: "second"}); err != nil {
		t.Fatal(err)
	}

	writeListTestState(t, root, "missing", "missing", "/work", now, now)
	empty := writeListTestState(t, root, "empty", "empty", "/work", now, now)
	if err := os.WriteFile(filepath.Join(empty, eventLog), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	malformed := writeListTestState(t, root, "malformed", "malformed", "/work", now, now)
	if err := os.WriteFile(filepath.Join(malformed, eventLog), []byte("{not-json}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	large := writeListTestState(t, root, "large", "large", "/work", now, now)
	largePrompt := strings.Repeat("界", maxSummaryPromptRunes+100)
	if err := AppendEvent(large, Event{Type: EventUser, Text: largePrompt}); err != nil {
		t.Fatal(err)
	}

	summaries, skipped, err := List(root, ListOptions{All: true, IncludePrompt: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(summaries) != 5 {
		t.Fatalf("List summaries = %d, want 5; skipped=%v", len(summaries), skipped)
	}
	byID := make(map[string]Summary, len(summaries))
	for _, summary := range summaries {
		byID[summary.ID] = summary
	}
	if got := byID["first"].InitialPrompt; got != "first\n\tprompt" {
		t.Fatalf("first prompt = %q", got)
	}
	if byID["missing"].InitialPrompt != "" || byID["empty"].InitialPrompt != "" || byID["malformed"].InitialPrompt != "" {
		t.Fatalf("empty prompt summaries = missing %q empty %q malformed %q", byID["missing"].InitialPrompt, byID["empty"].InitialPrompt, byID["malformed"].InitialPrompt)
	}
	bounded := byID["large"].InitialPrompt
	if utf8.RuneCountInString(bounded) != maxSummaryPromptRunes || !utf8.ValidString(bounded) {
		t.Fatalf("bounded prompt has %d runes, valid=%v", utf8.RuneCountInString(bounded), utf8.ValidString(bounded))
	}
	if len(skipped) != 1 || !strings.Contains(skipped[0].Error(), filepath.Join(malformed, eventLog)) {
		t.Fatalf("skipped = %v, want malformed replay warning", skipped)
	}
}

func TestListSkipsBadStatesWithoutHidingValidSessions(t *testing.T) {
	root := t.TempDir()
	valid := writeListTestState(t, root, "valid", "valid", "/work", time.Now(), time.Now())
	badCases := map[string][]byte{
		"malformed":   []byte("{"),
		"unsupported": []byte(`{"version":6,"id":"old"}`),
		"missing-id":  []byte(`{"version":7}`),
		"oversized":   bytes.Repeat([]byte("x"), maxListStateBytes+1),
	}
	for name, data := range badCases {
		dir := filepath.Join(root, name)
		if err := os.Mkdir(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, stateFile), data, 0o644); err != nil {
			t.Fatal(err)
		}
	}

	summaries, skipped, err := List(root, ListOptions{All: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(summaries) != 1 || summaries[0].Path != valid {
		t.Fatalf("valid summaries = %+v", summaries)
	}
	if len(skipped) != len(badCases) {
		t.Fatalf("skipped = %v, want %d entries", skipped, len(badCases))
	}
	var unsupported *UnsupportedSchemaVersionError
	for _, skip := range skipped {
		if errors.As(skip, &unsupported) {
			break
		}
	}
	if unsupported == nil || unsupported.Got != 6 || unsupported.Want != Version {
		t.Fatalf("unsupported schema skip = %#v, want typed 6 -> %d error", unsupported, Version)
	}
}

func TestListDoesNotCreateRepairOrRewriteSessionFiles(t *testing.T) {
	root := t.TempDir()
	dir := writeListTestState(t, root, "session", "id", "/work", time.Now(), time.Now())
	if err := AppendEvent(dir, Event{Type: EventUser, Text: "hello"}); err != nil {
		t.Fatal(err)
	}
	checkpointPath := filepath.Join(dir, activeTurnFile)
	checkpoint := []byte("stale checkpoint that Load would inspect")
	if err := os.WriteFile(checkpointPath, checkpoint, 0o600); err != nil {
		t.Fatal(err)
	}
	paths := []string{filepath.Join(dir, stateFile), filepath.Join(dir, eventLog), checkpointPath}
	before := make(map[string][]byte, len(paths))
	beforeInfo := make(map[string]os.FileInfo, len(paths))
	for _, path := range paths {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		before[path] = data
		beforeInfo[path], err = os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
	}

	summaries, skipped, err := List(root, ListOptions{All: true, IncludePrompt: true, ProbeActivity: true})
	if err != nil || len(skipped) != 0 || len(summaries) != 1 || summaries[0].Activity != ActivityInactive {
		t.Fatalf("List = %+v, %v, %v", summaries, skipped, err)
	}
	if _, err := os.Lstat(filepath.Join(dir, lockFile)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("listing created a lock: %v", err)
	}
	for _, path := range paths {
		after, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(after, before[path]) || info.Mode() != beforeInfo[path].Mode() || info.Size() != beforeInfo[path].Size() || !info.ModTime().Equal(beforeInfo[path].ModTime()) {
			t.Fatalf("listing changed %s", path)
		}
	}
}

func TestDefaultRootAndPathsShareCanonicalRoot(t *testing.T) {
	stateDir := filepath.Join(t.TempDir(), "state")
	at := time.Date(2026, 8, 10, 12, 34, 56, 0, time.UTC)
	root := DefaultRoot(stateDir)
	if root != filepath.Join(stateDir, "harness", "sessions") {
		t.Fatalf("DefaultRoot = %q", root)
	}
	if filepath.Dir(DefaultPath(stateDir, at)) != root || filepath.Dir(DefaultPathForID(stateDir, at, "abcdefghijk")) != root {
		t.Fatalf("default paths do not use %q", root)
	}
}
