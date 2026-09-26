// Package server wires providers into a pool, refreshes it on a ticker and
// serves the /poster redirect and /healthz endpoints.
package server

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/ygelfand/posterlink/internal/pool"
	"github.com/ygelfand/posterlink/internal/provider"
)

// Server holds the running state: the providers, the shared pool and the
// last-good URLs per provider (so one provider failing never blanks the wall).
type Server struct {
	providers []provider.Provider
	pool      *pool.Pool
	interval  time.Duration
	log       *slog.Logger

	images *http.Client
	bad    *badURLs

	mu        sync.Mutex
	lastGood  map[string][]provider.Image
	enriching map[string]bool
}

// New constructs a Server.
func New(providers []provider.Provider, interval time.Duration, log *slog.Logger) *Server {
	return &Server{
		providers: providers,
		pool:      pool.New(),
		interval:  interval,
		log:       log,
		images:    &http.Client{Timeout: fetchTimeout},
		bad:       newBadURLs(),
		lastGood:  make(map[string][]provider.Image),
		enriching: make(map[string]bool),
	}
}

// Run does a synchronous first refresh, then refreshes on a ticker until ctx is
// cancelled. It returns immediately if the first refresh cannot be scheduled;
// the pool simply stays empty (the handler returns 503) until a refresh lands.
func (s *Server) Run(ctx context.Context) {
	s.refresh(ctx)

	ticker := time.NewTicker(s.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.refresh(ctx)
		}
	}
}

// refresh fetches every provider concurrently and rebuilds the pool from the
// merged last-good results.
func (s *Server) refresh(ctx context.Context) {
	var wg sync.WaitGroup
	for _, p := range s.providers {
		wg.Add(1)
		go func(p provider.Provider) {
			defer wg.Done()
			images, err := p.Fetch(ctx)
			if err != nil {
				s.log.Warn("provider fetch failed", "provider", p.Name(), "error", err)
				return
			}
			if len(images) == 0 {
				s.log.Warn("provider returned no images", "provider", p.Name())
				return
			}
			s.mu.Lock()
			s.lastGood[p.Name()] = images
			s.mu.Unlock()
			s.log.Debug("provider refreshed", "provider", p.Name(), "images", len(images))

			if e, ok := p.(provider.Enricher); ok {
				s.enrich(p, e, images)
			}
		}(p)
	}
	wg.Wait()

	s.publish()
	s.bad.reset()
	s.log.Info("pool refreshed", "size", s.pool.Size(), "sources", s.pool.Stats())
}

// publish rebuilds the pool from the merged last-good results.
func (s *Server) publish() {
	sources := make([]pool.Source, 0, len(s.providers))
	s.mu.Lock()
	for _, p := range s.providers {
		if images, ok := s.lastGood[p.Name()]; ok {
			sources = append(sources, pool.Source{
				Name:   p.Name(),
				Weight: p.Weight(),
				Images: images,
			})
		}
	}
	s.mu.Unlock()
	s.pool.Set(sources)
}

// enrich fills in metadata that costs extra requests, in the background: the
// pool is already serving by then, and the result is published when it lands.
// It works on a copy, since the pool is reading the original.
func (s *Server) enrich(p provider.Provider, e provider.Enricher, images []provider.Image) {
	name := p.Name()

	s.mu.Lock()
	if s.enriching[name] {
		s.mu.Unlock()
		return
	}
	s.enriching[name] = true
	s.mu.Unlock()

	go func() {
		defer func() {
			s.mu.Lock()
			s.enriching[name] = false
			s.mu.Unlock()
		}()

		ctx, cancel := context.WithTimeout(context.Background(), enrichTimeout)
		defer cancel()

		enriched := e.Enrich(ctx, slices.Clone(images))
		s.mu.Lock()
		s.lastGood[name] = enriched
		s.mu.Unlock()

		s.publish()
		s.log.Debug("provider metadata enriched", "provider", name, "images", len(enriched))
	}()
}

