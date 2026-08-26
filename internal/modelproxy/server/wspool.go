package server

import (
	"errors"
	"fmt"
	"sync"
	"time"

	"harness/internal/llm"
)

const (
	defaultWSPoolMaxSize     = 64
	defaultWSPoolIdleTTL     = time.Hour
	defaultWSPoolMaxAge      = 0
	defaultWSPoolJanitorTick = 30 * time.Second
)

var errWSPoolClosed = errors.New("model proxy websocket pool is closed")

type providerCloser interface {
	Close() error
}

type responseContinuationAvailability interface {
	CanContinueResponse(responseID string) bool
}

// wsPoolKey keeps transport configuration and session affinity independently
// hashed. Both arrays are comparable, so the key can be used directly in a map
// without delimiter-sensitive string construction.
type wsPoolKey struct {
	Connection [32]byte
	Session    [32]byte
}

type wsPoolOptions struct {
	maxSize     int
	idleTTL     time.Duration
	maxAge      time.Duration
	janitorTick time.Duration
	now         func() time.Time
	ticks       <-chan time.Time
	stopTicks   func()
	onEvent     func(string)
	onSize      func(current, capacity int)
}

type wsPoolEntry struct {
	provider llm.Provider
	created  time.Time
	lastUsed time.Time
	inUse    int
	retiring bool
	closed   bool
}

type wsPool struct {
	mu       sync.Mutex
	entries  map[wsPoolKey]*wsPoolEntry
	maxSize  int
	idleTTL  time.Duration
	maxAge   time.Duration
	now      func() time.Time
	onEvent  func(string)
	onSize   func(current, capacity int)
	stop     chan struct{}
	done     chan struct{}
	stopOnce sync.Once
	closeErr error
}

func newWSPool(opts wsPoolOptions) *wsPool {
	if opts.maxSize <= 0 {
		opts.maxSize = defaultWSPoolMaxSize
	}
	if opts.idleTTL <= 0 {
		opts.idleTTL = defaultWSPoolIdleTTL
	}
	if opts.maxAge <= 0 {
		opts.maxAge = defaultWSPoolMaxAge
	}
	if opts.janitorTick <= 0 {
		opts.janitorTick = defaultWSPoolJanitorTick
	}
	if opts.now == nil {
		opts.now = time.Now
	}

	ticks := opts.ticks
	stopTicks := opts.stopTicks
	if ticks == nil {
		ticker := time.NewTicker(opts.janitorTick)
		ticks = ticker.C
		stopTicks = ticker.Stop
	}
	p := &wsPool{
		entries: make(map[wsPoolKey]*wsPoolEntry),
		maxSize: opts.maxSize,
		idleTTL: opts.idleTTL,
		maxAge:  opts.maxAge,
		now:     opts.now,
		onEvent: opts.onEvent,
		onSize:  opts.onSize,
		stop:    make(chan struct{}),
		done:    make(chan struct{}),
	}
	p.reportSize(0)
	go p.runJanitor(ticks, stopTicks)
	return p
}

func (p *wsPool) runJanitor(ticks <-chan time.Time, stopTicks func()) {
	defer close(p.done)
	if stopTicks != nil {
		defer stopTicks()
	}
	for {
		select {
		case _, ok := <-ticks:
			if !ok {
				return
			}
			p.expire()
		case <-p.stop:
			return
		}
	}
}

// Acquire leases the provider associated with key. The returned release
// function is always safe to call more than once.
func (p *wsPool) Acquire(key wsPoolKey, newProvider func() (llm.Provider, error)) (llm.Provider, func(), error) {
	if newProvider == nil {
		return nil, func() {}, fmt.Errorf("model proxy websocket pool: nil provider constructor")
	}
	now := p.now()
	p.mu.Lock()
	if p.stoppedLocked() {
		p.mu.Unlock()
		return nil, func() {}, errWSPoolClosed
	}
	toClose, events := p.expireLocked(now)
	if entry := p.entries[key]; entry != nil {
		entry.inUse++
		entry.lastUsed = now
		size := len(p.entries)
		p.mu.Unlock()
		p.closeEntries(toClose)
		p.recordEvents(events)
		p.recordEvent("hit")
		p.reportSize(size)
		return entry.provider, p.releaseFunc(entry), nil
	}
	p.mu.Unlock()
	p.closeEntries(toClose)
	p.recordEvents(events)
	p.recordEvent("miss")

	provider, err := newProvider()
	if err != nil {
		return nil, func() {}, err
	}
	p.recordEvent("create")

	now = p.now()
	p.mu.Lock()
	if p.stoppedLocked() {
		p.mu.Unlock()
		_ = closeProvider(provider)
		return nil, func() {}, errWSPoolClosed
	}
	toClose, events = p.expireLocked(now)
	if entry := p.entries[key]; entry != nil {
		entry.inUse++
		entry.lastUsed = now
		size := len(p.entries)
		p.mu.Unlock()
		toClose = append(toClose, &wsPoolEntry{provider: provider})
		p.closeEntries(toClose)
		p.recordEvents(events)
		p.recordEvent("hit")
		p.reportSize(size)
		return entry.provider, p.releaseFunc(entry), nil
	}

	if len(p.entries) >= p.maxSize {
		if lruKey, lru := p.leastRecentlyUsedIdleLocked(); lru != nil {
			delete(p.entries, lruKey)
			lru.retiring = true
			if p.markCloseLocked(lru) {
				toClose = append(toClose, lru)
			}
			events = append(events, "evict_lru")
		} else {
			size := len(p.entries)
			p.mu.Unlock()
			p.closeEntries(toClose)
			p.recordEvents(events)
			p.recordEvent("overflow")
			p.reportSize(size)
			return provider, closeOnceRelease(provider), nil
		}
	}

	entry := &wsPoolEntry{
		provider: provider,
		created:  now,
		lastUsed: now,
		inUse:    1,
	}
	p.entries[key] = entry
	size := len(p.entries)
	p.mu.Unlock()
	p.closeEntries(toClose)
	p.recordEvents(events)
	p.reportSize(size)
	return provider, p.releaseFunc(entry), nil
}

