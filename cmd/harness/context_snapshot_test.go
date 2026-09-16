package main

import (
	"crypto/sha256"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
)

// Explicit opt-in only: go test ./cmd/harness -run TestContextNotesRecoveryAcrossDialects -update-context-snapshots
var updateContextSnapshots = flag.Bool("update-context-snapshots", false, "rewrite context request snapshots")

// Snapshot actual wire bodies, not a second approximation of provider encoding.
// Only known volatile values are relabeled. Long strings and tool inventories
// retain fingerprints, so omitted details still participate in the assertion.
// Request deltas include removed fields, not just additions. Window labels come
// from the exercised lifecycle, independently of stateful/stateless transport.
func renderContextRequests(requests []map[string]any, boundaries map[int]string, replacements []string) string {
	normalize := strings.NewReplacer(replacements...)
	var out strings.Builder
	var previous map[string]any
	window := 0
	for i, request := range requests {
		current := snapshotValue(request, normalize).(map[string]any)
		if inventory, ok := current["tools"].([]any); ok {
			current["tools"] = snapshotTools(inventory)
		}
		fresh := boundaries[i] != ""
		if fresh {
			window++
			fmt.Fprintf(&out, "\n=== window %d: %s ===\n", window, boundaries[i])
		}
		fmt.Fprintf(&out, "\nrequest %d\n", i+1)
		keys := make([]string, 0, len(current)+len(previous))
		for key := range current {
			keys = append(keys, key)
		}
		for key := range previous {
			if _, ok := current[key]; !ok {
				keys = append(keys, key)
			}
		}
		slices.Sort(keys)
		for _, key := range keys {
			value, exists := current[key]
			old, hadOld := previous[key]
			force := fresh && (key == "input" || key == "messages" || key == "system" || key == "instructions" || key == "system_instruction")
			if !force && exists && hadOld && reflect.DeepEqual(value, old) {
				continue
			}
			if !exists {
				fmt.Fprintf(&out, "%s: removed\n", key)
				continue
			}
			label := key
			// Only elide a genuine unchanged prefix. Rewrites (including changed
			// request-only guidance) must remain visible rather than look appended.
			if items, ok := value.([]any); !fresh && ok && (key == "input" || key == "messages") {
				if prefix, ok := old.([]any); ok && len(prefix) > 0 && len(items) >= len(prefix) && reflect.DeepEqual(items[:len(prefix)], prefix) {
					label += fmt.Sprintf(": append after %d items", len(prefix))
					value = items[len(prefix):]
				}
			}
			data, err := json.MarshalIndent(value, "", "  ")
			if err != nil {
				panic(err)
			}
			fmt.Fprintf(&out, "%s: %s\n", label, data)
		}
		previous = current
	}
	return out.String()
}

// Keep inventories reviewable without duplicating entire schemas in each golden.
func snapshotTools(inventory []any) []any {
	result := make([]any, len(inventory))
	for i, tool := range inventory {
		value, ok := tool.(map[string]any)
		if !ok {
			result[i] = tool
			continue
		}
		name := value["name"]
		if function, ok := value["function"].(map[string]any); ok {
			name = function["name"]
		}
		result[i] = map[string]any{"name": name, "sha256": fmt.Sprintf("%x", sha256.Sum256([]byte(contextDialectJSON(tool))))}
	}
	return result
}

// Canonical tree offsets depend on the temp-directory length in its header.
// Relabel only real record boundaries and EOF, preserving unknown offsets.
func contextSnapshotOffsets(t *testing.T, dir string) []string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(dir, "tree.ndjson"))
	if err != nil {
		t.Fatal(err)
	}
	var replacements []string
	offset := 0
	for i, line := range strings.Split(string(data), "\n") {
		label := fmt.Sprintf("<record-%d-offset>", i)
		if offset == len(data) {
			label = "<tree-end-offset>"
		}
		for _, key := range []string{"offset", "next_offset"} {
			for _, end := range []string{",", "}"} {
				replacements = append(replacements, fmt.Sprintf(`"%s":%d%s`, key, offset, end), fmt.Sprintf(`"%s":%q%s`, key, label, end))
			}
		}
		offset += len(line) + 1
	}
	return replacements
}

// Offset labels apply only to this scenario's history_search result, not to
// notes pagination, user instructions, tool arguments, or recovered evidence.
// JSON escaping protects offset-looking text inside each hit's text field.
func snapshotHistoryOffsets(value any, offsets *strings.Replacer, inResult bool) any {
	switch value := value.(type) {
	case map[string]any:
		result := make(map[string]any, len(value))
		if value["call_id"] == "find-evidence" || value["tool_use_id"] == "find-evidence" || value["tool_call_id"] == "find-evidence" {
			inResult = value["type"] == "function_call_output" || value["type"] == "function_result" || value["type"] == "tool_result" || value["role"] == "tool"
		}
		for key, child := range value {
			result[key] = snapshotHistoryOffsets(child, offsets, inResult)
		}
		return result
	case []any:
		result := make([]any, len(value))
		for i, child := range value {
			result[i] = snapshotHistoryOffsets(child, offsets, inResult)
		}
		return result
	case string:
		if inResult {
			var result struct {
				Hits []json.RawMessage `json:"hits"`
			}
			if json.Unmarshal([]byte(value), &result) == nil && result.Hits != nil {
				return offsets.Replace(value)
			}
		}
	}
	return value
}

