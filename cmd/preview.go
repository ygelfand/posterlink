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
	previewProvider    string
	previewURLs        bool
	previewOut         string
	previewNoOpen      bool
	previewConcurrency int
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
	previewCmd.Flags().IntVar(&previewConcurrency, "concurrency", 6, "max images to load at once in the HTML (lower for heavy/rate-limited sources)")
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
		p, err := buildProvider(cfg, name)
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
	conc := max(previewConcurrency, 1)
	data := struct {
		Sections    []section
		Concurrency int
	}{sections, conc}
	if err := previewTmpl.Execute(f, data); err != nil {
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
  .grid img { height:180px; border-radius:4px; background:#222; min-width:60px; }
  .grid img.broken { outline:2px solid #833; opacity:.4; }
</style>
<h1>posterlink preview <span class="count" id="status"></span></h1>
{{range .Sections}}
<h2>{{.Provider}} <span class="count">(weight {{.Weight}})</span></h2>
{{range .Groups}}
<h3>{{.Label}} <span class="count">— {{len .URLs}}</span></h3>
<div class="grid">
{{range .URLs}}<img data-src="{{.}}" height="180">{{end}}
</div>
{{end}}
{{end}}
<script>
(function(){
  var CONC = {{.Concurrency}};
  var imgs = Array.prototype.slice.call(document.querySelectorAll('img[data-src]'));
  var total = imgs.length, done = 0, i = 0, active = 0;
  var status = document.getElementById('status');
  function tick(){ status.textContent = done + '/' + total + ' loaded'; }
  function finish(img){ done++; active--; tick(); next(); }
  function load(img){
    var tries = 0;
    img.onload = function(){ finish(img); };
    img.onerror = function(){
      if (tries++ < 2){ setTimeout(function(){ img.src = img.dataset.src + (img.dataset.src.indexOf('?')<0?'?':'&') + '_r=' + tries; }, 500*tries); return; }
      img.classList.add('broken'); finish(img);
    };
    img.src = img.dataset.src;
  }
  function next(){
    while (active < CONC && i < imgs.length){ active++; load(imgs[i++]); }
  }
  tick(); next();
})();
</script>
`))
