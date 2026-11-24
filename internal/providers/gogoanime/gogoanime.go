// Package gogoanime resolves gogoanime.by episodes. Search is the site's
// GET /?s= page. Sub episodes end in english-subbed and dub episodes end
// in english-dubbed. The player page embeds MegaPlay or MegaVid, and the
// playable file is the HLS playlist that page states.
package gogoanime

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
	ID = "gogoanime"
	// Name is the display name.
	Name = "gogoanime"
	// Origin is the site the resolver scrapes.
	Origin = "https://gogoanime.by"
	// MegaPlayOrigin is the MegaPlay player host.
	MegaPlayOrigin = "https://megaplay.su"
	// MegaVidOrigin is the MegaVid player host.
	MegaVidOrigin = "https://megavid.buzz"
	// UserAgent is the browser the site expects.
	UserAgent = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/131.0.0.0 Safari/537.36"
)

const (
	megaPlayHost = "megaplay.su"
	megaVidHost  = "megavid.buzz"
)

// Provider implements providers.Provider for gogoanime.by.
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

type gogoShow struct {
	title string
	url   string
}

type gogoPlayer struct {
	url   string
	label string
}

type gogoPending struct {
	url   string
	tracks []protocol.SubtitleTrack
}

// Resolve turns an AniList id + episode into a playable HLS master.
func (p *Provider) Resolve(ctx context.Context, req resolve.Request) ([]protocol.Source, error) {
	if req.AnilistID <= 0 || req.Episode <= 0 {
		return nil, resolve.Missing(Name, "anilistId and episode are required")
	}
	dub := strings.EqualFold(req.Audio, "dub")
	found, err := p.findShow(ctx, req.TitleHint, dub)
	if err != nil {
		return nil, err
	}
	episodeURL, err := p.findEpisode(ctx, found.url, req.Episode, dub)
	if err != nil {
		return nil, err
	}
	players, err := p.openPlayers(ctx, episodeURL, dub)
	if err != nil {
		return nil, err
	}
	for _, player := range players {
		source, openErr := p.openPlayer(ctx, player, dub)
		if openErr != nil {
			return nil, openErr
		}
		if source != nil {
			return []protocol.Source{*source}, nil
		}
	}
	return nil, resolve.Missing(Name, "no playable player")
}

func (p *Provider) findShow(ctx context.Context, titleHint string, dub bool) (*gogoShow, error) {
	title := cleanTitle(titleHint)
	if title == "" {
		return nil, resolve.Missing(Name, "needs a title to find this show")
	}
	for _, query := range queries(title) {
		page, err := p.client.GetText(ctx, p.origin+"/?s="+encode(query), p.origin+"/")
		if err != nil {
			continue
		}
		if blocked(page.Code, page.Body) || page.Code < 200 || page.Code > 299 {
			continue
		}
		if found := pickShow(searchHits(page.Body), title, dub); found != nil {
			return found, nil
		}
	}
	return nil, resolve.Missing(Name, "title not found")
}

func (p *Provider) findEpisode(ctx context.Context, seriesURL string, episode int, dub bool) (string, error) {
	page, err := p.client.GetText(ctx, seriesURL, p.origin+"/")
	if err != nil {
		return "", resolve.Blocked(Name, err.Error())
	}
	if blocked(page.Code, page.Body) {
		return "", resolve.Blocked(Name, "bot wall")
	}
	if page.Code < 200 || page.Code > 299 {
		return "", resolve.Blocked(Name, "status "+strconv.Itoa(page.Code))
	}
	for _, row := range episodes(page.Body) {
		if row.number == episode && row.dub == dub {
			return row.url, nil
		}
	}
	return "", resolve.Missing(Name, "episode not listed")
}

func (p *Provider) openPlayers(ctx context.Context, episodeURL string, dub bool) ([]gogoPlayer, error) {
	page, err := p.client.GetText(ctx, episodeURL, p.origin+"/")
	if err != nil {
		return nil, resolve.Blocked(Name, err.Error())
	}
	if blocked(page.Code, page.Body) {
		return nil, resolve.Blocked(Name, "bot wall")
	}
	if page.Code < 200 || page.Code > 299 {
		return nil, resolve.Blocked(Name, "status "+strconv.Itoa(page.Code))
	}
	players := players(page.Body, dub)
	if len(players) == 0 {
		return nil, resolve.Missing(Name, "no players")
	}
	return players, nil
}

