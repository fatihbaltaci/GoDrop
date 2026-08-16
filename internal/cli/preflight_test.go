package cli

import (
	"context"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/fatihbaltaci/GoDrop/internal/wizard"
)

// TestMain keeps the whole suite off this machine's real ports. Setup refuses
// to write a configuration it cannot start, so a test that binds for real
// would pass or fail according to whatever else happens to be listening on
// the developer's laptop. Tests that care about a busy port say so themselves,
// with stubTooling.
func TestMain(m *testing.M) {
	listenOn = func(string) (io.Closer, error) { return io.NopCloser(strings.NewReader("")), nil }

	// A home of this run's own, for the same reason. Setup, update and
	// uninstall all work out where they live from the home directory, and a
	// test that reached the real one would be operating on the installation
	// belonging to whoever is running the suite.
	home, err := os.MkdirTemp("", "godrop-cli-test-home")
	if err != nil {
		panic(err)
	}
	for _, key := range []string{"HOME", "APPDATA", "USERPROFILE", "XDG_DATA_HOME"} {
		if err := os.Setenv(key, home); err != nil {
			panic(err)
		}
	}
	// Uninstall removes "the godrop binary", which without this is the test
	// binary itself: the first test to run it takes the executable out from
	// under every test after it. This stands in for it, and comes back after
	// each removal so that the next test finds one too.
	osExecutable = func() (string, error) {
		path := filepath.Join(home, "bin", "godrop")
		if _, err := os.Stat(path); err != nil {
			if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
				return "", err
			}
			if err := os.WriteFile(path, []byte("#!/bin/sh\n"), 0o600); err != nil {
				return "", err
			}
		}
		return path, nil
	}

	code := m.Run()
	_ = os.RemoveAll(home)
	os.Exit(code)
}

// stubTooling replaces the three seams the checks use, so a test can describe
// a machine with or without docker, sudo and a free port.
type stubTooling struct {
	found    map[string]bool
	ran      []string
	runErr   error
	listenAs error
	// busy names the ports that listenAs applies to. Empty means all of them,
	// which is the machine where nothing nearby is free either.
	busy map[string]bool
	root bool
	// What the commands that are asked a question answer: the container
	// docker compose is running, and what it keeps the uploads on.
	container  string
	mount      string
	outErr     error
	inspectErr error
	// failArgs is the one command that fails, for a machine where most of the
	// tooling works and one thing does not.
	failArgs string
	// says answers a command whose output matters, keyed by a substring of it.
	says map[string]string
	// ranOut records the commands that were asked a question.
	ranOut []string
}

func (s *stubTooling) install(t *testing.T) {
	t.Helper()
	originals := struct {
		lookPath   func(string) (string, error)
		runCommand func(context.Context, string, ...string) error
		runQuietly func(context.Context, string, ...string) error
		listenOn   func(string) (io.Closer, error)
		euid       func() int
	}{lookPath, runCommand, runQuietly, listenOn, euid}
	t.Cleanup(func() {
		lookPath, runCommand, runQuietly = originals.lookPath, originals.runCommand, originals.runQuietly
		listenOn, euid = originals.listenOn, originals.euid
	})

	lookPath = func(name string) (string, error) {
		if s.found[name] {
			return "/usr/bin/" + name, nil
		}
		return "", errors.New(name + ": not found")
	}
	record := func(_ context.Context, name string, args ...string) error {
		line := strings.TrimSpace(name + " " + strings.Join(args, " "))
		s.ran = append(s.ran, line)
		if s.failArgs != "" {
			if strings.Contains(line, s.failArgs) {
				return errors.New(s.failArgs + ": stubbed failure")
			}
			return nil
		}
		return s.runErr
	}
	runCommand, runQuietly = record, record
	originalOutput := runOutput
	t.Cleanup(func() { runOutput = originalOutput })
	runOutput = func(_ context.Context, name string, args ...string) (string, error) {
		line := strings.Join(args, " ")
		s.ranOut = append(s.ranOut, strings.TrimSpace(name+" "+line))
		if s.outErr != nil {
			return "", s.outErr
		}
		for match, answer := range s.says {
			if strings.Contains(line, match) {
				return answer, nil
			}
		}
		if name == "docker" && len(args) > 0 && args[0] == "inspect" {
			return s.mount, s.inspectErr
		}
		return s.container, nil
	}
	listenOn = func(addr string) (io.Closer, error) {
		_, port, _ := net.SplitHostPort(addr)
		if s.listenAs != nil && (len(s.busy) == 0 || s.busy[port]) {
			return nil, s.listenAs
		}
		return io.NopCloser(strings.NewReader("")), nil
	}
	euid = func() int {
		if s.root {
			return 0
		}
		return 1000
	}
}

