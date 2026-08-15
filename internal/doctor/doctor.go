// Package doctor diagnoses a GoDrop installation: configuration, storage,
// security posture, reachability from the internet and available updates.
//
// Every check reports pass, warn or fail together with the exact command that
// fixes it. A failing check makes `godrop doctor` exit non-zero, so it can be
// used as a deployment gate.
package doctor

import (
	"context"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/fatihbaltaci/GoDrop/internal/config"
	"github.com/fatihbaltaci/GoDrop/internal/netcheck"
	"github.com/fatihbaltaci/GoDrop/internal/storage"
	"github.com/fatihbaltaci/GoDrop/internal/tokens"
)

// Status is the outcome of a single check.
type Status string

// Possible check outcomes.
const (
	Pass Status = "pass"
	Warn Status = "warn"
	Fail Status = "fail"
	Skip Status = "skip"
)

// Check is one diagnosed item.
type Check struct {
	Group  string `json:"group"`
	Name   string `json:"name"`
	Status Status `json:"status"`
	Detail string `json:"detail,omitempty"`
	Fix    string `json:"fix,omitempty"`
}

// Report is the full result.
type Report struct {
	OK      bool    `json:"ok"`
	Version string  `json:"version"`
	Checks  []Check `json:"checks"`
}

// Failed reports whether any check failed.
func (r Report) Failed() bool {
	for _, c := range r.Checks {
		if c.Status == Fail {
			return true
		}
	}
	return false
}

// Options configures Run.
type Options struct {
	Config    *config.Config
	ConfigErr error
	Version   string
	Env       func(string) string
	HTTP      *http.Client
	Now       func() time.Time
	Runner    netcheck.Runner
	Offline   bool
	CheckURL  string
	// TLSRoots overrides the trust store used for the certificate check, for
	// deployments behind a private certificate authority.
	TLSRoots *x509.CertPool
	// TargetURL and Token diagnose a remote instance instead of the local one.
	TargetURL string
	Token     string
	WorkDir   string
}

type runner struct {
	Options
	checks []Check
	env    func(string) string
	now    func() time.Time
	http   *http.Client
}

// Run executes every applicable check.
func Run(ctx context.Context, opts Options) Report {
	r := &runner{Options: opts}
	r.env = opts.Env
	if r.env == nil {
		r.env = os.Getenv
	}
	r.now = opts.Now
	if r.now == nil {
		r.now = func() time.Time { return time.Now().UTC() }
	}
	r.http = opts.HTTP
	if r.http == nil {
		r.http = &http.Client{Timeout: 15 * time.Second}
	}

	r.checkConfig()
	r.checkStorage()
	r.checkSecurity()
	r.checkNetwork(ctx)
	r.checkEndToEnd(ctx)
	r.checkVersion(ctx)

	report := Report{Version: opts.Version, Checks: r.checks}
	report.OK = !report.Failed()
	return report
}

func (r *runner) add(group, name string, status Status, detail, fix string) {
	r.checks = append(r.checks, Check{Group: group, Name: name, Status: status, Detail: detail, Fix: fix})
}

// ------------------------------------------------------------------- config

func (r *runner) checkConfig() {
	const g = "config"
	if r.ConfigErr != nil {
		r.add(g, "configuration", Fail, r.ConfigErr.Error(), "fix the environment variables, or run `godrop init`")
		return
	}
	cfg := r.Config
	if cfg == nil {
		r.add(g, "configuration", Skip, "no local configuration (diagnosing a remote instance)", "")
		return
	}
	r.add(g, "tokens", Pass, fmt.Sprintf("%d token(s) configured", len(cfg.Tokens)), "")

	if cfg.BaseURL == "" {
		r.add(g, "base_url", Warn,
			"GODROP_BASE_URL is not set; URLs are derived from the request Host header",
			"set GODROP_BASE_URL=https://files.example.com so returned URLs are always correct")
	} else if u, err := url.Parse(cfg.BaseURL); err != nil || u.Scheme == "" || u.Host == "" {
		r.add(g, "base_url", Fail, fmt.Sprintf("GODROP_BASE_URL=%q is not a valid absolute URL", cfg.BaseURL),
			"use the form https://files.example.com")
	} else {
		r.add(g, "base_url", Pass, cfg.BaseURL, "")
	}

	r.add(g, "max_file_size", Pass, config.FormatSize(cfg.MaxFileSize), "")
	switch {
	case cfg.MaxTotalSize == 0:
		r.add(g, "storage_quota", Warn, "no quota set; uploads can fill the disk",
			"set GODROP_MAX_TOTAL_SIZE=20GB (leave headroom for the operating system)")
	case cfg.MaxFileSize > cfg.MaxTotalSize:
		// An upload holds its share of the quota for as long as it runs, so a
		// per-file limit larger than the whole quota means one upload in
		// progress can turn every other one away.
		r.add(g, "storage_quota", Warn,
			fmt.Sprintf("the per-file limit (%s) is larger than the whole quota (%s)",
				config.FormatSize(cfg.MaxFileSize), config.FormatSize(cfg.MaxTotalSize)),
			"lower GODROP_MAX_FILE_SIZE, or raise GODROP_MAX_TOTAL_SIZE above it")
	default:
		r.add(g, "storage_quota", Pass, config.FormatSize(cfg.MaxTotalSize), "")
	}
}

