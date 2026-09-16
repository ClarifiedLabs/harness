package protocol

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

// LimitsReport is account-wide subscription quota, never token/cost accounting.
type LimitsReport struct {
	Providers []ProviderLimits `json:"providers"`
}

type ProviderLimits struct {
	Provider     string              `json:"provider"`
	FetchedAt    time.Time           `json:"fetched_at"`
	Plan         string              `json:"plan,omitempty"`
	Pools        []LimitPool         `json:"pools,omitempty"`
	ResetCredits *ResetCreditSummary `json:"reset_credits,omitempty"`
	Error        *LimitsError        `json:"error,omitempty"`
	Warnings     []LimitsError       `json:"warnings,omitempty"`
}

// LimitPool keeps independent quotas and authoritative admission flags separate.
type LimitPool struct {
	ID           string        `json:"id"`
	Name         string        `json:"name"`
	Allowed      *bool         `json:"allowed,omitempty"`
	LimitReached *bool         `json:"limit_reached,omitempty"`
	Windows      []LimitWindow `json:"windows,omitempty"`
}

// Pointers preserve explicit zero/false versus absent provider data. Quota units
// are provider-defined and must not be treated as model token usage.
type LimitWindow struct {
	ID                string     `json:"id"`
	Name              string     `json:"name"`
	Unit              string     `json:"unit,omitempty"`
	Limit             *float64   `json:"limit,omitempty"`
	Used              *float64   `json:"used,omitempty"`
	Remaining         *float64   `json:"remaining,omitempty"`
	UsedPercent       *float64   `json:"used_percent,omitempty"`
	RemainingPercent  *float64   `json:"remaining_percent,omitempty"`
	DurationSeconds   *int64     `json:"duration_seconds,omitempty"`
	ResetAt           *time.Time `json:"reset_at,omitempty"`
	ResetAfterSeconds *int64     `json:"reset_after_seconds,omitempty"`
}

type ResetCreditSummary struct {
	AvailableCount *int64 `json:"available_count,omitempty"`
}

type ResetCredits struct {
	Provider       string        `json:"provider"`
	FetchedAt      time.Time     `json:"fetched_at"`
	AvailableCount *int64        `json:"available_count,omitempty"`
	Credits        []ResetCredit `json:"credits"`
	Error          *LimitsError  `json:"error,omitempty"`
	Warnings       []LimitsError `json:"warnings,omitempty"`
}

type ResetCredit struct {
	ID          string     `json:"id"`
	ResetType   string     `json:"reset_type"`
	Status      string     `json:"status"`
	GrantedAt   *time.Time `json:"granted_at,omitempty"`
	ExpiresAt   *time.Time `json:"expires_at,omitempty"`
	Title       string     `json:"title,omitempty"`
	Description string     `json:"description,omitempty"`
	Redeemable  *bool      `json:"redeemable,omitempty"`
}

type ResetRequest struct {
	Provider  string `json:"provider"`
	CreditID  string `json:"credit_id"`
	RequestID string `json:"request_id"`
}

type ResetResult struct {
	Provider  string    `json:"provider"`
	CreditID  string    `json:"credit_id"`
	RequestID string    `json:"request_id"`
	FetchedAt time.Time `json:"fetched_at"`
	// Outcome is reset, nothing_to_reset, no_credit, already_redeemed, or
	// indeterminate when a sent operation may have consumed the credit.
	Outcome      string          `json:"outcome"`
	WindowsReset *int64          `json:"windows_reset,omitempty"`
	Status       *ProviderLimits `json:"status,omitempty"`
	Credits      *ResetCredits   `json:"credits,omitempty"`
	Error        *LimitsError    `json:"error,omitempty"`
	Warnings     []LimitsError   `json:"warnings,omitempty"`
}

// LimitsError contains only bounded, safe messages; never an upstream body,
// credential, URL, or response header. Indeterminate forbids a fresh-ID retry.
type LimitsError struct {
	Code          string `json:"code"`
	Message       string `json:"message"`
	Indeterminate bool   `json:"indeterminate,omitempty"`
}

func (e *LimitsError) Error() string { return e.Message }

func NewResetRequestID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("generate reset request ID: %w", err)
	}
	return hex.EncodeToString(b[:]), nil
}

// ValidLimitsID accepts opaque IDs that can safely be printed in retry commands.
func ValidLimitsID(s string) bool {
	if len(s) == 0 || len(s) > 128 {
		return false
	}
	for _, r := range s {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || strings.ContainsRune("._:-", r)) {
			return false
		}
	}
	return s[0] != '-'
}

func (r ResetRequest) Validate() error {
	if r.Provider != "openai-codex" {
		return &LimitsError{Code: "unsupported_provider", Message: "reset credits are supported only for openai-codex"}
	}
	if !ValidLimitsID(r.CreditID) || !ValidLimitsID(r.RequestID) {
		return &LimitsError{Code: "invalid_request", Message: "credit_id and request_id must be 1–128 safe identifier characters"}
	}
	return nil
}

// LimitsLabel converts provider-controlled display text to bounded plain text.
// Drop escape sequences as well as control/format characters, including bidi.
func LimitsLabel(s string) string {
	var out strings.Builder
	n := 0
	for i := 0; i < len(s); {
		if s[i] == 0x1b {
			i++
			if i < len(s) && s[i] == '[' {
				i++
				for i < len(s) {
					c := s[i]
					i++
					if c >= 0x40 && c <= 0x7e {
						break
					}
				}
			} else if i < len(s) && s[i] == ']' {
				i++
				for i < len(s) {
					if s[i] == 7 {
						i++
						break
					}
					if s[i] == 0x1b && i+1 < len(s) && s[i+1] == '\\' {
						i += 2
						break
					}
					i++
				}
			}
			continue
		}
		r, size := utf8.DecodeRuneInString(s[i:])
		i += size
		if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) || r == unicode.ReplacementChar {
			continue
		}
		if n == 200 {
			break
		}
		out.WriteRune(r)
		n++
	}
	return strings.TrimSpace(out.String())
}
