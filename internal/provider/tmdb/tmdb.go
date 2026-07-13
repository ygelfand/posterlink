// Package tmdb implements a poster image provider backed by The Movie Database.
//
// TMDB splits its JSON API (which exposes poster_path values) from its image
// CDN (image.tmdb.org). There is no endpoint that returns a random poster
// image, so this provider pulls a pool of poster paths from the list endpoints
// and builds direct CDN URLs the server can 302-redirect to.
package tmdb

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/ygelfand/posterlink/internal/provider"
)

const (
	defaultAPIBase   = "https://api.themoviedb.org/3/"
	defaultImageBase = "https://image.tmdb.org/t/p/"
	defaultSize      = "w780"
)

var defaultLists = []string{
	"trending/movie/week",
	"movie/popular",
	"movie/now_playing",
}

func init() {
	provider.Register("tmdb", New)
}

// TMDB is the provider implementation.
type TMDB struct {
	provider.Base

	apiKey      string
	accessToken string
	apiBase     string
	imageBase   string
	size        string
	lists       []string
	pages       int

	client *http.Client
}

// New constructs a TMDB provider from its configuration subtree.
func New(opts provider.Options) (provider.Provider, error) {
	apiKey := opts.String("api_key", "")
	token := opts.String("access_token", "")
	if apiKey == "" && token == "" {
		return nil, fmt.Errorf("tmdb: one of api_key or access_token is required")
	}

	return &TMDB{
		Base:        provider.NewBase("tmdb", opts),
		apiKey:      apiKey,
		accessToken: token,
		apiBase:     strings.TrimRight(opts.String("api_base", defaultAPIBase), "/") + "/",
		imageBase:   strings.TrimRight(opts.String("image_base", defaultImageBase), "/") + "/",
		size:        opts.String("size", defaultSize),
		lists:       opts.Strings("lists", defaultLists),
		pages:       opts.Int("pages", 3),
		client:      &http.Client{Timeout: 10 * time.Second},
	}, nil
}

type listResponse struct {
	Results []struct {
		PosterPath string `json:"poster_path"`
	} `json:"results"`
}

// Fetch pulls every configured list and returns CDN URLs deduped across lists.
func (t *TMDB) Fetch(ctx context.Context) ([]string, error) {
	seen := make(map[string]struct{})
	var urls []string
	var firstErr error

	for _, list := range t.lists {
		got, err := t.fetchList(ctx, list)
		if err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		for _, u := range got {
			if _, dup := seen[u]; dup {
				continue
			}
			seen[u] = struct{}{}
			urls = append(urls, u)
		}
	}

	if len(urls) == 0 && firstErr != nil {
		return nil, firstErr
	}
	return urls, nil
}

// Preview returns one labeled group per configured list, deduped within each
// list but not across lists (so overlap between lists is visible).
func (t *TMDB) Preview(ctx context.Context) ([]provider.Group, error) {
	var groups []provider.Group
	var firstErr error

	for _, list := range t.lists {
		urls, err := t.fetchList(ctx, list)
		if err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		groups = append(groups, provider.Group{Label: list, URLs: urls})
	}

	if len(groups) == 0 && firstErr != nil {
		return nil, firstErr
	}
	return groups, nil
}

// fetchList pulls all pages of a single list, deduped within the list.
func (t *TMDB) fetchList(ctx context.Context, list string) ([]string, error) {
	seen := make(map[string]struct{})
	var urls []string
	var firstErr error

	for page := 1; page <= t.pages; page++ {
		paths, err := t.fetchPage(ctx, list, page)
		if err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		for _, p := range paths {
			// poster_path already begins with "/"; avoid a double slash.
			u := t.imageBase + t.size + p
			if _, dup := seen[u]; dup {
				continue
			}
			seen[u] = struct{}{}
			urls = append(urls, u)
		}
	}

	if len(urls) == 0 && firstErr != nil {
		return nil, firstErr
	}
	return urls, nil
}

func (t *TMDB) fetchPage(ctx context.Context, list string, page int) ([]string, error) {
	u, err := url.Parse(t.apiBase + list)
	if err != nil {
		return nil, err
	}
	q := u.Query()
	q.Set("page", strconv.Itoa(page))
	if t.apiKey != "" {
		q.Set("api_key", t.apiKey)
	}
	u.RawQuery = q.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	if t.accessToken != "" {
		req.Header.Set("Authorization", "Bearer "+t.accessToken)
	}

	resp, err := t.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("tmdb: %s page %d: unexpected status %s", list, page, resp.Status)
	}

	var body listResponse
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return nil, fmt.Errorf("tmdb: decode %s page %d: %w", list, page, err)
	}

	paths := make([]string, 0, len(body.Results))
	for _, r := range body.Results {
		if r.PosterPath != "" {
			paths = append(paths, r.PosterPath)
		}
	}
	return paths, nil
}
