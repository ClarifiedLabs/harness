package apikey

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestGenerateShapeAndPrefix(t *testing.T) {
	key, err := Generate("laptop", ModelProxyPrefix)
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if !strings.HasPrefix(key, ModelProxyPrefix) {
		t.Fatalf("key %q missing prefix %q", key, ModelProxyPrefix)
	}
	wantLen := len(ModelProxyPrefix) + 43 // 32 bytes base64-raw-url = 43 chars
	if len(key) != wantLen {
		t.Fatalf("key length = %d, want %d", len(key), wantLen)
	}
}

func TestGenerateUniqueness(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 100; i++ {
		key, err := Generate(fmt.Sprintf("key-%d", i), MCPProxyPrefix)
		if err != nil {
			t.Fatalf("Generate: %v", err)
		}
		if seen[key] {
			t.Fatalf("duplicate key generated: %q", key)
		}
		seen[key] = true
	}
}

func TestGenerateNameValidation(t *testing.T) {
	_, err := Generate("bad name!", ModelProxyPrefix)
	if err == nil {
		t.Fatal("expected error for invalid name")
	}
}

func TestHashDeterministic(t *testing.T) {
	a := Hash("hmp_abc")
	b := Hash("hmp_abc")
	if len(a) != 32 || len(b) != 32 {
		t.Fatalf("hash length wrong: %d/%d", len(a), len(b))
	}
	for i := range a {
		if a[i] != b[i] {
			t.Fatal("same key produced different hashes")
		}
	}
}

func TestStoreAuthorize(t *testing.T) {
	var s Store
	s.Add("laptop", "hmp_secret", time.Time{})

	reqOK := httptest.NewRequest(http.MethodGet, "/", nil)
	reqOK.Header.Set("Authorization", "Bearer hmp_secret")
	if !s.Authorize(reqOK) {
		t.Fatal("expected valid key to authorize")
	}

	reqNoAuth := httptest.NewRequest(http.MethodGet, "/", nil)
	if s.Authorize(reqNoAuth) {
		t.Fatal("expected missing auth to be rejected")
	}

	reqBadScheme := httptest.NewRequest(http.MethodGet, "/", nil)
	reqBadScheme.Header.Set("Authorization", "Basic hmp_secret")
	if s.Authorize(reqBadScheme) {
		t.Fatal("expected non-Bearer scheme to be rejected")
	}

	reqWrong := httptest.NewRequest(http.MethodGet, "/", nil)
	reqWrong.Header.Set("Authorization", "Bearer hmp_wrong")
	if s.Authorize(reqWrong) {
		t.Fatal("expected wrong key to be rejected")
	}
}

func TestStoreNotRequired(t *testing.T) {
	var s Store
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	if !s.Authorize(req) {
		t.Fatal("empty store should allow all requests")
	}
}

func TestMiddlewareAllowsAndRejects(t *testing.T) {
	var s Store
	s.Add("laptop", "hmp_secret", time.Time{})

	var reached bool
	var gotName string
	var gotOK bool
	next := http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		reached = true
		gotName, gotOK = AuthorizedName(r)
	})
	handler := s.Middleware(next)

	w := httptest.NewRecorder()
	handler.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/", nil))
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("unauth code = %d, want 401", w.Code)
	}
	if reached {
		t.Fatal("unauth request should not reach next handler")
	}

	reached = false
	w = httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Authorization", "Bearer hmp_secret")
	handler.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("auth code = %d, want 200", w.Code)
	}
	if !reached {
		t.Fatal("auth request did not reach next handler")
	}
	if !gotOK || gotName != "laptop" {
		t.Fatalf("AuthorizedName = (%q, %v), want (\"laptop\", true)", gotName, gotOK)
	}
}

func TestMiddlewareAuthorizedEntryCarriesBudget(t *testing.T) {
	var s Store
	s.AddWithBudget("laptop", "hmp_secret", time.Time{}, time.Time{}, &CostBudget{LimitUSD: 5, PeriodSeconds: 3600})

	var got Entry
	var gotOK bool
	next := http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		got, gotOK = AuthorizedEntry(r)
		got.CostBudget.LimitUSD = 99
	})
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Authorization", "Bearer hmp_secret")
	s.Middleware(next).ServeHTTP(httptest.NewRecorder(), req)
	if !gotOK || got.Name != "laptop" || got.CostBudget == nil || got.CostBudget.LimitUSD != 99 || got.CostBudget.PeriodSeconds != 3600 {
		t.Fatalf("AuthorizedEntry = %+v, ok=%v", got, gotOK)
	}
	if s.Entries[0].CostBudget == nil || s.Entries[0].CostBudget.LimitUSD != 5 {
		t.Fatalf("AuthorizedEntry returned shared budget pointer; store entry = %+v", s.Entries[0])
	}
}

func TestMiddlewareWrongKeyHasNoName(t *testing.T) {
	var s Store
	s.Add("laptop", "hmp_secret", time.Time{})

	var reached bool
	next := http.HandlerFunc(func(http.ResponseWriter, *http.Request) { reached = true })
	handler := s.Middleware(next)

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Authorization", "Bearer hmp_wrong")
	handler.ServeHTTP(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("wrong key code = %d, want 401", w.Code)
	}
	if reached {
		t.Fatal("wrong key should not reach next handler")
	}
}

