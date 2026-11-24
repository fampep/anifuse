package anikoto

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"encoding/base64"
	"encoding/json"
	"testing"
	"time"
)

// mustJSON parses a fixture body into a jsonObj.
func mustJSON(t *testing.T, body string) jsonObj {
	t.Helper()
	obj, err := parseJSON(body)
	if err != nil {
		t.Fatalf("fixture JSON: %v", err)
	}
	return obj
}

func mustParse(t *testing.T, body string) jsonObj { return mustJSON(t, body) }

// ---------------------------------------------------------------------------
// KotoplayFallbackTest mirrors.

func TestSuggestPrefersTheAnilistID(t *testing.T) {
	data := mustParse(t, `
		{"anime":[
		  {"slug":"one-piece-recap","title":"One Piece Recap","ani_id":999,"mal_id":1},
		  {"slug":"one-piece-odmau","title":"One Piece","ani_id":21,"mal_id":21}
		]}`)
	hits := parseKotoSuggest(data)
	picked := pickKotoHit(hits, 21, []string{"One Piece"}, 21)
	if picked == nil || picked.slug != "one-piece-odmau" {
		t.Fatalf("picked = %+v", picked)
	}
}

func TestChaptersAndServersKeepSubDubAndSkipTheEmptyWindow(t *testing.T) {
	anime, ok := kotoAnimeObject(mustParse(t, `
		{"anime":{"slug":"hell-girl","ani_id":228,"chapters":[
		  {"id":5810,"chapter_number":"1","is_sub":1,"is_dub":0}
		]}}`))
	if !ok {
		t.Fatal("kotoAnimeObject failed")
	}
	chapters := parseKotoChapters(anime)
	if len(chapters) != 1 || chapters[0].number != 1 || chapters[0].chapterID != 5810 ||
		!chapters[0].hasSub || chapters[0].hasDub {
		t.Fatalf("chapters = %+v", chapters)
	}
	links := parseKotoServerLinks(mustParse(t, `
		{"servers":{"sub":[
		  {"name":"HD-1","link":"https://megaplay.buzz/stream/s-2/17534/sub?s=tcdn","skip_data":{"intro":[0,0],"outro":[0,0]}},
		  {"name":"Vidstream-2","link":"https://megaplay.buzz/stream/s-2/17534/sub","skip_data":{"intro":[12,90],"outro":[1300,1400]}}
		]}}`), "sub")
	if len(links) != 2 || links[0].label != "HD-1" || links[1].label != "Vidstream-2" {
		t.Fatalf("links = %+v", links)
	}
	if !contains(links[1].url, "/sub") || contains(links[1].url, "tcdn") {
		t.Fatalf("links[1].url = %q", links[1].url)
	}
}

func TestTitleMatchDoesNotStealADifferentAnilistID(t *testing.T) {
	hits := []kotoHit{
		{slug: "other", title: "Ghost in the Shell", aniID: 40, malID: 40},
		{slug: "untitled", title: "Ghost in the Shell", aniID: 0, malID: 0},
	}
	if pickKotoHit(hits, 43, []string{"Ghost in the Shell"}, 43) != nil {
		t.Fatal("a hit without the requested MAL id was picked")
	}
}

func TestMalIDStillSelectsTheRow(t *testing.T) {
	hits := []kotoHit{
		{slug: "sequel", title: "Ghost in the Shell", aniID: 0, malID: 99},
		{slug: "film", title: "Ghost in the Shell", aniID: 0, malID: 43},
	}
	picked := pickKotoHit(hits, 43, []string{"Ghost in the Shell"}, 43)
	if picked == nil || picked.slug != "film" {
		t.Fatalf("picked = %+v", picked)
	}
}