func (p *wsPool) releaseFunc(entry *wsPoolEntry) func() {
	var once sync.Once
	return func() {
		once.Do(func() {
			var shouldClose bool
			p.mu.Lock()
			if entry.inUse > 0 {
				entry.inUse--
			}
			entry.lastUsed = p.now()
			if entry.retiring && entry.inUse == 0 {
				shouldClose = p.markCloseLocked(entry)
			}
			p.mu.Unlock()
			if shouldClose {
				p.recordCloseError(closeProvider(entry.provider))
			}
		})
	}
}

func closeOnceRelease(provider llm.Provider) func() {
	var once sync.Once
	return func() {
		once.Do(func() {
			_ = closeProvider(provider)
		})
	}
}

func (p *wsPool) expire() {
	p.mu.Lock()
	if p.stoppedLocked() {
		p.mu.Unlock()
		return
	}
	toClose, events := p.expireLocked(p.now())
	size := len(p.entries)
	p.mu.Unlock()
	p.closeEntries(toClose)
	p.recordEvents(events)
	p.reportSize(size)
}

func (p *wsPool) expireLocked(now time.Time) (toClose []*wsPoolEntry, events []string) {
	for key, entry := range p.entries {
		event := ""
		switch {
		case p.maxAge > 0 && now.Sub(entry.created) >= p.maxAge:
			event = "evict_age"
		case entry.inUse == 0 && p.idleTTL > 0 && now.Sub(entry.lastUsed) >= p.idleTTL:
			event = "evict_idle"
		}
		if event == "" {
			continue
		}
		delete(p.entries, key)
		entry.retiring = true
		if entry.inUse == 0 && p.markCloseLocked(entry) {
			toClose = append(toClose, entry)
		}
		events = append(events, event)
	}
	return toClose, events
}

func (p *wsPool) leastRecentlyUsedIdleLocked() (wsPoolKey, *wsPoolEntry) {
	var selectedKey wsPoolKey
	var selected *wsPoolEntry
	for key, entry := range p.entries {
		if entry.inUse != 0 {
			continue
		}
		if selected == nil || entry.lastUsed.Before(selected.lastUsed) {
			selectedKey, selected = key, entry
		}
	}
	return selectedKey, selected
}

func (p *wsPool) markCloseLocked(entry *wsPoolEntry) bool {
	if entry.closed {
		return false
	}
	entry.closed = true
	return true
}

func (p *wsPool) stoppedLocked() bool {
	select {
	case <-p.stop:
		return true
	default:
		return false
	}
}

func (p *wsPool) closeEntries(entries []*wsPoolEntry) {
	for _, entry := range entries {
		if entry != nil {
			p.recordCloseError(closeProvider(entry.provider))
		}
	}
}

func closeProvider(provider llm.Provider) error {
	if closer, ok := provider.(providerCloser); ok {
		return closer.Close()
	}
	return nil
}

func (p *wsPool) recordCloseError(err error) {
	if err == nil {
		return
	}
	p.mu.Lock()
	p.closeErr = errors.Join(p.closeErr, err)
	p.mu.Unlock()
}

func (p *wsPool) recordEvent(event string) {
	if p.onEvent != nil {
		p.onEvent(event)
	}
}

func (p *wsPool) recordEvents(events []string) {
	for _, event := range events {
		p.recordEvent(event)
	}
}

func (p *wsPool) reportSize(size int) {
	if p.onSize != nil {
		p.onSize(size, p.maxSize)
	}
}

// Close stops the janitor, removes every provider from lookup, closes idle
// providers, and retires active leases so they close on release.
func (p *wsPool) Close() error {
	if p == nil {
		return nil
	}
	p.stopOnce.Do(func() {
		close(p.stop)
		<-p.done
		var toClose []*wsPoolEntry
		p.mu.Lock()
		for key, entry := range p.entries {
			delete(p.entries, key)
			entry.retiring = true
			if entry.inUse == 0 && p.markCloseLocked(entry) {
				toClose = append(toClose, entry)
			}
		}
		p.mu.Unlock()
		p.closeEntries(toClose)
		p.reportSize(0)
	})
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.closeErr
}
