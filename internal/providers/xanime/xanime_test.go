package xanime

import "testing"

func TestSealOpenRoundTrip(t *testing.T) {
	sealed, err := seal(`{"query":"q","variables":{}}`)
	if err != nil {
		t.Fatalf("seal: %v", err)
	}
	opened, err := open(sealed)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if opened != `{"query":"q","variables":{}}` {
		t.Fatalf("opened = %q", opened)
	}
	if passthrough, err := open(`{"data":{"ok":true}}`); err != nil || passthrough != `{"data":{"ok":true}}` {
		t.Fatalf("passthrough = %q %v", passthrough, err)
	}
	if _, err := open("not json"); err == nil {
		t.Fatal("bad body accepted")
	}
}

func TestPickShowMatchesTheAnilistID(t *testing.T) {
	payload := `{"data":{"get_q27":{"items":[
		{"id":"x1","data":{"ani_id":"a1","info_title":"Naruto","al_id":"123"}},
		{"id":"x2","data":{"ani_id":"a2","info_title":"One Piece","al_id":21}}
	]}}}`
	hit := pickShow(payload, 21, "One Piece")
	if hit == nil || hit.id != "a2" {
		t.Fatalf("hit = %+v", hit)
	}
	if hit := pickShow(payload, 0, "naruto"); hit == nil || hit.id != "a1" {
		t.Fatalf("title hit = %+v", hit)
	}
	if pickShow(payload, 0, "Missing") != nil {
		t.Fatal("missing matched")
	}
}

func TestEpisodeSkipsSubIndexParts(t *testing.T) {
	payload := `{"data":{"get_q01":{"paging":{"pages":1,"page":1},"items":[
		{"data":{"ep_id":"e1","ep_index":1,"ep_sub_index":0,"sourcesNode_list":[{"data":{"src_type":"sub"}}]}},
		{"data":{"ep_id":"e2","ep_index":1,"ep_sub_index":1,"sourcesNode_list":[{"data":{"src_type":"dub"}}]}}
	]}}}`
	if episodePages(payload) != 1 {
		t.Fatal("pages wrong")
	}
	hit := episodeRow(payload, 1)
	if hit == nil || hit.id != "e1" || !hit.hasSub || hit.hasDub {
		t.Fatalf("hit = %+v", hit)
	}
	if episodeRow(payload, 2) != nil {
		t.Fatal("missing episode matched")
	}
}

func TestLinksAndSubtitles(t *testing.T) {
	payload := `{"data":{"get_q07":{"data":{"sourcesNode_list":[
		{"data":{"src_type":"sub","souPath":"/masters/a-index-1.ico","track":[{"label":"English","kind":"captions","trackPath":"/subs/en.vtt"}]}},
		{"data":{"src_type":"dub","souPath":"/masters/dub.ico"}},
		{"data":{"src_type":"sub","m3u8_lists":[{"iframe":"https://player.example/e/2"}]}}
	]}}}}`
	got := links(payload, false, "https://xanime.app", "ua")
	if len(got) != 2 {
		t.Fatalf("links = %d", len(got))
	}
	if got[0].url != "https://xanime.app/masters/a-index-1.ico" {
		t.Fatalf("url0 = %q", got[0].url)
	}
	if got[0].headers["Referer"] != "https://xanime.app/" || got[0].headers["User-Agent"] != "ua" {
		t.Fatalf("headers = %+v", got[0].headers)
	}
	if len(got[0].subtitles) != 1 || got[0].subtitles[0].URL != "https://xanime.app/subs/en.vtt" {
		t.Fatalf("subtitles = %+v", got[0].subtitles)
	}
	if got[1].url != "https://player.example/e/2" {
		t.Fatalf("url1 = %q", got[1].url)
	}
	if links(payload, true, "https://xanime.app", "ua")[0].url != "https://xanime.app/masters/dub.ico" {
		t.Fatal("dub pick wrong")
	}
}

func TestPlayable(t *testing.T) {
	playlist := "#EXTM3U\n#EXT-X-STREAM-INF:BANDWIDTH=1,RESOLUTION=1280x720\nv1.m3u8\n"
	src := playable(mediaLink{url: "https://xanime.app/masters/a.ico", headers: map[string]string{"Referer": "https://xanime.app/"}}, playlist, true)
	if src == nil || src.Quality != "720p" || src.Audio != "DUB" || src.Label != "720p" {
		t.Fatalf("src = %+v", src)
	}
	if playable(mediaLink{url: "https://xanime.app/x"}, "not a playlist", false) != nil {
		t.Fatal("non-playlist accepted")
	}
	if !blocked(403, "") || !blocked(200, "challenge-platform") || blocked(200, "ok") {
		t.Fatal("blocked detection wrong")
	}
	if normalizeTitle("  One Piece: Wano!! ") != "one piece wano" {
		t.Fatal("normalize wrong")
	}
}
