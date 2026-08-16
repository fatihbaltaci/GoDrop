package wizard

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// absDir is a directory that is absolute on whatever platform the tests run on:
// "/var/lib/godrop" is not an absolute path on Windows. Tests that assert on
// the contents of a Linux-only artefact (a systemd unit, say) set the path they
// need explicitly instead.
var absDir = Defaults().DataDir

func TestValidateBaseURL(t *testing.T) {
	t.Parallel()
	valid := []string{"", "  ", "https://files.example.com", "http://localhost:8080", "https://f.example.com/"}
	for _, v := range valid {
		if err := ValidateBaseURL(v); err != nil {
			t.Errorf("ValidateBaseURL(%q) = %v, want nil", v, err)
		}
	}
	// A bare host name is what people type, so it is accepted and normalised.
	for _, v := range []string{"files.example.com", "FILES.example.com", "files.example.com/"} {
		if err := ValidateBaseURL(v); err != nil {
			t.Errorf("ValidateBaseURL(%q) = %v, want a bare host accepted", v, err)
		}
	}
	invalid := map[string]string{
		"ftp://files.example.com":        "http",
		"https://":                       "host",
		"https://files.example.com/path": "path",
		"http://a b":                     "",
		"https://user@files.example.com": "user name",
		"godrop":                         "example.com",
		"files_example.com":              "underscore",
	}
	for v, want := range invalid {
		err := ValidateBaseURL(v)
		if err == nil {
			t.Errorf("ValidateBaseURL(%q) = nil, want an error", v)
			continue
		}
		if want != "" && !strings.Contains(err.Error(), want) {
			t.Errorf("ValidateBaseURL(%q) = %q, want it to mention %q", v, err, want)
		}
	}
}

func TestValidateSize(t *testing.T) {
	t.Parallel()
	for _, v := range []string{"100MB", "2GB", " 512KB ", "1024"} {
		if err := ValidateSize(v); err != nil {
			t.Errorf("ValidateSize(%q) = %v", v, err)
		}
	}
	for _, v := range []string{"", "   ", "lots", "0", "-5MB"} {
		if err := ValidateSize(v); err == nil {
			t.Errorf("ValidateSize(%q) = nil, want an error", v)
		}
	}
	if err := ValidateOptionalSize(""); err != nil {
		t.Errorf("an empty quota means unlimited: %v", err)
	}
	if err := ValidateOptionalSize("nonsense"); err == nil {
		t.Error("a malformed optional size should still be rejected")
	}
}

func TestValidateRetention(t *testing.T) {
	t.Parallel()
	for _, v := range []string{"", "30d", "12h", "90m", "1h30m"} {
		if err := ValidateRetention(v); err != nil {
			t.Errorf("ValidateRetention(%q) = %v", v, err)
		}
	}
	for _, v := range []string{"forever", "30days", "0", "-1h"} {
		if err := ValidateRetention(v); err == nil {
			t.Errorf("ValidateRetention(%q) = nil, want an error", v)
		}
	}
}

