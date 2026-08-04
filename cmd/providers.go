package cmd

import (
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
