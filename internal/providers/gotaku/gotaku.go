// Package gotaku resolves gotaku.to episodes through the gotakudl.top embed
// player. Titles map by search and AniList identity, episodes carry sub/dub
// flags, and the embed page hands out a manifest token whose response names
// the encrypted HLS master. Playlists and segments are unwrapped server-side
// by the /v1/gotaku transform proxy, so the provider returns proxy URLs, not
// the raw CDN.
package gotaku

import (
	"context"
	"encoding/json"
	"errors"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/fampep/anifuse/internal/httpx"
	"github.com/fampep/anifuse/internal/protocol"
	"github.com/fampep/anifuse/internal/resolve"
)

const (
	// ID is the registry id.
	ID = "gotaku"
	// Name is the display name.
	Name = "Gotaku"
	// SiteURL is the site the resolver scrapes.
	SiteURL = "https://gotaku.to"
	// GOTAKU_DL is the CDN/embed host the site hands out, and the fallback
	// Origin header target.
	GOTAKU_DL = "https://gotakudl.top"
	// CHROME_UA is the browser both sites expect.
	CHROME_UA = "Mozilla/5.0 (Linux; Android 14) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0.0.0 Mobile Safari/537.36"
)

// Provider implements resolve.Provider for gotaku.to + gotakudl.top.
type Provider struct {
	client *httpx.Client
}

// New builds the provider with default timeouts (the Kotlin call timeout).
func New() *Provider {
	return &Provider{client: httpx.NewClient(CHROME_UA, 22*time.Second)}
}

func (p *Provider) ID() string      { return ID }
func (p *Provider) Name() string    { return Name }
func (p *Provider) SiteURL() string { return SiteURL }

type titleHit struct {
	id     string
	name   string
	poster string
}

type titleInfo struct {
	anilistID   int
	latestSub   int
	listedTotal int
}

type episodeHit struct {
	id     string
	number int
	sub    bool
	dub    bool
}

type variant struct {
	bandwidth int
	height    int
	url       string
	inf       string
}

type embedPage struct {
	stamp string
	base  string
}

// manifest is the decrypted embed manifest response.
type manifest struct {
	keySeed      string
	token        string
	source       string
	segmentBytes int
	captions     []protocol.SubtitleTrack
}

// Resolve turns an AniList id + episode into playable sources served through
// the /v1/gotaku transform proxy.
func (p *Provider) Resolve(ctx context.Context, req resolve.Request) ([]protocol.Source, error) {
	if req.AnilistID <= 0 || req.Episode <= 0 {
		return nil, resolve.Missing(Name, "anilistId and episode are required")
	}
	mode := "sub"
	if strings.EqualFold(req.Audio, "dub") {
		mode = "dub"
	}
	titleID, err := p.mapTitle(ctx, req.AnilistID, req.TitleHint)
	if err != nil {
		return nil, err
	}
	ep, err := p.findEpisode(ctx, titleID, req.Episode)
	if err != nil {
		return nil, err
	}
	if mode == "dub" && !ep.dub {
		return nil, resolve.Missing(Name, "dub unavailable for episode "+strconv.Itoa(req.Episode))
	}
	if mode != "dub" && !ep.sub {
		return nil, resolve.Missing(Name, "sub unavailable for episode "+strconv.Itoa(req.Episode))
	}
	embedType := "hard_sub"
	if mode == "dub" {
		embedType = "dub"
	}
	embedURL, err := p.fetchEmbedURL(ctx, ep.id, embedType)
	if err != nil {
		return nil, err
	}
	page, err := p.fetchEmbedPage(ctx, embedURL)
	if err != nil {
		return nil, err
	}
	nonce, token, err := BuildManifestToken(page.stamp)
	if err != nil {
		return nil, resolve.Unplayable(Name, "manifest token: "+err.Error())
	}
	mf, err := p.fetchManifest(ctx, page.base, token, nonce, embedURL)
	if err != nil {
		return nil, err
	}
	keySeed, err := DecodeKeySeed(mf.keySeed)
	if err != nil {
		return nil, resolve.Unplayable(Name, "manifest keySeed: "+err.Error())
	}
	masterRaw, err := p.httpBytes(ctx, mf.source, embedURL)
	if err != nil {
		return nil, err
	}
	masterPlain, err := DecryptPlaylist(masterRaw, keySeed, mf.token)
	if err != nil {
		return nil, resolve.Unplayable(Name, "master playlist decrypt: "+err.Error())
	}
	variants := sortVariantsTallest(parseVariants(string(masterPlain)))
	headers := streamHeaders(embedURL)
	out := make([]protocol.Source, 0, len(variants)+1)
	if len(variants) == 0 {
		if !validPlaylistURL(mf.source) {
			return nil, resolve.Unplayable(Name, "manifest source is not a playable https /p/1 URL")
		}
		out = append(out, protocol.Source{
			URL:     transformPlayURL(mf.source, embedURL, mf.keySeed, mf.token, mf.segmentBytes),
			Headers: headers,
			Label:   Name,
			Audio:   modeWord(mode),
		})
	} else {
		for _, v := range variants {
			if !validPlaylistURL(v.url) {
				continue
			}
			quality := heightLabel(v.height)
			label := quality
			if label == "" {
				label = Name
			}
			out = append(out, protocol.Source{
				URL:     transformPlayURL(v.url, embedURL, mf.keySeed, mf.token, mf.segmentBytes),
				Headers: headers,
				Label:   label,
				Quality: quality,
				Audio:   modeWord(mode),
			})
		}
		if len(out) == 0 {
			return nil, resolve.Unplayable(Name, "no playable variants in the master playlist")
		}
	}
	if len(out) > 0 && len(mf.captions) > 0 {
		out[0].Subtitles = mf.captions
	}
	return out, nil
}

