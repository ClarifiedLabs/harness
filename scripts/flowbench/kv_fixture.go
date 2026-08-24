package main

var kvInitialFiles = map[string]string{
	"types.go": `package kvstore

import (
	"errors"
	"time"
)

var (
	ErrInvalidCapacity = errors.New("kvstore: capacity must be positive")
	ErrInvalidKey      = errors.New("kvstore: key must not be empty")
	ErrInvalidTTL      = errors.New("kvstore: ttl must not be negative")
	ErrInvalidMutation = errors.New("kvstore: invalid mutation")
	ErrInvalidSnapshot = errors.New("kvstore: invalid snapshot")
	ErrNotImplemented  = errors.New("kvstore: not implemented")
)

type Clock func() time.Time

type MutationKind string

const (
	MutationPut    MutationKind = "put"
	MutationDelete MutationKind = "delete"
)

type Mutation struct {
	Kind  MutationKind
	Key   string
	Value []byte
	TTL   time.Duration
}

type Stats struct {
	Items     int
	Evictions uint64
}
`,
	"clone.go": `package kvstore

func cloneBytes(value []byte) []byte {
	// BUG: callers can mutate values stored in or returned by the Store.
	return value
}
`,
	"store.go": `package kvstore

import (
	"container/list"
	"sync"
	"time"
)

type entry struct {
	key       string
	value     []byte
	expiresAt time.Time
	element   *list.Element
}

type Store struct {
	mu        sync.Mutex
	capacity  int
	now       Clock
	items     map[string]*entry
	order     *list.List // most recently used at the front
	evictions uint64
}

func New(capacity int, now Clock) (*Store, error) {
	// BUG: invalid capacities are accepted.
	if now == nil {
		now = time.Now
	}
	return &Store{capacity: capacity, now: now, items: make(map[string]*entry), order: list.New()}, nil
}

func (s *Store) Put(key string, value []byte, ttl time.Duration) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.putLocked(key, value, ttl, s.now())
}

func (s *Store) putLocked(key string, value []byte, ttl time.Duration, now time.Time) error {
	// BUG: key and TTL validation is missing, expired entries are retained, and
	// updates neither refresh expiry nor become most-recently-used.
	if current, ok := s.items[key]; ok {
		current.value = cloneBytes(value)
		return nil
	}
	item := &entry{key: key, value: cloneBytes(value)}
	if ttl > 0 {
		item.expiresAt = now.Add(ttl)
	}
	item.element = s.order.PushFront(item)
	s.items[key] = item
	for len(s.items) > s.capacity {
		s.evictLocked()
	}
	return nil
}

func (s *Store) Get(key string) ([]byte, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	item, ok := s.items[key]
	if !ok {
		return nil, false
	}
	if item.expired(s.now()) {
		s.deleteLocked(item)
		return nil, false
	}
	s.order.MoveToFront(item.element)
	return cloneBytes(item.value), true
}

func (s *Store) Delete(key string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	item, ok := s.items[key]
	if !ok {
		return false
	}
	s.deleteLocked(item)
	return true
}

func (s *Store) deleteLocked(item *entry) {
	delete(s.items, item.key)
	s.order.Remove(item.element)
}

func (s *Store) Len() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	// BUG: expired entries remain visible in Len and Stats.
	return len(s.items)
}

func (s *Store) Stats() Stats {
	s.mu.Lock()
	defer s.mu.Unlock()
	return Stats{Items: len(s.items), Evictions: s.evictions}
}
`,
	"expiry.go": `package kvstore

import "time"

func (item *entry) expired(now time.Time) bool {
	// BUG: immortal entries are treated as expired and deadlines are ignored.
	return item.expiresAt.IsZero()
}

func (s *Store) purgeExpiredLocked(now time.Time) {
	// BUG: expiration is never purged as a group.
}
`,
	"lru.go": `package kvstore

func (s *Store) evictLocked() {
	// BUG: this removes the most-recently-used item instead of the least.
	element := s.order.Front()
	if element == nil {
		return
	}
	s.deleteLocked(element.Value.(*entry))
	s.evictions++
}
`,
	"batch.go": `package kvstore

func (s *Store) Batch(mutations []Mutation) error {
	return ErrNotImplemented
}
`,
	"snapshot.go": `package kvstore

func (s *Store) Snapshot() ([]byte, error) {
	return nil, ErrNotImplemented
}

func (s *Store) Restore(data []byte) error {
	return ErrNotImplemented
}
`,
}