func TestValidateDir(t *testing.T) {
	t.Parallel()
	if err := ValidateDir(absDir); err != nil {
		t.Errorf("ValidateDir = %v", err)
	}
	for _, v := range []string{"", "  ", "relative/path", "./data"} {
		if err := ValidateDir(v); err == nil {
			t.Errorf("ValidateDir(%q) = nil, want an error", v)
		}
	}
	file := filepath.Join(t.TempDir(), "afile")
	if err := os.WriteFile(file, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := ValidateDir(file); err == nil {
		t.Error("a path already occupied by a file should be rejected")
	}
	if err := ValidateDir(t.TempDir()); err != nil {
		t.Errorf("an existing directory is fine: %v", err)
	}
}

func TestValidatePort(t *testing.T) {
	t.Parallel()
	for _, v := range []string{"80", "8080", "65535", "1"} {
		if err := ValidatePort(v); err != nil {
			t.Errorf("ValidatePort(%q) = %v", v, err)
		}
	}
	for _, v := range []string{"", "  ", "0", "65536", "-1", "http"} {
		if err := ValidatePort(v); err == nil {
			t.Errorf("ValidatePort(%q) = nil, want an error", v)
		}
	}
}

func TestThePortQuestionAsksTheMachineToo(t *testing.T) {
	// Not parallel: PortInUse is the seam the CLI fills in with a real bind.
	original := PortInUse
	t.Cleanup(func() { PortInUse = original })
	PortInUse = func(port string) error { return errors.New("something is on " + port) }

	// A number that is not a port never reaches the machine.
	if err := validateFreePort("nonsense"); err == nil || strings.Contains(err.Error(), "something is on") {
		t.Errorf("err = %v, want the range complaint", err)
	}
	if err := validateFreePort(" 8747 "); err == nil || !strings.Contains(err.Error(), "8747") {
		t.Errorf("err = %v, want the port named as taken", err)
	}
	// On its own the wizard opens no sockets and asserts nothing.
	PortInUse = original
	if err := validateFreePort("8747"); err != nil {
		t.Errorf("err = %v, want the wizard to keep out of the network", err)
	}
}

func TestLimitsOptionsSayWhatRecommendedMeans(t *testing.T) {
	t.Parallel()
	a := Defaults()
	if got := LimitsOptions(a)[0].Label; !strings.Contains(got, "20GB quota") {
		t.Errorf("label = %q, want the quota in it", got)
	}
	// An empty quota is unlimited, which "no quota" says and " quota" does not.
	a.MaxTotalSize = ""
	if got := LimitsOptions(a)[0].Label; !strings.Contains(got, "no quota") {
		t.Errorf("label = %q, want unlimited spelled out", got)
	}
}

func TestEnvFileContainsEverythingNeededToRun(t *testing.T) {
	t.Parallel()
	a := Defaults()
	a.BaseURL = "https://files.example.com"
	a.Token = "gd_secret"
	a.DataDir = "/var/lib/godrop"
	a.Deployment = DeployEnv

	env := EnvFile(a)
	for _, want := range []string{
		"GODROP_TOKENS=gd_secret",
		"GODROP_BASE_URL=https://files.example.com",
		"GODROP_DATA_DIR=/var/lib/godrop",
		"GODROP_ADDR=:" + Defaults().Port,
		"GODROP_MAX_FILE_SIZE=100MB",
		"GODROP_MAX_TOTAL_SIZE=20GB",
	} {
		if !strings.Contains(env, want) {
			t.Errorf(".env should contain %q:\n%s", want, env)
		}
	}
	if !strings.Contains(env, "# GODROP_TELEMETRY=off") {
		t.Error("the .env should document how to switch telemetry off")
	}
}

func TestEnvFileRecordsTelemetryOptOut(t *testing.T) {
	t.Parallel()
	a := Defaults()
	a.Telemetry = false
	if !strings.Contains(EnvFile(a), "GODROP_TELEMETRY=off") {
		t.Error("an explicit opt-out should be written into the .env")
	}
}

func TestComposeUsesAVolumeSoDataSurvives(t *testing.T) {
	t.Parallel()
	a := Defaults()
	a.DataDir = "/srv/godrop-data"
	compose := ComposeFile(a)

	if !strings.Contains(compose, "/srv/godrop-data:/data") {
		t.Errorf("the host directory must be mounted:\n%s", compose)
	}
	if !strings.Contains(compose, "env_file: .env") {
		t.Error("the compose file should read the generated .env")
	}
	if !strings.Contains(compose, `"`+Defaults().Port+`:`+Defaults().Port+`"`) {
		t.Error("the chosen port should be published")
	}
	if !strings.Contains(compose, "healthcheck") {
		t.Error("a healthcheck makes restarts observable")
	}
	if !strings.Contains(compose, "restart: unless-stopped") {
		t.Error("the service should come back after a reboot")
	}
}

func TestComposeRunsWithTheContainerDataDir(t *testing.T) {
	t.Parallel()
	a := Defaults()
	a.DataDir = "/srv/godrop-data"
	a.Deployment = DeployCompose
	if !strings.Contains(EnvFile(a), "GODROP_DATA_DIR=/data") {
		t.Error("inside a container the service must use the mount point, not the host path")
	}
}

func TestSystemdUnitIsHardened(t *testing.T) {
	t.Parallel()
	a := Defaults()
	a.Deployment = DeploySystemd
	a.DataDir = "/var/lib/godrop" // a systemd unit is a Linux artefact
	unit := SystemdUnit(a, "/usr/local/bin/godrop")

	for _, want := range []string{
		"ExecStart=/usr/local/bin/godrop serve",
		"User=godrop",
		"NoNewPrivileges=true",
		"ProtectSystem=strict",
		"ReadWritePaths=/var/lib/godrop",
		"Restart=on-failure",
		"EnvironmentFile=/var/lib/godrop/godrop.env",
	} {
		if !strings.Contains(unit, want) {
			t.Errorf("the unit should contain %q:\n%s", want, unit)
		}
	}
}

func TestSystemdUnitDefaultsTheBinaryPath(t *testing.T) {
	t.Parallel()
	if !strings.Contains(SystemdUnit(Defaults(), ""), "ExecStart=/usr/local/bin/godrop serve") {
		t.Error("a missing binary path should fall back to the usual install location")
	}
}

func TestCaddyfileRaisesTheBodyLimit(t *testing.T) {
	t.Parallel()
	a := Defaults()
	a.BaseURL = "https://files.example.com"
	a.MaxFileSize = "250MB"
	caddy := Caddyfile(a)

	if !strings.Contains(caddy, "\nfiles.example.com {") {
		t.Errorf("the site block should use the configured host:\n%s", caddy)
	}
	if !strings.Contains(caddy, "max_size 250MB") {
		t.Error("the proxy body limit must match the upload limit, or large uploads fail before reaching GoDrop")
	}
	if !strings.Contains(caddy, "reverse_proxy 127.0.0.1:"+Defaults().Port) {
		t.Error("the proxy should forward to the local service")
	}
}

func TestCaddyfileFallsBackToAPlaceholderHost(t *testing.T) {
	t.Parallel()
	a := Defaults()
	a.MaxFileSize = ""
	caddy := Caddyfile(a)
	if !strings.Contains(caddy, "\nfiles.example.com {") {
		t.Errorf("without a base URL a placeholder host is used:\n%s", caddy)
	}
	if !strings.Contains(caddy, "max_size 100MB") {
		t.Error("a missing size should fall back to the default")
	}
}

func TestFilesPerDeploymentStyle(t *testing.T) {
	t.Parallel()
	tests := []struct {
		deployment string
		baseURL    string
		want       []string
		absent     []string
	}{
		{DeployCompose, "https://f.example.com", []string{".env", "docker-compose.yml", "Caddyfile"}, nil},
		{DeploySystemd, "https://f.example.com", []string{".env", "godrop.service", "Caddyfile"}, []string{"docker-compose.yml"}},
		{DeployEnv, "", []string{".env"}, []string{"docker-compose.yml", "godrop.service", "Caddyfile"}},
		{DeployCompose, "http://localhost:8080", []string{".env", "docker-compose.yml"}, []string{"Caddyfile"}},
	}
	for _, tt := range tests {
		a := Defaults()
		a.Deployment = tt.deployment
		a.BaseURL = tt.baseURL
		names := map[string]os.FileMode{}
		for _, f := range Files(a, "/usr/local/bin/godrop") {
			names[f.Name] = f.Perm
		}
		for _, want := range tt.want {
			if _, ok := names[want]; !ok {
				t.Errorf("%s deployment should generate %s, got %v", tt.deployment, want, names)
			}
		}
		for _, absent := range tt.absent {
			if _, ok := names[absent]; ok {
				t.Errorf("%s deployment should not generate %s", tt.deployment, absent)
			}
		}
		if perm := names[".env"]; perm != 0o600 {
			t.Errorf(".env mode = %#o, want 0600, it holds the token", perm)
		}
	}
}

func TestWriteCreatesFilesWithTheRightPermissions(t *testing.T) {
	requirePOSIXModes(t)
	t.Parallel()
	dir := t.TempDir()
	a := Defaults()
	a.Token = "gd_secret"
	a.BaseURL = "https://files.example.com"

	written, err := Write(dir, Files(a, ""), false)
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	if len(written) != 3 {
		t.Fatalf("wrote %v, want three files", written)
	}
	info, err := os.Stat(filepath.Join(dir, ".env"))
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf(".env mode = %#o, want 0600", perm)
	}
	data, err := os.ReadFile(filepath.Join(dir, ".env"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "gd_secret") {
		t.Error("the token should be written into the .env")
	}
}

func TestWriteRefusesToClobber(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	a := Defaults()
	if _, err := Write(dir, Files(a, ""), false); err != nil {
		t.Fatal(err)
	}
	_, err := Write(dir, Files(a, ""), false)
	if err == nil || !strings.Contains(err.Error(), "--force") {
		t.Fatalf("err = %v, want a refusal that names the escape hatch", err)
	}
	if _, err := Write(dir, Files(a, ""), true); err != nil {
		t.Errorf("--force should overwrite: %v", err)
	}
}

func TestWriteReportsFailures(t *testing.T) {
	t.Parallel()
	requireStrictPermissions(t)
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })
	if _, err := Write(dir, Files(Defaults(), ""), false); err == nil {
		t.Fatal("an unwritable directory should be reported")
	}
}

