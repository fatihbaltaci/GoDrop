package cli

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/fatihbaltaci/GoDrop/internal/telemetry"
	"github.com/fatihbaltaci/GoDrop/internal/tokens"
	"github.com/fatihbaltaci/GoDrop/internal/wizard"
)

// initArgs builds a non-interactive setup that writes into temporary
// directories, which is exactly the shape an agent or a CI job would use.
func initArgs(outDir, dataDir string, extra ...string) []string {
	args := []string{
		"init", "--non-interactive",
		"--out-dir", outDir,
		"--data-dir", dataDir,
		"--no-external-check",
		// Compose keeps its files in a docker volume and needs no data
		// directory at all, so the tests that care about one say so.
		"--deployment", "env",
	}
	return append(args, extra...)
}

func TestInitNonInteractiveWritesEverything(t *testing.T) {
	requirePOSIXModes(t)
	outDir, dataDir := t.TempDir(), t.TempDir()
	code, out, stderr := run(t, testBuild(), initArgs(outDir, dataDir,
		"--base-url", "https://files.example.com", "--deployment", "compose", "--json")...)
	if code != 0 {
		t.Fatalf("exit = %d, stderr = %s", code, stderr)
	}

	var got struct {
		Token     string   `json:"token"`
		TokenName string   `json:"token_name"`
		Files     []string `json:"files"`
		BaseURL   string   `json:"base_url"`
		DataDir   string   `json:"data_dir"`
		NextSteps []string `json:"next_steps"`
	}
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("not JSON: %v (%q)", err, out)
	}
	if !strings.HasPrefix(got.Token, tokens.Prefix) {
		t.Errorf("token = %q", got.Token)
	}
	// Under compose the files live in a docker volume, so there is no host
	// directory in the answers at all.
	if got.BaseURL != "https://files.example.com" || got.DataDir != "" {
		t.Errorf("answers = %+v", got)
	}
	if len(got.NextSteps) == 0 {
		t.Error("the next steps should be part of the machine-readable output")
	}

	// A public URL selects automatic TLS, so GoDrop terminates it itself and
	// there is no proxy configuration to write.
	names := map[string]bool{}
	for _, f := range got.Files {
		names[filepath.Base(f)] = true
	}
	for _, want := range []string{".env", "docker-compose.yml"} { //nolint:gocritic // read below
		if !names[want] {
			t.Errorf("%s was not written, got %v", want, got.Files)
		}
	}
	if names["Caddyfile"] {
		t.Errorf("a Caddyfile is pointless when GoDrop serves https itself: %v", got.Files)
	}

	env, err := os.ReadFile(filepath.Join(outDir, ".env"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(env), got.Token) {
		t.Error("the .env should carry the generated token")
	}
	info, err := os.Stat(filepath.Join(outDir, ".env"))
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf(".env mode = %#o, want 0600", perm)
	}

	// With a volume there is no token file to write, so the token in .env is
	// the token: the server reads it from the environment.
	store, err := tokens.New(tokens.Path(t.TempDir()), []string{got.Token})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := store.Verify(got.Token); !ok {
		t.Error("the generated token should be accepted")
	}
	_ = dataDir
}

func TestInitWritesATokenFileWhenTheFilesLiveOnThisMachine(t *testing.T) {
	outDir, dataDir := t.TempDir(), t.TempDir()
	code, out, stderr := run(t, testBuild(), initArgs(outDir, dataDir, "--json")...)
	if code != 0 {
		t.Fatalf("exit = %d, stderr = %s", code, stderr)
	}
	var got struct {
		Token   string `json:"token"`
		DataDir string `json:"data_dir"`
	}
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatal(err)
	}
	if got.DataDir != dataDir {
		t.Errorf("data dir = %q, want the one that was asked for", got.DataDir)
	}
	store, err := tokens.New(tokens.Path(dataDir), nil)
	if err != nil {
		t.Fatal(err)
	}
	if name, ok := store.Verify(got.Token); !ok || name != "default" {
		t.Errorf("the generated token does not verify (%q, %t)", name, ok)
	}
}

