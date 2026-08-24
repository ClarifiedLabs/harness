package main

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"harness/internal/cli"
	"harness/internal/session"
	"harness/internal/ui"
)

func runSessionEvidence(env environment, invocation cli.Invocation) int {
	format := strings.ToLower(cliLast(invocation.Flags, "format", "text"))
	if format != "text" && format != "json" {
		fmt.Fprintf(env.stderr, "harness: session evidence: unsupported --format %q (want text or json)\n", format)
		return ui.ExitUsage
	}
	prompt, err := parseSessionEvidenceInt("--prompt", cliLast(invocation.Flags, "prompt", "0"))
	if err != nil {
		fmt.Fprintf(env.stderr, "harness: session evidence: %v\n", err)
		return ui.ExitUsage
	}
	limit, err := parseSessionEvidenceInt("--limit", cliLast(invocation.Flags, "limit", fmt.Sprint(session.DefaultEvidenceLimit)))
	if err != nil {
		fmt.Fprintf(env.stderr, "harness: session evidence: %v\n", err)
		return ui.ExitUsage
	}
	query := session.EvidenceQuery{
		Kind:   cliLast(invocation.Flags, "kind", ""),
		Status: cliLast(invocation.Flags, "status", ""),
		Prompt: prompt,
		Limit:  limit,
	}
	if len(invocation.Args) == 2 {
		query.ID = invocation.Args[1]
		query.Limit = 1
	}
	if err := session.ValidateEvidenceQuery(query); err != nil {
		fmt.Fprintf(env.stderr, "harness: session evidence: %v\n", err)
		return ui.ExitUsage
	}
	page, err := session.QueryEvidence(invocation.Args[0], query)
	if err != nil {
		fmt.Fprintf(env.stderr, "harness: session evidence: %v\n", err)
		return ui.ExitRuntime
	}
	if query.ID != "" && len(page.Records) == 0 {
		fmt.Fprintf(env.stderr, "harness: session evidence: record %q not found\n", query.ID)
		return ui.ExitRuntime
	}
	if format == "json" {
		encoder := json.NewEncoder(env.stdout)
		encoder.SetIndent("", "  ")
		if err := encoder.Encode(page); err != nil {
			fmt.Fprintf(env.stderr, "harness: session evidence: encode JSON: %v\n", err)
			return ui.ExitRuntime
		}
		return ui.ExitOK
	}
	if query.ID != "" {
		fmt.Fprintln(env.stdout, ui.EvidenceRecordText(page.Records[0]))
	} else {
		fmt.Fprintln(env.stdout, ui.EvidenceListText(page))
	}
	return ui.ExitOK
}

func parseSessionEvidenceInt(name, value string) (int, error) {
	parsed, err := strconv.Atoi(value)
	if err != nil {
		return 0, fmt.Errorf("%s must be an integer", name)
	}
	return parsed, nil
}
