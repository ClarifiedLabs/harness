package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"harness/internal/sysprompt"
)

func TestBuild_IncludesRgHintWhenAvailable(t *testing.T) {
	dir := t.TempDir()
	prog := filepath.Join(dir, "rg")
	if err := os.WriteFile(prog, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatalf("write fake rg: %v", err)
	}
	t.Setenv("PATH", dir+":"+os.Getenv("PATH"))
	if !ripgrepAvailable() {
		t.Fatalf("ripgrepAvailable = false after adding fake rg to PATH=%q", os.Getenv("PATH"))
	}
	if got := searchBackend(); got != "rg" {
		t.Fatalf("searchBackend = %q, want rg", got)
	}
	out := sysprompt.Build(sysprompt.Options{
		RuntimeHints: []string{rgSystemHint},
		AgentPrompt:  "agent section",
		NoEnv:        true,
	})
	if !strings.Contains(out, rgSystemHint) {
		t.Fatalf("system prompt should contain rg hint when available:\n%s", out)
	}
	hintIdx := strings.Index(out, rgSystemHint)
	agentIdx := strings.Index(out, "agent section")
	if hintIdx < 0 || agentIdx < 0 || hintIdx >= agentIdx {
		t.Fatalf("rg hint should appear before AgentPrompt: hintIdx=%d agentIdx=%d\n%s", hintIdx, agentIdx, out)
	}
}

func TestBuild_NoRgHintWhenUnavailable(t *testing.T) {
	empty := t.TempDir()
	t.Setenv("PATH", empty)
	if ripgrepAvailable() {
		t.Fatalf("ripgrepAvailable = true with PATH=%q, want false", os.Getenv("PATH"))
	}
	if got := searchBackend(); got != "go" {
		t.Fatalf("searchBackend = %q, want go", got)
	}
	out := sysprompt.Build(sysprompt.Options{
		RuntimeHints: nil,
		AgentPrompt:  "agent section",
		NoEnv:        true,
	})
	if strings.Contains(out, rgSystemHint) {
		t.Fatalf("system prompt should not contain rg hint when unavailable:\n%s", out)
	}
	// Also verify that an explicit empty hint list does not leave extra blank lines.
	if strings.Contains(out, "\n\n\n\n") {
		t.Fatalf("empty hints should not leave extra blank lines:\n%q", out)
	}
}