// Handler returns the HTTP router.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /poster", s.handlePoster)
	mux.HandleFunc("GET /poster.jpg", s.handlePosterImage)
	mux.HandleFunc("GET /healthz", s.handleHealthz)
	return mux
}

// handlePoster 302-redirects to a random image. An optional comma-delimited
// ?providers= filter restricts the pick to those (enabled) providers; other
// query params (e.g. the wallpanel cache-buster) are ignored.
func (s *Server) handlePoster(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Access-Control-Allow-Origin", "*")

	allow := parseProviders(r.URL.Query().Get("providers"))
	url, ok := s.pool.RandomFrom(allow)
	if !ok {
		if s.pool.Size() == 0 {
			http.Error(w, "warming up", http.StatusServiceUnavailable)
		} else {
			http.Error(w, "no images for requested providers", http.StatusNotFound)
		}
		return
	}
	http.Redirect(w, r, url, http.StatusFound)
}

// handlePosterImage serves the image bytes instead of redirecting, retrying
// with another image when one fails to download or does not validate.
func (s *Server) handlePosterImage(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Access-Control-Allow-Origin", "*")

	allow := parseProviders(r.URL.Query().Get("providers"))
	skip := s.bad.snapshot()

	attempted := 0
	for range fetchAttempts {
		pick, ok := s.pool.RandomPick(allow, skip)
		if !ok {
			break
		}
		attempted++

		img, permanent, err := s.fetch(r.Context(), pick)
		if err == nil {
			w.Header().Set("Content-Type", img.mime)
			w.Header().Set("Content-Length", strconv.Itoa(len(img.body)))
			w.Header().Set("X-Poster-Provider", pick.Provider)
			w.Header().Set("X-Poster-Source", pick.URL)
			_, _ = w.Write(img.body)
			return
		}
		if r.Context().Err() != nil {
			return
		}

		s.log.Warn("image fetch failed", "provider", pick.Provider, "url", pick.URL,
			"permanent", permanent, "error", err)
		if permanent {
			s.bad.add(pick.URL)
		}
		if skip == nil {
			skip = make(map[string]struct{})
		}
		skip[pick.URL] = struct{}{}
	}

	switch {
	case attempted > 0:
		http.Error(w, "no image could be fetched", http.StatusBadGateway)
	case s.pool.Size() == 0:
		http.Error(w, "warming up", http.StatusServiceUnavailable)
	default:
		http.Error(w, "no images for requested providers", http.StatusNotFound)
	}
}

// parseProviders splits a comma-delimited providers value into a set. Only
// providers present in the pool (i.e. enabled) will actually match.
func parseProviders(raw string) map[string]struct{} {
	if raw == "" {
		return nil
	}
	set := make(map[string]struct{})
	for name := range strings.SplitSeq(raw, ",") {
		if name = strings.TrimSpace(name); name != "" {
			set[name] = struct{}{}
		}
	}
	return set
}

// handleHealthz reports readiness and pool composition.
func (s *Server) handleHealthz(w http.ResponseWriter, _ *http.Request) {
	size := s.pool.Size()
	w.Header().Set("Content-Type", "application/json")
	if size == 0 {
		w.WriteHeader(http.StatusServiceUnavailable)
	}
	_ = json.NewEncoder(w).Encode(map[string]any{
		"status":  statusFor(size),
		"size":    size,
		"sources": s.pool.Stats(),
		"invalid": s.bad.len(),
	})
}

func statusFor(size int) string {
	if size == 0 {
		return "warming up"
	}
	return "ok"
}

// ListenAndServe runs the HTTP server on addr until ctx is cancelled, then
// shuts down gracefully.
func (s *Server) ListenAndServe(ctx context.Context, addr string) error {
	srv := &http.Server{
		Addr:              addr,
		Handler:           s.Handler(),
		ReadHeaderTimeout: 5 * time.Second,
	}

	errCh := make(chan error, 1)
	go func() {
		s.log.Info("listening", "addr", addr)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			errCh <- err
		}
	}()

	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := srv.Shutdown(shutdownCtx); err != nil {
			return fmt.Errorf("shutdown: %w", err)
		}
		return nil
	}
}
