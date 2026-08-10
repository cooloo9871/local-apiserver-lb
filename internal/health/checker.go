package health

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/cooloo9871/local-apiserver-lb/internal/backend"
)

// Options configures a Checker.
type Options struct {
	Interval time.Duration // time between probes of one backend
	Timeout  time.Duration // per-probe timeout
	Fall     int           // consecutive failures before unhealthy
	Rise     int           // consecutive successes before healthy
	Logger   *slog.Logger
}

// Checker drives health probes for every backend in a pool. Each backend
// gets its own monitor goroutine with its own ticker, so probes are
// parallel by construction and a slow backend never delays the others.
// The monitor set is reconciled against the pool every interval, which
// picks up SIGHUP-driven backend changes.
type Checker struct {
	pool   *backend.Pool
	prober Prober
	opts   Options

	mu       sync.Mutex
	monitors map[*backend.Backend]context.CancelFunc

	degraded bool // true while no backend is healthy (under mu)
}

// NewChecker builds a Checker. Run must be called to start probing.
func NewChecker(pool *backend.Pool, prober Prober, opts Options) *Checker {
	return &Checker{
		pool:     pool,
		prober:   prober,
		opts:     opts,
		monitors: make(map[*backend.Backend]context.CancelFunc),
	}
}

// Run reconciles and supervises the per-backend monitors until ctx is
// canceled.
func (c *Checker) Run(ctx context.Context) {
	c.reconcile(ctx)
	ticker := time.NewTicker(c.opts.Interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			c.mu.Lock()
			for _, cancel := range c.monitors {
				cancel()
			}
			clear(c.monitors)
			c.mu.Unlock()
			return
		case <-ticker.C:
			c.reconcile(ctx)
		}
	}
}

// reconcile starts monitors for new backends and stops monitors whose
// backend left the pool.
func (c *Checker) reconcile(ctx context.Context) {
	current := make(map[*backend.Backend]bool)
	for _, b := range c.pool.Backends() {
		current[b] = true
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	for b, cancel := range c.monitors {
		if !current[b] {
			cancel()
			delete(c.monitors, b)
		}
	}
	for b := range current {
		if _, ok := c.monitors[b]; !ok {
			mctx, cancel := context.WithCancel(ctx)
			c.monitors[b] = cancel
			go c.monitor(mctx, b)
		}
	}
}

// monitor probes one backend until its context is canceled. The first
// probe fires immediately so a dead backend is detected at startup, not
// one interval later.
func (c *Checker) monitor(ctx context.Context, b *backend.Backend) {
	ticker := time.NewTicker(c.opts.Interval)
	defer ticker.Stop()

	for {
		c.probeOnce(ctx, b)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// probeOnce runs a single probe and feeds the result into the backend
// state machine, handling transition logging and connection draining.
func (c *Checker) probeOnce(ctx context.Context, b *backend.Backend) {
	pctx, cancel := context.WithTimeout(ctx, c.opts.Timeout)
	err := c.prober.Probe(pctx, b.Addr())
	cancel()

	if ctx.Err() != nil {
		// Shutting down; a probe aborted by cancellation is not a
		// health signal.
		return
	}

	if err != nil {
		c.opts.Logger.Debug("health probe failed", "backend", b.Addr(), "error", err)
	}

	if transitioned := b.ReportHealth(err == nil, c.opts.Fall, c.opts.Rise); !transitioned {
		return
	}

	if b.Healthy() {
		c.opts.Logger.Info("backend transitioned to healthy", "backend", b.Addr())
	} else {
		c.opts.Logger.Warn("backend transitioned to unhealthy", "backend", b.Addr(), "error", err)
		if drained := b.DrainAll(); drained > 0 {
			c.opts.Logger.Warn("drained active connections from unhealthy backend",
				"backend", b.Addr(), "connections", drained)
		}
	}
	c.updateDegraded()
}

// updateDegraded logs entry into and exit from best-effort mode, exactly
// once per transition.
func (c *Checker) updateDegraded() {
	degraded := c.pool.HealthyCount() == 0

	c.mu.Lock()
	changed := degraded != c.degraded
	c.degraded = degraded
	c.mu.Unlock()

	if !changed {
		return
	}
	if degraded {
		c.opts.Logger.Warn("no healthy backends remain; entering best-effort mode: " +
			"every connection will try all backends in order")
	} else {
		c.opts.Logger.Info("at least one backend is healthy again; leaving best-effort mode")
	}
}
