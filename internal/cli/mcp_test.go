package cli

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/fatihbaltaci/GoDrop/internal/config"
	"github.com/fatihbaltaci/GoDrop/internal/server"
	"github.com/fatihbaltaci/GoDrop/internal/storage"
	"github.com/fatihbaltaci/GoDrop/internal/tokens"
	"github.com/fatihbaltaci/GoDrop/internal/wizard"
)

// The bridge is tested against a real GoDrop rather than a description of one:
// what it has to get right is the endpoint's own rules, and a stub would only
// prove that this side agrees with itself.

const bridgeToken = "bridge-token-with-enough-entropy"

func instanceFor(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	cfg, err := config.LoadFrom(func(key string) string {
		switch key {
		case "GODROP_TOKENS":
			return bridgeToken
		case "GODROP_DATA_DIR":
			return dir
		}
		return ""
	})
	if err != nil {
		t.Fatalf("config: %v", err)
	}
	store, err := storage.New(cfg.DataDir, cfg.MaxTotalSize)
	if err != nil {
		t.Fatalf("storage: %v", err)
	}
	tokenStore, err := tokens.New(tokens.Path(dir), cfg.Tokens)
	if err != nil {
		t.Fatalf("tokens: %v", err)
	}
	ts := httptest.NewServer(server.New(server.Options{
		Config: cfg, Store: store, Tokens: tokenStore, Version: "test",
	}))
	t.Cleanup(ts.Close)
	return ts.URL
}

func bridgeTo(t *testing.T, base, root string) *bridge {
	t.Helper()
	b, err := newBridge(testBuild(), base, bridgeToken, root)
	if err != nil {
		t.Fatalf("newBridge: %v", err)
	}
	return b
}

// talk runs the bridge over a script of messages and returns what it wrote,
// keyed by the id each answer belongs to. Answers are not ordered: each
// message is handled on its own, so an upload cannot hold up a keepalive
// behind it.
func talk(t *testing.T, b *bridge, messages ...string) map[string]map[string]any {
	t.Helper()
	var out strings.Builder
	if err := b.serve(context.Background(), strings.NewReader(strings.Join(messages, "\n")), &out); err != nil {
		t.Fatalf("serve: %v", err)
	}
	replies := map[string]map[string]any{}
	dec := json.NewDecoder(strings.NewReader(out.String()))
	for {
		var reply map[string]any
		if err := dec.Decode(&reply); err != nil {
			break
		}
		id, _ := json.Marshal(reply["id"])
		replies[string(id)] = reply
	}
	return replies
}

// one runs a single message and insists on an answer to it.
func one(t *testing.T, b *bridge, message string) map[string]any {
	t.Helper()
	replies := talk(t, b, message)
	if len(replies) != 1 {
		t.Fatalf("got %d answers, want 1: %v", len(replies), replies)
	}
	for _, reply := range replies {
		if reply["jsonrpc"] != "2.0" {
			t.Errorf("jsonrpc = %v", reply["jsonrpc"])
		}
		return reply
	}
	return nil
}

// result digs out the result of a tool call, failing when the answer was an
// error instead.
func result(t *testing.T, reply map[string]any) map[string]any {
	t.Helper()
	if e, ok := reply["error"]; ok {
		t.Fatalf("error = %v", e)
	}
	got, _ := reply["result"].(map[string]any)
	if got == nil {
		t.Fatalf("no result in %v", reply)
	}
	return got
}

// toolText is what the model reads back.
func toolText(t *testing.T, reply map[string]any) string {
	t.Helper()
	var b strings.Builder
	content, _ := result(t, reply)["content"].([]any)
	for _, item := range content {
		block, _ := item.(map[string]any)
		if block["type"] == "text" {
			b.WriteString(block["text"].(string))
		}
	}
	return b.String()
}

func callLocal(args string) string {
	return `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"` +
		toolUploadLocal + `","arguments":` + args + `}}`
}

// ------------------------------------------------------------------ passthrough