// mapTitle finds the site title id for the AniList entry: search every query
// from buildTitleQueries, fall back to the browse listing when all searches
// came up empty, and pick the hit that really is the AniList id, preferring
// poster URLs that carry bx{id} and the entry with the most episodes.
func (p *Provider) mapTitle(ctx context.Context, anilistID int, titleHint string) (string, error) {
	var lastErr error
	var pools [][]titleHit
	for _, q := range buildTitleQueries(titleHint) {
		hits, err := p.searchTitles(ctx, q)
		if err != nil {
			lastErr = err
			continue
		}
		if len(hits) > 0 {
			pools = append(pools, hits)
		}
	}
	if len(pools) == 0 {
		if browse, err := p.searchTitles(ctx, ""); err != nil {
			lastErr = err
		} else if len(browse) > 0 {
			pools = append(pools, browse)
		}
	}
	for _, hits := range pools {
		if id := p.pickAnilistMatch(ctx, hits, anilistID); id != "" {
			return id, nil
		}
	}
	if len(pools) == 0 && lastErr != nil {
		return "", lastErr
	}
	return "", resolve.Missing(Name, "title not found for AniList #"+strconv.Itoa(anilistID))
}

// pickAnilistMatch mirrors the Kotlin: the same AniList id can exist more
// than once (full series vs a short cut), so poster hits are preferred and
// the entry with the most episodes wins.
func (p *Provider) pickAnilistMatch(ctx context.Context, hits []titleHit, anilistID int) string {
	var posterHits []titleHit
	for _, hit := range hits {
		if posterMatchesAnilist(hit.poster, anilistID) {
			posterHits = append(posterHits, hit)
		}
	}
	pool := hits
	if len(posterHits) > 0 {
		pool = posterHits
	}
	if len(pool) > 8 {
		pool = pool[:8]
	}
	bestID := ""
	bestCount := -1
	for _, hit := range pool {
		info, err := p.fetchTitleInfo(ctx, hit.id)
		if err != nil {
			continue
		}
		if info.anilistID != anilistID {
			continue
		}
		count := info.latestSub
		if info.listedTotal > count {
			count = info.listedTotal
		}
		if count > bestCount {
			bestCount = count
			bestID = hit.id
		}
	}
	return bestID
}

