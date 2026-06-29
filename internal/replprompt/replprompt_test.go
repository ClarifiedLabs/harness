package replprompt

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestRenderPlaceholdersAndEscapes(t *testing.T) {
	tmpl, err := Compile(`{agent} {cwd} {hostname} {git_branch} {model}\n\{\}\\\t`)
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}
	got := tmpl.Render(Values{
		Agent:     "plan",
		CWD:       "/repo",
		Hostname:  "devbox",
		GitBranch: "main",
		Model:     "openai:gpt-5.5",
	})
	want := "plan /repo devbox main openai:gpt-5.5\n{}\\\t"
	if got != want {
		t.Fatalf("render = %q, want %q", got, want)
	}
}

func TestHostnameLabel(t *testing.T) {
	cases := []struct {
		hostname string
		style    string
		want     string
	}{
		{"host.example.com", "short", "host"},
		{"host.example.com", "long", "host.example.com"},
		{"host.example.com", "", "host"},
		{"devbox", "short", "devbox"},
		{"devbox", "long", "devbox"},
		{"", "short", ""},
		// Unknown style renders the full host name.
		{"host.example.com", "bogus", "host.example.com"},
	}
	for _, tt := range cases {
		t.Run(tt.hostname+"/"+tt.style, func(t *testing.T) {
			if got := HostnameLabel(tt.hostname, tt.style); got != tt.want {
				t.Fatalf("HostnameLabel(%q, %q) = %q, want %q", tt.hostname, tt.style, got, tt.want)
			}
		})
	}
}

