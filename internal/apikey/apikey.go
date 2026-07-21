// Package apikey implements optional API-key authentication for harness proxies.
// It is stdlib-only and provider-neutral.
package apikey

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync/atomic"
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
	Name       string      `json:"name"`
	Hash       []byte      `json:"hash"`
	Added      time.Time   `json:"added"`
	ExpiresAt  time.Time   `json:"expires_at,omitempty"`
	CostBudget *CostBudget `json:"cost_budget,omitempty"`
}

const maxCostBudgetPeriodSeconds = int64((1<<63 - 1) / int64(time.Second))

// CostBudget is an optional per-API-key model-proxy spend quota. PeriodSeconds
// is the fixed window size in seconds. RejectUnpriced controls whether the model
// proxy rejects requests whose cost cannot be known before/after the call.
type CostBudget struct {
	LimitUSD       float64 `json:"limit_usd,omitempty"`
	PeriodSeconds  int64   `json:"period_seconds,omitempty"`
	RejectUnpriced bool    `json:"reject_unpriced,omitempty"`
}

// Enabled reports whether b carries a usable budget limit/window pair.
func (b CostBudget) Enabled() bool {
	return b.LimitUSD > 0 && b.PeriodSeconds > 0
}

// Expired reports whether e is expired as of now. A zero ExpiresAt never
// expires.
func (e Entry) Expired(now time.Time) bool {
	return !e.ExpiresAt.IsZero() && !now.Before(e.ExpiresAt)
}

