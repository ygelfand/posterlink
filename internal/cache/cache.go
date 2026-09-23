// Package cache mirrors the configured providers' images onto the local
// filesystem so a photo library (Immich, say) can index them, instead of
// serving redirects to remote URLs.
//
// The layout is one directory per provider config block:
//
//	<dir>/tmdb/dune-part-two-6f3a1c9d21.jpg
//	<dir>/tmdb/.posterlink.json
//	<dir>/itunes_jazz/kind-of-blue-0b77e1ca04.jpg
//
// Each directory carries a manifest keyed by a hash of the source URL, so a
// sync is idempotent and cheap: URLs already on disk are left untouched (no
// request is made for them at all — posters are assumed immutable), URLs the
// providers no longer return are deleted, and new ones are downloaded and
// tagged with EXIF/XMP metadata.
//
// Deletion is deliberately narrow: only files the manifest records as
// downloaded by posterlink are removed, and only directories holding such a
// manifest. Anything else in the cache root — a directory of your own photos,
// a file you dropped in next to the posters — is left alone, so the root can
// be shared with other content.
package cache

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/ygelfand/posterlink/internal/exifwrite"
	"github.com/ygelfand/posterlink/internal/provider"
)

const (
	// manifestName is the per-directory state file; it doubles as the marker
	// that says "posterlink owns this directory and may delete from it".
	manifestName    = ".posterlink.json"
	manifestVersion = 1
	tmpPrefix       = ".posterlink-tmp-"
	sidecarExt      = ".xmp"

	maxImageBytes = 64 << 20
	hashLen       = 10 // hex characters of the URL hash kept in filenames
	slugLen       = 48
)

// Options controls a sync run.
type Options struct {
	// Dir is the cache root; provider directories are created beneath it.
	Dir string
	// Concurrency is the number of parallel downloads (default 4).
	Concurrency int
	// Metadata enables EXIF/XMP tagging of downloaded images.
	Metadata bool
	// Prune deletes cached files (and whole directories, for providers that
	// are gone from the config) that the providers no longer return.
	Prune bool
	// KeepDirs names provider directories that must survive pruning even
	// though they are not part of this run — e.g. a provider whose fetch just
	// failed, or one excluded by --provider. Without this a transient API
	// failure would look like "the provider has no images" and wipe its cache.
	KeepDirs []string
	// DryRun reports what would change without writing anything.
	DryRun bool
	// UserAgent is sent with every image request.
	UserAgent string
	// Timeout bounds a single image download (default 60s).
	Timeout time.Duration
	// Client, if set, is used for downloads (tests inject one).
	Client *http.Client
	// Log receives progress at debug level and problems at warn level.
	Log *slog.Logger
}

// Source is one provider instance to mirror. Groups are the provider's labeled
// URL sets (a single group for providers that are not provider.Previewer); the
// label is recorded in each image's metadata.
type Source struct {
	Name   string
	Dir    string // directory name; defaults to Name
	Groups []provider.Group
	Limit  int // cap on images kept for this provider; 0 means no cap
}

// Result summarizes what a sync did to one provider directory.
type Result struct {
	Name    string
	Dir     string
	Kept    int
	Added   int
	Removed int
	Failed  int
	Bytes   int64
	Err     error
}

// Total is the number of images in the directory after the sync.
func (r Result) Total() int { return r.Kept + r.Added }

// manifest is the on-disk state of one provider directory.
type manifest struct {
	Version  int             `json:"version"`
	Provider string          `json:"provider"`
	Updated  time.Time       `json:"updated"`
	Items    map[string]item `json:"items"` // keyed by URL hash
}

// item is one cached image.
type item struct {
	URL        string    `json:"url"`
	File       string    `json:"file"`
	Label      string    `json:"label,omitempty"`
	Sidecar    bool      `json:"sidecar,omitempty"`
	Bytes      int64     `json:"bytes"`
	Downloaded time.Time `json:"downloaded"`
}

