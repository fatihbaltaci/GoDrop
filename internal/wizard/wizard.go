// Package wizard implements `godrop init`.
//
// The decisions live here as pure functions: validation, the exact content of
// every generated file, and the follow-up steps shown at the end. The
// interactive layer is a thin Prompter behind an interface, so the parts that
// can be wrong are the parts that are tested.
package wizard

import (
	"errors"
	"fmt"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/fatihbaltaci/GoDrop/internal/config"
)

// Deployment styles the wizard can set up.
const (
	DeployCompose = "compose"
	DeploySystemd = "systemd"
	DeployEnv     = "env"
)

// Answers is everything the wizard collects.
type Answers struct {
	BaseURL       string
	DataDir       string
	Port          string
	MaxFileSize   string
	MaxTotalSize  string
	Retention     string
	Deployment    string
	TLS           string // see TLSOptions
	TLSCert       string
	TLSKey        string
	TokenName     string
	Token         string // generated; shown once, never re-read from disk
	Telemetry     bool
	ExternalCheck bool
	Image         string
}

// Defaults returns the starting point offered to the user.
func Defaults() Answers {
	return Answers{
		DataDir:       DefaultDataDir(runtime.GOOS, os.Getenv),
		Port:          strings.TrimPrefix(config.DefaultAddr, ":"),
		MaxFileSize:   "100MB",
		MaxTotalSize:  "20GB",
		Retention:     "",
		Deployment:    DeployCompose,
		TokenName:     "default",
		Telemetry:     true,
		ExternalCheck: true,
		Image:         "ghcr.io/fatihbaltaci/godrop:latest",
	}
}

// How the certificate is obtained. These are the wizard's own names for it;
// the configuration they produce is described in SECURITY.md.
const (
	TLSAuto  = "auto"  // GoDrop gets one from Let's Encrypt and renews it
	TLSFile  = "file"  // a certificate the operator already has
	TLSProxy = "proxy" // something in front terminates TLS
	TLSNone  = "none"  // plain http, which is right on a private network
)

// TLSOptions offers the ways to get https, in the order most people want
// them. Automatic only appears for a public name, because Let's Encrypt
// cannot issue a certificate for an address or for something.local.
func TLSOptions(baseURL string) []Option {
	auto := Option{
		Label: "automatic, from Let's Encrypt (recommended)",
		Value: TLSAuto,
		Desc:  "GoDrop obtains the certificate and renews it. Needs ports 80 and 443, and a domain pointing here.",
	}
	rest := []Option{
		{Label: "I have a certificate", Value: TLSFile,
			Desc: "A certificate and key on disk, from certbot, your own CA or your cloud provider."},
		{Label: "something in front handles it", Value: TLSProxy,
			Desc: "Caddy, nginx, a load balancer or Cloudflare terminates TLS and forwards to GoDrop."},
		{Label: "none, plain http", Value: TLSNone,
			Desc: "Right on your own machine, a home network or Tailscale. Not on a public address."},
	}
	if CanAutoTLS(baseURL) {
		return append([]Option{auto}, rest...)
	}
	return rest
}

// CanAutoTLS reports whether Let's Encrypt could issue for this base URL.
func CanAutoTLS(baseURL string) bool {
	// The same rule the server enforces at startup, so the wizard never
	// offers an answer that would fail to start.
	return config.ValidTLSDomain(hostOf(baseURL)) == nil
}

func hostOf(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	return u.Hostname()
}

// ServesTLS reports whether GoDrop itself terminates TLS, which fixes the
// ports it listens on: 443 for traffic and 80 for the certificate challenge
// and the redirect.
func ServesTLS(a Answers) bool { return a.TLS == TLSAuto || a.TLS == TLSFile }

// needsProxy reports whether the setup relies on something in front of GoDrop
// for TLS, which is the only case where a proxy configuration is worth writing.
func needsProxy(a Answers) bool {
	return !ServesTLS(a) && strings.HasPrefix(a.BaseURL, "https://")
}

// ListenPort is the port GoDrop binds to.
func ListenPort(a Answers) string {
	if ServesTLS(a) {
		return "443"
	}
	return a.Port
}

