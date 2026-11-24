// Package providers is the resolver registry. Every anime site behind the
// API implements resolve.Provider; the registry tries them in order when a
// request does not pin one.
package providers

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/fampep/anifuse/internal/protocol"
	"github.com/fampep/anifuse/internal/providers/anigato"
	"github.com/fampep/anifuse/internal/providers/anihosts"
	"github.com/fampep/anifuse/internal/providers/animegg"
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
			anigato.New(),
			animegg.New(),
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
func (r *Registry) Resolve(ctx context.Context, req resolve.Request) ([]protocol.Source, error) {
	var attempts []resolve.Provider
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
