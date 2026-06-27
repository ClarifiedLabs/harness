package tools

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func runApplyPatchRaw(t *testing.T, input any) (string, error) {
	t.Helper()
	b, err := json.Marshal(input)
	if err != nil {
		t.Fatalf("marshal input: %v", err)
	}
	return applyPatch{}.Run(context.Background(), b)
}

func codexAddPatch(path, content string) string {
	var b strings.Builder
	b.WriteString("*** Begin Patch\n")
	b.WriteString("*** Add File: ")
	b.WriteString(path)
	b.WriteByte('\n')
	for _, line := range strings.Split(strings.TrimSuffix(content, "\n"), "\n") {
		b.WriteByte('+')
		b.WriteString(line)
		b.WriteByte('\n')
	}
	b.WriteString("*** End Patch\n")
	return b.String()
}

func TestApplyPatchSchemaAdvertisesCanonicalPatchOnly(t *testing.T) {
	schema := string((applyPatch{}).Schema())
	if !strings.Contains(schema, `"patch"`) {
		t.Fatalf("schema should advertise canonical patch field: %s", schema)
	}
	for _, legacy := range []string{`"patchText"`, `"patch_text"`} {
		if strings.Contains(schema, legacy) {
			t.Fatalf("schema should not advertise legacy alias %s: %s", legacy, schema)
		}
	}
}

func TestApplyPatchInputAliases(t *testing.T) {
	cases := []struct {
		name  string
		input func(string) any
	}{
		{
			name: "patch",
			input: func(text string) any {
				return map[string]any{"patch": text}
			},
		},
		{
			name: "patchText",
			input: func(text string) any {
				return map[string]any{"patchText": text}
			},
		},
		{
			name: "patch_text",
			input: func(text string) any {
				return map[string]any{"patch_text": text}
			},
		},
		{
			name: "raw_json_string",
			input: func(text string) any {
				return text
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "created.txt")
			text := codexAddPatch(path, "hello from "+tc.name+"\n")
			out, err := runApplyPatchRaw(t, tc.input(text))
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if !strings.Contains(out, "A "+path) {
				t.Fatalf("success output should report created file %q: %q", path, out)
			}
			got, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("read created file: %v", err)
			}
			if string(got) != "hello from "+tc.name+"\n" {
				t.Fatalf("created content = %q", got)
			}
		})
	}
}

func TestApplyPatchDuplicateAliasAllowedWhenSame(t *testing.T) {
	path := filepath.Join(t.TempDir(), "created.txt")
	text := codexAddPatch(path, "same\n")
	_, err := runApplyPatchRaw(t, map[string]any{
		"patch":     text,
		"patchText": text,
	})
	if err != nil {
		t.Fatalf("same patch supplied through aliases should be accepted: %v", err)
	}
}

func TestApplyPatchRejectsConflictingAliases(t *testing.T) {
	dir := t.TempDir()
	first := codexAddPatch(filepath.Join(dir, "first.txt"), "first\n")
	second := codexAddPatch(filepath.Join(dir, "second.txt"), "second\n")

	_, err := runApplyPatchRaw(t, map[string]any{
		"patch":     first,
		"patchText": second,
	})
	if err == nil {
		t.Fatal("expected conflicting alias error")
	}
	if !strings.Contains(err.Error(), "conflicting patch values") {
		t.Fatalf("error should explain conflicting aliases: %v", err)
	}
}

func TestApplyPatchParseErrorIncludesFormatHint(t *testing.T) {
	_, err := runApplyPatchRaw(t, map[string]any{
		"patch": "*** Begin Patch\n*** End Patch\n",
	})
	if err == nil {
		t.Fatal("expected parse error")
	}
	for _, want := range []string{
		"invalid Codex patch",
		"Expected one raw patch envelope",
		"Do not wrap the patch in markdown fences",
		"Blank context lines",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error %q should contain %q", err, want)
		}
	}
}

func TestApplyPatchMissingPatchNamesCanonicalField(t *testing.T) {
	_, err := runApplyPatchRaw(t, map[string]any{})
	if err == nil {
		t.Fatal("expected missing patch error")
	}
	if err.Error() != "patch is required" {
		t.Fatalf("error should name canonical field only: %v", err)
	}
}

func TestApplyPatchMutatedPathsIncludesMoveEndpoints(t *testing.T) {
	text := "*** Begin Patch\n" +
		"*** Update File: old.txt\n" +
		"*** Move to: new.txt\n" +
		"@@\n" +
		"-old\n" +
		"+new\n" +
		"*** End Patch\n"

	paths, err := (applyPatch{}).MutatedPaths(mustJSON(t, map[string]any{"patch": text}))
	if err != nil {
		t.Fatalf("MutatedPaths: %v", err)
	}
	if len(paths) != 2 || paths[0] != "old.txt" || paths[1] != "new.txt" {
		t.Fatalf("MutatedPaths = %v, want [old.txt new.txt]", paths)
	}
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return b
}