// DefaultDataDir is where uploads live on each platform: the usual place for
// service state on Unix, and ProgramData on Windows.
func DefaultDataDir(goos string, env func(string) string) string {
	if goos == "windows" {
		if base := env("ProgramData"); base != "" {
			return filepath.Join(base, "GoDrop")
		}
		return `C:\ProgramData\GoDrop`
	}
	return "/var/lib/godrop"
}

// DeploymentOptions lists the ways GoDrop can be set up on this platform.
// systemd is offered only where it exists. On macOS and Windows it would be
// advice the user cannot follow.
func DeploymentOptions(goos string) []Option {
	options := []Option{
		{Label: "docker compose (recommended)", Value: DeployCompose, Desc: "writes docker-compose.yml and .env"},
	}
	if goos == "linux" {
		options = append(options, Option{
			Label: "systemd service", Value: DeploySystemd, Desc: "writes a hardened unit file",
		})
	}
	return append(options, Option{
		Label: ".env file only", Value: DeployEnv, Desc: "you start the binary yourself",
	})
}

// ValidateBaseURL accepts an absolute http(s) URL, or an empty string meaning
// "derive it from the request".
func ValidateBaseURL(s string) error {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil
	}
	u, err := url.Parse(s)
	if err != nil {
		return errors.New("not a valid URL")
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return errors.New("must start with http:// or https://")
	}
	if u.Host == "" {
		return errors.New("missing host name, e.g. https://files.example.com")
	}
	if u.Path != "" && u.Path != "/" {
		return errors.New("must not contain a path")
	}
	return nil
}

// ValidateSize accepts a human readable size such as 100MB.
func ValidateSize(s string) error {
	s = strings.TrimSpace(s)
	if s == "" {
		return errors.New("required, e.g. 100MB")
	}
	n, err := config.ParseSize(s)
	if err != nil {
		return errors.New("use a form like 100MB, 2GB or 512KB")
	}
	if n <= 0 {
		return errors.New("must be greater than zero")
	}
	return nil
}

// ValidateOptionalSize is ValidateSize but allows an empty value.
func ValidateOptionalSize(s string) error {
	if strings.TrimSpace(s) == "" {
		return nil
	}
	return ValidateSize(s)
}

// ValidateRetention accepts an empty value (keep forever) or a duration.
func ValidateRetention(s string) error {
	if strings.TrimSpace(s) == "" {
		return nil
	}
	d, err := config.ParseDuration(s)
	if err != nil {
		return errors.New("use a form like 30d, 12h or 90m")
	}
	if d <= 0 {
		return errors.New("must be greater than zero")
	}
	return nil
}

// ValidateDir requires an absolute, usable directory path.
func ValidateDir(s string) error {
	s = strings.TrimSpace(s)
	if s == "" {
		return errors.New("required, e.g. /var/lib/godrop")
	}
	if !filepath.IsAbs(s) {
		return errors.New("use an absolute path so the service finds it whatever its working directory is")
	}
	if info, err := os.Stat(s); err == nil && !info.IsDir() {
		return errors.New("a file already exists at that path")
	}
	return nil
}

// writeTLS records how the certificate is obtained, and says nothing at all
// when something else is terminating TLS.
func writeTLS(b *strings.Builder, a Answers) {
	switch a.TLS {
	case TLSAuto:
		writeEnv(b, "GODROP_TLS", "auto", "obtain and renew a certificate from Let's Encrypt")
		writeEnv(b, "GODROP_TLS_DOMAINS", hostOf(a.BaseURL), "the names the certificate covers")
		b.WriteString("# GODROP_TLS_EMAIL=you@example.com   # optional, for expiry warnings from Let's Encrypt\n")
	case TLSFile:
		writeEnv(b, "GODROP_TLS_CERT", a.TLSCert, "certificate chain, PEM")
		writeEnv(b, "GODROP_TLS_KEY", a.TLSKey, "private key, PEM")
	}
}

