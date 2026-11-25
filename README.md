# AniFuse

<div align="center">

**One API for every anime source.**

*A self-hosted Go service that bundles anime site resolvers behind a single `/v1/resolve` endpoint.*

> **Rebrand.** AniFuse is the new name of the **AniKitty** ecosystem: the
> Android app, this resolver API and the extension protocol all carry the
> AniFuse brand. The app repo is
> [anikitty-android](https://github.com/anikitty/anikitty-android) and the
> extension catalog lives in
> [anikitty-extensions](https://github.com/fampep/anikitty-extensions).

[![Go](https://img.shields.io/badge/Go-1.23-00ADD8?logo=go&logoColor=white)](go.mod)
[![Docker](https://img.shields.io/badge/Docker-ready-2496ED?logo=docker&logoColor=white)](Dockerfile)
[![License](https://img.shields.io/badge/License-Apache_2.0-D22128)](LICENSE)
[![CI](https://github.com/fampep/anifuse/actions/workflows/ci.yml/badge.svg)](https://github.com/fampep/anifuse/actions/workflows/ci.yml)

</div>

AniFuse is a small, fast resolver API. Give it an AniList id and an episode; it asks the bundled anime sites and answers with the direct stream URL the phone or browser can play — HLS masters, MP4 files, sidecar captions, and the headers the CDNs expect. It hosts nothing; it only resolves what the sites already serve publicly.

It also speaks the [AniKitty extension protocol](https://github.com/fampep/anikitty-extensions/blob/main/spec/PROTOCOL.md), so a running instance is directly installable as an AniKitty source: paste `https://your-host.example.com` in the app and it becomes one more "Source" in the player. AniFuse is now the **main AniKitty extension** — the app ships it first-class (see [`anikitty-extension.json`](anikitty-extension.json) at the repo root), while the old per-site on-device resolvers are deprecated.

## Providers

Every provider is one package under [`internal/providers`](internal/providers). The registry tries them in order when a request does not pin one, and each answers with the same `[]Source` shape.

| ID | Site | Status |
| --- | --- | --- |
| `anihosts` | anihosts.xyz | ✅ shipped |
| `anigato` | anigato.lol | ✅ shipped |
| `animegg` | animegg.org | ✅ shipped |
| `anikoto` | anikoto.cz (+ kotoplay.to fallback) | ✅ shipped |
| `gotaku` | gotaku.to | ⛔ disabled (its CDN blocks datacenter IPs, so the hosted API cannot play it; the transform-proxy plumbing stays for when that changes) |
| `reanime` | reanime.cz | ⛔ on-device only (WASM crypto unwraps each segment on the player) |
| `mkissa` | mkissa.to | ✅ shipped |
| `kaa` | kaa.lt | ✅ shipped |
| `animex` | animex.one | ✅ shipped |
| `xanime` | xanime.app | ✅ shipped |
| `aninz` | ani.nz | ✅ shipped |
| `gogoanime` | gogoanime.by | ✅ shipped |
| `aniwaves` | aniwaves.tv | ✅ shipped |
| `anizone` | anizone.to | ✅ shipped |

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

## Live demo

A demo deployment of this exact repository runs at
**<https://143-246-203-212.sslip.io>** (HTTPS via Let's Encrypt, auto-renewed).
Its landing page is the playground: it resolves real episodes against the
live sites, so you can watch the race, the cache, and the provider health
board work before self-hosting anything. Treat it as a demo, not a service:
availability is best-effort, and it resolves nothing you could not scrape
yourself.

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
  ],
  "provider": "anihosts",
  "tookMs": 812
}
```

| Field | Notes |
| --- | --- |
| `anilistId` | required, > 0 |
| `episode` | required, > 0 |
| `audio` | `"sub"` (default) or `"dub"` |
| `titleHint` | optional search helper |
| `provider` | optional; pin one bundled provider. Omitted = ask every healthy provider in parallel and collect all their streams, ranked highest quality first. |

The extra `provider` and `tookMs` fields name the provider of the
top-ranked stream and the upstream resolve time; `cached: true` appears
when a cached answer was replayed. The `sources` array is every stream
every provider found — deduplicated and ranked highest quality first.
Errors are non-2xx with `{"error":"human message"}`. An empty
`sources` array is a failure. Full protocol in [docs/API.md](docs/API.md).

### `GET /v1/latest`

The aggregated newest-episodes feed: every provider that publishes one contributes rows. Optional `?provider=` pins one site, `?limit=` caps the total (default 40, max 200), `?search=` filters titles case-insensitively, `?sort=ep|name|mixed` orders the rows.

```bash
curl -sS "http://localhost:8080/v1/latest"
```

```json
{
  "latest": [
    {
      "provider": "anihosts",
      "title": "Renegade Immortal",
      "episode": 168,
      "imageUrl": "https://img.example/backdrop.jpg",
      "siteUrl": "https://anihosts.xyz/anime/renegade-immortal-55809"
    }
  ]
}
```

The landing page renders this feed live under **Latest episodes**.

### Outbound proxy

Every provider request can travel through one outbound proxy. Set the
environment variable and restart:

```bash
ANIFUSE_PROXY="http://user:password@host:port" ./anifuse
```

`http` and `https` proxy URLs are supported; unset (or `none`) falls back to
the standard `HTTP_PROXY`/`HTTPS_PROXY` environment variables.

### `GET /v1/providers`

Lists the bundled providers with live health: `id`, `name`, `siteUrl`,
`status` (`ok` | `degraded` | `down`), `successRate`, `attempts`, and
`cooldownSeconds` while a site cools off after repeated blocked failures.

### `GET /healthz`

Liveness: `{"status","providers","uptimeSeconds","unhealthy"}`.

### `GET /metrics`

Request counters and per-provider stats for dashboards.

## Speed

- **Collecting fan-out** — an unpinned resolve asks every healthy provider
  at once and merges every stream it finds, ranked highest quality first.
  A short settle window after the first success keeps the answer fast even
  when one site is slow.
- **TTL cache + singleflight** — repeat resolves replay for 10 minutes,
  the latest feed for 3, and bursts of identical requests share one
  upstream fetch. `X-AniFuse-Cache: HIT|SHARED|MISS` tells you which.
- **Health memory** — providers that fail blocked three times in a row
  cool off for a minute instead of slowing every race.
- **gzip + ETag + CORS** — responses compress when asked, revalidate with
  `If-None-Match`, and the `/v1/*` endpoints are open to browser clients.

## Configuration

| Env | Default | Notes |
| --- | --- | --- |
| `PORT` | `8080` | listen port |
| `ANIFUSE_RESOLVE_TIMEOUT` | `30s` | overall deadline for one resolve race |
| `ANIFUSE_RESOLVE_CACHE_TTL` | `10m` | how long a resolved answer is replayed |
| `ANIFUSE_LATEST_CACHE_TTL` | `3m` | how long the latest feed is cached |
| `ANIFUSE_TLS_CERT` / `ANIFUSE_TLS_KEY` | unset | certificate pair; set both to also serve HTTPS |
| `ANIFUSE_TLS_PORT` | `443` | HTTPS port when a pair is set |
| `ANIFUSE_DEMO` | unset | `1` labels the deployment as a public demo in `/healthz` |

## Design

- **Stdlib only** — zero runtime dependencies; one static binary.
- **Registry pattern** — a provider is a `Resolve(ctx, Request) ([]Source, error)`; adding a site is one package plus one line in `internal/providers/providers.go`.- **Typed failures** — `missing` (404), `blocked` / `unplayable` (502) so clients can tell "not here" from "try again later".
- **Bounded everything** — per-request timeouts, response body caps, redirect caps, `ReadHeaderTimeout`.
- **Container-safe** — runs as `nobody`, read-only rootfs, healthcheck included.
- **Transform proxy for gotaku** — gotaku's CDN encrypts its HLS playlists (AES-GCM) and XORs the head of every segment. AniFuse carries the full unwrap (`/v1/gotaku/play`, `/v1/gotaku/seg`) but the provider is **disabled by default**: the CDN refuses datacenter IPs, so a hosted API cannot play it. The plumbing stays for self-hosters on residential connections.

## Disclaimer

AniFuse is not affiliated with AniList, any studio, or any of the resolved sites. It hosts no video: a stream plays only when a site serves it, and the sites control those streams. You are responsible for the laws where you are and for the terms of every site it queries. Use at your own risk.

## License

Apache-2.0 — see [LICENSE](LICENSE).
