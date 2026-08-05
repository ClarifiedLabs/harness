package tools

import (
	"fmt"
	"maps"
	"strconv"
	"strings"

	"harness/internal/llm"
)

const (
	searchMetricBatchMember              = "search_batch_member"
	searchMetricBatchOwner               = "search_batch_metrics_owner"
	searchMetricBatchCalls               = "search_batch_calls"
	searchMetricContextBeforeBatch       = "search_batch_context_lines_before"
	searchMetricUniqueContextBeforeLimit = "search_batch_unique_context_lines"
	searchMetricContextAfterBatch        = "search_batch_context_lines_after"
	searchMetricDuplicateLines           = "search_batch_duplicate_lines_suppressed"
	searchMetricBudgetOmittedLines       = "search_batch_budget_lines_omitted"
	searchMetricBytesBeforeBatch         = "search_batch_bytes_before"
	searchMetricBytesAfterBatch          = "search_batch_bytes_after"
	searchMetricLowYieldCalls            = "search_batch_low_yield_calls"
	searchMetricOverlapPermille          = "search_batch_overlap_permille"
	searchMetricYieldPermille            = "search_batch_yield_permille"
)

type parsedSearchResult struct {
	summary string
	lines   []searchSourceLine
	notes   []string
}

type searchSourceLine struct {
	path   string
	number int
	text   string
}

type batchedSearchResult struct {
	resultIndex int
	parsed      parsedSearchResult
	owned       []searchSourceLine
	duplicates  int
}

// PrepareReadOnlyBatch applies cross-call result shaping that requires the
// complete concurrent island. Search calls retain one result per tool_use, but
// do not repeat file/line context already returned by an earlier sibling
// search. Every call retains its individually bounded unique context; the batch
// layer does not impose another aggregate line or byte cap.
func (*Registry) PrepareReadOnlyBatch(calls []llm.ToolCall, results []llm.ToolResult) {
	if len(calls) != len(results) {
		return
	}
	items := make([]batchedSearchResult, 0, len(calls))
	for i := range calls {
		if calls[i].Name != "search" || results[i].IsError {
			continue
		}
		parsed, ok := parseSearchResultForBatch(results[i].Text)
		if !ok {
			continue
		}
		items = append(items, batchedSearchResult{resultIndex: i, parsed: parsed})
	}
	if len(items) < 2 {
		return
	}

	seen := make(map[string]bool)
	contextBefore := 0
	for i := range items {
		contextBefore += len(items[i].parsed.lines)
		for _, line := range items[i].parsed.lines {
			key := searchSourceLineKey(line)
			if seen[key] {
				items[i].duplicates++
				continue
			}
			seen[key] = true
			items[i].owned = append(items[i].owned, line)
		}
	}

	texts := renderBatchedSearchItems(items, results)

	contextAfter := len(seen)
	uniqueBeforeLimit := len(seen)
	duplicates := max(contextBefore-uniqueBeforeLimit, 0)
	bytesBefore := 0
	bytesAfter := 0
	lowYieldCalls := 0
	for i := range items {
		item := &items[i]
		result := &results[item.resultIndex]
		bytesBefore += len(result.Text)
		bytesAfter += len(texts[i])
		if len(item.owned) == 0 {
			lowYieldCalls++
		}
		if result.Metrics == nil {
			result.Metrics = make(map[string]int)
		} else {
			result.Metrics = maps.Clone(result.Metrics)
		}
		result.Metrics[searchMetricBatchMember] = 1
		result.Metrics["context_lines_before_batch"] = len(item.parsed.lines)
		result.Metrics["context_lines"] = len(item.owned)
		result.Metrics["files_shown"] = selectedSearchFileCount(item.owned)
		result.Metrics["batch_unique_lines_contributed"] = len(item.owned)
		result.Metrics["batch_duplicate_lines_suppressed"] = item.duplicates
		result.Metrics["batch_budget_lines_omitted"] = 0
		if texts[i] != result.Text {
			if result.OriginalBytes == 0 {
				result.OriginalBytes = len(result.Text)
			}
			result.Text = texts[i]
			result.ShownBytes = len(result.Text)
		}
	}

	owner := &results[items[0].resultIndex]
	owner.Metrics[searchMetricBatchOwner] = 1
	owner.Metrics[searchMetricBatchCalls] = len(items)
	owner.Metrics[searchMetricContextBeforeBatch] = contextBefore
	owner.Metrics[searchMetricUniqueContextBeforeLimit] = uniqueBeforeLimit
	owner.Metrics[searchMetricContextAfterBatch] = contextAfter
	owner.Metrics[searchMetricDuplicateLines] = duplicates
	owner.Metrics[searchMetricBudgetOmittedLines] = 0
	owner.Metrics[searchMetricBytesBeforeBatch] = bytesBefore
	owner.Metrics[searchMetricBytesAfterBatch] = bytesAfter
	owner.Metrics[searchMetricLowYieldCalls] = lowYieldCalls
	if contextBefore > 0 {
		owner.Metrics[searchMetricOverlapPermille] = duplicates * 1000 / contextBefore
		owner.Metrics[searchMetricYieldPermille] = uniqueBeforeLimit * 1000 / contextBefore
	}
}

