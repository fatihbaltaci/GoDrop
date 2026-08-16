package server

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/fatihbaltaci/GoDrop/internal/config"
	"github.com/fatihbaltaci/GoDrop/internal/storage"
)

// The MCP endpoint is driven the way a conforming client drives it: one POST
// per message, the metadata in the body, and the headers mirroring it.

// rpcReply is a response as it arrives, with the result left undecoded so each
// test can read it as the shape that method returns.
type rpcReply struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  json.RawMessage `json:"result"`
	Error   *mcpError       `json:"error"`
}

// mcpBody builds one JSON-RPC message, carrying the per-request metadata that
// replaced the handshake.
func mcpBody(id any, method string, params map[string]any) map[string]any {
	if params == nil {
		params = map[string]any{}
	}
	params["_meta"] = map[string]any{
		metaVersion:      mcpVersion,
		metaCapabilities: map[string]any{},
		"io.modelcontextprotocol/clientInfo": map[string]any{
			"name": "godrop-test", "version": "1",
		},
	}
	msg := map[string]any{"jsonrpc": "2.0", "method": method, "params": params}
	if id != nil {
		msg["id"] = id
	}
	return msg
}

// rpc posts one message. Headers named in override replace what a conforming
// client would have sent, and an empty value removes the header entirely,
// which is how the validation tests misbehave on purpose.
func (h *harness) rpc(t *testing.T, msg map[string]any, override map[string]string) *http.Response {
	t.Helper()
	raw, err := json.Marshal(msg)
	if err != nil {
		t.Fatal(err)
	}
	return h.rpcRaw(t, raw, override)
}

func (h *harness) rpcRaw(t *testing.T, raw []byte, override map[string]string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, h.URL+"/mcp", bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set("Authorization", "Bearer "+testToken)
	req.Header.Set("MCP-Protocol-Version", mcpVersion)

	var msg mcpRequest
	if err := json.Unmarshal(raw, &msg); err == nil {
		req.Header.Set("Mcp-Method", msg.Method)
		if msg.Method == callToolMethod {
			req.Header.Set("Mcp-Name", msg.Params.Name)
		}
	}
	for k, v := range override {
		if v == "" {
			req.Header.Del(k)
			continue
		}
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = resp.Body.Close() })
	return resp
}

// reply sends a message and decodes the answer, insisting on the status code
// the caller expects.
func (h *harness) reply(t *testing.T, status int, msg map[string]any, override map[string]string) rpcReply {
	t.Helper()
	resp := h.rpc(t, msg, override)
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != status {
		t.Fatalf("status = %d, want %d: %s", resp.StatusCode, status, body)
	}
	var got rpcReply
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("response is not JSON-RPC: %v\n%s", err, body)
	}
	if got.JSONRPC != "2.0" {
		t.Errorf("jsonrpc = %q", got.JSONRPC)
	}
	return got
}

// call runs one tool and returns the result it produced. Nil arguments leave
// the field out altogether, which is what a client sends for a tool that takes
// none.
func (h *harness) call(t *testing.T, tool string, args map[string]any) callToolResult {
	t.Helper()
	params := map[string]any{"name": tool}
	if args != nil {
		params["arguments"] = args
	}
	reply := h.reply(t, http.StatusOK, mcpBody(1, callToolMethod, params), nil)
	if reply.Error != nil {
		t.Fatalf("%s: %+v", tool, reply.Error)
	}
	var result callToolResult
	if err := json.Unmarshal(reply.Result, &result); err != nil {
		t.Fatalf("result: %v\n%s", err, reply.Result)
	}
	if result.ResultType != completeResult {
		t.Errorf("resultType = %q", result.ResultType)
	}
	if result.Meta.ServerInfo.Name != "godrop" {
		t.Errorf("serverInfo = %+v", result.Meta.ServerInfo)
	}
	return result
}

// text joins what a tool said, which is the part a model reads.
func (r callToolResult) text() string {
	var b strings.Builder
	for _, c := range r.Content {
		if c.Type == "text" {
			b.WriteString(c.Text)
		}
	}
	return b.String()
}

// ------------------------------------------------------------------ handshake

