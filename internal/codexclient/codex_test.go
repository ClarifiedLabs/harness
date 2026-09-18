package codexclient

import (
	"net/http"
	"regexp"
	"strings"
	"testing"
)

func TestFormatUserAgent(t *testing.T) {
	tests := []struct {
		name   string
		info   hostInfo
		suffix string
		want   string
	}{
		{
			name: "full identity",
			info: hostInfo{
				codexVersion: "0.154.0",
				osName:       "Mac OS",
				osVersion:    "15.0.1",
				arch:         "arm64",
				terminal:     "Apple_Terminal/450.1",
			},
			suffix: "harness/0.5.45",
			want:   "codex_cli_rs/0.154.0 (Mac OS 15.0.1; arm64) Apple_Terminal/450.1 (harness/0.5.45)",
		},
		{
			name:   "missing os version",
			info:   hostInfo{codexVersion: "0.154.0", osName: "Windows", arch: "x86_64", terminal: "WindowsTerminal"},
			suffix: "harness/dev",
			want:   "codex_cli_rs/0.154.0 (Windows; x86_64) WindowsTerminal (harness/dev)",
		},
		{
			name:   "missing codex version",
			info:   hostInfo{osName: "Linux", osVersion: "6.8.0", arch: "aarch64", terminal: "xterm-256color"},
			suffix: "harness/dev",
			want:   "codex_cli_rs (Linux 6.8.0; aarch64) xterm-256color (harness/dev)",
		},
		{
			name:   "invalid terminal characters are sanitized",
			info:   hostInfo{codexVersion: "0.154.0", osName: "Mac OS", osVersion: "15.0.1", arch: "arm64", terminal: "bad\x01term\x7f"},
			suffix: "harness/dev",
			want:   "codex_cli_rs/0.154.0 (Mac OS 15.0.1; arm64) bad_term_ (harness/dev)",
		},
		{
			name:   "empty suffix",
			info:   hostInfo{codexVersion: "0.154.0", osName: "Mac OS", osVersion: "15.0.1", arch: "arm64", terminal: "unknown"},
			suffix: "  ",
			want:   "codex_cli_rs/0.154.0 (Mac OS 15.0.1; arm64) unknown",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := formatUserAgent(tt.info, tt.suffix); got != tt.want {
				t.Fatalf("formatUserAgent = %q, want %q", got, tt.want)
			}
		})
	}
}

// TestUserAgentHostShape pins the format the Codex CLI test suite asserts for
// the running host (originator/version (OS; arch) terminal (suffix)).
func TestUserAgentHostShape(t *testing.T) {
	got := UserAgent("0.154.0")
	want := regexp.MustCompile(`^codex_cli_rs/0\.154\.0 \([^;()]+;[^;()]+\) \S+ \(harness/\S+\)$`)
	if !want.MatchString(got) {
		t.Fatalf("UserAgent = %q, want match for %q", got, want)
	}
	if regexp.MustCompile(`[^\x20-\x7e]`).MatchString(got) {
		t.Fatalf("UserAgent = %q, want printable ASCII only", got)
	}
}

func TestApplySetsIdentityHeaders(t *testing.T) {
	header := http.Header{}
	Apply(header, "0.154.0")
	if got := header.Get("originator"); got != Originator {
		t.Fatalf("originator = %q, want %q", got, Originator)
	}
	if got := header.Values("User-Agent"); len(got) != 1 || !strings.HasPrefix(got[0], Originator+"/0.154.0 ") {
		t.Fatalf("User-Agent = %q, want one codex_cli_rs/0.154.0 value", got)
	}
}

func TestTerminalToken(t *testing.T) {
	tests := []struct {
		name string
		env  map[string]string
		want string
	}{
		{name: "term program with version", env: map[string]string{"TERM_PROGRAM": "iTerm.app", "TERM_PROGRAM_VERSION": "3.5.0"}, want: "iTerm.app/3.5.0"},
		{name: "term program without version", env: map[string]string{"TERM_PROGRAM": "WarpTerminal"}, want: "WarpTerminal"},
		{name: "tmux falls through to term", env: map[string]string{"TERM_PROGRAM": "tmux", "TERM": "screen-256color"}, want: "screen-256color"},
		{name: "term fallback", env: map[string]string{"TERM": "xterm-256color"}, want: "xterm-256color"},
		{name: "nothing set", env: map[string]string{}, want: "unknown"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			getenv := func(key string) string { return tt.env[key] }
			if got := terminalToken(getenv); got != tt.want {
				t.Fatalf("terminalToken = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestArch(t *testing.T) {
	tests := []struct {
		goos, goarch, want string
	}{
		{"darwin", "arm64", "arm64"},
		{"linux", "arm64", "aarch64"},
		{"darwin", "amd64", "x86_64"},
		{"windows", "386", "i686"},
		{"plan9", "mips", "mips"},
	}
	for _, tt := range tests {
		if got := arch(tt.goos, tt.goarch); got != tt.want {
			t.Errorf("arch(%q, %q) = %q, want %q", tt.goos, tt.goarch, got, tt.want)
		}
	}
}
