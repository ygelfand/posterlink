// Package wikidata implements an image provider backed by the Wikidata Query
// Service (SPARQL) and Wikimedia Commons. It selects paintings ranked by fame —
// the number of Wikipedia language editions linking to them (wikibase:sitelinks)
// — and serves the artwork image from Commons.
//
// Personal/private display only: Commons hosts images under many licenses, and
// this provider does not filter by license.
package wikidata

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
	endpoint = "https://query.wikidata.org/sparql"
	painting = "wd:Q3305213" // instance of: painting
)

func init() {
	provider.Register("wikidata", New)
}

// Wikidata is the provider implementation.
type Wikidata struct {
	provider.Base

	minSitelinks int
	width        int
	limit        int
	collection   string // P195, e.g. Q19675 (Louvre)
	movement     string // P135, e.g. Q40415 (Impressionism)
	creator      string // P170
	genre        string // P136
	userAgent    string

	client *http.Client
}

// New constructs a Wikidata provider from its configuration subtree.
func New(opts provider.Options) (provider.Provider, error) {
	return &Wikidata{
		Base:         provider.NewBase("wikidata", opts),
		minSitelinks: opts.Int("min_sitelinks", 20),
		width:        opts.Int("width", 1080),
		limit:        opts.Int("limit", 500),
		collection:   opts.String("collection", ""),
		movement:     opts.String("movement", ""),
		creator:      opts.String("creator", ""),
		genre:        opts.String("genre", ""),
		userAgent:    opts.String("user_agent", "posterlink/1.0 (https://github.com/ygelfand/posterlink)"),
		client:       &http.Client{Timeout: 60 * time.Second},
	}, nil
}

// buildQuery assembles the SPARQL query from the configured filters.
func (w *Wikidata) buildQuery() string {
	var b strings.Builder
	b.WriteString("SELECT ?image WHERE { ?item wdt:P31 " + painting + "; wdt:P18 ?image; wikibase:sitelinks ?links. ")
	fmt.Fprintf(&b, "FILTER(?links >= %d) ", w.minSitelinks)
	if w.collection != "" {
		fmt.Fprintf(&b, "?item wdt:P195 %s. ", qref(w.collection))
	}
	if w.movement != "" {
		fmt.Fprintf(&b, "?item wdt:P135 %s. ", qref(w.movement))
	}
	if w.creator != "" {
		fmt.Fprintf(&b, "?item wdt:P170 %s. ", qref(w.creator))
	}
	if w.genre != "" {
		fmt.Fprintf(&b, "?item wdt:P136 %s. ", qref(w.genre))
	}
	fmt.Fprintf(&b, "} LIMIT %d", w.limit)
	return b.String()
}

// qref normalizes a Q-id into a SPARQL entity reference (wd:Qxxxx).
func qref(s string) string {
	s = strings.TrimSpace(s)
	if strings.HasPrefix(s, "wd:") {
		return s
	}
	return "wd:" + s
}

type sparqlResponse struct {
	Results struct {
		Bindings []struct {
			Image struct {
				Value string `json:"value"`
			} `json:"image"`
		} `json:"bindings"`
	} `json:"results"`
}

// Fetch runs the SPARQL query and returns Commons image URLs.
func (w *Wikidata) Fetch(ctx context.Context) ([]string, error) {
	u := endpoint + "?format=json&query=" + url.QueryEscape(w.buildQuery())
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/sparql-results+json")
	req.Header.Set("User-Agent", w.userAgent)

	resp, err := w.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("wikidata: unexpected status %s", resp.Status)
	}

	var body sparqlResponse
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return nil, fmt.Errorf("wikidata: decode: %w", err)
	}

	seen := make(map[string]struct{})
	var urls []string
	for _, b := range body.Results.Bindings {
		img := imageURL(b.Image.Value, w.width)
		if img == "" {
			continue
		}
		if _, dup := seen[img]; dup {
			continue
		}
		seen[img] = struct{}{}
		urls = append(urls, img)
	}
	return urls, nil
}

// imageURL upgrades the Commons FilePath URL to https and requests a scaled
// width. A width <= 0 leaves the original (full-resolution) image.
func imageURL(raw string, width int) string {
	if raw == "" {
		return ""
	}
	if after, ok := strings.CutPrefix(raw, "http://"); ok {
		raw = "https://" + after
	}
	if width > 0 {
		sep := "?"
		if strings.Contains(raw, "?") {
			sep = "&"
		}
		raw += sep + "width=" + strconv.Itoa(width)
	}
	return raw
}
