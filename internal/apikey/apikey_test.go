package apikey

import (
	"fmt"
	"net/http"
	"net/http/httptest"
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
