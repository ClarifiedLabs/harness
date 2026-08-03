package cli

import (
	"fmt"
	"io"
	"strconv"
	"strings"
)

// WriteHelp writes deterministic, scope-specific help for commandID. It lists
// only that command's flags and direct children; flags from ancestor scopes are
// not inherited. Declaration order is preserved. Writer errors are returned.
func WriteHelp(w io.Writer, catalog Catalog, commandID string) error {
	if catalog.root == nil {
		return validationError("catalog", "zero-value catalog")
	}
	node, ok := catalog.byID[commandID]
	if !ok {
		return &CommandError{CommandID: commandID, Problem: "command ID is not in the catalog"}
	}

	var output strings.Builder
	output.WriteString("Usage:\n  ")
	output.WriteString(strings.Join(node.path, " "))
	if len(node.command.Flags) > 0 {
		output.WriteString(" [flags]")
	}
	if len(node.children) > 0 {
		if node.command.Runnable {
			output.WriteString(" [command]")
		} else {
			output.WriteString(" <command>")
		}
	}
	if node.command.Args.Usage != "" {
		output.WriteByte(' ')
		output.WriteString(node.command.Args.Usage)
	}
	output.WriteByte('\n')

	description := node.command.Description
	if description == "" {
		description = node.command.Summary
	}
	if description != "" {
		output.WriteByte('\n')
		output.WriteString(description)
		output.WriteByte('\n')
	}

	if len(node.children) > 0 {
		output.WriteString("\nCommands:\n")
		labels := make([]string, len(node.children))
		width := 0
		for i, child := range node.children {
			label := child.command.Name
			if len(child.command.Aliases) > 0 {
				label += " (" + strings.Join(child.command.Aliases, ", ") + ")"
			}
			labels[i] = label
			if len(label) > width {
				width = len(label)
			}
		}
		for i, child := range node.children {
			fmt.Fprintf(&output, "  %-*s  %s\n", width, labels[i], child.command.Summary)
		}
	}

	if len(node.command.Flags) > 0 {
		output.WriteString("\nFlags:\n")
		labels := make([]string, len(node.command.Flags))
		width := 0
		for i, declaration := range node.command.Flags {
			labels[i] = flagHelpLabel(declaration)
			if len(labels[i]) > width {
				width = len(labels[i])
			}
		}
		for i, declaration := range node.command.Flags {
			fmt.Fprintf(&output, "  %-*s  %s\n", width, labels[i], flagHelpDescription(declaration))
		}
	}

	if len(node.command.Examples) > 0 {
		output.WriteString("\nExamples:\n")
		for _, example := range node.command.Examples {
			output.WriteString("  ")
			output.WriteString(example)
			output.WriteByte('\n')
		}
	}

	text := output.String()
	n, err := io.WriteString(w, text)
	if err != nil {
		return err
	}
	if n != len(text) {
		return io.ErrShortWrite
	}
	return nil
}

// WriteHelp writes help for commandID using c.
func (c Catalog) WriteHelp(w io.Writer, commandID string) error {
	return WriteHelp(w, c, commandID)
}

func flagHelpLabel(declaration Flag) string {
	names := make([]string, len(declaration.Names))
	for i, name := range declaration.Names {
		names[i] = "-" + name
	}
	label := strings.Join(names, ", ")
	if declaration.Kind == ValueFlag {
		valueName := declaration.ValueName
		if valueName == "" {
			valueName = "value"
		}
		label += " <" + valueName + ">"
	}
	return label
}

func flagHelpDescription(declaration Flag) string {
	details := make([]string, 0, 3)
	if declaration.Default != "" {
		defaultValue := strconv.Quote(declaration.Default)
		if declaration.Kind == BoolFlag {
			defaultValue = declaration.Default
		}
		details = append(details, "default "+defaultValue)
	}
	if len(declaration.Environment) > 0 {
		details = append(details, "env: "+strings.Join(declaration.Environment, ", "))
	}
	if declaration.Repeatable {
		details = append(details, "repeatable")
	}
	if len(details) == 0 {
		return declaration.Description
	}
	if declaration.Description == "" {
		return "(" + strings.Join(details, "; ") + ")"
	}
	return declaration.Description + " (" + strings.Join(details, "; ") + ")"
}
