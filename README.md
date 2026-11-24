# AniFuse

<div align="center">

**One API for every anime source.**

*A self-hosted Go service that bundles anime site resolvers behind a single `/v1/resolve` endpoint.*

[![Go](https://img.shields.io/badge/Go-1.23-00ADD8?logo=go&logoColor=white)](go.mod)
[![Docker](https://img.shields.io/badge/Docker-ready-2496ED?logo=docker&logoColor=white)](Dockerfile)
[![License](https://img.shields.io/badge/License-Apache_2.0-D22128)](LICENSE)
[![CI](https://github.com/fampep/anifuse/actions/workflows/ci.yml/badge.svg)](https://github.com/fampep/anifuse/actions/workflows/ci.yml)

</div>

AniFuse is a small, fast resolver API. Give it an AniList id and an episode; it asks the bundled anime sites and answers with the direct stream URL the phone or browser can play — HLS masters, MP4 files, sidecar captions, and the headers the CDNs expect. It hosts nothing; it only resolves what the sites already serve publicly.

It also speaks the [AniKitty extension protocol](https://github.com/fampep/anikitty-extensions/blob/main/spec/PROTOCOL.md), so a running instance is directly installable as an AniKitty source: paste `https://your-host.example.com` in the app and it becomes one more "Source" in the player.

## Providers

Every provider is one package under [`internal/providers`](internal/providers). The registry tries them in order when a request does not pin one, and each answers with the same `[]Source` shape.

| ID | Site | Status |
| --- | --- | --- |
| `anihosts` | anihosts.xyz | ✅ shipped |
| `anigato` | anigato.lol | ✅ shipped |
| `animegg` | animegg.org | ✅ shipped |
| `anikoto` | anikoto.cz (+ kotoplay.to fallback) | 🔜 porting |
| `gotaku` | gotaku.to | 🔜 porting (segment unwrap stays on-device) |
| `reanime` | reanime.cz | 🔜 porting |
| `mkissa` | mkissa.to | 🔜 porting |
| `kaa` | kaa.lt | 🔜 porting |
| `animex` | animex.one | 🔜 porting |
| `xanime` | xanime.app | 🔜 porting |
| `aninz` | ani.nz | 🔜 porting |
| `gogoanime` | gogoanime.by | 🔜 porting |
| `animegg` | animegg.org | ✅ shipped |
| `aniwaves` | aniwaves.tv | 🔜 porting |
| `anizone` | anizone.to | 🔜 porting |

Porting progress and per-site notes live in [PORTING.md](PORTING.md). The reference implementations are the bundled resolvers in [fampep/anikitty-extensions](https://github.com/fampep/anikitty-extensions/tree/main/providers).

## Quick start

```bash
docker compose up --build
```

or a plain container:

```bash
docker run --rm -p 8080:8080 ghcr.io/fampep/anifuse:latest
```

or from source:

```bash
go run ./cmd/api
```

`http://localhost:8080/healthz` → `{"status":"ok",...}`.

## API

### `POST /v1/resolve`

```bash
curl -sS -X POST http://localhost:8080/v1/resolve \
  -H 'content-type: application/json' \
  -d '{"anilistId":16498,"episode":1,"audio":"sub"}'
```

```json
{
  "sources": [
    {
      "url": "https://cdn.example.com/ep1/master.m3u8",
      "headers": { "Referer": "https://example.com/", "User-Agent": "Mozilla/5.0" },
      "label": "SUB",
      "subtitles": [
        { "url": "https://cdn.example.com/ep1/en.vtt", "label": "English", "language": "en", "mimeType": "text/vtt" }
      ]
    }
  ]
}
```

| Field | Notes |
| --- | --- |
| `anilistId` | required, > 0 |
| `episode` | required, > 0 |
| `audio` | `"sub"` (default) or `"dub"` |
| `titleHint` | optional search helper |
| `provider` | optional; pin one bundled provider. Omitted = failover across all. |

Errors are non-2xx with `{"error":"human message"}`. An empty `sources` array is a failure. Full protocol in [docs/API.md](docs/API.md).

### `GET /v1/providers`

Lists the bundled providers (`id`, `name`, `siteUrl`).

### `GET /healthz`

Liveness: `{"status":"ok","providers":N}`.

## Configuration

| Env | Default | Notes |
| --- | --- | --- |
| `PORT` | `8080` | listen port |

## Design

- **Stdlib only** — zero runtime dependencies; one static binary.
- **Registry pattern** — a provider is a `Resolve(ctx, Request) ([]Source, error)`; adding a site is one package plus one line in `internal/providers/providers.go`.
- **Typed failures** — `missing` (404), `blocked` / `unplayable` (502) so clients can tell "not here" from "try again later".
- **Bounded everything** — per-request timeouts, response body caps, redirect caps, `ReadHeaderTimeout`.
- **Container-safe** — runs as `nobody`, read-only rootfs, healthcheck included.

## Disclaimer

AniFuse is not affiliated with AniList, any studio, or any of the resolved sites. It hosts no video: a stream plays only when a site serves it, and the sites control those streams. You are responsible for the laws where you are and for the terms of every site it queries. Use at your own risk.

## License

Apache-2.0 — see [LICENSE](LICENSE).
