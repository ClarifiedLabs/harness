package main

import (
	"strings"
	"testing"

	"harness/internal/acp"
	"harness/internal/config"
	"harness/internal/mcpproxy"
)

func TestResolveMCPServicesSkipsDisabled(t *testing.T) {
	disabled, enabled := false, true
	cfg := config.MCPConfig{Servers: map[string]mcpproxy.ServerConfig{
		"disabled_stdio": {Enabled: &disabled, Command: "${UNUSED_COMMAND}"},
		"disabled_http":  {Enabled: &disabled, URL: "${UNUSED_URL}"},
		"default":        {Command: "default-bin"},
		"explicit":       {Enabled: &enabled, URL: "https://example.com/mcp"},
	}}
	servers, err := resolveMCPServices(cfg, "/workspace")
	if err != nil {
		t.Fatal(err)
	}
	if len(servers) != 2 || servers[0].Name != "default" || servers[1].Name != "explicit" {
		t.Fatalf("startup servers = %+v, want only default and explicit", servers)
	}
	for _, server := range servers {
		if server.WorkDir != "/workspace" {
			t.Fatalf("working directory = %q", server.WorkDir)
		}
	}
}

func TestResolveACPMCPServicesDisabledConfigSuppressesAttachment(t *testing.T) {
	disabled := false
	cfg := config.MCPConfig{Servers: map[string]mcpproxy.ServerConfig{
		"local":  {Enabled: &disabled, Command: "${UNUSED_COMMAND}"},
		"remote": {Enabled: &disabled, URL: "${UNUSED_URL}"},
	}}
	attachments := []acp.MCPServer{
		{Type: acp.MCPTransportStdio, Name: "local", Command: "/does/not/exist"},
		{Type: acp.MCPTransportHTTP, Name: "remote", URL: "https://disabled.example.com/mcp"},
		{Type: acp.MCPTransportHTTP, Name: "active", URL: "https://active.example.com/mcp"},
	}
	servers, err := resolveACPMCPServices(cfg, t.TempDir(), attachments)
	if err != nil {
		t.Fatal(err)
	}
	if len(servers) != 1 || servers[0].Name != "active" {
		t.Fatalf("startup servers = %+v, want only active attachment", servers)
	}
	// Suppression must not weaken attachment duplicate detection.
	if _, err := resolveACPMCPServices(cfg, t.TempDir(), append(attachments, attachments[0])); err == nil || !strings.Contains(err.Error(), "duplicate") {
		t.Fatalf("duplicate disabled attachments error = %v", err)
	}
}
