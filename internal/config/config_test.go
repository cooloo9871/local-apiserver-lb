package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestParseDefaults(t *testing.T) {
	cfg, err := Parse([]string{"--servers", "10.0.0.1:6443,10.0.0.2:6443"})
	if err != nil {
		t.Fatalf("Parse returned error: %v", err)
	}

	if cfg.Listen != "127.0.0.1:6443" {
		t.Errorf("Listen = %q, want 127.0.0.1:6443", cfg.Listen)
	}
	wantServers := []string{"10.0.0.1:6443", "10.0.0.2:6443"}
	if len(cfg.Servers) != len(wantServers) {
		t.Fatalf("Servers = %v, want %v", cfg.Servers, wantServers)
	}
	for i, s := range wantServers {
		if cfg.Servers[i] != s {
			t.Errorf("Servers[%d] = %q, want %q", i, cfg.Servers[i], s)
		}
	}
	if cfg.Balance != "round-robin" {
		t.Errorf("Balance = %q, want round-robin", cfg.Balance)
	}
	if cfg.HealthCheckMode != "readyz" {
		t.Errorf("HealthCheckMode = %q, want readyz", cfg.HealthCheckMode)
	}
	if cfg.HealthInterval != 3*time.Second {
		t.Errorf("HealthInterval = %v, want 3s", cfg.HealthInterval)
	}
	if cfg.HealthTimeout != 3*time.Second {
		t.Errorf("HealthTimeout = %v, want 3s", cfg.HealthTimeout)
	}
	if cfg.Fall != 2 {
		t.Errorf("Fall = %d, want 2", cfg.Fall)
	}
	if cfg.Rise != 2 {
		t.Errorf("Rise = %d, want 2", cfg.Rise)
	}
	if cfg.DialTimeout != 3*time.Second {
		t.Errorf("DialTimeout = %v, want 3s", cfg.DialTimeout)
	}
	if cfg.CAFile != "" {
		t.Errorf("CAFile = %q, want empty", cfg.CAFile)
	}
	if !cfg.InsecureSkipVerify {
		t.Error("InsecureSkipVerify = false, want true")
	}
	if cfg.KeepalivePeriod != 30*time.Second {
		t.Errorf("KeepalivePeriod = %v, want 30s", cfg.KeepalivePeriod)
	}
	if cfg.MetricsListen != "" {
		t.Errorf("MetricsListen = %q, want empty", cfg.MetricsListen)
	}
	if cfg.LogLevel != "info" {
		t.Errorf("LogLevel = %q, want info", cfg.LogLevel)
	}
	if cfg.LogFormat != "text" {
		t.Errorf("LogFormat = %q, want text", cfg.LogFormat)
	}
	if cfg.AllowNonLoopback {
		t.Error("AllowNonLoopback = true, want false")
	}
	if cfg.ShutdownGrace != 10*time.Second {
		t.Errorf("ShutdownGrace = %v, want 10s", cfg.ShutdownGrace)
	}
	if cfg.ShowVersion {
		t.Error("ShowVersion = true, want false")
	}
}

func TestParseOverrides(t *testing.T) {
	cfg, err := Parse([]string{
		"--listen", "127.0.0.1:7443",
		"--servers", "cp1:6443",
		"--balance", "least-conn",
		"--health-check-mode", "tcp",
		"--health-interval", "5s",
		"--health-timeout", "2s",
		"--fall", "3",
		"--rise", "4",
		"--dial-timeout", "1s",
		"--keepalive-period", "10s",
		"--metrics-listen", "127.0.0.1:9099",
		"--log-level", "debug",
		"--log-format", "json",
		"--allow-non-loopback",
		"--shutdown-grace", "5s",
	})
	if err != nil {
		t.Fatalf("Parse returned error: %v", err)
	}
	if cfg.Listen != "127.0.0.1:7443" {
		t.Errorf("Listen = %q", cfg.Listen)
	}
	if cfg.Balance != "least-conn" {
		t.Errorf("Balance = %q", cfg.Balance)
	}
	if cfg.HealthCheckMode != "tcp" {
		t.Errorf("HealthCheckMode = %q", cfg.HealthCheckMode)
	}
	if cfg.HealthInterval != 5*time.Second {
		t.Errorf("HealthInterval = %v", cfg.HealthInterval)
	}
	if cfg.Fall != 3 || cfg.Rise != 4 {
		t.Errorf("Fall = %d, Rise = %d", cfg.Fall, cfg.Rise)
	}
	if cfg.MetricsListen != "127.0.0.1:9099" {
		t.Errorf("MetricsListen = %q", cfg.MetricsListen)
	}
	if cfg.LogLevel != "debug" || cfg.LogFormat != "json" {
		t.Errorf("LogLevel = %q, LogFormat = %q", cfg.LogLevel, cfg.LogFormat)
	}
	if !cfg.AllowNonLoopback {
		t.Error("AllowNonLoopback = false, want true")
	}
	if cfg.ShutdownGrace != 5*time.Second {
		t.Errorf("ShutdownGrace = %v", cfg.ShutdownGrace)
	}
}

func TestParseServersTrimsWhitespace(t *testing.T) {
	cfg, err := Parse([]string{"--servers", " 10.0.0.1:6443 , 10.0.0.2:6443 "})
	if err != nil {
		t.Fatalf("Parse returned error: %v", err)
	}
	if cfg.Servers[0] != "10.0.0.1:6443" || cfg.Servers[1] != "10.0.0.2:6443" {
		t.Errorf("Servers = %v, want trimmed entries", cfg.Servers)
	}
}

