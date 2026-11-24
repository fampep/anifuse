// Package anikoto resolves anikoto.cz episodes through the site's MegaPlay
// embeds, with a kotoplay.to fallback when Anikoto is down. The watch page
// is mapped by search and title matching, the episode list comes from the
// multi-style ajax endpoint, and every playable file goes through the
// MegaPlay AES-CBC getSources envelope whose key, IV and PNG strip size are
// scraped from the player's public newclient.min.js at runtime.
package anikoto

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/fampep/anifuse/internal/httpx"
	"github.com/fampep/anifuse/internal/protocol"
	"github.com/fampep/anifuse/internal/resolve"
)

const (
	// ID is the registry id.
	ID = "anikoto"
	// Name is the display name.
	Name = "AniKoto"
	// Origin is the primary site the resolver scrapes.
	Origin = "https://anikoto.cz"
	// KotoplayOrigin is the mirror front end used when Anikoto is down.
	KotoplayOrigin = "https://kotoplay.to"
	// UserAgent is the browser both sites expect.
	UserAgent = "Mozilla/5.0 (Linux; Android 14) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0.0.0 Mobile Safari/537.36"
)

// Provider implements resolve.Provider for anikoto.cz + kotoplay.to.
type Provider struct {
	client   *httpx.Client
	crypto   *megaCrypto
	origin   string
	kotoplay string
}

// New builds the provider with default timeouts.
func New() *Provider {
	client := httpx.NewClient(UserAgent, 18*time.Second)
	return &Provider{
		client:   client,
		crypto:   newMegaCrypto(httpx.NewClient(UserAgent, 16*time.Second)),
		origin:   Origin,
		kotoplay: KotoplayOrigin,
	}
}

func (p *Provider) ID() string      { return ID }
func (p *Provider) Name() string    { return Name }
func (p *Provider) SiteURL() string { return Origin }

type mappedShow struct {
	siteID   string
	watchURL string
}

type kotoShow struct {
	slug     string
	chapters []kotoChapter
}

type siteEpisode struct {
	number int
	ids    string
	hasSub bool
	hasDub bool
}

type serverEmbed struct {
	url   string
	label string
}

type serverLink struct {
	id    string
	label string
}

type labeledSource struct {
	url     string
	quality string
}

type resolvedEmbed struct {
	origin    string
	file      string
	labeled   []labeledSource
	subtitles []protocol.SubtitleTrack
}

type kotoHit struct {
	slug  string
	title string
	aniID int
	malID int
}

type kotoChapter struct {
	number    int
	chapterID int
	hasSub    bool
	hasDub    bool
	malID     int
}

type kotoServerLink struct {
	url   string
	label string
}

// Resolve turns an AniList id + episode into playable MegaPlay files,
// trying anikoto.cz first and kotoplay.to when the primary is down or
// cannot serve the episode.
func (p *Provider) Resolve(ctx context.Context, req resolve.Request) ([]protocol.Source, error) {
	if req.AnilistID <= 0 || req.Episode <= 0 {
		return nil, resolve.Missing(Name, "anilistId and episode are required")
	}
	mode := "sub"
	if strings.EqualFold(req.Audio, "dub") {
		mode = "dub"
	}
	var lastErr error
	if p.hostAnswers(ctx, p.origin) {
		sources, err := p.resolveAnikoto(ctx, req, mode)
		if err == nil && len(sources) > 0 {
			return sources, nil
		}
		if errors.Is(err, context.Canceled) {
			return nil, err
		}
		if err != nil {
			lastErr = err
		}
	}
	if p.hostAnswers(ctx, p.kotoplay) {
		sources, err := p.resolveKotoplay(ctx, req, mode)
		if err == nil && len(sources) > 0 {
			return sources, nil
		}
		if errors.Is(err, context.Canceled) {
			return nil, err
		}
		if err != nil {
			lastErr = err
		}
	}
	if lastErr != nil {
		return nil, lastErr
	}
	return nil, resolve.Unplayable(Name, "no playable "+mode+" sources")
}

// resolveAnikoto walks the anikoto.cz front end: watch page, episode list,
// server list, server result, and the MegaPlay embeds.
func (p *Provider) resolveAnikoto(ctx context.Context, req resolve.Request, mode string) ([]protocol.Source, error) {
	titles, malID := hintParts(req.TitleHint)
	mapped, err := p.mapWatchPage(ctx, req.AnilistID, titles, malID)
	if err != nil {
		return nil, err
	}
	episodes, err := p.fetchEpisodes(ctx, mapped.siteID, mapped.watchURL)
	if err != nil {
		return nil, err
	}
	var ep *siteEpisode
	for i := range episodes {
		if episodes[i].number == req.Episode {
			ep = &episodes[i]
			break
		}
	}
	if ep == nil {
		return nil, resolve.Missing(Name, "episode "+strconv.Itoa(req.Episode)+" not found")
	}
	if mode == "dub" && !ep.hasDub {
		return nil, resolve.Missing(Name, "dub unavailable for episode "+strconv.Itoa(req.Episode))
	}
	if mode != "dub" && !ep.hasSub {
		return nil, resolve.Missing(Name, "sub unavailable for episode "+strconv.Itoa(req.Episode))
	}
	referer := strings.TrimRight(mapped.watchURL, "/") + "/ep-" + strconv.Itoa(req.Episode)
	embeds, err := p.resolveServerEmbeds(ctx, ep.ids, mode, mapped.watchURL)
	if err != nil {
		return nil, err
	}
	return p.collectPlayable(ctx, prioritizeEmbeds(embeds), referer, mode)
}

// resolveKotoplay walks the kotoplay.to /data routes for the same catalog.
func (p *Provider) resolveKotoplay(ctx context.Context, req resolve.Request, mode string) ([]protocol.Source, error) {
	titles, malID := hintParts(req.TitleHint)
	show, err := p.mapKotoplay(ctx, req.AnilistID, titles, malID)
	if err != nil {
		return nil, err
	}
	var ep *kotoChapter
	for i := range show.chapters {
		if show.chapters[i].number == req.Episode {
			ep = &show.chapters[i]
			break
		}
	}
	if ep == nil {
		return nil, resolve.Missing(Name, "episode "+strconv.Itoa(req.Episode)+" not found")
	}
	if mode == "dub" && !ep.hasDub {
		return nil, resolve.Missing(Name, "dub unavailable for episode "+strconv.Itoa(req.Episode))
	}
	if mode != "dub" && !ep.hasSub {
		return nil, resolve.Missing(Name, "sub unavailable for episode "+strconv.Itoa(req.Episode))
	}
	referer := p.kotoplay + "/watch/" + show.slug + "/" + strconv.Itoa(req.Episode)
	body, err := p.fetchJSON(ctx, p.kotoplay+"/data/servers?chapter="+strconv.Itoa(ep.chapterID), referer, "")
	if err != nil {
		return nil, err
	}
	links := parseKotoServerLinks(body, mode)
	if len(links) == 0 {
		return nil, resolve.Unplayable(Name, "no "+mode+" servers on kotoplay")
	}
	embeds := make([]serverEmbed, 0, len(links))
	for _, link := range links {
		embeds = append(embeds, serverEmbed{url: link.url, label: link.label})
	}
	return p.collectPlayable(ctx, prioritizeEmbeds(embeds), referer, mode)
}

