package lineage

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestManagerPreservesStrictlyImprovingAcceptedLineage(t *testing.T) {
	repo, worktree := newLinkedWorktree(t)
	sessionDir := filepath.Join(t.TempDir(), "session")
	manager, err := Open(worktree, sessionDir)
	if err != nil {
		t.Fatal(err)
	}

	writeTestFile(t, filepath.Join(worktree, "candidate.txt"), "candidate=one\n")
	score10 := 10.0
	first, err := manager.Observe(Input{
		Handler: "quality", Accepted: true, Score: &score10, ScoreDirection: ScoreDirectionMaximize,
		Candidate: "candidate:one", EvidenceRef: "evidence/one.txt", Prompt: 1, Turn: 2,
	})
	if err != nil {
		t.Fatal(err)
	}
	if first == nil || first.Sequence != 1 || first.ParentSequence != 0 || first.EvidenceBytes == 0 {
		t.Fatalf("first advance = %+v", first)
	}

	writeTestFile(t, filepath.Join(worktree, "candidate.txt"), "candidate=rejected\n")
	score99 := 99.0
	advance, err := manager.Observe(Input{
		Handler: "quality", Accepted: false, Score: &score99, ScoreDirection: ScoreDirectionMaximize,
		Candidate: "candidate:rejected", EvidenceRef: "evidence/two.txt", Prompt: 2, Turn: 1,
	})
	if err != nil || advance != nil {
		t.Fatalf("rejected result advance/error = %+v / %v", advance, err)
	}

	score5 := 5.0
	advance, err = manager.Observe(Input{
		Handler: "quality", Accepted: true, Score: &score5, ScoreDirection: ScoreDirectionMaximize,
		Candidate: "candidate:worse", EvidenceRef: "evidence/two.txt", Prompt: 2, Turn: 2,
	})
	if err != nil || advance != nil {
		t.Fatalf("non-improving result advance/error = %+v / %v", advance, err)
	}

	writeTestFile(t, filepath.Join(worktree, "candidate.txt"), "candidate=best\n")
	score20 := 20.0
	second, err := manager.Observe(Input{
		Handler: "quality", Accepted: true, Score: &score20, ScoreDirection: ScoreDirectionMaximize,
		Candidate: "candidate:best", EvidenceRef: "evidence/two.txt", Prompt: 3, Turn: 3,
	})
	if err != nil {
		t.Fatal(err)
	}
	if second == nil || second.Sequence != 2 || second.ParentSequence != 1 {
		t.Fatalf("second advance = %+v", second)
	}

	state, err := Load(sessionDir)
	if err != nil {
		t.Fatal(err)
	}
	if state.Version != SchemaVersion || state.BaseHead == "" || state.BaseTree == "" || len(state.Entries) != 2 {
		t.Fatalf("state = %+v", state)
	}
	if state.BestScore == nil || *state.BestScore != score20 || state.BestCandidate != "candidate:best" || state.Entries[1].ParentSequence != 1 {
		t.Fatalf("best state = %+v", state)
	}
	if got := strings.TrimSpace(runGit(t, repo, "show", state.BestTree+":candidate.txt")); got != "candidate=best" {
		t.Fatalf("best tree candidate = %q", got)
	}
	if evidence, err := os.ReadFile(filepath.Join(sessionDir, filepath.FromSlash(state.Entries[0].EvidenceArtifact))); err != nil || string(evidence) != "first evidence\n" {
		t.Fatalf("first evidence = %q, %v", evidence, err)
	}

	reopened, err := Open(worktree, sessionDir)
	if err != nil {
		t.Fatalf("reopen lineage: %v", err)
	}
	if got := reopened.Snapshot(); len(got.Entries) != 2 || got.BestTree != state.BestTree {
		t.Fatalf("reopened state = %+v", got)
	}
}

