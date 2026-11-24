// Package anizone resolves anizone.to episodes. Search is the site's anime
// search page; a card matches only when one of its titles is the same name.
// The episode page (/anime/{slug}/{n} and its index.php twin) states the HLS
// master on media-player plus labeled setVideo buttons, with <track> captions
// beside it. English audio is used only when the page gives that file.
package anizone

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
	ID = "anizone"
	// Name is the display name.
	Name = "AniZone"
	// Origin is the site the resolver scrapes.
	Origin = "https://anizone.to"
	// UserAgent is the browser the site expects.
	UserAgent = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/131.0.0.0 Safari/537.36"
)

// Provider implements resolve.Provider for anizone.to.
type Provider struct {
	client *httpx.Client
	origin string
}

// New builds the provider with default timeouts.
func New() *Provider {
	return newWithOrigin(httpx.NewClient(UserAgent, 22*time.Second), Origin)
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
	aliases []string
}

type offer struct {
	url     string
	english bool
	tracks  []protocol.SubtitleTrack
}

// Resolve turns an AniList id + episode into a playable master. The title
// hint is required: search matches a card only on an exact title.
func (p *Provider) Resolve(ctx context.Context, req resolve.Request) ([]protocol.Source, error) {
	if req.AnilistID <= 0 || req.Episode <= 0 {
		return nil, resolve.Missing(Name, "anilistId and episode are required")
	}
	dub := strings.EqualFold(req.Audio, "dub")
	found, err := p.findShow(ctx, req.TitleHint)
	if err != nil {
		return nil, err
	}
	page, err := p.openPage(ctx, episodeURLs(found.slug, req.Episode))
	if err != nil {
		return nil, err
	}
	picked := choose(offers(page.Body), dub)
	if picked == nil {
		if dub {
			return nil, resolve.Missing(Name, "no english audio file")
		}
		return nil, resolve.Missing(Name, "no japanese audio file")
	}
	playlist, err := p.getPlaylist(ctx, picked.url)
	if err != nil {
		return nil, err
	}
	if blocked(playlist.Code, playlist.Body) {
		return nil, resolve.Blocked(Name, "bot wall")
	}
	if playlist.Code < 200 || playlist.Code > 299 {
		return nil, resolve.Missing(Name, "playlist status "+strconv.Itoa(playlist.Code))
	}
	source := playable(picked.url, playlist.Body, dub, picked.tracks)
	if source == nil {
		return nil, resolve.Missing(Name, "no HLS playlist")
	}
	return []protocol.Source{*source}, nil
}

func (p *Provider) findShow(ctx context.Context, titleHint string) (*show, error) {
	title := cleanTitle(titleHint)
	if title == "" {
		return nil, resolve.Missing(Name, "needs a title to find this show")
	}
	for _, query := range queries(title) {
		page, err := p.openPage(ctx, searchURLs(query))
		if err != nil {
			return nil, err
		}
		if hit := pickShow(searchHits(page.Body), title); hit != nil {
			return hit, nil
		}
	}
	return nil, resolve.Missing(Name, "title not found")
}

// openPage tries each candidate URL until one answers a usable page.
func (p *Provider) openPage(ctx context.Context, urls []string) (httpx.Page, error) {
	blockedSeen := false
	for _, pageURL := range urls {
		page, err := p.client.GetWithHeaders(ctx, pageURL, map[string]string{
			"Accept":  "text/html,application/xhtml+xml",
			"Referer": p.origin + "/",
		})
		if err != nil {
			blockedSeen = true
			continue
		}
		if blocked(page.Code, page.Body) {
			blockedSeen = true
			continue
		}
		if page.Code == 404 {
			continue
		}
		if page.Code < 200 || page.Code > 299 {
			blockedSeen = true
			continue
		}
		if strings.TrimSpace(page.Body) == "" {
			continue
		}
		return page, nil
	}
	if blockedSeen {
		return httpx.Page{}, resolve.Blocked(Name, "site unreachable")
	}
	return httpx.Page{}, resolve.Missing(Name, "page not found")
}

