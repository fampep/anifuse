// Package anihosts resolves anihosts.xyz episodes. The AniList id is the
// watch id: /p/home/embed/anilist/{id}/{episode} answers with the episode
// list and the host embed the site picked. The packed jwplayer setup on
// that embed is unwrapped here and its first sources file is the HLS
// master. The master carries the original audio and the dub, so one URL
// serves a sub or a dub request.
package anihosts

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
	ID = "anihosts"
	// Name is the display name.
	Name = "AniHosts"
	// Origin is the site the resolver scrapes.
	Origin = "https://anihosts.xyz"
	// UserAgent is the browser the site expects.
	UserAgent = "Mozilla/5.0 (Linux; Android 14) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/122.0.0.0 Mobile Safari/537.36"
)

// Provider implements providers.Provider for anihosts.xyz.
type Provider struct {
	client *httpx.Client
	origin string
}

// New builds the provider with default timeouts.
func New() *Provider {
	return newWithOrigin(httpx.NewClient(UserAgent, 25*time.Second), Origin)
}

// NewWithClient builds the provider on a shared client.
func NewWithClient(client *httpx.Client) *Provider {
	return newWithOrigin(client, Origin)
}

func newWithOrigin(client *httpx.Client, origin string) *Provider {
	return &Provider{client: client, origin: origin}
}

func (p *Provider) ID() string      { return ID }
func (p *Provider) Name() string    { return Name }
func (p *Provider) SiteURL() string { return Origin }

// stateData is the JSON the embed page ships in its state-data script.
type stateData struct {
	Status   string `json:"status"`
	Src      string `json:"src"`
	Source   string `json:"source"`
	Episodes []int  `json:"episodes"`
}

// Resolve turns an AniList id + episode into playable sources.
func (p *Provider) Resolve(ctx context.Context, req resolve.Request) ([]protocol.Source, error) {
	if req.AnilistID <= 0 || req.Episode <= 0 {
		return nil, resolve.Missing(Name, "anilistId and episode are required")
	}
	embedURL := embedURLFor(p.origin, req.AnilistID, req.Episode)
	page, err := p.client.GetText(ctx, embedURL, p.origin+"/")
	if err != nil {
		return nil, resolve.Blocked(Name, err.Error())
	}
	if httpx.Challenge(page.Code, page.Body) {
		return nil, resolve.Blocked(Name, "bot wall")
	}
	if page.Code == 404 {
		return nil, resolve.Missing(Name, "page not found")
	}
	if page.Code < 200 || page.Code > 299 {
		return nil, resolve.Blocked(Name, "unexpected status "+strconv.Itoa(page.Code))
	}
	state, err := state(page.Body)
	if err != nil {
		return nil, resolve.Blocked(Name, "state JSON missing")
	}
	switch state.Status {
	case "ready":
	case "no_episode", "unknown_title":
		return nil, resolve.Missing(Name, state.Status)
	default:
		return nil, resolve.Blocked(Name, "status "+state.Status)
	}
	if !strings.HasPrefix(state.Src, "https://") {
		return nil, resolve.Blocked(Name, "no host embed")
	}

	// The host embed may itself be a direct file; otherwise unwrap it.
	file, ok := mediaURL(state.Src)
	var captions []protocol.SubtitleTrack
	if !ok {
		hostPage, err := p.client.GetText(ctx, state.Src, embedURL)
		if err != nil {
			return nil, resolve.Blocked(Name, err.Error())
		}
		if httpx.Challenge(hostPage.Code, hostPage.Body) {
			return nil, resolve.Blocked(Name, "host embed bot wall")
		}
		if hostPage.Code < 200 || hostPage.Code > 299 {
			return nil, resolve.Blocked(Name, "host embed unreachable")
		}
		config, err := unpackConfig(hostPage.Body)
		if err != nil {
			return nil, resolve.Unplayable(Name, "no player config")
		}
		file, ok = mediaURL(fileFromConfig(config))
		if !ok {
			return nil, resolve.Unplayable(Name, "no playable file in the player config")
		}
		captions = captionsFromConfig(config)
	}

	source, err := playable(file, state.Src, captions, strings.EqualFold(req.Audio, "dub"), UserAgent)
	if err != nil {
		return nil, resolve.Unplayable(Name, err.Error())
	}
	return []protocol.Source{*source}, nil
}