// answers builds a set of answers with a usable data directory.
func answersIn(t *testing.T, deployment string) wizard.Answers {
	t.Helper()
	a := wizard.Defaults()
	a.Deployment = deployment
	a.DataDir = filepath.Join(t.TempDir(), "data")
	return a
}

func TestPreflightPassesOnAnOrdinaryMachine(t *testing.T) {
	tooling := &stubTooling{found: map[string]bool{"docker": true}}
	tooling.install(t)

	var buf strings.Builder
	a := answersIn(t, wizard.DeployCompose)
	a.DataDir = ""
	if err := preflight(t.Context(), &output{w: &buf}, &flagPrompter{}, &a, t.TempDir(), false); err != nil {
		t.Fatalf("preflight: %v", err)
	}
	text := buf.String()
	for _, want := range []string{"docker volume godrop-data", "output directory", "docker compose", "is free"} {
		if !strings.Contains(text, want) {
			t.Errorf("output should mention %q:\n%s", want, text)
		}
	}
}

func TestPreflightCreatesTheDataDirectory(t *testing.T) {
	tooling := &stubTooling{found: map[string]bool{}}
	tooling.install(t)

	a := answersIn(t, wizard.DeployEnv)
	var buf strings.Builder
	if err := preflight(t.Context(), &output{w: &buf}, &flagPrompter{}, &a, t.TempDir(), false); err != nil {
		t.Fatalf("preflight: %v", err)
	}
	if !strings.Contains(buf.String(), "(created)") {
		t.Errorf("output = %s", buf.String())
	}
	if _, err := os.Stat(a.DataDir); err != nil {
		t.Errorf("the directory should exist now: %v", err)
	}
}

func TestPreflightOffersSudoForADirectoryItCannotCreate(t *testing.T) {
	requireStrictPermissions(t)
	tooling := &stubTooling{found: map[string]bool{"sudo": true}}
	tooling.install(t)

	// A directory inside an unwritable parent: exactly /var/lib/godrop as an
	// ordinary user, without needing to be one.
	parent := t.TempDir()
	if err := os.Chmod(parent, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(parent, 0o700) })

	a := answersIn(t, wizard.DeployEnv)
	a.DataDir = filepath.Join(parent, "godrop")

	var buf strings.Builder
	err := preflight(t.Context(), &output{w: &buf}, &flagPrompter{}, &a, t.TempDir(), false)
	// The stub "sudo" does nothing, so the directory is still missing and the
	// wizard stops, which is the point: it stops before writing anything.
	if err == nil {
		t.Fatal("preflight should fail when the directory is still not usable")
	}
	if len(tooling.ran) == 0 || !strings.Contains(tooling.ran[0], "install -d") {
		t.Errorf("ran = %v, want it to try sudo install -d", tooling.ran)
	}
}

func TestPreflightSaysWhatToRunWhenThereIsNoSudo(t *testing.T) {
	requireStrictPermissions(t)
	tooling := &stubTooling{found: map[string]bool{}}
	tooling.install(t)

	parent := t.TempDir()
	if err := os.Chmod(parent, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(parent, 0o700) })

	a := answersIn(t, wizard.DeployEnv)
	a.DataDir = filepath.Join(parent, "godrop")

	var buf strings.Builder
	err := preflight(t.Context(), &output{w: &buf}, &flagPrompter{}, &a, t.TempDir(), false)
	if err == nil {
		t.Fatal("preflight should stop, not carry on to write files")
	}
	if !strings.Contains(buf.String(), "mkdir -p") {
		t.Errorf("output should say what to run as root:\n%s", buf.String())
	}
}

func TestPreflightStopsWhenTheOutputDirectoryIsNotWritable(t *testing.T) {
	requireStrictPermissions(t)
	tooling := &stubTooling{found: map[string]bool{}}
	tooling.install(t)

	outDir := t.TempDir()
	if err := os.Chmod(outDir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(outDir, 0o700) })

	var buf strings.Builder
	a := answersIn(t, wizard.DeployEnv)
	err := preflight(t.Context(), &output{w: &buf}, &flagPrompter{}, &a, outDir, false)
	if err == nil || !strings.Contains(buf.String(), "--out-dir") {
		t.Errorf("err = %v, output = %s", err, buf.String())
	}
}

