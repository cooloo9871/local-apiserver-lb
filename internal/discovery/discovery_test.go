package discovery

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cooloo9871/local-apiserver-lb/internal/backend"
)

// fakeAPIServer is a TLS server acting as an apiserver that serves the
// default/kubernetes Endpoints object with a configurable address list.
type fakeAPIServer struct {
	srv  *httptest.Server
	addr string

	mu        sync.Mutex
	endpoints []string // host:port entries to report
	authSeen  []string // Authorization headers received
}

func newFakeAPIServer(t *testing.T) *fakeAPIServer {
	t.Helper()
	f := &fakeAPIServer{}
	f.srv = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.authSeen = append(f.authSeen, r.Header.Get("Authorization"))
		eps := make([]string, len(f.endpoints))
		copy(eps, f.endpoints)
		f.mu.Unlock()

		if r.URL.Path != "/api/v1/namespaces/default/endpoints/kubernetes" {
			http.NotFound(w, r)
			return
		}

		type address struct {
			IP string `json:"ip"`
		}
		type port struct {
			Name string `json:"name"`
			Port int    `json:"port"`
		}
		type subset struct {
			Addresses []address `json:"addresses"`
			Ports     []port    `json:"ports"`
		}
		// One subset per endpoint so entries with different ports (the
		// httptest server's random port vs fixed 6443 fakes) coexist.
		var subsets []subset
		for _, ep := range eps {
			host, p, err := net.SplitHostPort(ep)
			if err != nil {
				t.Errorf("bad test endpoint %q", ep)
				continue
			}
			portNum := 0
			fmt.Sscanf(p, "%d", &portNum)
			subsets = append(subsets, subset{
				Addresses: []address{{IP: host}},
				Ports:     []port{{Name: "https", Port: portNum}},
			})
		}
		json.NewEncoder(w).Encode(map[string]any{
			"kind":    "Endpoints",
			"subsets": subsets,
		})
	}))
	t.Cleanup(f.srv.Close)
	f.addr = strings.TrimPrefix(f.srv.URL, "https://")
	return f
}

func (f *fakeAPIServer) setEndpoints(eps ...string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.endpoints = eps
}

func (f *fakeAPIServer) lastAuth() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.authSeen) == 0 {
		return ""
	}
	return f.authSeen[len(f.authSeen)-1]
}

