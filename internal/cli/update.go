package cli

import (
	"context"
	"errors"
	"time"

	"github.com/spf13/cobra"

	"github.com/fatihbaltaci/GoDrop/internal/updater"
)

// The update command reaches out to GitHub and rewrites a file on disk, which
// the suite must not do for real. These are the seams it goes through; the
// updater package tests the behaviour behind them.
var (
	updateLatest = updater.Latest
	updateRun    = updater.Update
)

func newUpdateCmd(build Build) *cobra.Command {
	var (
		check   bool
		version string
	)
	cmd := &cobra.Command{
		Use:   "update",
		Short: "Update GoDrop to the latest release",
		Long: `Download the newest release and put it in place of this binary.

Nothing is replaced until the download has been checked against the published
SHA256SUMS and the new binary has been run and seen to report its own version,
so a failed update leaves the working installation exactly as it was. The file
is then swapped with a rename, which is atomic: a server that is already
running keeps serving from the binary it started with, and restarts into the
new one.

Installations owned by something else are refused rather than overwritten. Use
apt, dnf or brew for those, and pull a new image for a container.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx, cancel := context.WithTimeout(cmd.Context(), 5*time.Minute)
			defer cancel()

			out := newOutput(cmd)
			opts := updater.Options{Version: version}

			if check {
				latest, err := updateLatest(ctx, opts)
				if err != nil {
					return err
				}
				same := updater.SameVersion(build.Version, latest)
				if out.json {
					return out.emit(map[string]any{
						"current": build.Version, "latest": latest, "up_to_date": same,
					})
				}
				if same {
					out.success("godrop %s is the latest release", build.Version)
					return nil
				}
				out.warn("godrop %s is available (running %s)", latest, build.Version)
				out.hint("install it with: godrop update")
				return nil
			}

			res, err := updateRun(ctx, build.Version, opts)
			if err != nil {
				if errors.Is(err, updater.ErrManaged) && out.json {
					return out.emit(res)
				}
				return err
			}
			if out.json {
				return out.emit(res)
			}
			if res.UpToDate {
				out.success("already on %s, the latest release", build.Version)
				return nil
			}
			out.success("updated %s to %s", build.Version, res.To)
			out.printf("\n  %s\n", res.Path)
			out.hint("restart the service to run it: systemctl restart godrop")
			return nil
		},
	}
	cmd.Flags().BoolVar(&check, "check", false, "report whether a newer release exists, without installing it")
	cmd.Flags().StringVar(&version, "version", "", "install this release instead of the newest, e.g. v1.2.0")
	return cmd
}