// collectPlayable resolves up to 8 embeds into up to 16 deduplicated
// sources, sorted highest-quality first.
func (p *Provider) collectPlayable(ctx context.Context, embeds []serverEmbed, referer, mode string) ([]protocol.Source, error) {
	var out []protocol.Source
	seen := make(map[string]bool)
	lastErr := ""
	if len(embeds) > 8 {
		embeds = embeds[:8]
	}
	for _, embed := range embeds {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		resolved, err := p.resolveEmbed(ctx, embed.url, referer)
		if err != nil {
			lastErr = err.Error()
			continue
		}
		if !p.probeMaster(ctx, resolved.file, resolved.origin) {
			file := resolved.file
			if len(file) > 64 {
				file = file[:64]
			}
			lastErr = "CDN refused " + file
			continue
		}
		strip := p.crypto.stripBytes(ctx, resolved.origin)
		labeled := resolved.labeled
		if len(labeled) == 0 {
			labeled = []labeledSource{{url: resolved.file, quality: "Stream"}}
		}
		for _, l := range labeled {
			if !strings.HasPrefix(l.url, "http") || seen[l.url] {
				continue
			}
			seen[l.url] = true
			shown := qualityLabel(embed.label, l.quality)
			quality, audio, rating := sourceMeta(shown, mode)
			out = append(out, protocol.Source{
				URL:           l.url,
				Headers:       streamHeaders(resolved.origin),
				Label:         shown,
				StripBytes:    strip,
				Subtitles:     resolved.subtitles,
				Quality:       quality,
				Audio:         audio,
				QualityRating: rating,
			})
		}
		if len(out) >= 16 {
			break
		}
	}
	if len(out) == 0 {
		if lastErr == "" {
			lastErr = "no playable " + mode + " sources"
		}
		return nil, resolve.Unplayable(Name, lastErr)
	}
	sort.SliceStable(out, func(i, j int) bool {
		return qualityHeight(out[i].Label) > qualityHeight(out[j].Label)
	})
	return out, nil
}

// hostAnswers reports whether the front end answers at all. A dead
// connection skips that front end.
func (p *Provider) hostAnswers(ctx context.Context, base string) bool {
	page, err := p.client.GetWithHeaders(ctx, base, map[string]string{
		"Accept": "text/html,application/json",
	})
	if err != nil {
		return false
	}
	return page.Code < 500
}

// mapWatchPage finds the watch page for the AniList entry: a title-slug
// guess plus ajax search candidates, ranked by title identity, then
// validated against the page's AniList id, page title, and episode MAL ids.
func (p *Provider) mapWatchPage(ctx context.Context, anilistID int, titles []string, malID int) (mappedShow, error) {
	type candidate struct {
		url   string
		score int
	}
	var ranked []candidate
	if len(titles) > 8 {
		titles = titles[:8]
	}
	for _, q := range titles {
		if slug := titleSlug(q); slug != "" {
			ranked = append(ranked, candidate{url: p.origin + "/watch/" + slug, score: 1})
		}
		data, err := p.fetchJSON(ctx, p.origin+"/ajax/anime/search?keyword="+enc(q), p.origin, "")
		if err != nil {
			continue
		}
		html := ""
		if result, ok := data.obj("result"); ok {
			html = result.str("html")
		}
		if html == "" {
			html = data.str("result")
		}
		for _, cand := range parseSearchCandidates(html) {
			score := 2
			switch {
			case titlesIdentifySameWork(cand.name, titles):
				score = 100
			case searchNameMatches(cand.name, titles):
				score = 10
			}
			ranked = append(ranked, candidate{url: cand.url, score: score})
		}
	}
	sort.SliceStable(ranked, func(i, j int) bool { return ranked[i].score > ranked[j].score })
	seen := make(map[string]bool)
	var best []candidate
	for _, cand := range ranked {
		if !seen[cand.url] {
			seen[cand.url] = true
			best = append(best, cand)
		}
	}
	if len(best) > 8 {
		best = best[:8]
	}
	for _, cand := range best {
		abs := absoluteURL(cand.url, p.origin)
		html, err := p.fetchText(ctx, abs, p.origin)
		if err != nil {
			continue
		}
		if !pageMatchesAnilist(html, anilistID, titles) {
			continue
		}
		siteID := parseWatchID(html)
		if siteID == "" {
			continue
		}
		if !p.malAgrees(ctx, siteID, abs, malID) {
			continue
		}
		return mappedShow{siteID: siteID, watchURL: abs}, nil
	}
	return mappedShow{}, resolve.Missing(Name, "no watch page for AniList "+strconv.Itoa(anilistID))
}

// malAgrees rejects a page whose episodes belong to a different MAL id
// than the hint. A failed fetch never rejects the page.
func (p *Provider) malAgrees(ctx context.Context, siteID, watchURL string, malID int) bool {
	if malID <= 0 {
		return true
	}
	found, err := p.episodeMalIDs(ctx, siteID, watchURL)
	if err != nil || len(found) == 0 {
		return true
	}
	return found[malID]
}

func (p *Provider) episodeMalIDs(ctx context.Context, siteID, watchURL string) (map[int]bool, error) {
	data, err := p.fetchJSON(ctx, p.origin+"/ajax/episode/list/"+siteID+"?style=multi", watchURL, "")
	if err != nil {
		return nil, err
	}
	found := make(map[int]bool)
	for _, m := range dataMalRe.FindAllStringSubmatch(data.str("result"), -1) {
		if n, err := strconv.Atoi(m[1]); err == nil {
			found[n] = true
		}
	}
	return found, nil
}

func (p *Provider) fetchEpisodes(ctx context.Context, siteID, watchURL string) ([]siteEpisode, error) {
	data, err := p.fetchJSON(ctx, p.origin+"/ajax/episode/list/"+siteID+"?style=multi", watchURL, "")
	if err != nil {
		return nil, err
	}
	episodes := parseEpisodes(data.str("result"))
	if len(episodes) == 0 {
		return nil, resolve.Missing(Name, "empty episode list")
	}
	return episodes, nil
}

// resolveServerEmbeds turns the episode's server ids into MegaPlay embed
// URLs, in site order.
func (p *Provider) resolveServerEmbeds(ctx context.Context, episodeIDs, mode, watchURL string) ([]serverEmbed, error) {
	links, err := p.serverLinks(ctx, episodeIDs, mode, watchURL)
	if err != nil {
		return nil, err
	}
	if len(links) == 0 {
		return nil, resolve.Unplayable(Name, "no "+mode+" servers")
	}
	if len(links) > 6 {
		links = links[:6]
	}
	var out []serverEmbed
	for _, link := range links {
		result, err := p.fetchServerResult(ctx, link.id, watchURL)
		if err != nil {
			continue
		}
		embedURL := strings.TrimSpace(result.str("url"))
		if !strings.HasPrefix(embedURL, "http") {
			continue
		}
		out = append(out, serverEmbed{url: embedURL, label: link.label})
	}
	if len(out) == 0 {
		return nil, resolve.Unplayable(Name, "no embed URLs")
	}
	return out, nil
}

func (p *Provider) serverLinks(ctx context.Context, episodeIDs, mode, watchURL string) ([]serverLink, error) {
	data, err := p.fetchJSON(ctx, p.origin+"/ajax/server/list?servers="+enc(episodeIDs), watchURL, "")
	if err != nil {
		return nil, err
	}
	if intNum(data.get("status")) != 200 {
		return nil, nil
	}
	return parseServerLinks(data.str("result"), mode), nil
}

func (p *Provider) fetchServerResult(ctx context.Context, linkID, watchURL string) (jsonObj, error) {
	body, err := p.fetchJSON(ctx, p.origin+"/ajax/server?get="+enc(linkID), watchURL, "")
	if err != nil {
		return nil, err
	}
	if intNum(body.get("status")) != 200 {
		return nil, errors.New("anikoto: server status " + body.str("status"))
	}
	result, ok := body.obj("result")
	if !ok {
		return nil, errors.New("anikoto: no server result")
	}
	return result, nil
}

