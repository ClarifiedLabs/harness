package tools

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func runGrep(t *testing.T, args map[string]any) (string, error) {
	return runTool(t, grep{}, args)
}

func runGrepWithBG(t *testing.T, bg BackgroundJobStarter, args map[string]any) (string, error) {
	return runTool(t, grep{background: bg}, args)
}

func TestGrepRunsHostGrep(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "a.txt")
	mustWrite(t, p, "hello\nneedle here\n")

	out, err := runGrep(t, map[string]any{"args": []string{"-n", "needle", p}})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(out, "2:needle here") {
		t.Errorf("grep output missing match line: %q", out)
	}
	if !strings.Contains(out, "[exit code: 0]") {
		t.Errorf("grep output missing exit code: %q", out)
	}
}

func TestGrepArgsPassedLiterally(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "a.txt")
	mustWrite(t, p, "$HOME\na*b\n")

	out, err := runGrep(t, map[string]any{"args": []string{"-F", "-n", "$HOME", p}})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(out, "1:$HOME") {
		t.Errorf("argument should reach grep without shell expansion: %q", out)
	}
	if strings.Contains(out, os.Getenv("HOME")) && os.Getenv("HOME") != "$HOME" {
		t.Errorf("argument appears to have expanded through a shell: %q", out)
	}
}

func TestGrepCwdAndStdin(t *testing.T) {
	dir := t.TempDir()
	mustWrite(t, filepath.Join(dir, "a.txt"), "needle in file\n")

	out, err := runGrep(t, map[string]any{
		"args": []string{"-n", "needle", "a.txt"},
		"cwd":  dir,
	})
	if err != nil {
		t.Fatalf("unexpected cwd error: %v", err)
	}
	if !strings.Contains(out, "1:needle in file") {
		t.Errorf("grep did not run in cwd: %q", out)
	}

	out, err = runGrep(t, map[string]any{
		"args":  []string{"-n", "stdin"},
		"stdin": "stdin match\n",
	})
	if err != nil {
		t.Fatalf("unexpected stdin error: %v", err)
	}
	if !strings.Contains(out, "1:stdin match") {
		t.Errorf("stdin was not passed to grep: %q", out)
	}
}

func TestGrepBackgroundStartsJob(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "a.txt")
	mustWrite(t, p, "hello\nneedle here\n")

	starter := &fakeBackgroundStarter{}
	out, err := runGrepWithBG(t, starter, map[string]any{
		"args":       []string{"-n", "needle", p},
		"background": true,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if out != "background job bg_test started" {
		t.Fatalf("start output = %q", out)
	}
	if starter.req.Kind != "grep" {
		t.Fatalf("job kind = %q, want grep", starter.req.Kind)
	}
	if !strings.Contains(starter.req.Description, "grep") {
		t.Fatalf("job description = %q", starter.req.Description)
	}
	if starter.req.Run == nil {
		t.Fatal("background job runner missing")
	}

	result, err := starter.req.Run(context.Background(), "bg_test")
	if err != nil {
		t.Fatalf("background run: %v", err)
	}
	if !strings.Contains(result.Text, "needle here") || !strings.Contains(result.Text, "[exit code: 0]") {
		t.Fatalf("background result = %q", result.Text)
	}
}

func TestGrepBackgroundRequiresStarter(t *testing.T) {
	_, err := runGrep(t, map[string]any{
		"args":       []string{"-n", "needle", "."},
		"background": true,
	})
	if err == nil {
		t.Fatal("expected error when background manager is unavailable")
	}
	if !strings.Contains(err.Error(), "background manager") {
		t.Fatalf("error = %v", err)
	}
}

func TestGrepNonZeroExitNotToolError(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "a.txt")
	mustWrite(t, p, "nothing here\n")

	out, err := runGrep(t, map[string]any{"args": []string{"absent", p}})
	if err != nil {
		t.Fatalf("grep exit 1 must not be a tool error: %v", err)
	}
	if !strings.Contains(out, "[exit code: 1]") {
		t.Errorf("grep no-match should report exit 1: %q", out)
	}
}

func TestGrepValidatesArgs(t *testing.T) {
	if _, err := runGrep(t, map[string]any{}); err == nil {
		t.Fatal("expected error for missing args")
	}
	if _, err := runGrep(t, map[string]any{"args": []string{}}); err == nil {
		t.Fatal("expected error for empty args")
	}
	if _, err := runGrep(t, map[string]any{"args": []string{"x"}, "timeout_seconds": -1}); err == nil {
		t.Fatal("expected error for negative timeout")
	}
}

