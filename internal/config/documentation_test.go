package config

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	"harness/internal/configmeta"
	"harness/internal/mcpproxy"
	modelconfig "harness/internal/modelproxy/config"
)

func TestCanonicalDocumentationTokensMatchCatalogs(t *testing.T) {
	usagePath := filepath.Join("..", "..", "docs", "usage.md")
	mcpPath := filepath.Join("..", "..", "docs", "mcp.md")

	t.Run("harness root flags", func(t *testing.T) {
		documented := documentedFlagDeclarations(t, usagePath, "harness-root-flags")
		var catalog []string
		for _, flag := range CLIFlags() {
			catalog = append(catalog, flag.Names...)
		}
		assertSameTokens(t, documented, catalog)
	})

	tests := []struct {
		name    string
		path    string
		marker  string
		catalog configmeta.Catalog
	}{
		{name: "harness parameters", path: usagePath, marker: "harness-config-parameters", catalog: Catalog()},
		{name: "model proxy parameters", path: usagePath, marker: "model-proxy-config-parameters", catalog: modelconfig.Catalog()},
		{name: "MCP proxy parameters", path: mcpPath, marker: "mcp-proxy-config-parameters", catalog: mcpproxy.Catalog()},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			documented := documentedParameterTokens(t, test.path, test.marker)
			assertSameTokens(t, documented.keys, catalogParameterTokens(test.catalog).keys)
			assertSameTokens(t, documented.flags, catalogParameterTokens(test.catalog).flags)
			assertSameTokens(t, documented.jsonPaths, catalogParameterTokens(test.catalog).jsonPaths)
		})
	}
}

type parameterTokens struct {
	keys      []string
	flags     []string
	jsonPaths []string
}

func catalogParameterTokens(catalog configmeta.Catalog) parameterTokens {
	var tokens parameterTokens
	for _, parameter := range catalog.Parameters() {
		tokens.keys = append(tokens.keys, parameter.Key)
		tokens.flags = append(tokens.flags, parameter.Flags...)
		if parameter.JSONPath != "" {
			tokens.jsonPaths = append(tokens.jsonPaths, parameter.JSONPath)
		}
	}
	return tokens
}

func documentedParameterTokens(t *testing.T, path, marker string) parameterTokens {
	t.Helper()
	section := documentedSection(t, path, marker)
	var tokens parameterTokens
	for _, line := range strings.Split(section, "\n") {
		if !strings.HasPrefix(line, "| `") {
			continue
		}
		columns := strings.Split(strings.TrimSuffix(strings.TrimPrefix(line, "| "), " |"), " | ")
		if len(columns) != 9 {
			t.Fatalf("%s section %q has malformed parameter row %q", path, marker, line)
		}
		tokens.keys = append(tokens.keys, markdownCodeTokens(t, path, marker, columns[0])...)
		tokens.flags = append(tokens.flags, trimFlagPrefixes(markdownCodeTokens(t, path, marker, columns[3]))...)
		tokens.jsonPaths = append(tokens.jsonPaths, markdownCodeTokens(t, path, marker, columns[5])...)
	}
	if len(tokens.keys) == 0 {
		t.Fatalf("%s section %q has no parameter rows", path, marker)
	}
	return tokens
}

var markdownCodeToken = regexp.MustCompile("`([^`]+)`")

func markdownCodeTokens(t *testing.T, path, marker, column string) []string {
	t.Helper()
	if column == "-" {
		return nil
	}
	matches := markdownCodeToken.FindAllStringSubmatch(column, -1)
	if len(matches) == 0 {
		t.Fatalf("%s section %q has non-token column %q", path, marker, column)
	}
	tokens := make([]string, len(matches))
	for i, match := range matches {
		tokens[i] = match[1]
	}
	return tokens
}

func trimFlagPrefixes(flags []string) []string {
	for i := range flags {
		flags[i] = strings.TrimPrefix(flags[i], "-")
	}
	return flags
}

var declaredFlagToken = regexp.MustCompile(`(?:^|,\s+)-{1,2}([a-z][a-z0-9-]*)`)

func documentedFlagDeclarations(t *testing.T, path, marker string) []string {
	t.Helper()
	section := documentedSection(t, path, marker)
	var tokens []string
	for _, line := range strings.Split(section, "\n") {
		if !strings.HasPrefix(line, "-") {
			continue
		}
		for _, match := range declaredFlagToken.FindAllStringSubmatch(line, -1) {
			tokens = append(tokens, match[1])
		}
	}
	if len(tokens) == 0 {
		t.Fatalf("%s section %q has no flag declarations", path, marker)
	}
	return tokens
}

func documentedSection(t *testing.T, path, marker string) string {
	t.Helper()
	document, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	startMarker := "<!-- " + marker + ":start -->"
	endMarker := "<!-- " + marker + ":end -->"
	if count := strings.Count(string(document), startMarker); count != 1 {
		t.Fatalf("%s has %d %q markers, want 1", path, count, startMarker)
	}
	if count := strings.Count(string(document), endMarker); count != 1 {
		t.Fatalf("%s has %d %q markers, want 1", path, count, endMarker)
	}
	start := strings.Index(string(document), startMarker) + len(startMarker)
	end := strings.Index(string(document), endMarker)
	if end < start {
		t.Fatalf("%s has out-of-order %q markers", path, marker)
	}
	return string(document[start:end])
}

func assertSameTokens(t *testing.T, documented, catalog []string) {
	t.Helper()
	documented = uniqueSorted(documented)
	catalog = uniqueSorted(catalog)
	if strings.Join(documented, "\x00") != strings.Join(catalog, "\x00") {
		t.Fatalf("documented tokens do not match catalog\ndocumented: %v\ncatalog:    %v", documented, catalog)
	}
}

func uniqueSorted(tokens []string) []string {
	set := make(map[string]struct{}, len(tokens))
	for _, token := range tokens {
		set[token] = struct{}{}
	}
	tokens = tokens[:0]
	for token := range set {
		tokens = append(tokens, token)
	}
	sort.Strings(tokens)
	return tokens
}