// resolveEmbed opens one MegaPlay embed page and pulls the getSources
// envelope: the direct file, the labeled variants, and the captions.
func (p *Provider) resolveEmbed(ctx context.Context, embedURL, referer string) (*resolvedEmbed, error) {
	pageURL := embedURL
	origin := originOf(embedURL)
	html, err := p.fetchText(ctx, embedURL, referer)
	if err != nil {
		return nil, err
	}
	dataID := parseDataID(html)
	if dataID == "" {
		if m := iframeSrcRe.FindStringSubmatch(html); m != nil && m[1] != "" {
			inner := resolveIframe(m[1], origin)
			if innerHTML, innerErr := p.fetchText(ctx, inner, embedURL); innerErr == nil {
				html = innerHTML
				origin = originOf(inner)
				pageURL = inner
				dataID = parseDataID(html)
			}
		}
	}
	if dataID == "" {
		if m := streamIDRe.FindStringSubmatch(embedURL); m != nil && isValidDataID(m[1]) {
			dataID = m[1]
		}
	}
	if dataID == "" {
		return nil, resolve.Unplayable(Name, "MegaPlay data-id missing")
	}
	// The current player calls getSources (getSourcesNew still answers but
	// is the legacy name); platform/st select the non-iOS variant.
	srcURL := origin + "/stream/getSources?id=" + enc(dataID) + "&platform=OTHER&st=other"
	if u, err := url.Parse(pageURL); err == nil {
		if s := u.Query().Get("s"); s != "" {
			srcURL += "&s=" + enc(s)
		}
	}
	src, err := p.fetchJSON(ctx, srcURL, pageURL, origin)
	if err != nil {
		return nil, err
	}
	src["_anikittyOrigin"] = origin
	file := p.crypto.extractFile(ctx, src)
	if file == "" {
		return nil, resolve.Unplayable(Name, "MegaPlay file missing")
	}
	labeled := p.crypto.extractLabeled(ctx, src)
	if len(labeled) == 0 {
		labeled = []labeledSource{{url: file, quality: "Stream"}}
	}
	return &resolvedEmbed{
		origin:    origin,
		file:      file,
		labeled:   labeled,
		subtitles: p.crypto.extractSubtitles(ctx, src),
	}, nil
}

// probeMaster does a cheap GET on the master playlist with the MegaPlay
// headers, skipping dead CDNs early.
func (p *Provider) probeMaster(ctx context.Context, masterURL, origin string) bool {
	page, err := p.client.GetWithHeaders(ctx, masterURL, map[string]string{
		"Accept":  "*/*",
		"Referer": origin + "/",
		"Origin":  origin,
	})
	if err != nil || page.Code < 200 || page.Code > 299 {
		return false
	}
	return strings.Contains(page.Body, "#EXTM3U")
}

func (p *Provider) fetchText(ctx context.Context, pageURL, referer string) (string, error) {
	page, err := p.client.GetWithHeaders(ctx, pageURL, map[string]string{
		"Accept":           "text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8",
		"Referer":          referer,
		"X-Requested-With": "XMLHttpRequest",
	})
	if err != nil {
		return "", resolve.Blocked(Name, err.Error())
	}
	if page.Code < 200 || page.Code > 299 {
		return "", resolve.Blocked(Name, "HTTP "+strconv.Itoa(page.Code)+" for "+pageURL)
	}
	return page.Body, nil
}

func (p *Provider) fetchJSON(ctx context.Context, pageURL, referer, origin string) (jsonObj, error) {
	headers := map[string]string{
		"Accept":           "application/json,text/plain,*/*",
		"Referer":          referer,
		"X-Requested-With": "XMLHttpRequest",
	}
	if origin != "" {
		headers["Origin"] = origin
	}
	page, err := p.client.GetWithHeaders(ctx, pageURL, headers)
	if err != nil {
		return nil, resolve.Blocked(Name, err.Error())
	}
	if page.Code < 200 || page.Code > 299 {
		return nil, resolve.Blocked(Name, "HTTP "+strconv.Itoa(page.Code)+" for "+pageURL)
	}
	return parseJSON(page.Body)
}