func TestDecodeSearchCommandArgsAcceptsObjectAndBareArray(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  programArgs
	}{
		{
			name:  "object",
			input: `{"args":["-n","needle","."],"cwd":"/tmp","timeout_seconds":5}`,
			want:  programArgs{Args: []string{"-n", "needle", "."}, Cwd: "/tmp", TimeoutSeconds: 5},
		},
		{
			name:  "bare array",
			input: `["-n","needle","."]`,
			want:  programArgs{Args: []string{"-n", "needle", "."}},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := decodeSearchCommandArgs(json.RawMessage(tt.input))
			if err != nil {
				t.Fatalf("decodeSearchCommandArgs: %v", err)
			}
			if !slices.Equal(got.Args, tt.want.Args) || got.Cwd != tt.want.Cwd || got.TimeoutSeconds != tt.want.TimeoutSeconds {
				t.Errorf("decodeSearchCommandArgs() = %+v, want %+v", got, tt.want)
			}
		})
	}
}

func TestSearchCommandDescriptionsSteerToObjectArgs(t *testing.T) {
	for _, desc := range []string{grep{}.Description(), ripgrep{}.Description()} {
		if !strings.Contains(desc, "JSON object") || !strings.Contains(desc, `{"args":[`) {
			t.Errorf("description should show object-shaped args, got %q", desc)
		}
		if !strings.Contains(desc, "do not pass args as a string or JSON-encoded array") {
			t.Errorf("description should reject stringified args arrays, got %q", desc)
		}
		if strings.Contains(desc, "Pass grep options") || strings.Contains(desc, "Pass ripgrep options") {
			t.Errorf("description still encourages bare array args: %q", desc)
		}
	}
}

func TestRipgrepNotRegisteredWhenMissing(t *testing.T) {
	t.Setenv("PATH", t.TempDir())

	if RipgrepAvailable() {
		t.Fatal("rg should not be available from an empty PATH")
	}
	r := &Registry{}
	RegisterFileTools(r)
	if slices.Contains(r.Names(), "rg") {
		t.Errorf("RegisterFileTools registered rg even though it is missing: %v", r.Names())
	}
}

