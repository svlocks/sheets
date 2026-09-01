package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/svlocks/sheets/skills"
)

func TestSkillPrintsEmbeddedDocument(t *testing.T) {
	stdout, _, err := runCommand(t, "", "skill")
	if err != nil {
		t.Fatal(err)
	}
	if stdout != string(skills.SheetsSkill) {
		t.Fatalf("skill output differs from embedded document (%d vs %d bytes)", len(stdout), len(skills.SheetsSkill))
	}
	if !strings.HasPrefix(stdout, "---\nname: sheets\n") {
		t.Fatalf("skill output missing frontmatter, starts %q", stdout[:40])
	}
}

func TestSkillInstallWritesDiscoveryDirectories(t *testing.T) {
	base := t.TempDir()
	stdout, _, err := runCommand(t, "", "-C", base, "skill", "install")
	if err != nil {
		t.Fatal(err)
	}
	for _, dir := range []string{".claude", ".agents"} {
		path := filepath.Join(base, dir, "skills", "sheets", "SKILL.md")
		content, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("%s: %v", path, err)
		}
		if string(content) != string(skills.SheetsSkill) {
			t.Fatalf("%s differs from embedded document", path)
		}
		if !strings.Contains(stdout, path) {
			t.Fatalf("install output %q does not mention %s", stdout, path)
		}
	}
	// Reinstalling overwrites in place.
	if _, _, err := runCommand(t, "", "-C", base, "skill", "install"); err != nil {
		t.Fatal(err)
	}
}

func TestSkillInstallCustomDir(t *testing.T) {
	base := t.TempDir()
	custom := filepath.Join(base, "my-skills")
	if _, _, err := runCommand(t, "", "skill", "install", "--dir", custom); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(custom, "sheets", "SKILL.md")); err != nil {
		t.Fatal(err)
	}
}

func TestSkillInstallGlobalAndDirExclusive(t *testing.T) {
	if _, _, err := runCommand(t, "", "skill", "install", "--global", "--dir", t.TempDir()); err == nil {
		t.Fatal("expected mutually exclusive flag error")
	}
}
