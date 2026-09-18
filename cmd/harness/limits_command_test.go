package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"harness/internal/modelproxy/protocol"
	"harness/internal/ui"
)

func TestLimitsJSONEscapesTerminalControls(t *testing.T) {
	value := protocol.LimitsReport{Providers: []protocol.ProviderLimits{{Provider: "openai-codex", Plan: "\u009b31mred\u009dhyperlink"}}}
	var out bytes.Buffer
	if err := writeLimitsJSON(&out, value); err != nil {
		t.Fatal(err)
	}
	if strings.ContainsAny(out.String(), "\u009b\u009d") || !strings.Contains(out.String(), `\u009b`) {
		t.Fatal(out.String())
	}
	var got protocol.LimitsReport
	if err := json.Unmarshal(out.Bytes(), &got); err != nil || got.Providers[0].Plan != value.Providers[0].Plan {
		t.Fatalf("got=%+v err=%v", got, err)
	}
}

func TestLimitsCLIFormatsAndNoModelStartup(t *testing.T) {
	for _, format := range []string{"text", "json"} {
		t.Run(format, func(t *testing.T) {
			var calls atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				if r.URL.Path != "/v1/limits" || r.URL.Query().Get("provider") != "kimi-code-plan-cn" {
					t.Errorf("unexpected startup request: %s", r.URL)
				}
				fmt.Fprint(w, `{"providers":[{"provider":"kimi-code-plan-cn","plan":"coding"}]}`)
			}))
			defer srv.Close()
			state := t.TempDir()
			env, out, stderr := configCommandEnv(t, []string{"limits", "kimi-code-plan-cn", "-format", format, "-model-proxy-url", srv.URL}, map[string]string{"XDG_STATE_HOME": state})
			if code := run(env); code != 0 || calls.Load() != 1 || stderr.Len() != 0 {
				t.Fatalf("code=%d calls=%d out=%s err=%s", code, calls.Load(), out, stderr)
			}
			if !strings.Contains(out.String(), "kimi-code-plan-cn") {
				t.Fatal(out.String())
			}
			if format == "json" {
				var report protocol.LimitsReport
				if json.Unmarshal(out.Bytes(), &report) != nil || len(report.Providers) != 1 {
					t.Fatal(out.String())
				}
			}
			entries, err := os.ReadDir(state)
			if err != nil || len(entries) != 0 {
				t.Fatalf("session state created: %v %v", entries, err)
			}
		})
	}
}

func TestLimitsCLIResetOutcomesAndRequestIdentity(t *testing.T) {
	for _, tc := range []struct {
		outcome string
		code    int
	}{{"reset", 0}, {"already_redeemed", 0}, {"nothing_to_reset", 0}, {"no_credit", 1}, {"future_outcome", 1}, {"indeterminate", 1}} {
		for _, format := range []string{"text", "json"} {
			t.Run(tc.outcome+"/"+format, func(t *testing.T) {
				var calls atomic.Int32
				srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					calls.Add(1)
					var request protocol.ResetRequest
					_ = json.NewDecoder(r.Body).Decode(&request)
					if r.Method != "POST" || r.URL.Path != "/v1/limits/reset" || request.Provider != "openai-codex" || request.CreditID != "credit" || request.RequestID != "stable" {
						t.Errorf("request=%s %+v", r.URL, request)
					}
					_ = json.NewEncoder(w).Encode(protocol.ResetResult{Provider: request.Provider, CreditID: request.CreditID, RequestID: request.RequestID, Outcome: tc.outcome, Warnings: []protocol.LimitsError{{Code: "refresh", Message: "refresh failed"}}})
				}))
				defer srv.Close()
				env, out, stderr := configCommandEnv(t, []string{"limits", "reset", "openai-codex", "credit", "-request-id", "stable", "-format", format, "-model-proxy-url", srv.URL}, nil)
				if code := run(env); code != tc.code || calls.Load() != 1 {
					t.Fatalf("code=%d calls=%d out=%s err=%s", code, calls.Load(), out, stderr)
				}
				if !strings.Contains(out.String(), tc.outcome) || !strings.Contains(out.String(), "refresh failed") {
					t.Fatal(out.String())
				}
				if tc.outcome == "future_outcome" || tc.outcome == "indeterminate" {
					both := out.String() + stderr.String()
					if !strings.Contains(both, "harness limits reset openai-codex credit -request-id stable") || !strings.Contains(both, "/limits reset openai-codex credit stable") {
						t.Fatal(both)
					}
				}
				if format == "json" {
					var result protocol.ResetResult
					if json.Unmarshal(out.Bytes(), &result) != nil || result.RequestID != "stable" || result.Outcome != tc.outcome {
						t.Fatal(out.String())
					}
				}
			})
		}
	}
}