func TestMCPBridgeHandsEveryMessageToTheInstance(t *testing.T) {
	b := bridgeTo(t, instanceFor(t), "")

	// A client that opens with a handshake, which is still most of them.
	reply := one(t, b, `{"jsonrpc":"2.0","id":1,"method":"initialize",
		"params":{"protocolVersion":"2025-06-18","capabilities":{},
		"clientInfo":{"name":"desktop","version":"1"}}}`)
	if got := result(t, reply)["protocolVersion"]; got != "2025-06-18" {
		t.Errorf("protocolVersion = %v", got)
	}

	// The list is the instance's, with the one tool that has to run here.
	reply = one(t, b, `{"jsonrpc":"2.0","id":2,"method":"tools/list","params":{}}`)
	listed := result(t, reply)
	var names []string
	tools, _ := listed["tools"].([]any)
	for _, item := range tools {
		tool, _ := item.(map[string]any)
		names = append(names, tool["name"].(string))
	}
	want := "upload_file,delete_file,storage_stats," + toolUploadLocal
	if strings.Join(names, ",") != want {
		t.Errorf("tools = %v, want %s", names, want)
	}
	// What the instance said about caching its own list survives the addition.
	if listed["cacheScope"] != "public" || listed["resultType"] != "complete" {
		t.Errorf("the instance's own answer was rewritten: %v", listed)
	}

	// A tool of the instance's is answered by the instance.
	reply = one(t, b, `{"jsonrpc":"2.0","id":3,"method":"tools/call",
		"params":{"name":"storage_stats","arguments":{}}}`)
	if !strings.Contains(toolText(t, reply), "0 files") {
		t.Errorf("text = %q", toolText(t, reply))
	}
}

func TestMCPBridgeMirrorsTheHeadersTheNewerRevisionRequires(t *testing.T) {
	// The instance refuses a 2026-07-28 request whose headers do not repeat
	// what the body says, so this passing is the proof they are mirrored.
	b := bridgeTo(t, instanceFor(t), "")
	meta := `"_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28",` +
		`"io.modelcontextprotocol/clientCapabilities":{}}`

	reply := one(t, b, `{"jsonrpc":"2.0","id":1,"method":"server/discover","params":{`+meta+`}}`)
	if got := result(t, reply)["supportedVersions"]; got == nil {
		t.Errorf("result = %v", reply["result"])
	}

	reply = one(t, b, `{"jsonrpc":"2.0","id":2,"method":"tools/call",
		"params":{"name":"storage_stats","arguments":{},`+meta+`}}`)
	if result(t, reply)["resultType"] != "complete" {
		t.Errorf("result = %v", reply["result"])
	}
}

func TestMCPBridgeAnswersEachMessageOnItsOwn(t *testing.T) {
	b := bridgeTo(t, instanceFor(t), "")
	replies := talk(t, b,
		`{"jsonrpc":"2.0","id":1,"method":"ping","params":{}}`,
		`{"jsonrpc":"2.0","method":"notifications/initialized"}`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/list","params":{}}`,
	)
	// The notification is not one a stateless instance needs, and gets no
	// answer of its own.
	if len(replies) != 2 || replies["1"] == nil || replies["2"] == nil {
		t.Fatalf("answers = %v", replies)
	}
}

func TestMCPBridgeLetsTheInstanceRefuseWhatItCannotRead(t *testing.T) {
	b := bridgeTo(t, instanceFor(t), "")
	// Neither an id nor a method: not a notification, so it is passed on and
	// answered in the protocol rather than dropped in silence.
	reply := one(t, b, `[1,2]`)
	rpcErr, _ := reply["error"].(map[string]any)
	if rpcErr == nil {
		t.Fatalf("reply = %v", reply)
	}
}

func TestMCPBridgeAddsNothingToAListThatFailed(t *testing.T) {
	b := bridgeTo(t, instanceFor(t), "")
	// A 2026-07-28 request without the capabilities it must declare, so the
	// instance answers with an error and there is no list to add to.
	reply := one(t, b, `{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{
		"_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28"}}}`)
	if reply["error"] == nil {
		t.Fatalf("reply = %v", reply)
	}
}

// ---------------------------------------------------------------- the upload