// Latest lists the newest episodes from the homepage "Latest releases"
// shelf. The listing carries title, episode badge, art and a site link.
func (p *Provider) Latest(ctx context.Context, limit int) ([]protocol.LatestEntry, error) {
	if limit <= 0 {
		limit = 40
	}
	page, err := p.client.GetText(ctx, p.origin+"/", p.origin+"/")
	if err != nil {
		return nil, resolve.Blocked(Name, err.Error())
	}
	if httpx.Challenge(page.Code, page.Body) || page.Code < 200 || page.Code > 299 {
		return nil, resolve.Blocked(Name, "homepage status "+strconv.Itoa(page.Code))
	}
	var entries []protocol.LatestEntry
	for _, block := range latestCardRe.FindAllString(page.Body, -1) {
		href := latestHrefRe.FindStringSubmatch(block)
		if href == nil {
			continue
		}
		title := ""
		if t := latestTitleRe.FindStringSubmatch(block); t != nil {
			title = htmlUnescape(t[1])
		}
		episode := 0
		if b := latestBadgeRe.FindStringSubmatch(block); b != nil {
			episode, _ = strconv.Atoi(b[1])
		}
		image := ""
		if img := latestImageRe.FindStringSubmatch(block); img != nil {
			image = img[1]
		}
		if title == "" {
			continue
		}
		entries = append(entries, protocol.LatestEntry{
			Title:    title,
			Episode:  episode,
			ImageURL: image,
			SiteURL:  p.origin + href[1],
		})
		if len(entries) >= limit {
			break
		}
	}
	return entries, nil
}

// embedURLFor builds the embed page URL. Empty for invalid input.
func embedURLFor(origin string, anilistID, episode int) string {
	if anilistID <= 0 || episode <= 0 {
		return ""
	}
	return origin + "/p/home/embed/anilist/" + strconv.Itoa(anilistID) + "/" + strconv.Itoa(episode)
}

