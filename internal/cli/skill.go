package cli

import (
	_ "embed"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"
)

// The skill ships inside the binary, so `godrop skill install` works offline,
// on a machine with no git, and always installs the instructions that match
// the version being run. The repository keeps it at skills/godrop/SKILL.md,
// which is where `gh skill install` looks.
//
//go:embed skill/SKILL.md
var skillDoc string

// SkillName is the directory the skill is installed as.
const SkillName = "godrop"

// Agent directories.
//
// Most coding agents read a shared directory; Claude Code has its own. These
// two cover the common cases without inventing paths for the rest, and --dir
// handles anything else. `gh skill install fatihbaltaci/GoDrop godrop --agent
// <name>` knows the other fifty.
var agentDirs = map[string]struct{ project, user string }{
	"shared": {filepath.Join(".agents", "skills"), filepath.Join(".agents", "skills")},
	"claude": {filepath.Join(".claude", "skills"), filepath.Join(".claude", "skills")},
}

func newSkillCmd(build Build) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "skill",
		Short: "Install the agent skill that teaches a coding agent to use GoDrop",
		Long: `Agent skills are folders holding a SKILL.md that tells a coding agent how
to do something. GoDrop ships one, so an agent that has never seen it can
upload a file and hand back a link without being told how.

The skill needs no secrets: it reads GODROP_URL and GODROP_TOKEN from the
environment, so the same file is safe to commit alongside a project.

It can also be installed with the GitHub CLI, which supports every agent:

  gh skill install fatihbaltaci/GoDrop godrop --scope user`,
	}
	cmd.AddCommand(newSkillInstallCmd(build), newSkillShowCmd())
	return cmd
}

func newSkillInstallCmd(build Build) *cobra.Command {
	var (
		scope string
		agent string
		dir   string
		force bool
	)
	cmd := &cobra.Command{
		Use:   "install",
		Short: "Write the skill into an agent's skill directory",
		Long: `Write the skill into an agent's skill directory.

Project scope (the default) installs into the working directory, so the skill
travels with the repository. User scope installs into your home directory,
where every project can see it.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			target, err := skillDir(agent, scope, dir)
			if err != nil {
				return err
			}
			path := filepath.Join(target, SkillName, "SKILL.md")

			if _, err := os.Stat(path); err == nil && !force {
				return fmt.Errorf("%s already exists (pass --force to replace it)", path)
			} else if err != nil && !errors.Is(err, os.ErrNotExist) {
				return err
			}
			// Readable by whatever the agent runs as, which is not always the
			// account that installed it. A skill is documentation, not a secret.
			if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil { //nolint:gosec // G301
				return err
			}
			if err := os.WriteFile(path, []byte(skillDoc), 0o644); err != nil { //nolint:gosec // an agent must be able to read it
				return err
			}

			out := newOutput(cmd)
			if out.json {
				return out.emit(map[string]string{
					"skill":   SkillName,
					"path":    path,
					"scope":   scope,
					"agent":   agent,
					"version": build.Version,
				})
			}
			out.success("installed the %s skill to %s", SkillName, path)
			out.hint("the agent needs GODROP_URL and GODROP_TOKEN in its environment")
			return nil
		},
	}
	cmd.Flags().StringVar(&scope, "scope", "project", "project or user")
	cmd.Flags().StringVar(&agent, "agent", "shared",
		"shared (most agents) or claude; use --dir for anything else")
	cmd.Flags().StringVar(&dir, "dir", "", "install into this directory instead, overriding --agent and --scope")
	cmd.Flags().BoolVar(&force, "force", false, "replace an existing skill")
	return cmd
}

func newSkillShowCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "show",
		Short: "Print the skill, so it can be piped somewhere else",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			_, err := fmt.Fprint(cmd.OutOrStdout(), skillDoc)
			return err
		},
	}
}

// skillDir resolves where the skill should be written.
func skillDir(agent, scope, dir string) (string, error) {
	if dir != "" {
		return dir, nil
	}
	dirs, ok := agentDirs[strings.ToLower(agent)]
	if !ok {
		return "", fmt.Errorf("unknown agent %q: use shared, claude, or --dir", agent)
	}
	switch strings.ToLower(scope) {
	case "project":
		return dirs.project, nil
	case "user":
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("could not find your home directory: %w", err)
		}
		return filepath.Join(home, dirs.user), nil
	default:
		return "", fmt.Errorf("unknown scope %q: use project or user", scope)
	}
}