// ------------------------------------------------------------------ storage

func (r *runner) checkStorage() {
	const g = "storage"
	if r.Config == nil {
		return
	}
	dir := r.Config.DataDir
	abs, err := filepath.Abs(dir)
	if err == nil {
		dir = abs
	}

	info, err := os.Stat(dir)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		r.add(g, "data_dir", Warn, dir+" does not exist yet", "it is created on the first upload; `godrop serve` creates it at startup")
		return
	case err != nil:
		r.add(g, "data_dir", Fail, err.Error(), "check the path and its permissions")
		return
	case !info.IsDir():
		r.add(g, "data_dir", Fail, dir+" is not a directory", "point GODROP_DATA_DIR at a directory")
		return
	}

	st, err := storage.New(dir, r.Config.MaxTotalSize)
	if err != nil {
		r.add(g, "data_dir", Fail, err.Error(), "check ownership: chown -R $(id -u):$(id -g) "+dir)
		return
	}
	if err := st.Writable(); err != nil {
		r.add(g, "writable", Fail, err.Error(),
			"the volume may be read-only or not mounted: docker run -v godrop-data:/data ...")
	} else {
		r.add(g, "writable", Pass, dir+" is writable", "")
	}

	files, bytes := st.Stats()
	detail := fmt.Sprintf("%d file(s), %s", files, config.FormatSize(bytes))
	if q := r.Config.MaxTotalSize; q > 0 {
		detail += fmt.Sprintf(" of %s (%.0f%%)", config.FormatSize(q), float64(bytes)/float64(q)*100)
		if bytes > q*9/10 {
			r.add(g, "usage", Warn, detail, "delete old files or raise GODROP_MAX_TOTAL_SIZE")
		} else {
			r.add(g, "usage", Pass, detail, "")
		}
	} else {
		r.add(g, "usage", Pass, detail, "")
	}

	if free, total, ok := diskFree(dir); ok {
		d := fmt.Sprintf("%s free of %s", config.FormatSize(free), config.FormatSize(total))
		switch {
		case free < 100<<20:
			r.add(g, "disk_space", Fail, d, "free up space; uploads will start failing")
		case free < 1<<30:
			r.add(g, "disk_space", Warn, d, "less than 1GB left on the device")
		default:
			r.add(g, "disk_space", Pass, d, "")
		}
	}

	if orphans, err := st.Orphans(); err == nil && len(orphans) > 0 {
		names := make([]string, 0, 3)
		for _, o := range orphans[:min(3, len(orphans))] {
			names = append(names, filepath.Base(o.Path)+" ("+o.Reason+")")
		}
		r.add(g, "orphans", Warn,
			fmt.Sprintf("%d file(s) do not match the storage layout: %s", len(orphans), strings.Join(names, ", ")),
			"these are unreachable through the API; remove them manually if they are crash leftovers")
	} else {
		r.add(g, "orphans", Pass, "no stray files", "")
	}

	r.checkPersistence(dir)
}

// checkPersistence catches the single most common self-hosting mistake: running
// in a container without mounting a volume, so every restart wipes the uploads.
func (r *runner) checkPersistence(dir string) {
	if !inContainer() {
		return
	}
	mounts, err := readMounts()
	if err != nil {
		return
	}
	for line := range strings.SplitSeq(string(mounts), "\n") {
		fields := strings.Fields(line)
		if len(fields) >= 2 && (fields[1] == dir || strings.HasPrefix(dir, fields[1]+"/")) && fields[1] != "/" {
			r.add("storage", "persistence", Pass, dir+" is on a mounted volume", "")
			return
		}
	}
	r.add("storage", "persistence", Fail,
		dir+" is inside the container filesystem, not a volume — uploads are lost on restart",
		"mount a volume: docker run -v godrop-data:/data ... (Fly: [mounts], Render: disk, Railway: volume)")
}

// ----------------------------------------------------------------- security

