package cli

import (
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
			target := addr
			if target == "" {
				target = localHealthURL()
			}
			client := &http.Client{Timeout: 5 * time.Second}
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

func localHealthURL() string {
	addr := strings.TrimSpace(os.Getenv("GODROP_ADDR"))
	if addr == "" {
		addr = config.DefaultAddr
	}
	host, port, found := strings.Cut(addr, ":")
	if !found {
		return "http://127.0.0.1:" + addr + "/healthz"
	}
	if host == "" || host == "0.0.0.0" || host == "::" {
		host = "127.0.0.1"
	}
	return "http://" + host + ":" + port + "/healthz"
}