func TestNextStepsMatchTheDeploymentStyle(t *testing.T) {
	t.Parallel()
	behindProxy := withDeployment(DeployCompose, "https://f.example.com")
	behindProxy.Start = false
	behindProxy.TLS = TLSProxy
	compose := strings.Join(NextStepsFor("linux", behindProxy), "\n")
	if !strings.Contains(compose, "docker compose up -d") || !strings.Contains(compose, "godrop doctor") {
		t.Errorf("compose steps = %s", compose)
	}
	if !strings.Contains(compose, "caddy") {
		t.Error("a deployment that relies on a proxy should mention starting it")
	}
	// When GoDrop has the certificate itself there is no proxy to start.
	itsOwn := behindProxy
	itsOwn.TLS = TLSAuto
	if strings.Contains(strings.Join(NextStepsFor("linux", itsOwn), "\n"), "caddy") {
		t.Error("nothing should mention caddy when GoDrop serves https itself")
	}

	systemdAnswers := withDeployment(DeploySystemd, "")
	systemdAnswers.Start = false
	systemd := strings.Join(NextStepsFor("linux", systemdAnswers), "\n")
	for _, want := range []string{"useradd", "systemctl enable --now godrop", "/etc/godrop/godrop.env"} {
		if !strings.Contains(systemd, want) {
			t.Errorf("systemd steps should include %q:\n%s", want, systemd)
		}
	}

	plain := strings.Join(NextStepsFor("linux", withDeployment(DeployEnv, "")), "\n")
	if !strings.Contains(plain, ". ./.env") || !strings.Contains(plain, "godrop serve") {
		t.Errorf("env steps = %s", plain)
	}

	windows := strings.Join(NextStepsFor("windows", withDeployment(DeployEnv, "")), "\n")
	if strings.Contains(windows, "set -a") {
		t.Errorf("Windows must not be told to use shell syntax it cannot run:\n%s", windows)
	}
	if !strings.Contains(windows, "SetEnvironmentVariable") {
		t.Errorf("windows steps = %s", windows)
	}
}

func withDeployment(kind, baseURL string) Answers {
	a := Defaults()
	a.Deployment = kind
	a.BaseURL = baseURL
	return a
}

func TestPublicPort(t *testing.T) {
	t.Parallel()
	tests := []struct {
		baseURL, port string
		want          int
	}{
		{"https://files.example.com", "8080", 443},
		{"http://files.example.com", "8080", 80},
		{"https://files.example.com:8443", "8080", 8443},
		{"", "8080", 8080},
		{"", "not-a-port", 0},
		{"://broken", "9000", 9000},
	}
	for _, tt := range tests {
		a := Defaults()
		a.BaseURL, a.Port = tt.baseURL, tt.port
		if got := PublicPort(a); got != tt.want {
			t.Errorf("PublicPort(%q, %q) = %d, want %d", tt.baseURL, tt.port, got, tt.want)
		}
	}
}

func TestFirewallStepsNameCloudFirewallsToo(t *testing.T) {
	t.Parallel()
	steps := FirewallSteps(Defaults(), 443)
	joined := strings.Join(steps, "\n")
	for _, want := range []string{"ufw allow 443/tcp", "firewall-cmd", "security group"} {
		if !strings.Contains(joined, want) {
			t.Errorf("firewall steps should mention %q:\n%s", want, joined)
		}
	}
	if FirewallSteps(Defaults(), 0) != nil {
		t.Error("no port means no firewall advice")
	}
	if FirewallSteps(Defaults()) != nil {
		t.Error("no ports at all means no firewall advice")
	}
}

