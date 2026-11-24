package anihosts

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/fampep/anifuse/internal/httpx"
	"github.com/fampep/anifuse/internal/protocol"
	"github.com/fampep/anifuse/internal/resolve"
)

func TestEmbedURLUsesTheAnilistID(t *testing.T) {
	if got := embedURLFor(Origin, 16498, 1); got != "https://anihosts.xyz/p/home/embed/anilist/16498/1" {
		t.Fatalf("embedURL = %q", got)
	}
	if got := embedURLFor(Origin, 0, 1); got != "" {
		t.Fatalf("embedURL(0,1) = %q", got)
	}
	if got := embedURLFor(Origin, 16498, 0); got != "" {
		t.Fatalf("embedURL(16498,0) = %q", got)
	}
}

func TestStateKeepsStatusAndEpisodes(t *testing.T) {
	page := `<script type="application/json" id="state-data">{"nav":false,"requested":{"type":"anilist","id":"16498"},"episode":1,"episodes":[1,2,3,0],"status":"ready","src":"https://uqload.vc/embed-x.html","source":"uqload"}</script>`
	got, err := state(page)
	if err != nil {
		t.Fatalf("state: %v", err)
	}
	if got.Status != "ready" || got.Src != "https://uqload.vc/embed-x.html" {
		t.Fatalf("state = %+v", got)
	}
	if got.Episodes[0] != 1 || got.Episodes[2] != 3 {
		t.Fatalf("episodes = %v", got.Episodes)
	}
	if _, err := state("<html>no state</html>"); err == nil {
		t.Fatal("expected missing state error")
	}
}

func TestPackedConfigUnwrapsToTheMaster(t *testing.T) {
	config := `jwplayer("v").setup({sources:[{file:"https://strm.example/hls2/master.m3u8?t=TOKEN&s=EXP"}],image:"https://strm.example/poster.jpg",tracks:[{file:"https://strm.example/vtt/ep_eng.vtt",label:"English",kind:"captions","default":true},{file:"https://strm.example/vtt/ep_sli.vtt",kind:"thumbnails"},{file:"/srt/empty.srt",label:"Upload captions",kind:"captions"}]})`
	unpacked, err := unpackConfig(pack(config))
	if err != nil {
		t.Fatalf("unpack: %v", err)
	}
	if unpacked != config {
		t.Fatalf("unpacked = %q, want %q", unpacked, config)
	}
	if got := fileFromConfig(unpacked); got != "https://strm.example/hls2/master.m3u8?t=TOKEN&s=EXP" {
		t.Fatalf("file = %q", got)
	}
}

func TestFileAndCaptionsAreScopedToTheirArrays(t *testing.T) {
	config := `jwplayer("v").setup({sources:[{file:"https://strm.example/hls2/master.m3u8"}],image:"https://strm.example/poster.jpg",tracks:[{file:"https://strm.example/vtt/ep_eng.vtt",label:"English",kind:"captions"},{file:"https://strm.example/vtt/ep_sli.vtt",kind:"thumbnails"},{file:"/srt/empty.srt",label:"Upload captions",kind:"captions"}]})`
	if got := fileFromConfig(config); got != "https://strm.example/hls2/master.m3u8" {
		t.Fatalf("file = %q", got)
	}
	captions := captionsFromConfig(config)
	if len(captions) != 1 {
		t.Fatalf("captions = %d, want 1", len(captions))
	}
	if captions[0].URL != "https://strm.example/vtt/ep_eng.vtt" ||
		captions[0].Language != "en" ||
		captions[0].MimeType != "text/vtt" {
		t.Fatalf("caption = %+v", captions[0])
	}
}

