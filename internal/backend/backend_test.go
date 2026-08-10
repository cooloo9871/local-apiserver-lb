package backend

import (
	"net"
	"sync"
	"testing"
)

func TestNewBackendStartsHealthy(t *testing.T) {
	b := New("10.0.0.1:6443")
	if !b.Healthy() {
		t.Error("new backend is unhealthy; want optimistic healthy start")
	}
	if b.Addr() != "10.0.0.1:6443" {
		t.Errorf("Addr = %q", b.Addr())
	}
}

func TestFallThreshold(t *testing.T) {
	b := New("a:1")

	if transitioned := b.ReportHealth(false, 2, 2); transitioned {
		t.Error("transitioned after 1 failure with fall=2")
	}
	if !b.Healthy() {
		t.Error("unhealthy after 1 failure with fall=2")
	}

	if transitioned := b.ReportHealth(false, 2, 2); !transitioned {
		t.Error("no transition after 2nd consecutive failure with fall=2")
	}
	if b.Healthy() {
		t.Error("still healthy after crossing fall threshold")
	}

	// Further failures must not report additional transitions.
	if transitioned := b.ReportHealth(false, 2, 2); transitioned {
		t.Error("transition reported again while already unhealthy")
	}
}

func TestSuccessResetsFallCounter(t *testing.T) {
	b := New("a:1")
	b.ReportHealth(false, 2, 2)
	b.ReportHealth(true, 2, 2) // resets the streak
	if transitioned := b.ReportHealth(false, 2, 2); transitioned {
		t.Error("transitioned after non-consecutive failures")
	}
	if !b.Healthy() {
		t.Error("unhealthy after non-consecutive failures")
	}
}

func TestRiseThreshold(t *testing.T) {
	b := New("a:1")
	b.ReportHealth(false, 1, 2) // fall=1: immediately unhealthy
	if b.Healthy() {
		t.Fatal("fall=1 did not mark backend unhealthy after one failure")
	}

	if transitioned := b.ReportHealth(true, 1, 2); transitioned {
		t.Error("transitioned after 1 success with rise=2")
	}
	if transitioned := b.ReportHealth(true, 1, 2); !transitioned {
		t.Error("no transition after 2nd consecutive success with rise=2")
	}
	if !b.Healthy() {
		t.Error("still unhealthy after crossing rise threshold")
	}
}

func TestFailureResetsRiseCounter(t *testing.T) {
	b := New("a:1")
	b.ReportHealth(false, 1, 2)
	b.ReportHealth(true, 1, 2)  // 1 of 2
	b.ReportHealth(false, 1, 2) // resets the streak
	if transitioned := b.ReportHealth(true, 1, 2); transitioned {
		t.Error("transitioned after non-consecutive successes")
	}
	if b.Healthy() {
		t.Error("healthy after non-consecutive successes")
	}
}

func TestCheckFailuresCounter(t *testing.T) {
	b := New("a:1")
	b.ReportHealth(false, 5, 2)
	b.ReportHealth(false, 5, 2)
	b.ReportHealth(true, 5, 2)
	if got := b.Stats().CheckFailures.Load(); got != 2 {
		t.Errorf("CheckFailures = %d, want 2", got)
	}
}

func TestTrackAndClose(t *testing.T) {
	b := New("a:1")
	c1, c2 := net.Pipe()
	defer c2.Close()

	tracked := b.Track(c1)
	if got := b.ActiveConns(); got != 1 {
		t.Fatalf("ActiveConns = %d after Track, want 1", got)
	}
	if got := b.Stats().ConnsTotal.Load(); got != 1 {
		t.Errorf("ConnsTotal = %d, want 1", got)
	}

	if err := tracked.Close(); err != nil {
		t.Fatalf("Close returned error: %v", err)
	}
	if got := b.ActiveConns(); got != 0 {
		t.Errorf("ActiveConns = %d after Close, want 0", got)
	}

	// Double close must not panic or corrupt the count.
	tracked.Close()
	if got := b.ActiveConns(); got != 0 {
		t.Errorf("ActiveConns = %d after double Close, want 0", got)
	}
}

func TestDrainAll(t *testing.T) {
	b := New("a:1")
	var remotes []net.Conn
	for i := 0; i < 3; i++ {
		c1, c2 := net.Pipe()
		remotes = append(remotes, c2)
		b.Track(c1)
	}
	defer func() {
		for _, r := range remotes {
			r.Close()
		}
	}()

	if got := b.DrainAll(); got != 3 {
		t.Errorf("DrainAll = %d, want 3", got)
	}
	if got := b.ActiveConns(); got != 0 {
		t.Errorf("ActiveConns = %d after drain, want 0", got)
	}
	if got := b.Stats().Drained.Load(); got != 3 {
		t.Errorf("Drained = %d, want 3", got)
	}

	// Draining an already-drained backend is a no-op.
	if got := b.DrainAll(); got != 0 {
		t.Errorf("second DrainAll = %d, want 0", got)
	}

	// The tracked side of each pipe must actually be closed: a read on
	// the remote side must fail once the peer is closed.
	buf := make([]byte, 1)
	if _, err := remotes[0].Read(buf); err == nil {
		t.Error("remote read succeeded; tracked conn was not really closed")
	}
}

func TestConcurrentTrackDrainReport(t *testing.T) {
	// Exercised under -race: concurrent tracking, draining, health
	// reporting, and stat reads must be free of data races.
	b := New("a:1")
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 50; j++ {
				c1, c2 := net.Pipe()
				tracked := b.Track(c1)
				tracked.Close()
				c2.Close()
			}
		}()
	}
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 50; j++ {
				b.ReportHealth(j%3 == 0, 2, 2)
				b.DrainAll()
				b.Healthy()
				b.ActiveConns()
			}
		}()
	}
	wg.Wait()
}