func TestMCPDiscoverNeedsNoHandshake(t *testing.T) {
	h := newHarness(t, nil)
	reply := h.reply(t, http.StatusOK, mcpBody("discover-1", "server/discover", nil), nil)
	if reply.Error != nil {
		t.Fatalf("error = %+v", reply.Error)
	}
	if string(reply.ID) != `"discover-1"` {
		t.Errorf("id = %s", reply.ID)
	}
	var got discoverResult
	if err := json.Unmarshal(reply.Result, &got); err != nil {
		t.Fatal(err)
	}
	switch {
	case got.ResultType != completeResult:
		t.Errorf("resultType = %q", got.ResultType)
	case len(got.SupportedVersions) != 1 || got.SupportedVersions[0] != mcpVersion:
		t.Errorf("supportedVersions = %v", got.SupportedVersions)
	case got.CacheScope != "public" || got.TTLMs <= 0:
		t.Errorf("caching hints = %q %d", got.CacheScope, got.TTLMs)
	case got.Meta.ServerInfo.Name != "godrop" || got.Meta.ServerInfo.Version != "test":
		t.Errorf("serverInfo = %+v", got.Meta.ServerInfo)
	case !strings.Contains(got.Instructions, "without a token"):
		t.Errorf("instructions = %q", got.Instructions)
	}
	// Decoding proves the field is there; only the bytes prove it is under the
	// key the specification reserves for it.
	if !strings.Contains(string(reply.Result), `"`+metaServerInfo+`"`) {
		t.Errorf("the server should name itself under %s:\n%s", metaServerInfo, reply.Result)
	}

	// The revision this speaks has no sessions, so nothing may be minted that
	// asks the client to carry one.
	resp := h.rpc(t, mcpBody(1, "server/discover", nil), nil)
	if v := resp.Header.Get("Mcp-Session-Id"); v != "" {
		t.Errorf("Mcp-Session-Id = %q, this revision has no sessions", v)
	}
}

// legacy sends a message the way a client from before the change sends one:
// no per-request metadata in the body, and none of the headers that mirror it.
func (h *harness) legacy(t *testing.T, status int, msg map[string]any, headers map[string]string) rpcReply {
	t.Helper()
	override := map[string]string{"MCP-Protocol-Version": "", "Mcp-Method": "", "Mcp-Name": ""}
	for k, v := range headers {
		override[k] = v
	}
	return h.reply(t, status, msg, override)
}

func TestMCPAnswersAClientThatStillOpensWithAHandshake(t *testing.T) {
	// Most clients are still built on a revision that begins this way, and
	// the two messages that matter are identical in both eras.
	h := newHarness(t, nil)

	reply := h.legacy(t, http.StatusOK, map[string]any{
		"jsonrpc": "2.0", "id": 1, "method": "initialize",
		"params": map[string]any{
			"protocolVersion": "2025-06-18",
			"capabilities":    map[string]any{},
			"clientInfo":      map[string]any{"name": "old-client", "version": "1"},
		},
	}, nil)
	if reply.Error != nil {
		t.Fatalf("error = %+v", reply.Error)
	}
	var initialized initializeResult
	if err := json.Unmarshal(reply.Result, &initialized); err != nil {
		t.Fatal(err)
	}
	switch {
	case initialized.ProtocolVersion != "2025-06-18":
		t.Errorf("protocolVersion = %q, a version it asked for and this server knows",
			initialized.ProtocolVersion)
	case initialized.ServerInfo.Name != "godrop":
		t.Errorf("serverInfo = %+v", initialized.ServerInfo)
	case initialized.Instructions == "":
		t.Error("the instructions are the same in both eras")
	}

	// Nothing is minted for the client to carry: the handshake is answered,
	// not remembered.
	resp := h.rpc(t, map[string]any{
		"jsonrpc": "2.0", "id": 2, "method": "initialize", "params": map[string]any{},
	}, map[string]string{"MCP-Protocol-Version": "", "Mcp-Method": ""})
	if v := resp.Header.Get("Mcp-Session-Id"); v != "" {
		t.Errorf("Mcp-Session-Id = %q, this server has no sessions in either era", v)
	}

	// The notification that follows it is accepted and dropped.
	resp = h.rpc(t, map[string]any{
		"jsonrpc": "2.0", "method": "notifications/initialized",
	}, map[string]string{"MCP-Protocol-Version": "", "Mcp-Method": ""})
	if resp.StatusCode != http.StatusAccepted {
		t.Errorf("notifications/initialized = %d, want 202", resp.StatusCode)
	}

	// From here a client of that era names its version in the header and
	// nowhere else, which is what says it is not speaking the new one.
	reply = h.legacy(t, http.StatusOK, map[string]any{
		"jsonrpc": "2.0", "id": 3, "method": "tools/list", "params": map[string]any{},
	}, map[string]string{"MCP-Protocol-Version": "2025-11-25"})
	var tools toolsListResult
	if err := json.Unmarshal(reply.Result, &tools); err != nil {
		t.Fatal(err)
	}
	if len(tools.Tools) != 3 {
		t.Fatalf("tools = %d", len(tools.Tools))
	}

	reply = h.legacy(t, http.StatusOK, map[string]any{
		"jsonrpc": "2.0", "id": 4, "method": callToolMethod,
		"params": map[string]any{"name": toolUpload, "arguments": map[string]any{
			"filename": "old.txt", "content_base64": "aGVsbG8=",
		}},
	}, nil)
	var result callToolResult
	if err := json.Unmarshal(reply.Result, &result); err != nil {
		t.Fatal(err)
	}
	if result.IsError {
		t.Fatalf("upload failed: %s", result.text())
	}
	download := h.do(t, http.MethodGet, result.Content[0].Text, "")
	defer download.Body.Close()
	if body, _ := io.ReadAll(download.Body); string(body) != "hello" {
		t.Errorf("downloaded %q", body)
	}
}