func (p *Provider) getPlaylist(ctx context.Context, mediaURL string) (httpx.Page, error) {
	page, err := p.client.GetWithHeaders(ctx, mediaURL, map[string]string{
		"Accept":  "application/vnd.apple.mpegurl,application/x-mpegURL,*/*",
		"Referer": p.origin + "/",
		"Origin":  p.origin,
	})
	if err != nil {
		return httpx.Page{}, resolve.Blocked(Name, err.Error())
	}
	return page, nil
}

// slugOK reports whether a value is a plain lowercase site slug.
func slugOK(value string) bool { return slugRe.MatchString(value) }

// queries returns the search terms tried for a title: the full title, then
// its head before the first colon.
func queries(title string) []string {
	full := strings.TrimSpace(title)
	if full == "" {
		return nil
	}
	head := full
	if i := strings.Index(full, ":"); i >= 0 {
		head = full[:i]
	}
	head = strings.TrimSpace(head)
	out := []string{full}
	if head != "" && head != full {
		out = append(out, head)
	}
	return out
}

func searchURLs(query string) []string {
	q := encode(query)
	return []string{
		Origin + "/anime?search=" + q,
		Origin + "/index.php/anime?search=" + q,
	}
}

func episodeURLs(slug string, episode int) []string {
	return []string{
		Origin + "/anime/" + slug + "/" + strconv.Itoa(episode),
		Origin + "/index.php/anime/" + slug + "/" + strconv.Itoa(episode),
	}
}

// searchHits extracts every anime card on a search page. The current
// layout embeds the results as an Alpine items payload
// (JSON.parse('[{"slug":...,"main_title":...,"title_list":{...}}]')); the
// legacy layout renders /anime/{slug} links with getTitle(...) windows.
// Both paths feed the same show list, deduplicated by slug.
func searchHits(html string) []*show {
	page := unescape(html)
	var shows []*show
	seen := make(map[string]bool)
	add := func(s *show) {
		if s == nil || s.slug == "" || seen[s.slug] {
			return
		}
		seen[s.slug] = true
		shows = append(shows, s)
	}
	// Current layout: results embedded in Alpine state.
	for _, match := range jsonParseRe.FindAllStringSubmatch(page, -1) {
		for _, item := range parseItems(jsString(match[1])) {
			titles := itemTitles(item)
			if len(titles) == 0 {
				continue
			}
			add(&show{slug: item.Slug, title: titles[0], aliases: titles[1:]})
		}
	}
	// Legacy layout: cards as /anime/{slug} links with titles before them.
	for _, idx := range animeLinkRe.FindAllStringSubmatchIndex(page, -1) {
		slug := page[idx[2]:idx[3]]
		start := idx[0] - 2000
		if start < 0 {
			start = 0
		}
		window := page[start:idx[1]]
		// The link counts only when it ends at a boundary the site uses;
		// episode links like /anime/slug/4 must not count as cards.
		if idx[1] < len(page) {
			switch page[idx[1]] {
			case '"', '\'', '#', '?', ' ', '\t', '\n', '\r':
			default:
				continue
			}
		}
		titles := titlesInWindow(window)
		if len(titles) == 0 {
			continue
		}
		add(&show{slug: slug, title: titles[0], aliases: titles[1:]})
	}
	return shows
}

// rawItem is one embedded search result of the current site layout.
type rawItem struct {
	Slug      string            `json:"slug"`
	URL       string            `json:"url"`
	MainTitle string            `json:"main_title"`
	TitleList map[string]string `json:"title_list"`
}

// parseItems reads the embedded results array, keeping only rows whose slug
// looks like a site id.
func parseItems(raw string) []rawItem {
	var items []rawItem
	if err := json.Unmarshal([]byte(raw), &items); err != nil {
		return nil
	}
	out := make([]rawItem, 0, len(items))
	for _, item := range items {
		if slugOK(item.Slug) {
			out = append(out, item)
		}
	}
	return out
}

// itemTitles flattens one embedded result into a title list: the main title
// first, then the translated names in stable key order.
func itemTitles(item rawItem) []string {
	var titles []string
	seen := make(map[string]bool)
	add := func(title string) {
		title = strings.TrimSpace(title)
		if title == "" || seen[title] {
			return
		}
		seen[title] = true
		titles = append(titles, title)
	}
	add(item.MainTitle)
	keys := make([]string, 0, len(item.TitleList))
	for key := range item.TitleList {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		add(item.TitleList[key])
	}
	return titles
}

