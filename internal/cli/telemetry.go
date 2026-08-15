package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/spf13/cobra"

	"github.com/fatihbaltaci/GoDrop/internal/telemetry"
)

func newTelemetryCmd(build Build) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "telemetry",
		Short: "Inspect or change the anonymous heartbeat",
		Long: `GoDrop sends one anonymous heartbeat per day:

    {install_id, version, os, arch, deploy}

That is the whole payload. No file names, no identifiers, no counters, no
addresses, no base URL. Use ` + "`godrop telemetry status --json`" + ` to see the exact
body that would be transmitted.`,
	}
	cmd.PersistentFlags().String("data-dir", "", "data directory (default $GODROP_DATA_DIR or ./data)")
	cmd.AddCommand(
		newTelemetrySetCmd("on", false, "Enable the anonymous heartbeat"),
		newTelemetrySetCmd("off", true, "Disable the anonymous heartbeat"),
		newTelemetryStatusCmd(build),
	)
	return cmd
}

func newTelemetrySetCmd(name string, disable bool, short string) *cobra.Command {
	return &cobra.Command{
		Use:   name,
		Short: short,
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			dir := dataDir(cmd)
			if err := telemetry.SetDisabled(dir, disable); err != nil {
				return err
			}
			out := newOutput(cmd)
			if out.json {
				return out.emit(map[string]any{"telemetry": name})
			}
			if disable {
				out.success("telemetry off — nothing will be sent from this installation")
			} else {
				out.success("telemetry on — one anonymous heartbeat per day")
			}
			return nil
		},
	}
}

func newTelemetryStatusCmd(build Build) *cobra.Command {
	var send bool
	cmd := &cobra.Command{
		Use:   "status",
		Short: "Show whether telemetry is active and what would be sent",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			dir := dataDir(cmd)
			out := newOutput(cmd)
			optedOut := telemetry.Disabled(dir)
			compiled := build.PostHogKey != ""

			client, err := telemetry.New(telemetry.Options{
				Key:     build.PostHogKey,
				Host:    build.PostHogHost,
				Version: build.Version,
				DataDir: dir,
			})
			if err != nil {
				return err
			}

			state := "on"
			reason := ""
			switch {
			case !compiled:
				state, reason = "off", "this binary was built without a telemetry key (built from source)"
			case optedOut:
				state, reason = "off", "disabled with `godrop telemetry off`"
			}

			var payload any
			if client != nil {
				payload = client.Payload()
			}
			if out.json {
				return out.emit(map[string]any{
					"state":    state,
					"reason":   reason,
					"interval": telemetry.Interval.String(),
					"payload":  payload,
				})
			}

			out.heading("Telemetry: " + state)
			if reason != "" {
				out.skip("%s", reason)
			}
			if payload == nil {
				return nil
			}
			body, _ := json.MarshalIndent(payload, "  ", "  ")
			out.printf("\n  Sent once every %s, to %s:\n\n  %s\n", telemetry.Interval, build.PostHogHost, body)
			if state == "on" {
				out.printf("\n  Turn it off with: godrop telemetry off\n")
			}
			if send {
				ctx, cancel := context.WithTimeout(cmd.Context(), 10*time.Second)
				defer cancel()
				if err := client.Send(ctx); err != nil {
					return fmt.Errorf("send failed: %w", err)
				}
				out.success("test heartbeat delivered")
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&send, "send", false, "send one heartbeat now (for testing)")
	return cmd
}