// Latest serves the Anikoto home "Latest Episode" widget feed.
func (p *Provider) Latest(ctx context.Context, limit int) ([]protocol.LatestEntry, error) {
	if limit <= 0 {
		limit = 40
	}
	if !p.hostAnswers(ctx, p.origin) {
		return nil, nil
	}
	data, err := p.fetchJSON(ctx, p.origin+"/ajax/home/widget/updated-all?page=1", p.origin+"/", "")
	if err != nil {
		return nil, nil
	}
	if intNum(data.get("status")) != 200 && data.str("status") != "200" {
		return nil, nil
	}
	items := parseWidgetHTML(data.str("result"))
	out := make([]protocol.LatestEntry, 0, len(items))
	for _, item := range items {
		episode := item.latestSub
		if episode <= 0 {
			episode = item.hrefEpisode
		}
		out = append(out, protocol.LatestEntry{
			Title:    item.title,
			Episode:  episode,
			ImageURL: item.posterURL,
			SiteURL:  p.origin + "/watch/" + item.slug + "/ep-" + strconv.Itoa(item.hrefEpisode),
		})
		if len(out) >= limit {
			break
		}
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// MegaPlay crypto: key, IV and PNG strip size scraped from newclient.min.js.

const (
	keyTTL        = 6 * time.Hour
	fallbackStrip = 252
	fallbackKey   = "i?LMTAx0Q6,:}50U"
	fallbackIV    = "W0;27ToaUpl_P%'c"
	megaPlayHome  = "https://megaplay.buzz"
)

type megaKeys struct {
	key       []byte
	iv        []byte
	strip     int
	fetchedAt time.Time
}

type megaCrypto struct {
	mu     sync.Mutex
	cached *megaKeys
	client *httpx.Client
}

func newMegaCrypto(client *httpx.Client) *megaCrypto {
	return &megaCrypto{client: client}
}

// keysFor returns the live keys when a recent fetch succeeded, refetches
// otherwise, and falls back to the emergency constants (with an expired
// timestamp so the next call retries) when every candidate fails.
func (c *megaCrypto) keysFor(ctx context.Context, playerOrigin string) *megaKeys {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := time.Now()
	if c.cached != nil && !c.cached.fetchedAt.IsZero() && now.Sub(c.cached.fetchedAt) < keyTTL {
		return c.cached
	}
	if fetched := c.fetchKeys(ctx, playerOrigin); fetched != nil {
		c.cached = fetched
		return fetched
	}
	c.cached = &megaKeys{key: padKeyBytes(fallbackKey, 32), iv: padKeyBytes(fallbackIV, 16), strip: fallbackStrip}
	return c.cached
}

// padKeyBytes turns a short key material string into a fixed-size AES key,
// zero-padded like the player's sourceEncPadKeyBytes.
func padKeyBytes(s string, n int) []byte {
	out := make([]byte, n)
	copy(out, []byte(s))
	return out
}

func (c *megaCrypto) stripBytes(ctx context.Context, playerOrigin string) int {
	return c.keysFor(ctx, playerOrigin).strip
}

// decryptEnc opens the AES-CBC getSources envelope with the player keys.
func (c *megaCrypto) decryptEnc(ctx context.Context, enc, playerOrigin string) (jsonObj, error) {
	keys := c.keysFor(ctx, playerOrigin)
	padded := enc + strings.Repeat("=", (4-len(enc)%4)%4)
	raw, err := base64.URLEncoding.DecodeString(padded)
	if err != nil {
		return nil, err
	}
	block, err := aes.NewCipher(keys.key)
	if err != nil {
		return nil, err
	}
	if len(raw) == 0 || len(raw)%aes.BlockSize != 0 {
		return nil, errors.New("anikoto: ciphertext not block-aligned")
	}
	plain := make([]byte, len(raw))
	cipher.NewCBCDecrypter(block, keys.iv).CryptBlocks(plain, raw)
	plain, err = pkcs7Unpad(plain)
	if err != nil {
		return nil, err
	}
	return parseJSON(string(plain))
}

// extractFile mirrors the Kotlin extractFile: the direct sources object,
// the sources array, then the decrypted envelope.
func (c *megaCrypto) extractFile(ctx context.Context, data jsonObj) string {
	if sources, ok := data.obj("sources"); ok {
		if file := sources.str("file"); strings.HasPrefix(file, "http") {
			return file
		}
	}
	for _, row := range data.arr("sources") {
		obj, ok := row.(map[string]any)
		if !ok {
			continue
		}
		if file := jsonObj(obj).str("file"); strings.HasPrefix(file, "http") {
			return file
		}
	}
	if enc := data.str("enc"); enc != "" {
		if decrypted, err := c.decryptEnc(ctx, enc, encOrigin(data)); err == nil {
			if file := decrypted.str("file"); strings.HasPrefix(file, "http") {
				return file
			}
			if sources, ok := decrypted.obj("sources"); ok {
				if file := sources.str("file"); strings.HasPrefix(file, "http") {
					return file
				}
			}
		}
	}
	return ""
}

// extractLabeled mirrors the Kotlin extractLabeled: labeled variants from
// sources/source (array, single object, or quality map), the decrypted
// envelope, and finally the plain file, sorted tallest-first.
func (c *megaCrypto) extractLabeled(ctx context.Context, data jsonObj) []labeledSource {
	var out []labeledSource
	seen := make(map[string]bool)
	addFrom := func(obj jsonObj) {
		file := obj.str("file")
		if file == "" {
			file = obj.str("url")
		}
		if !strings.HasPrefix(file, "http") || seen[file] {
			return
		}
		seen[file] = true
		quality := obj.str("quality")
		if quality == "" {
			quality = obj.str("label")
		}
		if quality == "" {
			quality = obj.str("res")
		}
		if quality == "" {
			quality = obj.str("resolution")
		}
		if quality == "" {
			quality = obj.str("type")
		}
		if quality == "" {
			quality = "Stream"
		}
		out = append(out, labeledSource{url: file, quality: quality})
	}
	absorb := func(v any) {
		switch t := v.(type) {
		case []any:
			for _, row := range t {
				if obj, ok := row.(map[string]any); ok {
					addFrom(jsonObj(obj))
				}
			}
		case map[string]any:
			obj := jsonObj(t)
			if obj.has("file") || obj.has("url") {
				addFrom(obj)
			} else {
				for key, value := range t {
					switch vv := value.(type) {
					case map[string]any:
						addFrom(jsonObj(vv))
					case string:
						if strings.HasPrefix(vv, "http") && !seen[vv] {
							seen[vv] = true
							out = append(out, labeledSource{url: vv, quality: key})
						}
					}
				}
			}
		}
	}
	absorb(data.get("sources"))
	absorb(data.get("source"))
	if len(out) == 0 {
		if enc := data.str("enc"); enc != "" {
			if decrypted, err := c.decryptEnc(ctx, enc, encOrigin(data)); err == nil {
				absorb(decrypted.get("sources"))
				absorb(decrypted.get("source"))
				if len(out) == 0 {
					if file := decrypted.str("file"); strings.HasPrefix(file, "http") {
						out = append(out, labeledSource{url: file, quality: "Stream"})
					}
				}
			}
		}
	}
	if len(out) == 0 {
		if file := c.extractFile(ctx, data); file != "" {
			out = append(out, labeledSource{url: file, quality: "Stream"})
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		return qualityHeight(out[i].quality) > qualityHeight(out[j].quality)
	})
	return out
}

// extractSubtitles mirrors the Kotlin extractSubtitles: caption tracks from
// the plain envelope, then from the decrypted one.
func (c *megaCrypto) extractSubtitles(ctx context.Context, data jsonObj) []protocol.SubtitleTrack {
	fromTracks := func(tracks []any) []protocol.SubtitleTrack {
		var out []protocol.SubtitleTrack
		for _, row := range tracks {
			obj, ok := row.(map[string]any)
			if !ok {
				continue
			}
			kind := jsonObj(obj).str("kind")
			if kind == "" {
				kind = jsonObj(obj).str("type")
			}
			if kind != "" &&
				!strings.Contains(strings.ToLower(kind), "caption") &&
				!strings.Contains(strings.ToLower(kind), "sub") {
				continue
			}
			file := jsonObj(obj).str("file")
			if file == "" {
				file = jsonObj(obj).str("url")
			}
			if !strings.HasPrefix(file, "http") {
				continue
			}
			label := jsonObj(obj).str("label")
			if label == "" {
				label = jsonObj(obj).str("lang")
			}
			if label == "" {
				label = jsonObj(obj).str("language")
			}
			if label == "" {
				label = fmt.Sprintf("Subtitle %d", len(out)+1)
			}
			language := jsonObj(obj).str("lang")
			if language == "" {
				language = jsonObj(obj).str("language")
			}
			lower := strings.ToLower(file)
			mime := ""
			switch {
			case strings.Contains(lower, ".vtt"):
				mime = "text/vtt"
			case strings.Contains(lower, ".srt"):
				mime = "application/x-subrip"
			case strings.Contains(lower, ".ass") || strings.Contains(lower, ".ssa"):
				mime = "text/x-ssa"
			}
			out = append(out, protocol.SubtitleTrack{
				URL:      file,
				Label:    label,
				Language: language,
				MimeType: mime,
			})
		}
		return out
	}
	if subs := fromTracks(data.arr("tracks")); len(subs) > 0 {
		return subs
	}
	if enc := data.str("enc"); enc != "" {
		if decrypted, err := c.decryptEnc(ctx, enc, encOrigin(data)); err == nil {
			if subs := fromTracks(decrypted.arr("tracks")); len(subs) > 0 {
				return subs
			}
		}
	}
	return nil
}

func encOrigin(data jsonObj) string {
	if origin := data.str("_anikittyOrigin"); origin != "" {
		return origin
	}
	return megaPlayHome
}

func (c *megaCrypto) fetchKeys(ctx context.Context, playerOrigin string) *megaKeys {
	origin := strings.TrimRight(playerOrigin, "/")
	candidates := []string{
		origin + "/lib/newclient.min.js",
		origin + "/lib/newclient.min.js?v=4.22",
		megaPlayHome + "/lib/newclient.min.js",
	}
	for _, candidate := range candidates {
		page, err := c.client.GetWithHeaders(ctx, candidate, map[string]string{
			"Accept":  "*/*",
			"Referer": origin + "/",
		})
		if err != nil || page.Code < 200 || page.Code > 299 || strings.TrimSpace(page.Body) == "" {
			continue
		}
		key, iv, ok := parseKeys(page.Body)
		if !ok {
			continue
		}
		return &megaKeys{
			key:       padKeyBytes(key, 32),
			iv:        padKeyBytes(iv, 16),
			strip:     parseStripBytes(page.Body),
			fetchedAt: time.Now(),
		}
	}
	return nil
}

func parseKeys(js string) (string, string, bool) {
	key := ""
	for _, re := range keyPatterns {
		if m := re.FindStringSubmatch(js); m != nil {
			key = m[1]
			break
		}
	}
	iv := ""
	for _, re := range ivPatterns {
		if m := re.FindStringSubmatch(js); m != nil {
			iv = m[1]
			break
		}
	}
	if len(key) != 16 || len(iv) != 16 {
		return "", "", false
	}
	return key, iv, true
}

func parseStripBytes(js string) int {
	m := stripPattern.FindStringSubmatch(js)
	if m == nil {
		return fallbackStrip
	}
	raw := m[1]
	if raw == "" {
		raw = m[2]
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n <= 0 {
		return fallbackStrip
	}
	return n
}

func pkcs7Unpad(b []byte) ([]byte, error) {
	if len(b) == 0 {
		return nil, errors.New("anikoto: empty plaintext")
	}
	n := int(b[len(b)-1])
	if n == 0 || n > aes.BlockSize || n > len(b) {
		return nil, errors.New("anikoto: bad padding")
	}
	for i := 0; i < n; i++ {
		if b[len(b)-1-i] != byte(n) {
			return nil, errors.New("anikoto: bad padding")
		}
	}
	return b[:len(b)-n], nil
}

// ---------------------------------------------------------------------------
// kotoplay.to /data routes.

func (p *Provider) mapKotoplay(ctx context.Context, anilistID int, titles []string, malID int) (kotoShow, error) {
	var hits []kotoHit
	if len(titles) > 6 {
		titles = titles[:6]
	}
	for _, q := range titles {
		data, err := p.fetchJSON(ctx, p.kotoplay+"/data/suggest?q="+enc(q), p.kotoplay+"/", "")
		if err != nil {
			continue
		}
		hits = append(hits, parseKotoSuggest(data)...)
	}
	ranked := rankKotoHits(hits, anilistID, titles, malID)
	if len(ranked) == 0 {
		return kotoShow{}, resolve.Missing(Name, "no watch page for AniList "+strconv.Itoa(anilistID))
	}
	if len(ranked) > 8 {
		ranked = ranked[:8]
	}
	for _, hit := range ranked {
		page, err := p.fetchJSON(ctx, p.kotoplay+"/data/anime?slug="+enc(hit.slug), p.kotoplay+"/anime/"+hit.slug, "")
		if err != nil {
			continue
		}
		anime, ok := kotoAnimeObject(page)
		if !ok {
			continue
		}
		chapters := kotoChaptersIfMatch(anime, anilistID, malID)
		if chapters == nil {
			continue
		}
		return kotoShow{slug: hit.slug, chapters: chapters}, nil
	}
	return kotoShow{}, resolve.Missing(Name, "no watch page for AniList "+strconv.Itoa(anilistID))
}

func parseKotoSuggest(data jsonObj) []kotoHit {
	arr := data.arr("anime")
	if arr == nil {
		return nil
	}
	var out []kotoHit
	for _, row := range arr {
		obj, ok := row.(map[string]any)
		if !ok {
			continue
		}
		slug := strings.TrimSpace(jsonObj(obj).str("slug"))
		title := strings.TrimSpace(jsonObj(obj).str("title"))
		if slug == "" || title == "" {
			continue
		}
		out = append(out, kotoHit{
			slug:  slug,
			title: title,
			aniID: intNum(obj["ani_id"]),
			malID: intNum(obj["mal_id"]),
		})
	}
	return out
}

func kotoAnimeObject(data jsonObj) (jsonObj, bool) {
	switch anime := data.get("anime").(type) {
	case map[string]any:
		return jsonObj(anime), true
	case []any:
		if len(anime) > 0 {
			if obj, ok := anime[0].(map[string]any); ok {
				return jsonObj(obj), true
			}
		}
		return nil, false
	default:
		if data.has("slug") || data.has("chapters") {
			return data, true
		}
		return nil, false
	}
}

func parseKotoChapters(anime jsonObj) []kotoChapter {
	arr := anime.arr("chapters")
	if arr == nil {
		return nil
	}
	var out []kotoChapter
	for _, row := range arr {
		obj, ok := row.(map[string]any)
		if !ok {
			continue
		}
		number := 0
		if s := jsonObj(obj).str("chapter_number"); s != "" {
			number, _ = strconv.Atoi(s)
		}
		if number <= 0 {
			number = intNum(obj["chapter_number"])
		}
		chapterID := intNum(obj["id"])
		if number <= 0 || chapterID <= 0 {
			continue
		}
		out = append(out, kotoChapter{
			number:    number,
			chapterID: chapterID,
			hasSub:    kotoFlag(jsonObj(obj), "is_sub"),
			hasDub:    kotoFlag(jsonObj(obj), "is_dub"),
			malID:     intNum(obj["mal_id"]),
		})
	}
	return out
}

func parseKotoServerLinks(data jsonObj, mode string) []kotoServerLink {
	servers, ok := data.obj("servers")
	if !ok {
		return nil
	}
	arr := servers.arr(mode)
	if arr == nil {
		return nil
	}
	var out []kotoServerLink
	for _, row := range arr {
		obj, ok := row.(map[string]any)
		if !ok {
			continue
		}
		link := strings.TrimSpace(jsonObj(obj).str("link"))
		if !strings.HasPrefix(link, "http") {
			continue
		}
		label := jsonObj(obj).str("name")
		if label == "" {
			label = "Anikoto"
		}
		out = append(out, kotoServerLink{url: link, label: label})
	}
	return out
}

// rankKotoHits orders suggest rows by their match evidence, best first.
func rankKotoHits(hits []kotoHit, anilistID int, titles []string, malID int) []kotoHit {
	best := make(map[string]kotoHit)
	scores := make(map[string]int)
	for _, hit := range hits {
		score, ok := kotoHitScore(hit, anilistID, titles, malID)
		if !ok {
			continue
		}
		if prev, exists := scores[hit.slug]; !exists || score > prev {
			scores[hit.slug] = score
			best[hit.slug] = hit
		}
	}
	out := make([]kotoHit, 0, len(best))
	for _, hit := range best {
		out = append(out, hit)
	}
	sort.SliceStable(out, func(i, j int) bool { return scores[out[i].slug] > scores[out[j].slug] })
	return out
}

func pickKotoHit(hits []kotoHit, anilistID int, titles []string, malID int) *kotoHit {
	ranked := rankKotoHits(hits, anilistID, titles, malID)
	if len(ranked) == 0 {
		return nil
	}
	return &ranked[0]
}

func kotoHitScore(hit kotoHit, anilistID int, titles []string, malID int) (int, bool) {
	if hit.aniID > 0 && hit.aniID != anilistID {
		return 0, false
	}
	malKnown := malID > 0
	sameTitle := titlesIdentifySameWork(hit.title, titles)
	if hit.aniID == anilistID {
		if malKnown && hit.malID > 0 && hit.malID != malID {
			return 0, false
		}
		if sameTitle {
			return 320, true
		}
		return 300, true
	}
	if malKnown {
		if hit.malID != malID {
			return 0, false
		}
		if sameTitle {
			return 310, true
		}
		return 200, true
	}
	if sameTitle {
		return 100, true
	}
	return 0, false
}

// kotoChaptersIfMatch returns nil when the anime page is a different work;
// empty chapters are a miss too, so the caller opens the next row.
func kotoChaptersIfMatch(anime jsonObj, anilistID, malID int) []kotoChapter {
	listedAni := intNum(anime.get("ani_id"))
	if listedAni > 0 && listedAni != anilistID {
		return nil
	}
	listedMal := intNum(anime.get("mal_id"))
	if malID > 0 && listedMal > 0 && listedMal != malID {
		return nil
	}
	chapters := parseKotoChapters(anime)
	if len(chapters) == 0 {
		return nil
	}
	if malID > 0 {
		chapterMals := make(map[int]bool)
		for _, ch := range chapters {
			if ch.malID > 0 {
				chapterMals[ch.malID] = true
			}
		}
		if len(chapterMals) > 0 && !chapterMals[malID] {
			return nil
		}
	}
	return chapters
}

func kotoFlag(row jsonObj, key string) bool {
	value, ok := row[key]
	if !ok || value == nil {
		return true
	}
	switch v := value.(type) {
	case bool:
		return v
	case float64:
		return v != 0
	case string:
		return v == "1" || strings.EqualFold(v, "true")
	default:
		return false
	}
}

// ---------------------------------------------------------------------------
// JSON helpers over the dynamic site payloads.

type jsonObj map[string]any

func parseJSON(body string) (jsonObj, error) {
	var raw any
	if err := json.Unmarshal([]byte(body), &raw); err != nil {
		return nil, err
	}
	obj, ok := raw.(map[string]any)
	if !ok {
		return nil, errors.New("anikoto: JSON body is not an object")
	}
	return jsonObj(obj), nil
}

func (o jsonObj) get(key string) any { return o[key] }

func (o jsonObj) has(key string) bool {
	_, ok := o[key]
	return ok
}

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

// ---------------------------------------------------------------------------
// Title helpers.

func hintParts(titleHint string) ([]string, int) {
	raw := strings.TrimSpace(titleHint)
	mal := 0
	if m := malHintRe.FindStringSubmatch(raw); m != nil {
		if n, err := strconv.Atoi(m[1]); err == nil && n > 0 {
			mal = n
		}
	}
	cleaned := malHintRe.ReplaceAllString(raw, " ")
	cleaned = wsPipeRe.ReplaceAllString(cleaned, " || ")
	cleaned = strings.TrimSpace(cleaned)
	cleaned = strings.Trim(cleaned, "|")
	cleaned = strings.TrimSpace(cleaned)
	return buildTitleQueries(cleaned), mal
}

func buildTitleQueries(hint string) []string {
	raw := strings.TrimSpace(hint)
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
		if trimmed := strings.TrimSpace(part); trimmed != "" {
			parts = append(parts, trimmed)
		}
	}
	if len(parts) == 0 {
		parts = []string{raw}
	}
	for _, p := range parts {
		add(p)
		add(seasonWordRe.ReplaceAllStringFunc(p, func(m string) string {
			n, _ := strconv.Atoi(numRe.FindString(m))
			return ordinalFor(n) + " Season"
		}))
		add(seasonFirstRe.ReplaceAllStringFunc(p, func(m string) string {
			return "Season " + numRe.FindString(m)
		}))
		for _, sub := range strings.FieldsFunc(p, func(r rune) bool { return r == ':' || r == '—' }) {
			sub = strings.TrimSpace(sub)
			if utf8.RuneCountInString(sub) >= 3 {
				add(sub)
			}
		}
		base := strings.TrimSpace(seasonTailRe.ReplaceAllString(p, ""))
		if utf8.RuneCountInString(base) >= 3 {
			add(base)
		}
	}
	return out
}

func ordinalFor(n int) string {
	switch n {
	case 1:
		return "1st"
	case 2:
		return "2nd"
	case 3:
		return "3rd"
	default:
		return strconv.Itoa(n) + "th"
	}
}

func titleSlug(title string) string {
	var sb strings.Builder
	dash := false
	for _, ch := range title {
		switch {
		case unicode.IsLetter(ch) || unicode.IsDigit(ch):
			sb.WriteRune(unicode.ToLower(ch))
			dash = false
		case sb.Len() > 0 && !dash:
			sb.WriteByte('-')
			dash = true
		}
	}
	return strings.TrimRight(sb.String(), "-")
}

func normalizeTitleKey(title string) string {
	s := strings.ToLower(htmlUnescape(title))
	s = ordSeasonRe.ReplaceAllString(s, " season $1 ")
	s = seasonNumRe.ReplaceAllString(s, " season $1 ")
	s = partNumRe.ReplaceAllString(s, " part $1 ")
	s = word2ndRe.ReplaceAllString(s, " 2 ")
	s = word3rdRe.ReplaceAllString(s, " 3 ")
	s = ordWordRe.ReplaceAllString(s, " $1 ")
	var sb strings.Builder
	space := false
	for _, ch := range s {
		if unicode.IsLetter(ch) || unicode.IsDigit(ch) {
			sb.WriteRune(ch)
			space = false
		} else if sb.Len() > 0 && !space {
			sb.WriteByte(' ')
			space = true
		}
	}
	return strings.TrimSpace(sb.String())
}

func identityKey(title string) string {
	stripped := yearRe.ReplaceAllString(normalizeTitleKey(title), " ")
	stripped = strings.TrimSpace(spaceRunRe.ReplaceAllString(stripped, " "))
	var tokens []string
	for _, tok := range strings.Fields(stripped) {
		if utf8.RuneCountInString(tok) > 1 || allDigits(tok) {
			tokens = append(tokens, tok)
		}
	}
	return strings.Join(tokens, " ")
}

func allDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if !unicode.IsDigit(r) {
			return false
		}
	}
	return true
}