func (r *runner) checkSecurity() {
	const g = "security"
	if r.Config == nil {
		return
	}
	weak := 0
	for _, t := range r.Config.Tokens {
		if isWeakToken(t) {
			weak++
		}
	}
	switch {
	case weak > 0:
		r.add(g, "token_strength", Fail,
			fmt.Sprintf("%d token(s) are short or guessable", weak),
			"generate a strong one: godrop token create --name prod")
	default:
		r.add(g, "token_strength", Pass, "all tokens look strong", "")
	}

	if info, err := os.Stat(r.Config.DataDir); err == nil {
		if perm := info.Mode().Perm(); perm&0o007 != 0 {
			r.add(g, "data_dir_perms", Warn, fmt.Sprintf("%s is %#o (world-accessible)", r.Config.DataDir, perm),
				"chmod 700 "+r.Config.DataDir)
		} else {
			r.add(g, "data_dir_perms", Pass, fmt.Sprintf("%#o", perm), "")
		}
	}

	tokenFile := tokens.Path(r.Config.DataDir)
	if info, err := os.Stat(tokenFile); err == nil {
		if perm := info.Mode().Perm(); perm&0o077 != 0 {
			r.add(g, "token_file_perms", Fail, fmt.Sprintf("%s is %#o", tokenFile, perm), "chmod 600 "+tokenFile)
		} else {
			r.add(g, "token_file_perms", Pass, fmt.Sprintf("%#o", perm), "")
		}
	}

	r.checkEnvFile()

	if r.Config.BaseURL != "" && strings.HasPrefix(r.Config.BaseURL, "http://") &&
		!strings.Contains(r.Config.BaseURL, "localhost") && !strings.Contains(r.Config.BaseURL, "127.0.0.1") {
		r.add(g, "https", Fail, "GODROP_BASE_URL uses plain http; tokens travel in clear text",
			"put Caddy or nginx in front for automatic TLS (see deploy/Caddyfile)")
	} else if r.Config.BaseURL != "" {
		r.add(g, "https", Pass, "TLS in use", "")
	}

	for _, o := range r.Config.CORSOrigins {
		if o == "*" {
			r.add(g, "cors", Warn, "GODROP_CORS_ORIGINS is * (any site may call the API from a browser)",
				"restrict it if you only call GoDrop from your own front end: GODROP_CORS_ORIGINS=https://app.example.com")
			break
		}
	}

	if uid := geteuid(); uid == 0 {
		r.add(g, "privileges", Warn, "running as root",
			"run as an unprivileged user; the systemd unit in deploy/ already does")
	} else if uid > 0 {
		r.add(g, "privileges", Pass, fmt.Sprintf("uid=%d", uid), "")
	}
}

func (r *runner) checkEnvFile() {
	dir := r.WorkDir
	if dir == "" {
		dir = "."
	}
	envPath := filepath.Join(dir, ".env")
	info, err := os.Stat(envPath)
	if err != nil {
		return
	}
	if perm := info.Mode().Perm(); perm&0o077 != 0 {
		r.add("security", "env_file_perms", Fail, fmt.Sprintf("%s is %#o and contains your tokens", envPath, perm),
			"chmod 600 "+envPath)
	} else {
		r.add("security", "env_file_perms", Pass, fmt.Sprintf("%#o", perm), "")
	}
	if gitTracked(dir, ".env") {
		r.add("security", "env_in_git", Fail, ".env is tracked by git — your tokens are in the repository",
			"git rm --cached .env && echo .env >> .gitignore")
	}
}

func isWeakToken(t string) bool {
	if len(t) < 16 {
		return true
	}
	lower := strings.ToLower(t)
	for _, bad := range []string{"test", "changeme", "secret", "password", "token", "godrop", "example", "12345"} {
		if strings.Contains(lower, bad) {
			return true
		}
	}
	return false
}

// ------------------------------------------------------------------ network