func TestAShortSpecialWithTheSeriesIDsDoesNotOutrankTheSeries(t *testing.T) {
	hits := []kotoHit{
		{slug: "special", title: "Demon Slayer: Sibling's Bond", aniID: 101922, malID: 38000},
		{slug: "series", title: "Demon Slayer: Kimetsu no Yaiba", aniID: 0, malID: 38000},
		{slug: "naruto-movie", title: "Naruto the Movie: Ninja Clash in the Land of Snow", aniID: 20, malID: 20},
		{slug: "naruto", title: "Naruto", aniID: 20, malID: 20},
	}
	picked := pickKotoHit(hits, 101922, []string{"Kimetsu no Yaiba", "Demon Slayer: Kimetsu no Yaiba"}, 38000)
	if picked == nil || picked.slug != "series" {
		t.Fatalf("picked = %+v", picked)
	}
	picked = pickKotoHit(hits, 20, []string{"Naruto"}, 20)
	if picked == nil || picked.slug != "naruto" {
		t.Fatalf("picked = %+v", picked)
	}
}

func TestALaterRowStaysAvailableWhenTheFirstPageDisagrees(t *testing.T) {
	hits := []kotoHit{
		{slug: "recap", title: "One Piece", aniID: 21, malID: 0},
		{slug: "series", title: "One Piece", aniID: 0, malID: 21},
	}
	ranked := rankKotoHits(hits, 21, []string{"One Piece"}, 21)
	if len(ranked) != 2 || ranked[0].slug != "recap" || ranked[1].slug != "series" {
		t.Fatalf("ranked = %+v", ranked)
	}
	recap := mustParse(t, `{"ani_id":21,"chapters":[{"id":1,"chapter_number":"1","mal_id":1}]}`)
	series := mustParse(t, `{"mal_id":21,"chapters":[{"id":2,"chapter_number":"1","mal_id":21}]}`)
	if kotoChaptersIfMatch(recap, 21, 21) != nil {
		t.Fatal("recap page accepted despite a different chapter MAL id")
	}
	chapters := kotoChaptersIfMatch(series, 21, 21)
	if chapters == nil || len(chapters) != 1 || chapters[0].chapterID != 2 {
		t.Fatalf("series chapters = %+v", chapters)
	}
}

// ---------------------------------------------------------------------------
// AnikotoLatestParserTest mirror.

func TestParseWidgetHTMLKeepsSubAndDubCounts(t *testing.T) {
	html := `
            <div class="ani items">
              <div class="item">
                <div class="ani poster" data-tip="9081">
                  <a href="https://anikoto.cz/watch/kanata-kara-c16d8/ep-7">
                    <img src="https://cdn.example/poster.jpg" alt="From Far Away" />
                    <div class="meta"><div class="inner"><div class="left">
                      <span class="ep-status sub"><span> 7</span></span>
                      <span class="ep-status dub"><span> 4</span></span>
                      <span class="ep-status total"><span>12</span></span>
                    </div></div></div>
                  </a>
                </div>
                <div class="info">
                  <a class="name d-title" href="https://anikoto.cz/watch/kanata-kara-c16d8/ep-7"
                    data-jp="Kanata kara">From Far Away, From the Other Side</a>
                </div>
              </div>
              <div class="item">
                <div class="ani poster" data-tip="9063">
                  <a href="https://anikoto.cz/watch/my-girlfriend-s-friend-5cc36/ep-7">
                    <img src="https://cdn.example/g.jpg" />
                    <div class="meta"><div class="inner"><div class="left">
                      <span class="ep-status sub"><span> 7</span></span>
                    </div></div></div>
                  </a>
                </div>
                <div class="info">
                  <a class="name d-title" href="https://anikoto.cz/watch/my-girlfriend-s-friend-5cc36/ep-7">
                    My Girlfriend&#039;s Friend
                  </a>
                </div>
              </div>
            </div>
        `
	items := parseWidgetHTML(html)
	if len(items) != 2 {
		t.Fatalf("items = %+v", items)
	}
	first := items[0]
	if first.siteID != "9081" || first.slug != "kanata-kara-c16d8" ||
		first.latestSub != 7 || first.latestDub != 4 ||
		first.hrefEpisode != 7 || first.title != "From Far Away, From the Other Side" {
		t.Fatalf("first = %+v", first)
	}
	if items[1].latestSub != 7 || items[1].latestDub != 0 || !contains(items[1].title, "Girlfriend") {
		t.Fatalf("second = %+v", items[1])
	}
}

