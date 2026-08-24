package main

import (
	"bufio"
	"bytes"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"harness/internal/cli"
	"harness/internal/llm"
	"harness/internal/llm/llmtest"
	"harness/internal/session"
	"harness/internal/ui"
)

func sessionCommandEnv(t *testing.T, args []string, input string) (environment, *bytes.Buffer, *bytes.Buffer, string) {
	t.Helper()
	state := filepath.Join(t.TempDir(), "state")
	getenv := func(name string) string {
		if name == "XDG_STATE_HOME" {
			return state
		}
		return ""
	}
	var stdout, stderr bytes.Buffer
	return environment{
		args:       args,
		stdin:      strings.NewReader(input),
		stdout:     &stdout,
		stderr:     &stderr,
		getenv:     getenv,
		now:        time.Now,
		stdinPiped: false,
	}, &stdout, &stderr, session.DefaultRoot(state)
}

func saveSessionCommandFixture(t *testing.T, dir, cwd string, created, updated time.Time, prompt string) {
	t.Helper()
	state := session.Session{
		Provider: "anthropic",
		Model:    "claude-opus-4-8",
		CWD:      cwd,
		Created:  created,
		Updated:  updated,
	}
	if err := state.Save(dir); err != nil {
		t.Fatalf("save session %s: %v", dir, err)
	}
	if prompt != "" {
		if err := session.AppendEvent(dir, session.Event{Type: session.EventUser, Text: prompt}); err != nil {
			t.Fatalf("append prompt %s: %v", dir, err)
		}
	}
}

func saveResumableCommandFixture(t *testing.T, dir, history string) session.Session {
	t.Helper()
	state := session.Session{
		Provider: "anthropic",
		Model:    "claude-opus-4-8",
		CWD:      mustSessionCommandCWD(t),
		Created:  time.Date(2026, 8, 10, 12, 0, 0, 0, time.UTC),
		Updated:  time.Date(2026, 8, 10, 12, 1, 0, 0, time.UTC),
	}
	if history != "" {
		state.Messages = []llm.Message{
			{Role: llm.RoleUser, Content: []llm.ContentBlock{{Kind: llm.BlockText, Text: history}}},
			{Role: llm.RoleAssistant, Content: []llm.ContentBlock{{Kind: llm.BlockText, Text: "prior reply"}}},
		}
	}
	if err := state.Save(dir); err != nil {
		t.Fatalf("save resumable fixture: %v", err)
	}
	loaded, err := session.Load(dir)
	if err != nil {
		t.Fatalf("load resumable fixture: %v", err)
	}
	return loaded
}

func mustSessionCommandCWD(t *testing.T) string {
	t.Helper()
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	return cwd
}

