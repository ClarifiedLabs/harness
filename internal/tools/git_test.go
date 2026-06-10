package tools

import (
	"context"
	"encoding/json"
	"os/exec"
	"slices"
	"strings"
	"testing"
)

// gitAvailable reports whether a git binary is on PATH; tests skip without it.
func gitAvailable(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
}

func runGit(t *testing.T, dir string, args ...string) (string, error) {
	t.Helper()
	in := map[string]any{"args": args}
	if dir != "" {
		in["cwd"] = dir
	}
	b, err := json.Marshal(in)
	if err != nil {
		t.Fatalf("marshal args: %v", err)
	}
	return gitTool{}.Run(context.Background(), b)
}

// scratchRepo initializes a fresh git repo in a temp dir with identity set.
func scratchRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	for _, argv := range [][]string{
		{"init", "-q"},
		{"config", "user.email", "test@example.com"},
		{"config", "user.name", "Test"},
	} {
		cmd := exec.Command("git", append([]string{"-C", dir}, argv...)...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", argv, err, out)
		}
	}
	return dir
}

func TestGitStatusAddCommitLogRoundTrip(t *testing.T) {
	gitAvailable(t)
	dir := scratchRepo(t)
	mustWrite(t, dir+"/hello.txt", "hi\n")

	status, err := runGit(t, dir, "status", "--porcelain")
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	if !strings.Contains(status, "hello.txt") {
		t.Errorf("status should show the untracked file: %q", status)
	}
	if !strings.Contains(status, "[exit code: 0]") {
		t.Errorf("status missing exit code marker: %q", status)
	}

	if _, err := runGit(t, dir, "add", "hello.txt"); err != nil {
		t.Fatalf("add: %v", err)
	}
	if _, err := runGit(t, dir, "commit", "-m", "add hello"); err != nil {
		t.Fatalf("commit: %v", err)
	}

	logOut, err := runGit(t, dir, "log", "--oneline")
	if err != nil {
		t.Fatalf("log: %v", err)
	}
	if !strings.Contains(logOut, "add hello") {
		t.Errorf("log should show the commit subject: %q", logOut)
	}
}

func TestGitNonZeroExitNotError(t *testing.T) {
	gitAvailable(t)
	dir := t.TempDir() // not a repo
	out, err := runGit(t, dir, "status")
	if err != nil {
		t.Fatalf("git's own failure must surface as a result, not a tool error: %v", err)
	}
	if !strings.Contains(strings.ToLower(out), "not a git repository") {
		t.Errorf("git's error message should be surfaced: %q", out)
	}
	if !strings.Contains(out, "[exit code:") {
		t.Errorf("missing exit code marker: %q", out)
	}
}

func TestGitMissingArgs(t *testing.T) {
	_, err := gitTool{}.Run(context.Background(), json.RawMessage(`{}`))
	if err == nil {
		t.Fatal("expected error for missing args")
	}
}

func TestGitEmptyArgs(t *testing.T) {
	_, err := gitTool{}.Run(context.Background(), json.RawMessage(`{"args":[]}`))
	if err == nil {
		t.Fatal("expected error for empty args array")
	}
}

func TestDecodeGitArgsAcceptsObjectAndBareArray(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  []string
	}{
		{name: "object", input: `{"args":["status","--porcelain"]}`, want: []string{"status", "--porcelain"}},
		{name: "bare array", input: `["status","--porcelain"]`, want: []string{"status", "--porcelain"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gi, err := decodeGitArgs(json.RawMessage(tt.input))
			if err != nil {
				t.Fatalf("decodeGitArgs: %v", err)
			}
			if !slices.Equal(gi.Args, tt.want) {
				t.Errorf("decodeGitArgs().Args = %v, want %v", gi.Args, tt.want)
			}
		})
	}
}

func TestGitReadOnlyClassificationUsesArgs(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want bool
	}{
		{name: "status", in: `{"args":["status","--porcelain"]}`, want: true},
		{name: "diff", in: `{"args":["diff","--","internal/tools/git.go"]}`, want: true},
		{name: "log", in: `{"args":["log","--oneline"]}`, want: true},
		{name: "show", in: `{"args":["show","HEAD"]}`, want: true},
		{name: "grep", in: `{"args":["grep","-n","needle"]}`, want: true},
		{name: "commit", in: `{"args":["commit","-m","x"]}`, want: false},
		{name: "global option", in: `{"args":["-C","/tmp","status"]}`, want: false},
		{name: "output flag", in: `{"args":["diff","--output=/tmp/out"]}`, want: false},
		{name: "grep pager", in: `{"args":["grep","-nO/tmp/pager","needle"]}`, want: false},
		{name: "bad json", in: `{not json`, want: false},
		{name: "empty args", in: `{"args":[]}`, want: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := (gitTool{}).ReadOnly(json.RawMessage(tt.in))
			if got != tt.want {
				t.Fatalf("ReadOnly(%s) = %v, want %v", tt.in, got, tt.want)
			}
		})
	}
}

