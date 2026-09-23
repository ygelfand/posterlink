package cache

import (
	"bytes"
	"context"
	"encoding/json"
	"image"
	"image/color"
	"image/jpeg"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/ygelfand/posterlink/internal/provider"
)

// imageServer serves a distinct JPEG per path and counts requests.
type imageServer struct {
	*httptest.Server
	hits atomic.Int64
}

func newImageServer(t *testing.T) *imageServer {
	t.Helper()
	s := &imageServer{}
	s.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.hits.Add(1)
		switch {
		case strings.HasSuffix(r.URL.Path, ".html"):
			w.Header().Set("Content-Type", "text/html")
			_, _ = w.Write([]byte("<html>nope</html>"))
			return
		case strings.HasSuffix(r.URL.Path, ".missing"):
			http.NotFound(w, r)
			return
		}
		img := image.NewRGBA(image.Rect(0, 0, 8, 12))
		img.Set(0, 0, color.RGBA{uint8(len(r.URL.Path)), 20, 30, 255})
		w.Header().Set("Content-Type", "image/jpeg")
		var buf bytes.Buffer
		_ = jpeg.Encode(&buf, img, nil)
		_, _ = w.Write(buf.Bytes())
	}))
	t.Cleanup(s.Close)
	return s
}

func (s *imageServer) urls(names ...string) []string {
	out := make([]string, 0, len(names))
	for _, n := range names {
		out = append(out, s.URL+"/posters/"+n)
	}
	return out
}

func testOptions(dir string) Options {
	return Options{Dir: dir, Concurrency: 4, Metadata: true, Prune: true}
}

func source(name string, urls []string) []Source {
	return []Source{{Name: name, Groups: []provider.Group{{Label: "list/one", URLs: urls}}}}
}

// imageFiles lists the non-manifest files in a provider directory.
func imageFiles(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range entries {
		if e.IsDir() || e.Name() == manifestName {
			continue
		}
		names = append(names, e.Name())
	}
	slices.Sort(names)
	return names
}

func TestSyncDownloadsIntoPerProviderDirs(t *testing.T) {
	srv := newImageServer(t)
	root := t.TempDir()

	results, err := Sync(context.Background(), testOptions(root),
		source("tmdb", srv.urls("dune.jpg", "arrival.jpg", "sicario.jpg")))
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 1 {
		t.Fatalf("got %d results, want 1", len(results))
	}
	r := results[0]
	if r.Added != 3 || r.Kept != 0 || r.Removed != 0 || r.Failed != 0 {
		t.Fatalf("unexpected result: %+v", r)
	}
	if r.Bytes == 0 {
		t.Error("no bytes recorded")
	}

	dir := filepath.Join(root, "tmdb")
	files := imageFiles(t, dir)
	if len(files) != 3 {
		t.Fatalf("got files %v, want 3", files)
	}
	// Names are readable and hash-suffixed.
	if !slices.ContainsFunc(files, func(n string) bool { return strings.HasPrefix(n, "dune-") && strings.HasSuffix(n, ".jpg") }) {
		t.Errorf("no readable filename for dune.jpg: %v", files)
	}

	// Metadata made it into the file.
	data, err := os.ReadFile(filepath.Join(dir, files[0]))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"posterlink", "tmdb", "list/one"} {
		if !bytes.Contains(data, []byte(want)) {
			t.Errorf("cached image is missing metadata %q", want)
		}
	}

	m := loadManifest(dir)
	if m.Provider != "tmdb" || len(m.Items) != 3 {
		t.Fatalf("unexpected manifest: %+v", m)
	}
	for _, it := range m.Items {
		if !strings.HasPrefix(it.URL, srv.URL) || it.File == "" || it.Bytes == 0 {
			t.Errorf("incomplete manifest item: %+v", it)
		}
	}
}

