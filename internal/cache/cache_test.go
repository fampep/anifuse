package cache

import (
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestGetSetAndExpiry(t *testing.T) {
	c := New(time.Minute, 8)
	if _, ok := c.Get("k"); ok {
		t.Fatal("absent key hit")
	}
	c.Set("k", []byte("v"), 20*time.Millisecond)
	if body, ok := c.Get("k"); !ok || string(body) != "v" {
		t.Fatalf("body = %q, ok = %v", body, ok)
	}
	time.Sleep(30 * time.Millisecond)
	if _, ok := c.Get("k"); ok {
		t.Fatal("expired key hit")
	}
	if c.Len() != 0 {
		t.Fatalf("len = %d after expiry", c.Len())
	}
}

func TestSetSkipsNonPositiveTTL(t *testing.T) {
	c := New(time.Minute, 8)
	c.Set("k", []byte("v"), 0)
	if c.Len() != 0 {
		t.Fatal("zero-ttl entry stored")
	}
}

func TestBoundKeepsEntryCountCapped(t *testing.T) {
	c := New(time.Hour, 3)
	for i := 0; i < 10; i++ {
		c.Set(string(rune('a'+i)), []byte{byte(i)}, time.Hour)
	}
	if n := c.Len(); n > 3 {
		t.Fatalf("len = %d, want <= 3", n)
	}
	// The newest key always survives eviction.
	if _, ok := c.Get("j"); !ok {
		t.Fatal("newest entry evicted")
	}
}

func TestGroupCoalescesConcurrentCalls(t *testing.T) {
	g := &Group{}
	var calls int32
	var wg sync.WaitGroup
	results := make([]int, 8)
	for i := range results {
		wg.Add(1)
		go func(slot int) {
			defer wg.Done()
			body, shared, err := g.Do("key", func() ([]byte, error) {
				atomic.AddInt32(&calls, 1)
				time.Sleep(30 * time.Millisecond)
				return []byte("body"), nil
			})
			if err != nil || string(body) != "body" {
				t.Errorf("slot %d: body %q err %v", slot, body, err)
			}
			if shared {
				results[slot] = 2
			} else {
				results[slot] = 1
			}
		}(i)
	}
	wg.Wait()
	if n := atomic.LoadInt32(&calls); n != 1 {
		t.Fatalf("fn ran %d times, want 1", n)
	}
	leaders := 0
	for _, v := range results {
		if v == 1 {
			leaders++
		}
	}
	if leaders != 1 {
		t.Fatalf("leaders = %d, want 1", leaders)
	}
}

func TestGroupReturnsErrorToEveryWaiter(t *testing.T) {
	g := &Group{}
	want := errors.New("boom")
	done := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _, err := g.Do("key", func() ([]byte, error) {
				<-done
				return nil, want
			})
			if err != want {
				t.Errorf("err = %v, want %v", err, want)
			}
		}()
	}
	close(done)
	wg.Wait()
}

func TestGroupSurvivesPanic(t *testing.T) {
	g := &Group{}
	_, _, err := g.Do("key", func() ([]byte, error) {
		panic("kaboom")
	})
	if err == nil {
		t.Fatal("panic became a nil error")
	}
	// The flight is cleared; a second call runs again.
	body, _, err := g.Do("key", func() ([]byte, error) { return []byte("ok"), nil })
	if err != nil || string(body) != "ok" {
		t.Fatalf("retry: body %q err %v", body, err)
	}
}

func TestGroupSeparatesKeys(t *testing.T) {
	g := &Group{}
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		body, _, _ := g.Do("a", func() ([]byte, error) { return []byte("A"), nil })
		if string(body) != "A" {
			t.Errorf("a body = %q", body)
		}
	}()
	go func() {
		defer wg.Done()
		body, _, _ := g.Do("b", func() ([]byte, error) { return []byte("B"), nil })
		if string(body) != "B" {
			t.Errorf("b body = %q", body)
		}
	}()
	wg.Wait()
}