// ---------------------------------------------------------------------------
// Crypto: key extraction and the AES-CBC round trip.

const cryptoFixture = `
var keyList = ["TRUST_AES_KEY"],"0123456789abcdef";
var ivList = ["TRUST_AES_IV"],"fedcba9876543210";
var strip = ["stripBytes","STRIP_BYTES",256];
`

func TestParseKeysAndStripFromPlayerJS(t *testing.T) {
	key, iv, ok := parseKeys(cryptoFixture)
	if !ok || key != "0123456789abcdef" || iv != "fedcba9876543210" {
		t.Fatalf("key=%q iv=%q ok=%v", key, iv, ok)
	}
	if strip := parseStripBytes(cryptoFixture); strip != 256 {
		t.Fatalf("strip = %d", strip)
	}
	if strip := parseStripBytes(`var other = 1;`); strip != fallbackStrip {
		t.Fatalf("fallback strip = %d", strip)
	}
	if _, _, ok := parseKeys(`var x = "short";`); ok {
		t.Fatal("short markers accepted")
	}
}

func pkcs7Pad(b []byte, blockSize int) []byte {
	n := blockSize - len(b)%blockSize
	out := make([]byte, len(b)+n)
	copy(out, b)
	for i := 0; i < n; i++ {
		out[len(b)+i] = byte(n)
	}
	return out
}

func encryptPayload(t *testing.T, plain string) string {
	t.Helper()
	block, err := aes.NewCipher(padKeyBytes("0123456789abcdef", 32))
	if err != nil {
		t.Fatal(err)
	}
	padded := pkcs7Pad([]byte(plain), aes.BlockSize)
	out := make([]byte, len(padded))
	cipher.NewCBCEncrypter(block, padKeyBytes("fedcba9876543210", 16)).CryptBlocks(out, padded)
	return base64.RawURLEncoding.EncodeToString(out)
}

func testCrypto() *megaCrypto {
	return &megaCrypto{cached: &megaKeys{
		key:       padKeyBytes("0123456789abcdef", 32),
		iv:        padKeyBytes("fedcba9876543210", 16),
		strip:     256,
		fetchedAt: time.Now(),
	}}
}

// TestDecryptLiveEnvelope locks in the format the live player uses: URL-safe
// base64, AES-256-CBC with the key material zero-padded to 32 bytes, PKCS7.
func TestDecryptLiveEnvelope(t *testing.T) {
	c := &megaCrypto{cached: &megaKeys{
		key:       padKeyBytes(fallbackKey, 32),
		iv:        padKeyBytes(fallbackIV, 16),
		strip:     fallbackStrip,
		fetchedAt: time.Now(),
	}}
	enc := "wdeBruh3qqn_i5wUNnyaPcXqidp1UWP84FfPHzGyKXDraQPY7v8r51K1Ja3Z9p4xUCDdxN4j6qo8xbGWTaJj2Y-xKmYc3l8lRVDmslXzQRCI3uDxTpQpqcKjZr1q3EIF75hGCSlVpBlfehmqIsB3wv21GVry9H5MgqILrNY1PJM"
	decrypted, err := c.decryptEnc(context.Background(), enc, megaPlayHome)
	if err != nil {
		t.Fatalf("decryptEnc: %v", err)
	}
	want := "https://fetch.nexabloom.top/anime/9a4400501febb2a95e79248486a5f6d3/55f8961f7cd0bd1e3ed2ebeda925b075/master.m3u8"
	if file := decrypted.str("file"); file != want {
		t.Fatalf("file = %q, want %q", file, want)
	}
}

