// Package anigato resolves anigato.lol episodes. The AniList id is the
// watch id. /api/episodes/{id} lists episode numbers and
// /api/watch/{id}/sub|dub/{episode} names the files. A dub request plays
// only rows the site marks dub.
package anigato

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
	ID = "anigato"
	// Name is the display name.
	Name = "AniGato"
	// Origin is the site the resolver scrapes.
	Origin = "https://anigato.lol"
	// UserAgent is the browser the site expects.
	UserAgent = "Mozilla/5.0 (Linux; Android 14) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/122.0.0.0 Mobile Safari/537.36"
)

// Provider implements providers.Provider for anigato.lol.
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

// Resolve turns an AniList id + episode into playable sources.
func (p *Provider) Resolve(ctx context.Context, req resolve.Request) ([]protocol.Source, error) {
	if req.AnilistID <= 0 || req.Episode <= 0 {
		return nil, resolve.Missing(Name, "anilistId and episode are required")
	}
	dub := strings.EqualFold(req.Audio, "dub")
	page := p.origin + "/watch/" + strconv.Itoa(req.AnilistID) + "/" + strconv.Itoa(req.Episode)
	watchURL := p.origin + "/api/watch/" + strconv.Itoa(req.AnilistID) + "/" + audioWord(dub) + "/" + strconv.Itoa(req.Episode)

	body, err := p.open(ctx, watchURL, page)
	if err != nil {
		return nil, err
	}
	var payload apiWatch
	if err := json.Unmarshal([]byte(body), &payload); err != nil {
		return nil, resolve.Blocked(Name, "unreadable watch JSON")
	}
	if !payload.Success {
		return nil, resolve.Missing(Name, "site answered unsuccessfully")
	}
	if dub && payload.DubUnavailable {
		return nil, resolve.Missing(Name, "dub unavailable")
	}
	streams := payload.streamsFor(dub)
	if len(streams) == 0 {
		if payload.StillLoading() {
			body, err = p.open(ctx, watchURL, page)
			if err != nil {
				return nil, err
			}
			if err := json.Unmarshal([]byte(body), &payload); err == nil && payload.Success {
				streams = payload.streamsFor(dub)
			}
		}
	}
	if len(streams) == 0 {
		return nil, resolve.Missing(Name, "no streams")
	}

	type ranked struct {
		source protocol.Source
		height int
	}
	var out []ranked
	for _, stream := range streams {
		file, ok := playlistURL(p.origin, stream.URL)
		if !ok {
			continue
		}
		height, quality := p.probe(ctx, file, page)
		label := quality
		if label == "" {
			label = strings.ToUpper(audioWord(dub))
		}
		out = append(out, ranked{
			source: protocol.Source{
				URL: file,
				HLS: true,
				Headers: map[string]string{
					"Referer":    p.origin + "/",
					"Origin":     p.origin,
					"User-Agent": UserAgent,
				},
				Label:     label,
				Quality:   quality,
				Audio:     strings.ToUpper(audioWord(dub)),
				Subtitles: subtitles(p.origin, stream.Subtitles),
			},
			height: height,
		})
	}
	if len(out) == 0 {
		return nil, resolve.Unplayable(Name, "no playable playlist")
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].height > out[j].height })
	sources := make([]protocol.Source, len(out))
	for i, r := range out {
		sources[i] = r.source
	}
	return sources, nil
}

// open fetches a page with the site headers and maps status to typed errors.
func (p *Provider) open(ctx context.Context, pageURL, referer string) (string, error) {
	page, err := p.client.GetText(ctx, pageURL, referer)
	if err != nil {
		return "", resolve.Blocked(Name, err.Error())
	}
	if httpx.Challenge(page.Code, page.Body) {
		return "", resolve.Blocked(Name, "bot wall")
	}
	if page.Code == 404 {
		return "", resolve.Missing(Name, "not found")
	}
	if page.Code < 200 || page.Code > 299 {
		return "", resolve.Blocked(Name, "status "+strconv.Itoa(page.Code))
	}
	if strings.TrimSpace(page.Body) == "" {
		return "", resolve.Missing(Name, "empty answer")
	}
	return page.Body, nil
}

// probe fetches a playlist to learn the height the master states. A failed
// probe leaves the stream in the list without a badge.
func (p *Provider) probe(ctx context.Context, fileURL, referer string) (int, string) {
	page, err := p.client.GetText(ctx, fileURL, referer)
	if err != nil || page.Code < 200 || page.Code > 299 {
		return 0, ""
	}
	if !strings.HasPrefix(strings.TrimSpace(page.Body), "#EXTM3U") {
		return 0, ""
	}
	height := 0
	if m := heightRe.FindStringSubmatch(page.Body); m != nil {
		height, _ = strconv.Atoi(m[1])
	}
	return height, heightLabel(height)
}

