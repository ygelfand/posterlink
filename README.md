# posterlink

A tiny **random image** redirect service. It serves a single URL that returns a
**different image on every request**, so a screensaver — such as Home
Assistant's [wallpanel](https://github.com/j-a-n/lovelace-wallpanel) — can rotate
images with zero client-side state, the way `source.unsplash.com/random` used to.

Images come from pluggable **providers**. The flagship provider is **TMDB**
(movie posters); others (e.g. a generic passthrough to any random-image URL)
blend in by weight, so you can mix posters with other wallpapers.

## How it works

TMDB splits its JSON API (which exposes `poster_path`) from its image CDN
(`image.tmdb.org`), and offers no endpoint that returns a random poster *image*.
posterlink bridges the gap: it caches a pool of poster URLs from the API and, on
each request, **302-redirects** to a random one on the CDN. TMDB explicitly
permits direct-linking to the CDN, so no images are downloaded or hosted.

The pool is **in memory only**. It is filled at startup and rebuilt every
`refresh_interval` (default 30m); nothing is written to disk. If a refresh fails,
the last-good pool is kept so the screen never goes blank.

If you would rather have the images *on disk* — to feed a photo library such as
[Immich](https://immich.app) — `posterlink sync` mirrors them locally with EXIF
metadata attached. See [Local mirror](#local-mirror-posterlink-sync).

## Endpoints

| Method | Path        | Behavior                                                             |
| ------ | ----------- | -------------------------------------------------------------------- |
| `GET`  | `/poster`   | `302` → random image; `Cache-Control: no-store`; `503` while warming |
| `GET`  | `/healthz`  | JSON `{status, size, sources}` (`503` until the first pool lands)     |

Optional `?providers=` (comma-delimited) restricts `/poster` to specific enabled
providers, e.g. `/poster?providers=wikidata` or `/poster?providers=tmdb,steam`.
Requesting only disabled/unknown providers returns `404`. Other query params
(like the wallpanel `?ts=` cache-buster) are ignored.

## Configuration

Config resolves from (in order) a YAML file, `POSTERLINK_*` environment
variables, and flags. See [`posterlink.example.yaml`](./posterlink.example.yaml).

```yaml
port: 8088
refresh_interval: 30m
providers:
  tmdb:
    enabled: true
    weight: 1.0
    api_key: "" # or set TMDB_API_KEY
    size: w780
    pages: 3
    lists: [trending/movie/week, movie/popular, movie/now_playing]
```

Env equivalents replace `.` with `_` and add the `POSTERLINK_` prefix
(e.g. `POSTERLINK_PROVIDERS_TMDB_WEIGHT=1.0`). `TMDB_API_KEY` and
`TMDB_ACCESS_TOKEN` are also accepted directly.

## Providers

A provider implements a small interface and self-registers:

```go
type Provider interface {
    Name() string
    Weight() float64
    Fetch(ctx context.Context) ([]string, error)
}
```

Selection is **weighted across providers, uniform within** — so a `tmdb` weight
of `1.0` and an `unsplash` weight of `0.2` yields roughly an 83/17 blend
regardless of how many URLs each contributes.

Adding one is a single file under `internal/provider/<name>/` that calls
`provider.Register("<name>", New)` in `init()`, plus a blank import in
`cmd/serve.go`. Built-ins:

- **`tmdb`** — movie posters from the TMDB list endpoints. Needs `api_key` (v3)
  or `access_token` (v4).
- **`unsplash`** — photo wallpapers from the official Unsplash API
  (`/photos/random`). Needs an `access_key` (the app's Client-ID); supports
  `orientation`, `count` (≤30 per refresh), `size`, and an optional `query`.
- **`steam`** — video-game posters from Steam's 600x900 portrait capsule art.
  No API key; pulls popular app IDs from the charts/search endpoints and
  HEAD-validates each image. Supports `size` (`1x`/`2x`), `sources`, and `cc`.
- **`artic`** — famous paintings from the Art Institute of Chicago (no API key).
  Uses IIIF to crop server-side: `fit: fill` full-bleed-crops to your screen
  `aspect`; `fit: fit` letterboxes the whole work. Curated by `artists`.
- **`itunes`** — album covers (or book covers, etc.) from the iTunes Search API
  (no API key). Curated by `artist_ids` (exact via lookup) or `artists` (search
  + exact-name filter); generic over `media`/`entity`/`attribute`. Square art.

## Local mirror (`posterlink sync`)

`posterlink sync` downloads what the providers currently return into a local
directory, **one subdirectory per provider config block**, and tags every image
with EXIF and XMP metadata so a photo library indexes it properly:

```
/srv/immich/posters/
├── tmdb/
│   ├── .posterlink.json          # manifest: source URL of every file
│   ├── 1pdfLvkbY9ohJlCjQH2CZjjYVvJ-6f3a1c9d21.jpg
│   └── ...
├── itunes_jazz/
│   └── kind-of-blue-0b77e1ca04.jpg
└── wikidata/
    └── mona-lisa-by-leonardo-3c91af7e02.jpg
```

```sh
posterlink sync                       # mirror per the config
posterlink sync --dry-run             # show what would change
posterlink sync --provider tmdb -v    # one provider; others left untouched
```

### Idempotent and incremental

Runs are cheap to repeat, which is what makes this safe from cron. Each
directory keeps a `.posterlink.json` manifest keyed by a hash of the source URL,
and filenames are derived from that URL, so a sync:

- **skips** every image already on disk — no request is made for it at all
  (posters are assumed immutable, so they are never re-downloaded);
- **downloads** only URLs that are new since the last run;
- **deletes** images the providers no longer return, including whole
  directories for providers you removed from the config or set
  `cache_enabled: false` on;
- **leaves alone** anything it did not download itself. Only files recorded in
  a manifest are ever deleted, so the cache root can be shared with your own
  photos.

A provider whose API fails is **skipped, not pruned** — a transient outage never
wipes a working cache. `sync` exits non-zero in that case (and prints a
per-provider table), so cron can tell you about it.

### Metadata

Each image gets EXIF (a JPEG APP1 segment or a PNG `eXIf` chunk) and an XMP
packet; formats that cannot carry either get an `<image>.xmp` sidecar. No
`exiftool` needed.

| Field                                | Value                                          |
| ------------------------------------ | ---------------------------------------------- |
| `Make` / `Model`                     | `posterlink` / the provider name               |
| `ImageDescription`, `dc:description` | provider and list, e.g. `tmdb / movie/popular` |
| `DateTimeOriginal`, `xmp:CreateDate` | when the image was downloaded                  |
| `UserComment`, `dc:source`           | the source URL                                  |
| `dc:subject` (keywords)              | `posterlink`, the provider, the list            |

In Immich the make/model pair is searchable like a camera, so `posterlink` +
`tmdb` isolates exactly those images — handy for a smart album feeding a photo
frame. Point an [external
library](https://immich.app/docs/guides/external-library) at the cache root and
scan it after each sync.

### Config

```yaml
cache:
  dir: /srv/immich/posters # cache root (or pass --dir)
  concurrency: 4           # parallel downloads
  metadata: true           # embed EXIF/XMP
  # user_agent: posterlink/1.0   # some CDNs 403 a UA containing a URL

providers:
  tmdb:
    # ... provider settings as usual, plus:
    cache_limit: 500       # cap images kept for this provider (0 = all)
    cache_enabled: true    # false: never mirror this one (and prune its dir)
    cache_dir: posters     # subdirectory name (default: the block key)
```

From cron, hourly:

```cron
17 * * * * /usr/local/bin/posterlink sync --config /etc/posterlink/posterlink.yaml
```

## Running

```sh
make serve                              # uses ./posterlink.yaml
make sync                               # mirror to disk per ./posterlink.yaml
go run . serve --port 8088 -v           # env-driven
docker run -e TMDB_API_KEY=xxx -p 8088:8088 ghcr.io/ygelfand/posterlink:latest
```

Point the consumer at it:

```yaml
wallpanel:
  image_url: http://<host>:8088/poster
```

## Build & release

`make build` produces `./bin/posterlink`. Pushing a `v*` tag runs GoReleaser via
GitHub Actions, publishing archives, checksums, and multi-arch container images
to `ghcr.io/ygelfand/posterlink`. Non-tag pushes/PRs run tests, lint, and a
GoReleaser snapshot build check.

```sh
git tag v0.1.0 && git push origin v0.1.0
```
