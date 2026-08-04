// Package provider defines the image-source abstraction. TMDB is one provider;
// others (Unsplash, a static list, ...) plug in through the same interface and
// registry, so adding a source is a single self-registering file.
package provider

import (
	"context"
	"fmt"
	"sort"
)

// Options is a read-only, defaulting view of a provider's configuration
// subtree. config.Settings satisfies it.
type Options interface {
	String(key, def string) string
	Int(key string, def int) int
	Float(key string, def float64) float64
	Bool(key string, def bool) bool
	Strings(key string, def []string) []string
}

// Provider is an image source. Fetch returns a batch of fully-qualified image
// URLs and is called once at startup and again on every refresh tick.
type Provider interface {
	// Name identifies the provider (e.g. "tmdb").
	Name() string
	// Weight is the relative selection weight of this provider's images when
	// blending multiple sources. Defaults to 1.
	Weight() float64
	// Fetch returns the current set of candidate image URLs.
	Fetch(ctx context.Context) ([]string, error)
}

// Group is a labeled subset of a provider's images, used for inspection.
type Group struct {
	Label string
	URLs  []string
}

// Previewer is an optional interface for providers that can break their output
// into labeled groups (e.g. TMDB, one group per list endpoint). Providers that
// don't implement it are previewed as a single group.
type Previewer interface {
	Preview(ctx context.Context) ([]Group, error)
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
