// Package artic implements an image provider backed by the Art Institute of
// Chicago's public API (api.artic.edu). It needs no API key.
//
// Images are served through the IIIF Image API, whose URL encodes a crop
// region and output size — so the museum crops/resizes server-side. That lets
// posterlink (which only redirects) deliver a full-bleed, screen-shaped crop of
// any painting ("fill"), or the whole work letterboxed ("fit").
package artic

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

const searchURL = "https://api.artic.edu/api/v1/artworks/search"

// defaultArtists are painters the Art Institute holds iconic works by, so the
// out-of-box pool is famous rather than obscure.
var defaultArtists = []string{
	"Claude Monet", "Vincent van Gogh", "Georges Seurat", "Pierre-Auguste Renoir",
	"Gustave Caillebotte", "Paul Cézanne", "Paul Gauguin", "Edgar Degas",
	"Henri de Toulouse-Lautrec", "Edward Hopper", "Grant Wood", "Georgia O'Keeffe",
}

func init() {
	provider.Register("artic", New)
}

// ArtIC is the provider implementation.
type ArtIC struct {
	provider.Base

	width    int
	fill     bool
	aspectW  int
	aspectH  int
	portrait bool
	artists  []string
	query    string
	limit    int

	client *http.Client
}

// New constructs an Art Institute provider from its configuration subtree.
func New(opts provider.Options) (provider.Provider, error) {
	aw, ah, err := parseAspect(opts.String("aspect", "9:16"))
	if err != nil {
		return nil, err
	}
	mode := opts.String("fit", "fill")
	if mode != "fill" && mode != "fit" {
		return nil, fmt.Errorf("artic: invalid fit %q (want fill or fit)", mode)
	}
	return &ArtIC{
		Base:     provider.NewBase("artic", opts),
		width:    opts.Int("width", 1080),
		fill:     mode == "fill",
		aspectW:  aw,
		aspectH:  ah,
		portrait: opts.String("orientation", "any") == "portrait",
		artists:  opts.Strings("artists", defaultArtists),
		query:    opts.String("query", "painting"),
		limit:    opts.Int("limit", 100),
		client:   &http.Client{Timeout: 10 * time.Second},
	}, nil
}

func parseAspect(s string) (int, int, error) {
	parts := strings.SplitN(s, ":", 2)
	if len(parts) != 2 {
		return 0, 0, fmt.Errorf("artic: invalid aspect %q (want W:H)", s)
	}
	w, err1 := strconv.Atoi(strings.TrimSpace(parts[0]))
	h, err2 := strconv.Atoi(strings.TrimSpace(parts[1]))
	if err1 != nil || err2 != nil || w <= 0 || h <= 0 {
		return 0, 0, fmt.Errorf("artic: invalid aspect %q (want W:H)", s)
	}
	return w, h, nil
}

// queries returns the search terms to run: one per artist, or the general query.
func (a *ArtIC) queries() []string {
	if len(a.artists) > 0 {
		return a.artists
	}
	return []string{a.query}
}

// Fetch runs every query and returns IIIF image URLs deduped across queries.
func (a *ArtIC) Fetch(ctx context.Context) ([]string, error) {
	seen := make(map[string]struct{})
	var urls []string
	var firstErr error

	for _, q := range a.queries() {
		got, err := a.searchURLs(ctx, q)
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

// Preview returns one group per query (artist or the general term).
func (a *ArtIC) Preview(ctx context.Context) ([]provider.Group, error) {
	var groups []provider.Group
	var firstErr error

	for _, q := range a.queries() {
		urls, err := a.searchURLs(ctx, q)
		if err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		groups = append(groups, provider.Group{Label: "artic/" + q, URLs: urls})
	}

	if len(groups) == 0 && firstErr != nil {
		return nil, firstErr
	}
	return groups, nil
}

type searchResponse struct {
	Data []struct {
		ImageID     string `json:"image_id"`
		ArtistTitle string `json:"artist_title"`
		Thumbnail   struct {
			Width  int `json:"width"`
			Height int `json:"height"`
		} `json:"thumbnail"`
	} `json:"data"`
	Config struct {
		IIIFURL string `json:"iiif_url"`
	} `json:"config"`
}

func (a *ArtIC) searchURLs(ctx context.Context, q string) ([]string, error) {
	v := url.Values{}
	v.Set("q", q)
	v.Set("query[term][is_public_domain]", "true")
	v.Set("fields", "id,image_id,thumbnail,artist_title")
	v.Set("limit", strconv.Itoa(a.limit))

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, searchURL+"?"+v.Encode(), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("AIC-User-Agent", "posterlink (github.com/ygelfand/posterlink)")

	resp, err := a.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("artic: %q: unexpected status %s", q, resp.Status)
	}

	var body searchResponse
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return nil, fmt.Errorf("artic: decode %q: %w", q, err)
	}

	// In artist mode, AIC's fuzzy search pads results with related works, so
	// keep only pieces actually attributed to the queried artist.
	artistMode := len(a.artists) > 0
	wantArtist := strings.ToLower(q)

	base := strings.TrimRight(body.Config.IIIFURL, "/")
	var urls []string
	for _, d := range body.Data {
		if d.ImageID == "" || d.Thumbnail.Width <= 0 || d.Thumbnail.Height <= 0 {
			continue
		}
		if artistMode && !strings.Contains(strings.ToLower(d.ArtistTitle), wantArtist) {
			continue
		}
		if a.portrait && d.Thumbnail.Height <= d.Thumbnail.Width {
			continue
		}
		urls = append(urls, a.imageURL(base, d.ImageID, d.Thumbnail.Width, d.Thumbnail.Height))
	}
	return urls, nil
}

// imageURL builds the IIIF URL, cropping to the target aspect in fill mode.
// A width <= 0 requests the original (full) resolution, best paired with fit
// mode.
func (a *ArtIC) imageURL(base, id string, w, h int) string {
	if !a.fill {
		// Whole work; the consumer letterboxes (or pans/zooms).
		size := "full"
		if a.width > 0 {
			size = fmt.Sprintf("%d,", a.width)
		}
		return fmt.Sprintf("%s/%s/full/%s/0/default.jpg", base, id, size)
	}

	// Centered region matching the target aspect (aspectW:aspectH).
	var cropW, cropH, x, y int
	if w*a.aspectH > h*a.aspectW {
		// Image is wider than target → crop the sides.
		cropH = h
		cropW = h * a.aspectW / a.aspectH
		x = (w - cropW) / 2
	} else {
		// Image is taller than target → crop top/bottom.
		cropW = w
		cropH = w * a.aspectH / a.aspectW
		y = (h - cropH) / 2
	}
	size := "full"
	if a.width > 0 {
		size = fmt.Sprintf("%d,%d", a.width, a.width*a.aspectH/a.aspectW)
	}
	return fmt.Sprintf("%s/%s/%d,%d,%d,%d/%s/0/default.jpg", base, id, x, y, cropW, cropH, size)
}
