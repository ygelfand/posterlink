// Package pool holds the in-memory set of image URLs and serves a random one
// per request. Selection is weighted across providers, then uniform within the
// chosen provider, so blending (e.g. "80% posters, 20% photos") is a matter of
// relative weights rather than pool sizes.
//
// The pool is only ever replaced wholesale (never mutated in place), so reads
// are lock-free: they load an immutable snapshot via an atomic pointer while a
// refresh publishes a new one.
package pool

import (
	"math/rand/v2"
	"sync/atomic"
)

// Source is one provider's contribution to the pool.
type Source struct {
	Name   string
	Weight float64
	URLs   []string
}

// snapshot is an immutable view of the pool; it is replaced, never edited.
type snapshot struct {
	sources []Source
	total   float64
}

// Pool is a lock-free, weighted collection of image URLs.
type Pool struct {
	cur atomic.Pointer[snapshot]
}

// New returns an empty pool.
func New() *Pool {
	p := &Pool{}
	p.cur.Store(&snapshot{})
	return p
}

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
	p.cur.Store(&snapshot{sources: kept, total: total})
}

// Random returns a weighted-random URL across all sources.
func (p *Pool) Random() (string, bool) { return p.pick(nil) }

// RandomFrom returns a weighted-random URL restricted to the named sources. An
// empty set behaves like Random (all sources). The boolean is false when no
// matching source has any URLs.
func (p *Pool) RandomFrom(allow map[string]struct{}) (string, bool) {
	if len(allow) == 0 {
		allow = nil
	}
	return p.pick(allow)
}

// pick selects a weighted-random URL from the current snapshot, restricted to
// allow (nil = all sources).
func (p *Pool) pick(allow map[string]struct{}) (string, bool) {
	s := p.cur.Load()

	total := s.total
	if allow != nil {
		total = 0
		for _, src := range s.sources {
			if _, ok := allow[src.Name]; ok {
				total += src.Weight
			}
		}
	}
	if total <= 0 {
		return "", false
	}

	r := rand.Float64() * total
	var last *Source
	for i := range s.sources {
		src := &s.sources[i]
		if allow != nil {
			if _, ok := allow[src.Name]; !ok {
				continue
			}
		}
		last = src
		if r < src.Weight {
			return src.URLs[rand.IntN(len(src.URLs))], true
		}
		r -= src.Weight
	}
	// Floating-point remainder: fall back to the last eligible source.
	if last != nil {
		return last.URLs[rand.IntN(len(last.URLs))], true
	}
	return "", false
}

// Size returns the total number of URLs across all sources.
func (p *Pool) Size() int {
	s := p.cur.Load()
	n := 0
	for _, src := range s.sources {
		n += len(src.URLs)
	}
	return n
}

// Stats returns a per-source URL count, for the health endpoint.
func (p *Pool) Stats() map[string]int {
	s := p.cur.Load()
	out := make(map[string]int, len(s.sources))
	for _, src := range s.sources {
		out[src.Name] = len(src.URLs)
	}
	return out
}