func TestMCPAnswersAVersionItDoesNotKnowWithOneItDoes(t *testing.T) {
	h := newHarness(t, nil)
	reply := h.legacy(t, http.StatusOK, map[string]any{
		"jsonrpc": "2.0", "id": 1, "method": "initialize",
		"params": map[string]any{"protocolVersion": "2024-11-05"},
	}, nil)
	var initialized initializeResult
	if err := json.Unmarshal(reply.Result, &initialized); err != nil {
		t.Fatal(err)
	}
	if initialized.ProtocolVersion != legacyVersions[0] {
		t.Errorf("protocolVersion = %q, want the newest this server knows", initialized.ProtocolVersion)
	}
}

func TestMCPAnswersTheOlderKeepalive(t *testing.T) {
	// ping is gone from the current revision, so it is answered only for the
	// clients that still send it.
	h := newHarness(t, nil)
	reply := h.legacy(t, http.StatusOK, map[string]any{
		"jsonrpc": "2.0", "id": 1, "method": "ping", "params": map[string]any{},
	}, nil)
	if reply.Error != nil || string(reply.Result) != "{}" {
		t.Errorf("result = %s, error = %+v", reply.Result, reply.Error)
	}
	h.reply(t, http.StatusNotFound, mcpBody(1, "ping", nil), nil)
}

func TestMCPPointsAnOlderClientAtTheNewerAnswer(t *testing.T) {
	h := newHarness(t, nil)
	reply := h.legacy(t, http.StatusNotFound, map[string]any{
		"jsonrpc": "2.0", "id": 1, "method": "resources/list", "params": map[string]any{},
	}, nil)
	if reply.Error == nil || reply.Error.Code != codeMethodNotFound {
		t.Fatalf("error = %+v", reply.Error)
	}
	if !strings.Contains(reply.Error.Message, mcpVersion) {
		t.Errorf("message = %q, it should name the revision that may answer differently",
			reply.Error.Message)
	}
}

func TestMCPUnknownMethodIsNotFound(t *testing.T) {
	h := newHarness(t, nil)
	// 404 rather than 400: it is what tells a client that the endpoint is
	// there but the method is not.
	reply := h.reply(t, http.StatusNotFound, mcpBody(1, "resources/list", nil), nil)
	if reply.Error == nil || reply.Error.Code != codeMethodNotFound {
		t.Fatalf("error = %+v", reply.Error)
	}
	if !strings.Contains(reply.Error.Message, "resources/list") {
		t.Errorf("message = %q", reply.Error.Message)
	}
}

// ---------------------------------------------------------------------- tools

func TestMCPToolsListDescribesThisInstance(t *testing.T) {
	h := newHarness(t, func(c *config.Config) { c.Retention = 7 * 24 * time.Hour })
	reply := h.reply(t, http.StatusOK, mcpBody(1, "tools/list", nil), nil)
	var got toolsListResult
	if err := json.Unmarshal(reply.Result, &got); err != nil {
		t.Fatal(err)
	}
	if got.CacheScope != "public" || got.TTLMs <= 0 {
		t.Errorf("caching hints = %q %d", got.CacheScope, got.TTLMs)
	}
	var names []string
	for _, tool := range got.Tools {
		names = append(names, tool.Name)
		var schema map[string]any
		if err := json.Unmarshal(tool.InputSchema, &schema); err != nil {
			t.Fatalf("%s inputSchema: %v", tool.Name, err)
		}
		if schema["type"] != "object" {
			t.Errorf("%s inputSchema type = %v", tool.Name, schema["type"])
		}
		if err := json.Unmarshal(tool.OutputSchema, &schema); err != nil {
			t.Fatalf("%s outputSchema: %v", tool.Name, err)
		}
		if tool.Title == "" || tool.Description == "" {
			t.Errorf("%s is missing its title or description", tool.Name)
		}
	}
	if want := []string{toolUpload, toolDelete, toolStats}; strings.Join(names, ",") != strings.Join(want, ",") {
		t.Errorf("tools = %v, want %v", names, want)
	}

	upload := got.Tools[0]
	for _, want := range []string{"16.0MB", h.URL + "/upload", "168h0m0s"} {
		if !strings.Contains(upload.Description, want) {
			t.Errorf("the upload tool should say %q:\n%s", want, upload.Description)
		}
	}
	if upload.Annotations.ReadOnlyHint || got.Tools[1].ReadOnly() || !got.Tools[1].Annotations.DestructiveHint {
		t.Errorf("annotations = %+v %+v", upload.Annotations, got.Tools[1].Annotations)
	}
	if !got.Tools[2].Annotations.ReadOnlyHint {
		t.Errorf("%s should be marked read only", got.Tools[2].Name)
	}
}

// ReadOnly reads the hint the way a client would when deciding whether to ask
// a person first.
func (t mcpTool) ReadOnly() bool { return t.Annotations.ReadOnlyHint }