// apiWatch mirrors /api/watch/{id}/{audio}/{episode}.
type apiWatch struct {
	Success        bool     `json:"success"`
	DubUnavailable bool     `json:"dubUnavailable"`
	LoadingMore    bool     `json:"loadingMore"`
	PrimaryPending bool     `json:"primaryPending"`
	MainPending    bool     `json:"mainPending"`
	Streams        []stream `json:"streams"`
}

// StillLoading reports whether the site asked for a second pass.
func (w *apiWatch) StillLoading() bool {
	return w.LoadingMore || w.PrimaryPending || w.MainPending
}

func (w *apiWatch) streamsFor(dub bool) []stream {
	wanted := "sub"
	if dub {
		wanted = "dub"
	}
	var out []stream
	for _, s := range w.Streams {
		kind := strings.ToLower(strings.TrimSpace(s.Type))
		if kind != "" && kind != wanted {
			continue
		}
		out = append(out, s)
	}
	return out
}

type stream struct {
	Type      string     `json:"type"`
	URL       string     `json:"url"`
	Subtitles []subtitle `json:"subtitles"`
}

type subtitle struct {
	URL   string `json:"url"`
	File  string `json:"file"`
	Label string `json:"label"`
	Lang  string `json:"lang"`
}

func audioWord(dub bool) string {
	if dub {
		return "dub"
	}
	return "sub"
}

// playlistURL keeps only anigato.lol /api/stream/ links.
func playlistURL(origin, raw string) (string, bool) {
	text := strings.TrimSpace(raw)
	if text == "" || strings.Contains(text, "..") || strings.Contains(text, `\`) {
		return "", false
	}
	absolute := text
	switch {
	case strings.HasPrefix(text, "/api/stream/"):
		absolute = origin + text
	case strings.HasPrefix(text, Origin+"/api/stream/"):
	default:
		return "", false
	}
	u, err := url.Parse(absolute)
	if err != nil || u.User != nil {
		return "", false
	}
	if !strings.EqualFold(u.Scheme, "https") || !strings.EqualFold(u.Hostname(), "anigato.lol") {
		return "", false
	}
	if !strings.HasPrefix(u.Path, "/api/stream/") {
		return "", false
	}
	return absolute, true
}

func subtitles(origin string, rows []subtitle) []protocol.SubtitleTrack {
	var tracks []protocol.SubtitleTrack
	seen := map[string]bool{}
	for _, row := range rows {
		raw := strings.TrimSpace(row.URL)
		if raw == "" {
			raw = strings.TrimSpace(row.File)
		}
		absolute := raw
		switch {
		case strings.HasPrefix(raw, "/api/sub.vtt"):
			absolute = origin + raw
		case strings.HasPrefix(raw, Origin+"/api/sub.vtt"):
		default:
			continue
		}
		if seen[absolute] {
			continue
		}
		label := strings.TrimSpace(row.Label)
		if label == "" {
			label = strings.TrimSpace(row.Lang)
		}
		if label == "" {
			continue
		}
		seen[absolute] = true
		tracks = append(tracks, protocol.SubtitleTrack{
			URL:      absolute,
			Label:    label,
			Language: captionLanguage(label),
			MimeType: "text/vtt",
		})
	}
	return tracks
}

func captionLanguage(label string) string {
	text := strings.TrimSpace(label)
	if text == "" {
		return ""
	}
	if code, ok := captionNames[strings.ToLower(text)]; ok {
		return code
	}
	code := strings.ToLower(text)
	if len(code) < 2 || len(code) > 3 {
		return ""
	}
	for _, r := range code {
		if (r < 'a' || r > 'z') && (r < '0' || r > '9') {
			return ""
		}
	}
	return code
}

func heightLabel(height int) string {
	switch {
	case height >= 2000:
		return "4K"
	case height >= 360:
		return strconv.Itoa(height) + "p"
	default:
		return ""
	}
}

var captionNames = map[string]string{
	"english": "en", "japanese": "ja", "spanish": "es", "french": "fr",
	"german": "de", "portuguese": "pt", "italian": "it", "russian": "ru",
	"arabic": "ar", "korean": "ko", "chinese": "zh",
}

var heightRe = regexp.MustCompile(`RESOLUTION=\d+x(\d+)`)