func TestRipgrepBackgroundStartsJob(t *testing.T) {
	dir := t.TempDir()
	makeExecutable(t, filepath.Join(dir, "rg"), "#!/bin/sh\nprintf 'fake rg output'\n")
	t.Setenv("PATH", dir)

	starter := &fakeBackgroundStarter{}
	rg, ok := newRipgrep(starter)
	if !ok {
		t.Fatal("expected fake rg to be found on PATH")
	}
	out, err := runTool(t, rg, map[string]any{
		"args":       []string{"-n", "needle", "."},
		"background": true,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if out != "background job bg_test started" {
		t.Fatalf("start output = %q", out)
	}
	if starter.req.Kind != "rg" {
		t.Fatalf("job kind = %q, want rg", starter.req.Kind)
	}
	if !strings.Contains(starter.req.Description, "rg") {
		t.Fatalf("job description = %q", starter.req.Description)
	}
	if starter.req.Run == nil {
		t.Fatal("background job runner missing")
	}

	result, err := starter.req.Run(context.Background(), "bg_test")
	if err != nil {
		t.Fatalf("background run: %v", err)
	}
	if !strings.Contains(result.Text, "fake rg output") {
		t.Fatalf("background result = %q", result.Text)
	}
}

func TestRipgrepBackgroundRequiresStarter(t *testing.T) {
	dir := t.TempDir()
	makeExecutable(t, filepath.Join(dir, "rg"), "#!/bin/sh\nexit 0\n")
	t.Setenv("PATH", dir)

	rg, ok := newRipgrep(nil)
	if !ok {
		t.Fatal("expected fake rg to be found on PATH")
	}
	_, err := runTool(t, rg, map[string]any{
		"args":       []string{"-n", "needle", "."},
		"background": true,
	})
	if err == nil {
		t.Fatal("expected error when background manager is unavailable")
	}
	if !strings.Contains(err.Error(), "background manager") {
		t.Fatalf("error = %v", err)
	}
}

func TestRipgrepAddsDefaultGuardsToNormalSearch(t *testing.T) {
	dir := t.TempDir()
	makeExecutable(t, filepath.Join(dir, "rg"), `#!/bin/sh
printf 'fake rg:'
for arg in "$@"; do
  printf ' <%s>' "$arg"
done
printf '\n'
`)
	t.Setenv("PATH", dir)

	rg, ok := newRipgrep(nil)
	if !ok {
		t.Fatal("expected fake rg to be found on PATH")
	}
	out, err := rg.Run(context.Background(), json.RawMessage(`{"args":["-n","needle","."]}`))
	if err != nil {
		t.Fatalf("rg wrapper returned error: %v", err)
	}
	want := "fake rg: <--max-columns=1024> <--max-columns-preview> <--max-filesize=10M> <-n> <needle> <.>"
	if !strings.Contains(out, want) {
		t.Errorf("rg guards not injected before search args:\n got %q\nwant %q", out, want)
	}
}

func TestGuardRipgrepArgs(t *testing.T) {
	tests := []struct {
		name string
		in   []string
		want []string
	}{
		{
			name: "normal search",
			in:   []string{"-n", "needle", "."},
			want: []string{"--max-columns=1024", "--max-columns-preview", "--max-filesize=10M", "-n", "needle", "."},
		},
		{
			name: "explicit long line policy keeps filesize guard",
			in:   []string{"--max-columns=0", "needle", "."},
			want: []string{"--max-filesize=10M", "--max-columns=0", "needle", "."},
		},
		{
			name: "explicit short long line policy keeps filesize guard",
			in:   []string{"-M", "2048", "needle", "."},
			want: []string{"--max-filesize=10M", "-M", "2048", "needle", "."},
		},
		{
			name: "explicit filesize keeps long line guard",
			in:   []string{"--max-filesize", "100G", "needle", "."},
			want: []string{"--max-columns=1024", "--max-columns-preview", "--max-filesize", "100G", "needle", "."},
		},
		{
			name: "explicit policies win",
			in:   []string{"--max-columns=0", "--max-filesize=100G", "needle", "."},
			want: []string{"--max-columns=0", "--max-filesize=100G", "needle", "."},
		},
		{
			name: "json mode is raw",
			in:   []string{"--json", "needle", "."},
			want: []string{"--json", "needle", "."},
		},
		{
			name: "files mode is raw",
			in:   []string{"--files", "."},
			want: []string{"--files", "."},
		},
		{
			name: "delimiter stops flag detection",
			in:   []string{"needle", "--", "--json"},
			want: []string{"--max-columns=1024", "--max-columns-preview", "--max-filesize=10M", "needle", "--", "--json"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := guardRipgrepArgs(tt.in)
			if !slices.Equal(got, tt.want) {
				t.Errorf("guardRipgrepArgs(%v) = %v, want %v", tt.in, got, tt.want)
			}
		})
	}
}

func TestRipgrepRegisteredAndRunsWhenPresent(t *testing.T) {
	dir := t.TempDir()
	makeExecutable(t, filepath.Join(dir, "rg"), `#!/bin/sh
printf 'fake rg:'
for arg in "$@"; do
  printf ' <%s>' "$arg"
done
printf '\n'
`)
	t.Setenv("PATH", dir)

	rg, ok := newRipgrep(nil)
	if !ok {
		t.Fatal("expected fake rg to be found on PATH")
	}
	out, err := rg.Run(context.Background(), json.RawMessage(`{"args":["--json","needle with space"]}`))
	if err != nil {
		t.Fatalf("rg wrapper returned error: %v", err)
	}
	if !strings.Contains(out, "fake rg: <--json> <needle with space>") {
		t.Errorf("rg args not passed literally: %q", out)
	}

	r := &Registry{}
	RegisterFileTools(r)
	names := r.Names()
	grepIndex := slices.Index(names, "grep")
	rgIndex := slices.Index(names, "rg")
	editIndex := slices.Index(names, "edit")
	if rgIndex < 0 {
		t.Fatalf("RegisterFileTools did not include rg: %v", names)
	}
	if grepIndex >= 0 {
		t.Fatalf("auto search mode should expose rg instead of grep when rg is available: %v", names)
	}
	if !(rgIndex < editIndex) {
		t.Errorf("rg should be registered before edit: %v", names)
	}
}

func makeExecutable(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o755); err != nil {
		t.Fatalf("write executable %s: %v", path, err)
	}
}
