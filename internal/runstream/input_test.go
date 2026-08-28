package runstream

import (
	"errors"
	"io"
	"strings"
	"testing"
)

// decodeAll drains the decoder, returning the successfully decoded inputs and
// the per-line errors in arrival order. A *LineError is never fatal: decoding
// resumes at the next line.
func decodeAll(t *testing.T, input string) (inputs []Input, errs []*LineError) {
	t.Helper()
	dec := NewDecoder(strings.NewReader(input))
	for {
		in, err := dec.Decode()
		if errors.Is(err, io.EOF) {
			return inputs, errs
		}
		if err != nil {
			var le *LineError
			if !errors.As(err, &le) {
				t.Fatalf("Decode: %v (want *LineError or io.EOF)", err)
			}
			errs = append(errs, le)
			continue
		}
		inputs = append(inputs, in)
	}
}

func TestDecoderRoundTrip(t *testing.T) {
	inputs, errs := decodeAll(t,
		`{"type":"prompt","id":"p1","text":"hello","images":[{"path":"/tmp/a.png","detail":"high"}]}`+"\n"+
			`{"type":"interrupt"}`+"\n"+
			`{"type":"shutdown"}`+"\n")
	if len(errs) != 0 {
		t.Fatalf("errors = %v", errs)
	}
	if len(inputs) != 3 {
		t.Fatalf("decoded %d messages, want 3", len(inputs))
	}
	p := inputs[0]
	if p.Type != InputPrompt || p.ID != "p1" || p.Text != "hello" ||
		len(p.Images) != 1 || p.Images[0].Path != "/tmp/a.png" || p.Images[0].Detail != "high" {
		t.Fatalf("prompt message = %+v", p)
	}
	if inputs[1].Type != InputInterrupt || inputs[2].Type != InputShutdown {
		t.Fatalf("control messages = %+v %+v", inputs[1], inputs[2])
	}
}

func TestDecoderUnknownFieldsTolerated(t *testing.T) {
	inputs, errs := decodeAll(t, `{"type":"prompt","text":"hi","future_field":{"x":1}}`+"\n")
	if len(errs) != 0 || len(inputs) != 1 || inputs[0].Text != "hi" {
		t.Fatalf("inputs = %+v, errors = %v", inputs, errs)
	}
}

func TestDecoderMalformedLineDoesNotStopStream(t *testing.T) {
	inputs, errs := decodeAll(t, "not json\n"+`{"type":"interrupt"}`+"\n")
	if len(errs) != 1 || errs[0].Kind != LineMalformedJSON {
		t.Fatalf("errors = %v", errs)
	}
	if len(inputs) != 1 || inputs[0].Type != InputInterrupt {
		t.Fatalf("decoder did not resume after malformed line: %+v", inputs)
	}
}

func TestDecoderUnknownTypeAndFieldValidation(t *testing.T) {
	inputs, errs := decodeAll(t,
		`{"type":"bogus","id":"b1"}`+"\n"+
			`{"type":"prompt","id":"p1"}`+"\n"+ // no text, no images
			`{"type":"approval_response","id":"h1","approve":true}`+"\n")
	if len(inputs) != 0 {
		t.Fatalf("inputs = %+v, want none", inputs)
	}
	kinds := []LineErrorKind{LineUnknownType, LineInvalidFields, LineUnknownType}
	if len(errs) != len(kinds) {
		t.Fatalf("errors = %v", errs)
	}
	for i, want := range kinds {
		if errs[i].Kind != want {
			t.Fatalf("error %d kind = %q, want %q", i, errs[i].Kind, want)
		}
	}
	wantIDs := []string{"b1", "p1", "h1"}
	for i, want := range wantIDs {
		if errs[i].ID != want {
			t.Fatalf("error %d ID = %q, want %q", i, errs[i].ID, want)
		}
	}
}

func TestDecoderImageOnlyPromptValid(t *testing.T) {
	inputs, errs := decodeAll(t, `{"type":"prompt","images":[{"path":"/tmp/a.png"}]}`+"\n")
	if len(errs) != 0 || len(inputs) != 1 || len(inputs[0].Images) != 1 {
		t.Fatalf("inputs = %+v, errors = %v", inputs, errs)
	}
}

func TestDecoderSkipsBlankLinesAndDeliversFinalLine(t *testing.T) {
	inputs, errs := decodeAll(t, "\n\n"+`{"type":"interrupt"}`+"\n\n"+`{"type":"shutdown"}`)
	if len(errs) != 0 {
		t.Fatalf("errors = %v", errs)
	}
	if len(inputs) != 2 || inputs[0].Type != InputInterrupt || inputs[1].Type != InputShutdown {
		t.Fatalf("inputs = %+v", inputs)
	}
}

func TestDecoderOversizedLine(t *testing.T) {
	big := `{"type":"prompt","text":"` + strings.Repeat("x", MaxInputLine) + `"}`
	inputs, errs := decodeAll(t, big+"\n"+`{"type":"interrupt"}`+"\n")
	if len(errs) != 1 || errs[0].Kind != LineTooLong {
		t.Fatalf("errors = %v, want one line_too_long", errs)
	}
	if len(inputs) != 1 || inputs[0].Type != InputInterrupt {
		t.Fatalf("decoder did not resume after oversized line: %+v", inputs)
	}
}

func TestDecoderEmptyStreamIsEOF(t *testing.T) {
	dec := NewDecoder(strings.NewReader(""))
	if _, err := dec.Decode(); !errors.Is(err, io.EOF) {
		t.Fatalf("err = %v, want io.EOF", err)
	}
}
