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
| `anikoto` | `providers/anikoto/AnikotoServer.kt` | 🔜 | embed + MegaPlay crypto + kotoplay.to fallback |
| `gotaku` | `providers/gotaku/GotakuServer.kt` | 🔜 | master is fine to resolve, but the segment unwrap (`gotakuObf`) is a player-side transform and cannot travel over this protocol — document and skip |
| `reanime` | `providers/reanime/ReanimeServer.kt` | 🔜 | |
| `mkissa` | `providers/mkissa/MkissaServer.kt` | 🔜 | skip iframe hosts, direct file only |
| `kaa` | `providers/kaa/KaaServer.kt` | 🔜 | |
| `animex` | `providers/animex/AnimexServer.kt` | 🔜 | |
| `xanime` | `providers/xanime/XanimeServer.kt` | 🔜 | |
| `aninz` | `providers/aninz/AniNzServer.kt` | 🔜 | |
| `gogoanime` | `providers/gogoanime/GogoAnimeServer.kt` | 🔜 | |
| `aniwaves` | `providers/aniwaves/AniwavesServer.kt` | 🔜 | |
| `anizone` | `providers/anizone/AnizoneServer.kt` | 🔜 | |

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