// titlesInWindow reads the card titles out of the markup before a link: the
// last getTitle(...) argument and the last JSON.parse(...) object.
func titlesInWindow(window string) []string {
	var titles []string
	seen := make(map[string]bool)
	add := func(title string) {
		if title == "" || seen[title] {
			return
		}
		seen[title] = true
		titles = append(titles, title)
	}
	if matches := getTitleRe.FindAllStringSubmatch(window, -1); len(matches) > 0 {
		add(strings.TrimSpace(jsString(matches[len(matches)-1][1])))
	}
	if matches := jsonParseRe.FindAllStringSubmatch(window, -1); len(matches) > 0 {
		raw := jsString(matches[len(matches)-1][1])
		// Walk the object tokens in document order so the first listed name
		// stays the card's title, as the site's JSON object does.
		dec := json.NewDecoder(strings.NewReader(raw))
		if _, err := dec.Token(); err == nil {
			for dec.More() {
				_, _ = dec.Token() // key
				var value any
				if err := dec.Decode(&value); err != nil {
					break
				}
				if s, ok := value.(string); ok {
					add(strings.TrimSpace(s))
				}
			}
		}
	}
	return titles
}

// pickShow matches the hint against the card titles exactly, preferring the
// card's own title over an alias.
func pickShow(hits []*show, titleHint string) *show {
	want := normalize(titleHint)
	if want == "" {
		return nil
	}
	var best *show
	bestScore := 2
	for _, hit := range hits {
		score := 2
		if normalize(hit.title) == want {
			score = 0
		} else {
			for _, alias := range hit.aliases {
				if normalize(alias) == want {
					score = 1
					break
				}
			}
		}
		if score < bestScore {
			bestScore = score
			best = hit
		}
	}
	return best
}

// offers collects every playable file the episode page names, in page
// order. The current layout embeds one vidstackPlayer JSON config holding
// the master and its captions; the legacy layout uses labeled setVideo
// buttons plus a media-player master.
func offers(html string) []offer {
	page := unescape(html)
	var out []offer
	dup := func(u string) bool {
		for _, o := range out {
			if o.url == u {
				return true
			}
		}
		return false
	}
	// Current layout: vidstackPlayer(JSON.parse('{"src":...,"subtitles":[...]}')).
	// The embedded master is the Japanese file (the dub list loads behind a
	// Livewire modal, so only sub resolves statically).
	for _, match := range vidstackRe.FindAllStringSubmatch(page, -1) {
		cfg, ok := parseVidstack(jsString(match[1]))
		if !ok || !mediaURL(cfg.Src) || dup(cfg.Src) {
			continue
		}
		out = append(out, offer{url: cfg.Src, english: false, tracks: cfg.tracks})
	}
	// Legacy layout: labeled buttons first, then the media-player master.
	tracks := subtitleTracks(page)
	labeled := labeledOffers(page, tracks)
	players := playerURLs(page)
	// A button that embeds one language must not hide the master already on
	// the page. That master is the Japanese file unless the page is English
	// only.
	masterIsEnglish := len(labeled) == 0 && pageIsEnglishOnly(page)
	for _, o := range labeled {
		if !dup(o.url) {
			out = append(out, o)
		}
	}
	for _, u := range players {
		if dup(u) {
			continue
		}
		out = append(out, offer{url: u, english: masterIsEnglish, tracks: tracks})
	}
	return out
}

// vidstackCfg is the player config the episode page embeds.
type vidstackCfg struct {
	Src    string `json:"src"`
	tracks []protocol.SubtitleTrack
}

// parseVidstack reads the embedded player config and converts its caption
// list to protocol tracks.
func parseVidstack(raw string) (*vidstackCfg, bool) {
	var payload struct {
		Src       string `json:"src"`
		Subtitles []struct {
			Title    string `json:"title"`
			Format   string `json:"format"`
			Language string `json:"language"`
			File     string `json:"file"`
		} `json:"subtitles"`
	}
	if err := json.Unmarshal([]byte(raw), &payload); err != nil {
		return nil, false
	}
	cfg := &vidstackCfg{Src: payload.Src}
	for _, sub := range payload.Subtitles {
		if !strings.HasPrefix(sub.File, "https://") {
			continue
		}
		label := strings.TrimSpace(sub.Title)
		if label == "" {
			label = strings.TrimSpace(sub.Language)
		}
		if label == "" {
			continue
		}
		mime := "text/vtt"
		switch strings.ToLower(sub.Format) {
		case "ass":
			mime = "text/x-ssa"
		case "srt":
			mime = "application/x-subrip"
		}
		cfg.tracks = append(cfg.tracks, protocol.SubtitleTrack{
			URL:      sub.File,
			Label:    label,
			Language: sub.Language,
			MimeType: mime,
		})
	}
	return cfg, true
}