func TestManagerAcceptsInteractiveWorktreeBoundaries(t *testing.T) {
	repo, detached := newLinkedWorktree(t)
	if _, err := Open(repo, filepath.Join(t.TempDir(), "primary-session")); err != nil {
		t.Fatalf("primary checkout: %v", err)
	}

	branchWorktree := filepath.Join(t.TempDir(), "branch-worktree")
	runGit(t, repo, "worktree", "add", "-b", "lineage-test-branch", branchWorktree, "HEAD")
	if _, err := Open(branchWorktree, filepath.Join(t.TempDir(), "branch-session")); err != nil {
		t.Fatalf("branch worktree: %v", err)
	}
	if _, err := Open(detached, filepath.Join(t.TempDir(), "detached-session")); err != nil {
		t.Fatalf("detached linked worktree: %v", err)
	}

	if _, err := Open(detached, filepath.Join(detached, "session")); err == nil || !strings.Contains(err.Error(), "outside") {
		t.Fatalf("inside session error = %v", err)
	}
	subdir := filepath.Join(detached, "subdir")
	if err := os.MkdirAll(subdir, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(subdir, filepath.Join(t.TempDir(), "subdir-session")); err != nil {
		t.Fatalf("subdirectory launch: %v", err)
	}
	if _, err := Open(t.TempDir(), filepath.Join(t.TempDir(), "non-git-session")); err == nil || !strings.Contains(err.Error(), "requires a Git worktree") {
		t.Fatalf("non-Git directory error = %v", err)
	}
}

func TestManagerRejectsEvidenceOutsideWorktree(t *testing.T) {
	_, worktree := newLinkedWorktree(t)
	sessionDir := filepath.Join(t.TempDir(), "session")
	manager, err := Open(worktree, sessionDir)
	if err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(t.TempDir(), "outside.txt")
	writeTestFile(t, outside, "private\n")
	link := filepath.Join(worktree, "evidence", "outside.txt")
	if err := os.Symlink(outside, link); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	writeTestFile(t, filepath.Join(worktree, "candidate.txt"), "candidate=unsafe\n")
	score := 1.0
	advance, err := manager.Observe(Input{
		Handler: "quality", Accepted: true, Score: &score, ScoreDirection: ScoreDirectionMaximize,
		Candidate: "candidate:unsafe", EvidenceRef: "evidence/outside.txt",
	})
	if err == nil || !strings.Contains(err.Error(), "outside the worktree") || advance != nil {
		t.Fatalf("unsafe evidence advance/error = %+v / %v", advance, err)
	}
	if got := manager.Snapshot(); len(got.Entries) != 0 || got.BestScore != nil {
		t.Fatalf("unsafe evidence changed state: %+v", got)
	}
}

func TestOpenRejectsIncompleteArchiveWithoutManifest(t *testing.T) {
	_, worktree := newLinkedWorktree(t)
	sessionDir := filepath.Join(t.TempDir(), "session")
	stale := filepath.Join(sessionDir, "lineage", "patches", "0001.patch")
	writeTestFile(t, stale, "orphaned\n")
	if _, err := Open(worktree, sessionDir); err == nil || !strings.Contains(err.Error(), "incomplete archive") {
		t.Fatalf("incomplete archive error = %v", err)
	}
	if content, err := os.ReadFile(stale); err != nil || string(content) != "orphaned\n" {
		t.Fatalf("stale artifact was overwritten: %q, %v", content, err)
	}
}

func TestManagerPreservesAcrossHeadChanges(t *testing.T) {
	_, worktree := newLinkedWorktree(t)
	sessionDir := filepath.Join(t.TempDir(), "session")
	manager, err := Open(worktree, sessionDir)
	if err != nil {
		t.Fatal(err)
	}
	baseHead := manager.Snapshot().BaseHead
	runGit(t, worktree, "commit", "--allow-empty", "-qm", "test: move detached head")
	writeTestFile(t, filepath.Join(worktree, "candidate.txt"), "candidate=after-head-change\n")
	score := 1.0
	advance, err := manager.Observe(Input{
		Handler: "quality", Accepted: true, Score: &score, ScoreDirection: ScoreDirectionMaximize,
		Candidate: "candidate:moved", EvidenceRef: "evidence/one.txt",
	})
	if err != nil || advance == nil || advance.Sequence != 1 {
		t.Fatalf("HEAD change advance/error = %+v / %v", advance, err)
	}
	state := manager.Snapshot()
	if state.BaseHead != baseHead || state.Entries[0].Tree == state.BaseTree {
		t.Fatalf("state after HEAD change = %+v", state)
	}
	if _, err := Open(worktree, sessionDir); err != nil {
		t.Fatalf("reopen after HEAD change: %v", err)
	}
}

func TestManagerCaptureLeavesRealIndexAndRefUnchanged(t *testing.T) {
	repo, _ := newLinkedWorktree(t)
	writeTestFile(t, filepath.Join(repo, "candidate.txt"), "candidate=staged\n")
	runGit(t, repo, "add", "candidate.txt")
	writeTestFile(t, filepath.Join(repo, "candidate.txt"), "candidate=working\n")
	beforeIndex := readGitIndex(t, repo)
	beforeHead := strings.TrimSpace(runGit(t, repo, "rev-parse", "HEAD"))
	beforeRef := strings.TrimSpace(runGit(t, repo, "symbolic-ref", "HEAD"))

	manager, err := Open(repo, filepath.Join(t.TempDir(), "session"))
	if err != nil {
		t.Fatal(err)
	}
	score := 1.0
	advance, err := manager.Observe(Input{
		Handler: "quality", Accepted: true, Score: &score, ScoreDirection: ScoreDirectionMaximize,
		Candidate: "candidate:working", EvidenceRef: "evidence/one.txt",
	})
	if err != nil || advance == nil {
		t.Fatalf("advance/error = %+v / %v", advance, err)
	}
	if after := readGitIndex(t, repo); !bytes.Equal(after, beforeIndex) {
		t.Fatal("candidate capture changed the real Git index")
	}
	if after := strings.TrimSpace(runGit(t, repo, "rev-parse", "HEAD")); after != beforeHead {
		t.Fatalf("HEAD = %s, want %s", after, beforeHead)
	}
	if after := strings.TrimSpace(runGit(t, repo, "symbolic-ref", "HEAD")); after != beforeRef {
		t.Fatalf("symbolic ref = %s, want %s", after, beforeRef)
	}
}

func TestManagerExportsAndRestoresAcceptedCheckpoint(t *testing.T) {
	repo, _ := newLinkedWorktree(t)
	sessionDir := filepath.Join(t.TempDir(), "session")
	manager, err := Open(repo, sessionDir)
	if err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, filepath.Join(repo, "candidate.txt"), "candidate=best\n")
	score := 10.0
	if advance, err := manager.Observe(Input{
		Handler: "quality", Accepted: true, Score: &score, ScoreDirection: ScoreDirectionMaximize,
		Candidate: "candidate:best", EvidenceRef: "evidence/one.txt",
	}); err != nil || advance == nil {
		t.Fatalf("advance/error = %+v / %v", advance, err)
	}

	exportDir := filepath.Join(t.TempDir(), "accepted")
	exported, err := manager.Export(1, exportDir)
	if err != nil {
		t.Fatal(err)
	}
	if exported.Sequence != 1 || !samePath(exported.Path, exportDir) {
		t.Fatalf("export = %+v", exported)
	}
	if got, err := os.ReadFile(filepath.Join(exportDir, "candidate.txt")); err != nil || string(got) != "candidate=best\n" {
		t.Fatalf("exported candidate = %q, %v", got, err)
	}
	if _, err := manager.Export(1, exportDir); err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("second export error = %v", err)
	}
	if _, err := manager.Export(1, filepath.Join(repo, "export")); err == nil || !strings.Contains(err.Error(), "outside the source worktree") {
		t.Fatalf("in-worktree export error = %v", err)
	}

	runGit(t, repo, "add", "candidate.txt")
	writeTestFile(t, filepath.Join(repo, "candidate.txt"), "candidate=regressed\n")
	writeTestFile(t, filepath.Join(repo, "scratch.txt"), "unaccepted scratch\n")
	beforeIndex := readGitIndex(t, repo)
	beforeHead := strings.TrimSpace(runGit(t, repo, "rev-parse", "HEAD"))
	if _, err := manager.Restore(1, false); err == nil || !strings.Contains(err.Error(), "--force") {
		t.Fatalf("unguarded restore error = %v", err)
	}
	restored, err := manager.Restore(1, true)
	if err != nil {
		t.Fatal(err)
	}
	if !restored.Changed || restored.Sequence != 1 || restored.RecoveryPatch == "" || restored.RecoveryPatchBytes == 0 || !validDigest(restored.RecoveryPatchSHA256) {
		t.Fatalf("restore = %+v", restored)
	}
	if got, err := os.ReadFile(filepath.Join(repo, "candidate.txt")); err != nil || string(got) != "candidate=best\n" {
		t.Fatalf("restored candidate = %q, %v", got, err)
	}
	if _, err := os.Stat(filepath.Join(repo, "scratch.txt")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("restored scratch status = %v", err)
	}
	if after := readGitIndex(t, repo); !bytes.Equal(after, beforeIndex) {
		t.Fatal("candidate restore changed the real Git index")
	}
	if after := strings.TrimSpace(runGit(t, repo, "rev-parse", "HEAD")); after != beforeHead {
		t.Fatalf("HEAD after restore = %s, want %s", after, beforeHead)
	}
	backupPath := filepath.Join(sessionDir, filepath.FromSlash(restored.RecoveryPatch))
	runGit(t, repo, "apply", "--binary", backupPath)
	if got, err := os.ReadFile(filepath.Join(repo, "candidate.txt")); err != nil || string(got) != "candidate=regressed\n" {
		t.Fatalf("recovered candidate = %q, %v", got, err)
	}
	if got, err := os.ReadFile(filepath.Join(repo, "scratch.txt")); err != nil || string(got) != "unaccepted scratch\n" {
		t.Fatalf("recovered scratch = %q, %v", got, err)
	}
}

