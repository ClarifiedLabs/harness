package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"harness/internal/configmeta"
)

func TestACPTargetsResolveExpandAndRedact(t *testing.T) {
	path := writeConfig(t, `{
		"acp": {"targets": {
			"claude-code": {
				"command": "  claude  ",
				"args": ["--acp"],
				"env": {"ACP_TOKEN": "prefix-${TOKEN}", "FALLBACK": "${MISSING:-safe}"},
				"description": "General-purpose coding agent.",
				"workspace_access": "exclusive"
			}
		}}
	}`)
	result := load(t, nil, map[string]string{"TOKEN": "secret-value"}, path)
	target, ok := result.Config.ACP.Targets["claude-code"]
	if !ok {
		t.Fatalf("resolved targets = %#v", result.Config.ACP.Targets)
	}
	if target.Command != "claude" || len(target.Args) != 1 || target.Args[0] != "--acp" || target.Env["ACP_TOKEN"] != "prefix-secret-value" || target.Env["FALLBACK"] != "safe" || target.WorkspaceAccess != "exclusive" {
		t.Fatalf("resolved target = %#v", target)
	}
	if got := result.Sources["acp.targets"]; got != (configmeta.Source{Kind: configmeta.SourceFile, Name: path}) {
		t.Fatalf("source = %#v, want file %q", got, path)
	}

	projected, ok := Snapshot(result).Values["acp.targets"].(map[string]ACPTargetConfig)
	if !ok {
		t.Fatalf("projected targets type = %T", Snapshot(result).Values["acp.targets"])
	}
	if projected["claude-code"].Env["ACP_TOKEN"] != redactedValue || projected["claude-code"].Env["FALLBACK"] != redactedValue {
		t.Fatalf("projected target env = %#v", projected["claude-code"].Env)
	}
	encoded, err := json.Marshal(Project(result, false))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), "secret-value") || strings.Contains(string(encoded), "prefix-secret-value") {
		t.Fatalf("config projection leaked ACP environment: %s", encoded)
	}

	projected["claude-code"] = ACPTargetConfig{Command: "changed"}
	if result.Config.ACP.Targets["claude-code"].Command != "claude" {
		t.Fatal("ACP projection aliases resolved config")
	}
}

func TestACPTargetValidation(t *testing.T) {
	tests := []struct {
		name string
		body string
		want string
	}{
		{name: "invalid name", body: `{"bad name":{"command":"agent","workspace_access":"read_only"}}`, want: "name must match"},
		{name: "empty command", body: `{"agent":{"command":" ","workspace_access":"read_only"}}`, want: "command must not be empty"},
		{name: "empty argument", body: `{"agent":{"command":"agent","args":["ok"," "],"workspace_access":"read_only"}}`, want: "args[1] must not be empty"},
		{name: "invalid environment name", body: `{"agent":{"command":"agent","env":{"BAD-NAME":"x"},"workspace_access":"read_only"}}`, want: "invalid environment variable name"},
		{name: "missing access", body: `{"agent":{"command":"agent"}}`, want: "workspace_access"},
		{name: "invalid access", body: `{"agent":{"command":"agent","workspace_access":"shared"}}`, want: "workspace_access"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			path := writeConfig(t, `{"acp":{"targets":`+test.body+`}}`)
			_, err := Load(LoadOptions{LookupEnv: lookup(nil), DefaultConfigPath: path})
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("Load error = %v, want substring %q", err, test.want)
			}
		})
	}
}

func TestACPTargetEnvironmentRequiresReferences(t *testing.T) {
	path := writeConfig(t, `{"acp":{"targets":{"agent":{"command":"agent","env":{"TOKEN":"${MISSING}"},"workspace_access":"read_only"}}}}`)
	_, err := Load(LoadOptions{LookupEnv: lookup(nil), DefaultConfigPath: path})
	if err == nil || !strings.Contains(err.Error(), "acp.targets.agent.env.TOKEN") || !strings.Contains(err.Error(), "MISSING") {
		t.Fatalf("Load error = %v", err)
	}
}

func TestACPTargetProjectConfigOverridesGlobalLeaf(t *testing.T) {
	globalPath := writeConfig(t, `{"acp":{"targets":{"global":{"command":"global-agent","workspace_access":"read_only"}}}}`)
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	projectDir := filepath.Join(root, ".harness")
	if err := os.Mkdir(projectDir, 0o755); err != nil {
		t.Fatal(err)
	}
	projectPath := filepath.Join(projectDir, "config.json")
	if err := os.WriteFile(projectPath, []byte(`{"acp":{"targets":{"project":{"command":"project-agent","workspace_access":"exclusive"}}}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	result, err := Load(LoadOptions{LookupEnv: lookup(nil), DefaultConfigPath: globalPath, WorkingDir: root})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Config.ACP.Targets) != 1 || result.Config.ACP.Targets["project"].Command != "project-agent" {
		t.Fatalf("resolved targets = %#v, want project leaf replacement", result.Config.ACP.Targets)
	}
	if got := result.Sources["acp.targets"]; got != (configmeta.Source{Kind: configmeta.SourceFile, Name: projectPath}) {
		t.Fatalf("source = %#v, want project %q", got, projectPath)
	}
}