func TestInitHumanOutputGuidesTheUser(t *testing.T) {
	outDir, dataDir := t.TempDir(), t.TempDir()
	code, out, stderr := run(t, testBuild(), initArgs(outDir, dataDir,
		"--base-url", "https://files.example.com", "--deployment", "env")...)
	if code != 0 {
		t.Fatalf("exit = %d, stderr = %s", code, stderr)
	}
	for _, want := range []string{
		"Written", "Your API token", "shown once", "Verifying", "Next", "Use it",
		"GODROP_URL=", "GODROP_TOKEN=", "llms.txt",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("the summary should mention %q:\n%s", want, out)
		}
	}
}

func TestInitRefusesToOverwriteWithoutForce(t *testing.T) {
	outDir, dataDir := t.TempDir(), t.TempDir()
	if code, _, _ := run(t, testBuild(), initArgs(outDir, dataDir)...); code != 0 {
		t.Fatal("first run should succeed")
	}
	code, _, stderr := run(t, testBuild(), initArgs(outDir, t.TempDir())...)
	if code == 0 {
		t.Error("a second run should refuse to clobber the configuration")
	}
	if !strings.Contains(stderr, "--force") {
		t.Errorf("stderr = %q", stderr)
	}
	if code, _, _ := run(t, testBuild(), initArgs(outDir, t.TempDir(), "--force", "--token-name", "second")...); code != 0 {
		t.Error("--force should overwrite")
	}
}

func TestInitRejectsAnInvalidDeployment(t *testing.T) {
	code, _, stderr := run(t, testBuild(), initArgs(t.TempDir(), t.TempDir(), "--deployment", "kubernetes")...)
	if code == 0 {
		t.Error("an unknown deployment style should fail")
	}
	if !strings.Contains(stderr, "compose") {
		t.Errorf("stderr should list the valid choices: %q", stderr)
	}
}

func TestInitRejectsAnInvalidAnswer(t *testing.T) {
	code, _, stderr := run(t, testBuild(),
		initArgs(t.TempDir(), t.TempDir(), "--max-file-size", "lots", "--limits")...)
	if code == 0 {
		t.Error("an invalid size should fail")
	}
	if !strings.Contains(stderr, "Maximum file size") {
		t.Errorf("stderr should name the offending question: %q", stderr)
	}
}

func TestInitRecordsTelemetryOptOut(t *testing.T) {
	outDir, dataDir := t.TempDir(), t.TempDir()
	if code, _, stderr := run(t, testBuild(), initArgs(outDir, dataDir, "--telemetry=false")...); code != 0 {
		t.Fatalf("exit = %d: %s", code, stderr)
	}
	if !telemetry.Disabled(dataDir) {
		t.Error("declining telemetry during setup should be recorded for the service")
	}
}

func TestInitReportsATokenStoreFailure(t *testing.T) {
	requireStrictPermissions(t)
	dataDir := t.TempDir()
	if err := os.WriteFile(tokens.Path(dataDir), []byte("{{{"), 0o600); err != nil {
		t.Fatal(err)
	}
	code, _, stderr := run(t, testBuild(), initArgs(t.TempDir(), dataDir)...)
	if code == 0 {
		t.Error("a corrupt token store should stop the setup")
	}
	if !strings.Contains(stderr, "token") {
		t.Errorf("stderr = %q", stderr)
	}
}

func TestInitReportsAnUnwritableOutputDirectory(t *testing.T) {
	requireStrictPermissions(t)
	outDir := t.TempDir()
	if err := os.Chmod(outDir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(outDir, 0o700) })
	if code, _, _ := run(t, testBuild(), initArgs(outDir, t.TempDir())...); code == 0 {
		t.Error("an unwritable output directory should fail")
	}
}

