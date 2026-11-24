package httpapi

// Gotaku transform proxy.
//
// Gotaku encrypts its HLS playlists (AES-GCM) and XOR-obfuscates the first
// bytes of every segment (AES-CTR keystream). A plain URL is not playable,
// so AniFuse moves the unwrap server-side: /v1/gotaku/play serves decrypted
// playlists with every relative URI rewritten through this proxy, and
// /v1/gotaku/seg streams segments with the XOR applied. Upstreams are
// restricted to https URLs on gotaku's own /p/1 and /s/1 CDN paths, so the
// endpoints cannot act as a general open proxy.

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/fampep/anifuse/internal/providers/gotaku"
)

// gotakuProxyUserAgent mirrors the browser the gotaku CDN expects.
const gotakuProxyUserAgent = "Mozilla/5.0 (Linux; Android 14) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0.0.0 Mobile Safari/537.36"

const (
	gotakuMaxPlaylist = 2 << 20
	gotakuMaxSegBytes = 1 << 20
)

func gotakuIsPlaylistPath(path string) bool {
	return strings.Contains(path, "/p/1")
}

func gotakuIsSegmentPath(path string) bool {
	return strings.Contains(path, "/s/1")
}

// gotakuIsMediaName reports whether a URI under the /p/1 tree is a media
// file rather than a playlist: real playlists can reference their segments
// relative to themselves, and those must go through the segment XOR lane.
func gotakuIsMediaName(path string) bool {
	lower := strings.ToLower(path)
	for _, ext := range []string{".ts", ".m4s", ".mp4", ".aac", ".m4a", ".mp3", ".webm", ".mkv"} {
		if strings.HasSuffix(lower, ext) {
			return true
		}
	}
	return false
}

// gotakuUpstream validates and parses the upstream URL the proxy may fetch:
// https only, not a private/internal address, and the path must be one of
// gotaku's obfuscated CDN paths. The CDN rotates hostnames (gotakudl.top,
// *.avelvetsignal.top, ...), so hosts are not allowlisted — the /p/1 and
// /s/1 path shapes plus the transform itself keep the proxy useless for
// anything that is not gotaku-encrypted content.
func gotakuUpstream(raw string, playlist bool) (*url.URL, bool) {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.Host == "" {
		return nil, false
	}
	host := u.Hostname()
	if isPrivateHost(host) {
		return nil, false
	}
	if playlist && !gotakuIsPlaylistPath(u.Path) {
		return nil, false
	}
	if !playlist && !gotakuIsSegmentPath(u.Path) {
		return nil, false
	}
	return u, true
}

// isPrivateHost blocks loopback, link-local and RFC1918 targets so the
// proxy cannot be pointed at internal services.
func isPrivateHost(host string) bool {
	if host == "localhost" || strings.HasSuffix(host, ".localhost") || strings.HasSuffix(host, ".local") {
		return true
	}
	ip := net.ParseIP(host)
	if ip == nil {
		ips, err := net.LookupIP(host)
		if err != nil || len(ips) == 0 {
			return true // unresolved hosts are refused, not fetched
		}
		ip = ips[0]
	}
	return ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() || ip.IsUnspecified()
}

