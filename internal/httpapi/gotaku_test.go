package httpapi

import (
	"net/url"
	"strings"
	"testing"
)

func TestRewriteGotakuPlaylist(t *testing.T) {
	master := strings.Join([]string{
		"#EXTM3U",
		"#EXT-X-STREAM-INF:BANDWIDTH=800000,RESOLUTION=1280x720",
		"1/index.m3u8",
		"#EXT-X-STREAM-INF:BANDWIDTH=2000000,RESOLUTION=1920x1080",
		"https://gotakudl.top/p/1/other/2/index.m3u8",
		"#EXT-X-KEY:METHOD=NONE",
		"https://keys.example/k1",
		"",
	}, "\n")
	base, _ := url.Parse("https://gotakudl.top/p/1/one/master.m3u8")
	out := rewriteGotakuPlaylist(master, base, "https://gotakudl.top/embed", "c2VlZA", "tok", 8192)

	lines := strings.Split(out, "\n")
	if lines[0] != "#EXTM3U" {
		t.Fatalf("head = %q", lines[0])
	}
	if !strings.HasPrefix(lines[2], "/v1/gotaku/play?u=") || !strings.Contains(lines[2], "gotakudl.top%2Fp%2F1%2Fone%2F1%2Findex.m3u8") {
		t.Fatalf("relative variant not rewritten: %q", lines[2])
	}
	if !strings.Contains(lines[2], "seed=c2VlZA") || !strings.Contains(lines[2], "token=tok") || !strings.Contains(lines[2], "seg=8192") {
		t.Fatalf("params missing: %q", lines[2])
	}
	if !strings.HasPrefix(lines[4], "/v1/gotaku/play?u=") {
		t.Fatalf("absolute variant not rewritten: %q", lines[4])
	}
	// The key URI is neither a playlist nor a segment: absolutized, not proxied.
	if lines[6] != "https://keys.example/k1" {
		t.Fatalf("key uri = %q", lines[6])
	}
}

func TestRewriteGotakuSegments(t *testing.T) {
	media := strings.Join([]string{
		"#EXTM3U",
		"#EXTINF:9.009,",
		"seg-1.ts",
		"#EXTINF:9.009,",
		"https://gotakudl.top/s/1/two/seg-2.ts",
		"",
	}, "\n")
	base, _ := url.Parse("https://gotakudl.top/p/1/one/2/index.m3u8")
	out := rewriteGotakuPlaylist(media, base, "https://gotakudl.top/embed", "c2VlZA", "tok", 4096)

	lines := strings.Split(out, "\n")
	if !strings.HasPrefix(lines[2], "/v1/gotaku/seg?u=") || !strings.Contains(lines[2], "seg-1.ts") {
		t.Fatalf("relative segment not rewritten: %q", lines[2])
	}
	if !strings.Contains(lines[2], "seg=4096") {
		t.Fatalf("seg param missing: %q", lines[2])
	}
	if !strings.HasPrefix(lines[4], "/v1/gotaku/seg?u=") {
		t.Fatalf("absolute segment not rewritten: %q", lines[4])
	}
}

func TestGotakuUpstreamValidation(t *testing.T) {
	// Public hosts on gotaku's CDN path shapes are accepted (the CDN host
	// rotates, so hosts are not allowlisted).
	if _, ok := gotakuUpstream("https://gotakudl.top/p/1/x/master.m3u8", true); !ok {
		t.Fatal("valid playlist url rejected")
	}
	if _, ok := gotakuUpstream("https://gotakudl.top/s/1/x/seg.ts", false); !ok {
		t.Fatal("valid segment url rejected")
	}
	if _, ok := gotakuUpstream("https://example.com/p/1/x/master.m3u8", true); !ok {
		t.Fatal("rotated public CDN host rejected")
	}
	if _, ok := gotakuUpstream("http://gotakudl.top/p/1/x.m3u8", true); ok {
		t.Fatal("http upstream accepted")
	}
	if _, ok := gotakuUpstream("https://localhost/p/1/x.m3u8", true); ok {
		t.Fatal("localhost upstream accepted")
	}
	if _, ok := gotakuUpstream("https://127.0.0.1/p/1/x.m3u8", true); ok {
		t.Fatal("loopback upstream accepted")
	}
	if _, ok := gotakuUpstream("https://10.0.0.5/s/1/x.ts", false); ok {
		t.Fatal("private upstream accepted")
	}
	if _, ok := gotakuUpstream("https://gotakudl.top/other/x.m3u8", true); ok {
		t.Fatal("non-gotaku path accepted")
	}
	if _, ok := gotakuUpstream("https://gotakudl.top/p/1/x.m3u8", false); ok {
		t.Fatal("playlist url accepted as segment")
	}
	if _, ok := gotakuUpstream("not a url", true); ok {
		t.Fatal("garbage accepted")
	}
}

func TestGotakuRangeStart(t *testing.T) {
	if n := gotakuRangeStart("bytes=100-"); n != 100 {
		t.Fatalf("open range = %d", n)
	}
	if n := gotakuRangeStart("bytes=100-200"); n != 100 {
		t.Fatalf("closed range = %d", n)
	}
	if n := gotakuRangeStart("bytes=-200"); n != 0 {
		t.Fatalf("suffix range = %d", n)
	}
	if n := gotakuRangeStart("garbage"); n != 0 {
		t.Fatalf("garbage range = %d", n)
	}
}

func TestGotakuSegmentBytes(t *testing.T) {
	if n := gotakuSegmentBytes(""); n != 8192 {
		t.Fatalf("default = %d", n)
	}
	if n := gotakuSegmentBytes("4096"); n != 4096 {
		t.Fatalf("explicit = %d", n)
	}
	if n := gotakuSegmentBytes("99999999"); n != gotakuMaxSegBytes {
		t.Fatalf("capped = %d", n)
	}
	if n := gotakuSegmentBytes("-1"); n != 8192 {
		t.Fatalf("negative = %d", n)
	}
}