const kvCheckpointEvidence = `CHECKPOINT CONTRACT

Repair the existing flowbenchkv package without changing its public API.

1. New rejects capacity <= 0 with ErrInvalidCapacity and substitutes time.Now
   only when the Clock is nil.
2. Put rejects an empty key with ErrInvalidKey and a negative TTL with
   ErrInvalidTTL without mutating the store. A zero TTL never expires. Positive
   TTLs expire when now is at or past the deadline.
3. Stored and returned byte slices are defensive copies, including the
   cloneBytes helper. Updating a key refreshes its value and TTL and makes it
   most-recently-used.
4. Capacity uses true least-recently-used eviction. Put and successful Get make
   an item most-recently-used. Expired entries are purged before capacity is
   enforced and by Len and Stats. Only capacity eviction increments Evictions.
5. All exported Store operations remain safe for concurrent callers. Preserve
   the supplied map/list design and keep most-recently-used at the list front.

Run gofmt and ` + "`cd .flowbench-kvstore && go test ./...`" + ` after the repair.
`

const kvFinalEvidence = `RELEASE CONTRACT

Preserve every accepted checkpoint behavior and implement the two remaining
files without changing the public API.

1. Batch validates the complete mutation slice before taking effect. Valid
   kinds are MutationPut and MutationDelete; every key is non-empty; Put TTLs
   are non-negative; Delete requires nil Value and zero TTL. Any invalid item
   returns ErrInvalidMutation (wrapping ErrInvalidKey or ErrInvalidTTL when
   applicable) and leaves values, recency, expiry, and statistics unchanged.
   A valid batch applies in order under one lock, uses one captured clock value,
   deep-copies Put values, and has the same TTL/LRU/capacity semantics as Put.

2. Snapshot returns deterministic compact JSON with this exact logical shape:
   {"version":1,"evictions":N,"entries":[{"key":"k","value":"base64",
   "ttl_ns":N}]}. Entries are ordered most- to least-recently-used. ttl_ns is
   zero for no expiry and otherwise the positive remaining nanoseconds at the
   snapshot clock. Expired entries are omitted.

3. Restore atomically replaces the current contents from that format, rebasing
   positive remaining TTLs onto one current clock value and preserving order
   and the eviction counter. Reject malformed JSON, unknown fields, versions
   other than 1, empty or duplicate keys, negative TTLs, or more entries than
   capacity with ErrInvalidSnapshot, leaving the original store byte-for-byte
   behavior unchanged. Restored and snapshotted values must not alias buffers.

Run gofmt and ` + "`cd .flowbench-kvstore && go test ./...`" + ` after the repair.
`

