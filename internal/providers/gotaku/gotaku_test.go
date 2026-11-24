package gotaku

import (
	"net/url"
	"reflect"
	"testing"
)

// ---------------------------------------------------------------------------
// buildTitleQueries (Gotaku Kotlin port).

func TestBuildTitleQueriesEmpty(t *testing.T) {
	if got := buildTitleQueries(""); len(got) != 0 {
		t.Fatalf("empty hint = %v", got)
	}
	if got := buildTitleQueries("  "); len(got) != 0 {
		t.Fatalf("blank hint = %v", got)
	}
	if got := buildTitleQueries("a"); len(got) != 0 {
		t.Fatalf("one-char hint = %v", got)
	}
	if got := buildTitleQueries("ab"); !reflect.DeepEqual(got, []string{"ab"}) {
		t.Fatalf("two-char hint = %v", got)
	}
}

func TestBuildTitleQueriesDropsMalParts(t *testing.T) {
	got := buildTitleQueries("One Piece || mal:21")
	if !reflect.DeepEqual(got, []string{"One Piece"}) {
		t.Fatalf("mal part = %v", got)
	}
	got = buildTitleQueries("Attack on Titan||mal:1||English Dub")
	if !reflect.DeepEqual(got, []string{"Attack on Titan", "English Dub"}) {
		t.Fatalf("multi seeds = %v", got)
	}
}

func TestBuildTitleQueriesParenStrip(t *testing.T) {
	got := buildTitleQueries("Ghost in the Shell (2008) || mal:43")
	if !reflect.DeepEqual(got, []string{"Ghost in the Shell (2008)", "Ghost in the Shell"}) {
		t.Fatalf("paren = %v", got)
	}
	got = buildTitleQueries("K-On! (TV)")
	if !reflect.DeepEqual(got, []string{"K-On! (TV)", "K-On!"}) {
		t.Fatalf("paren tail = %v", got)
	}
}

func TestBuildTitleQueriesHeadCuts(t *testing.T) {
	got := buildTitleQueries("Frieren: Beyond Journey's End")
	if !reflect.DeepEqual(got, []string{"Frieren: Beyond Journey's End", "Frieren"}) {
		t.Fatalf("colon = %v", got)
	}
	got = buildTitleQueries("Naruto — Shippuden")
	if !reflect.DeepEqual(got, []string{"Naruto — Shippuden", "Naruto"}) {
		t.Fatalf("em dash = %v", got)
	}
	got = buildTitleQueries("One Piece - Wano Arc")
	if !reflect.DeepEqual(got, []string{"One Piece - Wano Arc", "One Piece"}) {
		t.Fatalf("hyphen head = %v", got)
	}
}

func TestBuildTitleQueriesSeasonStrip(t *testing.T) {
	got := buildTitleQueries("Sword Art Online Season 2")
	if !reflect.DeepEqual(got, []string{"Sword Art Online Season 2", "Sword Art Online"}) {
		t.Fatalf("season = %v", got)
	}
	got = buildTitleQueries("Sword Art Online season 1")
	if !reflect.DeepEqual(got, []string{"Sword Art Online season 1", "Sword Art Online"}) {
		t.Fatalf("lowercase season = %v", got)
	}
	// "Season" inside the title is kept.
	got = buildTitleQueries("The Season of the Witch")
	if !reflect.DeepEqual(got, []string{"The Season of the Witch"}) {
		t.Fatalf("inner season = %v", got)
	}
}

func TestBuildTitleQueriesCapsAtEight(t *testing.T) {
	hint := "Alpha: One || Beta: Two || Gamma: Three || Delta: Four || Epsilon: Five"
	got := buildTitleQueries(hint)
	want := []string{
		"Alpha: One", "Beta: Two", "Gamma: Three", "Delta: Four", "Epsilon: Five",
		"Alpha", "Beta", "Gamma",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("take(8) = %v", got)
	}
}

// ---------------------------------------------------------------------------
// Poster match.

