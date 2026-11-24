// Package aninz resolves ani.nz episodes. Search is GET /api/anime/search;
// the playback bootstrap (settlar) names the selection, the session call
// mints an embed token, and embed.settlar.io hands out the HLS master on
// media.settlar.io. Third-party hosts are refused.
package aninz

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
	ID = "aninz"
	// Name is the display name.
	Name = "ani.nz"
	// Origin is the site the resolver scrapes.
	Origin = "https://ani.nz"
	// EmbedOrigin is the settlar embed host.
	EmbedOrigin = "https://embed.settlar.io"
	// MediaHost is the only host the site plays from.
	MediaHost = "media.settlar.io"
	// OwnSession is the session name ani.nz uses for its own server.
	OwnSession = "anipm"
	// UserAgent is the browser the site expects.
	UserAgent = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/125.0.0.0 Safari/537.36"
)

// Provider implements providers.Provider for ani.nz.
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
	id     string
	source string
	title  string
	anilistID int
}

type bootstrap struct {
	selection string
	episode   int
	routeID   string
}

type pending struct {
	url       string
	subtitles []protocol.SubtitleTrack
}

// Resolve turns an AniList id + episode into a playable master.
func (p *Provider) Resolve(ctx context.Context, req resolve.Request) ([]protocol.Source, error) {
	if req.AnilistID <= 0 || req.Episode <= 0 {
		return nil, resolve.Missing(Name, "anilistId and episode are required")
	}
	dub := strings.EqualFold(req.Audio, "dub")
	lang := "sub"
	if dub {
		lang = "dub"
	}
	found, err := p.findShow(ctx, req.AnilistID, req.TitleHint)
	if err != nil {
		return nil, err
	}
	boot, err := p.openBootstrap(ctx, found, req.Episode, lang)
	if err != nil {
		return nil, err
	}
	token, err := p.openSession(ctx, boot, req.Episode, lang)
	if err != nil {
		return nil, err
	}
	source, err := p.openPlaylist(ctx, token, dub)
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
	page, err := p.get(ctx, p.origin+"/api/anime/search?q="+encode(title), siteReferer(p.origin), p.origin)
	if err != nil {
		return nil, err
	}
	found := pickShow(page.Body, anilistID, title)
	if found == nil {
		return nil, resolve.Missing(Name, "title not found")
	}
	return found, nil
}

func (p *Provider) openBootstrap(ctx context.Context, found *show, episode int, lang string) (*bootstrap, error) {
	numbered, err := p.loadBootstrap(ctx, found, strconv.Itoa(episode), lang)
	if err != nil {
		return nil, err
	}
	if numbered.routeID != "" && numbered.routeID != strconv.Itoa(episode) {
		routed, routeErr := p.loadBootstrap(ctx, found, numbered.routeID, lang)
		if routeErr == nil {
			return routed, nil
		}
	}
	return numbered, nil
}

func (p *Provider) loadBootstrap(ctx context.Context, found *show, episodeKey, lang string) (*bootstrap, error) {
	source := "settlar"
	if found.source == "anilist" {
		source = "anilist"
	}
	page, err := p.get(ctx, p.origin+"/api/anime/playback-bootstrap/"+source+"/"+encode(found.id)+"?ep="+encode(episodeKey)+"&lang="+lang, siteReferer(p.origin), p.origin)
	if err != nil {
		return nil, err
	}
	if page.Code == 404 {
		return nil, resolve.Missing(Name, "not found")
	}
	return bootstrapFrom(page.Body, lang), nil
}

func (p *Provider) openSession(ctx context.Context, boot *bootstrap, fallbackEpisode int, lang string) (string, error) {
	episode := boot.episode
	if episode <= 0 {
		episode = fallbackEpisode
	}
	query := "selection=" + encode(boot.selection) + "&provider=" + OwnSession + "&ep=" + strconv.Itoa(episode) + "&channel=" + lang + "&telemetry=0"
	page, err := p.get(ctx, p.origin+"/api/anime/settlar/session?"+query, siteReferer(p.origin), p.origin)
	if err != nil {
		return "", err
	}
	token := embedToken(page.Body)
	if token == "" {
		return "", resolve.Missing(Name, "no session token")
	}
	return token, nil
}