func TestManagerSwitchSessionStartsFreshAndFailsClosed(t *testing.T) {
	repo, _ := newLinkedWorktree(t)
	firstSession := filepath.Join(t.TempDir(), "first")
	manager, err := Open(repo, firstSession)
	if err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, filepath.Join(repo, "candidate.txt"), "candidate=first\n")
	score := 1.0
	if advance, err := manager.Observe(Input{
		Handler: "quality", Accepted: true, Score: &score, ScoreDirection: ScoreDirectionMaximize,
		Candidate: "candidate:first", EvidenceRef: "evidence/one.txt",
	}); err != nil || advance == nil {
		t.Fatalf("first advance/error = %+v / %v", advance, err)
	}

	secondSession := filepath.Join(t.TempDir(), "second")
	if err := manager.SwitchSession(secondSession); err != nil {
		t.Fatal(err)
	}
	if state, err := manager.Status(); err != nil || len(state.Entries) != 0 {
		t.Fatalf("fresh session state/error = %+v / %v", state, err)
	}
	if state, err := Load(firstSession); err != nil || len(state.Entries) != 1 {
		t.Fatalf("first session state/error = %+v / %v", state, err)
	}

	if err := manager.SwitchSession(filepath.Join(repo, "unsafe-session")); err == nil {
		t.Fatal("unsafe session switch succeeded")
	}
	if _, err := manager.Status(); err == nil {
		t.Fatal("failed switch did not disable manager")
	}
	if advance, err := manager.Observe(Input{}); err == nil || advance != nil {
		t.Fatalf("disabled observe = %+v, %v", advance, err)
	}
	thirdSession := filepath.Join(t.TempDir(), "third")
	if err := manager.SwitchSession(thirdSession); err != nil {
		t.Fatalf("recover session switch: %v", err)
	}
	if _, err := manager.Status(); err != nil {
		t.Fatalf("recovered manager status: %v", err)
	}
}