// ValidateFile requires a path to something readable. A certificate that
// turns out not to be there is much better caught here than at the first
// request, when nobody is watching.
func ValidateFile(s string) error {
	s = strings.TrimSpace(s)
	if s == "" {
		return errors.New("required")
	}
	if !filepath.IsAbs(s) {
		return errors.New("use an absolute path so the service finds it whatever its working directory is")
	}
	info, err := os.Stat(s)
	if err != nil {
		return errors.New("cannot read it: " + err.Error())
	}
	if info.IsDir() {
		return errors.New("that is a directory")
	}
	return nil
}

// ValidatePort requires a TCP port number.
func ValidatePort(s string) error {
	s = strings.TrimSpace(s)
	if s == "" {
		return errors.New("required, e.g. 8080")
	}
	var n int
	if _, err := fmt.Sscanf(s, "%d", &n); err != nil || n < 1 || n > 65535 {
		return errors.New("must be a number between 1 and 65535")
	}
	return nil
}

// EnvFile renders the .env file. It is the single source of truth for the
// other artefacts, which read the same values.
func EnvFile(a Answers) string {
	var b strings.Builder
	b.WriteString("# GoDrop configuration, generated by `godrop init`\n")
	b.WriteString("# Keep this file private: it contains your API token.\n\n")
	writeEnv(&b, "GODROP_TOKENS", a.Token, "API tokens, comma separated")
	writeEnv(&b, "GODROP_BASE_URL", a.BaseURL, "public URL; leave empty to derive it from the request")
	writeEnv(&b, "GODROP_DATA_DIR", a.dataDirForRuntime(), "where uploaded files live")
	writeEnv(&b, "GODROP_ADDR", ":"+ListenPort(a), "listen address")
	writeTLS(&b, a)
	b.WriteString("\n")
	writeEnv(&b, "GODROP_MAX_FILE_SIZE", a.MaxFileSize, "per-file limit")
	writeEnv(&b, "GODROP_MAX_TOTAL_SIZE", a.MaxTotalSize, "total storage quota; empty means unlimited")
	writeEnv(&b, "GODROP_RETENTION", a.Retention, "delete files after this long; empty means never")
	b.WriteString("\n")
	if !a.Telemetry {
		writeEnv(&b, "GODROP_TELEMETRY", "off", "anonymous heartbeat (install id, version, platform)")
	} else {
		b.WriteString("# GODROP_TELEMETRY=off   # disable the anonymous daily heartbeat\n")
	}
	return b.String()
}

func writeEnv(b *strings.Builder, key, value, comment string) {
	if comment != "" {
		fmt.Fprintf(b, "# %s\n", comment)
	}
	fmt.Fprintf(b, "%s=%s\n", key, value)
}

// dataDirForRuntime is the path the service sees. Inside a container the data
// directory is always /data; the host path becomes the volume source.
func (a Answers) dataDirForRuntime() string {
	if a.Deployment == DeployCompose {
		return "/data"
	}
	return a.DataDir
}

// ComposeFile renders docker-compose.yml.
func ComposeFile(a Answers) string {
	return fmt.Sprintf(`# GoDrop, generated by `+"`godrop init`"+`
# Start:  docker compose up -d
# Logs:   docker compose logs -f
services:
  godrop:
    image: %s
    restart: unless-stopped
    env_file: .env
    ports:
%s    volumes:
      - %s:/data
    healthcheck:
      test: ["CMD", "/godrop", "health"]
      interval: 30s
      timeout: 5s
      retries: 3
      start_period: 5s
`, a.Image, composePorts(a), a.DataDir)
}

// systemdCapabilities grants the one capability an unprivileged service needs
// to bind 443 and 80. Without it the unit starts as the godrop user and then
// fails with "permission denied", which is a confusing way to learn that
// ports below 1024 are privileged.
func systemdCapabilities(a Answers) string {
	if !ServesTLS(a) {
		return ""
	}
	return "\n# Binding 443 and 80 as an unprivileged user, and nothing else.\n" +
		"AmbientCapabilities=CAP_NET_BIND_SERVICE\n" +
		"CapabilityBoundingSet=CAP_NET_BIND_SERVICE\n"
}

