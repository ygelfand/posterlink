// Package pool holds the in-memory set of image URLs and serves a random one
// per request. Selection is weighted across providers, then uniform within the
// chosen provider, so blending (e.g. "80% posters, 20% photos") is a matter of
// relative weights rather than pool sizes.
package pool

import (
	"math/rand/v2"
	"sync"
)

// Source is one provider's contribution to the pool.
type Source struct {
	Name   string
	Weight float64
	URLs   []string
}

// Pool is a concurrency-safe, weighted collection of image URLs.
type Pool struct {
	mu       sync.RWMutex
	sources  []Source
	total    float64
	last     string // last served URL, to avoid immediate repeats
}

// New returns an empty pool.
func New() *Pool { return &Pool{} }

// Set replaces the pool contents. Sources with no URLs or non-positive weight
// are dropped. Set is a no-op if it would leave the pool empty, preserving the
// last-good contents on a total refresh failure.
func (p *Pool) Set(sources []Source) {
	kept := make([]Source, 0, len(sources))
	var total float64
	for _, s := range sources {
		if len(s.URLs) == 0 || s.Weight <= 0 {
			continue
		}
		kept = append(kept, s)
		total += s.Weight
	}
	if len(kept) == 0 {
		return
	}

	p.mu.Lock()
	defer p.mu.Unlock()
	p.sources = kept
	p.total = total
}

// Random returns a weighted-random URL. It re-rolls once to avoid serving the
// same URL twice in a row when the pool has more than one entry. The boolean is
// false only when the pool is empty.
func (p *Pool) Random() (string, bool) {
	p.mu.RLock()
	url, ok := p.pick()
	p.mu.RUnlock()
	if !ok {
		return "", false
	}

	if url == p.lastServed() && p.Size() > 1 {
		p.mu.RLock()
		if reroll, ok := p.pick(); ok {
			url = reroll
		}
		p.mu.RUnlock()
	}

	p.mu.Lock()
	p.last = url
	p.mu.Unlock()
	return url, true
}

// pick selects a URL under an already-held read lock.
func (p *Pool) pick() (string, bool) {
	if p.total <= 0 {
		return "", false
	}
	r := rand.Float64() * p.total
	for _, s := range p.sources {
		if r < s.Weight {
			return s.URLs[rand.IntN(len(s.URLs))], true
		}
		r -= s.Weight
	}
	// Floating-point remainder: fall back to the last source.
	last := p.sources[len(p.sources)-1]
	return last.URLs[rand.IntN(len(last.URLs))], true
}

func (p *Pool) lastServed() string {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.last
}

// Size returns the total number of URLs across all sources.
func (p *Pool) Size() int {
	p.mu.RLock()
	defer p.mu.RUnlock()
	n := 0
	for _, s := range p.sources {
		n += len(s.URLs)
	}
	return n
}

// Stats returns a per-source URL count, for the health endpoint.
func (p *Pool) Stats() map[string]int {
	p.mu.RLock()
	defer p.mu.RUnlock()
	out := make(map[string]int, len(p.sources))
	for _, s := range p.sources {
		out[s.Name] = len(s.URLs)
	}
	return out
}