// handleGotakuPlay serves a decrypted HLS playlist with every URI rewritten
// through this proxy so players see plain standard HLS end to end.
func (s *Server) handleGotakuPlay(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	upstream := q.Get("u")
	referer := q.Get("r")
	seedB64 := q.Get("seed")
	token := q.Get("token")
	segmentBytes := gotakuSegmentBytes(q.Get("seg"))

	base, ok := gotakuUpstream(upstream, true)
	if !ok {
		writeError(w, r, http.StatusBadRequest, "gotaku play: bad upstream url")
		return
	}
	seed, err := gotaku.DecodeKeySeed(seedB64)
	if err != nil || len(seed) == 0 {
		writeError(w, r, http.StatusBadRequest, "gotaku play: bad seed")
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 25*time.Second)
	defer cancel()
	resp, err := s.gotakuProxy.Open(ctx, http.MethodGet, base.String(), map[string]string{
		"Accept":  "*/*",
		"Referer": referer,
		"Origin":  originOfURL(base),
	})
	if err != nil {
		writeError(w, r, http.StatusBadGateway, "gotaku play: upstream unreachable")
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		writeError(w, r, http.StatusBadGateway, "gotaku play: upstream status "+strconv.Itoa(resp.StatusCode))
		return
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, gotakuMaxPlaylist))
	if err != nil {
		writeError(w, r, http.StatusBadGateway, "gotaku play: read failed")
		return
	}
	plain, err := gotaku.DecryptPlaylist(body, seed, token)
	if err != nil {
		writeError(w, r, http.StatusBadGateway, "gotaku play: playlist decrypt failed")
		return
	}
	rewritten := rewriteGotakuPlaylist(string(plain), base, referer, seedB64, token, segmentBytes)
	writeBody(w, r, http.StatusOK, "application/vnd.apple.mpegurl; charset=utf-8", []byte(rewritten))
}