func TestMediaURLStaysHTTPSAndPlayable(t *testing.T) {
	cases := map[string]bool{
		"https://strm.example/hls2/master.m3u8?t=1&s=2": true,
		"https://strm.example/file.mp4":                 true,
		"http://strm.example/master.m3u8":               false,
		"https://user:pass@strm.example/master.m3u8":    false,
		"https://strm.example/../master.m3u8":           false,
		`https://strm.example/master.m3u8\x`:            false,
		"/relative/master.m3u8":                         false,
	}
	for raw, want := range cases {
		if _, got := mediaURL(raw); got != want {
			t.Fatalf("mediaURL(%q) = %v, want %v", raw, got, want)
		}
	}
}

func TestPlayableCarriesTheEmbedHeaders(t *testing.T) {
	source, err := playable(
		"https://strm9.example/hls2/master.m3u8?t=1",
		"https://uqload.vc/embed-x.html",
		nil,
		false,
		"test-ua",
	)
	if err != nil {
		t.Fatalf("playable: %v", err)
	}
	if source.Headers["Referer"] != "https://uqload.vc/embed-x.html" {
		t.Fatalf("referer = %q", source.Headers["Referer"])
	}
	if source.Headers["Origin"] != "https://uqload.vc" {
		t.Fatalf("origin = %q", source.Headers["Origin"])
	}
	if source.Headers["User-Agent"] != "test-ua" {
		t.Fatalf("ua = %q", source.Headers["User-Agent"])
	}
	if source.Label != "SUB" {
		t.Fatalf("label = %q", source.Label)
	}
	dub, err := playable("https://strm9.example/master.m3u8", "https://uqload.vc/embed-x.html", nil, true, "test-ua")
	if err != nil {
		t.Fatalf("playable dub: %v", err)
	}
	if dub.Label != "DUB" {
		t.Fatalf("dub label = %q", dub.Label)
	}
	if _, err := playable("https://strm9.example/master.m3u8", "http://uqload.vc/embed-x.html", nil, false, "test-ua"); err == nil {
		t.Fatal("expected insecure referer to be rejected")
	}
}

// TestResolveEndToEnd drives the provider through a stub anihosts embed
// page whose src is itself a direct HLS master, plus a stub host embed for
// the packed-config path validation.
func TestResolveEndToEnd(t *testing.T) {
	const file = "https://strm9.example/hls2/master.m3u8?t=TOKEN&s=EXP"

	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/p/home/embed/anilist/16498/1" {
			t.Errorf("unexpected path %s", r.URL.Path)
		}
		body, _ := json.Marshal(map[string]any{
			"status":   "ready",
			"src":      file,
			"source":   "uqload",
			"episodes": []int{1, 2, 3},
		})
		_, _ = w.Write([]byte(`<script type="application/json" id="state-data">` + string(body) + `</script>`))
	}))
	defer origin.Close()

	provider := newWithOrigin(httpx.NewClient("test-ua", 0), origin.URL)
	sources, err := provider.Resolve(context.Background(), resolve.Request{
		AnilistID: 16498,
		Episode:   1,
		Audio:     "sub",
	})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if len(sources) != 1 {
		t.Fatalf("sources = %d, want 1", len(sources))
	}
	if sources[0].URL != file {
		t.Fatalf("url = %q, want %q", sources[0].URL, file)
	}
	if sources[0].Headers["Referer"] != file || sources[0].Headers["Origin"] != "https://strm9.example" {
		t.Fatalf("headers = %+v", sources[0].Headers)
	}
	if sources[0].Label != "SUB" {
		t.Fatalf("label = %q", sources[0].Label)
	}
}