// composePorts publishes what the deployment actually needs: 443 and 80 when
// GoDrop is doing TLS itself, because a certificate challenge arrives on 80.
func composePorts(a Answers) string {
	if ServesTLS(a) {
		return "      - \"443:443\"\n      - \"80:80\"\n"
	}
	return fmt.Sprintf("      - \"%s:%s\"\n", a.Port, a.Port)
}

// SystemdUnit renders a hardened service unit.
//
// Every path in it is joined with "/" rather than the local separator: a unit
// file is always read by Linux, even when it was generated on a Windows
// machine for a server somewhere else.
func SystemdUnit(a Answers, binaryPath string) string {
	if binaryPath == "" {
		binaryPath = "/usr/local/bin/godrop"
	}
	return fmt.Sprintf(`[Unit]
Description=GoDrop file host
Documentation=https://godrop.sh
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
User=godrop
Group=godrop
EnvironmentFile=%s
ExecStart=%s serve
Restart=on-failure
RestartSec=2

# Hardening: GoDrop only needs to read its binary and write its data directory.
NoNewPrivileges=true
PrivateTmp=true
PrivateDevices=true
ProtectSystem=strict
ProtectHome=true
ProtectKernelTunables=true
ProtectKernelModules=true
ProtectControlGroups=true
RestrictAddressFamilies=AF_INET AF_INET6
RestrictNamespaces=true
LockPersonality=true
MemoryDenyWriteExecute=true
ReadWritePaths=%s
%s
[Install]
WantedBy=multi-user.target
`, path.Join(a.DataDir, "godrop.env"), binaryPath, a.DataDir, systemdCapabilities(a))
}

// Caddyfile renders a reverse proxy with automatic TLS, including the body size
// limit that would otherwise silently break large uploads.
func Caddyfile(a Answers) string {
	host := "files.example.com"
	if u, err := url.Parse(a.BaseURL); err == nil && u.Host != "" {
		host = u.Host
	}
	size := a.MaxFileSize
	if size == "" {
		size = "100MB"
	}
	return fmt.Sprintf(`%s {
	# Caddy obtains and renews the TLS certificate automatically.
	reverse_proxy 127.0.0.1:%s

	# Must be at least as large as GODROP_MAX_FILE_SIZE, or the proxy will
	# reject big uploads before GoDrop ever sees them.
	request_body {
		max_size %s
	}
}
`, host, a.Port, size)
}

// GeneratedFile is one artefact the wizard writes.
type GeneratedFile struct {
	Name string
	Body string
	Perm os.FileMode
}

// Files returns everything to write for the chosen deployment style. The .env
// file is owner-only because it holds the token.
func Files(a Answers, binaryPath string) []GeneratedFile {
	files := []GeneratedFile{{Name: ".env", Body: EnvFile(a), Perm: 0o600}}
	switch a.Deployment {
	case DeployCompose:
		files = append(files, GeneratedFile{Name: "docker-compose.yml", Body: ComposeFile(a), Perm: 0o644})
	case DeploySystemd:
		files = append(files,
			GeneratedFile{Name: "godrop.service", Body: SystemdUnit(a, binaryPath), Perm: 0o644})
	}
	// A proxy configuration is only worth writing when something else is
	// terminating TLS. GoDrop doing it itself is the whole point of the
	// automatic and file answers.
	if needsProxy(a) {
		files = append(files, GeneratedFile{Name: "Caddyfile", Body: Caddyfile(a), Perm: 0o644})
	}
	return files
}

// Write persists the generated files into dir, refusing to clobber existing
// ones unless force is set.
func Write(dir string, files []GeneratedFile, force bool) ([]string, error) {
	var written []string
	for _, f := range files {
		path := filepath.Join(dir, f.Name)
		if _, err := os.Stat(path); err == nil {
			if !force {
				return written, fmt.Errorf("%s already exists (use --force to overwrite)", path)
			}
			// Remove first: WriteFile keeps the permissions of an existing
			// file, and .env must end up owner-only whatever it was before.
			_ = os.Remove(path)
		}
		if err := os.WriteFile(path, []byte(f.Body), f.Perm); err != nil {
			return written, fmt.Errorf("write %s: %w", path, err)
		}
		written = append(written, path)
	}
	return written, nil
}

