package cli

import (
	"context"
	"encoding/json"
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

// installation is where this machine's GoDrop keeps its files, which is not
// always where the shell is standing. Setup writes a .env that a prompt has
// never sourced, and a compose deployment keeps the data directory in a volume
// that only the container can reach.
type installation struct {
	dir string   // the data directory holding tokens.json
	env []string // tokens from GODROP_TOKENS, or from the generated .env
	// project is the compose directory when the token file is in a volume,
	// and empty when the file is one this machine can open for itself.
	project string
	// envFile is where the environment tokens were read from, for saying so.
	envFile string
	// address is where that installation answers, for the example below a new
	// token. The shell's own environment says nothing about it.
	address string
}

// containerised reports that the token file is inside the service's container.
func (s installation) containerised() bool { return s.project != "" }

// locate works out which of those applies. An explicit --data-dir or
// GODROP_DATA_DIR is the operator saying where to look, and is left alone.
func locate(cmd *cobra.Command) installation {
	src := installation{dir: dataDir(cmd), env: config.ParseTokens(os.Getenv("GODROP_TOKENS"))}
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
	src.address = wizard.PublicAddress(answersFromEnv(dir))
	if deploymentAt(dir) == wizard.DeployCompose {
		// GODROP_DATA_DIR in that file is a path inside the container, so the
		// token file is not one this machine can open. The commands below run
		// there instead of writing a token the service will never read.
		src.project = dir
		return src
	}
	if v := strings.TrimSpace(values["GODROP_DATA_DIR"]); v != "" {
		src.dir = v
	}
	return src
}

func (s installation) store() (*tokens.Store, error) {
	return tokens.New(tokens.Path(s.dir), s.env)
}

// run carries out a token command where the token file actually is.
func (s installation) run(ctx context.Context, args ...string) ([]byte, error) {
	return composeRun(ctx, s.project, args...)
}

func newTokenCreateCmd() *cobra.Command {
	var name string
	cmd := &cobra.Command{
		Use:   "create",
		Short: "Create a new API token",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			src := locate(cmd)
			out := newOutput(cmd)
			if src.containerised() {
				raw, err := src.run(cmd.Context(), "token", "create", "--name", name, "--json")
				if err != nil {
					return err
				}
				var created struct {
					Token string `json:"token"`
					Name  string `json:"name"`
				}
				if err := json.Unmarshal(raw, &created); err != nil {
					return fmt.Errorf("could not read the answer from the container: %w", err)
				}
				if out.json {
					_, err := out.w.Write(raw)
					return err
				}
				printCreatedToken(out, created.Token, created.Name, src.address)
				return nil
			}
			store, err := src.store()
			if err != nil {
				return err
			}
			plain, tok, err := store.Create(name)
			if err != nil {
				return err
			}
			if out.json {
				return out.emit(map[string]any{
					"token":   plain,
					"name":    tok.Name,
					"created": tok.Created.Format(time.RFC3339),
					"file":    tokens.Path(src.dir),
				})
			}
			printCreatedToken(out, plain, tok.Name, src.address)
			return nil
		},
	}
	cmd.Flags().StringVar(&name, "name", "", "label for this token (e.g. claude-code, ci, blog)")
	return cmd
}

// printCreatedToken shows a new token once, whichever side of the container
// it was written on.
func printCreatedToken(out *output, plain, name, address string) {
	if address == "" {
		address = localAddress()
	}
	out.heading("Token created")
	out.printf("\n")
	out.box(plain)
	out.printf("\n")
	out.success("name: %s (usable immediately, no restart needed)", name)
	out.warn("this is the only time the token is shown; store it now")
	out.printf("\n  Try it:\n")
	out.command(fmt.Sprintf(`curl -X POST -H "Authorization: Bearer %s" \`, plain))
	// The address this instance actually answers on: a printed example with
	// somebody else's port in it is not an example.
	out.command(fmt.Sprintf(`  -F "file=@photo.jpg" %s/upload`, address))
}

func newTokenListCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List tokens (names only, values are not recoverable)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			src := locate(cmd)
			out := newOutput(cmd)
			if src.containerised() {
				raw, err := src.run(cmd.Context(), "token", "list", "--json")
				if err != nil {
					return err
				}
				if out.json {
					_, err := out.w.Write(raw)
					return err
				}
				listed, envCount, err := decodeTokenList(raw)
				if err != nil {
					return err
				}
				printTokenList(out, listed, envCount, src.envFile)
				return nil
			}
			store, err := src.store()
			if err != nil {
				return err
			}
			list := store.List()
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
			printTokenList(out, list, store.EnvCount(), src.envFile)
			return nil
		},
	}
}

// decodeTokenList turns the container's answer back into the same rows a
// local store would have handed over.
func decodeTokenList(raw []byte) ([]tokens.Token, int, error) {
	var doc struct {
		Tokens []struct {
			Name     string `json:"name"`
			Created  string `json:"created"`
			LastUsed string `json:"last_used"`
		} `json:"tokens"`
		EnvTokens int `json:"env_tokens"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, 0, fmt.Errorf("could not read the answer from the container: %w", err)
	}
	list := make([]tokens.Token, 0, len(doc.Tokens))
	for _, t := range doc.Tokens {
		row := tokens.Token{Name: t.Name}
		// A timestamp that cannot be read is shown as the zero time rather
		// than dropping a token from a list somebody is auditing.
		row.Created, _ = time.Parse(time.RFC3339, t.Created)
		if used, err := time.Parse(time.RFC3339, t.LastUsed); err == nil {
			row.LastUsed = &used
		}
		list = append(list, row)
	}
	return list, doc.EnvTokens, nil
}

// printTokenList renders the table and says where the nameless ones are.
func printTokenList(out *output, list []tokens.Token, envCount int, envFile string) {
	if len(list) == 0 && envCount == 0 {
		out.printf("  No tokens yet. Create one with: godrop token create --name default\n")
		return
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
	// The token setup hands over lives in GODROP_TOKENS, because a compose
	// deployment has no data directory on this machine to write a file into
	// until the container has made the volume. It has no name, which is why it
	// is not a row above.
	if envCount > 0 {
		out.printf("\n")
		where := "your environment"
		if envFile != "" {
			where = envFile
		}
		out.skip("%d token(s) come from GODROP_TOKENS in %s, including the one setup gave you", envCount, where)
		out.skip("they have no name; edit that file to change them, and restart the service")
	}
}

func newTokenRevokeCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "revoke <name>",
		Short: "Revoke a token by name",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			src := locate(cmd)
			out := newOutput(cmd)
			if src.containerised() {
				if _, err := src.run(cmd.Context(), "token", "revoke", args[0], "--json"); err != nil {
					return err
				}
				if out.json {
					return out.emit(map[string]any{"revoked": args[0]})
				}
				out.success("%s revoked, effective immediately", args[0])
				return nil
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
