package tools

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strconv"
)

const executionStageKey = "_stage"

var executionStageSchema = json.RawMessage(`{"type":"integer","minimum":1}`)

// ExecutionMetadata is Harness-owned scheduling metadata extracted from a
// model-emitted tool input. It is never forwarded to the tool implementation.
type ExecutionMetadata struct {
	Stage    int
	HasStage bool
}

// ExtractExecutionMetadata returns an execution copy of input with Harness's
// reserved top-level scheduling field removed. Inputs without _stage are
// returned unchanged; empty input retains Registry's existing {} normalization.
func ExtractExecutionMetadata(input json.RawMessage) (json.RawMessage, ExecutionMetadata, error) {
	if len(input) == 0 {
		return json.RawMessage("{}"), ExecutionMetadata{}, nil
	}

	var object map[string]json.RawMessage
	if err := json.Unmarshal(input, &object); err != nil || object == nil {
		// Provider assemblers report malformed tool input separately. Preserve the
		// registry's historical handling when no top-level metadata can be read.
		return input, ExecutionMetadata{}, nil
	}
	stageValue, ok := object[executionStageKey]
	if !ok {
		return input, ExecutionMetadata{}, nil
	}

	delete(object, executionStageKey)
	clean, err := json.Marshal(object)
	if err != nil {
		return input, ExecutionMetadata{}, fmt.Errorf("remove %s: %w", executionStageKey, err)
	}

	trimmed := bytes.TrimSpace(stageValue)
	if len(trimmed) == 0 || !jsonIntegerToken(trimmed) {
		return clean, ExecutionMetadata{}, fmt.Errorf("%s must be a JSON integer greater than or equal to 1", executionStageKey)
	}
	stage64, err := strconv.ParseInt(string(trimmed), 10, 0)
	if err != nil || stage64 < 1 {
		return clean, ExecutionMetadata{}, fmt.Errorf("%s must be a JSON integer greater than or equal to 1", executionStageKey)
	}
	return clean, ExecutionMetadata{Stage: int(stage64), HasStage: true}, nil
}

func jsonIntegerToken(value []byte) bool {
	if len(value) == 0 {
		return false
	}
	start := 0
	if value[0] == '-' {
		if len(value) == 1 {
			return false
		}
		start = 1
	}
	if value[start] == '0' {
		return start == len(value)-1
	}
	if value[start] < '1' || value[start] > '9' {
		return false
	}
	for _, digit := range value[start+1:] {
		if digit < '0' || digit > '9' {
			return false
		}
	}
	return true
}

func modelSchemaWithExecutionMetadata(raw json.RawMessage) json.RawMessage {
	var schema map[string]json.RawMessage
	if err := json.Unmarshal(raw, &schema); err != nil || schema == nil {
		return raw
	}

	properties := make(map[string]json.RawMessage)
	if existing, ok := schema["properties"]; ok {
		if err := json.Unmarshal(existing, &properties); err != nil || properties == nil {
			return raw
		}
	}
	properties[executionStageKey] = append(json.RawMessage(nil), executionStageSchema...)
	encodedProperties, err := json.Marshal(properties)
	if err != nil {
		return raw
	}
	schema["properties"] = encodedProperties

	if requiredRaw, ok := schema["required"]; ok {
		var required []string
		if err := json.Unmarshal(requiredRaw, &required); err != nil {
			return raw
		}
		filtered := required[:0]
		for _, name := range required {
			if name != executionStageKey {
				filtered = append(filtered, name)
			}
		}
		if len(filtered) == 0 {
			delete(schema, "required")
		} else {
			encodedRequired, err := json.Marshal(filtered)
			if err != nil {
				return raw
			}
			schema["required"] = encodedRequired
		}
	}

	encoded, err := json.Marshal(schema)
	if err != nil {
		return raw
	}
	return json.RawMessage(encoded)
}