func titlesIdentifySameWork(pageTitle string, expected []string) bool {
	page := identityKey(pageTitle)
	if page == "" {
		return false
	}
	for _, title := range expected {
		if identityKey(title) == page {
			return true
		}
	}
	return false
}

func titlesLooselyMatch(a, b string) bool {
	na := normalizeTitleKey(a)
	nb := normalizeTitleKey(b)
	if na == "" || nb == "" {
		return false
	}
	if na == nb {
		return true
	}
	la, lb := utf8.RuneCountInString(na), utf8.RuneCountInString(nb)
	shorter, longer := la, lb
	if shorter > longer {
		shorter, longer = longer, shorter
	}
	if (strings.Contains(na, nb) || strings.Contains(nb, na)) && shorter >= 8 && shorter*10 >= longer*8 {
		return true
	}
	ta := wordSet(na)
	tb := wordSet(nb)
	if len(ta) == 0 || len(tb) == 0 {
		return false
	}
	intersection := 0
	for word := range ta {
		if tb[word] {
			intersection++
		}
	}
	need := min(3, min(len(ta), len(tb)))
	return intersection >= need
}

func wordSet(s string) map[string]bool {
	out := make(map[string]bool)
	for _, tok := range strings.Fields(s) {
		if utf8.RuneCountInString(tok) > 2 {
			out[tok] = true
		}
	}
	return out
}

