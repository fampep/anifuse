// Package xanime resolves xanime.app episodes. The public client posts
// GraphQL to POST https://xanime.app/z2/ inside the site's AES-GCM
// envelope: search is get_q27, episodes get_q01, the player get_q07.
// The master playlist is HLS; variant playlists are named a-index-….ico.
package xanime

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
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
	ID = "xanime"
	// Name is the display name.
	Name = "xanime"
	// Origin is the site the resolver scrapes.
	Origin = "https://xanime.app"
	// UserAgent is the browser the site expects.
	UserAgent = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/125.0.0.0 Safari/537.36"
)

const api = Origin + "/z2/"

const (
	secret   = "xanime-ph25-obfuscation-secret-key-2026"
	maxPages = 12
)

const (
	searchQuery   = `query get_q27($select: SearchAnime_Select) { get_q27(select: $select) { items { id data { ani_id info_title info_slug al_id } } } }`
	episodesQuery = `query get_q01($select: AnimesEpisodesList_Select) { get_q01(select: $select) { paging { pages page } items { data { ep_id ep_index ep_sub_index sourcesNode_list { data { src_type } } } } } }`
	videoQuery    = `query get_q07($select: Episodes_Select) { get_q07(select: $select) { data { sourcesNode_list { data { src_type souPath m3u8_lists { iframe } track { label kind trackPath } } } } } }`
)

// Provider implements providers.Provider for xanime.app.
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
	epID, err := p.openEpisode(ctx, show.id, req.Episode, dub)
	if err != nil {
		return nil, err
	}
	source, err := p.openSource(ctx, epID, dub)
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
	selectBody := map[string]any{
		"word": title, "sortby": "field_score", "page": 1,
		"incGenres": []any{}, "excGenres": []any{}, "origStatus": "",
		"type": "", "sources": "", "ep_total": "",
		"ignoreGlobalGenres": false, "ignoreGlobalBlocks": false, "size": 24,
	}
	body := map[string]any{"query": searchQuery, "variables": map[string]any{"select": selectBody}}
	payload, err := p.post(ctx, body)
	if err != nil {
		return nil, err
	}
	found := pickShow(payload, anilistID, title)
	if found == nil {
		return nil, resolve.Missing(Name, "title not found")
	}
	return found, nil
}

func (p *Provider) openEpisode(ctx context.Context, id string, episode int, dub bool) (string, error) {
	for page := 1; page <= maxPages; page++ {
		selectBody := map[string]any{"ani_id": id, "init": 50, "size": 50, "page": page}
		body := map[string]any{"query": episodesQuery, "variables": map[string]any{"select": selectBody}}
		payload, err := p.post(ctx, body)
		if err != nil {
			return "", err
		}
		pages := episodePages(payload)
		if pages < 1 {
			pages = 1
		}
		row := episodeRow(payload, episode)
		if row != nil {
			if dub && !row.hasDub {
				return "", resolve.Missing(Name, "dub not available")
			}
			if !dub && !row.hasSub {
				return "", resolve.Missing(Name, "sub not available")
			}
			return row.id, nil
		}
		if page >= pages {
			break
		}
	}
	return "", resolve.Missing(Name, "episode not listed")
}

func (p *Provider) openSource(ctx context.Context, epID string, dub bool) (*protocol.Source, error) {
	selectBody := map[string]any{"id": epID}
	body := map[string]any{"query": videoQuery, "variables": map[string]any{"select": selectBody}}
	payload, err := p.post(ctx, body)
	if err != nil {
		return nil, err
	}
	links := links(payload, dub, p.origin, UserAgent)
	if len(links) == 0 {
		return nil, resolve.Missing(Name, "no sources")
	}
	wasBlocked := false
	for _, link := range links {
		page, err := p.client.GetText(ctx, link.url, p.origin+"/")
		if err != nil {
			continue
		}
		if blocked(page.Code, page.Body) {
			wasBlocked = true
			continue
		}
		if page.Code < 200 || page.Code > 299 {
			continue
		}
		if source := playable(link, page.Body, dub); source != nil {
			return source, nil
		}
	}
	if wasBlocked {
		return nil, resolve.Blocked(Name, "sources challenged")
	}
	return nil, resolve.Missing(Name, "no playable source")
}

