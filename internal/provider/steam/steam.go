// Package steam implements an image provider backed by Steam's library capsule
// art (the 600x900, 2:3 portrait "poster" each game has).
//
// It needs no API key: popular app IDs come from the public charts and store
// featured endpoints, and images come straight off the Steam CDN. Not every
// app has portrait art (old titles, hardware listings), so candidate image
// URLs are HEAD-validated and only the ones that exist are pooled.
package steam

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
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

	client *http.Client
}

// New constructs a Steam provider from its configuration subtree.
func New(opts provider.Options) (provider.Provider, error) {
	size := opts.String("size", "2x")
	file, ok := sizeFiles[size]
	if !ok {
		return nil, fmt.Errorf("steam: invalid size %q (want 1x or 2x)", size)
	}
	return &Steam{
		Base:     provider.NewBase("steam", opts),
		file:     file,
		sources:  opts.Strings("sources", []string{"most_played", "top_sellers"}),
		cc:       opts.String("cc", "us"),
		count:    opts.Int("count", 100),
		validate: opts.Bool("validate", true),
		client:   &http.Client{Timeout: 10 * time.Second},
	}, nil
}

// Fetch collects app IDs from all configured sources (deduped) and returns the
// existing portrait-art URLs.
func (s *Steam) Fetch(ctx context.Context) ([]string, error) {
	seen := make(map[int]struct{})
	var appids []int
	var firstErr error

	for _, src := range s.sources {
		ids, err := s.collect(ctx, src)
		if err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		for _, id := range ids {
			if _, dup := seen[id]; dup {
				continue
			}
			seen[id] = struct{}{}
			appids = append(appids, id)
		}
	}

	urls := s.imagesFor(ctx, appids)
	if len(urls) == 0 && firstErr != nil {
		return nil, firstErr
	}
	return urls, nil
}

// Preview returns one labeled group per source.
func (s *Steam) Preview(ctx context.Context) ([]provider.Group, error) {
	var groups []provider.Group
	var firstErr error

	for _, src := range s.sources {
		ids, err := s.collect(ctx, src)
		if err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		groups = append(groups, provider.Group{Label: "steam/" + src, URLs: s.imagesFor(ctx, ids)})
	}

	if len(groups) == 0 && firstErr != nil {
		return nil, firstErr
	}
	return groups, nil
}

func (s *Steam) collect(ctx context.Context, source string) ([]int, error) {
	switch source {
	case "most_played":
		return s.mostPlayed(ctx)
	case "top_sellers":
		return s.search(ctx, "topsellers")
	default:
		return nil, fmt.Errorf("steam: unknown source %q (want most_played or top_sellers)", source)
	}
}

func (s *Steam) mostPlayed(ctx context.Context) ([]int, error) {
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
	ids := make([]int, 0, len(body.Response.Ranks))
	for _, r := range body.Response.Ranks {
		ids = append(ids, r.Appid)
	}
	return ids, nil
}

// search pulls app IDs from the store search results for a given filter (e.g.
// "topsellers"). The endpoint returns app IDs embedded in an HTML fragment, so
// we extract them by regex rather than JSON-decoding (the payload contains
// unescaped control characters).
func (s *Steam) search(ctx context.Context, filter string) ([]int, error) {
	u := fmt.Sprintf("%s?filter=%s&cc=%s&l=en&start=0&count=%d&infinite=1&json=1",
		searchURL, url.QueryEscape(filter), url.QueryEscape(s.cc), s.count)

	body, err := s.getRaw(ctx, u)
	if err != nil {
		return nil, err
	}

	seen := make(map[int]struct{})
	var ids []int
	for _, m := range appidRe.FindAllStringSubmatch(string(body), -1) {
		var id int
		if _, err := fmt.Sscan(m[1], &id); err != nil || id <= 0 {
			continue
		}
		if _, dup := seen[id]; dup {
			continue
		}
		seen[id] = struct{}{}
		ids = append(ids, id)
	}
	return ids, nil
}

// imagesFor builds the capsule URL for each app ID and, unless validation is
// disabled, keeps only the ones that actually exist (HEAD == 200). Order is
// preserved.
func (s *Steam) imagesFor(ctx context.Context, appids []int) []string {
	urls := make([]string, len(appids))
	keep := make([]bool, len(appids))

	sem := make(chan struct{}, 10)
	var wg sync.WaitGroup
	for i, id := range appids {
		urls[i] = fmt.Sprintf("%s%d/%s", imgBase, id, s.file)
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

	out := make([]string, 0, len(appids))
	for i := range appids {
		if keep[i] {
			out = append(out, urls[i])
		}
	}
	return out
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
