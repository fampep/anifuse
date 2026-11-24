// Package animex resolves animex.one episodes. The public client searches
// POST graphql.animex.one (`searchAnime`), then reads episodes, servers and
// sources from pp.animex.one/rest/api. The default server is tried first.
// The playlist is HLS even when the path is deeznuts.txt.
package animex

import (
	"context"
	"encoding/json"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/fampep/anifuse/internal/httpx"
	"github.com/fampep/anifuse/internal/protocol"
	"github.com/fampep/anifuse/internal/resolve"
)

const (
	// ID is the registry id.
	ID = "animex"
	// Name is the display name.
	Name = "animex"
	// Origin is the site the resolver scrapes.
	Origin = "https://animex.one"
	// UserAgent is the browser the site expects.
	UserAgent = "Mozilla/5.0 (Windows NT 10.0; Win64; x64; rv:109.0) Gecko/20100101 Firefox/121.0"
)

const (
	gql = "https://graphql.animex.one/graphql"
	api = "https://pp.animex.one/rest/api"
)

const searchQuery = `query Search($query: String!, $limit: Int) { searchAnime(query: $query, limit: $limit) { items { id anilistId titleRomaji titleEnglish } } }`

// Provider implements providers.Provider for animex.one.
type Provider struct {
	client *httpx.Client
	origin string
}

// New builds the provider with default timeouts.
func New() *Provider {
	return newWithOrigin(httpx.NewClient(UserAgent, 25*time.Second), Origin)
}

func newWithOrigin(client *httpx.Client, origin string) *Provider {
	return &Provider{client: client, origin: origin}
}

func (p *Provider) ID() string      { return ID }
func (p *Provider) Name() string    { return Name }
func (p *Provider) SiteURL() string { return Origin }

// Resolve turns an AniList id + episode into a playable HLS master.
func (p *Provider) Resolve(ctx context.Context, req resolve.Request) ([]protocol.Source, error) {
	if req.AnilistID <= 0 || req.Episode <= 0 {
		return nil, resolve.Missing(Name, "anilistId and episode are required")
	}
	dub := strings.EqualFold(req.Audio, "dub")
	show, err := p.findShow(ctx, req.AnilistID, req.TitleHint)
	if err != nil {
		return nil, err
	}
	if err := p.checkEpisode(ctx, show.id, req.Episode, dub); err != nil {
		return nil, err
	}
	source, err := p.openSource(ctx, show.id, req.Episode, dub)
	if err != nil {
		return nil, err
	}
	return []protocol.Source{*source}, nil
}

func (p *Provider) findShow(ctx context.Context, anilistID int, titleHint string) (*show, error) {
	title := cleanTitle(titleHint)
	if title == "" {
		return nil, resolve.Missing(Name, "needs a title to find this show")
	}
	body, err := json.Marshal(map[string]any{
		"query":     searchQuery,
		"variables": map[string]any{"query": title, "limit": 24},
	})
	if err != nil {
		return nil, resolve.Blocked(Name, "query build")
	}
	page, err := p.client.PostJSON(ctx, gql, string(body), map[string]string{
		"Origin":  p.origin,
		"Referer": p.origin + "/",
	})
	if err != nil {
		return nil, resolve.Blocked(Name, err.Error())
	}
	if blocked(page.Code, page.Body) {
		return nil, resolve.Blocked(Name, "bot wall")
	}
	if page.Code == 404 {
		return nil, resolve.Missing(Name, "not found")
	}
	if page.Code < 200 || page.Code > 299 {
		return nil, resolve.Blocked(Name, "status "+strconv.Itoa(page.Code))
	}
	found := pickShow(page.Body, anilistID, title)
	if found == nil {
		return nil, resolve.Missing(Name, "title not found")
	}
	return found, nil
}

func (p *Provider) checkEpisode(ctx context.Context, id string, number int, dub bool) error {
	page, err := p.client.GetText(ctx, api+"/episodes?id="+encode(id), p.origin+"/")
	if err != nil {
		return resolve.Blocked(Name, err.Error())
	}
	if blocked(page.Code, page.Body) {
		return resolve.Blocked(Name, "bot wall")
	}
	if page.Code == 404 {
		return resolve.Missing(Name, "not found")
	}
	if page.Code < 200 || page.Code > 299 {
		return resolve.Blocked(Name, "status "+strconv.Itoa(page.Code))
	}
	row := episode(page.Body, number)
	if row == nil {
		return resolve.Missing(Name, "episode not listed")
	}
	if dub && !row.hasDub {
		return resolve.Missing(Name, "dub not available")
	}
	if !dub && !row.hasSub {
		return resolve.Missing(Name, "sub not available")
	}
	return nil
}

