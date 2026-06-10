package replprompt

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestRenderPlaceholdersAndEscapes(t *testing.T) {
	tmpl, err := Compile(`{agent} {cwd} {git_branch} {provider} {model} {model_info}\n\{\}\\\t`)
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}
	got := tmpl.Render(Values{
		Agent:     "plan",
		CWD:       "/repo",
		GitBranch: "main",
		Provider:  "openai",
		Model:     "gpt-5.5",
	})
	want := "plan /repo main openai gpt-5.5 openai:gpt-5.5\n{}\\\t"
	if got != want {
		t.Fatalf("render = %q, want %q", got, want)
	}
}

func TestRenderModelInfoFallbacks(t *testing.T) {
	tests := []struct {
		name     string
		provider string
		model    string
		want     string
	}{
		{name: "provider and model", provider: "anthropic", model: "claude", want: "anthropic:claude"},
		{name: "model only", model: "local", want: "local"},
		{name: "empty", want: ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tmpl, err := Compile("{model_info}")
			if err != nil {
				t.Fatalf("Compile: %v", err)
			}
			got := tmpl.Render(Values{Provider: tt.provider, Model: tt.model})
			if got != tt.want {
				t.Fatalf("render = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestCompileRejectsInvalidFormat(t *testing.T) {
	tests := []string{
		"{unknown}",
		"{",
		"}",
		"{}",
		`bad\q`,
		`bad\`,
	}

	for _, format := range tests {
		t.Run(format, func(t *testing.T) {
			if _, err := Compile(format); err == nil {
				t.Fatalf("Compile(%q) succeeded, want error", format)
			}
		})
	}
}

func TestLiteralPromptStillWorks(t *testing.T) {
	tmpl, err := Compile("$ ")
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}
	if got := tmpl.Render(Values{Agent: "plan"}); got != "$ " {
		t.Fatalf("render = %q, want literal prompt", got)
	}
}

func TestUsesReportsReferencedPlaceholders(t *testing.T) {
	tmpl, err := Compile("{agent} {model_info}")
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}
	if !tmpl.Uses("agent") || !tmpl.Uses("model_info") {
		t.Fatalf("Uses should report referenced placeholders")
	}
	if tmpl.Uses("git_branch") || tmpl.Uses("missing") {
		t.Fatalf("Uses should ignore absent or invalid placeholders")
	}
}

func TestCurrentGitBranch(t *testing.T) {
	gitAvailable(t)
	dir := scratchRepo(t)
	if got := CurrentGitBranch(dir); got != "main" {
		t.Fatalf("CurrentGitBranch = %q, want main", got)
	}
	git(t, dir, "checkout", "-q", "-b", "feature/prompt")
	if got := CurrentGitBranch(dir); got != "feature/prompt" {
		t.Fatalf("CurrentGitBranch after branch switch = %q, want feature/prompt", got)
	}
}

func gitAvailable(t *testing.T) {
	t.Helper()
	if err := exec.Command("git", "--version").Run(); err != nil {
		t.Skipf("git not available: %v", err)
	}
}

func scratchRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	git(t, dir, "init", "-q", "-b", "main")
	git(t, dir, "config", "user.email", "test@example.com")
	git(t, dir, "config", "user.name", "Test User")
	path := filepath.Join(dir, "file.txt")
	if err := writeFile(path, "hello\n"); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	git(t, dir, "add", "file.txt")
	git(t, dir, "commit", "-q", "-m", "init")
	return dir
}

func git(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
}

func writeFile(path, content string) error {
	return os.WriteFile(path, []byte(content), 0o644)
}
