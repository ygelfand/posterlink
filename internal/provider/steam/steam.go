// Package steam implements an image provider backed by Steam's library capsule
// art (the 600x900, 2:3 portrait "poster" each game has).
//
// It needs no API key: popular app IDs come from the public charts and store
// featured endpoints, and images come straight off the Steam CDN. Not every
// app has portrait art (old titles, hardware listings), so candidate image
// URLs are HEAD-validated and only the ones that exist are pooled.
package steam

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	stdhtml "html"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/ygelfand/posterlink/internal/provider"
)

const (
	chartsURL = "https://api.steampowered.com/ISteamChartsService/GetMostPlayedGames/v1/"
	searchURL = "https://store.steampowered.com/search/results/"
	imgBase   = "https://cdn.cloudflare.steamstatic.com/steam/apps/"
)

// appidRe extracts app IDs from the store search results HTML (handles both
// bare and JSON-escaped quotes).
var appidRe = regexp.MustCompile(`data-ds-appid=\\?"?(\d+)`)

// rowRe pairs each search result's app ID with its title. The payload is a
// JSON-escaped HTML fragment, so quotes and the closing slash carry backslashes.
var rowRe = regexp.MustCompile(`data-ds-appid=\\?"?(\d+)[\s\S]*?<span class=\\?"title\\?">([^<]+)<\\?/span>`)

// imgAppidRe recovers the app ID from a capsule URL.
var imgAppidRe = regexp.MustCompile(`/apps/(\d+)/`)

const detailsURL = "https://store.steampowered.com/api/appdetails"

// sizeFiles maps the configured size to the CDN filename. Both are 2:3.
var sizeFiles = map[string]string{
	"1x": "library_600x900.jpg",    // 600x900
	"2x": "library_600x900_2x.jpg", // 1200x1800
}

func init() {
	provider.Register("steam", New)
}

// Steam is the provider implementation.
type Steam struct {
	provider.Base

	file     string
	sources  []string
	cc       string
	count    int
	validate bool

	names  *nameCache
	client *http.Client
}

// nameCache holds app names, which never change, for the lifetime of the
// process. The charts endpoint returns IDs only, so names for those apps cost
// one appdetails request each; Enrich fills them in.
type nameCache struct {
	mu sync.RWMutex
	m  map[int]string
}

func newNameCache() *nameCache { return &nameCache{m: make(map[int]string)} }

func (c *nameCache) get(id int) string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.m[id]
}