func TestServingTLSNeedsBothPortsOpen(t *testing.T) {
	t.Parallel()
	// Opening 443 and forgetting 80 is the commonest reason an otherwise
	// correct install never gets a certificate.
	a := Defaults()
	a.BaseURL = "https://files.example.com"
	a.TLS = TLSAuto
	if got := PublicPorts(a); len(got) != 2 || got[0] != 443 || got[1] != 80 {
		t.Errorf("PublicPorts = %v, want 443 and 80", got)
	}
	joined := strings.Join(FirewallSteps(a, PublicPorts(a)...), "\n")
	for _, want := range []string{"ufw allow 443,80/tcp", "--add-port=443/tcp --add-port=80/tcp", "443,80/tcp in your provider"} {
		if !strings.Contains(joined, want) {
			t.Errorf("firewall steps should mention %q:\n%s", want, joined)
		}
	}

	// Behind a proxy only the public port matters.
	a.TLS = TLSProxy
	if got := PublicPorts(a); len(got) != 1 || got[0] != 443 {
		t.Errorf("PublicPorts = %v, want just the public one", got)
	}
	// A plain http install on port 80 needs it named once, not twice.
	a.TLS = TLSFile
	a.BaseURL = "http://files.example.com"
	if got := PublicPorts(a); len(got) != 1 || got[0] != 80 {
		t.Errorf("PublicPorts = %v, want 80 alone", got)
	}
	// Nothing to say without an address at all.
	a.BaseURL, a.Port = "", ""
	if got := PublicPorts(a); len(got) != 1 || got[0] != 80 {
		t.Errorf("PublicPorts = %v, want the challenge port", got)
	}
	a.TLS = TLSNone
	if got := PublicPorts(a); got != nil {
		t.Errorf("PublicPorts = %v, want none", got)
	}
}

func TestCurlExamplesAreReadyToRun(t *testing.T) {
	t.Parallel()
	a := Defaults()
	a.BaseURL = "https://files.example.com"
	a.Token = "gd_abc"
	examples := strings.Join(CurlExamplesFor("linux", a), "\n")
	if !strings.Contains(examples, `-H "Authorization: Bearer gd_abc"`) {
		t.Errorf("examples should carry the real token:\n%s", examples)
	}
	if !strings.Contains(examples, "https://files.example.com/upload") {
		t.Error("examples should use the configured base URL")
	}

	a.BaseURL = ""
	local := strings.Join(CurlExamplesFor("linux", a), "\n")
	if !strings.Contains(local, "http://localhost:"+Defaults().Port+"/upload") {
		t.Errorf("without a base URL the examples should target localhost:\n%s", local)
	}

	// In PowerShell "curl" is an alias for Invoke-WebRequest, which would fail
	// on these flags, so Windows users are given curl.exe.
	windows := strings.Join(CurlExamplesFor("windows", a), "\n")
	if !strings.Contains(windows, "curl.exe -X POST") {
		t.Errorf("windows examples = %s", windows)
	}
}

// ------------------------------------------------------------------ the flow

// scriptedPrompter answers each question from a fixed script, which is how the
// whole wizard is exercised without a terminal.
type scriptedPrompter struct {
	inputs   []string
	selects  []string
	confirms []bool
	sections []string
	labels   []string
	err      error
	// failAfter makes the prompter fail once this many questions have been
	// answered, so every abort point in the flow can be exercised.
	failAfter int
	asked     int
}

func (p *scriptedPrompter) next() error {
	p.asked++
	if p.err != nil && p.asked > p.failAfter {
		return p.err
	}
	return nil
}

func (p *scriptedPrompter) Section(title, _ string) { p.sections = append(p.sections, title) }

func (p *scriptedPrompter) Input(label, _, def string, validate func(string) error) (string, error) {
	p.labels = append(p.labels, label)
	if err := p.next(); err != nil {
		return "", err
	}
	value := def
	if len(p.inputs) > 0 {
		value, p.inputs = p.inputs[0], p.inputs[1:]
	}
	if validate != nil {
		if err := validate(value); err != nil {
			return "", err
		}
	}
	return value, nil
}

func (p *scriptedPrompter) Select(_, _ string, _ []Option, def string) (string, error) {
	if err := p.next(); err != nil {
		return "", err
	}
	if len(p.selects) > 0 {
		value := p.selects[0]
		p.selects = p.selects[1:]
		return value, nil
	}
	return def, nil
}

func (p *scriptedPrompter) Confirm(_, _ string, def bool) (bool, error) {
	if err := p.next(); err != nil {
		return false, err
	}
	if len(p.confirms) > 0 {
		value := p.confirms[0]
		p.confirms = p.confirms[1:]
		return value, nil
	}
	return def, nil
}

func TestRunCollectsEveryAnswer(t *testing.T) {
	t.Parallel()
	dataDir := filepath.Join(absDir, "custom")
	// In order: public URL, data directory, then the four limit questions the
	// "no" to the recommended limits opens up.
	p := &scriptedPrompter{
		inputs:  []string{"https://files.example.com", dataDir, "250MB", "50GB", "30d", "9000"},
		selects: []string{DeploySystemd, TLSNone, LimitsAdvanced},
	}
	got, err := Run(p, Defaults())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	want := Answers{
		BaseURL: "https://files.example.com", DataDir: dataDir,
		MaxFileSize: "250MB", MaxTotalSize: "50GB", Retention: "30d",
		Port: "9000", Deployment: DeploySystemd, TLS: TLSNone,
	}
	if got.BaseURL != want.BaseURL || got.DataDir != want.DataDir || got.MaxFileSize != want.MaxFileSize ||
		got.MaxTotalSize != want.MaxTotalSize || got.Retention != want.Retention ||
		got.Port != want.Port || got.Deployment != want.Deployment || got.TLS != want.TLS {
		t.Errorf("answers = %+v, want %+v", got, want)
	}
	// The heartbeat and starting the service are not questions: they are told
	// at the end, so the defaults survive the whole conversation.
	if !got.Telemetry || !got.Start {
		t.Error("nothing in the wizard asks about the heartbeat or the start")
	}
	if len(p.sections) != 5 {
		t.Errorf("sections = %v, want one heading per step", p.sections)
	}
}

func TestRunKeepsDefaultsWhenAnswersAreEmpty(t *testing.T) {
	t.Parallel()
	got, err := Run(&scriptedPrompter{}, Defaults())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	// Compose keeps its files in a volume, so the default answers leave no
	// host directory behind.
	if got.DataDir != "" || got.MaxFileSize != "100MB" || got.Deployment != DeployCompose {
		t.Errorf("answers = %+v, want the defaults", got)
	}
	if !got.Telemetry || !got.Start {
		t.Error("the confirmations default to yes")
	}
}

