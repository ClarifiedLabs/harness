package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"harness/internal/tools/patch"
)

const applyPatchSchema = `{
  "type": "object",
  "properties": {
    "patch": {"type": "string", "description": "Codex apply_patch text beginning with *** Begin Patch and ending with *** End Patch. Supports *** Add File, *** Delete File, *** Update File, and *** Move to. Preferred field name."},
    "patchText": {"type": "string", "description": "Alias for patch, accepted for compatibility."},
    "patch_text": {"type": "string", "description": "Alias for patch, accepted for compatibility."}
  }
}`

type applyPatch struct{}

func (applyPatch) Name() string { return "apply_patch" }

func (applyPatch) Description() string {
	return "Apply a Codex-format patch. Provide a JSON object with patch as the raw patch text. Supports add, delete, update, and move. Prefer edit and write_file for ordinary changes; for renames you can also use run_command mv / git mv."
}

func (applyPatch) Schema() json.RawMessage { return json.RawMessage(applyPatchSchema) }

func (applyPatch) ReadOnly(json.RawMessage) bool { return false }

func (applyPatch) MutatedPaths(input json.RawMessage) ([]string, error) {
	text, err := decodeApplyPatchInput(input)
	if err != nil {
		return nil, err
	}
	files, err := patch.ParseCodex(text)
	if err != nil {
		return nil, err
	}
	paths := make([]string, 0, len(files)*2)
	for _, f := range files {
		if f.Old != "" {
			paths = append(paths, f.Old)
		}
		if f.New != "" {
			paths = append(paths, f.New)
		}
	}
	return paths, nil
}

func (applyPatch) Run(ctx context.Context, input json.RawMessage) (string, error) {
	text, err := decodeApplyPatchInput(input)
	if err != nil {
		return "", err
	}

	files, err := patch.ParseCodex(text)
	if err != nil {
		return "", badArgs("invalid Codex patch: %v\nExpected one raw patch envelope in the patch field: begin with %q, include one or more file operations, and end with %q. Do not wrap the patch in markdown fences. Blank context lines inside update hunks must be prefixed with a space.", err, "*** Begin Patch", "*** End Patch")
	}

	res := patch.ApplyCodex(files)
	report := formatReport(files, res)
	if len(res.Rejected) > 0 {
		return report, fmt.Errorf("%s", report)
	}
	return report, nil
}

func decodeApplyPatchInput(input json.RawMessage) (string, error) {
	var text string
	if err := json.Unmarshal(input, &text); err == nil {
		if text == "" {
			return "", badArgs("patch is required")
		}
		return text, nil
	}

	var args struct {
		Patch          string `json:"patch"`
		PatchText      string `json:"patchText"`
		PatchTextSnake string `json:"patch_text"`
	}
	if err := json.Unmarshal(input, &args); err != nil {
		return "", err
	}

	values := []struct {
		name  string
		value string
	}{
		{name: "patch", value: args.Patch},
		{name: "patchText", value: args.PatchText},
		{name: "patch_text", value: args.PatchTextSnake},
	}

	chosenName := ""
	chosenValue := ""
	for _, v := range values {
		if v.value != "" {
			if chosenValue == "" {
				chosenName = v.name
				chosenValue = v.value
				continue
			}
			if v.value != chosenValue {
				return "", badArgs("conflicting patch values: %s and %s differ; provide only one of patch, patchText, or patch_text", chosenName, v.name)
			}
		}
	}
	if chosenValue != "" {
		return chosenValue, nil
	}
	return "", badArgs("patch is required; provide patch, patchText, or patch_text")
}

func formatReport(files []patch.FilePatch, res patch.Result) string {
	if len(res.Rejected) > 0 {
		r := res.Rejected[0]
		return fmt.Sprintf("Failed to apply patch to %s: %s", r.Path, r.Reason)
	}
	if len(res.Applied) == 0 {
		return "no changes"
	}

	statusByPath := make(map[string]string, len(files))
	for _, f := range files {
		status := "M"
		switch {
		case f.IsCreate:
			status = "A"
		case f.IsDelete:
			status = "D"
		}
		statusByPath[f.Path()] = status
	}

	var b strings.Builder
	b.WriteString("Success. Updated the following files:\n")
	for _, path := range res.Applied {
		fmt.Fprintf(&b, "%s %s\n", statusByPath[path], path)
	}
	return strings.TrimRight(b.String(), "\n")
}