func TestInitDefaultsToTheWorkingDirectory(t *testing.T) {
	dir := t.TempDir()
	original, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(original) })

	code, _, stderr := run(t, testBuild(), "init", "--non-interactive", "--out-dir", ".",
		"--data-dir", t.TempDir(), "--no-external-check", "--deployment", "env")
	if code != 0 {
		t.Fatalf("exit = %d: %s", code, stderr)
	}
	if _, err := os.Stat(filepath.Join(dir, ".env")); err != nil {
		t.Errorf("--out-dir . should write into the working directory: %v", err)
	}
}

func TestInitRunsTheRealFormsWhenATerminalIsPresent(t *testing.T) {
	// Pretend we are on a terminal and drive the real huh forms with keystrokes:
	// nine questions, each accepting its default with Enter.
	originalInteractive, originalAsk := interactive, askInteractively
	interactive = func() bool { return true }
	askInteractively = func(a wizard.Answers) (wizard.Answers, error) {
		return runForm(repeat('\r'), io.Discard, a)
	}
	t.Cleanup(func() { interactive, askInteractively = originalInteractive, originalAsk })

	outDir, dataDir := t.TempDir(), t.TempDir()
	code, out, stderr := run(t, testBuild(), "init",
		"--out-dir", outDir, "--data-dir", dataDir, "--no-external-check", "--deployment", "env")
	if code != 0 {
		t.Fatalf("exit = %d, stderr = %s", code, stderr)
	}
	if !strings.Contains(out, "GoDrop 1.2.3 setup") {
		t.Errorf("the interactive run should print the banner:\n%s", out)
	}
	if _, err := os.Stat(filepath.Join(outDir, ".env")); err != nil {
		t.Errorf("the interactive run should write the configuration: %v", err)
	}
}

func TestInitReportsCancellation(t *testing.T) {
	originalInteractive, originalAsk := interactive, askInteractively
	interactive = func() bool { return true }
	askInteractively = func(a wizard.Answers) (wizard.Answers, error) {
		// Ctrl+C on the first question.
		return runForm(repeat('\x03'), io.Discard, a)
	}
	t.Cleanup(func() { interactive, askInteractively = originalInteractive, originalAsk })

	code, out, _ := run(t, testBuild(), "init", "--out-dir", t.TempDir(), "--data-dir", t.TempDir())
	if code == 0 {
		t.Error("an aborted wizard should exit non-zero")
	}
	if !strings.Contains(out, "Cancelled") {
		t.Errorf("output = %q", out)
	}
}

func TestInitMentionsAMissingTerminal(t *testing.T) {
	// Without --non-interactive and without a terminal, the wizard says so
	// rather than silently using defaults.
	code, out, _ := run(t, testBuild(), "init",
		"--out-dir", t.TempDir(), "--data-dir", t.TempDir(), "--no-external-check", "--deployment", "env")
	if code != 0 {
		t.Fatalf("exit = %d", code)
	}
	if !strings.Contains(out, "no interactive terminal") {
		t.Errorf("output = %q", out)
	}
}

// repeat feeds the same keystroke forever. Each question builds its own form,
// and a form whose input reaches EOF waits instead of returning, so the stream
// must not run out.
func repeat(b byte) io.Reader { return repeatingReader{b} }

type repeatingReader struct{ b byte }

func (r repeatingReader) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = r.b
	}
	return len(p), nil
}

// ------------------------------------------------------------------ prompts

func TestHuhPromptsAcceptScriptedInput(t *testing.T) {
	p := newHuhPrompter(&output{w: io.Discard})
	p.w = io.Discard

	p.in = repeat('\r')
	got, err := p.Input("Public URL", "help text", "https://files.example.com", wizard.ValidateBaseURL)
	if err != nil || got != "https://files.example.com" {
		t.Errorf("Input = %q, %v", got, err)
	}

	p.in = repeat('\r')
	choice, err := p.Select("Deployment", "help", []wizard.Option{
		{Label: "docker compose", Value: wizard.DeployCompose, Desc: "writes docker-compose.yml"},
		{Label: "systemd", Value: wizard.DeploySystemd},
	}, wizard.DeployCompose)
	if err != nil || choice != wizard.DeployCompose {
		t.Errorf("Select = %q, %v", choice, err)
	}

	p.in = repeat('\r')
	confirmed, err := p.Confirm("Telemetry?", "help", true)
	if err != nil || !confirmed {
		t.Errorf("Confirm = %t, %v", confirmed, err)
	}
}

