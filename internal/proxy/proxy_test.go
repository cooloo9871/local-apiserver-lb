package proxy

import (
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cooloo9871/local-apiserver-lb/internal/backend"
	"github.com/cooloo9871/local-apiserver-lb/internal/health"
)

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// startTLSBackend starts a fake apiserver: a TLS server that answers 200
// on /readyz and identifies itself on every other path.
func startTLSBackend(t *testing.T, id string) (*httptest.Server, string) {
	t.Helper()
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, id)
	}))
	t.Cleanup(srv.Close)
	return srv, strings.TrimPrefix(srv.URL, "https://")
}

// startProxy wires a Server in front of the pool and returns its address.
func startProxy(t *testing.T, pool *backend.Pool) (string, *Server) {
	t.Helper()
	srv := New(pool, Options{
		DialTimeout:     time.Second,
		KeepalivePeriod: time.Second,
		Logger:          discardLogger(),
	})
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go srv.Serve(ln)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		srv.Shutdown(ctx)
	})
	return ln.Addr().String(), srv
}

// clientVia returns an HTTP client whose every connection goes through
// the proxy, one fresh connection per request.
func clientVia(proxyAddr string) *http.Client {
	return &http.Client{
		Timeout: 3 * time.Second,
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
				var d net.Dialer
				return d.DialContext(ctx, network, proxyAddr)
			},
			TLSClientConfig:   &tls.Config{InsecureSkipVerify: true},
			DisableKeepAlives: true,
		},
	}
}

func get(t *testing.T, client *http.Client) (string, error) {
	t.Helper()
	resp, err := client.Get("https://proxied.invalid/")
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	return string(body), err
}

func newPool(t *testing.T, picker string, addrs ...string) *backend.Pool {
	t.Helper()
	pool, err := backend.NewPool(addrs, picker)
	if err != nil {
		t.Fatal(err)
	}
	return pool
}

func findBackend(t *testing.T, pool *backend.Pool, addr string) *backend.Backend {
	t.Helper()
	for _, b := range pool.Backends() {
		if b.Addr() == addr {
			return b
		}
	}
	t.Fatalf("backend %s not found", addr)
	return nil
}

func TestProxyForwardsTraffic(t *testing.T) {
	_, addr := startTLSBackend(t, "backend-0")
	pool := newPool(t, "round-robin", addr)
	proxyAddr, _ := startProxy(t, pool)

	body, err := get(t, clientVia(proxyAddr))
	if err != nil {
		t.Fatalf("request through proxy failed: %v", err)
	}
	if body != "backend-0" {
		t.Errorf("body = %q, want backend-0", body)
	}
}

func TestRoundRobinDistribution(t *testing.T) {
	var addrs []string
	for i := 0; i < 3; i++ {
		_, addr := startTLSBackend(t, fmt.Sprintf("backend-%d", i))
		addrs = append(addrs, addr)
	}
	pool := newPool(t, "round-robin", addrs...)
	proxyAddr, _ := startProxy(t, pool)
	client := clientVia(proxyAddr)

	counts := make(map[string]int)
	for i := 0; i < 30; i++ {
		body, err := get(t, client)
		if err != nil {
			t.Fatalf("request %d failed: %v", i, err)
		}
		counts[body]++
	}
	for id, n := range counts {
		if n != 10 {
			t.Errorf("%s served %d requests, want 10 (counts: %v)", id, n, counts)
		}
	}
}

func TestDialFallthroughOnDeadBackend(t *testing.T) {
	// One address refuses connections but is still considered healthy
	// (dial failures are not health verdicts). Every client request must
	// silently fall through to the live backend.
	deadLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	deadAddr := deadLn.Addr().String()
	deadLn.Close()

	_, liveAddr := startTLSBackend(t, "live")
	pool := newPool(t, "round-robin", deadAddr, liveAddr)
	proxyAddr, _ := startProxy(t, pool)
	client := clientVia(proxyAddr)

	for i := 0; i < 6; i++ {
		body, err := get(t, client)
		if err != nil {
			t.Fatalf("request %d failed despite live backend: %v", i, err)
		}
		if body != "live" {
			t.Errorf("body = %q, want live", body)
		}
	}
	if got := findBackend(t, pool, deadAddr).Stats().DialErrors.Load(); got == 0 {
		t.Error("DialErrors = 0 for dead backend, want > 0")
	}
}

