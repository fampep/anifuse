package animegg

import (
	"testing"
)

func TestQueriesBuildSearchTerms(t *testing.T) {
	got := queries("One Piece || Naruto")
	if len(got) == 0 || got[0] != "One Piece" || got[1] != "Naruto" {
		t.Fatalf("queries = %v", got)
	}
	if got := queries(""); len(got) != 0 {
		t.Fatalf("empty queries = %v", got)
	}
}

func TestSearchHitsKeepSlugAndTitle(t *testing.T) {
	page := `<a class="mse" href="https://www.animegg.org/series/naruto" title="x"><h2>Naruto</h2>Alt Titles : Naruto Shippuden</a>
	<a class="mse" href="/series/one-piece"><h2>One Piece</h2></a>
	<a class="mse" href="/search/other"><h2>Not A Show</h2></a>`
	hits := searchHits(page)
	if len(hits) != 2 {
		t.Fatalf("hits = %d", len(hits))
	}
	if hits[0].slug != "naruto" || hits[0].title != "Naruto" || len(hits[0].aliases) != 1 || hits[0].aliases[0] != "Naruto Shippuden" {
		t.Fatalf("hit = %+v", hits[0])
	}
	if pickShow(hits, "naruto shippuden") == nil || pickShow(hits, "nope") != nil {
		t.Fatal("alias pick failed")
	}
}

func TestEpisodesKeepNumbersAndLanguageMarkers(t *testing.T) {
	page := `<ul class="newmanga">
	<li><a href="/naruto-episode-1" class="anm_det_pop">Ep 1</a><div class="btn-subbed"></div></li>
	<li><a href="/naruto-episode-2" class="anm_det_pop">Ep 2</a><div class="btn-dubbed"></div></li>
	<li><a href="/naruto-episode-0" class="anm_det_pop">Bad</a></li>
	</ul>`
	rows := episodes(page)
	if len(rows) != 2 {
		t.Fatalf("rows = %d", len(rows))
	}
	if rows[0].number != 1 || !rows[0].sub || rows[0].dub {
		t.Fatalf("row0 = %+v", rows[0])
	}
	if rows[1].number != 2 || rows[1].sub || !rows[1].dub {
		t.Fatalf("row1 = %+v", rows[1])
	}
}

func TestMirrorsChooseTheRequestedLanguage(t *testing.T) {
	page := `<div id="subbed-Animegg" class="m"><iframe src="/embed/11"></iframe></div>
	<div id="dubbed-Other" class="m"><iframe src="/embed/22"></iframe></div>`
	list := mirrors(page)
	if len(list) != 2 {
		t.Fatalf("mirrors = %d", len(list))
	}
	if got := chooseMirror(list, true); got == nil || got.embedPath != "/embed/22" {
		t.Fatalf("dub mirror = %+v", got)
	}
	if got := chooseMirror(list, false); got == nil || got.embedPath != "/embed/11" {
		t.Fatalf("sub mirror = %+v", got)
	}
}

func TestFilesKeepOnlyPlayPaths(t *testing.T) {
	page := `file: "/play/1/video.mp4", label: "360p" 
	file: "https://www.animegg.org/play/2/video.mp4", label: "1080p"
	file: "https://evil.example/play/3/video.mp4", label: "4K"`
	files := mediaFiles(page)
	if len(files) != 2 {
		t.Fatalf("files = %d", len(files))
	}
	if files[0].url != Origin+"/play/1/video.mp4" || files[1].url != Origin+"/play/2/video.mp4" {
		t.Fatalf("files = %+v", files)
	}
	if height("1080p") != 1080 || height("360p") != 360 || heightLabel("1080p") != "1080p" {
		t.Fatal("height parsing wrong")
	}
	if gradeToken("1080p HD") != "HD" || gradeToken("360p") != "" {
		t.Fatal("grade token wrong")
	}
}

func TestEpisodeAndEmbedValidation(t *testing.T) {
	if got := episodeURL("/naruto-episode-12?x=1"); got != Origin+"/naruto-episode-12" {
		t.Fatalf("episodeURL = %q", got)
	}
	if got := episodeURL("/bad-episode-0"); got != "" {
		t.Fatalf("episodeURL(bad) = %q", got)
	}
	if got := embedPath("//www.animegg.org/embed/42?x=1"); got != "/embed/42" {
		t.Fatalf("embedPath = %q", got)
	}
	if got := embedPath("https://evil.example/embed/42"); got != "" {
		t.Fatalf("embedPath(evil) = %q", got)
	}
	if !blocked(403, "") || !blocked(200, "challenge-platform") || blocked(200, "<html>ok</html>") {
		t.Fatal("blocked detection wrong")
	}
	if normalize("  One Piece's!! ") != "one pieces" {
		t.Fatal("normalize wrong")
	}
}