func TestPreflightReportsMissingTooling(t *testing.T) {
	cases := []struct {
		name       string
		deployment string
		found      map[string]bool
		runErr     error
		failArgs   string
		want       string
	}{
		{"no docker", wizard.DeployCompose, map[string]bool{}, nil, "", "docker"},
		{"no compose plugin", wizard.DeployCompose, map[string]bool{"docker": true},
			errors.New("unknown command"), "", "`docker compose` is not"},
		// The plugin is there and answers; the daemon is the separate question,
		// and on a fresh server the answer is the docker group.
		{"daemon not reachable", wizard.DeployCompose, map[string]bool{"docker": true},
			nil, "docker info", "usermod -aG docker"},
		{"no systemd", wizard.DeploySystemd, map[string]bool{}, nil, "", "systemd"},
		{"systemd needs root", wizard.DeploySystemd, map[string]bool{"systemctl": true}, nil, "", "needs sudo"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tooling := &stubTooling{found: tc.found, runErr: tc.runErr, failArgs: tc.failArgs}
			tooling.install(t)
			var buf strings.Builder
			a := answersIn(t, tc.deployment)
			if err := preflight(t.Context(), &output{w: &buf}, &flagPrompter{}, &a, t.TempDir(), false); err != nil {
				t.Fatalf("missing tooling is a warning, not a failure: %v", err)
			}
			if !strings.Contains(buf.String(), tc.want) {
				t.Errorf("output should mention %q:\n%s", tc.want, buf.String())
			}
		})
	}
}

// busyPort describes a machine where the port GoDrop wants is taken and the
// one above it is not.
func busyPort(t *testing.T, port string) *stubTooling {
	t.Helper()
	tooling := &stubTooling{
		found:    map[string]bool{},
		listenAs: errors.New("address already in use"),
		busy:     map[string]bool{port: true},
	}
	tooling.install(t)
	return tooling
}

func TestPreflightRefusesToWriteForABusyPort(t *testing.T) {
	busyPort(t, "8747")
	var buf strings.Builder
	a := answersIn(t, wizard.DeployEnv)
	a.Port = "8747"
	// Nobody to ask: the setup stops here, with the flag that fixes it, rather
	// than writing files for a service that cannot come up.
	err := preflight(t.Context(), &output{w: &buf}, &flagPrompter{}, &a, t.TempDir(), false)
	if err == nil || !strings.Contains(err.Error(), "--port 8748") {
		t.Fatalf("err = %v", err)
	}
	if !strings.Contains(buf.String(), "already in use") {
		t.Errorf("output = %s", buf.String())
	}
}

func TestPreflightOffersTheNextFreePort(t *testing.T) {
	busyPort(t, "8747")
	var buf strings.Builder
	a := answersIn(t, wizard.DeployEnv)
	a.Port = "8747"
	// flagPrompter answers a confirmation with its default, which here is yes.
	if err := preflight(t.Context(), &output{w: &buf}, &flagPrompter{}, &a, t.TempDir(), true); err != nil {
		t.Fatal(err)
	}
	if a.Port != "8748" {
		t.Errorf("port = %q, want the free one", a.Port)
	}
	if !strings.Contains(buf.String(), "8748 is free") {
		t.Errorf("output = %s", buf.String())
	}
}

func TestPreflightStopsWhenTheOfferedPortIsDeclined(t *testing.T) {
	busyPort(t, "8747")
	a := answersIn(t, wizard.DeployEnv)
	a.Port = "8747"
	err := preflight(t.Context(), &output{w: io.Discard}, &decliningPrompter{}, &a, t.TempDir(), true)
	if err == nil || !strings.Contains(err.Error(), "8747 is already in use") {
		t.Fatalf("err = %v", err)
	}
	if a.Port != "8747" {
		t.Errorf("port = %q; declining changes nothing", a.Port)
	}
}

func TestPreflightPropagatesACancelledPortOffer(t *testing.T) {
	busyPort(t, "8747")
	a := answersIn(t, wizard.DeployEnv)
	a.Port = "8747"
	err := preflight(t.Context(), &output{w: io.Discard}, &cancellingPrompter{}, &a, t.TempDir(), true)
	if !errors.Is(err, errCancelled) {
		t.Errorf("err = %v, want the cancellation", err)
	}
}

