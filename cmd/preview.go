package cmd

import (
	"context"
	"fmt"
	"html/template"
	"os"
	"os/exec"
	"runtime"
	"slices"
	"time"

	"github.com/spf13/cobra"
	"github.com/ygelfand/posterlink/internal/config"
	"github.com/ygelfand/posterlink/internal/provider"
)

var (
	previewProvider string
	previewURLs     bool
	previewOut      string
	previewNoOpen   bool
)

var previewCmd = &cobra.Command{
	Use:   "preview",
	Short: "Render the configured providers' images as an HTML contact sheet",
	Long: `preview fetches images from the enabled providers using your current config
and writes an HTML grid, grouped by provider (and, for TMDB, by list), so you
can see exactly what the service would serve and where each image comes from.`,
	RunE: runPreview,
}

func init() {
	rootCmd.AddCommand(previewCmd)
	previewCmd.Flags().StringVar(&previewProvider, "provider", "", "only preview this provider (e.g. tmdb)")
	previewCmd.Flags().BoolVar(&previewURLs, "urls", false, "print image URLs instead of writing HTML")
	previewCmd.Flags().StringVar(&previewOut, "out", "", "HTML output path (default: a temp file)")
	previewCmd.Flags().BoolVar(&previewNoOpen, "no-open", false, "do not open the HTML in a browser")
}

// section is one provider's contribution to the preview.
type section struct {
	Provider string
	Weight   float64
	Groups   []provider.Group
}

func runPreview(_ *cobra.Command, _ []string) error {
	cfg, err := config.Load(cfgFile)
	if err != nil {
		return err
	}

	names := cfg.EnabledProviders()
	if previewProvider != "" {
		if !slices.Contains(names, previewProvider) {
			return fmt.Errorf("provider %q is not enabled (enabled: %v)", previewProvider, names)
		}
		names = []string{previewProvider}
	}
	if len(names) == 0 {
		return fmt.Errorf("no providers enabled; configure at least one under providers.*")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	var sections []section
	for _, name := range names {
		p, err := provider.Build(name, cfg.ProviderOptions(name))
		if err != nil {
			return fmt.Errorf("provider %q: %w", name, err)
		}

		var groups []provider.Group
		if pv, ok := p.(provider.Previewer); ok {
			groups, err = pv.Preview(ctx)
		} else {
			var urls []string
			urls, err = p.Fetch(ctx)
			groups = []provider.Group{{Label: name, URLs: urls}}
		}
		if err != nil {
			fmt.Fprintf(os.Stderr, "warning: %s: %v\n", name, err)
			continue
		}
		sections = append(sections, section{Provider: name, Weight: p.Weight(), Groups: groups})
	}
	if len(sections) == 0 {
		return fmt.Errorf("no images fetched")
	}

	if previewURLs {
		printURLs(sections)
		return nil
	}
	return writeHTML(sections)
}

func printURLs(sections []section) {
	for _, s := range sections {
		for _, g := range s.Groups {
			fmt.Printf("# %s / %s (%d)\n", s.Provider, g.Label, len(g.URLs))
			for _, u := range g.URLs {
				fmt.Println(u)
			}
		}
	}
}

func writeHTML(sections []section) error {
	out := previewOut
	if out == "" {
		f, err := os.CreateTemp("", "posterlink-preview-*.html")
		if err != nil {
			return err
		}
		out = f.Name()
		_ = f.Close()
	}

	f, err := os.Create(out)
	if err != nil {
		return err
	}
	if err := previewTmpl.Execute(f, sections); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}

	total := 0
	for _, s := range sections {
		for _, g := range s.Groups {
			total += len(g.URLs)
		}
	}
	fmt.Printf("wrote %d images to %s\n", total, out)

	if previewNoOpen {
		return nil
	}
	if err := openBrowser(out); err != nil {
		fmt.Fprintf(os.Stderr, "could not open browser: %v (open %s manually)\n", err, out)
	}
	return nil
}

func openBrowser(path string) error {
	switch runtime.GOOS {
	case "darwin":
		return exec.Command("open", path).Start()
	case "windows":
		return exec.Command("rundll32", "url.dll,FileProtocolHandler", path).Start()
	default:
		return exec.Command("xdg-open", path).Start()
	}
}

var previewTmpl = template.Must(template.New("preview").Parse(`<!doctype html>
<meta charset="utf-8">
<title>posterlink preview</title>
<style>
  body { background:#111; color:#eee; font-family:system-ui,sans-serif; margin:1rem; }
  h1 { font-size:1.2rem; }
  h2 { border-bottom:1px solid #444; padding-bottom:.3rem; margin-top:2rem; }
  h3 { color:#9cf; font-weight:normal; margin:1rem 0 .4rem; font-family:monospace; }
  .count { color:#888; }
  .grid { display:flex; flex-wrap:wrap; gap:6px; }
  .grid img { height:180px; border-radius:4px; background:#222; }
</style>
<h1>posterlink preview</h1>
{{range .}}
<h2>{{.Provider}} <span class="count">(weight {{.Weight}})</span></h2>
{{range .Groups}}
<h3>{{.Label}} <span class="count">— {{len .URLs}}</span></h3>
<div class="grid">
{{range .URLs}}<img loading="lazy" src="{{.}}">{{end}}
</div>
{{end}}
{{end}}
`))
