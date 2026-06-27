// Package apikey implements optional API-key authentication for harness proxies.
// It is stdlib-only and provider-neutral.
package apikey

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"time"
)

const (
	// ModelProxyPrefix identifies API keys for harness-model-proxy.
	ModelProxyPrefix = "hmp_"
	// MCPProxyPrefix identifies API keys for harness-mcp-proxy.
	MCPProxyPrefix = "hmcpp_"
)

// KeyNameRE constrains the human-readable name attached to a generated key.
var KeyNameRE = regexp.MustCompile(`^[a-zA-Z0-9_-]{1,64}$`)

// Generate returns a fresh plaintext API key with the given prefix. The caller is
// responsible for hashing it (with Hash) and storing the digest. The plaintext key
// is returned exactly once and never recoverable.
func Generate(name, prefix string) (plaintext string, err error) {
	if !KeyNameRE.MatchString(name) {
		return "", fmt.Errorf("key name %q must match %s", name, KeyNameRE.String())
	}
	var buf [32]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return "", fmt.Errorf("generate random suffix: %w", err)
	}
	return prefix + base64.RawURLEncoding.EncodeToString(buf[:]), nil
}

// Hash returns the SHA-256 digest of key. No salt is used: the random suffix
// already provides high entropy.
func Hash(key string) []byte {
	sum := sha256.Sum256([]byte(key))
	return sum[:]
}

// Entry is one stored API key: a human name and the SHA-256 hash of the key.
type Entry struct {
	Name  string    `json:"name"`
	Hash  []byte    `json:"hash"`
	Added time.Time `json:"added"`
}

// Store is a collection of API keys. It is safe for concurrent reads once
// populated; no mutation happens on the served path.
type Store struct {
	Entries []Entry
}

// IsRequired reports whether authentication is required (i.e. at least one key
// is configured).
func (s Store) IsRequired() bool {
	return len(s.Entries) > 0
}

// Add appends a new entry hashing plaintext, using now as the added timestamp.
// It does not validate the key format; callers should use Generate.
func (s *Store) Add(name, plaintext string, now time.Time) {
	s.Entries = append(s.Entries, Entry{
		Name:  name,
		Hash:  Hash(plaintext),
		Added: now,
	})
}

// authorizedHash reports whether digest matches any stored hash in constant time.
func (s Store) authorizedHash(digest []byte) bool {
	for _, e := range s.Entries {
		if subtle.ConstantTimeCompare(e.Hash, digest) == 1 {
			return true
		}
	}
	return false
}

// authorizeName reports whether the request presents a valid API key and, when
// it does, returns the matched key's stored Name. It mirrors Authorize but
// reveals which key authorized the request so handlers can bucket metrics.
func (s Store) authorizeName(r *http.Request) (string, bool) {
	if !s.IsRequired() {
		return "", false
	}
	key, ok := bearerKey(r)
	if !ok || key == "" {
		return "", false
	}
	digest := Hash(key)
	for _, e := range s.Entries {
		if subtle.ConstantTimeCompare(e.Hash, digest) == 1 {
			return e.Name, true
		}
	}
	return "", false
}

// bearerKey extracts the bearer token from r. It returns "", false when no
// Authorization header is present or the scheme is not Bearer.
func bearerKey(r *http.Request) (string, bool) {
	auth := strings.TrimSpace(r.Header.Get("Authorization"))
	if auth == "" {
		return "", false
	}
	const prefix = "Bearer "
	if !strings.HasPrefix(auth, prefix) {
		return "", false
	}
	return strings.TrimSpace(auth[len(prefix):]), true
}

// Authorize reports whether the request presents a valid API key. A request with
// no Authorization header or a non-Bearer scheme is rejected when keys are
// configured. It is separate from Middleware so callers can plug it into existing
// auth stacks if needed.
func (s Store) Authorize(r *http.Request) bool {
	if !s.IsRequired() {
		return true
	}
	key, ok := bearerKey(r)
	if !ok || key == "" {
		return false
	}
	return s.authorizedHash(Hash(key))
}

// ctxKey is the unexported context key holding the authorizing key's name.
type ctxKey struct{}

// Middleware wraps next with API-key authentication when keys are configured.
// It returns 401 Unauthorized for requests lacking a valid key and passes
// through otherwise. On a successful match the matched key's stored Name is
// stashed in the request context, retrievable via AuthorizedName.
func (s Store) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if name, ok := s.authorizeName(r); ok {
			next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), ctxKey{}, name)))
			return
		}
		if s.IsRequired() {
			w.Header().Set("WWW-Authenticate", `Bearer`)
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// AuthorizedName returns the matched API key's stored Name for r, if any. It
// returns ("", false) when auth is disabled (no keys configured), the request
// was not authenticated by Store.Middleware, or no key matched. Handlers use
// the false case to bucket metrics under the sentinel "anonymous".
func AuthorizedName(r *http.Request) (string, bool) {
	if r == nil {
		return "", false
	}
	v, ok := r.Context().Value(ctxKey{}).(string)
	if !ok || v == "" {
		return "", false
	}
	return v, true
}
