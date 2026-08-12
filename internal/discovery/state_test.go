package discovery

import (
	"os"
	"path/filepath"
	"testing"
)

func TestStateRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "servers.json")
	want := []string{"10.0.0.1:6443", "10.0.0.2:6443"}

	if err := saveState(path, want); err != nil {
		t.Fatalf("saveState returned error: %v", err)
	}
	got, err := LoadState(path)
	if err != nil {
		t.Fatalf("LoadState returned error: %v", err)
	}
	if len(got) != 2 || got[0] != want[0] || got[1] != want[1] {
		t.Errorf("LoadState = %v, want %v", got, want)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Errorf("state file mode = %v, want 0600", info.Mode().Perm())
	}
}

func TestStateSaveOverwrites(t *testing.T) {
	path := filepath.Join(t.TempDir(), "servers.json")
	if err := saveState(path, []string{"a:1"}); err != nil {
		t.Fatal(err)
	}
	if err := saveState(path, []string{"b:2", "c:3"}); err != nil {
		t.Fatal(err)
	}
	got, err := LoadState(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0] != "b:2" {
		t.Errorf("LoadState after overwrite = %v, want [b:2 c:3]", got)
	}
}

func TestStateLoadMissingFile(t *testing.T) {
	_, err := LoadState(filepath.Join(t.TempDir(), "missing.json"))
	if err == nil {
		t.Fatal("LoadState accepted a missing file, want error")
	}
	if !os.IsNotExist(err) {
		t.Errorf("error %v does not satisfy os.IsNotExist; callers distinguish first boot from corruption", err)
	}
}

func TestStateLoadCorruptFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "servers.json")
	if err := os.WriteFile(path, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadState(path); err == nil {
		t.Error("LoadState accepted corrupt JSON, want error")
	}
}

func TestStateLoadEmptyList(t *testing.T) {
	path := filepath.Join(t.TempDir(), "servers.json")
	if err := os.WriteFile(path, []byte(`{"servers":[]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadState(path); err == nil {
		t.Error("LoadState accepted an empty server list, want error")
	}
}
