package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/fatihbaltaci/GoDrop/internal/wizard"
)

func TestTokenListReadsBothSidesOfTheContainer(t *testing.T) {
	// The shell has none of the service's environment, and the named tokens
	// are in a volume: the list has to come from the container, and the one
	// setup handed over from the .env.
	tooling := &stubTooling{
		found:     map[string]bool{"docker": true},
		container: "godrop-godrop-1",
		says: map[string]string{
			"token list": `{"tokens":[{"name":"ci","created":"2026-08-15T10:00:00Z",` +
				`"last_used":"2026-08-16T09:00:00Z"}],"env_tokens":1}`,
		},
	}
	tooling.install(t)
	dir := installAt(t, wizard.DeployCompose, "")
	t.Setenv("GODROP_TOKENS", "")
	t.Setenv("GODROP_DATA_DIR", "")

	code, out, stderr := run(t, testBuild(), "token", "list")
	if code != 0 {
		t.Fatalf("exit = %d: %s", code, stderr)
	}
	if !strings.Contains(out, "ci") || strings.Contains(out, "No tokens yet") {
		t.Errorf("the container's tokens should be listed:\n%s", out)
	}
	if !strings.Contains(out, filepath.Join(dir, ".env")) || !strings.Contains(out, "setup gave you") {
		t.Errorf("output should say where the nameless one is:\n%s", out)
	}
	// A running container is asked directly rather than started again.
	if len(tooling.ranOut) == 0 || !strings.Contains(strings.Join(tooling.ranOut, "|"), "exec -T godrop /godrop token list") {
		t.Errorf("ranOut = %v", tooling.ranOut)
	}
	if strings.Contains(out, "docker compose") {
		t.Errorf("the command was run, not printed:\n%s", out)
	}
}

func TestTokenCreateGoesWhereTheFileIs(t *testing.T) {
	tooling := &stubTooling{
		found: map[string]bool{"docker": true},
		says:  map[string]string{"token create": `{"token":"gd_from_the_container","name":"claude-code"}`},
	}
	tooling.install(t)
	dir := installAt(t, wizard.DeployCompose, "")
	t.Setenv("GODROP_TOKENS", "")
	t.Setenv("GODROP_DATA_DIR", "")

	code, out, stderr := run(t, testBuild(), "token", "create", "--name", "claude-code")
	if code != 0 {
		t.Fatalf("exit = %d: %s", code, stderr)
	}
	if !strings.Contains(out, "gd_from_the_container") || !strings.Contains(out, "claude-code") {
		t.Errorf("the token the container made should be shown:\n%s", out)
	}
	// Nothing running, so a throwaway container reads the same volume.
	ran := strings.Join(tooling.ranOut, "|")
	if !strings.Contains(ran, "--project-directory "+dir) ||
		!strings.Contains(ran, "run --rm godrop token create --name claude-code --json") {
		t.Errorf("ranOut = %v", tooling.ranOut)
	}
	if _, err := os.Stat(filepath.Join(dir, "tokens.json")); err == nil {
		t.Error("nothing should have been written on this side")
	}
}

func TestTokenRevokeGoesWhereTheFileIs(t *testing.T) {
	tooling := &stubTooling{found: map[string]bool{"docker": true}, says: map[string]string{"token revoke": ""}}
	tooling.install(t)
	installAt(t, wizard.DeployCompose, "")
	t.Setenv("GODROP_TOKENS", "")
	t.Setenv("GODROP_DATA_DIR", "")

	code, out, stderr := run(t, testBuild(), "token", "revoke", "ci")
	if code != 0 {
		t.Fatalf("exit = %d: %s", code, stderr)
	}
	if !strings.Contains(out, "ci revoked") {
		t.Errorf("output = %s", out)
	}
	if !strings.Contains(strings.Join(tooling.ranOut, "|"), "token revoke ci") {
		t.Errorf("ranOut = %v", tooling.ranOut)
	}
}

func TestTokenCommandsSayWhenDockerIsNotThere(t *testing.T) {
	tooling := &stubTooling{found: map[string]bool{}}
	tooling.install(t)
	installAt(t, wizard.DeployCompose, "")
	t.Setenv("GODROP_TOKENS", "")
	t.Setenv("GODROP_DATA_DIR", "")

	code, _, stderr := run(t, testBuild(), "token", "create", "--name", "ci")
	if code == 0 || !strings.Contains(stderr, "docker is not on this machine") {
		t.Errorf("exit = %d, stderr = %q", code, stderr)
	}
}