func TestSyncIsIdempotent(t *testing.T) {
	srv := newImageServer(t)
	root := t.TempDir()
	urls := srv.urls("dune.jpg", "arrival.jpg")

	if _, err := Sync(context.Background(), testOptions(root), source("tmdb", urls)); err != nil {
		t.Fatal(err)
	}
	before := imageFiles(t, filepath.Join(root, "tmdb"))
	hits := srv.hits.Load()

	results, err := Sync(context.Background(), testOptions(root), source("tmdb", urls))
	if err != nil {
		t.Fatal(err)
	}
	if r := results[0]; r.Kept != 2 || r.Added != 0 || r.Removed != 0 {
		t.Fatalf("second sync was not a no-op: %+v", r)
	}
	if got := srv.hits.Load(); got != hits {
		t.Errorf("second sync made %d extra request(s); want 0", got-hits)
	}
	if after := imageFiles(t, filepath.Join(root, "tmdb")); !slices.Equal(before, after) {
		t.Errorf("files changed: %v -> %v", before, after)
	}
}

func TestSyncAddsAndPrunes(t *testing.T) {
	srv := newImageServer(t)
	root := t.TempDir()
	dir := filepath.Join(root, "tmdb")

	if _, err := Sync(context.Background(), testOptions(root),
		source("tmdb", srv.urls("dune.jpg", "arrival.jpg"))); err != nil {
		t.Fatal(err)
	}

	// arrival.jpg is gone from the provider; blade-runner.jpg is new.
	results, err := Sync(context.Background(), testOptions(root),
		source("tmdb", srv.urls("dune.jpg", "blade-runner.jpg")))
	if err != nil {
		t.Fatal(err)
	}
	if r := results[0]; r.Kept != 1 || r.Added != 1 || r.Removed != 1 {
		t.Fatalf("unexpected result: %+v", r)
	}

	files := imageFiles(t, dir)
	if len(files) != 2 {
		t.Fatalf("got %v, want 2 files", files)
	}
	for _, f := range files {
		if strings.HasPrefix(f, "arrival-") {
			t.Errorf("pruned image %s is still on disk", f)
		}
	}
	if len(loadManifest(dir).Items) != 2 {
		t.Error("manifest was not rewritten")
	}
}

func TestSyncNoPruneKeepsRemovedImages(t *testing.T) {
	srv := newImageServer(t)
	root := t.TempDir()

	if _, err := Sync(context.Background(), testOptions(root),
		source("tmdb", srv.urls("dune.jpg", "arrival.jpg"))); err != nil {
		t.Fatal(err)
	}

	opts := testOptions(root)
	opts.Prune = false
	results, err := Sync(context.Background(), opts, source("tmdb", srv.urls("dune.jpg")))
	if err != nil {
		t.Fatal(err)
	}
	if r := results[0]; r.Removed != 0 {
		t.Fatalf("removed %d files with --no-prune", r.Removed)
	}
	if files := imageFiles(t, filepath.Join(root, "tmdb")); len(files) != 2 {
		t.Errorf("got %v, want both files kept", files)
	}
}

func TestSyncLeavesForeignDirectoriesAlone(t *testing.T) {
	srv := newImageServer(t)
	root := t.TempDir()
	dir := filepath.Join(root, "tmdb")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	mine := filepath.Join(dir, "my-own-wallpaper.jpg")
	if err := os.WriteFile(mine, []byte("not posterlink's"), 0o644); err != nil {
		t.Fatal(err)
	}

	results, err := Sync(context.Background(), testOptions(root), source("tmdb", srv.urls("dune.jpg")))
	if err != nil {
		t.Fatal(err)
	}
	if r := results[0]; r.Added != 1 || r.Removed != 0 {
		t.Fatalf("unexpected result: %+v", r)
	}
	if !fileExists(mine) {
		t.Fatal("posterlink deleted a file it did not create")
	}

	// A second run, now with a manifest in place, must still not touch the
	// pre-existing file: posterlink only deletes what it downloaded itself.
	if _, err := Sync(context.Background(), testOptions(root), source("tmdb", srv.urls("dune.jpg"))); err != nil {
		t.Fatal(err)
	}
	if !fileExists(mine) {
		t.Error("foreign file deleted on the second run")
	}
}

