package cli

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/fatihbaltaci/GoDrop/internal/config"
	"github.com/fatihbaltaci/GoDrop/internal/doctor"
	"github.com/fatihbaltaci/GoDrop/internal/tokens"
)

func newDoctorCmd(build Build) *cobra.Command {
	var (
		offline  bool
		url      string
		token    string
		checkURL string
	)
	cmd := &cobra.Command{
		Use:   "doctor",
		Short: "Diagnose the installation and print how to fix what is broken",
		Long: `Checks configuration, storage, security posture, network reachability and
available updates, then prints the exact command that fixes each problem.

Run it on the server for the full picture, or point it at a remote instance
with --url. Exits non-zero when a check fails, so it works as a deployment
gate.

The token for a remote check is read from GODROP_TOKEN, so that it stays out
of the process list and the shell history:

  GODROP_TOKEN=gd_... godrop doctor --url https://files.example.com`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx, cancel := context.WithTimeout(cmd.Context(), 90*time.Second)
			defer cancel()

			cfg, cfgErr := config.Load()
			// A token on the command line ends up in the process list and in
			// shell history, where anyone with a local account can read it, so
			// the environment is the documented way to pass one.
			if token == "" {
				token = os.Getenv("GODROP_TOKEN")
			}
			opts := doctor.Options{
				Config:    cfg,
				ConfigErr: cfgErr,
				Version:   build.Version,
				Offline:   offline,
				TargetURL: url,
				Token:     token,
				CheckURL:  checkURL,
			}
			if wd, err := os.Getwd(); err == nil {
				opts.WorkDir = wd
			}
			// Running on the server we can mint a short-lived token and exercise
			// the real upload/download/delete path, then throw it away.
			if opts.Token == "" && cfg != nil {
				if t, cleanup, err := temporaryToken(cfg); err == nil {
					opts.Token = t
					defer cleanup()
				}
			}

			report := doctor.Run(ctx, opts)
			out := newOutput(cmd)
			if out.json {
				if err := out.emit(report); err != nil {
					return err
				}
			} else {
				printReport(out, report)
			}
			if report.Failed() {
				return silentError{fmt.Errorf("%d check(s) failed", countFailed(report))}
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&offline, "offline", false, "skip every check that needs the network")
	cmd.Flags().StringVar(&url, "url", "", "diagnose a remote instance at this base URL")
	cmd.Flags().StringVar(&token, "token", "",
		"API token for the round-trip check; prefer GODROP_TOKEN, which does not appear in the process list")
	cmd.Flags().StringVar(&checkURL, "check-url", "", "reachability service (default https://godrop.sh/api/check)")
	return cmd
}

// temporaryToken creates a throwaway token so the round-trip check can exercise
// the live API without the operator having to paste a real one.
func temporaryToken(cfg *config.Config) (string, func(), error) {
	store, err := tokens.New(tokens.Path(cfg.DataDir), cfg.Tokens)
	if err != nil {
		return "", nil, err
	}
	if len(cfg.Tokens) > 0 {
		return cfg.Tokens[0], func() {}, nil
	}
	name := fmt.Sprintf("doctor-%d", time.Now().UnixNano())
	plain, _, err := store.Create(name)
	if err != nil {
		return "", nil, err
	}
	return plain, func() { _ = store.Revoke(name) }, nil
}

func countFailed(r doctor.Report) int {
	n := 0
	for _, c := range r.Checks {
		if c.Status == doctor.Fail {
			n++
		}
	}
	return n
}

func printReport(out *output, report doctor.Report) {
	groups := []string{"config", "storage", "security", "network", "end_to_end", "version"}
	titles := map[string]string{
		"config":     "Configuration",
		"storage":    "Storage",
		"security":   "Security",
		"network":    "Network",
		"end_to_end": "End to end",
		"version":    "Version",
	}
	for _, g := range groups {
		var checks []doctor.Check
		for _, c := range report.Checks {
			if c.Group == g {
				checks = append(checks, c)
			}
		}
		if len(checks) == 0 {
			continue
		}
		out.heading(titles[g])
		for _, c := range checks {
			label := strings.ReplaceAll(c.Name, "_", " ")
			line := fmt.Sprintf("%-20s %s", label, c.Detail)
			switch c.Status {
			case doctor.Pass:
				out.success("%s", line)
			case doctor.Warn:
				out.warn("%s", line)
			case doctor.Fail:
				out.fail("%s", line)
			default:
				out.skip("%s", line)
			}
			if c.Fix != "" && c.Status != doctor.Pass {
				out.hint("%s", c.Fix)
			}
		}
	}
	out.printf("\n")
	if report.OK {
		out.success("everything looks good")
		return
	}
	out.fail("%d check(s) failed — fix the items marked above and run `godrop doctor` again", countFailed(report))
}
