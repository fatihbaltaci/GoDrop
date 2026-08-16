package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/fatihbaltaci/GoDrop/internal/wizard"
)

// Every command that reads or writes a file of the installation's has the same
// problem to solve: on a compose deployment that file is in a volume this
// machine cannot open. These are the commands, each doing the work rather than
// describing it.

func TestTelemetryOffReachesTheOneThatWouldSend(t *testing.T) {
	tooling := &stubTooling{
		found:     map[string]bool{"docker": true},
		container: "godrop-godrop-1",
		says:      map[string]string{"telemetry": `{"telemetry":"off"}`},
	}
	tooling.install(t)
	dir := installAt(t, wizard.DeployCompose, "")
	t.Setenv("GODROP_DATA_DIR", "")

	code, out, stderr := run(t, testBuild(), "telemetry", "off")
	if code != 0 {
		t.Fatalf("exit = %d: %s", code, stderr)
	}
	if !strings.Contains(out, "telemetry off") {
		t.Errorf("output = %s", out)
	}
	if !strings.Contains(strings.Join(tooling.ranOut, "|"), "telemetry off --json") {
		t.Errorf("ranOut = %v", tooling.ranOut)
	}
	// A marker written on this side would switch off a heartbeat nothing is
	// sending, and leave the container's own where it was.
	if _, err := os.Stat(filepath.Join(dir, "telemetry-disabled")); err == nil {
		t.Error("nothing should have been written on this side")
	}
}

func TestTelemetryStatusAsksTheContainer(t *testing.T) {
	answer := `{"state":"on","reason":"","interval":"24h0m0s",` +
		`"payload":{"event":"heartbeat","properties":{"deploy":"docker"}}}`
	tooling := &stubTooling{
		found:     map[string]bool{"docker": true},
		container: "godrop-godrop-1",
		says:      map[string]string{"telemetry status": answer},
	}
	tooling.install(t)
	installAt(t, wizard.DeployCompose, "")
	t.Setenv("GODROP_DATA_DIR", "")

	code, out, stderr := run(t, testBuild(), "telemetry", "status")
	if code != 0 {
		t.Fatalf("exit = %d: %s", code, stderr)
	}
	for _, want := range []string{"Telemetry: on", "heartbeat", "24h0m0s", "godrop telemetry off"} {
		if !strings.Contains(out, want) {
			t.Errorf("output should mention %q:\n%s", want, out)
		}
	}

	// --json hands back what the container said, unchanged.
	_, out, _ = run(t, testBuild(), "telemetry", "status", "--json")
	var got map[string]any
	if err := json.Unmarshal([]byte(out), &got); err != nil || got["state"] != "on" {
		t.Errorf("output = %q (%v)", out, err)
	}
}

func TestTelemetryStatusWithNothingToShow(t *testing.T) {
	tooling := &stubTooling{
		found:     map[string]bool{"docker": true},
		container: "godrop-godrop-1",
		says: map[string]string{
			"telemetry status": `{"state":"off","reason":"built from source","payload":null}`,
		},
	}
	tooling.install(t)
	installAt(t, wizard.DeployCompose, "")
	t.Setenv("GODROP_DATA_DIR", "")

	_, out, _ := run(t, testBuild(), "telemetry", "status")
	if !strings.Contains(out, "Telemetry: off") || !strings.Contains(out, "built from source") {
		t.Errorf("output = %s", out)
	}
}

func TestTelemetryReportsAContainerItCannotRead(t *testing.T) {
	tooling := &stubTooling{
		found:     map[string]bool{"docker": true},
		container: "godrop-godrop-1",
		says:      map[string]string{"telemetry": "not json"},
	}
	tooling.install(t)
	installAt(t, wizard.DeployCompose, "")
	t.Setenv("GODROP_DATA_DIR", "")

	code, _, stderr := run(t, testBuild(), "telemetry", "status")
	if code == 0 || !strings.Contains(stderr, "could not read the answer from the container") {
		t.Errorf("exit = %d, stderr = %q", code, stderr)
	}

	failing := &stubTooling{found: map[string]bool{"docker": true}, outErr: errAlreadyThere}
	failing.install(t)
	if code, _, stderr := run(t, testBuild(), "telemetry", "off"); code == 0 ||
		!strings.Contains(stderr, "telemetry off") {
		t.Errorf("exit = %d, stderr = %q", code, stderr)
	}
	if code, _, stderr := run(t, testBuild(), "telemetry", "status"); code == 0 ||
		!strings.Contains(stderr, "telemetry status") {
		t.Errorf("exit = %d, stderr = %q", code, stderr)
	}
}