func (p *Provider) openSource(ctx context.Context, id string, episode int, dub bool) (*protocol.Source, error) {
	kind := "sub"
	if dub {
		kind = "dub"
	}
	serversPage, err := p.client.GetText(ctx, api+"/servers?id="+encode(id)+"&epNum="+strconv.Itoa(episode)+"&type="+kind, p.origin+"/")
	if err != nil {
		return nil, resolve.Blocked(Name, err.Error())
	}
	if blocked(serversPage.Code, serversPage.Body) || serversPage.Code < 200 || serversPage.Code > 299 {
		return nil, resolve.Blocked(Name, "servers unreachable")
	}
	providers := providerIDs(serversPage.Body, dub)
	if len(providers) == 0 {
		return nil, resolve.Missing(Name, "no servers")
	}
	wasBlocked := false
	for _, provider := range providers {
		sourcesPage, err := p.client.GetText(ctx, api+"/sources?id="+encode(id)+"&epNum="+strconv.Itoa(episode)+"&type="+kind+"&providerId="+encode(provider), p.origin+"/")
		if err != nil {
			continue
		}
		if blocked(sourcesPage.Code, sourcesPage.Body) {
			wasBlocked = true
			continue
		}
		if sourcesPage.Code < 200 || sourcesPage.Code > 299 {
			continue
		}
		link := link(sourcesPage.Body, UserAgent)
		if link == nil {
			continue
		}
		playlistPage, err := p.client.GetText(ctx, link.url, p.origin+"/")
		if err != nil || playlistPage.Code < 200 || playlistPage.Code > 299 {
			continue
		}
		if source := playable(*link, playlistPage.Body, dub); source != nil {
			return source, nil
		}
	}
	if wasBlocked {
		return nil, resolve.Blocked(Name, "sources challenged")
	}
	return nil, resolve.Missing(Name, "no playable source")
}

type show struct {
	id        string
	title     string
	romaji    string
	anilistID int
}

type episodeRow struct {
	hasSub bool
	hasDub bool
}

type mediaLink struct {
	url       string
	headers   map[string]string
	subtitles []protocol.SubtitleTrack
}

// pickShow matches the search result on the AniList id, then on the title.
func pickShow(payload string, anilistID int, titleHint string) *show {
	var root map[string]any
	if err := json.Unmarshal([]byte(payload), &root); err != nil {
		return nil
	}
	items := walkItems(root, "data", "searchAnime", "items")
	if items == nil {
		return nil
	}
	var shows []*show
	for _, item := range items {
		obj, ok := item.(map[string]any)
		if !ok {
			continue
		}
		id := str(obj["id"])
		if id == "" {
			continue
		}
		english := str(obj["titleEnglish"])
		romaji := str(obj["titleRomaji"])
		title := english
		if title == "" {
			title = romaji
		}
		if title == "" {
			title = id
		}
		shows = append(shows, &show{id: id, title: title, romaji: romaji, anilistID: intNum(obj["anilistId"])})
	}
	if anilistID > 0 {
		for _, s := range shows {
			if s.anilistID == anilistID {
				return s
			}
		}
	}
	want := normalizeTitle(titleHint)
	if want == "" {
		return nil
	}
	for _, s := range shows {
		if normalizeTitle(s.title) == want || normalizeTitle(s.romaji) == want {
			return s
		}
	}
	for _, s := range shows {
		title := normalizeTitle(s.title)
		romaji := normalizeTitle(s.romaji)
		if (title != "" && (strings.Contains(title, want) || strings.Contains(want, title))) ||
			(romaji != "" && (strings.Contains(romaji, want) || strings.Contains(want, romaji))) {
			return s
		}
	}
	return nil
}

