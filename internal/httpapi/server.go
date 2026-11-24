// Package httpapi serves the resolve API: health, provider list, metrics,
// POST /v1/resolve speaking the AniKitty extension protocol, and the
// aggregated latest-episodes feed. Responses are gzip-compressed when the
// client asks, CORS-open for browser clients, and backed by a TTL cache
// plus singleflight so bursts of identical requests share one upstream
// fetch.
package httpapi

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha1"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/fampep/anifuse/internal/cache"
	"github.com/fampep/anifuse/internal/protocol"
	"github.com/fampep/anifuse/internal/providers"
	"github.com/fampep/anifuse/internal/resolve"
)

//go:embed web/index.html
var landingPage []byte

const (
	resolveCacheTTL       = 10 * time.Minute
	latestCacheTTL        = 3 * time.Minute
	defaultResolveTimeout = 30 * time.Second
)

// Server carries the provider registry, the response caches, and the
// request counters. demo marks a public demo deployment: the flag surfaces
// in /healthz so the landing page can label itself a demo.
type Server struct {
	registry       *providers.Registry
	resolveCache   *cache.Cache
	latestCache    *cache.Cache
	resolveGroup   *cache.Group
	latestGroup    *cache.Group
	startedAt      time.Time
	resolveTimeout time.Duration
	demo           bool

	total            int64
	resolveRequests  int64
	resolveCacheHits int64
	latestRequests   int64
	latestCacheHits  int64
}

// NewServer builds the handler on the bundled registry. demo labels the
// deployment as a public demo in /healthz. Caching knobs come from the
// environment: ANIFUSE_RESOLVE_TIMEOUT (duration, default 30s),
// ANIFUSE_RESOLVE_CACHE_TTL and ANIFUSE_LATEST_CACHE_TTL.
func NewServer(registry *providers.Registry, demo bool) *Server {
	return &Server{
		registry:       registry,
		resolveCache:   cache.New(envDuration("ANIFUSE_RESOLVE_CACHE_TTL", resolveCacheTTL), 2048),
		latestCache:    cache.New(envDuration("ANIFUSE_LATEST_CACHE_TTL", latestCacheTTL), 256),
		resolveGroup:   &cache.Group{},
		latestGroup:    &cache.Group{},
		startedAt:      time.Now(),
		resolveTimeout: envDuration("ANIFUSE_RESOLVE_TIMEOUT", defaultResolveTimeout),
		demo:           demo,
	}
}

// Handler returns the routed mux.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", s.handleLanding)
	mux.HandleFunc("GET /healthz", s.handleHealth)
	mux.HandleFunc("GET /metrics", s.handleMetrics)
	mux.HandleFunc("GET /v1/providers", s.handleProviders)
	mux.HandleFunc("GET /v1/latest", s.handleLatest)
	mux.HandleFunc("POST /v1/resolve", s.handleResolve)
	return s.logRequests(s.cors(mux))
}

