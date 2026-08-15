package wizard

import (
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
	invalid := map[string]string{
		"files.example.com":              "http",
		"ftp://files.example.com":        "http",
		"https://":                       "host",
		"https://files.example.com/path": "path",
		"http://a b":                     "",
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

	if !strings.HasPrefix(caddy, "files.example.com {") {
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
	if !strings.HasPrefix(caddy, "files.example.com {") {
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
	compose := strings.Join(NextStepsFor("linux", withDeployment(DeployCompose, "https://f.example.com")), "\n")
	if !strings.Contains(compose, "docker compose up -d") || !strings.Contains(compose, "godrop doctor") {
		t.Errorf("compose steps = %s", compose)
	}
	if !strings.Contains(compose, "caddy") {
		t.Error("an https deployment should mention starting the proxy")
	}

	systemd := strings.Join(NextStepsFor("linux", withDeployment(DeploySystemd, "")), "\n")
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
	p := &scriptedPrompter{
		inputs:   []string{"https://files.example.com", dataDir, "250MB", "50GB", "30d", "9000"},
		selects:  []string{TLSNone, DeploySystemd},
		confirms: []bool{false, false},
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
	if got.Telemetry || got.ExternalCheck {
		t.Error("both confirmations answered no")
	}
	if len(p.sections) != 5 {
		t.Errorf("sections = %v, want the wizard grouped into five steps", p.sections)
	}
}

func TestRunKeepsDefaultsWhenAnswersAreEmpty(t *testing.T) {
	t.Parallel()
	got, err := Run(&scriptedPrompter{}, Defaults())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got.DataDir != absDir || got.MaxFileSize != "100MB" || got.Deployment != DeployCompose {
		t.Errorf("answers = %+v, want the defaults", got)
	}
	if !got.Telemetry || !got.ExternalCheck {
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
	// Ten questions: base URL, data dir, max file, quota, retention,
	// certificate, port, deployment, telemetry, external check.
	for stage := range 10 {
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
	if got := DefaultDataDir("linux", func(string) string { return "" }); got != "/var/lib/godrop" {
		t.Errorf("linux data dir = %q", got)
	}
	if got := DefaultDataDir("darwin", func(string) string { return "" }); got != "/var/lib/godrop" {
		t.Errorf("darwin data dir = %q", got)
	}
	got := DefaultDataDir("windows", func(key string) string {
		if key == "ProgramData" {
			return `C:\ProgramData`
		}
		return ""
	})
	if got != filepath.Join(`C:\ProgramData`, "GoDrop") {
		t.Errorf("windows data dir = %q", got)
	}
	if got := DefaultDataDir("windows", func(string) string { return "" }); got != `C:\ProgramData\GoDrop` {
		t.Errorf("windows fallback = %q", got)
	}
}

func TestDefaultsUseThisPlatform(t *testing.T) {
	t.Parallel()
	if Defaults().DataDir != DefaultDataDir(runtime.GOOS, os.Getenv) {
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
		inputs:  []string{"https://files.example.com", absDir, "100MB", "", "", cert, key},
		selects: []string{TLSFile, DeploySystemd},
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
	// Stage 6 aborts on the certificate, stage 7 on the key.
	for stage := 6; stage <= 7; stage++ {
		p := &scriptedPrompter{
			inputs:  []string{"https://files.example.com", absDir, "100MB", "", "", cert, key},
			selects: []string{TLSFile, DeploySystemd},
			err:     errCancelledForTest, failAfter: stage,
		}
		if _, err := Run(p, Defaults()); err == nil {
			t.Fatalf("a cancelled prompt at step %d must abort the wizard", stage)
		}
	}
}