// File is the dedicated JSON API-key file shape used by proxy daemons.
type File struct {
	APIKeys []Entry `json:"api_keys"`
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

// Add appends a new non-expiring entry hashing plaintext, using now as the
// added timestamp. It does not validate the key format; callers should use
// Generate.
func (s *Store) Add(name, plaintext string, now time.Time) {
	s.AddWithExpiry(name, plaintext, now, time.Time{})
}

// AddWithExpiry appends a new entry hashing plaintext, using added as the added
// timestamp and expiresAt as the optional expiry time. A zero expiresAt means
// the key never expires.
func (s *Store) AddWithExpiry(name, plaintext string, added, expiresAt time.Time) {
	s.AddWithBudget(name, plaintext, added, expiresAt, nil)
}

// AddWithBudget appends a new entry hashing plaintext, using added as the added
// timestamp, expiresAt as the optional expiry time, and budget as optional
// per-key spend quota metadata.
func (s *Store) AddWithBudget(name, plaintext string, added, expiresAt time.Time, budget *CostBudget) {
	s.Entries = append(s.Entries, Entry{
		Name:       name,
		Hash:       Hash(plaintext),
		Added:      added,
		ExpiresAt:  expiresAt,
		CostBudget: cloneCostBudget(budget),
	})
}

// authorizeEntryAt reports whether the request presents a valid, unexpired API
// key and, when it does, returns the matched key entry. It ignores expired
// entries but does not decide whether auth is required; callers keep that based
// on total configured entries.
func authorizeEntryAt(entries []Entry, r *http.Request, now time.Time) (Entry, bool) {
	key, ok := bearerKey(r)
	if !ok || key == "" {
		return Entry{}, false
	}
	digest := Hash(key)
	for _, e := range entries {
		if e.Expired(now) {
			continue
		}
		if subtle.ConstantTimeCompare(e.Hash, digest) == 1 {
			return cloneEntry(e), true
		}
	}
	return Entry{}, false
}

// authorizeNameAt reports whether the request presents a valid API key and,
// when it does, returns the matched key's stored Name.
func authorizeNameAt(entries []Entry, r *http.Request, now time.Time) (string, bool) {
	entry, ok := authorizeEntryAt(entries, r, now)
	if !ok {
		return "", false
	}
	return entry.Name, true
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
	_, ok := authorizeNameAt(s.Entries, r, time.Now())
	return ok
}

// ctxKey is the unexported context key holding the authorizing key's name.
type ctxKey struct{}

// ctxEntryKey is the unexported context key holding the authorizing key entry.
type ctxEntryKey struct{}

// Middleware wraps next with API-key authentication when keys are configured.
// It returns 401 Unauthorized for requests lacking a valid key and passes
// through otherwise. On a successful match the matched key's stored Name is
// stashed in the request context, retrievable via AuthorizedName.
func (s Store) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if entry, ok := authorizeEntryAt(s.Entries, r, time.Now()); ok {
			ctx := context.WithValue(r.Context(), ctxKey{}, entry.Name)
			ctx = context.WithValue(ctx, ctxEntryKey{}, entry)
			next.ServeHTTP(w, r.WithContext(ctx))
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

// AuthorizedName returns the matched API key's stored Name for r, and whether
// the request was authenticated by Store.Middleware. ok is true whenever a key
// matched — even if that key's Name is empty — so an authenticated empty-name key
// is not conflated with the unauthenticated case. It returns ("", false) when
// auth is disabled (no keys configured), the request was not authenticated, or no
// key matched; handlers use the false case to bucket metrics under "anonymous".
func AuthorizedName(r *http.Request) (string, bool) {
	if r == nil {
		return "", false
	}
	return AuthorizedNameFromContext(r.Context())
}

// AuthorizedNameFromContext returns the matched API key's stored Name from ctx,
// and whether authentication middleware matched a key. The boolean preserves the
// distinction between an authenticated key with an empty stored name and an
// unauthenticated context.
func AuthorizedNameFromContext(ctx context.Context) (string, bool) {
	if ctx == nil {
		return "", false
	}
	v, ok := ctx.Value(ctxKey{}).(string)
	if !ok {
		return "", false
	}
	return v, true
}

// AuthorizedEntry returns the matched API key entry for r, and whether the
// request was authenticated by Store or DynamicStore middleware.
func AuthorizedEntry(r *http.Request) (Entry, bool) {
	if r == nil {
		return Entry{}, false
	}
	v, ok := r.Context().Value(ctxEntryKey{}).(Entry)
	if !ok {
		return Entry{}, false
	}
	return cloneEntry(v), true
}

// ValidateEntries verifies key-file entries are safe to serve. Key names are
// required so per-key metrics cannot be misattributed to the anonymous bucket.
func ValidateEntries(entries []Entry) error {
	for i, e := range entries {
		if !KeyNameRE.MatchString(e.Name) {
			return fmt.Errorf("api_keys[%d]: name %q must match %s", i, e.Name, KeyNameRE.String())
		}
		if e.CostBudget != nil {
			if err := ValidateCostBudget(*e.CostBudget); err != nil {
				return fmt.Errorf("api_keys[%d].cost_budget: %w", i, err)
			}
		}
	}
	return nil
}

// ValidateCostBudget verifies a per-key budget has either both limit/window set
// to positive values, or neither set (disabled). RejectUnpriced is valid either
// way so callers can keep explicit intent while disabling a budget.
func ValidateCostBudget(b CostBudget) error {
	if b.LimitUSD < 0 || math.IsNaN(b.LimitUSD) || math.IsInf(b.LimitUSD, 0) {
		return fmt.Errorf("limit_usd must be finite and non-negative")
	}
	if b.PeriodSeconds < 0 {
		return fmt.Errorf("period_seconds must be non-negative")
	}
	if b.PeriodSeconds > maxCostBudgetPeriodSeconds {
		return fmt.Errorf("period_seconds is too large")
	}
	limitSet := b.LimitUSD > 0
	periodSet := b.PeriodSeconds > 0
	if !limitSet && !periodSet {
		return nil
	}
	if !limitSet && periodSet {
		return fmt.Errorf("limit_usd must be positive when period_seconds is set")
	}
	if limitSet && !periodSet {
		return fmt.Errorf("period_seconds must be positive when limit_usd is set")
	}
	return nil
}

// LoadFile reads a dedicated API-key file and returns a validated copy of its
// entries.
func LoadFile(path string) ([]Entry, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var file File
	if err := json.Unmarshal(data, &file); err != nil {
		return nil, fmt.Errorf("parse api keys file %s: %w", path, err)
	}
	if err := ValidateEntries(file.APIKeys); err != nil {
		return nil, err
	}
	return cloneEntries(file.APIKeys), nil
}

// WriteFile atomically writes entries to a dedicated API-key file using mode
// 0600 for the temporary file that is renamed into place.
func WriteFile(path string, entries []Entry) error {
	if err := ValidateEntries(entries); err != nil {
		return err
	}
	data, err := json.MarshalIndent(File{APIKeys: entries}, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal api keys file: %w", err)
	}
	data = append(data, '\n')
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("create api keys dir: %w", err)
	}
	tmp, err := os.CreateTemp(dir, filepath.Base(path)+".*")
	if err != nil {
		return fmt.Errorf("create temp api keys file: %w", err)
	}
	tmpName := tmp.Name()
	cleanup := true
	defer func() {
		if cleanup {
			_ = os.Remove(tmpName)
		}
	}()
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("chmod temp api keys file: %w", err)
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("write temp api keys file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close temp api keys file: %w", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("rename temp api keys file: %w", err)
	}
	cleanup = false
	return nil
}

// DynamicStore is an atomically replaceable API-key store. Middleware built from
// it observes future Update calls without being rebuilt.
type DynamicStore struct {
	entries atomic.Value // stores []Entry
	now     func() time.Time
}

// NewDynamicStore returns a DynamicStore initialized with entries. now may be
// nil, in which case time.Now is used.
func NewDynamicStore(entries []Entry, now func() time.Time) *DynamicStore {
	if now == nil {
		now = time.Now
	}
	s := &DynamicStore{now: now}
	s.Update(entries)
	return s
}

// Update atomically replaces the served key snapshot.
func (s *DynamicStore) Update(entries []Entry) {
	if s == nil {
		return
	}
	s.entries.Store(cloneEntries(entries))
}

// IsRequired reports whether authentication is required for the current
// snapshot. Expired entries still count as configured keys, so an all-expired
// non-empty file requires auth and rejects all requests until changed.
func (s *DynamicStore) IsRequired() bool {
	return len(s.snapshot()) > 0
}

// Authorize reports whether the request presents a valid key for the current
// snapshot.
func (s *DynamicStore) Authorize(r *http.Request) bool {
	entries := s.snapshot()
	if len(entries) == 0 {
		return true
	}
	_, ok := authorizeNameAt(entries, r, s.nowTime())
	return ok
}

// Middleware wraps next with API-key authentication using the current snapshot.
func (s *DynamicStore) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		entries := s.snapshot()
		if entry, ok := authorizeEntryAt(entries, r, s.nowTime()); ok {
			ctx := context.WithValue(r.Context(), ctxKey{}, entry.Name)
			ctx = context.WithValue(ctx, ctxEntryKey{}, entry)
			next.ServeHTTP(w, r.WithContext(ctx))
			return
		}
		if len(entries) > 0 {
			w.Header().Set("WWW-Authenticate", `Bearer`)
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (s *DynamicStore) snapshot() []Entry {
	if s == nil {
		return nil
	}
	v := s.entries.Load()
	if v == nil {
		return nil
	}
	entries, _ := v.([]Entry)
	return entries
}

func (s *DynamicStore) nowTime() time.Time {
	if s == nil || s.now == nil {
		return time.Now()
	}
	return s.now()
}

// FileState tracks the last successfully loaded key-file metadata for polling
// reloads.
type FileState struct {
	ModTime time.Time
	Size    int64
	Loaded  bool
}

// LoadInitialFile loads path for startup. A missing non-explicit file yields an
// empty key set and a zero state; missing explicit files and malformed existing
// files are errors.
func LoadInitialFile(path string, explicit bool) ([]Entry, FileState, error) {
	info, err := os.Stat(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) && !explicit {
			return nil, FileState{}, nil
		}
		return nil, FileState{}, err
	}
	entries, err := LoadFile(path)
	if err != nil {
		return nil, FileState{}, err
	}
	return entries, FileState{ModTime: info.ModTime(), Size: info.Size(), Loaded: true}, nil
}

// ReloadFileIfChanged reloads path into store when its mtime or size differs
// from state. Missing/unreadable/malformed files return an error and leave both
// state and store unchanged. A missing file before any good snapshot is not an
// error, allowing conventional default files to be created later.
func ReloadFileIfChanged(path string, state *FileState, store *DynamicStore) (bool, error) {
	info, err := os.Stat(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) && (state == nil || !state.Loaded) {
			return false, nil
		}
		return false, err
	}
	if state != nil && state.Loaded && info.ModTime().Equal(state.ModTime) && info.Size() == state.Size {
		return false, nil
	}
	entries, err := LoadFile(path)
	if err != nil {
		return false, err
	}
	if store != nil {
		store.Update(entries)
	}
	if state != nil {
		*state = FileState{ModTime: info.ModTime(), Size: info.Size(), Loaded: true}
	}
	return true, nil
}

// WatchFile polls path until ctx is cancelled, reloading store after successful
// changes. Errors are reported through warn and keep the previous good snapshot.
func WatchFile(ctx context.Context, path string, state FileState, interval time.Duration, store *DynamicStore, warn func(error)) {
	if interval <= 0 {
		interval = 2 * time.Second
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if _, err := ReloadFileIfChanged(path, &state, store); err != nil && warn != nil {
				warn(err)
			}
		}
	}
}

func cloneEntries(entries []Entry) []Entry {
	if len(entries) == 0 {
		return nil
	}
	out := make([]Entry, len(entries))
	for i, entry := range entries {
		out[i] = cloneEntry(entry)
	}
	return out
}

func cloneEntry(entry Entry) Entry {
	entry.Hash = append([]byte(nil), entry.Hash...)
	entry.CostBudget = cloneCostBudget(entry.CostBudget)
	return entry
}

func cloneCostBudget(budget *CostBudget) *CostBudget {
	if budget == nil {
		return nil
	}
	copy := *budget
	return &copy
}
