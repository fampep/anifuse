// Package httpapi — AniList-backed catalog endpoints for API-backed
// extensions: search and title detail. No AniList auth is needed for
// browsing; responses are cached.
package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/fampep/anifuse/internal/protocol"
)

const anilistGraphQL = "https://graphql.anilist.co"

// AniList browser is a small read-only GraphQL client for the public
// AniList API used by the catalog endpoints.
type AniListBrowser struct {
	http *http.Client
}

func newAniListBrowser() *AniListBrowser {
	return &AniListBrowser{http: &http.Client{Timeout: 15 * time.Second}}
}

func (b *AniListBrowser) query(ctx context.Context, graphql string, variables map[string]any) (map[string]any, error) {
	body, err := json.Marshal(map[string]any{"query": graphql, "variables": variables})
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, anilistGraphQL, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "AniFuse/1.0 (catalog)")
	resp, err := b.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
	if err != nil {
		return nil, err
	}
	var envelope struct {
		Data   map[string]any `json:"data"`
		Errors []struct {
			Message string `json:"message"`
		} `json:"errors"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil {
		return nil, err
	}
	if len(envelope.Errors) > 0 {
		return nil, errors.New("anilist: " + envelope.Errors[0].Message)
	}
	return envelope.Data, nil
}

const searchQuery = `
query ($q: String, $page: Int) {
  Page(page: $page, perPage: 24) {
    pageInfo { hasNextPage }
    media(search: $q, type: ANIME, sort: SEARCH_MATCH) {
      id
      title { romaji english }
      coverImage { extraLarge }
      episodes
      status
      genres
    }
  }
}`

const detailQuery = `
query ($id: Int) {
  Media(id: $id, type: ANIME) {
    id
    title { romaji english }
    coverImage { extraLarge }
    bannerImage
    description
    episodes
    nextAiringEpisode { episode }
    status
    genres
    seasonYear
  }
}`

// handleSearch proxies AniList search for API-backed extensions.
func (s *Server) handleSearch(w http.ResponseWriter, r *http.Request) {
	q := strings.TrimSpace(r.URL.Query().Get("q"))
	if q == "" {
		writeError(w, r, http.StatusBadRequest, "q is required")
		return
	}
	page, _ := strconv.Atoi(r.URL.Query().Get("page"))
	if page < 1 {
		page = 1
	}
	key := "search|" + strings.ToLower(q) + "|" + strconv.Itoa(page)
	if body, ok := s.latestCache.Get(key); ok {
		s.writeCached(w, r, http.StatusOK, "application/json; charset=utf-8", body, 600)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	data, err := s.anilist.query(ctx, searchQuery, map[string]any{"q": q, "page": page})
	if err != nil {
		writeError(w, r, http.StatusBadGateway, "anilist search: "+err.Error())
		return
	}
	pageData, _ := data["Page"].(map[string]any)
	mediaList, _ := pageData["media"].([]any)
	pageInfo, _ := pageData["pageInfo"].(map[string]any)
	results := make([]protocol.SearchResult, 0, len(mediaList))
	for _, raw := range mediaList {
		media, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		results = append(results, searchResultFromMedia(media))
	}
	hasNext, _ := pageInfo["hasNextPage"].(bool)
	body, _ := json.Marshal(protocol.SearchResponse{Results: results, HasNextPage: hasNext})
	s.latestCache.Set(key, body, 10*time.Minute)
	s.writeCached(w, r, http.StatusOK, "application/json; charset=utf-8", body, 600)
}

// handleAnime returns one title's detail plus its episode count.
func (s *Server) handleAnime(w http.ResponseWriter, r *http.Request) {
	idStr := strings.TrimPrefix(r.URL.Path, "/v1/anime/")
	id, err := strconv.Atoi(idStr)
	if err != nil || id <= 0 {
		writeError(w, r, http.StatusBadRequest, "anime id is required")
		return
	}
	key := "anime|" + idStr
	if body, ok := s.latestCache.Get(key); ok {
		s.writeCached(w, r, http.StatusOK, "application/json; charset=utf-8", body, 600)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	data, err := s.anilist.query(ctx, detailQuery, map[string]any{"id": id})
	if err != nil {
		writeError(w, r, http.StatusBadGateway, "anilist detail: "+err.Error())
		return
	}
	media, _ := data["Media"].(map[string]any)
	if media == nil {
		writeError(w, r, http.StatusNotFound, "anime not found")
		return
	}
	title, _ := media["title"].(map[string]any)
	cover, _ := media["coverImage"].(map[string]any)
	genres := make([]string, 0)
	if raw, ok := media["genres"].([]any); ok {
		for _, g := range raw {
			if text, ok := g.(string); ok {
				genres = append(genres, text)
			}
		}
	}
	detail := protocol.AnimeDetail{
		AnilistID:   id,
		Title:       firstNonEmpty(strOf(title["romaji"]), strOf(title["english"])),
		Description: strOf(media["description"]),
		ImageURL:    strOf(cover["extraLarge"]),
		Episodes:    firstPositive(intNum(media["episodes"]), latestAiredEpisode(media["nextAiringEpisode"])),
		Status:      strings.ToLower(strOf(media["status"])),
		Genres:      genres,
		SeasonYear:  intNum(media["seasonYear"]),
	}
	body, _ := json.Marshal(detail)
	s.latestCache.Set(key, body, 30*time.Minute)
	s.writeCached(w, r, http.StatusOK, "application/json; charset=utf-8", body, 600)
}

func searchResultFromMedia(media map[string]any) protocol.SearchResult {
	title, _ := media["title"].(map[string]any)
	cover, _ := media["coverImage"].(map[string]any)
	return protocol.SearchResult{
		AnilistID: intNum(media["id"]),
		Title:     firstNonEmpty(strOf(title["romaji"]), strOf(title["english"])),
		ImageURL:  strOf(cover["extraLarge"]),
		Episodes:  intNum(media["episodes"]),
		Status:    strings.ToLower(strOf(media["status"])),
	}
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

func firstPositive(values ...int) int {
	for _, value := range values {
		if value > 0 {
			return value
		}
	}
	return 0
}

// latestAiredEpisode derives the newest aired episode number from the
// nextAiringEpisode object, used when Media.episodes is null.
func latestAiredEpisode(value any) int {
	next, ok := value.(map[string]any)
	if !ok {
		return 0
	}
	if episode := intNum(next["episode"]); episode > 1 {
		return episode - 1
	}
	return 0
}

func strOf(value any) string {
	if s, ok := value.(string); ok {
		return s
	}
	return ""
}

func intNum(value any) int {
	switch v := value.(type) {
	case float64:
		return int(v)
	case int:
		return v
	case string:
		n, _ := strconv.Atoi(strings.TrimSpace(v))
		return n
	default:
		return 0
	}
}
