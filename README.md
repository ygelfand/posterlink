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

## Endpoints

| Method | Path        | Behavior                                                             |
| ------ | ----------- | -------------------------------------------------------------------- |
| `GET`  | `/poster`   | `302` → random image; `Cache-Control: no-store`; `503` while warming |
| `GET`  | `/healthz`  | JSON `{status, size, sources}` (`503` until the first pool lands)     |

Query params are ignored (the wallpanel `?ts=` cache-buster is just noise).

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

## Running

```sh
make serve                              # uses ./posterlink.yaml
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
