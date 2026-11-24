// Package animegg resolves animegg.org episodes: search, series, episode,
// embed, then the /play/ file the player names.
package animegg

import (
	"context"
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
	ID = "animegg"
	// Name is the display name.
	Name = "AnimeGG"
	// Origin is the site the resolver scrapes.
	Origin = "https://www.animegg.org"
	// UserAgent is the browser the site expects.
	UserAgent = "Mozilla/5.0 (Linux; Android 14) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/122.0.0.0 Mobile Safari/537.36"
)

// Provider implements providers.Provider for animegg.org.
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
	aliases []string
}

type episodeRow struct {
	number int
	url    string
	sub    bool
	dub    bool
	raw    bool
}

type mirror struct {
	version   string
	embedPath string
}

type mediaFile struct {
	url   string
	label string
}

// Resolve turns an AniList id + episode into playable files. The AniList
// catalog is not searched; the title hint maps to the site's slug.
func (p *Provider) Resolve(ctx context.Context, req resolve.Request) ([]protocol.Source, error) {
	if req.AnilistID <= 0 || req.Episode <= 0 {
		return nil, resolve.Missing(Name, "anilistId and episode are required")
	}
	dub := strings.EqualFold(req.Audio, "dub")
	queries := queries(req.TitleHint)
	if len(queries) == 0 {
		return nil, resolve.Missing(Name, "needs a title to find this show")
	}
	var found *show
	for _, query := range queries {
		page, err := p.open(ctx, searchURL(p.origin, query), p.origin+"/")
		if err != nil {
			return nil, err
		}
		if hit := pickShow(searchHits(page), req.TitleHint); hit != nil {
			found = hit
			break
		}
	}
	if found == nil {
		return nil, resolve.Missing(Name, "title not found")
	}

	seriesURL := p.origin + "/series/" + found.slug
	seriesPage, err := p.open(ctx, seriesURL, p.origin+"/")
	if err != nil {
		return nil, err
	}
	rows := episodes(seriesPage)
	var row *episodeRow
	for i := range rows {
		if rows[i].number == req.Episode {
			row = &rows[i]
			break
		}
	}
	if row == nil {
		return nil, resolve.Missing(Name, "episode not listed")
	}
	if dub && !row.dub {
		return nil, resolve.Missing(Name, "dub not available")
	}
	if !dub && !row.sub && !row.raw {
		return nil, resolve.Missing(Name, "sub not available")
	}

	episodePage, err := p.open(ctx, row.url, seriesURL)
	if err != nil {
		return nil, err
	}
	chosen := chooseMirror(mirrors(episodePage), dub)
	if chosen == nil {
		return nil, resolve.Unplayable(Name, "no embed mirror")
	}
	embedURL := p.origin + chosen.embedPath
	embedPage, err := p.open(ctx, embedURL, row.url)
	if err != nil {
		return nil, err
	}
	files := mediaFiles(embedPage)
	if len(files) == 0 {
		return nil, resolve.Unplayable(Name, "no files on the embed")
	}
	sort.SliceStable(files, func(i, j int) bool { return height(files[i].label) > height(files[j].label) })
	sources := make([]protocol.Source, 0, len(files))
	for _, file := range files {
		fileURL, ok := fileURL(file.url)
		if !ok {
			continue
		}
		quality := heightLabel(file.label)
		label := quality
		if label == "" {
			label = strings.ToUpper(audioWord(dub))
		}
		sources = append(sources, protocol.Source{
			URL: fileURL,
			Headers: map[string]string{
				"Referer":    embedURL,
				"Origin":     p.origin,
				"User-Agent": UserAgent,
			},
			Label:         label,
			Quality:       quality,
			Audio:         strings.ToUpper(audioWord(dub)),
			QualityRating: gradeToken(file.label),
		})
	}
	if len(sources) == 0 {
		return nil, resolve.Unplayable(Name, "no playable file")
	}
	return sources, nil
}

func (p *Provider) open(ctx context.Context, pageURL, referer string) (string, error) {
	page, err := p.client.GetText(ctx, pageURL, referer)
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
	if strings.TrimSpace(page.Body) == "" {
		return "", resolve.Missing(Name, "empty answer")
	}
	return page.Body, nil
}

