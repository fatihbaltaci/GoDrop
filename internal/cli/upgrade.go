package cli

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/fatihbaltaci/GoDrop/internal/wizard"
)

// A second run of the installer, or of `godrop init`, lands on a machine that
// already has GoDrop on it. What that means is an update: the configuration,
// the token and the uploads stay exactly where they are, and the service moves
// to the release that was just downloaded.

// installedAt reports whether setup has already written a configuration here.
func installedAt(dir string) bool {
	return writtenByGoDrop(filepath.Join(dir, ".env"))
}

// deploymentAt works out how an existing installation runs, from what setup
// left next to its .env.
func deploymentAt(dir string) string {
	switch {
	case writtenByGoDrop(filepath.Join(dir, "docker-compose.yml")):
		return wizard.DeployCompose
	case writtenByGoDrop(filepath.Join(dir, "godrop.service")):
		return wizard.DeploySystemd
	default:
		return wizard.DeployEnv
	}
}

// upgrade moves an existing installation onto the current release and checks
// that it came back up.
func upgrade(ctx context.Context, out *output, build Build, dir string) error {
	deployment := deploymentAt(dir)
	if out.json {
		if err := upgradeService(ctx, out, dir, deployment); err != nil {
			return err
		}
		return out.emit(map[string]any{
			"config_dir": dir, "deployment": deployment, "version": build.Version,
		})
	}
	out.heading("Updating")
	if err := upgradeService(ctx, out, dir, deployment); err != nil {
		return err
	}
	runDoctor(ctx, out, build, answersFromEnv(dir))
	return nil
}

// upgradeService restarts whatever is running the service, onto the release
// that is now on this machine. Nothing here touches the .env, the token file
// or the uploads: a container's data lives in its volume, and a systemd
// service's in its data directory.
func upgradeService(ctx context.Context, out *output, dir, deployment string) error {
	switch deployment {
	case wizard.DeployCompose:
		if _, err := lookPath("docker"); err != nil {
			out.warn("docker is not installed, so there is no container to update")
			out.hint("install docker, then run: docker compose up -d")
			return nil
		}
		// Pull first. `up -d` on its own reuses whatever :latest means on this
		// machine, which is the image pulled the first time and nothing since.
		if err := runCommand(ctx, "docker", "compose", "--project-directory", dir, "pull"); err != nil {
			return fmt.Errorf("docker compose pull failed: %w", err)
		}
		// A changed image makes `up -d` replace the container and remove the
		// old one. The named volume is not part of that, so the uploads stay.
		if err := runCommand(ctx, "docker", "compose", "--project-directory", dir, "up", "-d"); err != nil {
			return fmt.Errorf("docker compose up failed: %w", err)
		}
		out.success("the container is running the newest image")
	case wizard.DeploySystemd:
		// The unit runs the binary that was just replaced, so all that is left
		// is to restart into it, and that needs root.
		if euid() != 0 {
			out.skip("restart the service to run the new binary: sudo systemctl restart godrop")
			return nil
		}
		if err := runCommand(ctx, "systemctl", "restart", "godrop"); err != nil {
			return fmt.Errorf("systemctl restart godrop failed: %w", err)
		}
		out.success("the service restarted into the new binary")
	default:
		out.skip("start the new binary yourself: godrop serve")
	}
	return nil
}

// answersFromEnv reads back the few values the verification needs from the
// .env setup wrote: where the service answers, and a token to talk to it with.
func answersFromEnv(dir string) wizard.Answers {
	values := readEnvFile(filepath.Join(dir, ".env"))
	a := wizard.Answers{
		BaseURL: values["GODROP_BASE_URL"],
		Port:    strings.TrimPrefix(values["GODROP_ADDR"], ":"),
		Token:   strings.Split(values["GODROP_TOKENS"], ",")[0],
	}
	if a.Port == "" {
		a.Port = wizard.Defaults().Port
	}
	return a
}

// readEnvFile parses KEY=VALUE lines. It is deliberately not a dotenv library:
// this reads a file this program wrote, in the shape this program writes it.
func readEnvFile(path string) map[string]string {
	values := map[string]string{}
	data, err := os.ReadFile(path) //nolint:gosec // G304, the path is this package's own
	if err != nil {
		return values
	}
	for line := range strings.SplitSeq(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		values[strings.TrimSpace(key)] = strings.Trim(strings.TrimSpace(value), `"`)
	}
	return values
}
