package responses

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"harness/internal/codexclient"
	"harness/internal/llm"
	"harness/internal/llm/llmtest"
	"harness/internal/ws"
)

func TestStreamSendsCodexIdentityHeaders(t *testing.T) {
	var headers http.Header
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		headers = r.Header.Clone()
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("event: response.completed\n" +
			`data: {"type":"response.completed","response":{"id":"resp_1","status":"completed","output":[],"usage":{"input_tokens":1,"output_tokens":1}}}` + "\n\n"))
	}))
	defer srv.Close()

	p := New(Config{
		APIKey:             "k",
		BaseURL:            srv.URL,
		ProviderName:       "openai-codex",
		CodexClientVersion: "0.154.0",
		Sleep:              func(time.Duration) {},
	})
	if _, err := llmtest.Drain(p.Stream(context.Background(), llmtest.SimpleRequest("gpt-5.5"))); err != nil {
		t.Fatalf("Stream: %v", err)
	}
	if got := headers.Get("originator"); got != codexclient.Originator {
		t.Fatalf("originator = %q, want %q", got, codexclient.Originator)
	}
	if got := headers.Values("User-Agent"); len(got) != 1 || !strings.HasPrefix(got[0], "codex_cli_rs/0.154.0 ") {
		t.Fatalf("User-Agent = %q, want a single codex_cli_rs/0.154.0 value", got)
	}
}

func TestStreamSendsCodexIdentityHeadersViaProfile(t *testing.T) {
	var headers http.Header
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		headers = r.Header.Clone()
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("event: response.completed\n" +
			`data: {"type":"response.completed","response":{"id":"resp_1","status":"completed","output":[],"usage":{"input_tokens":1,"output_tokens":1}}}` + "\n\n"))
	}))
	defer srv.Close()

	// A second ChatGPT subscription account under a non-canonical provider name
	// and a local base URL still selects Codex behavior via its profile.
	p := New(Config{
		APIKey:             "k",
		BaseURL:            srv.URL,
		ProviderName:       "openai-codex-2",
		Profile:            llm.ProfileCodex,
		CodexClientVersion: "0.154.0",
		Sleep:              func(time.Duration) {},
	})
	if _, err := llmtest.Drain(p.Stream(context.Background(), llmtest.SimpleRequest("gpt-5.5"))); err != nil {
		t.Fatalf("Stream: %v", err)
	}
	if got := headers.Get("originator"); got != codexclient.Originator {
		t.Fatalf("originator = %q, want %q", got, codexclient.Originator)
	}
}

func TestStreamOmitsCodexIdentityHeadersOffCodexBackend(t *testing.T) {
	var headers http.Header
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		headers = r.Header.Clone()
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("event: response.completed\n" +
			`data: {"type":"response.completed","response":{"id":"resp_1","status":"completed","output":[],"usage":{"input_tokens":1,"output_tokens":1}}}` + "\n\n"))
	}))
	defer srv.Close()

	p := New(Config{
		APIKey:             "k",
		BaseURL:            srv.URL,
		ProviderName:       "openrouter",
		CodexClientVersion: "0.154.0",
		Sleep:              func(time.Duration) {},
	})
	if _, err := llmtest.Drain(p.Stream(context.Background(), llmtest.SimpleRequest("gpt-5.5"))); err != nil {
		t.Fatalf("Stream: %v", err)
	}
	if got := headers.Get("originator"); got != "" {
		t.Fatalf("originator = %q, want unset off the Codex backend", got)
	}
	for _, value := range headers.Values("User-Agent") {
		if strings.Contains(value, codexclient.Originator) {
			t.Fatalf("User-Agent = %q, want no Codex identity off the Codex backend", value)
		}
	}
}

func TestWebSocketHandshakeSendsCodexIdentityHeaders(t *testing.T) {
	headers := make(chan http.Header, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		headers <- r.Header.Clone()
		h, ok := w.(http.Hijacker)
		if !ok {
			t.Fatalf("response writer is not a hijacker")
		}
		conn, rw, err := h.Hijack()
		if err != nil {
			t.Fatalf("hijack: %v", err)
		}
		defer conn.Close()
		fmt.Fprintf(rw, "HTTP/1.1 101 Switching Protocols\r\n")
		fmt.Fprintf(rw, "Upgrade: websocket\r\n")
		fmt.Fprintf(rw, "Connection: Upgrade\r\n")
		fmt.Fprintf(rw, "Sec-WebSocket-Accept: %s\r\n\r\n", testAcceptKey(r.Header.Get("Sec-WebSocket-Key")))
		if err := rw.Flush(); err != nil {
			t.Fatalf("flush handshake: %v", err)
		}
		if _, err := ws.ReadClientText(rw.Reader); err != nil {
			t.Errorf("read client request: %v", err)
			return
		}
		if err := ws.WriteServerText(conn, `{"type":"response.completed","response":{"id":"resp_1","status":"completed","output":[],"usage":{"input_tokens":1,"output_tokens":1}}}`); err != nil {
			t.Errorf("write completed: %v", err)
		}
	}))
	defer srv.Close()

	p := New(Config{
		APIKey:             "k",
		BaseURL:            srv.URL,
		ProviderName:       "openai-codex",
		CodexClientVersion: "0.154.0",
		UseWebSocket:       true,
		Sleep:              func(time.Duration) {},
	})
	if _, err := llmtest.Drain(p.Stream(context.Background(), llmtest.SimpleRequest("gpt-5.5"))); err != nil {
		t.Fatalf("Stream: %v", err)
	}
	got := <-headers
	if got.Get("originator") != codexclient.Originator {
		t.Fatalf("originator = %q, want %q", got.Get("originator"), codexclient.Originator)
	}
	if values := got.Values("User-Agent"); len(values) != 1 || !strings.HasPrefix(values[0], "codex_cli_rs/0.154.0 ") {
		t.Fatalf("User-Agent = %q, want a single codex_cli_rs/0.154.0 value", values)
	}
}

func TestCountInputTokensSendsCodexIdentityHeaders(t *testing.T) {
	var headers http.Header
	var body countRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		headers = r.Header.Clone()
		_ = json.NewDecoder(r.Body).Decode(&body)
		_, _ = w.Write([]byte(`{"input_tokens":42}`))
	}))
	defer srv.Close()

	p := New(Config{
		BaseURL:            srv.URL,
		ProviderName:       "openai-codex",
		CodexClientVersion: "0.154.0",
		Sleep:              func(time.Duration) {},
	})
	if _, err := p.CountInputTokens(context.Background(), llmtest.SimpleRequest("gpt-5.5")); err != nil {
		t.Fatalf("CountInputTokens: %v", err)
	}
	if headers.Get("originator") != codexclient.Originator {
		t.Fatalf("originator = %q, want %q", headers.Get("originator"), codexclient.Originator)
	}
	if values := headers.Values("User-Agent"); len(values) != 1 || !strings.HasPrefix(values[0], "codex_cli_rs/0.154.0 ") {
		t.Fatalf("User-Agent = %q, want a single codex_cli_rs/0.154.0 value", values)
	}
}
