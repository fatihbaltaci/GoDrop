// Package cli assembles GoDrop's command line: serving, guided setup,
// diagnosis, token management and the telemetry switch.
//
// Every command supports --json, so an agent can drive GoDrop as reliably as a
// human does. Colour and interactive prompts turn themselves off whenever the
// output is not a terminal.
package cli

import (
	"context"
	"fmt"
	"io"
	"os"

	"github.com/spf13/cobra"
)

// Build describes the running binary. The values are set with -ldflags at
// release time; a build from source leaves PostHogKey empty, which disables
// telemetry entirely.
type Build struct {
	Version     string
	Commit      string
	Date        string
	PostHogKey  string
	PostHogHost string
}

// Execute runs the command line and returns the process exit code.
func Execute(ctx context.Context, build Build, args []string) int {
	return ExecuteWith(ctx, build, args, os.Stdout, os.Stderr)
}

// ExecuteWith is Execute with explicit streams, so the command line can be
// driven and inspected from tests exactly as a user would drive it.
func ExecuteWith(ctx context.Context, build Build, args []string, stdout, stderr io.Writer) int {
	root := newRootCmd(build)
	root.SetArgs(args)
	root.SetOut(stdout)
	root.SetErr(stderr)
	if err := root.ExecuteContext(ctx); err != nil {
		if !isSilent(err) {
			fmt.Fprintln(stderr, errorPrefix()+err.Error())
		}
		return 1
	}
	return 0
}

type silentError struct{ error }

func isSilent(err error) bool {
	_, ok := err.(silentError)
	return ok
}

func newRootCmd(build Build) *cobra.Command {
	var jsonOut bool

	cmd := &cobra.Command{
		Use:   "godrop",
		Short: "Upload a file, get a hard-to-guess URL",
		Long: `GoDrop is a tiny self-hosted file host.

Running it without a subcommand starts the server, so a container image needs
no command of its own.`,
		SilenceUsage:  true,
		SilenceErrors: true,
		Args:          cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runServe(cmd, build)
		},
	}
	cmd.PersistentFlags().BoolVar(&jsonOut, "json", false, "machine-readable output")
	cmd.PersistentFlags().Bool("no-color", false, "disable coloured output")

	cmd.AddCommand(
		newServeCmd(build),
		newInitCmd(build),
		newDoctorCmd(build),
		newTokenCmd(build),
		newTelemetryCmd(build),
		newSkillCmd(build),
		newUpdateCmd(build),
		newUninstallCmd(build),
		newHealthCmd(),
		newVersionCmd(build),
	)
	return cmd
}

func newVersionCmd(build Build) *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print version information",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			out := newOutput(cmd)
			if out.json {
				return out.emit(map[string]string{
					"version": build.Version,
					"commit":  build.Commit,
					"date":    build.Date,
				})
			}
			fmt.Fprintf(cmd.OutOrStdout(), "godrop %s (%s, built %s)\n", build.Version, build.Commit, build.Date)
			return nil
		},
	}
}