// desired is one URL a provider currently returns.
type desired struct {
	key   string // hash of url
	url   string
	label string
}

type syncer struct {
	opts   Options
	client *http.Client
	log    *slog.Logger
}

// Sync mirrors every source into opts.Dir and returns one Result per source.
// Per-provider problems are reported in the Result (and joined into the error)
// rather than aborting the run, so one bad provider cannot stop the others.
func Sync(ctx context.Context, opts Options, sources []Source) ([]Result, error) {
	if strings.TrimSpace(opts.Dir) == "" {
		return nil, errors.New("cache: no directory configured (set cache.dir or pass --dir)")
	}
	root, err := filepath.Abs(opts.Dir)
	if err != nil {
		return nil, err
	}
	if opts.Concurrency < 1 {
		opts.Concurrency = 4
	}
	if opts.Timeout <= 0 {
		opts.Timeout = 60 * time.Second
	}
	if opts.Log == nil {
		opts.Log = slog.New(slog.DiscardHandler)
	}
	opts.Dir = root

	if !opts.DryRun {
		if err := os.MkdirAll(root, 0o755); err != nil {
			return nil, fmt.Errorf("cache: %w", err)
		}
	}

	if err := checkDirs(sources); err != nil {
		return nil, err
	}

	s := &syncer{opts: opts, client: opts.Client, log: opts.Log}
	if s.client == nil {
		s.client = &http.Client{Timeout: opts.Timeout}
	}

	results := make([]Result, 0, len(sources))
	var errs []error
	for _, src := range sources {
		r := s.syncSource(ctx, root, src)
		results = append(results, r)
		if r.Err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", src.Name, r.Err))
		}
		if ctx.Err() != nil {
			errs = append(errs, ctx.Err())
			break
		}
	}

	if opts.Prune && ctx.Err() == nil {
		if err := s.pruneDirs(root, sources); err != nil {
			errs = append(errs, err)
		}
	}
	return results, errors.Join(errs...)
}

// checkDirs rejects two sources that would share a directory: they would each
// prune the other's images out of the one manifest they both write.
func checkDirs(sources []Source) error {
	seen := make(map[string]string, len(sources))
	for _, src := range sources {
		dir := sanitize(dirName(src))
		if other, dup := seen[dir]; dup {
			return fmt.Errorf("cache: providers %q and %q would both use %s/ (set a distinct cache_dir on one)",
				other, src.Name, dir)
		}
		seen[dir] = src.Name
	}
	return nil
}

// dirName is the directory a source syncs into, before sanitizing.
func dirName(src Source) string {
	if src.Dir != "" {
		return src.Dir
	}
	return src.Name
}

// syncSource brings one provider directory in line with src.
func (s *syncer) syncSource(ctx context.Context, root string, src Source) Result {
	dir := filepath.Join(root, sanitize(dirName(src)))
	r := Result{Name: src.Name, Dir: dir}

	if err := s.ensureDir(dir); err != nil {
		r.Err = err
		return r
	}

	old := loadManifest(dir)
	next := manifest{
		Version:  manifestVersion,
		Provider: src.Name,
		Updated:  time.Now().UTC(),
		Items:    make(map[string]item),
	}

	var todo []desired
	for _, d := range flatten(src.Groups, src.Limit) {
		if it, ok := old.Items[d.key]; ok && fileExists(filepath.Join(dir, it.File)) {
			it.Label = d.label // labels can change without the image changing
			next.Items[d.key] = it
			r.Kept++
			continue
		}
		todo = append(todo, d)
	}

	s.log.Debug("cache plan", "provider", src.Name, "keep", r.Kept, "download", len(todo), "dir", dir)
	s.download(ctx, dir, todo, &next, &r)

	removed, err := s.pruneFiles(dir, old, next)
	r.Removed = removed
	if err != nil {
		r.Err = errors.Join(r.Err, err)
	}

	if !s.opts.DryRun {
		if err := writeManifest(dir, next); err != nil {
			r.Err = errors.Join(r.Err, err)
		}
	}
	return r
}

