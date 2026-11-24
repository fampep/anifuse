// Package gotaku resolves gotaku.to episodes through the gotakudl.top embed
// player. The site seals its /api/v1 responses (AES-GCM + HKDF + zlib), the
// embed mints an AES-GCM manifest token, and the HLS playlists are AES-GCM
// bodies whose segments have their leading bytes XOR-ed with an AES-CTR
// keystream. This file is a faithful port of GotakuCrypto.kt; the /v1/gotaku
// transform proxy reuses it to unwrap playlists and segments server-side.
package gotaku

import (
	"bytes"
	"compress/zlib"
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"io"
	"regexp"
	"strings"
)

// API_IKM is the stable HKDF input key material observed for ?k=10 sealed
// API responses.
var API_IKM = []byte{
	30, 99, 0xce, 0xec, 0xb0, 26, 0xe4, 0x93,
	55, 35, 0x98, 123, 6, 17, 126, 0xb3,
	40, 0xb5, 0xc5, 0xd2, 0xc2, 90, 0xe8, 35,
	0xf7, 20, 0x82, 43, 71, 117, 123, 82,
}

// EMBED_KEY is the stable AES-GCM key the gotakudl embed uses for the
// manifest token and the manifest response.
var EMBED_KEY = []byte{
	69, 0x8e, 84, 3, 0xc4, 10, 115, 51,
	39, 6, 0xdf, 64, 97, 0x93, 0xdd, 107,
	4, 10, 46, 0xa9, 122, 107, 0xb5, 0x9b,
	82, 0xb2, 0x94, 0xa6, 16, 6, 0xe6, 0xef,
}

// sealInfo / manifestInfo / playlistInfo are the HKDF info strings of the
// three crypto lanes, matching the Kotlin constants.
const (
	sealInfo     = "api-seal|v1"
	manifestInfo = "manifest-resp|v1"
	playlistInfo = "pl|v1"
)

// MaybeUnsealAPI opens a gotaku.to /api/v1 body. Unsealed bodies (leading
// '{' or '[') pass through untouched; sealed bodies are a 1-byte prefix,
// a 12-byte IV, and AES-GCM ciphertext whose plaintext is zlib-compressed
// JSON.
func MaybeUnsealAPI(raw []byte) ([]byte, error) {
	if len(raw) == 0 {
		return raw, nil
	}
	switch raw[0] {
	case '{', '[':
		return raw, nil
	}
	if len(raw) <= 13 {
		return nil, errors.New("gotaku: sealed body too short")
	}
	key := HKDFSHA256(API_IKM, nil, []byte(sealInfo), 32)
	plain, err := aesGCMDecrypt(key, raw[1:13], raw[13:])
	if err != nil {
		return nil, err
	}
	return zlibInflate(plain)
}

// BuildManifestToken mints the token the embed player expects:
// b64url-nopad of a 12-byte random IV followed by AES-GCM of
// {"nonce":"...","stamp":"..."} under EMBED_KEY. The nonce is 16 random
// bytes b64url-nopad and is the HKDF salt of the manifest response.
func BuildManifestToken(stamp string) (nonce string, token string, err error) {
	nonce, err = randomNonce()
	if err != nil {
		return "", "", err
	}
	plain := `{"nonce":"` + nonce + `","stamp":"` + stamp + `"}`
	iv := make([]byte, 12)
	if _, err := rand.Read(iv); err != nil {
		return "", "", err
	}
	ct, err := aesGCMEncrypt(EMBED_KEY, iv, []byte(plain))
	if err != nil {
		return "", "", err
	}
	blob := make([]byte, 0, len(iv)+len(ct))
	blob = append(blob, iv...)
	blob = append(blob, ct...)
	return nonce, b64URL(blob), nil
}

// DecryptManifestResponse opens the manifest the embed hands back: the body
// is b64url of IV(12) + AES-GCM ciphertext under HKDF(EMBED_KEY, salt=nonce,
// info="manifest-resp|v1").
func DecryptManifestResponse(body []byte, nonce string) (string, error) {
	blob, err := B64URLDecode(string(body))
	if err != nil {
		return "", err
	}
	salt, err := B64URLDecode(nonce)
	if err != nil {
		return "", err
	}
	key := HKDFSHA256(EMBED_KEY, salt, []byte(manifestInfo), 32)
	if len(blob) <= 12 {
		return "", errors.New("gotaku: manifest blob too short")
	}
	plain, err := aesGCMDecrypt(key, blob[:12], blob[12:])
	if err != nil {
		return "", err
	}
	return string(plain), nil
}

// DecryptPlaylist opens an HLS playlist body (/p/1 paths): IV(12) + AES-GCM
// ciphertext under HKDF(keySeed, salt=playlistToken, info="pl|v1").
func DecryptPlaylist(raw []byte, keySeed []byte, playlistToken string) ([]byte, error) {
	key := HKDFSHA256(keySeed, []byte(playlistToken), []byte(playlistInfo), 32)
	if len(raw) <= 12 {
		return nil, errors.New("gotaku: playlist too short")
	}
	return aesGCMDecrypt(key, raw[:12], raw[12:])
}

