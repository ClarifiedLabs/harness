package tmux

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
)

// fakeTmux records argv and emulates new-window/split-window -P id output.
type fakeTmux struct {
	mu         sync.Mutex
	calls      [][]string
	nextID     int
	nextPaneID int
	failNew    error
	failSplit  error
}

func (f *fakeTmux) run(_ context.Context, args ...string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, append([]string{}, args...))
	switch args[0] {
	case "new-window":
		if f.failNew != nil {
			return "", f.failNew
		}
		f.nextID++
		return fmt.Sprintf("@%d\n", f.nextID), nil
	case "split-window":
		if f.failSplit != nil {
			return "", f.failSplit
		}
		f.nextPaneID++
		return fmt.Sprintf("%%%d\n", f.nextPaneID), nil
	}
	return "", nil
}

func (f *fakeTmux) recorded() [][]string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([][]string{}, f.calls...)
}

func equalArgv(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func assertCalls(t *testing.T, got [][]string, want [][]string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("calls = %d, want %d:\ngot:  %v\nwant: %v", len(got), len(want), got, want)
	}
	for i := range want {
		if !equalArgv(got[i], want[i]) {
			t.Fatalf("call %d:\ngot:  %v\nwant: %v", i, got[i], want[i])
		}
	}
}

func TestSanitizeWindowName(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		in   string
		want string
	}{
		"plain child id":       {"delegate_20260729T120000Z_000001", "delegate_20260729T120000Z_000001"},
		"colon dot space":      {"a:b.c d", "a-b-c-d"},
		"all dots and dashes":  {"...---...", "child"},
		"all unsafe":           {"///", "child"},
		"empty":                {"", "child"},
		"non-ascii runs":       {"héllo wörld", "h-llo-w-rld"},
		"leading trailing":     {"--abc--", "abc"},
		"cap at 48 runes":      {strings.Repeat("a", 60), strings.Repeat("a", 48)},
		"cap trims cut dash":   {strings.Repeat("a", 47) + "-" + strings.Repeat("b", 10), strings.Repeat("a", 47)},
		"underscores and dots": {"a_b.c", "a_b-c"},
	} {
		t.Run(name, func(t *testing.T) {
			if got := sanitizeWindowName(tc.in); got != tc.want {
				t.Fatalf("sanitizeWindowName(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// TestViewerGoldenArgv asserts the exact tmux invocations for one open/close
// cycle: detached insert-after window creation with -P id printing, -- before
// the harness command, no -t on new-window, and explicit @id targets for the
// follow-up option, rename, and kill commands.
func TestViewerGoldenArgv(t *testing.T) {
	fake := &fakeTmux{}
	viewer := NewViewer(Client{Binary: "/usr/local/bin/tmux", run: fake.run}, ViewerOptions{
		HarnessBinary: "/usr/local/bin/harness",
		MaxWindows:    2,
	})

	h, err := viewer.Open(View{
		Name: "delegate_20260729T120000Z_000001",
		Dir:  "/sessions/children/delegate_20260729T120000Z_000001",
		Log:  "auto delegate_20260729T120000Z_000001",
	})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if h.ID() != "@1" {
		t.Fatalf("handle ID = %q, want @1", h.ID())
	}
	h.Close()

	assertCalls(t, fake.recorded(), [][]string{
		{"new-window", "-d", "-a", "-P", "-F", "#{window_id}", "--",
			"/usr/local/bin/harness", "session", "replay", "--follow", "--",
			"/sessions/children/delegate_20260729T120000Z_000001"},
		{"set-option", "-w", "-q", "-t", "@1", "remain-on-exit", "on"},
		{"set-option", "-w", "-q", "-t", "@1", "automatic-rename", "off"},
		{"rename-window", "-t", "@1", "--", "delegate_20260729T120000Z_000001"},
		{"kill-window", "-t", "@1"},
	})
}

func TestViewerCapAndCloseFreesSlot(t *testing.T) {
	fake := &fakeTmux{}
	viewer := NewViewer(Client{Binary: "/tmux", run: fake.run}, ViewerOptions{
		HarnessBinary: "/harness",
		MaxWindows:    2,
	})
	h1, err := viewer.Open(View{Name: "c1", Dir: "/d1", Log: "a c1"})
	if err != nil {
		t.Fatalf("Open 1: %v", err)
	}
	if _, err := viewer.Open(View{Name: "c2", Dir: "/d2", Log: "a c2"}); err != nil {
		t.Fatalf("Open 2: %v", err)
	}
	// At cap: rejected before any tmux invocation.
	if _, err := viewer.Open(View{Name: "c3", Dir: "/d3", Log: "a c3"}); !errors.Is(err, errWindowCap) {
		t.Fatalf("Open 3 error = %v, want window cap", err)
	}
	calls := fake.recorded()
	if got := countCalls(calls, "new-window"); got != 2 {
		t.Fatalf("new-window calls = %d, want 2 (cap rejected before spawning)", got)
	}
	// Closing one frees the slot.
	h1.Close()
	if _, err := viewer.Open(View{Name: "c3", Dir: "/d3", Log: "a c3"}); err != nil {
		t.Fatalf("Open after close: %v", err)
	}
	if got := countCalls(fake.recorded(), "new-window"); got != 3 {
		t.Fatalf("new-window calls = %d, want 3", got)
	}
}

func countCalls(calls [][]string, name string) int {
	n := 0
	for _, c := range calls {
		if len(c) > 0 && c[0] == name {
			n++
		}
	}
	return n
}

// TestViewerDrainOnShutdown covers the failed-window-kept path: a window whose
// handle is never Closed stays tracked and is killed by Shutdown, alongside
// any still-open ones. A second Shutdown is a no-op.
func TestViewerDrainOnShutdown(t *testing.T) {
	fake := &fakeTmux{}
	viewer := NewViewer(Client{Binary: "/tmux", run: fake.run}, ViewerOptions{HarnessBinary: "/harness", MaxWindows: 4})
	h1, err := viewer.Open(View{Name: "c1", Dir: "/d1", Log: "a c1"})
	if err != nil {
		t.Fatalf("Open 1: %v", err)
	}
	if _, err := viewer.Open(View{Name: "c2", Dir: "/d2", Log: "a c2"}); err != nil {
		t.Fatalf("Open 2: %v", err)
	}
	h1.Close() // success path kills @1 immediately; @2 stays tracked (failure path)
	viewer.Shutdown()
	viewer.Shutdown() // idempotent

	var kills [][]string
	for _, c := range fake.recorded() {
		if c[0] == "kill-window" {
			kills = append(kills, c)
		}
	}
	if len(kills) != 2 {
		t.Fatalf("kill-window calls = %v, want exactly 2", kills)
	}
	// Order between Close and Shutdown kills is deterministic here.
	assertCalls(t, kills, [][]string{{"kill-window", "-t", "@1"}, {"kill-window", "-t", "@2"}})
}

// TestHandleCloseAfterShutdown ensures a handle closed after Shutdown does not
// re-kill its window: whoever removes the id from the tracked set owns the
// kill, and Shutdown drained first.
func TestHandleCloseAfterShutdown(t *testing.T) {
	fake := &fakeTmux{}
	viewer := NewViewer(Client{Binary: "/tmux", run: fake.run}, ViewerOptions{HarnessBinary: "/harness", MaxWindows: 4})
	h, err := viewer.Open(View{Name: "c1", Dir: "/d1", Log: "a c1"})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	viewer.Shutdown()
	before := countCalls(fake.recorded(), "kill-window")
	h.Close()
	h.Close() // also idempotent
	if got := countCalls(fake.recorded(), "kill-window"); got != before {
		t.Fatalf("kill-window calls grew from %d to %d after post-shutdown Close", before, got)
	}
}

func TestViewerOpenFailures(t *testing.T) {
	t.Run("after shutdown", func(t *testing.T) {
		fake := &fakeTmux{}
		viewer := NewViewer(Client{Binary: "/tmux", run: fake.run}, ViewerOptions{HarnessBinary: "/harness", MaxWindows: 1})
		viewer.Shutdown()
		if _, err := viewer.Open(View{Name: "c", Dir: "/d", Log: "a c"}); err == nil {
			t.Fatal("Open after Shutdown should fail")
		}
		if got := countCalls(fake.recorded(), "new-window"); got != 0 {
			t.Fatalf("new-window calls = %d after shutdown, want 0", got)
		}
	})

	t.Run("new-window error propagates and logs", func(t *testing.T) {
		var logs strings.Builder
		logger := slog.New(slog.NewTextHandler(&logs, nil))
		fake := &fakeTmux{failNew: fmt.Errorf("exit status 1: no server")}
		viewer := NewViewer(Client{Binary: "/tmux", run: fake.run}, ViewerOptions{
			HarnessBinary: "/harness",
			MaxWindows:    1,
			Logger:        logger,
		})
		if _, err := viewer.Open(View{Name: "c", Dir: "/d", Log: "explore c"}); err == nil {
			t.Fatal("Open should return the tmux failure")
		}
		if !strings.Contains(logs.String(), "explore c") || !strings.Contains(logs.String(), "category=tmux") {
			t.Fatalf("warning should name the delegate and category, got %q", logs.String())
		}
		viewer.Shutdown() // nothing tracked: no kill-window
		if got := countCalls(fake.recorded(), "kill-window"); got != 0 {
			t.Fatalf("kill-window calls = %d, want 0", got)
		}
	})

	t.Run("nil viewer", func(t *testing.T) {
		var viewer *Viewer
		viewer.Shutdown() // must not panic
		if _, err := viewer.Open(View{Name: "c", Dir: "/d", Log: "a c"}); err == nil {
			t.Fatal("nil viewer Open should fail")
		}
	})

	t.Run("empty harness binary", func(t *testing.T) {
		fake := &fakeTmux{}
		viewer := NewViewer(Client{Binary: "/tmux", run: fake.run}, ViewerOptions{MaxWindows: 1})
		if _, err := viewer.Open(View{Name: "c", Dir: "/d", Log: "a c"}); err == nil {
			t.Fatal("empty harness binary should fail")
		}
	})
}

// TestFakeTmuxRecorder exercises the real subprocess argv marshalling: a fake
// tmux shell script records every invocation as one shell-quoted line (the
// shell is the file format, so a space in the binary path cannot masquerade
// as an argument separator), and the assertions parse each line back into
// fields before comparing.
func TestFakeTmuxRecorder(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "bin with space")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	logPath := filepath.Join(t.TempDir(), "tmux.log")
	binPath := filepath.Join(dir, "tmux")
	script := `#!/bin/sh
# Record $0 and argv as one shell-quoted line, then emulate new-window -P.
{
  write_field() {
    printf "'"
    s=$1
    while :; do
      case $s in
        *"'"*)
          printf "%s'\\''" "${s%%"'"*}"
          s=${s#*"'"}
          ;;
        *)
          printf "%s'" "$s"
          break
          ;;
      esac
    done
  }
  write_field "$0"
  for arg in "$@"; do
    printf " "
    write_field "$arg"
  done
  printf "\n"
} >> "$FAKE_TMUX_LOG"
if [ "$1" = "new-window" ]; then
  n=$(wc -l < "$FAKE_TMUX_LOG" | tr -d " ")
  printf "@%s\n" "$n"
fi
exit 0
`
	if err := os.WriteFile(binPath, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake tmux: %v", err)
	}
	t.Setenv("FAKE_TMUX_LOG", logPath)

	viewer := NewViewer(Client{Binary: binPath}, ViewerOptions{
		HarnessBinary: "/opt/harness dir/harness",
		MaxWindows:    4,
	})
	h, err := viewer.Open(View{Name: "delegate:one.two", Dir: "/sessions/children/delegate one", Log: "auto delegate one"})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	h.Close()
	if _, err := viewer.Open(View{Name: "c2", Dir: "/d2", Log: "a c2"}); err != nil {
		t.Fatalf("Open 2: %v", err)
	}
	viewer.Shutdown()

	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read recorder log: %v", err)
	}
	lines := strings.Split(strings.TrimRight(string(data), "\n"), "\n")
	var got [][]string
	for _, line := range lines {
		got = append(got, parseShellFields(t, line))
	}
	// Every recorded invocation names the space-containing binary as $0.
	for i, fields := range got {
		if len(fields) == 0 || fields[0] != binPath {
			t.Fatalf("line %d $0 = %q, want %q", i, fields, binPath)
		}
	}
	// Strip $0 for argv comparison.
	var argv [][]string
	for _, fields := range got {
		argv = append(argv, fields[1:])
	}
	assertCalls(t, argv, [][]string{
		{"new-window", "-d", "-a", "-P", "-F", "#{window_id}", "--",
			"/opt/harness dir/harness", "session", "replay", "--follow", "--", "/sessions/children/delegate one"},
		{"set-option", "-w", "-q", "-t", "@1", "remain-on-exit", "on"},
		{"set-option", "-w", "-q", "-t", "@1", "automatic-rename", "off"},
		{"rename-window", "-t", "@1", "--", "delegate-one-two"},
		{"kill-window", "-t", "@1"},
		{"new-window", "-d", "-a", "-P", "-F", "#{window_id}", "--",
			"/opt/harness dir/harness", "session", "replay", "--follow", "--", "/d2"},
		{"set-option", "-w", "-q", "-t", "@6", "remain-on-exit", "on"},
		{"set-option", "-w", "-q", "-t", "@6", "automatic-rename", "off"},
		{"rename-window", "-t", "@6", "--", "c2"},
		{"kill-window", "-t", "@6"},
	})
}

// TestViewerPaneGoldenArgv asserts the exact tmux invocations for one pane
// open/close cycle: horizontal split of the parent pane, per-pane option and
// title, and kill-pane; no automatic-rename or select-layout for a single pane.
func TestViewerPaneGoldenArgv(t *testing.T) {
	fake := &fakeTmux{}
	viewer := NewViewer(Client{Binary: "/usr/local/bin/tmux", run: fake.run}, ViewerOptions{
		HarnessBinary: "/usr/local/bin/harness",
		MaxWindows:    2,
		Layout:        LayoutPane,
		ParentPane:    "%9",
	})

	h, err := viewer.Open(View{
		Name: "delegate_20260729T120000Z_000001",
		Dir:  "/sessions/children/delegate_20260729T120000Z_000001",
		Log:  "auto delegate_20260729T120000Z_000001",
	})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if h.ID() != "%1" {
		t.Fatalf("handle ID = %q, want %%1", h.ID())
	}
	h.Close()

	assertCalls(t, fake.recorded(), [][]string{
		{"split-window", "-d", "-h", "-t", "%9", "-P", "-F", "#{pane_id}", "--",
			"/usr/local/bin/harness", "session", "replay", "--follow", "--",
			"/sessions/children/delegate_20260729T120000Z_000001"},
		{"set-option", "-p", "-q", "-t", "%1", "remain-on-exit", "on"},
		{"select-pane", "-T", "delegate_20260729T120000Z_000001", "-t", "%1"},
		{"kill-pane", "-t", "%1"},
	})
}

// TestViewerPaneStacksAdditionalDelegates checks that the second and third
// delegates split the previous delegate pane vertically and even the column,
// and that closing a middle pane leaves the others intact.
func TestViewerPaneStacksAdditionalDelegates(t *testing.T) {
	fake := &fakeTmux{}
	viewer := NewViewer(Client{Binary: "/tmux", run: fake.run}, ViewerOptions{
		HarnessBinary: "/harness",
		MaxWindows:    4,
		Layout:        LayoutPane,
		ParentPane:    "%9",
	})

	h1, err := viewer.Open(View{Name: "c1", Dir: "/d1", Log: "a c1"})
	if err != nil {
		t.Fatalf("Open 1: %v", err)
	}
	h2, err := viewer.Open(View{Name: "c2", Dir: "/d2", Log: "a c2"})
	if err != nil {
		t.Fatalf("Open 2: %v", err)
	}
	h3, err := viewer.Open(View{Name: "c3", Dir: "/d3", Log: "a c3"})
	if err != nil {
		t.Fatalf("Open 3: %v", err)
	}
	if h1.ID() != "%1" || h2.ID() != "%2" || h3.ID() != "%3" {
		t.Fatalf("handle IDs = %q/%q/%q, want %%1/%%2/%%3", h1.ID(), h2.ID(), h3.ID())
	}

	h2.Close()
	viewer.Shutdown()

	assertCalls(t, fake.recorded(), [][]string{
		{"split-window", "-d", "-h", "-t", "%9", "-P", "-F", "#{pane_id}", "--", "/harness", "session", "replay", "--follow", "--", "/d1"},
		{"set-option", "-p", "-q", "-t", "%1", "remain-on-exit", "on"},
		{"select-pane", "-T", "c1", "-t", "%1"},
		{"split-window", "-d", "-v", "-t", "%1", "-P", "-F", "#{pane_id}", "--", "/harness", "session", "replay", "--follow", "--", "/d2"},
		{"set-option", "-p", "-q", "-t", "%2", "remain-on-exit", "on"},
		{"select-pane", "-T", "c2", "-t", "%2"},
		{"select-layout", "-E", "-t", "%2"},
		{"split-window", "-d", "-v", "-t", "%2", "-P", "-F", "#{pane_id}", "--", "/harness", "session", "replay", "--follow", "--", "/d3"},
		{"set-option", "-p", "-q", "-t", "%3", "remain-on-exit", "on"},
		{"select-pane", "-T", "c3", "-t", "%3"},
		{"select-layout", "-E", "-t", "%3"},
		{"kill-pane", "-t", "%2"},
		{"kill-pane", "-t", "%1"},
		{"kill-pane", "-t", "%3"},
	})
}

// TestViewerPaneCapShared ensures the cap is checked while holding splitMu so
// a third concurrent opener never reaches tmux.
func TestViewerPaneCapShared(t *testing.T) {
	fake := &fakeTmux{}
	viewer := NewViewer(Client{Binary: "/tmux", run: fake.run}, ViewerOptions{
		HarnessBinary: "/harness",
		MaxWindows:    2,
		Layout:        LayoutPane,
		ParentPane:    "%9",
	})
	if _, err := viewer.Open(View{Name: "c1", Dir: "/d1", Log: "a c1"}); err != nil {
		t.Fatalf("Open 1: %v", err)
	}
	if _, err := viewer.Open(View{Name: "c2", Dir: "/d2", Log: "a c2"}); err != nil {
		t.Fatalf("Open 2: %v", err)
	}
	if _, err := viewer.Open(View{Name: "c3", Dir: "/d3", Log: "a c3"}); !errors.Is(err, errWindowCap) {
		t.Fatalf("Open 3 error = %v, want window cap", err)
	}
	if got := countCalls(fake.recorded(), "split-window"); got != 2 {
		t.Fatalf("split-window calls = %d, want 2 (cap rejected before spawning)", got)
	}
}

// TestViewerPaneRequiresParentPane ensures pane layout fails fast without a
// parent pane and never touches tmux.
func TestViewerPaneRequiresParentPane(t *testing.T) {
	fake := &fakeTmux{}
	viewer := NewViewer(Client{Binary: "/tmux", run: fake.run}, ViewerOptions{
		HarnessBinary: "/harness",
		MaxWindows:    1,
		Layout:        LayoutPane,
	})
	if _, err := viewer.Open(View{Name: "c1", Dir: "/d1", Log: "a c1"}); err == nil {
		t.Fatal("pane layout without parent pane should fail")
	}
	if got := countCalls(fake.recorded(), "split-window"); got != 0 {
		t.Fatalf("split-window calls = %d, want 0", got)
	}
}

// TestViewerPaneConcurrentFirstOpensSerialize verifies that two simultaneous
// first-opens cannot both horizontally split the parent: one opens first, and
// the second stacks vertically on it.
func TestViewerPaneConcurrentFirstOpensSerialize(t *testing.T) {
	fake := &fakeTmux{}
	viewer := NewViewer(Client{Binary: "/tmux", run: fake.run}, ViewerOptions{
		HarnessBinary: "/harness",
		MaxWindows:    4,
		Layout:        LayoutPane,
		ParentPane:    "%9",
	})

	start := make(chan struct{})
	var wg sync.WaitGroup
	var handles [2]*ViewHandle
	var errs [2]error
	for i := range 2 {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			handles[i], errs[i] = viewer.Open(View{Name: fmt.Sprintf("c%d", i+1), Dir: fmt.Sprintf("/d%d", i+1), Log: fmt.Sprintf("a c%d", i+1)})
		}(i)
	}
	close(start)
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("Open %d: %v", i+1, err)
		}
	}

	calls := fake.recorded()
	hSplits := countCallsWithTarget(calls, "split-window", "-h", "%9")
	vSplits := countCallsWithTarget(calls, "split-window", "-v", "%1")
	if hSplits != 1 || vSplits != 1 {
		t.Fatalf("got %d horizontal parent splits and %d vertical %%1 splits, want 1/1\ncalls: %v", hSplits, vSplits, calls)
	}
	for _, h := range handles {
		h.Close()
	}
}

