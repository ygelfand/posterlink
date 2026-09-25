package server

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"image"
	_ "image/gif"
	_ "image/jpeg"
	_ "image/png"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/ygelfand/posterlink/internal/exifwrite"
	"github.com/ygelfand/posterlink/internal/pool"
)

const (
	fetchAttempts = 3
	fetchTimeout  = 10 * time.Second
	enrichTimeout = 5 * time.Minute
	maxImageBytes = 32 << 20
	// artic.edu 403s any user agent containing a URL.
	imageUserAgent = "posterlink"
)

type fetched struct {
	body []byte
	mime string
}

type badURLs struct {
	mu  sync.RWMutex
	set map[string]struct{}
}

func newBadURLs() *badURLs { return &badURLs{set: make(map[string]struct{})} }

func (b *badURLs) add(url string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.set[url] = struct{}{}
}

func (b *badURLs) snapshot() map[string]struct{} {
	b.mu.RLock()
	defer b.mu.RUnlock()
	if len(b.set) == 0 {
		return nil
	}
	out := make(map[string]struct{}, len(b.set))
	for u := range b.set {
		out[u] = struct{}{}
	}
	return out
}

func (b *badURLs) reset() {
	b.mu.Lock()
	defer b.mu.Unlock()
	clear(b.set)
}

func (b *badURLs) len() int {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return len(b.set)
}

func (s *Server) fetch(ctx context.Context, pick pool.Pick) (fetched, bool, error) {
	ctx, cancel := context.WithTimeout(ctx, fetchTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, pick.URL, nil)
	if err != nil {
		return fetched{}, true, err
	}
	req.Header.Set("Accept", "image/*,*/*;q=0.8")
	req.Header.Set("User-Agent", imageUserAgent)

	resp, err := s.images.Do(req)
	if err != nil {
		return fetched{}, false, err
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		permanent := resp.StatusCode >= 400 && resp.StatusCode < 500 &&
			resp.StatusCode != http.StatusTooManyRequests
		return fetched{}, permanent, fmt.Errorf("unexpected status %s", resp.Status)
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxImageBytes+1))
	if err != nil {
		return fetched{}, false, err
	}
	if len(body) > maxImageBytes {
		return fetched{}, true, fmt.Errorf("image larger than %d bytes", maxImageBytes)
	}

	mime, err := validate(body)
	if err != nil {
		return fetched{}, true, err
	}

	if tagged, ok, err := exifwrite.Apply(body, imageMeta(pick)); err == nil && ok {
		body = tagged
	}
	return fetched{body: body, mime: mime}, false, nil
}

func validate(body []byte) (string, error) {
	if len(body) == 0 {
		return "", errors.New("empty response")
	}
	mime, _, _ := strings.Cut(http.DetectContentType(body), ";")
	if !strings.HasPrefix(mime, "image/") {
		return "", fmt.Errorf("not an image (%s)", mime)
	}
	switch mime {
	case "image/jpeg", "image/png", "image/gif":
		cfg, _, err := image.DecodeConfig(bytes.NewReader(body))
		if err != nil {
			return "", fmt.Errorf("corrupt %s: %w", mime, err)
		}
		if cfg.Width <= 0 || cfg.Height <= 0 {
			return "", fmt.Errorf("degenerate image %dx%d", cfg.Width, cfg.Height)
		}
	}
	return mime, nil
}

func imageMeta(pick pool.Pick) exifwrite.Meta {
	m := exifwrite.Meta{
		Description: pick.Caption(),
		Title:       pick.Title,
		Source:      pick.URL,
	}
	if t, ok := pick.Parsed(); ok {
		m.Taken = t
	}
	return m
}