func TestSyncPrunesStaleProviderDirs(t *testing.T) {
	srv := newImageServer(t)
	root := t.TempDir()

	sources := []Source{
		{Name: "tmdb", Groups: []provider.Group{{Label: "l", URLs: srv.urls("dune.jpg")}}},
		{Name: "itunes_jazz", Groups: []provider.Group{{Label: "l", URLs: srv.urls("kind-of-blue.jpg")}}},
	}
	if _, err := Sync(context.Background(), testOptions(root), sources); err != nil {
		t.Fatal(err)
	}
	// A directory posterlink did not create must survive.
	foreign := filepath.Join(root, "family-photos")
	if err := os.MkdirAll(foreign, 0o755); err != nil {
		t.Fatal(err)
	}

	// itunes_jazz is dropped from the config.
	if _, err := Sync(context.Background(), testOptions(root), sources[:1]); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "itunes_jazz")); !os.IsNotExist(err) {
		t.Error("stale provider directory was not removed")
	}
	if _, err := os.Stat(foreign); err != nil {
		t.Errorf("unrelated directory was removed: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "tmdb")); err != nil {
		t.Errorf("active provider directory was removed: %v", err)
	}
}

func TestSyncKeepDirsProtectsFailedProviders(t *testing.T) {
	srv := newImageServer(t)
	root := t.TempDir()

	sources := []Source{
		{Name: "tmdb", Groups: []provider.Group{{Label: "l", URLs: srv.urls("dune.jpg")}}},
		{Name: "steam", Groups: []provider.Group{{Label: "l", URLs: srv.urls("hades.jpg")}}},
	}
	if _, err := Sync(context.Background(), testOptions(root), sources); err != nil {
		t.Fatal(err)
	}

	// steam's API is down this run: it is not in sources, but is protected.
	opts := testOptions(root)
	opts.KeepDirs = []string{"steam"}
	if _, err := Sync(context.Background(), opts, sources[:1]); err != nil {
		t.Fatal(err)
	}
	if files := imageFiles(t, filepath.Join(root, "steam")); len(files) != 1 {
		t.Errorf("protected provider's cache was pruned: %v", files)
	}
}

func TestSyncSkipsNonImagesAndErrors(t *testing.T) {
	srv := newImageServer(t)
	root := t.TempDir()

	results, err := Sync(context.Background(), testOptions(root),
		source("tmdb", srv.urls("dune.jpg", "error-page.html", "gone.missing")))
	if err != nil {
		t.Fatal(err)
	}
	r := results[0]
	if r.Added != 1 || r.Failed != 2 {
		t.Fatalf("unexpected result: %+v", r)
	}
	if files := imageFiles(t, filepath.Join(root, "tmdb")); len(files) != 1 {
		t.Errorf("got %v, want only the real image", files)
	}
}

func TestSyncLimit(t *testing.T) {
	srv := newImageServer(t)
	root := t.TempDir()

	sources := source("tmdb", srv.urls("a.jpg", "b.jpg", "c.jpg", "d.jpg"))
	sources[0].Limit = 2
	results, err := Sync(context.Background(), testOptions(root), sources)
	if err != nil {
		t.Fatal(err)
	}
	if r := results[0]; r.Added != 2 {
		t.Fatalf("limit not applied: %+v", r)
	}
}

func TestSyncDryRunWritesNothing(t *testing.T) {
	srv := newImageServer(t)
	root := t.TempDir()

	opts := testOptions(root)
	opts.DryRun = true
	results, err := Sync(context.Background(), opts, source("tmdb", srv.urls("dune.jpg", "arrival.jpg")))
	if err != nil {
		t.Fatal(err)
	}
	if r := results[0]; r.Added != 2 {
		t.Fatalf("dry run should still report work: %+v", r)
	}
	if entries, err := os.ReadDir(root); err == nil && len(entries) > 0 {
		t.Errorf("dry run created %d entry/entries", len(entries))
	}
}

func TestSyncDedupesAcrossGroups(t *testing.T) {
	srv := newImageServer(t)
	root := t.TempDir()

	urls := srv.urls("dune.jpg")
	sources := []Source{{Name: "tmdb", Groups: []provider.Group{
		{Label: "movie/popular", URLs: urls},
		{Label: "trending/movie/week", URLs: urls},
	}}}
	results, err := Sync(context.Background(), testOptions(root), sources)
	if err != nil {
		t.Fatal(err)
	}
	if r := results[0]; r.Added != 1 {
		t.Fatalf("duplicate URL downloaded twice: %+v", r)
	}
	for _, it := range loadManifest(filepath.Join(root, "tmdb")).Items {
		if it.Label != "movie/popular" {
			t.Errorf("label = %q, want the first group's", it.Label)
		}
	}
}

