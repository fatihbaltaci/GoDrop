package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/fatihbaltaci/GoDrop/internal/wizard"
)

// composeInstall is an installation with a running container, for the commands
// that change one and then restart it.
func composeInstall(t *testing.T) (dir string, tooling *stubTooling) {
	t.Helper()
	tooling = &stubTooling{found: map[string]bool{"docker": true}, container: "godrop-godrop-1"}
	tooling.install(t)
	return installAt(t, wizard.DeployCompose, ""), tooling
}

func TestASettingNamedOnTheCommandLineIsChanged(t *testing.T) {
	dir, tooling := composeInstall(t)
	before, err := os.ReadFile(filepath.Join(dir, ".env"))
	if err != nil {
		t.Fatal(err)
	}

	code, out, stderr := run(t, testBuild(), "init", "--base-url", "godrop.example.com")
	if code != 0 {
		t.Fatalf("exit = %d: %s", code, stderr)
	}
	if !strings.Contains(out, "Changing") || !strings.Contains(out, "https://godrop.example.com") {
		t.Errorf("output should say what changed:\n%s", out)
	}

	values := readEnvFile(filepath.Join(dir, ".env"))
	if values["GODROP_BASE_URL"] != "https://godrop.example.com" {
		t.Errorf("base url = %q, want the bare host normalised", values["GODROP_BASE_URL"])
	}
	// Everything nobody mentioned stays, the token above all.
	if values["GODROP_TOKENS"] != "gd_a1b2c3d4e5f60718293a4b5c6d7e8f90" {
		t.Errorf("the token should be untouched: %q", values["GODROP_TOKENS"])
	}
	if !strings.Contains(string(before), "GODROP_MAX_FILE_SIZE=100MB") ||
		values["GODROP_MAX_FILE_SIZE"] != "100MB" {
		t.Errorf("the other settings should be untouched: %v", values)
	}
	// And the service is restarted into it, or the file would be a wish.
	if !strings.Contains(strings.Join(tooling.ran, "|"), "up -d") {
		t.Errorf("ran = %v", tooling.ran)
	}
}

func TestChangingThePortReachesTheFileThatPublishesIt(t *testing.T) {
	dir, _ := composeInstall(t)

	code, out, stderr := run(t, testBuild(), "init", "--port", "48262")
	if code != 0 {
		t.Fatalf("exit = %d: %s", code, stderr)
	}
	if !strings.Contains(out, "listen port") || !strings.Contains(out, "48262") {
		t.Errorf("output = %s", out)
	}
	if got := readEnvFile(filepath.Join(dir, ".env"))["GODROP_ADDR"]; got != ":48262" {
		t.Errorf("addr = %q", got)
	}
	compose, err := os.ReadFile(filepath.Join(dir, "docker-compose.yml"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(compose), `"48262:48262"`) {
		t.Errorf("the published port should have moved too:\n%s", compose)
	}
	// The volume is where the uploads are: a rewrite must not swap it for an
	// empty one.
	if !strings.Contains(string(compose), "godrop-data:/data") {
		t.Errorf("the volume should be the one it was:\n%s", compose)
	}
}

func TestAHostDirectoryStaysWhereItIs(t *testing.T) {
	dir, _ := composeInstall(t)
	compose := filepath.Join(dir, "docker-compose.yml")
	body, err := os.ReadFile(compose)
	if err != nil {
		t.Fatal(err)
	}
	// An installation from before named volumes, keeping its uploads on the
	// host.
	onHost := strings.ReplaceAll(string(body), "- godrop-data:/data", "- /srv/godrop:/data")
	if err := os.WriteFile(compose, []byte(onHost), 0o644); err != nil { //nolint:gosec
		t.Fatal(err)
	}

	if code, _, stderr := run(t, testBuild(), "init", "--port", "48263"); code != 0 {
		t.Fatalf("exit = %d: %s", code, stderr)
	}
	after, err := os.ReadFile(compose)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(after), "/srv/godrop:/data") {
		t.Errorf("the uploads must not be moved onto a fresh volume:\n%s", after)
	}
	if strings.Contains(string(after), "godrop-data:/data") {
		t.Errorf("the named volume should not have appeared:\n%s", after)
	}
}