func TestPreflightWhenNothingNearbyIsFree(t *testing.T) {
	// listenAs with no busy list: every port fails, so there is nothing to
	// offer and the message has to name the flag instead.
	tooling := &stubTooling{found: map[string]bool{}, listenAs: errors.New("address already in use")}
	tooling.install(t)
	a := answersIn(t, wizard.DeployEnv)
	a.Port = "8747"
	err := preflight(t.Context(), &output{w: io.Discard}, &flagPrompter{}, &a, t.TempDir(), true)
	if err == nil || !strings.Contains(err.Error(), "--port <port>") {
		t.Fatalf("err = %v", err)
	}
}

func TestThePortQuestionKnowsWhatIsListening(t *testing.T) {
	// The check the wizard borrows from here, in all three of its answers.
	busyPort(t, "8747")
	if err := wizard.PortInUse("8747"); err == nil || !strings.Contains(err.Error(), "already listening") {
		t.Errorf("err = %v, want the busy port reported", err)
	}
	if err := wizard.PortInUse("8748"); err != nil {
		t.Errorf("err = %v, want a free port to pass", err)
	}

	// A port that needs privileges is not a port somebody else holds: the
	// generated unit and docker both arrange for those.
	privileged := &stubTooling{found: map[string]bool{}, listenAs: os.ErrPermission}
	privileged.install(t)
	if err := wizard.PortInUse("443"); err != nil {
		t.Errorf("err = %v, want a privileged port to pass", err)
	}
}

func TestNextFreePortNeedsANumber(t *testing.T) {
	if got := nextFreePort("http"); got != "" {
		t.Errorf("nextFreePort = %q, want nothing to offer", got)
	}
	// One below the top of the range: there is no port above it to move to.
	busyPort(t, "65535")
	if got := nextFreePort("65535"); got != "" {
		t.Errorf("nextFreePort = %q, want nothing above 65535", got)
	}
}

func TestPreflightExplainsAPrivilegedPort(t *testing.T) {
	tooling := &stubTooling{found: map[string]bool{}, listenAs: os.ErrPermission}
	tooling.install(t)
	var buf strings.Builder
	a := answersIn(t, wizard.DeploySystemd)
	a.BaseURL = "https://files.example.com"
	a.TLS = wizard.TLSAuto
	if err := preflight(t.Context(), &output{w: &buf}, &flagPrompter{}, &a, t.TempDir(), false); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), "CAP_NET_BIND_SERVICE") || !strings.Contains(buf.String(), "unit") {
		t.Errorf("output = %s", buf.String())
	}
}

func TestUsableRejectsAFile(t *testing.T) {
	file := filepath.Join(t.TempDir(), "afile")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := usable(file); err == nil || !strings.Contains(err.Error(), "not a directory") {
		t.Errorf("err = %v", err)
	}
	if err := usable(filepath.Join(t.TempDir(), "missing")); err == nil {
		t.Error("a missing directory is not usable")
	}
}

func TestSudoIsNotOfferedToRoot(t *testing.T) {
	tooling := &stubTooling{found: map[string]bool{"sudo": true}, root: true}
	tooling.install(t)
	if _, ok := sudoAvailable(); ok {
		t.Error("root does not need sudo")
	}
}

func TestPreflightCreatesTheDirectoryWithSudo(t *testing.T) {
	requireStrictPermissions(t)
	created := filepath.Join(t.TempDir(), "godrop")
	tooling := &stubTooling{found: map[string]bool{"sudo": true}}
	tooling.install(t)
	// A "sudo" that actually creates the directory, which is what the real
	// one would do.
	runCommand = func(_ context.Context, name string, _ ...string) error {
		tooling.ran = append(tooling.ran, name)
		return os.MkdirAll(created, 0o700)
	}

	parent := filepath.Dir(created)
	if err := os.Chmod(parent, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(parent, 0o700) })

	a := answersIn(t, wizard.DeployEnv)
	a.DataDir = created
	var buf strings.Builder
	// The stub creates it despite the unwritable parent, so the check passes.
	_ = os.Chmod(parent, 0o700)
	if err := preflight(t.Context(), &output{w: &buf}, &flagPrompter{}, &a, t.TempDir(), false); err != nil {
		t.Fatalf("preflight: %v", err)
	}
}