func TestLimitsCLIParsingHelpAndInvalidSyntax(t *testing.T) {
	for _, args := range [][]string{{"limits", "--help"}, {"limits", "reset", "--help"}, {"limits", "resets", "--help"}} {
		env, out, stderr := configCommandEnv(t, args, nil)
		if code := run(env); code != 0 || !strings.Contains(out.String(), "model-proxy-url") {
			t.Fatalf("code=%d out=%s err=%s", code, out, stderr)
		}
	}
	for _, args := range [][]string{{"limits", "-model-proxy-api-key"}, {"limits", "reset", "openai-codex", "credit", "-model-proxy-api-key"}, {"limits", "-format", "yaml"}, {"limits", "a", "b"}, {"limits", "reset"}, {"limits", "reset", "openai-codex", "credit", "-request-id", ""}, {"limits", "reset", "openai-codex", "bad/id"}, {"limits", "resets", "openai-codex", "-request-id", "id"}, {"limits", "reset", "openai-codex", "credit", "-unknown", "x"}} {
		env, _, stderr := configCommandEnv(t, args, nil)
		if code := run(env); code != ui.ExitUsage {
			t.Errorf("args=%v code=%d err=%s", args, code, stderr)
		}
	}
}

func TestLimitsCLIFlagsBeforeSubcommand(t *testing.T) {
	for _, action := range []string{"reset", "resets"} {
		t.Run(action, func(t *testing.T) {
			var calls atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				if action == "resets" {
					if r.Method != "GET" || r.URL.Path != "/v1/limits/reset-credits" {
						t.Error(r.Method, r.URL.Path)
					}
					fmt.Fprint(w, `{"provider":"openai-codex","credits":[]}`)
				} else {
					if r.Method != "POST" || r.URL.Path != "/v1/limits/reset" {
						t.Error(r.Method, r.URL.Path)
					}
					var req protocol.ResetRequest
					if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
						t.Error(err)
					}
					if req.CreditID != "credit" || req.RequestID != "stable" {
						t.Errorf("request %+v", req)
					}
					_ = json.NewEncoder(w).Encode(protocol.ResetResult{Provider: req.Provider, CreditID: req.CreditID, RequestID: req.RequestID, Outcome: "nothing_to_reset"})
				}
			}))
			defer srv.Close()
			args := []string{"limits", "-format", "json", "-model-proxy-url", srv.URL, action, "openai-codex"}
			if action == "reset" {
				args = append(args, "credit", "-request-id", "stable")
			}
			env, out, stderr := configCommandEnv(t, args, nil)
			if code := run(env); code != 0 || calls.Load() != 1 || !json.Valid(out.Bytes()) {
				t.Fatalf("code=%d calls=%d out=%s err=%s", code, calls.Load(), out, stderr)
			}
		})
	}
}