// post seals the GraphQL body into the site's envelope and opens the reply.
func (p *Provider) post(ctx context.Context, body map[string]any) (string, error) {
	plain, err := json.Marshal(body)
	if err != nil {
		return "", resolve.Blocked(Name, "query build")
	}
	sealed, err := seal(string(plain))
	if err != nil {
		return "", resolve.Blocked(Name, "seal failed")
	}
	page, err := p.client.PostJSON(ctx, api, sealed, map[string]string{
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
	opened, err := open(page.Body)
	if err != nil {
		return "", resolve.Blocked(Name, "unreadable reply")
	}
	return opened, nil
}

// key is the AES key the site derives from its published secret.
func key() []byte {
	digest := sha256.Sum256([]byte(secret))
	return digest[:]
}

// seal wraps plain in the site's AES-GCM envelope.
func seal(plain string) (string, error) {
	block, err := aes.NewCipher(key())
	if err != nil {
		return "", err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", err
	}
	iv := make([]byte, gcm.NonceSize())
	if _, err := rand.Read(iv); err != nil {
		return "", err
	}
	sealed := gcm.Seal(nil, iv, []byte(plain), nil)
	envelope, err := json.Marshal(map[string]any{
		"v":  1,
		"iv": base64.StdEncoding.EncodeToString(iv),
		"ct": base64.StdEncoding.EncodeToString(sealed),
	})
	if err != nil {
		return "", err
	}
	return string(envelope), nil
}

// open unwraps the envelope; a plain object passes through unchanged.
func open(body string) (string, error) {
	var root map[string]any
	if err := json.Unmarshal([]byte(body), &root); err != nil {
		return "", err
	}
	ivText, _ := root["iv"].(string)
	ctText, _ := root["ct"].(string)
	if ivText == "" || ctText == "" {
		if _, ok := root["data"]; ok {
			return body, nil
		}
		return "", resolve.Blocked(Name, "no data in reply")
	}
	iv, err := base64.StdEncoding.DecodeString(ivText)
	if err != nil {
		return "", err
	}
	ct, err := base64.StdEncoding.DecodeString(ctText)
	if err != nil {
		return "", err
	}
	block, err := aes.NewCipher(key())
	if err != nil {
		return "", err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", err
	}
	plain, err := gcm.Open(nil, iv, ct, nil)
	if err != nil {
		return "", err
	}
	return string(plain), nil
}

type show struct {
	id        string
	title     string
	anilistID int
}

type episodeHit struct {
	id     string
	hasSub bool
	hasDub bool
}

type mediaLink struct {
	url       string
	headers   map[string]string
	subtitles []protocol.SubtitleTrack
}

// pickShow matches the search result on the AniList id, then the title.
func pickShow(payload string, anilistID int, titleHint string) *show {
	var root map[string]any
	if err := json.Unmarshal([]byte(payload), &root); err != nil {
		return nil
	}
	items := walkItems(root, "data", "get_q27", "items")
	if items == nil {
		return nil
	}
	var shows []*show
	for _, item := range items {
		row, ok := item.(map[string]any)
		if !ok {
			continue
		}
		data, ok := row["data"].(map[string]any)
		if !ok {
			data = row
		}
		id := str(data["ani_id"])
		if id == "" {
			id = str(row["id"])
		}
		if id == "" {
			continue
		}
		title := str(data["info_title"])
		if title == "" {
			title = id
		}
		shows = append(shows, &show{id: id, title: title, anilistID: intNum(data["al_id"])})
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

func episodePages(payload string) int {
	var root map[string]any
	if err := json.Unmarshal([]byte(payload), &root); err != nil {
		return 1
	}
	value, ok := root["data"].(map[string]any)
	if !ok {
		return 1
	}
	list, ok := value["get_q01"].(map[string]any)
	if !ok {
		return 1
	}
	paging, ok := list["paging"].(map[string]any)
	if !ok {
		return 1
	}
	pages := intNum(paging["pages"])
	if pages < 1 {
		pages = 1
	}
	return pages
}

func episodeRow(payload string, number int) *episodeHit {
	var root map[string]any
	if err := json.Unmarshal([]byte(payload), &root); err != nil {
		return nil
	}
	items := walkItems(root, "data", "get_q01", "items")
	for _, item := range items {
		row, ok := item.(map[string]any)
		if !ok {
			continue
		}
		data, ok := row["data"].(map[string]any)
		if !ok {
			continue
		}
		if intNum(data["ep_index"]) != number {
			continue
		}
		if part, present := data["ep_sub_index"]; present && part != nil {
			if intNum(part) != 0 {
				continue
			}
		}
		id := str(data["ep_id"])
		if id == "" {
			continue
		}
		types := sourceTypes(walkItems(data, "sourcesNode_list"))
		known := len(types) > 0
		return &episodeHit{
			id:     id,
			hasSub: !known || contains(types, "sub"),
			hasDub: !known || contains(types, "dub"),
		}
	}
	return nil
}

func sourceTypes(rows []any) []string {
	var types []string
	for _, row := range rows {
		obj, ok := row.(map[string]any)
		if !ok {
			continue
		}
		data, ok := obj["data"].(map[string]any)
		if !ok {
			continue
		}
		kind := strings.ToLower(strings.TrimSpace(str(data["src_type"])))
		if kind != "" {
			types = append(types, kind)
		}
	}
	return types
}

// links lists the sources for the requested language, direct paths first
// and iframes second.
func links(payload string, dub bool, origin, userAgent string) []mediaLink {
	var root map[string]any
	if err := json.Unmarshal([]byte(payload), &root); err != nil {
		return nil
	}
	rows := walkItems(root, "data", "get_q07", "data", "sourcesNode_list")
	want := "sub"
	if dub {
		want = "dub"
	}
	var out []mediaLink
	for _, row := range rows {
		obj, ok := row.(map[string]any)
		if !ok {
			continue
		}
		data, ok := obj["data"].(map[string]any)
		if !ok {
			continue
		}
		if strings.ToLower(strings.TrimSpace(str(data["src_type"]))) != want {
			continue
		}
		linkURL := absolute(str(data["souPath"]), origin)
		if linkURL == "" {
			linkURL = iframeURL(walkItems(data, "m3u8_lists"), origin)
		}
		if linkURL == "" {
			continue
		}
		out = append(out, mediaLink{
			url: linkURL,
			headers: map[string]string{
				"Referer":    origin + "/",
				"Origin":     origin,
				"User-Agent": userAgent,
			},
			subtitles: subtitles(walkItems(data, "track"), origin),
		})
	}
	return out
}

func iframeURL(rows []any, origin string) string {
	for _, row := range rows {
		obj, ok := row.(map[string]any)
		if !ok {
			continue
		}
		if linkURL := absolute(str(obj["iframe"]), origin); linkURL != "" {
			return linkURL
		}
	}
	return ""
}

func subtitles(rows []any, origin string) []protocol.SubtitleTrack {
	var tracks []protocol.SubtitleTrack
	seen := map[string]bool{}
	for _, row := range rows {
		obj, ok := row.(map[string]any)
		if !ok {
			continue
		}
		linkURL := absolute(str(obj["trackPath"]), origin)
		if linkURL == "" || seen[linkURL] {
			continue
		}
		kind := strings.ToLower(str(obj["kind"]))
		caption := kind == "" || kind == "captions" || kind == "subtitles" ||
			strings.Contains(strings.ToLower(linkURL), ".vtt") || strings.Contains(strings.ToLower(linkURL), ".srt")
		if !caption {
			continue
		}
		label := str(obj["label"])
		if label == "" {
			label = "English"
		}
		mime := "text/vtt"
		if strings.Contains(strings.ToLower(linkURL), ".srt") {
			mime = "application/x-subrip"
		}
		seen[linkURL] = true
		tracks = append(tracks, protocol.SubtitleTrack{URL: linkURL, Label: label, Language: label, MimeType: mime})
	}
	return tracks
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
		HLS:       true,
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

func absolute(raw, origin string) string {
	path := strings.TrimSpace(raw)
	switch {
	case strings.HasPrefix(path, "https://") || strings.HasPrefix(path, "http://"):
		return path
	case strings.HasPrefix(path, "//"):
		return "https:" + path
	case strings.HasPrefix(path, "/"):
		return strings.TrimSuffix(origin, "/") + path
	default:
		return ""
	}
}

func contains(list []string, want string) bool {
	for _, item := range list {
		if item == want {
			return true
		}
	}
	return false
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

var (
	malRe    = regexp.MustCompile(`(?i)\bmal:\d+\b`)
	heightRe = regexp.MustCompile(`RESOLUTION=\d+x(\d+)`)
)