func TestRenderHostnamePlaceholder(t *testing.T) {
	cases := []struct {
		name     string
		format   string
		hostname string
		want     string
	}{
		// Bare {hostname} defaults to the short host name.
		{"hostname bare fqdn", "{hostname}> ", "host.example.com", "host> "},
		{"hostname bare short", "{hostname}> ", "devbox", "devbox> "},
		{"hostname:short fqdn", "{hostname:short}> ", "host.example.com", "host> "},
		{"hostname:long fqdn", "{hostname:long}> ", "host.example.com", "host.example.com> "},
		{"hostname:long short", "{hostname:long}> ", "devbox", "devbox> "},
		// Bare {hostname} behaves identically to {hostname:short}.
		{"hostname bare equals short", "{hostname}={hostname:short}> ", "host.example.com", "host=host> "},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			tmpl, err := Compile(tt.format)
			if err != nil {
				t.Fatalf("Compile: %v", err)
			}
			got := tmpl.Render(Values{Hostname: tt.hostname})
			if got != tt.want {
				t.Fatalf("render = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestUsesHostnameReportsAnyVariant(t *testing.T) {
	for _, format := range []string{"{hostname}> ", "{hostname:long}> ", "{hostname:short}> "} {
		tmpl, err := Compile(format)
		if err != nil {
			t.Fatalf("Compile(%q): %v", format, err)
		}
		if !tmpl.UsesHostname() {
			t.Errorf("UsesHostname(%q) = false, want true", format)
		}
		name := format[1:strings.IndexByte(format, '}')]
		if !tmpl.Uses(name) {
			t.Errorf("Uses(%q) = false, want true for %q", name, format)
		}
	}
	tmpl, err := Compile("{agent}> ")
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}
	if tmpl.UsesHostname() {
		t.Fatalf("UsesHostname should be false when no hostname variant is present")
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
		"{vimode:bogus}",
		"{hostname:bogus}",
		"{agent:x}",
		"{provider}",
		"{model_info}",
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
	tmpl, err := Compile("{agent} {hostname} {model}")
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}
	if !tmpl.Uses("agent") || !tmpl.Uses("hostname") || !tmpl.Uses("model") {
		t.Fatalf("Uses should report referenced placeholders")
	}
	if tmpl.Uses("git_branch") || tmpl.Uses("provider") || tmpl.Uses("model_info") || tmpl.Uses("missing") {
		t.Fatalf("Uses should ignore absent or invalid placeholders")
	}
}

func TestViModeLabel(t *testing.T) {
	cases := []struct {
		mode  string
		style string
		want  string
	}{
		{"insert", "long", "INSERT"},
		{"normal", "long", "NORMAL"},
		{"", "long", ""},
		{"insert", "short", "I"},
		{"normal", "short", "N"},
		{"", "short", ""},
		// The default style (empty) behaves like long, used by bare {vimode}.
		{"insert", "", "INSERT"},
		{"normal", "", "NORMAL"},
		{"", "", ""},
		// Unknown style renders empty.
		{"insert", "bogus", ""},
	}
	for _, tt := range cases {
		t.Run(tt.mode+"/"+tt.style, func(t *testing.T) {
			if got := ViModeLabel(tt.mode, tt.style); got != tt.want {
				t.Fatalf("ViModeLabel(%q, %q) = %q, want %q", tt.mode, tt.style, got, tt.want)
			}
		})
	}
}

func TestRenderViModePlaceholder(t *testing.T) {
	cases := []struct {
		name   string
		format string
		mode   string
		want   string
	}{
		{"vimode insert", "{vimode}> ", "insert", "INSERT> "},
		{"vimode normal", "{vimode}> ", "normal", "NORMAL> "},
		{"vimode emacs empty", "{vimode}> ", "", "> "},
		{"vimode:long insert", "{vimode:long}> ", "insert", "INSERT> "},
		{"vimode:long normal", "{vimode:long}> ", "normal", "NORMAL> "},
		{"vimode:short insert", "{vimode:short}> ", "insert", "I> "},
		{"vimode:short normal", "{vimode:short}> ", "normal", "N> "},
		{"vimode:short emacs empty", "{vimode:short}> ", "", "> "},
		// Bare {vimode} behaves identically to {vimode:long}.
		{"vimode bare equals long", "{vimode}={vimode:long}> ", "normal", "NORMAL=NORMAL> "},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			tmpl, err := Compile(tt.format)
			if err != nil {
				t.Fatalf("Compile: %v", err)
			}
			got := tmpl.Render(Values{ViMode: tt.mode})
			if got != tt.want {
				t.Fatalf("render = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestUsesViModeReportsAnyVariant(t *testing.T) {
	for _, format := range []string{"{vimode}> ", "{vimode:long}> ", "{vimode:short}> "} {
		tmpl, err := Compile(format)
		if err != nil {
			t.Fatalf("Compile(%q): %v", format, err)
		}
		if !tmpl.UsesViMode() {
			t.Errorf("UsesViMode(%q) = false, want true", format)
		}
		// The specific variant should also be individually reported by Uses.
		name := format[1:strings.IndexByte(format, '}')]
		if !tmpl.Uses(name) {
			t.Errorf("Uses(%q) = false, want true for %q", name, format)
		}
	}
	tmpl, err := Compile("{agent}> ")
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}
	if tmpl.UsesViMode() {
		t.Fatalf("UsesViMode should be false when no vimode variant is present")
	}
}

func TestDefaultFormatUnchanged(t *testing.T) {
	if DefaultFormat != "[{agent}] > " {
		t.Fatalf("DefaultFormat = %q, want [{agent}] > ", DefaultFormat)
	}
	tmpl, err := Compile("")
	if err != nil {
		t.Fatalf("Compile(\"\"): %v", err)
	}
	got := tmpl.Render(Values{Agent: "auto", ViMode: "insert"})
	if got != "[auto] > " {
		t.Fatalf("default render = %q, want [auto] >  (ViMode must not leak in)", got)
	}
}

func TestAbbreviateHome(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		t.Skipf("home directory unavailable: %v", err)
	}
	for _, tt := range []struct {
		name string
		cwd  string
		want string
	}{
		{name: "empty", cwd: "", want: ""},
		{name: "exact home", cwd: home, want: "~"},
		{name: "under home", cwd: filepath.Join(home, "work"), want: "~/work"},
		{name: "nested under home", cwd: filepath.Join(home, "a", "b"), want: "~/a/b"},
		{name: "outside home", cwd: "/repo", want: "/repo"},
		{name: "home prefix only", cwd: home + "ish", want: home + "ish"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			tmpl, err := Compile("{cwd}")
			if err != nil {
				t.Fatalf("Compile: %v", err)
			}
			if got := tmpl.Render(Values{CWD: tt.cwd}); got != tt.want {
				t.Fatalf("render = %q, want %q", got, tt.want)
			}
		})
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