func TestPosterMatchesAnilistDigitBoundary(t *testing.T) {
	if !posterMatchesAnilist("https://cdn.example/bx21-poster.jpg", 21) {
		t.Fatal("bx21-poster did not match 21")
	}
	if posterMatchesAnilist("https://cdn.example/bx210-poster.jpg", 21) {
		t.Fatal("bx210 matched 21 (digit boundary ignored)")
	}
	if posterMatchesAnilist("https://cdn.example/bx2101-poster.jpg", 210) {
		t.Fatal("bx2101 matched 210 (digit boundary ignored)")
	}
	if !posterMatchesAnilist("https://cdn.example/abx21.jpg", 21) {
		t.Fatal("embedded bx21 did not match")
	}
	if !posterMatchesAnilist("https://cdn.example/bx21", 21) {
		t.Fatal("trailing bx21 did not match")
	}
	if posterMatchesAnilist("", 21) || posterMatchesAnilist("   ", 21) {
		t.Fatal("blank poster matched")
	}
	if posterMatchesAnilist("https://cdn.example/bx21.jpg", 0) {
		t.Fatal("non-positive anilist id matched")
	}
}

// ---------------------------------------------------------------------------
// Variant parsing.

const masterFixture = "#EXTM3U\n" +
	"#EXT-X-STREAM-INF:BANDWIDTH=5000000,RESOLUTION=1920x1080\r\n" +
	"https://cdn.example/p/1/1080/index.m3u8\r\n" +
	"#EXT-X-STREAM-INF:BANDWIDTH=2500000,RESOLUTION=1280x720\n" +
	"v720.m3u8\n" +
	"#EXT-X-STREAM-INF:BANDWIDTH=900000,RESOLUTION=854x480\n" +
	"https://cdn.example/p/1/480/index.m3u8\n" +
	"#EXT-X-STREAM-INF:BANDWIDTH=100000\n" +
	"https://cdn.example/p/1/audio-only/index.m3u8\n" +
	"#EXT-X-ENDLIST\n"

func TestParseVariantsKeepsOnlyHTTP(t *testing.T) {
	variants := parseVariants(masterFixture)
	if len(variants) != 3 {
		t.Fatalf("variants = %+v", variants)
	}
	if variants[0].height != 1080 || variants[0].bandwidth != 5000000 ||
		variants[0].url != "https://cdn.example/p/1/1080/index.m3u8" {
		t.Fatalf("v0 = %+v", variants[0])
	}
	if variants[1].height != 480 || variants[1].url != "https://cdn.example/p/1/480/index.m3u8" {
		t.Fatalf("v1 = %+v", variants[1])
	}
	if variants[2].height != 0 || variants[2].bandwidth != 100000 ||
		variants[2].url != "https://cdn.example/p/1/audio-only/index.m3u8" {
		t.Fatalf("v2 = %+v", variants[2])
	}
	for _, v := range variants {
		if v.url == "v720.m3u8" {
			t.Fatal("relative variant kept")
		}
	}
}

func TestSortVariantsTallestFirst(t *testing.T) {
	sorted := sortVariantsTallest(parseVariants(masterFixture))
	if len(sorted) != 3 || sorted[0].height != 1080 || sorted[1].height != 480 || sorted[2].height != 0 {
		t.Fatalf("sorted = %+v", sorted)
	}
}

// ---------------------------------------------------------------------------
// Embed page stamp/base.

const embedPageFixture = `<html><head></head><body>
<div id="player" data-manifest-stamp="stamp-abc-123"
     data-manifest-base="https://gotakudl.top/manifest"></div>
<script>window.boot()</script>
</body></html>`

func TestParseEmbedPage(t *testing.T) {
	page, ok := parseEmbedPage(embedPageFixture)
	if !ok || page.stamp != "stamp-abc-123" || page.base != "https://gotakudl.top/manifest" {
		t.Fatalf("page = %+v ok=%v", page, ok)
	}
	page, ok = parseEmbedPage(`<div DATA-MANIFEST-STAMP="s2" DATA-MANIFEST-BASE="https://b.example"></div>`)
	if !ok || page.stamp != "s2" || page.base != "https://b.example" {
		t.Fatalf("uppercase attributes = %+v ok=%v", page, ok)
	}
	if _, ok := parseEmbedPage(`<div data-manifest-stamp="only-stamp"></div>`); ok {
		t.Fatal("missing base accepted")
	}
	if _, ok := parseEmbedPage(`<div data-manifest-base="https://b.example"></div>`); ok {
		t.Fatal("missing stamp accepted")
	}
	if _, ok := parseEmbedPage(`<div data-manifest-stamp='single-quoted'></div>`); ok {
		t.Fatal("single-quoted attribute accepted")
	}
}

