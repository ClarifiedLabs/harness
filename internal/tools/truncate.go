package tools

import (
	"fmt"
	"strconv"
	"strings"
)

// Central output caps, applied in Dispatch as a backstop for every tool by
// default. A Registry can override these limits.
const (
	defaultMaxResultBytes    = 64 * 1024
	defaultMaxResultLines    = 1000
	defaultSearchResultBytes = 32 * 1024
	defaultSearchResultLines = 500
	defaultReadResultBytes   = 64 * 1024
	defaultReadResultLines   = 2000
)

type resultLimits struct {
	maxBytes int
	maxLines int
}

type truncationInfo struct {
	truncated     bool
	originalBytes int
	shownBytes    int
}

// truncateToolResult applies the generic result cap except when a read result
// already ends in its file-aware pagination notice. In that case, central
// clipping keeps complete numbered lines and updates that notice to the actual
// continuation offset instead of adding a second, generic truncation marker.
func truncateToolResult(toolName, s string, limits resultLimits) (string, truncationInfo) {
	if toolName == "read" {
		if out, info, ok := truncatePaginatedRead(s, limits); ok {
			return out, info
		}
	}
	return truncate(s, limits)
}

func (l resultLimits) withDefaults() resultLimits {
	if l.maxBytes <= 0 {
		l.maxBytes = defaultMaxResultBytes
	}
	if l.maxLines <= 0 {
		l.maxLines = defaultMaxResultLines
	}
	return l
}

// truncate caps s to the configured limits, appending a teaching marker that
// reports the original size and advises how to narrow. Both caps always hold:
// the line cap applies first, then the byte cap is re-applied to whatever
// remains so that many long lines cannot bypass the payload-size backstop. If
// neither cap triggers, s is returned unchanged.
func truncate(s string, limits resultLimits) (string, truncationInfo) {
	limits = limits.withDefaults()
	info := truncationInfo{
		originalBytes: len(s),
		shownBytes:    len(s),
	}
	totalLines := strings.Count(s, "\n")
	if !strings.HasSuffix(s, "\n") && len(s) > 0 {
		totalLines++
	}

	if totalLines > limits.maxLines {
		// Keep the first maxLines lines.
		idx := 0
		count := 0
		for count < limits.maxLines {
			nl := strings.IndexByte(s[idx:], '\n')
			if nl < 0 {
				idx = len(s)
				break
			}
			idx += nl + 1
			count++
		}
		kept := s[:idx]
		if !strings.HasSuffix(kept, "\n") {
			kept += "\n"
		}
		marker := fmt.Sprintf("[truncated: showing first %d of %d lines; use read offset/limit or a targeted shell command to narrow]", limits.maxLines, totalLines)
		// The byte cap is a payload-size backstop: re-apply it so that many
		// large lines under the line cap cannot bust the 64KB limit.
		out, byteTrunc := capBytes(kept+marker, len(s), limits.maxBytes)
		info.truncated = true
		info.shownBytes = len(out)
		if byteTrunc.truncated {
			info.shownBytes = byteTrunc.shownBytes
		}
		return out, info
	}

	out, byteTrunc := capBytes(s, len(s), limits.maxBytes)
	if byteTrunc.truncated {
		return out, byteTrunc
	}
	return out, info
}

// truncatePaginatedRead recognizes the stable notice emitted by readOneFile.
// When the central cap also fires, it drops only complete trailing file lines and
// rewrites the notice for the last line actually retained. ok is false only when
// the output is not a well-formed paginated read; callers then use the generic
// truncator.
func truncatePaginatedRead(s string, limits resultLimits) (out string, info truncationInfo, ok bool) {
	body, notice, ok := splitReadPaginationNotice(s)
	if !ok {
		return "", truncationInfo{}, false
	}
	limits = limits.withDefaults()
	info = truncationInfo{originalBytes: len(s), shownBytes: len(s)}
	totalOutputLines := strings.Count(s, "\n")
	if !strings.HasSuffix(s, "\n") && s != "" {
		totalOutputLines++
	}
	if len(s) <= limits.maxBytes && totalOutputLines <= limits.maxLines {
		return s, info, true
	}

	header, numberedBody := splitReadResultHeader(body)
	lines := strings.Split(numberedBody, "\n")
	if len(lines) == 0 {
		return "", truncationInfo{}, false
	}
	firstLine, valid := numberedReadResultLine(lines[0])
	if !valid {
		return "", truncationInfo{}, false
	}

	prefix := ""
	maxBodyLines := limits.maxLines - 1 // reserve one output line for the notice
	if header != "" {
		prefix = header + "\n"
		maxBodyLines--
	}
	if maxBodyLines <= 0 {
		return paginatedReadBeforeLine(header, firstLine, notice, limits, info)
	}
	if len(lines) > maxBodyLines {
		lines = lines[:maxBodyLines]
	}

	keptEnd := 0
	keptLines := 0
	lastLine := 0
	marker := ""
	for i, line := range lines {
		lineNumber, valid := numberedReadResultLine(line)
		if !valid || (lastLine > 0 && lineNumber != lastLine+1) {
			return "", truncationInfo{}, false
		}
		candidateEnd := keptEnd + len(line)
		if i > 0 {
			candidateEnd++ // newline between retained body lines
		}
		candidateMarker := notice.format(lineNumber)
		if len(prefix)+candidateEnd+1+len(candidateMarker) > limits.maxBytes {
			break
		}
		keptEnd = candidateEnd
		keptLines++
		lastLine = lineNumber
		marker = candidateMarker
	}
	if keptLines == 0 {
		return paginatedReadBeforeLine(header, firstLine, notice, limits, info)
	}

	out = prefix + numberedBody[:keptEnd] + "\n" + marker
	info.truncated = true
	info.shownBytes = len(out)
	return out, info, true
}