func (p *Provider) openPlayer(ctx context.Context, player gogoPlayer, dub bool) (*protocol.Source, error) {
	page, err := p.client.GetText(ctx, player.url, p.origin+"/")
	if err != nil {
		return nil, nil
	}
	if blocked(page.Code, page.Body) {
		return nil, resolve.Blocked(Name, "player challenged")
	}
	if page.Code < 200 || page.Code > 299 {
		return nil, nil
	}
	frame := frameURL(page.Body)
	if frame == "" {
		return nil, nil
	}
	switch host(frame) {
	case megaPlayHost:
		return p.openMegaPlay(ctx, frame, player.label, dub)
	case megaVidHost:
		return p.openMegaVid(ctx, frame, player.label, dub)
	default:
		return nil, nil
	}
}

func (p *Provider) openMegaPlay(ctx context.Context, frame, label string, dub bool) (*protocol.Source, error) {
	embed, err := p.client.GetText(ctx, frame, p.origin+"/")
	if err != nil || embed.Code < 200 || embed.Code > 299 || blocked(embed.Code, embed.Body) {
		return nil, nil
	}
	file := megaPlayFile(embed.Body)
	if file == "" {
		return nil, nil
	}
	playlist, err := p.client.GetText(ctx, file, MegaPlayOrigin+"/")
	if err != nil || playlist.Code < 200 || playlist.Code > 299 {
		return nil, nil
	}
	return playable(file, playlist.Body, label, dub, MegaPlayOrigin, nil), nil
}

func (p *Provider) openMegaVid(ctx context.Context, frame, label string, dub bool) (*protocol.Source, error) {
	page, err := p.client.GetText(ctx, frame, p.origin+"/")
	if err != nil || page.Code < 200 || page.Code > 299 || blocked(page.Code, page.Body) {
		return nil, nil
	}
	path := megaVidPath(page.Body)
	if path == "" {
		return nil, nil
	}
	primary := MegaVidOrigin + path
	fallback := primary
	if strings.Contains(primary, "?") {
		fallback = primary + "&provider=1"
	} else {
		fallback = primary + "?provider=1"
	}
	for _, sourceURL := range []string{primary, fallback} {
		body, err := p.client.GetText(ctx, sourceURL, frame)
		if err != nil || body.Code < 200 || body.Code > 299 || blocked(body.Code, body.Body) {
			continue
		}
		pending := megaVid(body.Body)
		if pending == nil {
			continue
		}
		playlist, err := p.client.GetText(ctx, pending.url, MegaVidOrigin+"/")
		if err != nil || playlist.Code < 200 || playlist.Code > 299 {
			continue
		}
		if source := playable(pending.url, playlist.Body, label, dub, MegaVidOrigin, pending.tracks); source != nil {
			return source, nil
		}
	}
	return nil, nil
}

// ---- parsers ----

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

func queries(title string) []string {
	full := strings.TrimSpace(title)
	if full == "" {
		return nil
	}
	skip := map[string]bool{
		"season": true, "second": true, "third": true, "first": true, "part": true,
		"movie": true, "special": true, "english": true, "dubbed": true, "subbed": true,
		"the": true, "and": true, "cour": true,
	}
	var tokens []string
	for _, token := range strings.Fields(full) {
		token = strings.TrimSpace(token)
		if len(token) >= 5 && !skip[strings.ToLower(token)] {
			tokens = append(tokens, token)
		}
	}
	sortByLengthDesc(tokens)
	out := []string{full}
	seen := map[string]bool{full: true}
	for _, token := range tokens {
		if !seen[token] {
			seen[token] = true
			out = append(out, token)
		}
	}
	return out
}

func sortByLengthDesc(list []string) {
	for i := 0; i < len(list); i++ {
		for j := i + 1; j < len(list); j++ {
			if len(list[j]) > len(list[i]) {
				list[i], list[j] = list[j], list[i]
			}
		}
	}
}

