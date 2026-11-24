package anigato

import (
	"testing"
)

func TestPlaylistURLStaysOnTheSiteStreamPath(t *testing.T) {
	ok := func(raw, want string) {
		got, valid := playlistURL(Origin, raw)
		if !valid || got != want {
			t.Fatalf("playlistURL(%q) = %q,%v want %q,true", raw, got, valid, want)
		}
	}
	bad := func(raw string) {
		if _, valid := playlistURL(Origin, raw); valid {
			t.Fatalf("playlistURL(%q) accepted", raw)
		}
	}
	ok("/api/stream/proxy?url=token", Origin+"/api/stream/proxy?url=token")
	ok(Origin+"/api/stream/main.m3u8?url=token", Origin+"/api/stream/main.m3u8?url=token")
	bad("https://anigato.lol/20/1/sub/master.m3u8")
	bad("https://evil.example/api/stream/proxy?url=1")
	bad("http://anigato.lol/api/stream/proxy?url=1")
	bad("https://user:pass@anigato.lol/api/stream/proxy?url=1")
	bad("/api/stream/../watch/1")
}

func TestStreamsFollowTheRequestedLanguage(t *testing.T) {
	payload := apiWatch{
		Success: true,
		Streams: []stream{
			{Type: "sub", URL: "/api/stream/sub.m3u8"},
			{Type: "dub", URL: "/api/stream/dub.m3u8"},
			{Type: "", URL: "/api/stream/any.m3u8"},
		},
	}
	if got := len(payload.streamsFor(false)); got != 2 {
		t.Fatalf("sub streams = %d", got)
	}
	if got := payload.streamsFor(true); len(got) != 2 || got[0].URL != "/api/stream/dub.m3u8" || got[1].URL != "/api/stream/any.m3u8" {
		t.Fatalf("dub streams = %+v", got)
	}
	if !payload.StillLoading() && !(&apiWatch{Success: true, LoadingMore: true}).StillLoading() {
		t.Fatal("loading flags not detected")
	}
}

func TestSubtitlesAreAbsoluteAndLabelled(t *testing.T) {
	tracks := subtitles(Origin, []subtitle{
		{URL: "/api/sub.vtt?url=caption", Label: "English", Lang: "en"},
		{URL: "/api/sub.vtt?url=caption", Label: "English", Lang: "en"},
		{URL: "https://evil.example/sub.vtt", Label: "Bad"},
	})
	if len(tracks) != 1 {
		t.Fatalf("tracks = %d", len(tracks))
	}
	if tracks[0].URL != Origin+"/api/sub.vtt?url=caption" || tracks[0].Language != "en" {
		t.Fatalf("track = %+v", tracks[0])
	}
}

func TestHeightLabel(t *testing.T) {
	if heightLabel(1080) != "1080p" || heightLabel(2160) != "4K" || heightLabel(240) != "" {
		t.Fatal("height label mapping wrong")
	}
	if got := heightRe.FindStringSubmatch("RESOLUTION=1920x1080"); got == nil || got[1] != "1080" {
		t.Fatal("height regex wrong")
	}
}
