package cmd

import (
	"context"

	"github.com/ygelfand/posterlink/internal/config"
	"github.com/ygelfand/posterlink/internal/provider"

	// Blank imports register the built-in provider types with the registry.
	_ "github.com/ygelfand/posterlink/internal/provider/artic"
	_ "github.com/ygelfand/posterlink/internal/provider/itunes"
	_ "github.com/ygelfand/posterlink/internal/provider/steam"
	_ "github.com/ygelfand/posterlink/internal/provider/tmdb"
	_ "github.com/ygelfand/posterlink/internal/provider/unsplash"
	_ "github.com/ygelfand/posterlink/internal/provider/wikidata"
)

// buildProvider constructs one provider instance named name. The implementation
// type comes from the block's "type" field, defaulting to name itself — so a
// plain "itunes:" block is type itunes, while an aliased "itunes_jazz:" block
// sets "type: itunes".
func buildProvider(cfg *config.Config, name string) (provider.Provider, error) {
	opts := cfg.ProviderOptions(name)
	typ := opts.String("type", name)
	return provider.Build(typ, name, opts)
}

// providerGroups returns a provider's images as labeled groups: one group per
// list/query for providers that implement provider.Previewer, otherwise a
// single group named after the provider. Both `preview` and `sync` use the
// labels to show (and record) where each image came from.
func providerGroups(ctx context.Context, p provider.Provider) ([]provider.Group, error) {
	if pv, ok := p.(provider.Previewer); ok {
		return pv.Preview(ctx)
	}
	urls, err := p.Fetch(ctx)
	if err != nil {
		return nil, err
	}
	return []provider.Group{{Label: p.Name(), URLs: urls}}, nil
}

// countURLs totals the images across groups.
func countURLs(groups []provider.Group) int {
	n := 0
	for _, g := range groups {
		n += len(g.URLs)
	}
	return n
}