func TestDecryptEncRoundTrip(t *testing.T) {
	c := testCrypto()
	plain := `{"sources":[{"file":"https://cdn.example/video.m3u8","quality":"1080p"}],"tracks":[{"kind":"captions","file":"https://cdn.example/en.vtt","label":"English","lang":"en"}]}`
	enc := encryptPayload(t, plain)
	decrypted, err := c.decryptEnc(context.Background(), enc, megaPlayHome)
	if err != nil {
		t.Fatalf("decryptEnc: %v", err)
	}
	if decrypted.str("sources") != "" {
		t.Fatalf("sources not an array: %+v", decrypted)
	}
	var roundTrip any
	if err := json.Unmarshal([]byte(plain), &roundTrip); err != nil {
		t.Fatal(err)
	}
	if file := c.extractFile(context.Background(), decrypted); file != "https://cdn.example/video.m3u8" {
		t.Fatalf("extractFile = %q", file)
	}
	labeled := c.extractLabeled(context.Background(), decrypted)
	if len(labeled) != 1 || labeled[0].url != "https://cdn.example/video.m3u8" || labeled[0].quality != "1080p" {
		t.Fatalf("labeled = %+v", labeled)
	}
	subs := c.extractSubtitles(context.Background(), decrypted)
	if len(subs) != 1 || subs[0].URL != "https://cdn.example/en.vtt" || subs[0].MimeType != "text/vtt" || subs[0].Language != "en" {
		t.Fatalf("subs = %+v", subs)
	}
}

func TestExtractFromEncryptedEnvelope(t *testing.T) {
	c := testCrypto()
	// The getSources shape with everything behind enc: quality map, tracks,
	// no plaintext file.
	plain := `{"sources":{"1080":"https://cdn.example/hd.m3u8","720":"https://cdn.example/sd.m3u8"},"tracks":[{"kind":"captions","file":"https://cdn.example/en.ass","label":"English","lang":"en"}]}`
	envelope := map[string]any{"enc": encryptPayload(t, plain)}
	data := jsonObj(envelope)

	if file := c.extractFile(context.Background(), data); file != "" {
		t.Fatalf("extractFile found a file in a quality map: %q", file)
	}
	labeled := c.extractLabeled(context.Background(), data)
	if len(labeled) != 2 {
		t.Fatalf("labeled = %+v", labeled)
	}
	if labeled[0].url != "https://cdn.example/hd.m3u8" || labeled[0].quality != "1080" {
		t.Fatalf("tallest first = %+v", labeled[0])
	}
	if labeled[1].url != "https://cdn.example/sd.m3u8" || labeled[1].quality != "720" {
		t.Fatalf("second = %+v", labeled[1])
	}
	subs := c.extractSubtitles(context.Background(), data)
	if len(subs) != 1 || subs[0].MimeType != "text/x-ssa" {
		t.Fatalf("subs = %+v", subs)
	}
}

func TestExtractLabeledPlainShapes(t *testing.T) {
	c := testCrypto()
	arrayShape := mustParse(t, `{"sources":[{"file":"https://cdn.example/a.m3u8","quality":"720p"},{"url":"https://cdn.example/b.m3u8","label":"1080p"}]}`)
	labeled := c.extractLabeled(context.Background(), arrayShape)
	if len(labeled) != 2 || labeled[0].url != "https://cdn.example/b.m3u8" || labeled[1].quality != "720p" {
		t.Fatalf("array shape = %+v", labeled)
	}
	objectShape := mustParse(t, `{"sources":{"file":"https://cdn.example/single.m3u8"}}`)
	labeled = c.extractLabeled(context.Background(), objectShape)
	if len(labeled) != 1 || labeled[0].url != "https://cdn.example/single.m3u8" || labeled[0].quality != "Stream" {
		t.Fatalf("object shape = %+v", labeled)
	}
	fileOnly := mustParse(t, `{"sources":{"file":"https://cdn.example/only.m3u8"}}`)
	if file := c.extractFile(context.Background(), fileOnly); file != "https://cdn.example/only.m3u8" {
		t.Fatalf("file-only = %q", file)
	}
}

