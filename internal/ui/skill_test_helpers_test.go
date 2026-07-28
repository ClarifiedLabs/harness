package ui

import (
	"os"
	"path/filepath"
	"testing"

	"harness/internal/skills"
)

func testSkill(t *testing.T, name, description, body string) skills.Skill {
	t.Helper()
	dir := filepath.Join(t.TempDir(), name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	location := filepath.Join(dir, "SKILL.md")
	if err := os.WriteFile(location, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return skills.Skill{Name: name, Description: description, Location: location}
}
