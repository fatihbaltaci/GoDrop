// Package netcheck answers the question every self-hosted install eventually
// runs into: "it works on the box, but can anyone actually reach it?"
//
// A server cannot answer that on its own. Requesting your own public address
// from inside the network may succeed through NAT hairpinning while the outside
// world sees nothing, and a cloud provider's security group is invisible from
// within the instance. So the definitive check is delegated to godrop.sh, which
// fetches the URL from the public internet and reports what it saw.
package netcheck

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"os/exec"
	"strings"
	"time"
)

// DefaultCheckURL is the hosted reachability probe.
const DefaultCheckURL = "https://godrop.sh/api/check"

// publicIPURL returns the caller's address as seen from the internet.
const publicIPURL = "https://cloudflare.com/cdn-cgi/trace"

// PublicIP reports the public address of this machine.
func PublicIP(ctx context.Context, client *http.Client) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, publicIPURL, nil)
	if err != nil {
		return "", err
	}
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
	if err != nil {
		return "", err
	}
	for line := range strings.SplitSeq(string(body), "\n") {
		if ip, ok := strings.CutPrefix(line, "ip="); ok {
			return strings.TrimSpace(ip), nil
		}
	}
	return "", errors.New("could not determine public IP")
}

// Resolve looks up the addresses a hostname points at.
func Resolve(ctx context.Context, resolver *net.Resolver, host string) ([]string, error) {
	if resolver == nil {
		resolver = net.DefaultResolver
	}
	addrs, err := resolver.LookupHost(ctx, host)
	if err != nil {
		return nil, err
	}
	return addrs, nil
}

// TLSInfo describes the certificate served by a host.
type TLSInfo struct {
	Valid     bool      `json:"valid"`
	NotAfter  time.Time `json:"not_after"`
	DaysLeft  int       `json:"days_left"`
	Issuer    string    `json:"issuer"`
	Error     string    `json:"error,omitempty"`
	Attempted bool      `json:"attempted"`
}

// CheckTLS dials host:port and inspects the presented certificate.
//
// The handshake deliberately skips verification and validates afterwards, so
// that an untrusted or expired certificate can still be described ("expires in
// 3 days", "self-signed") instead of collapsing into one opaque error. roots is
// nil in production, meaning the system trust store.
func CheckTLS(ctx context.Context, hostport string, now time.Time, roots *x509.CertPool) TLSInfo {
	info := TLSInfo{Attempted: true}
	host, _, err := net.SplitHostPort(hostport)
	if err != nil {
		info.Error = err.Error()
		return info
	}
	dialer := &tls.Dialer{
		NetDialer: &net.Dialer{Timeout: 5 * time.Second},
		Config:    &tls.Config{ServerName: host, InsecureSkipVerify: true}, //nolint:gosec // verified below
	}
	raw, err := dialer.DialContext(ctx, "tcp", hostport)
	if err != nil {
		info.Error = err.Error()
		return info
	}
	conn := raw.(*tls.Conn)
	defer conn.Close()
	return inspectCerts(conn.ConnectionState().PeerCertificates, host, now, roots)
}

// inspectCerts turns a presented certificate chain into a report. It is
// separate from the dialling so the interesting cases (a full chain, an
// expired leaf, a server that presents nothing) can be examined directly.
func inspectCerts(certs []*x509.Certificate, host string, now time.Time, roots *x509.CertPool) TLSInfo {
	info := TLSInfo{Attempted: true}
	if len(certs) == 0 {
		info.Error = "no certificate presented"
		return info
	}
	leaf := certs[0]
	info.NotAfter = leaf.NotAfter
	info.DaysLeft = int(leaf.NotAfter.Sub(now).Hours() / 24)
	info.Issuer = leaf.Issuer.CommonName

	intermediates := x509.NewCertPool()
	for _, c := range certs[1:] {
		intermediates.AddCert(c)
	}
	if _, err := leaf.Verify(x509.VerifyOptions{
		DNSName:       host,
		Roots:         roots,
		Intermediates: intermediates,
		CurrentTime:   now,
	}); err != nil {
		info.Error = err.Error()
		return info
	}
	info.Valid = true
	return info
}

// ExternalResult is the verdict from the hosted probe.
type ExternalResult struct {
	OK       bool   `json:"ok"`
	Status   int    `json:"status"`
	Location string `json:"location,omitempty"`
	Error    string `json:"error,omitempty"`
	Duration int    `json:"duration_ms,omitempty"`
}

