// Package aniwaves resolves aniwaves.tv episodes. Search is the site
// filter; the episode is ajax/server/list, then ajax/sources, then the
// embed's getSources playlist. Sub or dub follows the server block.
package aniwaves

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
	ID = "aniwaves"
	// Name is the display name.
	Name = "aniwaves"
	// Origin is the site the resolver scrapes.
	Origin = "https://aniwaves.tv"
	// UserAgent is the browser the site expects.
	UserAgent = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/125.0.0.0 Safari/537.36"
)

// Provider implements providers.Provider for aniwaves.tv.
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
	id    string
	slug  string
	title string
}

// Resolve turns an AniList id + episode into a playable HLS master.
func (p *Provider) Resolve(ctx context.Context, req resolve.Request) ([]protocol.Source, error) {
	if req.AnilistID <= 0 || req.Episode <= 0 {
		return nil, resolve.Missing(Name, "anilistId and episode are required")
	}
	dub := strings.EqualFold(req.Audio, "dub")
	found, err := p.findShow(ctx, req.TitleHint)
	if err != nil {
		return nil, err
	}
	watch := p.origin + "/watch/" + found.slug + "/ep-" + strconv.Itoa(req.Episode)

	listed, err := p.get(ctx, p.origin+"/ajax/server/list?servers="+encode(found.id)+"&eps="+strconv.Itoa(req.Episode), watch, p.origin)
	if err != nil {
		return nil, err
	}
	listHTML, _ := jsonString(listed.Body, "result")
	linkID := pickLink(listHTML, dub)
	if linkID == "" {
		return nil, resolve.Missing(Name, "no server for this language")
	}

	sourced, err := p.get(ctx, p.origin+"/ajax/sources?id="+encode(linkID)+"&asi=0&autoPlay=0", watch, p.origin)
	if err != nil {
		return nil, err
	}
	payload := sourced.Body
	media := mediaURL(payload)
	embed := embedURL(payload)
	if media == "" {
		if embed == "" {
			return nil, resolve.Missing(Name, "no source")
		}
		page, err := p.get(ctx, embed, embedReferer(embed), embedOrigin(embed))
		if err != nil {
			return nil, err
		}
		dataID := dataID(page.Body)
		if dataID == "" {
			return nil, resolve.Missing(Name, "no embed data id")
		}
		sourcesURL := sourcesURL(embed, dataID)
		if sourcesURL == "" {
			return nil, resolve.Missing(Name, "no getSources url")
		}
		body, err := p.get(ctx, sourcesURL, embedReferer(embed), embedOrigin(embed))
		if err != nil {
			return nil, err
		}
		payload = body.Body
		media = mediaURL(payload)
	}
	if media == "" {
		return nil, resolve.Missing(Name, "no playable media")
	}
	playlistPage, err := p.get(ctx, media, embedReferer(embed), embedOrigin(embed))
	if err != nil {
		return nil, err
	}
	source := playable(media, playlistPage.Body, tracks(payload), dub)
	if source == nil {
		return nil, resolve.Missing(Name, "no playable playlist")
	}
	return []protocol.Source{*source}, nil
}

