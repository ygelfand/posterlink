// Package server wires providers into a pool, refreshes it on a ticker and
// serves the /poster redirect and /healthz endpoints.
package server

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
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

	mu       sync.Mutex
	lastGood map[string][]string
}

// New constructs a Server.
func New(providers []provider.Provider, interval time.Duration, log *slog.Logger) *Server {
	return &Server{
		providers: providers,
		pool:      pool.New(),
		interval:  interval,
		log:       log,
		lastGood:  make(map[string][]string),
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
			urls, err := p.Fetch(ctx)
			if err != nil {
				s.log.Warn("provider fetch failed", "provider", p.Name(), "error", err)
				return
			}
			if len(urls) == 0 {
				s.log.Warn("provider returned no images", "provider", p.Name())
				return
			}
			s.mu.Lock()
			s.lastGood[p.Name()] = urls
			s.mu.Unlock()
			s.log.Debug("provider refreshed", "provider", p.Name(), "images", len(urls))
		}(p)
	}
	wg.Wait()

	sources := make([]pool.Source, 0, len(s.providers))
	s.mu.Lock()
	for _, p := range s.providers {
		if urls, ok := s.lastGood[p.Name()]; ok {
			sources = append(sources, pool.Source{
				Name:   p.Name(),
				Weight: p.Weight(),
				URLs:   urls,
			})
		}
	}
	s.mu.Unlock()

	s.pool.Set(sources)
	s.log.Info("pool refreshed", "size", s.pool.Size(), "sources", s.pool.Stats())
}

// Handler returns the HTTP router.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /poster", s.handlePoster)
	mux.HandleFunc("GET /healthz", s.handleHealthz)
	return mux
}

// handlePoster 302-redirects to a random image, ignoring query params (the
// wallpanel cache-buster is noise to us).
func (s *Server) handlePoster(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	url, ok := s.pool.Random()
	if !ok {
		http.Error(w, "warming up", http.StatusServiceUnavailable)
		return
	}
	http.Redirect(w, r, url, http.StatusFound)
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
