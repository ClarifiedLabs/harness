package subscription

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestKimiFixture(t *testing.T) {
	a := account(t, "kimi-for-coding", func(*http.Request) (*http.Response, error) { return response(200, fixture(t, "kimi")), nil })
	got, err := a.Status(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Pools) != 1 || len(got.Pools[0].Windows) != 4 {
		t.Fatalf("pools %+v", got.Pools)
	}
	windows := got.Pools[0].Windows
	if *windows[0].Used != 75 || *windows[0].Remaining != 25 || *windows[0].UsedPercent != 75 || windows[0].ResetAt.Format(time.RFC3339Nano) != "2026-09-17T05:24:18.443553353Z" || windows[0].Unit == "tokens" {
		t.Fatalf("weekly %+v", windows[0])
	}
	if windows[1].Name != "Short quota" || *windows[1].DurationSeconds != 18000 || *windows[1].Used != 0 || *windows[1].ResetAfterSeconds != 0 {
		t.Fatalf("short %+v", windows[1])
	}
	if windows[2].Limit != nil || windows[2].Used != nil || windows[2].UsedPercent != nil || *windows[2].Remaining != 0 {
		t.Fatalf("missing data became zero %+v", windows[2])
	}
	if *windows[3].Limit != 0 || windows[3].UsedPercent != nil || *windows[3].ResetAfterSeconds != 0 {
		t.Fatalf("zero limit %+v", windows[3])
	}
}

