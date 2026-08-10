// Package metrics exposes the load balancer's observability endpoints:
// /healthz (process liveness), /readyz (at least one healthy backend),
// and /metrics in Prometheus text format. The format is written by hand
// to keep the binary free of third-party dependencies.
package metrics

import (
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/cooloo9871/local-apiserver-lb/internal/backend"
)

// NewHandler returns the HTTP handler serving /healthz, /readyz, and
// /metrics for the given pool.
func NewHandler(pool *backend.Pool) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		io.WriteString(w, "ok\n")
	})
	mux.HandleFunc("/readyz", func(w http.ResponseWriter, r *http.Request) {
		if pool.HealthyCount() == 0 {
			http.Error(w, "no healthy backends", http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
		io.WriteString(w, "ok\n")
	})
	mux.HandleFunc("/metrics", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
		writeMetrics(w, pool)
	})
	return mux
}

// family describes one metric family and how to read its value from a
// backend.
type family struct {
	name  string
	kind  string // "gauge" or "counter"
	help  string
	value func(b *backend.Backend) uint64
}

var families = []family{
	{
		name: "apiserver_lb_backend_healthy",
		kind: "gauge",
		help: "Whether the backend is currently considered healthy (1) or not (0).",
		value: func(b *backend.Backend) uint64 {
			if b.Healthy() {
				return 1
			}
			return 0
		},
	},
	{
		name:  "apiserver_lb_backend_connections",
		kind:  "gauge",
		help:  "Number of in-flight proxied connections to the backend.",
		value: func(b *backend.Backend) uint64 { return uint64(b.ActiveConns()) },
	},
	{
		name:  "apiserver_lb_connections_total",
		kind:  "counter",
		help:  "Total connections proxied to the backend.",
		value: func(b *backend.Backend) uint64 { return b.Stats().ConnsTotal.Load() },
	},
	{
		name:  "apiserver_lb_dial_errors_total",
		kind:  "counter",
		help:  "Total failed dial attempts to the backend.",
		value: func(b *backend.Backend) uint64 { return b.Stats().DialErrors.Load() },
	},
	{
		name:  "apiserver_lb_health_check_failures_total",
		kind:  "counter",
		help:  "Total failed health probes against the backend.",
		value: func(b *backend.Backend) uint64 { return b.Stats().CheckFailures.Load() },
	},
	{
		name:  "apiserver_lb_connections_drained_total",
		kind:  "counter",
		help:  "Total connections force-closed because the backend turned unhealthy or was removed.",
		value: func(b *backend.Backend) uint64 { return b.Stats().Drained.Load() },
	},
}

// writeMetrics renders all metric families for all backends.
func writeMetrics(w io.Writer, pool *backend.Pool) {
	backends := pool.Backends()
	for _, f := range families {
		fmt.Fprintf(w, "# HELP %s %s\n", f.name, f.help)
		fmt.Fprintf(w, "# TYPE %s %s\n", f.name, f.kind)
		for _, b := range backends {
			fmt.Fprintf(w, "%s{server=\"%s\"} %d\n", f.name, escapeLabel(b.Addr()), f.value(b))
		}
	}
}

// escapeLabel escapes a label value per the Prometheus text format:
// backslash, double quote, and newline.
func escapeLabel(s string) string {
	r := strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`)
	return r.Replace(s)
}
