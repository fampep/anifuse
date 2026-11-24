// Package kaa resolves kaa.lt episodes. The public catalog is a POST
// /api/fsearch, then /api/show/{slug}/episodes and /api/show/{slug}/episode
// name the VidStreaming player whose id is the HLS master on
// hls.krussdomi.com. Sub or dub follows the episode language.
package kaa

import (
	"context"
	"encoding/json"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/fampep/anifuse/internal/httpx"
	"github.com/fampep/anifuse/internal/protocol"
	"github.com/fampep/anifuse/internal/resolve"
)

const (
	// ID is the registry id.
	ID = "kaa"
	// Name is the display name.
	Name = "kaa"
	// Origin is the site the resolver scrapes.
	Origin = "https://kaa.lt"
	// PlayerOrigin is the vidstream player host that serves the master.
	PlayerOrigin = "https://krussdomi.com"
	// UserAgent is the browser the site expects.
	UserAgent = "Mozilla/5.0 (Linux; Android 14) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/131.0.0.0 Mobile Safari/537.36"
)

const (
	subLocale = "ja-JP"
	dubLocale = "en-US"
)

// Provider implements providers.Provider for kaa.lt.
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

type show struct {
	slug    string
	title   string
	kind    string
	locales []string
}

// Resolve turns an AniList id + episode into a playable master.
func (p *Provider) Resolve(ctx context.Context, req resolve.Request) ([]protocol.Source, error) {
	if req.AnilistID <= 0 || req.Episode <= 0 {
		return nil, resolve.Missing(Name, "anilistId and episode are required")
	}
	dub := strings.EqualFold(req.Audio, "dub")
	locale := subLocale
	if dub {
		locale = dubLocale
	}
	found, err := p.findShow(ctx, req.TitleHint)
	if err != nil {
		return nil, err
	}
	if len(found.locales) > 0 && !contains(found.locales, locale) {
		return nil, resolve.Missing(Name, "language not available")
	}
	opened, err := p.openEpisode(ctx, found.slug, req.Episode, locale)
	if err != nil {
		return nil, err
	}
	source, err := playable(opened.detail, opened.playlist, opened.captions, dub)
	if err != nil {
		return nil, resolve.Missing(Name, err.Error())
	}
	return []protocol.Source{*source}, nil
}

func (p *Provider) findShow(ctx context.Context, titleHint string) (*show, error) {
	title := cleanTitle(titleHint)
	if title == "" {
		return nil, resolve.Missing(Name, "needs a title to find this show")
	}
	body, err := p.post(ctx, p.origin+"/api/fsearch", `{"query":`+jsonString(title)+`}`)
	if err != nil {
		return nil, err
	}
	found := pickShow(body, title)
	if found == nil {
		return nil, resolve.Missing(Name, "title not found")
	}
	return found, nil
}

func (p *Provider) openEpisode(ctx context.Context, slug string, episode int, locale string) (*opened, error) {
	list, err := p.get(ctx, p.origin+"/api/show/"+encode(slug)+"/episodes?ep="+strconv.Itoa(episode)+"&lang="+encode(locale), p.origin+"/", p.origin)
	if err != nil {
		return nil, err
	}
	path := episodePath(list.Body, episode)
	if path == "" {
		return nil, resolve.Missing(Name, "episode not listed")
	}
	detail, err := p.get(ctx, p.origin+"/api/show/"+encode(slug)+"/episode/"+encode(path), p.origin+"/", p.origin)
	if err != nil {
		return nil, err
	}
	player := vidStreamingURL(detail.Body)
	if player == "" {
		return nil, resolve.Missing(Name, "no vidstream server")
	}
	playerID := playerID(player)
	if playerID == "" {
		return nil, resolve.Missing(Name, "no player id")
	}
	master := masterURL(playerID)
	playlistPage, err := p.get(ctx, master, PlayerOrigin+"/", PlayerOrigin)
	playlist := ""
	if err == nil && playlistPage.Code >= 200 && playlistPage.Code <= 299 {
		playlist = playlistPage.Body
	}
	captions := ""
	if page, pageErr := p.get(ctx, player, p.origin+"/", p.origin); pageErr == nil && page.Code >= 200 && page.Code <= 299 {
		captions = page.Body
	}
	return &opened{detail: detail.Body, playlist: playlist, captions: captions}, nil
}

type opened struct {
	detail    string
	playlist  string
	captions  string
}

