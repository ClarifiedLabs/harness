package tools

import (
	"fmt"
	"strings"
)

// Central output caps, applied in Dispatch as a backstop for every tool by
// default. A Registry can override these limits.
const (
	defaultMaxResultBytes = 64 * 1024
	defaultMaxResultLines = 1000
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
		marker := fmt.Sprintf("[truncated: showing first %d of %d lines; use read_file offset/limit or grep to narrow]", limits.maxLines, totalLines)
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
	marker := fmt.Sprintf("\n[truncated: showing first %s of %s; use read_file offset/limit or grep to narrow]", HumanBytes(maxBytes), HumanBytes(origBytes))
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
