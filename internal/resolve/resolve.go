// Package resolve holds the shared resolver contract: the request, the
// typed failures, and the Provider interface every anime site implements.
// Concrete providers live under internal/providers and the registry wires
// them, so the contract sits here to keep the import graph acyclic.
package resolve

import (
	"context"
	"fmt"

	"github.com/fampep/anifuse/internal/protocol"
)

// Request is the provider-facing resolve input.
type Request struct {
	AnilistID int
	Episode   int
	Audio     string
	TitleHint string
	// Provider pins one bundled resolver. Empty asks the registry to try
	// them in order. Providers ignore it.
	Provider string
}

// ErrorKind tells the HTTP layer which status a failed resolve should get.
type ErrorKind int

const (
	// KindMissing: the site does not have this episode.
	KindMissing ErrorKind = iota
	// KindBlocked: the site could not be reached or challenged the request.
	KindBlocked
	// KindUnplayable: the site answered but named no playable stream.
	KindUnplayable
)

// Error is a typed resolve failure.
type Error struct {
	Kind    ErrorKind
	Message string
}

func (e *Error) Error() string { return e.Message }

// Missing builds a KindMissing error for a provider name.
func Missing(name, detail string) *Error {
	return &Error{Kind: KindMissing, Message: fmt.Sprintf("%s does not have this episode. (%s)", name, detail)}
}

// Blocked builds a KindBlocked error for a provider name.
func Blocked(name, detail string) *Error {
	return &Error{Kind: KindBlocked, Message: fmt.Sprintf("%s could not be reached. (%s)", name, detail)}
}

// Unplayable builds a KindUnplayable error for a provider name.
func Unplayable(name, detail string) *Error {
	return &Error{Kind: KindUnplayable, Message: fmt.Sprintf("%s does not serve a stream this player can open. (%s)", name, detail)}
}

// Provider is one bundled anime site resolver.
type Provider interface {
	ID() string
	Name() string
	SiteURL() string
	Resolve(ctx context.Context, req Request) ([]protocol.Source, error)
}

// LatestProvider is an optional capability: providers that can list the
// newest episodes on their site implement it. The registry skips providers
// that do not.
type LatestProvider interface {
	Latest(ctx context.Context, limit int) ([]protocol.LatestEntry, error)
}
