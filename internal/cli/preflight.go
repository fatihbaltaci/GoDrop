package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/fatihbaltaci/GoDrop/internal/wizard"
)

// Seams, so the checks can be exercised without a docker, a firewall or a
// privileged directory to hand.
var (
	lookPath   = exec.LookPath
	runCommand = func(ctx context.Context, name string, args ...string) error {
		cmd := exec.CommandContext(ctx, name, args...) //nolint:gosec // the arguments are built here, not by a user
		cmd.Stdout, cmd.Stderr = os.Stderr, os.Stderr
		return cmd.Run()
	}
	// runQuietly is for the checks, whose output is a tick or a cross, not the
	// version banner of whatever was probed.
	runQuietly = func(ctx context.Context, name string, args ...string) error {
		cmd := exec.CommandContext(ctx, name, args...) //nolint:gosec // the arguments are built here, not by a user
		return cmd.Run()
	}
	listenOn = func(addr string) (io.Closer, error) { return net.Listen("tcp", addr) }
	euid     = os.Geteuid
	// osExecutable is this binary, which setup re-runs for the diagnosis.
	osExecutable = os.Executable
)

// The wizard cannot open a socket for itself, so the port question borrows
// this one. A port that needs privileges is not a port that is taken: the
// generated unit and docker both arrange for those, so only a real listener
// counts as an answer worth showing.
func init() {
	wizard.PortInUse = func(port string) error {
		err := portInUse(port)
		if err == nil || isPermission(err) {
			return nil
		}
		return fmt.Errorf("something is already listening on %s", port)
	}
}

// preflight checks what the setup is about to depend on, before it writes
// anything at all.
//
// The failure this exists to prevent: answering every question, watching the
// files be written, and then finding out that the data directory needs root.
// Anything that can be fixed here is offered as a fix; anything that cannot
// stops the wizard while nothing has been created yet.
func preflight(ctx context.Context, out *output, p wizard.Prompter, a *wizard.Answers, outDir string, canAsk bool) error {
	out.heading("Checks")

	if err := checkDataDir(ctx, out, p, *a); err != nil {
		return err
	}
	if err := checkOutDir(out, outDir); err != nil {
		return err
	}
	checkTooling(ctx, out, *a)
	return checkPort(out, p, a, canAsk)
}

// checkDataDir makes sure uploads have somewhere to go, and offers sudo when
// the chosen directory belongs to root and this is not root.
func checkDataDir(ctx context.Context, out *output, p wizard.Prompter, a wizard.Answers) error {
	dir := a.DataDir
	if dir == "" {
		// Under docker compose the files live in a volume docker creates and
		// owns; there is no host directory to check.
		out.success("%-22s docker volume godrop-data", "storage")
		return nil
	}
	if err := usable(dir); err == nil {
		out.success("%-22s %s", "data directory", dir)
		return nil
	}

	// Creating it is the usual answer; being unable to is the interesting one.
	if err := os.MkdirAll(dir, 0o700); err == nil {
		if err := usable(dir); err == nil {
			out.success("%-22s %s (created)", "data directory", dir)
			return nil
		}
	}

	out.fail("%-22s cannot write to %s", "data directory", dir)
	sudo, haveSudo := sudoAvailable()
	if !haveSudo {
		out.hint("create it as root:  mkdir -p %s && chown %d:%d %s", dir, os.Getuid(), os.Getgid(), dir)
		out.hint("or choose a directory you own, e.g. %s", wizard.DefaultDataDir("linux", os.Getenv, false))
		return silentError{errors.New("the data directory is not writable")}
	}

	ok, err := p.Confirm("Create it with sudo?",
		fmt.Sprintf("Runs: sudo install -d -m 700 -o %d -g %d %s", os.Getuid(), os.Getgid(), dir), true)
	if err != nil {
		return err
	}
	if !ok {
		out.hint("choose a directory you own, e.g. %s", wizard.DefaultDataDir("linux", os.Getenv, false))
		return silentError{errors.New("the data directory is not writable")}
	}
	args := []string{"install", "-d", "-m", "700",
		"-o", strconv.Itoa(os.Getuid()), "-g", strconv.Itoa(os.Getgid()), dir}
	if err := runCommand(ctx, sudo, args...); err != nil {
		return fmt.Errorf("create %s with sudo: %w", dir, err)
	}
	if err := usable(dir); err != nil {
		return fmt.Errorf("create %s: %w", dir, err)
	}
	out.success("%-22s %s (created with sudo)", "data directory", dir)
	return nil
}

// checkOutDir makes sure the generated files have somewhere to go.
func checkOutDir(out *output, dir string) error {
	if dir == "" {
		dir = "."
	}
	// Setup owns this directory, so creating it is part of the job rather
	// than something to ask about.
	_ = os.MkdirAll(dir, 0o700)
	if err := usable(dir); err != nil {
		out.fail("%-22s cannot write to %s", "output directory", dir)
		out.hint("run setup from a directory you own, or pass --out-dir")
		return silentError{errors.New("the output directory is not writable")}
	}
	// Abs only fails when the working directory itself has gone, in which
	// case the path as given is still the best thing to print.
	if abs, err := filepath.Abs(dir); err == nil {
		dir = abs
	}
	out.success("%-22s %s", "output directory", dir)
	return nil
}

