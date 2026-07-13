package cmd

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/spf13/cobra"
	"github.com/ygelfand/posterlink/internal/config"
	"github.com/ygelfand/posterlink/internal/provider"
	"github.com/ygelfand/posterlink/internal/server"
)

var verbose bool

var serveCmd = &cobra.Command{
	Use:   "serve",
	Short: "Run the HTTP redirect server",
	RunE:  runServe,
}

func init() {
	rootCmd.AddCommand(serveCmd)
	serveCmd.Flags().Int("port", 0, "port to listen on (default 8088)")
	serveCmd.Flags().BoolVarP(&verbose, "verbose", "v", false, "enable debug logging")
}

func runServe(cmd *cobra.Command, _ []string) error {
	level := slog.LevelInfo
	if verbose {
		level = slog.LevelDebug
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level}))

	cfg, err := config.Load(cfgFile)
	if err != nil {
		return err
	}
	if f := cfg.ConfigFileUsed(); f != "" {
		log.Info("loaded config", "file", f)
	}
	if p, _ := cmd.Flags().GetInt("port"); p != 0 {
		cfg.Port = p
	}

	names := cfg.EnabledProviders()
	if len(names) == 0 {
		return fmt.Errorf("no providers enabled; configure at least one under providers.* (available: %v)", provider.Registered())
	}

	providers := make([]provider.Provider, 0, len(names))
	for _, name := range names {
		p, err := provider.Build(name, cfg.ProviderOptions(name))
		if err != nil {
			return fmt.Errorf("provider %q: %w", name, err)
		}
		providers = append(providers, p)
		log.Info("provider enabled", "provider", name, "weight", p.Weight())
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	srv := server.New(providers, cfg.RefreshInterval, log)
	go srv.Run(ctx)

	return srv.ListenAndServe(ctx, fmt.Sprintf(":%d", cfg.Port))
}