// ensureDir creates the provider's directory if it is missing.
func (s *syncer) ensureDir(dir string) error {
	info, err := os.Stat(dir)
	switch {
	case errors.Is(err, os.ErrNotExist):
		if s.opts.DryRun {
			return nil
		}
		return os.MkdirAll(dir, 0o755)
	case err != nil:
		return err
	case !info.IsDir():
		return fmt.Errorf("%s exists and is not a directory", dir)
	}
	return nil
}

// flatten dedupes the groups' URLs (first label wins) and applies limit.
func flatten(groups []provider.Group, limit int) []desired {
	seen := make(map[string]struct{})
	var out []desired
	for _, g := range groups {
		for _, u := range g.URLs {
			if u == "" {
				continue
			}
			if _, dup := seen[u]; dup {
				continue
			}
			seen[u] = struct{}{}
			out = append(out, desired{key: hashURL(u), url: u, label: g.Label})
			if limit > 0 && len(out) == limit {
				return out
			}
		}
	}
	return out
}

// download fetches todo with bounded concurrency, recording successes in next.
func (s *syncer) download(ctx context.Context, dir string, todo []desired, next *manifest, r *Result) {
	if len(todo) == 0 {
		return
	}
	sem := make(chan struct{}, s.opts.Concurrency)
	var wg sync.WaitGroup
	var mu sync.Mutex

	for _, d := range todo {
		if ctx.Err() != nil {
			break
		}
		wg.Add(1)
		sem <- struct{}{}
		go func(d desired) {
			defer wg.Done()
			defer func() { <-sem }()

			it, err := s.fetch(ctx, dir, d, next.Provider)
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				if ctx.Err() == nil {
					s.log.Warn("cache download failed", "provider", next.Provider, "url", d.url, "error", err)
				}
				r.Failed++
				return
			}
			next.Items[d.key] = it
			r.Added++
			r.Bytes += it.Bytes
			s.log.Debug("cached image", "provider", next.Provider, "file", it.File, "bytes", it.Bytes)
		}(d)
	}
	wg.Wait()
}

// fetch downloads one image, tags it and writes it into dir atomically.
func (s *syncer) fetch(ctx context.Context, dir string, d desired, providerName string) (item, error) {
	ctx, cancel := context.WithTimeout(ctx, s.opts.Timeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, d.url, nil)
	if err != nil {
		return item{}, err
	}
	req.Header.Set("Accept", "image/*,*/*;q=0.8")
	if s.opts.UserAgent != "" {
		req.Header.Set("User-Agent", s.opts.UserAgent)
	}

	resp, err := s.client.Do(req)
	if err != nil {
		return item{}, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return item{}, fmt.Errorf("unexpected status %s", resp.Status)
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxImageBytes+1))
	if err != nil {
		return item{}, err
	}
	if len(body) > maxImageBytes {
		return item{}, fmt.Errorf("image larger than %d bytes", maxImageBytes)
	}
	if len(body) == 0 {
		return item{}, errors.New("empty response")
	}

	ext, err := imageExt(resp.Header.Get("Content-Type"), body)
	if err != nil {
		return item{}, err
	}

	now := time.Now()
	it := item{
		URL:        d.url,
		File:       fileName(d, providerName, ext),
		Label:      d.label,
		Bytes:      int64(len(body)),
		Downloaded: now.UTC(),
	}

	out := body
	if s.opts.Metadata {
		tagged, ok, err := exifwrite.Apply(body, s.meta(d, providerName, now))
		switch {
		case err != nil:
			s.log.Warn("could not embed metadata; writing an XMP sidecar instead",
				"provider", providerName, "url", d.url, "error", err)
			it.Sidecar = true
		case ok:
			out = tagged
		default:
			it.Sidecar = true // format we cannot write into
		}
	}

	if s.opts.DryRun {
		return it, nil
	}
	if err := writeFile(filepath.Join(dir, it.File), out, now); err != nil {
		return item{}, err
	}
	if it.Sidecar {
		sidecar := exifwrite.Sidecar(s.meta(d, providerName, now))
		if err := writeFile(filepath.Join(dir, it.File+sidecarExt), sidecar, now); err != nil {
			return item{}, err
		}
	}
	return it, nil
}