func TestAllBackendsDead(t *testing.T) {
	deadLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	deadAddr := deadLn.Addr().String()
	deadLn.Close()

	pool := newPool(t, "round-robin", deadAddr)
	proxyAddr, _ := startProxy(t, pool)

	// The client connection must be closed, surfacing as a request
	// error; the proxy itself must survive (checked by a second try).
	for i := 0; i < 2; i++ {
		if _, err := get(t, clientVia(proxyAddr)); err == nil {
			t.Error("request succeeded with all backends dead")
		}
	}
}

func TestUnhealthyBackendReceivesNoTraffic(t *testing.T) {
	_, addrA := startTLSBackend(t, "a")
	_, addrB := startTLSBackend(t, "b")
	pool := newPool(t, "round-robin", addrA, addrB)
	findBackend(t, pool, addrB).ReportHealth(false, 1, 1)

	proxyAddr, _ := startProxy(t, pool)
	client := clientVia(proxyAddr)
	for i := 0; i < 10; i++ {
		body, err := get(t, client)
		if err != nil {
			t.Fatalf("request failed: %v", err)
		}
		if body != "a" {
			t.Errorf("unhealthy backend served a request (body %q)", body)
		}
	}
}

func TestBestEffortWhenAllUnhealthy(t *testing.T) {
	// Backends are marked unhealthy but actually alive: best-effort mode
	// must still serve traffic instead of failing closed.
	_, addr := startTLSBackend(t, "alive-but-flagged")
	pool := newPool(t, "round-robin", addr)
	findBackend(t, pool, addr).ReportHealth(false, 1, 1)

	proxyAddr, _ := startProxy(t, pool)
	body, err := get(t, clientVia(proxyAddr))
	if err != nil {
		t.Fatalf("best-effort request failed: %v", err)
	}
	if body != "alive-but-flagged" {
		t.Errorf("body = %q", body)
	}
}

func TestHalfClose(t *testing.T) {
	// The backend reads until EOF, then replies. This only works if the
	// proxy translates the client's write-side shutdown into CloseWrite
	// toward the backend while keeping the backend->client path open.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				data, _ := io.ReadAll(c) // returns only on client EOF
				fmt.Fprintf(c, "got:%s", data)
			}(c)
		}
	}()

	pool := newPool(t, "round-robin", ln.Addr().String())
	proxyAddr, _ := startProxy(t, pool)

	conn, err := net.Dial("tcp", proxyAddr)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	if _, err := conn.Write([]byte("hello")); err != nil {
		t.Fatal(err)
	}
	if err := conn.(*net.TCPConn).CloseWrite(); err != nil {
		t.Fatal(err)
	}
	reply, err := io.ReadAll(conn)
	if err != nil {
		t.Fatalf("reading reply after half-close: %v", err)
	}
	if string(reply) != "got:hello" {
		t.Errorf("reply = %q, want got:hello", reply)
	}
}

