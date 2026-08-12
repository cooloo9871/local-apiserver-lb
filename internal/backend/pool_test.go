package backend

import (
	"net"
	"testing"
)

func newTestPool(t *testing.T, picker string, addrs ...string) *Pool {
	t.Helper()
	p, err := NewPool(addrs, picker)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestNewPoolUnknownPicker(t *testing.T) {
	if _, err := NewPool([]string{"a:1"}, "random"); err == nil {
		t.Error("unknown picker accepted, want error")
	}
}

func TestRoundRobinFairness(t *testing.T) {
	p := newTestPool(t, "round-robin", "a:1", "b:1", "c:1")
	counts := make(map[string]int)
	for i := 0; i < 300; i++ {
		order, degraded := p.Candidates()
		if degraded {
			t.Fatal("degraded with all backends healthy")
		}
		counts[order[0].Addr()]++
	}
	for addr, n := range counts {
		if n != 100 {
			t.Errorf("backend %s picked %d times, want 100", addr, n)
		}
	}
}

func TestRoundRobinFailoverOrder(t *testing.T) {
	// The full candidate list must contain every healthy backend exactly
	// once, so a dial failure can fall through to the next one.
	p := newTestPool(t, "round-robin", "a:1", "b:1", "c:1")
	order, _ := p.Candidates()
	if len(order) != 3 {
		t.Fatalf("Candidates returned %d backends, want 3", len(order))
	}
	seen := map[string]bool{}
	for _, b := range order {
		if seen[b.Addr()] {
			t.Errorf("backend %s appears twice in candidate order", b.Addr())
		}
		seen[b.Addr()] = true
	}
}

func TestRoundRobinSkipsUnhealthy(t *testing.T) {
	p := newTestPool(t, "round-robin", "a:1", "b:1", "c:1")
	markUnhealthy(t, p, "b:1")

	for i := 0; i < 20; i++ {
		order, degraded := p.Candidates()
		if degraded {
			t.Fatal("degraded with 2 of 3 backends healthy")
		}
		for _, b := range order {
			if b.Addr() == "b:1" {
				t.Fatal("unhealthy backend appeared in candidates")
			}
		}
		if len(order) != 2 {
			t.Fatalf("Candidates returned %d backends, want 2", len(order))
		}
	}
}

func TestAllUnhealthyFallback(t *testing.T) {
	p := newTestPool(t, "round-robin", "a:1", "b:1")
	markUnhealthy(t, p, "a:1")
	markUnhealthy(t, p, "b:1")

	order, degraded := p.Candidates()
	if !degraded {
		t.Error("degraded = false with no healthy backend")
	}
	if len(order) != 2 {
		t.Fatalf("best-effort candidates = %d backends, want all 2", len(order))
	}
}

func TestLeastConnPrefersIdleBackend(t *testing.T) {
	p := newTestPool(t, "least-conn", "a:1", "b:1")
	busy := findBackend(t, p, "a:1")

	// Give a:1 two in-flight connections.
	for i := 0; i < 2; i++ {
		c1, c2 := net.Pipe()
		t.Cleanup(func() { c1.Close(); c2.Close() })
		busy.Track(c1)
	}

	for i := 0; i < 10; i++ {
		order, _ := p.Candidates()
		if order[0].Addr() != "b:1" {
			t.Fatalf("least-conn picked %s first, want idle b:1", order[0].Addr())
		}
	}
}

func TestLeastConnTieRotates(t *testing.T) {
	// With equal connection counts, the first pick must not always be
	// the same backend, or the first backend would absorb every burst.
	p := newTestPool(t, "least-conn", "a:1", "b:1", "c:1")
	first := make(map[string]bool)
	for i := 0; i < 30; i++ {
		order, _ := p.Candidates()
		first[order[0].Addr()] = true
	}
	if len(first) < 2 {
		t.Errorf("least-conn tie always picks the same backend: %v", first)
	}
}

func TestSetAddrsPreservesExistingBackends(t *testing.T) {
	p := newTestPool(t, "round-robin", "a:1", "b:1")
	before := findBackend(t, p, "a:1")

	added, removed := p.SetAddrs([]string{"a:1", "c:1"})

	if len(added) != 1 || added[0].Addr() != "c:1" {
		t.Errorf("added = %v, want [c:1]", addrsOf(added))
	}
	if len(removed) != 1 || removed[0].Addr() != "b:1" {
		t.Errorf("removed = %v, want [b:1]", addrsOf(removed))
	}

	after := findBackend(t, p, "a:1")
	if before != after {
		t.Error("SetAddrs replaced a retained backend; existing state must be preserved")
	}
	if len(p.Backends()) != 2 {
		t.Errorf("pool has %d backends, want 2", len(p.Backends()))
	}
}

func TestSetAddrsNoChange(t *testing.T) {
	p := newTestPool(t, "round-robin", "a:1", "b:1")
	added, removed := p.SetAddrs([]string{"a:1", "b:1"})
	if len(added) != 0 || len(removed) != 0 {
		t.Errorf("added = %v, removed = %v, want none", addrsOf(added), addrsOf(removed))
	}
}

func TestHealthyCount(t *testing.T) {
	p := newTestPool(t, "round-robin", "a:1", "b:1", "c:1")
	if got := p.HealthyCount(); got != 3 {
		t.Errorf("HealthyCount = %d, want 3", got)
	}
	markUnhealthy(t, p, "a:1")
	if got := p.HealthyCount(); got != 2 {
		t.Errorf("HealthyCount = %d, want 2", got)
	}
}

func TestConcurrentCandidatesAndSetAddrs(t *testing.T) {
	// Exercised under -race: discovery reloads happen while connections
	// are being routed; the pool must tolerate that concurrency.
	p := newTestPool(t, "least-conn", "a:1", "b:1")
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 200; i++ {
			p.SetAddrs([]string{"a:1", "b:1", "c:1"})
			p.SetAddrs([]string{"a:1", "b:1"})
		}
	}()
	for i := 0; i < 500; i++ {
		order, _ := p.Candidates()
		if len(order) == 0 {
			t.Fatal("Candidates returned empty order")
		}
		p.HealthyCount()
	}
	<-done
}

func markUnhealthy(t *testing.T, p *Pool, addr string) {
	t.Helper()
	findBackend(t, p, addr).ReportHealth(false, 1, 1)
}

func findBackend(t *testing.T, p *Pool, addr string) *Backend {
	t.Helper()
	for _, b := range p.Backends() {
		if b.Addr() == addr {
			return b
		}
	}
	t.Fatalf("backend %s not found in pool", addr)
	return nil
}

func addrsOf(bs []*Backend) []string {
	var out []string
	for _, b := range bs {
		out = append(out, b.Addr())
	}
	return out
}
