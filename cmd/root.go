package cmd

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"
	"github.com/ygelfand/posterlink/internal/config"
)

var cfgFile string

var rootCmd = &cobra.Command{
	Use:   "posterlink",
	Short: "A random movie-poster redirect service for wall-panel screensavers",
	Long: `posterlink serves a single URL that 302-redirects to a different image on
every request, so a screensaver (e.g. Home Assistant wallpanel) can rotate
posters with zero client-side state.

Images come from pluggable providers — TMDB is one; others blend in by weight.`,
	Version:       config.Version,
	SilenceUsage:  true,
	SilenceErrors: true,
}

// Execute runs the root command.
func Execute() {
	if err := rootCmd.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func init() {
	rootCmd.SetVersionTemplate(fmt.Sprintf(
		"posterlink version {{.Version}} (commit: %s, date: %s)\n",
		config.GitCommit, config.BuildDate,
	))
	rootCmd.PersistentFlags().StringVar(&cfgFile, "config", "", "config file (default: ./posterlink.yaml)")
}
