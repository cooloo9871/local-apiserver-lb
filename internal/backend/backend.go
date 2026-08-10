// Package backend tracks the state of upstream apiservers: health as
// judged by the health checker, in-flight connections so they can be
// drained on failure, and per-backend statistics.
package backend

import (
	"fmt"
	"net"
	"sync"
	"sync/atomic"
)

// Stats holds per-backend counters. All fields are atomic and safe for
// lock-free access; they only ever increase.
type Stats struct {
	ConnsTotal    atomic.Uint64 // connections proxied to this backend
	DialErrors    atomic.Uint64 // failed dial attempts
	CheckFailures atomic.Uint64 // failed health probes
	Drained       atomic.Uint64 // connections force-closed by draining
}

// Backend is one upstream server. The zero value is not usable; use New.
type Backend struct {
	addr string

	mu      sync.Mutex
	healthy bool
	fails   int // consecutive probe failures while healthy
	rises   int // consecutive probe successes while unhealthy
	conns   map[net.Conn]struct{}

	stats Stats
}

// New returns a backend for addr. It starts healthy: the proxy may need
// to serve connections before the first probe round completes, and the
// fall threshold corrects an optimistic start within seconds.
func New(addr string) *Backend {
	return &Backend{
		addr:    addr,
		healthy: true,
		conns:   make(map[net.Conn]struct{}),
	}
}

// Addr returns the backend's host:port.
func (b *Backend) Addr() string { return b.addr }

// Healthy reports the current health state.
func (b *Backend) Healthy() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.healthy
}

// Stats returns the backend's counters.
func (b *Backend) Stats() *Stats { return &b.stats }

// ReportHealth feeds one probe result into the fall/rise state machine
// and reports whether the health state changed. Draining on a transition
// to unhealthy is the caller's responsibility, so that the caller can
// log the transition and the drain result together.
func (b *Backend) ReportHealth(ok bool, fall, rise int) (transitioned bool) {
	if !ok {
		b.stats.CheckFailures.Add(1)
	}

	b.mu.Lock()
	defer b.mu.Unlock()

	if b.healthy {
		if ok {
			b.fails = 0
			return false
		}
		b.fails++
		if b.fails < fall {
			return false
		}
		b.healthy = false
		b.fails, b.rises = 0, 0
		return true
	}

	if !ok {
		b.rises = 0
		return false
	}
	b.rises++
	if b.rises < rise {
		return false
	}
	b.healthy = true
	b.fails, b.rises = 0, 0
	return true
}

// Track wraps c so that it is registered with the backend until closed.
// The returned conn must be used in place of c.
func (b *Backend) Track(c net.Conn) net.Conn {
	b.stats.ConnsTotal.Add(1)
	tc := &trackedConn{Conn: c, backend: b}

	b.mu.Lock()
	b.conns[tc] = struct{}{}
	b.mu.Unlock()
	return tc
}

// ActiveConns returns the number of in-flight connections.
func (b *Backend) ActiveConns() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.conns)
}

// DrainAll force-closes every in-flight connection and returns how many
// were closed. It is called when the backend turns unhealthy or is
// removed from the pool, so that clients holding long-lived watches
// reconnect immediately instead of waiting for TCP keepalive timeouts.
func (b *Backend) DrainAll() int {
	b.mu.Lock()
	conns := make([]net.Conn, 0, len(b.conns))
	for c := range b.conns {
		conns = append(conns, c)
	}
	b.mu.Unlock()

	// Close outside the lock: trackedConn.Close re-acquires it.
	for _, c := range conns {
		c.Close()
	}
	b.stats.Drained.Add(uint64(len(conns)))
	return len(conns)
}

// trackedConn removes itself from the backend's connection set on Close.
type trackedConn struct {
	net.Conn
	backend *Backend
	once    sync.Once
}

func (tc *trackedConn) Close() error {
	tc.once.Do(func() {
		tc.backend.mu.Lock()
		delete(tc.backend.conns, tc)
		tc.backend.mu.Unlock()
	})
	return tc.Conn.Close()
}

// CloseWrite forwards TCP half-close to the underlying connection, which
// the embedding would otherwise hide. The connection stays tracked: it is
// still alive in the read direction.
func (tc *trackedConn) CloseWrite() error {
	if cw, ok := tc.Conn.(interface{ CloseWrite() error }); ok {
		return cw.CloseWrite()
	}
	return fmt.Errorf("underlying connection %T does not support CloseWrite", tc.Conn)
}