func TestDecodeGitArgsExtractsCwd(t *testing.T) {
	gi, err := decodeGitArgs(json.RawMessage(`{"args":["status"],"cwd":"/tmp"}`))
	if err != nil {
		t.Fatalf("decodeGitArgs: %v", err)
	}
	if gi.Cwd != "/tmp" {
		t.Errorf("Cwd = %q, want %q", gi.Cwd, "/tmp")
	}
}

func TestGitCwd(t *testing.T) {
	gitAvailable(t)
	dir := scratchRepo(t)
	mustWrite(t, dir+"/hello.txt", "hi\n")

	in := map[string]any{"args": []string{"status", "--porcelain"}, "cwd": dir}
	b, err := json.Marshal(in)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	out, err := gitTool{}.Run(context.Background(), b)
	if err != nil {
		t.Fatalf("git status with cwd: %v", err)
	}
	if !strings.Contains(out, "hello.txt") {
		t.Errorf("status should show the untracked file: %q", out)
	}
}

func TestGitCwdInvalidDirectory(t *testing.T) {
	in := map[string]any{"args": []string{"status"}, "cwd": "/nonexistent/path"}
	b, err := json.Marshal(in)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	_, err = gitTool{}.Run(context.Background(), b)
	if err == nil {
		t.Fatal("expected error for nonexistent cwd")
	}
}

func TestGitDetachedFromControllingTTY(t *testing.T) {
	gitAvailable(t)
	if !hasForegroundTTY() {
		t.Skip("no foreground controlling terminal")
	}
	dir := scratchRepo(t)

	out, err := runGit(t, dir,
		"-c", `alias.tty=!sh -c 'if : < /dev/tty 2>/dev/null; then echo has-tty; else echo no-tty; fi'`,
		"tty",
	)
	if err != nil {
		t.Fatalf("git tty probe: %v", err)
	}
	if !strings.Contains(out, "no-tty") {
		t.Fatalf("git command should not have a controlling tty, got %q", out)
	}
	if strings.Contains(out, "has-tty") {
		t.Fatalf("git command unexpectedly accessed controlling tty: %q", out)
	}
}

func TestGitDescriptionsSteerToObjectArgs(t *testing.T) {
	for _, desc := range []string{gitTool{}.Description(), gitReadonly{}.Description()} {
		if !strings.Contains(desc, "JSON object") || !strings.Contains(desc, `{"args":[`) {
			t.Errorf("description should show object-shaped args, got %q", desc)
		}
		if !strings.Contains(desc, "do not pass args as a string or JSON-encoded array") {
			t.Errorf("description should reject stringified args arrays, got %q", desc)
		}
		if strings.Contains(desc, "Pass arguments as an array") {
			t.Errorf("description still encourages bare array args: %q", desc)
		}
	}
}

// Env-inspection seam: the command builder must inject --no-pager as the first
// arg and GIT_TERMINAL_PROMPT=0 into the environment, without running git.
func TestGitCommandSeam(t *testing.T) {
	cmd := buildGitCommand(context.Background(), "", []string{"status", "--porcelain"})

	// --no-pager injected immediately after the program name.
	if len(cmd.Args) < 2 || cmd.Args[1] != "--no-pager" {
		t.Errorf("--no-pager not injected as first arg: %v", cmd.Args)
	}
	if cmd.Args[len(cmd.Args)-2] != "status" || cmd.Args[len(cmd.Args)-1] != "--porcelain" {
		t.Errorf("user args not preserved in order: %v", cmd.Args)
	}

	found := false
	for _, kv := range cmd.Env {
		if kv == "GIT_TERMINAL_PROMPT=0" {
			found = true
		}
	}
	if !found {
		t.Errorf("GIT_TERMINAL_PROMPT=0 not set in env: %v", cmd.Env)
	}
}