func (r *runner) checkNetwork(ctx context.Context) {
	const g = "network"
	target := r.targetURL()
	if target == "" {
		r.add(g, "reachability", Skip, "no base URL configured; nothing to probe from outside", "")
		return
	}

	if r.Config != nil {
		addr := localAddr(r.Config.Addr)
		if netcheck.Listening(ctx, addr) {
			r.add(g, "listening", Pass, addr+" accepts connections", "")
		} else {
			r.add(g, "listening", Warn, "nothing is listening on "+addr,
				"start the service: docker compose up -d  (or systemctl start godrop)")
		}
		if port := portOf(r.Config.Addr, target); port > 0 {
			fw := netcheck.CheckFirewall(ctx, r.Runner, port)
			switch {
			case !fw.Inspected:
				r.add(g, "firewall", Skip, fw.Hint, "")
			case fw.PortOpen:
				r.add(g, "firewall", Pass, fmt.Sprintf("%s allows port %d", fw.Tool, port), "")
			default:
				r.add(g, "firewall", Fail, fmt.Sprintf("%s is active and port %d is not allowed", fw.Tool, port), fw.Hint)
			}
		}
	}

	if r.Offline {
		r.add(g, "external", Skip, "offline mode", "")
		return
	}

	u, err := url.Parse(target)
	if err != nil || u.Host == "" {
		r.add(g, "dns", Fail, fmt.Sprintf("cannot parse %q", target), "use the form https://files.example.com")
		return
	}

	if host := u.Hostname(); host != "" && !isLocalHost(host) {
		addrs, err := netcheck.Resolve(ctx, nil, host)
		if err != nil {
			r.add(g, "dns", Fail, host+" does not resolve: "+err.Error(),
				"add an A record pointing "+host+" at this server's public IP")
		} else {
			r.add(g, "dns", Pass, host+" → "+strings.Join(addrs, ", "), "")
			if ip, err := netcheck.PublicIP(ctx, r.http); err == nil {
				if !slicesContains(addrs, ip) {
					r.add(g, "dns_points_here", Warn,
						fmt.Sprintf("%s resolves to %s but this machine's public IP is %s", host, strings.Join(addrs, ", "), ip),
						"this is expected behind Cloudflare or a load balancer; otherwise fix the DNS record")
				} else {
					r.add(g, "dns_points_here", Pass, "DNS points at this machine ("+ip+")", "")
				}
			}
		}
	}

	if strings.HasPrefix(target, "https://") {
		if hostport, err := netcheck.HostPort(target); err == nil {
			tlsInfo := netcheck.CheckTLS(ctx, hostport, r.now(), r.TLSRoots)
			switch {
			case tlsInfo.Error != "":
				r.add(g, "tls", Fail, tlsInfo.Error, "check that your reverse proxy has a certificate for this name")
			case tlsInfo.DaysLeft < 14:
				r.add(g, "tls", Warn, fmt.Sprintf("certificate expires in %d days", tlsInfo.DaysLeft), "check certificate renewal")
			default:
				r.add(g, "tls", Pass, fmt.Sprintf("valid, %d days left (%s)", tlsInfo.DaysLeft, tlsInfo.Issuer), "")
			}
		}
	}

	res, err := netcheck.External(ctx, r.http, r.CheckURL, strings.TrimRight(target, "/")+"/healthz")
	switch {
	case err != nil:
		r.add(g, "external", Warn, "reachability service unavailable: "+err.Error(),
			"verify manually from another machine: curl -sI "+target+"/healthz")
	case res.OK:
		detail := fmt.Sprintf("reachable from the internet (HTTP %d", res.Status)
		if res.Location != "" {
			detail += ", checked from " + res.Location
		}
		r.add(g, "external", Pass, detail+")", "")
	default:
		msg := res.Error
		if msg == "" {
			msg = fmt.Sprintf("HTTP %d", res.Status)
		}
		r.add(g, "external", Fail, "not reachable from the internet: "+msg,
			"open the port in your cloud provider's firewall/security group, and check that your reverse proxy forwards to GoDrop")
	}
}

// --------------------------------------------------------------- end to end

