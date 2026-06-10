package sse

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"
)

func collect(t *testing.T, ctx context.Context, r io.Reader) ([]Event, error) {
	t.Helper()
	var events []Event
	var lastErr error
	for ev, err := range Read(ctx, r) {
		if err != nil {
			lastErr = err
			break
		}
		events = append(events, ev)
	}
	return events, lastErr
}

func TestRead(t *testing.T) {
	bigData := strings.Repeat("a", 700*1024)

	tests := []struct {
		name  string
		input string
		want  []Event
	}{
		{
			name:  "single data frame",
			input: "data: hello\n\n",
			want:  []Event{{Data: "hello"}},
		},
		{
			name:  "multi-line data joined with newline",
			input: "data: line1\ndata: line2\n\n",
			want:  []Event{{Data: "line1\nline2"}},
		},
		{
			name:  "event and data pair",
			input: "event: message_stop\ndata: {}\n\n",
			want:  []Event{{Type: "message_stop", Data: "{}"}},
		},
		{
			name:  "leading space stripped only once",
			input: "data:  x\n\n",
			want:  []Event{{Data: " x"}},
		},
		{
			name:  "no leading space preserved",
			input: "data:x\n\n",
			want:  []Event{{Data: "x"}},
		},
		{
			name:  "comment lines ignored",
			input: ": ping\ndata: real\n\n",
			want:  []Event{{Data: "real"}},
		},
		{
			name:  "two frames separated by blank line",
			input: "data: first\n\ndata: second\n\n",
			want:  []Event{{Data: "first"}, {Data: "second"}},
		},
		{
			name:  "CRLF line endings",
			input: "event: e\r\ndata: d\r\n\r\n",
			want:  []Event{{Type: "e", Data: "d"}},
		},
		{
			name:  "700KB single data line",
			input: "data: " + bigData + "\n\n",
			want:  []Event{{Data: bigData}},
		},
		{
			name:  "partial frame at EOF without trailing blank line",
			input: "data: partial\n",
			want:  []Event{{Data: "partial"}},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			events, err := collect(t, context.Background(), strings.NewReader(tt.input))
			if err != nil {
				t.Fatalf("Read returned error: %v", err)
			}
			if len(events) != len(tt.want) {
				t.Fatalf("got %d events %+v, want %d %+v", len(events), events, len(tt.want), tt.want)
			}
			for i := range tt.want {
				if events[i] != tt.want[i] {
					t.Errorf("event %d = %+v, want %+v", i, events[i], tt.want[i])
				}
			}
		})
	}
}

func TestReadCleanEOFAfterPartialFrame(t *testing.T) {
	events, err := collect(t, context.Background(), strings.NewReader("data: partial\n"))
	if err != nil {
		t.Fatalf("partial frame then EOF should be a clean EOF, got error: %v", err)
	}
	if len(events) != 1 || events[0].Data != "partial" {
		t.Fatalf("got %+v, want one partial frame", events)
	}
}

func TestReadContextCancelledMidRead(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())

	pr, pw := io.Pipe()
	go func() {
		_, _ = io.WriteString(pw, "data: first\n\n")
		cancel()
		// Block so the reader is forced to observe cancellation rather than EOF.
		<-ctx.Done()
		_ = pw.Close()
	}()

	var events []Event
	var lastErr error
	for ev, err := range Read(ctx, pr) {
		if err != nil {
			lastErr = err
			break
		}
		events = append(events, ev)
	}

	if !errors.Is(lastErr, context.Canceled) {
		t.Fatalf("terminal error = %v, want context.Canceled", lastErr)
	}
}
