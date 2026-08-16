package cli

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/fatihbaltaci/GoDrop/internal/updater"
	"github.com/fatihbaltaci/GoDrop/internal/wizard"
)

// existingInstall writes what setup would have written, so that a second run
// finds an installation to update.
func existingInstall(t *testing.T, deployment string) string {
	t.Helper()
	// The update ends in the same verification setup runs, and nothing is
	// listening for these tests, so there is no point waiting for it.
	original := healthWait
	healthWait = 10 * time.Millisecond
	t.Cleanup(func() { healthWait = original })

	dir := t.TempDir()
	a := wizard.Defaults()
	a.Deployment = deployment
	a.Token = "gd_existing"
	a.Port = "48123"
	if deployment == wizard.DeploySystemd {
		a.DataDir = filepath.Join(dir, "data")
	}
	wizard.Finalise(&a)
	if _, err := wizard.Write(dir, wizard.Files(a, "/usr/local/bin/godrop"), false); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestInitOverAnExistingInstallationUpdatesIt(t *testing.T) {
	tooling := &stubTooling{found: map[string]bool{"docker": true}}
	tooling.install(t)
	dir := existingInstall(t, wizard.DeployCompose)
	// The token file would be rewritten by a second setup; this proves it is
	// not, because the wizard never runs.
	before, err := os.ReadFile(filepath.Join(dir, ".env"))
	if err != nil {
		t.Fatal(err)
	}

	code, out, stderr := run(t, testBuild(), "init", "--no-input", "--out-dir", dir)
	if code != 0 {
		t.Fatalf("exit = %d: %s", code, stderr)
	}
	if !strings.Contains(out, "already set up") || !strings.Contains(out, "--force") {
		t.Errorf("output should say what it found and how to start over:\n%s", out)
	}
	after, err := os.ReadFile(filepath.Join(dir, ".env"))
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Error("an update must leave the configuration and the token alone")
	}
	// Pull, then up: without the pull, :latest stays whatever it was.
	if len(tooling.ran) < 2 ||
		!strings.Contains(tooling.ran[0], "compose --project-directory "+dir+" pull") ||
		!strings.Contains(tooling.ran[1], "compose --project-directory "+dir+" up -d") {
		t.Errorf("ran = %v", tooling.ran)
	}
}

func TestInitWithForceStartsOverInstead(t *testing.T) {
	tooling := &stubTooling{found: map[string]bool{"docker": true}}
	tooling.install(t)
	dir := existingInstall(t, wizard.DeployCompose)

	code, out, stderr := run(t, testBuild(), "init", "--no-input", "--out-dir", dir, "--force", "--no-external-check")
	if code != 0 {
		t.Fatalf("exit = %d: %s", code, stderr)
	}
	if strings.Contains(out, "already set up") {
		t.Errorf("--force means configure from scratch:\n%s", out)
	}
	if !strings.Contains(out, "Your API token") {
		t.Errorf("starting over generates a new token:\n%s", out)
	}
}

func TestInitOverAnExistingInstallationInJSON(t *testing.T) {
	tooling := &stubTooling{found: map[string]bool{"docker": true}}
	tooling.install(t)
	dir := existingInstall(t, wizard.DeployCompose)

	code, out, stderr := run(t, testBuild(), "init", "--no-input", "--out-dir", dir, "--json")
	if code != 0 {
		t.Fatalf("exit = %d: %s", code, stderr)
	}
	var got struct {
		ConfigDir  string `json:"config_dir"`
		Deployment string `json:"deployment"`
	}
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("output is not JSON: %v\n%s", err, out)
	}
	if got.ConfigDir != dir || got.Deployment != wizard.DeployCompose {
		t.Errorf("got %+v", got)
	}
}

func TestUpdateSaysWhenDockerIsMissing(t *testing.T) {
	tooling := &stubTooling{found: map[string]bool{}}
	tooling.install(t)
	dir := existingInstall(t, wizard.DeployCompose)

	var buf strings.Builder
	if err := upgrade(context.Background(), &output{w: &buf}, testBuild(), dir); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), "docker is not installed") {
		t.Errorf("output = %s", buf.String())
	}
	if len(tooling.ran) != 0 {
		t.Errorf("nothing should have been run: %v", tooling.ran)
	}
}

func TestUpdateReportsAFailingCompose(t *testing.T) {
	dir := existingInstall(t, wizard.DeployCompose)
	for _, tc := range []struct {
		name string
		fail string
		want string
	}{
		{"pull", "pull", "docker compose pull failed"},
		{"up", "up", "docker compose up failed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tooling := &stubTooling{found: map[string]bool{"docker": true}}
			tooling.install(t)
			failing := runCommand
			runCommand = func(ctx context.Context, name string, args ...string) error {
				if strings.Contains(strings.Join(args, " "), " "+tc.fail) {
					return errAlreadyThere
				}
				return failing(ctx, name, args...)
			}
			err := upgrade(context.Background(), &output{w: &strings.Builder{}}, testBuild(), dir)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("err = %v, want %q", err, tc.want)
			}
		})
	}
}