func TestChangingHowTheCertificateIsGot(t *testing.T) {
	dir, _ := composeInstall(t)

	code, out, stderr := run(t, testBuild(), "init", "--tls", "proxy")
	if code != 0 {
		t.Fatalf("exit = %d: %s", code, stderr)
	}
	if !strings.Contains(out, "tls") || !strings.Contains(out, "proxy") {
		t.Errorf("output = %s", out)
	}
	if got := readEnvFile(filepath.Join(dir, ".env"))["GODROP_TLS"]; got != "proxy" {
		t.Errorf("tls = %q", got)
	}
}

func TestAnInstallationWithNoComposeFileToRewrite(t *testing.T) {
	tooling := &stubTooling{found: map[string]bool{}}
	tooling.install(t)
	dir := installAt(t, wizard.DeployEnv, "")

	code, _, stderr := run(t, testBuild(), "init", "--port", "48264")
	if code != 0 {
		t.Fatalf("exit = %d: %s", code, stderr)
	}
	if got := readEnvFile(filepath.Join(dir, ".env"))["GODROP_ADDR"]; got != ":48264" {
		t.Errorf("addr = %q", got)
	}
}

func TestReconfigureStopsWhenTheFileCannotBeWritten(t *testing.T) {
	requireStrictPermissions(t)
	dir, _ := composeInstall(t)
	path := filepath.Join(dir, ".env")
	if err := os.Chmod(path, 0o400); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(path, 0o600) })

	if code, _, stderr := run(t, testBuild(), "init", "--base-url", "https://x.example.com"); code == 0 {
		t.Errorf("exit = %d, stderr = %q", code, stderr)
	}
}

func TestAComposeFileThatCannotBeRewritten(t *testing.T) {
	requireStrictPermissions(t)
	dir, _ := composeInstall(t)
	compose := filepath.Join(dir, "docker-compose.yml")
	if err := os.Chmod(compose, 0o400); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(compose, 0o600) })

	// The port moved in the .env and cannot move in the compose file: saying
	// so beats a service published on the old port with no explanation.
	if code, _, stderr := run(t, testBuild(), "init", "--port", "48265"); code == 0 {
		t.Errorf("exit = %d, stderr = %q", code, stderr)
	}
}

func TestASettingThatIsAlreadyWhatItIs(t *testing.T) {
	dir, _ := composeInstall(t)
	values := readEnvFile(filepath.Join(dir, ".env"))

	_, out, _ := run(t, testBuild(), "init", "--max-file-size", values["GODROP_MAX_FILE_SIZE"])
	if !strings.Contains(out, "unchanged") {
		t.Errorf("output = %s", out)
	}
}

func TestTelemetryAsASetting(t *testing.T) {
	dir, _ := composeInstall(t)
	path := filepath.Join(dir, ".env")

	if code, _, stderr := run(t, testBuild(), "init", "--telemetry=false"); code != 0 {
		t.Fatalf("exit = %d: %s", code, stderr)
	}
	if got := readEnvFile(path)["GODROP_TELEMETRY"]; got != "off" {
		t.Errorf("telemetry = %q", got)
	}
	// Back on is the line going away, since on is the default.
	if code, _, stderr := run(t, testBuild(), "init", "--telemetry=true"); code != 0 {
		t.Fatalf("exit = %d: %s", code, stderr)
	}
	if got := readEnvFile(path)["GODROP_TELEMETRY"]; got != "" {
		t.Errorf("telemetry = %q, want the setting removed", got)
	}
	// Twice in a row is not a change at all.
	_, out, _ := run(t, testBuild(), "init", "--telemetry=true")
	if !strings.Contains(out, "unchanged") {
		t.Errorf("output = %s", out)
	}
}

