package anizone

import "testing"

func TestSearchReadsTheCardTitleAndTheIndexPath(t *testing.T) {
	html := `
            <div x-data="window.getTitle(this.anmTitles, 'One Piece')">
              <a href="https://anizone.to/index.php/anime/siuvpqmq">One Piece</a>
            </div>
            <div x-data="window.getTitle(this.anmTitles, 'One Piece Film Red')">
              <a href="/anime/filmred1">Film</a>
            </div>
            <a href="/anime/siuvpqmq/4">Episode 4</a>
        `
	hits := searchHits(html)
	if len(hits) != 2 || hits[0].slug != "siuvpqmq" || hits[1].slug != "filmred1" {
		t.Fatalf("hits = %+v", hits)
	}
	if got := pickShow(hits, "One Piece"); got == nil || got.slug != "siuvpqmq" {
		t.Fatalf("One Piece hit = %+v", got)
	}
	if got := pickShow(hits, "One Piece Film Red"); got == nil || got.slug != "filmred1" {
		t.Fatalf("Film Red hit = %+v", got)
	}
	if pickShow(hits, "Naruto") != nil {
		t.Fatal("Naruto matched")
	}
}

func TestSearchKeepsATranslatedNameOnTheSameCard(t *testing.T) {
	html := `
            <div x-data="{ anmTitles: JSON.parse('{\"en\":\"Frieren\",\"ja\":\"\\u846c\\u9001\\u306e\\u30d5\\u30ea\\u30fc\\u30ec\\u30f3\"}') }">
              <a href="/anime/fri12345">Frieren</a>
            </div>
        `
	hits := searchHits(html)
	show := pickShow(hits, "葬送のフリーレン")
	if show == nil || show.slug != "fri12345" {
		t.Fatalf("japanese hit = %+v", show)
	}
	if show.title != "Frieren" {
		t.Fatalf("title = %q aliases = %v", show.title, show.aliases)
	}
	if got := pickShow(hits, "Frieren"); got == nil || got.slug != "fri12345" {
		t.Fatalf("english hit = %+v", got)
	}
}

func TestEpisodePageUsesTheMasterAndTheSubtitleTrack(t *testing.T) {
	html := `
            <media-player src="https://files.example/hls/siuvpqmq/1/master.m3u8">
              <track kind="subtitles" label="English" srclang="en" src="https://files.example/sub/en.vtt">
            </media-player>
            <button wire:click="setVideo(10)">Japanese</button>
        `
	list := offers(html)
	sub := choose(list, false)
	if sub == nil || sub.url != "https://files.example/hls/siuvpqmq/1/master.m3u8" || sub.english {
		t.Fatalf("sub = %+v", sub)
	}
	if len(sub.tracks) != 1 || sub.tracks[0].URL != "https://files.example/sub/en.vtt" || sub.tracks[0].Language != "en" {
		t.Fatalf("tracks = %+v", sub.tracks)
	}
	if choose(list, true) != nil {
		t.Fatal("dub offer exists on a sub-only page")
	}
}

func TestEnglishButtonKeepsThePlayerMasterForSub(t *testing.T) {
	html := `
            <media-player src="https://files.example/hls/siuvpqmq/1/master.m3u8">
              <track kind="subtitles" label="English" srclang="en" src="https://files.example/sub/en.vtt">
            </media-player>
            <button wire:click="setVideo(&quot;https://cdn.example/en/master.m3u8&quot;)">English</button>
        `
	list := offers(html)
	sub := choose(list, false)
	if sub == nil || sub.url != "https://files.example/hls/siuvpqmq/1/master.m3u8" || sub.english {
		t.Fatalf("sub = %+v", sub)
	}
	dub := choose(list, true)
	if dub == nil || dub.url != "https://cdn.example/en/master.m3u8" {
		t.Fatalf("dub = %+v", dub)
	}
}

