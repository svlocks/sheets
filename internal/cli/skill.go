package cli

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/svlocks/sheets/skills"
	"github.com/spf13/cobra"
)

// Agent-skill discovery directories. `.claude/skills` is read by Claude Code;
// `.agents/skills` is the shared directory read by Codex, Cursor, Copilot,
// Gemini CLI, oh-my-pi, Crush, Cline, OpenCode, Warp, and other Agent Skills
// adopters.
var skillDirs = []string{
	filepath.Join(".claude", "skills"),
	filepath.Join(".agents", "skills"),
}

func (e *commandEnvironment) skillCommand() *cobra.Command {
	command := &cobra.Command{
		Use:   "skill",
		Short: "Print the sheets agent skill (SKILL.md)",
		Long: "Print the embedded Agent Skills document that teaches coding agents how\n" +
			"to use sheets. Use `sheets skill install` to write it where agents\n" +
			"discover skills.",
		Args: cobra.NoArgs,
		RunE: func(command *cobra.Command, _ []string) error {
			_, err := command.OutOrStdout().Write(skills.SheetsSkill)
			return err
		},
	}
	command.AddCommand(e.skillInstallCommand())
	return command
}

func (e *commandEnvironment) skillInstallCommand() *cobra.Command {
	var global bool
	var custom string
	command := &cobra.Command{
		Use:   "install",
		Short: "Install the sheets agent skill for coding agents",
		Long: "Write the embedded skill to <scope>/.claude/skills/sheets/SKILL.md and\n" +
			"<scope>/.agents/skills/sheets/SKILL.md, the directories scanned by Claude\n" +
			"Code and by other Agent Skills adopters (Codex, Cursor, Copilot, Gemini\n" +
			"CLI, oh-my-pi, Crush, Cline, OpenCode, Warp, ...). The scope is the\n" +
			"-C/--directory path (default: the current directory), or the home\n" +
			"directory with --global. Existing files are overwritten so reinstalling\n" +
			"upgrades in place.",
		Args: cobra.NoArgs,
		RunE: func(command *cobra.Command, _ []string) error {
			var targets []string
			switch {
			case custom != "":
				targets = []string{custom}
			case global:
				home, err := os.UserHomeDir()
				if err != nil {
					return err
				}
				for _, dir := range skillDirs {
					targets = append(targets, filepath.Join(home, dir))
				}
			default:
				for _, dir := range skillDirs {
					targets = append(targets, filepath.Join(e.start, dir))
				}
			}
			for _, target := range targets {
				path := filepath.Join(target, "sheets", "SKILL.md")
				if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
					return err
				}
				if err := os.WriteFile(path, skills.SheetsSkill, 0o644); err != nil {
					return err
				}
				if _, err := fmt.Fprintf(command.OutOrStdout(), "Installed %s\n", path); err != nil {
					return err
				}
			}
			return nil
		},
	}
	command.Flags().BoolVar(&global, "global", false, "install into the home directory instead of the project")
	command.Flags().StringVar(&custom, "dir", "", "install into `path`/sheets/SKILL.md instead of the standard directories")
	command.MarkFlagsMutuallyExclusive("global", "dir")
	return command
}