func TestAuthorizedNameAuthenticatedEmptyNameReportsAuthenticated(t *testing.T) {
	// A key with an empty Name still authenticates; AuthorizedName must report it
	// as authenticated (ok=true) so it is not conflated with the unauthenticated
	// "anonymous" case (ok=false) and its metrics misattributed.
	var s Store
	s.Add("", "hmp_secret", time.Time{}) // empty name, valid hash

	var gotName string
	var gotOK bool
	next := http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		gotName, gotOK = AuthorizedName(r)
	})
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Authorization", "Bearer hmp_secret")
	s.Middleware(next).ServeHTTP(httptest.NewRecorder(), req)

	if !gotOK || gotName != "" {
		t.Fatalf("AuthorizedName = (%q, %v), want (\"\", true)", gotName, gotOK)
	}
}

func TestAuthorizedNameAuthDisabled(t *testing.T) {
	var s Store // no keys -> auth not required
	next := http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		if name, ok := AuthorizedName(r); ok || name != "" {
			t.Fatalf("AuthorizedName = (%q, %v), want false when auth disabled", name, ok)
		}
	})
	s.Middleware(next).ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/", nil))
}

func TestAuthorizedNameNilRequest(t *testing.T) {
	if name, ok := AuthorizedName(nil); ok || name != "" {
		t.Fatalf("AuthorizedName(nil) = (%q, %v), want false", name, ok)
	}
}

func TestStoreExpiry(t *testing.T) {
	now := time.Now()
	var s Store
	s.AddWithExpiry("future", "hmp_future", now, now.Add(time.Hour))
	s.AddWithExpiry("expired", "hmp_expired", now.Add(-2*time.Hour), now.Add(-time.Hour))

	future := httptest.NewRequest(http.MethodGet, "/", nil)
	future.Header.Set("Authorization", "Bearer hmp_future")
	if !s.Authorize(future) {
		t.Fatal("unexpired key should authorize")
	}

	expired := httptest.NewRequest(http.MethodGet, "/", nil)
	expired.Header.Set("Authorization", "Bearer hmp_expired")
	if s.Authorize(expired) {
		t.Fatal("expired key should be rejected")
	}
}

func TestAllExpiredStoreStillRequiresAuth(t *testing.T) {
	now := time.Now()
	var s Store
	s.AddWithExpiry("expired", "hmp_expired", now.Add(-2*time.Hour), now.Add(-time.Hour))
	if !s.IsRequired() {
		t.Fatal("non-empty store should require auth even when all keys are expired")
	}
	w := httptest.NewRecorder()
	reached := false
	s.Middleware(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { reached = true })).ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/", nil))
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", w.Code)
	}
	if reached {
		t.Fatal("expired-only store should not reach next")
	}
}

func TestDynamicStoreUpdateAndAuthorizedName(t *testing.T) {
	now := time.Date(2026, 7, 7, 12, 0, 0, 0, time.UTC)
	clock := func() time.Time { return now }
	store := NewDynamicStore(nil, clock)

	reached := false
	var gotName string
	var gotOK bool
	handler := store.Middleware(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		reached = true
		gotName, gotOK = AuthorizedName(r)
	}))

	// Empty store allows unauthenticated requests.
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/", nil))
	if w.Code != http.StatusOK || !reached {
		t.Fatalf("empty dynamic store status=%d reached=%v, want 200 true", w.Code, reached)
	}

	var s Store
	s.AddWithExpiry("laptop", "hmp_secret", now, now.Add(time.Hour))
	store.Update(s.Entries)
	reached = false
	w = httptest.NewRecorder()
	noKey := httptest.NewRequest(http.MethodGet, "/", nil)
	handler.ServeHTTP(w, noKey)
	if w.Code != http.StatusUnauthorized || reached {
		t.Fatalf("configured dynamic store without key status=%d reached=%v, want 401 false", w.Code, reached)
	}

	w = httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Authorization", "Bearer hmp_secret")
	handler.ServeHTTP(w, req)
	if w.Code != http.StatusOK || !gotOK || gotName != "laptop" {
		t.Fatalf("authorized dynamic request status=%d name=(%q,%v), want 200 laptop true", w.Code, gotName, gotOK)
	}

	// Rotate the snapshot without rebuilding middleware.
	var rotated Store
	rotated.Add("desktop", "hmp_new", now)
	store.Update(rotated.Entries)
	oldReq := httptest.NewRequest(http.MethodGet, "/", nil)
	oldReq.Header.Set("Authorization", "Bearer hmp_secret")
	w = httptest.NewRecorder()
	handler.ServeHTTP(w, oldReq)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("old key status=%d, want 401 after rotation", w.Code)
	}
	newReq := httptest.NewRequest(http.MethodGet, "/", nil)
	newReq.Header.Set("Authorization", "Bearer hmp_new")
	w = httptest.NewRecorder()
	handler.ServeHTTP(w, newReq)
	if w.Code != http.StatusOK {
		t.Fatalf("new key status=%d, want 200 after rotation", w.Code)
	}

	store.Update(nil)
	w = httptest.NewRecorder()
	handler.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("removed keys status=%d, want 200", w.Code)
	}
}