func TestMCPBridgeUploadsAFileThatIsAlreadyOnDisk(t *testing.T) {
	base := instanceFor(t)
	b := bridgeTo(t, base, "")
	path := filepath.Join(t.TempDir(), "checkout.png")
	if err := os.WriteFile(path, []byte("pretend png"), 0o600); err != nil {
		t.Fatal(err)
	}

	reply := one(t, b, callLocal(`{"path":`+quote(path)+`}`))
	got := result(t, reply)
	if got["isError"] == true {
		t.Fatalf("upload failed: %s", toolText(t, reply))
	}
	stored, _ := got["structuredContent"].(map[string]any)
	url, _ := stored["url"].(string)
	switch {
	case !strings.HasPrefix(url, base+"/f/"):
		t.Errorf("url = %q", url)
	case stored["name"] != "checkout.png":
		t.Errorf("name = %v", stored["name"])
	case stored["size_bytes"] != float64(len("pretend png")):
		t.Errorf("size_bytes = %v", stored["size_bytes"])
	}
	// The URL comes back on its own line, ready to be pasted.
	if toolText(t, reply) != url {
		t.Errorf("text = %q, want the bare URL", toolText(t, reply))
	}

	resp, err := http.Get(url) //nolint:gosec,noctx // the test's own httptest server
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if body, _ := io.ReadAll(resp.Body); string(body) != "pretend png" {
		t.Errorf("downloaded %q", body)
	}
}

func TestMCPBridgeUploadTakesANameAndAnExpiry(t *testing.T) {
	b := bridgeTo(t, instanceFor(t), "")
	path := filepath.Join(t.TempDir(), "tmp-4821.dat")
	if err := os.WriteFile(path, []byte("bytes"), 0o600); err != nil {
		t.Fatal(err)
	}

	reply := one(t, b, callLocal(`{"path":`+quote(path)+`,"name":"report.pdf","expires_in":"7d"}`))
	stored, _ := result(t, reply)["structuredContent"].(map[string]any)
	if stored["name"] != "report.pdf" {
		t.Errorf("name = %v", stored["name"])
	}
	if stored["expires_at"] == nil {
		t.Fatalf("expires_at is missing: %v", stored)
	}
	if !strings.Contains(toolText(t, reply), "stops working") {
		t.Errorf("the model should be told when the link dies: %q", toolText(t, reply))
	}
}

func TestMCPBridgeExpandsAPathFromHome(t *testing.T) {
	b := bridgeTo(t, instanceFor(t), "")
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, "shot.png"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Remove(filepath.Join(home, "shot.png")) })

	reply := one(t, b, callLocal(`{"path":"~/shot.png"}`))
	if result(t, reply)["isError"] == true {
		t.Fatalf("upload failed: %s", toolText(t, reply))
	}
}

func TestMCPBridgeReportsWhatItCannotUpload(t *testing.T) {
	dir := t.TempDir()
	b := bridgeTo(t, instanceFor(t), "")
	for name, tc := range map[string]struct{ args, want string }{
		"no path":     {`{}`, `"path" is required`},
		"blank path":  {`{"path":"  "}`, `"path" is required`},
		"not there":   {`{"path":` + quote(filepath.Join(dir, "missing.png")) + `}`, "cannot be read"},
		"a directory": {`{"path":` + quote(dir) + `}`, "is a directory"},
	} {
		t.Run(name, func(t *testing.T) {
			reply := one(t, b, callLocal(tc.args))
			if result(t, reply)["isError"] != true {
				t.Fatalf("result = %v", reply["result"])
			}
			if !strings.Contains(toolText(t, reply), tc.want) {
				t.Errorf("text = %q, want %q", toolText(t, reply), tc.want)
			}
		})
	}

	// Arguments that are not arguments at all are the request's mistake, not
	// something the model fixes by trying again the same way.
	reply := one(t, b, callLocal(`["a"]`))
	rpcErr, _ := reply["error"].(map[string]any)
	if rpcErr == nil || rpcErr["code"] != float64(codeInvalidArgs) {
		t.Fatalf("error = %v", reply["error"])
	}
}

func TestMCPBridgeReportsAnUploadTheInstanceRefuses(t *testing.T) {
	// The instance's own limits are its to enforce, and its reason is the one
	// worth passing on.
	base := instanceFor(t)
	b := bridgeTo(t, base, "")
	path := filepath.Join(t.TempDir(), "big.bin")
	if err := os.WriteFile(path, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}

	reply := one(t, b, callLocal(`{"path":`+quote(path)+`,"expires_in":"soon"}`))
	if result(t, reply)["isError"] != true {
		t.Fatalf("result = %v", reply["result"])
	}
	if !strings.Contains(toolText(t, reply), "expiry") {
		t.Errorf("text = %q", toolText(t, reply))
	}
}

// ------------------------------------------------------------------- the root