func (p *Provider) openPlaylist(ctx context.Context, token string, dub bool) (*protocol.Source, error) {
	sessionPage, err := p.get(ctx, EmbedOrigin+"/api/embed/session?t="+encode(token), EmbedOrigin+"/", EmbedOrigin)
	if err != nil {
		return nil, err
	}
	pending := pendingFrom(sessionPage.Body)
	if pending == nil {
		return nil, resolve.Missing(Name, "no settlar stream")
	}
	playlistPage, err := p.get(ctx, pending.url, EmbedOrigin+"/", EmbedOrigin)
	if err != nil {
		return nil, err
	}
	return playable(*pending, playlistPage.Body, dub), nil
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

func siteReferer(origin string) string { return origin + "/" }

// pickShow matches the search result on the AniList id, then the title.
func pickShow(payload string, anilistID int, titleHint string) *show {
	var root map[string]any
	if err := json.Unmarshal([]byte(payload), &root); err != nil {
		return nil
	}
	items, ok := root["items"].([]any)
	if !ok {
		return nil
	}
	var shows []*show
	for _, item := range items {
		row, ok := item.(map[string]any)
		if !ok {
			continue
		}
		id := str(row["id"])
		if id == "" || id == "null" {
			continue
		}
		source := str(row["source"])
		if source == "" {
			source = "settlar"
		}
		title := str(row["title"])
		if title == "" {
			title = id
		}
		shows = append(shows, &show{id: id, source: source, title: title, anilistID: intNum(row["anilistId"])})
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
		if normalizeTitle(s.title) == want {
			return s
		}
	}
	for _, s := range shows {
		title := normalizeTitle(s.title)
		if title != "" && (strings.Contains(title, want) || strings.Contains(want, title)) {
			return s
		}
	}
	return nil
}

// bootstrapFrom reads the selection the session call needs.
func bootstrapFrom(payload, want string) *bootstrap {
	var root map[string]any
	if err := json.Unmarshal([]byte(payload), &root); err != nil {
		return nil
	}
	language := strings.ToLower(strings.TrimSpace(str(root["effectiveLanguage"])))
	if language != want {
		return nil
	}
	selection := strings.TrimSpace(str(root["settlarSelection"]))
	if selection == "" {
		return nil
	}
	episode := intNum(root["episode"])
	route := strings.TrimSpace(str(root["episodeRouteId"]))
	return &bootstrap{selection: selection, episode: episode, routeID: route}
}

// embedToken pulls the t= parameter out of the session embed URL.
func embedToken(payload string) string {
	var root map[string]any
	if err := json.Unmarshal([]byte(payload), &root); err != nil {
		return ""
	}
	raw := strings.TrimSpace(str(root["embedUrl"]))
	if !strings.HasPrefix(raw, EmbedOrigin+"/") {
		return ""
	}
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	return u.Query().Get("t")
}

// pendingFrom keeps the settlar HLS source, refusing third-party hosts.
func pendingFrom(payload string) *pending {
	var root map[string]any
	if err := json.Unmarshal([]byte(payload), &root); err != nil {
		return nil
	}
	if str(root["kind"]) != "hls" {
		return nil
	}
	source := strings.TrimSpace(str(root["source"]))
	if !ownMedia(source) {
		return nil
	}
	return &pending{url: source, subtitles: subtitles(root)}
}

func ownMedia(raw string) bool {
	if !strings.HasPrefix(raw, "https://") {
		return false
	}
	u, err := url.Parse(raw)
	if err != nil {
		return false
	}
	return strings.EqualFold(u.Hostname(), MediaHost)
}

func subtitles(root map[string]any) []protocol.SubtitleTrack {
	rows, ok := root["subtitles"].([]any)
	if !ok {
		return nil
	}
	var tracks []protocol.SubtitleTrack
	for _, row := range rows {
		obj, ok := row.(map[string]any)
		if !ok {
			continue
		}
		ass := strings.EqualFold(str(obj["pick"]), "ass")
		raw := strings.TrimSpace(str(obj["url"]))
		if raw == "" && ass {
			raw = strings.TrimSpace(str(obj["assUrl"]))
		}
		if !strings.HasPrefix(raw, "https://") {
			continue
		}
		label := strings.TrimSpace(str(obj["label"]))
		if label == "" {
			label = strings.TrimSpace(str(obj["srclang"]))
		}
		if label == "" {
			continue
		}
		mime := "text/vtt"
		if ass {
			mime = "text/x-ssa"
		}
		tracks = append(tracks, protocol.SubtitleTrack{
			URL:      raw,
			Label:    label,
			Language: strings.TrimSpace(str(obj["srclang"])),
			MimeType: mime,
		})
	}
	return tracks
}

func playable(pend pending, playlist string, dub bool) *protocol.Source {
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
		URL: pend.url,
		Headers: map[string]string{
			"Referer":    EmbedOrigin + "/",
			"Origin":     EmbedOrigin,
			"User-Agent": UserAgent,
		},
		Label:     label,
		Quality:   quality,
		Audio:     mode,
		Subtitles: pend.subtitles,
	}
}

func bestVariant(playlist string) string {
	var best string
	bestHeight := -1
	for _, line := range strings.Split(playlist, "\n") {
		match := heightRe.FindStringSubmatch(line)
		if match == nil {
			continue
		}
		height, _ := strconv.Atoi(match[1])
		if height > bestHeight {
			bestHeight = height
			best = line
		}
	}
	return best
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

func str(value any) string {
	if value == nil {
		return ""
	}
	if s, ok := value.(string); ok {
		return strings.TrimSpace(s)
	}
	switch v := value.(type) {
	case float64:
		return strconv.FormatFloat(v, 'f', -1, 64)
	default:
		return ""
	}
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

var (
	malRe    = regexp.MustCompile(`(?i)\bmal:\d+\b`)
	heightRe = regexp.MustCompile(`(?i)RESOLUTION=\d+x(\d+)`)
)