func TestParseUnknownFlag(t *testing.T) {
	_, err := Parse([]string{"--servers", "a:1", "--no-such-flag"})
	if err == nil {
		t.Fatal("Parse accepted unknown flag, want error")
	}
}

func TestParseBadDuration(t *testing.T) {
	_, err := Parse([]string{"--servers", "a:1", "--health-interval", "banana"})
	if err == nil {
		t.Fatal("Parse accepted invalid duration, want error")
	}
}

func TestParseVersionFlag(t *testing.T) {
	cfg, err := Parse([]string{"--version"})
	if err != nil {
		t.Fatalf("Parse returned error: %v", err)
	}
	if !cfg.ShowVersion {
		t.Error("ShowVersion = false, want true")
	}
}

func TestParseExplicitTracking(t *testing.T) {
	cfg, err := Parse([]string{"--servers", "a:1", "--fall", "5"})
	if err != nil {
		t.Fatalf("Parse returned error: %v", err)
	}
	if !cfg.Explicit("fall") {
		t.Error("Explicit(fall) = false, want true")
	}
	if cfg.Explicit("rise") {
		t.Error("Explicit(rise) = true, want false")
	}
}

func TestParseErrorMentionsFlag(t *testing.T) {
	_, err := Parse([]string{"--servers", "a:1", "--fall", "x"})
	if err == nil {
		t.Fatal("want error")
	}
	if !strings.Contains(err.Error(), "fall") {
		t.Errorf("error %q does not mention the offending flag", err)
	}
}

func TestParseDiscoveryDefaults(t *testing.T) {
	cfg, err := Parse([]string{"--servers", "a:1"})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.DiscoveryKubeconfig != "" {
		t.Errorf("DiscoveryKubeconfig = %q, want empty (disabled)", cfg.DiscoveryKubeconfig)
	}
	if cfg.DiscoveryInterval != 30*time.Second {
		t.Errorf("DiscoveryInterval = %v, want 30s", cfg.DiscoveryInterval)
	}
}

func TestParseDiscoveryFlags(t *testing.T) {
	cfg, err := Parse([]string{
		"--servers", "a:1",
		"--discovery-kubeconfig", "/etc/kubernetes/kubelet.conf",
		"--discovery-interval", "10s",
	})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.DiscoveryKubeconfig != "/etc/kubernetes/kubelet.conf" {
		t.Errorf("DiscoveryKubeconfig = %q", cfg.DiscoveryKubeconfig)
	}
	if cfg.DiscoveryInterval != 10*time.Second {
		t.Errorf("DiscoveryInterval = %v", cfg.DiscoveryInterval)
	}
}

func TestValidateDiscoveryInterval(t *testing.T) {
	cfg, err := Parse([]string{"--servers", "10.0.0.1:6443", "--discovery-interval", "0s"})
	if err != nil {
		t.Fatal(err)
	}
	if err := cfg.Validate(nil); err == nil {
		t.Error("discovery-interval=0 accepted, want error")
	}
}

func TestDiscoveryKubeconfigMayNotExistYet(t *testing.T) {
	// Unlike --ca-file, the discovery kubeconfig is allowed to be
	// missing at startup: pre-join workers point it at kubelet.conf,
	// which only appears after kubeadm join.
	cfg, err := Parse([]string{
		"--servers", "10.0.0.1:6443",
		"--discovery-kubeconfig", "/nonexistent/kubelet.conf",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := cfg.Validate(nil); err != nil {
		t.Errorf("missing discovery kubeconfig rejected at startup: %v", err)
	}
}

func TestParseStateFile(t *testing.T) {
	cfg, err := Parse([]string{
		"--servers", "a:1",
		"--discovery-kubeconfig", "/etc/kubernetes/kubelet.conf",
		"--state-file", "/var/lib/apiserver-lb/servers.json",
	})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.StateFile != "/var/lib/apiserver-lb/servers.json" {
		t.Errorf("StateFile = %q", cfg.StateFile)
	}
}

func TestValidateStateFileRequiresDiscovery(t *testing.T) {
	cfg, err := Parse([]string{
		"--servers", "10.0.0.1:6443",
		"--state-file", "/var/lib/apiserver-lb/servers.json",
	})
	if err != nil {
		t.Fatal(err)
	}
	verr := cfg.Validate(nil)
	if verr == nil {
		t.Fatal("--state-file without --discovery-kubeconfig accepted, want error")
	}
	if !strings.Contains(verr.Error(), "discovery") {
		t.Errorf("error %q does not explain the discovery requirement", verr)
	}
}

func TestConfigFileFlagRemoved(t *testing.T) {
	// The YAML config file (and its SIGHUP reload) was removed in
	// v0.6.0: dynamic discovery plus the state file replaced its only
	// real use case. The flag must now be rejected as unknown so that
	// stale deployments fail loudly instead of silently ignoring their
	// config file — even when the file exists and is valid.
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte("servers:\n  - 10.0.0.9:6443\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := Parse([]string{"--servers", "a:1", "--config", path})
	if err == nil {
		t.Fatal("--config accepted, want unknown-flag error")
	}
}
