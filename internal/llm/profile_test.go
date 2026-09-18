package llm

import (
	"testing"

	"harness/internal/auth"
)

func TestValidateProfile(t *testing.T) {
	tests := []struct {
		name    string
		pc      ProviderConfig
		wantErr bool
	}{
		{name: "empty", pc: ProviderConfig{Name: "openai", APIType: "responses"}},
		{name: "codex with responses", pc: ProviderConfig{Name: "openai-codex-2", APIType: "responses", Profile: "codex"}},
		{name: "kimi quota profile", pc: ProviderConfig{Name: "kimi-work", APIType: "openai", Profile: ProfileKimiCodePlan}},
		{name: "zai quota profile, anthropic dialect allowed", pc: ProviderConfig{Name: "zai-work", APIType: "anthropic", Profile: ProfileZAICodingPlan}},
		{name: "codex case normalized", pc: ProviderConfig{Name: "openai-codex-2", APIType: "responses", Profile: " Codex "}},
		{name: "codex api_type inferred from provider name", pc: ProviderConfig{Name: "responses", Profile: "codex"}},
		{name: "codex name inference case normalized", pc: ProviderConfig{Name: " Responses ", Profile: "codex"}},
		{name: "unknown profile", pc: ProviderConfig{Name: "x", APIType: "responses", Profile: "openrouter"}, wantErr: true},
		{name: "codex requires responses", pc: ProviderConfig{Name: "x", APIType: "anthropic", Profile: "codex"}, wantErr: true},
		{name: "codex rejects unresolvable dialect", pc: ProviderConfig{Name: "x", Profile: "codex"}, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidateProfile(tt.pc)
			if tt.wantErr && err == nil {
				t.Fatalf("ValidateProfile(%+v) = nil, want error", tt.pc)
			}
			if !tt.wantErr && err != nil {
				t.Fatalf("ValidateProfile(%+v) = %v, want nil", tt.pc, err)
			}
		})
	}
}

func TestCodexBackendProfilePrecedenceAndLegacyDetection(t *testing.T) {
	tests := []struct {
		name string
		pc   ProviderConfig
		want bool
	}{
		{name: "profile codex wins over name and URL",
			pc:   ProviderConfig{Name: "openai-codex-2", APIType: "responses", Profile: ProfileCodex, BaseURL: "https://example.test/responses"},
			want: true},
		{name: "legacy canonical name",
			pc:   ProviderConfig{Name: "openai-codex", APIType: "responses"},
			want: true},
		{name: "legacy codex oauth auth",
			pc:   ProviderConfig{Name: "mine", APIType: "responses", Auth: &auth.Config{Type: auth.TypeCodexOAuth}},
			want: true},
		{name: "legacy canonical base URL",
			pc:   ProviderConfig{Name: "mine", APIType: "responses", BaseURL: "https://chatgpt.com/backend-api/codex/"},
			want: true},
		{name: "non-codex",
			pc:   ProviderConfig{Name: "openai", APIType: "responses", BaseURL: "https://api.openai.com/v1"},
			want: false},
		{name: "chatgpt host alone is not codex",
			pc:   ProviderConfig{Name: "mine", APIType: "responses", BaseURL: "https://chatgpt.com/backend-api"},
			want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.pc.CodexBackend(); got != tt.want {
				t.Fatalf("CodexBackend(%+v) = %v, want %v", tt.pc, got, tt.want)
			}
		})
	}
}
