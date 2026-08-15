package cli

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/fatihbaltaci/GoDrop/internal/netcheck"
	"github.com/fatihbaltaci/GoDrop/internal/telemetry"
	"github.com/fatihbaltaci/GoDrop/internal/tokens"
	"github.com/fatihbaltaci/GoDrop/internal/wizard"
)

func newInitCmd(build Build) *cobra.Command {
	var (
		answers        = wizard.Defaults()
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
files, creates your first API token and — if you like — starts the service and
confirms that the outside world can actually reach it.

Every answer can be supplied as a flag instead; with --non-interactive the same
wizard runs without asking anything, which is what CI and agents should use.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			out := newOutput(cmd)
			answers.ExternalCheck = answers.ExternalCheck && !skipExternal

			if outDir == "" {
				outDir = "."
			}

			var prompter wizard.Prompter
			switch {
			case nonInteractive || !interactive():
				if !nonInteractive {
					out.skip("no interactive terminal detected — using defaults and flags")
				}
				prompter = &flagPrompter{out: out}
			default:
				printBanner(out, build)
				prompter = newInteractivePrompter(out)
			}

			collected, err := wizard.Run(prompter, answers)
			if err != nil {
				if errors.Is(err, errCancelled) {
					out.printf("\n  Cancelled. Nothing was written.\n")
					return silentError{err}
				}
				return err
			}
			answers = collected

			// The token is created before the files are written so that .env can
			// carry it, and so a failure leaves nothing half-configured.
			store, err := tokens.New(tokens.Path(answers.DataDir), nil)
			if err != nil {
				return fmt.Errorf("prepare token store: %w", err)
			}
			plain, _, err := store.Create(answers.TokenName)
			if err != nil {
				return fmt.Errorf("create token: %w", err)
			}
			answers.Token = plain

			binary, _ := os.Executable()
			files := wizard.Files(answers, binary)
			written, err := wizard.Write(outDir, files, force)
			if err != nil {
				return err
			}

			if !answers.Telemetry {
				if err := telemetry.SetDisabled(answers.DataDir, true); err != nil {
					return err
				}
			}

			if out.json {
				names := make([]string, 0, len(written))
				for _, w := range written {
					names = append(names, w)
				}
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
			if err := maybeStart(cmd.Context(), out, answers, start); err != nil {
				return err
			}
			verify(cmd.Context(), out, answers)
			printFinish(out, answers)
			return nil
		},
	}

	f := cmd.Flags()
	f.StringVar(&answers.BaseURL, "base-url", answers.BaseURL, "public URL, e.g. https://files.example.com")
	f.StringVar(&answers.DataDir, "data-dir", answers.DataDir, "where uploaded files are stored")
	f.StringVar(&answers.Port, "port", answers.Port, "listen port")
	f.StringVar(&answers.MaxFileSize, "max-file-size", answers.MaxFileSize, "per-file limit, e.g. 100MB")
	f.StringVar(&answers.MaxTotalSize, "max-total-size", answers.MaxTotalSize, "storage quota, empty for unlimited")
	f.StringVar(&answers.Retention, "retention", answers.Retention, "delete files after this long, e.g. 30d")
	f.StringVar(&answers.Deployment, "deployment", answers.Deployment, "compose, systemd or env")
	f.StringVar(&answers.TokenName, "token-name", answers.TokenName, "name for the generated token")
	f.BoolVar(&answers.Telemetry, "telemetry", answers.Telemetry, "send the anonymous daily heartbeat")
	f.BoolVar(&nonInteractive, "non-interactive", false, "never prompt; use flags and defaults")
	f.BoolVar(&force, "force", false, "overwrite existing configuration files")
	f.BoolVar(&skipExternal, "no-external-check", false, "do not ask godrop.sh to verify reachability")
	f.BoolVar(&start, "start", false, "start the service when setup finishes")
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
	out.printf("\n  GoDrop %s — setup\n", build.Version)
	out.printf("  Upload a file, get a hard-to-guess URL.\n")
}

func reportSetup(out *output, a wizard.Answers, written []string) {
	out.heading("Written")
	for _, w := range written {
		mode := ""
		if filepath.Base(w) == ".env" {
			mode = "  (chmod 600 — contains your token)"
		}
		out.success("%s%s", w, mode)
	}

	out.heading("Your API token")
	out.printf("\n")
	out.box(a.Token)
	out.printf("\n")
	out.warn("shown once and never again — copy it now")
	out.skip("stored as a SHA-256 digest in %s", tokens.Path(a.DataDir))
}

// maybeStart offers to bring the service up, because a setup wizard that ends
// with "now go and run something else" is where most installs stall.
func maybeStart(ctx context.Context, out *output, a wizard.Answers, forceStart bool) error {
	if a.Deployment != wizard.DeployCompose {
		return nil
	}
	if !forceStart {
		return nil
	}
	if _, err := exec.LookPath("docker"); err != nil {
		out.warn("docker not found; start it yourself with: docker compose up -d")
		return nil
	}
	out.heading("Starting")
	cmd := exec.CommandContext(ctx, "docker", "compose", "up", "-d")
	cmd.Stdout, cmd.Stderr = os.Stderr, os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("docker compose up failed: %w", err)
	}
	out.success("containers started")
	return nil
}

// verify runs the reachability checks that matter right after installation:
// is anything listening, does the firewall allow the public port, and — the
// question a server cannot answer about itself — can the internet reach it.
func verify(ctx context.Context, out *output, a wizard.Answers) {
	out.heading("Verifying")

	local := "127.0.0.1:" + a.Port
	ctx, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()

	if netcheck.Listening(ctx, local) {
		out.success("%-22s listening", local)
	} else {
		out.warn("%-22s nothing listening yet", local)
		out.hint("start it, then run: godrop doctor")
	}

	port := wizard.PublicPort(a)
	fw := checkFirewall(ctx, nil, port)
	switch {
	case !fw.Inspected:
		out.skip("%-22s no host firewall detected", "firewall")
	case fw.PortOpen:
		out.success("%-22s %s allows port %d", "firewall", fw.Tool, port)
	default:
		out.fail("%-22s %s blocks port %d", "firewall", fw.Tool, port)
		out.hint("%s", fw.Hint)
	}
	for _, step := range wizard.FirewallSteps(a, port) {
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
		out.hint("open port %d in your cloud provider's firewall (AWS security group, Hetzner firewall, GCP rule)", port)
		out.hint("and check that your reverse proxy forwards to 127.0.0.1:%s", a.Port)
	}
}

func printFinish(out *output, a wizard.Answers) {
	out.heading("Next")
	for _, step := range wizard.NextSteps(a) {
		out.command(step)
	}
	out.heading("Use it")
	for _, ex := range wizard.CurlExamples(a) {
		out.command(ex)
	}
	out.printf("\n  Point an AI agent at it with two values:\n")
	base := a.BaseURL
	if base == "" {
		base = "http://localhost:" + a.Port
	}
	out.command("GODROP_URL=" + base)
	out.command("GODROP_TOKEN=" + a.Token)
	out.printf("\n  It can learn the rest by itself: %s/llms.txt\n\n", base)
}
