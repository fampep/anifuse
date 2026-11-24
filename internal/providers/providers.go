// Package providers is the resolver registry. Every anime site behind the
// API implements resolve.Provider. An unpinned request races every healthy
// provider in parallel and returns the fastest working stream; a pinned
// request uses only that provider.
package providers

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/fampep/anifuse/internal/protocol"
	"github.com/fampep/anifuse/internal/providers/anigato"
	"github.com/fampep/anifuse/internal/providers/anihosts"
	"github.com/fampep/anifuse/internal/providers/anikoto"
	"github.com/fampep/anifuse/internal/providers/animegg"
	"github.com/fampep/anifuse/internal/providers/animex"
	"github.com/fampep/anifuse/internal/providers/aninz"
	"github.com/fampep/anifuse/internal/providers/aniwaves"
	"github.com/fampep/anifuse/internal/providers/anizone"
	"github.com/fampep/anifuse/internal/providers/gogoanime"
	"github.com/fampep/anifuse/internal/providers/gotaku"
	"github.com/fampep/anifuse/internal/providers/kaa"
	"github.com/fampep/anifuse/internal/providers/mkissa"
	"github.com/fampep/anifuse/internal/providers/xanime"
	"github.com/fampep/anifuse/internal/resolve"
)

const (
	// cooldownErrors is how many consecutive blocked failures put a provider
	// into cooldown for unpinned races.
	cooldownErrors = 3
	// cooldownTime is how long a failing provider stays out of unpinned races.
	cooldownTime = 60 * time.Second
	// degradedRate marks a provider degraded when its reachability rate over
	// at least three attempts sits below this fraction.
	degradedRate = 0.34
)

// Registry holds the bundled providers, their live health, and the resolve
// fan-out.
type Registry struct {
	list      []resolve.Provider
	startedAt time.Time

	mu     sync.Mutex
	health map[string]*healthStat
}

type healthStat struct {
	attempts          int
	successes         int
	misses            int
	errors            int
	consecutiveErrors int
	cooldownUntil     time.Time
}

// NewRegistry wires the bundled providers in failover order. The order only
// matters as a tie-breaker: unpinned requests race providers in parallel.
func NewRegistry() *Registry {
	r := &Registry{
		list: []resolve.Provider{
			anihosts.New(),
			aninz.New(),
			aniwaves.New(),
			anigato.New(),
			animegg.New(),
			kaa.New(),
			mkissa.New(),
			xanime.New(),
			animex.New(),
			gogoanime.New(),
			anizone.New(),
			anikoto.New(),
			gotaku.New(),
		},
		startedAt: time.Now(),
		health:    make(map[string]*healthStat),
	}
	for _, p := range r.list {
		r.health[p.ID()] = &healthStat{}
	}
	return r
}

// Providers returns the bundled list, sorted by name for the API.
func (r *Registry) Providers() []resolve.Provider {
	out := make([]resolve.Provider, len(r.list))
	copy(out, r.list)
	sort.SliceStable(out, func(i, j int) bool { return out[i].Name() < out[j].Name() })
	return out
}

// Get returns the provider with id, or nil.
func (r *Registry) Get(id string) resolve.Provider {
	for _, p := range r.list {
		if p.ID() == id {
			return p
		}
	}
	return nil
}

// StartedAt is the registry creation time, used for uptime reporting.
func (r *Registry) StartedAt() time.Time { return r.startedAt }

// Health is one provider's live stats for the API.
type Health struct {
	ID              string
	Attempts        int
	Successes       int
	Misses          int
	Errors          int
	SuccessRate     float64
	Status          string // ok | degraded | down
	CooldownSeconds int
}