// ---------------------------------------------------------------------------
// HTML parsers.

func parseWatchID(html string) string {
	m := watchIDRe.FindStringSubmatch(html)
	if m == nil {
		return ""
	}
	if m[1] != "" {
		return m[1]
	}
	return m[2]
}

func pageAnilistID(html string) int {
	for _, re := range pageAnilistPatterns {
		if m := re.FindStringSubmatch(html); m != nil {
			if n, err := strconv.Atoi(m[1]); err == nil {
				return n
			}
		}
	}
	return 0
}

func parsePageTitle(html string) string {
	if m := h1TitleRe.FindStringSubmatch(html); m != nil {
		if title := htmlUnescape(stripTags(m[1])); title != "" {
			return title
		}
	}
	if m := titleTagRe.FindStringSubmatch(html); m != nil {
		if title := htmlUnescape(strings.TrimSpace(m[1])); title != "" {
			return title
		}
	}
	return ""
}

func pageMatchesAnilist(html string, anilistID int, expectedTitles []string) bool {
	if id := pageAnilistID(html); id > 0 {
		return id == anilistID
	}
	title := parsePageTitle(html)
	if title == "" {
		return false
	}
	return titlesIdentifySameWork(title, expectedTitles)
}

func searchNameMatches(candidateName string, expectedTitles []string) bool {
	for _, title := range expectedTitles {
		if titlesLooselyMatch(title, candidateName) {
			return true
		}
	}
	return false
}

