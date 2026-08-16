package cli

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/spf13/cobra"

	"github.com/fatihbaltaci/GoDrop/internal/updater"
	"github.com/fatihbaltaci/GoDrop/internal/wizard"
)

func newUninstallCmd(build Build) *cobra.Command {
	var (
		purge bool
		yes   bool
	)
	cmd := &cobra.Command{
		Use:   "uninstall",
		Short: "Remove GoDrop from this machine",
		Long: `Removes what "godrop init" put on this machine: the generated configuration,
and with --purge the uploaded files as well.

Uploaded files are kept unless you ask for them to go, because deleting
somebody's files is not something a command should do as a side effect. What
will be removed is listed first, and nothing happens until you agree.

An installation that belongs to a package manager is left alone, with the
command that removes it properly.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			out := newOutput(cmd)
			return runUninstall(cmd.Context(), out, build, purge, yes)
		},
	}
	cmd.Flags().BoolVar(&purge, "purge", false, "delete the uploaded files and the token file too")
	cmd.Flags().BoolVarP(&yes, "yes", "y", false, "do not ask for confirmation")
	return cmd
}

// removal is one thing uninstall would delete.
type removal struct {
	Path string `json:"path"`
	What string `json:"what"`
}

func runUninstall(ctx context.Context, out *output, build Build, purge, yes bool) error {
	// A package manager owns its files; removing them from underneath it
	// leaves a system nobody can reason about.
	if manager, ok := updater.ManagedBy(); ok {
		return fmt.Errorf("this GoDrop was installed with %s; remove it the same way:\n\n  %s",
			manager.Name, manager.Remove)
	}

	items := plannedRemovals(purge)
	if len(items) == 0 {
		out.success("nothing to remove")
		return nil
	}

	if out.json {
		return out.emit(map[string]any{"removals": items, "version": build.Version})
	}

	out.heading("This will remove")
	for _, item := range items {
		out.skip("%-52s %s", item.Path, item.What)
	}
	if !purge {
		out.printf("\n")
		out.hint("uploaded files are kept; add --purge to delete them as well")
	}

	if !yes {
		ok, err := newInteractivePrompter(out).Confirm("Remove these?", "", false)
		if err != nil {
			return err
		}
		if !ok {
			out.printf("\n  Nothing was removed.\n")
			return nil
		}
	}

	out.heading("Removing")
	for _, item := range items {
		if err := os.RemoveAll(item.Path); err != nil {
			out.fail("%s: %v", item.Path, err)
			continue
		}
		out.success("%s", item.Path)
	}
	out.printf("\n  Stop the service yourself if it is still running:\n")
	out.command("docker compose down    # or: sudo systemctl disable --now godrop")
	_ = ctx
	return nil
}

// plannedRemovals lists what is actually there, so the confirmation describes
// this machine rather than a general idea of an installation.
func plannedRemovals(purge bool) []removal {
	var items []removal
	add := func(path, what string) {
		if path == "" {
			return
		}
		// The paths come from this package's own idea of where things are, not
		// from a request or an argument.
		if _, err := os.Stat(path); err != nil { //nolint:gosec // G703
			return
		}
		for _, existing := range items {
			if existing.Path == path {
				return
			}
		}
		items = append(items, removal{Path: path, What: what})
	}

	configDir := wizard.ConfigDir(runtime.GOOS, os.Getenv, os.Geteuid() == 0)
	add(configDir, "generated configuration")
	// A docker-compose.yml in the working directory may well be somebody
	// else's, and deleting a file this program did not write is not something
	// an uninstall gets to do. Every generated file says so on its first line,
	// so that is what is checked.
	for _, name := range []string{".env", "docker-compose.yml", "godrop.service", "Caddyfile", wizard.SampleName} {
		path := filepath.Join(".", name)
		if writtenByGoDrop(path) {
			add(path, "written by godrop init")
		}
	}
	if purge {
		add(wizard.DefaultDataDir(runtime.GOOS, os.Getenv, os.Geteuid() == 0), "uploaded files and tokens")
		add(os.Getenv("GODROP_DATA_DIR"), "uploaded files and tokens")
	}
	if self, err := osExecutable(); err == nil {
		if resolved, err := filepath.EvalSymlinks(self); err == nil {
			self = resolved
		}
		add(self, "the godrop binary")
	}
	return items
}

// writtenByGoDrop reports whether a file carries the line every generated file
// starts with. Anything else in the directory belongs to somebody else.
func writtenByGoDrop(path string) bool {
	data, err := os.ReadFile(path) //nolint:gosec // G304, the paths are this package's own
	if err != nil {
		return false
	}
	// A picture cannot carry a comment line, so the sample is recognised by
	// being, byte for byte, the picture setup draws. A binary from a different
	// release might draw it a shade differently, and then this leaves the file
	// alone, which is the right way round to be wrong.
	if filepath.Base(path) == wizard.SampleName {
		return string(data) == wizard.SampleImage()
	}
	first, _, _ := strings.Cut(string(data), "\n")
	return strings.Contains(first, "generated by `godrop init`")
}