func TestHuhPromptsWithoutDescriptions(t *testing.T) {
	p := newHuhPrompter(&output{w: io.Discard})
	p.w = io.Discard

	p.in = repeat('\r')
	if _, err := p.Input("Label", "", "", nil); err != nil {
		t.Errorf("Input without a description or default: %v", err)
	}
	p.in = repeat('\r')
	if _, err := p.Select("Label", "", []wizard.Option{{Label: "one", Value: "one"}}, "one"); err != nil {
		t.Errorf("Select without a description: %v", err)
	}
	p.in = repeat('\r')
	if _, err := p.Confirm("Label", "", false); err != nil {
		t.Errorf("Confirm without a description: %v", err)
	}
}

func TestHuhSectionPrintsAHeading(t *testing.T) {
	var buf strings.Builder
	p := newHuhPrompter(&output{w: &buf})
	p.Section("Storage", "where files live")
	p.Section("Service", "")
	if !strings.Contains(buf.String(), "Storage") || !strings.Contains(buf.String(), "where files live") {
		t.Errorf("output = %q", buf.String())
	}
	if !strings.Contains(buf.String(), "Service") {
		t.Errorf("a section without a description should still print: %q", buf.String())
	}
}

func TestFlagPrompterEchoesItsAnswers(t *testing.T) {
	var buf strings.Builder
	p := &flagPrompter{out: &output{w: &buf}}

	p.Section("Storage", "ignored")
	// The default has to satisfy the validator on this platform: "/var/lib" is
	// not an absolute path on Windows.
	dataDir := wizard.Defaults().DataDir
	if got, err := p.Input("Data directory", "", dataDir, wizard.ValidateDir); err != nil || got != dataDir {
		t.Errorf("Input = %q, %v", got, err)
	}
	if got, err := p.Input("Public URL", "", "", nil); err != nil || got != "" {
		t.Errorf("Input = %q, %v", got, err)
	}
	if got, err := p.Select("Deployment", "", []wizard.Option{{Label: "compose", Value: "compose"}}, "compose"); err != nil || got != "compose" {
		t.Errorf("Select = %q, %v", got, err)
	}
	if got, err := p.Confirm("Telemetry", "", true); err != nil || !got {
		t.Errorf("Confirm = %t, %v", got, err)
	}
	text := buf.String()
	for _, want := range []string{"Storage", "Data directory", "(empty)", "compose", "true"} {
		if !strings.Contains(text, want) {
			t.Errorf("echo should contain %q:\n%s", want, text)
		}
	}
}

func TestFlagPrompterRejectsBadValues(t *testing.T) {
	p := &flagPrompter{}
	if _, err := p.Input("Maximum file size", "", "lots", wizard.ValidateSize); err == nil {
		t.Error("an invalid flag value should be reported")
	}
	if _, err := p.Select("Deployment", "", []wizard.Option{{Label: "compose", Value: "compose"}}, "kubernetes"); err == nil {
		t.Error("an unknown choice should be reported")
	}
	// A nil output must not panic.
	if _, err := p.Confirm("Telemetry", "", false); err != nil {
		t.Errorf("Confirm: %v", err)
	}
}

func TestMaybeStartWithoutDocker(t *testing.T) {
	var buf strings.Builder
	out := &output{w: &buf}
	a := wizard.Defaults()
	a.Start = false

	// Not requested: nothing happens.
	if err := maybeStart(t.Context(), out, testBuild(), a, "."); err != nil {
		t.Fatal(err)
	}
	if buf.Len() != 0 {
		t.Errorf("nothing should be printed: %q", buf.String())
	}

	// A systemd unit cannot be installed without root, so setup says so
	// rather than pretending to start anything.
	a.Deployment, a.Start = wizard.DeploySystemd, true
	if err := maybeStart(t.Context(), out, testBuild(), a, "."); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), "needs root") {
		t.Errorf("output = %q", buf.String())
	}
}