func (r *runner) checkEndToEnd(ctx context.Context) {
	const g = "end_to_end"
	target, token := r.targetURL(), r.Token
	if target == "" || token == "" {
		r.add(g, "round_trip", Skip, "needs a URL and a token (--url/--token, or run on the server)", "")
		return
	}

	payload := []byte("godrop doctor round trip\n")
	body, contentType := multipartBody("doctor.txt", payload)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(target, "/")+"/upload", body)
	if err != nil {
		r.add(g, "round_trip", Fail, err.Error(), "")
		return
	}
	req.Header.Set("Content-Type", contentType)
	req.Header.Set("Authorization", "Bearer "+token)

	resp, err := r.http.Do(req)
	if err != nil {
		r.add(g, "upload", Fail, err.Error(), "is the service running and reachable at "+target+"?")
		return
	}
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	_ = resp.Body.Close()
	if resp.StatusCode == http.StatusRequestEntityTooLarge && !strings.Contains(string(data), `"error"`) {
		r.add(g, "proxy_body_limit", Fail,
			"a proxy rejected a tiny upload with 413 before it reached GoDrop",
			"raise the proxy limit — nginx: client_max_body_size 100m;  Caddy: request_body { max_size 100MB }")
		return
	}
	if resp.StatusCode != http.StatusCreated {
		r.add(g, "upload", Fail, fmt.Sprintf("HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(data))),
			"check the token and the server logs")
		return
	}
	var uploaded struct {
		URL   string `json:"url"`
		Files []struct {
			URL  string `json:"url"`
			Size int64  `json:"size"`
		} `json:"files"`
	}
	if err := json.Unmarshal(data, &uploaded); err != nil || uploaded.URL == "" {
		r.add(g, "upload", Fail, "unexpected response body: "+strings.TrimSpace(string(data)), "")
		return
	}
	// The URL to fetch and then delete comes from the server being diagnosed,
	// and the delete carries the API token. A server that answered with an
	// address somewhere else would turn this check into a way to reach into
	// the operator's network and hand the token to a third party.
	if !sameOrigin(target, uploaded.URL) {
		r.add(g, "upload", Fail,
			fmt.Sprintf("the server answered with a URL somewhere else entirely: %s", uploaded.URL),
			"check GODROP_BASE_URL on the server, and whether anything is rewriting responses in between")
		return
	}
	r.add(g, "upload", Pass, "201 created", "")

	downloaded, status, err := r.get(ctx, uploaded.URL)
	switch {
	case err != nil:
		r.add(g, "download", Fail, err.Error(), "")
	case status != http.StatusOK:
		r.add(g, "download", Fail, fmt.Sprintf("HTTP %d", status), "")
	case string(downloaded) != string(payload):
		r.add(g, "download", Fail, "downloaded bytes differ from what was uploaded",
			"a proxy may be rewriting responses; check compression and buffering settings")
	default:
		r.add(g, "download", Pass, "bytes match, no authentication required", "")
	}

	delReq, err := http.NewRequestWithContext(ctx, http.MethodDelete, uploaded.URL, nil)
	if err == nil {
		delReq.Header.Set("Authorization", "Bearer "+token)
		if delResp, err := r.http.Do(delReq); err == nil {
			_, _ = io.Copy(io.Discard, io.LimitReader(delResp.Body, 4<<10))
			_ = delResp.Body.Close()
			if delResp.StatusCode == http.StatusNoContent {
				r.add(g, "delete", Pass, "204 no content", "")
			} else {
				r.add(g, "delete", Fail, fmt.Sprintf("HTTP %d", delResp.StatusCode), "")
			}
		} else {
			r.add(g, "delete", Fail, err.Error(), "")
		}
	}
}

// sameOrigin reports whether returned points at the same scheme, host and port
// as base. Anything the diagnosed server hands back is followed and, for the
// delete, followed with the API token attached, so it must not be able to send
// that anywhere but itself.
func sameOrigin(base, returned string) bool {
	a, aOK := origin(base)
	b, bOK := origin(returned)
	return aOK && bOK && a == b
}

// origin normalises a URL down to scheme, host and port, with the port left
// out when it is the default for the scheme, so that https://example.com and
// https://example.com:443 compare equal.
func origin(raw string) (string, bool) {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return "", false
	}
	scheme, host, port := strings.ToLower(u.Scheme), strings.ToLower(u.Hostname()), u.Port()
	if port == "" || (scheme == "https" && port == "443") || (scheme == "http" && port == "80") {
		return scheme + "://" + host, true
	}
	return scheme + "://" + host + ":" + port, true
}

func (r *runner) get(ctx context.Context, rawURL string) ([]byte, int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, 0, err
	}
	resp, err := r.http.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	return data, resp.StatusCode, err
}

// ------------------------------------------------------------------ version

func (r *runner) checkVersion(ctx context.Context) {
	if r.Offline {
		r.add("version", "update", Skip, "offline mode", "")
		return
	}
	latest, err := LatestRelease(ctx, r.http)
	if err != nil {
		r.add("version", "update", Skip, "could not check for updates: "+err.Error(), "")
		return
	}
	current := strings.TrimPrefix(r.Version, "v")
	if latest != "" && strings.TrimPrefix(latest, "v") != current && current != "dev" {
		r.add("version", "update", Warn, fmt.Sprintf("%s is available (running %s)", latest, r.Version),
			"curl -fsSL https://godrop.sh/install.sh | sh")
		return
	}
	r.add("version", "update", Pass, "running the latest version ("+r.Version+")", "")
}

// LatestRelease returns the newest published tag, or "" when unknown.
func LatestRelease(ctx context.Context, client *http.Client) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		"https://api.github.com/repos/fatihbaltaci/GoDrop/releases/latest", nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("github returned %s", resp.Status)
	}
	var out struct {
		TagName string `json:"tag_name"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&out); err != nil {
		return "", err
	}
	return out.TagName, nil
}