func TestASettingThatWouldStopTheServiceComingBack(t *testing.T) {
	dir, _ := composeInstall(t)
	before := readEnvFile(filepath.Join(dir, ".env"))

	for _, args := range [][]string{
		{"init", "--base-url", "not a url"},
		{"init", "--port", "no"},
		{"init", "--max-file-size", "lots"},
		{"init", "--max-total-size", "some"},
		{"init", "--retention", "forever"},
		{"init", "--tls", "maybe"},
		{"init", "--tls-cert", filepath.Join(dir, "nothing.pem")},
	} {
		code, _, stderr := run(t, testBuild(), args...)
		if code == 0 || !strings.Contains(stderr, args[1]) {
			t.Errorf("%v: exit = %d, stderr = %q", args, code, stderr)
		}
	}
	if after := readEnvFile(filepath.Join(dir, ".env")); after["GODROP_BASE_URL"] != before["GODROP_BASE_URL"] {
		t.Error("nothing should have been written")
	}
}

func TestTheOtherLinesOfTheEnvFileSurvive(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".env")
	body := "# a comment\nGODROP_TOKENS=gd_keep\nGODROP_CORS_ORIGINS=https://app.example.com\n" +
		"GODROP_BASE_URL=http://old\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}

	after, err := applyToEnvFile(path, map[string]string{
		"GODROP_BASE_URL":  "https://new",
		"GODROP_RETENTION": "30d", // not there yet: appended
		"GODROP_TELEMETRY": "",    // not there either: nothing to remove
	})
	if err != nil {
		t.Fatal(err)
	}
	if after["GODROP_BASE_URL"] != "https://new" || after["GODROP_RETENTION"] != "30d" {
		t.Errorf("values = %v", after)
	}
	if after["GODROP_TOKENS"] != "gd_keep" || after["GODROP_CORS_ORIGINS"] != "https://app.example.com" {
		t.Errorf("hand-written settings should survive: %v", after)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), "# a comment") {
		t.Errorf("comments should survive:\n%s", raw)
	}
	if strings.Contains(string(raw), "GODROP_TELEMETRY") {
		t.Errorf("an empty value should not add a line:\n%s", raw)
	}

	// Removing one is how a setting goes back to its default.
	if _, err := applyToEnvFile(path, map[string]string{"GODROP_RETENTION": ""}); err != nil {
		t.Fatal(err)
	}
	if got := readEnvFile(path)["GODROP_RETENTION"]; got != "" {
		t.Errorf("retention = %q, want the line gone", got)
	}
}

func TestAnEnvFileThatCannotBeWritten(t *testing.T) {
	requireStrictPermissions(t)
	dir := t.TempDir()
	path := filepath.Join(dir, ".env")
	if err := os.WriteFile(path, []byte("GODROP_BASE_URL=http://old\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	// Read-only: a file this account can read and not replace.
	if err := os.Chmod(path, 0o400); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(path, 0o600) })

	if _, err := applyToEnvFile(path, map[string]string{"GODROP_BASE_URL": "https://new"}); err == nil {
		t.Error("a file that cannot be written should be reported")
	}
}

func TestReconfigureReportsAFileItCannotRead(t *testing.T) {
	dir, _ := composeInstall(t)
	if err := os.Remove(filepath.Join(dir, ".env")); err != nil {
		t.Fatal(err)
	}
	// installedAt is false now, so this goes through the setup path instead:
	// what matters is that the writer says so rather than carrying on.
	if _, err := applyToEnvFile(filepath.Join(dir, ".env"), map[string]string{"X": "1"}); err == nil {
		t.Error("a missing file should be reported")
	}
}

func TestTheVolumeAnUnreadableComposeFileWouldName(t *testing.T) {
	if got := hostVolumeIn("services:\n  godrop:\n    volumes:\n      - godrop-data:/data\n"); got != "" {
		t.Errorf("hostVolumeIn = %q, want the named volume", got)
	}
	if got := hostVolumeIn("      - /srv/godrop:/data"); got != "/srv/godrop" {
		t.Errorf("hostVolumeIn = %q", got)
	}
	if got := hostVolumeIn("nothing about volumes"); got != "" {
		t.Errorf("hostVolumeIn = %q", got)
	}
}