func TestVerifyReportsWhatItFound(t *testing.T) {
	var buf strings.Builder
	a := wizard.Defaults()
	a.BaseURL = ""
	a.Port = "1"
	a.Start = false
	verify(t.Context(), &output{w: &buf}, a)

	text := buf.String()
	if !strings.Contains(text, "Verifying") || !strings.Contains(text, "nothing listening yet") {
		t.Errorf("output = %q", text)
	}
	if !strings.Contains(text, "no public URL configured") {
		t.Errorf("without a base URL the external check should be skipped: %q", text)
	}
}

func TestVerifySkipsTheExternalCheckOnRequest(t *testing.T) {
	var buf strings.Builder
	a := wizard.Defaults()
	a.BaseURL = "https://files.example.com"
	a.ExternalCheck = false
	a.Port = "1"
	verify(t.Context(), &output{w: &buf}, a)

	if !strings.Contains(buf.String(), "curl -sI https://files.example.com/healthz") {
		t.Errorf("a skipped check should leave the user a manual command:\n%s", buf.String())
	}
}

// ------------------------------------------------------------ uninstall

func TestUninstallListsWhatItWouldRemove(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	config := filepath.Join(dir, ".godrop")
	if err := os.MkdirAll(config, 0o700); err != nil {
		t.Fatal(err)
	}

	code, out, _ := run(t, testBuild(), "uninstall", "--json")
	if code != 0 {
		t.Fatalf("exit = %d", code)
	}
	if !strings.Contains(out, config) {
		t.Errorf("output should list the configuration directory:\n%s", out)
	}
}

func TestUninstallLeavesFilesAloneWithoutPurge(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	data := filepath.Join(dir, "uploads")
	if err := os.MkdirAll(data, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GODROP_DATA_DIR", data)

	code, out, _ := run(t, testBuild(), "uninstall", "--json")
	if code != 0 {
		t.Fatalf("exit = %d", code)
	}
	if strings.Contains(out, data) {
		t.Errorf("uploads should be kept without --purge:\n%s", out)
	}

	code, out, _ = run(t, testBuild(), "uninstall", "--purge", "--json")
	if code != 0 || !strings.Contains(out, data) {
		t.Errorf("--purge should include the uploads:\n%s", out)
	}
}

func TestUninstallRemovesWhatItListed(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	config := filepath.Join(dir, ".godrop")
	if err := os.MkdirAll(config, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(config, ".env"), []byte("GODROP_TOKENS=gd_x"), 0o600); err != nil {
		t.Fatal(err)
	}

	code, out, stderr := run(t, testBuild(), "uninstall", "--yes")
	if code != 0 {
		t.Fatalf("exit = %d, stderr = %s", code, stderr)
	}
	if _, err := os.Stat(config); !os.IsNotExist(err) {
		t.Errorf("the configuration should be gone: %v", err)
	}
	if !strings.Contains(out, "Removing") {
		t.Errorf("output = %s", out)
	}
}

func TestUninstallRefusesAPackagedInstallation(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	original := osExecutable
	osExecutable = func() (string, error) { return filepath.Join(dir, "godrop"), nil }
	t.Cleanup(func() { osExecutable = original })

	// Pretend the binary came from a package: uninstall must point at the
	// package manager rather than deleting files it does not own.
	fakeManagedInstall(t)

	code, _, stderr := run(t, testBuild(), "uninstall", "--yes")
	if code == 0 {
		t.Fatal("a packaged installation should not be removed by hand")
	}
	if !strings.Contains(stderr, "apt remove") {
		t.Errorf("stderr = %q", stderr)
	}
}

func TestUninstallWithNothingToRemove(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	original := osExecutable
	osExecutable = func() (string, error) { return "", errors.New("no path") }
	t.Cleanup(func() { osExecutable = original })

	code, out, _ := run(t, testBuild(), "uninstall", "--yes")
	if code != 0 {
		t.Fatalf("exit = %d", code)
	}
	if !strings.Contains(out, "nothing to remove") {
		t.Errorf("output = %s", out)
	}
}

// fakeManagedInstall puts a dpkg on PATH that claims to own everything, which
// is how the updater decides an installation belongs to a package manager.
func fakeManagedInstall(t *testing.T) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("the stub is a POSIX shell script")
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "dpkg"), []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
}