func (c *nameCache) set(id int, name string) {
	if name == "" {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.m[id] = name
}

// app is one Steam application and what the source knew about it.
type app struct {
	id   int
	name string
}

// New constructs a Steam provider from its configuration subtree.
func New(name string, opts provider.Options) (provider.Provider, error) {
	size := opts.String("size", "2x")
	file, ok := sizeFiles[size]
	if !ok {
		return nil, fmt.Errorf("steam: invalid size %q (want 1x or 2x)", size)
	}
	return &Steam{
		Base:     provider.NewBase(name, opts),
		file:     file,
		sources:  opts.Strings("sources", []string{"most_played", "top_sellers"}),
		cc:       opts.String("cc", "us"),
		count:    opts.Int("count", 100),
		validate: opts.Bool("validate", true),
		names:    newNameCache(),
		client:   &http.Client{Timeout: 10 * time.Second},
	}, nil
}

// Fetch collects app IDs from all configured sources (deduped) and returns the
// existing portrait-art URLs.
func (s *Steam) Fetch(ctx context.Context) ([]provider.Image, error) {
	seen := make(map[int]struct{})
	var apps []app
	var firstErr error

	for _, src := range s.sources {
		got, err := s.collect(ctx, src)
		if err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		for _, a := range got {
			if _, dup := seen[a.id]; dup {
				continue
			}
			seen[a.id] = struct{}{}
			apps = append(apps, a)
		}
	}

	images := s.imagesFor(ctx, apps)
	if len(images) == 0 && firstErr != nil {
		return nil, firstErr
	}
	return images, nil
}

// Preview returns one labeled group per source.
func (s *Steam) Preview(ctx context.Context) ([]provider.Group, error) {
	var groups []provider.Group
	var firstErr error

	for _, src := range s.sources {
		apps, err := s.collect(ctx, src)
		if err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		groups = append(groups, provider.Group{Label: "steam/" + src, Images: s.imagesFor(ctx, apps)})
	}

	if len(groups) == 0 && firstErr != nil {
		return nil, firstErr
	}
	return groups, nil
}

func (s *Steam) collect(ctx context.Context, source string) ([]app, error) {
	switch source {
	case "most_played":
		return s.mostPlayed(ctx)
	case "top_sellers":
		return s.search(ctx, "topsellers")
	default:
		return nil, fmt.Errorf("steam: unknown source %q (want most_played or top_sellers)", source)
	}
}

// mostPlayed returns the charts app IDs. The endpoint carries no names.
func (s *Steam) mostPlayed(ctx context.Context) ([]app, error) {
	var body struct {
		Response struct {
			Ranks []struct {
				Appid int `json:"appid"`
			} `json:"ranks"`
		} `json:"response"`
	}
	if err := s.getJSON(ctx, chartsURL, &body); err != nil {
		return nil, err
	}
	apps := make([]app, 0, len(body.Response.Ranks))
	for _, r := range body.Response.Ranks {
		apps = append(apps, app{id: r.Appid})
	}
	return apps, nil
}

// search pulls app IDs from the store search results for a given filter (e.g.
// "topsellers"). The endpoint returns app IDs embedded in an HTML fragment, so
// we extract them by regex rather than JSON-decoding (the payload contains
// unescaped control characters).
func (s *Steam) search(ctx context.Context, filter string) ([]app, error) {
	u := fmt.Sprintf("%s?filter=%s&cc=%s&l=en&start=0&count=%d&infinite=1&json=1",
		searchURL, url.QueryEscape(filter), url.QueryEscape(s.cc), s.count)

	body, err := s.getRaw(ctx, u)
	if err != nil {
		return nil, err
	}
	html := string(body)

	names := make(map[int]string)
	for _, m := range rowRe.FindAllStringSubmatch(html, -1) {
		id, err := strconv.Atoi(m[1])
		if err != nil || id <= 0 {
			continue
		}
		name := strings.TrimSpace(stdhtml.UnescapeString(m[2]))
		names[id] = name
		s.names.set(id, name)
	}

	seen := make(map[int]struct{})
	var apps []app
	for _, m := range appidRe.FindAllStringSubmatch(html, -1) {
		id, err := strconv.Atoi(m[1])
		if err != nil || id <= 0 {
			continue
		}
		if _, dup := seen[id]; dup {
			continue
		}
		seen[id] = struct{}{}
		apps = append(apps, app{id: id, name: names[id]})
	}
	return apps, nil
}

// imagesFor builds the capsule URL for each app ID and, unless validation is
// disabled, keeps only the ones that actually exist (HEAD == 200). Order is
// preserved.
func (s *Steam) imagesFor(ctx context.Context, apps []app) []provider.Image {
	urls := make([]string, len(apps))
	keep := make([]bool, len(apps))

	sem := make(chan struct{}, 10)
	var wg sync.WaitGroup
	for i, a := range apps {
		urls[i] = fmt.Sprintf("%s%d/%s", imgBase, a.id, s.file)
		if !s.validate {
			keep[i] = true
			continue
		}
		wg.Add(1)
		sem <- struct{}{}
		go func(i int, u string) {
			defer wg.Done()
			defer func() { <-sem }()
			keep[i] = s.exists(ctx, u)
		}(i, urls[i])
	}
	wg.Wait()

	out := make([]provider.Image, 0, len(apps))
	for i, a := range apps {
		if !keep[i] {
			continue
		}
		out = append(out, provider.Image{
			URL:   urls[i],
			Title: cmp.Or(a.name, s.names.get(a.id)),
		})
	}
	return out
}

// Enrich fills in the titles the charts endpoint does not carry, one
// appdetails request per app, and remembers them for the rest of the process.
func (s *Steam) Enrich(ctx context.Context, images []provider.Image) []provider.Image {
	type todo struct {
		index int
		id    int
	}
	var work []todo
	for i, img := range images {
		if img.Title != "" {
			continue
		}
		id := appidFromURL(img.URL)
		if id == 0 {
			continue
		}
		if name := s.names.get(id); name != "" {
			images[i].Title = name
			continue
		}
		work = append(work, todo{index: i, id: id})
	}
	if len(work) == 0 {
		return images
	}

	sem := make(chan struct{}, 4)
	var wg sync.WaitGroup
	var mu sync.Mutex
	throttled := false

	for _, w := range work {
		if ctx.Err() != nil {
			break
		}
		mu.Lock()
		stop := throttled
		mu.Unlock()
		if stop {
			break
		}

		wg.Add(1)
		sem <- struct{}{}
		go func(w todo) {
			defer wg.Done()
			defer func() { <-sem }()

			name, limited := s.appName(ctx, w.id)
			mu.Lock()
			defer mu.Unlock()
			if limited {
				throttled = true
				return
			}
			if name != "" {
				s.names.set(w.id, name)
				images[w.index].Title = name
			}
		}(w)
	}
	wg.Wait()
	return images
}

// appName resolves one app's name. The response is keyed by an ID that does
// not always match the one requested, so the single entry is taken as-is.
func (s *Steam) appName(ctx context.Context, id int) (string, bool) {
	u := fmt.Sprintf("%s?appids=%d&filters=basic&l=en", detailsURL, id)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return "", false
	}
	resp, err := s.client.Do(req)
	if err != nil {
		return "", false
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode == http.StatusTooManyRequests {
		return "", true
	}
	if resp.StatusCode != http.StatusOK {
		return "", false
	}

	var body map[string]struct {
		Success bool `json:"success"`
		Data    struct {
			Name string `json:"name"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return "", false
	}
	for _, entry := range body {
		if entry.Success {
			return entry.Data.Name, false
		}
	}
	return "", false
}

func appidFromURL(u string) int {
	m := imgAppidRe.FindStringSubmatch(u)
	if m == nil {
		return 0
	}
	id, err := strconv.Atoi(m[1])
	if err != nil {
		return 0
	}
	return id
}

func (s *Steam) exists(ctx context.Context, u string) bool {
	req, err := http.NewRequestWithContext(ctx, http.MethodHead, u, nil)
	if err != nil {
		return false
	}
	resp, err := s.client.Do(req)
	if err != nil {
		return false
	}
	_ = resp.Body.Close()
	return resp.StatusCode == http.StatusOK
}

func (s *Steam) getRaw(ctx context.Context, u string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	resp, err := s.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("steam: %s: unexpected status %s", u, resp.Status)
	}
	return io.ReadAll(resp.Body)
}

func (s *Steam) getJSON(ctx context.Context, u string, v any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")

	resp, err := s.client.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("steam: %s: unexpected status %s", u, resp.Status)
	}
	return json.NewDecoder(resp.Body).Decode(v)
}
