package delegate

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"harness/internal/llm"
	"harness/internal/llm/llmtest"
	"harness/internal/tools"
)

func TestAnnotateRunError(t *testing.T) {
	t.Run("rate limited", func(t *testing.T) {
		err := annotateRunError(context.Background(), &llm.APIError{StatusCode: 429, Message: "slow down", Retryable: true})
		if got := tools.KindOf(err); got != llm.ToolErrorRateLimited {
			t.Errorf("kind = %q, want %q", got, llm.ToolErrorRateLimited)
		}
		for _, want := range []string{"transient provider error", "429", "rate_limited", "retry the delegate call once", "report the blocker"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("error %q missing %q", err, want)
			}
		}
	})

	t.Run("overloaded", func(t *testing.T) {
		err := annotateRunError(context.Background(), &llm.APIError{StatusCode: 529, Message: "overloaded", Retryable: true})
		if got := tools.KindOf(err); got != llm.ToolErrorRateLimited {
			t.Errorf("kind = %q, want %q", got, llm.ToolErrorRateLimited)
		}
		if !strings.Contains(err.Error(), "overloaded") {
			t.Errorf("error %q missing overloaded class", err)
		}
	})

	t.Run("provider 5xx", func(t *testing.T) {
		err := annotateRunError(context.Background(), &llm.APIError{StatusCode: 503, Message: "unavailable", Retryable: true})
		if got := tools.KindOf(err); got != llm.ToolErrorProviderError {
			t.Errorf("kind = %q, want %q", got, llm.ToolErrorProviderError)
		}
		if !strings.Contains(err.Error(), "provider_5xx") {
			t.Errorf("error %q missing provider_5xx class", err)
		}
	})

	t.Run("provider-side timeout", func(t *testing.T) {
		err := annotateRunError(context.Background(), fmt.Errorf("stream read: %w", context.DeadlineExceeded))
		if got := tools.KindOf(err); got != llm.ToolErrorProviderError {
			t.Errorf("kind = %q, want %q", got, llm.ToolErrorProviderError)
		}
		if !strings.Contains(err.Error(), "timeout") {
			t.Errorf("error %q missing timeout class", err)
		}
	})

	t.Run("parent cancellation is not transient", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		in := fmt.Errorf("stream read: %w", context.DeadlineExceeded)
		if got := annotateRunError(ctx, in); !errors.Is(got, in) || got.Error() != in.Error() {
			t.Errorf("canceled parent ctx error rewritten: %v", got)
		}
	})

	t.Run("permanent failures unchanged", func(t *testing.T) {
		for _, in := range []error{
			&llm.APIError{StatusCode: 400, Message: "bad request"},
			&llm.APIError{StatusCode: 401, Message: "unauthorized"},
			errors.New("unknown agent \"nope\""),
		} {
			if got := annotateRunError(context.Background(), in); got.Error() != in.Error() || tools.KindOf(got) != "" {
				t.Errorf("permanent error %v rewritten to %v (kind %q)", in, got, tools.KindOf(got))
			}
		}
	})
}

// A foreground delegate whose child fails transiently must surface the failure
// classes, retry-once guidance, and the rate_limited kind to the parent model.
func TestDelegateTransientFailureGuidance(t *testing.T) {
	rateLimited := llmtest.Step{Err: &llm.APIError{StatusCode: 429, Message: "slow down", Retryable: true}}
	// One step per stream attempt: the agent loop retries a retryable terminal
	// stream error streamRetries times before giving up.
	fp := llmtest.New("fake", rateLimited, rateLimited, rateLimited)
	childTools := &tools.Registry{}
	sessionPath := filepath.Join(t.TempDir(), "session")
	runtime := Runtime{Provider: fp, Model: "model", Registry: llm.NewRegistry(nil), SessionPath: sessionPath}
	runner := NewRunner(func() Runtime { return runtime }, func(runtime Runtime, _ string) (Launch, error) {
		return Launch{Provider: runtime.Provider, Model: runtime.Model, Registry: runtime.Registry, Tools: childTools, Agent: "worker"}, nil
	}, Options{ActivityRegistry: NewActivityRegistry(NewActivityFeed())})
	tool := NewTool(runner)

	_, err := tool.RunMetered(context.Background(), json.RawMessage(`{"task":"inspect"}`))
	if err == nil {
		t.Fatal("RunMetered should fail")
	}
	if got := tools.KindOf(err); got != llm.ToolErrorRateLimited {
		t.Fatalf("kind = %q, want %q (err: %v)", got, llm.ToolErrorRateLimited, err)
	}
	for _, want := range []string{"delegate completion: unknown", "host/unavailable", "transient provider error", "rate_limited", "retry the delegate call once", "report the blocker"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q missing %q", err, want)
		}
	}
}

// A permanent provider failure (400) stays verbatim: no transient classes, no
// retry guidance, no kind.
func TestDelegatePermanentFailureUnchanged(t *testing.T) {
	fp := llmtest.New("fake", llmtest.Step{Err: &llm.APIError{StatusCode: 400, Message: "bad request", Retryable: false}})
	childTools := &tools.Registry{}
	sessionPath := filepath.Join(t.TempDir(), "session")
	runtime := Runtime{Provider: fp, Model: "model", Registry: llm.NewRegistry(nil), SessionPath: sessionPath}
	runner := NewRunner(func() Runtime { return runtime }, func(runtime Runtime, _ string) (Launch, error) {
		return Launch{Provider: runtime.Provider, Model: runtime.Model, Registry: runtime.Registry, Tools: childTools, Agent: "worker"}, nil
	}, Options{ActivityRegistry: NewActivityRegistry(NewActivityFeed())})
	tool := NewTool(runner)

	_, err := tool.RunMetered(context.Background(), json.RawMessage(`{"task":"inspect"}`))
	if err == nil {
		t.Fatal("RunMetered should fail")
	}
	if got := tools.KindOf(err); got != "" {
		t.Fatalf("kind = %q, want unclassified", got)
	}
	if !strings.Contains(err.Error(), "delegate completion: unknown") || !strings.Contains(err.Error(), "host/unavailable") {
		t.Fatalf("permanent failure missing completion receipt: %v", err)
	}
	if strings.Contains(err.Error(), "transient provider error") || strings.Contains(err.Error(), "retry the delegate call once") {
		t.Fatalf("permanent failure gained transient guidance: %v", err)
	}
	var apiErr *llm.APIError
	if !errors.As(err, &apiErr) || apiErr.StatusCode != 400 {
		t.Fatalf("error = %v, want the original 400 APIError", err)
	}
}