func TestSyncRecoversFromCorruptManifest(t *testing.T) {
	srv := newImageServer(t)
	root := t.TempDir()
	urls := srv.urls("dune.jpg")

	if _, err := Sync(context.Background(), testOptions(root), source("tmdb", urls)); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(root, "tmdb")
	if err := os.WriteFile(filepath.Join(dir, manifestName), []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}

	results, err := Sync(context.Background(), testOptions(root), source("tmdb", urls))
	if err != nil {
		t.Fatal(err)
	}
	// The image is re-downloaded to the same deterministic name, and the
	// orphaned copy is not left behind.
	if r := results[0]; r.Added != 1 {
		t.Fatalf("unexpected result: %+v", r)
	}
	if files := imageFiles(t, dir); len(files) != 1 {
		t.Errorf("got %v, want 1 file", files)
	}
	var m manifest
	data, err := os.ReadFile(filepath.Join(dir, manifestName))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatalf("manifest not rewritten: %v", err)
	}
}

func TestSyncSidecarForUnwritableFormat(t *testing.T) {
	root := t.TempDir()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "image/webp")
		_, _ = w.Write([]byte("RIFF....WEBPVP8 fake"))
	}))
	defer srv.Close()

	results, err := Sync(context.Background(), testOptions(root), source("unsplash", []string{srv.URL + "/photo.webp"}))
	if err != nil {
		t.Fatal(err)
	}
	if r := results[0]; r.Added != 1 {
		t.Fatalf("unexpected result: %+v", r)
	}

	dir := filepath.Join(root, "unsplash")
	files := imageFiles(t, dir)
	if len(files) != 2 {
		t.Fatalf("got %v, want the image plus an .xmp sidecar", files)
	}
	var sidecar string
	for _, f := range files {
		if strings.HasSuffix(f, ".xmp") {
			sidecar = f
		}
	}
	if sidecar == "" {
		t.Fatalf("no sidecar written: %v", files)
	}
	data, err := os.ReadFile(filepath.Join(dir, sidecar))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(data, []byte("unsplash")) {
		t.Error("sidecar is missing the provider")
	}

	// The sidecar survives a no-op sync.
	if _, err := Sync(context.Background(), testOptions(root), source("unsplash", []string{srv.URL + "/photo.webp"})); err != nil {
		t.Fatal(err)
	}
	if files := imageFiles(t, dir); len(files) != 2 {
		t.Errorf("got %v after re-sync, want image + sidecar", files)
	}
}

func TestSyncRejectsCollidingProviderDirs(t *testing.T) {
	root := t.TempDir()
	sources := []Source{
		{Name: "itunes", Dir: "music", Groups: []provider.Group{{URLs: []string{"http://x/a.jpg"}}}},
		{Name: "itunes_jazz", Dir: "music", Groups: []provider.Group{{URLs: []string{"http://x/b.jpg"}}}},
	}
	_, err := Sync(context.Background(), testOptions(root), sources)
	if err == nil {
		t.Fatal("expected an error for two providers sharing a directory")
	}
	if !strings.Contains(err.Error(), "cache_dir") {
		t.Errorf("error should suggest the fix: %v", err)
	}
}

func TestSyncRequiresDir(t *testing.T) {
	if _, err := Sync(context.Background(), Options{}, nil); err == nil {
		t.Fatal("expected an error with no directory configured")
	}
}