func TestInitialKeyFileMissingBehavior(t *testing.T) {
	path := filepath.Join(t.TempDir(), "api_keys.json")
	entries, state, err := LoadInitialFile(path, false)
	if err != nil {
		t.Fatalf("LoadInitialFile missing default: %v", err)
	}
	if len(entries) != 0 || state.Loaded {
		t.Fatalf("missing non-explicit file entries=%+v state=%+v, want empty unloaded", entries, state)
	}
	if _, _, err := LoadInitialFile(path, true); err == nil {
		t.Fatal("missing explicit file should fail startup")
	}
}

func TestReloadFileIfChangedCreateUpdateAndInvalidKeepsSnapshot(t *testing.T) {
	now := time.Date(2026, 7, 7, 12, 0, 0, 0, time.UTC)
	path := filepath.Join(t.TempDir(), "api_keys.json")
	store := NewDynamicStore(nil, func() time.Time { return now })
	var state FileState
	changed, err := ReloadFileIfChanged(path, &state, store)
	if err != nil || changed || state.Loaded {
		t.Fatalf("missing before first load changed=%v state=%+v err=%v, want false unloaded nil", changed, state, err)
	}

	var first Store
	first.Add("first", "hmp_first", now)
	if err := WriteFile(path, first.Entries); err != nil {
		t.Fatalf("write first: %v", err)
	}
	changed, err = ReloadFileIfChanged(path, &state, store)
	if err != nil || !changed || !state.Loaded {
		t.Fatalf("first reload changed=%v state=%+v err=%v, want true loaded nil", changed, state, err)
	}
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Authorization", "Bearer hmp_first")
	if !store.Authorize(req) {
		t.Fatal("first key should authorize after reload")
	}

	if err := os.WriteFile(path, []byte(`{"api_keys":[{"name":"bad name!","hash":"AAAA"}]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if changed, err = ReloadFileIfChanged(path, &state, store); err == nil || changed {
		t.Fatalf("invalid reload changed=%v err=%v, want false error", changed, err)
	}
	if !store.Authorize(req) {
		t.Fatal("invalid reload should keep previous good snapshot")
	}

	var second Store
	second.Add("second", "hmp_second", now)
	if err := WriteFile(path, second.Entries); err != nil {
		t.Fatalf("write second: %v", err)
	}
	changed, err = ReloadFileIfChanged(path, &state, store)
	if err != nil || !changed {
		t.Fatalf("second reload changed=%v err=%v, want true nil", changed, err)
	}
	oldReq := httptest.NewRequest(http.MethodGet, "/", nil)
	oldReq.Header.Set("Authorization", "Bearer hmp_first")
	if store.Authorize(oldReq) {
		t.Fatal("old key should be rejected after valid reload")
	}
	newReq := httptest.NewRequest(http.MethodGet, "/", nil)
	newReq.Header.Set("Authorization", "Bearer hmp_second")
	if !store.Authorize(newReq) {
		t.Fatal("new key should authorize after valid reload")
	}
}

func TestKeyFileRoundTripAndValidation(t *testing.T) {
	now := time.Date(2026, 7, 7, 12, 0, 0, 0, time.UTC)
	expires := now.Add(time.Hour)
	entries := []Entry{{Name: "laptop", Hash: Hash("hmp_secret"), Added: now, ExpiresAt: expires}}
	path := filepath.Join(t.TempDir(), "nested", "api_keys.json")
	if err := WriteFile(path, entries); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat key file: %v", err)
	}
	if got := info.Mode().Perm(); got&0o077 != 0 {
		t.Fatalf("key file mode = %v, want no group/other permissions", got)
	}
	got, err := LoadFile(path)
	if err != nil {
		t.Fatalf("LoadFile: %v", err)
	}
	if len(got) != 1 || got[0].Name != "laptop" || string(got[0].Hash) != string(entries[0].Hash) || !got[0].Added.Equal(now) || !got[0].ExpiresAt.Equal(expires) {
		t.Fatalf("loaded entries = %+v, want %+v", got, entries)
	}

	bad := filepath.Join(t.TempDir(), "api_keys.json")
	if err := os.WriteFile(bad, []byte(`{"api_keys":[{"name":"bad name!","hash":"AAAA"}]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadFile(bad); err == nil {
		t.Fatal("LoadFile accepted invalid key name")
	}

	badBudget := filepath.Join(t.TempDir(), "api_keys.json")
	if err := os.WriteFile(badBudget, []byte(`{"api_keys":[{"name":"laptop","hash":"AAAA","cost_budget":{"limit_usd":1}}]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadFile(badBudget); err == nil || !strings.Contains(err.Error(), "period_seconds") {
		t.Fatalf("LoadFile invalid budget err = %v, want period_seconds", err)
	}
}
