package config

import (
	"fmt"
	"net"
	"os"
	"strconv"
	"time"
)

// LookupFunc resolves a hostname to IP addresses. It exists so that tests
// can inject a static resolver; nil means net.LookupIP.
type LookupFunc func(host string) ([]net.IP, error)

// Validate checks the effective configuration and returns the first
// problem found. It must be called before the config is used; a config
// that fails validation must not be run.
//
// As a side effect, a set --ca-file implies certificate verification, so
// InsecureSkipVerify is forced to false unless the user explicitly asked
// for the contradictory combination (which is an error).
func (c *Config) Validate(lookup LookupFunc) error {
	if lookup == nil {
		lookup = net.LookupIP
	}

	switch c.Balance {
	case "round-robin", "least-conn":
	default:
		return fmt.Errorf("--balance must be \"round-robin\" or \"least-conn\", got %q", c.Balance)
	}
	switch c.HealthCheckMode {
	case "readyz", "tcp":
	default:
		return fmt.Errorf("--health-check-mode must be \"readyz\" or \"tcp\", got %q", c.HealthCheckMode)
	}
	switch c.LogLevel {
	case "debug", "info", "warn", "error":
	default:
		return fmt.Errorf("--log-level must be one of debug, info, warn, error, got %q", c.LogLevel)
	}
	switch c.LogFormat {
	case "text", "json":
	default:
		return fmt.Errorf("--log-format must be \"text\" or \"json\", got %q", c.LogFormat)
	}

	if c.Fall < 1 {
		return fmt.Errorf("--fall must be >= 1, got %d", c.Fall)
	}
	if c.Rise < 1 {
		return fmt.Errorf("--rise must be >= 1, got %d", c.Rise)
	}
	for _, d := range []struct {
		name string
		val  time.Duration
	}{
		{"health-interval", c.HealthInterval},
		{"health-timeout", c.HealthTimeout},
		{"dial-timeout", c.DialTimeout},
		{"shutdown-grace", c.ShutdownGrace},
		{"discovery-interval", c.DiscoveryInterval},
	} {
		if d.val <= 0 {
			return fmt.Errorf("--%s must be > 0, got %v", d.name, d.val)
		}
	}
	// Note: c.DiscoveryKubeconfig is deliberately NOT checked for
	// existence. Pre-join workers point it at kubelet.conf, which only
	// appears after kubeadm join; discovery waits for it at runtime.
	if c.KeepalivePeriod < 0 {
		return fmt.Errorf("--keepalive-period must be >= 0, got %v", c.KeepalivePeriod)
	}

	listenHost, err := validateHostPort(c.Listen)
	if err != nil {
		return fmt.Errorf("--listen: %w", err)
	}
	listenIPs, err := resolveHost(listenHost, lookup)
	if err != nil {
		return fmt.Errorf("--listen: resolving %q: %w", listenHost, err)
	}
	if !c.AllowNonLoopback {
		for _, ip := range listenIPs {
			if !ip.IsLoopback() {
				return fmt.Errorf(
					"refusing to listen on non-loopback address %s: the backends behind this port "+
						"accept connections without any additional authentication layer, so exposing it "+
						"beyond localhost opens the apiserver to the network; pass --allow-non-loopback "+
						"if this is really intended", c.Listen)
			}
		}
	}

	if err := validateServers(c.Servers, c.Listen, listenIPs, lookup); err != nil {
		return err
	}

	if c.MetricsListen != "" {
		if _, err := validateHostPort(c.MetricsListen); err != nil {
			return fmt.Errorf("--metrics-listen: %w", err)
		}
	}

	if c.CAFile != "" {
		if c.Explicit("insecure-skip-verify") && c.InsecureSkipVerify {
			return fmt.Errorf("--ca-file and --insecure-skip-verify=true are contradictory: " +
				"remove one of them (--ca-file alone enables chain verification)")
		}
		f, err := os.Open(c.CAFile)
		if err != nil {
			return fmt.Errorf("--ca-file: %w", err)
		}
		f.Close()
		c.InsecureSkipVerify = false
	} else if c.Explicit("insecure-skip-verify") && !c.InsecureSkipVerify {
		return fmt.Errorf("--insecure-skip-verify=false requires --ca-file: the kubernetes CA is " +
			"not in the system trust store, so verification without a CA bundle would always fail")
	}

	return nil
}

// ValidateServers validates a backend list against the (already validated)
// listen address. It is used both by Validate and by SIGHUP reloads.
func ValidateServers(servers []string, listen string, lookup LookupFunc) error {
	if lookup == nil {
		lookup = net.LookupIP
	}
	listenHost, err := validateHostPort(listen)
	if err != nil {
		return fmt.Errorf("listen address: %w", err)
	}
	listenIPs, err := resolveHost(listenHost, lookup)
	if err != nil {
		return fmt.Errorf("listen address: resolving %q: %w", listenHost, err)
	}
	return validateServers(servers, listen, listenIPs, lookup)
}

func validateServers(servers []string, listen string, listenIPs []net.IP, lookup LookupFunc) error {
	if len(servers) == 0 {
		return fmt.Errorf("--servers is required and must not be empty")
	}
	_, listenPort, _ := net.SplitHostPort(listen)

	seen := make(map[string]bool, len(servers))
	for _, s := range servers {
		if seen[s] {
			return fmt.Errorf("--servers: duplicate entry %q", s)
		}
		seen[s] = true

		host, err := validateHostPort(s)
		if err != nil {
			return fmt.Errorf("--servers: entry %q: %w", s, err)
		}
		ips, err := resolveHost(host, lookup)
		if err != nil {
			return fmt.Errorf("--servers: entry %q: resolving %q: %w", s, host, err)
		}
		_, port, _ := net.SplitHostPort(s)

		for _, ip := range ips {
			if ip.IsLoopback() {
				return fmt.Errorf(
					"--servers: entry %q resolves to loopback address %s: forwarding to loopback "+
						"would make the load balancer dial itself in an infinite loop; this usually "+
						"means an /etc/hosts entry points the control plane endpoint at 127.0.0.1 on "+
						"this node — use the real control plane IPs in --servers instead", s, ip)
			}
			if port == listenPort {
				for _, lip := range listenIPs {
					if ip.Equal(lip) {
						return fmt.Errorf(
							"--servers: entry %q is the listen address itself (%s): forwarding to "+
								"ourselves would create an infinite loop", s, listen)
					}
				}
			}
		}
	}
	return nil
}

// validateHostPort checks that s is a syntactically valid "host:port" with
// a non-empty host and a port in [1, 65535], returning the host part.
func validateHostPort(s string) (host string, err error) {
	host, port, err := net.SplitHostPort(s)
	if err != nil {
		return "", fmt.Errorf("not a valid host:port: %w", err)
	}
	if host == "" {
		return "", fmt.Errorf("host must not be empty in %q", s)
	}
	n, err := strconv.Atoi(port)
	if err != nil || n < 1 || n > 65535 {
		return "", fmt.Errorf("invalid port %q (must be 1-65535)", port)
	}
	return host, nil
}

// resolveHost returns the IPs for a host, which may be an IP literal
// (returned as-is, no resolver involved) or a hostname.
func resolveHost(host string, lookup LookupFunc) ([]net.IP, error) {
	if ip := net.ParseIP(host); ip != nil {
		return []net.IP{ip}, nil
	}
	return lookup(host)
}