// cors opens the machine endpoints to browser clients and answers the
// preflight OPTIONS requests they send.
func (s *Server) cors(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		api := strings.HasPrefix(r.URL.Path, "/v1/") || r.URL.Path == "/healthz" || r.URL.Path == "/metrics"
		if !api {
			next.ServeHTTP(w, r)
			return
		}
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Vary", "Accept-Encoding")
		if r.Method == http.MethodOptions {
			w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
			w.Header().Set("Access-Control-Allow-Headers", "Content-Type")
			w.Header().Set("Access-Control-Max-Age", "86400")
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// handleLanding serves the documentation shell at the root.
func (s *Server) handleLanding(w http.ResponseWriter, r *http.Request) {
	writeBody(w, r, http.StatusOK, "text/html; charset=utf-8", landingPage)
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	health := s.registry.Health()
	unhealthy := make([]string, 0)
	status := "ok"
	for _, h := range health {
		if h.Status != "ok" {
			unhealthy = append(unhealthy, h.ID)
			status = "degraded"
		}
	}
	s.writeJSON(w, r, http.StatusOK, protocol.HealthResponse{
		Status:        status,
		Providers:     len(s.registry.Providers()),
		UptimeSeconds: int64(time.Since(s.startedAt).Seconds()),
		Unhealthy:     unhealthy,
		Demo:          s.demo,
	})
}

func (s *Server) handleProviders(w http.ResponseWriter, r *http.Request) {
	health := make(map[string]providers.Health)
	for _, h := range s.registry.Health() {
		health[h.ID] = h
	}
	list := s.registry.Providers()
	out := make([]protocol.ProviderInfo, 0, len(list))
	for _, p := range list {
		info := protocol.ProviderInfo{
			ID:      p.ID(),
			Name:    p.Name(),
			SiteURL: p.SiteURL(),
			Bundled: true,
			Status:  "ok",
		}
		if h, ok := health[p.ID()]; ok {
			info.Status = h.Status
			info.SuccessRate = h.SuccessRate
			info.Attempts = h.Attempts
			info.CooldownSeconds = h.CooldownSeconds
		}
		out = append(out, info)
	}
	s.writeJSON(w, r, http.StatusOK, map[string]any{"providers": out})
}

// handleLatest serves the aggregated newest-episodes feed. Optional query
// parameters: provider pins one site, limit caps the total (default 40,
// max 200), search filters titles case-insensitively, sort picks
// mixed (round-robin), ep (episode number, high first) or name (title A-Z).
// Responses carry an ETag for 304 revalidation and are cached for a few
// minutes, so the feed stays fast under load.
func (s *Server) handleLatest(w http.ResponseWriter, r *http.Request) {
	atomic.AddInt64(&s.total, 1)
	atomic.AddInt64(&s.latestRequests, 1)
	query := r.URL.Query()
	providerID := query.Get("provider")
	limit, _ := strconv.Atoi(query.Get("limit"))
	search := strings.TrimSpace(query.Get("search"))
	order := query.Get("sort")
	key := strings.Join([]string{providerID, strconv.Itoa(limit), search, order}, "|")

	if body, ok := s.latestCache.Get(key); ok {
		atomic.AddInt64(&s.latestCacheHits, 1)
		w.Header().Set("X-AniFuse-Cache", "HIT")
		s.writeCached(w, r, http.StatusOK, "application/json; charset=utf-8", body, 180)
		return
	}

	body, shared, err := s.latestGroup.Do(key, func() ([]byte, error) {
		// A search scans the widest window available instead of just the
		// requested page, then the output is capped below.
		fetchLimit := limit
		if search != "" {
			fetchLimit = 200
		}
		entries, ferr := s.registry.Latest(r.Context(), providerID, fetchLimit)
		if ferr != nil {
			return nil, ferr
		}
		if search != "" {
			want := strings.ToLower(search)
			kept := entries[:0]
			for _, e := range entries {
				if strings.Contains(strings.ToLower(e.Title), want) {
					kept = append(kept, e)
				}
			}
			entries = kept
		}
		switch order {
		case "ep":
			sortByEpisode(entries)
		case "name":
			sortByTitle(entries)
		}
		if limit > 0 && len(entries) > limit {
			entries = entries[:limit]
		}
		return json.Marshal(protocol.LatestResponse{Latest: entries})
	})
	if err != nil {
		writeError(w, r, http.StatusBadGateway, err.Error())
		return
	}
	s.latestCache.Set(key, body, latestCacheTTL)
	w.Header().Set("X-AniFuse-Cache", cacheTag(shared))
	s.writeCached(w, r, http.StatusOK, "application/json; charset=utf-8", body, 180)
}

func (s *Server) handleResolve(w http.ResponseWriter, r *http.Request) {
	atomic.AddInt64(&s.total, 1)
	atomic.AddInt64(&s.resolveRequests, 1)
	var req protocol.ResolveRequest
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
	if err := dec.Decode(&req); err != nil {
		writeError(w, r, http.StatusBadRequest, "invalid JSON body")
		return
	}
	if req.AnilistID <= 0 || req.Episode <= 0 {
		writeError(w, r, http.StatusBadRequest, "anilistId and episode are required")
		return
	}
	if req.Audio == "" {
		req.Audio = "sub"
	}
	key := strings.Join([]string{
		req.Provider,
		strconv.Itoa(req.AnilistID),
		strconv.Itoa(req.Episode),
		strings.ToLower(strings.TrimSpace(req.Audio)),
		strings.ToLower(strings.TrimSpace(req.TitleHint)),
	}, "|")

	if body, ok := s.resolveCache.Get(key); ok {
		atomic.AddInt64(&s.resolveCacheHits, 1)
		var resp protocol.ResolveResponse
		if err := json.Unmarshal(body, &resp); err == nil {
			resp.Cached = true
			if fresh, err := json.Marshal(resp); err == nil {
				body = fresh
			}
		}
		w.Header().Set("X-AniFuse-Cache", "HIT")
		s.writeCached(w, r, http.StatusOK, "application/json; charset=utf-8", body, 120)
		return
	}

	start := time.Now()
	body, shared, err := s.resolveGroup.Do(key, func() ([]byte, error) {
		ctx, cancel := context.WithTimeout(r.Context(), s.resolveTimeout)
		defer cancel()
		sources, winner, rerr := s.registry.Resolve(ctx, resolve.Request{
			AnilistID: req.AnilistID,
			Episode:   req.Episode,
			Audio:     req.Audio,
			TitleHint: req.TitleHint,
			Provider:  req.Provider,
		})
		if rerr != nil {
			return nil, rerr
		}
		return json.Marshal(protocol.ResolveResponse{
			Sources:  sources,
			Provider: winner,
			TookMs:   time.Since(start).Milliseconds(),
		})
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
		writeError(w, r, status, err.Error())
		return
	}
	s.resolveCache.Set(key, body, resolveCacheTTL)
	w.Header().Set("X-AniFuse-Cache", cacheTag(shared))
	s.writeCached(w, r, http.StatusOK, "application/json; charset=utf-8", body, 120)
}

func (s *Server) handleMetrics(w http.ResponseWriter, r *http.Request) {
	health := s.registry.Health()
	stats := make([]protocol.ProviderStat, 0, len(health))
	for _, h := range health {
		stats = append(stats, protocol.ProviderStat{
			ID:              h.ID,
			Status:          h.Status,
			Attempts:        h.Attempts,
			Successes:       h.Successes,
			Misses:          h.Misses,
			Errors:          h.Errors,
			SuccessRate:     h.SuccessRate,
			CooldownSeconds: h.CooldownSeconds,
		})
	}
	s.writeJSON(w, r, http.StatusOK, protocol.MetricsResponse{
		UptimeSeconds: int64(time.Since(s.startedAt).Seconds()),
		Requests: protocol.RequestMetrics{
			Total:           atomic.LoadInt64(&s.total),
			Resolve:         atomic.LoadInt64(&s.resolveRequests),
			ResolveCacheHit: atomic.LoadInt64(&s.resolveCacheHits),
			Latest:          atomic.LoadInt64(&s.latestRequests),
			LatestCacheHit:  atomic.LoadInt64(&s.latestCacheHits),
		},
		Providers: stats,
	})
}

func (s *Server) logRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		next.ServeHTTP(w, r)
		log.Printf("%s %s %s", r.Method, r.URL.Path, time.Since(start).Round(time.Millisecond))
	})
}

