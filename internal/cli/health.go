package cli

import (
	"crypto/tls"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/fatihbaltaci/GoDrop/internal/config"
)

// newHealthCmd exists so the container image can declare a HEALTHCHECK without
// shipping curl. A distroless image has no shell and no tools; the binary
// checking itself is the whole point.
func newHealthCmd() *cobra.Command {
	var addr string
	cmd := &cobra.Command{
		Use:   "health",
		Short: "Probe a running instance (used by the container HEALTHCHECK)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			target, insecure := addr, false
			if target == "" {
				target, insecure = localHealthURL()
			}
			client := &http.Client{Timeout: 5 * time.Second}
			if insecure {
				// The certificate is issued for the public name and this
				// probe connects to the loopback address, so it can never
				// match. Nothing is trusted here: it is a liveness check on
				// a connection that does not leave the machine.
				client.Transport = &http.Transport{
					TLSClientConfig: &tls.Config{InsecureSkipVerify: true}, //nolint:gosec // loopback liveness probe
				}
			}
			resp, err := client.Get(target)
			if err != nil {
				return err
			}
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusOK {
				return fmt.Errorf("%s returned %s", target, resp.Status)
			}
			out := newOutput(cmd)
			if out.json {
				return out.emit(map[string]any{"status": "ok", "url": target})
			}
			out.success("%s is healthy", target)
			return nil
		},
	}
	cmd.Flags().StringVar(&addr, "url", "", "URL to probe (default: the local listen address)")
	return cmd
}

// localHealthURL derives the probe from the environment the same way the
// server derives its listen address, because TLS moves both the scheme and
// the port and a health check that misses that reports a healthy server as
// dead for ever.
func localHealthURL() (target string, insecure bool) {
	mode, _ := config.ParseTLSMode(os.Getenv("GODROP_TLS"),
		os.Getenv("GODROP_TLS_CERT") != "" || os.Getenv("GODROP_TLS_KEY") != "")
	scheme, fallback := "http", config.DefaultAddr
	if mode != config.TLSOff {
		scheme, fallback = "https", config.DefaultTLSAddr
	}

	addr := strings.TrimSpace(os.Getenv("GODROP_ADDR"))
	if addr == "" {
		addr = fallback
	}
	host, port, found := strings.Cut(addr, ":")
	if !found {
		return scheme + "://127.0.0.1:" + addr + "/healthz", scheme == "https"
	}
	if host == "" || host == "0.0.0.0" || host == "::" {
		host = "127.0.0.1"
	}
	return scheme + "://" + host + ":" + port + "/healthz", scheme == "https"
}