// writeKubeconfig writes a token kubeconfig trusting the fake server.
func writeKubeconfig(t *testing.T, f *fakeAPIServer) string {
	t.Helper()
	caPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: f.srv.Certificate().Raw})
	content := `
apiVersion: v1
kind: Config
clusters:
- cluster:
    certificate-authority-data: ` + base64.StdEncoding.EncodeToString(caPEM) + `
    server: https://ignored.invalid:6443
  name: c
contexts:
- context: {cluster: c, user: u}
  name: ctx
current-context: ctx
users:
- name: u
  user: {token: test-token}
`
	path := filepath.Join(t.TempDir(), "kubeconfig")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func startPoller(t *testing.T, pool *backend.Pool, kubeconfigPath string, validate func([]string) error) {
	t.Helper()
	if validate == nil {
		validate = func([]string) error { return nil }
	}
	p := New(pool, Options{
		KubeconfigPath: kubeconfigPath,
		Interval:       20 * time.Millisecond,
		Timeout:        time.Second,
		Validate:       validate,
		Logger:         slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go p.Run(ctx)
}

func poolAddrs(pool *backend.Pool) map[string]bool {
	out := map[string]bool{}
	for _, b := range pool.Backends() {
		out[b.Addr()] = true
	}
	return out
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

func newTestPool(t *testing.T, addrs ...string) *backend.Pool {
	t.Helper()
	pool, err := backend.NewPool(addrs, "round-robin")
	if err != nil {
		t.Fatal(err)
	}
	return pool
}

func TestDiscoveryAddsBackends(t *testing.T) {
	f := newFakeAPIServer(t)
	f.setEndpoints(f.addr, "10.99.0.1:6443")

	pool := newTestPool(t, f.addr)
	startPoller(t, pool, writeKubeconfig(t, f), nil)

	waitFor(t, "discovered backend to be added", func() bool {
		return poolAddrs(pool)["10.99.0.1:6443"]
	})
	if !poolAddrs(pool)[f.addr] {
		t.Error("original backend was dropped")
	}
	if got := f.lastAuth(); got != "Bearer test-token" {
		t.Errorf("Authorization = %q, want Bearer test-token", got)
	}
}

func TestDiscoveryRemovesAndDrainsBackends(t *testing.T) {
	f := newFakeAPIServer(t)
	f.setEndpoints(f.addr, "10.99.0.1:6443")

	pool := newTestPool(t, f.addr, "10.99.0.1:6443")

	// Give the soon-to-be-removed backend an in-flight connection.
	var removed *backend.Backend
	for _, b := range pool.Backends() {
		if b.Addr() == "10.99.0.1:6443" {
			removed = b
		}
	}
	c1, c2 := net.Pipe()
	defer c2.Close()
	removed.Track(c1)

	startPoller(t, pool, writeKubeconfig(t, f), nil)
	waitFor(t, "initial sync", func() bool { return poolAddrs(pool)["10.99.0.1:6443"] })

	f.setEndpoints(f.addr) // 10.99.0.1 disappears from endpoints

	waitFor(t, "removed backend to leave the pool", func() bool {
		return !poolAddrs(pool)["10.99.0.1:6443"]
	})
	waitFor(t, "removed backend's connections to be drained", func() bool {
		return removed.ActiveConns() == 0
	})
	if got := removed.Stats().Drained.Load(); got != 1 {
		t.Errorf("Drained = %d, want 1", got)
	}
}

func TestDiscoveryIgnoresEmptyList(t *testing.T) {
	f := newFakeAPIServer(t)
	f.setEndpoints() // endpoints object exists but has no addresses

	pool := newTestPool(t, f.addr, "10.99.0.1:6443")
	startPoller(t, pool, writeKubeconfig(t, f), nil)

	time.Sleep(100 * time.Millisecond) // several poll rounds
	if len(poolAddrs(pool)) != 2 {
		t.Errorf("pool = %v, want unchanged 2 backends", poolAddrs(pool))
	}
}

func TestDiscoveryRejectsInvalidList(t *testing.T) {
	f := newFakeAPIServer(t)
	f.setEndpoints(f.addr, "10.99.0.9:6443")

	pool := newTestPool(t, f.addr)
	rejectAll := func(addrs []string) error { return fmt.Errorf("rejected by test") }
	startPoller(t, pool, writeKubeconfig(t, f), rejectAll)

	time.Sleep(100 * time.Millisecond)
	if poolAddrs(pool)["10.99.0.9:6443"] {
		t.Error("invalid list was applied to the pool")
	}
}

func TestDiscoveryNoChurnOnSameList(t *testing.T) {
	f := newFakeAPIServer(t)
	f.setEndpoints(f.addr)

	pool := newTestPool(t, f.addr)
	before := pool.Backends()[0]

	startPoller(t, pool, writeKubeconfig(t, f), nil)
	time.Sleep(100 * time.Millisecond)

	if pool.Backends()[0] != before {
		t.Error("backend instance replaced despite unchanged list; state was lost")
	}
}

func TestDiscoveryWaitsForKubeconfig(t *testing.T) {
	// Pre-join scenario: the kubeconfig (e.g. kubelet.conf) does not
	// exist when the service starts. Discovery must stay dormant, then
	// activate once the file appears.
	f := newFakeAPIServer(t)
	f.setEndpoints(f.addr, "10.99.0.5:6443")

	dir := t.TempDir()
	path := filepath.Join(dir, "kubelet.conf") // not written yet

	pool := newTestPool(t, f.addr)
	startPoller(t, pool, path, nil)

	time.Sleep(100 * time.Millisecond)
	if poolAddrs(pool)["10.99.0.5:6443"] {
		t.Fatal("discovery ran without a kubeconfig")
	}

	// Kubeconfig appears (node joined).
	real := writeKubeconfig(t, f)
	data, _ := os.ReadFile(real)
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}

	waitFor(t, "discovery to activate after kubeconfig appears", func() bool {
		return poolAddrs(pool)["10.99.0.5:6443"]
	})
}

func TestDiscoverySurvivesFetchErrors(t *testing.T) {
	f := newFakeAPIServer(t)
	f.setEndpoints(f.addr)
	kubeconfigPath := writeKubeconfig(t, f)

	// Pool contains only an unreachable backend: every fetch fails.
	deadLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	deadAddr := deadLn.Addr().String()
	deadLn.Close()

	pool := newTestPool(t, deadAddr)
	startPoller(t, pool, kubeconfigPath, nil)

	time.Sleep(100 * time.Millisecond) // must not panic or alter the pool
	if len(poolAddrs(pool)) != 1 || !poolAddrs(pool)[deadAddr] {
		t.Errorf("pool changed despite fetch errors: %v", poolAddrs(pool))
	}
}

func TestDiscoveryStopsOnCancel(t *testing.T) {
	f := newFakeAPIServer(t)
	f.setEndpoints(f.addr)

	pool := newTestPool(t, f.addr)
	p := New(pool, Options{
		KubeconfigPath: writeKubeconfig(t, f),
		Interval:       20 * time.Millisecond,
		Timeout:        time.Second,
		Validate:       func([]string) error { return nil },
		Logger:         slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		p.Run(ctx)
		close(done)
	}()

	waitFor(t, "first poll", func() bool { return f.lastAuth() != "" })
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Error("Run did not return after context cancellation")
	}
}
