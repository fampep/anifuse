# Porting status

Each provider is a Go port of a bundled resolver from
[fampep/anikitty-extensions](https://github.com/fampep/anikitty-extensions/tree/main/providers).
The Kotlin file is the reference; the Go package must reproduce its resolve
flow — endpoints, parsing, URL validation, and the playable headers — and ship
parser tests that mirror its Kotlin test.

| ID | Kotlin reference | Status | Notes |
| --- | --- | --- | --- |
| `anihosts` | `providers/anihosts/AniHostsServer.kt` | ✅ shipped | packed jwplayer unwrap + multi-audio master |
| `anigato` | `providers/anigato/AnigatoServer.kt` | ✅ | `/api/episodes/{id}`, `/api/watch/{id}/sub|dub/{ep}` |
| `animegg` | `providers/animegg/AnimeggServer.kt` | ✅ | search → series → episode → embed → `/play/` file |
| `anikoto` | `providers/anikoto/AnikotoServer.kt` | ✅ | MegaPlay AES-CBC getSources (keys scraped from newclient.min.js, zero-padded like sourceEncPadKeyBytes) + PNG strip bytes; kotoplay.to /data fallback lane; home widget latest feed |
| `gotaku` | `providers/gotaku/GotakuServer.kt` | ⛔ disabled | port + crypto + /v1/gotaku proxy shipped, but the CDN blocks datacenter IPs, so the hosted API cannot play it; stays for residential self-hosts |
| `reanime` | `providers/reanime/ReanimeServer.kt` | ⛔ on-device only | Flix WASM crypto; playlist and segments are unwrapped on the phone |
| `mkissa` | `providers/mkissa/MkissaServer.kt` | ✅ | aa-crypto boot/query lane ported (HMAC + AES-GCM); direct MP4/HLS plays |
| `kaa` | `providers/kaa/KaaServer.kt` | ✅ | vidstream master on hls.krussdomi.com |
| `animex` | `providers/animex/AnimexServer.kt` | ✅ | GraphQL search + REST sources, HLS even as deeznuts.txt |
| `xanime` | `providers/xanime/XanimeServer.kt` | ✅ | AES-GCM request envelope ported; master is HLS, variants a-index-….ico |
| `aninz` | `providers/aninz/AniNzServer.kt` | ✅ | settlar bootstrap + session token + media.settlar.io master |
| `gogoanime` | `providers/gogoanime/GogoAnimeServer.kt` | ✅ | MegaPlay / MegaVid HLS with title scoring |
| `aniwaves` | `providers/aniwaves/AniwavesServer.kt` | ✅ | ajax server list + embed getSources playlist |
| `anizone` | `providers/anizone/AnizoneServer.kt` | ✅ | embedded items search + vidstackPlayer master/captions, plus the legacy media-player/setVideo/<track> layout as fallback |

## Adding a provider

1. Create `internal/providers/{id}/` with `{id}.go` implementing
   `providers.Provider` and `{id}_test.go` with parser tests.
2. Register it in `internal/providers/providers.go` (`NewRegistry`).
3. Update the table above and the README table.

## Conventions

- Stdlib only. `internal/httpx` for fetches, `internal/protocol` for types.
- Typed errors: `providers.Missing / Blocked / Unplayable`.
- Validate every returned URL: HTTPS only, no userinfo, no `..` / `\`.
- Media requests always carry `Referer`, `Origin`, and the browser UA.
- Never return HTML pages — only direct media URLs.
