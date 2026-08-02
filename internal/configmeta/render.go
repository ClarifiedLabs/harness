package configmeta

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"text/tabwriter"
)

const absent = "-"

// WriteText writes a deterministic, tabular parameter reference in catalog
// order. Absent input surfaces and defaults are rendered as "-".
func WriteText(w io.Writer, catalog Catalog) error {
	var output bytes.Buffer
	tw := tabwriter.NewWriter(&output, 0, 4, 2, ' ', 0)
	_, _ = fmt.Fprintln(tw, "KEY\tTYPE\tACCEPTED\tFLAGS\tENVIRONMENT\tJSON PATH\tDEFAULT\tSENSITIVE\tDESCRIPTION")
	for _, parameter := range catalog.parameters {
		_, _ = fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
			oneLine(parameter.Key),
			oneLine(parameter.Type),
			joinOrAbsent(parameter.Accepted, identity),
			joinOrAbsent(parameter.Flags, flagName),
			joinOrAbsent(parameter.Environment, identity),
			valueOrAbsent(parameter.JSONPath),
			formatDefault(parameter.Default),
			yesNo(parameter.Sensitive),
			oneLine(parameter.Description),
		)
	}
	if err := tw.Flush(); err != nil {
		return fmt.Errorf("render text reference: %w", err)
	}
	if _, err := w.Write(output.Bytes()); err != nil {
		return fmt.Errorf("write text reference: %w", err)
	}
	return nil
}

// WriteJSON writes a deterministic, indented JSON parameter reference in
// catalog order. Flag names include the leading dash users type.
func WriteJSON(w io.Writer, catalog Catalog) error {
	type jsonParameter struct {
		Key         string   `json:"key"`
		Type        string   `json:"type"`
		Accepted    []string `json:"accepted,omitempty"`
		Flags       []string `json:"flags,omitempty"`
		Environment []string `json:"environment,omitempty"`
		JSONPath    string   `json:"json_path,omitempty"`
		Default     *Default `json:"default,omitempty"`
		Sensitive   bool     `json:"sensitive,omitempty"`
		Description string   `json:"description"`
	}
	type reference struct {
		Parameters []jsonParameter `json:"parameters"`
	}

	parameters := make([]jsonParameter, 0, catalog.Len())
	for _, parameter := range catalog.parameters {
		flags := make([]string, len(parameter.Flags))
		for i, flag := range parameter.Flags {
			flags[i] = flagName(flag)
		}
		var defaultValue *Default
		if parameter.Default.Kind != "" {
			value := parameter.Default
			defaultValue = &value
		}
		parameters = append(parameters, jsonParameter{
			Key:         parameter.Key,
			Type:        parameter.Type,
			Accepted:    append([]string(nil), parameter.Accepted...),
			Flags:       flags,
			Environment: append([]string(nil), parameter.Environment...),
			JSONPath:    parameter.JSONPath,
			Default:     defaultValue,
			Sensitive:   parameter.Sensitive,
			Description: parameter.Description,
		})
	}

	var output bytes.Buffer
	encoder := json.NewEncoder(&output)
	encoder.SetEscapeHTML(false)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(reference{Parameters: parameters}); err != nil {
		return fmt.Errorf("render JSON reference: %w", err)
	}
	if _, err := w.Write(output.Bytes()); err != nil {
		return fmt.Errorf("write JSON reference: %w", err)
	}
	return nil
}

// WriteMarkdown writes a deterministic Markdown table in catalog order.
func WriteMarkdown(w io.Writer, catalog Catalog) error {
	var output strings.Builder
	output.WriteString("| Key | Type | Accepted | Flags | Environment | JSON path | Default | Sensitive | Description |\n")
	output.WriteString("| --- | --- | --- | --- | --- | --- | --- | --- | --- |\n")
	for _, parameter := range catalog.parameters {
		fmt.Fprintf(&output, "| %s | %s | %s | %s | %s | %s | %s | %s | %s |\n",
			markdownCode(parameter.Key),
			markdownCode(parameter.Type),
			markdownList(parameter.Accepted, identity),
			markdownList(parameter.Flags, flagName),
			markdownList(parameter.Environment, identity),
			markdownOptionalCode(parameter.JSONPath),
			markdownText(formatDefault(parameter.Default)),
			yesNo(parameter.Sensitive),
			markdownText(parameter.Description),
		)
	}
	if _, err := io.WriteString(w, output.String()); err != nil {
		return fmt.Errorf("write Markdown reference: %w", err)
	}
	return nil
}

func formatDefault(value Default) string {
	if value.Kind == "" {
		return absent
	}
	display := value.Display
	if display == "" {
		encoded, err := json.Marshal(value.Value)
		if err != nil {
			// Catalog validation ensures this cannot occur.
			display = fmt.Sprint(value.Value)
		} else {
			display = string(encoded)
		}
	}
	if value.Kind == DefaultDerived {
		display = "derived: " + display
	}
	if value.Note != "" {
		display += " (" + oneLine(value.Note) + ")"
	}
	return oneLine(display)
}

func joinOrAbsent(values []string, transform func(string) string) string {
	if len(values) == 0 {
		return absent
	}
	formatted := make([]string, len(values))
	for i, value := range values {
		formatted[i] = oneLine(transform(value))
	}
	return strings.Join(formatted, ", ")
}

func markdownList(values []string, transform func(string) string) string {
	if len(values) == 0 {
		return absent
	}
	formatted := make([]string, len(values))
	for i, value := range values {
		formatted[i] = markdownCode(transform(value))
	}
	return strings.Join(formatted, ", ")
}

func markdownOptionalCode(value string) string {
	if value == "" {
		return absent
	}
	return markdownCode(value)
}

func markdownCode(value string) string {
	value = strings.ReplaceAll(value, "`", "&#96;")
	value = strings.ReplaceAll(value, "|", "&#124;")
	value = strings.ReplaceAll(value, "\r\n", "<br>")
	value = strings.NewReplacer("\r", "<br>", "\n", "<br>").Replace(value)
	return "`" + value + "`"
}

func markdownText(value string) string {
	value = strings.ReplaceAll(value, "\\", "\\\\")
	value = strings.ReplaceAll(value, "|", "\\|")
	value = strings.ReplaceAll(value, "\r\n", "<br>")
	return strings.NewReplacer("\r", "<br>", "\n", "<br>").Replace(value)
}

func oneLine(value string) string {
	return strings.Join(strings.Fields(value), " ")
}

func identity(value string) string { return value }

func flagName(value string) string { return "-" + value }

func valueOrAbsent(value string) string {
	if value == "" {
		return absent
	}
	return oneLine(value)
}

func yesNo(value bool) string {
	if value {
		return "yes"
	}
	return "no"
}
