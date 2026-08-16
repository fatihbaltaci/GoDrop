package cli

import (
	"bytes"
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
			src := locate(cmd)
			out := newOutput(cmd)
			// The marker lives next to the uploads, so on a compose
			// installation it is inside the volume: writing one here would
			// switch off a heartbeat nothing is sending.
			if src.containerised() {
				if _, err := src.run(cmd.Context(), "telemetry", name, "--json"); err != nil {
					return err
				}
			} else if err := telemetry.SetDisabled(src.dir, disable); err != nil {
				return err
			}
			if out.json {
				return out.emit(map[string]any{"telemetry": name})
			}
			if disable {
				out.success("telemetry off, nothing will be sent from this installation")
			} else {
				out.success("telemetry on, one anonymous heartbeat per day")
			}
			return nil
		},
	}
}

// printTelemetryStatus renders what the container answered, in the shape a
// local installation prints.
func printTelemetryStatus(out *output, raw []byte) error {
	var status struct {
		State    string          `json:"state"`
		Reason   string          `json:"reason"`
		Interval string          `json:"interval"`
		Payload  json.RawMessage `json:"payload"`
	}
	if err := json.Unmarshal(raw, &status); err != nil {
		return fmt.Errorf("could not read the answer from the container: %w", err)
	}
	out.heading("Telemetry: " + status.State)
	if status.Reason != "" {
		out.skip("%s", status.Reason)
	}
	if len(status.Payload) == 0 || string(status.Payload) == "null" {
		return nil
	}
	// The payload arrived inside a document that has already parsed, so
	// laying it out again cannot fail.
	var pretty bytes.Buffer
	_ = json.Indent(&pretty, status.Payload, "  ", "  ")
	out.printf("\n  Sent once every %s:\n\n  %s\n", status.Interval, pretty.String())
	if status.State == "on" {
		out.printf("\n  Turn it off with: godrop telemetry off\n")
	}
	return nil
}

func newTelemetryStatusCmd(build Build) *cobra.Command {
	var send bool
	cmd := &cobra.Command{
		Use:   "status",
		Short: "Show whether telemetry is active and what would be sent",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			src := locate(cmd)
			out := newOutput(cmd)
			// The container is the one that would do the sending, so it is the
			// one that knows whether it is going to.
			if src.containerised() {
				raw, err := src.run(cmd.Context(), "telemetry", "status", "--json")
				if err != nil {
					return err
				}
				if out.json {
					_, err := out.w.Write(raw)
					return err
				}
				return printTelemetryStatus(out, raw)
			}
			dir := src.dir
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
