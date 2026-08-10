package config

import (
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeLookup returns a LookupFunc backed by a static host table.
func fakeLookup(table map[string][]string) LookupFunc {
	return func(host string) ([]net.IP, error) {
		addrs, ok := table[host]
		if !ok {
			return nil, fmt.Errorf("no such host: %s", host)
		}
		var ips []net.IP
		for _, a := range addrs {
			ips = append(ips, net.ParseIP(a))
		}
		return ips, nil
	}
}

func validConfig(t *testing.T) *Config {
	t.Helper()
	cfg, err := Parse([]string{"--servers", "10.0.0.1:6443,10.0.0.2:6443"})
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

func TestValidateOK(t *testing.T) {
	cfg := validConfig(t)
	if err := cfg.Validate(nil); err != nil {
		t.Errorf("Validate returned error for valid config: %v", err)
	}
}

func TestValidateEmptyServers(t *testing.T) {
	cfg, err := Parse([]string{})
	if err != nil {
		t.Fatal(err)
	}
	if err := cfg.Validate(nil); err == nil {
		t.Error("empty server list accepted, want error")
	}
}

func TestValidateBadServerEntries(t *testing.T) {
	for _, servers := range []string{
		"10.0.0.1",          // missing port
		"10.0.0.1:notaport", // non-numeric port
		"10.0.0.1:70000",    // port out of range
		"10.0.0.1:0",        // port zero
		":6443",             // empty host
	} {
		cfg, err := Parse([]string{"--servers", servers})
		if err != nil {
			t.Fatal(err)
		}
		if err := cfg.Validate(nil); err == nil {
			t.Errorf("servers=%q accepted, want error", servers)
		}
	}
}

func TestValidateRejectsLoopbackServerLiterals(t *testing.T) {
	for _, servers := range []string{
		"127.0.0.1:6443",
		"127.5.5.5:6443", // anywhere in 127.0.0.0/8
		"[::1]:6443",
		"10.0.0.1:6443,127.0.0.1:6443", // mixed with valid entries
	} {
		cfg, err := Parse([]string{"--servers", servers})
		if err != nil {
			t.Fatal(err)
		}
		err = cfg.Validate(nil)
		if err == nil {
			t.Errorf("servers=%q accepted, want loopback rejection", servers)
			continue
		}
		if !strings.Contains(err.Error(), "loopback") {
			t.Errorf("error %q does not explain the loopback problem", err)
		}
	}
}

func TestValidateRejectsHostnameResolvingToLoopback(t *testing.T) {
	// The real-world trap: controlPlaneEndpoint pointed at 127.0.0.1 via
	// /etc/hosts, and the same name used in --servers.
	cfg, err := Parse([]string{"--servers", "k8s-api.example.com:6443"})
	if err != nil {
		t.Fatal(err)
	}
	lookup := fakeLookup(map[string][]string{"k8s-api.example.com": {"127.0.0.1"}})
	err = cfg.Validate(lookup)
	if err == nil {
		t.Fatal("hostname resolving to loopback accepted, want error")
	}
	if !strings.Contains(err.Error(), "loopback") {
		t.Errorf("error %q does not explain the loopback problem", err)
	}
}

func TestValidateAcceptsResolvableHostname(t *testing.T) {
	cfg, err := Parse([]string{"--servers", "cp1.example.com:6443"})
	if err != nil {
		t.Fatal(err)
	}
	lookup := fakeLookup(map[string][]string{"cp1.example.com": {"10.0.0.1"}})
	if err := cfg.Validate(lookup); err != nil {
		t.Errorf("resolvable hostname rejected: %v", err)
	}
}

func TestValidateRejectsUnresolvableHostname(t *testing.T) {
	cfg, err := Parse([]string{"--servers", "no-such-host.example.com:6443"})
	if err != nil {
		t.Fatal(err)
	}
	lookup := fakeLookup(map[string][]string{})
	if err := cfg.Validate(lookup); err == nil {
		t.Error("unresolvable hostname accepted, want error")
	}
}

func TestValidateRejectsServerEqualToListen(t *testing.T) {
	cfg, err := Parse([]string{
		"--listen", "10.0.0.5:6443",
		"--allow-non-loopback",
		"--servers", "10.0.0.1:6443,10.0.0.5:6443",
	})
	if err != nil {
		t.Fatal(err)
	}
	err = cfg.Validate(nil)
	if err == nil {
		t.Fatal("server equal to listen address accepted, want error")
	}
}

func TestValidateNonLoopbackListenRequiresFlag(t *testing.T) {
	cfg, err := Parse([]string{"--listen", "0.0.0.0:6443", "--servers", "10.0.0.1:6443"})
	if err != nil {
		t.Fatal(err)
	}
	err = cfg.Validate(nil)
	if err == nil {
		t.Fatal("non-loopback listen accepted without --allow-non-loopback")
	}
	if !strings.Contains(err.Error(), "allow-non-loopback") {
		t.Errorf("error %q does not mention --allow-non-loopback", err)
	}

	cfg, err = Parse([]string{
		"--listen", "0.0.0.0:6443",
		"--allow-non-loopback",
		"--servers", "10.0.0.1:6443",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := cfg.Validate(nil); err != nil {
		t.Errorf("non-loopback listen with flag rejected: %v", err)
	}
}

func TestValidateListenHostname(t *testing.T) {
	cfg, err := Parse([]string{"--listen", "localhost:6443", "--servers", "10.0.0.1:6443"})
	if err != nil {
		t.Fatal(err)
	}
	lookup := fakeLookup(map[string][]string{"localhost": {"127.0.0.1"}})
	if err := cfg.Validate(lookup); err != nil {
		t.Errorf("localhost listen rejected: %v", err)
	}
}

func TestValidateEnumFields(t *testing.T) {
	cases := []struct{ flagName, value string }{
		{"balance", "random"},
		{"health-check-mode", "icmp"},
		{"log-level", "loud"},
		{"log-format", "xml"},
	}
	for _, tc := range cases {
		cfg, err := Parse([]string{"--servers", "10.0.0.1:6443", "--" + tc.flagName, tc.value})
		if err != nil {
			t.Fatal(err)
		}
		if err := cfg.Validate(nil); err == nil {
			t.Errorf("--%s=%s accepted, want error", tc.flagName, tc.value)
		}
	}
}

func TestValidateNumericBounds(t *testing.T) {
	cases := [][]string{
		{"--fall", "0"},
		{"--rise", "0"},
		{"--fall", "-1"},
		{"--health-interval", "0s"},
		{"--health-timeout", "-1s"},
		{"--dial-timeout", "0s"},
		{"--shutdown-grace", "-1s"},
	}
	for _, extra := range cases {
		args := append([]string{"--servers", "10.0.0.1:6443"}, extra...)
		cfg, err := Parse(args)
		if err != nil {
			t.Fatal(err)
		}
		if err := cfg.Validate(nil); err == nil {
			t.Errorf("args %v accepted, want error", extra)
		}
	}
}

func TestValidateKeepaliveZeroAllowed(t *testing.T) {
	// 0 disables keepalive; that is a valid (if discouraged) setting.
	cfg, err := Parse([]string{"--servers", "10.0.0.1:6443", "--keepalive-period", "0s"})
	if err != nil {
		t.Fatal(err)
	}
	if err := cfg.Validate(nil); err != nil {
		t.Errorf("keepalive-period=0 rejected: %v", err)
	}
}

func TestValidateCAFile(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "missing.crt")
	cfg, err := Parse([]string{"--servers", "10.0.0.1:6443", "--ca-file", missing})
	if err != nil {
		t.Fatal(err)
	}
	if err := cfg.Validate(nil); err == nil {
		t.Error("missing CA file accepted, want error")
	}

	present := filepath.Join(t.TempDir(), "ca.crt")
	if err := os.WriteFile(present, []byte("dummy"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err = Parse([]string{"--servers", "10.0.0.1:6443", "--ca-file", present})
	if err != nil {
		t.Fatal(err)
	}
	if err := cfg.Validate(nil); err != nil {
		t.Fatalf("readable CA file rejected: %v", err)
	}
	if cfg.InsecureSkipVerify {
		t.Error("InsecureSkipVerify still true after --ca-file; want implicit false")
	}
}

func TestValidateCAFileConflictsWithExplicitSkipVerify(t *testing.T) {
	present := filepath.Join(t.TempDir(), "ca.crt")
	if err := os.WriteFile(present, []byte("dummy"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := Parse([]string{
		"--servers", "10.0.0.1:6443",
		"--ca-file", present,
		"--insecure-skip-verify=true",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := cfg.Validate(nil); err == nil {
		t.Error("--ca-file with explicit --insecure-skip-verify=true accepted, want conflict error")
	}
}

func TestValidateSkipVerifyFalseRequiresCAFile(t *testing.T) {
	cfg, err := Parse([]string{"--servers", "10.0.0.1:6443", "--insecure-skip-verify=false"})
	if err != nil {
		t.Fatal(err)
	}
	if err := cfg.Validate(nil); err == nil {
		t.Error("--insecure-skip-verify=false without --ca-file accepted, want error")
	}
}

func TestValidateMetricsListen(t *testing.T) {
	cfg, err := Parse([]string{"--servers", "10.0.0.1:6443", "--metrics-listen", "notanaddress"})
	if err != nil {
		t.Fatal(err)
	}
	if err := cfg.Validate(nil); err == nil {
		t.Error("bad metrics-listen accepted, want error")
	}
}

func TestValidateServersForReload(t *testing.T) {
	lookup := fakeLookup(map[string][]string{"cp1": {"10.0.0.1"}})
	if err := ValidateServers([]string{"cp1:6443"}, "127.0.0.1:6443", lookup); err != nil {
		t.Errorf("valid reload list rejected: %v", err)
	}
	if err := ValidateServers([]string{"127.0.0.1:9999"}, "127.0.0.1:6443", lookup); err == nil {
		t.Error("loopback entry accepted on reload, want error")
	}
	if err := ValidateServers(nil, "127.0.0.1:6443", lookup); err == nil {
		t.Error("empty reload list accepted, want error")
	}
}
