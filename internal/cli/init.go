package cli

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/fatihbaltaci/GoDrop/internal/doctor"
	"github.com/fatihbaltaci/GoDrop/internal/netcheck"
	"github.com/fatihbaltaci/GoDrop/internal/telemetry"
	"github.com/fatihbaltaci/GoDrop/internal/tokens"
	"github.com/fatihbaltaci/GoDrop/internal/wizard"
)

func newInitCmd(build Build) *cobra.Command {
	var (
		answers        = wizard.Defaults()
		dataDir        string
		setLimits      bool
		nonInteractive bool
		force          bool
		skipExternal   bool
		start          bool
		outDir         string
	)

	cmd := &cobra.Command{
		Use:   "init",
		Short: "Guided setup: configure, generate a token, start and verify",
		Long: `Walks through the handful of decisions GoDrop needs, writes the configuration
files, creates your first API token and, if you like, starts the service and
confirms that the outside world can actually reach it.

Every answer can be supplied as a flag instead; with --no-input the same wizard
runs without asking anything, which is what CI and agents should use. Prompts
are skipped automatically when there is no terminal.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			out := newOutput(cmd)
			answers.ExternalCheck = answers.ExternalCheck && !skipExternal
			// A limit given on the command line is the same statement as
			// choosing the advanced path in the wizard.
			if setLimits || anyLimitFlagSet(cmd) {
				answers.Limits = wizard.LimitsAdvanced
			}
			if dataDir != "" {
				answers.DataDir = dataDir
			}

			if outDir == "" {
				outDir = wizard.ConfigDir(runtime.GOOS, os.Getenv, os.Geteuid() == 0)
			}

			// The prompter is still used for the checks below, which may have
			// to ask about sudo, so it outlives the questions themselves.
			prompter := wizard.Prompter(&flagPrompter{out: out})
			interactiveRun := !nonInteractive && interactive()
			switch {
			case interactiveRun:
				printBanner(out, build)
				collected, err := askInteractively(nil, nil, answers)
				if err != nil {
					if errors.Is(err, errCancelled) {
						out.printf("\n  Cancelled. Nothing was written.\n")
						return silentError{err}
					}
					return err
				}
				answers = collected
				// Nobody sets a service up in order to leave it stopped, and
				// starting it is what proves the answers were right, so it is
				// not a question. --start=false is there for the rare case.
				answers.Start = !cmd.Flags().Changed("start") || start
				prompter = newInteractivePrompter(out)
				echoAnswers(out, answers)
			default:
				if !nonInteractive {
					out.skip("no interactive terminal detected, using defaults and flags")
				}
				// The flag prompter answers from flags and defaults, so the
				// only error it can produce is an answer that fails its own
				// validation, which is worth reporting as it is.
				collected, err := wizard.Run(prompter, answers)
				if err != nil {
					return err
				}
				answers = collected
				// Without a terminal, starting a service is something to be
				// asked for explicitly rather than defaulted into.
				answers.Start = start
			}
			wizard.Finalise(&answers)

			// Everything the answers depend on is checked before a single file
			// is written, because a wizard that fails after the last question
			// has wasted the whole conversation.
			if err := preflight(cmd.Context(), out, prompter, &answers, outDir, interactiveRun); err != nil {
				return err
			}

			// The token is created before the files are written so that .env can
			// carry it, and so a failure leaves nothing half-configured.
			plain, err := createToken(answers)
			if err != nil {
				return err
			}
			answers.Token = plain

			binary, _ := os.Executable()
			files := wizard.Files(answers, binary)
			written, err := wizard.Write(outDir, files, force)
			if err != nil {
				return err
			}

			if !answers.Telemetry && answers.DataDir != "" {
				if err := telemetry.SetDisabled(answers.DataDir, true); err != nil {
					return err
				}
			}

			if out.json {
				names := make([]string, 0, len(written))
				names = append(names, written...)
				return out.emit(map[string]any{
					"token":      plain,
					"token_name": answers.TokenName,
					"files":      names,
					"base_url":   answers.BaseURL,
					"data_dir":   answers.DataDir,
					"next_steps": wizard.NextSteps(answers),
				})
			}

			reportSetup(out, answers, written)
			if err := maybeStart(cmd.Context(), out, build, answers, outDir); err != nil {
				return err
			}
			verify(cmd.Context(), out, answers)
			printFinish(out, answers, outDir)
			return nil
		},
	}

	f := cmd.Flags()
	f.StringVar(&answers.BaseURL, "base-url", answers.BaseURL, "public URL, e.g. https://files.example.com")
	// The default depends on who is running setup and on where their home
	// directory is, so it is described rather than printed: a help text that
	// differs from machine to machine cannot be documented.
	f.StringVar(&dataDir, "data-dir", "",
		"where uploaded files are stored (default /var/lib/godrop as root, ~/.local/share/godrop otherwise)")
	f.StringVar(&answers.Port, "port", answers.Port, "listen port")
	f.StringVar(&answers.MaxFileSize, "max-file-size", answers.MaxFileSize, "per-file limit, e.g. 100MB")
	f.StringVar(&answers.MaxTotalSize, "max-total-size", answers.MaxTotalSize, "storage quota, empty for unlimited")
	f.StringVar(&answers.Retention, "retention", answers.Retention, "delete files after this long, e.g. 30d")
	f.StringVar(&answers.TLS, "tls", answers.TLS, "auto (Let's Encrypt), file, proxy or none")
	f.StringVar(&answers.TLSCert, "tls-cert", answers.TLSCert, "certificate chain in PEM, with --tls=file")
	f.StringVar(&answers.TLSKey, "tls-key", answers.TLSKey, "private key in PEM, with --tls=file")
	f.StringVar(&answers.Deployment, "deployment", answers.Deployment, "compose, systemd or env")
	f.StringVar(&answers.TokenName, "token-name", answers.TokenName, "name for the generated token")
	f.BoolVar(&answers.Telemetry, "telemetry", answers.Telemetry, "send the anonymous daily heartbeat")
	// The wizard asks one question about the limits and only opens the four
	// detailed ones when the answer is no; this is that answer, for a script.
	f.BoolVar(&setLimits, "limits", false, "set the size, quota, retention and port questions yourself")
	// --no-input is the conventional name for "never prompt"; the older spelling
	// stays as a hidden alias so existing scripts keep working.
	f.BoolVar(&nonInteractive, "no-input", false, "never prompt; use flags and defaults (for CI and agents)")
	f.BoolVar(&nonInteractive, "non-interactive", false, "alias for --no-input")
	_ = f.MarkHidden("non-interactive")
	f.BoolVar(&force, "force", false, "overwrite existing configuration files")
	f.BoolVar(&skipExternal, "no-external-check", false, "do not ask godrop.sh to verify reachability")
	f.BoolVar(&start, "start", false,
		"start the service when setup finishes (interactive setup does anyway; --start=false stops it)")
	f.StringVar(&outDir, "out-dir", "", "where to write the generated files (default: working directory)")
	return cmd
}

// checkFirewall and externalCheck are variables so the verification step can be
// exercised without a firewall or the hosted probe.
var (
	checkFirewall = netcheck.CheckFirewall
	externalCheck = netcheck.External
)

// newInteractivePrompter builds the terminal wizard. It is a variable so tests
// can drive the real forms with scripted keystrokes.
var newInteractivePrompter = func(out *output) wizard.Prompter { return newHuhPrompter(out) }

func printBanner(out *output, build Build) {
	out.printf("\n  GoDrop %s setup\n", build.Version)
	out.printf("  Upload a file, get a hard-to-guess URL.\n")
}

func reportSetup(out *output, a wizard.Answers, written []string) {
	out.heading("Written")
	for _, w := range written {
		note := ""
		switch filepath.Base(w) {
		case ".env":
			note = "  (chmod 600, contains your token)"
		case wizard.SampleName:
			note = "  (a picture, so the first example below uploads something)"
		}
		out.success("%s%s", w, note)
	}

	out.heading("Your API token")
	out.printf("\n")
	out.box(a.Token)
	out.printf("\n")
	out.warn("shown once and never again, so copy it now")
	out.skip("stored as a SHA-256 digest in %s", tokens.Path(a.DataDir))
}

// maybeStart brings the service up, because a setup that ends with "now go and
// run something else" is where most installs stall. Only the steps that need
// root are left for the operator, and those are printed at the end.
func maybeStart(ctx context.Context, out *output, build Build, a wizard.Answers, outDir string) error {
	if !a.Start {
		return nil
	}
	switch a.Deployment {
	case wizard.DeployCompose:
		if _, err := lookPath("docker"); err != nil {
			out.warn("docker not found; start it yourself with: docker compose up -d")
			return nil
		}
		out.heading("Starting")
		if err := runCommand(ctx, "docker", "compose", "--project-directory", outDir, "up", "-d"); err != nil {
			return fmt.Errorf("docker compose up failed: %w", err)
		}
		out.success("containers started")
		runDoctor(ctx, out, build, a)
	case wizard.DeploySystemd:
		out.heading("Starting")
		out.skip("installing a systemd unit needs root; the commands are at the end")
	case wizard.DeployEnv:
		out.heading("Starting")
		out.skip("run it yourself: set -a && . ./.env && set +a && godrop serve")
	}
	return nil
}

// anyLimitFlagSet reports whether a limit was given on the command line, which
// is the same as asking for them to be set by hand.
func anyLimitFlagSet(cmd *cobra.Command) bool {
	for _, name := range []string{"max-file-size", "max-total-size", "retention", "port"} {
		if cmd.Flags().Changed(name) {
			return true
		}
	}
	return false
}

// runDoctor runs the diagnosis the operator would have run next, so that the
// setup ends with the answer rather than with the command that finds it.
func runDoctor(ctx context.Context, out *output, build Build, a wizard.Answers) {
	target := a.BaseURL
	if target == "" {
		target = "http://127.0.0.1:" + wizard.ListenPort(a)
	}
	out.heading("Checking it")
	// A container takes a moment to answer, and a diagnosis run too early
	// reports a problem that fixes itself a second later.
	waitForHealth(ctx, target)

	// In process rather than as a subprocess: the same code the doctor
	// command runs, without depending on this binary still being where it was
	// when setup started.
	report := doctor.Run(ctx, doctor.Options{
		TargetURL: target,
		Token:     a.Token,
		Version:   build.Version,
		Offline:   true,
	})
	printReport(out, report)
	if report.Failed() {
		out.hint("run it again once everything is in place: godrop doctor --url %s", target)
	}
}

// healthWait is how long a freshly started service is given to answer. It is
// a variable so a test does not have to wait it out.
var healthWait = 20 * time.Second

// waitForHealth gives the service a few seconds to start answering.
func waitForHealth(ctx context.Context, base string) {
	client := &http.Client{Timeout: 2 * time.Second}
	deadline := time.Now().Add(healthWait)
	for time.Now().Before(deadline) {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(base, "/")+"/healthz", nil)
		if err != nil {
			return
		}
		resp, err := client.Do(req)
		if err == nil {
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(500 * time.Millisecond):
		}
	}
}

// createToken mints the first token. Under docker compose the data directory
// belongs to a volume that does not exist yet, so the token goes into .env
// rather than into a token file the host cannot write.
func createToken(a wizard.Answers) (string, error) {
	if a.DataDir == "" {
		return tokens.Generate()
	}
	store, err := tokens.New(tokens.Path(a.DataDir), nil)
	if err != nil {
		return "", fmt.Errorf("prepare token store: %w", err)
	}
	plain, _, err := store.Create(a.TokenName)
	if err != nil {
		return "", fmt.Errorf("create token: %w", err)
	}
	return plain, nil
}

// askInteractively runs the whole wizard as one form, reading the terminal.
// It is a variable holding runForm itself, so a test drives the real code with
// scripted keystrokes and nothing ever has to wait on a stdin that will not
// answer.
var askInteractively = runForm

// echoAnswers repeats what was chosen, because the form clears itself when it
// finishes and the answers are what everything below refers to.
func echoAnswers(out *output, a wizard.Answers) {
	out.heading("Answers")
	out.skip("%-22s %s", "public URL", displayValue(a.BaseURL))
	out.skip("%-22s %s", "deployment", a.Deployment)
	if wizard.AsksTLS(a) {
		out.skip("%-22s %s", "certificate", a.TLS)
	}
	if a.DataDir != "" {
		out.skip("%-22s %s", "data directory", a.DataDir)
	} else {
		out.skip("%-22s docker volume godrop-data", "data")
	}
	out.skip("%-22s %s per file, %s quota", "limits", a.MaxFileSize, displayValue(a.MaxTotalSize))
}

// verify runs the reachability checks that matter right after installation:
// is anything listening, does the firewall allow the public port, and the
// question a server cannot answer about itself: can the internet reach it.
func verify(ctx context.Context, out *output, a wizard.Answers) {
	out.heading("Verifying")

	local := "127.0.0.1:" + wizard.ListenPort(a)
	ctx, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()

	// When the service was started here, doctor has already reported on the
	// local side of it; repeating that is noise. What no machine can check
	// about itself is whether the internet can reach it, and that is below.
	if !a.Start {
		if netcheck.Listening(ctx, local) {
			out.success("%-22s listening", local)
		} else {
			out.warn("%-22s nothing listening yet", local)
			out.hint("start it, then run: godrop doctor")
		}
	}

	// With a certificate of its own GoDrop needs port 80 as well as 443, and
	// an install that opens only 443 never gets past the challenge.
	ports := wizard.PublicPorts(a)
	for i, port := range ports {
		if a.Start {
			break // doctor looked at the firewall already
		}
		fw := checkFirewall(ctx, nil, port)
		switch {
		case !fw.Inspected:
			// Saying it once is enough: there is one firewall, not one per port.
			if i == 0 {
				out.skip("%-22s no host firewall detected", "firewall")
			}
		case fw.PortOpen:
			out.success("%-22s %s allows port %d", "firewall", fw.Tool, port)
		default:
			out.fail("%-22s %s blocks port %d", "firewall", fw.Tool, port)
			out.hint("%s", fw.Hint)
		}
	}
	for _, step := range wizard.FirewallSteps(a, ports...) {
		if strings.HasPrefix(step, "also open") {
			out.skip("%s", step)
		}
	}

	if a.BaseURL == "" {
		out.skip("%-22s no public URL configured", "external access")
		return
	}
	if !a.ExternalCheck {
		out.skip("%-22s skipped", "external access")
		out.hint("verify from another machine: curl -sI %s/healthz", a.BaseURL)
		return
	}

	client := &http.Client{Timeout: 20 * time.Second}
	res, err := externalCheck(ctx, client, "", strings.TrimRight(a.BaseURL, "/")+"/healthz")
	switch {
	case err != nil:
		out.warn("%-22s could not run the check: %v", "external access", err)
		out.hint("verify from another machine: curl -sI %s/healthz", a.BaseURL)
	case res.OK:
		detail := fmt.Sprintf("reachable (HTTP %d", res.Status)
		if res.Location != "" {
			detail += ", from " + res.Location
		}
		out.success("%-22s %s)", "external access", detail)
	default:
		msg := res.Error
		if msg == "" {
			msg = fmt.Sprintf("HTTP %d", res.Status)
		}
		out.fail("%-22s not reachable: %s", "external access", msg)
		out.hint("open %s in your cloud provider's firewall (AWS security group, Hetzner firewall, GCP rule)",
			portList(ports))
		if !wizard.ServesTLS(a) {
			out.hint("and check that your reverse proxy forwards to 127.0.0.1:%s", a.Port)
		}
	}
}

// portList renders the ports for a sentence: "port 443" or "ports 443 and 80".
func portList(ports []int) string {
	switch len(ports) {
	case 0:
		return "the public port"
	case 1:
		return fmt.Sprintf("port %d", ports[0])
	default:
		parts := make([]string, 0, len(ports))
		for _, p := range ports {
			parts = append(parts, strconv.Itoa(p))
		}
		return "ports " + strings.Join(parts[:len(parts)-1], ", ") + " and " + parts[len(parts)-1]
	}
}

func printFinish(out *output, a wizard.Answers, outDir string) {
	if steps := wizard.NextSteps(a); len(steps) > 0 {
		out.heading("Next")
		if outDir != "." {
			out.skip("from %s:", outDir)
		}
		for _, step := range steps {
			out.command(step)
		}
	}
	out.heading("Use it")
	// The example uploads the picture written a moment ago, by its full path:
	// a command that only works from one directory is a command that fails.
	for _, ex := range wizard.CurlExamples(a, filepath.Join(outDir, wizard.SampleName)) {
		out.command(ex)
	}
	out.printf("\n  Point an AI agent at it with two values:\n")
	base := a.BaseURL
	if base == "" {
		base = "http://localhost:" + wizard.ListenPort(a)
	}
	out.command("GODROP_URL=" + base)
	out.command("GODROP_TOKEN=" + a.Token)
	out.printf("\n  It can learn the rest by itself: %s/llms.txt\n", base)

	// The heartbeat is told, not asked. This says exactly what leaves the
	// machine and gives the command that stops it, which is everything a
	// yes/no in the middle of the questions could offer.
	if a.Telemetry {
		out.heading("Anonymous heartbeat")
		out.skip("once a day: {install_id, version, os, arch, deploy}")
		out.skip("no file names, no counts, no addresses, no base URL")
		out.hint("turn it off any time with: godrop telemetry off")
	}
	out.printf("\n")
}