func (p *Provider) findShow(ctx context.Context, titleHint string) (*show, error) {
	title := cleanTitle(titleHint)
	if title == "" {
		return nil, resolve.Missing(Name, "needs a title to find this show")
	}
	page, err := p.get(ctx, p.origin+"/search?keyword="+encode(title), p.origin+"/", p.origin)
	if err != nil {
		return nil, err
	}
	found := pickShow(searchHits(page.Body), title)
	if found == nil {
		return nil, resolve.Missing(Name, "title not found")
	}
	return found, nil
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

func searchHits(html string) []*show {
	var hits []*show
	seen := map[string]bool{}
	for _, match := range cardRe.FindAllStringSubmatch(html, -1) {
		href := strings.TrimSpace(match[1])
		id := showID(href)
		if id == "" || seen[id] {
			continue
		}
		slug := strings.Trim(strings.TrimPrefix(href, "/watch/"), "/")
		if at := strings.Index(slug, "/"); at > 0 {
			slug = slug[:at]
		}
		title := strings.TrimSpace(match[2])
		if title == "" {
			continue
		}
		seen[id] = true
		hits = append(hits, &show{id: id, slug: slug, title: title})
	}
	return hits
}

func showID(href string) string {
	path := strings.SplitN(href, "?", 2)[0]
	path = strings.TrimSuffix(path, "/")
	slug := ""
	if match := watchRe.FindStringSubmatch(path); match != nil {
		slug = match[1]
	} else {
		parts := strings.Split(path, "/")
		slug = parts[len(parts)-1]
	}
	match := trailingDigitsRe.FindString(slug)
	if match == "" {
		return ""
	}
	if !strings.Contains(slug, "-") || len(slug) <= len(match) {
		return ""
	}
	return match
}

func pickShow(hits []*show, titleHint string) *show {
	want := normalize(titleHint)
	if want == "" {
		return nil
	}
	for _, hit := range hits {
		if normalize(hit.title) == want {
			return hit
		}
	}
	words := strings.Split(want, " ")
	var kept []string
	for _, word := range words {
		if len(word) >= 3 || strings.IndexFunc(word, func(r rune) bool { return r >= '0' && r <= '9' }) >= 0 {
			kept = append(kept, word)
		}
	}
	if len(kept) == 0 {
		return nil
	}
	var best *show
	bestCount := 1 << 30
	for _, hit := range hits {
		name := strings.Fields(normalize(hit.title))
		ok := true
		for _, word := range kept {
			if !containsWord(name, word) {
				ok = false
				break
			}
		}
		if ok && len(name) < bestCount {
			best = hit
			bestCount = len(name)
		}
	}
	return best
}

func containsWord(words []string, want string) bool {
	for _, word := range words {
		if word == want {
			return true
		}
	}
	return false
}

// pickLink reads the server link id for the requested language block.
func pickLink(html string, dub bool) string {
	kind := "sub"
	if dub {
		kind = "dub"
	}
	blockRe := regexp.MustCompile(`(?is)<div[^>]*data-type="` + kind + `"[^>]*>([\s\S]*?)</div>`)
	block := blockRe.FindStringSubmatch(html)
	if block == nil {
		return ""
	}
	match := linkIDRe.FindStringSubmatch(block[1])
	if match == nil {
		return ""
	}
	return strings.TrimSpace(match[1])
}

func embedURL(payload string) string {
	var root map[string]any
	if err := json.Unmarshal([]byte(payload), &root); err != nil {
		return ""
	}
	value, ok := root["result"].(map[string]any)
	if !ok {
		return ""
	}
	raw := strings.TrimSpace(str(value["url"]))
	if !strings.HasPrefix(raw, "https://") {
		return ""
	}
	return raw
}

func dataID(html string) string {
	match := dataIDRe.FindStringSubmatch(html)
	if match == nil {
		return ""
	}
	return strings.TrimSpace(match[1])
}

func embedOrigin(embedURL string) string {
	u, err := url.Parse(embedURL)
	if err != nil {
		return ""
	}
	port := ""
	if u.Port() != "" && u.Port() != "443" {
		port = ":" + u.Port()
	}
	return "https://" + u.Hostname() + port
}

func embedReferer(embedURL string) string { return embedURL }

func sourcesURL(embedURL, dataID string) string {
	origin := embedOrigin(embedURL)
	if origin == "" {
		return ""
	}
	u, err := url.Parse(embedURL)
	if err != nil {
		return ""
	}
	prefix := "embed-1"
	if match := embedPrefixRe.FindStringSubmatch(u.Path); match != nil {
		prefix = match[1]
	}
	return origin + "/" + prefix + "/getSources?id=" + encode(dataID)
}

// mediaURL reads the HLS source out of the ajax/sources payload.
func mediaURL(payload string) string {
	var root map[string]any
	if err := json.Unmarshal([]byte(payload), &root); err != nil {
		return ""
	}
	if file := fileFrom(root["sources"]); file != "" {
		return file
	}
	if result, ok := root["result"].(map[string]any); ok {
		if file := fileFrom(result["sources"]); file != "" {
			return file
		}
		if file := fileFrom(result["url"]); file != "" {
			return file
		}
	}
	return ""
}

// fileFrom accepts an HLS URL from a string, array, or array of objects.
func fileFrom(value any) string {
	switch v := value.(type) {
	case string:
		if strings.Contains(strings.ToLower(v), ".m3u8") && strings.HasPrefix(v, "https://") {
			return v
		}
	case []any:
		for _, item := range v {
			if file := fileFrom(item); file != "" {
				return file
			}
		}
	case map[string]any:
		file := strings.TrimSpace(str(v["file"]))
		if file == "" {
			file = strings.TrimSpace(str(v["url"]))
		}
		if file != "" && strings.HasPrefix(file, "https://") &&
			(strings.Contains(strings.ToLower(file), ".m3u8") || strings.Contains(strings.ToLower(str(v["type"])), "hls")) {
			return file
		}
	}
	return ""
}

func tracks(payload string) []protocol.SubtitleTrack {
	var root map[string]any
	if err := json.Unmarshal([]byte(payload), &root); err != nil {
		return nil
	}
	rows, _ := root["tracks"].([]any)
	if rows == nil {
		if result, ok := root["result"].(map[string]any); ok {
			rows, _ = result["tracks"].([]any)
		}
	}
	var out []protocol.SubtitleTrack
	seen := map[string]bool{}
	for _, row := range rows {
		obj, ok := row.(map[string]any)
		if !ok {
			continue
		}
		kind := strings.ToLower(str(obj["kind"]))
		if kind != "" && kind != "captions" && kind != "subtitles" {
			continue
		}
		raw := strings.TrimSpace(str(obj["src"]))
		if raw == "" {
			raw = strings.TrimSpace(str(obj["file"]))
		}
		if !strings.HasPrefix(raw, "https://") || seen[raw] {
			continue
		}
		label := strings.TrimSpace(str(obj["label"]))
		if label == "" {
			label = strings.TrimSpace(str(obj["srclang"]))
		}
		if label == "" {
			continue
		}
		seen[raw] = true
		out = append(out, protocol.SubtitleTrack{
			URL:      raw,
			Label:    label,
			Language: strings.TrimSpace(str(obj["srclang"])),
			MimeType: "text/vtt",
		})
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Language == "en" && out[j].Language != "en" })
	return out
}

