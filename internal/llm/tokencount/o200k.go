package tokencount

import (
	"bufio"
	"bytes"
	_ "embed"
	"encoding/base64"
	"strconv"
	"strings"
	"sync"
	"unicode"
	"unicode/utf8"

	"harness/internal/llm"
)

const (
	chatMessageOverhead = 4
	chatBlockOverhead   = 2
	chatToolOverhead    = 8
	imageTokenEstimate  = 1600
)

//go:embed o200k_base.tiktoken
var o200kData []byte

var o200k struct {
	once  sync.Once
	ranks map[string]int
	err   error
}

// ShouldEstimateOpenAIChat reports whether the local OpenAI-compatible chat
// estimator is a reasonable fallback for providerName.
func ShouldEstimateOpenAIChat(providerName string) bool {
	name := strings.ToLower(strings.TrimSpace(providerName))
	switch name {
	case "openai", "openrouter":
		return true
	default:
		return strings.HasPrefix(name, "openai:") ||
			strings.HasPrefix(name, "openrouter:") ||
			strings.HasPrefix(name, "openai-codex:")
	}
}

// EstimateOpenAIChat returns a conservative o200k_base estimate for an
// OpenAI-compatible chat request, including text/tool bytes and coarse chat
// framing overhead. It returns 0 only if the embedded encoding cannot be parsed.
func EstimateOpenAIChat(req llm.Request) int {
	enc, err := O200KBase()
	if err != nil {
		return 0
	}
	total := enc.CountText(req.System)
	for _, t := range req.Tools {
		total += chatToolOverhead
		total += enc.CountText(t.Name)
		total += enc.CountText(t.Description)
		total += enc.CountText(string(t.Parameters))
	}
	for _, t := range req.ServerTools {
		total += chatToolOverhead
		total += enc.CountText(t.Name)
		total += enc.CountText(t.Kind)
		total += enc.CountText(string(t.Parameters))
	}
	for _, m := range req.Messages {
		total += chatMessageOverhead
		total += enc.CountText(string(m.Role))
		for _, b := range m.Content {
			total += chatBlockOverhead
			switch b.Kind {
			case llm.BlockImage:
				total += imageTokenEstimate
				total += enc.CountText(b.ImageMediaType)
				total += enc.CountText(b.ImageDetail)
				total += enc.CountText(b.ImageName)
			default:
				total += enc.CountText(string(b.Kind))
				total += enc.CountText(b.Text)
				total += enc.CountText(b.ToolUseID)
				total += enc.CountText(b.ToolName)
				total += enc.CountText(string(b.ToolInput))
				total += enc.CountText(b.ResultForID)
				total += enc.CountText(b.ResultText)
				total += enc.CountText(b.ReasoningID)
				total += enc.CountText(b.ReasoningEncrypted)
				total += enc.CountText(b.RedactedData)
				total += enc.CountText(b.ThinkingSignature)
			}
		}
	}
	total += enc.CountText(llm.RequestContextText(req.RequestContext))
	return total
}

type Encoding struct {
	ranks map[string]int
}

func O200KBase() (*Encoding, error) {
	o200k.once.Do(func() {
		o200k.ranks, o200k.err = parseRanks(o200kData)
	})
	if o200k.err != nil {
		return nil, o200k.err
	}
	return &Encoding{ranks: o200k.ranks}, nil
}

func parseRanks(data []byte) (map[string]int, error) {
	ranks := make(map[string]int, 200_000)
	sc := bufio.NewScanner(bytes.NewReader(data))
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		token64, rankText, ok := strings.Cut(line, " ")
		if !ok {
			continue
		}
		token, err := base64.StdEncoding.DecodeString(token64)
		if err != nil {
			return nil, err
		}
		rank, err := strconv.Atoi(rankText)
		if err != nil {
			return nil, err
		}
		ranks[string(token)] = rank
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	return ranks, nil
}

func (e *Encoding) CountText(s string) int {
	if s == "" {
		return 0
	}
	total := 0
	for _, piece := range splitPieces(s) {
		total += e.countPiece(piece)
	}
	return total
}

func (e *Encoding) countPiece(piece string) int {
	if piece == "" {
		return 0
	}
	raw := []byte(piece)
	if _, ok := e.ranks[string(raw)]; ok {
		return 1
	}
	parts := make([][]byte, 0, len(raw))
	for _, b := range raw {
		parts = append(parts, []byte{b})
	}
	for len(parts) > 1 {
		bestRank, best := -1, -1
		for i := 0; i < len(parts)-1; i++ {
			rank, ok := e.pairRank(parts[i], parts[i+1])
			if !ok {
				continue
			}
			if bestRank < 0 || rank < bestRank {
				bestRank, best = rank, i
			}
		}
		if best < 0 {
			break
		}
		merged := append(append([]byte{}, parts[best]...), parts[best+1]...)
		parts[best] = merged
		copy(parts[best+1:], parts[best+2:])
		parts = parts[:len(parts)-1]
	}
	return len(parts)
}

func (e *Encoding) pairRank(a, b []byte) (int, bool) {
	merged := make([]byte, 0, len(a)+len(b))
	merged = append(merged, a...)
	merged = append(merged, b...)
	rank, ok := e.ranks[string(merged)]
	return rank, ok
}

func splitPieces(s string) []string {
	var out []string
	for i := 0; i < len(s); {
		r, size := utf8.DecodeRuneInString(s[i:])
		if r == utf8.RuneError && size == 1 {
			out = append(out, s[i:i+1])
			i++
			continue
		}
		start := i
		switch {
		case unicode.IsSpace(r):
			i += size
			for i < len(s) {
				next, n := utf8.DecodeRuneInString(s[i:])
				if !unicode.IsSpace(next) {
					break
				}
				i += n
			}
			if i < len(s) {
				next, n := utf8.DecodeRuneInString(s[i:])
				if isWordRune(next) {
					i += n
					for i < len(s) {
						next, n = utf8.DecodeRuneInString(s[i:])
						if !isWordRune(next) {
							break
						}
						i += n
					}
				}
			}
		case isWordRune(r):
			i += size
			for i < len(s) {
				next, n := utf8.DecodeRuneInString(s[i:])
				if !isWordRune(next) {
					break
				}
				i += n
			}
		case unicode.IsDigit(r):
			for n := 0; n < 3 && i < len(s); n++ {
				next, size := utf8.DecodeRuneInString(s[i:])
				if !unicode.IsDigit(next) {
					break
				}
				i += size
			}
		default:
			i += size
			for i < len(s) {
				next, n := utf8.DecodeRuneInString(s[i:])
				if unicode.IsSpace(next) || isWordRune(next) || unicode.IsDigit(next) {
					break
				}
				i += n
			}
		}
		out = append(out, s[start:i])
	}
	return out
}

func isWordRune(r rune) bool {
	return unicode.IsLetter(r) || unicode.IsMark(r) || r == '\''
}