// ---------------------------------------------------------------------------
// Anikoto HTML parser fixtures.

func TestParseWatchIDAndPageMatching(t *testing.T) {
	if id := parseWatchID(`<div id="watch-main" data-id="12345" class="x">`); id != "12345" {
		t.Fatalf("watch id = %q", id)
	}
	if id := parseWatchID(`<div data-id="678" id="watch-main">`); id != "678" {
		t.Fatalf("reversed watch id = %q", id)
	}
	if id := parseWatchID(`<div id="watch-other" data-id="1">`); id != "" {
		t.Fatalf("unrelated id = %q", id)
	}
	if id := pageAnilistID(`<script>var data = {"idAnilist": 16498};</script>`); id != 16498 {
		t.Fatalf("anilist id = %d", id)
	}
	if id := pageAnilistID(`<img src="https://cdn.example/anime/cover/small/b16498-x.jpg">`); id != 16498 {
		t.Fatalf("cover id = %d", id)
	}
	if title := parsePageTitle(`<h1 class="title">Attack on Titan &amp; More</h1>`); title != "Attack on Titan & More" {
		t.Fatalf("h1 title = %q", title)
	}
	if title := parsePageTitle(`<title>Watch Attack on Titan Episode 1 Sub English</title>`); title != "Attack on Titan" {
		t.Fatalf("title tag = %q", title)
	}
	if !pageMatchesAnilist(`<script>{"idAnilist": 16498}</script>`, 16498, []string{"Other"}) {
		t.Fatal("anilist id page did not match")
	}
	if pageMatchesAnilist(`<script>{"idAnilist": 999}</script>`, 16498, []string{"Other"}) {
		t.Fatal("wrong anilist id page matched")
	}
	if !pageMatchesAnilist(`<h1 class="title">Attack on Titan</h1>`, 16498, []string{"Attack on Titan"}) {
		t.Fatal("title-matched page rejected")
	}
}

func TestParseEpisodes(t *testing.T) {
	html := `
		<div class="episodes">
		<a data-num="1" data-sub="1" data-dub="0" data-ids="a1,b2" href="x">1</a>
		<a data-num="2" data-sub="0" data-dub="1" data-ids="c3&amp;d4" href="x">2</a>
		<a data-num="0" data-sub="1" data-dub="1" data-ids="x" href="x">0</a>
		</div>`
	eps := parseEpisodes(html)
	if len(eps) != 2 {
		t.Fatalf("eps = %+v", eps)
	}
	if eps[0].number != 1 || !eps[0].hasSub || eps[0].hasDub || eps[0].ids != "a1,b2" {
		t.Fatalf("ep1 = %+v", eps[0])
	}
	if eps[1].number != 2 || eps[1].hasSub || !eps[1].hasDub || eps[1].ids != "c3&d4" {
		t.Fatalf("ep2 = %+v", eps[1])
	}
}

func TestParseServerLinksBlocks(t *testing.T) {
	html := `
		<div class="server-list" data-type="sub">
		  <li data-link-id="l1"><span>HD-1</span></li>
		  <li data-link-id="l2"><span>HD-2</span></li>
		</div>
		<div class="type">Dub</div>
		<div class="server-list" data-type="dub">
		  <li data-link-id="d1"><span>Dub-1</span></li>
		</div>`
	sub := parseServerLinks(html, "sub")
	if len(sub) != 2 || sub[0].id != "l1" || sub[1].label != "HD-2" {
		t.Fatalf("sub = %+v", sub)
	}
	dub := parseServerLinks(html, "dub")
	if len(dub) != 1 || dub[0].id != "d1" || dub[0].label != "Dub-1" {
		t.Fatalf("dub = %+v", dub)
	}
	// No dub block: fall back to the sub window.
	subOnly := `<div class="server-list" data-type="sub"><li data-link-id="s1"><span>S</span></li></div>`
	dubFallback := parseServerLinks(subOnly, "dub")
	if len(dubFallback) != 1 || dubFallback[0].id != "s1" {
		t.Fatalf("dub fallback = %+v", dubFallback)
	}
}

