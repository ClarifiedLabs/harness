package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"
	"time"

	"harness/internal/llm"
)

func TestWithKindRoundTripAndNil(t *testing.T) {
	if got := WithKind(nil, llm.ToolErrorPanic); got != nil {
		t.Fatalf("WithKind(nil) = %v, want nil", got)
	}
	base := errors.New("boom")
	if got := WithKind(base, ""); got != base {
		t.Fatalf("WithKind with empty kind should return err unchanged, got %v", got)
	}
	wrapped := WithKind(base, llm.ToolErrorPanic)
	if wrapped.Error() != base.Error() {
		t.Fatalf("WithKind changed the message: %q, want %q", wrapped.Error(), base.Error())
	}
	if got := KindOf(fmt.Errorf("outer: %w", wrapped)); got != llm.ToolErrorPanic {
		t.Fatalf("KindOf through wrap chain = %q, want %q", got, llm.ToolErrorPanic)
	}
	if got := KindOf(base); got != "" {
		t.Fatalf("KindOf plain error = %q, want empty", got)
	}
}

func TestDispatchStampsErrorKinds(t *testing.T) {
	t.Run("unknown tool", func(t *testing.T) {
		r := &Registry{}
		res := r.Dispatch(context.Background(), llm.ToolCall{ID: "1", Name: "nope", Input: json.RawMessage(`{}`)})
		if !res.IsError || res.ErrorKind != llm.ToolErrorUnknownTool {
			t.Fatalf("result = %+v, want is_error with kind %q", res, llm.ToolErrorUnknownTool)
		}
	})

	t.Run("invalid args", func(t *testing.T) {
		r := &Registry{}
		r.Register(fakeTool{
			name:   "needsarg",
			desc:   "validates args",
			schema: `{"type":"object"}`,
			run: func(_ context.Context, _ json.RawMessage) (string, error) {
				return "", badArgs("path is required")
			},
		})
		res := r.Dispatch(context.Background(), llm.ToolCall{ID: "1", Name: "needsarg", Input: json.RawMessage(`{}`)})
		if !res.IsError || res.ErrorKind != llm.ToolErrorInvalidArgs {
			t.Fatalf("result = %+v, want is_error with kind %q", res, llm.ToolErrorInvalidArgs)
		}
	})

	t.Run("timeout", func(t *testing.T) {
		r := &Registry{}
		r.Register(ctxTool{})
		r.SetDispatchTimeout(20 * time.Millisecond)
		res := r.Dispatch(context.Background(), llm.ToolCall{ID: "1", Name: "ctx_tool", Input: json.RawMessage(`{}`)})
		if !res.IsError || res.ErrorKind != llm.ToolErrorTimeout {
			t.Fatalf("result = %+v, want is_error with kind %q", res, llm.ToolErrorTimeout)
		}
	})

	t.Run("panic", func(t *testing.T) {
		r := &Registry{}
		r.Register(fakeTool{
			name:   "boom",
			desc:   "panics",
			schema: `{"type":"object"}`,
			run: func(_ context.Context, _ json.RawMessage) (string, error) {
				panic("kaboom")
			},
		})
		res := r.Dispatch(context.Background(), llm.ToolCall{ID: "1", Name: "boom", Input: json.RawMessage(`{}`)})
		if !res.IsError || res.ErrorKind != llm.ToolErrorPanic {
			t.Fatalf("result = %+v, want is_error with kind %q", res, llm.ToolErrorPanic)
		}
	})

	t.Run("generic error stays unclassified", func(t *testing.T) {
		r := &Registry{}
		r.Register(fakeTool{
			name:   "err",
			desc:   "errors",
			schema: `{"type":"object"}`,
			run: func(_ context.Context, _ json.RawMessage) (string, error) {
				return "", errors.New("something broke")
			},
		})
		res := r.Dispatch(context.Background(), llm.ToolCall{ID: "1", Name: "err", Input: json.RawMessage(`{}`)})
		if !res.IsError || res.ErrorKind != "" {
			t.Fatalf("result = %+v, want is_error with empty kind", res)
		}
	})

	t.Run("tool-declared kind surfaces", func(t *testing.T) {
		r := &Registry{}
		r.Register(fakeTool{
			name:   "kinded",
			desc:   "declares a kind",
			schema: `{"type":"object"}`,
			run: func(_ context.Context, _ json.RawMessage) (string, error) {
				return "", WithKind(errors.New("missing file"), llm.ToolErrorPathNotFound)
			},
		})
		res := r.Dispatch(context.Background(), llm.ToolCall{ID: "1", Name: "kinded", Input: json.RawMessage(`{}`)})
		if !res.IsError || res.ErrorKind != llm.ToolErrorPathNotFound {
			t.Fatalf("result = %+v, want is_error with kind %q", res, llm.ToolErrorPathNotFound)
		}
		if res.Text != "missing file" {
			t.Fatalf("kind wrapper must not change the message: %q", res.Text)
		}
	})

	t.Run("outer cancellation is distinct", func(t *testing.T) {
		r := &Registry{}
		r.Register(ctxTool{})
		r.SetDispatchTimeout(time.Minute)
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		res := r.Dispatch(ctx, llm.ToolCall{ID: "1", Name: "ctx_tool", Input: json.RawMessage(`{}`)})
		if !res.IsError || res.ErrorKind != llm.ToolErrorCancelled {
			t.Fatalf("result = %+v, want is_error with kind %q", res, llm.ToolErrorCancelled)
		}
	})
}
