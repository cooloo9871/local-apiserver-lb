// Command local-apiserver-lb is a node-local L4 TCP load balancer for
// Kubernetes apiservers. Worker components (kubelet, kube-proxy) connect
// to 127.0.0.1:6443 and this process forwards each connection to a
// healthy control plane, with active health checking and connection
// draining on failover. TLS is never terminated: traffic is passed
// through byte for byte.
package main

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/cooloo9871/local-apiserver-lb/internal/backend"
	"github.com/cooloo9871/local-apiserver-lb/internal/config"
	"github.com/cooloo9871/local-apiserver-lb/internal/discovery"
	"github.com/cooloo9871/local-apiserver-lb/internal/health"
	"github.com/cooloo9871/local-apiserver-lb/internal/metrics"
	"github.com/cooloo9871/local-apiserver-lb/internal/proxy"
	"github.com/cooloo9871/local-apiserver-lb/internal/version"
)

func main() {
	os.Exit(run(os.Args[1:]))
}

func run(args []string) int {
	cfg, err := config.Parse(args)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		return 2
	}
	if cfg.ShowVersion {
		fmt.Println(version.String())
		return 0
	}
	if err := cfg.Validate(nil); err != nil {
		fmt.Fprintf(os.Stderr, "error: invalid configuration: %v\n", err)
		return 1
	}

	logger := newLogger(cfg)
	logger.Info("starting "+version.String(),
		"listen", cfg.Listen,
		"servers", cfg.Servers,
		"balance", cfg.Balance,
		"health_check_mode", cfg.HealthCheckMode,
		"health_interval", cfg.HealthInterval,
		"health_timeout", cfg.HealthTimeout,
		"fall", cfg.Fall,
		"rise", cfg.Rise,
		"dial_timeout", cfg.DialTimeout,
		"ca_file", cfg.CAFile,
		"insecure_skip_verify", cfg.InsecureSkipVerify,
		"keepalive_period", cfg.KeepalivePeriod,
		"metrics_listen", cfg.MetricsListen,
		"shutdown_grace", cfg.ShutdownGrace,
		"config_file", cfg.ConfigFile,
		"discovery_kubeconfig", cfg.DiscoveryKubeconfig,
		"discovery_interval", cfg.DiscoveryInterval,
	)

	pool, err := backend.NewPool(cfg.Servers, cfg.Balance)
	if err != nil {
		logger.Error("failed to build backend pool", "error", err)
		return 1
	}

	prober, err := health.NewProber(cfg.HealthCheckMode, cfg.CAFile)
	if err != nil {
		logger.Error("failed to build health prober", "error", err)
		return 1
	}
	checker := health.NewChecker(pool, prober, health.Options{
		Interval: cfg.HealthInterval,
		Timeout:  cfg.HealthTimeout,
		Fall:     cfg.Fall,
		Rise:     cfg.Rise,
		Logger:   logger,
	})

	ln, err := net.Listen("tcp", cfg.Listen)
	if err != nil {
		logger.Error("failed to listen", "address", cfg.Listen, "error", err)
		return 1
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()
	go checker.Run(ctx)

	if cfg.DiscoveryKubeconfig != "" {
		poller := discovery.New(pool, discovery.Options{
			KubeconfigPath: cfg.DiscoveryKubeconfig,
			Interval:       cfg.DiscoveryInterval,
			Timeout:        10 * time.Second,
			Validate: func(servers []string) error {
				return config.ValidateServers(servers, cfg.Listen, nil)
			},
			Logger: logger,
		})
		go poller.Run(ctx)
		logger.Info("dynamic backend discovery enabled",
			"kubeconfig", cfg.DiscoveryKubeconfig, "interval", cfg.DiscoveryInterval)
	}

	server := proxy.New(pool, proxy.Options{
		DialTimeout:     cfg.DialTimeout,
		KeepalivePeriod: cfg.KeepalivePeriod,
		Logger:          logger,
	})

	serveErr := make(chan error, 1)
	go func() { serveErr <- server.Serve(ln) }()
	logger.Info("load balancer is running",
		"listen", ln.Addr().String(), "backends", cfg.Servers)

	var metricsSrv *http.Server
	if cfg.MetricsListen != "" {
		metricsSrv = &http.Server{
			Addr:              cfg.MetricsListen,
			Handler:           metrics.NewHandler(pool),
			ReadHeaderTimeout: 5 * time.Second,
		}
		go func() {
			logger.Info("metrics server is running", "listen", cfg.MetricsListen)
			if err := metricsSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
				logger.Error("metrics server failed", "error", err)
			}
		}()
	}

	hup := make(chan os.Signal, 1)
	signal.Notify(hup, syscall.SIGHUP)
	go func() {
		for range hup {
			reloadServers(cfg, pool, logger)
		}
	}()

	select {
	case <-ctx.Done():
		logger.Info("received shutdown signal; stopping accept and draining",
			"grace", cfg.ShutdownGrace)
	case err := <-serveErr:
		logger.Error("listener failed", "error", err)
		return 1
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), cfg.ShutdownGrace)
	defer cancel()
	if err := server.Shutdown(shutdownCtx); err != nil {
		logger.Error("shutdown failed", "error", err)
		return 1
	}
	if metricsSrv != nil {
		metricsSrv.Close()
	}
	logger.Info("shutdown complete")
	return 0
}

// reloadServers handles SIGHUP: it re-reads only the server list from the
// config file, validates it, reconciles the pool, and drains connections
// on backends that were removed. All other settings stay as loaded at
// startup.
func reloadServers(cfg *config.Config, pool *backend.Pool, logger *slog.Logger) {
	if cfg.ConfigFile == "" {
		logger.Warn("SIGHUP received but no --config file is in use; nothing to reload")
		return
	}
	servers, err := config.LoadServersFromFile(cfg.ConfigFile)
	if err != nil {
		logger.Error("SIGHUP reload failed; keeping current backends", "error", err)
		return
	}
	if err := config.ValidateServers(servers, cfg.Listen, nil); err != nil {
		logger.Error("SIGHUP reload rejected; keeping current backends", "error", err)
		return
	}

	added, removed := pool.SetAddrs(servers)
	if len(added) == 0 && len(removed) == 0 {
		logger.Info("SIGHUP reload: server list unchanged")
		return
	}

	logger.Info("SIGHUP reload applied", "servers", servers)
	for _, b := range added {
		logger.Info("backend added", "backend", b.Addr())
	}
	for _, b := range removed {
		drained := b.DrainAll()
		logger.Info("backend removed", "backend", b.Addr(), "drained_connections", drained)
	}
}

// newLogger builds the slog logger from config: text or JSON handler,
// writing to stderr for journald to capture.
func newLogger(cfg *config.Config) *slog.Logger {
	levels := map[string]slog.Level{
		"debug": slog.LevelDebug,
		"info":  slog.LevelInfo,
		"warn":  slog.LevelWarn,
		"error": slog.LevelError,
	}
	opts := &slog.HandlerOptions{Level: levels[cfg.LogLevel]}
	var handler slog.Handler
	if cfg.LogFormat == "json" {
		handler = slog.NewJSONHandler(os.Stderr, opts)
	} else {
		handler = slog.NewTextHandler(os.Stderr, opts)
	}
	return slog.New(handler)
}
