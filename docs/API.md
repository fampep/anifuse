# AniFuse API

## Endpoints

### `POST /v1/resolve`

Request:

```json
{
  "anilistId": 16498,
  "episode": 1,
  "audio": "sub",
  "titleHint": "Attack on Titan",
  "provider": "anihosts"
}
```

| Field | Required | Notes |
| --- | --- | --- |
| `anilistId` | yes | AniList media id, > 0 |
| `episode` | yes | 1-based episode number, > 0 |
| `audio` | no | `"sub"` (default) or `"dub"` |
| `titleHint` | no | Optional search helper for providers that need one (anizone requires it) |
| `provider` | no | Pin one bundled provider; omitted = race every healthy provider in parallel |

Success `200`:

```json
{
  "sources": [
    {
      "url": "https://cdn.example.com/ep1/master.m3u8",
      "headers": {
        "Referer": "https://embed.example.com/",
        "Origin": "https://embed.example.com",
        "User-Agent": "Mozilla/5.0"
      },
      "label": "SUB",
      "quality": "1080p",
      "audio": "AAC 2.0 · SUB",
      "qualityRating": "FHD",
      "stripBytes": 0,
      "subtitles": [
        { "url": "https://cdn.example.com/ep1/en.vtt", "label": "English", "language": "en", "mimeType": "text/vtt" }
      ]
    }
  ],
  "provider": "anihosts",
  "tookMs": 812
}
```

| Source field | Notes |
| --- | --- |
| `url` | Direct media URL (HLS master or MP4). Required; a source without it is dropped. |
| `headers` | Headers the CDN expects on the media request (Referer / Origin / User-Agent matter for most). |
| `label` | Short display label, e.g. `"SUB"` or `"1080p"`. |
| `quality` | The size the site stated, e.g. `"720p"`. Never derived from `qualityRating`. |
| `audio` | Audio the site stated, e.g. `"AAC 2.0 · SUB"`. |
| `qualityRating` | Grade the site stated, e.g. `"HD"`. |
| `stripBytes` | Leading bytes to drop on every segment (`0` = none). |
| `subtitles` | Sidecar caption tracks (`url`, `label`, `language`, `mimeType`). |

Extra response fields on top of the AniKitty protocol:

| Field | Notes |
| --- | --- |
| `provider` | The id of the provider whose answer won the race. |
| `tookMs` | Upstream resolve time (fan-out start → result). |
| `cached` | `true` when a cached answer was replayed instead of a live resolve. |

Errors — non-2xx with:

```json
{ "error": "AniHosts does not have this episode." }
```

Status mapping: `400` bad request, `404` the pinned provider (or every
provider) lacks the episode, `502` the sites were unreachable, challenged,
or named nothing playable.

An empty `sources` array is a failure. A dropped connection is worth one
retry by the caller; an HTTP error is final.

### `GET /v1/latest`

Aggregated newest-episodes feed across the bundled providers, fetched in
parallel and merged round-robin so the top of the list shows every site.

| Query | Notes |
| --- | --- |
| `provider` | Optional; pin one provider (e.g. `anihosts`). |
| `limit` | Optional; default 40, max 200. |
| `search` | Optional; keep rows whose title contains the term (case-insensitive). |
| `sort` | Optional; `mixed` (round-robin, default), `ep` (episode number, high first) or `name` (title A–Z). |

```json
{
  "latest": [
    {
      "provider": "anihosts",
      "title": "Renegade Immortal",
      "episode": 168,
      "anilistId": 0,
      "imageUrl": "https://img.example/backdrop.jpg",
      "siteUrl": "https://anihosts.xyz/anime/renegade-immortal-55809",
      "updatedAt": ""
    }
  ]
}
```

`anilistId` is set only when the site exposes it on its listing; entries are
meant to be opened at `siteUrl` or matched by title. Providers without a
feed contribute nothing and are skipped.

### `GET /v1/providers`

```json
{
  "providers": [
    {
      "id": "anihosts",
      "name": "AniHosts",
      "siteUrl": "https://anihosts.xyz",
      "bundled": true,
      "status": "ok",
      "successRate": 0.92,
      "attempts": 25
    }
  ]
}
```

The health fields are live: `status` is `ok`, `degraded` (low reachability
over the last attempts) or `down` (cooling off after repeated blocked
failures); `cooldownSeconds` appears while a provider is cooling off.

### `GET /healthz`

```json
{ "status": "ok", "providers": 11, "uptimeSeconds": 4213, "unhealthy": [] }
```

`status` is `degraded` while any provider is unhealthy; `unhealthy` lists
their ids.

### `GET /metrics`

```json
{
  "uptimeSeconds": 4213,
  "requests": { "total": 512, "resolve": 210, "resolveCacheHits": 141, "latest": 84, "latestCacheHits": 61 },
  "providers": [
    { "id": "anihosts", "status": "ok", "attempts": 60, "successes": 12, "misses": 43, "errors": 5, "successRate": 0.91 }
  ]
}
```

## Gotaku transform proxy

Gotaku encrypts its HLS playlists (AES-GCM) and XOR-obfuscates the first
bytes of every segment, so its plain URLs are not playable. AniFuse moves
the unwrap server-side: gotaku sources resolve to `…/v1/gotaku/play` URLs
which serve decrypted playlists with every URI rewritten through the proxy,
and `…/v1/gotaku/seg` streams segments with the XOR removed, honoring
`Range` requests. Both endpoints accept only `https` URLs on gotaku's own
`/p/1` and `/s/1` CDN paths pointing at public hosts, so they cannot act as
a general proxy. Nothing is stored — the bytes flow through in real time.

## Caching and transport

- **Server cache** — successful resolves replay for 10 minutes, the latest
  feed for 3 (env-tunable). Responses carry `X-AniFuse-Cache: HIT`,
  `SHARED` (served to a request that shared an in-flight upstream fetch) or
  `MISS`, plus `Cache-Control`.
- **Singleflight** — concurrent identical requests share one upstream
  fetch; the sites see one request instead of a burst.
- **ETag** — the latest feed serves an ETag; send `If-None-Match` for a
  `304` revalidation.
- **gzip** — bodies over ~512 bytes compress when the client sends
  `Accept-Encoding: gzip`.
- **CORS** — `/v1/*`, `/healthz` and `/metrics` answer
  `Access-Control-Allow-Origin: *` and handle `OPTIONS` preflights, so
  browser clients can call the API directly.

## Configuration

| Env | Default | Notes |
| --- | --- | --- |
| `PORT` | `8080` | listen port |
| `ANIFUSE_PROXY` | unset | outbound proxy for provider requests (`http`/`https` URL, optional credentials) |
| `ANIFUSE_RESOLVE_TIMEOUT` | `30s` | overall deadline for one resolve race |
| `ANIFUSE_RESOLVE_CACHE_TTL` | `10m` | how long a resolved answer is replayed |
| `ANIFUSE_LATEST_CACHE_TTL` | `3m` | how long the latest feed is cached |
| `ANIFUSE_TLS_CERT` / `ANIFUSE_TLS_KEY` | unset | certificate pair; set both to also serve HTTPS |
| `ANIFUSE_TLS_PORT` | `443` | HTTPS port when a pair is set |
| `ANIFUSE_DEMO` | unset | `1` labels the deployment as a public demo in `/healthz` |

## AniKitty install

AniFuse speaks the AniKitty extension protocol, so a deployed instance is a
first-class AniKitty source. Deploy anywhere HTTPS (see `docker-compose.yml`),
then in the app add an extension with your base URL — for example
`https://anifuse.example.com` — and AniFuse appears in the player's source list.