func TestMCPToolsListIsByteIdenticalBetweenCalls(t *testing.T) {
	// A client is told it may cache this list, which is only safe while the
	// same set of tools comes back in the same order.
	h := newHarness(t, nil)
	first, _ := io.ReadAll(h.rpc(t, mcpBody(1, "tools/list", nil), nil).Body)
	second, _ := io.ReadAll(h.rpc(t, mcpBody(1, "tools/list", nil), nil).Body)
	if !bytes.Equal(first, second) {
		t.Errorf("the tool list changed between calls:\n%s\n%s", first, second)
	}
}

func TestMCPToolsListWithoutRetention(t *testing.T) {
	h := newHarness(t, nil)
	reply := h.reply(t, http.StatusOK, mcpBody(1, "tools/list", nil), nil)
	var got toolsListResult
	if err := json.Unmarshal(reply.Result, &got); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(got.Tools[0].Description, "deletes everything") {
		t.Errorf("nothing is deleted on a schedule here:\n%s", got.Tools[0].Description)
	}
}

// --------------------------------------------------------------------- upload

func TestMCPUploadReturnsAURLThatOpensWithoutAToken(t *testing.T) {
	h := newHarness(t, nil)
	result := h.call(t, toolUpload, map[string]any{
		"filename":       "checkout.png",
		"content_base64": base64.StdEncoding.EncodeToString([]byte("pretend png")),
	})
	if result.IsError {
		t.Fatalf("upload failed: %s", result.text())
	}

	var info fileInfo
	structured, _ := json.Marshal(result.StructuredContent)
	if err := json.Unmarshal(structured, &info); err != nil {
		t.Fatal(err)
	}
	switch {
	case !strings.HasPrefix(info.URL, h.URL+"/f/"):
		t.Errorf("url = %q", info.URL)
	case info.Name != "checkout.png":
		t.Errorf("name = %q", info.Name)
	case info.SizeBytes != int64(len("pretend png")):
		t.Errorf("size = %d", info.SizeBytes)
	}
	// The first thing the model reads is the URL on its own, so it can be
	// pasted without editing.
	if result.Content[0].Type != "text" || result.Content[0].Text != info.URL {
		t.Errorf("content[0] = %+v, want the bare URL", result.Content[0])
	}
	link := result.Content[1]
	if link.Type != "resource_link" || link.URI != info.URL || link.MimeType != "image/png" {
		t.Errorf("content[1] = %+v", link)
	}

	// The point of the whole thing: no token, and the bytes come back.
	resp := h.do(t, http.MethodGet, info.URL, "")
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK || string(body) != "pretend png" {
		t.Errorf("download = %d %q", resp.StatusCode, body)
	}
}

func TestMCPUploadAcceptsEveryBase64AnAgentMightSend(t *testing.T) {
	h := newHarness(t, nil)
	raw := []byte{0xfb, 0xff, 0xbe, 0x01, 0x02}
	std := base64.StdEncoding.EncodeToString(raw)
	for name, content := range map[string]string{
		"padded":   std,
		"unpadded": strings.TrimRight(std, "="),
		"url safe": base64.URLEncoding.EncodeToString(raw),
		"wrapped":  std[:4] + "\r\n" + std[4:] + "\n",
		"data url": "data:image/png;base64," + std,
	} {
		t.Run(name, func(t *testing.T) {
			result := h.call(t, toolUpload, map[string]any{
				"filename": "a.bin", "content_base64": content,
			})
			if result.IsError {
				t.Fatalf("upload failed: %s", result.text())
			}
			resp := h.do(t, http.MethodGet, result.Content[0].Text, "")
			defer resp.Body.Close()
			if body, _ := io.ReadAll(resp.Body); !bytes.Equal(body, raw) {
				t.Errorf("stored % x, want % x", body, raw)
			}
		})
	}
}

func TestMCPUploadCanExpireOnItsOwn(t *testing.T) {
	h := newHarness(t, nil)
	result := h.call(t, toolUpload, map[string]any{
		"filename": "invoice.pdf", "content_base64": "aGk=", "expires_in": " 7d ",
	})
	var info fileInfo
	structured, _ := json.Marshal(result.StructuredContent)
	if err := json.Unmarshal(structured, &info); err != nil {
		t.Fatal(err)
	}
	if info.ExpiresAt == "" {
		t.Fatalf("expires_at is missing: %s", structured)
	}
	if !strings.Contains(result.text(), info.ExpiresAt) {
		t.Errorf("the model should be told when the link dies:\n%s", result.text())
	}
	at, err := time.Parse(time.RFC3339, info.ExpiresAt)
	if err != nil || time.Until(at) < 6*24*time.Hour {
		t.Errorf("expires_at = %q (%v)", info.ExpiresAt, err)
	}
}