func TestFileNameAndExt(t *testing.T) {
	name := func(u, providerName string) string {
		return fileName(desired{key: hashURL(u), url: u}, providerName, ".jpg")
	}

	t.Run("carries the url hash, so the name is stable across runs", func(t *testing.T) {
		u := "https://image.tmdb.org/t/p/original/1pdfLvkbY9ohJlCjQH2CZjjYVvJ.jpg"
		if got := name(u, "tmdb"); !strings.Contains(got, hashURL(u)[:hashLen]) {
			t.Errorf("fileName = %q, want it to contain %q", got, hashURL(u)[:hashLen])
		}
	})

	// One case per provider's URL shape: the descriptive segment is not always
	// the last one.
	for _, tc := range []struct {
		name, url, provider, want string
	}{
		{
			name:     "tmdb poster id",
			url:      "https://image.tmdb.org/t/p/original/1pdfLvkbY9ohJlCjQH2CZjjYVvJ.jpg",
			provider: "tmdb",
			want:     "1pdflvkby9ohjlcjqh2czjjyvvj-",
		},
		{
			name:     "percent-encoded commons title",
			url:      "https://commons.wikimedia.org/wiki/Special:FilePath/Mona%20Lisa%2C%20by%20Leonardo.jpg?width=1080",
			provider: "wikidata",
			want:     "mona-lisa-by-leonardo-",
		},
		{
			name:     "steam app id over the size token",
			url:      "https://cdn.cloudflare.steamstatic.com/steam/apps/1091500/library_600x900_2x.jpg",
			provider: "steam",
			want:     "1091500-",
		},
		{
			name:     "itunes size token falls back to the provider",
			url:      "https://is1-ssl.mzstatic.com/image/thumb/Music116/v4/2f/ab/9c/2fab9c4e1d0f4a7b8c3d/1200x1200bb.jpg",
			provider: "itunes_jazz",
			want:     "itunes_jazz-",
		},
		{
			name:     "itunes original upload name, extension and all",
			url:      "https://is1-ssl.mzstatic.com/image/thumb/Music116/v4/2f/ab/9c/dj-gwfgbuhy.jpg/1200x1200bb.jpg",
			provider: "itunes",
			want:     "dj-gwfgbuhy-",
		},
		{
			name:     "artic iiif default falls back to the provider",
			url:      "https://www.artic.edu/iiif/2/3c27b499-af56-f0d5-93b5-a7f2f1ad5813/2025,0,4758,8460/800,1422/0/default.jpg",
			provider: "artic",
			want:     "artic-",
		},
		{
			name:     "unsplash photo id",
			url:      "https://images.unsplash.com/photo-1506905925346-21bda4d32df4?ixlib=rb-4.0.3&w=1080",
			provider: "unsplash",
			want:     "photo-1506905925346-21bda4d32df4-",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := name(tc.url, tc.provider)
			if !strings.HasPrefix(got, tc.want) || !strings.HasSuffix(got, ".jpg") {
				t.Errorf("fileName = %q, want prefix %q", got, tc.want)
			}
			if len(got) > slugLen+hashLen+8 {
				t.Errorf("fileName %q is too long", got)
			}
		})
	}

	t.Run("hash only when nothing is usable", func(t *testing.T) {
		got := name("https://example.com/", "")
		if len(got) != 16+len(".jpg") {
			t.Errorf("fileName = %q", got)
		}
	})

	t.Run("content type wins over the url", func(t *testing.T) {
		ext, err := imageExt("image/png; charset=binary", nil)
		if err != nil || ext != ".png" {
			t.Errorf("ext = %q, err = %v", ext, err)
		}
	})

	t.Run("sniffs octet-stream", func(t *testing.T) {
		var buf bytes.Buffer
		_ = jpeg.Encode(&buf, image.NewRGBA(image.Rect(0, 0, 4, 4)), nil)
		ext, err := imageExt("application/octet-stream", buf.Bytes())
		if err != nil || ext != ".jpg" {
			t.Errorf("ext = %q, err = %v", ext, err)
		}
	})

	t.Run("rejects non-images", func(t *testing.T) {
		if _, err := imageExt("text/html", []byte("<html>")); err == nil {
			t.Error("expected an error for text/html")
		}
	})
}

func TestSanitizeProviderName(t *testing.T) {
	cases := map[string]string{
		"tmdb":        "tmdb",
		"itunes_jazz": "itunes_jazz",
		"../escape":   "escape",
		"a/b":         "a-b",
		"":            "provider",
	}
	for in, want := range cases {
		if got := sanitize(in); got != want {
			t.Errorf("sanitize(%q) = %q, want %q", in, got, want)
		}
	}
}
