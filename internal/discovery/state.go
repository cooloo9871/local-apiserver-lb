package discovery

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// stateFile is the on-disk format of the persisted backend list. It
// records the last server list discovery successfully applied, so a
// restarted balancer resumes from the cluster's current membership even
// if the static --servers seed has gone stale.
type stateFile struct {
	Servers []string `json:"servers"`
}

// LoadState reads a persisted server list. A missing file satisfies
// os.IsNotExist so callers can distinguish first boot from corruption.
func LoadState(path string) ([]string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var st stateFile
	if err := json.Unmarshal(data, &st); err != nil {
		return nil, fmt.Errorf("parsing state file %s: %w", path, err)
	}
	if len(st.Servers) == 0 {
		return nil, fmt.Errorf("state file %s contains no servers", path)
	}
	return st.Servers, nil
}

// saveState atomically writes the server list: temp file in the same
// directory, fsync-free rename. Mode 0600 — the content is not secret
// (apiserver addresses), but there is no reason to widen it.
func saveState(path string, servers []string) error {
	data, err := json.Marshal(stateFile{Servers: servers})
	if err != nil {
		return err
	}

	tmp, err := os.CreateTemp(filepath.Dir(path), ".servers-*.tmp")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name()) // no-op after successful rename

	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}
