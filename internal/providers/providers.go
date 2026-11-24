// Package providers is the resolver registry. Every anime site behind the
// API implements resolve.Provider; the registry tries them in order when a
// request does not pin one.
package providers

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"

	"github.com/fampep/anifuse/internal/protocol"
	"github.com/fampep/anifuse/internal/providers/anigato"
	"github.com/fampep/anifuse/internal/providers/aniwaves"
	"github.com/fampep/anifuse/internal/providers/aninz"
	"github.com/fampep/anifuse/internal/providers/anihosts"
	"github.com/fampep/anifuse/internal/providers/animegg"
	"github.com/fampep/anifuse/internal/providers/gogoanime"
	"github.com/fampep/anifuse/internal/providers/animex"
	"github.com/fampep/anifuse/internal/providers/kaa"
	"github.com/fampep/anifuse/internal/providers/mkissa"
	"github.com/fampep/anifuse/internal/providers/xanime"
	"github.com/fampep/anifuse/internal/resolve"
)

// Registry holds the bundled providers and resolves through them.
type Registry struct {
	list []resolve.Provider
}

// NewRegistry wires the bundled providers in failover order.
func NewRegistry() *Registry {
	return &Registry{
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
		},
	}
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

// Resolve tries the pinned provider, or every provider in order when none
// is pinned, and returns the first non-empty source list.
func (r *Registry) Resolve(ctx context.Context, req resolve.Request) ([]protocol.Source, error) {	var attempts []resolve.Provider
	if req.Provider != "" {
		p := r.Get(req.Provider)
		if p == nil {
			return nil, &resolve.Error{Kind: resolve.KindBlocked, Message: fmt.Sprintf("unknown provider %q", req.Provider)}
		}
		attempts = []resolve.Provider{p}
	} else {
		attempts = r.list
	}

	var failures []string
	var first *resolve.Error
	for _, p := range attempts {
		sources, err := p.Resolve(ctx, req)
		if err == nil && len(sources) > 0 {
			return sources, nil
		}
		if err != nil {
			failures = append(failures, p.ID()+": "+err.Error())
			if first == nil {
				if typed, ok := err.(*resolve.Error); ok {
					first = typed
				} else {
					first = &resolve.Error{Kind: resolve.KindUnplayable, Message: err.Error()}
				}
			}
			continue
		}
		failures = append(failures, p.ID()+": empty sources")
	}
	if first != nil {
		return nil, first
	}
	return nil, &resolve.Error{Kind: resolve.KindMissing, Message: "no provider could resolve this episode (" + strings.Join(failures, "; ") + ")"}
}

// Latest aggregates the newest-episodes feeds of every provider that has
// one. Providers are merged round-robin so the top of the list shows every
// site, not the first provider's whole page. A pinned provider limits the
// list to that site; limit caps the total entries returned.
func (r *Registry) Latest(ctx context.Context, providerID string, limit int) ([]protocol.LatestEntry, error) {
	if limit <= 0 {
		limit = 40
	}
	if limit > 200 {
		limit = 200
	}
	attempts := r.list
	if providerID != "" {
		p := r.Get(providerID)
		if p == nil {
			return nil, &resolve.Error{Kind: resolve.KindBlocked, Message: fmt.Sprintf("unknown provider %q", providerID)}
		}
		attempts = []resolve.Provider{p}
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
			if err != nil || len(entries) == 0 {
				return
			}
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
