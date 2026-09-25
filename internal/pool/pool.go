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

	"github.com/ygelfand/posterlink/internal/provider"
)

// Source is one provider's contribution to the pool.
type Source struct {
	Name   string
	Weight float64
	Images []provider.Image
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
		if len(s.Images) == 0 || s.Weight <= 0 {
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

// Pick is a chosen image and the source it came from.
type Pick struct {
	provider.Image
	Provider string
}

// Random returns a weighted-random URL across all sources.
func (p *Pool) Random() (string, bool) {
	pick, ok := p.pick(nil, nil)
	return pick.URL, ok
}

// RandomFrom returns a weighted-random URL restricted to the named sources. An
// empty set behaves like Random (all sources). The boolean is false when no
// matching source has any URLs.
func (p *Pool) RandomFrom(allow map[string]struct{}) (string, bool) {
	pick, ok := p.RandomPick(allow, nil)
	return pick.URL, ok
}

// RandomPick returns a weighted-random image and its source, restricted to the
// named sources (empty = all) and never returning a URL in skip.
func (p *Pool) RandomPick(allow, skip map[string]struct{}) (Pick, bool) {
	if len(allow) == 0 {
		allow = nil
	}
	if len(skip) == 0 {
		skip = nil
	}
	return p.pick(allow, skip)
}

// pick selects a weighted-random image from the current snapshot, restricted to
// allow (nil = all sources) and excluding skip (nil = exclude nothing).
func (p *Pool) pick(allow, skip map[string]struct{}) (Pick, bool) {
	s := p.cur.Load()

	total := s.total
	if allow != nil || skip != nil {
		total = 0
		for i := range s.sources {
			src := &s.sources[i]
			if !eligible(src, allow, skip) {
				continue
			}
			total += src.Weight
		}
	}
	if total <= 0 {
		return Pick{}, false
	}

	r := rand.Float64() * total
	var last *Source
	for i := range s.sources {
		src := &s.sources[i]
		if !eligible(src, allow, skip) {
			continue
		}
		last = src
		if r < src.Weight {
			return choose(src, skip)
		}
		r -= src.Weight
	}
	// Floating-point remainder: fall back to the last eligible source.
	if last != nil {
		return choose(last, skip)
	}
	return Pick{}, false
}

// eligible reports whether src is allowed and still holds an unskipped URL.
func eligible(src *Source, allow, skip map[string]struct{}) bool {
	if allow != nil {
		if _, ok := allow[src.Name]; !ok {
			return false
		}
	}
	if skip == nil {
		return true
	}
	for _, img := range src.Images {
		if _, bad := skip[img.URL]; !bad {
			return true
		}
	}
	return false
}

// choose returns a uniform-random image from src that is not in skip.
func choose(src *Source, skip map[string]struct{}) (Pick, bool) {
	if skip == nil {
		return Pick{Image: src.Images[rand.IntN(len(src.Images))], Provider: src.Name}, true
	}
	usable := make([]provider.Image, 0, len(src.Images))
	for _, img := range src.Images {
		if _, bad := skip[img.URL]; !bad {
			usable = append(usable, img)
		}
	}
	if len(usable) == 0 {
		return Pick{}, false
	}
	return Pick{Image: usable[rand.IntN(len(usable))], Provider: src.Name}, true
}

// Size returns the total number of images across all sources.
func (p *Pool) Size() int {
	s := p.cur.Load()
	n := 0
	for _, src := range s.sources {
		n += len(src.Images)
	}
	return n
}

// Stats returns a per-source image count, for the health endpoint.
func (p *Pool) Stats() map[string]int {
	s := p.cur.Load()
	out := make(map[string]int, len(s.sources))
	for _, src := range s.sources {
		out[src.Name] = len(src.Images)
	}
	return out
}