// ------------------------------------------------------ start, doctor, form

func TestRunDoctorReportsAgainstTheRunningService(t *testing.T) {
	// A real GoDrop, so the diagnosis has something to look at.
	const token = "gd_a1b2c3d4e5f60718293a4b5c6d7e8f90"
	base, stop := serveInBackground(t, token, nil)
	defer stop()

	a := wizard.Defaults()
	a.BaseURL = base
	a.Token = token
	var buf strings.Builder
	runDoctor(t.Context(), &output{w: &buf}, testBuild(), a)

	text := buf.String()
	if !strings.Contains(text, "Checking it") {
		t.Errorf("output = %s", text)
	}
	if !strings.Contains(text, "reachability") && !strings.Contains(text, "upload") {
		t.Errorf("the diagnosis should have run:\n%s", text)
	}
}

func TestWaitForHealthGivesUpQuietly(t *testing.T) {
	// Nothing is listening, and the context is already done: waiting has to
	// end rather than hold up the setup for twenty seconds.
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	waitForHealth(ctx, "http://127.0.0.1:1")
}

func TestEchoAnswersRepeatsWhatTheFormCollected(t *testing.T) {
	var buf strings.Builder
	a := wizard.Defaults()
	a.BaseURL = "https://files.example.com"
	a.TLS = wizard.TLSAuto
	wizard.Finalise(&a)
	echoAnswers(&output{w: &buf}, a)
	for _, want := range []string{"public URL", "certificate", "docker volume", "limits"} {
		if !strings.Contains(buf.String(), want) {
			t.Errorf("output should mention %q:\n%s", want, buf.String())
		}
	}

	buf.Reset()
	a.Deployment = wizard.DeploySystemd
	a.DataDir = "/var/lib/godrop"
	echoAnswers(&output{w: &buf}, a)
	if !strings.Contains(buf.String(), "/var/lib/godrop") {
		t.Errorf("output should name the data directory:\n%s", buf.String())
	}
}

func TestTheFormCollectsTheSameAnswersAsTheFlags(t *testing.T) {
	// Driving the real form with Enter accepts every default, which should
	// land in the same place the non-interactive path does.
	got, err := runForm(repeat('\r'), io.Discard, wizard.Defaults())
	if err != nil {
		t.Fatalf("runForm: %v", err)
	}
	want, err := wizard.Run(&flagPrompter{}, wizard.Defaults())
	if err != nil {
		t.Fatal(err)
	}
	if got.Deployment != want.Deployment || got.DataDir != want.DataDir ||
		got.MaxFileSize != want.MaxFileSize || got.TLS != want.TLS {
		t.Errorf("form = %+v\nflags = %+v", got, want)
	}
}

func TestTheFormAsksTheCertificateQuestionForAPublicName(t *testing.T) {
	a := wizard.Defaults()
	a.BaseURL = "https://files.example.com"
	// The default answer for a public name is the automatic certificate, and
	// pressing Enter through the form accepts it.
	got, err := runForm(repeat('\r'), io.Discard, a)
	if err != nil {
		t.Fatalf("runForm: %v", err)
	}
	if got.TLS != wizard.TLSAuto {
		t.Errorf("TLS = %q, want the automatic certificate", got.TLS)
	}
}

func TestTheFormMovesOffAnAnswerThatIsNoLongerOffered(t *testing.T) {
	// Automatic TLS was pre-filled, then the address turned out to be one no
	// public authority can issue for: the option disappears, and the answer
	// cannot stay pointing at it.
	a := wizard.Defaults()
	a.BaseURL = "http://nas.local"
	a.TLS = wizard.TLSAuto
	got, err := runForm(repeat('\r'), io.Discard, a)
	if err != nil {
		t.Fatalf("runForm: %v", err)
	}
	if got.TLS == wizard.TLSAuto {
		t.Error("automatic TLS is not on offer for a .local address")
	}
}