func TestPatchWriterEnforcesLimit(t *testing.T) {
	var destination bytes.Buffer
	writer := &patchWriter{destination: &destination, remaining: 4}
	n, err := writer.Write([]byte("abcdef"))
	if n != 4 || !errors.Is(err, errPatchTooLarge) || !writer.exceeded || destination.String() != "abcd" {
		t.Fatalf("bounded write = n:%d err:%v exceeded:%t data:%q", n, err, writer.exceeded, destination.String())
	}
}

func TestLoadRejectsUnsafeOrOversizedManifest(t *testing.T) {
	t.Run("symlink", func(t *testing.T) {
		sessionDir := t.TempDir()
		lineageDir := filepath.Join(sessionDir, "lineage")
		if err := os.MkdirAll(lineageDir, 0o755); err != nil {
			t.Fatal(err)
		}
		outside := filepath.Join(t.TempDir(), "state.json")
		writeTestFile(t, outside, "{}\n")
		if err := os.Symlink(outside, filepath.Join(lineageDir, "state.json")); err != nil {
			t.Skipf("symlink unavailable: %v", err)
		}
		if _, err := Load(sessionDir); err == nil || !strings.Contains(err.Error(), "regular file") {
			t.Fatalf("symlink manifest error = %v", err)
		}
	})
	t.Run("oversized", func(t *testing.T) {
		sessionDir := t.TempDir()
		path := filepath.Join(sessionDir, "lineage", "state.json")
		writeTestFile(t, path, strings.Repeat("x", MaxStateBytes+1))
		if _, err := Load(sessionDir); err == nil || !strings.Contains(err.Error(), "exceeds") {
			t.Fatalf("oversized manifest error = %v", err)
		}
	})
	t.Run("invalid object id", func(t *testing.T) {
		_, worktree := newLinkedWorktree(t)
		sessionDir := filepath.Join(t.TempDir(), "session")
		manager, err := Open(worktree, sessionDir)
		if err != nil {
			t.Fatal(err)
		}
		state := manager.Snapshot()
		state.BaseTree = strings.Repeat("z", 40)
		data, err := json.Marshal(state)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(sessionDir, "lineage", "state.json"), data, 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := Open(worktree, sessionDir); err == nil || !strings.Contains(err.Error(), "base snapshot") {
			t.Fatalf("invalid object id error = %v", err)
		}
	})
}