func TestParseSearchCandidates(t *testing.T) {
	html := `
		<div class="list">
		<a class="item" href="https://anikoto.cz/watch/one-piece-xyz">
		  <div class="info"><div class="name">One Piece &amp; More</div></div>
		</a>
		<a class="item" href="https://anikoto.cz/watch/naruto-abc">
		  <div class="name">Naruto</div>
		</a>
		</div>`
	cands := parseSearchCandidates(html)
	if len(cands) != 2 {
		t.Fatalf("cands = %+v", cands)
	}
	if cands[0].url != "https://anikoto.cz/watch/one-piece-xyz" || cands[0].name != "One Piece & More" {
		t.Fatalf("cand0 = %+v", cands[0])
	}
	if cands[1].url != "https://anikoto.cz/watch/naruto-abc" || cands[1].name != "Naruto" {
		t.Fatalf("cand1 = %+v", cands[1])
	}
}

func TestParseDataID(t *testing.T) {
	if id := parseDataID(`<div id="megaplay-player" data-id="a-1_b.c"></div>`); id != "a-1_b.c" {
		t.Fatalf("player id = %q", id)
	}
	if id := parseDataID(`<div data-id="z-9" id="megaplay-player"></div>`); id != "z-9" {
		t.Fatalf("reversed player id = %q", id)
	}
	if id := parseDataID(`<section data-id="first"></section><div data-id="second!"></div>`); id != "first" {
		t.Fatalf("any data id = %q", id)
	}
	if id := parseDataID(`<section data-id="bad id"></section>`); id != "" {
		t.Fatalf("invalid data id = %q", id)
	}
	if !isValidDataID("a-1_b.c") || isValidDataID("bad id") || isValidDataID("") {
		t.Fatal("isValidDataID wrong")
	}
}

// ---------------------------------------------------------------------------
// Title helpers.

func TestTitlesIdentifySameWork(t *testing.T) {
	if !titlesIdentifySameWork("Ghost in the Shell", []string{"Ghost in the Shell (2008)"}) {
		t.Fatal("year suffix did not identify the same work")
	}
	if !titlesIdentifySameWork("Sword Art Online Season 2", []string{"Sword Art Online 2nd Season"}) {
		t.Fatal("season spelling did not identify the same work")
	}
	if titlesIdentifySameWork("Ghost in the Shell", []string{"Ghost in the Shell 2.0"}) {
		t.Fatal("2.0 identified as the original")
	}
	if titlesIdentifySameWork("The Ghost in the Shell", []string{"Ghost in the Shell"}) {
		t.Fatal("the-prefix title identified as the original")
	}
	if titlesIdentifySameWork("", []string{"Anything"}) {
		t.Fatal("empty page title identified")
	}
}

func TestTitlesLooselyMatch(t *testing.T) {
	if !titlesLooselyMatch("Attack on Titan", "Attack on Titan Final Season") {
		t.Fatal("prefix containment did not match")
	}
	if !titlesLooselyMatch("Naruto", "Naruto: Shippuden") {
		t.Fatal("shared token did not match")
	}
	if titlesLooselyMatch("", "Anything") || titlesLooselyMatch("X", "Y") {
		t.Fatal("empty or unrelated titles matched")
	}
}

