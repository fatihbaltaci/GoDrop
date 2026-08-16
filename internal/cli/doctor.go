package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/fatihbaltaci/GoDrop/internal/config"
	"github.com/fatihbaltaci/GoDrop/internal/doctor"
	"github.com/fatihbaltaci/GoDrop/internal/tokens"
	"github.com/fatihbaltaci/GoDrop/internal/wizard"
)

// withEnvFile answers from the environment first and the generated .env
// second: what the operator exported for this command wins over what setup
// wrote weeks ago.
func withEnvFile(base func(string) string, values map[string]string) func(string) string {
	return func(key string) string {
		if v := base(key); v != "" {
			return v
		}
		return values[key]
	}
}

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

			out := newOutput(cmd)
			// A token on the command line ends up in the process list and in
			// shell history, where anyone with a local account can read it, so
			// the environment is the documented way to pass one.
			if token == "" {
				token = os.Getenv("GODROP_TOKEN")
			}

			// A shell is not the service's environment: setup wrote a .env
			// that this prompt has never sourced, and without it the diagnosis
			// is of a machine with GoDrop configured nowhere, which is nobody's
			// question. --url means the operator has already said what to look
			// at, and is left alone.
			env := os.Getenv
			inContainer := ""
			if url == "" {
				if dir := installationDir(); installedAt(dir) {
					a := answersFromEnv(dir)
					if deploymentAt(dir) == wizard.DeployCompose {
						// The files are in a volume only the container can
						// reach, so from out here the honest view is the one
						// over HTTP; the rest is one command away, printed
						// with the report.
						url = wizard.PublicAddress(a)
						if token == "" {
							token = a.Token
						}
						// exec, not run: inside the running container the
						// port and the volume are the service's own, so the
						// report comes back complete rather than half red.
						inContainer = dir
						out.skip("the installation in %s runs in a container", dir)
					} else {
						env = withEnvFile(env, readEnvFile(filepath.Join(dir, ".env")))
						out.skip("using the configuration in %s", filepath.Join(dir, ".env"))
					}
				}
			}

			// Diagnosing something over HTTP means this machine's own
			// configuration is not the subject: its data directory, its port
			// and its tokens belong to a different installation, and reporting
			// on them here is answering a question nobody asked.
			var (
				cfg    *config.Config
				cfgErr error
			)
			if url == "" {
				cfg, cfgErr = config.LoadFrom(env)
			}
			opts := doctor.Options{
				Config:    cfg,
				Env:       env,
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
			// The storage, the permissions and the token file are inside the
			// container, so the container is asked about them and the two
			// answers become one report: it is one installation.
			if inContainer != "" {
				inside, err := containerReport(ctx, inContainer)
				if err != nil {
					out.warn("could not diagnose inside the container: %v", err)
				} else {
					report = mergeReports(inside, report)
				}
			}
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

// containerReport is the service's own diagnosis of the parts of itself that
// only it can see: its data directory, its permissions and its token file.
func containerReport(ctx context.Context, project string) (doctor.Report, error) {
	var report doctor.Report
	raw, err := composeRun(ctx, project, "doctor", "--offline", "--json")
	if err != nil {
		return report, err
	}
	if err := json.Unmarshal(raw, &report); err != nil {
		return report, fmt.Errorf("could not read the answer from the container: %w", err)
	}
	return report, nil
}

// mergeReports takes each group from whichever side can answer for it. The
// container knows about its own files; only this machine can say whether the
// internet reaches the port, and what the newest release is.
func mergeReports(inside, outside doctor.Report) doctor.Report {
	// The round trip belongs out here too: what matters is that the published
	// port answers, which is the question a container asking itself cannot
	// tell apart from its own loopback.
	fromOutside := map[string]bool{"network": true, "version": true, "end_to_end": true}
	merged := doctor.Report{Version: outside.Version}
	for _, c := range inside.Checks {
		if !fromOutside[c.Group] {
			merged.Checks = append(merged.Checks, c)
		}
	}
	for _, c := range outside.Checks {
		if fromOutside[c.Group] {
			merged.Checks = append(merged.Checks, c)
		}
	}
	merged.OK = !merged.Failed()
	return merged
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
	out.fail("%d check(s) failed. Fix the items marked above and run `godrop doctor` again", countFailed(report))
}
