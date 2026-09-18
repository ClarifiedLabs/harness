package responses

import (
	"net/http"

	"harness/internal/codexclient"
)

// applyCodexHeaders sets the ChatGPT Codex backend identity headers. The
// official Codex CLI installs these as defaults on its shared HTTP client, so
// every Codex request carries them. Non-Codex Responses backends are left
// untouched.
func (p *Provider) applyCodexHeaders(header http.Header) {
	if !p.isCodexBackend() {
		return
	}
	codexclient.Apply(header, p.codexClientVersion)
}