func TestKimiPrefixedDurationUnits(t *testing.T) {
	for unit, multiplier := range map[string]int64{"TIME_UNIT_SECOND": 1, "TIME_UNIT_MINUTE": 60, "TIME_UNIT_HOUR": 3600, "TIME_UNIT_DAY": 86400, "TIME_UNIT_WEEK": 604800} {
		body := `{"limits":[{"window":{"duration":300,"timeUnit":"` + unit + `"},"detail":{"limit":100,"remaining":84}}]}`
		a := account(t, "kimi-for-coding", func(*http.Request) (*http.Response, error) { return response(200, body), nil })
		report, err := a.Status(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		seconds := report.Pools[0].Windows[0].DurationSeconds
		if seconds == nil || *seconds != 300*multiplier || len(report.Warnings) != 0 {
			t.Fatalf("%s: report %+v", unit, report)
		}
	}
}

func TestKimiAliasesAndInconsistency(t *testing.T) {
	for _, alias := range []string{"reset_at", "resetAt", "reset_time", "resetTime"} {
		body := `{"usage":{"limit":"10","used":"3","` + alias + `":"2026-09-17T01:02:03.123456789+01:00"}}`
		a := account(t, "kimi-for-coding", func(*http.Request) (*http.Response, error) { return response(200, body), nil })
		got, err := a.Status(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		w := got.Pools[0].Windows[0]
		if *w.Remaining != 7 || w.ResetAt.UTC().Format(time.RFC3339Nano) != "2026-09-17T00:02:03.123456789Z" {
			t.Fatalf("alias %s %+v", alias, w)
		}
	}
	for _, alias := range []string{"reset_in", "resetIn", "ttl", "window"} {
		body := `{"usage":{"used":0,"` + alias + `":"42"}}`
		a := account(t, "kimi-for-coding", func(*http.Request) (*http.Response, error) { return response(200, body), nil })
		got, err := a.Status(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if *got.Pools[0].Windows[0].ResetAfterSeconds != 42 {
			t.Fatal("relative reset")
		}
	}
	a := account(t, "kimi-for-coding", func(*http.Request) (*http.Response, error) {
		return response(200, `{"limits":[{"limit":10,"used":2,"remaining":5,"duration":2,"timeUnit":"HOUR"}]}`), nil
	})
	got, err := a.Status(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	w := got.Pools[0].Windows[0]
	if *w.Used != 2 || *w.Remaining != 5 || w.UsedPercent != nil || *w.DurationSeconds != 7200 || len(got.Warnings) != 1 {
		t.Fatalf("inconsistent %+v warnings %+v", w, got.Warnings)
	}
}

func TestZaiFixture(t *testing.T) {
	a := account(t, "zai-coding-plan", func(*http.Request) (*http.Response, error) { return response(200, fixture(t, "zai")), nil })
	got, err := a.Status(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got.Plan != "Pro" || len(got.Pools) != 4 {
		t.Fatalf("got %+v", got)
	}
	token, credit, mcp, unknown := got.Pools[0].Windows[0], got.Pools[1].Windows[0], got.Pools[2].Windows[0], got.Pools[3].Windows[0]
	if *token.UsedPercent != 0 || *token.Used != 25 || *token.Remaining != 50 || *token.DurationSeconds != 18000 || token.ResetAt.UnixMilli() != 1789606800123 {
		t.Fatalf("token %+v", token)
	}
	if credit.UsedPercent != nil || credit.Used != nil || *credit.Remaining != 0 || *credit.DurationSeconds != 604800 || credit.Unit != "quota credits" {
		t.Fatalf("credit %+v", credit)
	}
	if mcp.Name != "Monthly MCP quota" || mcp.DurationSeconds != nil || mcp.Unit != "calls" || mcp.ResetAt != nil {
		t.Fatalf("MCP %+v", mcp)
	}
	if unknown.DurationSeconds != nil || *unknown.UsedPercent != 10 || unknown.Unit != "quota units" {
		t.Fatalf("unknown %+v", unknown)
	}
	found := map[string]bool{}
	for _, w := range got.Warnings {
		found[w.Code] = true
	}
	if !found["implausible_reset"] || !found["inconsistent_counts"] || !found["unknown_quota_type"] || !found["unknown_quota_unit"] {
		t.Fatalf("warnings %+v", got.Warnings)
	}
}

func TestZaiPeriodsAndBusinessErrors(t *testing.T) {
	for unit, want := range map[int64]int64{1: 86400, 3: 3600, 5: 60, 6: 604800} {
		b, _ := json.Marshal(map[string]any{"success": true, "code": 200, "data": map[string]any{"limits": []any{map[string]any{"type": "CREDIT_LIMIT", "unit": unit, "number": 2, "currentValue": 0}}}})
		a := account(t, "zai-coding-plan", func(*http.Request) (*http.Response, error) { return response(200, string(b)), nil })
		got, err := a.Status(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if *got.Pools[0].Windows[0].DurationSeconds != 2*want {
			t.Fatal("duration")
		}
	}
	for _, body := range []string{`{"success":false,"code":200,"msg":"secret"}`, `{"success":true,"code":401,"msg":"secret"}`} {
		a := account(t, "zai-coding-plan", func(*http.Request) (*http.Response, error) { return response(200, body), nil })
		_, err := a.Status(context.Background())
		limitsError(t, err, "upstream_error")
	}
}

func TestCodexFixture(t *testing.T) {
	a := account(t, "openai-codex", func(*http.Request) (*http.Response, error) { return response(200, fixture(t, "codex")), nil })
	got, err := a.Status(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got.Plan != "plus" || len(got.Pools) != 2 || got.ResetCredits == nil || *got.ResetCredits.AvailableCount != 0 {
		t.Fatalf("got %+v", got)
	}
	primary, extra := got.Pools[0], got.Pools[1]
	if !*primary.Allowed || *primary.LimitReached || *primary.Windows[0].UsedPercent != 100 || len(primary.Windows) != 1 || primary.Windows[0].ResetAt.Unix() != 1789606800 || *primary.Windows[0].ResetAfterSeconds != 0 {
		t.Fatalf("primary %+v", primary)
	}
	if *extra.Allowed || !*extra.LimitReached || *extra.Windows[0].UsedPercent != 0 || extra.Name != "Review" {
		t.Fatalf("extra %+v", extra)
	}
	b, _ := json.Marshal(got)
	if strings.Contains(string(b), "private") || strings.Contains(string(b), "account_id") {
		t.Fatalf("metadata leaked %s", b)
	}
	for _, body := range []string{`{"plan_type":"plus","rate_limit":null}`, `{"rate_limit":{"allowed":false,"primary_window":null,"secondary_window":null}}`, `{"rate_limit_reset_credits":{"available_count":0}}`} {
		a := account(t, "openai-codex", func(*http.Request) (*http.Response, error) { return response(200, body), nil })
		got, err := a.Status(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		for _, pool := range got.Pools {
			if len(pool.Windows) != 0 {
				t.Fatal("fabricated windows")
			}
		}
	}
}

func TestResetCreditsFixture(t *testing.T) {
	calls := 0
	a := account(t, "openai-codex", func(r *http.Request) (*http.Response, error) {
		calls++
		if r.Method != "GET" || r.URL.String() != codexBase+"/rate-limit-reset-credits" {
			t.Fatal("wrong credit route")
		}
		return response(200, fixture(t, "credits")), nil
	})
	got, err := a.ResetCredits(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if calls != 1 || *got.AvailableCount != 1 || len(got.Credits) != 6 || got.Provider != "openai-codex" || !got.FetchedAt.Equal(testNow) {
		t.Fatalf("credits %+v", got)
	}
	for i, credit := range got.Credits {
		if credit.Redeemable == nil || *credit.Redeemable != (i == 0) {
			t.Fatalf("eligibility %+v", credit)
		}
	}
	if got.Credits[0].Title != "Full reset" || got.Credits[0].Description != "Readynow" || got.Credits[0].GrantedAt == nil {
		t.Fatalf("credit %+v", got.Credits[0])
	}
	b, _ := json.Marshal(got)
	if strings.Contains(string(b), "private") {
		t.Fatal("credit metadata leaked")
	}
	a = account(t, "openai-codex", func(*http.Request) (*http.Response, error) {
		return response(200, `{"credits":[],"available_count":0}`), nil
	})
	got, err = a.ResetCredits(context.Background())
	if err != nil || got.Credits == nil || *got.AvailableCount != 0 {
		t.Fatal("empty credits")
	}
}

func TestProviderShapeValidation(t *testing.T) {
	cases := map[string][]string{
		"kimi-for-coding": {`{}`, `{"usage":{}}`, `{"usage":[]}`, `{"usage":{"used":true}}`, `{"usage":{"used":"NaN"}}`, `{"usage":{"used":-1}}`, `{"usage":{"used":0,"resetTime":"bad"}}`, `{"limits":[1]}`, `{"limits":[{"used":0,"detail":[]}]}`, `{"limits":[{"used":0,"window":[]}]}`, `{"usage":{"used":0,"duration":1.5,"timeUnit":"HOUR"}}`},
		"zai-coding-plan": {`{}`, `{"data":{"limits":[]}}`, `{"data":[]}`, `{"data":{"limits":[1]}}`, `{"data":{"limits":[{"type":"TOKENS_LIMIT"}]}}`, `{"data":{"limits":[{"type":"TOKENS_LIMIT","percentage":"0"}]}}`, `{"data":{"limits":[{"type":"TIME_LIMIT","percentage":0,"usageDetails":{}}]}}`, `{"data":{"limits":[{"type":"TIME_LIMIT","percentage":0,"nextResetTime":-1}]}}`},
		"openai-codex":    {`{}`, `{"rate_limit":{}}`, `{"additional_rate_limits":[{"rate_limit":{}}]}`, `{"rate_limit":[]}`, `{"rate_limit":{"allowed":"true"}}`, `{"rate_limit":{"primary_window":[]}}`, `{"rate_limit":{"primary_window":{}}}`, `{"rate_limit":{"primary_window":{"used_percent":-1}}}`, `{"rate_limit":{"primary_window":{"reset_at":99999999999999}}}`, `{"additional_rate_limits":[{}]}`, `{"rate_limit_reset_credits":{"available_count":-1}}`},
	}
	for provider, bodies := range cases {
		for _, body := range bodies {
			t.Run(provider+body, func(t *testing.T) {
				a := account(t, provider, func(*http.Request) (*http.Response, error) { return response(200, body), nil })
				_, err := a.Status(context.Background())
				limitsError(t, err, "invalid_payload")
			})
		}
	}
	for _, body := range []string{`{}`, `{"credits":null}`, `{"credits":{}}`, `{"credits":[{}]}`, `{"credits":[],"available_count":-1}`, `{"credits":[{"id":"unsafe id","status":"available","reset_type":"codex_rate_limits"}]}`, `{"credits":[{"id":"safe","status":"available","reset_type":"codex_rate_limits","expires_at":"bad"}]}`} {
		a := account(t, "openai-codex", func(*http.Request) (*http.Response, error) { return response(200, body), nil })
		_, err := a.ResetCredits(context.Background())
		limitsError(t, err, "invalid_payload")
	}
}