type searchCandidate struct {
	url  string
	name string
}

func parseSearchCandidates(html string) []searchCandidate {
	var out []searchCandidate
	seen := make(map[string]bool)
	for _, m := range searchItemRe.FindAllStringSubmatch(html, -1) {
		u := htmlUnescape(m[1])
		if seen[u] {
			continue
		}
		seen[u] = true
		out = append(out, searchCandidate{url: u, name: htmlUnescape(stripTags(m[2]))})
	}
	return out
}

func parseEpisodes(html string) []siteEpisode {
	var out []siteEpisode
	for _, m := range episodeRe.FindAllStringSubmatch(html, -1) {
		number, err := strconv.Atoi(m[1])
		if err != nil || number <= 0 {
			continue
		}
		out = append(out, siteEpisode{
			number: number,
			hasSub: m[2] == "1",
			hasDub: m[3] == "1",
			ids:    htmlUnescape(m[4]),
		})
	}
	return out
}

func parseServerLinks(html, mode string) []serverLink {
	needle := `data-type="` + mode + `"`
	start := strings.Index(html, needle)
	if start < 0 && mode == "dub" {
		start = strings.Index(html, `data-type="sub"`)
	}
	if start < 0 {
		start = 0
	}
	end := len(html)
	if relative := strings.Index(html[start+1:], `<div class="type"`); relative >= 0 {
		end = start + 1 + relative
	}
	var out []serverLink
	for _, m := range serverLiRe.FindAllStringSubmatch(html[start:end], -1) {
		id := htmlUnescape(m[1])
		if id == "" {
			continue
		}
		out = append(out, serverLink{id: id, label: htmlUnescape(stripTags(m[2]))})
	}
	return out
}

func isValidDataID(id string) bool {
	if id == "" || utf8.RuneCountInString(id) > 256 {
		return false
	}
	for _, r := range id {
		if unicode.IsLetter(r) || unicode.IsDigit(r) || r == '.' || r == '-' || r == '_' {
			continue
		}
		return false
	}
	return true
}

func parseDataID(html string) string {
	if m := playerIDRe.FindStringSubmatch(html); m != nil {
		id := m[1]
		if id == "" {
			id = m[2]
		}
		if isValidDataID(id) {
			return id
		}
	}
	for _, m := range anyDataIDRe.FindAllStringSubmatch(html, -1) {
		if isValidDataID(m[1]) {
			return m[1]
		}
	}
	return ""
}

// ---------------------------------------------------------------------------
// Small helpers.

func originOf(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme == "" || u.Hostname() == "" {
		return megaPlayHome
	}
	return u.Scheme + "://" + u.Hostname()
}

func resolveIframe(src, origin string) string {
	switch {
	case strings.HasPrefix(src, "//"):
		return "https:" + src
	case strings.HasPrefix(src, "/"):
		return origin + src
	default:
		return src
	}
}

func absoluteURL(raw, base string) string {
	if strings.HasPrefix(raw, "http") {
		return raw
	}
	return base + raw
}

func streamHeaders(origin string) map[string]string {
	return map[string]string{
		"Referer":    origin + "/",
		"Origin":     origin,
		"User-Agent": UserAgent,
	}
}

func qualityLabel(server, quality string) string {
	s := strings.TrimSpace(server)
	q := strings.TrimSpace(quality)
	switch {
	case s == "" && (q == "" || strings.EqualFold(q, "stream")):
		return "Anikoto"
	case s == "":
		return q
	case q == "" || strings.EqualFold(q, "stream"):
		return s
	default:
		return s + " · " + q
	}
}

// qualityHeight parses 1080 / 720 / "1080p" style labels for sorting
// highest-first.
func qualityHeight(label string) int {
	if m := pHeightRe.FindStringSubmatch(label); m != nil {
		if h, err := strconv.Atoi(m[1]); err == nil {
			return h
		}
	}
	if m := bareHeightRe.FindStringSubmatch(label); m != nil {
		if h, err := strconv.Atoi(m[1]); err == nil {
			return h
		}
	}
	lower := strings.ToLower(label)
	switch {
	case strings.Contains(lower, "ultra") || strings.Contains(lower, "4k"):
		return 2160
	case strings.Contains(lower, "hd") || strings.Contains(lower, "high"):
		return 1080
	case strings.Contains(lower, "sd") || strings.Contains(lower, "low"):
		return 480
	default:
		return 0
	}
}

// prioritizeEmbeds prefers non-tcdn embeds first, keeping tcdn (PNG-stripped)
// ones as fallback.
func prioritizeEmbeds(embeds []serverEmbed) []serverEmbed {
	var prefer, rest []serverEmbed
	for _, embed := range embeds {
		if strings.Contains(strings.ToLower(embed.url), "tcdn") {
			rest = append(rest, embed)
		} else {
			prefer = append(prefer, embed)
		}
	}
	return append(prefer, rest...)
}

// sourceMeta is the BuiltInSourceMeta.anikoto port: quality and grade come
// from the label; the audio mode word carries the language.
func sourceMeta(label, mode string) (quality, audio, rating string) {
	quality = resolutionToken(label)
	rating = gradeToken(label)
	if mode == "dub" {
		audio = "DUB"
	} else {
		audio = "SUB"
	}
	return quality, audio, rating
}

func resolutionToken(text string) string {
	if m := resolutionRe.FindStringSubmatch(text); m != nil {
		if h, err := strconv.Atoi(m[1]); err == nil {
			return heightLabel(h)
		}
	}
	if m := dimPRe.FindStringSubmatch(text); m != nil {
		if h, err := strconv.Atoi(m[1]); err == nil {
			return heightLabel(h)
		}
	}
	if word4KRe.MatchString(text) {
		return "4K"
	}
	return ""
}