func TestHealthProbesWhereTheInstallationAnswers(t *testing.T) {
	// The shell knows nothing about the port; the installation does.
	installAt(t, wizard.DeployCompose, "")
	t.Setenv("GODROP_ADDR", "")
	target, insecure := localHealthURL()
	if !strings.Contains(target, wizard.Defaults().Port) || insecure {
		t.Errorf("localHealthURL = %q, %v", target, insecure)
	}

	// An installation behind https is probed there, certificate and all.
	installAt(t, wizard.DeployCompose, "https://files.example.com")
	target, insecure = localHealthURL()
	if target != "https://files.example.com/healthz" || !insecure {
		t.Errorf("localHealthURL = %q, %v", target, insecure)
	}
}

func TestUninstallTakesTheContainerWithIt(t *testing.T) {
	tooling := &stubTooling{found: map[string]bool{"docker": true}}
	tooling.install(t)
	dir := installAt(t, wizard.DeployCompose, "")

	code, out, stderr := run(t, testBuild(), "uninstall", "--yes")
	if code != 0 {
		t.Fatalf("exit = %d: %s", code, stderr)
	}
	if !strings.Contains(out, "the container and its network") {
		t.Errorf("output = %s", out)
	}
	ran := strings.Join(tooling.ran, "|")
	if !strings.Contains(ran, "compose --project-directory "+dir+" down") {
		t.Errorf("ran = %v", tooling.ran)
	}
	// Without --purge the volume, and so the uploads, stay.
	if strings.Contains(ran, "--volumes") {
		t.Errorf("uploads should survive an uninstall without --purge: %v", tooling.ran)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Errorf("the configuration should be gone: %v", err)
	}
}

func TestUninstallWithPurgeTakesTheVolumeToo(t *testing.T) {
	tooling := &stubTooling{found: map[string]bool{"docker": true}}
	tooling.install(t)
	installAt(t, wizard.DeployCompose, "")

	code, out, stderr := run(t, testBuild(), "uninstall", "--purge", "--yes")
	if code != 0 {
		t.Fatalf("exit = %d: %s", code, stderr)
	}
	if !strings.Contains(out, "the volume with the uploads") {
		t.Errorf("output = %s", out)
	}
	if !strings.Contains(strings.Join(tooling.ran, "|"), "down --volumes") {
		t.Errorf("ran = %v", tooling.ran)
	}
}

func TestUninstallReportsADockerThatRefuses(t *testing.T) {
	tooling := &stubTooling{found: map[string]bool{"docker": true}, runErr: errAlreadyThere}
	tooling.install(t)
	dir := installAt(t, wizard.DeployCompose, "")

	code, out, _ := run(t, testBuild(), "uninstall", "--yes")
	if code != 0 {
		t.Errorf("exit = %d; the files can still go", code)
	}
	if !strings.Contains(out, "✗") {
		t.Errorf("the failure should be visible:\n%s", out)
	}
	// The rest of the removal carries on: a container that would not stop is
	// not a reason to leave the configuration behind.
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Errorf("the configuration should be gone: %v", err)
	}
}

func TestUninstallSaysWhatTheUnitNeeds(t *testing.T) {
	tooling := &stubTooling{found: map[string]bool{}}
	tooling.install(t)
	installAt(t, wizard.DeploySystemd, "")

	_, out, _ := run(t, testBuild(), "uninstall", "--yes")
	// Removing a unit from /etc/systemd/system is root's work, and this is the
	// one thing uninstall cannot do for you.
	if !strings.Contains(out, "systemctl disable --now godrop") {
		t.Errorf("output = %s", out)
	}
}
