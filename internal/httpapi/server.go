// Package httpapi serves the resolve API: health, provider list, and
// POST /v1/resolve speaking the AniKitty extension protocol.
package httpapi

import (
	_ "embed"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"strconv"
	"time"

	"github.com/fampep/anifuse/internal/protocol"
	"github.com/fampep/anifuse/internal/providers"
	"github.com/fampep/anifuse/internal/resolve"
)

//go:embed web/index.html
var landingPage []byte

// Server carries the provider registry.
type Server struct {
	registry *providers.Registry
}

// NewServer builds the handler on the bundled registry.
func NewServer(registry *providers.Registry) *Server {
	return &Server{registry: registry}
}

// Handler returns the routed mux.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", s.handleLanding)
	mux.HandleFunc("GET /healthz", s.handleHealth)
	mux.HandleFunc("GET /v1/providers", s.handleProviders)
	mux.HandleFunc("GET /v1/latest", s.handleLatest)
	mux.HandleFunc("POST /v1/resolve", s.handleResolve)
	return s.logRequests(mux)
}

// handleLanding serves the clean documentation page at the root.
func (s *Server) handleLanding(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(landingPage)
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"status":    "ok",
		"providers": len(s.registry.Providers()),
	})
}

func (s *Server) handleProviders(w http.ResponseWriter, r *http.Request) {
	list := s.registry.Providers()
	out := make([]protocol.ProviderInfo, 0, len(list))
	for _, p := range list {
		out = append(out, protocol.ProviderInfo{
			ID:      p.ID(),
			Name:    p.Name(),
			SiteURL: p.SiteURL(),
			Bundled: true,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"providers": out})
}

// handleLatest aggregates the newest-episodes feeds of the bundled
// providers, optionally pinned to one with ?provider=.
func (s *Server) handleLatest(w http.ResponseWriter, r *http.Request) {
	providerID := r.URL.Query().Get("provider")
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	entries, err := s.registry.Latest(r.Context(), providerID, limit)
	if err != nil {
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, protocol.LatestResponse{Latest: entries})
}

func (s *Server) handleResolve(w http.ResponseWriter, r *http.Request) {
	var req protocol.ResolveRequest
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
	if err := dec.Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	if req.AnilistID <= 0 || req.Episode <= 0 {
		writeError(w, http.StatusBadRequest, "anilistId and episode are required")
		return
	}
	if req.Audio == "" {
		req.Audio = "sub"
	}
	sources, err := s.registry.Resolve(r.Context(), resolve.Request{
		AnilistID: req.AnilistID,
		Episode:   req.Episode,
		Audio:     req.Audio,
		TitleHint: req.TitleHint,
		Provider:  req.Provider,
	})
	if err != nil {
		status := http.StatusBadGateway
		var typed *resolve.Error
		if errors.As(err, &typed) {
			switch typed.Kind {
			case resolve.KindMissing:
				status = http.StatusNotFound
			case resolve.KindBlocked, resolve.KindUnplayable:
				status = http.StatusBadGateway
			}
		}
		writeError(w, status, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, protocol.ResolveResponse{Sources: sources})
}

func (s *Server) logRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		next.ServeHTTP(w, r)
		log.Printf("%s %s %s", r.Method, r.URL.Path, time.Since(start).Round(time.Millisecond))
	})
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func writeError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, protocol.ErrorResponse{Error: message})
}