var errAlreadyThere = errors.New("no such image")

func TestUpdateInJSONStillFailsOutLoud(t *testing.T) {
	tooling := &stubTooling{found: map[string]bool{"docker": true}, runErr: errAlreadyThere}
	tooling.install(t)
	dir := existingInstall(t, wizard.DeployCompose)

	err := upgrade(context.Background(), &output{w: io.Discard, json: true}, testBuild(), dir)
	if err == nil || !strings.Contains(err.Error(), "docker compose pull failed") {
		t.Errorf("err = %v", err)
	}
}

func TestUpdateRestartsASystemdService(t *testing.T) {
	dir := existingInstall(t, wizard.DeploySystemd)
	if got := deploymentAt(dir); got != wizard.DeploySystemd {
		t.Fatalf("deployment = %q", got)
	}

	// Not root: the restart is a command to run, not something to attempt.
	tooling := &stubTooling{found: map[string]bool{"systemctl": true}}
	tooling.install(t)
	var buf strings.Builder
	if err := upgradeService(context.Background(), &output{w: &buf}, dir, wizard.DeploySystemd); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), "sudo systemctl restart godrop") {
		t.Errorf("output = %s", buf.String())
	}

	// As root, it is done here.
	asRoot := &stubTooling{found: map[string]bool{"systemctl": true}, root: true}
	asRoot.install(t)
	buf.Reset()
	if err := upgradeService(context.Background(), &output{w: &buf}, dir, wizard.DeploySystemd); err != nil {
		t.Fatal(err)
	}
	if len(asRoot.ran) != 1 || asRoot.ran[0] != "systemctl restart godrop" {
		t.Errorf("ran = %v", asRoot.ran)
	}

	failing := &stubTooling{found: map[string]bool{"systemctl": true}, root: true, runErr: errAlreadyThere}
	failing.install(t)
	err := upgradeService(context.Background(), &output{w: io.Discard}, dir, wizard.DeploySystemd)
	if err == nil || !strings.Contains(err.Error(), "systemctl restart godrop failed") {
		t.Errorf("err = %v", err)
	}
}

func TestUpdateOfAPlainBinaryInstallation(t *testing.T) {
	dir := existingInstall(t, wizard.DeployEnv)
	if got := deploymentAt(dir); got != wizard.DeployEnv {
		t.Fatalf("deployment = %q", got)
	}
	var buf strings.Builder
	if err := upgradeService(context.Background(), &output{w: &buf}, dir, wizard.DeployEnv); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), "godrop serve") {
		t.Errorf("output = %s", buf.String())
	}
}

func TestUpdateMovesTheServiceOnTooWhenThereIsOne(t *testing.T) {
	tooling := &stubTooling{found: map[string]bool{"docker": true}}
	tooling.install(t)
	dir := existingInstall(t, wizard.DeployCompose)
	setHome(t, filepath.Dir(dir))
	// ConfigDir is where update looks for the installation, so the .env has to
	// be where a real one would be.
	config := configDirForTest(t)
	if err := os.MkdirAll(config, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{".env", "docker-compose.yml"} {
		body, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(config, name), body, 0o600); err != nil {
			t.Fatal(err)
		}
	}

	stubUpdater(t, nil, func(_ context.Context, current string, _ updater.Options) (updater.Result, error) {
		return updater.Result{From: current, To: "v9.9.9", Path: "/usr/local/bin/godrop", Updated: true}, nil
	})
	code, out, stderr := run(t, testBuild(), "update")
	if code != 0 {
		t.Fatalf("exit = %d: %s", code, stderr)
	}
	if !strings.Contains(out, "newest image") {
		t.Errorf("the container should be updated too:\n%s", out)
	}
	if len(tooling.ran) != 2 {
		t.Errorf("ran = %v, want a pull and an up", tooling.ran)
	}
}

func TestTheSummarySaysWhereEverythingIs(t *testing.T) {
	tooling := &stubTooling{
		found:     map[string]bool{"docker": true},
		container: "godrop-godrop-1",
		mount:     "docker volume godrop_godrop-data",
	}
	tooling.install(t)
	dir := existingInstall(t, wizard.DeployCompose)

	var buf strings.Builder
	if err := upgrade(context.Background(), &output{w: &buf}, testBuild(), dir); err != nil {
		t.Fatal(err)
	}
	text := buf.String()
	for _, want := range []string{
		"location", dir,
		"docker compose, container godrop-godrop-1",
		"docker volume godrop_godrop-data",
		"http://localhost:48123",
		"Use it", "-F \"file=@" + filepath.Join(dir, wizard.SampleName) + "\"",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("the summary should mention %q:\n%s", want, text)
		}
	}
	// An installation from before there was a picture gets one, so that the
	// example above is a command that works.
	if _, err := os.Stat(filepath.Join(dir, wizard.SampleName)); err != nil {
		t.Errorf("the sample should have been written: %v", err)
	}
}

