package gotaku

import (
	"bytes"
	"compress/zlib"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/hex"
	"testing"
)

// ---------------------------------------------------------------------------
// Test-side seal helpers. They re-implement the site's encryption so the
// round trips exercise the real decrypt path.

func testGCM(t *testing.T, key, iv, plain []byte) []byte {
	t.Helper()
	block, err := aes.NewCipher(key)
	if err != nil {
		t.Fatal(err)
	}
	gcm, err := cipher.NewGCMWithNonceSize(block, 12)
	if err != nil {
		t.Fatal(err)
	}
	return gcm.Seal(nil, iv, plain, nil)
}

func testSealBody(t *testing.T, plain []byte, prefix byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zlib.NewWriter(&buf)
	if _, err := zw.Write(plain); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	key := HKDFSHA256(API_IKM, nil, []byte(sealInfo), 32)
	iv := make([]byte, 12)
	if _, err := rand.Read(iv); err != nil {
		t.Fatal(err)
	}
	ct := testGCM(t, key, iv, buf.Bytes())
	out := make([]byte, 0, 1+len(iv)+len(ct))
	out = append(out, prefix)
	out = append(out, iv...)
	out = append(out, ct...)
	return out
}

func testManifestBody(t *testing.T, nonce string, plain []byte) []byte {
	t.Helper()
	salt, err := B64URLDecode(nonce)
	if err != nil {
		t.Fatal(err)
	}
	key := HKDFSHA256(EMBED_KEY, salt, []byte(manifestInfo), 32)
	iv := make([]byte, 12)
	if _, err := rand.Read(iv); err != nil {
		t.Fatal(err)
	}
	ct := testGCM(t, key, iv, plain)
	blob := append(iv, ct...)
	return []byte(b64URL(blob))
}

func testPlaylistBody(t *testing.T, keySeed []byte, token string, plain []byte) []byte {
	t.Helper()
	key := HKDFSHA256(keySeed, []byte(token), []byte(playlistInfo), 32)
	iv := make([]byte, 12)
	if _, err := rand.Read(iv); err != nil {
		t.Fatal(err)
	}
	ct := testGCM(t, key, iv, plain)
	return append(iv, ct...)
}