// External asks the hosted probe to fetch target from the public internet.
// Only the URL is transmitted.
func External(ctx context.Context, client *http.Client, checkURL, target string) (ExternalResult, error) {
	if checkURL == "" {
		checkURL = DefaultCheckURL
	}
	// A map of strings always marshals cleanly.
	body, _ := json.Marshal(map[string]string{"url": target})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, checkURL, bytes.NewReader(body))
	if err != nil {
		return ExternalResult{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return ExternalResult{}, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if err != nil {
		return ExternalResult{}, err
	}
	if resp.StatusCode >= 300 {
		return ExternalResult{}, fmt.Errorf("reachability service returned %s", resp.Status)
	}
	var out ExternalResult
	if err := json.Unmarshal(data, &out); err != nil {
		return ExternalResult{}, fmt.Errorf("reachability service returned an unexpected body: %w", err)
	}
	return out, nil
}

// Firewall summarises the host firewall, when one can be inspected.
type Firewall struct {
	Tool      string // ufw, firewalld, nftables, iptables or ""
	Active    bool
	PortOpen  bool
	Detail    string
	Inspected bool
	Hint      string
}

// Runner executes a command and returns its combined output. It is a variable
// so tests can drive every branch without a firewall being installed.
type Runner func(ctx context.Context, name string, args ...string) (string, error)

// ExecRunner runs commands for real.
func ExecRunner(ctx context.Context, name string, args ...string) (string, error) {
	if _, err := exec.LookPath(name); err != nil {
		return "", err
	}
	// The command and its arguments are literals from CheckFirewall, and no
	// shell interprets them.
	out, err := exec.CommandContext(ctx, name, args...).CombinedOutput() //nolint:gosec // G204
	return string(out), err
}

// CheckFirewall inspects the host firewall for a rule covering port.
func CheckFirewall(ctx context.Context, run Runner, port int) Firewall {
	if run == nil {
		run = ExecRunner
	}
	portStr := fmt.Sprint(port)

	if out, err := run(ctx, "ufw", "status"); err == nil {
		fw := Firewall{Tool: "ufw", Inspected: true, Detail: firstLine(out)}
		fw.Active = strings.Contains(strings.ToLower(out), "status: active")
		fw.PortOpen = !fw.Active || strings.Contains(out, portStr)
		if fw.Active && !fw.PortOpen {
			fw.Hint = fmt.Sprintf("sudo ufw allow %d/tcp", port)
		}
		return fw
	}
	if out, err := run(ctx, "firewall-cmd", "--list-all"); err == nil {
		fw := Firewall{Tool: "firewalld", Inspected: true, Active: true, Detail: firstLine(out)}
		fw.PortOpen = strings.Contains(out, portStr+"/tcp") ||
			strings.Contains(out, "http") && (port == 80 || port == 443)
		if !fw.PortOpen {
			fw.Hint = fmt.Sprintf("sudo firewall-cmd --permanent --add-port=%d/tcp && sudo firewall-cmd --reload", port)
		}
		return fw
	}
	if out, err := run(ctx, "nft", "list", "ruleset"); err == nil {
		fw := Firewall{Tool: "nftables", Inspected: true, Detail: firstLine(out)}
		fw.Active = strings.TrimSpace(out) != ""
		fw.PortOpen = !fw.Active || strings.Contains(out, portStr)
		if fw.Active && !fw.PortOpen {
			fw.Hint = fmt.Sprintf("sudo nft add rule inet filter input tcp dport %d accept", port)
		}
		return fw
	}
	return Firewall{Hint: "could not inspect a host firewall; check your cloud provider's security group as well"}
}

// Scope says where a host can be reached from, which is what decides whether
// plain http is a problem or a perfectly reasonable choice.
type Scope int

const (
	// Public is reachable from the internet, so anything sent in clear text is
	// readable by every network in between.
	Public Scope = iota
	// Private is a LAN, a container network or a VPN subnet.
	Private
	// Loopback never leaves the machine.
	Loopback
	// Encrypted is a network that encrypts the connection itself, so plain
	// http on top of it is still private. Tailscale is the common case.
	Encrypted
)

// tailscaleCGNAT is the range Tailscale assigns to nodes (100.64.0.0/10).
var tailscaleCGNAT = netip.MustParsePrefix("100.64.0.0/10")

// HostScope classifies a host name or address. Names are matched by suffix,
// which is all that can be done without resolving them, and resolving is not
// this function's job: it answers "what did the operator configure", not "what
// does it point at today".
func HostScope(host string) Scope {
	host = strings.ToLower(strings.Trim(host, "[]"))
	if host == "" {
		return Public
	}
	if addr, err := netip.ParseAddr(host); err == nil {
		switch {
		case addr.IsLoopback():
			return Loopback
		case tailscaleCGNAT.Contains(addr):
			return Encrypted
		case addr.IsPrivate(), addr.IsLinkLocalUnicast(), addr.IsLinkLocalMulticast():
			return Private
		}
		return Public
	}
	switch {
	case host == "localhost", strings.HasSuffix(host, ".localhost"):
		return Loopback
	case strings.HasSuffix(host, ".ts.net"):
		return Encrypted
	}
	for _, suffix := range []string{".local", ".internal", ".lan", ".home.arpa", ".home", ".localdomain"} {
		if strings.HasSuffix(host, suffix) {
			return Private
		}
	}
	return Public
}

// Listening reports whether something accepts TCP connections on addr.
func Listening(ctx context.Context, addr string) bool {
	d := net.Dialer{Timeout: 2 * time.Second}
	conn, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		return false
	}
	_ = conn.Close()
	return true
}

// HostPort splits a base URL into a dialable host:port, defaulting the port
// from the scheme.
func HostPort(rawURL string) (string, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return "", err
	}
	if u.Host == "" {
		return "", fmt.Errorf("invalid URL %q", rawURL)
	}
	if u.Port() != "" {
		return u.Host, nil
	}
	if u.Scheme == "https" {
		return u.Hostname() + ":443", nil
	}
	return u.Hostname() + ":80", nil
}

func firstLine(s string) string {
	line, _, _ := strings.Cut(strings.TrimSpace(s), "\n")
	return strings.TrimSpace(line)
}
