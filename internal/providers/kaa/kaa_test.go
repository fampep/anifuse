package kaa

import (
	"strings"
	"testing"
)

func TestPickShowPrefersTheTvMatch(t *testing.T) {
	payload := `{"result":[
		{"slug":"naruto","title_en":"Naruto","type":"tv","locales":["ja-JP","en-US"]},
		{"slug":"naruto-movie","title_en":"Naruto Movie","type":"movie","locales":["ja-JP"]},
		{"slug":"other","title_en":"Something Else","type":"tv","locales":["ja-JP"]}
	]}`
	hit := pickShow(payload, "Naruto")
	if hit == nil || hit.slug != "naruto" {
		t.Fatalf("hit = %+v", hit)
	}
	if hit.kind != "tv" || len(hit.locales) != 2 {
		t.Fatalf("hit meta = %+v", hit)
	}
	if pickShow(payload, "Missing Show") != nil {
		t.Fatal("missing show matched")
	}
}

func TestEpisodePathKeepsTheEpisode(t *testing.T) {
	payload := `{"result":[
		{"episode_number":2,"id":"ep-2-something"},
		{"episode":3,"slug":"ep-3-other"},
		{"episode_number":5,"slug":"five"}
	]}`
	if got := episodePath(payload, 2); got != "ep-2-something" {
		t.Fatalf("ep2 = %q", got)
	}
	if got := episodePath(payload, 3); got != "ep-3-other" {
		t.Fatalf("ep3 = %q", got)
	}
	if got := episodePath(payload, 5); got != "ep-5-five" {
		t.Fatalf("ep5 = %q", got)
	}
	if got := episodePath(payload, 9); got != "" {
		t.Fatalf("ep9 = %q", got)
	}
}

func TestVidStreamingServerAndMaster(t *testing.T) {
	payload := `{"servers":[
		{"name":"Streamtape","src":"https://streamtape.example/embed/1","id":"/x"},
		{"name":"VidStreaming","src":"https://krussdomi.com/player?id=abc123&ep=vidstream","id":"abc123"},
		{"name":"Other","src":"/player?id=fallback"}
	]}`
	if got := vidStreamingURL(payload); got != "https://krussdomi.com/player?id=abc123&ep=vidstream" {
		t.Fatalf("player = %q", got)
	}
	if got := playerID("https://krussdomi.com/player?id=abc123&ep=vidstream"); got != "abc123" {
		t.Fatalf("id = %q", got)
	}
	if got := masterURL("abc123"); got != "https://hls.krussdomi.com/manifest/abc123/master.m3u8" {
		t.Fatalf("master = %q", got)
	}
	bare := `{"servers":[{"name":"vidstream","id":"xyz789"}]}`
	if got := vidStreamingURL(bare); got != "https://krussdomi.com/player?id=xyz789&ep=vidstream" {
		t.Fatalf("bare = %q", got)
	}
	fallback := `{"servers":[{"name":"Meh","src":"https://krussdomi.com/player?id=f1"}]}`
	if got := vidStreamingURL(fallback); got != "https://krussdomi.com/player?id=f1" {
		t.Fatalf("fallback = %q", got)
	}
}

func TestBestVariantAndSubtitles(t *testing.T) {
	playlist := "#EXTM3U\n" +
		"#EXT-X-STREAM-INF:BANDWIDTH=1,RESOLUTION=640x360\nv1.m3u8\n" +
		"#EXT-X-STREAM-INF:BANDWIDTH=2,RESOLUTION=1920x1080\nv2.m3u8\n"
	best := bestVariant(playlist)
	if !strings.Contains(best, "1920x1080") {
		t.Fatalf("best = %q", best)
	}
	html := `{"language":[0,"en"],"name":[0,"English"],"src":[0,"https://cdn.example/ep1.vtt"]}` +
		`{"language":[0,"es"],"name":[0,"Español"],"src":[0,"https://cdn.example/ep1-es.vtt"]}`
	tracks := subtitles(html)
	if len(tracks) != 2 {
		t.Fatalf("tracks = %d", len(tracks))
	}
	if tracks[0].Language != "en" || tracks[0].URL != "https://cdn.example/ep1.vtt" {
		t.Fatalf("track0 = %+v", tracks[0])
	}
}

func TestBlockedAndTitles(t *testing.T) {
	if !blocked(403, "") || !blocked(200, "challenge-platform") || blocked(200, "<html>ok</html>") {
		t.Fatal("blocked detection wrong")
	}
	if normalizeTitle("  Attack on Titan: Final Season  ") != "attack on titan final season" {
		t.Fatal("normalize wrong")
	}
}