func parseSearchResultForBatch(text string) (parsedSearchResult, bool) {
	lines := strings.Split(strings.TrimRight(text, "\n"), "\n")
	if len(lines) == 0 {
		return parsedSearchResult{}, false
	}
	if lines[0] == "(no matches)" {
		return parsedSearchResult{summary: lines[0]}, true
	}
	if !strings.HasPrefix(lines[0], "matches: ") {
		return parsedSearchResult{}, false
	}
	parsed := parsedSearchResult{summary: lines[0]}
	currentPath := ""
	for _, line := range lines[1:] {
		if path, ok := parseSearchContextHeader(line); ok {
			currentPath = path
			continue
		}
		if currentPath != "" {
			if tab := strings.IndexByte(line, '\t'); tab > 0 {
				number, err := strconv.Atoi(line[:tab])
				if err == nil {
					parsed.lines = append(parsed.lines, searchSourceLine{path: currentPath, number: number, text: line[tab+1:]})
					continue
				}
			}
		}
		if strings.TrimSpace(line) != "" {
			parsed.notes = append(parsed.notes, line)
		}
	}
	return parsed, true
}

func parseSearchContextHeader(line string) (string, bool) {
	if !strings.HasPrefix(line, "==> ") || !strings.HasSuffix(line, " <==") {
		return "", false
	}
	body := strings.TrimSuffix(strings.TrimPrefix(line, "==> "), " <==")
	colon := strings.LastIndexByte(body, ':')
	if colon <= 0 {
		return "", false
	}
	span := body[colon+1:]
	dash := strings.LastIndexByte(span, '-')
	if dash <= 0 {
		return "", false
	}
	if _, err := strconv.Atoi(span[:dash]); err != nil {
		return "", false
	}
	if _, err := strconv.Atoi(span[dash+1:]); err != nil {
		return "", false
	}
	return body[:colon], true
}

func searchSourceLineKey(line searchSourceLine) string {
	return line.path + "\x00" + strconv.Itoa(line.number)
}

func renderBatchedSearchItems(items []batchedSearchResult, results []llm.ToolResult) []string {
	texts := make([]string, len(items))
	for i, item := range items {
		if item.duplicates == 0 && len(item.owned) == len(item.parsed.lines) {
			texts[i] = results[item.resultIndex].Text
			continue
		}
		texts[i] = renderBatchedSearchResult(item.parsed, item.owned, item.duplicates)
	}
	return texts
}

func renderBatchedSearchResult(parsed parsedSearchResult, selected []searchSourceLine, duplicates int) string {
	var b strings.Builder
	b.WriteString(parsed.summary)
	for _, group := range groupSearchSourceLines(selected) {
		fmt.Fprintf(&b, "\n\n==> %s:%d-%d <==\n", group[0].path, group[0].number, group[len(group)-1].number)
		for _, line := range group {
			fmt.Fprintf(&b, "%d\t%s\n", line.number, line.text)
		}
	}
	for _, note := range parsed.notes {
		b.WriteString("\n")
		b.WriteString(note)
	}
	if duplicates > 0 {
		fmt.Fprintf(&b, "\n[search batch: %d duplicate context lines shown by sibling searches]", duplicates)
	}
	return strings.TrimRight(b.String(), "\n")
}

func groupSearchSourceLines(lines []searchSourceLine) [][]searchSourceLine {
	if len(lines) == 0 {
		return nil
	}
	groups := [][]searchSourceLine{{lines[0]}}
	for _, line := range lines[1:] {
		lastGroup := groups[len(groups)-1]
		last := lastGroup[len(lastGroup)-1]
		if line.path == last.path && line.number == last.number+1 {
			groups[len(groups)-1] = append(lastGroup, line)
			continue
		}
		groups = append(groups, []searchSourceLine{line})
	}
	return groups
}

func selectedSearchFileCount(lines []searchSourceLine) int {
	files := make(map[string]bool)
	for _, line := range lines {
		files[line.path] = true
	}
	return len(files)
}