func TestTheFormReportsCancellation(t *testing.T) {
	if _, err := runForm(repeat('\x03'), io.Discard, wizard.Defaults()); !errors.Is(err, errCancelled) {
		t.Errorf("err = %v, want the cancellation", err)
	}
}

func TestAnyLimitFlagSet(t *testing.T) {
	cmd := newInitCmd(testBuild())
	if anyLimitFlagSet(cmd) {
		t.Error("nothing was set")
	}
	if err := cmd.Flags().Set("retention", "30d"); err != nil {
		t.Fatal(err)
	}
	if !anyLimitFlagSet(cmd) {
		t.Error("a retention on the command line means the limits were set by hand")
	}
}

func TestUninstallAsksBeforeRemovingAnything(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	config := filepath.Join(dir, ".godrop")
	if err := os.MkdirAll(config, 0o700); err != nil {
		t.Fatal(err)
	}

	original := newInteractivePrompter
	newInteractivePrompter = func(*output) wizard.Prompter { return &decliningPrompter{} }
	t.Cleanup(func() { newInteractivePrompter = original })

	code, out, _ := run(t, testBuild(), "uninstall")
	if code != 0 {
		t.Fatalf("exit = %d", code)
	}
	if !strings.Contains(out, "Nothing was removed") {
		t.Errorf("output = %s", out)
	}
	if _, err := os.Stat(config); err != nil {
		t.Errorf("saying no should leave everything alone: %v", err)
	}
}

func TestUninstallPropagatesACancelledConfirmation(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	if err := os.MkdirAll(filepath.Join(dir, ".godrop"), 0o700); err != nil {
		t.Fatal(err)
	}
	original := newInteractivePrompter
	newInteractivePrompter = func(*output) wizard.Prompter { return &cancellingPrompter{} }
	t.Cleanup(func() { newInteractivePrompter = original })

	if code, _, _ := run(t, testBuild(), "uninstall"); code == 0 {
		t.Error("a cancelled confirmation should not exit 0")
	}
}

func TestUninstallReportsWhatItCouldNotRemove(t *testing.T) {
	requireStrictPermissions(t)
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	config := filepath.Join(dir, ".godrop")
	if err := os.MkdirAll(filepath.Join(config, "inner"), 0o700); err != nil {
		t.Fatal(err)
	}
	// A directory whose parent forbids removal.
	if err := os.Chmod(config, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(config, 0o700) })

	code, out, _ := run(t, testBuild(), "uninstall", "--yes")
	if code != 0 {
		t.Fatalf("exit = %d", code)
	}
	if !strings.Contains(out, "Removing") {
		t.Errorf("output = %s", out)
	}
}

func TestInitWritesIntoItsOwnDirectoryByDefault(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	code, _, stderr := run(t, testBuild(), "init", "--non-interactive",
		"--data-dir", t.TempDir(), "--no-external-check", "--deployment", "env")
	if code != 0 {
		t.Fatalf("exit = %d: %s", code, stderr)
	}
	// Not the working directory: setup keeps its files together, under the
	// home directory, rather than leaving them wherever it was run.
	if _, err := os.Stat(filepath.Join(home, ".godrop", ".env")); err != nil {
		t.Errorf("the configuration should land in ~/.godrop: %v", err)
	}
}

func TestInitStopsWhenTheChecksFail(t *testing.T) {
	requireStrictPermissions(t)
	outDir := t.TempDir()
	if err := os.Chmod(outDir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(outDir, 0o700) })

	code, out, _ := run(t, testBuild(), initArgs(outDir, t.TempDir())...)
	if code == 0 {
		t.Error("setup should stop before writing anything")
	}
	if !strings.Contains(out, "cannot write to") {
		t.Errorf("output = %s", out)
	}
	if _, err := os.Stat(filepath.Join(outDir, ".env")); err == nil {
		t.Error("nothing should have been written")
	}
}