func searchHits(html string) []gogoShow {
	from := 0
	for from < len(html) {
		at := strings.Index(html[from:], `class="listupd"`)
		if at < 0 {
			return nil
		}
		at += from
		tagStart := strings.LastIndex(html[:at], "<")
		if tagStart < 0 {
			tagStart = 0
		}
		tagEnd := strings.Index(html[at:], ">")
		if tagEnd < 0 {
			return nil
		}
		tagEnd += at
		tag := html[tagStart : tagEnd+1]
		if !strings.Contains(tag, "external-results") {
			next := strings.Index(html[tagEnd+1:], `class="listupd"`)
			end := len(html)
			if next >= 0 {
				end = tagEnd + 1 + next
			}
			return articles(html[tagEnd+1 : end])
		}
		from = tagEnd + 1
	}
	return nil
}

func articles(block string) []gogoShow {
	var hits []gogoShow
	seen := map[string]bool{}
	for _, match := range articleRe.FindAllStringSubmatch(block, -1) {
		raw := unescape(match[1])
		if !strings.HasSuffix(raw, "/") {
			raw += "/"
		}
		if !sameSite(raw) || seen[raw] {
			continue
		}
		seen[raw] = true
		hits = append(hits, gogoShow{title: unescape(match[2]), url: raw})
	}
	return hits
}

func pickShow(hits []gogoShow, titleHint string, dub bool) *gogoShow {
	want := normalize(titleHint)
	if want == "" {
		return nil
	}
	bestScore := 49
	var best *gogoShow
	bestLength := 0
	for i := range hits {
		score := score(hits[i].title, want, dub)
		length := len(normalize(hits[i].title))
		if score > bestScore || (score == bestScore && best != nil && length < bestLength) || (score >= 50 && best == nil) {
			if score > bestScore || best == nil {
				bestScore = score
				best = &hits[i]
				bestLength = length
			} else if length < bestLength {
				best = &hits[i]
				bestLength = length
			}
		}
	}
	return best
}

func score(title, want string, dub bool) int {
	name := normalize(title)
	if name == "" {
		return 0
	}
	dubbed := dubWordRe.MatchString(name)
	if dub && !dubbed {
		return 0
	}
	if !dub && dubbed {
		return 0
	}
	if name == want {
		return 10000
	}
	wantWords := words(want)
	nameWords := strings.Fields(name)
	if len(wantWords) == 0 {
		return 0
	}
	if allIn(wantWords, nameWords) {
		extras := 0
		for _, word := range nameWords {
			if !containsWord(wantWords, word) && !languageWords[word] {
				extras++
			}
		}
		return 5000 - extras*10
	}
	shared := 0
	for _, word := range wantWords {
		if containsWord(nameWords, word) {
			shared++
		}
	}
	if shared == 0 {
		return 0
	}
	return shared*15 + 5
}

func words(normalized string) []string {
	var out []string
	for _, word := range strings.Fields(normalized) {
		if len(word) >= 3 || strings.IndexFunc(word, func(r rune) bool { return r >= '0' && r <= '9' }) >= 0 {
			out = append(out, word)
		}
	}
	return out
}

func allIn(want, have []string) bool {
	for _, word := range want {
		if !containsWord(have, word) {
			return false
		}
	}
	return true
}

func containsWord(list []string, want string) bool {
	for _, word := range list {
		if word == want {
			return true
		}
	}
	return false
}

func episodes(html string) []episodeRow {
	var rows []episodeRow
	for _, match := range episodeRe.FindAllStringSubmatch(html, -1) {
		number, _ := strconv.Atoi(match[1])
		raw := unescape(match[2])
		if !sameSite(raw) {
			continue
		}
		dubbed := strings.Contains(raw, "english-dubbed")
		subbed := strings.Contains(raw, "english-subbed")
		if !dubbed && !subbed {
			continue
		}
		rows = append(rows, episodeRow{number: number, url: raw, dub: dubbed})
	}
	return rows
}

type episodeRow struct {
	number int
	url    string
	dub    bool
}