func TestEnglishAudioUsesTheFileTheButtonNames(t *testing.T) {
	html := `
            <button wire:click="setVideo(&quot;https://cdn.example/jp/master.m3u8&quot;)">Japanese</button>
            <button wire:click="setVideo(&quot;https://cdn.example/en/master.m3u8&quot;)">English</button>
            <track kind="captions" label="English" srclang="en" src="https://cdn.example/en.ass">
        `
	list := offers(html)
	sub := choose(list, false)
	if sub == nil || sub.url != "https://cdn.example/jp/master.m3u8" {
		t.Fatalf("sub = %+v", sub)
	}
	dub := choose(list, true)
	if dub == nil || dub.url != "https://cdn.example/en/master.m3u8" || !dub.english {
		t.Fatalf("dub = %+v", dub)
	}
	if len(dub.tracks) != 1 || dub.tracks[0].MimeType != "text/x-ssa" {
		t.Fatalf("tracks = %+v", dub.tracks)
	}
}

func TestPlaylistKeepsTheTallestRungAndTheSiteHeaders(t *testing.T) {
	playlist := `
            #EXTM3U
            #EXT-X-STREAM-INF:BANDWIDTH=800000,RESOLUTION=1280x720,CODECS="avc1.4d401f,mp4a.40.2"
            low.m3u8
            #EXT-X-STREAM-INF:BANDWIDTH=2000000,RESOLUTION=1920x1080,CODECS="avc1.640028,mp4a.40.2"
            high.m3u8
        `
	source := playable("https://files.example/master.m3u8", playlist, false, nil)
	if source == nil {
		t.Fatal("playable returned nil")
	}
	if source.Quality != "1080p" || source.Audio != "AAC · SUB" {
		t.Fatalf("source = %+v", source)
	}
	if source.Headers["Origin"] != Origin || source.Headers["Referer"] != Origin+"/" || source.Headers["User-Agent"] != UserAgent {
		t.Fatalf("headers = %+v", source.Headers)
	}
	if playable("http://files.example/master.m3u8", playlist, false, nil) != nil {
		t.Fatal("plain-http URL accepted")
	}
	if playable("https://files.example/master.m3u8", "not a playlist", false, nil) != nil {
		t.Fatal("non-playlist accepted")
	}
}

func TestPathsStayOnTheShowTheSearchFound(t *testing.T) {
	got := searchURLs("One Piece")
	want := []string{
		"https://anizone.to/anime?search=One%20Piece",
		"https://anizone.to/index.php/anime?search=One%20Piece",
	}
	if len(got) != 2 || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("search urls = %v", got)
	}
	got = episodeURLs("siuvpqmq", 3)
	want = []string{
		"https://anizone.to/anime/siuvpqmq/3",
		"https://anizone.to/index.php/anime/siuvpqmq/3",
	}
	if len(got) != 2 || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("episode urls = %v", got)
	}
	queries := queries("Frieren: Beyond Journey's End")
	if len(queries) != 2 || queries[0] != "Frieren: Beyond Journey's End" || queries[1] != "Frieren" {
		t.Fatalf("queries = %v", queries)
	}
}

func TestAChallengePageIsBlockedWithoutTheStatusCode(t *testing.T) {
	if !blocked(403, "") {
		t.Fatal("403 not blocked")
	}
	if !blocked(200, "<title>Just a moment...</title> challenge-platform") {
		t.Fatal("challenge page not blocked")
	}
	if blocked(200, `<media-player src="https://files.example/master.m3u8">`) {
		t.Fatal("normal page blocked")
	}
	if !blocked(200, "you have been blocked") {
		t.Fatal("blocked message missed")
	}
}

func TestNormalizeAndLabels(t *testing.T) {
	if got := normalize("Attack on Titan!!"); got != "attack on titan" {
		t.Fatalf("normalize = %q", got)
	}
	if got := normalize("HUNTER×HUNTER"); got != "hunter hunter" {
		t.Fatalf("normalize = %q", got)
	}
	if !englishLabel("English Dub") || englishLabel("Japanese") || englishLabel("English Subtitles") {
		t.Fatal("englishLabel wrong")
	}
	if !japaneseLabel("Japanese") || !japaneseLabel("Sub") || japaneseLabel("English") {
		t.Fatal("japaneseLabel wrong")
	}
	if !captionLabel("English Subtitles") || !captionLabel("Captions") || captionLabel("Japanese") {
		t.Fatal("captionLabel wrong")
	}
}

func TestCleanTitleDropsAniKittyHints(t *testing.T) {
	if got := cleanTitle("Attack on Titan||mal:16498"); got != "Attack on Titan" {
		t.Fatalf("cleanTitle = %q", got)
	}
}