func gradeToken(text string) string {
	m := gradeRe.FindStringSubmatch(text)
	if m == nil {
		return ""
	}
	return strings.ToUpper(m[1])
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

func htmlUnescape(s string) string {
	return strings.NewReplacer(
		"&amp;", "&",
		"&#039;", "'",
		"&apos;", "'",
		"&quot;", "\"",
		"&lt;", "<",
		"&gt;", ">",
		"&nbsp;", " ",
		"\\/", "/",
	).Replace(s)
}

func stripTags(s string) string {
	return strings.TrimSpace(tagRe.ReplaceAllString(s, ""))
}

func enc(s string) string {
	return url.QueryEscape(s)
}

// ---------------------------------------------------------------------------
// Widget feed parser (AnikotoLatestParser port). The Kotlin ITEM regex uses
// a lookahead RE2 cannot express; the split below is the Kotlin fallback
// branch and produces the same blocks.

type latestItem struct {
	siteID      string
	slug        string
	title       string
	posterURL   string
	hrefEpisode int
	latestSub   int
	latestDub   int
}

func parseWidgetHTML(html string) []latestItem {
	if strings.TrimSpace(html) == "" {
		return nil
	}
	var out []latestItem
	seen := make(map[string]bool)
	blocks := itemSplitRe.Split(html, -1)
	if len(blocks) > 0 {
		blocks = blocks[1:]
	}
	for _, block := range blocks {
		m := latestHrefRe.FindStringSubmatch(block)
		if m == nil {
			continue
		}
		slug := m[1]
		if seen[slug] {
			continue
		}
		seen[slug] = true
		tip := ""
		if t := latestTipRe.FindStringSubmatch(block); t != nil {
			tip = t[1]
		}
		if tip == "" {
			continue
		}
		episode, err := strconv.Atoi(m[2])
		if err != nil {
			continue
		}
		sub := 0
		if s := latestSubRe.FindStringSubmatch(block); s != nil {
			sub, _ = strconv.Atoi(s[1])
		}
		dub := 0
		if d := latestDubRe.FindStringSubmatch(block); d != nil {
			dub, _ = strconv.Atoi(d[1])
		}
		title := ""
		if n := latestNameRe.FindStringSubmatch(block); n != nil {
			title = strings.TrimSpace(htmlUnescape(stripTags(n[1])))
		}
		if title == "" {
			title = strings.ReplaceAll(slug, "-", " ")
		}
		poster := ""
		if img := latestPosterRe.FindStringSubmatch(block); img != nil {
			poster = htmlUnescape(img[1])
		}
		out = append(out, latestItem{
			siteID:      tip,
			slug:        slug,
			title:       title,
			posterURL:   poster,
			hrefEpisode: episode,
			latestSub:   sub,
			latestDub:   dub,
		})
	}
	return out
}

// ---------------------------------------------------------------------------
// Regexes. RE2-safe: the Kotlin lookahead in the widget ITEM regex is
// replaced by the split approach above.

var (
	watchIDRe    = regexp.MustCompile(`(?i)id=["']watch-main["'][^>]*\bdata-id=["'](\d+)["']|data-id=["'](\d+)["'][^>]*\bid=["']watch-main["']`)
	searchItemRe = regexp.MustCompile(`(?is)<a\s+class=["'][^"']*\bitem\b[^"']*["'][^>]+href=["']([^"']+/watch/[^"']+)["'].{0,900}?<div\s+class=["'][^"']*\bname\b[^"']*["'][^>]*>(.*?)</div>`)
	episodeRe    = regexp.MustCompile(`(?is)<a\b[^>]*\bdata-num=["'](\d+)["'][^>]*\bdata-sub=["']([^"']*)["'][^>]*\bdata-dub=["']([^"']*)["'][^>]*\bdata-ids=["']([^"']+)["'][^>]*>`)
	dataMalRe    = regexp.MustCompile(`(?i)\bdata-mal=["'](\d+)["']`)
	malHintRe    = regexp.MustCompile(`(?i)\bmal:(\d+)\b`)
	serverLiRe   = regexp.MustCompile(`(?is)<li\b[^>]*\bdata-link-id=["']([^"']+)["'][^>]*>(.*?)</li>`)
	playerIDRe   = regexp.MustCompile(`(?i)id=["']megaplay-player["'][^>]*data-id=["']([^"']+)["']|data-id=["']([^"']+)["'][^>]*id=["']megaplay-player["']`)
	anyDataIDRe  = regexp.MustCompile(`(?i)data-id=["']([^"']+)["']`)
	iframeSrcRe  = regexp.MustCompile(`(?i)<iframe[^>]+\bsrc=["']([^"']*/stream/[^"']*)["']`)
	streamIDRe   = regexp.MustCompile(`(?i)/stream/s-\d+/([^/]+)/`)
	tagRe        = regexp.MustCompile(`<[^>]+>`)

	ordSeasonRe   = regexp.MustCompile(`\b(\d+)(?:st|nd|rd|th)\s*season\b`)
	seasonNumRe   = regexp.MustCompile(`\bseason\s*(\d+)\b`)
	partNumRe     = regexp.MustCompile(`\bpart\s*(\d+)\b`)
	word2ndRe     = regexp.MustCompile(`\b2nd\b`)
	word3rdRe     = regexp.MustCompile(`\b3rd\b`)
	ordWordRe     = regexp.MustCompile(`\b(\d+)(st|nd|rd|th)\b`)
	yearRe        = regexp.MustCompile(`\b(?:19|20)\d{2}\b`)
	spaceRunRe    = regexp.MustCompile(`\s+`)
	wsPipeRe      = regexp.MustCompile(`\s*\|\|\s*`)
	seasonWordRe  = regexp.MustCompile(`(?i)\bseason\s+(\d+)\b`)
	seasonFirstRe = regexp.MustCompile(`(?i)\b(\d+)(?:st|nd|rd|th)\s+season\b`)
	seasonTailRe  = regexp.MustCompile(`(?i)[\s:]*((?:season|part)\s*\d+|\d+(?:st|nd|rd|th)\s*season)\s*$`)
	numRe         = regexp.MustCompile(`\d+`)

	h1TitleRe           = regexp.MustCompile(`(?is)<h1[^>]*\bclass=["'][^"']*\btitle\b[^"']*["'][^>]*>(.*?)</h1>`)
	titleTagRe          = regexp.MustCompile(`(?is)<title>\s*Watch\s+(.*?)\s+Episode\s+\d+`)
	pageAnilistPatterns = []*regexp.Regexp{
		regexp.MustCompile(`"idAnilist"\s*:\s*(\d+)`),
		regexp.MustCompile(`\\"idAnilist\\"\s*:\s*(\d+)`),
		regexp.MustCompile(`/anime/banner/(\d+)-`),
		regexp.MustCompile(`/anime/cover/(?:small|medium|large)/b(?:x)?(\d+)-`),
	}

	pHeightRe    = regexp.MustCompile(`(?i)(\d{3,4})\s*p`)
	bareHeightRe = regexp.MustCompile(`\b(2160|1440|1080|720|480|360|240)\b`)
	resolutionRe = regexp.MustCompile(`(?i)RESOLUTION=\d+x(\d+)`)
	dimPRe       = regexp.MustCompile(`(?i)(\d{3,4})\s*p`)
	word4KRe     = regexp.MustCompile(`(?i)\b4\s*k\b`)
	gradeRe      = regexp.MustCompile(`(?i)\b(UHD|FHD|HD|SD|HQ)\b`)

	keyPatterns = []*regexp.Regexp{
		regexp.MustCompile(`TRUST_AES_KEY"\s*\]\s*,\s*"([^"]{16})"`),
		regexp.MustCompile(`TRUST_AES_KEY"\s*,\s*"([^"]{16})"`),
		regexp.MustCompile(`trustAesKey[^"]*"TRUST_AES_KEY"\s*\]\s*,\s*"([^"]{16})"`),
	}
	ivPatterns = []*regexp.Regexp{
		regexp.MustCompile(`TRUST_AES_IV"\s*\]\s*,\s*"([^"]{16})"`),
		regexp.MustCompile(`TRUST_AES_IV"\s*,\s*"([^"]{16})"`),
		regexp.MustCompile(`trustAesIv[^"]*"TRUST_AES_IV"\s*\]\s*,\s*"([^"]{16})"`),
	}
	stripPattern = regexp.MustCompile(`(?:stripBytes","STRIP_BYTES"\]?,(\d+)|STRIP_BYTES"\s*\]\s*,\s*(\d+))`)

	itemSplitRe    = regexp.MustCompile(`(?i)<div\s+class=["']item["']>`)
	latestHrefRe   = regexp.MustCompile(`(?i)href=["']https?://[^"']*/watch/([^"/]+)/ep-(\d+)["']`)
	latestTipRe    = regexp.MustCompile(`(?i)data-tip=["'](\d+)["']`)
	latestNameRe   = regexp.MustCompile(`(?is)<a\s+class=["'][^"']*\bname\b[^"']*["'][^>]*>(.*?)</a>`)
	latestPosterRe = regexp.MustCompile(`(?is)<img\b[^>]*\bsrc=["']([^"']+)["']`)
	latestSubRe    = regexp.MustCompile(`(?is)ep-status\s+sub["'][^>]*>\s*<span>\s*(\d+)\s*</span>`)
	latestDubRe    = regexp.MustCompile(`(?is)ep-status\s+dub["'][^>]*>\s*<span>\s*(\d+)\s*</span>`)
)