// ---------------------------------------------------------------------------
// JSON shaping helpers.

func TestParseTitleHits(t *testing.T) {
	data := mustParseJSON(t, `{"data":[
		{"id":"t1","name":"One Piece","poster_url":"https://cdn.example/bx21-1.jpg"},
		{"id":"t2","name":"Other"},
		{"name":"no id"}
	]}`)
	hits := parseTitleHits(data)
	if len(hits) != 2 {
		t.Fatalf("hits = %+v", hits)
	}
	if hits[0].id != "t1" || hits[0].poster != "https://cdn.example/bx21-1.jpg" {
		t.Fatalf("h0 = %+v", hits[0])
	}
	if hits[1].poster != "" {
		t.Fatalf("h1 poster = %q", hits[1].poster)
	}
}

func TestParseEpisodes(t *testing.T) {
	data := mustParseJSON(t, `{"data":[
		{"number":1,"id":"e1","sub":true},
		{"number":2,"id":"e2","sub":false,"dub":true},
		{"number":3,"id":"e3"},
		{"number":"4","id":"e4","sub":"false","dub":"true"},
		{"id":"no-number"}
	]}`)
	eps := parseEpisodes(data)
	if len(eps) != 5 {
		t.Fatalf("eps = %+v", eps)
	}
	if eps[0].number != 1 || eps[0].id != "e1" || !eps[0].sub || eps[0].dub {
		t.Fatalf("ep0 = %+v", eps[0])
	}
	if eps[1].sub || !eps[1].dub {
		t.Fatalf("ep1 = %+v", eps[1])
	}
	if eps[2].number != 3 || !eps[2].sub || eps[2].dub {
		t.Fatalf("ep2 defaults = %+v", eps[2])
	}
	if eps[3].number != 4 || eps[3].sub || !eps[3].dub {
		t.Fatalf("ep3 string flags = %+v", eps[3])
	}
	if eps[4].number != -1 {
		t.Fatalf("ep4 number = %d", eps[4].number)
	}
}

func TestParseEmbedURL(t *testing.T) {
	if got := parseEmbedURL(mustParseJSON(t, `{"data":{"url":"https://gotakudl.top/embed/x"}}`)); got != "https://gotakudl.top/embed/x" {
		t.Fatalf("embed url = %q", got)
	}
	if got := parseEmbedURL(mustParseJSON(t, `{"data":{"url":"  https://gotakudl.top/embed/x  "}}`)); got != "https://gotakudl.top/embed/x" {
		t.Fatalf("trimmed embed url = %q", got)
	}
	if got := parseEmbedURL(mustParseJSON(t, `{"data":{}}`)); got != "" {
		t.Fatalf("missing embed url = %q", got)
	}
}

func TestParseManifest(t *testing.T) {
	data := mustParseJSON(t, `{
		"keySeed":"c2VlZA",
		"token":"pl-tok-9",
		"source":"https://cdn.example/p/1/master.m3u8",
		"obf":{"segmentBytes":4096},
		"captions":[
			{"url":"https://cdn.example/en.vtt","label":"English","lang":"en"},
			{"file":"https://cdn.example/ja.vtt","lang":"ja"},
			{"url":"/relative.vtt","label":"Relative"},
			"not-an-object"
		]
	}`)
	m := parseManifest(data)
	if m.keySeed != "c2VlZA" || m.token != "pl-tok-9" ||
		m.source != "https://cdn.example/p/1/master.m3u8" || m.segmentBytes != 4096 {
		t.Fatalf("manifest = %+v", m)
	}
	if len(m.captions) != 2 {
		t.Fatalf("captions = %+v", m.captions)
	}
	if m.captions[0].URL != "https://cdn.example/en.vtt" || m.captions[0].Label != "English" || m.captions[0].Language != "en" {
		t.Fatalf("caption0 = %+v", m.captions[0])
	}
	if m.captions[1].URL != "https://cdn.example/ja.vtt" || m.captions[1].Label != "ja" {
		t.Fatalf("caption1 = %+v", m.captions[1])
	}
}

