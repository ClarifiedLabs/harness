// Package tmux opens best-effort, display-only tmux windows that follow
// delegate child sessions live via `harness session replay --follow`. It is a
// stdlib-only leaf: every operation is best-effort, failures are reported to
// the configured logger (a nil logger is silent) and otherwise dropped, and no
// failure here ever propagates into delegate execution.
package tmux

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os/exec"
	"regexp"
	"strings"
	"sync"
	"time"

	"harness/internal/logging"
)

const (
	// defaultMaxWindows caps simultaneous delegate windows when ViewerOptions
	// does not override it. Config validation enforces the same default.
	defaultMaxWindows = 4
	// commandTimeout bounds every tmux CLI invocation so a wedged tmux server
	// can never stall a delegate turn.
	commandTimeout = 2 * time.Second
	// maxWindowNameRunes caps the sanitized window name.
	maxWindowNameRunes = 48
)

// Client shells out to tmux. Binary is the path of the tmux executable.
type Client struct {
	Binary string

	// run executes one tmux CLI invocation and returns its stdout. nil uses a
	// real subprocess; tests inject it to capture argv without a tmux server.
	run func(ctx context.Context, args ...string) (string, error)
}

// NewWindow opens a detached window next to the active one, running windowCmd
// as the window's shell command (each element stays a separate argv element;
// tmux re-quotes when it joins them into the sh -c string). It returns the
// new window's globally unique @id, printed via -P -F '#{window_id}'.
func (c Client) NewWindow(windowCmd []string) (string, error) {
	args := []string{"new-window", "-d", "-a", "-P", "-F", "#{window_id}", "--"}
	args = append(args, windowCmd...)
	out, err := c.output(args...)
	if err != nil {
		return "", err
	}
	id := strings.TrimSpace(out)
	if id == "" {
		return "", errors.New("tmux: new-window returned an empty window id")
	}
	return id, nil
}

// SetOption sets one window option on the window with the given @id.
func (c Client) SetOption(id, name, value string) error {
	_, err := c.output("set-option", "-w", "-q", "-t", id, name, value)
	return err
}

// RenameWindow renames the window with the given @id. The name is sanitized
// before it gets here; -- keeps any residual leading dash from parsing as a
// flag.
func (c Client) RenameWindow(id, name string) error {
	_, err := c.output("rename-window", "-t", id, "--", name)
	return err
}

// CloseWindow kills the window with the given @id.
func (c Client) CloseWindow(id string) error {
	_, err := c.output("kill-window", "-t", id)
	return err
}

// output runs one tmux CLI invocation bounded by commandTimeout, surfacing
// the server's stderr on failure.
func (c Client) output(args ...string) (string, error) {
	if strings.TrimSpace(c.Binary) == "" {
		return "", errors.New("tmux: empty binary path")
	}
	run := c.run
	if run == nil {
		binary := c.Binary
		run = func(ctx context.Context, args ...string) (string, error) {
			cmd := exec.CommandContext(ctx, binary, args...) // nosemgrep: dangerous-exec-command
			var stderr bytes.Buffer
			cmd.Stderr = &stderr
			out, err := cmd.Output()
			if err != nil {
				if detail := strings.TrimSpace(stderr.String()); detail != "" {
					return "", fmt.Errorf("%w: %s", err, detail)
				}
				return "", err
			}
			return string(out), nil
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), commandTimeout)
	defer cancel()
	return run(ctx, args...)
}

// View describes one delegate window to open. Name is the window name (it is
// sanitized before use), Dir the child session directory with a running
// meta.json, and Log a short operator-log label (agent plus child ID) used
// only in log messages.
type View struct {
	Name string
	Dir  string
	Log  string
}

// ViewerOptions configures a Viewer.
type ViewerOptions struct {
	// HarnessBinary is the absolute path of the harness executable the window
	// runs; required.
	HarnessBinary string
	// MaxWindows caps simultaneously open delegate windows; <= 0 uses
	// defaultMaxWindows.
	MaxWindows int
	// Logger receives best-effort failure reports; nil is silent.
	Logger *slog.Logger
}

// Viewer owns the set of open delegate windows. Open serializes window
// creation under a short mutex; tmux CLI calls run outside the lock so a slow
// server never blocks other delegates.
type Viewer struct {
	client Client
	opts   ViewerOptions

	mu     sync.Mutex
	open   map[string]*ViewHandle
	closed bool
}

// NewViewer builds a Viewer around client.
func NewViewer(client Client, opts ViewerOptions) *Viewer {
	if opts.MaxWindows <= 0 {
		opts.MaxWindows = defaultMaxWindows
	}
	return &Viewer{client: client, opts: opts, open: make(map[string]*ViewHandle)}
}

// errWindowCap reports that the window cap rejected a new delegate window.
var errWindowCap = errors.New("tmux: delegate window cap reached")