func TestMCPUploadReportsWhatTheModelCanFix(t *testing.T) {
	h := newHarness(t, nil)
	for name, tc := range map[string]struct {
		args map[string]any
		want string
	}{
		"no name":     {map[string]any{"content_base64": "aGk="}, `"filename" is required`},
		"blank name":  {map[string]any{"filename": "  ", "content_base64": "aGk="}, `"filename" is required`},
		"no content":  {map[string]any{"filename": "a.png"}, `"content_base64" is required`},
		"not base64":  {map[string]any{"filename": "a.png", "content_base64": "!!!!"}, "not valid base64"},
		"bad expiry":  {map[string]any{"filename": "a.png", "content_base64": "aGk=", "expires_in": "soon"}, "invalid expiry"},
		"past expiry": {map[string]any{"filename": "a.png", "content_base64": "aGk=", "expires_in": "-1h"}, "in the future"},
	} {
		t.Run(name, func(t *testing.T) {
			// A mistake the model can correct comes back as a result it can
			// read, not as a protocol error it cannot act on.
			result := h.call(t, toolUpload, tc.args)
			if !result.IsError || !strings.Contains(result.text(), tc.want) {
				t.Errorf("isError = %v, text = %q, want %q", result.IsError, result.text(), tc.want)
			}
		})
	}
}

func TestMCPUploadPointsOversizeFilesAtTheStreamingEndpoint(t *testing.T) {
	h := newHarness(t, func(c *config.Config) { c.MaxFileSize = 1 << 10 })
	result := h.call(t, toolUpload, map[string]any{
		"filename":       "big.bin",
		"content_base64": base64.StdEncoding.EncodeToString(bytes.Repeat([]byte("x"), 2000)),
	})
	if !result.IsError {
		t.Fatal("an oversize file should be reported to the model")
	}
	for _, want := range []string{"1.0KB", "/upload"} {
		if !strings.Contains(result.text(), want) {
			t.Errorf("text = %q, want %q", result.text(), want)
		}
	}
}

func TestMCPUploadReportsAFullDisk(t *testing.T) {
	h := newHarness(t, func(c *config.Config) { c.MaxTotalSize = 8 })
	result := h.call(t, toolUpload, map[string]any{
		"filename":       "a.txt",
		"content_base64": base64.StdEncoding.EncodeToString([]byte("more than eight bytes")),
	})
	if !result.IsError || !strings.Contains(result.text(), "out of storage") {
		t.Errorf("isError = %v, text = %q", result.IsError, result.text())
	}
}

func TestMCPUploadReportsAStorageFailureAsAProtocolError(t *testing.T) {
	requireStrictPermissions(t)
	h := newHarness(t, nil)
	if err := os.Chmod(h.store.Root(), 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(h.store.Root(), 0o700) })

	// Nothing the model does differently will fix a disk it cannot write to,
	// so this one is an error rather than a result.
	reply := h.reply(t, http.StatusInternalServerError, mcpBody(1, callToolMethod, map[string]any{
		"name": toolUpload,
		"arguments": map[string]any{
			"filename": "a.txt", "content_base64": "aGk=",
		},
	}), nil)
	if reply.Error == nil || reply.Error.Code != codeInternal {
		t.Fatalf("error = %+v", reply.Error)
	}
}

// --------------------------------------------------------------------- delete

func TestMCPDeleteTakesTheURLTheUploadReturned(t *testing.T) {
	h := newHarness(t, nil)
	uploaded := h.uploadOK(t, [2]string{"photo.jpg", "bytes"})
	url := uploaded.Files[0].URL

	result := h.call(t, toolDelete, map[string]any{"url": url})
	if result.IsError {
		t.Fatalf("delete failed: %s", result.text())
	}
	var got mcpDeleted
	structured, _ := json.Marshal(result.StructuredContent)
	if err := json.Unmarshal(structured, &got); err != nil || !got.Deleted {
		t.Fatalf("structuredContent = %s (%v)", structured, err)
	}
	resp := h.do(t, http.MethodGet, url, "")
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("the file is still there: %d", resp.StatusCode)
	}

	// Asking twice says so rather than pretending.
	again := h.call(t, toolDelete, map[string]any{"url": url})
	if !again.IsError || !strings.Contains(again.text(), "already") {
		t.Errorf("isError = %v, text = %q", again.IsError, again.text())
	}
}

func TestMCPDeleteAcceptsTheStoredNameOnItsOwn(t *testing.T) {
	h := newHarness(t, nil)
	uploaded := h.uploadOK(t, [2]string{"photo.jpg", "bytes"})
	name := storedName(t, uploaded.Files[0].URL)

	if result := h.call(t, toolDelete, map[string]any{"url": name}); result.IsError {
		t.Fatalf("delete failed: %s", result.text())
	}
}

func TestMCPDeleteRejectsSomethingThatIsNotAURLFromHere(t *testing.T) {
	h := newHarness(t, nil)
	result := h.call(t, toolDelete, map[string]any{"url": "https://example.com/cat.png"})
	if !result.IsError || !strings.Contains(result.text(), "not a URL from this service") {
		t.Errorf("isError = %v, text = %q", result.IsError, result.text())
	}
}

func TestMCPDeleteReportsAStorageFailure(t *testing.T) {
	requireStrictPermissions(t)
	h := newHarness(t, nil)
	uploaded := h.uploadOK(t, [2]string{"photo.jpg", "bytes"})
	id, ext := storage.SplitName(storedName(t, uploaded.Files[0].URL))
	path, err := h.store.Path(id, ext)
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Dir(path)
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })

	reply := h.reply(t, http.StatusInternalServerError, mcpBody(1, callToolMethod, map[string]any{
		"name":      toolDelete,
		"arguments": map[string]any{"url": uploaded.Files[0].URL},
	}), nil)
	if reply.Error == nil || reply.Error.Code != codeInternal {
		t.Fatalf("error = %+v", reply.Error)
	}
}

