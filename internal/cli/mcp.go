package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/spf13/cobra"

	"github.com/fatihbaltaci/GoDrop/internal/config"
	"github.com/fatihbaltaci/GoDrop/internal/wizard"
)

// godrop mcp: the Model Context Protocol on standard input and output.
//
// The instance already answers MCP over HTTP, and an agent that can reach it
// needs nothing from this command. What that endpoint cannot do is read a
// file: the bytes have to arrive base64 encoded inside the message, so the
// client must be holding them already, and a large file cannot go through at
// all. A process running where the file is has neither problem.
//
// So this stands between the two and does as little as possible. Every message
// is handed to the instance unchanged, and the answer comes back unchanged;
// the only additions are one tool that has to run here, and the token, which
// this side already knows and the client therefore never has to be told.

const (
	toolUploadLocal = "upload_local_file"
	callToolMethod  = "tools/call"
	listToolsMethod = "tools/list"
	codeInternal    = -32603
	codeInvalidArgs = -32602
)

func newMCPCmd(build Build) *cobra.Command {
	var url, token, root string
	cmd := &cobra.Command{
		Use:   "mcp",
		Short: "Serve the Model Context Protocol on stdin and stdout",
		Long: `Serve MCP on standard input and output, for a client that runs it as a
subprocess rather than calling an endpoint.

Messages are passed through to the instance's own /mcp endpoint, so the tools
are the ones it offers, plus one that only works here: uploading a file that is
already on this machine, by path, without encoding it into a message first.

Point a client at it with no configuration beyond the command itself:

    {"mcpServers": {"godrop": {"command": "godrop", "args": ["mcp"]}}}

The address and the token come from the installation on this machine. Set
GODROP_URL and GODROP_TOKEN, or the flags below, to use a different one.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			bridge, err := newBridge(build, url, token, root)
			if err != nil {
				return err
			}
			return bridge.serve(cmd.Context(), cmd.InOrStdin(), cmd.OutOrStdout())
		},
	}
	cmd.Flags().StringVar(&url, "url", "", "instance to talk to (default $GODROP_URL, or the installation on this machine)")
	cmd.Flags().StringVar(&token, "token", "", "API token (default $GODROP_TOKEN, or the one this installation was set up with)")
	cmd.Flags().StringVar(&root, "root", "", "refuse to upload anything outside this directory")
	return cmd
}

// bridge is one run of the command: where the instance is, what may be read
// from this machine, and the lock that keeps two answers from interleaving on
// the one stream they share.
type bridge struct {
	base    string
	token   string
	root    string
	version string
	client  *http.Client
	write   sync.Mutex
	out     *json.Encoder
}

func newBridge(build Build, url, token, root string) (*bridge, error) {
	base := strings.TrimRight(firstNonEmpty(url, os.Getenv("GODROP_URL"), installedAddress()), "/")
	key := firstNonEmpty(token, os.Getenv("GODROP_TOKEN"), firstToken(os.Getenv("GODROP_TOKENS")), installedToken())
	if key == "" {
		return nil, errors.New("no API token: set GODROP_TOKEN, or create one with `godrop token create`")
	}
	b := &bridge{base: base, token: key, version: build.Version, client: &http.Client{}}
	if root == "" {
		return b, nil
	}
	// Resolved once, at startup: a client that asks for a path is answered
	// against the directory as it was when the operator named it.
	abs, err := filepath.EvalSymlinks(root)
	if err != nil {
		return nil, fmt.Errorf("--root %s: %w", root, err)
	}
	b.root, err = filepath.Abs(abs)
	return b, err
}

// installedAddress is where the GoDrop on this machine answers. A client
// launching this command has no environment of its own to say.
func installedAddress() string {
	if dir := installationDir(); installedAt(dir) {
		return wizard.PublicAddress(answersFromEnv(dir))
	}
	return localAddress()
}

// installedToken reads a usable token out of the installation. The token file
// holds digests and can never give one back, but the file setup generated has
// the token itself, which is the one the service was started with.
func installedToken() string {
	dir := installationDir()
	if !installedAt(dir) {
		return ""
	}
	return firstToken(readEnvFile(filepath.Join(dir, ".env"))["GODROP_TOKENS"])
}

func firstToken(list string) string {
	if tokens := config.ParseTokens(list); len(tokens) > 0 {
		return tokens[0]
	}
	return ""
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v = strings.TrimSpace(v); v != "" {
			return v
		}
	}
	return ""
}

// ------------------------------------------------------------------ the loop

// serve reads messages until the client closes the stream.
//
// Each one is answered on its own goroutine: an upload of a few hundred
// megabytes takes as long as it takes, and a client waiting on a keepalive
// behind it would decide this process had died.
func (b *bridge) serve(ctx context.Context, in io.Reader, out io.Writer) error {
	b.out = json.NewEncoder(out)
	// A URL is not HTML and does not want its slashes spelled out.
	b.out.SetEscapeHTML(false)

	var wg sync.WaitGroup
	defer wg.Wait()

	dec := json.NewDecoder(in)
	for {
		var raw json.RawMessage
		if err := dec.Decode(&raw); err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			return err
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			if reply := b.answer(ctx, raw); reply != nil {
				b.write.Lock()
				defer b.write.Unlock()
				_ = b.out.Encode(reply)
			}
		}()
	}
}

// message is as much of one as this side has any business reading.
type message struct {
	ID     json.RawMessage `json:"id"`
	Method string          `json:"method"`
	Params struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
		Meta      struct {
			Version string `json:"io.modelcontextprotocol/protocolVersion"`
		} `json:"_meta"`
	} `json:"params"`
}

func (b *bridge) answer(ctx context.Context, raw json.RawMessage) any {
	var msg message
	// A message this side cannot read is still the instance's to refuse: it
	// answers in the protocol, and this one would have to invent an answer.
	_ = json.Unmarshal(raw, &msg)

	switch {
	case len(msg.ID) == 0 && msg.Method != "":
		// A notification carries nothing a stateless instance needs and
		// nothing this side holds, so there is nothing to pass on and nothing
		// to answer. Something with neither an id nor a method is not a
		// notification but a mistake, and goes on to be told so.
		return nil
	case msg.Method == callToolMethod && msg.Params.Name == toolUploadLocal:
		return b.uploadLocal(ctx, &msg)
	}

	body, status, err := b.forward(ctx, raw, &msg)
	switch {
	case err != nil:
		return rpcFailure(msg.ID, codeInternal, fmt.Sprintf("%s is not answering: %s", b.base, err))
	case status == http.StatusUnauthorized, status == http.StatusForbidden:
		return rpcFailure(msg.ID, codeInternal, tokenRefused)
	case !isRPC(body):
		// A reverse proxy having a bad day answers in HTML, and a client that
		// is handed it has no way to know what happened.
		return rpcFailure(msg.ID, codeInternal,
			fmt.Sprintf("%s answered something that is not MCP: %s", b.base, snippet(body)))
	}
	if msg.Method == listToolsMethod {
		return json.RawMessage(b.withLocalTool(body))
	}
	return json.RawMessage(body)
}

const tokenRefused = "the token was refused; create one with `godrop token create` and set GODROP_TOKEN"

// isRPC reports whether an answer is one the client can make sense of.
func isRPC(body []byte) bool {
	var envelope struct {
		JSONRPC string `json:"jsonrpc"`
	}
	return json.Unmarshal(body, &envelope) == nil && envelope.JSONRPC == "2.0"
}

// snippet is as much of an unexpected answer as belongs in an error message.
func snippet(body []byte) string {
	text := strings.Join(strings.Fields(string(body)), " ")
	if len(text) > 120 {
		text = text[:120] + "..."
	}
	if text == "" {
		return "nothing at all"
	}
	return text
}

// forward hands one message to the instance and brings the answer back
// untouched, so that everything the protocol says about it stays true.
func (b *bridge) forward(ctx context.Context, raw json.RawMessage, msg *message) ([]byte, int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, b.base+"/mcp", bytes.NewReader(raw))
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set("Authorization", "Bearer "+b.token)
	if v := msg.Params.Meta.Version; v != "" {
		// The headers that mirror the body belong to the revision that carries
		// its version in the body. A client from before it sends neither.
		req.Header.Set("MCP-Protocol-Version", v)
		req.Header.Set("Mcp-Method", msg.Method)
		if msg.Method == callToolMethod {
			req.Header.Set("Mcp-Name", msg.Params.Name)
		}
	}
	resp, err := b.client.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	return body, resp.StatusCode, err
}

// withLocalTool adds the tool that only exists on this side to the list the
// instance answered with. Everything else in that answer, the caching hints
// included, is left as the instance wrote it.
func (b *bridge) withLocalTool(body []byte) []byte {
	var reply map[string]any
	_ = json.Unmarshal(body, &reply)
	result, ok := reply["result"].(map[string]any)
	if !ok {
		return body // an error, and not one this side improves on
	}
	var local any
	_ = json.Unmarshal([]byte(localTool), &local)
	tools, _ := result["tools"].([]any)
	result["tools"] = append(tools, local)
	next, _ := json.Marshal(reply)
	return next
}

// ------------------------------------------------------------ the local tool

const localTool = `{
  "name": "` + toolUploadLocal + `",
  "title": "Upload a file from this machine",
  "description": "Upload a file that is already on the machine this server runs on, by path, and get back a public URL that opens without a token. Prefer this over upload_file whenever the file is on disk: nothing is encoded into the message, so there is no size limit beyond the one the instance enforces.",
  "inputSchema": {
    "type": "object",
    "properties": {
      "path": {
        "type": "string",
        "description": "Path to the file. A leading ~ is expanded, and a relative path is resolved against the directory this server was started in."
      },
      "name": {
        "type": "string",
        "description": "Optional name to store it under. Defaults to the file's own name. The extension decides the media type the URL is served with."
      },
      "expires_in": {
        "type": "string",
        "description": "Optional lifetime such as \"30m\", \"12h\" or \"7d\". The file deletes itself when it runs out."
      }
    },
    "required": ["path"],
    "additionalProperties": false
  },
  "outputSchema": {
    "type": "object",
    "properties": {
      "url": { "type": "string", "description": "The public URL. It opens without a token." },
      "name": { "type": "string" },
      "size_bytes": { "type": "integer" },
      "expires_at": { "type": "string", "description": "RFC 3339, present only when the upload expires." }
    },
    "required": ["url", "size_bytes"]
  },
  "annotations": {
    "readOnlyHint": false,
    "destructiveHint": false,
    "idempotentHint": false,
    "openWorldHint": false
  }
}`

func (b *bridge) uploadLocal(ctx context.Context, msg *message) any {
	var in struct {
		Path      string `json:"path"`
		Name      string `json:"name"`
		ExpiresIn string `json:"expires_in"`
	}
	args := msg.Params.Arguments
	if len(args) == 0 {
		args = json.RawMessage("{}")
	}
	if err := json.Unmarshal(args, &in); err != nil {
		return rpcFailure(msg.ID, codeInvalidArgs, toolUploadLocal+": arguments must be an object ("+err.Error()+")")
	}
	if strings.TrimSpace(in.Path) == "" {
		return b.toolFailure(msg.ID, `"path" is required: where the file is on this machine.`)
	}

	path, err := b.resolve(in.Path)
	if err != nil {
		return b.toolFailure(msg.ID, "%s", err)
	}
	// What it is comes first, because a directory opens perfectly well and
	// then fails halfway through being read.
	info, err := os.Stat(path)
	switch {
	case err != nil:
		return b.toolFailure(msg.ID, "%s cannot be read: %s", path, err)
	case info.IsDir():
		return b.toolFailure(msg.ID, "%s is a directory. Name a file inside it.", path)
	}
	file, err := os.Open(path) //nolint:gosec // G304, opening a named file is what this tool is
	if err != nil {
		return b.toolFailure(msg.ID, "%s cannot be read: %s", path, err)
	}
	defer file.Close()

	name := strings.TrimSpace(in.Name)
	if name == "" {
		name = filepath.Base(path)
	}
	uploaded, status, err := b.upload(ctx, name, in.ExpiresIn, file)
	switch {
	case err != nil:
		return rpcFailure(msg.ID, codeInternal, fmt.Sprintf("%s is not answering: %s", b.base, err))
	case status == http.StatusUnauthorized, status == http.StatusForbidden:
		return rpcFailure(msg.ID, codeInternal, tokenRefused)
	case status != http.StatusCreated:
		// Everything else the API refuses, it refuses with a reason that says
		// what to do differently, so the model is the right audience for it.
		return b.toolFailure(msg.ID, "%s", uploaded.Error)
	}

	stored := uploaded.Files[0]
	text := stored.URL
	if stored.ExpiresAt != "" {
		text += "\n\nThis link stops working at " + stored.ExpiresAt + "."
	}
	return b.toolResult(msg.ID, map[string]any{
		"content": []any{
			map[string]any{"type": "text", "text": text},
			map[string]any{"type": "resource_link", "uri": stored.URL, "name": stored.Name},
		},
		"structuredContent": stored,
		"isError":           false,
	})
}

// absPath is a seam: resolving a relative path fails only when the working
// directory has been taken out from under the process, which a test cannot
// arrange for itself.
var absPath = filepath.Abs

// resolve turns what the client asked for into a path this command is willing
// to open.
func (b *bridge) resolve(path string) (string, error) {
	if rest, ok := strings.CutPrefix(path, "~"); ok {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("%s: there is no home directory here to expand ~ to", path)
		}
		path = filepath.Join(home, rest)
	}
	abs, err := absPath(path)
	if err != nil {
		return "", fmt.Errorf("%s: %w", path, err)
	}
	if b.root == "" {
		return abs, nil
	}
	// A symlink is a path too, and the answer has to be about where it lands.
	target, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return "", fmt.Errorf("%s cannot be read: %w", abs, err)
	}
	rel, err := filepath.Rel(b.root, target)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("%s is outside %s, which is the only place this server may read", abs, b.root)
	}
	return target, nil
}

// uploadResponse is the part of the API's answer this side reads: the file it
// stored, or the reason it did not.
type uploadResponse struct {
	Files []struct {
		URL       string `json:"url"`
		Name      string `json:"name"`
		SizeBytes int64  `json:"size_bytes"`
		ExpiresAt string `json:"expires_at,omitempty"`
	} `json:"files"`
	Error string `json:"error"`
}

// upload streams the file to the API rather than reading it: the whole point
// of this tool is the file that would not fit in a message.
func (b *bridge) upload(ctx context.Context, name, expires string, file io.Reader) (uploadResponse, int, error) {
	pr, pw := io.Pipe()
	form := multipart.NewWriter(pw)
	go func() {
		part, err := form.CreateFormFile("file", name)
		if err == nil {
			_, err = io.Copy(part, file)
		}
		if err != nil {
			_ = pw.CloseWithError(err)
			return
		}
		_ = pw.CloseWithError(form.Close())
	}()
	defer pr.Close()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, b.base+"/upload", pr)
	if err != nil {
		return uploadResponse{}, 0, err
	}
	req.Header.Set("Content-Type", form.FormDataContentType())
	req.Header.Set("Authorization", "Bearer "+b.token)
	if expires = strings.TrimSpace(expires); expires != "" {
		req.Header.Set("X-Expires-In", expires)
	}
	resp, err := b.client.Do(req)
	if err != nil {
		return uploadResponse{}, 0, err
	}
	defer resp.Body.Close()

	var answer uploadResponse
	if err := json.NewDecoder(resp.Body).Decode(&answer); err != nil {
		return uploadResponse{}, resp.StatusCode, fmt.Errorf("the answer could not be read: %w", err)
	}
	if resp.StatusCode == http.StatusCreated && len(answer.Files) == 0 {
		return uploadResponse{}, resp.StatusCode, errors.New("the upload succeeded but named no file")
	}
	return answer, resp.StatusCode, nil
}

// ------------------------------------------------------------------- answers

func (b *bridge) meta() map[string]any {
	return map[string]any{
		"io.modelcontextprotocol/serverInfo": map[string]any{"name": "godrop", "version": b.version},
	}
}

func (b *bridge) toolResult(id json.RawMessage, result map[string]any) map[string]any {
	result["resultType"] = "complete"
	result["_meta"] = b.meta()
	return map[string]any{"jsonrpc": "2.0", "id": id, "result": result}
}

// toolFailure is a mistake the model can correct on its own, which is why it
// is a result and not an error: the text is what it reads.
func (b *bridge) toolFailure(id json.RawMessage, format string, a ...any) map[string]any {
	return b.toolResult(id, map[string]any{
		"content": []any{map[string]any{"type": "text", "text": fmt.Sprintf(format, a...)}},
		"isError": true,
	})
}

func rpcFailure(id json.RawMessage, code int, message string) map[string]any {
	return map[string]any{
		"jsonrpc": "2.0",
		"id":      id,
		"error":   map[string]any{"code": code, "message": message},
	}
}
