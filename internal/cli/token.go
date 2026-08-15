package cli

import (
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/fatihbaltaci/GoDrop/internal/config"
	"github.com/fatihbaltaci/GoDrop/internal/tokens"
)

func newTokenCmd(_ Build) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "token",
		Short: "Create, list and revoke API tokens",
		Long: `Manage the API tokens that authorise uploads and deletes.

Tokens are stored as SHA-256 digests, so the clear-text value is shown exactly
once, when it is created. A running server notices changes within a second —
no restart is needed.`,
	}
	cmd.PersistentFlags().String("data-dir", "", "data directory (default $GODROP_DATA_DIR or ./data)")
	cmd.AddCommand(newTokenCreateCmd(), newTokenListCmd(), newTokenRevokeCmd())
	return cmd
}

// dataDir resolves where the token file lives, honouring the flag first and the
// environment second.
func dataDir(cmd *cobra.Command) string {
	if v, _ := cmd.Flags().GetString("data-dir"); v != "" {
		return v
	}
	if v := strings.TrimSpace(os.Getenv("GODROP_DATA_DIR")); v != "" {
		return v
	}
	return config.DefaultDataDir
}

func openTokenStore(cmd *cobra.Command) (*tokens.Store, string, error) {
	dir := dataDir(cmd)
	store, err := tokens.New(tokens.Path(dir), config.ParseTokens(os.Getenv("GODROP_TOKENS")))
	if err != nil {
		return nil, "", err
	}
	return store, dir, nil
}

func newTokenCreateCmd() *cobra.Command {
	var name string
	cmd := &cobra.Command{
		Use:   "create",
		Short: "Create a new API token",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			store, dir, err := openTokenStore(cmd)
			if err != nil {
				return err
			}
			plain, tok, err := store.Create(name)
			if err != nil {
				return err
			}
			out := newOutput(cmd)
			if out.json {
				return out.emit(map[string]any{
					"token":   plain,
					"name":    tok.Name,
					"created": tok.Created.Format(time.RFC3339),
					"file":    tokens.Path(dir),
				})
			}
			out.heading("Token created")
			out.printf("\n")
			out.box(plain)
			out.printf("\n")
			out.success("name: %s — usable immediately, no restart needed", tok.Name)
			out.warn("this is the only time the token is shown; store it now")
			out.printf("\n  Try it:\n")
			out.command(fmt.Sprintf(`curl -X POST -H "Authorization: Bearer %s" \`, plain))
			out.command(`  -F "file=@photo.jpg" http://localhost:8080/upload`)
			return nil
		},
	}
	cmd.Flags().StringVar(&name, "name", "", "label for this token (e.g. claude-code, ci, blog)")
	return cmd
}

func newTokenListCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List tokens (names only — values are not recoverable)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			store, _, err := openTokenStore(cmd)
			if err != nil {
				return err
			}
			list := store.List()
			out := newOutput(cmd)
			if out.json {
				items := make([]map[string]any, 0, len(list))
				for _, t := range list {
					item := map[string]any{"name": t.Name, "created": t.Created.Format(time.RFC3339)}
					if t.LastUsed != nil {
						item["last_used"] = t.LastUsed.Format(time.RFC3339)
					}
					items = append(items, item)
				}
				return out.emit(map[string]any{"tokens": items, "env_tokens": store.EnvCount()})
			}
			if len(list) == 0 && store.EnvCount() == 0 {
				out.printf("  No tokens yet. Create one with: godrop token create --name default\n")
				return nil
			}
			out.printf("\n  %-24s %-22s %s\n", "NAME", "CREATED", "LAST USED")
			for _, t := range list {
				last := "never"
				if t.LastUsed != nil {
					last = humanSince(*t.LastUsed)
				}
				out.printf("  %-24s %-22s %s\n", t.Name, humanSince(t.Created), last)
			}
			if n := store.EnvCount(); n > 0 {
				out.printf("\n")
				out.skip("%d token(s) come from GODROP_TOKENS and are managed in your environment", n)
			}
			return nil
		},
	}
}

func newTokenRevokeCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "revoke <name>",
		Short: "Revoke a token by name",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			store, _, err := openTokenStore(cmd)
			if err != nil {
				return err
			}
			if err := store.Revoke(args[0]); err != nil {
				if store.EnvCount() > 0 {
					return fmt.Errorf("%w — tokens set through GODROP_TOKENS are removed by editing your environment", err)
				}
				return err
			}
			out := newOutput(cmd)
			if out.json {
				return out.emit(map[string]any{"revoked": args[0]})
			}
			out.success("%s revoked — effective immediately", args[0])
			return nil
		},
	}
}

func humanSince(t time.Time) string {
	d := time.Since(t)
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%d min ago", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%d hours ago", int(d.Hours()))
	case d < 30*24*time.Hour:
		return fmt.Sprintf("%d days ago", int(d.Hours()/24))
	default:
		return t.Format("2006-01-02")
	}
}