// meta describes one image for the metadata writer. The provider goes in the
// make/model pair so a photo library can filter by it like a camera, and the
// group label (e.g. a TMDB list) becomes the description.
func (s *syncer) meta(d desired, providerName string, now time.Time) exifwrite.Meta {
	// Some providers already prefix their group labels with their own name
	// (e.g. "itunes/id:3864756"); do not repeat it.
	label := strings.TrimPrefix(d.label, providerName+"/")
	desc := providerName
	if label != "" && label != providerName {
		desc = providerName + " / " + label
	}
	keywords := []string{"posterlink", providerName}
	if label != "" && label != providerName {
		keywords = append(keywords, label)
	}
	return exifwrite.Meta{
		Title:       titleFrom(d.url),
		Description: desc,
		Make:        "posterlink",
		Model:       providerName,
		Software:    "posterlink",
		Source:      d.url,
		UniqueID:    d.key,
		Keywords:    keywords,
		Taken:       now,
	}
}

// pruneFiles deletes the images the providers no longer return. Only files a
// previous sync recorded in the manifest are candidates, so nothing posterlink
// did not download is ever removed. Leftover temp files (from an interrupted
// run) are always cleaned up, and an image whose manifest entry was lost is
// simply overwritten on the next download, since filenames are derived from
// the source URL.
func (s *syncer) pruneFiles(dir string, old, next manifest) (int, error) {
	keep := map[string]bool{manifestName: true}
	for _, it := range next.Items {
		keep[it.File] = true
		keep[it.File+sidecarExt] = true
	}
	known := make(map[string]bool, len(old.Items)*2)
	for _, it := range old.Items {
		known[it.File] = true
		known[it.File+sidecarExt] = true
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return 0, nil // dry run on a directory that does not exist yet
		}
		return 0, err
	}

	removed := 0
	var errs []error
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if keep[name] {
			continue
		}
		if !strings.HasPrefix(name, tmpPrefix) && (!s.opts.Prune || !known[name]) {
			continue
		}
		if s.opts.DryRun {
			s.log.Debug("would remove", "file", filepath.Join(dir, name))
			removed++
			continue
		}
		if err := os.Remove(filepath.Join(dir, name)); err != nil {
			errs = append(errs, err)
			continue
		}
		s.log.Debug("removed", "file", filepath.Join(dir, name))
		removed++
	}
	return removed, errors.Join(errs...)
}

// pruneDirs removes cache directories whose provider is no longer configured.
// Only directories holding a posterlink manifest are ever removed.
func (s *syncer) pruneDirs(root string, sources []Source) error {
	keep := make(map[string]bool, len(sources)+len(s.opts.KeepDirs))
	for _, src := range sources {
		keep[sanitize(dirName(src))] = true
	}
	for _, name := range s.opts.KeepDirs {
		keep[sanitize(name)] = true
	}

	entries, err := os.ReadDir(root)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}

	var errs []error
	for _, e := range entries {
		if !e.IsDir() || keep[e.Name()] {
			continue
		}
		dir := filepath.Join(root, e.Name())
		if !fileExists(filepath.Join(dir, manifestName)) {
			continue // not ours
		}
		if s.opts.DryRun {
			s.log.Info("would remove stale provider directory", "dir", dir)
			continue
		}
		if err := os.RemoveAll(dir); err != nil {
			errs = append(errs, err)
			continue
		}
		s.log.Info("removed stale provider directory", "dir", dir)
	}
	return errors.Join(errs...)
}

