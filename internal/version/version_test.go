package version

import (
	"strings"
	"testing"
)

func TestStringContainsAllFields(t *testing.T) {
	old := [3]string{Version, Commit, BuildDate}
	defer func() { Version, Commit, BuildDate = old[0], old[1], old[2] }()

	Version, Commit, BuildDate = "v1.2.3", "abc1234", "2026-08-10T00:00:00Z"
	s := String()
	for _, want := range []string{"v1.2.3", "abc1234", "2026-08-10T00:00:00Z"} {
		if !strings.Contains(s, want) {
			t.Errorf("String() = %q, missing %q", s, want)
		}
	}
}
