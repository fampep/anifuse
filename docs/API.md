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
| `titleHint` | no | Optional search helper for providers that need one |
| `provider` | no | Pin one bundled provider; omitted = try all in order |

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
  ]
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

Errors — non-2xx with:

```json
{ "error": "AniHosts does not have this episode." }
```

Status mapping: `400` bad request, `404` the pinned provider (or every provider) lacks the episode, `502` the sites were unreachable, challenged, or named nothing playable.

An empty `sources` array is a failure. A dropped connection is worth one retry by the caller; an HTTP error is final.

### `GET /v1/providers`

```json
{
  "providers": [
    { "id": "anihosts", "name": "AniHosts", "siteUrl": "https://anihosts.xyz", "bundled": true }
  ]
}
```

### `GET /healthz`

```json
{ "status": "ok", "providers": 1 }
```

## AniKitty install

AniFuse speaks the AniKitty extension protocol, so a deployed instance is a
first-class AniKitty source. Deploy anywhere HTTPS (see `docker-compose.yml`),
then in the app add an extension with your base URL — for example
`https://anifuse.example.com` — and AniFuse appears in the player's source list.