// ── helpers ─────────────────────────────────────────────────────────────────

func hashURL(u string) string {
	sum := sha256.Sum256([]byte(u))
	return hex.EncodeToString(sum[:])[:16]
}

// fileName is deterministic in the URL, so the same image always lands on the
// same path however often it is synced: a readable slug plus enough of the URL
// hash to keep it unique.
func fileName(d desired, providerName, ext string) string {
	slug := slugify(basename(d.url))
	if slug == "" {
		// Nothing in the URL names the image; the provider name at least
		// matches the directory it lands in.
		slug = strings.ToLower(slugifyKeepUnderscore(providerName))
	}
	if slug == "" {
		return d.key[:min(len(d.key), 16)] + ext
	}
	return slug + "-" + d.key[:min(len(d.key), hashLen)] + ext
}

// basename picks the most descriptive path segment of a URL. Image CDNs vary:
// Commons and TMDB put the name last ("Mona Lisa.jpg"), Apple and the Art
// Institute end every URL with the same rendering parameters
// ("1200x1200bb.jpg", "default.jpg"), where the segment before it is the one
// worth keeping. It returns "" when nothing in the URL is descriptive.
func basename(raw string) string {
	p := raw
	if u, err := url.Parse(raw); err == nil {
		p = u.Path
	}
	if decoded, err := url.PathUnescape(p); err == nil {
		p = decoded
	}

	segments := strings.Split(strings.Trim(p, "/"), "/")
	for i := len(segments) - 1; i >= 0 && i >= len(segments)-2; i-- {
		// Apple serves "<original filename>.jpg/1200x1200bb.jpg", so the
		// candidate segment can carry an extension wherever it sits.
		name := trimImageExt(segments[i])
		if descriptive(name) {
			return name
		}
	}
	return ""
}

// trimImageExt drops a trailing file extension from a path segment.
func trimImageExt(name string) string {
	if ext := path.Ext(name); len(ext) > 1 && len(ext) <= 5 {
		return strings.TrimSuffix(name, ext)
	}
	return name
}

// boilerplate names appear at the end of every URL from a given CDN and say
// nothing about the individual image.
var boilerplate = map[string]bool{
	"default": true, "original": true, "full": true, "image": true,
	"images": true, "img": true, "photo": true, "thumb": true,
	"thumbnail": true, "index": true, "file": true, "download": true,
	"large": true, "small": true, "medium": true, "raw": true,
}

// descriptive reports whether a path segment is worth putting in a filename:
// not boilerplate, not a bare size token ("1200x1200bb", "library_600x900_2x"),
// and not a long opaque hex digest.
func descriptive(name string) bool {
	if len(name) < 3 {
		return false
	}
	lower := strings.ToLower(name)
	if boilerplate[lower] {
		return false
	}
	if sizeToken(lower) {
		return false
	}
	return !isHexDigest(lower)
}

// sizeToken matches names that are only dimensions, optionally with a scale or
// crop suffix: "600x900", "1200x1200bb", "library_600x900_2x".
func sizeToken(name string) bool {
	name = strings.TrimPrefix(name, "library_")
	name = strings.TrimPrefix(name, "header_")
	for _, suffix := range []string{"bb", "_2x", "_1x", "2x", "1x"} {
		name = strings.TrimSuffix(name, suffix)
	}
	w, h, ok := strings.Cut(name, "x")
	return ok && allDigits(w) && allDigits(h)
}

func allDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// isHexDigest matches the opaque hashes and UUIDs CDNs use as directory names.
func isHexDigest(s string) bool {
	hex := 0
	for _, r := range s {
		switch {
		case r >= '0' && r <= '9', r >= 'a' && r <= 'f':
			hex++
		case r == '-':
		default:
			return false
		}
	}
	return hex >= 16
}