// checkTooling reports whether the commands the chosen deployment needs are
// actually installed. Finding out after the files are written, from a shell
// error, is worse than finding out here.
func checkTooling(ctx context.Context, out *output, a wizard.Answers) {
	switch a.Deployment {
	case wizard.DeployCompose:
		if _, err := lookPath("docker"); err != nil {
			out.warn("%-22s not installed", "docker")
			out.hint("install it: https://docs.docker.com/engine/install/  (or choose another deployment style)")
			return
		}
		if err := runQuietly(ctx, "docker", "compose", "version"); err != nil {
			out.warn("%-22s docker is installed, but `docker compose` is not", "docker compose")
			out.hint("install the compose plugin: https://docs.docker.com/compose/install/")
			return
		}
		out.success("%-22s available", "docker compose")
	case wizard.DeploySystemd:
		if _, err := lookPath("systemctl"); err != nil {
			out.warn("%-22s not found on this machine", "systemd")
			out.hint("the unit will be written anyway; install it yourself, or choose another deployment style")
			return
		}
		out.success("%-22s available", "systemd")
		if euid() != 0 {
			out.skip("%-22s installing the unit needs sudo (the commands are printed at the end)", "systemd")
		}
	}
}

// checkPort reports a port that is already taken, which is the other way a
// finished setup fails at the first start.
// checkPort will not let setup write a configuration that cannot start.
//
// Setup starts the service at the end, so a port somebody else already holds
// is not a warning: it is that failure, arriving a minute early and with
// nothing written yet. When there is somebody to ask, the next free port is
// offered rather than the whole conversation thrown away.
func checkPort(out *output, p wizard.Prompter, a *wizard.Answers, canAsk bool) error {
	port := wizard.ListenPort(*a)
	if port == "" {
		return nil
	}
	switch err := portInUse(port); {
	case err == nil:
		out.success("%-22s %s is free", "port", port)
		return nil
	case isPermission(err):
		// Ports below 1024 need a capability the wizard has already arranged
		// for in the unit it writes, and Docker publishes them regardless.
		out.skip("%-22s %s needs root or CAP_NET_BIND_SERVICE, which the generated %s handles",
			"port", port, deploymentName(a.Deployment))
		return nil
	}

	out.fail("%-22s %s is already in use", "port", port)
	free := nextFreePort(port)
	if canAsk && free != "" {
		ok, err := p.Confirm("Use port "+free+" instead?",
			"Something else is already listening on "+port+".", true)
		if err != nil {
			return err
		}
		if ok {
			a.Port = free
			out.success("%-22s %s is free", "port", free)
			return nil
		}
	}
	if free == "" {
		return fmt.Errorf("port %s is already in use; stop whatever is listening, "+
			"or run again with --port <port>", port)
	}
	return fmt.Errorf("port %s is already in use; stop whatever is listening, "+
		"or run again with --port %s", port, free)
}

// portInUse reports whether something is already listening, by trying to
// listen itself: no other check consults the whole story (another process, a
// container, a socket in TIME_WAIT, a permission).
func portInUse(port string) error {
	closer, err := listenOn(net.JoinHostPort("", port))
	if err != nil {
		return err
	}
	return closer.Close()
}

// nextFreePort looks just above the busy one, where a person would look. It
// gives up rather than wander: an empty answer means "say so and stop".
func nextFreePort(port string) string {
	n, err := strconv.Atoi(port)
	if err != nil {
		return ""
	}
	for candidate := n + 1; candidate <= n+20 && candidate <= 65535; candidate++ {
		next := strconv.Itoa(candidate)
		if portInUse(next) == nil {
			return next
		}
	}
	return ""
}

func deploymentName(deployment string) string {
	if deployment == wizard.DeploySystemd {
		return "unit"
	}
	return "compose file"
}

// usable reports whether a directory exists and can be written to. Checking
// the mode bits is not the same thing: ownership, ACLs, a read-only mount and
// SELinux all have a say, and only writing a file consults all of them.
func usable(dir string) error {
	info, err := os.Stat(dir)
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return errors.New(dir + " is not a directory")
	}
	probe, err := os.CreateTemp(dir, ".godrop-write-check-*")
	if err != nil {
		return err
	}
	_ = probe.Close()
	return os.Remove(probe.Name())
}

// sudoAvailable reports whether sudo could be used, which it cannot be if this
// is already root or sudo is not installed.
func sudoAvailable() (string, bool) {
	if euid() == 0 {
		return "", false
	}
	path, err := lookPath("sudo")
	if err != nil {
		return "", false
	}
	return path, true
}

func isPermission(err error) bool {
	return errors.Is(err, os.ErrPermission) || strings.Contains(err.Error(), "permission denied")
}
