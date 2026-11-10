package mcpproxy

import (
	"strings"
	"testing"
)

func TestServerConfigIsEnabled(t *testing.T) {
	for _, tc := range []struct {
		name  string
		value *bool
		want  bool
	}{
		{name: "omitted", want: true},
		{name: "true", value: boolPointer(true), want: true},
		{name: "false", value: boolPointer(false)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := (ServerConfig{Enabled: tc.value}).IsEnabled(); got != tc.want {
				t.Fatalf("IsEnabled() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestLoadConfigDisabledServersNeverResolveOrExpand(t *testing.T) {
	// An invalid expanded header would produce a validation warning if the
	// disabled entry were expanded; an unset command would produce two warnings.
	t.Setenv("MCP_DISABLED_HEADER", "invalid\nheader")
	cfg, err := LoadConfig(writeConfig(t, `{"mcpServers":{
		"disabled_stdio":{"enabled":false,"command":"${MCP_DISABLED_UNSET_COMMAND}","env":{"TOKEN":"${MCP_DISABLED_UNSET_TOKEN}"}},
		"disabled_http":{"enabled":false,"url":"https://example.com/mcp","headers":{"Authorization":"${MCP_DISABLED_HEADER}"}},
		"default":{"command":"default-bin"},
		"explicit":{"enabled":true,"command":"explicit-bin"}
	}}`))
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Warnings) != 0 {
		t.Fatalf("disabled entries expanded or failed validation: %v", cfg.Warnings)
	}
	if len(cfg.Servers) != 2 || cfg.Servers[0].Name != "default" || cfg.Servers[1].Name != "explicit" {
		t.Fatalf("startup servers = %+v, want only default and explicit", cfg.Servers)
	}
}

func TestLoadConfigDisabledServerStillValidatesSchema(t *testing.T) {
	cfg, err := LoadConfig(writeConfig(t, `{"mcpServers":{"disabled":{"enabled":false,"type":"unsupported","command":"bin"}}}`))
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Servers) != 0 || len(cfg.Warnings) != 1 || !strings.Contains(cfg.Warnings[0], "unknown type") {
		t.Fatalf("disabled schema validation: servers=%+v warnings=%v", cfg.Servers, cfg.Warnings)
	}
	if _, err := LoadConfig(writeConfig(t, `{"mcpServers":{"disabled":{"enabled":"false","command":"bin"}}}`)); err == nil {
		t.Fatal("accepted non-boolean enabled")
	}
}
