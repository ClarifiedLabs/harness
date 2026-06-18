package llm

import (
	"bytes"
	"encoding/json"
	"fmt"
)

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

// ValidateToolInputObject verifies that raw is a complete JSON object. Unlike
// NormalizeToolInputObject, empty input is invalid because persisted transcript
// blocks must already carry their normalized object form.
func ValidateToolInputObject(raw []byte) error {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 {
		return fmt.Errorf("tool input must be a JSON object")
	}
	if !json.Valid(raw) {
		return fmt.Errorf("invalid JSON")
	}
	if raw[0] != '{' {
		return fmt.Errorf("tool input must be a JSON object")
	}
	return nil
}