func newLinkedWorktree(t *testing.T) (string, string) {
	t.Helper()
	repo := filepath.Join(t.TempDir(), "repo")
	if err := os.MkdirAll(repo, 0o755); err != nil {
		t.Fatal(err)
	}
	runGit(t, repo, "init", "-q")
	runGit(t, repo, "config", "user.name", "Harness Test")
	runGit(t, repo, "config", "user.email", "harness@example.test")
	writeTestFile(t, filepath.Join(repo, "candidate.txt"), "candidate=base\n")
	writeTestFile(t, filepath.Join(repo, "evidence", "one.txt"), "first evidence\n")
	writeTestFile(t, filepath.Join(repo, "evidence", "two.txt"), "second evidence\n")
	runGit(t, repo, "add", "-A")
	runGit(t, repo, "commit", "-qm", "test: seed lineage fixture")
	worktree := filepath.Join(t.TempDir(), "worktree")
	runGit(t, repo, "worktree", "add", "--detach", worktree, "HEAD")
	return repo, worktree
}

func runGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	cmd.Env = append(os.Environ(), "LC_ALL=C", "GIT_CONFIG_NOSYSTEM=1")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return string(out)
}

func readGitIndex(t *testing.T, dir string) []byte {
	t.Helper()
	path := strings.TrimSpace(runGit(t, dir, "rev-parse", "--git-path", "index"))
	if !filepath.IsAbs(path) {
		path = filepath.Join(dir, path)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func writeTestFile(t *testing.T, path, contents string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(contents), 0o644); err != nil {
		t.Fatal(err)
	}
}
