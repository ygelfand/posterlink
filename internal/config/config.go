// Package config loads posterlink settings from a config file, environment
// variables (prefixed POSTERLINK_) and command-line flags, and exposes the
// build metadata stamped in via -ldflags.
package config

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/spf13/viper"
)

// Build metadata, overridden at link time via -ldflags. See the Makefile.
var (
	Version   = "dev"
	GitCommit = "none"
	BuildDate = "unknown"
)

// Config holds the resolved server-level settings and the underlying viper
// instance used to hand per-provider settings to each provider.
type Config struct {
	v *viper.Viper

	Port            int
	RefreshInterval time.Duration
}

// Load resolves configuration from the given file (optional), the standard
// search paths and the environment. It never errors on a missing config file,
// since posterlink can run entirely from environment variables.
func Load(cfgFile string) (*Config, error) {
	v := viper.New()

	v.SetConfigName("posterlink")
	v.SetConfigType("yaml")
	v.AddConfigPath(".")
	v.AddConfigPath("$HOME/.config/posterlink")
	v.AddConfigPath("/etc/posterlink")
	if cfgFile != "" {
		v.SetConfigFile(cfgFile)
	}

	v.SetEnvPrefix("POSTERLINK")
	v.SetEnvKeyReplacer(strings.NewReplacer(".", "_"))
	v.AutomaticEnv()

	// Convenience aliases so the well-known TMDB variables work without the
	// POSTERLINK_PROVIDERS_TMDB_ prefix.
	_ = v.BindEnv("providers.tmdb.api_key", "TMDB_API_KEY")
	_ = v.BindEnv("providers.tmdb.access_token", "TMDB_ACCESS_TOKEN")
	_ = v.BindEnv("providers.unsplash.access_key", "UNSPLASH_ACCESS_KEY", "UNSPLASH_CLIENT_ID")

	v.SetDefault("port", 8088)
	v.SetDefault("refresh_interval", "30m")

	if err := v.ReadInConfig(); err != nil {
		if _, ok := err.(viper.ConfigFileNotFoundError); !ok {
			return nil, fmt.Errorf("reading config: %w", err)
		}
	}

	interval, err := time.ParseDuration(v.GetString("refresh_interval"))
	if err != nil {
		return nil, fmt.Errorf("invalid refresh_interval %q: %w", v.GetString("refresh_interval"), err)
	}

	return &Config{
		v:               v,
		Port:            v.GetInt("port"),
		RefreshInterval: interval,
	}, nil
}

// ConfigFileUsed reports the config file that was loaded, if any.
func (c *Config) ConfigFileUsed() string { return c.v.ConfigFileUsed() }

// EnabledProviders returns the sorted names of providers that are configured
// and not explicitly disabled (enabled defaults to true when a provider block
// is present).
func (c *Config) EnabledProviders() []string {
	block := c.v.GetStringMap("providers")
	names := make([]string, 0, len(block))
	for name := range block {
		if c.ProviderOptions(name).Bool("enabled", true) {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	return names
}

// ProviderOptions returns a read-only settings accessor scoped to a single
// provider's config subtree (e.g. providers.tmdb.*).
func (c *Config) ProviderOptions(name string) Settings {
	return Settings{v: c.v, prefix: "providers." + name}
}

// Settings is a prefix-scoped, defaulting accessor over viper. It satisfies the
// provider.Options interface without coupling providers to viper directly.
type Settings struct {
	v      *viper.Viper
	prefix string
}

func (s Settings) key(k string) string { return s.prefix + "." + k }

// String returns the string at key, or def if unset/empty.
func (s Settings) String(key, def string) string {
	if v := s.v.GetString(s.key(key)); v != "" {
		return v
	}
	return def
}

// Int returns the int at key, or def if unset.
func (s Settings) Int(key string, def int) int {
	if !s.v.IsSet(s.key(key)) {
		return def
	}
	return s.v.GetInt(s.key(key))
}

// Float returns the float at key, or def if unset.
func (s Settings) Float(key string, def float64) float64 {
	if !s.v.IsSet(s.key(key)) {
		return def
	}
	return s.v.GetFloat64(s.key(key))
}

// Bool returns the bool at key, or def if unset.
func (s Settings) Bool(key string, def bool) bool {
	if !s.v.IsSet(s.key(key)) {
		return def
	}
	return s.v.GetBool(s.key(key))
}

// Strings returns the string slice at key, or def if unset. It accepts both
// YAML lists and comma-separated environment values.
func (s Settings) Strings(key string, def []string) []string {
	if !s.v.IsSet(s.key(key)) {
		return def
	}
	if v := s.v.GetStringSlice(s.key(key)); len(v) > 0 {
		return v
	}
	if raw := s.v.GetString(s.key(key)); raw != "" {
		parts := strings.Split(raw, ",")
		for i := range parts {
			parts[i] = strings.TrimSpace(parts[i])
		}
		return parts
	}
	return def
}
