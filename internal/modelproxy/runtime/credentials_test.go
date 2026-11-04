package runtime

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"harness/internal/auth"
	"harness/internal/llm"
	"harness/internal/llm/factory"
	"harness/internal/modelproxy"
	proxyclient "harness/internal/modelproxy/client"
)

type credentialTransport func(*http.Request) (*http.Response, error)

func (f credentialTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func credentialFailureHandler(t *testing.T, transport credentialTransport) (*Handler, *bytes.Buffer, *atomic.Int32) {
	t.Helper()
	var calls atomic.Int32
	logs := &bytes.Buffer{}
	h := newTestHandler(t, "p.json", `{"name":"p","api_type":"responses","responses_compaction":true,"models":[{"name":"m"}]}`, Options{Logger: slog.New(slog.NewJSONHandler(logs, nil)), New: func(factory.Options) (llm.Provider, error) {
		calls.Add(1)
		return nil, errors.New("provider must not be constructed after auth failure")
	}})
	path := filepath.Join(t.TempDir(), "private-token-file.json")
	if err := os.WriteFile(path, []byte(`{"access_token":"private-access-token","refresh_token":"private-refresh-token","expiry":"2000-01-01T00:00:00Z"}`), 0600); err != nil {
		t.Fatal(err)
	}
	source, err := auth.NewSource(auth.Config{Type: auth.TypeOAuth2, Flow: auth.FlowDeviceCode, ClientID: "client", DeviceURL: "https://private-host.invalid/device", TokenURL: "https://private-host.invalid/token", TokenFile: path}, auth.Options{Name: "private-provider-description", Client: &http.Client{Transport: transport}})
	if err != nil {
		t.Fatal(err)
	}
	h.authSources["p"] = source
	return h, logs, &calls
}

func credentialOperation(ctx context.Context, p llm.Provider, operation string) error {
	switch operation {
	case "stream":
		var result error
		for _, err := range p.Stream(ctx, llm.Request{}) {
			if err != nil {
				result = err
			}
		}
		return result
	case "count":
		_, err := p.(llm.InputTokenCounter).CountInputTokens(ctx, llm.Request{})
		return err
	case "compact":
		_, err := p.(llm.ContextCompactor).CompactContext(ctx, llm.Request{})
		return err
	default:
		panic(operation)
	}
}

func TestCredentialFailuresAreSafeLocallyRemotelyAndInLogs(t *testing.T) {
	for _, remote := range []bool{false, true} {
		for _, operation := range []string{"stream", "count", "compact"} {
			for _, kind := range []string{"ordinary", "api_error"} {
				t.Run(fmt.Sprintf("remote=%v/%s/%s", remote, operation, kind), func(t *testing.T) {
					secret := "private-token-value /private/host/credential-path private-provider-description"
					raw := error(errors.New(secret))
					if kind == "api_error" {
						raw = &llm.APIError{StatusCode: 503, Code: secret, Message: secret, ResponsePayload: llm.DiagnosticPayload(`{"token":"private-token-value"}`), Retryable: true, Diagnostic: &llm.APIErrorDiagnostic{UpstreamRequestID: secret}}
					}
					h, logs, calls := credentialFailureHandler(t, func(*http.Request) (*http.Response, error) { return nil, raw })
					var backend modelproxy.Backend = h.Runtime
					if remote {
						server := httptest.NewServer(h)
						defer server.Close()
						c, err := proxyclient.New(server.URL, server.Client())
						if err != nil {
							t.Fatal(err)
						}
						backend = c
					}
					err := credentialOperation(context.Background(), backend.Provider("p:m"), operation)
					var safe *llm.APIError
					if !errors.As(err, &safe) || safe.Message != "provider credentials are unavailable" || safe.Code != "provider_credentials_unavailable" || safe.Retryable || safe.ResponsePayload != "" {
						t.Fatalf("unsafe or unstable credential error: %#v", err)
					}
					if errors.Is(err, raw) {
						t.Fatal("credential error retains raw cause")
					}
					encoded, encodeErr := json.Marshal(safe)
					if encodeErr != nil {
						t.Fatal(encodeErr)
					}
					output := err.Error() + string(encoded) + logs.String()
					for _, private := range []string{"private-token-value", "/private/host", "private-provider-description", "private-host.invalid", "private-access-token", "private-refresh-token", "private-token-file.json"} {
						if strings.Contains(output, private) {
							t.Fatalf("credential detail %q leaked: %s", private, output)
						}
					}
					if calls.Load() != 0 {
						t.Fatal("provider constructed after credential resolution failed")
					}
				})
			}
		}
	}
}

func TestCredentialFailurePreservesCancellationAndDeadline(t *testing.T) {
	for _, operation := range []string{"stream", "count", "compact"} {
		for _, want := range []error{context.Canceled, context.DeadlineExceeded} {
			t.Run(operation+"/"+want.Error(), func(t *testing.T) {
				h, logs, calls := credentialFailureHandler(t, func(*http.Request) (*http.Response, error) {
					return nil, fmt.Errorf("private-token-value /private/path: %w", want)
				})
				err := credentialOperation(context.Background(), h.Runtime.Provider("p:m"), operation)
				if !errors.Is(err, want) {
					t.Fatalf("error=%v, want %v", err, want)
				}
				if strings.Contains(err.Error()+logs.String(), "private-") {
					t.Fatal("cancellation exposed credential details")
				}
				if calls.Load() != 0 {
					t.Fatal("provider constructed after credential cancellation")
				}
			})
		}
	}
	for _, want := range []error{context.Canceled, context.DeadlineExceeded} {
		var ctx context.Context
		var cancel context.CancelFunc
		if want == context.Canceled {
			ctx, cancel = context.WithCancel(context.Background())
			cancel()
		} else {
			ctx, cancel = context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
			defer cancel()
		}
		err := credentialResolutionError(ctx, errors.New("private-token-value"))
		if !errors.Is(err, want) {
			t.Fatalf("context precedence=%v, want %v", err, want)
		}
	}
}

func TestRemoteCancellationDuringCredentialResolution(t *testing.T) {
	for _, operation := range []string{"stream", "count", "compact"} {
		t.Run(operation, func(t *testing.T) {
			started, finished := make(chan struct{}), make(chan struct{})
			h, _, calls := credentialFailureHandler(t, func(r *http.Request) (*http.Response, error) {
				close(started)
				defer close(finished)
				<-r.Context().Done()
				return nil, fmt.Errorf("private-token-value: %w", r.Context().Err())
			})
			server := httptest.NewServer(h)
			defer server.Close()
			c, err := proxyclient.New(server.URL, server.Client())
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			done := make(chan error, 1)
			go func() { done <- credentialOperation(ctx, c.Provider("p:m"), operation) }()
			<-started
			cancel()
			err = <-done
			<-finished
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("remote cancellation=%v", err)
			}
			if calls.Load() != 0 {
				t.Fatal("provider constructed after cancellation")
			}
		})
	}
}