func TestMCPBridgeStaysInsideTheDirectoryItWasGiven(t *testing.T) {
	root := t.TempDir()
	b := bridgeTo(t, instanceFor(t), root)
	inside := filepath.Join(root, "inside.txt")
	if err := os.WriteFile(inside, []byte("fine"), 0o600); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(t.TempDir(), "secrets.txt")
	if err := os.WriteFile(outside, []byte("private"), 0o600); err != nil {
		t.Fatal(err)
	}

	if result(t, one(t, b, callLocal(`{"path":`+quote(inside)+`}`)))["isError"] == true {
		t.Error("a file inside the root should upload")
	}

	reply := one(t, b, callLocal(`{"path":`+quote(outside)+`}`))
	if result(t, reply)["isError"] != true || !strings.Contains(toolText(t, reply), "outside") {
		t.Errorf("text = %q", toolText(t, reply))
	}

	// A symlink is a path too, and where it lands is what the answer is about.
	link := filepath.Join(root, "shortcut.txt")
	if err := os.Symlink(outside, link); err != nil {
		t.Skipf("symlinks are not available here: %v", err)
	}
	reply = one(t, b, callLocal(`{"path":`+quote(link)+`}`))
	if result(t, reply)["isError"] != true {
		t.Errorf("a link out of the root is still out of the root: %v", reply["result"])
	}

	// Nothing to resolve is nothing to allow.
	reply = one(t, b, callLocal(`{"path":`+quote(filepath.Join(root, "missing.txt"))+`}`))
	if result(t, reply)["isError"] != true {
		t.Errorf("result = %v", reply["result"])
	}
}

func TestMCPBridgeReportsAPathItCannotResolve(t *testing.T) {
	b := bridgeTo(t, instanceFor(t), "")

	// No home to expand ~ against.
	for _, key := range []string{"HOME", "USERPROFILE"} {
		t.Setenv(key, "")
	}
	reply := one(t, b, callLocal(`{"path":"~/shot.png"}`))
	if result(t, reply)["isError"] != true || !strings.Contains(toolText(t, reply), "no home directory") {
		t.Errorf("text = %q", toolText(t, reply))
	}

	// A working directory that has gone away, which is the only thing that
	// stops a relative path from resolving.
	absPath = func(string) (string, error) { return "", os.ErrNotExist }
	t.Cleanup(func() { absPath = filepath.Abs })
	reply = one(t, b, callLocal(`{"path":"shot.png"}`))
	if result(t, reply)["isError"] != true || !strings.Contains(toolText(t, reply), "shot.png") {
		t.Errorf("text = %q", toolText(t, reply))
	}
}

func TestMCPBridgeReportsAFileItMayNotRead(t *testing.T) {
	requireStrictPermissions(t)
	b := bridgeTo(t, instanceFor(t), "")
	path := filepath.Join(t.TempDir(), "private.txt")
	if err := os.WriteFile(path, []byte("secret"), 0o000); err != nil {
		t.Fatal(err)
	}

	reply := one(t, b, callLocal(`{"path":`+quote(path)+`}`))
	if result(t, reply)["isError"] != true || !strings.Contains(toolText(t, reply), "cannot be read") {
		t.Errorf("text = %q", toolText(t, reply))
	}
}

func TestMCPBridgeUploadWithNoArgumentsAtAll(t *testing.T) {
	b := bridgeTo(t, instanceFor(t), "")
	reply := one(t, b, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"`+
		toolUploadLocal+`"}}`)
	if !strings.Contains(toolText(t, reply), `"path" is required`) {
		t.Errorf("text = %q", toolText(t, reply))
	}
}