func TestRunStopsAtTheFirstFailure(t *testing.T) {
	t.Parallel()
	p := &scriptedPrompter{inputs: []string{"not a url"}}
	if _, err := Run(p, Defaults()); err == nil {
		t.Fatal("an invalid answer should stop the wizard")
	}
	if len(p.labels) != 1 {
		t.Errorf("the wizard asked %d questions after a failure, want to stop at the first", len(p.labels))
	}
}

func TestRunPropagatesCancellationAtEveryStep(t *testing.T) {
	t.Parallel()
	// With the defaults there are three: public URL, deployment and the
	// recommended limits. Compose keeps its files in a volume, so there is no
	// data directory to ask about.
	for stage := range 3 {
		p := &scriptedPrompter{err: errCancelledForTest, failAfter: stage}
		if _, err := Run(p, Defaults()); err == nil {
			t.Fatalf("a cancelled prompt at step %d must abort the wizard", stage)
		}
	}
}

var errCancelledForTest = errCancelled{}

type errCancelled struct{}

func (errCancelled) Error() string { return "cancelled" }

// requireStrictPermissions skips a test that depends on POSIX permission
// semantics. As root every mode is writable anyway, and on Windows chmod only
// toggles a read-only bit, so the situations these tests create cannot exist.
func requireStrictPermissions(t *testing.T) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("file modes are advisory on Windows")
	}
	if os.Geteuid() == 0 {
		t.Skip("permission checks are meaningless as root")
	}
}

// requirePOSIXModes skips a test that asserts exact file modes. Windows has no
// POSIX permission bits, so a file created with 0600 does not report 0600.
func requirePOSIXModes(t *testing.T) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("file modes are not POSIX bits on Windows")
	}
}

func TestDeploymentOptionsMatchThePlatform(t *testing.T) {
	t.Parallel()
	linux := DeploymentOptions("linux")
	if len(linux) != 3 {
		t.Fatalf("linux options = %d, want compose, systemd and env", len(linux))
	}
	if !strings.Contains(linux[0].Label, "recommended") {
		t.Errorf("the first option should be marked recommended, got %q", linux[0].Label)
	}

	for _, goos := range []string{"darwin", "windows"} {
		for _, o := range DeploymentOptions(goos) {
			if o.Value == DeploySystemd {
				t.Errorf("%s has no systemd, so it must not be offered", goos)
			}
		}
		if len(DeploymentOptions(goos)) != 2 {
			t.Errorf("%s options = %+v, want compose and env", goos, DeploymentOptions(goos))
		}
	}
}

func TestDefaultDataDirPerPlatform(t *testing.T) {
	t.Parallel()
	none := func(string) string { return "" }
	if got := DefaultDataDir("linux", none, true); got != "/var/lib/godrop" {
		t.Errorf("linux data dir as root = %q", got)
	}
	if got := DefaultDataDir("darwin", none, true); got != "/var/lib/godrop" {
		t.Errorf("darwin data dir as root = %q", got)
	}
	got := DefaultDataDir("windows", func(key string) string {
		if key == "ProgramData" {
			return `C:\ProgramData`
		}
		return ""
	}, false)
	if got != filepath.Join(`C:\ProgramData`, "GoDrop") {
		t.Errorf("windows data dir = %q", got)
	}
	if got := DefaultDataDir("windows", none, false); got != `C:\ProgramData\GoDrop` {
		t.Errorf("windows fallback = %q", got)
	}
}

func TestWithoutRootTheDataDirectoryIsSomewhereWritable(t *testing.T) {
	t.Parallel()
	// Suggesting /var/lib/godrop to someone who cannot create it is how a
	// wizard gets to the last question and then fails on a mkdir.
	env := func(key string) string {
		switch key {
		case "XDG_DATA_HOME":
			return "/home/ubuntu/.data"
		case "HOME":
			return "/home/ubuntu"
		}
		return ""
	}
	if got := DefaultDataDir("linux", env, false); got != "/home/ubuntu/.data/godrop" {
		t.Errorf("data dir = %q, want the XDG location", got)
	}
	home := func(key string) string {
		if key == "HOME" {
			return "/home/ubuntu"
		}
		return ""
	}
	if got := DefaultDataDir("linux", home, false); got != "/home/ubuntu/.local/share/godrop" {
		t.Errorf("data dir = %q, want the home location", got)
	}
	if got := DefaultDataDir("linux", func(string) string { return "" }, false); got != "/var/lib/godrop" {
		t.Errorf("data dir = %q, want the system location when there is no home", got)
	}
}

func TestDefaultsUseThisPlatform(t *testing.T) {
	t.Parallel()
	if Defaults().DataDir != DefaultDataDir(runtime.GOOS, os.Getenv, os.Geteuid() == 0) {
		t.Error("Defaults should use this platform's data directory")
	}
}

func TestPlatformWrappersUseThisHost(t *testing.T) {
	t.Parallel()
	a := withDeployment(DeployEnv, "")
	a.Token = "gd_abc"

	if got, want := NextSteps(a), NextStepsFor(runtime.GOOS, a); strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("NextSteps should render for this platform:\n%v\n%v", got, want)
	}
	if got, want := CurlExamples(a), CurlExamplesFor(runtime.GOOS, a); strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("CurlExamples should render for this platform:\n%v\n%v", got, want)
	}
}

// ---------------------------------------------------------------- HTTPS

