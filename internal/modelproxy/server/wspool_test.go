package server

import (
	"context"
	"errors"
	"iter"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"harness/internal/llm"
)

type poolTestProvider struct {
	closes atomic.Int32
	closed chan struct{}
	once   sync.Once
}

func newPoolTestProvider() *poolTestProvider {
	return &poolTestProvider{closed: make(chan struct{})}
}

func (*poolTestProvider) Name() string { return "pool-test" }

func (*poolTestProvider) Stream(context.Context, llm.Request) iter.Seq2[llm.StreamEvent, error] {
	return func(func(llm.StreamEvent, error) bool) {}
}

func (p *poolTestProvider) Close() error {
	p.closes.Add(1)
	p.once.Do(func() { close(p.closed) })
	return nil
}

type poolTestClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *poolTestClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *poolTestClock) Add(d time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(d)
	c.mu.Unlock()
}

func poolKey(value byte) wsPoolKey {
	var key wsPoolKey
	key.Connection[0] = value
	key.Session[0] = value
	return key
}

func TestWSPoolDefaultsRetainHealthySocketsForOneIdleHour(t *testing.T) {
	ticks := make(chan time.Time)
	pool := newWSPool(wsPoolOptions{ticks: ticks})
	defer pool.Close()

	if pool.idleTTL != time.Hour {
		t.Fatalf("idle TTL = %s, want 1h", pool.idleTTL)
	}
	if pool.maxAge != 0 {
		t.Fatalf("absolute max age = %s, want disabled", pool.maxAge)
	}
}

func TestWSPoolLRUEvictionAndDoubleRelease(t *testing.T) {
	clock := &poolTestClock{now: time.Unix(1, 0)}
	ticks := make(chan time.Time)
	pool := newWSPool(wsPoolOptions{
		maxSize: 2,
		idleTTL: time.Hour,
		maxAge:  time.Hour,
		now:     clock.Now,
		ticks:   ticks,
	})
	defer pool.Close()

	created := []*poolTestProvider{}
	acquire := func(key wsPoolKey) (*poolTestProvider, func()) {
		provider, release, err := pool.Acquire(key, func() (llm.Provider, error) {
			next := newPoolTestProvider()
			created = append(created, next)
			return next, nil
		})
		if err != nil {
			t.Fatal(err)
		}
		return provider.(*poolTestProvider), release
	}

	first, releaseFirst := acquire(poolKey(1))
	releaseFirst()
	releaseFirst()
	clock.Add(time.Minute)
	second, releaseSecond := acquire(poolKey(2))
	releaseSecond()
	clock.Add(time.Minute)
	hit, releaseHit := acquire(poolKey(1))
	if hit != first {
		t.Fatal("pool hit returned a different provider")
	}
	releaseHit()
	clock.Add(time.Minute)
	third, releaseThird := acquire(poolKey(3))
	releaseThird()

	if second.closes.Load() != 1 {
		t.Fatalf("LRU closes = %d, want 1", second.closes.Load())
	}
	if first.closes.Load() != 0 || third.closes.Load() != 0 {
		t.Fatalf("active pool entries closed early: first=%d third=%d", first.closes.Load(), third.closes.Load())
	}
	pool.mu.Lock()
	size := len(pool.entries)
	pool.mu.Unlock()
	if size != 2 || len(created) != 3 {
		t.Fatalf("pool size=%d created=%d, want 2 and 3", size, len(created))
	}
}

func TestWSPoolActiveAgeExpiryRetiresUntilRelease(t *testing.T) {
	clock := &poolTestClock{now: time.Unix(1, 0)}
	ticks := make(chan time.Time)
	events := make(chan string, 8)
	pool := newWSPool(wsPoolOptions{
		maxSize: 2,
		idleTTL: time.Hour,
		maxAge:  5 * time.Minute,
		now:     clock.Now,
		ticks:   ticks,
		onEvent: func(event string) { events <- event },
	})
	defer pool.Close()

	first := newPoolTestProvider()
	provider, releaseFirst, err := pool.Acquire(poolKey(1), func() (llm.Provider, error) { return first, nil })
	if err != nil || provider != first {
		t.Fatalf("Acquire = %T, %v", provider, err)
	}
	clock.Add(6 * time.Minute)
	ticks <- clock.Now()
	waitPoolEvent(t, events, "evict_age")
	if first.closes.Load() != 0 {
		t.Fatal("active expired provider closed before release")
	}

	second := newPoolTestProvider()
	provider, releaseSecond, err := pool.Acquire(poolKey(1), func() (llm.Provider, error) { return second, nil })
	if err != nil || provider != second {
		t.Fatalf("replacement Acquire = %T, %v", provider, err)
	}
	releaseFirst()
	if first.closes.Load() != 1 {
		t.Fatalf("retired provider closes = %d, want 1", first.closes.Load())
	}
	releaseFirst()
	if first.closes.Load() != 1 {
		t.Fatalf("double release closes = %d, want 1", first.closes.Load())
	}
	releaseSecond()
}