const kvCheckpointTests = `package kvstore

import (
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
)

type oracleClock struct {
	mu  sync.Mutex
	now time.Time
}

func newOracleClock() *oracleClock {
	return &oracleClock{now: time.Unix(1_700_000_000, 0)}
}

func oracleContains(value, fragment string) bool {
	return strings.Contains(value, fragment)
}

func (c *oracleClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *oracleClock) Advance(d time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(d)
	c.mu.Unlock()
}

func TestOracleValidationAndCopies(t *testing.T) {
	if _, err := New(0, nil); !errors.Is(err, ErrInvalidCapacity) {
		t.Fatalf("New(0) error = %v", err)
	}
	clock := newOracleClock()
	store, err := New(4, clock.Now)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Put("", []byte("x"), 0); !errors.Is(err, ErrInvalidKey) {
		t.Fatalf("empty-key error = %v", err)
	}
	if err := store.Put("x", []byte("x"), -time.Second); !errors.Is(err, ErrInvalidTTL) {
		t.Fatalf("negative-TTL error = %v", err)
	}
	input := []byte("alpha")
	if err := store.Put("a", input, 0); err != nil {
		t.Fatal(err)
	}
	input[0] = 'X'
	got, ok := store.Get("a")
	if !ok || string(got) != "alpha" {
		t.Fatalf("stored value = %q, %t", got, ok)
	}
	got[0] = 'Y'
	got, _ = store.Get("a")
	if string(got) != "alpha" {
		t.Fatalf("returned value aliases store: %q", got)
	}
	nilClone := cloneBytes(nil)
	if nilClone != nil {
		t.Fatalf("cloneBytes(nil) = %#v", nilClone)
	}
}

func TestOracleTTLAndLRU(t *testing.T) {
	clock := newOracleClock()
	store, err := New(2, clock.Now)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Put("a", []byte("one"), 0); err != nil { t.Fatal(err) }
	if err := store.Put("b", []byte("two"), 0); err != nil { t.Fatal(err) }
	if _, ok := store.Get("a"); !ok { t.Fatal("a missing") }
	if err := store.Put("c", []byte("three"), 0); err != nil { t.Fatal(err) }
	if _, ok := store.Get("b"); ok { t.Fatal("least-recently-used b was not evicted") }
	if stats := store.Stats(); stats.Items != 2 || stats.Evictions != 1 {
		t.Fatalf("stats after eviction = %+v", stats)
	}
	if err := store.Put("a", []byte("updated"), 3*time.Second); err != nil { t.Fatal(err) }
	if err := store.Put("d", []byte("four"), 0); err != nil { t.Fatal(err) }
	if _, ok := store.Get("c"); ok { t.Fatal("updated a did not become MRU") }
	clock.Advance(3 * time.Second)
	if _, ok := store.Get("a"); ok { t.Fatal("a survived its deadline") }
	if stats := store.Stats(); stats.Items != 1 || stats.Evictions != 2 {
		t.Fatalf("expired item affected eviction stats: %+v", stats)
	}
	if store.Len() != 1 { t.Fatalf("Len = %d", store.Len()) }
}

func TestOracleConcurrentCalls(t *testing.T) {
	clock := newOracleClock()
	store, err := New(64, clock.Now)
	if err != nil { t.Fatal(err) }
	var wg sync.WaitGroup
	for i := 0; i < 32; i++ {
		i := i
		wg.Add(1)
		go func() {
			defer wg.Done()
			key := string(rune('a' + i))
			if err := store.Put(key, []byte{byte(i)}, 0); err != nil { t.Errorf("Put: %v", err); return }
			if _, ok := store.Get(key); !ok { t.Errorf("%q missing", key) }
		}()
	}
	wg.Wait()
	if store.Len() != 32 { t.Fatalf("Len = %d", store.Len()) }
}
`