func TestAutomaticTLSIsOfferedOnlyForAPublicName(t *testing.T) {
	t.Parallel()
	// Let's Encrypt cannot issue for an address or for a private name, and
	// offering it there would only produce a failure later.
	public := []string{"https://files.example.com", "http://files.example.com", "https://a.b.example.org"}
	for _, url := range public {
		if !CanAutoTLS(url) {
			t.Errorf("CanAutoTLS(%q) = false, want it offered", url)
		}
		if TLSOptions(url)[0].Value != TLSAuto {
			t.Errorf("%s: automatic should be the first option", url)
		}
	}
	private := []string{"", "https://192.0.2.10", "https://localhost:8747", "https://nas.local",
		"https://laptop.tail1234.ts.net", "https://godrop.internal", "://nonsense"}
	for _, url := range private {
		if CanAutoTLS(url) {
			t.Errorf("CanAutoTLS(%q) = true, want it left out", url)
		}
		for _, o := range TLSOptions(url) {
			if o.Value == TLSAuto {
				t.Errorf("%s: automatic should not be offered", url)
			}
		}
	}
}

func TestTheCertificateQuestionStartsOnTheEasiestAnswer(t *testing.T) {
	t.Parallel()
	a := Defaults()
	a.BaseURL = "https://files.example.com"
	if got := defaultTLS(a); got != TLSAuto {
		t.Errorf("defaultTLS = %q, want automatic for a public name", got)
	}
	a.BaseURL = ""
	if got := defaultTLS(a); got != TLSNone {
		t.Errorf("defaultTLS = %q, want none without a URL", got)
	}
	a.BaseURL = "https://nas.local"
	if got := defaultTLS(a); got != TLSProxy {
		t.Errorf("defaultTLS = %q, want a proxy for a name Let's Encrypt cannot issue for", got)
	}
	a.TLS = TLSFile
	if got := defaultTLS(a); got != TLSFile {
		t.Errorf("defaultTLS = %q, want an explicit answer respected", got)
	}
}

func TestServingTLSFixesThePorts(t *testing.T) {
	t.Parallel()
	a := Defaults()
	a.BaseURL = "https://files.example.com"
	a.TLS = TLSAuto

	if !ServesTLS(a) || ListenPort(a) != "443" {
		t.Errorf("with automatic TLS, GoDrop listens on 443, got %q", ListenPort(a))
	}
	env := EnvFile(a)
	for _, want := range []string{"GODROP_ADDR=:443", "GODROP_TLS=auto", "GODROP_TLS_DOMAINS=files.example.com"} {
		if !strings.Contains(env, want) {
			t.Errorf(".env should contain %q:\n%s", want, env)
		}
	}
	compose := ComposeFile(a)
	for _, want := range []string{`"443:443"`, `"80:80"`} {
		if !strings.Contains(compose, want) {
			t.Errorf("compose should publish %s, since the challenge arrives on 80:\n%s", want, compose)
		}
	}
	// An unprivileged service cannot bind 443 without being allowed to.
	unit := SystemdUnit(a, "")
	if !strings.Contains(unit, "AmbientCapabilities=CAP_NET_BIND_SERVICE") {
		t.Errorf("the unit needs the capability to bind 443:\n%s", unit)
	}

	a.TLS = TLSNone
	if ServesTLS(a) || ListenPort(a) != a.Port {
		t.Errorf("without TLS the chosen port is used, got %q", ListenPort(a))
	}
	if strings.Contains(SystemdUnit(a, ""), "AmbientCapabilities") {
		t.Error("a service on a high port needs no capability")
	}
	if strings.Contains(EnvFile(a), "GODROP_TLS") {
		t.Error("nothing about TLS should be written when there is none")
	}
}

func TestYourOwnCertificateIsWrittenOut(t *testing.T) {
	t.Parallel()
	a := Defaults()
	a.TLS = TLSFile
	a.TLSCert = "/etc/letsencrypt/live/files.example.com/fullchain.pem"
	a.TLSKey = "/etc/letsencrypt/live/files.example.com/privkey.pem"
	env := EnvFile(a)
	if !strings.Contains(env, "GODROP_TLS_CERT="+a.TLSCert) || !strings.Contains(env, "GODROP_TLS_KEY="+a.TLSKey) {
		t.Errorf(".env should name both files:\n%s", env)
	}
}