func audioWord(dub bool) string {
	if dub {
		return "dub"
	}
	return "sub"
}

// queries turns the AniKitty title hint into search terms.
func queries(titleHint string) []string {
	rawParts := strings.Split(titleHint, "||")
	parts := make([]string, 0, len(rawParts))
	for _, part := range rawParts {
		part = strings.TrimSpace(part)
		if part != "" {
			parts = append(parts, part)
		}
	}
	if len(parts) == 0 {
		parts = []string{cleanTitle(titleHint)}
	} else {
		for i, part := range parts {
			parts[i] = strings.TrimSpace(malRe.ReplaceAllString(part, ""))
		}
	}
	seen := map[string]bool{}
	var out []string
	for _, part := range parts {
		if len(part) < 2 {
			continue
		}
		if !seen[part] {
			seen[part] = true
			out = append(out, part)
		}
		head := strings.TrimSpace(strings.SplitN(part, ":", 2)[0])
		if len(head) >= 2 && !strings.EqualFold(head, part) && !seen[head] {
			seen[head] = true
			out = append(out, head)
		}
		if len(out) >= 6 {
			break
		}
	}
	return out
}

// cleanTitle trims catalog cruft the hint may carry.
func cleanTitle(raw string) string {
	return strings.TrimSpace(malRe.ReplaceAllString(raw, ""))
}

func searchURL(origin, query string) string {
	encoded := url.QueryEscape(query)
	encoded = strings.ReplaceAll(encoded, "+", "%20")
	return origin + "/search/?q=" + encoded
}

func searchHits(html string) []*show {
	page := htmlDecode(html)
	var shows []*show
	seen := map[string]bool{}
	for _, match := range searchCardRe.FindAllStringSubmatch(page, -1) {
		slug := seriesSlug(attr(match[1], "href"))
		if slug == "" || seen[slug] {
			continue
		}
		body := match[2]
		title := strings.TrimSpace(h2Title(body))
		if title == "" {
			continue
		}
		seen[slug] = true
		shows = append(shows, &show{slug: slug, title: title, aliases: aliases(body)})
	}
	return shows
}

func pickShow(hits []*show, titleHint string) *show {
	want := normalize(titleHint)
	if want == "" {
		return nil
	}
	var primary, alias []*show
	for _, hit := range hits {
		if normalize(hit.title) == want {
			primary = append(primary, hit)
			continue
		}
		for _, alt := range hit.aliases {
			if normalize(alt) == want {
				alias = append(alias, hit)
				break
			}
		}
	}
	shortest := func(list []*show) *show {
		var best *show
		for _, hit := range list {
			if best == nil || len(normalize(hit.title)) < len(normalize(best.title)) {
				best = hit
			}
		}
		return best
	}
	if hit := shortest(primary); hit != nil {
		return hit
	}
	return shortest(alias)
}

func episodes(html string) []episodeRow {
	page := htmlDecode(html)
	start := strings.Index(page, `<ul class="newmanga">`)
	if start < 0 {
		return nil
	}
	end := strings.Index(page[start:], "</ul>")
	if end < 0 {
		end = len(page[start:])
	}
	list := page[start : start+end]
	var rows []episodeRow
	seen := map[int]bool{}
	for _, match := range episodeAnchorRe.FindAllStringSubmatchIndex(list, -1) {
		href := list[match[2]:match[3]]
		number, _ := strconv.Atoi(list[match[4]:match[5]])
		attrs := list[match[6]:match[7]]
		if !strings.Contains(attrs, "anm_det_pop") {
			continue
		}
		if number <= 0 || seen[number] {
			continue
		}
		tail := strings.Index(list[match[7]:], "</li>")
		if tail < 0 {
			tail = len(list) - match[7]
		}
		block := list[match[7] : match[7]+tail]
		episodeURL := episodeURL(href)
		if episodeURL == "" {
			continue
		}
		seen[number] = true
		rows = append(rows, episodeRow{
			number: number,
			url:    episodeURL,
			sub:    strings.Contains(block, "#subbed") || strings.Contains(block, "btn-subbed"),
			dub:    strings.Contains(block, "#dubbed") || strings.Contains(block, "btn-dubbed"),
			raw:    strings.Contains(block, "#raw") || strings.Contains(block, "btn-raw"),
		})
	}
	return rows
}