func countCallsWithTarget(calls [][]string, name, flag, target string) int {
	n := 0
	for _, c := range calls {
		if len(c) == 0 || c[0] != name {
			continue
		}
		if slices.Contains(c, flag) && slices.Contains(c, target) {
			n++
		}
	}
	return n
}

// TestViewerPaneDrainOnShutdown covers the same drain/close-after-shutdown
// invariants for pane layout.
func TestViewerPaneDrainOnShutdown(t *testing.T) {
	fake := &fakeTmux{}
	viewer := NewViewer(Client{Binary: "/tmux", run: fake.run}, ViewerOptions{
		HarnessBinary: "/harness",
		MaxWindows:    4,
		Layout:        LayoutPane,
		ParentPane:    "%9",
	})
	h1, err := viewer.Open(View{Name: "c1", Dir: "/d1", Log: "a c1"})
	if err != nil {
		t.Fatalf("Open 1: %v", err)
	}
	if _, err := viewer.Open(View{Name: "c2", Dir: "/d2", Log: "a c2"}); err != nil {
		t.Fatalf("Open 2: %v", err)
	}
	h1.Close()
	viewer.Shutdown()
	viewer.Shutdown()

	var kills [][]string
	for _, c := range fake.recorded() {
		if c[0] == "kill-pane" {
			kills = append(kills, c)
		}
	}
	if len(kills) != 2 {
		t.Fatalf("kill-pane calls = %v, want exactly 2", kills)
	}
	assertCalls(t, kills, [][]string{{"kill-pane", "-t", "%1"}, {"kill-pane", "-t", "%2"}})
}