func (p *Provider) get(ctx context.Context, pageURL, referer, origin string) (httpx.Page, error) {
	page, err := p.client.GetText(ctx, pageURL, referer)
	if err != nil {
		return httpx.Page{}, resolve.Blocked(Name, err.Error())
	}
	if blocked(page.Code, page.Body) {
		return httpx.Page{}, resolve.Blocked(Name, "bot wall")
	}
	if page.Code == 404 {
		return httpx.Page{}, resolve.Missing(Name, "not found")
	}
	if page.Code < 200 || page.Code > 299 {
		return httpx.Page{}, resolve.Blocked(Name, "status "+strconv.Itoa(page.Code))
	}
	return page, nil
}

func (p *Provider) post(ctx context.Context, pageURL, body string) (string, error) {
	page, err := p.client.PostJSON(ctx, pageURL, body, map[string]string{
		"Origin":  p.origin,
		"Referer": p.origin + "/",
	})
	if err != nil {
		return "", resolve.Blocked(Name, err.Error())
	}
	if blocked(page.Code, page.Body) {
		return "", resolve.Blocked(Name, "bot wall")
	}
	if page.Code == 404 {
		return "", resolve.Missing(Name, "not found")
	}
	if page.Code < 200 || page.Code > 299 {
		return "", resolve.Blocked(Name, "status "+strconv.Itoa(page.Code))
	}
	return page.Body, nil
}

// cleanTitle trims catalog cruft the hint may carry.
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

func pickShow(payload, titleHint string) *show {
	want := normalizeTitle(titleHint)
	if want == "" {
		return nil
	}
	var root map[string]any
	if err := json.Unmarshal([]byte(payload), &root); err != nil {
		return nil
	}
	rows := arrayAt(root, "result")
	if rows == nil {
		rows = arrayAt(root, "data", "result")
	}
	if rows == nil {
		return nil
	}
	var shows []*show
	for _, row := range rows {
		obj, ok := row.(map[string]any)
		if !ok {
			continue
		}
		slug := str(obj["slug"])
		if slug == "" {
			continue
		}
		title := str(obj["title_en"])
		if title == "" {
			title = str(obj["title"])
		}
		if title == "" {
			title = slug
		}
		var locales []string
		if list, ok := obj["locales"].([]any); ok {
			for _, code := range list {
				if s := str(code); s != "" {
					locales = append(locales, s)
				}
			}
		}
		shows = append(shows, &show{slug: slug, title: title, kind: str(obj["type"]), locales: locales})
	}
	var exact []*show
	var loose []*show
	for _, s := range shows {
		norm := normalizeTitle(s.title)
		if norm == want {
			exact = append(exact, s)
		} else if norm != "" && (strings.Contains(norm, want) || strings.Contains(want, norm)) {
			loose = append(loose, s)
		}
	}
	pool := exact
	if len(pool) == 0 {
		pool = loose
	}
	for _, s := range pool {
		if strings.EqualFold(s.kind, "tv") {
			return s
		}
	}
	if len(pool) > 0 {
		return pool[0]
	}
	return nil
}

func episodePath(payload string, number int) string {
	var root map[string]any
	if err := json.Unmarshal([]byte(payload), &root); err != nil {
		return ""
	}
	rows := arrayAt(root, "result")
	if rows == nil {
		rows = arrayAt(root, "episodes")
	}
	if rows == nil {
		rows = arrayAt(root, "data", "result")
	}
	for _, row := range rows {
		obj, ok := row.(map[string]any)
		if !ok {
			continue
		}
		var listed int
		if v, ok := obj["episode_number"].(float64); ok {
			listed = int(v)
		} else if v, ok := obj["episode"].(float64); ok {
			listed = int(v)
		} else {
			continue
		}
		if listed != number {
			continue
		}
		id := strings.TrimSpace(strings.TrimPrefix(str(obj["id"]), "/"))
		if strings.HasPrefix(id, "ep-") {
			return id
		}
		slug := strings.TrimSpace(strings.TrimPrefix(str(obj["slug"]), "/"))
		if strings.HasPrefix(slug, "ep-") {
			return slug
		}
		if slug == "" {
			continue
		}
		return "ep-" + strconv.Itoa(number) + "-" + slug
	}
	return ""
}

func vidStreamingURL(payload string) string {
	var root map[string]any
	if err := json.Unmarshal([]byte(payload), &root); err != nil {
		return ""
	}
	servers := arrayAt(root, "servers")
	if servers == nil {
		servers = arrayAt(root, "data", "servers")
	}
	var fallback string
	for _, row := range servers {
		obj, ok := row.(map[string]any)
		if !ok {
			continue
		}
		src := str(obj["src"])
		if src == "" {
			src = str(obj["url"])
		}
		name := str(obj["name"])
		named := strings.EqualFold(name, "VidStreaming") ||
			strings.EqualFold(name, "vidstream") ||
			strings.Contains(strings.ToLower(src), "vidstream")
		if absolute := absolutePlayer(src); absolute != "" {
			if named {
				return absolute
			}
			if fallback == "" && strings.Contains(strings.ToLower(absolute), "krussdomi") {
				fallback = absolute
			}
		}
		bareID := strings.TrimSpace(str(obj["id"]))
		if named && bareID != "" && !strings.Contains(bareID, "/") {
			return PlayerOrigin + "/player?id=" + bareID + "&ep=vidstream"
		}
	}
	return fallback
}