// titleFrom turns a URL's descriptive segment into a readable title tag.
func titleFrom(raw string) string {
	name := strings.NewReplacer("_", " ", "-", " ").Replace(basename(raw))
	return strings.TrimSpace(strings.Join(strings.Fields(name), " "))
}

// slugify reduces s to a filename-safe slug.
func slugify(s string) string {
	var b strings.Builder
	dash := false
	for _, r := range strings.ToLower(s) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
			dash = false
		case b.Len() > 0 && !dash:
			b.WriteByte('-')
			dash = true
		}
		if b.Len() >= slugLen {
			break
		}
	}
	return strings.Trim(b.String(), "-")
}

// sanitize makes a provider name safe to use as a single path element.
func sanitize(name string) string {
	if s := slugifyKeepUnderscore(name); s != "" {
		return s
	}
	return "provider"
}

func slugifyKeepUnderscore(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '_', r == '-', r == '.':
			b.WriteRune(r)
		default:
			b.WriteByte('-')
		}
	}
	return strings.Trim(b.String(), "-.")
}

// imageExt picks a file extension from the response's content type, falling
// back to sniffing. A non-image body (an HTML error page, say) is an error so
// it never reaches the cache.
func imageExt(contentType string, body []byte) (string, error) {
	typ := contentType
	if mt, _, err := mime.ParseMediaType(contentType); err == nil {
		typ = mt
	}
	typ = strings.ToLower(strings.TrimSpace(typ))
	if typ == "" || typ == "application/octet-stream" || typ == "binary/octet-stream" {
		typ, _, _ = strings.Cut(http.DetectContentType(body), ";")
		typ = strings.ToLower(strings.TrimSpace(typ))
	}

	known := map[string]string{
		"image/jpeg":    ".jpg",
		"image/jpg":     ".jpg",
		"image/png":     ".png",
		"image/webp":    ".webp",
		"image/gif":     ".gif",
		"image/avif":    ".avif",
		"image/tiff":    ".tif",
		"image/heic":    ".heic",
		"image/heif":    ".heif",
		"image/bmp":     ".bmp",
		"image/svg+xml": ".svg",
	}
	if ext, ok := known[typ]; ok {
		return ext, nil
	}
	if !strings.HasPrefix(typ, "image/") {
		return "", fmt.Errorf("not an image (content-type %q)", contentType)
	}
	if exts, err := mime.ExtensionsByType(typ); err == nil && len(exts) > 0 {
		slices.Sort(exts)
		return exts[0], nil
	}
	return ".img", nil
}

func fileExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.Mode().IsRegular()
}

// writeFile writes data atomically and stamps the file with mtime, so a photo
// library that falls back to the filesystem date agrees with the EXIF one.
func writeFile(dst string, data []byte, mtime time.Time) error {
	dir := filepath.Dir(dst)
	f, err := os.CreateTemp(dir, tmpPrefix+"*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer func() {
		if tmp != "" {
			_ = os.Remove(tmp)
		}
	}()

	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmp, 0o644); err != nil {
		return err
	}
	if err := os.Chtimes(tmp, mtime, mtime); err != nil {
		return err
	}
	if err := os.Rename(tmp, dst); err != nil {
		return err
	}
	tmp = ""
	return nil
}

// loadManifest reads dir's manifest, returning an empty one if it is missing
// or unreadable (a corrupt manifest just means everything gets re-downloaded).
func loadManifest(dir string) manifest {
	empty := manifest{Version: manifestVersion, Items: map[string]item{}}
	data, err := os.ReadFile(filepath.Join(dir, manifestName))
	if err != nil {
		return empty
	}
	var m manifest
	if err := json.Unmarshal(data, &m); err != nil || m.Items == nil {
		return empty
	}
	return m
}

func writeManifest(dir string, m manifest) error {
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	return writeFile(filepath.Join(dir, manifestName), append(data, '\n'), time.Now())
}
