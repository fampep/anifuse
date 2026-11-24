package gogoanime

import "testing"

func TestQueriesDropNoiseWords(t *testing.T) {
	got := queries("Attack on Titan Final Season")
	if len(got) < 2 || got[0] != "Attack on Titan Final Season" {
		t.Fatalf("queries = %v", got)
	}
	if got := queries("The And"); len(got) != 1 {
		t.Fatalf("noise queries = %v", got)
	}
}

func TestSearchHitsAndScore(t *testing.T) {
	page := `<div class="listupd"><a href="https://gogoanime.by/series/naruto-shippuden/" title="Naruto Shippuden"></a>
	<a href="https://gogoanime.by/series/naruto/" title="Naruto (2002)"></a></div>`
	hits := searchHits(page)
	if len(hits) != 2 {
		t.Fatalf("hits = %d", len(hits))
	}
	if hits[0].url != "https://gogoanime.by/series/naruto-shippuden/" {
		t.Fatalf("hit0 = %+v", hits[0])
	}
	if got := pickShow(hits, "Naruto Shippuden", false); got == nil || got.url != "https://gogoanime.by/series/naruto-shippuden/" {
		t.Fatalf("pick = %+v", got)
	}
	if got := pickShow(hits, "Naruto Shippuden", true); got != nil {
		t.Fatal("dub pick accepted a non-dubbed hit")
	}
}

func TestEpisodesAndPlayers(t *testing.T) {
	page := `<div data-episode-number="2" href="https://gogoanime.by/naruto-episode-2-english-subbed">
	<div data-episode-number="3" href="https://gogoanime.by/naruto-episode-3-english-dubbed">`
	rows := episodes(page)
	if len(rows) != 2 || rows[0].dub || !rows[1].dub {
		t.Fatalf("rows = %+v", rows)
	}
	playersPage := `<div class="type" data-type="sub">
	<li class="player-type-link" data-src="https://gogoanime.by/player/2?source=embed">MegaVid</li>
	<li class="player-type-link" data-src="https://gogoanime.by/player/1">MegaPlay</li></div>`
	got := players(playersPage, false)
	if len(got) != 2 || !stringsHas(got[0].url, "source=embed") {
		t.Fatalf("players = %+v", got)
	}
	if len(players(playersPage, true)) != 0 {
		t.Fatal("dub players found in sub section")
	}
}

func stringsHas(s, sub string) bool {
	return len(s) >= len(sub) && (s == sub || len(sub) == 0 || (func() bool {
		for i := 0; i+len(sub) <= len(s); i++ {
			if s[i:i+len(sub)] == sub {
				return true
			}
		}
		return false
	})())
}

func TestFrameAndFiles(t *testing.T) {
	page := `<iframe src="https://megavid.buzz/embed-123"></iframe><iframe src="https://evil.example/x"></iframe>`
	if got := frameURL(page); got != "https://megavid.buzz/embed-123" {
		t.Fatalf("frame = %q", got)
	}
	if got := megaPlayFile(`file: "https://megaplay.su/hls/master.m3u8?t=1"`); got != "https://megaplay.su/hls/master.m3u8?t=1" {
		t.Fatalf("file = %q", got)
	}
	if got := megaPlayFile(`file: "https://evil.example/x.m3u8"`); got != "" {
		t.Fatalf("foreign file = %q", got)
	}
	payloadPage := `<script id="player-payload">{"status":"ok","sourceUrl":"/masters/x.m3u8"}</script>`
	if got := megaVidPath(payloadPage); got != "/masters/x.m3u8" {
		t.Fatalf("path = %q", got)
	}
	pending := megaVid(`{"status":"ok","source":"https://megavid.se/masters/x.m3u8","tracks":[{"file":"https://megavid.se/subs/en.vtt","label":"English"}]}`)
	if pending == nil || len(pending.tracks) != 1 {
		t.Fatalf("pending = %+v", pending)
	}
	if megaVid(`{"status":"ok","source":"https://evil.example/x.m3u8"}`) != nil {
		t.Fatal("foreign media accepted")
	}
}

func TestPlayableAndUnescape(t *testing.T) {
	playlist := "#EXTM3U\n#EXT-X-STREAM-INF:BANDWIDTH=1,RESOLUTION=1920x1080\nv1.m3u8\n"
	src := playable("https://megavid.buzz/masters/x.m3u8", playlist, "HD", false, MegaVidOrigin, nil)
	if src == nil || src.Quality != "1080p" || src.QualityRating != "HD" || src.Audio != "SUB" {
		t.Fatalf("src = %+v", src)
	}
	if playable("https://megavid.buzz/x.m3u8", "not a playlist", "HD", false, MegaVidOrigin, nil) != nil {
		t.Fatal("non-playlist accepted")
	}
	if got := unescape("&#x2F;a&#38;b&amp;c"); got != "/a&b&c" {
		t.Fatalf("unescape = %q", got)
	}
	if normalize("  Naruto: Shippūden!! 2nd ") != "naruto shipp den 2" {
		t.Fatalf("normalize = %q", normalize("  Naruto: Shippūden!! 2nd "))
	}
	if !blocked(403, "") || !blocked(200, "challenge-platform") || blocked(200, "ok") {
		t.Fatal("blocked detection wrong")
	}
}