// posterMatchesAnilist checks whether the poster URL carries "bx{anilistId}"
// not followed by another digit. RE2 has no lookahead, so the (?!\d) from
// the Kotlin regex is a manual check on the byte after the digits.
func posterMatchesAnilist(poster string, anilistID int) bool {
	if anilistID <= 0 || strings.TrimSpace(poster) == "" {
		return false
	}
	needle := "bx" + strconv.Itoa(anilistID)
	for i := 0; i+len(needle) <= len(poster); i++ {
		if poster[i:i+len(needle)] != needle {
			continue
		}
		end := i + len(needle)
		if end == len(poster) || poster[end] < '0' || poster[end] > '9' {
			return true
		}
	}
	return false
}

func (p *Provider) fetchTitleInfo(ctx context.Context, titleID string) (titleInfo, error) {
	data, err := p.apiGet(ctx, "/api/v1/titles/"+titleID+"?k=10")
	if err != nil {
		return titleInfo{}, err
	}
	info := titleInfo{anilistID: -1}
	if root, ok := data.obj("data"); ok {
		if title, ok := root.obj("title"); ok {
			info.anilistID = optInt(title, "anilist_id", -1)
			if episodes, ok := title.obj("episodes"); ok {
				info.latestSub = optInt(episodes, "latest_sub", 0)
				info.listedTotal = optInt(episodes, "total", 0)
			}
		}
	}
	return info, nil
}

func (p *Provider) searchTitles(ctx context.Context, query string) ([]titleHit, error) {
	path := "/api/v1/titles?k=10"
	if strings.TrimSpace(query) != "" {
		path += "&q=" + enc(query)
	}
	data, err := p.apiGet(ctx, path)
	if err != nil {
		return nil, err
	}
	return parseTitleHits(data), nil
}

func (p *Provider) findEpisode(ctx context.Context, titleID string, episode int) (episodeHit, error) {
	data, err := p.apiGet(ctx, "/api/v1/titles/"+titleID+"/episodes?k=10")
	if err != nil {
		return episodeHit{}, err
	}
	for _, ep := range parseEpisodes(data) {
		if ep.number == episode {
			return ep, nil
		}
	}
	return episodeHit{}, resolve.Missing(Name, "episode "+strconv.Itoa(episode)+" not found on Gotaku")
}

func (p *Provider) fetchEmbedURL(ctx context.Context, episodeID, embedType string) (string, error) {
	data, err := p.apiGet(ctx, "/api/v1/episodes/"+episodeID+"/embed?type="+embedType+"&k=10")
	if err != nil {
		return "", err
	}
	embedURL := parseEmbedURL(data)
	if !strings.HasPrefix(embedURL, "http") {
		return "", resolve.Missing(Name, "Gotaku embed missing url")
	}
	return embedURL, nil
}

func parseEmbedURL(data jsonObj) string {
	if root, ok := data.obj("data"); ok {
		return strings.TrimSpace(root.str("url"))
	}
	return ""
}

func (p *Provider) fetchEmbedPage(ctx context.Context, embedURL string) (embedPage, error) {
	page, err := p.client.GetWithHeaders(ctx, embedURL, map[string]string{
		"Accept":  "*/*",
		"Referer": SiteURL,
		"Origin":  originOf(SiteURL),
	})
	if err != nil {
		return embedPage{}, resolve.Blocked(Name, err.Error())
	}
	if page.Code < 200 || page.Code > 299 {
		return embedPage{}, resolve.Blocked(Name, "HTTP "+strconv.Itoa(page.Code)+" for "+shortURL(embedURL))
	}
	parsed, ok := parseEmbedPage(page.Body)
	if !ok {
		return embedPage{}, resolve.Missing(Name, "Gotaku embed missing data-manifest-stamp/base")
	}
	return parsed, nil
}

// fetchManifest opens the embed manifest: base/{token} is b64url of the
// AES-GCM manifest body sealed under HKDF(EMBED_KEY, salt=nonce).
func (p *Provider) fetchManifest(ctx context.Context, manifestBase, token, nonce, embedURL string) (manifest, error) {
	u := strings.TrimRight(manifestBase, "/") + "/" + token
	body, err := p.httpBytes(ctx, u, embedURL)
	if err != nil {
		return manifest{}, err
	}
	plain, err := DecryptManifestResponse(body, nonce)
	if err != nil {
		return manifest{}, resolve.Unplayable(Name, "manifest decrypt: "+err.Error())
	}
	data, err := parseJSON([]byte(plain))
	if err != nil {
		return manifest{}, resolve.Unplayable(Name, "manifest JSON: "+err.Error())
	}
	return parseManifest(data), nil
}