func playerID(playerURL string) string {
	u, err := url.Parse(playerURL)
	if err != nil {
		return ""
	}
	return u.Query().Get("id")
}

func masterURL(playerID string) string {
	return "https://hls.krussdomi.com/manifest/" + playerID + "/master.m3u8"
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

func subtitles(html string) []protocol.SubtitleTrack {
	if html == "" {
		return nil
	}
	decoded := strings.NewReplacer(`\u0026`, "&", `\/`, "/", "&amp;", "&").Replace(html)
	var tracks []protocol.SubtitleTrack
	seen := map[string]bool{}
	for _, match := range captionsRe.FindAllStringSubmatch(decoded, -1) {
		language := match[1]
		if language == "" {
			language = "und"
		}
		label := match[2]
		if label == "" {
			label = language
		}
		if seen[match[3]] {
			continue
		}
		seen[match[3]] = true
		tracks = append(tracks, protocol.SubtitleTrack{
			URL:      match[3],
			Label:    label,
			Language: language,
			MimeType: "text/vtt",
		})
	}
	sort.SliceStable(tracks, func(i, j int) bool { return tracks[i].Language == "en" && tracks[j].Language != "en" })
	return tracks
}

func playable(detail, playlist, captions string, dub bool) (*protocol.Source, error) {
	player := vidStreamingURL(detail)
	if player == "" {
		return nil, resolve.Missing(Name, "no vidstream server")
	}
	id := playerID(player)
	if id == "" {
		return nil, resolve.Missing(Name, "no player id")
	}
	quality := ""
	height := 0
	if match := heightRe.FindStringSubmatch(bestVariant(playlist)); match != nil {
		height, _ = strconv.Atoi(match[1])
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
		URL: masterURL(id),
		Headers: map[string]string{
			"Referer":    PlayerOrigin + "/",
			"Origin":     PlayerOrigin,
			"User-Agent": UserAgent,
		},
		Label:     label,
		Quality:   quality,
		Audio:     mode,
		Subtitles: subtitles(captions),
	}, nil
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

func absolutePlayer(src string) string {
	text := strings.TrimSpace(src)
	switch {
	case strings.HasPrefix(text, "https://") || strings.HasPrefix(text, "http://"):
		return text
	case strings.HasPrefix(text, "//"):
		return "https:" + text
	case strings.HasPrefix(text, "/"):
		return PlayerOrigin + text
	default:
		return ""
	}
}

func encode(value string) string {
	return strings.ReplaceAll(url.QueryEscape(value), "+", "%20")
}

func contains(list []string, want string) bool {
	for _, item := range list {
		if item == want {
			return true
		}
	}
	return false
}

// arrayAt walks the object tree and returns the first array found at the
// named key at any level given.
func arrayAt(root map[string]any, path ...string) []any {
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
	switch next := value.(type) {
	case map[string]any:
		return arrayAt(next, path[1:]...)
	case []any:
		// JSON shapes wrap arrays in objects; walk one level like the
		// Kotlin helper does for "data" wrappers.
		if len(next) > 0 {
			if obj, ok := next[0].(map[string]any); ok {
				return arrayAt(obj, path[1:]...)
			}
		}
		return nil
	default:
		return nil
	}
}

func str(value any) string {
	if value == nil {
		return ""
	}
	if s, ok := value.(string); ok {
		return strings.TrimSpace(s)
	}
	return strings.TrimSpace(jsonString(value))
}

func jsonString(value any) string {
	if value == nil {
		return ""
	}
	switch v := value.(type) {
	case string:
		return v
	default:
		encoded, _ := json.Marshal(v)
		return string(encoded)
	}
}

var (
	malRe      = regexp.MustCompile(`(?i)\bmal:\d+\b`)
	heightRe   = regexp.MustCompile(`RESOLUTION=\d+x(\d+)`)
	captionsRe = regexp.MustCompile(`(?i)"language":\[0,"([^"]*)"\],"name":\[0,"([^"]*)"\],"src":\[0,"(https?:[^"]+\.vtt)"\]`)
)
