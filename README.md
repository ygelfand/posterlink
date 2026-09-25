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

| Method | Path          | Behavior                                                                   |
| ------ | ------------- | -------------------------------------------------------------------------- |
| `GET`  | `/poster`     | `302` → random image; `Cache-Control: no-store`; `503` while warming       |
| `GET`  | `/poster.jpg` | the image **bytes**, fetched and validated server-side; `502` if none works |
| `GET`  | `/healthz`    | JSON `{status, size, sources, invalid}` (`503` until the first pool lands)  |

Optional `?providers=` (comma-delimited) restricts either poster endpoint to
specific enabled providers, e.g. `/poster?providers=wikidata` or
`/poster.jpg?providers=tmdb,steam`. Requesting only disabled/unknown providers
returns `404`. Other query params (like the wallpanel `?ts=` cache-buster) are
ignored.

### `/poster.jpg` — serve instead of redirect

For clients that want a real image at a real image URL rather than a redirect,
`/poster.jpg` fetches the picked image itself, follows redirects (Wikimedia's
`Special:FilePath` needs it), and **validates** the result: the body has to
sniff as an image, and JPEG/PNG/GIF must decode. A non-image, a truncated file
or a 404 is not served — posterlink picks another image and tries again, up to
three times, then returns `502`.

Failures that are permanent (`4xx`, a corrupt body) are remembered and those
URLs are skipped until the next pool refresh clears the list; `/healthz` reports
the count as `invalid`. Timeouts and `5xx` are treated as transient and not
remembered.

The response carries the real content type (a PNG source is served as
`image/png` — nothing is transcoded), plus `X-Poster-Provider` and
`X-Poster-Source` headers naming where the image came from. EXIF and XMP
describing the image are embedded on the way through, as with the local mirror
below.

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
    Fetch(ctx context.Context) ([]Image, error)
}

type Image struct {
    URL, Title, Creator, Date, Description string
}
```

Everything but `URL` is optional; whatever a source knows becomes the image's
caption and EXIF. A provider whose metadata costs extra requests can also
implement `Enrich(ctx, []Image) []Image`, which `serve` runs in the background
(the pool is already serving; the result is published when it lands) and which
`sync` and `preview` wait for.

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
  Titles come free from the store search HTML; the charts endpoint returns app
  IDs only, so those names are resolved through `appdetails` and cached in
  memory for the life of the process.
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
│   ├── dune-part-two-6f3a1c9d21.jpg
│   └── ...
├── itunes_jazz/
│   └── kind-of-blue-f226ee03d5.jpg
└── wikidata/
    └── mona-lisa-fa08baa2b8.jpg
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

The description is what the source actually knows about the picture, not the
plumbing that fetched it:

| provider   | `ImageDescription`                  |
| ---------- | ----------------------------------- |
| `tmdb`     | Dune: Part Two (2024)               |
| `itunes`   | Kind of Blue - Miles Davis (1959)   |
| `artic`    | Water Lilies - Claude Monet (1906)  |
| `wikidata` | Mona Lisa - Leonardo da Vinci (1503) |
| `unsplash` | foggy pine forest - Jane Doe (2019) |
| `steam`    | Hades                               |

| Field                                | Value                                             |
| ------------------------------------ | ------------------------------------------------- |
| `ImageDescription`, `dc:description` | the caption above                                 |
| `DateTimeOriginal`, `xmp:CreateDate` | the **work's own** date, omitted when unknown     |
| `UserComment`, `dc:source`           | the source URL                                    |

Because the date is the work's, *Kind of Blue* lands in 1959 on an Immich
timeline and *Mona Lisa* in 1503, rather than all of them bunching at the
moment you happened to sync. Point an [external
library](https://immich.app/docs/guides/external-library) at the cache root and
scan it after each sync; the captions are searchable there.

EXIF string values are 8-bit, so they are written as Latin-1 — `Eugène` is one
byte per character, and anything Latin-1 cannot represent is dropped. The XMP
packet is UTF-8 and keeps the full text either way.

Filenames come from the title too, so the cache reads as a library:

```
tmdb/dune-part-two-6f3a1c9d21.jpg
itunes_jazz/kind-of-blue-f226ee03d5.jpg
wikidata/mona-lisa-fa08baa2b8.jpg
```

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