func players(html string, dub bool) []gogoPlayer {
	kind := "sub"
	if dub {
		kind = "dub"
	}
	sectionRe := regexp.MustCompile(`(?s)<div class="type" data-type="` + kind + `">([\s\S]*?)</div>`)
	section := sectionRe.FindStringSubmatch(html)
	if section == nil {
		return nil
	}
	var out []gogoPlayer
	for _, match := range playerItemRe.FindAllStringSubmatch(section[1], -1) {
		raw := strings.TrimSpace(unescape(match[1]))
		if !strings.HasPrefix(raw, Origin+"/player/") {
			continue
		}
		out = append(out, gogoPlayer{url: raw, label: strings.TrimSpace(unescape(match[2]))})
	}
	sort.SliceStable(out, func(i, j int) bool {
		iEmbed := strings.Contains(out[i].url, "source=embed")
		jEmbed := strings.Contains(out[j].url, "source=embed")
		if iEmbed != jEmbed {
			return iEmbed
		}
		return false
	})
	return out
}

func frameURL(html string) string {
	for _, match := range frameRe.FindAllStringSubmatch(html, -1) {
		raw := unescape(match[1])
		switch host(raw) {
		case megaPlayHost, megaVidHost:
			return raw
		}
	}
	return ""
}

func megaPlayFile(html string) string {
	for _, match := range fileRe.FindAllStringSubmatch(html, -1) {
		raw := match[1]
		if host(raw) == megaPlayHost && strings.Contains(raw, ".m3u8") {
			return raw
		}
	}
	return ""
}

func megaVidPath(html string) string {
	marker := `id="player-payload">`
	at := strings.Index(html, marker)
	if at < 0 {
		return ""
	}
	start := strings.Index(html[at:], "{")
	if start < 0 {
		return ""
	}
	start += at
	end := strings.Index(html[start:], "</script>")
	if end < 0 {
		return ""
	}
	var root map[string]any
	if err := json.Unmarshal([]byte(html[start:start+end]), &root); err != nil {
		return ""
	}
	path := strings.TrimSpace(str(root["sourceUrl"]))
	if !strings.HasPrefix(path, "/") || strings.HasPrefix(path, "//") || strings.Contains(path, "..") {
		return ""
	}
	return path
}

func megaVid(payload string) *gogoPending {
	var root map[string]any
	if err := json.Unmarshal([]byte(payload), &root); err != nil {
		return nil
	}
	if str(root["status"]) != "ok" {
		return nil
	}
	raw := strings.TrimSpace(str(root["source"]))
	if !allowedMedia(raw) {
		return nil
	}
	return &gogoPending{url: raw, tracks: tracks(root)}
}

func tracks(root map[string]any) []protocol.SubtitleTrack {
	rows, _ := root["tracks"].([]any)
	var out []protocol.SubtitleTrack
	for _, row := range rows {
		obj, ok := row.(map[string]any)
		if !ok {
			continue
		}
		raw := strings.TrimSpace(str(obj["file"]))
		if !allowedMedia(raw) {
			continue
		}
		label := strings.TrimSpace(str(obj["label"]))
		if label == "" {
			continue
		}
		mime := ""
		if strings.HasSuffix(strings.ToLower(strings.SplitN(raw, "?", 2)[0]), ".vtt") {
			mime = "text/vtt"
		}
		out = append(out, protocol.SubtitleTrack{URL: raw, Label: label, MimeType: mime})
	}
	return out
}

