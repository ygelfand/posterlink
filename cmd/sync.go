package cmd

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"slices"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"
	"github.com/ygelfand/posterlink/internal/cache"
	"github.com/ygelfand/posterlink/internal/config"
)

var (
	syncDir          string
	syncProviders    []string
	syncConcurrency  int
	syncDryRun       bool
	syncNoPrune      bool
	syncNoMetadata   bool
	syncVerbose      bool
	syncTimeout      time.Duration
	syncFetchTimeout time.Duration
)

var syncCmd = &cobra.Command{
	Use:     "sync",
	Aliases: []string{"cache"},
	Short:   "Mirror the configured providers' images to a local directory",
	Long: `sync downloads the images your providers currently return into a local
directory — one subdirectory per provider config block — and tags each file
with EXIF and XMP metadata (description, date, and the provider as the
make/model pair) so a photo library such as Immich indexes it sensibly.

It is idempotent and incremental: images already on disk are never
re-downloaded (posters are assumed immutable), images the providers no longer
return are deleted, and only the new ones are fetched. Each directory keeps a
.posterlink.json manifest that records the source URL of every file; that
manifest is also what marks the directory as posterlink-owned, so a directory
posterlink did not create is added to but never pruned.

Point the cache root at a directory in an Immich external library and run this
from cron:

    posterlink sync --config /etc/posterlink/posterlink.yaml`,
	RunE: runSync,
}

func init() {
	rootCmd.AddCommand(syncCmd)
	syncCmd.Flags().StringVar(&syncDir, "dir", "", "cache root directory (default: cache.dir from the config)")
	syncCmd.Flags().StringSliceVar(&syncProviders, "provider", nil, "only sync these providers (repeatable/comma-separated); others are left untouched")
	syncCmd.Flags().IntVar(&syncConcurrency, "concurrency", 0, "parallel downloads (default: cache.concurrency, else 4)")
	syncCmd.Flags().BoolVar(&syncDryRun, "dry-run", false, "report what would change without writing anything")
	syncCmd.Flags().BoolVar(&syncNoPrune, "no-prune", false, "keep cached images the providers no longer return")
	syncCmd.Flags().BoolVar(&syncNoMetadata, "no-metadata", false, "do not write EXIF/XMP metadata into the images")
	syncCmd.Flags().DurationVar(&syncTimeout, "timeout", 60*time.Second, "timeout for a single image download")
	syncCmd.Flags().DurationVar(&syncFetchTimeout, "fetch-timeout", 3*time.Minute, "timeout for listing one provider's images")
	syncCmd.Flags().BoolVarP(&syncVerbose, "verbose", "v", false, "enable debug logging")
}

func runSync(_ *cobra.Command, _ []string) error {
	level := slog.LevelInfo
	if syncVerbose {
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

	dir := syncDir
	if dir == "" {
		dir = cfg.Cache.Dir
	}
	if dir == "" {
		return fmt.Errorf("no cache directory: set cache.dir in the config or pass --dir")
	}
	concurrency := syncConcurrency
	if concurrency == 0 {
		concurrency = cfg.Cache.Concurrency
	}

	names := cfg.EnabledProviders()
	if len(names) == 0 {
		return fmt.Errorf("no providers enabled; configure at least one under providers.*")
	}
	for _, want := range syncProviders {
		if !slices.Contains(names, want) {
			return fmt.Errorf("provider %q is not enabled (enabled: %v)", want, names)
		}
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	sources, keep, failed := collectSources(ctx, cfg, names, log)
	if len(sources) == 0 {
		return fmt.Errorf("no provider returned any images")
	}

	results, err := cache.Sync(ctx, cache.Options{
		Dir:         dir,
		Concurrency: concurrency,
		Metadata:    cfg.Cache.Metadata && !syncNoMetadata,
		Prune:       !syncNoPrune,
		KeepDirs:    keep,
		DryRun:      syncDryRun,
		UserAgent:   cfg.Cache.UserAgent,
		Timeout:     syncTimeout,
		Log:         log,
	}, sources)

	printResults(dir, results)
	if err != nil {
		return err
	}
	if failed > 0 {
		return fmt.Errorf("%d provider(s) could not be listed; their cached images were left alone", failed)
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	return nil
}

// collectSources lists every cacheable provider's images. A provider that
// cannot be built or listed is skipped, and its directory name is returned in
// keep so a transient API failure never prunes an existing cache.
func collectSources(ctx context.Context, cfg *config.Config, names []string, log *slog.Logger) (sources []cache.Source, keep []string, failed int) {
	for _, name := range names {
		opts := cfg.ProviderOptions(name)
		dirName := opts.String("cache_dir", name)

		if !opts.Bool("cache_enabled", true) {
			log.Debug("provider not cached", "provider", name)
			continue
		}
		if len(syncProviders) > 0 && !slices.Contains(syncProviders, name) {
			keep = append(keep, dirName)
			continue
		}
		if ctx.Err() != nil {
			keep = append(keep, dirName)
			continue
		}

		p, err := buildProvider(cfg, name)
		if err != nil {
			log.Warn("provider unavailable", "provider", name, "error", err)
			keep = append(keep, dirName)
			failed++
			continue
		}

		fetchCtx, cancel := context.WithTimeout(ctx, syncFetchTimeout)
		groups, err := providerGroups(fetchCtx, p)
		cancel()
		if err != nil {
			log.Warn("provider listing failed", "provider", name, "error", err)
			keep = append(keep, dirName)
			failed++
			continue
		}
		if countURLs(groups) == 0 {
			log.Warn("provider returned no images", "provider", name)
			keep = append(keep, dirName)
			failed++
			continue
		}

		log.Info("provider listed", "provider", name, "images", countURLs(groups))
		sources = append(sources, cache.Source{
			Name:   name,
			Dir:    dirName,
			Groups: groups,
			Limit:  opts.Int("cache_limit", 0),
		})
	}
	return sources, keep, failed
}

func printResults(dir string, results []cache.Result) {
	if syncDryRun {
		fmt.Printf("dry run — nothing written to %s\n", dir)
	} else {
		fmt.Printf("cache root: %s\n", dir)
	}

	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', tabwriter.AlignRight)
	_, _ = fmt.Fprintln(w, "provider\ttotal\tkept\tadded\tremoved\tfailed\tdownloaded\t")

	var total, kept, added, removed, failed int
	var bytes int64
	for _, r := range results {
		_, _ = fmt.Fprintf(w, "%s\t%d\t%d\t%d\t%d\t%d\t%s\t\n",
			r.Name, r.Total(), r.Kept, r.Added, r.Removed, r.Failed, humanBytes(r.Bytes))
		total += r.Total()
		kept += r.Kept
		added += r.Added
		removed += r.Removed
		failed += r.Failed
		bytes += r.Bytes
	}
	if len(results) > 1 {
		_, _ = fmt.Fprintf(w, "%s\t%d\t%d\t%d\t%d\t%d\t%s\t\n",
			"all", total, kept, added, removed, failed, humanBytes(bytes))
	}
	_ = w.Flush()

	for _, r := range results {
		if r.Err != nil {
			fmt.Fprintf(os.Stderr, "warning: %s: %v\n", r.Name, r.Err)
		}
	}
}

func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	v := float64(n)
	for _, suffix := range []string{"KiB", "MiB", "GiB", "TiB"} {
		v /= unit
		if v < unit {
			return fmt.Sprintf("%.1f %s", v, suffix)
		}
	}
	return fmt.Sprintf("%.1f PiB", v)
}
