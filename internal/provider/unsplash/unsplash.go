// Package unsplash implements an image provider backed by the official Unsplash
// API (api.unsplash.com/photos/random).
//
// Unlike TMDB's poster CDN, Unsplash's /photos/random returns JSON photo
// metadata; this provider extracts the direct image URLs from it. It requires
// an application access key (Authorization: Client-ID <key>). Note this is the
// official API — the old, unauthenticated source.unsplash.com/random redirect
// service was retired.
package unsplash

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/ygelfand/posterlink/internal/provider"
)

const (
	defaultAPIBase     = "https://api.unsplash.com"
	defaultCount       = 30 // /photos/random caps count at 30
	defaultOrientation = "portrait"
	defaultSize        = "regular" // raw | full | regular | small | thumb
)

func init() {
	provider.Register("unsplash", New)
}

// Unsplash is the provider implementation.
type Unsplash struct {
	provider.Base

	accessKey   string
	apiBase     string
	count       int
	orientation string
	query       string
	size        string

	client *http.Client
}

// New constructs an Unsplash provider from its configuration subtree.
func New(opts provider.Options) (provider.Provider, error) {
	// Unsplash's dashboard labels this the "Access Key"; it is sent as the
	// Client-ID. Accept either name.
	key := opts.String("access_key", "")
	if key == "" {
		key = opts.String("client_id", "")
	}
	if key == "" {
		return nil, fmt.Errorf("unsplash: access_key (client_id) is required (register an app at unsplash.com/developers)")
	}
	return &Unsplash{
		Base:        provider.NewBase("unsplash", opts),
		accessKey:   key,
		apiBase:     opts.String("api_base", defaultAPIBase),
		count:       opts.Int("count", defaultCount),
		orientation: opts.String("orientation", defaultOrientation),
		query:       opts.String("query", ""),
		size:        opts.String("size", defaultSize),
		client:      &http.Client{Timeout: 10 * time.Second},
	}, nil
}

type randomPhoto struct {
	URLs map[string]string `json:"urls"`
}

// Fetch pulls a batch of random photos and returns their direct image URLs.
func (u *Unsplash) Fetch(ctx context.Context) ([]string, error) {
	endpoint, err := url.Parse(u.apiBase + "/photos/random")
	if err != nil {
		return nil, err
	}
	q := endpoint.Query()
	q.Set("count", strconv.Itoa(u.count))
	if u.orientation != "" {
		q.Set("orientation", u.orientation)
	}
	if u.query != "" {
		q.Set("query", u.query)
	}
	endpoint.RawQuery = q.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint.String(), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Accept-Version", "v1")
	req.Header.Set("Authorization", "Client-ID "+u.accessKey)

	resp, err := u.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("unsplash: unexpected status %s", resp.Status)
	}

	// With count set, /photos/random returns an array.
	var photos []randomPhoto
	if err := json.NewDecoder(resp.Body).Decode(&photos); err != nil {
		return nil, fmt.Errorf("unsplash: decode: %w", err)
	}

	urls := make([]string, 0, len(photos))
	for _, p := range photos {
		if img := p.URLs[u.size]; img != "" {
			urls = append(urls, img)
		}
	}
	return urls, nil
}
