// Package provider defines the image-source abstraction. TMDB is one provider;
// others (Unsplash, a static list, ...) plug in through the same interface and
// registry, so adding a source is a single self-registering file.
package provider

import (
	"context"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"
)

// Image is one image and what its source knows about it.
type Image struct {
	URL         string
	Title       string
	Creator     string
	Date        string
	Description string
}

var yearRe = regexp.MustCompile(`\b(1\d{3}|20\d{2})\b`)

// Date parses the free-form date strings the providers return: RFC 3339 from
// iTunes and Wikidata, "2024-03-01" from TMDB, "c. 1906" from the Art Institute.
func (i Image) Parsed() (time.Time, bool) {
	raw := strings.TrimSpace(i.Date)
	if raw == "" {
		return time.Time{}, false
	}
	for _, layout := range []string{time.RFC3339, "2006-01-02", "2006-01", "2006"} {
		if t, err := time.Parse(layout, raw); err == nil {
			return t, true
		}
	}
	if m := yearRe.FindString(raw); m != "" {
		if t, err := time.Parse("2006", m); err == nil {
			return t, true
		}
	}
	return time.Time{}, false
}

// Caption renders what the source knows into one line: "Kind of Blue — Miles
// Davis (1959)". It is empty when the source knows nothing.
func (i Image) Caption() string {
	var b strings.Builder
	b.WriteString(strings.TrimSpace(i.Title))
	if c := strings.TrimSpace(i.Creator); c != "" {
		if b.Len() > 0 {
			b.WriteString(" - ")
		}
		b.WriteString(c)
	}
	if b.Len() == 0 {
		return ""
	}
	if t, ok := i.Parsed(); ok {
		fmt.Fprintf(&b, " (%d)", t.Year())
	}
	return b.String()
}

// URLs extracts the URLs from images, preserving order.
func URLs(images []Image) []string {
	out := make([]string, 0, len(images))
	for _, img := range images {
		out = append(out, img.URL)
	}
	return out
}

// Options is a read-only, defaulting view of a provider's configuration
// subtree. config.Settings satisfies it.
type Options interface {
	String(key, def string) string
	Int(key string, def int) int
	Float(key string, def float64) float64
	Bool(key string, def bool) bool
	Strings(key string, def []string) []string
}

// Provider is an image source. Fetch returns a batch of images and is called
// once at startup and again on every refresh tick.
type Provider interface {
	// Name identifies the provider (e.g. "tmdb").
	Name() string
	// Weight is the relative selection weight of this provider's images when
	// blending multiple sources. Defaults to 1.
	Weight() float64
	// Fetch returns the current set of candidate images.
	Fetch(ctx context.Context) ([]Image, error)
}

// Group is a labeled subset of a provider's images, used for inspection.
type Group struct {
	Label  string
	Images []Image
}

// Previewer is an optional interface for providers that can break their output
// into labeled groups (e.g. TMDB, one group per list endpoint). Providers that
// don't implement it are previewed as a single group.
type Previewer interface {
	Preview(ctx context.Context) ([]Group, error)
}

// Enricher is an optional interface for providers whose metadata costs extra
// requests. Callers decide whether to wait: serve fills the cache in the
// background for the next refresh, sync waits for the result.
type Enricher interface {
	Enrich(ctx context.Context, images []Image) []Image
}

// Factory constructs a provider instance. name is the instance name (the
// config block key, which may be an alias like "itunes_jazz"); it becomes the
// provider's Name() so aliased instances are distinct pool sources.
type Factory func(name string, opts Options) (Provider, error)

var registry = map[string]Factory{}

// Register makes a provider type available. Providers call this from init().
func Register(typ string, f Factory) {
	if _, dup := registry[typ]; dup {
		panic("provider: duplicate registration for " + typ)
	}
	registry[typ] = f
}

// Build constructs an instance named name of the given provider type.
func Build(typ, name string, opts Options) (Provider, error) {
	f, ok := registry[typ]
	if !ok {
		return nil, fmt.Errorf("unknown provider type %q (registered: %v)", typ, Registered())
	}
	return f(name, opts)
}

// Registered returns the sorted names of all registered provider types.
func Registered() []string {
	names := make([]string, 0, len(registry))
	for name := range registry {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// Base is an embeddable helper that supplies Name and Weight.
type Base struct {
	ProviderName   string
	ProviderWeight float64
}

// NewBase builds a Base, reading "weight" (default 1) from opts.
func NewBase(name string, opts Options) Base {
	return Base{ProviderName: name, ProviderWeight: opts.Float("weight", 1)}
}

func (b Base) Name() string    { return b.ProviderName }
func (b Base) Weight() float64 { return b.ProviderWeight }
