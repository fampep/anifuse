package aninz

import "testing"

func TestPickShowMatchesTheAnilistID(t *testing.T) {
	payload := `{"items":[
		{"id":"x1","source":"settlar","title":"Naruto","anilistId":123},
		{"id":"x2","source":"anilist","title":"One Piece","anilistId":21}
	]}`
	hit := pickShow(payload, 21, "One Piece")
	if hit == nil || hit.id != "x2" || hit.source != "anilist" {
		t.Fatalf("hit = %+v", hit)
	}
	if hit := pickShow(payload, 0, "naruto"); hit == nil || hit.id != "x1" {
		t.Fatalf("title hit = %+v", hit)
	}
	if pickShow(payload, 0, "Missing") != nil {
		t.Fatal("missing matched")
	}
}

func TestBootstrapAndToken(t *testing.T) {
	boot := bootstrapFrom(`{"effectiveLanguage":"sub","settlarSelection":"sel-1","episode":7,"episodeRouteId":"e-7"}`, "sub")
	if boot == nil || boot.selection != "sel-1" || boot.episode != 7 || boot.routeID != "e-7" {
		t.Fatalf("boot = %+v", boot)
	}
	if bootstrapFrom(`{"effectiveLanguage":"dub","settlarSelection":"s"}`, "sub") != nil {
		t.Fatal("wrong language accepted")
	}
	token := embedToken(`{"embedUrl":"https://embed.settlar.io/api/embed/session?t=abc123"}`)
	if token != "abc123" {
		t.Fatalf("token = %q", token)
	}
	if embedToken(`{"embedUrl":"https://evil.example/?t=abc"}`) != "" {
		t.Fatal("foreign embed accepted")
	}
}

func TestPendingKeepsOnlySettlarMedia(t *testing.T) {
	pend := pendingFrom(`{"kind":"hls","source":"https://media.settlar.io/masters/x.m3u8","subtitles":[{"label":"English","srclang":"en","url":"https://media.settlar.io/subs/en.vtt"}]}`)
	if pend == nil || pend.url != "https://media.settlar.io/masters/x.m3u8" {
		t.Fatalf("pend = %+v", pend)
	}
	if len(pend.subtitles) != 1 || pend.subtitles[0].Language != "en" {
		t.Fatalf("subtitles = %+v", pend.subtitles)
	}
	if pendingFrom(`{"kind":"hls","source":"https://third.example/x.m3u8"}`) != nil {
		t.Fatal("third-party host accepted")
	}
	if pendingFrom(`{"kind":"mp4","source":"https://media.settlar.io/x.mp4"}`) != nil {
		t.Fatal("non-hls accepted")
	}
	if !ownMedia("https://media.settlar.io/a.m3u8") || ownMedia("https://media.settlar.io.evil.example/a.m3u8") {
		t.Fatal("ownMedia wrong")
	}
}

func TestPlayable(t *testing.T) {
	playlist := "#EXTM3U\n#EXT-X-STREAM-INF:BANDWIDTH=1,RESOLUTION=1920x1080\nv1.m3u8\n"
	src := playable(pending{url: "https://media.settlar.io/masters/x.m3u8"}, playlist, false)
	if src == nil || src.Quality != "1080p" || src.Audio != "SUB" || src.Headers["Origin"] != EmbedOrigin {
		t.Fatalf("src = %+v", src)
	}
	if playable(pending{url: "https://media.settlar.io/x"}, "not a playlist", false) != nil {
		t.Fatal("non-playlist accepted")
	}
	if !blocked(403, "") || !blocked(200, "challenge-platform") || blocked(200, "ok") {
		t.Fatal("blocked detection wrong")
	}
}
