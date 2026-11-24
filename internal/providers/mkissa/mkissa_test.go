package mkissa

import (
	"crypto/aes"
	"crypto/cipher"
	"testing"
)

func TestMaskKeyDeterministic(t *testing.T) {
	a := maskKey()
	b := maskKey()
	if len(a) != 32 || string(a) != string(b) {
		t.Fatal("maskKey not deterministic")
	}
	if string(a) == string(make([]byte, 32)) {
		t.Fatal("maskKey zero")
	}
}

func TestBootTokenStable(t *testing.T) {
	if bootToken(1835, "mkissa.to", "mkissa") != bootToken(1835, "mkissa.to", "mkissa") {
		t.Fatal("boot token unstable")
	}
	if bootToken(1835, "mkissa.to", "mkissa") == bootToken(1836, "mkissa.to", "mkissa") {
		t.Fatal("boot token ignores epoch")
	}
}

func TestSealOpenRoundTrip(t *testing.T) {
	key, err := aesKey(base64Part(32))
	if err != nil {
		t.Fatalf("aesKey: %v", err)
	}
	hash := sha256Hex("query text")
	sealed, err := seal(key, hash, 1835, 123_000)
	if err != nil {
		t.Fatalf("seal: %v", err)
	}
	opened, err := open(key, sealed)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	want := `{"v":1,"ts":123000,"epoch":1835,"buildId":"189","qh":"` + hash + `","k":"k7"}`
	if opened != want {
		t.Fatalf("opened = %q", opened)
	}
}

func TestPickShowAndDirectSource(t *testing.T) {
	payload := `{"data":{"shows":{"edges":[
		{"_id":"s1","aniListId":"21","availableEpisodesDetail":{"sub":["1","2"]}},
		{"_id":"s2","aniListId":"123","availableEpisodesDetail":{"sub":["1"]}}
	]}}}`
	hit := pickShow(payload, 21, 2, "sub")
	if hit == nil || hit.id != "s1" || hit.episodeString != "2" {
		t.Fatalf("hit = %+v", hit)
	}
	episode := map[string]any{
		"sourceUrls": []any{
			map[string]any{"type": "iframe", "sourceUrl": "https://x.example/embed/1"},
			map[string]any{"type": "player", "priority": float64(3), "sourceUrl": "https://cdn.example/e2.mp4", "fileExtenstion": "mp4"},
			map[string]any{"type": "player", "priority": float64(9), "sourceUrl": "https://cdn.example/e2-hd.m3u8", "fileExtenstion": "m3u8"},
		},
		"episodeInfo": map[string]any{"vidInforssub": map[string]any{"vidResolution": 1080}},
	}
	src := directSource(episode, "sub")
	if src == nil || src.URL != "https://cdn.example/e2-hd.m3u8" || src.Quality != "1080p" || src.Audio != "SUB" {
		t.Fatalf("src = %+v", src)
	}
}

func TestUnwrapEpisode(t *testing.T) {
	key, err := aesKey(base64Part(32))
	if err != nil {
		t.Fatalf("aesKey: %v", err)
	}
	plain := `{"episode":{"sourceUrls":[{"type":"player","sourceUrl":"https://cdn.example/e1.mp4"}]}}`
	block := encryptBlock(t, key, plain)
	root := `{"data":{"tobeparsed":"` + block + `"}}`
	episode, err := unwrapEpisode(root, key)
	if err != nil {
		t.Fatalf("unwrap: %v", err)
	}
	if episode == nil {
		t.Fatal("episode nil")
	}
	if _, err := unwrapEpisode(`{"errors":[{"message":"x"}]}`, key); err == nil {
		t.Fatal("error payload accepted without data")
	}
}

func encryptBlock(t *testing.T, key []byte, plain string) string {
	block, err := aes.NewCipher(key)
	if err != nil {
		t.Fatalf("aes: %v", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		t.Fatalf("gcm: %v", err)
	}
	iv := make([]byte, 12)
	for i := range iv {
		iv[i] = byte(i * 3)
	}
	body := gcm.Seal(nil, iv, []byte(plain), nil)
	wire := make([]byte, 1+12+len(body))
	wire[0] = 1
	copy(wire[1:13], iv)
	copy(wire[13:], body)
	return encodeBase64(wire)
}

func base64Part(size int) string {
	// Build a partB whose decoded form is exactly 32 bytes.
	raw := make([]byte, size)
	for i := range raw {
		raw[i] = byte(i*7 + 3)
	}
	return encodeBase64(raw)
}

func encodeBase64(raw []byte) string {
	const chars = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+/"
	out := make([]byte, 0, (len(raw)+2)/3*4)
	for i := 0; i < len(raw); i += 3 {
		var b0, b1, b2 byte
		b0 = raw[i]
		if i+1 < len(raw) {
			b1 = raw[i+1]
		}
		if i+2 < len(raw) {
			b2 = raw[i+2]
		}
		out = append(out, chars[b0>>2], chars[(b0&3)<<4|b1>>4])
		if i+1 < len(raw) {
			out = append(out, chars[(b1&15)<<2|b2>>6])
		} else {
			out = append(out, '=')
		}
		if i+2 < len(raw) {
			out = append(out, chars[b2&63])
		} else {
			out = append(out, '=')
		}
	}
	return string(out)
}