// httpBytes mirrors the Kotlin httpBytes: browser UA, Accept */*, Referer
// and Origin.
func (p *Provider) httpBytes(ctx context.Context, u, referer string) ([]byte, error) {
	page, err := p.client.GetWithHeaders(ctx, u, map[string]string{
		"Accept":  "*/*",
		"Referer": referer,
		"Origin":  originOf(referer),
	})
	if err != nil {
		return nil, resolve.Blocked(Name, err.Error())
	}
	if page.Code < 200 || page.Code > 299 {
		return nil, resolve.Blocked(Name, "HTTP "+strconv.Itoa(page.Code)+" for "+shortURL(u))
	}
	return []byte(page.Body), nil
}

// apiGet fetches a gotaku.to /api/v1 path and opens the JSON, unsealing the
// bodies the site encrypts (sealed when they do not start with '{' or '[').
func (p *Provider) apiGet(ctx context.Context, path string) (jsonObj, error) {
	u := path
	if !strings.HasPrefix(path, "http") {
		u = SiteURL + path
	}
	page, err := p.client.GetWithHeaders(ctx, u, map[string]string{
		"Accept":  "*/*",
		"Referer": SiteURL,
		"Origin":  originOf(SiteURL),
	})
	if err != nil {
		return nil, resolve.Blocked(Name, err.Error())
	}
	if page.Code < 200 || page.Code > 299 {
		return nil, resolve.Blocked(Name, "HTTP "+strconv.Itoa(page.Code)+" for "+shortURL(u))
	}
	body, err := MaybeUnsealAPI([]byte(page.Body))
	if err != nil {
		return nil, resolve.Unplayable(Name, "sealed API body: "+err.Error())
	}
	return parseJSON(body)
}

// ---------------------------------------------------------------------------
// Parsers (package-level so tests reach them).

func parseTitleHits(data jsonObj) []titleHit {
	var out []titleHit
	for _, row := range data.arr("data") {
		o, ok := row.(map[string]any)
		if !ok {
			continue
		}
		jo := jsonObj(o)
		id := jo.str("id")
		if id == "" {
			continue
		}
		out = append(out, titleHit{id: id, name: jo.str("name"), poster: jo.str("poster_url")})
	}
	return out
}

func parseEpisodes(data jsonObj) []episodeHit {
	var out []episodeHit
	for _, row := range data.arr("data") {
		o, ok := row.(map[string]any)
		if !ok {
			continue
		}
		jo := jsonObj(o)
		id := jo.str("id")
		if id == "" {
			continue
		}
		out = append(out, episodeHit{
			number: optInt(jo, "number", -1),
			id:     id,
			sub:    optBool(jo, "sub", true),
			dub:    optBool(jo, "dub", false),
		})
	}
	return out
}

func parseManifest(data jsonObj) manifest {
	m := manifest{
		keySeed:      data.str("keySeed"),
		token:        data.str("token"),
		source:       data.str("source"),
		segmentBytes: 8192,
	}
	if obf, ok := data.obj("obf"); ok {
		if v := obf.get("segmentBytes"); v != nil {
			switch t := v.(type) {
			case float64:
				m.segmentBytes = int(t)
			case string:
				if n, err := strconv.Atoi(strings.TrimSpace(t)); err == nil {
					m.segmentBytes = n
				}
			}
		}
	}
	m.captions = parseCaptions(data)
	return m
}

func parseCaptions(data jsonObj) []protocol.SubtitleTrack {
	var out []protocol.SubtitleTrack
	for _, row := range data.arr("captions") {
		o, ok := row.(map[string]any)
		if !ok {
			continue
		}
		c := jsonObj(o)
		u := strings.TrimSpace(c.str("url"))
		if u == "" {
			u = strings.TrimSpace(c.str("file"))
		}
		if !strings.HasPrefix(u, "http") {
			continue
		}
		label := c.str("label")
		if label == "" {
			label = c.str("lang")
		}
		if label == "" {
			label = "Subs"
		}
		out = append(out, protocol.SubtitleTrack{
			URL:      u,
			Label:    label,
			Language: c.str("lang"),
		})
	}
	return out
}