func fromHex(t *testing.T, s string) []byte {
	t.Helper()
	b, err := hex.DecodeString(s)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// ---------------------------------------------------------------------------
// API seal.

func TestMaybeUnsealAPIRoundTrip(t *testing.T) {
	fixture := []byte(`{"data":[{"id":"t1","name":"One Piece"}]}`)
	sealed := testSealBody(t, fixture, 0x01)
	plain, err := MaybeUnsealAPI(sealed)
	if err != nil {
		t.Fatalf("MaybeUnsealAPI: %v", err)
	}
	if !bytes.Equal(plain, fixture) {
		t.Fatalf("unsealed = %q, want %q", plain, fixture)
	}
}

func TestMaybeUnsealAPIPassThrough(t *testing.T) {
	for _, body := range [][]byte{[]byte(`{"a":1}`), []byte(`[1,2,3]`), []byte{}} {
		got, err := MaybeUnsealAPI(body)
		if err != nil {
			t.Fatalf("passthrough %q: %v", body, err)
		}
		if !bytes.Equal(got, body) {
			t.Fatalf("passthrough %q returned %q", body, got)
		}
	}
}

func TestMaybeUnsealAPIRejectsShortOrTampered(t *testing.T) {
	if _, err := MaybeUnsealAPI([]byte{0x01, 0x02}); err == nil {
		t.Fatal("short sealed body accepted")
	}
	sealed := testSealBody(t, []byte(`{"x":1}`), 0x01)
	sealed[len(sealed)-1] ^= 0xff // flip the tag
	if _, err := MaybeUnsealAPI(sealed); err == nil {
		t.Fatal("tampered sealed body accepted")
	}
}

// ---------------------------------------------------------------------------
// Embed manifest token + response.

func TestBuildManifestTokenRoundTrip(t *testing.T) {
	nonce, token, err := BuildManifestToken("stamp-42")
	if err != nil {
		t.Fatalf("BuildManifestToken: %v", err)
	}
	nonceBytes, err := B64URLDecode(nonce)
	if err != nil || len(nonceBytes) != 16 {
		t.Fatalf("nonce = %q (%d bytes), err %v", nonce, len(nonceBytes), err)
	}
	// The token itself is iv + AES-GCM({"nonce":...,"stamp":...}) under
	// EMBED_KEY; open it back.
	blob, err := B64URLDecode(token)
	if err != nil {
		t.Fatalf("token decode: %v", err)
	}
	if len(blob) <= 12 {
		t.Fatalf("token blob too short: %d bytes", len(blob))
	}
	plain, err := aesGCMDecrypt(EMBED_KEY, blob[:12], blob[12:])
	if err != nil {
		t.Fatalf("token open: %v", err)
	}
	want := `{"nonce":"` + nonce + `","stamp":"stamp-42"}`
	if string(plain) != want {
		t.Fatalf("token plain = %q, want %q", plain, want)
	}

	// The manifest response: encrypt a fixture under HKDF(EMBED_KEY,
	// salt=nonce, info=manifest-resp|v1) and decrypt through the API.
	fixture := `{"keySeed":"c2VlZA","token":"pl-tok","source":"https://cdn.example/p/1/master.m3u8","obf":{"segmentBytes":8192}}`
	body := testManifestBody(t, nonce, []byte(fixture))
	got, err := DecryptManifestResponse(body, nonce)
	if err != nil {
		t.Fatalf("DecryptManifestResponse: %v", err)
	}
	if got != fixture {
		t.Fatalf("manifest = %q, want %q", got, fixture)
	}
}

func TestDecryptManifestResponseFailures(t *testing.T) {
	if _, err := DecryptManifestResponse([]byte("not-base64!"), "abc"); err == nil {
		t.Fatal("bad body accepted")
	}
	if _, err := DecryptManifestResponse([]byte("AAAA"), "!!!"); err == nil {
		t.Fatal("bad nonce accepted")
	}
	if _, err := DecryptManifestResponse([]byte("QUFBQUFBQUFBQUFB"), "AAAA"); err == nil {
		t.Fatal("short blob accepted")
	}
	// Wrong nonce → different key → GCM authentication must fail.
	body := testManifestBody(t, "c2VlZA", []byte(`{"x":1}`))
	if _, err := DecryptManifestResponse(body, "b3RoZXI"); err == nil {
		t.Fatal("wrong-nonce manifest accepted")
	}
}

// ---------------------------------------------------------------------------
// Playlist.

func TestDecryptPlaylistRoundTrip(t *testing.T) {
	keySeed := []byte("0123456789abcdef0123456789abcdef")
	token := "pl-tok-1"
	plain := []byte("#EXTM3U\n#EXT-X-STREAM-INF:BANDWIDTH=5000000,RESOLUTION=1920x1080\nhttps://cdn.example/p/1/1080/index.m3u8\n")
	raw := testPlaylistBody(t, keySeed, token, plain)
	got, err := DecryptPlaylist(raw, keySeed, token)
	if err != nil {
		t.Fatalf("DecryptPlaylist: %v", err)
	}
	if !bytes.Equal(got, plain) {
		t.Fatalf("playlist = %q, want %q", got, plain)
	}
	// The wrong token salts a different key.
	raw = testPlaylistBody(t, keySeed, token, plain)
	if _, err := DecryptPlaylist(raw, keySeed, "other"); err == nil {
		t.Fatal("wrong-token playlist accepted")
	}
	if _, err := DecryptPlaylist([]byte("short"), keySeed, token); err == nil {
		t.Fatal("short playlist accepted")
	}
}

func TestDecodeKeySeed(t *testing.T) {
	seed, err := DecodeKeySeed(b64URL(EMBED_KEY))
	if err != nil {
		t.Fatalf("DecodeKeySeed: %v", err)
	}
	if !bytes.Equal(seed, EMBED_KEY) {
		t.Fatalf("seed round trip failed")
	}
	if _, err := DecodeKeySeed("***"); err == nil {
		t.Fatal("bad seed accepted")
	}
}

// ---------------------------------------------------------------------------
// CTR keystream + path key + counter.

// The AES-128 zero-key/zero-block ciphertext is the well-known vector
// 66e94bd4ef8a2c3b884cfa59ca342b2e, so the CTR keystream for a zero key and
// zero counter must start with exactly those bytes.
func TestCTRKeystreamHandVector(t *testing.T) {
	key := make([]byte, 16)
	counter := make([]byte, 16)
	keystream, err := CTRKeystream(key, counter, 16)
	if err != nil {
		t.Fatalf("CTRKeystream: %v", err)
	}
	want := fromHex(t, "66e94bd4ef8a2c3b884cfa59ca342b2e")
	if !bytes.Equal(keystream, want) {
		t.Fatalf("keystream = %x, want %x", keystream, want)
	}
}

func TestCTRKeystreamXORRestoresPlaintext(t *testing.T) {
	key := []byte("gotaku key seed 0123456789abcdef")
	counter := CTRCounter("a/b/seg-1")
	plain := []byte("the quick brown fox jumps over the lazy dog 0123456789")
	keystream, err := CTRKeystream(key, counter, len(plain))
	if err != nil {
		t.Fatalf("CTRKeystream: %v", err)
	}
	ciphertext := make([]byte, len(plain))
	for i := range plain {
		ciphertext[i] = plain[i] ^ keystream[i]
	}
	restored, err := CTRKeystream(key, counter, len(plain))
	if err != nil {
		t.Fatal(err)
	}
	for i := range restored {
		restored[i] ^= ciphertext[i]
	}
	if !bytes.Equal(restored, plain) {
		t.Fatalf("restored = %q, want %q", restored, plain)
	}
	if ks, err := CTRKeystream(key, counter, 0); err != nil || len(ks) != 0 {
		t.Fatalf("zero length = %v, %v", ks, err)
	}
	if ks, err := CTRKeystream(key, counter, -5); err != nil || len(ks) != 0 {
		t.Fatalf("negative length = %v, %v", ks, err)
	}
	if _, err := CTRKeystream([]byte("short"), counter, 16); err == nil {
		t.Fatal("bad key length accepted")
	}
	if _, err := CTRKeystream(key, []byte("not16"), 16); err == nil {
		t.Fatal("bad counter length accepted")
	}
}

func TestSegmentPathKey(t *testing.T) {
	cases := map[string]string{
		"/s/1/a/b/seg-1.ts":  "a/b/seg-1",
		"/s/1/seg-1.ts":      "seg-1",
		"/s/1/video":         "video",
		"/s/1/a.b/seg.ts":    "a.b/seg",
		"/s/1/seg":           "seg",
		"/p/1/master.m3u8":   "master",
		"":                   "",
		"/s/1/":              "",
		"/s/1/file.tar.gz":   "file.tar",
		"//s//1//x//y.ts///": "x/y",
	}
	for path, want := range cases {
		if got := SegmentPathKey(path); got != want {
			t.Fatalf("SegmentPathKey(%q) = %q, want %q", path, got, want)
		}
	}
}

func TestCTRCounter(t *testing.T) {
	counter := CTRCounter("a/b/seg-1")
	if len(counter) != 16 {
		t.Fatalf("counter length = %d", len(counter))
	}
	for i := 8; i < 16; i++ {
		if counter[i] != 0 {
			t.Fatalf("counter tail not zero: %x", counter)
		}
	}
	other := CTRCounter("a/b/seg-2")
	if bytes.Equal(counter[:8], other[:8]) {
		t.Fatal("different path keys share a counter prefix")
	}
}

// ---------------------------------------------------------------------------
// HKDF + base64url.

// RFC 5869 A.1: salt + info, multi-block output.
func TestHKDFRFC5869Case1(t *testing.T) {
	ikm := bytes.Repeat([]byte{0x0b}, 22)
	salt := fromHex(t, "000102030405060708090a0b0c")
	info := fromHex(t, "f0f1f2f3f4f5f6f7f8f9")
	want := fromHex(t, "3cb25f25faacd57a90434f64d0362f2a"+
		"2d2d0a90cf1a5a4c5db02d56ecc4c5bf"+
		"34007208d5b887185865")
	if got := HKDFSHA256(ikm, salt, info, 42); !bytes.Equal(got, want) {
		t.Fatalf("HKDF A.1 = %x, want %x", got, want)
	}
}

// RFC 5869 A.3: empty salt (32 zero bytes) and empty info.
func TestHKDFRFC5869Case3EmptySalt(t *testing.T) {
	ikm := bytes.Repeat([]byte{0x0b}, 22)
	want := fromHex(t, "8da4e775a563c18f715f802a063c5a31"+
		"b8a11f5c5ee1879ec3454e5f3c738d2d"+
		"9d201395faa4b61a96c8")
	if got := HKDFSHA256(ikm, nil, nil, 42); !bytes.Equal(got, want) {
		t.Fatalf("HKDF A.3 = %x, want %x", got, want)
	}
}

func TestAPISealKeyIsStable(t *testing.T) {
	a := HKDFSHA256(API_IKM, nil, []byte(sealInfo), 32)
	b := HKDFSHA256(API_IKM, nil, []byte(sealInfo), 32)
	if len(a) != 32 || !bytes.Equal(a, b) {
		t.Fatalf("API seal key not stable: %x vs %x", a, b)
	}
}

func TestB64URLDecode(t *testing.T) {
	if got, err := B64URLDecode("aGVsbG8"); err != nil || string(got) != "hello" {
		t.Fatalf("unpadded = %q, %v", got, err)
	}
	if got, err := B64URLDecode("aGVsbG8="); err != nil || string(got) != "hello" {
		t.Fatalf("padded = %q, %v", got, err)
	}
	if got, err := B64URLDecode("_-_"); err != nil || !bytes.Equal(got, []byte{0xff, 0xef}) {
		t.Fatalf("alphabet = %x, %v", got, err)
	}
	if got, err := B64URLDecode("  aGVsbG8\n"); err != nil || string(got) != "hello" {
		t.Fatalf("whitespace = %q, %v", got, err)
	}
	if _, err := B64URLDecode("not base64!!"); err == nil {
		t.Fatal("garbage accepted")
	}
	// Round trip every exported key material.
	if decoded, err := B64URLDecode(b64URL(EMBED_KEY)); err != nil || !bytes.Equal(decoded, EMBED_KEY) {
		t.Fatal("EMBED_KEY round trip failed")
	}
	if decoded, err := B64URLDecode(b64URL(API_IKM)); err != nil || !bytes.Equal(decoded, API_IKM) {
		t.Fatal("API_IKM round trip failed")
	}
}

func TestKeyConstants(t *testing.T) {
	if len(API_IKM) != 32 || len(EMBED_KEY) != 32 {
		t.Fatalf("key lengths: API_IKM=%d EMBED_KEY=%d", len(API_IKM), len(EMBED_KEY))
	}
}