func playable(file, playlist, siteLabel string, dub bool, origin string, subtitles []protocol.SubtitleTrack) *protocol.Source {
	if !strings.HasPrefix(strings.TrimSpace(playlist), "#EXTM3U") {
		return nil
	}
	if !allowedMedia(file) {
		return nil
	}
	quality := ""
	rating := ""
	if match := heightRe.FindStringSubmatch(bestVariant(playlist)); match != nil {
		height, _ := strconv.Atoi(match[1])
		quality = heightLabel(height)
	}
	if grade := gradeRe.FindString(siteLabel); grade != "" {
		rating = strings.ToUpper(grade)
	}
	label := quality
	if label == "" {
		label = rating
	}
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
		URL: file,
		Headers: map[string]string{
			"Referer":    origin + "/",
			"Origin":     origin,
			"User-Agent": UserAgent,
		},
		Label:         label,
		Quality:       quality,
		QualityRating: rating,
		Audio:         mode,
		Subtitles:     subtitles,
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

func allowedMedia(raw string) bool {
	if !strings.HasPrefix(raw, "https://") {
		return false
	}
	h := host(raw)
	if h == "" {
		return false
	}
	return h == megaPlayHost || h == megaVidHost || h == "megavid.se" || strings.HasSuffix(h, ".megavid.se")
}

func sameSite(raw string) bool {
	return strings.HasPrefix(raw, "https://") && host(raw) == "gogoanime.by"
}

func host(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	return strings.ToLower(u.Hostname())
}

func unescape(raw string) string {
	named := strings.NewReplacer(
		"&amp;", "&",
		"&#038;", "&",
		"&quot;", `"`,
		"&#039;", "'",
		"&apos;", "'",
	).Replace(raw)
	return numericEntityRe.ReplaceAllStringFunc(named, func(ent string) string {
		body := strings.TrimSuffix(strings.TrimPrefix(ent, "&#"), ";")
		base := 10
		if strings.HasPrefix(strings.ToLower(body), "x") {
			base = 16
			body = body[1:]
		}
		code, err := strconv.ParseInt(body, base, 32)
		if err != nil || code < 1 || code > 0xFFFF {
			return ent
		}
		return string(rune(code))
	})
}

func normalize(raw string) string {
	cleaned := cleanTitle(raw)
	if cleaned == "" {
		return ""
	}
	var folded strings.Builder
	for _, r := range cleaned {
		if replacement, ok := homoglyphs[r]; ok {
			folded.WriteRune(replacement)
		} else {
			folded.WriteRune(r)
		}
	}
	lower := strings.ToLower(folded.String())
	lower = nonAlnumRe.ReplaceAllString(lower, " ")
	lower = strings.TrimSpace(lower)
	parts := strings.Fields(lower)
	for i, token := range parts {
		if match := ordinalRe.FindStringSubmatch(token); match != nil {
			parts[i] = match[1]
		}
	}
	return strings.Join(parts, " ")
}

func cleanTitle(raw string) string {
	return strings.TrimSpace(malRe.ReplaceAllString(strings.TrimSpace(raw), ""))
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

var languageWords = map[string]bool{
	"english": true, "dub": true, "dubbed": true, "sub": true, "subbed": true,
}

var homoglyphs = map[rune]rune{
	'а': 'a', 'А': 'a', 'е': 'e', 'Е': 'e', 'о': 'o', 'О': 'o',
	'р': 'p', 'Р': 'p', 'с': 'c', 'С': 'c', 'у': 'y', 'У': 'y',
	'х': 'x', 'Х': 'x', 'і': 'i', 'І': 'i',
}

var (
	malRe           = regexp.MustCompile(`(?i)\bmal:\d+\b`)
	nonAlnumRe      = regexp.MustCompile(`[^a-z0-9]+`)
	ordinalRe       = regexp.MustCompile(`^(\d+)(?:st|nd|rd|th)$`)
	dubWordRe       = regexp.MustCompile(`\bdub\b|\bdubbed\b`)
	heightRe        = regexp.MustCompile(`(?i)RESOLUTION=\d+x(\d+)`)
	gradeRe         = regexp.MustCompile(`(?i)\b(UHD|FHD|HD|SD|HQ)\b`)
	numericEntityRe = regexp.MustCompile(`&#(?:x?[0-9a-fA-F]+);`)
	articleRe       = regexp.MustCompile(`(?is)<a[^>]+href="(https://gogoanime\.by/series/[^"#?]+)"[^>]*title="([^"]+)`)
	episodeRe       = regexp.MustCompile(`data-episode-number="(\d+)"[\s\S]{0,500}?href="(https://gogoanime\.by/[^"]+)`)
	playerItemRe    = regexp.MustCompile(`<li class="player-type-link" data-src="([^"]*)">([^<]*)</li>`)
	frameRe         = regexp.MustCompile(`(?i)<iframe[^>]+src="(https://[^"]+)`)
	fileRe          = regexp.MustCompile(`file:\s*"(https://[^"]+)`)
)
