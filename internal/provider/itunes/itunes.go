// Package itunes implements an image provider backed by the public iTunes
// Search API (no API key). It is generic over Apple's media types, so the same
// code serves album covers by artist, book covers by author, etc.
//
// Two curation modes:
//   - artist_ids: resolved via the lookup endpoint, which returns only that
//     exact artist's releases (no fuzzy matching — the robust path).
//   - artists: resolved via search with attribute=artistTerm, then filtered to
//     rows whose artistName actually matches (search alone fuzzy-matches album
//     titles, e.g. an "OMA" album titled "MF DOOM ...").
package itunes

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/ygelfand/posterlink/internal/provider"
)

const (
	searchURL = "https://itunes.apple.com/search"
	lookupURL = "https://itunes.apple.com/lookup"
)

// sizeRe matches the WxHbb size token in an artwork URL (e.g. 100x100bb).
var sizeRe = regexp.MustCompile(`\d+x\d+bb`)

func init() {
	provider.Register("itunes", New)
}

// ITunes is the provider implementation.
type ITunes struct {
	provider.Base

	size      int
	media     string
	entity    string
	attribute string
	country   string
	limit     int
	artistIDs []string
	artists   []string

	client *http.Client
}

// New constructs an iTunes provider from its configuration subtree.
func New(name string, opts provider.Options) (provider.Provider, error) {
	ids := opts.Strings("artist_ids", nil)
	names := opts.Strings("artists", nil)
	if len(ids) == 0 && len(names) == 0 {
		return nil, fmt.Errorf("itunes: at least one of artist_ids or artists is required")
	}
	return &ITunes{
		Base:      provider.NewBase(name, opts),
		size:      opts.Int("size", 2000),
		media:     opts.String("media", "music"),
		entity:    opts.String("entity", "album"),
		attribute: opts.String("attribute", "artistTerm"),
		country:   opts.String("country", "US"),
		limit:     opts.Int("limit", 100),
		artistIDs: ids,
		artists:   names,
		client:    &http.Client{Timeout: 10 * time.Second},
	}, nil
}

// item is one curation source: an artist ID (lookup) or an artist name (search).
type item struct {
	label string
	id    string // set for lookup
	name  string // set for search
}

func (it *ITunes) items() []item {
	items := make([]item, 0, len(it.artistIDs)+len(it.artists))
	for _, id := range it.artistIDs {
		items = append(items, item{label: "id:" + id, id: id})
	}
	for _, name := range it.artists {
		items = append(items, item{label: name, name: name})
	}
	return items
}

// Fetch runs every curation source and returns artwork URLs deduped across them.
func (it *ITunes) Fetch(ctx context.Context) ([]string, error) {
	seen := make(map[string]struct{})
	var urls []string
	var firstErr error

	for _, q := range it.items() {
		got, err := it.run(ctx, q)
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

// Preview returns one group per curation source.
func (it *ITunes) Preview(ctx context.Context) ([]provider.Group, error) {
	var groups []provider.Group
	var firstErr error

	for _, q := range it.items() {
		urls, err := it.run(ctx, q)
		if err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		groups = append(groups, provider.Group{Label: "itunes/" + q.label, URLs: urls})
	}

	if len(groups) == 0 && firstErr != nil {
		return nil, firstErr
	}
	return groups, nil
}

func (it *ITunes) run(ctx context.Context, q item) ([]string, error) {
	if q.id != "" {
		return it.lookup(ctx, q.id)
	}
	return it.search(ctx, q.name)
}

type result struct {
	ArtistName    string `json:"artistName"`
	ArtworkURL100 string `json:"artworkUrl100"`
}

type response struct {
	Results []result `json:"results"`
}

// lookup returns artwork for an exact artist ID — no fuzzy matching.
func (it *ITunes) lookup(ctx context.Context, id string) ([]string, error) {
	v := url.Values{}
	v.Set("id", id)
	v.Set("entity", it.entity)
	v.Set("limit", strconv.Itoa(it.limit))
	if it.country != "" {
		v.Set("country", it.country)
	}

	body, err := it.get(ctx, lookupURL+"?"+v.Encode())
	if err != nil {
		return nil, err
	}
	// lookup returns the artist as results[0] (no artwork); take everything else.
	return it.artworkURLs(body.Results, ""), nil
}

// search returns artwork for an artist name, filtered to exact artistName
// matches to drop fuzzy album-title hits.
func (it *ITunes) search(ctx context.Context, name string) ([]string, error) {
	v := url.Values{}
	v.Set("term", name)
	v.Set("media", it.media)
	v.Set("entity", it.entity)
	v.Set("attribute", it.attribute)
	v.Set("limit", strconv.Itoa(it.limit))
	if it.country != "" {
		v.Set("country", it.country)
	}

	body, err := it.get(ctx, searchURL+"?"+v.Encode())
	if err != nil {
		return nil, err
	}
	return it.artworkURLs(body.Results, name), nil
}

// artworkURLs builds sized artwork URLs. If wantArtist is non-empty, only rows
// whose artistName matches (case-insensitive) are kept.
func (it *ITunes) artworkURLs(results []result, wantArtist string) []string {
	want := strings.ToLower(strings.TrimSpace(wantArtist))
	size := fmt.Sprintf("%dx%dbb", it.size, it.size)

	seen := make(map[string]struct{})
	var urls []string
	for _, r := range results {
		if r.ArtworkURL100 == "" {
			continue
		}
		if want != "" && strings.ToLower(strings.TrimSpace(r.ArtistName)) != want {
			continue
		}
		u := sizeRe.ReplaceAllString(r.ArtworkURL100, size)
		if _, dup := seen[u]; dup {
			continue
		}
		seen[u] = struct{}{}
		urls = append(urls, u)
	}
	return urls
}

func (it *ITunes) get(ctx context.Context, u string) (*response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")

	resp, err := it.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("itunes: %s: unexpected status %s", u, resp.Status)
	}

	var body response
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return nil, fmt.Errorf("itunes: decode: %w", err)
	}
	return &body, nil
}