// DecodeKeySeed decodes the manifest keySeed field (base64url).
func DecodeKeySeed(b64 string) ([]byte, error) {
	return B64URLDecode(b64)
}

var segmentExtRe = regexp.MustCompile(`\.[^.]*$`)

// SegmentPathKey is the CTR path key: the pathname segments after the first
// two ("s/1/NAME.ext" → "NAME"), with the extension stripped.
func SegmentPathKey(urlPath string) string {
	var parts []string
	for _, part := range strings.Split(urlPath, "/") {
		if part != "" {
			parts = append(parts, part)
		}
	}
	rest := ""
	if len(parts) > 2 {
		rest = strings.Join(parts[2:], "/")
	}
	return segmentExtRe.ReplaceAllString(rest, "")
}

// CTRCounter is the AES-CTR initial counter: SHA-256(pathKey)[:8] followed by
// 8 zero bytes.
func CTRCounter(pathKey string) []byte {
	sum := sha256.Sum256([]byte(pathKey))
	out := make([]byte, 16)
	copy(out, sum[:8])
	return out
}

// CTRKeystream expands AES-CTR over zero bytes: the keystream the segment
// XOR obfuscation uses for the leading bytes of /s/1 segments.
func CTRKeystream(keySeed []byte, counter []byte, length int) ([]byte, error) {
	if length <= 0 {
		return []byte{}, nil
	}
	if len(counter) != aes.BlockSize {
		return nil, errors.New("gotaku: CTR counter must be 16 bytes")
	}
	block, err := aes.NewCipher(keySeed)
	if err != nil {
		return nil, err
	}
	out := make([]byte, length)
	cipher.NewCTR(block, counter).XORKeyStream(out, out)
	return out, nil
}

// B64URLDecode decodes base64url text with -/_ alphabet and padding
// tolerance, matching the Kotlin b64UrlDecode.
func B64URLDecode(text string) ([]byte, error) {
	s := strings.TrimSpace(text)
	s = strings.ReplaceAll(s, "-", "+")
	s = strings.ReplaceAll(s, "_", "/")
	switch len(s) % 4 {
	case 2:
		s += "=="
	case 3:
		s += "="
	}
	return base64.StdEncoding.DecodeString(s)
}

// HKDFSHA256 is RFC 5869 HKDF with SHA-256: extract (HMAC-SHA256 with the
// salt, 32 zero bytes when the salt is empty — exactly like the Kotlin
// hkdfSha256) then expand to length bytes.
func HKDFSHA256(ikm, salt, info []byte, length int) []byte {
	if len(salt) == 0 {
		salt = make([]byte, sha256.Size)
	}
	prk := hmacSHA256(salt, ikm)
	out := make([]byte, 0, length)
	var prev []byte
	for counter := byte(1); len(out) < length; counter++ {
		h := hmac.New(sha256.New, prk)
		h.Write(prev)
		h.Write(info)
		h.Write([]byte{counter})
		prev = h.Sum(nil)
		out = append(out, prev...)
	}
	return out[:length]
}

// b64URL encodes with the URL-safe alphabet, no wrap, no padding.
func b64URL(data []byte) string {
	return base64.RawURLEncoding.EncodeToString(data)
}

// randomNonce is 16 random bytes b64url-nopad.
func randomNonce() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return b64URL(b), nil
}

// aesGCMEncrypt seals with AES-GCM: the nonce size is pinned to 12 bytes via
// NewGCMWithNonceSize (the site's format, set explicitly even though 12 is
// the Go default) and there is no additional data.
func aesGCMEncrypt(key, iv, plain []byte) ([]byte, error) {
	gcm, err := newGCM12(key)
	if err != nil {
		return nil, err
	}
	return gcm.Seal(nil, iv, plain, nil), nil
}

// aesGCMDecrypt opens an AES-GCM body with a 12-byte nonce and no AAD.
func aesGCMDecrypt(key, iv, ciphertext []byte) ([]byte, error) {
	gcm, err := newGCM12(key)
	if err != nil {
		return nil, err
	}
	return gcm.Open(nil, iv, ciphertext, nil)
}

func newGCM12(key []byte) (cipher.AEAD, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCMWithNonceSize(block, 12)
}

// zlibInflate mirrors the Kotlin Inflater: a zlib (RFC 1950) stream.
func zlibInflate(data []byte) ([]byte, error) {
	r, err := zlib.NewReader(bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	defer r.Close()
	return io.ReadAll(r)
}

func hmacSHA256(key, data []byte) []byte {
	h := hmac.New(sha256.New, key)
	h.Write(data)
	return h.Sum(nil)
}
