package llm

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
)

const toolInputPreviewBytes = 160

// NormalizeToolInputObject returns a complete JSON object suitable for a tool
// input block. Empty input normalizes to {}, matching provider behavior for
// tools with no arguments.
func NormalizeToolInputObject(raw []byte) (json.RawMessage, error) {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 {
		return json.RawMessage(`{}`), nil
	}
	if err := ValidateToolInputObject(raw); err != nil {
		return nil, err
	}
	return append(json.RawMessage(nil), raw...), nil
}

// InvalidToolInputObject returns a valid JSON object that can stand in for a
// malformed streamed tool-call argument payload. The original invalid bytes stay
// out of the transcript; the paired tool result carries the actionable error.
func InvalidToolInputObject(err error) json.RawMessage {
	msg := "invalid tool input"
	if err != nil {
		msg = err.Error()
	}
	b, marshalErr := json.Marshal(map[string]any{
		"_harness_invalid_tool_input": true,
		"error":                       msg,
	})
	if marshalErr != nil {
		return json.RawMessage(`{"_harness_invalid_tool_input":true}`)
	}
	return json.RawMessage(b)
}

// ValidateToolInputObject verifies that raw is a complete JSON object. Unlike
// NormalizeToolInputObject, empty input is invalid because persisted transcript
// blocks must already carry their normalized object form.
func ValidateToolInputObject(raw []byte) error {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 {
		return fmt.Errorf("tool input must be a JSON object")
	}

	dec := json.NewDecoder(bytes.NewReader(raw))
	var decoded any
	if err := dec.Decode(&decoded); err != nil {
		return invalidToolInputJSONError(raw, err, dec.InputOffset())
	}
	offset := dec.InputOffset()
	if extra, ok := firstNonSpace(raw[offset:]); ok {
		extraOffset := offset + int64(extra)
		return fmt.Errorf("invalid JSON at byte offset %d: trailing data after JSON value; input preview %s", extraOffset, toolInputPreview(raw, extraOffset))
	}

	if _, ok := decoded.(map[string]any); !ok {
		return fmt.Errorf("tool input must be a JSON object")
	}
	return nil
}

func invalidToolInputJSONError(raw []byte, err error, fallbackOffset int64) error {
	offset := fallbackOffset
	var syntaxErr *json.SyntaxError
	if errors.As(err, &syntaxErr) && syntaxErr.Offset > 0 {
		offset = syntaxErr.Offset
	}
	if offset <= 0 {
		offset = int64(len(raw))
	}
	return fmt.Errorf("invalid JSON at byte offset %d: %v; input preview %s", offset, err, toolInputPreview(raw, offset))
}

func toolInputPreview(raw []byte, offset int64) string {
	if len(raw) <= toolInputPreviewBytes {
		return strconv.QuoteToASCII(string(raw))
	}

	pos := int(offset)
	if pos > 0 {
		pos--
	}
	if pos < 0 {
		pos = 0
	}
	if pos > len(raw) {
		pos = len(raw)
	}

	start := pos - toolInputPreviewBytes/2
	if start < 0 {
		start = 0
	}
	end := start + toolInputPreviewBytes
	if end > len(raw) {
		end = len(raw)
		start = end - toolInputPreviewBytes
		if start < 0 {
			start = 0
		}
	}

	preview := string(raw[start:end])
	if start > 0 {
		preview = "..." + preview
	}
	if end < len(raw) {
		preview += "..."
	}
	return strconv.QuoteToASCII(preview)
}

func firstNonSpace(raw []byte) (int, bool) {
	for i, b := range raw {
		if b != ' ' && b != '\n' && b != '\r' && b != '\t' {
			return i, true
		}
	}
	return 0, false
}
