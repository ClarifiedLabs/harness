package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestFindProjectConfig_NestedSubdirFindsGitRootConfig(t *testing.T) {
	root := t.TempDir()
	// git root at root
	if err := os.MkdirAll(filepath.Join(root, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, ".harness"), 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := filepath.Join(root, ".harness", "config.json")
	if err := os.WriteFile(cfg, []byte(`{"model":"root"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	nested := filepath.Join(root, "a", "b", "c")
	if err := os.MkdirAll(nested, 0o755); err != nil {
		t.Fatal(err)
	}
	got, err := findProjectConfig(nested)
	if err != nil {
		t.Fatalf("findProjectConfig: %v", err)
	}
	if got != cfg {
		t.Fatalf("got %q want %q", got, cfg)
	}
}

func TestFindProjectConfig_NearestWins(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	rootCfg := filepath.Join(root, ".harness", "config.json")
	if err := os.MkdirAll(filepath.Join(root, ".harness"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(rootCfg, []byte(`{}`), 0o644); err != nil {
		t.Fatal(err)
	}
	mid := filepath.Join(root, "a")
	midCfg := filepath.Join(mid, ".harness", "config.json")
	if err := os.MkdirAll(filepath.Join(mid, ".harness"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(midCfg, []byte(`{}`), 0o644); err != nil {
		t.Fatal(err)
	}
	leaf := filepath.Join(mid, "b", "c")
	if err := os.MkdirAll(leaf, 0o755); err != nil {
		t.Fatal(err)
	}
	got, err := findProjectConfig(leaf)
	if err != nil {
		t.Fatalf("findProjectConfig: %v", err)
	}
	if got != midCfg {
		t.Fatalf("got %q want nearest %q (root %q should be ignored)", got, midCfg, rootCfg)
	}
}

func TestFindProjectConfig_CwdOnlyOutsideRepo(t *testing.T) {
	outer := t.TempDir()
	// home-like config above outer
	homeLike := filepath.Join(outer, ".harness", "config.json")
	if err := os.MkdirAll(filepath.Join(outer, ".harness"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(homeLike, []byte(`{}`), 0o644); err != nil {
		t.Fatal(err)
	}
	// working dir inside outer but no .git anywhere
	cwd := filepath.Join(outer, "project", "sub")
	if err := os.MkdirAll(cwd, 0o755); err != nil {
		t.Fatal(err)
	}
	// No config at cwd, should return empty even though outer has one
	got, err := findProjectConfig(cwd)
	if err != nil {
		t.Fatalf("findProjectConfig: %v", err)
	}
	if got != "" {
		t.Fatalf("got %q want empty (outside repo only cwd eligible)", got)
	}
	// Now put config exactly at cwd
	cwdCfg := filepath.Join(cwd, ".harness", "config.json")
	if err := os.MkdirAll(filepath.Join(cwd, ".harness"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cwdCfg, []byte(`{}`), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err = findProjectConfig(cwd)
	if err != nil {
		t.Fatalf("findProjectConfig: %v", err)
	}
	if got != cwdCfg {
		t.Fatalf("got %q want %q", got, cwdCfg)
	}
}

func TestFindProjectConfig_GitFileTreatedAsRootMarker(t *testing.T) {
	root := t.TempDir()
	// .git as file (worktree)
	if err := os.WriteFile(filepath.Join(root, ".git"), []byte("gitdir: /somewhere"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := filepath.Join(root, ".harness", "config.json")
	if err := os.MkdirAll(filepath.Join(root, ".harness"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cfg, []byte(`{}`), 0o644); err != nil {
		t.Fatal(err)
	}
	sub := filepath.Join(root, "sub")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	got, err := findProjectConfig(sub)
	if err != nil {
		t.Fatalf("findProjectConfig: %v", err)
	}
	if got != cfg {
		t.Fatalf("got %q want %q ( .git file should count as root)", got, cfg)
	}
}

func TestFindProjectConfig_IgnoresAboveGitRoot(t *testing.T) {
	outer := t.TempDir()
	outerCfg := filepath.Join(outer, ".harness", "config.json")
	if err := os.MkdirAll(filepath.Join(outer, ".harness"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(outerCfg, []byte(`{}`), 0o644); err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(outer, "repo")
	if err := os.MkdirAll(filepath.Join(root, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	sub := filepath.Join(root, "sub")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	got, err := findProjectConfig(sub)
	if err != nil {
		t.Fatalf("findProjectConfig: %v", err)
	}
	if got != "" {
		t.Fatalf("got %q want empty (above git root %q must be ignored)", got, outerCfg)
	}
	// Add config at repo root; should be found
	repoCfg := filepath.Join(root, ".harness", "config.json")
	if err := os.MkdirAll(filepath.Join(root, ".harness"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(repoCfg, []byte(`{}`), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err = findProjectConfig(sub)
	if err != nil {
		t.Fatalf("findProjectConfig: %v", err)
	}
	if got != repoCfg {
		t.Fatalf("got %q want %q", got, repoCfg)
	}
}

func TestFindProjectConfig_DirectoryIgnored(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	// Create directory where file should be
	dirAsFile := filepath.Join(root, ".harness", "config.json")
	if err := os.MkdirAll(dirAsFile, 0o755); err != nil {
		t.Fatal(err)
	}
	sub := filepath.Join(root, "sub")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	got, err := findProjectConfig(sub)
	if err != nil {
		t.Fatalf("findProjectConfig: %v", err)
	}
	if got != "" {
		t.Fatalf("got %q want empty (directory should be ignored)", got)
	}
}

func TestFindProjectConfig_EmptyStartDir(t *testing.T) {
	got, err := findProjectConfig("")
	if err != nil {
		t.Fatalf("findProjectConfig: %v", err)
	}
	if got != "" {
		t.Fatalf("got %q want empty", got)
	}
}
