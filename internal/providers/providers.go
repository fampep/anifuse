// Package providers is the resolver registry. Every anime site behind the
// API implements resolve.Provider. An unpinned request asks every healthy
// provider in parallel, merges all their streams and ranks them highest
// quality first; a pinned request uses only that provider.
package providers

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strconv"
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
	// settleWindow is how long a resolve keeps collecting streams after the
	// first success so the answer lists every provider's streams while
	// staying fast.
	settleWindow = 1200 * time.Millisecond
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
			// gotaku is disabled: its CDN refuses datacenter IPs, so the
			// hosted API cannot play it. The package, crypto and the
			// /v1/gotaku transform proxy stay for when that changes.
			// gotaku.New(),
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
// none is pinned. Streams from every provider that answers are collected
// (a short settle window after the first success keeps the answer fast),
// deduplicated and ranked highest quality first. The winner is the
// provider of the top-ranked stream. All providers run with the request
// context, so a returned answer cancels the stragglers.
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
				// The settle window closed and the answer was already sent,
				// so this attempt was cut short; it must not count against
				// this provider's health.
				results <- result{id: p.ID(), cancelled: true}
				return
			}
			r.record(p.ID(), srcs, e)
			results <- result{id: p.ID(), sources: srcs, err: e}
		}(p)
	}

	collected := make(map[string][]protocol.Source, len(attempts))
	typedErrs := make(map[string]*resolve.Error, len(attempts))
	plainErrs := make(map[string]error, len(attempts))
	var settle *time.Timer
	var settleC <-chan time.Time
	haveSuccess := false
	var failures []string
	for range attempts {
		select {
		case res := <-results:
			if res.cancelled {
				continue
			}
			if res.err == nil && len(res.sources) > 0 {
				collected[res.id] = res.sources
				if !haveSuccess {
					haveSuccess = true
					// Give other healthy providers a brief moment to add
					// their streams, then answer with everything collected.
					settle = time.NewTimer(settleWindow)
					settleC = settle.C
				}
				continue
			}
			if res.err != nil {
				var typed *resolve.Error
				if errors.As(res.err, &typed) {
					typedErrs[res.id] = typed
				} else {
					plainErrs[res.id] = res.err
				}
				failures = append(failures, res.id+": "+res.err.Error())
			} else {
				failures = append(failures, res.id+": empty sources")
			}
		case <-settleC:
			// The window after the first success is over; answer now with
			// what arrived instead of waiting on slow stragglers.
			if settle != nil {
				settle.Stop()
			}
			out, top := r.aggregate(collected)
			return out, top, nil
		}
	}
	if settle != nil {
		settle.Stop()
	}
	if len(collected) > 0 {
		out, top := r.aggregate(collected)
		return out, top, nil
	}
	// Nothing succeeded. Choose the most informative failure in failover
	// priority order so the HTTP status stays deterministic regardless of
	// arrival order.
	var firstErr *resolve.Error
	var plainErr error
	for _, p := range attempts {
		if e, ok := typedErrs[p.ID()]; ok && firstErr == nil {
			firstErr = e
		}
	}
	if firstErr == nil {
		for _, p := range attempts {
			if e, ok := plainErrs[p.ID()]; ok && plainErr == nil {
				plainErr = e
			}
		}
	}
	if firstErr != nil {
		return nil, "", firstErr
	}
	if plainErr != nil {
		return nil, "", &resolve.Error{Kind: resolve.KindUnplayable, Message: plainErr.Error()}
	}
	return nil, "", &resolve.Error{Kind: resolve.KindMissing, Message: "no provider could resolve this episode (" + strings.Join(failures, "; ") + ")"}
}

// aggregate merges every collected source list, drops duplicate URLs and
// ranks the streams highest quality first. The winner is the provider of
// the top-ranked stream.
func (r *Registry) aggregate(collected map[string][]protocol.Source) ([]protocol.Source, string) {
	type ranked struct {
		src    protocol.Source
		id     string
		height int
	}
	var rows []ranked
	seen := make(map[string]bool)
	for _, p := range r.list {
		for _, src := range collected[p.ID()] {
			if seen[src.URL] {
				continue
			}
			seen[src.URL] = true
			rows = append(rows, ranked{src: src, id: p.ID(), height: qualityHeight(src)})
		}
	}
	sort.SliceStable(rows, func(i, j int) bool {
		if rows[i].height != rows[j].height {
			return rows[i].height > rows[j].height
		}
		return rows[i].src.Label < rows[j].src.Label
	})
	out := make([]protocol.Source, len(rows))
	winner := ""
	for i, row := range rows {
		out[i] = row.src
		if i == 0 {
			winner = row.id
		}
	}
	return out, winner
}

// qualityHeight ranks a source's stated quality for the top-down sort,
// mirroring the AniKitty app's label scoring.
func qualityHeight(src protocol.Source) int {
	probe := strings.ToLower(src.Quality + " " + src.Label + " " + src.QualityRating)
	if m := qualityResRe.FindStringSubmatch(probe); m != nil {
		if h, err := strconv.Atoi(m[1]); err == nil {
			return h
		}
	}
	if m := qualityNumRe.FindStringSubmatch(probe); m != nil {
		if h, err := strconv.Atoi(m[1]); err == nil {
			return h
		}
	}
	switch {
	case strings.Contains(probe, "4k") || strings.Contains(probe, "ultra") || strings.Contains(probe, "2160"):
		return 2160
	case strings.Contains(probe, "fhd") || strings.Contains(probe, "hd") || strings.Contains(probe, "high"):
		return 1080
	case strings.Contains(probe, "sd") || strings.Contains(probe, "low"):
		return 480
	default:
		return 0
	}
}

var (
	qualityResRe = regexp.MustCompile(`(\d{3,4})\s*p`)
	qualityNumRe = regexp.MustCompile(`\b(2160|1440|1080|720|480|360|240)\b`)
)

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
