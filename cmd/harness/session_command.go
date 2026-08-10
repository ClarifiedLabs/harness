package main

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"harness/internal/cli"
	"harness/internal/session"
	"harness/internal/ui"
)

const sessionPromptPreviewRunes = 120

func runSessionList(env environment, invocation cli.Invocation) int {
	cwd, err := os.Getwd()
	if err != nil {
		fmt.Fprintf(env.stderr, "harness: session ls: determine working directory: %v\n", err)
		return ui.ExitRuntime
	}
	getenv := env.getenv
	if getenv == nil {
		getenv = func(string) string { return "" }
	}
	long := cliBool(invocation.Flags, "long")
	summaries, skipped, err := session.List(session.DefaultRoot(stateDir(getenv)), session.ListOptions{
		CWD:           cwd,
		All:           cliBool(invocation.Flags, "all"),
		IncludePrompt: long,
		ProbeActivity: long,
	})
	writeSessionListWarnings(env.stderr, skipped)
	if err != nil {
		fmt.Fprintf(env.stderr, "harness: session ls: %v\n", err)
		return ui.ExitRuntime
	}
	if !long {
		for _, summary := range summaries {
			fmt.Fprintln(env.stdout, summary.Path)
		}
		return ui.ExitOK
	}
	if len(summaries) == 0 {
		return ui.ExitOK
	}
	writeSessionLongHeader(env.stdout, "")
	for _, summary := range summaries {
		writeSessionLongRow(env.stdout, "", summary)
	}
	return ui.ExitOK
}

func runSessionResume(env environment, invocation cli.Invocation) int {
	if cliBool(invocation.Flags, "help") {
		if err := commandCatalog(env).WriteHelp(env.stdout, "session.resume"); err != nil {
			fmt.Fprintf(env.stderr, "harness: session resume help: %v\n", err)
			return ui.ExitRuntime
		}
		return ui.ExitOK
	}

	var source string
	if len(invocation.Args) == 1 {
		source = invocation.Args[0]
	} else {
		if sessionResumeRootInformational(invocation) {
			rootInvocation, err := rootInvocationForResume(env, invocation, "")
			if err != nil {
				fmt.Fprintf(env.stderr, "harness: session resume: forward root options: %v\n", err)
				return ui.ExitRuntime
			}
			return runRoot(env, rootInvocation)
		}
		if env.stdinPiped {
			fmt.Fprintln(env.stderr, "harness: session resume requires an explicit session path when stdin is piped")
			return ui.ExitUsage
		}
		cwd, err := os.Getwd()
		if err != nil {
			fmt.Fprintf(env.stderr, "harness: session resume: determine working directory: %v\n", err)
			return ui.ExitRuntime
		}
		getenv := env.getenv
		if getenv == nil {
			getenv = func(string) string { return "" }
		}
		summaries, skipped, err := session.List(session.DefaultRoot(stateDir(getenv)), session.ListOptions{
			CWD:           cwd,
			IncludePrompt: true,
			ProbeActivity: true,
		})
		writeSessionListWarnings(env.stderr, skipped)
		if err != nil {
			fmt.Fprintf(env.stderr, "harness: session resume: %v\n", err)
			return ui.ExitRuntime
		}
		if len(summaries) == 0 {
			fmt.Fprintf(env.stderr, "harness: no recorded sessions for working directory %s; use `harness session ls -a` or pass an explicit session path\n", cwd)
			return ui.ExitRuntime
		}
		entries := make([]sessionPickerEntry, len(summaries))
		for i, summary := range summaries {
			entries[i] = sessionPickerEntry{Summary: summary}
		}
		if env.stdin == nil {
			fmt.Fprintln(env.stderr, "harness: session resume: picker has no input reader")
			return ui.ExitUsage
		}
		reader := bufio.NewReader(&noReadAheadReader{reader: env.stdin})
		readLine := func(prompt string) (string, error) {
			if _, err := fmt.Fprint(env.stderr, prompt); err != nil {
				return "", err
			}
			line, err := reader.ReadString('\n')
			if err != nil {
				if errors.Is(err, io.EOF) && line != "" {
					return strings.TrimSuffix(line, "\r"), nil
				}
				return "", err
			}
			return strings.TrimSuffix(strings.TrimSuffix(line, "\n"), "\r"), nil
		}
		selected, err := ui.Pick(readLine, env.stderr, ui.PickerOptions[sessionPickerEntry]{
			Items:       entries,
			PageSize:    sessionPickerPageSize(env),
			Prompt:      "Session (number/path, /search, n/p, q): ",
			Kind:        "session",
			CancelError: ui.ErrPickerCancelled,
			PrintPage:   printSessionPickerPage,
		})
		if err != nil {
			if errors.Is(err, ui.ErrPickerCancelled) {
				fmt.Fprintln(env.stderr, "harness: session selection cancelled")
			} else {
				fmt.Fprintf(env.stderr, "harness: session selection: %v\n", err)
			}
			return ui.ExitUsage
		}
		source = selected.Path
	}

	rootInvocation, err := rootInvocationForResume(env, invocation, source)
	if err != nil {
		fmt.Fprintf(env.stderr, "harness: session resume: forward root options: %v\n", err)
		return ui.ExitRuntime
	}
	return runRoot(env, rootInvocation)
}