func labeledOffers(page string, tracks []protocol.SubtitleTrack) []offer {
	var out []offer
	for _, match := range setVideoRe.FindAllStringSubmatch(page, -1) {
		u := httpsURLRe.FindString(jsString(match[1]))
		if u == "" || !mediaURL(u) {
			continue
		}
		label := match[2]
		if captionLabel(label) {
			continue
		}
		out = append(out, offer{url: u, english: englishLabel(label), tracks: tracks})
	}
	return out
}

func playerURLs(page string) []string {
	var out []string
	for _, match := range mediaPlayerRe.FindAllStringSubmatch(page, -1) {
		if mediaURL(match[1]) {
			out = append(out, match[1])
		}
	}
	if len(out) > 0 {
		return out
	}
	seen := make(map[string]bool)
	for _, u := range masterRe.FindAllString(page, -1) {
		if mediaURL(u) && !seen[u] {
			seen[u] = true
			out = append(out, u)
		}
	}
	return out
}

func subtitleTracks(page string) []protocol.SubtitleTrack {
	var out []protocol.SubtitleTrack
	for _, match := range trackRe.FindAllStringSubmatch(page, -1) {
		tag := match[1]
		src := attr(tag, "src")
		if !strings.HasPrefix(src, "https://") {
			continue
		}
		kind := strings.ToLower(attr(tag, "kind"))
		path := src
		if i := strings.Index(path, "?"); i >= 0 {
			path = path[:i]
		}
		caption := kind == "subtitles" || kind == "captions" ||
			strings.HasSuffix(strings.ToLower(path), ".vtt") ||
			strings.HasSuffix(strings.ToLower(path), ".srt") ||
			strings.HasSuffix(strings.ToLower(path), ".ass")
		if !caption {
			continue
		}
		label := attr(tag, "label")
		if label == "" {
			label = attr(tag, "srclang")
		}
		if label == "" {
			continue
		}
		mime := "text/vtt"
		switch {
		case strings.HasSuffix(strings.ToLower(path), ".srt"):
			mime = "application/x-subrip"
		case strings.HasSuffix(strings.ToLower(path), ".ass"):
			mime = "text/x-ssa"
		}
		out = append(out, protocol.SubtitleTrack{
			URL:      src,
			Label:    label,
			Language: attr(tag, "srclang"),
			MimeType: mime,
		})
	}
	return out
}

func pageIsEnglishOnly(page string) bool {
	var labels []string
	for _, match := range setVideoRe.FindAllStringSubmatch(page, -1) {
		if !captionLabel(match[2]) {
			labels = append(labels, match[2])
		}
	}
	if len(labels) == 0 {
		return false
	}
	anyEnglish, anyJapanese := false, false
	for _, label := range labels {
		anyEnglish = anyEnglish || englishLabel(label)
		anyJapanese = anyJapanese || japaneseLabel(label)
	}
	return anyEnglish && !anyJapanese
}

// choose picks the offer for the requested audio: the first English file
// for dub, the first non-English file for sub.
func choose(list []offer, dub bool) *offer {
	for i := range list {
		if dub == list[i].english {
			return &list[i]
		}
	}
	return nil
}

func englishLabel(text string) bool {
	if captionLabel(text) || japaneseLabel(text) {
		return false
	}
	lower := strings.ToLower(text)
	return strings.Contains(lower, "english") || wordDubRe.MatchString(lower)
}

func japaneseLabel(text string) bool {
	lower := strings.ToLower(text)
	return strings.Contains(lower, "japan") || wordSubRe.MatchString(lower)
}

func captionLabel(text string) bool {
	lower := strings.ToLower(text)
	return strings.Contains(lower, "subtitle") || strings.Contains(lower, "caption")
}