// episode finds the episode row in the episodes array.
func episode(payload string, number int) *episodeRow {
	var rows []any
	if err := json.Unmarshal([]byte(payload), &rows); err != nil {
		return nil
	}
	for _, item := range rows {
		obj, ok := item.(map[string]any)
		if !ok {
			continue
		}
		if intNum(obj["number"]) != number {
			continue
		}
		hasSub := true
		hasDub := true
		if _, ok := obj["hasSub"]; ok {
			hasSub = boolNum(obj["hasSub"])
		}
		if _, ok := obj["hasDub"]; ok {
			hasDub = boolNum(obj["hasDub"])
		}
		return &episodeRow{hasSub: hasSub, hasDub: hasDub}
	}
	return nil
}

// providerIDs lists the servers with the default one first.
func providerIDs(payload string, dub bool) []string {
	var root map[string]any
	if err := json.Unmarshal([]byte(payload), &root); err != nil {
		return nil
	}
	key := "subProviders"
	if dub {
		key = "dubProviders"
	}
	rows := arrayAt(root, key)
	var list []rankedProvider
	for _, row := range rows {
		obj, ok := row.(map[string]any)
		if !ok {
			continue
		}
		id := str(obj["id"])
		if id == "" {
			continue
		}
		list = append(list, rankedProvider{def: boolNum(obj["default"]), id: id})
	}
	sortRanked(list)
	out := make([]string, len(list))
	for i, r := range list {
		out[i] = r.id
	}
	return out
}

// link reads the first playable source URL and the headers the server
// listed for it.
func link(payload, userAgent string) *mediaLink {
	var root map[string]any
	if err := json.Unmarshal([]byte(payload), &root); err != nil {
		return nil
	}
	sources := arrayAt(root, "sources")
	urlText := ""
	for _, row := range sources {
		obj, ok := row.(map[string]any)
		if !ok {
			continue
		}
		candidate := str(obj["url"])
		if strings.HasPrefix(candidate, "https://") || strings.HasPrefix(candidate, "http://") {
			urlText = candidate
			break
		}
	}
	if urlText == "" {
		return nil
	}
	headers := map[string]string{}
	if raw, ok := root["headers"].(map[string]any); ok {
		for name, value := range raw {
			lower := strings.ToLower(name)
			if !headerAllowed(lower) {
				continue
			}
			text := str(value)
			if text != "" {
				headers[canonicalHeader(name)] = text
			}
		}
	}
	if _, ok := headers["User-Agent"]; !ok {
		headers["User-Agent"] = userAgent
	}
	if _, ok := headers["Referer"]; !ok {
		headers["Referer"] = Origin + "/"
	}
	return &mediaLink{
		url:       urlText,
		headers:   headers,
		subtitles: subtitles(arrayAt(root, "tracks")),
	}
}

func playable(link mediaLink, playlist string, dub bool) *protocol.Source {
	if !strings.HasPrefix(strings.TrimSpace(playlist), "#EXTM3U") {
		return nil
	}
	quality := ""
	if match := heightRe.FindStringSubmatch(bestVariant(playlist)); match != nil {
		height, _ := strconv.Atoi(match[1])
		quality = heightLabel(height)
	}
	label := quality
	if label == "" {
		if dub {
			label = "DUB"
		} else {
			label = "SUB"
		}
	}
	mode := "SUB"
	if dub {
		mode = "DUB"
	}
	return &protocol.Source{
		URL:       link.url,
		Headers:   link.headers,
		Label:     label,
		Quality:   quality,
		Audio:     mode,
		Subtitles: link.subtitles,
	}
}

func bestVariant(playlist string) string {
	var best string
	bestHeight := -1
	for _, line := range strings.Split(playlist, "\n") {
		if !strings.HasPrefix(line, "#EXT-X-STREAM-INF") {
			continue
		}
		match := heightRe.FindStringSubmatch(line)
		height := 0
		if match != nil {
			height, _ = strconv.Atoi(match[1])
		}
		if height >= bestHeight {
			best = line
			bestHeight = height
		}
	}
	return best
}