func TestLimitsCLIProxyConfigurationPrecedence(t *testing.T) {
	for _, level := range []string{"config", "env", "flag"} {
		t.Run(level, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("Authorization") != "Bearer "+level {
					t.Errorf("auth=%q", r.Header.Get("Authorization"))
				}
				fmt.Fprint(w, `{"providers":[]}`)
			}))
			defer srv.Close()
			fileURL, fileKey := "http://127.0.0.1:1", "config"
			if level == "config" {
				fileURL = srv.URL
			}
			path := writeMainConfig(t, fmt.Sprintf(`{"model_proxy_url":%q,"model_proxy_api_key":%q}`, fileURL, fileKey))
			values := map[string]string{}
			args := []string{"limits", "-config", path}
			if level != "config" {
				values["HARNESS_MODEL_PROXY_URL"] = srv.URL
				values["HARNESS_MODEL_PROXY_API_KEY"] = "env"
			}
			if level == "flag" {
				values["HARNESS_MODEL_PROXY_URL"] = "http://127.0.0.1:1"
				args = append(args, "-model-proxy-url", srv.URL, "-model-proxy-api-key", "flag")
			}
			env, _, stderr := configCommandEnv(t, args, values)
			if code := run(env); code != 0 {
				t.Fatalf("code=%d err=%s", code, stderr)
			}
		})
	}
}

func TestLimitsCLIPartialFailureAndOldProxy(t *testing.T) {
	for _, tc := range []struct {
		name, body, want string
		status           int
	}{{"partial", `{"providers":[{"provider":"kimi-code-plan-cn"},{"provider":"openai-codex","error":{"code":"timeout","message":"unavailable"}}]}`, "kimi-code-plan-cn", 200}, {"old", "404 page not found", "unsupported_feature", 404}} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(tc.status); fmt.Fprint(w, tc.body) }))
			defer srv.Close()
			env, out, stderr := configCommandEnv(t, []string{"limits", "-format", "json", "-model-proxy-url", srv.URL}, nil)
			if code := run(env); code != 1 || !strings.Contains(out.String(), tc.want) || !json.Valid(out.Bytes()) {
				t.Fatalf("code=%d out=%s err=%s", code, out, stderr)
			}
		})
	}
}

func TestLimitsCLITransportAmbiguityAndInterrupt(t *testing.T) {
	for _, interrupt := range []bool{false, true} {
		t.Run(fmt.Sprint(interrupt), func(t *testing.T) {
			signals := make(chan os.Signal, 1)
			var calls atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				var request protocol.ResetRequest
				_ = json.NewDecoder(r.Body).Decode(&request)
				if interrupt {
					signals <- os.Interrupt
					<-r.Context().Done()
					return
				}
				conn, _, _ := w.(http.Hijacker).Hijack()
				_ = conn.Close()
			}))
			defer srv.Close()
			env, out, stderr := configCommandEnv(t, []string{"limits", "reset", "openai-codex", "credit", "-format", "json", "-model-proxy-url", srv.URL}, nil)
			env.sigCh = signals
			want := 1
			if interrupt {
				want = 130
			}
			if code := run(env); code != want || calls.Load() != 1 {
				t.Fatalf("code=%d calls=%d out=%s err=%s", code, calls.Load(), out, stderr)
			}
			var result protocol.ResetResult
			if json.Unmarshal(out.Bytes(), &result) != nil || result.RequestID == "" || result.Error == nil || !result.Error.Indeterminate || !strings.Contains(stderr.String(), "-request-id "+result.RequestID) {
				t.Fatalf("out=%s err=%s", out, stderr)
			}
		})
	}
}

func TestLimitsCLIResets(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/limits/reset-credits" || r.URL.Query().Get("provider") != "openai-codex" {
			t.Error(r.URL)
		}
		fmt.Fprint(w, `{"provider":"openai-codex","credits":[{"id":"earned","reset_type":"codex_rate_limits","status":"available"}]}`)
	}))
	defer srv.Close()
	env, out, stderr := configCommandEnv(t, []string{"limits", "resets", "openai-codex", "-model-proxy-url", srv.URL}, nil)
	if code := run(env); code != 0 || !strings.Contains(out.String(), "earned") {
		t.Fatalf("code=%d out=%s err=%s", code, out, stderr)
	}
	if _, err := os.Stat(filepath.Join(env.lookup("HOME"), ".local", "state", "harness")); !os.IsNotExist(err) {
		t.Fatalf("session dir unexpectedly present: %v", err)
	}
}
