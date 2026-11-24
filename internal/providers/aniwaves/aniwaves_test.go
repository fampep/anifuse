package aniwaves

import "testing"

func TestSearchHitsAndShowID(t *testing.T) {
	page := `<a href="/watch/naruto-123/ep-1"><a class="name d-title">Naruto</a></a>
	<a href="/watch/one-piece-21"><a class="name d-title">One Piece</a></a>`
	hits := searchHits(page)
	if len(hits) != 2 {
		t.Fatalf("hits = %d", len(hits))
	}
	if hits[0].id != "123" || hits[0].slug != "naruto-123" || hits[0].title != "Naruto" {
		t.Fatalf("hit0 = %+v", hits[0])
	}
	if showID("/watch/one-piece-21") != "21" || showID("/watch/nonumberslug") != "" {
		t.Fatal("showID wrong")
	}
}

func TestPickShowMatchesWords(t *testing.T) {
	hits := []*show{
		{id: "1", slug: "naruto-shippuden-99", title: "Naruto Shippuden"},
		{id: "2", slug: "naruto-123", title: "Naruto"},
		{id: "3", slug: "other-1", title: "Something Else"},
	}
	if got := pickShow(hits, "Naruto Shippuden"); got == nil || got.id != "1" {
		t.Fatalf("got = %+v", got)
	}
	if got := pickShow(hits, "Naruto"); got == nil || got.id != "2" {
		t.Fatalf("got2 = %+v", got)
	}
	if pickShow(hits, "Missing Show") != nil {
		t.Fatal("missing matched")
	}
}

func TestLinkEmbedAndMedia(t *testing.T) {
	html := `<div data-type="sub"><div data-link-id="l1"></div></div><div data-type="dub"><div data-link-id="l2"></div></div>`
	if got := pickLink(html, false); got != "l1" {
		t.Fatalf("sub link = %q", got)
	}
	if got := pickLink(html, true); got != "l2" {
		t.Fatalf("dub link = %q", got)
	}
	if got := embedURL(`{"result":{"url":"https://streamani.example/embed-3/abc"}}`); got != "https://streamani.example/embed-3/abc" {
		t.Fatalf("embed = %q", got)
	}
	if got := dataID(`<div data-id="xyz123">`); got != "xyz123" {
		t.Fatalf("dataId = %q", got)
	}
	if got := sourcesURL("https://streamani.example/embed-3/abc", "xyz123"); got != "https://streamani.example/embed-3/getSources?id=xyz123" {
		t.Fatalf("sourcesUrl = %q", got)
	}
	payload := `{"result":{"sources":[{"file":"https://cdn.example/master.m3u8","type":"hls"}],"tracks":[{"label":"English","srclang":"en","src":"https://cdn.example/en.vtt","kind":"captions"}]}}`
	if got := mediaURL(payload); got != "https://cdn.example/master.m3u8" {
		t.Fatalf("media = %q", got)
	}
	if got := tracks(payload); len(got) != 1 || got[0].Language != "en" {
		t.Fatalf("tracks = %+v", got)
	}
}

func TestPlayable(t *testing.T) {
	playlist := "#EXTM3U\n#EXT-X-STREAM-INF:BANDWIDTH=1,RESOLUTION=1920x1080\nv1.m3u8\n"
	src := playable("https://cdn.example/master.m3u8", playlist, nil, true)
	if src == nil || src.Quality != "1080p" || src.Audio != "DUB" {
		t.Fatalf("src = %+v", src)
	}
	if playable("https://cdn.example/master.m3u8", "not a playlist", nil, false) != nil {
		t.Fatal("non-playlist accepted")
	}
	if !blocked(403, "") || !blocked(200, "challenge-platform") || blocked(200, "ok") {
		t.Fatal("blocked detection wrong")
	}
}