func TestValidateFile(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	cert := filepath.Join(dir, "fullchain.pem")
	if err := os.WriteFile(cert, []byte("-----BEGIN CERTIFICATE-----"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := ValidateFile(cert); err != nil {
		t.Errorf("ValidateFile = %v", err)
	}
	for _, bad := range []string{"", "  ", "relative/cert.pem", filepath.Join(dir, "not-there"), dir} {
		if err := ValidateFile(bad); err == nil {
			t.Errorf("ValidateFile(%q) = nil, want an error", bad)
		}
	}
}

func TestRunAsksForACertificateWhenYouBringYourOwn(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	cert, key := filepath.Join(dir, "fullchain.pem"), filepath.Join(dir, "privkey.pem")
	for _, f := range []string{cert, key} {
		if err := os.WriteFile(f, []byte("-----BEGIN-----"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	p := &scriptedPrompter{
		inputs:  []string{"https://files.example.com", cert, key, absDir},
		selects: []string{DeploySystemd, TLSFile},
	}
	got, err := Run(p, Defaults())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got.TLSCert != cert || got.TLSKey != key {
		t.Errorf("answers = %+v, want both files recorded", got)
	}
	// Terminating TLS means the ports are decided, so the wizard does not ask.
	for _, label := range p.labels {
		if label == "Listen port" {
			t.Error("the listen port should not be asked when GoDrop serves 443")
		}
	}
}

func TestRunPropagatesCancellationWhileAskingForTheCertificate(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	cert, key := filepath.Join(dir, "fullchain.pem"), filepath.Join(dir, "privkey.pem")
	for _, f := range []string{cert, key} {
		if err := os.WriteFile(f, []byte("-----BEGIN-----"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	// Stage 2 aborts on the certificate, stage 3 on the key.
	for stage := 2; stage <= 3; stage++ {
		p := &scriptedPrompter{
			inputs:  []string{"https://files.example.com", cert, key, absDir},
			selects: []string{DeploySystemd, TLSFile},
			err:     errCancelledForTest, failAfter: stage,
		}
		if _, err := Run(p, Defaults()); err == nil {
			t.Fatalf("a cancelled prompt at step %d must abort the wizard", stage)
		}
	}
}

func TestNormalizeBaseURL(t *testing.T) {
	t.Parallel()
	cases := map[string]string{
		"":                              "",
		"  ":                            "",
		"files.gurubase.io":             "https://files.gurubase.io",
		"FILES.Gurubase.io":             "https://files.gurubase.io",
		"files.gurubase.io/":            "https://files.gurubase.io",
		"https://files.gurubase.io":     "https://files.gurubase.io",
		"http://files.gurubase.io":      "http://files.gurubase.io",
		"http://localhost:8747":         "http://localhost:8747",
		"//files.gurubase.io":           "https://files.gurubase.io",
		"https://files.gurubase.io:443": "https://files.gurubase.io:443",
	}
	for in, want := range cases {
		if got := NormalizeBaseURL(in); got != want {
			t.Errorf("NormalizeBaseURL(%q) = %q, want %q", in, got, want)
		}
	}
	// Something url.Parse refuses comes back untouched, for the validator to
	// report rather than the normaliser to mangle.
	if got := NormalizeBaseURL("https://a b"); got != "https://a b" {
		t.Errorf("NormalizeBaseURL = %q", got)
	}
}

func TestTheCertificateQuestionIsOnlyAskedWhenItHasAnAnswer(t *testing.T) {
	t.Parallel()
	a := Defaults()
	if AsksTLS(a) {
		t.Error("without a public URL there is nothing to get a certificate for")
	}
	a.BaseURL = "http://nas.local:8747"
	if AsksTLS(a) {
		t.Error("an http:// address has already answered the question")
	}
	a.BaseURL = "https://files.example.com"
	if !AsksTLS(a) {
		t.Error("an https address needs a certificate from somewhere")
	}
}

func TestRunSkipsTheCertificateQuestionWithoutAPublicURL(t *testing.T) {
	t.Parallel()
	p := &scriptedPrompter{}
	got, err := Run(p, Defaults())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got.TLS != TLSNone {
		t.Errorf("TLS = %q, want none", got.TLS)
	}
	for _, section := range p.sections {
		if section == "HTTPS" {
			t.Errorf("sections = %v, want no HTTPS step", p.sections)
		}
	}
}

// ------------------------------------------------------- the question list

func TestTheQuestionListIsShortAndDependsOnTheAnswers(t *testing.T) {
	t.Parallel()
	// A local run with the recommended limits: as few questions as the wizard
	// can get away with.
	a := Defaults()
	asked := applicable(QuestionsFor("linux"), a)
	if len(asked) > 5 {
		t.Errorf("a local run asks %d questions (%v), want a handful", len(asked), asked)
	}
	for _, label := range asked {
		if label == "Certificate" || label == "Maximum file size" {
			t.Errorf("%q should not be asked with defaults and no public URL", label)
		}
	}

	// A public address with docker compose: a certificate question, and no
	// data directory, because docker keeps the files in a volume.
	a.BaseURL = "https://files.example.com"
	a.Deployment = DeployCompose
	asked = applicable(QuestionsFor("linux"), a)
	if !contains(asked, "Certificate") {
		t.Errorf("questions = %v, want the certificate question", asked)
	}
	if contains(asked, "Data directory") {
		t.Errorf("questions = %v, want no data directory under compose", asked)
	}

	// systemd puts the files on this machine, so it has to be asked.
	a.Deployment = DeploySystemd
	if !contains(applicable(QuestionsFor("linux"), a), "Data directory") {
		t.Error("a systemd service needs a directory to write into")
	}

	// Setting the limits by hand opens four more questions.
	a.Limits = LimitsAdvanced
	a.TLS = TLSProxy
	asked = applicable(QuestionsFor("linux"), a)
	for _, want := range []string{"Maximum file size", "Storage quota", "Delete files after", "Listen port"} {
		if !contains(asked, want) {
			t.Errorf("questions = %v, want %q", asked, want)
		}
	}
	// Serving TLS fixes the port, so that question goes away again.
	a.TLS = TLSAuto
	if contains(applicable(QuestionsFor("linux"), a), "Listen port") {
		t.Error("the port is 443 when GoDrop serves TLS; there is nothing to ask")
	}
}

func TestEveryQuestionCanAnswerForItself(t *testing.T) {
	t.Parallel()
	a := Defaults()
	for _, q := range QuestionsFor("linux") {
		if q.Label == "" || q.Section == "" {
			t.Errorf("question %+v needs a label and a section", q)
		}
		if q.Describe(a) == "" {
			t.Errorf("%q has no description; every question explains itself", q.Label)
		}
		switch q.Kind {
		case KindSelect:
			if q.Options == nil || len(q.Options(a)) == 0 {
				t.Errorf("%q is a select with no options", q.Label)
			}
			fallthrough
		case KindInput:
			if q.Str == nil {
				t.Errorf("%q has nowhere to put the answer", q.Label)
			}
		}
	}
}

func TestAQuestionWithoutADescriptionSaysNothing(t *testing.T) {
	t.Parallel()
	if got := (Question{Label: "x"}).Describe(Defaults()); got != "" {
		t.Errorf("Describe = %q, want nothing", got)
	}
}

func TestFinaliseClearsWhatWasNeverAsked(t *testing.T) {
	t.Parallel()
	a := Defaults()
	a.Deployment = DeployCompose
	a.DataDir = "/var/lib/godrop"
	a.BaseURL = "files.example.com"
	Finalise(&a)
	if a.DataDir != "" {
		t.Errorf("DataDir = %q, want it cleared for a docker volume", a.DataDir)
	}
	if a.BaseURL != "https://files.example.com" {
		t.Errorf("BaseURL = %q, want it normalised", a.BaseURL)
	}

	a.Deployment = DeploySystemd
	a.DataDir = "/var/lib/godrop"
	Finalise(&a)
	if a.DataDir != "/var/lib/godrop" {
		t.Errorf("DataDir = %q, want it kept when the service writes to disk", a.DataDir)
	}
}

func TestComposeUsesAVolumeWhenNoDirectoryWasChosen(t *testing.T) {
	t.Parallel()
	a := Defaults()
	a.Deployment = DeployCompose
	a.DataDir = ""
	compose := ComposeFile(a)
	if !strings.Contains(compose, "- godrop-data:/data") || !strings.Contains(compose, "\nvolumes:\n  godrop-data:") {
		t.Errorf("compose should declare and mount a named volume:\n%s", compose)
	}

	a.DataDir = absDir
	compose = ComposeFile(a)
	if !strings.Contains(compose, "- "+absDir+":/data") {
		t.Errorf("compose should mount the chosen directory:\n%s", compose)
	}
	if strings.Contains(compose, "\nvolumes:") {
		t.Errorf("a host directory needs no volume declaration:\n%s", compose)
	}
}

func TestNextStepsLeaveOutWhatSetupAlreadyDid(t *testing.T) {
	t.Parallel()
	a := Defaults()
	a.Deployment = DeployCompose
	a.Start = true
	steps := strings.Join(NextStepsFor("linux", a), "\n")
	if strings.Contains(steps, "docker compose up") || strings.Contains(steps, "godrop doctor") {
		t.Errorf("steps = %q, want nothing that setup has already run", steps)
	}

	a.Start = false
	steps = strings.Join(NextStepsFor("linux", a), "\n")
	if !strings.Contains(steps, "docker compose up") || !strings.Contains(steps, "godrop doctor") {
		t.Errorf("steps = %q, want the commands when nothing was started", steps)
	}
}

func TestConfigDir(t *testing.T) {
	t.Parallel()
	none := func(string) string { return "" }
	if got := ConfigDir("linux", none, true); got != "/etc/godrop" {
		t.Errorf("root config dir = %q", got)
	}
	home := func(key string) string {
		if key == "HOME" {
			return "/home/ubuntu"
		}
		return ""
	}
	if got := ConfigDir("linux", home, false); got != "/home/ubuntu/.godrop" {
		t.Errorf("config dir = %q, want it under the home directory", got)
	}
	if got := ConfigDir("linux", none, false); got != "." {
		t.Errorf("config dir = %q, want the working directory as a last resort", got)
	}

	// Windows has no HOME: settings live in APPDATA, with USERPROFILE and
	// then the machine-wide ProgramData as fallbacks.
	appdata := func(key string) string {
		switch key {
		case "APPDATA":
			return `C:\Users\fatih\AppData\Roaming`
		case "USERPROFILE":
			return `C:\Users\fatih`
		}
		return ""
	}
	if got := ConfigDir("windows", appdata, false); got != filepath.Join(`C:\Users\fatih\AppData\Roaming`, "GoDrop") {
		t.Errorf("windows config dir = %q", got)
	}
	profile := func(key string) string {
		if key == "USERPROFILE" {
			return `C:\Users\fatih`
		}
		return ""
	}
	if got := ConfigDir("windows", profile, false); got != filepath.Join(`C:\Users\fatih`, ".godrop") {
		t.Errorf("windows config dir = %q", got)
	}
	machine := func(key string) string {
		if key == "ProgramData" {
			return `C:\ProgramData`
		}
		return ""
	}
	if got := ConfigDir("windows", machine, false); got != filepath.Join(`C:\ProgramData`, "GoDrop") {
		t.Errorf("windows config dir = %q", got)
	}
	if got := ConfigDir("windows", none, false); got != "." {
		t.Errorf("windows config dir = %q", got)
	}
}

// applicable lists the labels of the questions that would be put to someone
// with these answers.
func applicable(questions []Question, a Answers) []string {
	var labels []string
	for _, q := range questions {
		if q.Applies(a) {
			labels = append(labels, q.Label)
		}
	}
	return labels
}

func contains(list []string, want string) bool {
	for _, item := range list {
		if item == want {
			return true
		}
	}
	return false
}

func TestAnAnswerThatIsNoLongerOfferedIsReplaced(t *testing.T) {
	t.Parallel()
	// Automatic was chosen for a public name, and then the address was edited
	// into one no public authority can issue for.
	a := Defaults()
	a.BaseURL = "https://nas.local"
	a.TLS = TLSAuto
	Finalise(&a)
	if a.TLS != TLSProxy {
		t.Errorf("TLS = %q, want the answer moved off an option nobody can choose", a.TLS)
	}

	// And an unanswered certificate question starts on the easiest answer.
	a.BaseURL = "https://files.example.com"
	a.TLS = ""
	Finalise(&a)
	if a.TLS != TLSAuto {
		t.Errorf("TLS = %q, want the automatic certificate", a.TLS)
	}
}

func TestEveryGeneratedFileSaysWhoWroteIt(t *testing.T) {
	t.Parallel()
	// `godrop uninstall` removes a file in the working directory only when it
	// carries this line, so that it never deletes somebody else's compose
	// file or Caddyfile.
	a := Defaults()
	a.BaseURL = "https://files.example.com"
	a.Token = "gd_abc"
	for name, body := range map[string]string{
		".env":               EnvFile(a),
		"docker-compose.yml": ComposeFile(a),
		"godrop.service":     SystemdUnit(a, "/usr/bin/godrop"),
		"Caddyfile":          Caddyfile(a),
	} {
		first, _, _ := strings.Cut(body, "\n")
		if !strings.Contains(first, "generated by `godrop init`") {
			t.Errorf("%s starts with %q, want it to say who wrote it", name, first)
		}
	}
}