// writeCached writes a JSON body with caching headers: Cache-Control
// max-age and an ETag so clients can revalidate with If-None-Match.
func (s *Server) writeCached(w http.ResponseWriter, r *http.Request, status int, contentType string, body []byte, maxAge int) {
	etag := `"` + hashBody(body) + `"`
	w.Header().Set("ETag", etag)
	w.Header().Set("Cache-Control", "public, max-age="+strconv.Itoa(maxAge))
	if r.Header.Get("If-None-Match") == etag {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	writeBody(w, r, status, contentType, body)
}

func (s *Server) writeJSON(w http.ResponseWriter, r *http.Request, status int, value any) {
	body, err := json.Marshal(value)
	if err != nil {
		writeError(w, r, http.StatusInternalServerError, "encode failure")
		return
	}
	body = append(body, '\n')
	writeBody(w, r, status, "application/json; charset=utf-8", body)
}

func writeError(w http.ResponseWriter, r *http.Request, status int, message string) {
	body, _ := json.Marshal(protocol.ErrorResponse{Error: message})
	body = append(body, '\n')
	writeBody(w, r, status, "application/json; charset=utf-8", body)
}

// writeBody writes the final bytes, gzip-compressing bodies the client
// accepts and that are worth compressing.
func writeBody(w http.ResponseWriter, r *http.Request, status int, contentType string, body []byte) {
	if strings.Contains(r.Header.Get("Accept-Encoding"), "gzip") && len(body) > 512 {
		var buf bytes.Buffer
		gz := gzip.NewWriter(&buf)
		_, _ = gz.Write(body)
		_ = gz.Close()
		w.Header().Set("Content-Encoding", "gzip")
		body = buf.Bytes()
	}
	w.Header().Set("Content-Type", contentType)
	w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	w.WriteHeader(status)
	_, _ = w.Write(body)
}

func cacheTag(shared bool) string {
	if shared {
		return "SHARED"
	}
	return "MISS"
}

func hashBody(body []byte) string {
	sum := sha1.Sum(body)
	return hex.EncodeToString(sum[:])
}

func sortByEpisode(entries []protocol.LatestEntry) {
	for i := 1; i < len(entries); i++ {
		for j := i; j > 0 && entries[j].Episode > entries[j-1].Episode; j-- {
			entries[j], entries[j-1] = entries[j-1], entries[j]
		}
	}
}

func sortByTitle(entries []protocol.LatestEntry) {
	for i := 1; i < len(entries); i++ {
		for j := i; j > 0 && strings.ToLower(entries[j].Title) < strings.ToLower(entries[j-1].Title); j-- {
			entries[j], entries[j-1] = entries[j-1], entries[j]
		}
	}
}

func envDuration(name string, fallback time.Duration) time.Duration {
	raw := strings.TrimSpace(os.Getenv(name))
	if raw == "" {
		return fallback
	}
	d, err := time.ParseDuration(raw)
	if err != nil || d <= 0 {
		log.Printf("anifuse: ignoring bad %s %q", name, raw)
		return fallback
	}
	return d
}
