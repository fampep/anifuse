// Package cache provides a bounded TTL byte cache and a singleflight group
// so concurrent identical requests share one upstream fetch. Both are
// stdlib-only and safe for concurrent use.
package cache

import (
	"fmt"
	"sync"
	"time"
)

// Cache is a bounded TTL cache of byte bodies. Keys are short strings built
// by the caller; values are the exact bytes written to the client.
type Cache struct {
	mu    sync.Mutex
	items map[string]entry
	ttl   time.Duration
	max   int
}

type entry struct {
	body    []byte
	expires time.Time
}

// New builds a cache with a default TTL and a hard entry cap. Entries set
// with an explicit TTL override the default.
func New(ttl time.Duration, max int) *Cache {
	return &Cache{ttl: ttl, max: max}
}

// Get returns a live copy of the cached body, or false when the key is
// absent or expired. Expired entries are dropped on access.
func (c *Cache) Get(key string) ([]byte, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.items[key]
	if !ok {
		return nil, false
	}
	if time.Now().After(e.expires) {
		delete(c.items, key)
		return nil, false
	}
	return e.body, true
}

// Set stores a body until ttl elapses. A non-positive ttl is a no-op.
func (c *Cache) Set(key string, body []byte, ttl time.Duration) {
	if ttl <= 0 {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.items == nil {
		c.items = make(map[string]entry)
	}
	c.items[key] = entry{body: body, expires: time.Now().Add(ttl)}
	// Bounded memory: past the cap, evict some other entry. Eviction order
	// is deliberately arbitrary; the cache is a speed layer, not a store.
	for len(c.items) > c.max {
		for k := range c.items {
			if k != key {
				delete(c.items, k)
				break
			}
		}
	}
}

// Len reports the live entry count (used by tests and metrics).
func (c *Cache) Len() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.items)
}

// Group coalesces concurrent identical work: the first caller runs fn, the
// rest wait for its result. shared is true for every waiter.
type Group struct {
	mu      sync.Mutex
	flights map[string]*flight
}

type flight struct {
	done chan struct{}
	body []byte
	err  error
}

// Do runs fn for key, or joins the in-flight call already running for key.
// fn is executed exactly once per in-flight key.
func (g *Group) Do(key string, fn func() ([]byte, error)) (body []byte, shared bool, err error) {
	g.mu.Lock()
	if g.flights == nil {
		g.flights = make(map[string]*flight)
	}
	if f, ok := g.flights[key]; ok {
		g.mu.Unlock()
		<-f.done
		return f.body, true, f.err
	}
	f := &flight{done: make(chan struct{})}
	g.flights[key] = f
	g.mu.Unlock()

	defer func() {
		if r := recover(); r != nil {
			f.err = fmt.Errorf("cache: flight panic: %v", r)
		}
		// Named returns carry the outcome to the leader even on panic.
		body, err = f.body, f.err
		close(f.done)
		g.mu.Lock()
		delete(g.flights, key)
		g.mu.Unlock()
	}()
	f.body, f.err = fn()
	return f.body, false, f.err
}