// NextSteps is the closing checklist: how to start the service, how to verify
// it, and when a port must be reachable, how to open it.
func NextSteps(a Answers) []string { return NextStepsFor(runtime.GOOS, a) }

// NextStepsFor renders the checklist for a given platform. Commands a user
// cannot run are worse than no advice, so the shell syntax follows the host.
func NextStepsFor(goos string, a Answers) []string {
	var steps []string
	switch a.Deployment {
	case DeployCompose:
		steps = append(steps, "docker compose up -d")
	case DeploySystemd:
		steps = append(steps,
			"sudo useradd --system --home "+a.DataDir+" --shell /usr/sbin/nologin godrop || true",
			"sudo mkdir -p "+a.DataDir+" /etc/godrop && sudo chown godrop:godrop "+a.DataDir,
			"sudo install -m 640 -o root -g godrop .env /etc/godrop/godrop.env && rm .env",
			"sudo mv godrop.service /etc/systemd/system/godrop.service",
			"sudo systemctl daemon-reload && sudo systemctl enable --now godrop")
	case DeployEnv:
		if goos == "windows" {
			steps = append(steps,
				`Get-Content .env | ForEach-Object { if ($_ -match '^([^#=]+)=(.*)$') { [Environment]::SetEnvironmentVariable($Matches[1], $Matches[2]) } }`,
				"godrop serve")
		} else {
			steps = append(steps, "set -a && . ./.env && set +a", "godrop serve")
		}
	}
	if needsProxy(a) {
		steps = append(steps, "sudo caddy run --config Caddyfile   # or: sudo systemctl reload caddy")
	}
	steps = append(steps, "godrop doctor   # verify storage, firewall, TLS and reachability")
	return steps
}

// FirewallSteps returns the commands that open the public port, when the setup
// needs one. Cloud firewalls are named explicitly because they are invisible
// from inside the machine and are the most common reason a fresh install is
// unreachable.
func FirewallSteps(_ Answers, publicPort int) []string {
	if publicPort <= 0 {
		return nil
	}
	return []string{
		fmt.Sprintf("sudo ufw allow %d/tcp        # Debian/Ubuntu", publicPort),
		fmt.Sprintf("sudo firewall-cmd --permanent --add-port=%d/tcp && sudo firewall-cmd --reload   # RHEL/Fedora", publicPort),
		fmt.Sprintf("also open %d/tcp in your provider's firewall (AWS security group, Hetzner firewall, GCP VPC rule)", publicPort),
	}
}

// PublicPort is the port the outside world connects to, derived from the base
// URL when there is one.
func PublicPort(a Answers) int {
	if a.BaseURL != "" {
		u, err := url.Parse(a.BaseURL)
		if err == nil {
			if p := u.Port(); p != "" {
				var n int
				if _, err := fmt.Sscanf(p, "%d", &n); err == nil {
					return n
				}
			}
			if u.Scheme == "https" {
				return 443
			}
			if u.Scheme == "http" {
				return 80
			}
		}
	}
	var n int
	if _, err := fmt.Sscanf(a.Port, "%d", &n); err == nil {
		return n
	}
	return 0
}

// CurlExamples renders ready-to-run commands for the finished installation.
func CurlExamples(a Answers) []string { return CurlExamplesFor(runtime.GOOS, a) }

// CurlExamplesFor renders the examples for a given platform. On Windows the
// command is curl.exe: plain "curl" in PowerShell is an alias for
// Invoke-WebRequest, which does not understand these flags.
func CurlExamplesFor(goos string, a Answers) []string {
	base := a.BaseURL
	if base == "" {
		base = "http://localhost:" + a.Port
	}
	curl := "curl"
	if goos == "windows" {
		curl = "curl.exe"
	}
	return []string{
		fmt.Sprintf("%s -X POST -H \"Authorization: Bearer %s\" -F \"file=@photo.jpg\" %s/upload", curl, a.Token, base),
		fmt.Sprintf("%s -O %s/f/<id>/<name>", curl, base),
		fmt.Sprintf("%s -X DELETE -H \"Authorization: Bearer %s\" %s/f/<id>/<name>", curl, a.Token, base),
	}
}