// parseVariants reads EXT-X-STREAM-INF rows whose next line is an absolute
// http(s) URL, mirroring the Kotlin parser (no sorting here; the caller
// sorts tallest first like sortedByDescending).
func parseVariants(master string) []variant {
	lines := strings.Split(strings.ReplaceAll(master, "\r", ""), "\n")
	var out []variant
	for i := 0; i < len(lines); {
		line := strings.TrimSpace(lines[i])
		if strings.HasPrefix(line, "#EXT-X-STREAM-INF") {
			bandwidth := 0
			if m := bandwidthRe.FindStringSubmatch(line); m != nil {
				bandwidth, _ = strconv.Atoi(m[1])
			}
			height := 0
			if m := resolutionRe.FindStringSubmatch(line); m != nil {
				height, _ = strconv.Atoi(m[1])
			}
			next := ""
			if i+1 < len(lines) {
				next = strings.TrimSpace(lines[i+1])
			}
			if strings.HasPrefix(next, "http") {
				out = append(out, variant{bandwidth: bandwidth, height: height, url: next, inf: line})
				i += 2
				continue
			}
		}
		i++
	}
	return out
}

func sortVariantsTallest(variants []variant) []variant {
	sorted := append([]variant(nil), variants...)
	sort.SliceStable(sorted, func(i, j int) bool { return sorted[i].height > sorted[j].height })
	return sorted
}

var (
	dataStampRe = regexp.MustCompile(`(?i)data-manifest-stamp="([^"]+)"`)
	dataBaseRe  = regexp.MustCompile(`(?i)data-manifest-base="([^"]+)"`)
)

func parseEmbedPage(html string) (embedPage, bool) {
	stamp := dataStampRe.FindStringSubmatch(html)
	if stamp == nil {
		return embedPage{}, false
	}
	base := dataBaseRe.FindStringSubmatch(html)
	if base == nil {
		return embedPage{}, false
	}
	return embedPage{stamp: stamp[1], base: base[1]}, true
}

// ---------------------------------------------------------------------------
// Small helpers.

// transformPlayURL is the relative proxy URL the HTTP layer absolutizes.
func transformPlayURL(variantURL, embedURL, keySeed, token string, segmentBytes int) string {
	return "/v1/gotaku/play" +
		"?u=" + url.QueryEscape(variantURL) +
		"&r=" + url.QueryEscape(embedURL) +
		"&seed=" + url.QueryEscape(keySeed) +
		"&token=" + url.QueryEscape(token) +
		"&seg=" + strconv.Itoa(segmentBytes)
}

// validPlaylistURL keeps the provider's convention: HTTPS only, no userinfo,
// no path traversal, and a gotaku obfuscated playlist path the transform
// proxy accepts.
func validPlaylistURL(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil || !strings.EqualFold(u.Scheme, "https") || u.Hostname() == "" || u.User != nil {
		return false
	}
	if strings.Contains(u.Path, "..") || strings.Contains(u.Path, "\\") {
		return false
	}
	return strings.Contains(u.Path, "/p/1")
}

func streamHeaders(embedURL string) map[string]string {
	return map[string]string{
		"User-Agent": CHROME_UA,
		"Accept":     "*/*",
		"Referer":    embedURL,
		"Origin":     originOf(embedURL),
	}
}

var originRe = regexp.MustCompile(`(?i)^(https?://[^/]+)`)

