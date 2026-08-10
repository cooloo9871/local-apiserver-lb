package health

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cooloo9871/local-apiserver-lb/internal/backend"
)

// scriptedProber fails or succeeds per address, tracks call counts, and
// records peak probe concurrency.
type scriptedProber struct {
	mu      sync.Mutex
	failing map[string]bool
	calls   map[string]int
	delay   time.Duration

	inFlight, peak int
}

func newScriptedProber() *scriptedProber {
	return &scriptedProber{
		failing: make(map[string]bool),
		calls:   make(map[string]int),
	}
}

func (p *scriptedProber) setFailing(addr string, failing bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.failing[addr] = failing
}

func (p *scriptedProber) callCount(addr string) int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.calls[addr]
}

func (p *scriptedProber) peakConcurrency() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.peak
}

func (p *scriptedProber) Probe(ctx context.Context, addr string) error {
	p.mu.Lock()
	p.calls[addr]++
	p.inFlight++
	if p.inFlight > p.peak {
		p.peak = p.inFlight
	}
	failing := p.failing[addr]
	delay := p.delay
	p.mu.Unlock()

	if delay > 0 {
		select {
		case <-time.After(delay):
		case <-ctx.Done():
		}
	}

	p.mu.Lock()
	p.inFlight--
	p.mu.Unlock()

	if failing {
		return fmt.Errorf("scripted failure for %s", addr)
	}
	return nil
}

type checkerHarness struct {
	pool    *backend.Pool
	prober  *scriptedProber
	logBuf  *bytes.Buffer
	logMu   sync.Mutex
	checker *Checker
	cancel  context.CancelFunc
}

// syncBuffer serializes writes so slog output can be read while the
// checker is still running.
type syncBuffer struct {
	mu  *sync.Mutex
	buf *bytes.Buffer
}

func (b syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func startChecker(t *testing.T, fall, rise int, addrs ...string) *checkerHarness {
	t.Helper()
	pool, err := backend.NewPool(addrs, "round-robin")
	if err != nil {
		t.Fatal(err)
	}
	h := &checkerHarness{
		pool:   pool,
		prober: newScriptedProber(),
		logBuf: &bytes.Buffer{},
	}
	logger := slog.New(slog.NewTextHandler(syncBuffer{mu: &h.logMu, buf: h.logBuf}, nil))
	h.checker = NewChecker(pool, h.prober, Options{
		Interval: 10 * time.Millisecond,
		Timeout:  50 * time.Millisecond,
		Fall:     fall,
		Rise:     rise,
		Logger:   logger,
	})
	ctx, cancel := context.WithCancel(context.Background())
	h.cancel = cancel
	go h.checker.Run(ctx)
	t.Cleanup(cancel)
	return h
}

func (h *checkerHarness) logs() string {
	h.logMu.Lock()
	defer h.logMu.Unlock()
	return h.logBuf.String()
}

func (h *checkerHarness) backend(t *testing.T, addr string) *backend.Backend {
	t.Helper()
	for _, b := range h.pool.Backends() {
		if b.Addr() == addr {
			return b
		}
	}
	t.Fatalf("backend %s not found", addr)
	return nil
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func TestCheckerMarksBackendUnhealthyAndDrains(t *testing.T) {
	h := startChecker(t, 2, 2, "a:1", "b:1")
	b := h.backend(t, "a:1")

	// Give the failing backend an in-flight connection to drain.
	c1, c2 := net.Pipe()
	defer c2.Close()
	b.Track(c1)

	h.prober.setFailing("a:1", true)

	waitFor(t, "a:1 to become unhealthy", func() bool { return !b.Healthy() })
	waitFor(t, "connections to be drained", func() bool { return b.ActiveConns() == 0 })

	if got := b.Stats().Drained.Load(); got != 1 {
		t.Errorf("Drained = %d, want 1", got)
	}
	if other := h.backend(t, "b:1"); !other.Healthy() {
		t.Error("healthy backend b:1 was affected by a:1's failure")
	}

	logs := h.logs()
	if !strings.Contains(logs, "a:1") || !strings.Contains(logs, "unhealthy") {
		t.Errorf("state transition not logged; logs:\n%s", logs)
	}
}

func TestCheckerRecovery(t *testing.T) {
	h := startChecker(t, 1, 2, "a:1")
	b := h.backend(t, "a:1")

	h.prober.setFailing("a:1", true)
	waitFor(t, "a:1 to become unhealthy", func() bool { return !b.Healthy() })

	h.prober.setFailing("a:1", false)
	waitFor(t, "a:1 to recover", func() bool { return b.Healthy() })

	if logs := h.logs(); !strings.Contains(logs, "healthy") {
		t.Errorf("recovery not logged; logs:\n%s", logs)
	}
}

func TestCheckerProbesInParallel(t *testing.T) {
	pool, err := backend.NewPool([]string{"a:1", "b:1", "c:1"}, "round-robin")
	if err != nil {
		t.Fatal(err)
	}
	prober := newScriptedProber()
	prober.delay = 30 * time.Millisecond

	checker := NewChecker(pool, prober, Options{
		Interval: 10 * time.Millisecond,
		Timeout:  100 * time.Millisecond,
		Fall:     2,
		Rise:     2,
		Logger:   slog.New(slog.NewTextHandler(bytes.NewBuffer(nil), nil)),
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go checker.Run(ctx)

	waitFor(t, "parallel probes", func() bool { return prober.peakConcurrency() >= 2 })
}

func TestCheckerAllUnhealthyLogged(t *testing.T) {
	h := startChecker(t, 1, 1, "a:1", "b:1")
	h.prober.setFailing("a:1", true)
	h.prober.setFailing("b:1", true)

	waitFor(t, "all backends unhealthy", func() bool { return h.pool.HealthyCount() == 0 })
	waitFor(t, "best-effort warning in logs", func() bool {
		return strings.Contains(h.logs(), "best-effort")
	})

	h.prober.setFailing("a:1", false)
	waitFor(t, "recovery from best-effort mode", func() bool { return h.pool.HealthyCount() == 1 })
}

func TestCheckerReconcilesPoolChanges(t *testing.T) {
	h := startChecker(t, 2, 2, "a:1")

	waitFor(t, "a:1 to be probed", func() bool { return h.prober.callCount("a:1") > 0 })
	if h.prober.callCount("c:1") != 0 {
		t.Fatal("c:1 probed before being added")
	}

	// Add c:1, drop a:1.
	h.pool.SetAddrs([]string{"c:1"})

	waitFor(t, "c:1 to be probed", func() bool { return h.prober.callCount("c:1") > 0 })

	// a:1's monitor must stop: its call count stops increasing.
	waitFor(t, "a:1 probing to stop", func() bool {
		before := h.prober.callCount("a:1")
		time.Sleep(50 * time.Millisecond)
		return h.prober.callCount("a:1") == before
	})
}

func TestCheckerStopsOnContextCancel(t *testing.T) {
	h := startChecker(t, 2, 2, "a:1")
	waitFor(t, "first probe", func() bool { return h.prober.callCount("a:1") > 0 })

	h.cancel()
	// After cancellation settles, probing must stop entirely.
	waitFor(t, "probing to stop after cancel", func() bool {
		before := h.prober.callCount("a:1")
		time.Sleep(50 * time.Millisecond)
		return h.prober.callCount("a:1") == before
	})
}