func paginatedReadBeforeLine(header string, nextLine int, notice readPaginationNotice, limits resultLimits, info truncationInfo) (string, truncationInfo, bool) {
	budget := min(limits.maxBytes, info.originalBytes)
	markers := []string{
		notice.formatBefore(nextLine),
		notice.formatCompactBefore(nextLine),
		fmt.Sprintf("[read offset=%d]", nextLine),
	}
	marker := markers[len(markers)-1]
	for _, candidate := range markers {
		if len(candidate) <= budget {
			marker = candidate
			break
		}
	}
	if len(marker) > budget {
		marker = marker[:budget]
	}

	out := marker
	if header != "" && limits.maxLines >= 2 {
		// Prefer retaining an explicitly requested digest with a more compact
		// continuation notice over dropping the digest for richer metadata.
		for _, candidate := range markers {
			withHeader := header + "\n" + candidate
			if len(withHeader) <= budget {
				out = withHeader
				break
			}
		}
	}
	info.truncated = true
	info.shownBytes = len(out)
	return out, info, true
}

func splitReadResultHeader(body string) (header, numberedBody string) {
	first, rest, found := strings.Cut(body, "\n")
	if !found || !isReadSHA256Header(first) {
		return "", body
	}
	return first, rest
}

func isReadSHA256Header(line string) bool {
	const prefix = "[sha256:"
	if !strings.HasPrefix(line, prefix) || !strings.HasSuffix(line, "]") {
		return false
	}
	digest := line[len(prefix) : len(line)-1]
	if len(digest) != 64 {
		return false
	}
	for _, c := range digest {
		if !('0' <= c && c <= '9') && !('a' <= c && c <= 'f') {
			return false
		}
	}
	return true
}

const readPaginationNoticePrefix = "[file truncated at line "

func splitReadPaginationNotice(s string) (string, readPaginationNotice, bool) {
	idx := strings.LastIndex(s, "\n"+readPaginationNoticePrefix)
	if idx < 0 {
		return "", readPaginationNotice{}, false
	}
	notice, ok := parseReadPaginationNotice(s[idx+1:])
	if !ok {
		return "", readPaginationNotice{}, false
	}
	return s[:idx], notice, true
}

func parseReadPaginationNotice(marker string) (readPaginationNotice, bool) {
	if !strings.HasPrefix(marker, readPaginationNoticePrefix) || !strings.HasSuffix(marker, "]") {
		return readPaginationNotice{}, false
	}
	rest := strings.TrimSuffix(strings.TrimPrefix(marker, readPaginationNoticePrefix), "]")
	linePart, rest, ok := strings.Cut(rest, "; file size ")
	if !ok {
		return readPaginationNotice{}, false
	}
	sizePart, offsetPart, ok := strings.Cut(rest, "; continue with offset=")
	if !ok {
		return readPaginationNotice{}, false
	}

	lastPart := linePart
	totalPart := ""
	if before, after, found := strings.Cut(linePart, " of "); found {
		lastPart, totalPart = before, after
	}
	lastLine, err := strconv.Atoi(lastPart)
	if err != nil || lastLine <= 0 {
		return readPaginationNotice{}, false
	}
	nextLine, err := strconv.Atoi(offsetPart)
	if err != nil || nextLine != lastLine+1 {
		return readPaginationNotice{}, false
	}
	fileSize := int64(0)
	fileSizeKnown := sizePart != "unknown"
	if fileSizeKnown {
		sizeBytes, found := strings.CutSuffix(sizePart, " bytes")
		if !found {
			return readPaginationNotice{}, false
		}
		fileSize, err = strconv.ParseInt(sizeBytes, 10, 64)
		if err != nil || fileSize < 0 {
			return readPaginationNotice{}, false
		}
	}
	totalLines := 0
	if totalPart != "" {
		totalLines, err = strconv.Atoi(totalPart)
		if err != nil || totalLines <= lastLine {
			return readPaginationNotice{}, false
		}
	}
	return readPaginationNotice{totalLines: totalLines, fileSize: fileSize, fileSizeKnown: fileSizeKnown}, true
}

func numberedReadResultLine(line string) (int, bool) {
	number, _, ok := strings.Cut(line, "\t")
	if !ok {
		return 0, false
	}
	lineNumber, err := strconv.Atoi(number)
	return lineNumber, err == nil && lineNumber > 0
}

// capBytes enforces maxBytes on s, appending a marker that reports the
// original byte size (origBytes) when it trims. If s already carries a
// line-truncation marker, capBytes trims the kept body, not the marker tail.
func capBytes(s string, origBytes, maxBytes int) (string, truncationInfo) {
	info := truncationInfo{
		originalBytes: origBytes,
		shownBytes:    len(s),
	}
	if len(s) <= maxBytes {
		return s, info
	}
	marker := fmt.Sprintf("\n[truncated: showing first %s of %s; use read offset/limit or a targeted shell command to narrow]", HumanBytes(maxBytes), HumanBytes(origBytes))
	keep := maxBytes - len(marker)
	if keep < 0 {
		keep = 0
	}
	out := s[:keep] + marker
	info.truncated = true
	info.shownBytes = len(out)
	return out, info
}

// HumanBytes renders a byte count as a short human-readable size: 2150 -> "2.1KB".
// Exported for the ui renderer, which formats the same truncation notices.
func HumanBytes(n int) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%dB", n)
	}
	div, exp := int64(unit), 0
	for v := int64(n) / unit; v >= unit; v /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f%cB", float64(n)/float64(div), "KMGTPE"[exp])
}
