package cli

import (
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/fatihbaltaci/GoDrop/internal/config"
	"github.com/fatihbaltaci/GoDrop/internal/tokens"
	"github.com/fatihbaltaci/GoDrop/internal/wizard"
)

func newTokenCmd(_ Build) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "token",
		Short: "Create, list and revoke API tokens",
		Long: `Manage the API tokens that authorise uploads and deletes.

Tokens are stored as SHA-256 digests, so the clear-text value is shown exactly
once, when it is created. A running server notices changes within a
second, with no restart needed.`,
	}
	cmd.PersistentFlags().String("data-dir", "", "data directory (default $GODROP_DATA_DIR or ./data)")
	cmd.AddCommand(newTokenCreateCmd(), newTokenListCmd(), newTokenRevokeCmd())
	return cmd
}

// localAddress is where this instance answers, from the same environment the
// server reads: the base URL if it has one, and the listening port otherwise.
func localAddress() string {
	if base := strings.TrimRight(os.Getenv("GODROP_BASE_URL"), "/"); base != "" {
		return base
	}
	addr := strings.TrimSpace(os.Getenv("GODROP_ADDR"))
	if addr == "" {
		addr = config.DefaultAddr
	}
	if _, port, err := net.SplitHostPort(addr); err == nil && port != "" {
		return "http://localhost:" + port
	}
	return "http://localhost" + addr
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

// tokenSource is where this machine's tokens are, which is not always where
// the shell is standing. Setup writes a .env that a prompt has never sourced,
// and a compose deployment keeps the token file in a volume that only the
// container can reach.
type tokenSource struct {
	dir string   // the data directory holding tokens.json
	env []string // tokens from GODROP_TOKENS, or from the generated .env
	// inDocker is the prefix of the command that reaches the token file this
	// machine cannot, and empty when the file is right here.
	inDocker string
	// envFile is where the environment tokens were read from, for saying so.
	envFile string
}

// resolveTokens works out which of those applies.
func resolveTokens(cmd *cobra.Command) tokenSource {
	src := tokenSource{dir: dataDir(cmd), env: config.ParseTokens(os.Getenv("GODROP_TOKENS"))}
	flagged, _ := cmd.Flags().GetString("data-dir")
	if flagged != "" || os.Getenv("GODROP_DATA_DIR") != "" || len(src.env) > 0 {
		// The operator has said where to look, one way or another.
		return src
	}
	dir := installationDir()
	if !installedAt(dir) {
		return src
	}
	values := readEnvFile(filepath.Join(dir, ".env"))
	src.env = config.ParseTokens(values["GODROP_TOKENS"])
	src.envFile = filepath.Join(dir, ".env")
	if deploymentAt(dir) == wizard.DeployCompose {
		// GODROP_DATA_DIR in that file is a path inside the container, so the
		// token file is out of reach from here and saying which command
		// reaches it beats writing a token the service will never read.
		src.inDocker = "docker compose --project-directory " + dir + " run --rm godrop token "
		return src
	}
	if v := strings.TrimSpace(values["GODROP_DATA_DIR"]); v != "" {
		src.dir = v
	}
	return src
}

func (s tokenSource) store() (*tokens.Store, error) {
	return tokens.New(tokens.Path(s.dir), s.env)
}

func newTokenCreateCmd() *cobra.Command {
	var name string
	cmd := &cobra.Command{
		Use:   "create",
		Short: "Create a new API token",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			src := resolveTokens(cmd)
			if src.inDocker != "" {
				return fmt.Errorf(`this installation keeps its token file inside the container, where the service reads it:

  %screate --name %s

A token written here would go to a file nothing is running against`, src.inDocker, tokenName(name))
			}
			store, err := src.store()
			if err != nil {
				return err
			}
			dir := src.dir
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
			out.success("name: %s (usable immediately, no restart needed)", tok.Name)
			out.warn("this is the only time the token is shown; store it now")
			out.printf("\n  Try it:\n")
			out.command(fmt.Sprintf(`curl -X POST -H "Authorization: Bearer %s" \`, plain))
			// The address this instance actually answers on: a printed
			// example with somebody else's port in it is not an example.
			out.command(fmt.Sprintf(`  -F "file=@photo.jpg" %s/upload`, localAddress()))
			return nil
		},
	}
	cmd.Flags().StringVar(&name, "name", "", "label for this token (e.g. claude-code, ci, blog)")
	return cmd
}

// tokenName is the name to show in an error before the flag has been read.
func tokenName(name string) string {
	if name == "" {
		return "default"
	}
	return name
}

func newTokenListCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List tokens (names only, values are not recoverable)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			src := resolveTokens(cmd)
			store, err := src.store()
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
			if len(list) > 0 {
				out.printf("\n  %-24s %-22s %s\n", "NAME", "CREATED", "LAST USED")
				for _, t := range list {
					last := "never"
					if t.LastUsed != nil {
						last = humanSince(*t.LastUsed)
					}
					out.printf("  %-24s %-22s %s\n", t.Name, humanSince(t.Created), last)
				}
			}
			// The token setup hands over lives in GODROP_TOKENS, because a
			// compose deployment has no data directory on this machine to
			// write a file into until the container has created the volume.
			// It has no name here, which is why it is not a row above.
			if n := store.EnvCount(); n > 0 {
				out.printf("\n")
				where := "your environment"
				if src.envFile != "" {
					where = src.envFile
				}
				out.skip("%d token(s) come from GODROP_TOKENS in %s, including the one setup gave you", n, where)
				out.skip("they have no name here; edit that file to change them, and restart the service")
			}
			if src.inDocker != "" {
				out.printf("\n")
				out.skip("named tokens live in the container:")
				out.command(src.inDocker + "list")
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
			src := resolveTokens(cmd)
			if src.inDocker != "" {
				return fmt.Errorf(`this installation keeps its token file inside the container:

  %srevoke %s`, src.inDocker, args[0])
			}
			store, err := src.store()
			if err != nil {
				return err
			}
			if err := store.Revoke(args[0]); err != nil {
				if store.EnvCount() > 0 {
					return fmt.Errorf("%w. Tokens set through GODROP_TOKENS are removed by editing your environment", err)
				}
				return err
			}
			out := newOutput(cmd)
			if out.json {
				return out.emit(map[string]any{"revoked": args[0]})
			}
			out.success("%s revoked, effective immediately", args[0])
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
