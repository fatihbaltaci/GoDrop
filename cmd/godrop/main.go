// Command godrop is a tiny self-hosted file host: upload a file, get a
// hard-to-guess public URL.
package main

import (
	"context"
	"os"

	"github.com/fatihbaltaci/GoDrop/internal/cli"
)

// Build information, injected at release time with -ldflags. posthogKey is
// deliberately empty by default: a binary built from source never reports
// telemetry, only the official releases do.
var (
	version     = "dev"
	commit      = "none"
	date        = "unknown"
	posthogKey  = ""
	posthogHost = "https://eu.i.posthog.com"
)

func main() {
	os.Exit(cli.Execute(context.Background(), cli.Build{
		Version:     version,
		Commit:      commit,
		Date:        date,
		PostHogKey:  posthogKey,
		PostHogHost: posthogHost,
	}, os.Args[1:]))
}
