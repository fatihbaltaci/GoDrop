// Package tlsconf turns the TLS settings into something http.Server can use.
//
// GoDrop can serve https itself, which is the difference between "install a
// reverse proxy, learn its configuration language, arrange for certificate
// renewal" and answering one question during setup. A certificate you already
// have works just as well: the two paths differ only in where the certificate
// comes from.
package tlsconf

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/crypto/acme"
	"golang.org/x/crypto/acme/autocert"

	"github.com/fatihbaltaci/GoDrop/internal/config"
)

// Server describes how to serve, whatever the certificate's origin.
type Server struct {
	// TLSConfig is nil when TLS is off.
	TLSConfig *tls.Config
	// CertFile and KeyFile are empty unless the certificate came from disk;
	// http.ListenAndServeTLS takes them straight.
	CertFile, KeyFile string
	// Challenge answers the ACME http-01 challenge and redirects everything
	// else to https. It is nil when there is nothing to serve on port 80.
	Challenge http.Handler
	// Describe says, in one line, what a reader needs to know at startup.
	Describe string
}

// New prepares the TLS setup, or returns a Server with no TLSConfig when TLS
// is off. It touches the filesystem only to check that what it was given can
// actually be used: a certificate that cannot be read should stop the service
// now, with a clear reason, rather than at the first request.
func New(cfg *config.Config) (*Server, error) {
	switch cfg.TLS {
	case config.TLSOff:
		return &Server{Describe: "plain http"}, nil

	case config.TLSFile:
		cert, err := tls.LoadX509KeyPair(cfg.TLSCert, cfg.TLSKey)
		if err != nil {
			return nil, fmt.Errorf("read the certificate: %w", err)
		}
		return &Server{
			TLSConfig: &tls.Config{
				MinVersion:   tls.VersionTLS12,
				Certificates: []tls.Certificate{cert},
			},
			CertFile:  cfg.TLSCert,
			KeyFile:   cfg.TLSKey,
			Challenge: redirect(cfg.BaseURL, names(cfg, certNames(cert.Leaf))),
			Describe:  "TLS from " + cfg.TLSCert,
		}, nil

	case config.TLSAuto:
		// The cache holds the account key and the certificates. Without it,
		// every restart asks Let's Encrypt for a new certificate and runs
		// into their rate limits within a day.
		if err := os.MkdirAll(cfg.TLSCacheDir, 0o700); err != nil {
			return nil, fmt.Errorf("create the certificate cache: %w", err)
		}
		m := &autocert.Manager{
			Prompt:     autocert.AcceptTOS,
			Cache:      autocert.DirCache(cfg.TLSCacheDir),
			HostPolicy: autocert.HostWhitelist(cfg.TLSDomains...),
			Email:      cfg.TLSEmail,
		}
		conf := m.TLSConfig()
		conf.MinVersion = tls.VersionTLS12
		// acme-tls/1 is what lets a certificate be issued over port 443
		// alone, for anyone who cannot open port 80.
		conf.NextProtos = append(conf.NextProtos, acme.ALPNProto)
		return &Server{
			TLSConfig: conf,
			Challenge: m.HTTPHandler(redirect(cfg.BaseURL, names(cfg, cfg.TLSDomains))),
			Describe:  "automatic TLS for " + join(cfg.TLSDomains),
		}, nil
	}
	return nil, fmt.Errorf("unknown TLS mode %q", cfg.TLS)
}

// Enabled reports whether anything should be served over TLS.
func (s *Server) Enabled() bool { return s != nil && s.TLSConfig != nil }

// certNames is every name a certificate covers. Addresses count: a
// certificate from a company CA is often issued for one.
func certNames(leaf *x509.Certificate) []string {
	out := append([]string{}, leaf.DNSNames...)
	for _, ip := range leaf.IPAddresses {
		if v4 := ip.To4(); v4 == nil {
			// A Host header brackets an IPv6 literal, so the list must too.
			out = append(out, "["+ip.String()+"]")
			continue
		}
		out = append(out, ip.String())
	}
	return out
}

// names is the list of hosts this server answers for: the configured domains,
// or the ones in the certificate, plus whatever the public URL says.
func names(cfg *config.Config, from []string) []string {
	out := append([]string{}, from...)
	if h := hostOf(cfg.BaseURL); h != "" {
		out = append(out, h)
	}
	return out
}

// redirect sends every plain request to the https URL for the same thing.
//
// The public URL is used when there is one, because it is the only thing that
// knows the port the outside world connects on. Otherwise the host comes from
// the list this server has a certificate for, looked up by the request's own
// Host header. Taking that header at its word would let anyone hand out a
// link that leaves through GoDrop and arrives somewhere else entirely, so a
// name that is not ours is refused rather than redirected.
//
// A permanent redirect would be cached by browsers for a name that may later
// go back to plain http, so it is a temporary one.
func redirect(baseURL string, hosts []string) http.Handler {
	base := strings.TrimSuffix(baseURL, "/")
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		target := base
		if target == "" {
			name, ok := match(hosts, hostOnly(r.Host))
			if !ok {
				http.Error(w, "unknown host", http.StatusMisdirectedRequest)
				return
			}
			target = "https://" + name
		}
		// The host is one of ours, taken from the configuration rather than
		// from the request, so the path that follows cannot move the target
		// to another origin.
		http.Redirect(w, r, target+r.URL.RequestURI(), http.StatusFound) //nolint:gosec // G710
	})
}

// match finds a requested host in the list, returning the configured spelling
// of it rather than the one the request supplied.
func match(hosts []string, want string) (string, bool) {
	for _, h := range hosts {
		if strings.EqualFold(h, want) {
			return h, true
		}
	}
	return "", false
}

// hostOf returns the host of a base URL, without its port.
func hostOf(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	return u.Hostname()
}

// hostOnly drops any port from a Host header, so the redirect does not send a
// browser to https on port 80.
func hostOnly(host string) string {
	if i := lastColon(host); i >= 0 {
		return host[:i]
	}
	return host
}

// lastColon finds a port separator, ignoring the colons inside an IPv6
// literal, which is always bracketed when a port follows it.
func lastColon(host string) int {
	for i := len(host) - 1; i >= 0; i-- {
		switch host[i] {
		case ':':
			return i
		case ']':
			return -1
		}
	}
	return -1
}

func join(domains []string) string {
	switch len(domains) {
	case 0:
		return "no domain"
	case 1:
		return domains[0]
	default:
		return fmt.Sprintf("%s and %d more", domains[0], len(domains)-1)
	}
}

// CheckCache reports a certificate cache that cannot be used. It is separate
// from New so `godrop doctor` can say so without starting a server.
func CheckCache(dir string) error {
	info, err := os.Stat(dir)
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return errors.New(dir + " is not a directory")
	}
	probe := filepath.Join(dir, ".probe")
	if err := os.WriteFile(probe, nil, 0o600); err != nil {
		return err
	}
	return os.Remove(probe)
}
