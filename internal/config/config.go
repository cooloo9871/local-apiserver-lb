// Package config handles command-line flag parsing and validation of
// the effective configuration.
package config

import (
	"flag"
	"fmt"
	"io"
	"strings"
	"time"
)

// Config is the effective configuration of the load balancer.
type Config struct {
	Listen             string
	Servers            []string
	Balance            string
	HealthCheckMode    string
	HealthInterval     time.Duration
	HealthTimeout      time.Duration
	Fall               int
	Rise               int
	DialTimeout        time.Duration
	CAFile             string
	InsecureSkipVerify bool
	KeepalivePeriod    time.Duration
	MetricsListen      string
	LogLevel           string
	LogFormat          string
	AllowNonLoopback   bool
	ShutdownGrace      time.Duration
	ShowVersion        bool

	// DiscoveryKubeconfig enables dynamic backend discovery from the
	// default/kubernetes Endpoints object when non-empty. The file may
	// legitimately not exist yet at startup (pre-join kubelet.conf).
	DiscoveryKubeconfig string
	DiscoveryInterval   time.Duration

	// StateFile persists discovery's applied server list across
	// restarts. Only meaningful together with DiscoveryKubeconfig.
	StateFile string

	// explicit records flag names the user set on the command line,
	// used for config-file precedence and conflict detection.
	explicit map[string]bool
}

// Explicit reports whether the named flag was explicitly set on the
// command line.
func (c *Config) Explicit(name string) bool {
	return c.explicit[name]
}

// Parse parses command-line arguments into a Config. Parse does not
// validate the result; call Validate for that.
func Parse(args []string) (*Config, error) {
	cfg := &Config{explicit: make(map[string]bool)}

	fs := flag.NewFlagSet("local-apiserver-lb", flag.ContinueOnError)
	fs.SetOutput(io.Discard)

	var serversCSV string
	fs.StringVar(&cfg.Listen, "listen", "127.0.0.1:6443", "listen address")
	fs.StringVar(&serversCSV, "servers", "", "comma-separated backend list (host:port)")
	fs.StringVar(&cfg.Balance, "balance", "round-robin", "balancing strategy: round-robin or least-conn")
	fs.StringVar(&cfg.HealthCheckMode, "health-check-mode", "readyz", "health check mode: readyz or tcp")
	fs.DurationVar(&cfg.HealthInterval, "health-interval", 3*time.Second, "health check interval")
	fs.DurationVar(&cfg.HealthTimeout, "health-timeout", 3*time.Second, "health check timeout")
	fs.IntVar(&cfg.Fall, "fall", 2, "consecutive failures before marking a backend unhealthy")
	fs.IntVar(&cfg.Rise, "rise", 2, "consecutive successes before marking a backend healthy")
	fs.DurationVar(&cfg.DialTimeout, "dial-timeout", 3*time.Second, "backend dial timeout")
	fs.StringVar(&cfg.CAFile, "ca-file", "", "CA bundle for health check chain verification")
	fs.BoolVar(&cfg.InsecureSkipVerify, "insecure-skip-verify", true, "skip TLS verification on health checks")
	fs.DurationVar(&cfg.KeepalivePeriod, "keepalive-period", 30*time.Second, "TCP keepalive period")
	fs.StringVar(&cfg.MetricsListen, "metrics-listen", "", "metrics listen address (empty disables)")
	fs.StringVar(&cfg.LogLevel, "log-level", "info", "log level: debug, info, warn, error")
	fs.StringVar(&cfg.LogFormat, "log-format", "text", "log format: text or json")
	fs.BoolVar(&cfg.AllowNonLoopback, "allow-non-loopback", false, "allow binding to a non-loopback address")
	fs.DurationVar(&cfg.ShutdownGrace, "shutdown-grace", 10*time.Second, "grace period for in-flight connections on shutdown")
	fs.BoolVar(&cfg.ShowVersion, "version", false, "print version and exit")
	fs.StringVar(&cfg.DiscoveryKubeconfig, "discovery-kubeconfig", "",
		"kubeconfig for dynamic backend discovery from the default/kubernetes Endpoints object (empty disables)")
	fs.DurationVar(&cfg.DiscoveryInterval, "discovery-interval", 30*time.Second,
		"how often to refresh the backend list when discovery is enabled")
	fs.StringVar(&cfg.StateFile, "state-file", "",
		"persist the discovered backend list here and restore it at startup (requires --discovery-kubeconfig)")

	if err := fs.Parse(args); err != nil {
		return nil, err
	}
	if fs.NArg() > 0 {
		return nil, fmt.Errorf("unexpected positional arguments: %v", fs.Args())
	}

	fs.Visit(func(f *flag.Flag) { cfg.explicit[f.Name] = true })

	if serversCSV != "" {
		cfg.Servers = splitServers(serversCSV)
	}

	return cfg, nil
}

// splitServers splits a comma-separated server list, trimming whitespace
// and dropping empty entries.
func splitServers(csv string) []string {
	var out []string
	for _, s := range strings.Split(csv, ",") {
		if s = strings.TrimSpace(s); s != "" {
			out = append(out, s)
		}
	}
	return out
}