func TestSessionCommandCatalogAliasesHelpAndArity(t *testing.T) {
	for _, group := range []string{"session", "sessions"} {
		t.Run(group+" ls", func(t *testing.T) {
			env, stdout, stderr, _ := sessionCommandEnv(t, []string{group, "ls"}, "")
			if code := run(env); code != ui.ExitOK || stdout.Len() != 0 || stderr.Len() != 0 {
				t.Fatalf("exit=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
			}
		})
		t.Run(group+" resume help", func(t *testing.T) {
			env, stdout, stderr, _ := sessionCommandEnv(t, []string{group, "resume", "--help"}, "")
			env.stdinPiped = true
			if code := run(env); code != ui.ExitOK || stderr.Len() != 0 {
				t.Fatalf("exit=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
			}
			if !strings.Contains(stdout.String(), "Usage:\n  harness session resume") {
				t.Fatalf("help is not canonical singular path:\n%s", stdout.String())
			}
		})
	}

	for _, args := range [][]string{{"session", "ls", "extra"}, {"session", "resume", "one", "two"}} {
		env, _, stderr, _ := sessionCommandEnv(t, args, "")
		if code := run(env); code != ui.ExitUsage || !strings.Contains(stderr.String(), "positional arguments") {
			t.Fatalf("run(%v) exit=%d stderr=%q", args, code, stderr.String())
		}
	}
}

func TestRunSessionListFiltersOrdersAndFormats(t *testing.T) {
	cwd := mustSessionCommandCWD(t)
	base := time.Date(2026, 8, 10, 10, 0, 0, 0, time.UTC)
	env, stdout, stderr, root := sessionCommandEnv(t, []string{"session", "ls"}, "")
	oldPath := filepath.Join(root, "old")
	newPath := filepath.Join(root, "new")
	otherPath := filepath.Join(root, "other")
	saveSessionCommandFixture(t, oldPath, cwd, base, base.Add(time.Minute), "")
	saveSessionCommandFixture(t, newPath, cwd, base.Add(time.Hour), base.Add(time.Hour+time.Minute), "first\n\t multiline   prompt")
	saveSessionCommandFixture(t, otherPath, filepath.Join(t.TempDir(), "other-cwd"), base.Add(2*time.Hour), base.Add(2*time.Hour+time.Minute), "other")

	if code := run(env); code != ui.ExitOK || stderr.Len() != 0 {
		t.Fatalf("short list exit=%d stderr=%q", code, stderr.String())
	}
	if got, want := stdout.String(), newPath+"\n"+oldPath+"\n"; got != want {
		t.Fatalf("short list = %q, want %q", got, want)
	}

	env.args = []string{"sessions", "ls", "-a"}
	stdout.Reset()
	stderr.Reset()
	if code := run(env); code != ui.ExitOK || stderr.Len() != 0 {
		t.Fatalf("all list exit=%d stderr=%q", code, stderr.String())
	}
	if got, want := stdout.String(), otherPath+"\n"+newPath+"\n"+oldPath+"\n"; got != want {
		t.Fatalf("all list = %q, want %q", got, want)
	}

	lock, err := session.AcquireLock(newPath)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	env.args = []string{"session", "ls", "-a", "-l"}
	stdout.Reset()
	stderr.Reset()
	if code := run(env); code != ui.ExitOK || stderr.Len() != 0 {
		t.Fatalf("long list exit=%d stderr=%q", code, stderr.String())
	}
	long := stdout.String()
	for _, want := range []string{"STATUS", "START", "END", "SESSION", "active", "inactive", newPath, oldPath, otherPath, "prompt: first multiline prompt", "prompt: -", base.Format(time.RFC3339)} {
		if !strings.Contains(long, want) {
			t.Errorf("long list missing %q:\n%s", want, long)
		}
	}
	newLine := "active    "
	if !strings.Contains(long, newLine) || !strings.Contains(long, "-                          "+newPath) {
		t.Errorf("active row does not suppress END:\n%s", long)
	}
}

func TestRunSessionListNoMatchesWarningsAndDefaultRootBoundary(t *testing.T) {
	cwd := mustSessionCommandCWD(t)
	env, stdout, stderr, root := sessionCommandEnv(t, []string{"session", "ls"}, "")
	saveSessionCommandFixture(t, filepath.Join(root, "other"), filepath.Join(t.TempDir(), "other"), time.Now(), time.Now(), "other")
	external := filepath.Join(t.TempDir(), "external")
	saveSessionCommandFixture(t, external, cwd, time.Now().Add(time.Hour), time.Now().Add(time.Hour), "external")
	if code := run(env); code != ui.ExitOK || stdout.Len() != 0 || stderr.Len() != 0 {
		t.Fatalf("no matches exit=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}

	healthy := filepath.Join(root, "healthy")
	saveSessionCommandFixture(t, healthy, cwd, time.Now(), time.Now(), "healthy")
	bad := filepath.Join(root, "bad")
	if err := os.MkdirAll(bad, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bad, "state.json"), []byte("{"), 0o644); err != nil {
		t.Fatal(err)
	}
	old := filepath.Join(root, "old-schema")
	if err := os.MkdirAll(old, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(old, "state.json"), []byte(`{"version":6,"id":"old"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	env.args = []string{"session", "ls", "-a"}
	stdout.Reset()
	stderr.Reset()
	if code := run(env); code != ui.ExitOK {
		t.Fatalf("partial list exit=%d stderr=%q", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), healthy) || strings.Contains(stdout.String(), external) {
		t.Fatalf("default-root list stdout=%q", stdout.String())
	}
	if !strings.Contains(stderr.String(), filepath.Join(bad, "state.json")) || !strings.Contains(stderr.String(), "warning") {
		t.Fatalf("partial warning stderr=%q", stderr.String())
	}
	if strings.Contains(stderr.String(), filepath.Join(old, "state.json")) || strings.Contains(stderr.String(), "unsupported schema version") {
		t.Fatalf("old-schema skip should be silent, stderr=%q", stderr.String())
	}
}

func TestPrintSessionPickerPageUsesPromptFirstAlignedRows(t *testing.T) {
	created := time.Date(2026, 8, 10, 10, 29, 57, 0, time.FixedZone("PDT", -7*60*60))
	entries := []sessionPickerEntry{
		{Summary: session.Summary{Path: "/sessions/new", Created: created, Updated: created.Add(time.Hour), InitialPrompt: "first\n prompt"}},
		{Summary: session.Summary{Path: "/sessions/old", Created: created.Add(-time.Hour), Updated: created, InitialPrompt: "second"}},
	}
	var out bytes.Buffer
	printSessionPickerPage(&out, entries, 0, 2, "")
	want := "Sessions 1-2 of 2\n" +
		"1. prompt: first prompt\n" +
		"   start: 2026-08-10T10:29:57-07:00 · end: 2026-08-10T11:29:57-07:00 · session: /sessions/new\n" +
		"2. prompt: second\n" +
		"   start: 2026-08-10T09:29:57-07:00 · end: 2026-08-10T10:29:57-07:00 · session: /sessions/old\n"
	if got := out.String(); got != want {
		t.Fatalf("picker page:\n%s\nwant:\n%s", got, want)
	}
}

func TestSessionPromptPreviewNormalizesAndClipsRunes(t *testing.T) {
	if got := sessionPromptPreview("  one\n\ttwo   three "); got != "one two three" {
		t.Fatalf("normalized preview = %q", got)
	}
	if got := sessionPromptPreview(" \n\t "); got != "-" {
		t.Fatalf("empty preview = %q", got)
	}
	got := sessionPromptPreview(strings.Repeat("界", sessionPromptPreviewRunes+10))
	if utf8.RuneCountInString(got) != sessionPromptPreviewRunes || !utf8.ValidString(got) || !strings.HasSuffix(got, "…") {
		t.Fatalf("clipped preview runes=%d valid=%v suffix=%q", utf8.RuneCountInString(got), utf8.ValidString(got), got[len(got)-3:])
	}
}

func TestRootInvocationForResumePreservesRootFlagOccurrences(t *testing.T) {
	env := environment{getenv: func(string) string { return "" }}
	invocation, err := commandCatalog(env).Parse([]string{
		"session", "resume",
		"-p=hello world",
		"--model=anthropic:claude-opus-4-8",
		"--quiet=false",
		"--config=",
		"--image=low:a.png",
		"--image=high:b.png",
		"source path",
	})
	if err != nil {
		t.Fatal(err)
	}
	root, err := rootInvocationForResume(env, invocation, "selected path")
	if err != nil {
		t.Fatal(err)
	}
	got := root.Flags.Occurrences()
	want := append(invocation.Flags.Occurrences(), cli.Occurrence{ID: "resume", Name: "resume", Value: "selected path"})
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("forwarded occurrences = %#v, want %#v", got, want)
	}
	if values := root.Flags.All("image"); !reflect.DeepEqual(values, []string{"low:a.png", "high:b.png"}) {
		t.Fatalf("forwarded repeatable images = %v", values)
	}
}

func TestRunSessionResumeExplicitDefaultAndExternalPaths(t *testing.T) {
	for _, location := range []string{"default", "external"} {
		t.Run(location, func(t *testing.T) {
			fp := llmtest.New("fake", okStep())
			env, stdout, stderr, _ := fakeProviderEnv(t, []string{"session", "resume", "-model", "claude-opus-4-8", "-p", "continue"}, fp, "")
			var source string
			if location == "default" {
				source = filepath.Join(session.DefaultRoot(stateDir(env.getenv)), "recorded")
			} else {
				source = filepath.Join(t.TempDir(), "external-session")
			}
			saveResumableCommandFixture(t, source, "prior history")
			env.args = append(env.args, source)
			if code := run(env); code != ui.ExitOK {
				t.Fatalf("resume exit=%d stderr=%q", code, stderr.String())
			}
			if len(fp.Requests) != 1 || fp.Requests[0].Messages[0].Content[0].Text != "prior history" {
				t.Fatalf("resumed requests = %+v", fp.Requests)
			}
			if !strings.Contains(stdout.String(), "ok") {
				t.Fatalf("resumed stdout=%q", stdout.String())
			}
		})
	}
}

func TestRunSessionResumeSelectedPathOverridesEnvironmentAndCloneDestination(t *testing.T) {
	t.Run("environment override", func(t *testing.T) {
		fp := llmtest.New("fake", okStep())
		env, _, stderr, _ := fakeProviderEnv(t, []string{"session", "resume", "-model", "claude-opus-4-8", "-p", "continue"}, fp, "")
		selected := filepath.Join(t.TempDir(), "selected")
		wrong := filepath.Join(t.TempDir(), "wrong")
		saveResumableCommandFixture(t, selected, "selected history")
		saveResumableCommandFixture(t, wrong, "wrong history")
		baseGetenv := env.getenv
		env.getenv = func(name string) string {
			if name == "HARNESS_RESUME" {
				return wrong
			}
			return baseGetenv(name)
		}
		env.args = append(env.args, selected)
		if code := run(env); code != ui.ExitOK {
			t.Fatalf("resume exit=%d stderr=%q", code, stderr.String())
		}
		if got := fp.Requests[0].Messages[0].Content[0].Text; got != "selected history" {
			t.Fatalf("resumed environment path history = %q", got)
		}
	})

	t.Run("clone destination", func(t *testing.T) {
		fp := llmtest.New("fake", okStep())
		destination := filepath.Join(t.TempDir(), "clone")
		env, _, stderr, _ := fakeProviderEnv(t, []string{"session", "resume", "-model", "claude-opus-4-8", "-session", destination, "-p", "continue"}, fp, "")
		sourcePath := filepath.Join(t.TempDir(), "source")
		source := saveResumableCommandFixture(t, sourcePath, "source history")
		env.args = append(env.args, sourcePath)
		if code := run(env); code != ui.ExitOK {
			t.Fatalf("clone resume exit=%d stderr=%q", code, stderr.String())
		}
		clone, err := session.Load(destination)
		if err != nil {
			t.Fatal(err)
		}
		if clone.ParentSession != source.ID || clone.ParentEntryID != source.ActiveLeaf {
			t.Fatalf("clone parent link = %q@%q, want %q@%q", clone.ParentSession, clone.ParentEntryID, source.ID, source.ActiveLeaf)
		}
	})
}

func TestRunSessionResumePickerSelectionsUseStderr(t *testing.T) {
	cases := []struct {
		name  string
		input string
	}{
		{name: "numeric", input: "1\n"},
		{name: "basename prefix", input: "target\n"},
		{name: "prompt search", input: "/needle\n1\n"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fp := llmtest.New("fake", okStep())
			env, stdout, stderr, _ := fakeProviderEnv(t, []string{"session", "resume", "-model", "claude-opus-4-8", "-p", "continue"}, fp, tc.input)
			root := session.DefaultRoot(stateDir(env.getenv))
			target := filepath.Join(root, "target-session")
			saveSessionCommandFixture(t, target, mustSessionCommandCWD(t), time.Now().Add(time.Hour), time.Now().Add(time.Hour), "needle prompt")
			saveSessionCommandFixture(t, filepath.Join(root, "other-session"), mustSessionCommandCWD(t), time.Now(), time.Now(), "other prompt")
			if code := run(env); code != ui.ExitOK {
				t.Fatalf("picker resume exit=%d stderr=%q", code, stderr.String())
			}
			if len(fp.Requests) != 1 {
				t.Fatalf("provider requests=%d", len(fp.Requests))
			}
			if !strings.Contains(stderr.String(), "Sessions") || !strings.Contains(stderr.String(), target) {
				t.Fatalf("picker stderr=%q", stderr.String())
			}
			if strings.Contains(stdout.String(), "Sessions") || !strings.Contains(stdout.String(), "ok") {
				t.Fatalf("picker/resume stdout=%q", stdout.String())
			}
		})
	}
}

func TestRunSessionResumeInformationalFlagDoesNotRequireSource(t *testing.T) {
	env, stdout, stderr, _ := sessionCommandEnv(t, []string{"session", "resume", "--version"}, "")
	env.stdinPiped = true
	if code := run(env); code != ui.ExitOK {
		t.Fatalf("version exit=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	if stdout.Len() == 0 || strings.Contains(stderr.String(), "explicit session path") {
		t.Fatalf("version stdout=%q stderr=%q", stdout.String(), stderr.String())
	}
}

func TestSessionResumeRootInformationalFlags(t *testing.T) {
	env, _, _, _ := sessionCommandEnv(t, nil, "")
	for _, name := range []string{"version", "agents", "models", "check-model-proxy"} {
		invocation, err := commandCatalog(env).Parse([]string{"session", "resume", "--" + name})
		if err != nil {
			t.Fatalf("parse --%s: %v", name, err)
		}
		if !sessionResumeRootInformational(invocation) {
			t.Errorf("flag --%s did not trigger a root informational invocation", name)
		}
	}
	invocation, err := commandCatalog(env).Parse([]string{"session", "resume"})
	if err != nil {
		t.Fatal(err)
	}
	if sessionResumeRootInformational(invocation) {
		t.Fatal("empty flags triggered a root informational invocation")
	}
}

func TestRunSessionResumePickerPaginationCancellationAndInputErrors(t *testing.T) {
	t.Run("pagination and cancellation", func(t *testing.T) {
		env, stdout, stderr, root := sessionCommandEnv(t, []string{"session", "resume"}, "n\nq\n")
		env.terminalRows = func() int { return 11 } // two two-line entries per page
		cwd := mustSessionCommandCWD(t)
		for i := 0; i < 6; i++ {
			saveSessionCommandFixture(t, filepath.Join(root, string(rune('a'+i))), cwd, time.Unix(int64(i), 0), time.Unix(int64(i), 0), "prompt")
		}
		if code := run(env); code != ui.ExitUsage || stdout.Len() != 0 {
			t.Fatalf("cancel exit=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
		}
		if !strings.Contains(stderr.String(), "Sessions 3-4 of 6") || !strings.Contains(stderr.String(), "selection cancelled") {
			t.Fatalf("pagination stderr=%q", stderr.String())
		}
	})

	t.Run("no candidates", func(t *testing.T) {
		env, stdout, stderr, _ := sessionCommandEnv(t, []string{"session", "resume"}, "")
		if code := run(env); code != ui.ExitRuntime || stdout.Len() != 0 || !strings.Contains(stderr.String(), "session ls -a") {
			t.Fatalf("no candidates exit=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
		}
	})

	t.Run("piped stdin", func(t *testing.T) {
		env, stdout, stderr, _ := sessionCommandEnv(t, []string{"session", "resume"}, "piped prompt")
		env.stdinPiped = true
		if code := run(env); code != ui.ExitUsage || stdout.Len() != 0 || !strings.Contains(stderr.String(), "explicit session path") {
			t.Fatalf("piped exit=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
		}
	})
}

func TestRunSessionResumeActiveSelectionAndListToResumeRaceUseRootLock(t *testing.T) {
	for _, tc := range []struct {
		name string
		race bool
	}{
		{name: "already active"},
		{name: "becomes active after listing", race: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fp := llmtest.New("fake", okStep())
			env, stdout, stderr, _ := fakeProviderEnv(t, []string{"session", "resume", "-model", "claude-opus-4-8", "-p", "continue"}, fp, "1\n")
			path := filepath.Join(session.DefaultRoot(stateDir(env.getenv)), "active")
			saveSessionCommandFixture(t, path, mustSessionCommandCWD(t), time.Now(), time.Now(), "active prompt")
			var lock *session.Lock
			var lockErr error
			if tc.race {
				env.stdin = &callbackReader{reader: strings.NewReader("1\n"), callback: func() {
					lock, lockErr = session.AcquireLock(path)
				}}
			} else {
				lock, lockErr = session.AcquireLock(path)
			}
			if lockErr != nil {
				t.Fatal(lockErr)
			}
			defer func() {
				if lock != nil {
					_ = lock.Close()
				}
			}()
			if code := run(env); code != ui.ExitRuntime {
				t.Fatalf("active selection exit=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
			}
			if lockErr != nil {
				t.Fatalf("race lock: %v", lockErr)
			}
			if !strings.Contains(stderr.String(), "is active in process") {
				t.Fatalf("active stderr=%q", stderr.String())
			}
			if len(fp.Requests) != 0 {
				t.Fatalf("active selection made %d provider requests", len(fp.Requests))
			}
		})
	}
}

func TestNoReadAheadReaderPreservesTypeaheadForResumedREPL(t *testing.T) {
	underlying := strings.NewReader("1\nfollow-up\n")
	lineReader := bufio.NewReader(&noReadAheadReader{reader: underlying})
	line, err := lineReader.ReadString('\n')
	if err != nil || line != "1\n" {
		t.Fatalf("picker line = %q, %v", line, err)
	}
	rest, err := io.ReadAll(underlying)
	if err != nil || string(rest) != "follow-up\n" {
		t.Fatalf("typeahead = %q, %v", rest, err)
	}

	fp := llmtest.New("fake", okStep())
	env, _, stderr, _ := fakeProviderEnv(t, []string{"session", "resume", "-model", "claude-opus-4-8"}, fp, "")
	path := filepath.Join(session.DefaultRoot(stateDir(env.getenv)), "session")
	saveSessionCommandFixture(t, path, mustSessionCommandCWD(t), time.Now(), time.Now(), "initial")
	input := strings.NewReader("1\nfollow-up\n/exit\n")
	env.stdin = input
	if code := run(env); code != ui.ExitOK {
		t.Fatalf("interactive picker exit=%d stderr=%q", code, stderr.String())
	}
	if env.stdin != input {
		t.Fatal("session resume replaced the environment stdin reader")
	}
	if len(fp.Requests) != 1 {
		t.Fatalf("typeahead produced %d requests, want 1", len(fp.Requests))
	}
	last := fp.Requests[0].Messages[len(fp.Requests[0].Messages)-1]
	if len(last.Content) == 0 || last.Content[0].Text != "follow-up" {
		t.Fatalf("resumed prompt = %+v", last)
	}
}

type callbackReader struct {
	reader   io.Reader
	callback func()
	called   bool
}

func (reader *callbackReader) Read(p []byte) (int, error) {
	if !reader.called {
		reader.called = true
		reader.callback()
	}
	return reader.reader.Read(p)
}

func TestRootInvocationForResumeRejectsUnexpectedCommand(t *testing.T) {
	// The helper's normal parse path is fully covered above. Keep its error contract
	// anchored by proving the source is always encoded as one resume value, even
	// when it begins with a dash.
	env := environment{getenv: func(string) string { return "" }}
	invocation, err := commandCatalog(env).Parse([]string{"session", "resume", "--", "-session-dir"})
	if err != nil {
		t.Fatal(err)
	}
	root, err := rootInvocationForResume(env, invocation, "-session-dir")
	if err != nil {
		t.Fatal(err)
	}
	if value, ok := root.Flags.Last("resume"); !ok || value != "-session-dir" {
		t.Fatalf("resume value = %q, %v", value, ok)
	}
}