// state pulls the state-data script out of the embed page.
func state(html string) (*stateData, error) {
	match := stateRe.FindStringSubmatch(html)
	if match == nil {
		return nil, errNoState
	}
	var out stateData
	if err := json.Unmarshal([]byte(match[1]), &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// unpackConfig unwraps the packed jwplayer setup. Same replacements as the
// page's own eval: dictionary words from the highest index down to 1,
// skipping empties, then string-literal unescaping.
func unpackConfig(html string) (string, error) {
	match := packedRe.FindStringSubmatch(html)
	if match == nil {
		return "", errNoConfig
	}
	payload := strings.TrimSpace(match[1])
	payload = strings.TrimPrefix(payload, "'")
	payload = strings.TrimPrefix(payload, "\"")
	payload = strings.TrimSuffix(payload, "'")
	payload = strings.TrimSuffix(payload, "\"")
	count, err := strconv.Atoi(match[2])
	if err != nil {
		return "", err
	}
	words := strings.Split(match[3], "|")
	out := payload
	for index := count - 1; index > 0; index-- {
		var word string
		if index < len(words) {
			word = words[index]
		}
		if word == "" {
			continue
		}
		out = replaceWord(out, strconv.FormatInt(int64(index), 36), word)
	}
	out = strings.NewReplacer(`\'`, `'`, `\"`, `"`, `\\`, `\`).Replace(out)
	return out, nil
}

// replaceWord swaps token for word with the same word-boundary rules the
// page's RegExp('\\b' + token + '\\b') applies.
func replaceWord(payload, token, word string) string {
	re := regexp.MustCompile(`(^|[^0-9A-Za-z_])` + regexp.QuoteMeta(token) + `($|[^0-9A-Za-z_])`)
	return re.ReplaceAllString(payload, "${1}"+word+"${2}")
}

// fileFromConfig returns the first sources[].file, scoped to the sources
// array so tracks are ignored.
func fileFromConfig(config string) string {
	start := strings.Index(config, "sources:[")
	if start < 0 {
		return ""
	}
	end := strings.IndexByte(config[start:], ']')
	if end < 0 {
		return ""
	}
	match := fileRe.FindStringSubmatch(config[start : start+end])
	if match == nil {
		return ""
	}
	return match[1]
}

// captionsFromConfig returns the caption tracks the host listed. Thumbnail
// sprites and upload rows stay out.
func captionsFromConfig(config string) []protocol.SubtitleTrack {
	start := strings.Index(config, "tracks:[")
	if start < 0 {
		return nil
	}
	end := strings.IndexByte(config[start:], ']')
	if end < 0 {
		return nil
	}
	section := config[start : start+end]
	var tracks []protocol.SubtitleTrack
	seen := map[string]bool{}
	for _, block := range trackRe.FindAllString(section, -1) {
		kind := kindRe.FindStringSubmatch(block)
		if kind == nil || !strings.EqualFold(kind[1], "captions") {
			continue
		}
		file := fileRe.FindStringSubmatch(block)
		if file == nil || !strings.HasSuffix(strings.ToLower(file[1]), ".vtt") {
			continue
		}
		raw, ok := captionURL(file[1])
		if !ok || seen[raw] {
			continue
		}
		label := ""
		if lab := labelRe.FindStringSubmatch(block); lab != nil {
			label = lab[1]
		}
		if strings.EqualFold(label, "Upload captions") {
			continue
		}
		if label == "" {
			label = "English"
		}
		seen[raw] = true
		tracks = append(tracks, protocol.SubtitleTrack{
			URL:      raw,
			Label:    label,
			Language: captionLanguage(label),
			MimeType: "text/vtt",
		})
	}
	return tracks
}

// mediaURL accepts an HTTPS HLS master or direct file the host itself
// linked. Everything else is rejected.
func mediaURL(raw string) (string, bool) {
	text := strings.TrimSpace(raw)
	if text == "" || strings.Contains(text, "..") || strings.Contains(text, `\`) {
		return "", false
	}
	u, err := url.Parse(text)
	if err != nil || u.User != nil {
		return "", false
	}
	if !strings.EqualFold(u.Scheme, "https") {
		return "", false
	}
	path := strings.ToLower(u.Path)
	if !strings.HasSuffix(path, ".m3u8") && !strings.HasSuffix(path, ".mp4") {
		return "", false
	}
	return text, true
}

// playable shapes the final source with the headers the CDN expects.
func playable(file, referer string, captions []protocol.SubtitleTrack, dub bool, userAgent string) (*protocol.Source, error) {
	file, ok := mediaURL(file)
	if !ok {
		return nil, errNoFile
	}
	if !strings.HasPrefix(referer, "https://") {
		return nil, errNoFile
	}
	origin := httpx.Origin(referer)
	if origin == "" {
		return nil, errNoFile
	}
	label := "SUB"
	if dub {
		label = "DUB"
	}
	return &protocol.Source{
		URL: file,
		Headers: map[string]string{
			"Referer":    referer,
			"Origin":     origin,
			"User-Agent": userAgent,
		},
		Label:     label,
		Subtitles: captions,
	}, nil
}

func captionURL(raw string) (string, bool) {
	text := strings.TrimSpace(raw)
	if text == "" || strings.Contains(text, "..") || strings.Contains(text, `\`) {
		return "", false
	}
	u, err := url.Parse(text)
	if err != nil || u.User != nil {
		return "", false
	}
	if !strings.EqualFold(u.Scheme, "https") || !strings.HasSuffix(strings.ToLower(u.Path), ".vtt") {
		return "", false
	}
	return text, true
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

var captionNames = map[string]string{
	"english": "en", "japanese": "ja", "spanish": "es", "french": "fr",
	"german": "de", "portuguese": "pt", "italian": "it", "russian": "ru",
	"arabic": "ar", "korean": "ko", "chinese": "zh",
}

var (
	errNoState  = &resolve.Error{Kind: resolve.KindBlocked, Message: "anihosts state JSON missing"}
	errNoConfig = &resolve.Error{Kind: resolve.KindUnplayable, Message: "no player config"}
	errNoFile   = &resolve.Error{Kind: resolve.KindUnplayable, Message: "no playable file"}
)

var (
	stateRe = regexp.MustCompile(`(?s)<script[^>]*id="state-data"[^>]*>\s*(\{.*?\})\s*</script>`)
	packedRe = regexp.MustCompile(`(?s)eval\(function\(p,a,c,k,e,d\)\{[^{}]*?return p\}\((.+?),36,(\d+),'(.+?)'\.split\('\|'\)`)
	fileRe   = regexp.MustCompile(`file\s*:\s*["']([^"']+)["']`)
	trackRe  = regexp.MustCompile(`\{[^{}]*?file\s*:\s*"[^"]+"[^{}]*?\}`)
	kindRe   = regexp.MustCompile(`kind\s*:\s*["']([^"']+)["']`)
	labelRe  = regexp.MustCompile(`label\s*:\s*["']([^"']*)["']`)

	// Homepage "Latest releases" cards: the thumbnail anchor names the
	// title page, the badge carries "EP n", the h3 the title.
	latestCardRe  = regexp.MustCompile(`(?s)<div class="yt-card">[\s\S]*?<button[^>]*yt-card__more`)
	latestHrefRe  = regexp.MustCompile(`<a class="yt-card__thumb-box" href="(/anime/[^"]+)"`)
	latestTitleRe = regexp.MustCompile(`yt-card__title"[^>]*title="([^"]+)"`)
	latestBadgeRe = regexp.MustCompile(`yt-card__badge">EP (\d+)<`)
	latestImageRe = regexp.MustCompile(`class="img-fade yt-card__thumb" src="([^"]+)"`)
)

// htmlUnescape decodes the few entities the homepage uses.
func htmlUnescape(raw string) string {
	return strings.NewReplacer(
		"&amp;", "&",
		"&quot;", `"`,
		"&#34;", `"`,
		"&#39;", "'",
		"&apos;", "'",
		"&lt;", "<",
		"&gt;", ">",
	).Replace(raw)
}
