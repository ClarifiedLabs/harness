// Package codexclient builds the client identity the official Codex CLI sends
// to OpenAI's Codex backend: the `originator` header and a Codex-shaped
// `User-Agent`. It is shared by the Responses dialect's inference requests and
// the model proxy's non-inference account/catalog calls, and depends on no
// dialect.
//
// Source format (codex-rs login/src/auth/default_client.rs):
//
//	originator: codex_cli_rs
//	User-Agent: codex_cli_rs/<version> (<os> <os version>; <arch>) <terminal>
package codexclient

import (
	"net/http"
	"os"
	"runtime"
	"strings"

	"harness/internal/buildinfo"
)

// Originator is the originator value the official Codex CLI sends with every
// request (codex-rs DEFAULT_ORIGINATOR).
const Originator = "codex_cli_rs"

// maxUserAgentLength bounds a header value built from environment-provided
// terminal identifiers.
const maxUserAgentLength = 256

// Apply sets the Codex client identity headers. Callers decide when the
// destination is OpenAI's Codex backend; this function has no policy of its own.
func Apply(header http.Header, version string) {
	header.Set("originator", Originator)
	header.Set("User-Agent", UserAgent(version))
}

// UserAgent renders the User-Agent the Codex CLI would send for this host:
//
//	codex_cli_rs/<version> (<os> <os version>; <arch>) <terminal> (harness/<build>)
//
// The Codex version is the vendored client-compatibility version, not the
// harness build version; the harness build is appended as a suffix so requests
// remain attributable.
func UserAgent(version string) string {
	info := hostInfo{
		codexVersion: strings.TrimSpace(version),
		osName:       osName(runtime.GOOS),
		osVersion:    osVersion(),
		arch:         arch(runtime.GOOS, runtime.GOARCH),
		terminal:     terminalToken(os.Getenv),
	}
	return formatUserAgent(info, agentSuffix())
}

func agentSuffix() string {
	if version := strings.TrimSpace(buildinfo.Version); version != "" {
		return "harness/" + version
	}
	return "harness"
}

type hostInfo struct {
	codexVersion string
	osName       string
	osVersion    string
	arch         string
	terminal     string
}

func formatUserAgent(info hostInfo, suffix string) string {
	var b strings.Builder
	b.WriteString(Originator)
	if info.codexVersion != "" {
		b.WriteByte('/')
		b.WriteString(info.codexVersion)
	}
	platform := strings.TrimSpace(info.osName + " " + info.osVersion)
	if platform != "" || info.arch != "" {
		b.WriteString(" (")
		b.WriteString(platform)
		if info.arch != "" {
			b.WriteString("; ")
			b.WriteString(info.arch)
		}
		b.WriteByte(')')
	}
	if terminal := strings.TrimSpace(info.terminal); terminal != "" {
		b.WriteByte(' ')
		b.WriteString(terminal)
	}
	if suffix = strings.TrimSpace(suffix); suffix != "" {
		b.WriteString(" (")
		b.WriteString(suffix)
		b.WriteByte(')')
	}
	return sanitizeUserAgent(b.String())
}

// osName matches the os_info name the Codex CLI reports for each platform.
func osName(goos string) string {
	switch goos {
	case "darwin":
		return "Mac OS"
	case "linux":
		return "Linux"
	case "windows":
		return "Windows"
	default:
		return goos
	}
}

// arch matches the os_info architecture spelling the Codex CLI reports.
func arch(goos, goarch string) string {
	switch goarch {
	case "amd64":
		return "x86_64"
	case "arm64":
		if goos == "darwin" {
			return "arm64"
		}
		return "aarch64"
	case "386":
		return "i686"
	default:
		return goarch
	}
}

// terminalToken mirrors the Codex CLI's terminal detection order: TERM_PROGRAM
// (plus TERM_PROGRAM_VERSION), then TERM, then "unknown". A tmux TERM_PROGRAM is
// skipped so the outer terminal is reported, as Codex does.
func terminalToken(getenv func(string) string) string {
	program := strings.TrimSpace(getenv("TERM_PROGRAM"))
	if program != "" && program != "tmux" {
		if version := strings.TrimSpace(getenv("TERM_PROGRAM_VERSION")); version != "" {
			return program + "/" + version
		}
		return program
	}
	if term := strings.TrimSpace(getenv("TERM")); term != "" {
		return term
	}
	return "unknown"
}

// sanitizeUserAgent replaces characters outside the printable ASCII range,
// matching the Codex CLI's fallback, and bounds the length.
func sanitizeUserAgent(value string) string {
	if len(value) > maxUserAgentLength {
		value = value[:maxUserAgentLength]
	}
	var b strings.Builder
	b.Grow(len(value))
	for _, r := range value {
		if r < 0x20 || r > 0x7e {
			b.WriteByte('_')
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}
