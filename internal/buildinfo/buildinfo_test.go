package buildinfo

import "testing"

func TestFull(t *testing.T) {
	oldVersion, oldCommit, oldDate := Version, Commit, BuildDate
	t.Cleanup(func() {
		Version, Commit, BuildDate = oldVersion, oldCommit, oldDate
	})
	Version, Commit, BuildDate = "v1.2.3", "abcdef", "2026-08-02T00:00:00Z"
	if got := Full(); got !=
		"v1.2.3 (commit=abcdef, built=2026-08-02T00:00:00Z)" {
		t.Fatalf("Full()=%q", got)
	}
}