func mediaURL(raw string) bool {
	return strings.HasPrefix(raw, "https://") && !strings.Contains(raw, " ") && len(raw) < 2000
}

// blocked reports whether a page or status looks like a bot wall.
func blocked(code int, body string) bool {
	if code == 403 || code == 429 || code == 503 {
		return true
	}
	head := strings.ToLower(body)
	if len(head) > 5000 {
		head = head[:5000]
	}
	return strings.Contains(head, "just a moment") ||
		strings.Contains(head, "cf-browser-verification") ||
		strings.Contains(head, "challenge-platform") ||
		strings.Contains(head, "attention required") ||
		strings.Contains(head, "you have been blocked")
}

// playable validates the master and derives its metadata from the tallest
// variant the playlist states.
func playable(mediaURL, playlist string, dub bool, tracks []protocol.SubtitleTrack) *protocol.Source {
	if !strings.HasPrefix(mediaURL, "https://") {
		return nil
	}
	if !strings.HasPrefix(strings.TrimSpace(playlist), "#EXTM3U") {
		return nil
	}
	mode := "SUB"
	if dub {
		mode = "DUB"
	}
	best := bestVariant(playlist)
	quality := heightLabel(resolutionOf(best))
	parts := []string{}
	if codec := audioCodec(attr(best, "CODECS")); codec != "" {
		parts = append(parts, codec)
	}
	if channels := channelLabel(attr(best, "CHANNELS")); channels != "" {
		parts = append(parts, channels)
	}
	parts = append(parts, mode)
	audio := strings.Join(parts, " · ")
	label := quality
	if label == "" {
		label = mode
	}
	return &protocol.Source{
		URL: mediaURL,
		Headers: map[string]string{
			"Referer":    Origin + "/",
			"Origin":     Origin,
			"User-Agent": UserAgent,
		},
		Label:     label,
		Quality:   quality,
		Audio:     audio,
		Subtitles: tracks,
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

func resolutionOf(line string) int {
	match := heightRe.FindStringSubmatch(line)
	if match == nil {
		return 0
	}
	h, _ := strconv.Atoi(match[1])
	return h
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

// audioCodec maps a CODECS list to the word the player shows.
func audioCodec(codecs string) string {
	parts := strings.Split(codecs, ",")
	for _, part := range parts {
		switch {
		case strings.HasPrefix(part, "mp4a") || strings.HasPrefix(part, "aac"):
			return "AAC"
		case strings.HasPrefix(part, "ac-3") || part == "ac3":
			return "AC3"
		case strings.HasPrefix(part, "ec-3") || part == "eac3":
			return "EAC3"
		case strings.HasPrefix(part, "opus"):
			return "Opus"
		case strings.HasPrefix(part, "flac"):
			return "FLAC"
		case strings.HasPrefix(part, "dts"):
			return "DTS"
		}
	}
	return ""
}

func channelLabel(raw string) string {
	switch strings.TrimSpace(raw) {
	case "1":
		return "1.0"
	case "2":
		return "2.0"
	case "6":
		return "5.1"
	case "8":
		return "7.1"
	default:
		return ""
	}
}

// attr reads one attribute out of a tag fragment, quoted or bare.
func attr(tag, name string) string {
	if m := attrQuotedRes[name].FindStringSubmatch(tag); m != nil {
		return strings.TrimSpace(m[1])
	}
	if m := attrBareRes[name].FindStringSubmatch(tag); m != nil {
		return strings.TrimSpace(m[1])
	}
	return ""
}

var attrQuotedRes = map[string]*regexp.Regexp{
	"src":      quotedAttrRe("src"),
	"kind":     quotedAttrRe("kind"),
	"label":    quotedAttrRe("label"),
	"srclang":  quotedAttrRe("srclang"),
	"CODECS":   quotedAttrRe("CODECS"),
	"CHANNELS": quotedAttrRe("CHANNELS"),
}

var attrBareRes = map[string]*regexp.Regexp{
	"src":      bareAttrRe("src"),
	"kind":     bareAttrRe("kind"),
	"label":    bareAttrRe("label"),
	"srclang":  bareAttrRe("srclang"),
	"CODECS":   bareAttrRe("CODECS"),
	"CHANNELS": bareAttrRe("CHANNELS"),
}

func quotedAttrRe(name string) *regexp.Regexp {
	return regexp.MustCompile(`(?i)\b` + regexp.QuoteMeta(name) + `\s*=\s*["']([^"']*)["']`)
}

func bareAttrRe(name string) *regexp.Regexp {
	return regexp.MustCompile(`(?i)\b` + regexp.QuoteMeta(name) + `\s*=\s*([^,\s>]+)`)
}

// unescape decodes the entities the site's inline scripts use.
func unescape(raw string) string {
	replacer := strings.NewReplacer(
		"&amp;", "&",
		"&quot;", "\"",
		"&#34;", "\"",
		"&#39;", "'",
		"&apos;", "'",
	)
	return replacer.Replace(raw)
}

// jsString decodes a single-quoted JavaScript string literal's escapes.
func jsString(raw string) string {
	var out strings.Builder
	out.Grow(len(raw))
	for i := 0; i < len(raw); {
		if raw[i] != '\\' || i+1 >= len(raw) {
			out.WriteByte(raw[i])
			i++
			continue
		}
		next := raw[i+1]
		switch {
		case next == 'u' && i+5 < len(raw):
			if code, err := strconv.ParseUint(raw[i+2:i+6], 16, 16); err == nil {
				out.WriteRune(rune(code))
				i += 6
			} else {
				out.WriteByte(raw[i])
				i++
			}
		case next == '\\' || next == '\'' || next == '"' || next == '/':
			out.WriteByte(next)
			i += 2
		case next == 'n':
			out.WriteByte('\n')
			i += 2
		default:
			out.WriteByte(next)
			i += 2
		}
	}
	return out.String()
}

func cleanTitle(raw string) string {
	cleaned := raw
	if i := strings.Index(cleaned, "||"); i >= 0 {
		cleaned = cleaned[:i]
	}
	return strings.TrimSpace(malRe.ReplaceAllString(cleaned, ""))
}

func normalize(raw string) string {
	cleaned := strings.ToLower(raw)
	cleaned = strings.ReplaceAll(cleaned, "’", "'")
	cleaned = strings.ReplaceAll(cleaned, "'", "")
	return strings.TrimSpace(nonAlnumRe.ReplaceAllString(cleaned, " "))
}

func encode(value string) string {
	return strings.ReplaceAll(url.QueryEscape(value), "+", "%20")
}

var (
	slugRe        = regexp.MustCompile(`^[a-z0-9]+$`)
	animeLinkRe   = regexp.MustCompile(`(?:https://anizone\.to)?(?:/index\.php)?/anime/([a-z0-9]+)/?`)
	getTitleRe    = regexp.MustCompile(`getTitle\(\s*this\.anmTitles\s*,\s*'((?:\\.|[^'\\])*)'\)`)
	jsonParseRe   = regexp.MustCompile(`JSON\.parse\(\s*'((?:\\.|[^'\\])*)'\s*\)`)
	vidstackRe    = regexp.MustCompile(`vidstackPlayer\(\s*JSON\.parse\(\s*'((?:\\.|[^'\\])*)'\s*\)\s*\)`)
	mediaPlayerRe = regexp.MustCompile(`(?i)<media-player\b[^>]*\bsrc\s*=\s*["'](https://[^"']+)["']`)
	masterRe      = regexp.MustCompile(`https://[^"'\\\s<>]+master\.m3u8`)
	trackRe       = regexp.MustCompile(`(?i)<track\b([^>]*)/?>`)
	setVideoRe    = regexp.MustCompile(`(?i)wire:click\s*=\s*["']setVideo\(([^)]*)\)["'][^>]*>([^<]*)`)
	httpsURLRe    = regexp.MustCompile(`https://[^"'\\\s<>]+`)
	heightRe      = regexp.MustCompile(`(?i)RESOLUTION=\d+x(\d+)`)
	malRe         = regexp.MustCompile(`(?i)\bmal:\d+\b`)
	wordDubRe     = regexp.MustCompile(`\bdub\b`)
	wordSubRe     = regexp.MustCompile(`\bsub\b`)
	nonAlnumRe    = regexp.MustCompile(`[^\p{L}\p{N}]+`)
)