func TestParseManifestDefaults(t *testing.T) {
	data := mustParseJSON(t, `{"keySeed":"k","token":"t","source":"s"}`)
	m := parseManifest(data)
	if m.segmentBytes != 8192 {
		t.Fatalf("default segmentBytes = %d", m.segmentBytes)
	}
	if len(m.captions) != 0 {
		t.Fatalf("default captions = %+v", m.captions)
	}
	data = mustParseJSON(t, `{"keySeed":"k","token":"t","source":"s","obf":{"segmentBytes":0}}`)
	if m := parseManifest(data); m.segmentBytes != 0 {
		t.Fatalf("explicit zero segmentBytes = %d", m.segmentBytes)
	}
}

// ---------------------------------------------------------------------------
// Proxy URL, headers, origin, labels.

func TestTransformPlayURL(t *testing.T) {
	variant := "https://cdn.example/p/1/1080/index.m3u8?a=1&b=2+3"
	embed := "https://gotakudl.top/embed/xyz"
	raw := transformPlayURL(variant, embed, "c2VlZA", "pl-tok", 8192)
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if u.Path != "/v1/gotaku/play" {
		t.Fatalf("path = %q", u.Path)
	}
	q := u.Query()
	if q.Get("u") != variant || q.Get("r") != embed || q.Get("seed") != "c2VlZA" ||
		q.Get("token") != "pl-tok" || q.Get("seg") != "8192" {
		t.Fatalf("query = %v", q)
	}
	// The URL is relative; the HTTP layer absolutizes it.
	if len(raw) == 0 || raw[0] != '/' {
		t.Fatalf("url not relative: %q", raw)
	}
}

func TestValidPlaylistURL(t *testing.T) {
	if !validPlaylistURL("https://cdn.example/p/1/1080/index.m3u8") {
		t.Fatal("valid /p/1 URL rejected")
	}
	if validPlaylistURL("http://cdn.example/p/1/1080/index.m3u8") {
		t.Fatal("http scheme accepted")
	}
	if validPlaylistURL("https://user:pass@cdn.example/p/1/x.m3u8") {
		t.Fatal("userinfo accepted")
	}
	if validPlaylistURL("https://cdn.example/p/1/../x.m3u8") {
		t.Fatal("path traversal accepted")
	}
	if validPlaylistURL("https://cdn.example/s/1/x.ts") {
		t.Fatal("segment path accepted as playlist")
	}
	if validPlaylistURL("not a url") {
		t.Fatal("garbage accepted")
	}
}

func TestOriginOf(t *testing.T) {
	if got := originOf("https://gotakudl.top/embed/abc?x=1"); got != "https://gotakudl.top" {
		t.Fatalf("embed origin = %q", got)
	}
	if got := originOf("https://gotaku.to"); got != "https://gotaku.to" {
		t.Fatalf("site origin = %q", got)
	}
	if got := originOf("not a url"); got != GOTAKU_DL {
		t.Fatalf("fallback origin = %q", got)
	}
}

func TestStreamHeaders(t *testing.T) {
	headers := streamHeaders("https://gotakudl.top/embed/abc")
	if headers["User-Agent"] != CHROME_UA || headers["Accept"] != "*/*" ||
		headers["Referer"] != "https://gotakudl.top/embed/abc" ||
		headers["Origin"] != "https://gotakudl.top" {
		t.Fatalf("headers = %v", headers)
	}
}

func TestHeightLabelAndModeWord(t *testing.T) {
	cases := map[int]string{1080: "1080p", 720: "720p", 2160: "4K", 2400: "4K", 360: "360p", 240: "", 0: ""}
	for height, want := range cases {
		if got := heightLabel(height); got != want {
			t.Fatalf("heightLabel(%d) = %q, want %q", height, got, want)
		}
	}
	if modeWord("dub") != "DUB" || modeWord("sub") != "SUB" || modeWord("raw") != "SUB" {
		t.Fatal("modeWord wrong")
	}
}

// ---------------------------------------------------------------------------
// Helpers.

func mustParseJSON(t *testing.T, body string) jsonObj {
	t.Helper()
	obj, err := parseJSON([]byte(body))
	if err != nil {
		t.Fatalf("fixture JSON: %v", err)
	}
	return obj
}
