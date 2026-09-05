package acptool

import (
	"context"
	"encoding/json"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"harness/internal/agentsession"
	"harness/internal/config"
	"harness/internal/tools"
)

type recordingStarter struct {
	request tools.BackgroundJobRequest
}

func (s *recordingStarter) StartBackgroundJob(request tools.BackgroundJobRequest) (tools.BackgroundJobInfo, error) {
	s.request = request
	return tools.BackgroundJobInfo{
		ID:          "job-1",
		Status:      "running",
		SessionID:   request.SessionID,
		Operation:   request.Operation,
		ResourceKey: request.ResourceKey,
		Access:      request.Access,
	}, nil
}

func TestSchemaExposesOnlyModelFacingStartInputs(t *testing.T) {
	tool := NewTool(nil, config.ACPConfig{Targets: map[string]config.ACPTargetConfig{
		"zeta":  {Command: "secret-zeta-command", Env: map[string]string{"TOKEN": "secret-zeta-token"}, Description: "Zeta agent.", WorkspaceAccess: "exclusive"},
		"alpha": {Command: "secret-alpha-command", WorkspaceAccess: "read_only"},
	}}, nil)
	var schema struct {
		Properties map[string]struct {
			Enum []string `json:"enum"`
		} `json:"properties"`
	}
	if err := json.Unmarshal(tool.Schema(), &schema); err != nil {
		t.Fatal(err)
	}
	propertyNames := make([]string, 0, len(schema.Properties))
	for name := range schema.Properties {
		propertyNames = append(propertyNames, name)
	}
	slices.Sort(propertyNames)
	if !slices.Equal(propertyNames, []string{"action", "cwd", "prompt", "target"}) {
		t.Fatalf("schema properties = %v", propertyNames)
	}
	if !slices.Equal(schema.Properties["action"].Enum, []string{"targets", "start"}) {
		t.Fatalf("action enum = %v", schema.Properties["action"].Enum)
	}
	if !slices.Equal(schema.Properties["target"].Enum, []string{"alpha", "zeta"}) {
		t.Fatalf("target enum = %v", schema.Properties["target"].Enum)
	}
	encoded := string(tool.Schema())
	for _, forbidden := range []string{"secret-zeta-command", "secret-alpha-command", "secret-zeta-token", "workspace_access", `"command"`, `"env"`} {
		if strings.Contains(encoded, forbidden) {
			t.Fatalf("schema leaked %q: %s", forbidden, encoded)
		}
	}
}

func TestTargetsIsInertWithoutConfiguration(t *testing.T) {
	tool := NewTool(nil, config.ACPConfig{}, nil)
	result, err := tool.RunResult(context.Background(), json.RawMessage(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	if result.Text != "No ACP targets configured." || !result.Useless || result.BackgroundJobID != "" {
		t.Fatalf("targets result = %#v", result)
	}
	if _, err := tool.Run(context.Background(), json.RawMessage(`{"action":"start","target":"agent","prompt":"work"}`)); err == nil || !strings.Contains(err.Error(), "no ACP targets") {
		t.Fatalf("start error = %v", err)
	}
}

func TestStartUsesConfiguredTargetAndDetachedSession(t *testing.T) {
	starter := &recordingStarter{}
	manager := agentsession.NewManager(agentsession.Options{Background: starter})
	configured := config.ACPTargetConfig{
		Command:         "agent-command",
		Args:            []string{"--acp"},
		Env:             map[string]string{"TOKEN": "resolved-secret"},
		Description:     "Configured agent.",
		WorkspaceAccess: tools.BackgroundAccessReadOnly,
	}
	var builtTarget config.ACPTargetConfig
	var builtCWD string
	factory := agentsession.Factory(func(context.Context, agentsession.SessionInfo) (agentsession.Runtime, error) {
		return nil, nil
	})
	tool := NewTool(manager, config.ACPConfig{Targets: map[string]config.ACPTargetConfig{"agent": configured}}, func(target config.ACPTargetConfig, cwd string) agentsession.Factory {
		builtTarget, builtCWD = target, cwd
		return factory
	})
	result, err := tool.RunResult(context.Background(), json.RawMessage(`{"action":"start","target":"agent","prompt":" inspect this ","cwd":"."}`))
	if err != nil {
		t.Fatal(err)
	}
	wantCWD, err := tools.DefaultBackgroundResource(".")
	if err != nil {
		t.Fatal(err)
	}
	if !filepath.IsAbs(builtCWD) || builtCWD != wantCWD {
		t.Fatalf("builder cwd = %q, want absolute %q", builtCWD, wantCWD)
	}
	if builtTarget.Command != configured.Command || !slices.Equal(builtTarget.Args, configured.Args) || builtTarget.Env["TOKEN"] != configured.Env["TOKEN"] {
		t.Fatalf("builder target = %#v, want %#v", builtTarget, configured)
	}
	if starter.request.Kind != "acp" || starter.request.Agent != "agent" || starter.request.Description != "inspect this" {
		t.Fatalf("background identity = kind %q agent %q description %q", starter.request.Kind, starter.request.Agent, starter.request.Description)
	}
	if starter.request.WaitForPrompt {
		t.Fatal("ACP start unexpectedly waits for prompt")
	}
	if starter.request.ResourceKey != wantCWD || starter.request.Access != tools.BackgroundAccessReadOnly {
		t.Fatalf("lease = %q/%q, want %q/read_only", starter.request.ResourceKey, starter.request.Access, wantCWD)
	}
	if result.BackgroundJobID != "job-1" || !strings.Contains(result.Text, "job-1") || !strings.Contains(result.Text, "ACP session as_") {
		t.Fatalf("start result = %#v", result)
	}
	snapshot := manager.List()
	if len(snapshot) != 1 || !strings.Contains(result.Text, snapshot[0].ID) || snapshot[0].LastJobID != "job-1" {
		t.Fatalf("session snapshot = %#v; result = %#v", snapshot, result)
	}

	builtTarget.Args[0] = "changed"
	builtTarget.Env["TOKEN"] = "changed"
	listed, err := tool.Run(context.Background(), json.RawMessage(`{"action":"targets"}`))
	if err != nil || listed != "agent\tConfigured agent." {
		t.Fatalf("targets after builder mutation = %q, %v", listed, err)
	}
}

func TestReadOnlyUsesConfiguredWorkspaceAccess(t *testing.T) {
	tool := NewTool(nil, config.ACPConfig{Targets: map[string]config.ACPTargetConfig{
		"reader": {WorkspaceAccess: tools.BackgroundAccessReadOnly},
		"writer": {WorkspaceAccess: tools.BackgroundAccessExclusive},
	}}, nil)
	if !tool.ReadOnly(json.RawMessage(`{"action":"targets"}`)) || !tool.ReadOnly(json.RawMessage(`{"action":"start","target":"reader"}`)) {
		t.Fatal("targets and read-only target must be read-only")
	}
	if tool.ReadOnly(json.RawMessage(`{"action":"start","target":"writer"}`)) || tool.ReadOnly(json.RawMessage(`{"action":"start","target":"unknown"}`)) {
		t.Fatal("exclusive and unknown targets must not be read-only")
	}
}