// Health snapshots every provider's stats, sorted by id.
func (r *Registry) Health() []Health {
	r.mu.Lock()
	defer r.mu.Unlock()
	now := time.Now()
	out := make([]Health, 0, len(r.list))
	for _, p := range r.list {
		s := r.health[p.ID()]
		h := Health{ID: p.ID(), Status: "ok"}
		if s == nil {
			out = append(out, h)
			continue
		}
		h.Attempts = s.attempts
		h.Successes = s.successes
		h.Misses = s.misses
		h.Errors = s.errors
		if s.attempts > 0 {
			h.SuccessRate = float64(s.successes+s.misses) / float64(s.attempts)
		}
		switch {
		case now.Before(s.cooldownUntil):
			h.Status = "down"
			h.CooldownSeconds = int(time.Until(s.cooldownUntil).Seconds()) + 1
		case s.attempts >= 3 && h.SuccessRate < degradedRate:
			h.Status = "degraded"
		}
		out = append(out, h)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

func (r *Registry) recordSuccess(id string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	s := r.health[id]
	if s == nil {
		return
	}
	s.attempts++
	s.successes++
	s.consecutiveErrors = 0
	s.cooldownUntil = time.Time{}
}

func (r *Registry) recordMiss(id string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	s := r.health[id]
	if s == nil {
		return
	}
	s.attempts++
	s.misses++
	// A miss proves the site answered, so it also clears a failing streak.
	s.consecutiveErrors = 0
	s.cooldownUntil = time.Time{}
}

func (r *Registry) recordError(id string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	s := r.health[id]
	if s == nil {
		return
	}
	s.attempts++
	s.errors++
	s.consecutiveErrors++
	if s.consecutiveErrors >= cooldownErrors {
		s.cooldownUntil = time.Now().Add(cooldownTime)
	}
}

// resolveOrder ranks providers for an unpinned race: reachable providers
// first (by reachability rate), cooled providers last. Providers in cooldown
// are dropped unless every provider is cooling, in which case all run.
func (r *Registry) resolveOrder() []resolve.Provider {
	r.mu.Lock()
	stats := make(map[string]healthStat, len(r.list))
	for id, s := range r.health {
		stats[id] = *s
	}
	r.mu.Unlock()

	now := time.Now()
	type ranked struct {
		p    resolve.Provider
		cool bool
		rate float64
		idx  int
	}
	rows := make([]ranked, 0, len(r.list))
	allCool := true
	for i, p := range r.list {
		s := stats[p.ID()]
		cool := now.Before(s.cooldownUntil)
		if !cool {
			allCool = false
		}
		rate := 0.0
		if s.attempts > 0 {
			rate = float64(s.successes+s.misses) / float64(s.attempts)
		}
		rows = append(rows, ranked{p: p, cool: cool, rate: rate, idx: i})
	}
	sort.SliceStable(rows, func(i, j int) bool {
		if allCool {
			return rows[i].idx < rows[j].idx
		}
		if rows[i].cool != rows[j].cool {
			return !rows[i].cool
		}
		if rows[i].rate != rows[j].rate {
			return rows[i].rate > rows[j].rate
		}
		return rows[i].idx < rows[j].idx
	})
	out := make([]resolve.Provider, 0, len(rows))
	for _, row := range rows {
		if row.cool && !allCool {
			continue
		}
		out = append(out, row.p)
	}
	if len(out) == 0 {
		for _, p := range r.list {
			out = append(out, p)
		}
	}
	return out
}

// Resolve tries the pinned provider, or races every healthy provider when
// none is pinned. The fastest non-empty source list wins and the winner's id
// is returned alongside it. All providers run with the request context, so a
// returned winner cancels the losers.
func (r *Registry) Resolve(ctx context.Context, req resolve.Request) (sources []protocol.Source, winner string, err error) {
	var attempts []resolve.Provider
	if req.Provider != "" {
		p := r.Get(req.Provider)
		if p == nil {
			return nil, "", &resolve.Error{Kind: resolve.KindBlocked, Message: fmt.Sprintf("unknown provider %q", req.Provider)}
		}
		attempts = []resolve.Provider{p}
	} else {
		attempts = r.resolveOrder()
	}

	type result struct {
		id        string
		sources   []protocol.Source
		err       error
		cancelled bool
	}
	results := make(chan result, len(attempts))
	for _, p := range attempts {
		go func(p resolve.Provider) {
			srcs, e := p.Resolve(ctx, req)
			if errors.Is(e, context.Canceled) {
				// A faster provider already won the race, so this attempt
				// was cut short; it must not count against this provider's
				// health.
				results <- result{id: p.ID(), cancelled: true}
				return
			}
			r.record(p.ID(), srcs, e)
			results <- result{id: p.ID(), sources: srcs, err: e}
		}(p)
	}

	collected := make(map[string]result, len(attempts))
	for range attempts {
		res := <-results
		if res.cancelled {
			continue
		}
		if res.err == nil && len(res.sources) > 0 {
			return res.sources, res.id, nil
		}
		collected[res.id] = res
	}
	// Nothing won the race. Choose the most informative failure in
	// failover priority order so the HTTP status stays deterministic.
	var first *resolve.Error
	var plain error
	var failures []string
	for _, p := range attempts {
		res, ok := collected[p.ID()]
		if !ok {
			continue
		}
		if res.err == nil {
			failures = append(failures, p.ID()+": empty sources")
			continue
		}
		var typed *resolve.Error
		if errors.As(res.err, &typed) {
			if first == nil {
				first = typed
			}
		} else if plain == nil {
			plain = res.err
		}
		failures = append(failures, p.ID()+": "+res.err.Error())
	}
	if first != nil {
		return nil, "", first
	}
	if plain != nil {
		return nil, "", &resolve.Error{Kind: resolve.KindUnplayable, Message: plain.Error()}
	}
	return nil, "", &resolve.Error{Kind: resolve.KindMissing, Message: "no provider could resolve this episode (" + strings.Join(failures, "; ") + ")"}
}

// record folds one attempt into the provider's health counters. A miss
// proves the site answered, so it also clears a failing streak; only
// blocked failures count toward cooldown.
func (r *Registry) record(id string, sources []protocol.Source, err error) {
	switch {
	case err == nil && len(sources) > 0:
		r.recordSuccess(id)
	case err == nil:
		r.recordMiss(id)
	default:
		var typed *resolve.Error
		if errors.As(err, &typed) && typed.Kind != resolve.KindBlocked {
			r.recordMiss(id)
		} else {
			r.recordError(id)
		}
	}
}

// Latest aggregates the newest-episodes feeds of every provider that has
// one. Providers are merged round-robin so the top of the list shows every
// site, not the first provider's whole page. A pinned provider limits the
// list to that site; limit caps the total entries returned. Feed failures
// feed the same health counters as resolves.
func (r *Registry) Latest(ctx context.Context, providerID string, limit int) ([]protocol.LatestEntry, error) {
	if limit <= 0 {
		limit = 40
	}
	if limit > 200 {
		limit = 200
	}
	var attempts []resolve.Provider
	if providerID != "" {
		p := r.Get(providerID)
		if p == nil {
			return nil, &resolve.Error{Kind: resolve.KindBlocked, Message: fmt.Sprintf("unknown provider %q", providerID)}
		}
		attempts = []resolve.Provider{p}
	} else {
		attempts = r.resolveOrder()
	}

	type feed struct {
		id      string
		entries []protocol.LatestEntry
	}
	// Fetch every feed in parallel so one slow site does not stall the
	// whole list; the round-robin below restores interleaved order.
	feeds := make([]feed, len(attempts))
	var wg sync.WaitGroup
	for i, p := range attempts {
		feeder, ok := p.(resolve.LatestProvider)
		if !ok {
			continue
		}
		wg.Add(1)
		go func(slot int, feeder resolve.LatestProvider, id string) {
			defer wg.Done()
			entries, err := feeder.Latest(ctx, limit)
			switch {
			case err != nil:
				r.recordError(id)
				return
			case len(entries) == 0:
				r.recordMiss(id)
				return
			}
			r.recordSuccess(id)
			for j := range entries {
				entries[j].Provider = id
			}
			feeds[slot] = feed{id: id, entries: entries}
		}(i, feeder, p.ID())
	}
	wg.Wait()
	ordered := make([]feed, 0, len(attempts))
	for _, f := range feeds {
		if len(f.entries) > 0 {
			ordered = append(ordered, f)
		}
	}

	var out []protocol.LatestEntry
	for round := 0; len(out) < limit && len(ordered) > 0; round++ {
		added := false
		for i := range ordered {
			if len(out) >= limit {
				break
			}
			if round >= len(ordered[i].entries) {
				continue
			}
			out = append(out, ordered[i].entries[round])
			added = true
		}
		if !added {
			break
		}
	}
	return out, nil
}