// Open creates one delegate window. On success it returns a ViewHandle whose
// Close kills the window; every failure is logged and returned so the caller
// can drop it. Open never blocks on the delegate run.
func (v *Viewer) Open(view View) (*ViewHandle, error) {
	if v == nil {
		return nil, errors.New("tmux: viewer unavailable")
	}
	if strings.TrimSpace(v.opts.HarnessBinary) == "" {
		return nil, errors.New("tmux: harness binary path is empty")
	}
	v.mu.Lock()
	if v.closed {
		v.mu.Unlock()
		return nil, errors.New("tmux: viewer shut down")
	}
	if len(v.open) >= v.opts.MaxWindows {
		max := v.opts.MaxWindows
		v.mu.Unlock()
		v.logDebug(fmt.Sprintf("tmux: delegate window cap (%d) reached; no window for %s", max, view.Log))
		return nil, errWindowCap
	}
	v.mu.Unlock()

	// remain-on-exit must land before the follow process can exit, or a
	// fast-finishing child would close its own window before anyone saw it.
	// The window opens before the child agent runs, so this separate
	// invocation always wins in practice; an explicit -t @id is required
	// because a tmux command sequence and in-window shells both resolve the
	// session's active window, not the new one.
	id, err := v.client.NewWindow([]string{v.opts.HarnessBinary, "session", "replay", "--follow", "--", view.Dir})
	if err != nil {
		v.logWarn(fmt.Sprintf("tmux: cannot open delegate window for %s: %v", view.Log, err))
		return nil, err
	}
	handle := &ViewHandle{viewer: v, id: id}
	v.mu.Lock()
	if v.closed {
		v.mu.Unlock()
		v.closeWindow(id)
		return nil, errors.New("tmux: viewer shut down")
	}
	v.open[id] = handle
	v.mu.Unlock()

	// Window dressing is best-effort: failures leave a working but
	// auto-named window.
	if err := v.client.SetOption(id, "remain-on-exit", "on"); err != nil {
		v.logDebug(fmt.Sprintf("tmux: cannot set remain-on-exit on %s: %v", id, err))
	}
	if err := v.client.SetOption(id, "automatic-rename", "off"); err != nil {
		v.logDebug(fmt.Sprintf("tmux: cannot set automatic-rename on %s: %v", id, err))
	}
	if err := v.client.RenameWindow(id, sanitizeWindowName(view.Name)); err != nil {
		v.logDebug(fmt.Sprintf("tmux: cannot name delegate window %s: %v", id, err))
	}
	return handle, nil
}

// ViewHandle owns one open delegate window. Close is idempotent.
type ViewHandle struct {
	viewer *Viewer
	id     string
	once   sync.Once
}

// ID returns the tmux @id of the owned window, for tests and logs.
func (h *ViewHandle) ID() string { return h.id }

// Close kills the window. It is a no-op after the first call, and skips the
// kill when Shutdown has already drained the viewer (Shutdown owns those
// kills).
func (h *ViewHandle) Close() {
	h.once.Do(func() {
		h.viewer.close(h.id)
	})
}

// close removes id from the tracked set and kills it. Whoever removes the id
// from the map owns the kill: if Shutdown drained first, close is a no-op.
func (v *Viewer) close(id string) {
	v.mu.Lock()
	_, tracked := v.open[id]
	if tracked {
		delete(v.open, id)
	}
	v.mu.Unlock()
	if !tracked {
		return
	}
	v.closeWindow(id)
}

func (v *Viewer) closeWindow(id string) {
	if err := v.client.CloseWindow(id); err != nil {
		v.logDebug(fmt.Sprintf("tmux: cannot close delegate window %s: %v", id, err))
	}
}

// Shutdown kills every tracked window unconditionally and stops new Opens.
// It is idempotent and safe on a nil receiver. Drain failures are logged at
// debug and dropped: process exit must not fail over a display-only window.
func (v *Viewer) Shutdown() {
	if v == nil {
		return
	}
	v.mu.Lock()
	if v.closed {
		v.mu.Unlock()
		return
	}
	v.closed = true
	ids := make([]string, 0, len(v.open))
	for id := range v.open {
		ids = append(ids, id)
	}
	v.open = make(map[string]*ViewHandle)
	v.mu.Unlock()
	for _, id := range ids {
		v.closeWindow(id)
	}
}

func (v *Viewer) logDebug(msg string) {
	if v == nil || v.opts.Logger == nil {
		return
	}
	v.opts.Logger.Debug(msg, logging.Category("tmux"))
}

func (v *Viewer) logWarn(msg string) {
	if v == nil || v.opts.Logger == nil {
		return
	}
	v.opts.Logger.Warn(msg, logging.Category("tmux"))
}

// badWindowChars matches runs that collapse to a dash in tmux window names.
// Colons, dots, and whitespace are excluded so a name can never parse as a
// tmux target; the set stays ASCII to avoid width surprises in the tab bar.
var badWindowChars = regexp.MustCompile(`[^A-Za-z0-9_-]+`)

// sanitizeWindowName maps a child ID to a tmux-safe window name: unsafe runs
// become a dash, leading/trailing dashes are dropped, the result is capped at
// maxWindowNameRunes runes, and an empty result falls back to "child".
func sanitizeWindowName(name string) string {
	mapped := strings.Trim(badWindowChars.ReplaceAllString(name, "-"), "-")
	runes := []rune(mapped)
	if len(runes) > maxWindowNameRunes {
		mapped = strings.TrimRight(string(runes[:maxWindowNameRunes]), "-")
	}
	if mapped == "" {
		return "child"
	}
	return mapped
}
