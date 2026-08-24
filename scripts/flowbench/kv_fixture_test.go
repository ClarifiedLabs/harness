package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

var kvCheckpointReferenceFiles = map[string]string{
	"clone.go": `package kvstore

func cloneBytes(value []byte) []byte {
	if value == nil {
		return nil
	}
	return append([]byte(nil), value...)
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
	order     *list.List
	evictions uint64
}

func New(capacity int, now Clock) (*Store, error) {
	if capacity <= 0 {
		return nil, ErrInvalidCapacity
	}
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
	if key == "" {
		return ErrInvalidKey
	}
	if ttl < 0 {
		return ErrInvalidTTL
	}
	s.purgeExpiredLocked(now)
	expiresAt := time.Time{}
	if ttl > 0 {
		expiresAt = now.Add(ttl)
	}
	if current, ok := s.items[key]; ok {
		current.value = cloneBytes(value)
		current.expiresAt = expiresAt
		s.order.MoveToFront(current.element)
		return nil
	}
	item := &entry{key: key, value: cloneBytes(value), expiresAt: expiresAt}
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
	s.purgeExpiredLocked(s.now())
	return len(s.items)
}

func (s *Store) Stats() Stats {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.purgeExpiredLocked(s.now())
	return Stats{Items: len(s.items), Evictions: s.evictions}
}
`,
	"expiry.go": `package kvstore

import "time"

func (item *entry) expired(now time.Time) bool {
	return !item.expiresAt.IsZero() && !now.Before(item.expiresAt)
}

func (s *Store) purgeExpiredLocked(now time.Time) {
	for element := s.order.Back(); element != nil; {
		previous := element.Prev()
		item := element.Value.(*entry)
		if item.expired(now) {
			s.deleteLocked(item)
		}
		element = previous
	}
}
`,
	"lru.go": `package kvstore

func (s *Store) evictLocked() {
	element := s.order.Back()
	if element == nil {
		return
	}
	s.deleteLocked(element.Value.(*entry))
	s.evictions++
}
`,
}

var kvFinalReferenceFiles = map[string]string{
	"batch.go": `package kvstore

import "errors"

func (s *Store) Batch(mutations []Mutation) error {
	for _, mutation := range mutations {
		if mutation.Key == "" {
			return errors.Join(ErrInvalidMutation, ErrInvalidKey)
		}
		switch mutation.Kind {
		case MutationPut:
			if mutation.TTL < 0 {
				return errors.Join(ErrInvalidMutation, ErrInvalidTTL)
			}
		case MutationDelete:
			if mutation.Value != nil || mutation.TTL != 0 {
				return ErrInvalidMutation
			}
		default:
			return ErrInvalidMutation
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	for _, mutation := range mutations {
		switch mutation.Kind {
		case MutationPut:
			if err := s.putLocked(mutation.Key, mutation.Value, mutation.TTL, now); err != nil {
				return err
			}
		case MutationDelete:
			if item, ok := s.items[mutation.Key]; ok {
				s.deleteLocked(item)
			}
		}
	}
	return nil
}
`,
	"snapshot.go": `package kvstore

import (
	"bytes"
	"container/list"
	"encoding/json"
	"errors"
	"io"
	"time"
)

type snapshotEnvelope struct {
	Version   int             ` + "`json:\"version\"`" + `
	Evictions uint64          ` + "`json:\"evictions\"`" + `
	Entries   []snapshotEntry ` + "`json:\"entries\"`" + `
}

type snapshotEntry struct {
	Key   string ` + "`json:\"key\"`" + `
	Value []byte ` + "`json:\"value\"`" + `
	TTLNS int64  ` + "`json:\"ttl_ns\"`" + `
}

func (s *Store) Snapshot() ([]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	s.purgeExpiredLocked(now)
	out := snapshotEnvelope{Version: 1, Evictions: s.evictions, Entries: make([]snapshotEntry, 0, len(s.items))}
	for element := s.order.Front(); element != nil; element = element.Next() {
		item := element.Value.(*entry)
		var ttl int64
		if !item.expiresAt.IsZero() {
			ttl = item.expiresAt.Sub(now).Nanoseconds()
		}
		out.Entries = append(out.Entries, snapshotEntry{Key: item.key, Value: cloneBytes(item.value), TTLNS: ttl})
	}
	return json.Marshal(out)
}

func (s *Store) Restore(data []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var input snapshotEnvelope
	if err := decoder.Decode(&input); err != nil {
		return errors.Join(ErrInvalidSnapshot, err)
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return errors.Join(ErrInvalidSnapshot, err)
	}
	if input.Version != 1 || len(input.Entries) > s.capacity {
		return ErrInvalidSnapshot
	}
	seen := make(map[string]bool, len(input.Entries))
	for _, item := range input.Entries {
		if item.Key == "" || item.TTLNS < 0 || seen[item.Key] {
			return ErrInvalidSnapshot
		}
		seen[item.Key] = true
	}
	now := s.now()
	items := make(map[string]*entry, len(input.Entries))
	order := list.New()
	for _, saved := range input.Entries {
		item := &entry{key: saved.Key, value: cloneBytes(saved.Value)}
		if saved.TTLNS > 0 {
			item.expiresAt = now.Add(time.Duration(saved.TTLNS))
		}
		item.element = order.PushBack(item)
		items[item.key] = item
	}
	s.items = items
	s.order = order
	s.evictions = input.Evictions
	return nil
}
`,
}

func writeKVReferenceFiles(t *testing.T, root string, files map[string]string) {
	t.Helper()
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(root, name), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

func decodeEvaluatorOutput(t *testing.T, value string) evaluatorOutput {
	t.Helper()
	var output evaluatorOutput
	if err := json.Unmarshal([]byte(value), &output); err != nil {
		t.Fatalf("decode verifier output %q: %v", value, err)
	}
	return output
}