func TestPreflightStopsWhenSudoIsDeclined(t *testing.T) {
	requireStrictPermissions(t)
	tooling := &stubTooling{found: map[string]bool{"sudo": true}}
	tooling.install(t)

	parent := t.TempDir()
	if err := os.Chmod(parent, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(parent, 0o700) })

	a := answersIn(t, wizard.DeployEnv)
	a.DataDir = filepath.Join(parent, "godrop")
	var buf strings.Builder
	err := preflight(t.Context(), &output{w: &buf}, &decliningPrompter{}, &a, t.TempDir(), false)
	if err == nil {
		t.Fatal("saying no should stop the wizard, not carry on")
	}
	if len(tooling.ran) != 0 {
		t.Errorf("nothing should have been run: %v", tooling.ran)
	}
}

func TestPreflightPropagatesACancelledConfirmation(t *testing.T) {
	requireStrictPermissions(t)
	tooling := &stubTooling{found: map[string]bool{"sudo": true}}
	tooling.install(t)

	parent := t.TempDir()
	if err := os.Chmod(parent, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(parent, 0o700) })

	a := answersIn(t, wizard.DeployEnv)
	a.DataDir = filepath.Join(parent, "godrop")
	err := preflight(t.Context(), &output{w: io.Discard}, &cancellingPrompter{}, &a, t.TempDir(), false)
	if !errors.Is(err, errCancelled) {
		t.Errorf("err = %v, want the cancellation", err)
	}
}

func TestDeploymentName(t *testing.T) {
	if got := deploymentName(wizard.DeploySystemd); got != "unit" {
		t.Errorf("deploymentName = %q", got)
	}
	if got := deploymentName(wizard.DeployCompose); got != "compose file" {
		t.Errorf("deploymentName = %q", got)
	}
}

// decliningPrompter answers no to everything.
type decliningPrompter struct{ flagPrompter }

func (p *decliningPrompter) Confirm(string, string, bool) (bool, error) { return false, nil }

// cancellingPrompter is somebody pressing Ctrl+C.
type cancellingPrompter struct{ flagPrompter }

func (p *cancellingPrompter) Confirm(string, string, bool) (bool, error) { return false, errCancelled }

func TestPreflightReportsAFailingSudo(t *testing.T) {
	requireStrictPermissions(t)
	tooling := &stubTooling{found: map[string]bool{"sudo": true}, runErr: errors.New("wrong password")}
	tooling.install(t)

	parent := t.TempDir()
	if err := os.Chmod(parent, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(parent, 0o700) })

	a := answersIn(t, wizard.DeployEnv)
	a.DataDir = filepath.Join(parent, "godrop")
	err := preflight(t.Context(), &output{w: io.Discard}, &flagPrompter{}, &a, t.TempDir(), false)
	if err == nil || !strings.Contains(err.Error(), "with sudo") {
		t.Errorf("err = %v, want the sudo failure reported", err)
	}
}

func TestPreflightSucceedsWhenSudoFixesTheDirectory(t *testing.T) {
	requireStrictPermissions(t)
	dir := filepath.Join(t.TempDir(), "godrop")
	if err := os.MkdirAll(dir, 0o500); err != nil { // exists, but not writable
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })

	tooling := &stubTooling{found: map[string]bool{"sudo": true}}
	tooling.install(t)
	runCommand = func(context.Context, string, ...string) error { return os.Chmod(dir, 0o700) }

	a := answersIn(t, wizard.DeployEnv)
	a.DataDir = dir
	var buf strings.Builder
	if err := preflight(t.Context(), &output{w: &buf}, &flagPrompter{}, &a, t.TempDir(), false); err != nil {
		t.Fatalf("preflight: %v", err)
	}
	if !strings.Contains(buf.String(), "created with sudo") {
		t.Errorf("output = %s", buf.String())
	}
}

func TestPreflightWithoutAnOutputDirectoryUsesTheWorkingOne(t *testing.T) {
	tooling := &stubTooling{found: map[string]bool{}}
	tooling.install(t)
	dir := t.TempDir()
	original, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(original) })

	a := answersIn(t, wizard.DeployEnv)
	// No port either: nothing to check about one that was never chosen.
	a.Port = ""
	var buf strings.Builder
	if err := preflight(t.Context(), &output{w: &buf}, &flagPrompter{}, &a, "", false); err != nil {
		t.Fatalf("preflight: %v", err)
	}
	if !strings.Contains(buf.String(), "output directory") {
		t.Errorf("output = %s", buf.String())
	}
	if strings.Contains(buf.String(), "port") {
		t.Errorf("there is no port to report:\n%s", buf.String())
	}
}