func TestLatestCardsKeepTitleEpisodeAndLink(t *testing.T) {
	page := `<section class="yt-latest"><h2>Latest releases</h2><div class="yt-grid">
	<div class="yt-card"><a class="yt-card__thumb-box" href="/anime/renegade-immortal-55809"><img alt="Renegade Immortal" class="img-fade yt-card__thumb" src="https://img.example/backdrop.jpg"><span class="yt-card__badge">EP 168</span></a><div class="yt-card__meta-row"><div class="yt-card__info"><h3 class="yt-card__title" title="Renegade Immortal">Renegade Immortal</h3></div></div><button type="button" class="yt-card__more"></button></div>
	<div class="yt-card"><a class="yt-card__thumb-box" href="/anime/other-show-1"><img alt="Other" class="img-fade yt-card__thumb" src="https://img.example/other.jpg"><span class="yt-card__badge">NEW</span></a><div class="yt-card__meta-row"><div class="yt-card__info"><h3 class="yt-card__title" title="Other Show">Other Show</h3></div></div><button type="button" class="yt-card__more"></button></div>
	</div></section>`
	var entries []protocol.LatestEntry
	for _, block := range latestCardRe.FindAllString(page, -1) {
		href := latestHrefRe.FindStringSubmatch(block)
		if href == nil {
			continue
		}
		title := ""
		if m := latestTitleRe.FindStringSubmatch(block); m != nil {
			title = htmlUnescape(m[1])
		}
		episode := 0
		if m := latestBadgeRe.FindStringSubmatch(block); m != nil {
			episode, _ = strconv.Atoi(m[1])
		}
		image := ""
		if m := latestImageRe.FindStringSubmatch(block); m != nil {
			image = m[1]
		}
		entries = append(entries, protocol.LatestEntry{Title: title, Episode: episode, ImageURL: image, SiteURL: "https://anihosts.xyz" + href[1]})
	}
	if len(entries) != 2 {
		t.Fatalf("entries = %d", len(entries))
	}
	if entries[0].Title != "Renegade Immortal" || entries[0].Episode != 168 || entries[0].SiteURL != "https://anihosts.xyz/anime/renegade-immortal-55809" {
		t.Fatalf("entry0 = %+v", entries[0])
	}
	if entries[0].ImageURL != "https://img.example/backdrop.jpg" {
		t.Fatalf("image = %q", entries[0].ImageURL)
	}
	if entries[1].Episode != 0 || entries[1].Title != "Other Show" {
		t.Fatalf("entry1 = %+v", entries[1])
	}
}

// pack builds a packed eval blob the way the host page does: every
// dictionary word is swapped for its base-36 index, longest first.
func pack(config string) string {
	words := []string{
		"",
		"file",
		"sources",
		"tracks",
		"kind",
		"label",
		"captions",
		"thumbnails",
		"English",
		"Upload captions",
		"image",
		"default",
		"setup",
		"jwplayer",
		"https://strm.example/hls2/master.m3u8?t=TOKEN&s=EXP",
		"https://strm.example/hls2/master.m3u8",
		"https://strm.example/poster.jpg",
		"https://strm.example/vtt/ep_eng.vtt",
		"https://strm.example/vtt/ep_sli.vtt",
		"/srt/empty.srt",
		"https://strm9.example/hls2/master.m3u8?t=TOKEN&s=EXP",
		"https://strm9.example/vtt/ep_eng.vtt",
	}
	payload := config
	for _, index := range longestFirst(words) {
		payload = strings.ReplaceAll(payload, words[index], base36(index))
	}
	return "eval(function(p,a,c,k,e,d){while(c--)if(k[c])p=p.replace(new RegExp('\\b'+c.toString(a)+'\\b','g'),k[c]);return p}('" +
		payload + "',36," + strconv.Itoa(len(words)) + ",'" + strings.Join(words, "|") + "'.split('|'),0,{}))"
}

func longestFirst(words []string) []int {
	order := make([]int, 0, len(words)-1)
	for i := 1; i < len(words); i++ {
		order = append(order, i)
	}
	for i := 0; i < len(order); i++ {
		for j := i + 1; j < len(order); j++ {
			if len(words[order[j]]) > len(words[order[i]]) {
				order[i], order[j] = order[j], order[i]
			}
		}
	}
	return order
}

func base36(n int) string {
	const digits = "0123456789abcdefghijklmnopqrstuvwxyz"
	if n == 0 {
		return "0"
	}
	var out []byte
	for n > 0 {
		out = append([]byte{digits[n%36]}, out...)
		n /= 36
	}
	return string(out)
}