func playable(media, playlist string, subtitles []protocol.SubtitleTrack, dub bool) *protocol.Source {
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
		URL: media,
		HLS: true,
		Headers: map[string]string{
			"Referer":    embedOrigin(media),
			"User-Agent": UserAgent,
		},
		Label:     label,
		Quality:   quality,
		Audio:     mode,
		Subtitles: subtitles,
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

func normalize(raw string) string {
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
	return ""
}

// jsonString decodes {"result": html} into the html string.
func jsonString(payload, key string) (string, bool) {
	var root map[string]any
	if err := json.Unmarshal([]byte(payload), &root); err != nil {
		return "", false
	}
	value, ok := root[key].(string)
	if !ok {
		return "", false
	}
	return value, true
}

var (
	malRe            = regexp.MustCompile(`(?i)\bmal:\d+\b`)
	heightRe         = regexp.MustCompile(`(?i)RESOLUTION=\d+x(\d+)`)
	cardRe           = regexp.MustCompile(`(?is)<a\s+href="(/watch/[^"#?]+)"[\s\S]*?<a\s+class="name\s+d-title"[^>]*>([^<]+)</a>`)
	watchRe          = regexp.MustCompile(`^/watch/([^/]+)`)
	trailingDigitsRe = regexp.MustCompile(`(\d+)$`)
	linkIDRe         = regexp.MustCompile(`data-link-id="([^"]+)"`)
	dataIDRe         = regexp.MustCompile(`data-id="([^"]+)"`)
	embedPrefixRe    = regexp.MustCompile(`/(embed-\d+)/`)
)