func snapshotValue(value any, replacements *strings.Replacer) any {
	switch value := value.(type) {
	case map[string]any:
		result := make(map[string]any, len(value))
		for key, child := range value {
			result[key] = snapshotValue(child, replacements)
		}
		return result
	case []any:
		result := make([]any, len(value))
		for i, child := range value {
			result[i] = snapshotValue(child, replacements)
		}
		return result
	case string:
		value = replacements.Replace(value)
		if len(value) > 512 {
			// Preserve a useful preview, plus the digest of every omitted byte.
			preview := []rune(value)
			if len(preview) > 160 {
				preview = preview[:160]
			}
			return fmt.Sprintf("%s… [bytes=%d sha256=%x]", string(preview), len(value), sha256.Sum256([]byte(value)))
		}
		return value
	default:
		return value
	}
}

func assertContextRequests(t *testing.T, dialect string, requests []map[string]any, boundaries map[int]string, replacements []string) {
	t.Helper()
	got := renderContextRequests(requests, boundaries, replacements)
	path := filepath.Join("testdata", "context", dialect+".golden")
	if *updateContextSnapshots {
		if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(got), 0644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if got != string(want) {
		wantLines, gotLines := strings.Split(string(want), "\n"), strings.Split(got, "\n")
		for i := 0; i < min(len(wantLines), len(gotLines)); i++ {
			if wantLines[i] != gotLines[i] {
				t.Fatalf("%s:%d snapshot changed\nwant: %s\n got: %s\nUpdate deliberately with -update-context-snapshots", path, i+1, wantLines[i], gotLines[i])
			}
		}
		t.Fatalf("%s snapshot line count changed: %d -> %d; update deliberately with -update-context-snapshots", path, len(wantLines), len(gotLines))
	}
}

func TestContextSnapshotOffsetsOnlyNormalizeHistoryResults(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "tree.ndjson"), []byte("{}\n{}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	offsets := strings.NewReplacer(contextSnapshotOffsets(t, dir)...)
	search := `{"hits":[{"offset":3,"text":"evidence {\"offset\":3,\"next_offset\":6}","id":"entry"},{"offset":99}],"next_offset":6,"end":true}`
	note := `{"path":"task-notes.md","next_offset":6,"end":true}`
	input := []any{
		map[string]any{"type": "function_call_output", "call_id": "find-evidence", "output": search},
		map[string]any{"type": "function_call_output", "call_id": "recover-notes", "output": note},
		map[string]any{"role": "user", "content": search},
	}
	before := contextDialectJSON(input)
	got := snapshotHistoryOffsets(input, offsets, false).([]any)
	output := got[0].(map[string]any)["output"].(string)
	var parsed map[string]any
	if err := json.Unmarshal([]byte(output), &parsed); err != nil {
		t.Fatal(err)
	}
	hits := parsed["hits"].([]any)
	first := hits[0].(map[string]any)
	if first["offset"] != "<record-1-offset>" || parsed["next_offset"] != "<tree-end-offset>" || hits[1].(map[string]any)["offset"] != float64(99) {
		t.Fatalf("offset labels lost identity: %s", output)
	}
	if first["text"] != `evidence {"offset":3,"next_offset":6}` || got[1].(map[string]any)["output"] != note || got[2].(map[string]any)["content"] != search {
		t.Fatal("normalization changed notes, evidence, or user input")
	}
	if contextDialectJSON(input) != before {
		t.Fatal("normalization mutated source")
	}
}

func TestContextRequestSnapshotRenderer(t *testing.T) {
	requests := []map[string]any{
		{"model": "a", "input": []any{"first"}, "tools": []any{"read"}, "prompt_cache_key": "random-a"},
		{"model": "a", "input": []any{"first", "second"}, "tools": []any{"read"}, "prompt_cache_key": "random-a"},
		{"model": "b", "input": []any{"rewritten"}, "tools": []any{"read", "write"}, "prompt_cache_key": "random-b"},
		{"model": "b", "input": []any{"fresh"}},
	}
	before := contextDialectJSON(requests)
	got := renderContextRequests(requests, map[int]string{0: "start", 3: "reset"}, []string{"random-a", "cache-1", "random-b", "cache-2"})
	for _, want := range []string{"window 1: start", "window 2: reset", "append after 1 items", `model: "b"`, "rewritten", "write", "cache-1", "cache-2"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in %s", want, got)
		}
	}
	if before != contextDialectJSON(requests) {
		t.Fatal("snapshot renderer mutated request")
	}
	removed := renderContextRequests(requests[2:], map[int]string{0: "start"}, nil)
	if !strings.Contains(removed, "tools: removed") || !strings.Contains(removed, "prompt_cache_key: removed") {
		t.Fatalf("missing removals: %s", removed)
	}
	// Equal previews must not conceal a change in omitted content, even when
	// volatile paths differ. Preserve relationships between relabeled IDs.
	a := []map[string]any{{"text": "path-a/" + strings.Repeat("x", 600) + "A"}}
	b := []map[string]any{{"text": "path-b/" + strings.Repeat("x", 600) + "A"}}
	replace := []string{"path-a", "<dir>", "path-b", "<dir>"}
	if renderContextRequests(a, nil, replace) != renderContextRequests(b, nil, replace) {
		t.Fatal("volatile paths changed snapshot")
	}
	b[0]["text"] = "path-b/" + strings.Repeat("x", 600) + "B"
	if renderContextRequests(a, nil, replace) == renderContextRequests(b, nil, replace) {
		t.Fatal("omitted content change was hidden")
	}
	tool := map[string]any{"name": "read", "parameters": map[string]any{"type": "object"}}
	inventory := []map[string]any{{"tools": []any{tool}}}
	original := renderContextRequests(inventory, nil, nil)
	tool["parameters"] = map[string]any{"type": "string"}
	if renderContextRequests(inventory, nil, nil) == original {
		t.Fatal("schema change was hidden by tool fingerprint")
	}
}
