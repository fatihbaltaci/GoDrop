package cli

import (
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

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
	}
	return append(args, extra...)
}

func TestInitNonInteractiveWritesEverything(t *testing.T) {
	requirePOSIXModes(t)
	outDir, dataDir := t.TempDir(), t.TempDir()
	code, out, stderr := run(t, testBuild(), initArgs(outDir, dataDir,
		"--base-url", "https://files.example.com", "--json")...)
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
	if got.BaseURL != "https://files.example.com" || got.DataDir != dataDir {
		t.Errorf("answers = %+v", got)
	}
	if len(got.NextSteps) == 0 {
		t.Error("the next steps should be part of the machine-readable output")
	}

	// Compose deployment with an https base URL writes three files.
	names := map[string]bool{}
	for _, f := range got.Files {
		names[filepath.Base(f)] = true
	}
	for _, want := range []string{".env", "docker-compose.yml", "Caddyfile"} {
		if !names[want] {
			t.Errorf("%s was not written, got %v", want, got.Files)
		}
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

	// The token must be usable straight away.
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
	code, _, stderr := run(t, testBuild(), initArgs(t.TempDir(), t.TempDir(), "--max-file-size", "lots")...)
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

	code, _, stderr := run(t, testBuild(), "init", "--non-interactive",
		"--data-dir", t.TempDir(), "--no-external-check", "--deployment", "env")
	if code != 0 {
		t.Fatalf("exit = %d: %s", code, stderr)
	}
	if _, err := os.Stat(filepath.Join(dir, ".env")); err != nil {
		t.Errorf("the .env should land in the working directory: %v", err)
	}
}

func TestInitRunsTheRealFormsWhenATerminalIsPresent(t *testing.T) {
	// Pretend we are on a terminal and drive the real huh forms with keystrokes:
	// nine questions, each accepting its default with Enter.
	originalInteractive, originalPrompter := interactive, newInteractivePrompter
	interactive = func() bool { return true }
	newInteractivePrompter = func(out *output) wizard.Prompter {
		p := newHuhPrompter(out)
		p.in = repeat('\r')
		p.w = io.Discard
		return p
	}
	t.Cleanup(func() { interactive, newInteractivePrompter = originalInteractive, originalPrompter })

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
	originalInteractive, originalPrompter := interactive, newInteractivePrompter
	interactive = func() bool { return true }
	newInteractivePrompter = func(out *output) wizard.Prompter {
		p := newHuhPrompter(out)
		// Ctrl+C on the first question.
		p.in = repeat('\x03')
		p.w = io.Discard
		return p
	}
	t.Cleanup(func() { interactive, newInteractivePrompter = originalInteractive, originalPrompter })

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

	// Not requested: nothing happens.
	if err := maybeStart(t.Context(), out, a, false); err != nil {
		t.Fatal(err)
	}
	if buf.Len() != 0 {
		t.Errorf("nothing should be printed: %q", buf.String())
	}

	// Not a compose deployment: nothing happens either.
	a.Deployment = wizard.DeploySystemd
	if err := maybeStart(t.Context(), out, a, true); err != nil {
		t.Fatal(err)
	}
	if buf.Len() != 0 {
		t.Errorf("nothing should be printed: %q", buf.String())
	}
}

func TestVerifyReportsWhatItFound(t *testing.T) {
	var buf strings.Builder
	a := wizard.Defaults()
	a.BaseURL = ""
	a.Port = "1"
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
