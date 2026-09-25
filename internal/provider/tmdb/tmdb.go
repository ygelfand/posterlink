// Package tmdb implements a poster image provider backed by The Movie Database.
//
// TMDB splits its JSON API (which exposes poster_path values) from its image
// CDN (image.tmdb.org). There is no endpoint that returns a random poster
// image, so this provider pulls a pool of poster paths from the list endpoints
// and builds direct CDN URLs the server can 302-redirect to.
package tmdb

import (
	"cmp"
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
func New(name string, opts provider.Options) (provider.Provider, error) {
	apiKey := opts.String("api_key", "")
	token := opts.String("access_token", "")
	if apiKey == "" && token == "" {
		return nil, fmt.Errorf("tmdb: one of api_key or access_token is required")
	}

	return &TMDB{
		Base:        provider.NewBase(name, opts),
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
		PosterPath   string `json:"poster_path"`
		Title        string `json:"title"`
		Name         string `json:"name"`
		ReleaseDate  string `json:"release_date"`
		FirstAirDate string `json:"first_air_date"`
		Overview     string `json:"overview"`
	} `json:"results"`
}

// Fetch pulls every configured list and returns images deduped across lists.
func (t *TMDB) Fetch(ctx context.Context) ([]provider.Image, error) {
	seen := make(map[string]struct{})
	var images []provider.Image
	var firstErr error

	for _, list := range t.lists {
		got, err := t.fetchList(ctx, list)
		if err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		for _, img := range got {
			if _, dup := seen[img.URL]; dup {
				continue
			}
			seen[img.URL] = struct{}{}
			images = append(images, img)
		}
	}

	if len(images) == 0 && firstErr != nil {
		return nil, firstErr
	}
	return images, nil
}

// Preview returns one labeled group per configured list, deduped within each
// list but not across lists (so overlap between lists is visible).
func (t *TMDB) Preview(ctx context.Context) ([]provider.Group, error) {
	var groups []provider.Group
	var firstErr error

	for _, list := range t.lists {
		images, err := t.fetchList(ctx, list)
		if err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		groups = append(groups, provider.Group{Label: list, Images: images})
	}

	if len(groups) == 0 && firstErr != nil {
		return nil, firstErr
	}
	return groups, nil
}

// fetchList pulls all pages of a single list, deduped within the list.
func (t *TMDB) fetchList(ctx context.Context, list string) ([]provider.Image, error) {
	seen := make(map[string]struct{})
	var images []provider.Image
	var firstErr error

	for page := 1; page <= t.pages; page++ {
		got, err := t.fetchPage(ctx, list, page)
		if err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		for _, img := range got {
			if _, dup := seen[img.URL]; dup {
				continue
			}
			seen[img.URL] = struct{}{}
			images = append(images, img)
		}
	}

	if len(images) == 0 && firstErr != nil {
		return nil, firstErr
	}
	return images, nil
}

func (t *TMDB) fetchPage(ctx context.Context, list string, page int) ([]provider.Image, error) {
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

	images := make([]provider.Image, 0, len(body.Results))
	for _, r := range body.Results {
		if r.PosterPath == "" {
			continue
		}
		title := cmp.Or(r.Title, r.Name)
		// poster_path already begins with "/"; avoid a double slash.
		images = append(images, provider.Image{
			URL:         t.imageBase + t.size + r.PosterPath,
			Title:       title,
			Date:        cmp.Or(r.ReleaseDate, r.FirstAirDate),
			Description: r.Overview,
		})
	}
	return images, nil
}