func TestSlugAndMediaURL(t *testing.T) {
	if !slugOK("siuvpqmq") || slugOK("bad-slug") {
		t.Fatal("slugOK wrong")
	}
	if !mediaURL("https://files.example/master.m3u8") || mediaURL("http://x.example/a") || mediaURL("https://x.example/a b.m3u8") {
		t.Fatal("mediaURL wrong")
	}
}

// The live site embeds search results as Alpine state; the parsers must
// read them from there, not from server-rendered links.

func TestSearchReadsEmbeddedItems(t *testing.T) {
	html := `
        <div wire:id="abc" x-data="{
            items: JSON.parse('[{\u0022slug\u0022:\u0022uyyyn4kf\u0022,\u0022url\u0022:\u0022http:\\\/\\\/anizone.to\\\/anime\\\/uyyyn4kf\u0022,\u0022main_title\u0022:\u0022One Piece\u0022,\u0022title_list\u0022:{\u00221\u0022:\u0022One Piece\u0022,\u002238\u0022:\u0022\\u6d77\\u8cca\\u738b\u0022}},{\u0022slug\u0022:\u0022not a slug\u0022,\u0022main_title\u0022:\u0022Bad\u0022,\u0022title_list\u0022:{}}]')
        }">
        </div>`
	hits := searchHits(html)
	if len(hits) != 1 || hits[0].slug != "uyyyn4kf" || hits[0].title != "One Piece" {
		t.Fatalf("hits = %+v", hits)
	}
	if len(hits[0].aliases) == 0 {
		t.Fatal("title_list translations lost")
	}
	if got := pickShow(hits, "海賊王"); got == nil || got.slug != "uyyyn4kf" {
		t.Fatalf("translated title not matched: %+v", got)
	}
	if got := pickShow(hits, "One Piece"); got == nil || got.slug != "uyyyn4kf" {
		t.Fatalf("main title not matched: %+v", got)
	}
}

func TestEpisodePageReadsTheVidstackPlayer(t *testing.T) {
	html := `
        <div x-data="vidstackPlayer(JSON.parse('{\u0022src\u0022:\u0022https:\\\/\\\/seiryuu.vid-cdn.xyz\\\/a83ec4ac\\\/master.m3u8\u0022,\u0022subtitles\u0022:[{\u0022title\u0022:\u0022English - Signs\\\/Songs\u0022,\u0022format\u0022:\u0022ass\u0022,\u0022language\u0022:\u0022en\u0022,\u0022file\u0022:\u0022https:\\\/\\\/seiryuu.vid-cdn.xyz\\\/a83ec4ac\\\/subtitles\\\/2_en.ass\u0022}]}'))">
        </div>`
	list := offers(html)
	if len(list) != 1 {
		t.Fatalf("offers = %+v", list)
	}
	sub := choose(list, false)
	if sub == nil || sub.english || sub.url != "https://seiryuu.vid-cdn.xyz/a83ec4ac/master.m3u8" {
		t.Fatalf("sub = %+v", sub)
	}
	if len(sub.tracks) != 1 || sub.tracks[0].URL != "https://seiryuu.vid-cdn.xyz/a83ec4ac/subtitles/2_en.ass" ||
		sub.tracks[0].MimeType != "text/x-ssa" || sub.tracks[0].Language != "en" {
		t.Fatalf("tracks = %+v", sub.tracks)
	}
	// The embedded master is Japanese; nothing serves a dub request.
	if choose(list, true) != nil {
		t.Fatal("dub offer exists on the embedded player")
	}
}

func TestVidstackOffersMergeWithLegacyButtons(t *testing.T) {
	html := `
        <div x-data="vidstackPlayer(JSON.parse('{\u0022src\u0022:\u0022https:\\\/\\\/files.example\\\/sub.m3u8\u0022}'))"></div>
        <button wire:click="setVideo(&quot;https://cdn.example/en.m3u8&quot;)">English</button>`
	list := offers(html)
	sub := choose(list, false)
	dub := choose(list, true)
	if sub == nil || sub.url != "https://files.example/sub.m3u8" {
		t.Fatalf("sub = %+v", sub)
	}
	if dub == nil || dub.url != "https://cdn.example/en.m3u8" {
		t.Fatalf("dub = %+v", dub)
	}
}
