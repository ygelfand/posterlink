// Package cache mirrors the providers' images onto the local filesystem.
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
	manifestName    = ".posterlink.json"
	manifestVersion = 1
	tmpPrefix       = ".posterlink-tmp-"
	sidecarExt      = ".xmp"

	maxImageBytes = 64 << 20
	hashLen       = 10
	slugLen       = 48
)

type Options struct {
	Dir         string
	Concurrency int
	Metadata    bool
	Prune       bool
	KeepDirs    []string
	DryRun      bool
	UserAgent   string
	Timeout     time.Duration
	Client      *http.Client
	Log         *slog.Logger
}

type Source struct {
	Name   string
	Dir    string
	Groups []provider.Group
	Limit  int
}

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

func (r Result) Total() int { return r.Kept + r.Added }

type manifest struct {
	Version  int             `json:"version"`
	Provider string          `json:"provider"`
	Updated  time.Time       `json:"updated"`
	Items    map[string]item `json:"items"`
}

type item struct {
	URL        string    `json:"url"`
	File       string    `json:"file"`
	Label      string    `json:"label,omitempty"`
	Sidecar    bool      `json:"sidecar,omitempty"`
	Bytes      int64     `json:"bytes"`
	Downloaded time.Time `json:"downloaded"`
}

type desired struct {
	key   string
	url   string
	label string
	image provider.Image
}

type syncer struct {
	opts   Options
	client *http.Client
	log    *slog.Logger
}

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

func dirName(src Source) string {
	if src.Dir != "" {
		return src.Dir
	}
	return src.Name
}

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
			it.Label = d.label
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

func flatten(groups []provider.Group, limit int) []desired {
	seen := make(map[string]struct{})
	var out []desired
	for _, g := range groups {
		for _, img := range g.Images {
			if img.URL == "" {
				continue
			}
			if _, dup := seen[img.URL]; dup {
				continue
			}
			seen[img.URL] = struct{}{}
			out = append(out, desired{key: hashURL(img.URL), url: img.URL, label: g.Label, image: img})
			if limit > 0 && len(out) == limit {
				return out
			}
		}
	}
	return out
}

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
			it.Sidecar = true
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

func (s *syncer) meta(d desired, providerName string, now time.Time) exifwrite.Meta {
	m := exifwrite.Meta{
		Description: d.image.Caption(),
		Title:       d.image.Title,
		Source:      d.url,
		UniqueID:    d.key,
		Taken:       now,
	}
	if t, ok := d.image.Parsed(); ok {
		m.Taken = t
	}
	return m
}

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
			return 0, nil
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
			continue
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

func hashURL(u string) string {
	sum := sha256.Sum256([]byte(u))
	return hex.EncodeToString(sum[:])[:16]
}

func fileName(d desired, providerName, ext string) string {
	slug := slugify(d.image.Title)
	if slug == "" {
		slug = slugify(basename(d.url))
	}
	if slug == "" {
		slug = strings.ToLower(slugifyKeepUnderscore(providerName))
	}
	if slug == "" {
		return d.key[:min(len(d.key), 16)] + ext
	}
	return slug + "-" + d.key[:min(len(d.key), hashLen)] + ext
}

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
		name := trimImageExt(segments[i])
		if descriptive(name) {
			return name
		}
	}
	return ""
}

func trimImageExt(name string) string {
	if ext := path.Ext(name); len(ext) > 1 && len(ext) <= 5 {
		return strings.TrimSuffix(name, ext)
	}
	return name
}

var boilerplate = map[string]bool{
	"default": true, "original": true, "full": true, "image": true,
	"images": true, "img": true, "photo": true, "thumb": true,
	"thumbnail": true, "index": true, "file": true, "download": true,
	"large": true, "small": true, "medium": true, "raw": true,
}

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
