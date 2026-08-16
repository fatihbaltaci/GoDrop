package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"runtime"
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

// lookupHome is a seam: the home directory of another user comes from the
// password database, which a test has no business editing.
var lookupHome = func(name string) (string, error) {
	u, err := user.Lookup(name)
	if err != nil {
		return "", err
	}
	return u.HomeDir, nil
}

// installationDir is where this machine's configuration is, from the point of
// view of whoever asked.
//
// sudo changes the answer: replacing a binary in /usr/local/bin needs root,
// but the installation being updated belongs to the person who ran setup, and
// root's own /etc/godrop is usually empty. Their configuration is the one to
// act on, not a directory nobody has written to.
func installationDir() string {
	dir := wizard.ConfigDir(runtime.GOOS, os.Getenv, os.Geteuid() == 0)
	if installedAt(dir) {
		return dir
	}
	name := os.Getenv("SUDO_USER")
	if name == "" {
		return dir
	}
	home, err := lookupHome(name)
	if err != nil || home == "" {
		return dir
	}
	// Their home is the whole environment that matters here: sudo exists on
	// the platforms where the configuration directory is derived from it.
	theirs := wizard.ConfigDir(runtime.GOOS, func(string) string { return home }, false)
	if installedAt(theirs) {
		return theirs
	}
	return dir
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
	a := answersFromEnv(dir)
	runDoctor(ctx, out, build, a)

	// An installation from before there was a sample picture has none, and the
	// example below names one.
	sample := filepath.Join(dir, wizard.SampleName)
	if _, err := os.Stat(sample); err != nil {
		if err := os.WriteFile(sample, []byte(wizard.SampleImage()), 0o644); err != nil { //nolint:gosec // G306, a picture is not a secret
			sample = ""
		}
	}

	printInstallation(ctx, out, a, dir, deployment)
	printUseIt(out, a, sample, dir, deployment, a.DataDir)
	return nil
}

// printUseIt is the block somebody copies from: the three requests, and the
// command that makes a token for whatever they are wiring GoDrop into.
func printUseIt(out *output, a wizard.Answers, sample, dir, deployment, dataDir string) {
	out.heading("Use it")
	for _, ex := range wizard.CurlExamples(a, sample) {
		out.command(ex)
	}
	out.printf("\n  A token of its own for an agent, a script or a second machine:\n")
	out.command(tokenCommand(dir, deployment, dataDir))
	out.printf("\n")
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

// printInstallation is the closing summary: where the files are, what is
// running the service, where the uploads end up and the address to send one
// to. It is what somebody comes back for a week later, when the terminal that
// printed the setup is long gone.
func printInstallation(ctx context.Context, out *output, a wizard.Answers, dir, deployment string) {
	out.heading("Your installation")
	out.skip("%-22s %s", "location", dir)
	switch deployment {
	case wizard.DeployCompose:
		// The container's name is docker's to decide, from the directory and
		// rules that are docker's rather than ours, so it is asked for rather
		// than guessed. A machine with no docker on it simply says less.
		name, storage := composeContainer(ctx, dir)
		if name == "" {
			out.skip("%-22s docker compose", "service")
			break
		}
		out.skip("%-22s docker compose, container %s", "service", name)
		if storage != "" {
			out.skip("%-22s %s", "uploads", storage)
		}
	case wizard.DeploySystemd:
		out.skip("%-22s systemd unit godrop", "service")
		out.skip("%-22s %s", "uploads", a.DataDir)
	default:
		out.skip("%-22s none; you run it yourself with godrop serve", "service")
		out.skip("%-22s %s", "uploads", a.DataDir)
	}
	out.skip("%-22s %s", "address", wizard.PublicAddress(a))
}

// tokenCommand is how this installation makes another token. Where the token
// file is decides it: a compose deployment keeps it in a volume only the
// container can reach, so the command has to go through docker.
func tokenCommand(dir, deployment, dataDir string) string {
	if deployment == wizard.DeployCompose {
		return "docker compose --project-directory " + dir + " run --rm godrop token create --name claude-code"
	}
	if dataDir == "" {
		return godropCommand() + " token create --name claude-code"
	}
	return godropCommand() + " token create --data-dir " + dataDir + " --name claude-code"
}

// godropCommand is how to invoke this program from a shell. Usually that is
// its name; when it was installed somewhere that is not on the PATH, printing
// the name would print a command that answers "command not found".
func godropCommand() string {
	if _, err := lookPath("godrop"); err == nil {
		return "godrop"
	}
	self, err := osExecutable()
	if err != nil {
		return "godrop"
	}
	return self
}

// composeRun carries out a godrop command where the installation actually is:
// inside its container, where the token file and the uploads are.
//
// The program there is this program, so the answer is the one a local
// installation would give and there is no second implementation to keep
// honest. The running container is used when there is one, and a throwaway
// one when the service is stopped, because both can read the volume.
func composeRun(ctx context.Context, project string, args ...string) ([]byte, error) {
	if _, err := lookPath("docker"); err != nil {
		return nil, fmt.Errorf("this installation runs in a container, and docker is not on this machine: %w", err)
	}
	argv := []string{"compose", "--project-directory", project}
	if name, _ := composeContainer(ctx, project); name != "" {
		// -T: no terminal, so the answer comes back as it was written.
		argv = append(argv, "exec", "-T", "godrop", "/godrop")
	} else {
		argv = append(argv, "run", "--rm", "godrop")
	}
	out, err := runOutput(ctx, "docker", append(argv, args...)...)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", strings.Join(args, " "), withStderr(err))
	}
	return []byte(out), nil
}

// withStderr puts what the command said on its error stream into the error,
// which is where docker explains itself.
func withStderr(err error) error {
	var exit *exec.ExitError
	if errors.As(err, &exit) && len(exit.Stderr) > 0 {
		return fmt.Errorf("%w: %s", err, strings.TrimSpace(string(exit.Stderr)))
	}
	return err
}

// composeContainer asks docker which container is running the service, and
// what it keeps the uploads on. Both are empty when there is nothing to ask
// or nothing running: a summary with a wrong name in it is worse than a
// summary with one line fewer.
func composeContainer(ctx context.Context, dir string) (name, storage string) {
	if _, err := lookPath("docker"); err != nil {
		return "", ""
	}
	name, err := runOutput(ctx, "docker", "compose", "--project-directory", dir, "ps", "--format", "{{.Name}}")
	if err != nil || name == "" {
		return "", ""
	}
	name = strings.Fields(name)[0]
	// /data is where the compose file mounts the uploads, whether that is a
	// named volume or a directory on the host.
	const format = `{{range .Mounts}}{{if eq .Destination "/data"}}` +
		`{{if eq .Type "volume"}}docker volume {{.Name}}{{else}}{{.Source}}{{end}}{{end}}{{end}}`
	storage, err = runOutput(ctx, "docker", "inspect", "--format", format, name)
	if err != nil {
		storage = ""
	}
	return name, storage
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