func mirrors(html string) []mirror {
	page := htmlDecode(html)
	var out []mirror
	for _, match := range mirrorRe.FindAllStringSubmatch(page, -1) {
		path := embedPath(match[3])
		if path == "" {
			continue
		}
		out = append(out, mirror{version: strings.ToLower(match[1]), embedPath: path})
	}
	return out
}

func chooseMirror(list []mirror, dub bool) *mirror {
	versions := []string{"subbed", "raw"}
	if dub {
		versions = []string{"dubbed"}
	}
	for _, version := range versions {
		for i := range list {
			if list[i].version == version {
				return &list[i]
			}
		}
	}
	return nil
}

func mediaFiles(html string) []mediaFile {
	var files []mediaFile
	for _, match := range fileRe.FindAllStringSubmatch(html, -1) {
		file, ok := fileURL(match[1])
		if !ok {
			continue
		}
		label := strings.TrimSpace(htmlDecode(match[2]))
		if label == "" {
			continue
		}
		dup := false
		for _, existing := range files {
			if existing.url == file {
				dup = true
				break
			}
		}
		if !dup {
			files = append(files, mediaFile{url: file, label: label})
		}
	}
	return files
}

// fileURL keeps only the site's own /play/ files.
func fileURL(raw string) (string, bool) {
	text := strings.TrimSpace(raw)
	absolute := text
	switch {
	case strings.HasPrefix(text, "/play/"):
		absolute = Origin + text
	case strings.HasPrefix(text, Origin+"/play/"),
		strings.HasPrefix(text, "https://animegg.org/play/"):
	default:
		return "", false
	}
	if strings.HasPrefix(text, "https://animegg.org/play/") {
		absolute = Origin + "/play/" + strings.TrimPrefix(text, "https://animegg.org/play/")
	}
	u, err := url.Parse(absolute)
	if err != nil || u.User != nil {
		return "", false
	}
	if !strings.EqualFold(u.Scheme, "https") || !strings.EqualFold(u.Hostname(), "www.animegg.org") {
		return "", false
	}
	path := u.Path
	if !playPathRe.MatchString(path) {
		return "", false
	}
	if strings.Contains(u.RawQuery, "#") || (u.RawQuery != "" && !queryRe.MatchString(u.RawQuery)) {
		return "", false
	}
	if u.RawQuery == "" {
		return Origin + path, true
	}
	return Origin + path + "?" + u.RawQuery, true
}

func episodeURL(raw string) string {
	text := strings.TrimSpace(raw)
	path := strings.SplitN(strings.SplitN(text, "?", 2)[0], "#", 2)[0]
	match := episodePathRe.FindStringSubmatch(path)
	if match == nil {
		return ""
	}
	number, _ := strconv.Atoi(match[2])
	if number <= 0 {
		return ""
	}
	slug := strings.Trim(match[1], "/")
	if !slugRe.MatchString(slug) {
		return ""
	}
	return Origin + "/" + slug + "-episode-" + strconv.Itoa(number)
}

func embedPath(src string) string {
	text := strings.TrimSpace(src)
	path := text
	switch {
	case strings.HasPrefix(text, "/embed/"):
	case strings.HasPrefix(text, "//www.animegg.org/embed/"):
		path = strings.TrimPrefix(text, "//www.animegg.org")
	case strings.HasPrefix(text, "//animegg.org/embed/"):
		path = strings.TrimPrefix(text, "//animegg.org")
	case strings.HasPrefix(text, Origin+"/embed/"):
		path = strings.TrimPrefix(text, Origin)
	case strings.HasPrefix(text, "https://animegg.org/embed/"):
		path = strings.TrimPrefix(text, "https://animegg.org")
	default:
		return ""
	}
	path = strings.SplitN(strings.SplitN(path, "?", 2)[0], "#", 2)[0]
	if !embedPathRe.MatchString(path) {
		return ""
	}
	return path
}

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

