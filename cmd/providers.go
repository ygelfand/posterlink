package cmd

// Blank imports register the built-in providers with the provider registry.
import (
	_ "github.com/ygelfand/posterlink/internal/provider/steam"
	_ "github.com/ygelfand/posterlink/internal/provider/tmdb"
	_ "github.com/ygelfand/posterlink/internal/provider/unsplash"
)