func TestMCPBridgeReportsAnAddressThatIsNotOne(t *testing.T) {
	// A typo in --url is not a request that can be built, let alone sent.
	b := bridgeTo(t, "http://what is this", "")
	path := filepath.Join(t.TempDir(), "a.txt")
	if err := os.WriteFile(path, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, message := range []string{
		`{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{}}`,
		callLocal(`{"path":` + quote(path) + `}`),
	} {
		reply := one(t, b, message)
		rpcErr, _ := reply["error"].(map[string]any)
		if rpcErr == nil || !strings.Contains(rpcErr["message"].(string), "not answering") {
			t.Errorf("error = %v", reply["error"])
		}
	}
}

func TestMCPBridgeRefusesARootThatIsNotThere(t *testing.T) {
	_, err := newBridge(testBuild(), "http://localhost:1", bridgeToken,
		filepath.Join(t.TempDir(), "nowhere"))
	if err == nil || !strings.Contains(err.Error(), "--root") {
		t.Fatalf("err = %v", err)
	}
}

// ---------------------------------------------------------------- the instance

func TestMCPBridgeSaysWhenTheInstanceIsNotThere(t *testing.T) {
	// A closed port on this machine, so nothing is waited for.
	closed := httptest.NewServer(http.NotFoundHandler())
	base := closed.URL
	closed.Close()
	b := bridgeTo(t, base, "")

	reply := one(t, b, `{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{}}`)
	rpcErr, _ := reply["error"].(map[string]any)
	if rpcErr == nil || !strings.Contains(rpcErr["message"].(string), "not answering") {
		t.Fatalf("error = %v", reply["error"])
	}

	path := filepath.Join(t.TempDir(), "a.txt")
	if err := os.WriteFile(path, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	reply = one(t, b, callLocal(`{"path":`+quote(path)+`}`))
	rpcErr, _ = reply["error"].(map[string]any)
	if rpcErr == nil || !strings.Contains(rpcErr["message"].(string), "not answering") {
		t.Fatalf("error = %v", reply["error"])
	}
}

func TestMCPBridgeSaysWhenTheTokenIsRefused(t *testing.T) {
	base := instanceFor(t)
	b, err := newBridge(testBuild(), base, "not-the-token", "")
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "a.txt")
	if err := os.WriteFile(path, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}

	for _, message := range []string{
		`{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{}}`,
		callLocal(`{"path":` + quote(path) + `}`),
	} {
		reply := one(t, b, message)
		rpcErr, _ := reply["error"].(map[string]any)
		if rpcErr == nil || !strings.Contains(rpcErr["message"].(string), "godrop token create") {
			t.Errorf("error = %v", reply["error"])
		}
	}
}

func TestMCPBridgeSaysWhenTheAnswerIsNotMCPAtAll(t *testing.T) {
	// What a reverse proxy having a bad day sends back.
	var body string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/upload") {
			w.WriteHeader(http.StatusCreated)
		}
		_, _ = io.WriteString(w, body)
	}))
	defer ts.Close()
	b := bridgeTo(t, ts.URL, "")

	body = "<html><body>" + strings.Repeat("502 Bad Gateway ", 40) + "</body></html>"
	reply := one(t, b, `{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{}}`)
	rpcErr, _ := reply["error"].(map[string]any)
	if rpcErr == nil || !strings.Contains(rpcErr["message"].(string), "not MCP") {
		t.Fatalf("error = %v", reply["error"])
	}
	if message := rpcErr["message"].(string); !strings.Contains(message, "...") {
		t.Errorf("a wall of HTML should be cut short: %q", message)
	}

	body = ""
	reply = one(t, b, `{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{}}`)
	rpcErr, _ = reply["error"].(map[string]any)
	if rpcErr == nil || !strings.Contains(rpcErr["message"].(string), "nothing at all") {
		t.Fatalf("error = %v", reply["error"])
	}

	// The same thing where the upload goes: a 201 that names no file.
	path := filepath.Join(t.TempDir(), "a.txt")
	if err := os.WriteFile(path, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	body = `{"files":[]}`
	reply = one(t, b, callLocal(`{"path":`+quote(path)+`}`))
	rpcErr, _ = reply["error"].(map[string]any)
	if rpcErr == nil || !strings.Contains(rpcErr["message"].(string), "named no file") {
		t.Fatalf("error = %v", reply["error"])
	}

	body = "not json"
	reply = one(t, b, callLocal(`{"path":`+quote(path)+`}`))
	rpcErr, _ = reply["error"].(map[string]any)
	if rpcErr == nil || !strings.Contains(rpcErr["message"].(string), "could not be read") {
		t.Fatalf("error = %v", reply["error"])
	}
}

func TestMCPBridgeStopsWhenTheStreamBreaks(t *testing.T) {
	b := bridgeTo(t, instanceFor(t), "")
	if err := b.serve(context.Background(), &failingReader{}, io.Discard); err == nil {
		t.Error("a stream that cannot be read is not a clean end")
	}
}

type failingReader struct{}

func (*failingReader) Read([]byte) (int, error) { return 0, os.ErrInvalid }

// ------------------------------------------------------------------ the setup