func TestMCPFileRefReadsEveryShapeOfURL(t *testing.T) {
	id := "20260815-143022-8f4e2c91b7934b38a72d4c5e6f708192"
	for name, tc := range map[string]struct {
		in       string
		wantID   string
		wantExt  string
		wantOK   bool
		wantSlug bool
	}{
		"cosmetic url": {in: "https://files.example.com/f/" + id + "/checkout.png", wantID: id, wantExt: "png", wantOK: true},
		"short url":    {in: "https://files.example.com/f/" + id + ".png", wantID: id, wantExt: "png", wantOK: true},
		"path only":    {in: "/f/" + id + "/checkout.png", wantID: id, wantExt: "png", wantOK: true},
		"stored name":  {in: id + ".png", wantID: id, wantExt: "png", wantOK: true},
		"upper case":   {in: "/f/" + id + "/Checkout.PNG", wantID: id, wantExt: "png", wantOK: true},
		// A file stored without an extension is reached the same way, which is
		// why an empty one is an answer rather than a refusal.
		"no extension": {in: "/f/" + id + "/checkout", wantID: id, wantOK: true},
		"unstorable":   {in: "/f/" + id + "/checkout.abcdefghijk", wantOK: false},
		"not an id":    {in: "https://example.com/cat.png", wantOK: false},
		"empty":        {in: "", wantOK: false},
		"nonsense":     {in: "://", wantOK: false},
	} {
		t.Run(name, func(t *testing.T) {
			gotID, gotExt, ok := mcpFileRef(tc.in)
			if ok != tc.wantOK || gotID != tc.wantID || gotExt != tc.wantExt {
				t.Errorf("mcpFileRef(%q) = %q, %q, %v", tc.in, gotID, gotExt, ok)
			}
		})
	}
}

// ---------------------------------------------------------------------- stats

func TestMCPStatsCountsWhatIsStored(t *testing.T) {
	h := newHarness(t, nil)
	h.uploadOK(t, [2]string{"a.txt", "12345"})

	result := h.call(t, toolStats, nil)
	var got mcpUsage
	structured, _ := json.Marshal(result.StructuredContent)
	if err := json.Unmarshal(structured, &got); err != nil {
		t.Fatal(err)
	}
	switch {
	case got.Files != 1 || got.Bytes != 5:
		t.Errorf("usage = %+v", got)
	case got.MaxUploadBytes != mcpMaxUpload:
		t.Errorf("max_upload_bytes = %d", got.MaxUploadBytes)
	case got.QuotaBytes != 0 || got.Retention != "":
		t.Errorf("this instance has neither a quota nor a retention period: %+v", got)
	}
	if !strings.Contains(result.text(), "1 files") {
		t.Errorf("text = %q", result.text())
	}
}

func TestMCPStatsNamesTheLimitsWhenThereAreSome(t *testing.T) {
	h := newHarness(t, func(c *config.Config) {
		c.MaxTotalSize = 20 << 30
		c.Retention = 7 * 24 * time.Hour
	})
	result := h.call(t, toolStats, map[string]any{})
	for _, want := range []string{"Quota 20.0GB", "deleted 168h0m0s"} {
		if !strings.Contains(result.text(), want) {
			t.Errorf("text = %q, want %q", result.text(), want)
		}
	}
	var got mcpUsage
	structured, _ := json.Marshal(result.StructuredContent)
	if err := json.Unmarshal(structured, &got); err != nil || got.Retention != "168h0m0s" {
		t.Errorf("usage = %s (%v)", structured, err)
	}
}

// ----------------------------------------------------------------- validation

func TestMCPRejectsAHeaderThatDisagreesWithTheBody(t *testing.T) {
	// An intermediary routes on the header while the server acts on the body,
	// so the two saying different things is a request nobody should serve.
	h := newHarness(t, nil)
	call := func() map[string]any {
		return mcpBody(1, callToolMethod, map[string]any{"name": toolStats, "arguments": map[string]any{}})
	}
	for name, tc := range map[string]struct {
		msg      map[string]any
		override map[string]string
		want     string
	}{
		"method lies":     {mcpBody(1, "tools/list", nil), map[string]string{"Mcp-Method": "tools/call"}, "Mcp-Method"},
		"method missing":  {mcpBody(1, "tools/list", nil), map[string]string{"Mcp-Method": ""}, "Mcp-Method"},
		"name lies":       {call(), map[string]string{"Mcp-Name": "upload_file"}, "Mcp-Name"},
		"name missing":    {call(), map[string]string{"Mcp-Name": ""}, "Mcp-Name"},
		"version missing": {mcpBody(1, "tools/list", nil), map[string]string{"MCP-Protocol-Version": ""}, "MCP-Protocol-Version"},
		"version lies":    {mcpBody(1, "tools/list", nil), map[string]string{"MCP-Protocol-Version": "2025-11-25"}, "MCP-Protocol-Version"},
	} {
		t.Run(name, func(t *testing.T) {
			reply := h.reply(t, http.StatusBadRequest, tc.msg, tc.override)
			if reply.Error == nil || reply.Error.Code != codeHeaderMismatch {
				t.Fatalf("error = %+v, want %d", reply.Error, codeHeaderMismatch)
			}
			if !strings.Contains(reply.Error.Message, tc.want) {
				t.Errorf("message = %q, want %q", reply.Error.Message, tc.want)
			}
		})
	}
}