const kvFinalTests = kvCheckpointTests + `
func TestOracleBatchAtomicityAndSemantics(t *testing.T) {
	clock := newOracleClock()
	store, err := New(2, clock.Now)
	if err != nil { t.Fatal(err) }
	value := []byte("one")
	if err := store.Batch([]Mutation{
		{Kind: MutationPut, Key: "a", Value: value},
		{Kind: MutationPut, Key: "b", Value: []byte("two"), TTL: 5 * time.Second},
	}); err != nil { t.Fatal(err) }
	value[0] = 'X'
	if got, _ := store.Get("a"); string(got) != "one" { t.Fatalf("batch value = %q", got) }
	before, err := store.Snapshot()
	if err != nil { t.Fatal(err) }
	bad := []Mutation{
		{Kind: MutationPut, Key: "c", Value: []byte("three")},
		{Kind: MutationDelete, Key: "a", Value: []byte("forbidden")},
	}
	if err := store.Batch(bad); !errors.Is(err, ErrInvalidMutation) { t.Fatalf("invalid batch error = %v", err) }
	after, _ := store.Snapshot()
	if string(after) != string(before) { t.Fatalf("invalid batch mutated store\nbefore=%s\nafter=%s", before, after) }
	if err := store.Batch([]Mutation{
		{Kind: MutationPut, Key: "a", Value: []byte("new")},
		{Kind: MutationDelete, Key: "b"},
		{Kind: MutationPut, Key: "c", Value: []byte("three")},
	}); err != nil { t.Fatal(err) }
	if got, _ := store.Get("a"); string(got) != "new" { t.Fatalf("a = %q", got) }
	if _, ok := store.Get("b"); ok { t.Fatal("b survived delete") }
	if got, _ := store.Get("c"); string(got) != "three" { t.Fatalf("c = %q", got) }
}

func TestOracleSnapshotRoundTripOrderAndTTL(t *testing.T) {
	clock := newOracleClock()
	store, err := New(3, clock.Now)
	if err != nil { t.Fatal(err) }
	_ = store.Put("a", []byte("one"), 0)
	_ = store.Put("b", []byte("two"), 10*time.Second)
	_ = store.Put("c", []byte("three"), 0)
	_, _ = store.Get("a") // MRU order: a, c, b
	first, err := store.Snapshot()
	if err != nil { t.Fatal(err) }
	second, err := store.Snapshot()
	if err != nil || string(first) != string(second) { t.Fatalf("snapshot is not deterministic: %v\n%s\n%s", err, first, second) }
	if !oracleContains(string(first), ` + "`\"version\":1`" + `) || !oracleContains(string(first), ` + "`\"ttl_ns\":10000000000`" + `) {
		t.Fatalf("snapshot shape = %s", first)
	}
	restored, _ := New(3, clock.Now)
	if err := restored.Restore(first); err != nil { t.Fatal(err) }
	if err := restored.Put("d", []byte("four"), 0); err != nil { t.Fatal(err) }
	if _, ok := restored.Get("b"); ok { t.Fatal("restore lost LRU order; b should be evicted") }
	for key, want := range map[string]string{"a":"one", "c":"three", "d":"four"} {
		got, ok := restored.Get(key)
		if !ok || string(got) != want { t.Fatalf("%s = %q, %t", key, got, ok) }
	}

	ttlStore, _ := New(2, clock.Now)
	_ = ttlStore.Put("ttl", []byte("value"), 10*time.Second)
	ttlSnapshot, _ := ttlStore.Snapshot()
	clock.Advance(2 * time.Second)
	ttlRestored, _ := New(2, clock.Now)
	if err := ttlRestored.Restore(ttlSnapshot); err != nil { t.Fatal(err) }
	clock.Advance(9 * time.Second)
	if _, ok := ttlRestored.Get("ttl"); !ok { t.Fatal("restored TTL expired too early") }
	clock.Advance(time.Second)
	if _, ok := ttlRestored.Get("ttl"); ok { t.Fatal("restored TTL did not expire") }
}

func TestOracleRestoreRejectsAtomically(t *testing.T) {
	clock := newOracleClock()
	store, _ := New(2, clock.Now)
	_ = store.Put("safe", []byte("value"), 0)
	want, _ := store.Snapshot()
	invalid := [][]byte{
		[]byte("{"),
		[]byte(` + "`{\"version\":2,\"evictions\":0,\"entries\":[]}`" + `),
		[]byte(` + "`{\"version\":1,\"evictions\":0,\"entries\":[{\"key\":\"\",\"value\":\"eA==\",\"ttl_ns\":0}]}`" + `),
		[]byte(` + "`{\"version\":1,\"evictions\":0,\"entries\":[{\"key\":\"x\",\"value\":\"eA==\",\"ttl_ns\":0},{\"key\":\"x\",\"value\":\"eQ==\",\"ttl_ns\":0}]}`" + `),
		[]byte(` + "`{\"version\":1,\"evictions\":0,\"entries\":[{\"key\":\"x\",\"value\":\"eA==\",\"ttl_ns\":-1}]}`" + `),
		[]byte(` + "`{\"version\":1,\"evictions\":0,\"entries\":[],\"unknown\":true}`" + `),
	}
	for _, data := range invalid {
		if err := store.Restore(data); !errors.Is(err, ErrInvalidSnapshot) { t.Fatalf("Restore(%s) error = %v", data, err) }
		got, _ := store.Snapshot()
		if string(got) != string(want) { t.Fatalf("invalid restore mutated store\nwant=%s\ngot=%s", want, got) }
	}
	tooMany := []byte(` + "`{\"version\":1,\"evictions\":0,\"entries\":[{\"key\":\"a\",\"value\":\"YQ==\",\"ttl_ns\":0},{\"key\":\"b\",\"value\":\"Yg==\",\"ttl_ns\":0},{\"key\":\"c\",\"value\":\"Yw==\",\"ttl_ns\":0}]}`" + `)
	if err := store.Restore(tooMany); !errors.Is(err, ErrInvalidSnapshot) { t.Fatalf("too-many error = %v", err) }
}
`