func TestMCPFindsTheInstallationOnThisMachine(t *testing.T) {
	for _, key := range []string{"GODROP_URL", "GODROP_TOKEN", "GODROP_TOKENS"} {
		t.Setenv(key, "")
	}
	dir := installAt(t, wizard.DeploySystemd, "https://files.example.com")
	values := readEnvFile(filepath.Join(dir, ".env"))

	b, err := newBridge(testBuild(), "", "", "")
	if err != nil {
		t.Fatalf("newBridge: %v", err)
	}
	if b.base != "https://files.example.com" {
		t.Errorf("base = %q", b.base)
	}
	if want := firstToken(values["GODROP_TOKENS"]); b.token != want || want == "" {
		t.Errorf("token = %q, want the one setup wrote (%q)", b.token, want)
	}

	// What is said explicitly wins over what was found.
	t.Setenv("GODROP_URL", "https://from-the-environment.example")
	t.Setenv("GODROP_TOKEN", "from-the-environment")
	if b, err = newBridge(testBuild(), "", "", ""); err != nil {
		t.Fatal(err)
	}
	if b.base != "https://from-the-environment.example" || b.token != "from-the-environment" {
		t.Errorf("bridge = %q %q", b.base, b.token)
	}
	if b, err = newBridge(testBuild(), "https://flag.example/", "flag-token", ""); err != nil {
		t.Fatal(err)
	}
	if b.base != "https://flag.example" || b.token != "flag-token" {
		t.Errorf("bridge = %q %q", b.base, b.token)
	}
}

func TestMCPFallsBackToTheLocalAddress(t *testing.T) {
	for _, key := range []string{"GODROP_URL", "GODROP_TOKEN", "GODROP_BASE_URL", "GODROP_ADDR"} {
		t.Setenv(key, "")
	}
	t.Setenv("GODROP_TOKENS", "from-the-environment,second")

	b, err := newBridge(testBuild(), "", "", "")
	if err != nil {
		t.Fatalf("newBridge: %v", err)
	}
	if !strings.HasPrefix(b.base, "http://localhost:") || b.token != "from-the-environment" {
		t.Errorf("bridge = %q %q", b.base, b.token)
	}
}

func TestMCPSaysHowToGetAToken(t *testing.T) {
	for _, key := range []string{"GODROP_URL", "GODROP_TOKEN", "GODROP_TOKENS"} {
		t.Setenv(key, "")
	}
	code, _, stderr := run(t, testBuild(), "mcp")
	if code == 0 || !strings.Contains(stderr, "godrop token create") {
		t.Fatalf("exit = %d, stderr = %q", code, stderr)
	}
}

func TestMCPServesUntilTheClientClosesTheStream(t *testing.T) {
	// The client runs this as a subprocess and ends it by closing the pipe,
	// which is the only way out that is not an error.
	t.Setenv("GODROP_URL", instanceFor(t))
	t.Setenv("GODROP_TOKEN", bridgeToken)

	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.WriteString(w, `{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{}}`); err != nil {
		t.Fatal(err)
	}
	_ = w.Close()
	stdin := os.Stdin
	os.Stdin = r
	t.Cleanup(func() { os.Stdin = stdin; _ = r.Close() })

	code, out, stderr := run(t, testBuild(), "mcp")
	if code != 0 {
		t.Fatalf("exit = %d: %s", code, stderr)
	}
	if !strings.Contains(out, toolUploadLocal) {
		t.Errorf("output = %q", out)
	}
}

func TestLocalToolIsValidAndDescribesItself(t *testing.T) {
	var tool struct {
		Name        string         `json:"name"`
		Title       string         `json:"title"`
		Description string         `json:"description"`
		InputSchema map[string]any `json:"inputSchema"`
	}
	if err := json.Unmarshal([]byte(localTool), &tool); err != nil {
		t.Fatalf("the tool this side adds is not valid JSON: %v", err)
	}
	switch {
	case tool.Name != toolUploadLocal:
		t.Errorf("name = %q", tool.Name)
	case tool.Title == "" || tool.Description == "":
		t.Error("a tool with no description is one no model will pick")
	case tool.InputSchema["type"] != "object":
		t.Errorf("inputSchema = %v", tool.InputSchema)
	}
}

// quote renders a path as a JSON string, which is the only safe way to put a
// Windows path inside a message.
func quote(path string) string {
	out, _ := json.Marshal(path)
	return string(out)
}
