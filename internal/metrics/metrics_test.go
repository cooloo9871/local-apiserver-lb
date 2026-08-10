package metrics

import (
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/cooloo9871/local-apiserver-lb/internal/backend"
)

func newTestPool(t *testing.T, addrs ...string) *backend.Pool {
	t.Helper()
	pool, err := backend.NewPool(addrs, "round-robin")
	if err != nil {
		t.Fatal(err)
	}
	return pool
}

func doGet(t *testing.T, h http.Handler, path string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
	return rec
}

func TestMetricsOutput(t *testing.T) {
	pool := newTestPool(t, "10.0.0.1:6443", "10.0.0.2:6443")

	var b1 *backend.Backend
	for _, b := range pool.Backends() {
		if b.Addr() == "10.0.0.1:6443" {
			b1 = b
		}
	}

	// Shape some state: b1 unhealthy with counters, one live conn.
	b1.ReportHealth(false, 1, 1) // unhealthy, CheckFailures=1
	b1.Stats().DialErrors.Add(3)
	c1, c2 := net.Pipe()
	defer c1.Close()
	defer c2.Close()
	b1.Track(c1) // ConnsTotal=1, ActiveConns=1

	rec := doGet(t, NewHandler(pool), "/metrics")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /metrics = %d, want 200", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/plain") {
		t.Errorf("Content-Type = %q, want text/plain", ct)
	}
	body := rec.Body.String()

	for _, want := range []string{
		`# TYPE apiserver_lb_backend_healthy gauge`,
		`# TYPE apiserver_lb_backend_connections gauge`,
		`# TYPE apiserver_lb_connections_total counter`,
		`# TYPE apiserver_lb_dial_errors_total counter`,
		`# TYPE apiserver_lb_health_check_failures_total counter`,
		`# TYPE apiserver_lb_connections_drained_total counter`,
		`apiserver_lb_backend_healthy{server="10.0.0.1:6443"} 0`,
		`apiserver_lb_backend_healthy{server="10.0.0.2:6443"} 1`,
		`apiserver_lb_backend_connections{server="10.0.0.1:6443"} 1`,
		`apiserver_lb_backend_connections{server="10.0.0.2:6443"} 0`,
		`apiserver_lb_connections_total{server="10.0.0.1:6443"} 1`,
		`apiserver_lb_dial_errors_total{server="10.0.0.1:6443"} 3`,
		`apiserver_lb_health_check_failures_total{server="10.0.0.1:6443"} 1`,
		`apiserver_lb_connections_drained_total{server="10.0.0.1:6443"} 0`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("metrics output missing %q\n--- body ---\n%s", want, body)
		}
	}

	// Every HELP line must precede its TYPE line.
	if !strings.Contains(body, "# HELP apiserver_lb_backend_healthy ") {
		t.Error("missing HELP line for apiserver_lb_backend_healthy")
	}
}

func TestMetricsLabelEscaping(t *testing.T) {
	// Addresses are validated host:port so this is defense in depth, but
	// the text format must never be corrupted by a label value.
	pool := newTestPool(t, `weird"host:6443`)
	body := doGet(t, NewHandler(pool), "/metrics").Body.String()
	if !strings.Contains(body, `server="weird\"host:6443"`) {
		t.Errorf("label value not escaped:\n%s", body)
	}
}

func TestHealthz(t *testing.T) {
	pool := newTestPool(t, "10.0.0.1:6443")
	rec := doGet(t, NewHandler(pool), "/healthz")
	if rec.Code != http.StatusOK {
		t.Errorf("GET /healthz = %d, want 200", rec.Code)
	}
}

func TestReadyzReflectsBackendHealth(t *testing.T) {
	pool := newTestPool(t, "10.0.0.1:6443", "10.0.0.2:6443")
	h := NewHandler(pool)

	if rec := doGet(t, h, "/readyz"); rec.Code != http.StatusOK {
		t.Errorf("GET /readyz = %d with healthy backends, want 200", rec.Code)
	}

	for _, b := range pool.Backends() {
		b.ReportHealth(false, 1, 1)
	}
	if rec := doGet(t, h, "/readyz"); rec.Code != http.StatusServiceUnavailable {
		t.Errorf("GET /readyz = %d with no healthy backend, want 503", rec.Code)
	}

	pool.Backends()[0].ReportHealth(true, 1, 1)
	if rec := doGet(t, h, "/readyz"); rec.Code != http.StatusOK {
		t.Errorf("GET /readyz = %d after recovery, want 200", rec.Code)
	}
}

func TestUnknownPath(t *testing.T) {
	pool := newTestPool(t, "10.0.0.1:6443")
	if rec := doGet(t, NewHandler(pool), "/nope"); rec.Code != http.StatusNotFound {
		t.Errorf("GET /nope = %d, want 404", rec.Code)
	}
}