func TestWSPoolOverflowIsUnpooledAndShutdownRetiresActive(t *testing.T) {
	ticks := make(chan time.Time)
	pool := newWSPool(wsPoolOptions{maxSize: 1, idleTTL: time.Hour, maxAge: time.Hour, ticks: ticks})

	pooled := newPoolTestProvider()
	_, releasePooled, err := pool.Acquire(poolKey(1), func() (llm.Provider, error) { return pooled, nil })
	if err != nil {
		t.Fatal(err)
	}
	overflow := newPoolTestProvider()
	_, releaseOverflow, err := pool.Acquire(poolKey(2), func() (llm.Provider, error) { return overflow, nil })
	if err != nil {
		t.Fatal(err)
	}
	pool.mu.Lock()
	size := len(pool.entries)
	pool.mu.Unlock()
	if size != 1 {
		t.Fatalf("pool size = %d, want 1", size)
	}
	releaseOverflow()
	releaseOverflow()
	if overflow.closes.Load() != 1 {
		t.Fatalf("overflow closes = %d, want 1", overflow.closes.Load())
	}

	if err := pool.Close(); err != nil {
		t.Fatal(err)
	}
	if pooled.closes.Load() != 0 {
		t.Fatal("shutdown closed an active lease")
	}
	if _, _, err := pool.Acquire(poolKey(3), func() (llm.Provider, error) {
		return newPoolTestProvider(), nil
	}); !errors.Is(err, errWSPoolClosed) {
		t.Fatalf("Acquire after shutdown = %v, want pool closed", err)
	}
	releasePooled()
	if pooled.closes.Load() != 1 {
		t.Fatalf("retired shutdown provider closes = %d, want 1", pooled.closes.Load())
	}
	select {
	case <-pool.done:
	default:
		t.Fatal("pool janitor did not stop")
	}
}

func TestWSPoolIdleExpiryAndSizeBound(t *testing.T) {
	clock := &poolTestClock{now: time.Unix(1, 0)}
	ticks := make(chan time.Time)
	pool := newWSPool(wsPoolOptions{
		maxSize: 8,
		idleTTL: time.Minute,
		maxAge:  time.Hour,
		now:     clock.Now,
		ticks:   ticks,
	})

	idle := newPoolTestProvider()
	_, release, err := pool.Acquire(poolKey(1), func() (llm.Provider, error) { return idle, nil })
	if err != nil {
		t.Fatal(err)
	}
	release()
	clock.Add(2 * time.Minute)
	ticks <- clock.Now()
	select {
	case <-idle.closed:
	case <-time.After(time.Second):
		t.Fatal("idle provider was not evicted")
	}

	created := make([]*poolTestProvider, 0, 200)
	for i := 0; i < 200; i++ {
		key := wsPoolKey{Connection: [32]byte{byte(i), byte(i >> 8)}, Session: [32]byte{byte(i), byte(i >> 8)}}
		_, release, err := pool.Acquire(key, func() (llm.Provider, error) {
			provider := newPoolTestProvider()
			created = append(created, provider)
			return provider, nil
		})
		if err != nil {
			t.Fatal(err)
		}
		release()
	}
	pool.mu.Lock()
	size := len(pool.entries)
	pool.mu.Unlock()
	if size != 8 {
		t.Fatalf("pool size after 200 sessions = %d, want 8", size)
	}
	if err := pool.Close(); err != nil {
		t.Fatal(err)
	}
	for i, provider := range created {
		if provider.closes.Load() != 1 {
			t.Fatalf("provider %d closes = %d, want 1", i, provider.closes.Load())
		}
	}
}

func TestHandlerCloseIsIdempotentAndStopsPoolJanitor(t *testing.T) {
	ticks := make(chan time.Time)
	pool := newWSPool(wsPoolOptions{ticks: ticks})
	handler := &Handler{wsPool: pool}
	if err := handler.Close(); err != nil {
		t.Fatal(err)
	}
	if err := handler.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-pool.done:
	default:
		t.Fatal("Handler.Close did not stop the pool janitor")
	}
}

func waitPoolEvent(t *testing.T, events <-chan string, want string) {
	t.Helper()
	for {
		select {
		case event := <-events:
			if event == want {
				return
			}
		case <-time.After(time.Second):
			t.Fatalf("pool event %q not observed", want)
		}
	}
}
