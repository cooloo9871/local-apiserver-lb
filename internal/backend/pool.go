package backend

import (
	"fmt"
	"sort"
	"sync"
	"sync/atomic"
)

// Picker orders healthy backends by preference for one connection attempt.
// The returned slice must contain every input backend exactly once, so
// that the proxy can fall through to the next candidate on dial failure.
type Picker interface {
	Name() string
	Order(healthy []*Backend) []*Backend
}

// Pool holds the current backend set and the balancing strategy.
type Pool struct {
	mu       sync.RWMutex
	backends []*Backend
	picker   Picker
}

// NewPool builds a pool from addrs using the named strategy
// ("round-robin" or "least-conn").
func NewPool(addrs []string, pickerName string) (*Pool, error) {
	picker, err := newPicker(pickerName)
	if err != nil {
		return nil, err
	}
	p := &Pool{picker: picker}
	for _, a := range addrs {
		p.backends = append(p.backends, New(a))
	}
	return p, nil
}

// Backends returns a snapshot of the backend list.
func (p *Pool) Backends() []*Backend {
	p.mu.RLock()
	defer p.mu.RUnlock()
	out := make([]*Backend, len(p.backends))
	copy(out, p.backends)
	return out
}

// HealthyCount returns the number of healthy backends.
func (p *Pool) HealthyCount() int {
	n := 0
	for _, b := range p.Backends() {
		if b.Healthy() {
			n++
		}
	}
	return n
}

// Candidates returns the backends to try, in order. When no backend is
// healthy it falls back to best-effort mode: every backend in list order,
// with degraded=true so the caller can log the condition.
func (p *Pool) Candidates() (order []*Backend, degraded bool) {
	all := p.Backends()
	healthy := make([]*Backend, 0, len(all))
	for _, b := range all {
		if b.Healthy() {
			healthy = append(healthy, b)
		}
	}
	if len(healthy) == 0 {
		return all, true
	}

	p.mu.RLock()
	picker := p.picker
	p.mu.RUnlock()
	return picker.Order(healthy), false
}

// SetAddrs reconciles the backend set against addrs, preserving existing
// Backend instances (and their state) for retained addresses. It returns
// the added and removed backends; draining removed ones is the caller's
// responsibility. This is the entry point used by dynamic discovery.
func (p *Pool) SetAddrs(addrs []string) (added, removed []*Backend) {
	p.mu.Lock()
	defer p.mu.Unlock()

	current := make(map[string]*Backend, len(p.backends))
	for _, b := range p.backends {
		current[b.Addr()] = b
	}

	next := make([]*Backend, 0, len(addrs))
	keep := make(map[string]bool, len(addrs))
	for _, a := range addrs {
		if b, ok := current[a]; ok {
			next = append(next, b)
		} else {
			b := New(a)
			next = append(next, b)
			added = append(added, b)
		}
		keep[a] = true
	}
	for _, b := range p.backends {
		if !keep[b.Addr()] {
			removed = append(removed, b)
		}
	}

	p.backends = next
	return added, removed
}

func newPicker(name string) (Picker, error) {
	switch name {
	case "round-robin":
		return &roundRobin{}, nil
	case "least-conn":
		return &leastConn{}, nil
	default:
		return nil, fmt.Errorf("unknown balance strategy %q (want \"round-robin\" or \"least-conn\")", name)
	}
}

// roundRobin rotates through the healthy backends with an atomic counter.
type roundRobin struct {
	next atomic.Uint64
}

func (r *roundRobin) Name() string { return "round-robin" }

func (r *roundRobin) Order(healthy []*Backend) []*Backend {
	n := len(healthy)
	start := int((r.next.Add(1) - 1) % uint64(n))
	out := make([]*Backend, 0, n)
	for i := 0; i < n; i++ {
		out = append(out, healthy[(start+i)%n])
	}
	return out
}

// leastConn orders backends by in-flight connection count. Ties are
// broken by round-robin rotation so that equal backends share bursts
// instead of the first one absorbing everything.
type leastConn struct {
	rr roundRobin
}

func (l *leastConn) Name() string { return "least-conn" }

func (l *leastConn) Order(healthy []*Backend) []*Backend {
	out := l.rr.Order(healthy) // rotated copy, safe to sort in place
	sort.SliceStable(out, func(i, j int) bool {
		return out[i].ActiveConns() < out[j].ActiveConns()
	})
	return out
}