func TestRunDoctorPointsAtTheCommandWhenSomethingIsWrong(t *testing.T) {
	original := healthWait
	healthWait = 10 * time.Millisecond
	t.Cleanup(func() { healthWait = original })

	a := wizard.Defaults()
	a.BaseURL = "http://127.0.0.1:1" // nothing is listening there
	a.Token = "gd_a1b2c3d4e5f60718293a4b5c6d7e8f90"
	var buf strings.Builder
	runDoctor(t.Context(), &output{w: &buf}, testBuild(), a)
	if !strings.Contains(buf.String(), "godrop doctor --url") {
		t.Errorf("output = %s", buf.String())
	}
}

func TestWaitForHealthIgnoresAnImpossibleURL(t *testing.T) {
	waitForHealth(t.Context(), "://not a url")
}

func TestTheInteractiveFormReadsTheTerminal(t *testing.T) {
	// The real entry point, with no terminal behind it: it has to come back
	// rather than block the setup for ever.
	if _, err := askInteractively(wizard.Defaults()); err == nil {
		t.Log("the form completed without a terminal, which is fine too")
	}
}

func TestTheHuhPrompterReportsCancellation(t *testing.T) {
	p := newHuhPrompter(&output{w: io.Discard})
	p.in, p.w = repeat('\x03'), io.Discard
	if _, err := p.Input("Public URL", "", "", nil); !errors.Is(err, errCancelled) {
		t.Errorf("err = %v, want the cancellation", err)
	}
}

func TestInitReportsAFormFailure(t *testing.T) {
	// Anything other than a cancellation comes back as it is: the wizard has
	// nothing useful to add to "the terminal went away".
	originalInteractive, originalAsk := interactive, askInteractively
	interactive = func() bool { return true }
	askInteractively = func(wizard.Answers) (wizard.Answers, error) {
		return wizard.Answers{}, errors.New("the terminal went away")
	}
	t.Cleanup(func() { interactive, askInteractively = originalInteractive, originalAsk })

	code, _, stderr := run(t, testBuild(), "init", "--out-dir", t.TempDir(), "--data-dir", t.TempDir())
	if code == 0 {
		t.Error("a failed form should not exit 0")
	}
	if !strings.Contains(stderr, "terminal went away") {
		t.Errorf("stderr = %q", stderr)
	}
}

func TestPlannedRemovalsListsEachPathOnce(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	t.Setenv("GODROP_DATA_DIR", "") // nothing configured: nothing to add
	config := filepath.Join(dir, ".godrop")
	if err := os.MkdirAll(config, 0o700); err != nil {
		t.Fatal(err)
	}

	// Nothing configured: the empty value is skipped rather than listed.
	for _, item := range plannedRemovals(true) {
		if item.Path == "" {
			t.Error("an empty path should never be listed")
		}
	}

	// The same directory twice, from the default and from the environment.
	t.Setenv("GODROP_DATA_DIR", config)
	seen := map[string]int{}
	for _, item := range plannedRemovals(true) {
		seen[item.Path]++
	}
	if seen[config] != 1 {
		t.Errorf("%s appears %d times, want once", config, seen[config])
	}
}

func TestUninstallLeavesSomebodyElsesComposeFileAlone(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	dir := t.TempDir()
	original, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(original) })

	// One compose file this program never wrote, and one it did.
	theirs := filepath.Join(dir, "docker-compose.yml")
	if err := os.WriteFile(theirs, []byte("services:\n  postgres:\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	ours := filepath.Join(dir, ".env")
	if err := os.WriteFile(ours, []byte("# GoDrop configuration, generated by `godrop init`\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	code, _, stderr := run(t, testBuild(), "uninstall", "--yes")
	if code != 0 {
		t.Fatalf("exit = %d, stderr = %s", code, stderr)
	}
	if _, err := os.Stat(theirs); err != nil {
		t.Errorf("a compose file GoDrop did not write must survive: %v", err)
	}
	if _, err := os.Stat(ours); !os.IsNotExist(err) {
		t.Errorf("the generated .env should be gone: %v", err)
	}
}
