package sysprompt

import (
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"harness/prompts"
)

func writeFileForTest(path, content string) error {
	return os.WriteFile(path, []byte(content), 0o644)
}

func gitAvailable(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
}

// scratchRepo initializes a fresh git repo on a known branch with identity set.
func scratchRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	for _, argv := range [][]string{
		{"init", "-q", "-b", "main"},
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

func git(t *testing.T, dir string, argv ...string) {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, argv...)...)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", argv, err, out)
	}
}

func write(t *testing.T, path, content string) {
	t.Helper()
	if err := writeFileForTest(path, content); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

var fixedDate = time.Date(2026, 6, 9, 0, 0, 0, 0, time.UTC)

func TestEnvBlockShape(t *testing.T) {
	dir := t.TempDir() // not a git repo
	env := EnvContext(EnvOptions{Dir: dir, Now: func() time.Time { return fixedDate }})

	if !strings.HasPrefix(env, "Environment:\n") {
		t.Fatalf("env block missing Environment header:\n%s", env)
	}
	if strings.Contains(env, "<env>") || strings.Contains(env, "</env>") {
		t.Fatalf("env block should not use XML tags:\n%s", env)
	}
	if !strings.Contains(env, "cwd: "+dir) {
		t.Errorf("env missing cwd line:\n%s", env)
	}
	if !strings.Contains(env, "os: ") {
		t.Errorf("env missing os line:\n%s", env)
	}
	if !strings.Contains(env, "date: 2026-06-09") {
		t.Errorf("env missing date line:\n%s", env)
	}
}

func TestEnvNonRepo(t *testing.T) {
	dir := t.TempDir()
	env := EnvContext(EnvOptions{Dir: dir, Now: func() time.Time { return fixedDate }})
	if !strings.Contains(env, "git: (not a git repository)") {
		t.Errorf("non-repo dir should report not-a-repo:\n%s", env)
	}
}

func TestEnvGitSummary(t *testing.T) {
	gitAvailable(t)
	dir := scratchRepo(t)
	// One committed-then-modified file, one untracked file.
	write(t, dir+"/tracked.txt", "v1\n")
	git(t, dir, "add", "tracked.txt")
	git(t, dir, "commit", "-q", "-m", "init")
	write(t, dir+"/tracked.txt", "v2\n")
	write(t, dir+"/new.txt", "hello\n")

	env := EnvContext(EnvOptions{Dir: dir, Now: func() time.Time { return fixedDate }})

	if !strings.Contains(env, "branch=main") {
		t.Errorf("git line should name the branch:\n%s", env)
	}
	if !strings.Contains(env, "1 modified") {
		t.Errorf("git line should count modified files:\n%s", env)
	}
	if !strings.Contains(env, "1 untracked") {
		t.Errorf("git line should count untracked files:\n%s", env)
	}
}

func TestBuildIncludesBuiltinAndEnv(t *testing.T) {
	dir := t.TempDir()
	out := Build(Options{
		Env: EnvOptions{Dir: dir, Now: func() time.Time { return fixedDate }},
	})
	if !strings.Contains(out, prompts.System()) {
		t.Errorf("builder should keep builtin instructions")
	}
	if !strings.Contains(out, "Environment:\n") {
		t.Errorf("env block should be present by default")
	}
	// builtin then env then append, with the env block intact.
	if !strings.Contains(out, prompts.System()+"\n\nEnvironment:\n") {
		t.Errorf("builtin should be followed by the env block:\n%s", out)
	}
}

func TestBuildIncludesAgentsMDSections(t *testing.T) {
	dir := t.TempDir()
	out := Build(Options{
		UserAgentsMD:    "# User rules\nPrefer personal defaults.",
		ProjectAgentsMD: "# Project rules\nAlways write tests.",
		Env:             EnvOptions{Dir: dir, Now: func() time.Time { return fixedDate }},
	})
	if !strings.Contains(out, "# User rules\nPrefer personal defaults.") {
		t.Errorf("user AGENTS.md content should appear in system prompt:\n%s", out)
	}
	if !strings.Contains(out, "# Project rules\nAlways write tests.") {
		t.Errorf("project AGENTS.md content should appear in system prompt:\n%s", out)
	}
	// Order: builtin -> env -> user AGENTS.md -> project AGENTS.md.
	envIdx := strings.Index(out, "Environment:\n")
	userIdx := strings.Index(out, "# User rules")
	projectIdx := strings.Index(out, "# Project rules")
	if envIdx < 0 || userIdx < 0 || projectIdx < 0 || envIdx >= userIdx || userIdx >= projectIdx {
		t.Errorf("AGENTS.md sections should come after env in user-before-project order:\n%s", out)
	}
}

func TestBuildEmptyAgentsMDOmitted(t *testing.T) {
	dir := t.TempDir()
	out := Build(Options{
		UserAgentsMD:    "",
		ProjectAgentsMD: "",
		Env:             EnvOptions{Dir: dir, Now: func() time.Time { return fixedDate }},
	})
	// No double blank-line gap beyond the normal separators.
	if strings.Contains(out, "\n\n\n\n") {
		t.Errorf("empty AGENTS.md should not leave extra blank lines:\n%q", out)
	}
}

// The agent prompt is the final section: after builtin instructions, env,
// AGENTS.md sections, and skills, so an agent layers on top of everything else.
func TestBuildAgentPromptAppendedLast(t *testing.T) {
	out := Build(Options{
		ProjectAgentsMD: "agents rules",
		AgentPrompt:     "agent section",
		NoEnv:           true,
	})
	if !strings.HasSuffix(out, "agents rules\n\nagent section") {
		t.Errorf("agent prompt should be the final section:\n%s", out)
	}
	if !strings.Contains(out, prompts.System()) {
		t.Errorf("builtin instructions must be kept")
	}
}

func TestBuildEmptyAgentPromptOmitted(t *testing.T) {
	out := Build(Options{NoEnv: true})
	if out != prompts.System() {
		t.Errorf("no options should yield just the builtin instructions:\n%s", out)
	}
}

func TestBuildNoEnvDropsEnvBlock(t *testing.T) {
	dir := t.TempDir()
	out := Build(Options{
		NoEnv: true,
		Env:   EnvOptions{Dir: dir, Now: func() time.Time { return fixedDate }},
	})
	if strings.Contains(out, "Environment:\n") {
		t.Errorf("-no-env should drop the env block:\n%s", out)
	}
	if !strings.Contains(out, prompts.System()) {
		t.Errorf("builtin should remain with -no-env")
	}
	if strings.HasSuffix(out, "\n\n") {
		t.Errorf("no trailing separator should remain when env is dropped: %q", out)
	}
}