func normalize(raw string) string {
	text := strings.ToLower(strings.TrimSpace(raw))
	text = strings.ReplaceAll(text, "’", "'")
	text = strings.ReplaceAll(text, "'", "")
	return strings.Join(strings.FieldsFunc(text, func(r rune) bool {
		return !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r >= 'à' && r <= 'ÿ')
	}), " ")
}

func seriesSlug(href string) string {
	path := strings.SplitN(strings.SplitN(href, "?", 2)[0], "#", 2)[0]
	at := strings.Index(path, "/series/")
	if at < 0 {
		return ""
	}
	slug := strings.Trim(path[at+len("/series/"):], "/")
	if !slugRe.MatchString(slug) {
		return ""
	}
	return slug
}

func aliases(body string) []string {
	match := altRe.FindStringSubmatch(body)
	if match == nil {
		return nil
	}
	var out []string
	for _, part := range strings.FieldsFunc(match[1], func(r rune) bool {
		return r == ',' || r == '、' || r == ';'
	}) {
		part = strings.TrimSpace(part)
		if len(part) >= 2 {
			out = append(out, part)
		}
	}
	return out
}

func height(label string) int {
	match := heightRe.FindStringSubmatch(label)
	if match == nil {
		return 0
	}
	value, _ := strconv.Atoi(match[1])
	return value
}

func heightLabel(label string) string {
	return heightLabelOf(height(label))
}

func heightLabelOf(h int) string {
	switch {
	case h >= 2000:
		return "4K"
	case h >= 360:
		return strconv.Itoa(h) + "p"
	default:
		return ""
	}
}

func gradeToken(label string) string {
	match := gradeRe.FindStringSubmatch(label)
	if match == nil {
		return ""
	}
	return strings.ToUpper(match[1])
}

func attr(tag, name string) string {
	match := attrRe(name).FindStringSubmatch(tag)
	if match == nil {
		return ""
	}
	return strings.TrimSpace(match[1])
}

func h2Title(body string) string {
	match := h2Re.FindStringSubmatch(body)
	if match == nil {
		return ""
	}
	return match[1]
}

func htmlDecode(raw string) string {
	replacer := strings.NewReplacer(
		"&amp;", "&",
		"&quot;", `"`,
		"&#34;", `"`,
		"&#39;", "'",
		"&apos;", "'",
	)
	return replacer.Replace(raw)
}

var (
	slugRe          = regexp.MustCompile(`^[a-z0-9]+(?:-+[a-z0-9]+)*$`)
	episodePathRe   = regexp.MustCompile(`^/([a-z0-9-]+)-episode-(\d+)$`)
	playPathRe      = regexp.MustCompile(`^/play/\d+/video\.mp4$`)
	embedPathRe     = regexp.MustCompile(`^/embed/\d+$`)
	queryRe         = regexp.MustCompile(`^[A-Za-z0-9._~%=&-]+$`)
	malRe           = regexp.MustCompile(`(?i)\bmal:\d+\b`)
	heightRe        = regexp.MustCompile(`(?i)(\d{3,4})\s*p`)
	gradeRe         = regexp.MustCompile(`(?i)\b(UHD|FHD|HD|SD|HQ)\b`)
	searchCardRe    = regexp.MustCompile(`(?is)<a\b([^>]*\bclass="mse(?:\s[^"]*)?"[^>]*)>([\s\S]*?)</a>`)
	h2Re            = regexp.MustCompile(`(?i)<h2>([^<]*)</h2>`)
	altRe           = regexp.MustCompile(`(?i)Alt Titles\s*:\s*([^<]+)`)
	episodeAnchorRe = regexp.MustCompile(`(?i)<a\s+href="(/[a-z0-9-]+-episode-(\d+))"([^>]*)>`)
	mirrorRe        = regexp.MustCompile(`(?is)<div\s+id="(subbed|dubbed|raw)-([^"]+)"[^>]*>\s*<iframe[^>]*\ssrc="([^"]+)`)
	fileRe          = regexp.MustCompile(`(?i)file\s*:\s*["']([^"']+)["']\s*,\s*label\s*:\s*["']([^"']+)["']`)
)

func attrRe(name string) *regexp.Regexp {
	return regexp.MustCompile(`(?i)\b` + regexp.QuoteMeta(name) + `\s*=\s*["']([^"']*)["']`)
}