func TestTheSummarySaysLessWhenDockerCannotAnswer(t *testing.T) {
	dir := existingInstall(t, wizard.DeployCompose)
	a := answersFromEnv(dir)

	// No docker at all.
	none := &stubTooling{found: map[string]bool{}}
	none.install(t)
	var buf strings.Builder
	printInstallation(context.Background(), &output{w: &buf}, a, dir, wizard.DeployCompose)
	if !strings.Contains(buf.String(), "docker compose") || strings.Contains(buf.String(), "container ") {
		t.Errorf("output = %s", buf.String())
	}

	// Docker, but nothing running: no name to print, and no mount either.
	stopped := &stubTooling{found: map[string]bool{"docker": true}}
	stopped.install(t)
	buf.Reset()
	printInstallation(context.Background(), &output{w: &buf}, a, dir, wizard.DeployCompose)
	if strings.Contains(buf.String(), "container ") {
		t.Errorf("output = %s", buf.String())
	}

	// Running, but the mount cannot be read: the name is still worth printing.
	half := &stubTooling{found: map[string]bool{"docker": true}, container: "x-godrop-1"}
	half.install(t)
	buf.Reset()
	printInstallation(context.Background(), &output{w: &buf}, a, dir, wizard.DeployCompose)
	if !strings.Contains(buf.String(), "container x-godrop-1") || strings.Contains(buf.String(), "uploads") {
		t.Errorf("output = %s", buf.String())
	}

	// Running, and the question about its mounts failing: still the name.
	noMount := &stubTooling{
		found: map[string]bool{"docker": true}, container: "x-godrop-1", inspectErr: errAlreadyThere,
	}
	noMount.install(t)
	buf.Reset()
	printInstallation(context.Background(), &output{w: &buf}, a, dir, wizard.DeployCompose)
	if !strings.Contains(buf.String(), "container x-godrop-1") || strings.Contains(buf.String(), "uploads") {
		t.Errorf("output = %s", buf.String())
	}

	// The question itself failing is the same as no answer.
	broken := &stubTooling{found: map[string]bool{"docker": true}, outErr: errAlreadyThere}
	broken.install(t)
	buf.Reset()
	printInstallation(context.Background(), &output{w: &buf}, a, dir, wizard.DeployCompose)
	if strings.Contains(buf.String(), "container ") {
		t.Errorf("output = %s", buf.String())
	}
}

func TestUpdateCarriesOnWhenThePictureCannotBeWritten(t *testing.T) {
	requireStrictPermissions(t)
	tooling := &stubTooling{found: map[string]bool{"docker": true}, container: "godrop-godrop-1"}
	tooling.install(t)
	dir := existingInstall(t, wizard.DeployCompose)
	// An installation from before there was a picture, in a directory that
	// cannot be written to now.
	if err := os.Remove(filepath.Join(dir, wizard.SampleName)); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })

	var buf strings.Builder
	if err := upgrade(context.Background(), &output{w: &buf}, testBuild(), dir); err != nil {
		t.Fatal(err)
	}
	// No picture to name, so the example falls back to the documentation's.
	if !strings.Contains(buf.String(), "file=@photo.jpg") {
		t.Errorf("output = %s", buf.String())
	}
}

func TestTheSummaryNamesTheUnitAndTheDataDirectory(t *testing.T) {
	dir := existingInstall(t, wizard.DeploySystemd)
	a := answersFromEnv(dir)
	a.DataDir = "/var/lib/godrop"

	var buf strings.Builder
	printInstallation(context.Background(), &output{w: &buf}, a, dir, wizard.DeploySystemd)
	if !strings.Contains(buf.String(), "systemd unit godrop") || !strings.Contains(buf.String(), "/var/lib/godrop") {
		t.Errorf("output = %s", buf.String())
	}
}

func TestReadEnvFileIgnoresWhatIsNotAValue(t *testing.T) {
	dir := t.TempDir()
	body := "# a comment\n\nGODROP_ADDR=:48123\n  GODROP_BASE_URL = https://files.example.com \nnonsense\n"
	if err := os.WriteFile(filepath.Join(dir, ".env"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	a := answersFromEnv(dir)
	if a.Port != "48123" || a.BaseURL != "https://files.example.com" {
		t.Errorf("answers = %+v", a)
	}
	// No file at all, and the defaults are still something to check against.
	if got := answersFromEnv(t.TempDir()); got.Port != wizard.Defaults().Port {
		t.Errorf("port = %q, want the default", got.Port)
	}
}
