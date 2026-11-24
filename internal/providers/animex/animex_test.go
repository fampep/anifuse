package animex

import "testing"

func TestPickShowMatchesTheAnilistID(t *testing.T) {
	payload := `{"data":{"searchAnime":{"items":[
		{"id":"a1","anilistId":123,"titleRomaji":"Naruto","titleEnglish":"Naruto"},
		{"id":"a2","anilistId":21,"titleRomaji":"One Piece","titleEnglish":"One Piece"}
	]}}}`
	hit := pickShow(payload, 21, "One Piece")
	if hit == nil || hit.id != "a2" || hit.anilistID != 21 {
		t.Fatalf("hit = %+v", hit)
	}
	if hit := pickShow(payload, 0, "naruto"); hit == nil || hit.id != "a1" {
		t.Fatalf("title hit = %+v", hit)
	}
	if pickShow(payload, 0, "Missing") != nil {
		t.Fatal("missing matched")
	}
}

func TestEpisodeAndProviders(t *testing.T) {
	if row := episode(`[{"number":1,"hasSub":true,"hasDub":false},{"number":2,"hasSub":true,"hasDub":true}]`, 1); row == nil || row.hasDub {
		t.Fatalf("row1 = %+v", row)
	}
	if row := episode(`[{"number":2}]`, 2); row == nil || !row.hasSub || !row.hasDub {
		t.Fatalf("row2 = %+v", row)
	}
	if episode(`[{"number":2}]`, 3) != nil {
		t.Fatal("missing episode matched")
	}
	ids := providerIDs(`{"subProviders":[{"id":"slow"},{"id":"main","default":true}]}`, false)
	if len(ids) != 2 || ids[0] != "main" || ids[1] != "slow" {
		t.Fatalf("ids = %v", ids)
	}
}

func TestLinkKeepsTheServerHeaders(t *testing.T) {
	payload := `{"sources":[{"url":"https://cdn.example/deeznuts.txt"}],
		"headers":{"Referer":"https://player.example/","Origin":"https://player.example","X-Evil":"drop"},
		"tracks":[{"url":"https://cdn.example/en.vtt","label":"English","lang":"en","kind":"captions"}]}`
	got := link(payload, "test-ua")
	if got == nil {
		t.Fatal("link nil")
	}
	if got.url != "https://cdn.example/deeznuts.txt" {
		t.Fatalf("url = %q", got.url)
	}
	if got.headers["Referer"] != "https://player.example/" || got.headers["Origin"] != "https://player.example" {
		t.Fatalf("headers = %+v", got.headers)
	}
	if _, ok := got.headers["X-Evil"]; ok {
		t.Fatal("non-whitelisted header leaked")
	}
	if got.headers["User-Agent"] != "test-ua" {
		t.Fatalf("ua = %q", got.headers["User-Agent"])
	}
	if len(got.subtitles) != 1 || got.subtitles[0].Language != "en" {
		t.Fatalf("subtitles = %+v", got.subtitles)
	}
}

func TestPlayableAndVariant(t *testing.T) {
	playlist := "#EXTM3U\n#EXT-X-STREAM-INF:BANDWIDTH=1,RESOLUTION=1920x1080\nv1.m3u8\n"
	src := playable(mediaLink{url: "https://cdn.example/deeznuts.txt", headers: map[string]string{"Referer": "https://player.example/"}}, playlist, false)
	if src == nil || src.URL != "https://cdn.example/deeznuts.txt" || src.Quality != "1080p" || src.Audio != "SUB" {
		t.Fatalf("src = %+v", src)
	}
	if playable(mediaLink{url: "https://cdn.example/x"}, "not a playlist", false) != nil {
		t.Fatal("non-playlist accepted")
	}
	if !blocked(403, "") || !blocked(200, "challenge-platform") || blocked(200, "ok") {
		t.Fatal("blocked detection wrong")
	}
	if normalizeTitle("  Naruto Shippuden!! ") != "naruto shippuden" {
		t.Fatal("normalize wrong")
	}
}