// TestViewerPaneSplitFailurePropagatesAndLogs mirrors the window failure test
// for the pane path.
func TestViewerPaneSplitFailurePropagatesAndLogs(t *testing.T) {
	var logs strings.Builder
	logger := slog.New(slog.NewTextHandler(&logs, nil))
	fake := &fakeTmux{failSplit: fmt.Errorf("exit status 1: no server")}
	viewer := NewViewer(Client{Binary: "/tmux", run: fake.run}, ViewerOptions{
		HarnessBinary: "/harness",
		MaxWindows:    1,
		Layout:        LayoutPane,
		ParentPane:    "%9",
		Logger:        logger,
	})
	if _, err := viewer.Open(View{Name: "c", Dir: "/d", Log: "explore c"}); err == nil {
		t.Fatal("Open should return the tmux failure")
	}
	if !strings.Contains(logs.String(), "explore c") || !strings.Contains(logs.String(), "category=tmux") {
		t.Fatalf("warning should name the delegate and category, got %q", logs.String())
	}
	viewer.Shutdown()
	if got := countCalls(fake.recorded(), "kill-pane"); got != 0 {
		t.Fatalf("kill-pane calls = %d, want 0", got)
	}
}

// parseShellFields decodes one recorder line: single-quoted fields with an
// embedded quote encoded as the classic '\” sequence.
func parseShellFields(t *testing.T, line string) []string {
	t.Helper()
	var fields []string
	i := 0
	for i < len(line) {
		for i < len(line) && line[i] == ' ' {
			i++
		}
		if i >= len(line) {
			break
		}
		if line[i] != '\'' {
			t.Fatalf("field %d does not start with a quote: %q", len(fields), line)
		}
		i++
		var b strings.Builder
		for {
			if i >= len(line) {
				t.Fatalf("unterminated field in %q", line)
			}
			if line[i] == '\'' {
				if i+3 < len(line) && line[i+1] == '\\' && line[i+2] == '\'' && line[i+3] == '\'' {
					b.WriteByte('\'')
					i += 4
					continue
				}
				i++
				break
			}
			b.WriteByte(line[i])
			i++
		}
		fields = append(fields, b.String())
	}
	return fields
}
