package version

import (
	"strings"
	"testing"
)

func TestStringContainsDefaults(t *testing.T) {
	s := String()
	for _, want := range []string{"git-bridge", Version, GitCommit, BuildDate} {
		if !strings.Contains(s, want) {
			t.Errorf("String() = %q, want substring %q", s, want)
		}
	}
}

func TestDefaults(t *testing.T) {
	if Version == "" {
		t.Error("Version must have a default value")
	}
	if GitCommit == "" {
		t.Error("GitCommit must have a default value")
	}
	if BuildDate == "" {
		t.Error("BuildDate must have a default value")
	}
}
