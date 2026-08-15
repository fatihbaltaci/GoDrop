package updater

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// managedBy reports the package manager or platform that owns this binary,
// with the command that updates it.
//
// Replacing a file another tool believes it owns leaves a system whose state
// nobody can explain: the package database says one version, the disk holds
// another, and the next upgrade quietly reverts the update.
func managedBy(binary string) (manager, hint string) {
	switch {
	case inContainer():
		return "a container image",
			"pull a new image instead: docker pull ghcr.io/" + Repo + ":latest"
	case dpkgOwns(binary):
		return "dpkg", "update it with your package manager: sudo apt update && sudo apt install --only-upgrade godrop"
	case rpmOwns(binary):
		return "rpm", "update it with your package manager: sudo dnf upgrade godrop"
	case underHomebrew(binary):
		return "Homebrew", "update it with: brew upgrade godrop"
	}
	return "", ""
}

// lookup is a seam so the package-manager branches can be exercised without
// dpkg or rpm being installed.
var lookup = func(name string, args ...string) error {
	if _, err := exec.LookPath(name); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	// The command and its flags are literals; only the path is variable, and
	// no shell interprets it.
	return exec.CommandContext(ctx, name, args...).Run() //nolint:gosec // G204
}

func dpkgOwns(binary string) bool { return lookup("dpkg", "-S", binary) == nil }
func rpmOwns(binary string) bool  { return lookup("rpm", "-qf", binary) == nil }

func underHomebrew(binary string) bool {
	resolved, err := filepath.EvalSymlinks(binary)
	if err != nil {
		resolved = binary
	}
	for _, prefix := range []string{"/opt/homebrew/", "/usr/local/Cellar/", "/home/linuxbrew/"} {
		if strings.HasPrefix(resolved, prefix) {
			return true
		}
	}
	return false
}

// inContainer reports whether we are running inside a container. Updating the
// binary there would be lost the moment the container is recreated.
var inContainer = func() bool { return inContainerAt("/.dockerenv", "/proc/1/cgroup") }

// inContainerAt takes the two markers as arguments so both of them can be
// examined on a machine that is not in a container.
func inContainerAt(dockerEnv, cgroup string) bool {
	if _, err := os.Stat(dockerEnv); err == nil {
		return true
	}
	data, err := os.ReadFile(cgroup)
	return err == nil && (strings.Contains(string(data), "docker") || strings.Contains(string(data), "containerd"))
}

// execVersion runs a freshly downloaded binary to prove it works before it is
// installed. It is the last gate: whatever else went wrong, an update never
// replaces a working binary with one that cannot start.
func execVersion(path string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, path, "version").CombinedOutput() //nolint:gosec // the path is the file we just wrote
	return string(out), err
}
