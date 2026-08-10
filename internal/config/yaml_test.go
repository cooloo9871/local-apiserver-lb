package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func writeTemp(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestParseYAMLScalarsAndBlockList(t *testing.T) {
	doc, err := parseYAMLSubset([]byte(`
# comment line
listen: "127.0.0.1:7443"
balance: least-conn   # trailing comment
fall: 3
servers:
  - 10.0.0.1:6443
  - "10.0.0.2:6443"
`))
	if err != nil {
		t.Fatalf("parseYAMLSubset returned error: %v", err)
	}
	if doc.scalars["listen"] != "127.0.0.1:7443" {
		t.Errorf("listen = %q", doc.scalars["listen"])
	}
	if doc.scalars["balance"] != "least-conn" {
		t.Errorf("balance = %q", doc.scalars["balance"])
	}
	if doc.scalars["fall"] != "3" {
		t.Errorf("fall = %q", doc.scalars["fall"])
	}
	want := []string{"10.0.0.1:6443", "10.0.0.2:6443"}
	got := doc.lists["servers"]
	if len(got) != 2 || got[0] != want[0] || got[1] != want[1] {
		t.Errorf("servers = %v, want %v", got, want)
	}
}

func TestParseYAMLFlowList(t *testing.T) {
	doc, err := parseYAMLSubset([]byte(`servers: [10.0.0.1:6443, "10.0.0.2:6443"]`))
	if err != nil {
		t.Fatalf("parseYAMLSubset returned error: %v", err)
	}
	got := doc.lists["servers"]
	if len(got) != 2 || got[0] != "10.0.0.1:6443" || got[1] != "10.0.0.2:6443" {
		t.Errorf("servers = %v", got)
	}
}

func TestParseYAMLRejectsNesting(t *testing.T) {
	_, err := parseYAMLSubset([]byte("outer:\n  inner: value\n"))
	if err == nil {
		t.Fatal("nested mapping accepted, want error")
	}
}

func TestParseYAMLRejectsDuplicateKey(t *testing.T) {
	_, err := parseYAMLSubset([]byte("listen: a\nlisten: b\n"))
	if err == nil {
		t.Fatal("duplicate key accepted, want error")
	}
}

func TestParseYAMLRejectsTabs(t *testing.T) {
	_, err := parseYAMLSubset([]byte("servers:\n\t- 10.0.0.1:6443\n"))
	if err == nil {
		t.Fatal("tab indentation accepted, want error")
	}
}

func TestConfigFileProvidesValues(t *testing.T) {
	path := writeTemp(t, `
listen: 127.0.0.1:7443
servers:
  - 10.0.0.1:6443
  - 10.0.0.2:6443
fall: 5
health-interval: 7s
insecure-skip-verify: false
`)
	cfg, err := Parse([]string{"--config", path})
	if err != nil {
		t.Fatalf("Parse returned error: %v", err)
	}
	if cfg.Listen != "127.0.0.1:7443" {
		t.Errorf("Listen = %q", cfg.Listen)
	}
	if len(cfg.Servers) != 2 {
		t.Errorf("Servers = %v", cfg.Servers)
	}
	if cfg.Fall != 5 {
		t.Errorf("Fall = %d", cfg.Fall)
	}
	if cfg.HealthInterval != 7*time.Second {
		t.Errorf("HealthInterval = %v", cfg.HealthInterval)
	}
	if cfg.InsecureSkipVerify {
		t.Error("InsecureSkipVerify = true, want false from file")
	}
}

func TestExplicitFlagBeatsConfigFile(t *testing.T) {
	path := writeTemp(t, "listen: 127.0.0.1:7443\nservers: [10.0.0.1:6443]\nfall: 5\n")
	cfg, err := Parse([]string{"--config", path, "--fall", "9"})
	if err != nil {
		t.Fatalf("Parse returned error: %v", err)
	}
	if cfg.Fall != 9 {
		t.Errorf("Fall = %d, want explicit flag value 9", cfg.Fall)
	}
	if cfg.Listen != "127.0.0.1:7443" {
		t.Errorf("Listen = %q, want file value", cfg.Listen)
	}
}

func TestConfigFileUnknownKey(t *testing.T) {
	path := writeTemp(t, "servers: [a:1]\nno-such-key: true\n")
	_, err := Parse([]string{"--config", path})
	if err == nil {
		t.Fatal("unknown key accepted, want error")
	}
	if !strings.Contains(err.Error(), "no-such-key") {
		t.Errorf("error %q does not name the unknown key", err)
	}
}

func TestConfigFileBadValue(t *testing.T) {
	path := writeTemp(t, "servers: [a:1]\nfall: banana\n")
	_, err := Parse([]string{"--config", path})
	if err == nil {
		t.Fatal("bad value accepted, want error")
	}
}

func TestConfigFileMissing(t *testing.T) {
	_, err := Parse([]string{"--config", "/nonexistent/config.yaml"})
	if err == nil {
		t.Fatal("missing config file accepted, want error")
	}
}

func TestConfigFileServersFlagStillWins(t *testing.T) {
	path := writeTemp(t, "servers: [10.0.0.1:6443]\n")
	cfg, err := Parse([]string{"--config", path, "--servers", "10.9.9.9:6443"})
	if err != nil {
		t.Fatalf("Parse returned error: %v", err)
	}
	if len(cfg.Servers) != 1 || cfg.Servers[0] != "10.9.9.9:6443" {
		t.Errorf("Servers = %v, want explicit flag value", cfg.Servers)
	}
}

func TestLoadServersFromFile(t *testing.T) {
	path := writeTemp(t, "servers:\n  - 10.0.0.1:6443\n  - 10.0.0.3:6443\n")
	servers, err := LoadServersFromFile(path)
	if err != nil {
		t.Fatalf("LoadServersFromFile returned error: %v", err)
	}
	if len(servers) != 2 || servers[1] != "10.0.0.3:6443" {
		t.Errorf("servers = %v", servers)
	}
}

func TestLoadServersFromFileEmpty(t *testing.T) {
	path := writeTemp(t, "listen: 127.0.0.1:6443\n")
	_, err := LoadServersFromFile(path)
	if err == nil {
		t.Fatal("file without servers accepted, want error")
	}
}