func sessionResumeRootInformational(invocation cli.Invocation) bool {
	return cliBool(invocation.Flags, "version") ||
		cliBool(invocation.Flags, "show_agents") ||
		cliBool(invocation.Flags, "show_models") ||
		cliBool(invocation.Flags, "check_model_proxy")
}

func rootInvocationForResume(env environment, invocation cli.Invocation, source string) (cli.Invocation, error) {
	argv := make([]string, 0, invocation.Flags.Len()+1)
	for _, occurrence := range invocation.Flags.Occurrences() {
		argv = append(argv, "--"+occurrence.Name+"="+occurrence.Value)
	}
	argv = append(argv, "--resume="+source)
	rootInvocation, err := commandCatalog(env).Parse(argv)
	if err != nil {
		return cli.Invocation{}, err
	}
	if rootInvocation.CommandID != "root" || rootInvocation.Action != cli.Run {
		return cli.Invocation{}, fmt.Errorf("forwarded invocation selected command %q", rootInvocation.CommandID)
	}
	return rootInvocation, nil
}

func writeSessionListWarnings(w io.Writer, skipped []error) {
	for _, warning := range skipped {
		var unsupported *session.UnsupportedSchemaVersionError
		if errors.As(warning, &unsupported) {
			continue
		}
		fmt.Fprintf(w, "harness: warning: %v\n", warning)
	}
}

func writeSessionLongHeader(w io.Writer, prefix string) {
	fmt.Fprintf(w, "%s%-8s  %-25s  %-25s  %s\n", prefix, "STATUS", "START", "END", "SESSION")
}

func writeSessionLongRow(w io.Writer, prefix string, summary session.Summary) {
	status := summary.Activity
	if status == "" {
		status = session.ActivityUnknown
	}
	end := formatSessionTime(summary.Updated)
	if status == session.ActivityActive {
		end = "-"
	}
	fmt.Fprintf(w, "%s%-8s  %-25s  %-25s  %s\n", prefix, status, formatSessionTime(summary.Created), end, summary.Path)
	fmt.Fprintf(w, "%s    prompt: %s\n", prefix, sessionPromptPreview(summary.InitialPrompt))
}

func formatSessionTime(value time.Time) string {
	if value.IsZero() {
		return "-"
	}
	return value.Format(time.RFC3339)
}

func sessionPromptPreview(prompt string) string {
	normalized := strings.Join(strings.Fields(prompt), " ")
	if normalized == "" {
		return "-"
	}
	runes := []rune(normalized)
	if len(runes) <= sessionPromptPreviewRunes {
		return normalized
	}
	return string(runes[:sessionPromptPreviewRunes-1]) + "…"
}

type sessionPickerEntry struct {
	session.Summary
}

func (entry sessionPickerEntry) PickerID() string { return entry.Path }

func (entry sessionPickerEntry) PickerName() string {
	return strings.TrimSpace(filepath.Base(entry.Path) + " " + entry.InitialPrompt)
}

func sessionPickerPageSize(env environment) int {
	size := pickerPageSize(env) / 2 // each session occupies two terminal rows
	if size < 1 {
		return 1
	}
	return size
}

func printSessionPickerPage(w io.Writer, entries []sessionPickerEntry, page, pageSize int, filter string) {
	start, end := ui.PickerPageBounds(page, pageSize, len(entries))
	title := fmt.Sprintf("Sessions %d-%d of %d", start+1, end, len(entries))
	if filter != "" {
		title += fmt.Sprintf(" matching %q", filter)
	}
	fmt.Fprintln(w, title)
	numberWidth := len(fmt.Sprintf("%d", len(entries)))
	indent := strings.Repeat(" ", numberWidth+2)
	for i := start; i < end; i++ {
		summary := entries[i].Summary
		endTime := formatSessionTime(summary.Updated)
		if summary.Activity == session.ActivityActive {
			endTime = "-"
		}
		fmt.Fprintf(w, "%*d. prompt: %s\n", numberWidth, i+1, sessionPromptPreview(summary.InitialPrompt))
		fmt.Fprintf(w, "%sstart: %s · end: %s · session: %s\n", indent, formatSessionTime(summary.Created), endTime, summary.Path)
	}
}

// noReadAheadReader restricts each underlying read to one byte. A buffered line
// reader over it can stop exactly at a picker newline without consuming pasted
// or typed-ahead bytes intended for the resumed REPL.
type noReadAheadReader struct {
	reader io.Reader
}

func (reader *noReadAheadReader) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	return reader.reader.Read(p[:1])
}