func TestDrainOnHealthTransitionKillsConnection(t *testing.T) {
	// Acceptance criterion: when a backend goes down, its in-flight
	// connections are force-closed within health-interval x fall, using
	// the real checker and readyz prober end to end.
	//
	// The failure is simulated by flipping /readyz to 500 while keeping
	// established connections alive: the case where the apiserver is
	// dead or wedged but TCP never got a FIN, which is exactly when
	// active draining matters.
	var sickA, sickB atomic.Bool
	sickHandler := func(sick *atomic.Bool, id string) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/readyz" && sick.Load() {
				w.WriteHeader(http.StatusInternalServerError)
				return
			}
			fmt.Fprint(w, id)
		})
	}
	srvA := httptest.NewTLSServer(sickHandler(&sickA, "a"))
	t.Cleanup(srvA.Close)
	srvB := httptest.NewTLSServer(sickHandler(&sickB, "b"))
	t.Cleanup(srvB.Close)
	addrA := strings.TrimPrefix(srvA.URL, "https://")
	addrB := strings.TrimPrefix(srvB.URL, "https://")
	pool := newPool(t, "round-robin", addrA, addrB)
	proxyAddr, _ := startProxy(t, pool)

	prober, err := health.NewProber("readyz", "")
	if err != nil {
		t.Fatal(err)
	}
	checker := health.NewChecker(pool, prober, health.Options{
		Interval: 50 * time.Millisecond,
		Timeout:  200 * time.Millisecond,
		Fall:     2,
		Rise:     2,
		Logger:   discardLogger(),
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go checker.Run(ctx)

	// Open a long-lived connection through the proxy (TLS handshake
	// against whichever backend it lands on, then block on reads, like a
	// kubelet watch).
	raw, err := net.Dial("tcp", proxyAddr)
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	tlsConn := tls.Client(raw, &tls.Config{InsecureSkipVerify: true})
	if err := tlsConn.Handshake(); err != nil {
		t.Fatal(err)
	}

	// Make whichever backend holds the connection start failing /readyz.
	bA, bB := findBackend(t, pool, addrA), findBackend(t, pool, addrB)
	var victim *backend.Backend
	switch {
	case bA.ActiveConns() == 1:
		victim = bA
		sickA.Store(true)
	case bB.ActiveConns() == 1:
		victim = bB
		sickB.Store(true)
	default:
		t.Fatal("no backend holds the connection")
	}

	readErr := make(chan error, 1)
	go func() {
		buf := make([]byte, 1)
		_, err := tlsConn.Read(buf)
		readErr <- err
	}()

	select {
	case err := <-readErr:
		if err == nil {
			t.Fatal("read returned nil error after backend death")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("connection not drained within 3s of backend death")
	}

	if got := victim.Stats().Drained.Load(); got == 0 {
		t.Error("Drained counter = 0, want > 0")
	}
}

func TestGracefulShutdownWaitsForActiveConns(t *testing.T) {
	_, addr := startTLSBackend(t, "x")
	pool := newPool(t, "round-robin", addr)

	srv := New(pool, Options{DialTimeout: time.Second, KeepalivePeriod: time.Second, Logger: discardLogger()})
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go srv.Serve(ln)

	// Hold a connection open through the proxy.
	conn, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	tlsConn := tls.Client(conn, &tls.Config{InsecureSkipVerify: true})
	if err := tlsConn.Handshake(); err != nil {
		t.Fatal(err)
	}

	// Shutdown with an expired grace: the held connection must be
	// force-closed and Shutdown must return promptly.
	start := time.Now()
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	if err := srv.Shutdown(ctx); err != nil {
		t.Fatalf("Shutdown returned error: %v", err)
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Errorf("Shutdown took %v, want prompt force-close after grace", elapsed)
	}

	// The held connection must now be dead.
	tlsConn.SetReadDeadline(time.Now().Add(time.Second))
	buf := make([]byte, 1)
	if _, err := tlsConn.Read(buf); err == nil {
		t.Error("held connection still alive after Shutdown")
	}

	// And the listener must refuse new connections.
	if _, err := net.DialTimeout("tcp", ln.Addr().String(), 500*time.Millisecond); err == nil {
		t.Error("listener still accepting after Shutdown")
	}
}

func TestShutdownReturnsWhenIdle(t *testing.T) {
	_, addr := startTLSBackend(t, "x")
	pool := newPool(t, "round-robin", addr)

	srv := New(pool, Options{DialTimeout: time.Second, KeepalivePeriod: time.Second, Logger: discardLogger()})
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	serveDone := make(chan struct{})
	go func() {
		srv.Serve(ln)
		close(serveDone)
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := srv.Shutdown(ctx); err != nil {
		t.Fatalf("Shutdown returned error: %v", err)
	}
	select {
	case <-serveDone:
	case <-time.After(time.Second):
		t.Error("Serve did not return after Shutdown")
	}
}

func TestConcurrentConnections(t *testing.T) {
	// Exercised under -race: many concurrent client connections.
	var addrs []string
	for i := 0; i < 2; i++ {
		_, addr := startTLSBackend(t, fmt.Sprintf("b%d", i))
		addrs = append(addrs, addr)
	}
	pool := newPool(t, "least-conn", addrs...)
	proxyAddr, _ := startProxy(t, pool)

	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			client := clientVia(proxyAddr)
			for j := 0; j < 5; j++ {
				if _, err := get(t, client); err != nil {
					t.Errorf("concurrent request failed: %v", err)
					return
				}
			}
		}()
	}
	wg.Wait()
}