func TestAnAnswerTheContainerCannotHaveMeant(t *testing.T) {
	tooling := &stubTooling{
		found: map[string]bool{"docker": true},
		says:  map[string]string{"token": "not json at all"},
	}
	tooling.install(t)
	installAt(t, wizard.DeployCompose, "")
	t.Setenv("GODROP_TOKENS", "")
	t.Setenv("GODROP_DATA_DIR", "")

	for _, args := range [][]string{{"token", "create", "--name", "ci"}, {"token", "list"}} {
		code, _, stderr := run(t, testBuild(), args...)
		if code == 0 || !strings.Contains(stderr, "could not read the answer from the container") {
			t.Errorf("%v: exit = %d, stderr = %q", args, code, stderr)
		}
	}
}

func TestDockerErrorsCarryWhatDockerSaid(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the failing command is a POSIX shell")
	}
	// A plain error is left as it was.
	if got := withStderr(errAlreadyThere); got != errAlreadyThere {
		t.Errorf("withStderr = %v", got)
	}
	// A command that failed is worth reporting with what it said about it,
	// which for docker is the difference between a typo and a permission.
	_, err := exec.Command("sh", "-c", "echo 'permission denied on docker.sock' >&2; exit 3").Output() //nolint:gosec
	if err == nil {
		t.Fatal("the command was supposed to fail")
	}
	if got := withStderr(err).Error(); !strings.Contains(got, "permission denied on docker.sock") {
		t.Errorf("withStderr = %q", got)
	}
}

func TestTokenCommandsReportAFailingDocker(t *testing.T) {
	tooling := &stubTooling{found: map[string]bool{"docker": true}, outErr: errAlreadyThere}
	tooling.install(t)
	installAt(t, wizard.DeployCompose, "")
	t.Setenv("GODROP_TOKENS", "")
	t.Setenv("GODROP_DATA_DIR", "")

	code, _, stderr := run(t, testBuild(), "token", "list")
	if code == 0 || !strings.Contains(stderr, "token list") {
		t.Errorf("exit = %d, stderr = %q", code, stderr)
	}
}

func TestTokenRevokeReportsAFailingContainer(t *testing.T) {
	tooling := &stubTooling{found: map[string]bool{"docker": true}, outErr: errAlreadyThere}
	tooling.install(t)
	installAt(t, wizard.DeployCompose, "")
	t.Setenv("GODROP_TOKENS", "")
	t.Setenv("GODROP_DATA_DIR", "")

	code, _, stderr := run(t, testBuild(), "token", "revoke", "ci")
	if code == 0 || !strings.Contains(stderr, "token revoke ci") {
		t.Errorf("exit = %d, stderr = %q", code, stderr)
	}
}

func TestTokenRevokeInJSON(t *testing.T) {
	tooling := &stubTooling{found: map[string]bool{"docker": true}, says: map[string]string{"token revoke": ""}}
	tooling.install(t)
	installAt(t, wizard.DeployCompose, "")
	t.Setenv("GODROP_TOKENS", "")
	t.Setenv("GODROP_DATA_DIR", "")

	_, out, _ := run(t, testBuild(), "token", "revoke", "ci", "--json")
	var got map[string]any
	if err := json.Unmarshal([]byte(out), &got); err != nil || got["revoked"] != "ci" {
		t.Errorf("output = %q (%v)", out, err)
	}
}

func TestTokenJSONReportsAWriteFailure(t *testing.T) {
	tooling := &stubTooling{
		found: map[string]bool{"docker": true},
		says: map[string]string{
			"token create": `{"token":"gd_x","name":"ci"}`,
			"token list":   `{"tokens":[],"env_tokens":0}`,
		},
	}
	tooling.install(t)
	installAt(t, wizard.DeployCompose, "")
	t.Setenv("GODROP_TOKENS", "")
	t.Setenv("GODROP_DATA_DIR", "")

	for _, args := range [][]string{
		{"token", "create", "--name", "ci", "--json"},
		{"token", "list", "--json"},
	} {
		var stderr bytes.Buffer
		if code := ExecuteWith(context.Background(), testBuild(), args, failingWriter{}, &stderr); code == 0 {
			t.Errorf("%v: a broken output stream should be reported", args)
		}
	}
}

func TestTokenJSONPassesTheContainersAnswerThrough(t *testing.T) {
	answer := `{"token":"gd_x","name":"ci","created":"2026-08-16T10:00:00Z","file":"/data/tokens.json"}`
	tooling := &stubTooling{
		found: map[string]bool{"docker": true},
		says:  map[string]string{"token create": answer, "token list": `{"tokens":[],"env_tokens":1}`},
	}
	tooling.install(t)
	installAt(t, wizard.DeployCompose, "")
	t.Setenv("GODROP_TOKENS", "")
	t.Setenv("GODROP_DATA_DIR", "")

	_, out, _ := run(t, testBuild(), "token", "create", "--name", "ci", "--json")
	var got map[string]any
	if err := json.Unmarshal([]byte(out), &got); err != nil || got["token"] != "gd_x" {
		t.Errorf("output = %q (%v)", out, err)
	}
	_, out, _ = run(t, testBuild(), "token", "list", "--json")
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Errorf("output = %q (%v)", out, err)
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
