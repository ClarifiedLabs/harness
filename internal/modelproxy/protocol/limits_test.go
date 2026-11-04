package protocol

import (
	"encoding/json"
	"harness/internal/modelproxy"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestLimitsPresenceRoundTrip(t *testing.T) {
	zero, no, count := 0.0, false, int64(0)
	now := time.Date(2026, 9, 16, 0, 0, 0, 123456789, time.UTC)
	input := modelproxy.ProviderLimits{Provider: "openai-codex", FetchedAt: now, Pools: []modelproxy.LimitPool{
		{ID: "primary", Name: "Coding", Allowed: &no, LimitReached: &no, Windows: []modelproxy.LimitWindow{{ID: "zero", UsedPercent: &zero, Limit: &zero, ResetAfterSeconds: &count, ResetAt: &now}, {ID: "unknown"}}},
	}, ResetCredits: &modelproxy.ResetCreditSummary{AvailableCount: &count}}
	data, err := json.Marshal(input)
	if err != nil {
		t.Fatal(err)
	}
	var got modelproxy.ProviderLimits
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(input, got) {
		t.Fatalf("round trip: %s", data)
	}
	if !strings.Contains(string(data), `"allowed":false`) || !strings.Contains(string(data), `"used_percent":0`) {
		t.Fatalf("presence lost: %s", data)
	}
}

func TestResetContractRoundTrip(t *testing.T) {
	no := false
	input := modelproxy.ResetResult{Provider: "openai-codex", CreditID: "credit-1", RequestID: "req-1", Outcome: "indeterminate", Error: &modelproxy.LimitsError{Code: "timeout", Message: "request timed out", Indeterminate: true}, Warnings: []modelproxy.LimitsError{{Code: "refresh_failed", Message: "refresh failed"}}, Credits: &modelproxy.ResetCredits{Credits: []modelproxy.ResetCredit{{ID: "credit-1", Status: "future", Redeemable: &no}}}}
	data, err := json.Marshal(input)
	if err != nil {
		t.Fatal(err)
	}
	var got modelproxy.ResetResult
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(input, got) {
		t.Fatalf("roundtrip: %s", data)
	}
}

func TestLimitsLabelsAndIdentifiers(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"\x1b[31mCoding\x1b[0m\nPlan\u202e", "CodingPlan"},
		{"\x1b]8;;https://secret\aLink\x1b]8;;\x1b\\", "Link"},
		{"中文\xffx", "中文x"},
		{strings.Repeat("é", 201), strings.Repeat("é", 200)},
	} {
		if got := modelproxy.LimitsLabel(tc.in); got != tc.want {
			t.Errorf("label %q, want %q", got, tc.want)
		}
	}
	for _, s := range []string{"", "-flag", "with space", "x;echo", "\x1b", strings.Repeat("a", 129)} {
		if modelproxy.ValidLimitsID(s) {
			t.Errorf("unsafe identifier %q", s)
		}
	}
	a, err := modelproxy.NewResetRequestID()
	if err != nil {
		t.Fatal(err)
	}
	b, err := modelproxy.NewResetRequestID()
	if err != nil {
		t.Fatal(err)
	}
	if a == b || !modelproxy.ValidLimitsID(a) {
		t.Fatal("invalid random identifiers")
	}
	if err := (modelproxy.ResetRequest{Provider: "openai-codex", CreditID: "credit-1", RequestID: a}).Validate(); err != nil {
		t.Fatal(err)
	}
}