func TestMCPAcceptsANameEncodedForTheHeader(t *testing.T) {
	// A tool name that cannot be written in a header travels base64 encoded,
	// and has to be decoded before it is compared with the body.
	h := newHarness(t, nil)
	encoded := base64SentinelStart + base64.StdEncoding.EncodeToString([]byte(toolStats)) + base64SentinelEnd
	reply := h.reply(t, http.StatusOK, mcpBody(1, callToolMethod, map[string]any{
		"name": toolStats, "arguments": map[string]any{},
	}), map[string]string{"Mcp-Name": encoded})
	if reply.Error != nil {
		t.Fatalf("error = %+v", reply.Error)
	}
}

func TestDecodeHeaderValue(t *testing.T) {
	encoded := base64.StdEncoding.EncodeToString([]byte("Şubat.pdf"))
	for name, tc := range map[string]struct{ in, want string }{
		"plain":       {"upload_file", "upload_file"},
		"encoded":     {base64SentinelStart + encoded + base64SentinelEnd, "Şubat.pdf"},
		"no end":      {base64SentinelStart + encoded, base64SentinelStart + encoded},
		"not base64":  {base64SentinelStart + "!!!" + base64SentinelEnd, base64SentinelStart + "!!!" + base64SentinelEnd},
		"looks alike": {"=?base16?ab?=", "=?base16?ab?="},
	} {
		t.Run(name, func(t *testing.T) {
			if got := decodeHeaderValue(tc.in); got != tc.want {
				t.Errorf("decodeHeaderValue(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestMCPRequiresTheMetadataThatReplacedTheHandshake(t *testing.T) {
	h := newHarness(t, nil)
	for name, params := range map[string]map[string]any{
		"no version":      {metaCapabilities: map[string]any{}},
		"no capabilities": {metaVersion: mcpVersion},
	} {
		t.Run(name, func(t *testing.T) {
			msg := map[string]any{
				"jsonrpc": "2.0", "id": 1, "method": "tools/list",
				"params": map[string]any{"_meta": params},
			}
			reply := h.reply(t, http.StatusBadRequest, msg, nil)
			if reply.Error == nil || reply.Error.Code != codeInvalidParams {
				t.Fatalf("error = %+v, want %d", reply.Error, codeInvalidParams)
			}
		})
	}
}

func TestMCPNamesTheVersionsItSupports(t *testing.T) {
	h := newHarness(t, nil)
	msg := map[string]any{
		"jsonrpc": "2.0", "id": 1, "method": "tools/list",
		"params": map[string]any{"_meta": map[string]any{
			metaVersion: "2025-11-25", metaCapabilities: map[string]any{},
		}},
	}
	reply := h.reply(t, http.StatusBadRequest, msg,
		map[string]string{"MCP-Protocol-Version": "2025-11-25"})
	if reply.Error == nil || reply.Error.Code != codeUnsupportedVer {
		t.Fatalf("error = %+v, want %d", reply.Error, codeUnsupportedVer)
	}
	data, _ := reply.Error.Data.(map[string]any)
	versions, _ := data["supported"].([]any)
	if len(versions) != 1 || versions[0] != mcpVersion || data["requested"] != "2025-11-25" {
		t.Errorf("data = %+v", reply.Error.Data)
	}
}

func TestMCPRejectsMessagesThatAreNotJSONRPC(t *testing.T) {
	h := newHarness(t, nil)

	resp := h.rpcRaw(t, []byte("{not json"), nil)
	body, _ := io.ReadAll(resp.Body)
	var reply rpcReply
	if err := json.Unmarshal(body, &reply); err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusBadRequest || reply.Error.Code != codeParse {
		t.Errorf("status = %d, error = %+v", resp.StatusCode, reply.Error)
	}

	for name, msg := range map[string]map[string]any{
		"wrong version": {"jsonrpc": "1.0", "id": 1, "method": "tools/list"},
		"null id":       {"jsonrpc": "2.0", "id": nil, "method": "tools/list"},
		"no method":     {"jsonrpc": "2.0", "id": 1},
	} {
		t.Run(name, func(t *testing.T) {
			// A null id is not the same as no id: the first is a broken
			// request, the second is a notification.
			raw, _ := json.Marshal(msg)
			if name == "null id" {
				raw = []byte(`{"jsonrpc":"2.0","id":null,"method":"tools/list"}`)
			}
			resp := h.rpcRaw(t, raw, nil)
			body, _ := io.ReadAll(resp.Body)
			var reply rpcReply
			if err := json.Unmarshal(body, &reply); err != nil {
				t.Fatal(err)
			}
			if resp.StatusCode != http.StatusBadRequest || reply.Error.Code != codeInvalidRequest {
				t.Errorf("status = %d, error = %+v", resp.StatusCode, reply.Error)
			}
		})
	}
}

func TestMCPAcceptsAndDropsNotifications(t *testing.T) {
	h := newHarness(t, nil)
	resp := h.rpc(t, mcpBody(nil, "notifications/cancelled", nil), nil)
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusAccepted || len(body) != 0 {
		t.Errorf("status = %d, body = %q, want 202 and nothing", resp.StatusCode, body)
	}
}

func TestMCPCallNeedsArgumentsShapedLikeArguments(t *testing.T) {
	h := newHarness(t, nil)
	for _, tool := range []string{toolUpload, toolDelete, toolStats} {
		t.Run(tool, func(t *testing.T) {
			reply := h.reply(t, http.StatusBadRequest, mcpBody(1, callToolMethod, map[string]any{
				"name": tool, "arguments": []string{"a"},
			}), nil)
			if reply.Error == nil || reply.Error.Code != codeInvalidParams {
				t.Fatalf("error = %+v", reply.Error)
			}
		})
	}
}

func TestMCPRejectsAToolItDoesNotHave(t *testing.T) {
	h := newHarness(t, nil)
	reply := h.reply(t, http.StatusBadRequest, mcpBody(1, callToolMethod, map[string]any{
		"name": "rm_rf", "arguments": map[string]any{},
	}), nil)
	if reply.Error == nil || !strings.Contains(reply.Error.Message, "rm_rf") {
		t.Fatalf("error = %+v", reply.Error)
	}
}

func TestMCPBoundsTheMessageItWillHold(t *testing.T) {
	// The bytes arrive base64 encoded inside JSON that has to be parsed whole,
	// so the message is held in memory and the limit is what keeps that small.
	h := newHarness(t, func(c *config.Config) { c.MaxFileSize = 1 << 10 })
	if got := h.srv.mcpUploadLimit(); got != 1<<10 {
		t.Fatalf("mcpUploadLimit = %d, want the instance's own limit", got)
	}
	resp := h.rpcRaw(t, bytes.Repeat([]byte("x"), int(h.srv.mcpMaxBody())+1), nil)
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want 413: %s", resp.StatusCode, body)
	}
	if !strings.Contains(string(body), "/upload") {
		t.Errorf("the answer should point at the endpoint that streams: %s", body)
	}
}

// ---------------------------------------------------------------------- guard

func TestMCPNeedsAToken(t *testing.T) {
	h := newHarness(t, nil)
	resp := h.rpc(t, mcpBody(1, "tools/list", nil), map[string]string{"Authorization": ""})
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", resp.StatusCode)
	}
	if got := resp.Header.Get("WWW-Authenticate"); !strings.Contains(got, "Bearer") {
		t.Errorf("WWW-Authenticate = %q", got)
	}
}

func TestMCPTurnsAwayAPageThatIsNotAllowedHere(t *testing.T) {
	// A website that points its own name at 127.0.0.1 can make a browser send
	// this request; the origin is what says the request is not the client's.
	h := newHarness(t, func(c *config.Config) { c.CORSOrigins = []string{"https://app.example.com"} })

	resp := h.rpc(t, mcpBody(1, "tools/list", nil), map[string]string{"Origin": "https://evil.example"})
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %d, want 403: %s", resp.StatusCode, body)
	}
	if !strings.Contains(string(body), "evil.example") {
		t.Errorf("body = %s", body)
	}

	for name, origin := range map[string]string{
		"configured":  "https://app.example.com",
		"same origin": "http://" + strings.TrimPrefix(h.URL, "http://"),
	} {
		t.Run(name, func(t *testing.T) {
			resp := h.rpc(t, mcpBody(1, "tools/list", nil), map[string]string{"Origin": origin})
			if resp.StatusCode != http.StatusOK {
				t.Errorf("status = %d, want 200", resp.StatusCode)
			}
		})
	}
}

func TestMCPRefusesTheVerbsOfTheOlderTransport(t *testing.T) {
	// GET opened a stream and DELETE ended a session; this revision has
	// neither, and says so rather than looking like a broken endpoint.
	h := newHarness(t, nil)
	for _, method := range []string{http.MethodGet, http.MethodDelete} {
		resp := h.do(t, method, h.URL+"/mcp", testToken)
		resp.Body.Close()
		if resp.StatusCode != http.StatusMethodNotAllowed {
			t.Errorf("%s /mcp = %d, want 405", method, resp.StatusCode)
		}
	}
}

func TestDecodeContentRejectsWhatIsNotBase64(t *testing.T) {
	if _, err := decodeContent("not base64 at all!"); err == nil {
		t.Error("that is not base64")
	}
	if got, err := decodeContent("data:text/plain,hello"); err == nil {
		t.Errorf("a data URL that is not base64 = % x", got)
	}
}