func TestHintPartsAndBuildTitleQueries(t *testing.T) {
	titles, mal := hintParts("One Piece||mal:21")
	if mal != 21 {
		t.Fatalf("mal = %d", mal)
	}
	found := false
	for _, title := range titles {
		if title == "One Piece" {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("titles = %v", titles)
	}
	queries := buildTitleQueries("Sword Art Online Season 2")
	if !containsAll(queries, "Sword Art Online Season 2", "Sword Art Online 2nd Season", "Sword Art Online") {
		t.Fatalf("queries = %v", queries)
	}
	queries = buildTitleQueries("Frieren: Beyond Journey's End")
	if !containsAll(queries, "Frieren: Beyond Journey's End", "Frieren") {
		t.Fatalf("colon queries = %v", queries)
	}
	if got := titleSlug("One Piece!"); got != "one-piece" {
		t.Fatalf("titleSlug = %q", got)
	}
	if got := titleSlug("  *** "); got != "" {
		t.Fatalf("empty titleSlug = %q", got)
	}
	if got := normalizeTitleKey("2nd Season"); got != normalizeTitleKey("Season 2") {
		t.Fatalf("season variants differ: %q vs %q", got, normalizeTitleKey("Season 2"))
	}
}

func TestQualityHelpers(t *testing.T) {
	if got := qualityLabel("HD-1", "1080p"); got != "HD-1 · 1080p" {
		t.Fatalf("qualityLabel = %q", got)
	}
	if got := qualityLabel("", "stream"); got != "Anikoto" {
		t.Fatalf("qualityLabel empty = %q", got)
	}
	if got := qualityLabel("Vidstream-2", "Stream"); got != "Vidstream-2" {
		t.Fatalf("qualityLabel stream = %q", got)
	}
	if qualityHeight("1080p") != 1080 || qualityHeight("HD-1") != 1080 || qualityHeight("Ultra 4K") != 2160 || qualityHeight("x") != 0 {
		t.Fatal("qualityHeight wrong")
	}
	quality, audio, rating := sourceMeta("HD-1 · 1080p", "sub")
	if quality != "1080p" || audio != "SUB" || rating != "HD" {
		t.Fatalf("sourceMeta = %q %q %q", quality, audio, rating)
	}
	_, audio, _ = sourceMeta("SD", "dub")
	if audio != "DUB" {
		t.Fatalf("dub audio = %q", audio)
	}
}

func TestPrioritizeEmbeds(t *testing.T) {
	embeds := []serverEmbed{
		{url: "https://megaplay.buzz/stream/s-2/1/sub?s=tcdn", label: "t"},
		{url: "https://megaplay.buzz/stream/s-2/2/sub", label: "plain"},
	}
	ordered := prioritizeEmbeds(embeds)
	if ordered[0].label != "plain" || ordered[1].label != "t" {
		t.Fatalf("ordered = %+v", ordered)
	}
}

func TestOriginAndIframe(t *testing.T) {
	if got := originOf("https://megaplay.buzz/stream/s-2/1/sub"); got != "https://megaplay.buzz" {
		t.Fatalf("originOf = %q", got)
	}
	if got := originOf("not a url"); got != megaPlayHome {
		t.Fatalf("originOf fallback = %q", got)
	}
	if got := resolveIframe("//cdn.example/x", "https://megaplay.buzz"); got != "https://cdn.example/x" {
		t.Fatalf("protocol-relative iframe = %q", got)
	}
	if got := resolveIframe("/stream/s-2/1", "https://megaplay.buzz"); got != "https://megaplay.buzz/stream/s-2/1" {
		t.Fatalf("root-relative iframe = %q", got)
	}
	if got := absoluteURL("/watch/x", "https://anikoto.cz"); got != "https://anikoto.cz/watch/x" {
		t.Fatalf("absoluteURL = %q", got)
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

func containsAll(list []string, want ...string) bool {
	for _, w := range want {
		ok := false
		for _, item := range list {
			if item == w {
				ok = true
				break
			}
		}
		if !ok {
			return false
		}
	}
	return true
}