// handleGotakuSeg streams one segment with the leading XOR obfuscation
// removed, honoring Range requests so players can seek.
func (s *Server) handleGotakuSeg(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	upstream := q.Get("u")
	referer := q.Get("r")
	seedB64 := q.Get("seed")
	segmentBytes := gotakuSegmentBytes(q.Get("seg"))

	parsed, ok := gotakuUpstream(upstream, false)
	if !ok {
		writeError(w, r, http.StatusBadRequest, "gotaku seg: bad upstream url")
		return
	}
	seed, err := gotaku.DecodeKeySeed(seedB64)
	if err != nil || len(seed) == 0 {
		writeError(w, r, http.StatusBadRequest, "gotaku seg: bad seed")
		return
	}

	hasRange := false
	rangeStart := int64(0)
	if rng := r.Header.Get("Range"); strings.HasPrefix(rng, "bytes=") {
		hasRange = true
		rangeStart = gotakuRangeStart(rng)
	}

	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()
	headers := map[string]string{
		"Accept":  "*/*",
		"Referer": referer,
		"Origin":  originOfURL(parsed),
	}
	if hasRange {
		headers["Range"] = r.Header.Get("Range")
	}
	resp, err := s.gotakuProxy.Open(ctx, http.MethodGet, parsed.String(), headers)
	if err != nil {
		writeError(w, r, http.StatusBadGateway, "gotaku seg: upstream unreachable")
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusPartialContent {
		writeError(w, r, http.StatusBadGateway, "gotaku seg: upstream status "+strconv.Itoa(resp.StatusCode))
		return
	}

	// The XOR offset counts from the start of the segment file, so it is
	// the requested range start, not the byte offset inside the response.
	xorStart := int64(0)
	status := resp.StatusCode
	if resp.StatusCode == http.StatusPartialContent {
		xorStart = rangeStart
	} else if hasRange && rangeStart > 0 && resp.ContentLength > rangeStart {
		// Upstream ignored the range: drop the prefix ourselves and serve 206.
		status = http.StatusPartialContent
		xorStart = rangeStart
		w.Header().Set("Content-Range", "bytes "+strconv.FormatInt(rangeStart, 10)+"-"+
			strconv.FormatInt(resp.ContentLength-1, 10)+"/"+strconv.FormatInt(resp.ContentLength, 10))
		w.Header().Set("Content-Length", strconv.FormatInt(resp.ContentLength-rangeStart, 10))
		if _, err := io.CopyN(io.Discard, resp.Body, rangeStart); err != nil {
			writeError(w, r, http.StatusBadGateway, "gotaku seg: range skip failed")
			return
		}
	}

	for _, name := range []string{"Content-Type", "Accept-Ranges", "ETag", "Cache-Control"} {
		if value := resp.Header.Get(name); value != "" {
			w.Header().Set(name, value)
		}
	}
	if w.Header().Get("Content-Length") == "" && resp.ContentLength >= 0 {
		w.Header().Set("Content-Length", strconv.FormatInt(resp.ContentLength, 10))
	}
	w.WriteHeader(status)

	if xorStart >= int64(segmentBytes) {
		_, _ = io.Copy(w, resp.Body)
		return
	}
	keystream, err := gotaku.CTRKeystream(seed, gotaku.CTRCounter(gotaku.SegmentPathKey(parsed.Path)), segmentBytes)
	if err != nil {
		_, _ = io.Copy(w, resp.Body)
		return
	}
	buf := make([]byte, 32<<10)
	pos := xorStart
	for {
		n, readErr := resp.Body.Read(buf)
		for i := 0; i < n; i++ {
			if pos+int64(i) < int64(segmentBytes) {
				buf[i] ^= keystream[pos+int64(i)]
			}
		}
		if n > 0 {
			if _, err := w.Write(buf[:n]); err != nil {
				return
			}
			pos += int64(n)
		}
		if readErr == io.EOF {
			return
		}
		if readErr != nil {
			return
		}
	}
}

// rewriteGotakuPlaylist turns a decrypted playlist into one that plays
// through this proxy: /p/1 playlists stay proxied, /s/1 segments become
// seg-proxy URLs, anything else is absolutized against the playlist URL.
func rewriteGotakuPlaylist(plain string, base *url.URL, referer, seedB64, token string, segmentBytes int) string {
	var out strings.Builder
	lines := strings.Split(strings.ReplaceAll(plain, "\r", ""), "\n")
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			out.WriteString(line)
			out.WriteString("\n")
			continue
		}
		uri := trimmed
		if !strings.HasPrefix(uri, "http") {
			ref, err := url.Parse(uri)
			if err != nil {
				out.WriteString(line)
				out.WriteString("\n")
				continue
			}
			uri = base.ResolveReference(ref).String()
		}
		parsed, err := url.Parse(uri)
		if err != nil {
			out.WriteString(line)
			out.WriteString("\n")
			continue
		}
		switch {
		case gotakuIsSegmentPath(parsed.Path):
			out.WriteString("/v1/gotaku/seg?u=" + url.QueryEscape(uri) +
				"&r=" + url.QueryEscape(referer) +
				"&seed=" + url.QueryEscape(seedB64) +
				"&seg=" + strconv.Itoa(segmentBytes))
		case gotakuIsPlaylistPath(parsed.Path) && !gotakuIsMediaName(parsed.Path):
			out.WriteString("/v1/gotaku/play?u=" + url.QueryEscape(uri) +
				"&r=" + url.QueryEscape(referer) +
				"&seed=" + url.QueryEscape(seedB64) +
				"&token=" + url.QueryEscape(token) +
				"&seg=" + strconv.Itoa(segmentBytes))
		case gotakuIsPlaylistPath(parsed.Path) && gotakuIsMediaName(parsed.Path):
			// A media file referenced relative to its own playlist lives
			// under the /p/1 tree; it still needs the segment XOR lane.
			out.WriteString("/v1/gotaku/seg?u=" + url.QueryEscape(uri) +
				"&r=" + url.QueryEscape(referer) +
				"&seed=" + url.QueryEscape(seedB64) +
				"&seg=" + strconv.Itoa(segmentBytes))
		default:
			out.WriteString(uri)
		}
		out.WriteString("\n")
	}
	return out.String()
}

func gotakuSegmentBytes(raw string) int {
	n, err := strconv.Atoi(strings.TrimSpace(raw))
	if err != nil || n <= 0 {
		return 8192
	}
	if n > gotakuMaxSegBytes {
		return gotakuMaxSegBytes
	}
	return n
}

// gotakuRangeStart parses the start of a "bytes=start-" / "bytes=start-end"
// range, defaulting to 0 on anything malformed.
func gotakuRangeStart(rng string) int64 {
	spec := strings.TrimPrefix(rng, "bytes=")
	start := spec
	if dash := strings.IndexByte(spec, '-'); dash >= 0 {
		start = spec[:dash]
	}
	n, err := strconv.ParseInt(strings.TrimSpace(start), 10, 64)
	if err != nil || n < 0 {
		return 0
	}
	return n
}

func originOfURL(u *url.URL) string {
	return u.Scheme + "://" + u.Host
}
