package buildinfo

import "testing"

func TestInfoHonorsInjectedVersion(t *testing.T) {
	oldVersion, oldCommit, oldDate := Version, Commit, Date
	t.Cleanup(func() { Version, Commit, Date = oldVersion, oldCommit, oldDate })
	Version, Commit, Date = "v1.2.3", "abc123", "2026-08-31"

	version, commit, date := Info()
	if version != Version || commit != Commit || date != Date {
		t.Fatalf("Info() = (%q, %q, %q)", version, commit, date)
	}
}
