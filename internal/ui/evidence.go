package ui

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"harness/internal/session"
)

const evidenceCommandUsage = "/evidence [list [--kind evaluator|tool] [--status STATUS] [--prompt N] [--limit N] | show <id>]"

func (app *App) evidenceCommand(arg string) {
	action, rest := splitHandoffCommandToken(arg)
	switch {
	case action == "", strings.EqualFold(action, "list"), strings.HasPrefix(action, "--"):
		if strings.HasPrefix(action, "--") {
			rest = strings.TrimSpace(arg)
		}
		query, err := parseEvidenceQuery(rest)
		if err != nil {
			fmt.Fprintf(app.Errw, "[evidence: %v; usage: %s]\n", err, evidenceCommandUsage)
			return
		}
		page, err := session.QueryEvidence(app.SessionPath, query)
		if err != nil {
			fmt.Fprintf(app.Errw, "[evidence unavailable: %v]\n", err)
			return
		}
		fmt.Fprintln(app.Errw, EvidenceListText(page))
	case strings.EqualFold(action, "show"):
		fields := strings.Fields(rest)
		if len(fields) != 1 {
			fmt.Fprintf(app.Errw, "[evidence show: usage: %s]\n", evidenceCommandUsage)
			return
		}
		page, err := session.QueryEvidence(app.SessionPath, session.EvidenceQuery{ID: fields[0], Limit: 1})
		if err != nil {
			fmt.Fprintf(app.Errw, "[evidence unavailable: %v]\n", err)
			return
		}
		if len(page.Records) == 0 {
			fmt.Fprintf(app.Errw, "[evidence record %q not found]\n", fields[0])
			return
		}
		fmt.Fprintln(app.Errw, EvidenceRecordText(page.Records[0]))
	default:
		fmt.Fprintf(app.Errw, "[evidence: unknown action %q; usage: %s]\n", action, evidenceCommandUsage)
	}
}

func parseEvidenceQuery(input string) (session.EvidenceQuery, error) {
	fields := strings.Fields(input)
	query := session.EvidenceQuery{Limit: session.DefaultEvidenceLimit}
	for i := 0; i < len(fields); i++ {
		name, value, inline := strings.Cut(fields[i], "=")
		if !inline {
			if i+1 >= len(fields) {
				return session.EvidenceQuery{}, fmt.Errorf("%s requires a value", name)
			}
			i++
			value = fields[i]
		}
		switch name {
		case "--kind":
			query.Kind = value
		case "--status":
			query.Status = value
		case "--prompt":
			prompt, err := strconv.Atoi(value)
			if err != nil {
				return session.EvidenceQuery{}, fmt.Errorf("--prompt must be an integer")
			}
			query.Prompt = prompt
		case "--limit":
			limit, err := strconv.Atoi(value)
			if err != nil {
				return session.EvidenceQuery{}, fmt.Errorf("--limit must be an integer")
			}
			query.Limit = limit
		default:
			return session.EvidenceQuery{}, fmt.Errorf("unknown option %q", name)
		}
	}
	if err := session.ValidateEvidenceQuery(query); err != nil {
		return session.EvidenceQuery{}, err
	}
	return query, nil
}

// EvidenceListText renders the shared bounded human-facing catalog list used
// by the REPL and the session evidence CLI command.
func EvidenceListText(page session.EvidencePage) string {
	if page.Total == 0 {
		return "evidence catalog: no evaluator results, archived tool outputs, or tool errors"
	}
	if page.Matched == 0 {
		return fmt.Sprintf("evidence catalog: no matching records (%d total)", page.Total)
	}
	var b strings.Builder
	fmt.Fprintf(&b, "evidence catalog: %d of %d matching record", page.Returned, page.Matched)
	if page.Matched != 1 {
		b.WriteByte('s')
	}
	fmt.Fprintf(&b, " (%d total; newest first)", page.Total)
	if page.Omitted > 0 {
		fmt.Fprintf(&b, " · %d omitted by limit", page.Omitted)
	}
	for _, record := range page.Records {
		fmt.Fprintf(&b, "\n  %s · %s · %s · %s", record.ID, record.Kind, record.Outcome, record.Status)
		if record.Prompt > 0 {
			fmt.Fprintf(&b, " · p%d", record.Prompt)
			if record.Turn > 0 {
				fmt.Fprintf(&b, "/t%d", record.Turn)
			}
		}
		if record.Source != "" {
			fmt.Fprintf(&b, " · %s", clipEvidenceField(record.Source, 48))
		}
		if record.Reference != "" {
			fmt.Fprintf(&b, " · %s", clipEvidenceField(record.Reference, 96))
		}
		if record.Path != "" && (record.Status == session.EvidenceStatusAvailable || record.Status == session.EvidenceStatusStale) {
			fmt.Fprintf(&b, " · %d bytes", record.Bytes)
		}
	}
	return b.String()
}

// EvidenceRecordText renders one metadata record without reading its artifact
// body. Paths and references are shown only to the human command caller.
func EvidenceRecordText(record session.EvidenceRecord) string {
	var b strings.Builder
	fmt.Fprintf(&b, "evidence %s\n  kind: %s\n  outcome: %s\n  status: %s", record.ID, record.Kind, record.Outcome, record.Status)
	if record.Prompt > 0 {
		fmt.Fprintf(&b, "\n  prompt: %d", record.Prompt)
	}
	if record.Turn > 0 {
		fmt.Fprintf(&b, "\n  turn: %d", record.Turn)
	}
	if record.Time != nil {
		fmt.Fprintf(&b, "\n  event time: %s", record.Time.Format(time.RFC3339Nano))
	}
	if record.Source != "" {
		fmt.Fprintf(&b, "\n  source: %s", record.Source)
	}
	if record.Reference != "" {
		fmt.Fprintf(&b, "\n  reference: %s", record.Reference)
	}
	if record.Path != "" {
		fmt.Fprintf(&b, "\n  resolved path: %s", record.Path)
	}
	if record.Path != "" && (record.Status == session.EvidenceStatusAvailable || record.Status == session.EvidenceStatusStale) {
		fmt.Fprintf(&b, "\n  bytes: %d", record.Bytes)
	}
	if record.Modified != nil {
		fmt.Fprintf(&b, "\n  modified: %s", record.Modified.Format(time.RFC3339Nano))
	}
	if record.Score != nil {
		fmt.Fprintf(&b, "\n  score: %g", *record.Score)
	}
	if record.ScoreDirection != "" {
		fmt.Fprintf(&b, "\n  score direction: %s", record.ScoreDirection)
	}
	if record.Candidate != "" {
		fmt.Fprintf(&b, "\n  candidate: %s", record.Candidate)
	}
	if record.ErrorKind != "" {
		fmt.Fprintf(&b, "\n  error kind: %s", record.ErrorKind)
	}
	if record.ErrorExcerpt != "" {
		fmt.Fprintf(&b, "\n  error excerpt: %s", record.ErrorExcerpt)
	}
	if record.Summary != "" {
		fmt.Fprintf(&b, "\n  summary: %s", record.Summary)
	}
	return b.String()
}

func clipEvidenceField(value string, limit int) string {
	value = strings.Join(strings.Fields(value), " ")
	runes := []rune(value)
	if len(runes) <= limit {
		return value
	}
	left := (limit - 1) / 2
	right := limit - 1 - left
	return string(runes[:left]) + "…" + string(runes[len(runes)-right:])
}