func subtitles(rows []any) []protocol.SubtitleTrack {
	var tracks []protocol.SubtitleTrack
	seen := map[string]bool{}
	for _, row := range rows {
		obj, ok := row.(map[string]any)
		if !ok {
			continue
		}
		raw := str(obj["url"])
		if raw == "" {
			raw = str(obj["file"])
		}
		if !strings.HasPrefix(raw, "https://") && !strings.HasPrefix(raw, "http://") {
			continue
		}
		kind := strings.ToLower(str(obj["kind"]))
		caption := kind == "" || kind == "captions" || kind == "subtitles" ||
			strings.Contains(strings.ToLower(raw), ".vtt") || strings.Contains(strings.ToLower(raw), ".srt")
		if !caption || seen[raw] {
			continue
		}
		language := str(obj["lang"])
		if language == "" {
			language = str(obj["language"])
		}
		if language == "" {
			language = "und"
		}
		label := str(obj["label"])
		if label == "" {
			label = language
		}
		mime := "text/vtt"
		if strings.Contains(strings.ToLower(raw), ".srt") {
			mime = "application/x-subrip"
		}
		seen[raw] = true
		tracks = append(tracks, protocol.SubtitleTrack{URL: raw, Label: label, Language: language, MimeType: mime})
	}
	return tracks
}

func blocked(code int, body string) bool {
	if code == 403 || code == 503 {
		return true
	}
	probe := strings.ToLower(body)
	if len(probe) > 800 {
		probe = probe[:800]
	}
	return strings.Contains(probe, "just a moment") ||
		strings.Contains(probe, "cf-mitigated") ||
		strings.Contains(probe, "challenge-platform")
}

func cleanTitle(raw string) string {
	return strings.TrimSpace(malRe.ReplaceAllString(strings.TrimSpace(raw), ""))
}

func normalizeTitle(raw string) string {
	cleaned := cleanTitle(raw)
	if cleaned == "" {
		return ""
	}
	lower := strings.ToLower(cleaned)
	return strings.Join(strings.FieldsFunc(lower, func(r rune) bool {
		return !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9')
	}), " ")
}

func heightLabel(h int) string {
	switch {
	case h >= 2000:
		return "4K"
	case h >= 360:
		return strconv.Itoa(h) + "p"
	default:
		return ""
	}
}

func encode(value string) string {
	return strings.ReplaceAll(url.QueryEscape(value), "+", "%20")
}

func headerAllowed(name string) bool {
	switch name {
	case "referer", "referrer", "origin", "user-agent", "accept":
		return true
	default:
		return false
	}
}

func canonicalHeader(name string) string {
	switch strings.ToLower(name) {
	case "user-agent":
		return "User-Agent"
	case "referer", "referrer":
		return "Referer"
	case "origin":
		return "Origin"
	case "accept":
		return "Accept"
	default:
		return name
	}
}

type rankedProvider struct {
	def bool
	id  string
}

func sortRanked(list []rankedProvider) {
	for i := 0; i < len(list); i++ {
		for j := i + 1; j < len(list); j++ {
			if !list[i].def && list[j].def {
				list[i], list[j] = list[j], list[i]
			}
		}
	}
}

// walkItems descends the JSON tree along the given keys and returns the
// array found at the end.
func walkItems(root map[string]any, path ...string) []any {
	if len(path) == 0 {
		return nil
	}
	value, ok := root[path[0]]
	if !ok {
		return nil
	}
	if len(path) == 1 {
		if list, ok := value.([]any); ok {
			return list
		}
		return nil
	}
	if obj, ok := value.(map[string]any); ok {
		return walkItems(obj, path[1:]...)
	}
	return nil
}

// arrayAt returns the array at key, or inside a "data" wrapper.
func arrayAt(root map[string]any, key string) []any {
	if list, ok := root[key].([]any); ok {
		return list
	}
	if data, ok := root["data"].(map[string]any); ok {
		if list, ok := data[key].([]any); ok {
			return list
		}
	}
	return nil
}

func str(value any) string {
	if value == nil {
		return ""
	}
	if s, ok := value.(string); ok {
		return strings.TrimSpace(s)
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

func boolNum(value any) bool {
	switch v := value.(type) {
	case bool:
		return v
	case float64:
		return v != 0
	case int:
		return v != 0
	case string:
		n, _ := strconv.Atoi(strings.TrimSpace(v))
		return n != 0
	default:
		return false
	}
}

var (
	malRe    = regexp.MustCompile(`(?i)\bmal:\d+\b`)
	heightRe = regexp.MustCompile(`RESOLUTION=\d+x(\d+)`)
)
