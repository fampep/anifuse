package providers

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/fampep/anifuse/internal/protocol"
	"github.com/fampep/anifuse/internal/resolve"
)

// fakeProvider is a scriptable provider for registry behavior tests.
type fakeProvider struct {
	id     string
	calls  int32
	delay  time.Duration
	srcs   []protocol.Source
	err    error
	latest []protocol.LatestEntry
}

func (f *fakeProvider) ID() string      { return f.id }
func (f *fakeProvider) Name() string    { return "fake " + f.id }
func (f *fakeProvider) SiteURL() string { return "https://example.com/" + f.id }

func (f *fakeProvider) Resolve(ctx context.Context, req resolve.Request) ([]protocol.Source, error) {
	atomic.AddInt32(&f.calls, 1)
	if f.delay > 0 {
		select {
		case <-time.After(f.delay):
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	return f.srcs, f.err
}

func (f *fakeProvider) Latest(ctx context.Context, limit int) ([]protocol.LatestEntry, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.latest, nil
}

func newFakeRegistry(providers ...resolve.Provider) *Registry {
	r := &Registry{
		list:      providers,
		startedAt: time.Now(),
		health:    make(map[string]*healthStat),
	}
	for _, p := range providers {
		r.health[p.ID()] = &healthStat{}
	}
	return r
}

func TestResolvePinnedUsesOnlyThatProvider(t *testing.T) {
	a := &fakeProvider{id: "a", srcs: []protocol.Source{{URL: "https://a.example/1.m3u8"}}}
	b := &fakeProvider{id: "b", srcs: []protocol.Source{{URL: "https://b.example/1.m3u8"}}}
	r := newFakeRegistry(a, b)

	sources, winner, err := r.Resolve(context.Background(), resolve.Request{Provider: "a"})
	if err != nil || winner != "a" || len(sources) != 1 || sources[0].URL != "https://a.example/1.m3u8" {
		t.Fatalf("sources=%v winner=%q err=%v", sources, winner, err)
	}
	if n := atomic.LoadInt32(&b.calls); n != 0 {
		t.Fatalf("pinned resolve called %d extra providers", n)
	}
}

func TestResolveUnknownProvider(t *testing.T) {
	r := newFakeRegistry(&fakeProvider{id: "a"})
	_, _, err := r.Resolve(context.Background(), resolve.Request{Provider: "nope"})
	if err == nil || !strings.Contains(err.Error(), "unknown provider") {
		t.Fatalf("err = %v", err)
	}
}

func TestResolveRacesAndReturnsFirstSuccess(t *testing.T) {
	slow := &fakeProvider{id: "slow", delay: 200 * time.Millisecond, srcs: []protocol.Source{{URL: "https://slow.example/1.m3u8"}}}
	fast := &fakeProvider{id: "fast", delay: 10 * time.Millisecond, srcs: []protocol.Source{{URL: "https://fast.example/1.m3u8"}}}
	r := newFakeRegistry(slow, fast)

	start := time.Now()
	sources, winner, err := r.Resolve(context.Background(), resolve.Request{})
	elapsed := time.Since(start)
	if err != nil || winner != "fast" || len(sources) != 1 || sources[0].URL != "https://fast.example/1.m3u8" {
		t.Fatalf("sources=%v winner=%q err=%v", sources, winner, err)
	}
	if elapsed > 150*time.Millisecond {
		t.Fatalf("race took %v; the slow provider stalled the result", elapsed)
	}
	// Both providers were attempted in parallel.
	if n := atomic.LoadInt32(&slow.calls); n != 1 {
		t.Fatalf("slow called %d times", n)
	}
}

// waitHealth polls until a provider's health satisfies want. Health updates
// from racers that lost to a winner land just after the winner returns.
func waitHealth(t *testing.T, r *Registry, id string, want func(Health) bool) Health {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		for _, h := range r.Health() {
			if h.ID == id && want(h) {
				return h
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("health for %s never satisfied", id)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestResolveRecordsHealth(t *testing.T) {
	ok := &fakeProvider{id: "ok", srcs: []protocol.Source{{URL: "https://ok.example/1.m3u8"}}}
	missing := &fakeProvider{id: "missing", err: resolve.Missing("missing", "nope")}
	dead := &fakeProvider{id: "dead", err: resolve.Blocked("dead", "nope")}
	r := newFakeRegistry(ok, missing, dead)

	_, winner, err := r.Resolve(context.Background(), resolve.Request{})
	if err != nil || winner != "ok" {
		t.Fatalf("winner=%q err=%v", winner, err)
	}
	okHealth := waitHealth(t, r, "ok", func(h Health) bool { return h.Successes == 1 })
	if okHealth.Status != "ok" {
		t.Fatalf("ok health = %+v", okHealth)
	}
	missingHealth := waitHealth(t, r, "missing", func(h Health) bool { return h.Misses == 1 })
	if missingHealth.Status != "ok" {
		t.Fatalf("missing health = %+v", missingHealth)
	}
	deadHealth := waitHealth(t, r, "dead", func(h Health) bool { return h.Errors == 1 })
	if deadHealth.Status != "ok" {
		t.Fatalf("dead health = %+v", deadHealth)
	}
}

func TestResolveCooldownDropsFailingProvider(t *testing.T) {
	good := &fakeProvider{id: "good", srcs: []protocol.Source{{URL: "https://good.example/1.m3u8"}}}
	bad := &fakeProvider{id: "bad", err: resolve.Blocked("bad", "nope")}
	r := newFakeRegistry(bad, good)

	// Pin the failing provider three times to trip its cooldown.
	for i := 0; i < cooldownErrors; i++ {
		_, _, err := r.Resolve(context.Background(), resolve.Request{Provider: "bad"})
		if err == nil {
			t.Fatal("bad provider unexpectedly resolved")
		}
	}
	health := map[string]Health{}
	for _, h := range r.Health() {
		health[h.ID] = h
	}
	if health["bad"].Status != "down" || health["bad"].CooldownSeconds <= 0 {
		t.Fatalf("bad health = %+v", health["bad"])
	}
	// An unpinned resolve now skips the cooled provider.
	_, winner, err := r.Resolve(context.Background(), resolve.Request{})
	if err != nil || winner != "good" {
		t.Fatalf("winner=%q err=%v", winner, err)
	}
	if n := atomic.LoadInt32(&bad.calls); n != cooldownErrors {
		t.Fatalf("cooled provider called %d times, want %d", n, cooldownErrors)
	}
	// A pinned request still reaches it (user asked for it explicitly).
	_, _, err = r.Resolve(context.Background(), resolve.Request{Provider: "bad"})
	if err == nil {
		t.Fatal("pinned bad provider unexpectedly resolved")
	}
	if n := atomic.LoadInt32(&bad.calls); n != cooldownErrors+1 {
		t.Fatalf("pinned cooled provider calls = %d", n)
	}
}

func TestResolveNoProviderSucceedsReportsFirstError(t *testing.T) {
	a := &fakeProvider{id: "a", err: resolve.Missing("a", "nope")}
	b := &fakeProvider{id: "b", err: errors.New("plain failure")}
	r := newFakeRegistry(a, b)
	_, _, err := r.Resolve(context.Background(), resolve.Request{})
	if err == nil {
		t.Fatal("expected an error")
	}
	typed, ok := err.(*resolve.Error)
	if !ok || typed.Kind != resolve.KindMissing {
		t.Fatalf("err = %v (%T), want KindMissing", err, err)
	}
}

func TestLatestMergesRoundRobin(t *testing.T) {
	a := &fakeProvider{id: "a", latest: []protocol.LatestEntry{{Title: "A1"}, {Title: "A2"}}}
	b := &fakeProvider{id: "b", latest: []protocol.LatestEntry{{Title: "B1"}}}
	r := newFakeRegistry(a, b)
	entries, err := r.Latest(context.Background(), "", 10)
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if len(entries) != 3 || entries[0].Title != "A1" || entries[1].Title != "B1" || entries[2].Title != "A2" {
		t.Fatalf("entries = %+v", entries)
	}
	if entries[0].Provider != "a" || entries[1].Provider != "b" {
		t.Fatalf("provider tags = %+v", entries)
	}
}

func TestLatestPinnedAndLimited(t *testing.T) {
	a := &fakeProvider{id: "a", latest: []protocol.LatestEntry{{Title: "A1"}, {Title: "A2"}, {Title: "A3"}}}
	b := &fakeProvider{id: "b", latest: []protocol.LatestEntry{{Title: "B1"}}}
	r := newFakeRegistry(a, b)
	entries, err := r.Latest(context.Background(), "a", 2)
	if err != nil || len(entries) != 2 || entries[0].Title != "A1" {
		t.Fatalf("entries = %+v err = %v", entries, err)
	}
	if _, err := r.Latest(context.Background(), "nope", 2); err == nil {
		t.Fatal("unknown provider accepted")
	}
}

func TestResolveOrderSkipsNothingWhenAllCooled(t *testing.T) {
	bad1 := &fakeProvider{id: "bad1", err: resolve.Blocked("bad1", "nope")}
	bad2 := &fakeProvider{id: "bad2", err: resolve.Blocked("bad2", "nope")}
	r := newFakeRegistry(bad1, bad2)
	for i := 0; i < cooldownErrors; i++ {
		_, _, _ = r.Resolve(context.Background(), resolve.Request{Provider: "bad1"})
		_, _, _ = r.Resolve(context.Background(), resolve.Request{Provider: "bad2"})
	}
	order := r.resolveOrder()
	if len(order) != 2 {
		t.Fatalf("order = %d providers, want both cooled providers", len(order))
	}
}

func TestFanoutRunsAllProvidersConcurrently(t *testing.T) {
	a := &fakeProvider{id: "a", delay: 50 * time.Millisecond}
	b := &fakeProvider{id: "b", delay: 50 * time.Millisecond, srcs: []protocol.Source{{URL: "https://b.example/1.m3u8"}}}
	r := newFakeRegistry(a, b)
	start := time.Now()
	_, _, err := r.Resolve(context.Background(), resolve.Request{})
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	// Two 50ms providers in sequence would take ~100ms; in parallel the
	// whole race settles near the slower one's single delay.
	if elapsed := time.Since(start); elapsed > 130*time.Millisecond {
		t.Fatalf("providers did not run in parallel: %v", elapsed)
	}
}
