package cli

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/fatihbaltaci/GoDrop/internal/wizard"
)

func TestTokenListFindsTheTokenSetupGaveYou(t *testing.T) {
	// The shell has none of the service's environment, so without reading the
	// generated .env this says "No tokens yet" to somebody holding one.
	dir := installAt(t, wizard.DeployCompose, "")
	t.Setenv("GODROP_TOKENS", "")
	t.Setenv("GODROP_DATA_DIR", "")

	code, out, stderr := run(t, testBuild(), "token", "list")
	if code != 0 {
		t.Fatalf("exit = %d: %s", code, stderr)
	}
	if strings.Contains(out, "No tokens yet") {
		t.Errorf("there is a token, in %s/.env:\n%s", dir, out)
	}
	if !strings.Contains(out, filepath.Join(dir, ".env")) || !strings.Contains(out, "setup gave you") {
		t.Errorf("output should say where that token is:\n%s", out)
	}
	if !strings.Contains(out, "run --rm godrop token list") {
		t.Errorf("named tokens are in the container:\n%s", out)
	}
}

func TestTokenCreateRefusesToWriteWhereNothingReads(t *testing.T) {
	installAt(t, wizard.DeployCompose, "")
	t.Setenv("GODROP_TOKENS", "")
	t.Setenv("GODROP_DATA_DIR", "")

	code, _, stderr := run(t, testBuild(), "token", "create", "--name", "claude-code")
	if code == 0 {
		t.Fatal("a token written on the host is one the container never sees")
	}
	if !strings.Contains(stderr, "run --rm godrop token create --name claude-code") {
		t.Errorf("stderr should carry the command that works: %q", stderr)
	}

	code, _, stderr = run(t, testBuild(), "token", "revoke", "ci")
	if code == 0 || !strings.Contains(stderr, "run --rm godrop token revoke ci") {
		t.Errorf("exit = %d, stderr = %q", code, stderr)
	}

	// Without --name the message still has to name something runnable.
	_, _, stderr = run(t, testBuild(), "token", "create")
	if !strings.Contains(stderr, "token create --name default") {
		t.Errorf("stderr = %q", stderr)
	}
}

func TestTokenCommandsUseTheInstallationsDataDirectory(t *testing.T) {
	dir := installAt(t, wizard.DeployEnv, "")
	t.Setenv("GODROP_TOKENS", "")
	t.Setenv("GODROP_DATA_DIR", "")

	code, _, stderr := run(t, testBuild(), "token", "create", "--name", "ci")
	if code != 0 {
		t.Fatalf("exit = %d: %s", code, stderr)
	}
	// Not ./data next to whatever directory this shell happened to be in: the
	// one the service reads, from the generated .env.
	values := readEnvFile(filepath.Join(dir, ".env"))
	if _, err := os.Stat(filepath.Join(values["GODROP_DATA_DIR"], "tokens.json")); err != nil {
		t.Errorf("the token should be where the service looks: %v", err)
	}
}

func TestAnExplicitDataDirectoryWins(t *testing.T) {
	installAt(t, wizard.DeployCompose, "")
	elsewhere := t.TempDir()
	t.Setenv("GODROP_TOKENS", "")
	t.Setenv("GODROP_DATA_DIR", "")

	// A flag is the operator saying where to look, so the installation is not
	// consulted at all and the container never comes into it.
	code, _, stderr := run(t, testBuild(), "token", "create", "--name", "ci", "--data-dir", elsewhere)
	if code != 0 {
		t.Fatalf("exit = %d: %s", code, stderr)
	}
	if _, err := os.Stat(filepath.Join(elsewhere, "tokens.json")); err != nil {
		t.Errorf("the flag should decide: %v", err)
	}

	// So is GODROP_DATA_DIR.
	another := t.TempDir()
	t.Setenv("GODROP_DATA_DIR", another)
	if code, _, stderr := run(t, testBuild(), "token", "create", "--name", "ci2"); code != 0 {
		t.Fatalf("exit = %d: %s", code, stderr)
	}
	if _, err := os.Stat(filepath.Join(another, "tokens.json")); err != nil {
		t.Errorf("the environment should decide: %v", err)
	}
}

func TestSudoFindsTheInstallationOfWhoeverRanSetup(t *testing.T) {
	// Replacing a binary in /usr/local/bin needs root, but the installation
	// belongs to the person who ran setup and root's own directory is empty.
	dir := installAt(t, wizard.DeployCompose, "")
	home := filepath.Dir(dir)

	original := lookupHome
	lookupHome = func(name string) (string, error) {
		if name != "ubuntu" {
			return "", errAlreadyThere
		}
		return home, nil
	}
	t.Cleanup(func() { lookupHome = original })

	// Root, whose own configuration directory has nothing in it.
	setHome(t, t.TempDir())
	if got := installationDir(); installedAt(got) {
		t.Fatalf("this test needs an empty configuration directory, got %s", got)
	}
	t.Setenv("SUDO_USER", "ubuntu")
	if got := installationDir(); got != dir {
		t.Errorf("installationDir = %q, want the one sudo came from (%q)", got, dir)
	}

	// A user the password database does not know, and one who has never run
	// setup: both leave the answer where it was.
	t.Setenv("SUDO_USER", "nobody")
	if got := installationDir(); installedAt(got) {
		t.Errorf("installationDir = %q, want no installation", got)
	}
	lookupHome = func(string) (string, error) { return t.TempDir(), nil }
	if got := installationDir(); installedAt(got) {
		t.Errorf("installationDir = %q, want no installation", got)
	}
}

func TestLookingUpAHomeDirectory(t *testing.T) {
	// The real thing, since the seam above replaces it everywhere else: root
	// is the one account every unix has.
	if runtime.GOOS == "windows" {
		t.Skip("no password database to read")
	}
	if home, err := lookupHome("root"); err != nil || home == "" {
		t.Errorf("lookupHome(root) = %q, %v", home, err)
	}
	if _, err := lookupHome("no-such-user-anywhere"); err == nil {
		t.Error("an unknown account should be reported")
	}
}

func TestTokenListWithoutAnyInstallation(t *testing.T) {
	t.Setenv("GODROP_DATA_DIR", t.TempDir())
	t.Setenv("GODROP_TOKENS", "")
	_, out, _ := run(t, testBuild(), "token", "list")
	if !strings.Contains(out, "No tokens yet") {
		t.Errorf("output = %s", out)
	}

	// Nothing said, and nothing set up either: the default data directory is
	// all there is to go on.
	t.Setenv("GODROP_DATA_DIR", "")
	setHome(t, t.TempDir())
	if _, out, _ := run(t, testBuild(), "token", "list"); !strings.Contains(out, "No tokens yet") {
		t.Errorf("output = %s", out)
	}
}