func originOf(raw string) string {
	if m := originRe.FindStringSubmatch(raw); m != nil {
		return m[1]
	}
	return GOTAKU_DL
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

func modeWord(mode string) string {
	if mode == "dub" {
		return "DUB"
	}
	return "SUB"
}

func enc(s string) string {
	return url.QueryEscape(s)
}

func shortURL(u string) string {
	if len(u) > 96 {
		return u[:96]
	}
	return u
}

func substringBefore(s, sep string) string {
	if i := strings.Index(s, sep); i >= 0 {
		return s[:i]
	}
	return s
}

// ---------------------------------------------------------------------------
// Title query building (the Gotaku Kotlin port: || split, mal:NN exclusion,
// paren strip, head cut, season strip, take(8)).

var (
	malPartRe    = regexp.MustCompile(`(?i)^mal:\d+$`)
	parenRe      = regexp.MustCompile(`\s*\(.*?\)\s*`)
	spaceRunRe   = regexp.MustCompile(`\s+`)
	seasonTailRe = regexp.MustCompile(`(?i)\s+Season\s+\d+\s*$`)

	bandwidthRe  = regexp.MustCompile(`BANDWIDTH=(\d+)`)
	resolutionRe = regexp.MustCompile(`RESOLUTION=\d+x(\d+)`)
)

// buildTitleQueries expands the "Romaji || English || Native" hint into
// search queries: the seeds themselves, then per seed the paren-stripped,
// head-cut, and season-stripped forms, deduplicated in insertion order and
// capped at eight.
func buildTitleQueries(titleHint string) []string {
	raw := strings.TrimSpace(titleHint)
	if raw == "" {
		return nil
	}
	var out []string
	seen := make(map[string]bool)
	add := func(s string) {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	var parts []string
	for _, part := range strings.Split(raw, "||") {
		p := strings.TrimSpace(part)
		if titleLen(p) >= 2 && !malPartRe.MatchString(p) {
			parts = append(parts, p)
		}
	}
	seeds := parts
	if len(seeds) == 0 {
		seeds = []string{raw}
	}
	for _, full := range seeds {
		add(full)
	}
	for _, full := range seeds {
		noParen := strings.TrimSpace(spaceRunRe.ReplaceAllString(parenRe.ReplaceAllString(full, " "), " "))
		if titleLen(noParen) >= 2 {
			add(noParen)
		}
		head := strings.TrimSpace(substringBefore(substringBefore(substringBefore(full, ":"), "—"), " - "))
		if titleLen(head) >= 2 {
			add(head)
		}
		noSeason := strings.TrimSpace(seasonTailRe.ReplaceAllString(full, ""))
		if titleLen(noSeason) >= 2 {
			add(noSeason)
		}
	}
	var filtered []string
	for _, s := range out {
		if titleLen(s) >= 2 {
			filtered = append(filtered, s)
		}
		if len(filtered) == 8 {
			break
		}
	}
	return filtered
}

// titleLen mirrors the Kotlin String.length threshold (UTF-16 units); rune
// count matches it for every BMP title.
func titleLen(s string) int { return utf8.RuneCountInString(s) }

// ---------------------------------------------------------------------------
// JSON helpers over the dynamic site payloads.

type jsonObj map[string]any

func parseJSON(body []byte) (jsonObj, error) {
	var raw any
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, err
	}
	obj, ok := raw.(map[string]any)
	if !ok {
		return nil, errors.New("gotaku: JSON body is not an object")
	}
	return jsonObj(obj), nil
}

func (o jsonObj) get(key string) any { return o[key] }

func (o jsonObj) str(key string) string {
	if s, ok := o[key].(string); ok {
		return s
	}
	return ""
}

func (o jsonObj) obj(key string) (jsonObj, bool) {
	obj, ok := o[key].(map[string]any)
	if !ok {
		return nil, false
	}
	return jsonObj(obj), true
}

func (o jsonObj) arr(key string) []any {
	if arr, ok := o[key].([]any); ok {
		return arr
	}
	return nil
}

func optInt(o jsonObj, key string, fallback int) int {
	switch v := o.get(key).(type) {
	case float64:
		return int(v)
	case string:
		if n, err := strconv.Atoi(strings.TrimSpace(v)); err == nil {
			return n
		}
	}
	return fallback
}

func optBool(o jsonObj, key string, fallback bool) bool {
	switch v := o.get(key).(type) {
	case bool:
		return v
	case string:
		if strings.EqualFold(v, "true") {
			return true
		}
		if strings.EqualFold(v, "false") {
			return false
		}
	}
	return fallback
}
