package server

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/fatihbaltaci/GoDrop/internal/config"
	"github.com/fatihbaltaci/GoDrop/internal/storage"
)

// The Model Context Protocol endpoint, for agents that cannot run a shell.
//
// Revision 2026-07-28 took the sessions out of MCP: there is no initialize
// handshake, no session header and no long-lived stream, and every request
// carries its own protocol version, capabilities and identity. What is left is
// a JSON-RPC request in an HTTP POST, answered and forgotten, which is the same
// shape as the rest of this service. That is why the protocol is written out
// here rather than pulled in: the part of it a file host needs is this file.

const (
	// mcpVersion is the revision this endpoint is built on: the one that took
	// the sessions out of the protocol.
	mcpVersion = "2026-07-28"

	// The keys MCP reserves inside _meta.
	metaVersion      = "io.modelcontextprotocol/protocolVersion"
	metaCapabilities = "io.modelcontextprotocol/clientCapabilities"
	metaServerInfo   = "io.modelcontextprotocol/serverInfo"
)

// JSON-RPC error codes, plus the two from the range the MCP specification
// reserves for itself (-32020 to -32099).
const (
	codeParse           = -32700
	codeInvalidRequest  = -32600
	codeMethodNotFound  = -32601
	codeInvalidParams   = -32602
	codeInternal        = -32603
	codeHeaderMismatch  = -32020
	codeUnsupportedVer  = -32022
	discoverTTLMs       = 3_600_000
	toolsTTLMs          = 300_000
	completeResult      = "complete"
	toolUpload          = "upload_file"
	toolDelete          = "delete_file"
	toolStats           = "storage_stats"
	callToolMethod      = "tools/call"
	base64SentinelStart = "=?base64?"
	base64SentinelEnd   = "?="
)

// legacyVersions are the revisions from before the protocol went stateless,
// which is still what most clients are built on. They open with a handshake
// and never send per-request metadata. The newest is first: it is the answer
// to a client that asks for a version this server does not know.
var legacyVersions = []string{"2025-11-25", "2025-06-18", "2025-03-26"}

// mcpMaxUpload caps what one tool call may store. JSON carries the bytes
// base64 encoded and the whole message has to be parsed before any of it can
// be written, so the message is held in memory; bounding that is worth more
// than the ability to push a disk image through an agent. Anything larger has
// POST /upload, which streams and never holds the file.
const mcpMaxUpload = 16 << 20

// ------------------------------------------------------------------- messages

type mcpRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Method  string          `json:"method"`
	Params  mcpParams       `json:"params"`
}

type mcpParams struct {
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments"`
	Meta      mcpRequestMeta  `json:"_meta"`
	// ProtocolVersion is where a client from before the change puts the same
	// thing: in the parameters of the handshake, once, instead of on every
	// request.
	ProtocolVersion string `json:"protocolVersion"`
}

type mcpRequestMeta struct {
	Version string `json:"io.modelcontextprotocol/protocolVersion"`
	// Capabilities is raw because the requirement is that the client declared
	// them at all, not what is in them: a server may not use a capability it
	// was not told about, and this endpoint uses none.
	Capabilities json.RawMessage `json:"io.modelcontextprotocol/clientCapabilities"`
}

type mcpResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Result  any             `json:"result,omitempty"`
	Error   *mcpError       `json:"error,omitempty"`
}

type mcpError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    any    `json:"data,omitempty"`
}

// mcpMeta is what every result carries: who answered it.
type mcpMeta struct {
	ServerInfo mcpImplementation `json:"io.modelcontextprotocol/serverInfo"`
}

type mcpImplementation struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

type discoverResult struct {
	ResultType        string          `json:"resultType"`
	SupportedVersions []string        `json:"supportedVersions"`
	Capabilities      mcpCapabilities `json:"capabilities"`
	Instructions      string          `json:"instructions"`
	TTLMs             int64           `json:"ttlMs"`
	CacheScope        string          `json:"cacheScope"`
	Meta              mcpMeta         `json:"_meta"`
}

// mcpCapabilities lists what this server has. Tools, and deliberately nothing
// else: resources would need a listing endpoint, which this service does not
// have by design, and prompts would describe work it does not do.
type mcpCapabilities struct {
	Tools struct{} `json:"tools"`
}

// initializeResult is the handshake a client from the older era expects, and
// the only place this endpoint answers in that shape.
type initializeResult struct {
	ProtocolVersion string            `json:"protocolVersion"`
	Capabilities    mcpCapabilities   `json:"capabilities"`
	ServerInfo      mcpImplementation `json:"serverInfo"`
	Instructions    string            `json:"instructions"`
}

type toolsListResult struct {
	ResultType string    `json:"resultType"`
	Tools      []mcpTool `json:"tools"`
	TTLMs      int64     `json:"ttlMs"`
	CacheScope string    `json:"cacheScope"`
	Meta       mcpMeta   `json:"_meta"`
}

type mcpTool struct {
	Name         string          `json:"name"`
	Title        string          `json:"title"`
	Description  string          `json:"description"`
	InputSchema  json.RawMessage `json:"inputSchema"`
	OutputSchema json.RawMessage `json:"outputSchema,omitempty"`
	Annotations  mcpAnnotations  `json:"annotations"`
}

// mcpAnnotations are hints about what calling a tool does to the world. They
// are what a client shows a person before asking whether to allow the call.
type mcpAnnotations struct {
	ReadOnlyHint    bool `json:"readOnlyHint"`
	DestructiveHint bool `json:"destructiveHint"`
	IdempotentHint  bool `json:"idempotentHint"`
	OpenWorldHint   bool `json:"openWorldHint"`
}

type callToolResult struct {
	ResultType        string       `json:"resultType"`
	Content           []mcpContent `json:"content"`
	StructuredContent any          `json:"structuredContent,omitempty"`
	IsError           bool         `json:"isError"`
	Meta              mcpMeta      `json:"_meta"`
}

type mcpContent struct {
	Type     string `json:"type"`
	Text     string `json:"text,omitempty"`
	URI      string `json:"uri,omitempty"`
	Name     string `json:"name,omitempty"`
	MimeType string `json:"mimeType,omitempty"`
}

// mcpUsage is what storage_stats answers with.
type mcpUsage struct {
	Files          int64  `json:"files"`
	Bytes          int64  `json:"bytes"`
	BytesHuman     string `json:"bytes_human"`
	QuotaBytes     int64  `json:"quota_bytes"`
	MaxFileBytes   int64  `json:"max_file_bytes"`
	MaxUploadBytes int64  `json:"max_upload_bytes"`
	Retention      string `json:"retention,omitempty"`
}

// mcpDeleted is what delete_file answers with.
type mcpDeleted struct {
	Deleted bool   `json:"deleted"`
	URL     string `json:"url"`
}

// -------------------------------------------------------------------- limits

// mcpUploadLimit is the largest file a tool call may store: this instance's
// own limit, but never more than one message may hold.
func (s *Server) mcpUploadLimit() int64 {
	if s.cfg.MaxFileSize < mcpMaxUpload {
		return s.cfg.MaxFileSize
	}
	return mcpMaxUpload
}

// mcpMaxBody is that limit as it arrives on the wire: base64 spends four bytes
// on every three, and the JSON around it needs room of its own.
func (s *Server) mcpMaxBody() int64 { return s.mcpUploadLimit()/3*4 + 4 + 64<<10 }

// --------------------------------------------------------------------- guard

// mcpOrigin turns away a browser page that has no business here. The transport
// requires the check: without it a page on any website could reach a GoDrop
// bound to localhost by pointing its own name at 127.0.0.1. The list is the
// one CORS uses, because an operator who has already said which origins may
// call this service has answered this question too.
func (s *Server) mcpOrigin(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		origin := r.Header.Get("Origin")
		if origin != "" && !s.originAllowed(origin, r.Host) {
			rpcError(w, http.StatusForbidden, nil, mcpError{
				Code:    codeInvalidRequest,
				Message: "origin " + origin + " is not allowed to reach this endpoint",
			})
			return
		}
		next(w, r)
	}
}

func (s *Server) originAllowed(origin, host string) bool {
	if allowed, _ := s.allowedOrigin(origin); allowed {
		return true
	}
	u, err := url.Parse(origin)
	return err == nil && u.Host != "" && strings.EqualFold(u.Host, host)
}

// ------------------------------------------------------------------ dispatch

func (s *Server) handleMCP(w http.ResponseWriter, r *http.Request, token string) {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, s.mcpMaxBody()))
	if err != nil {
		rpcError(w, http.StatusRequestEntityTooLarge, nil, mcpError{
			Code: codeInvalidRequest,
			Message: fmt.Sprintf("the message is larger than this endpoint holds: %s takes files up to %s, and POST /upload takes the rest",
				toolUpload, config.FormatSize(s.mcpUploadLimit())),
		})
		return
	}

	var req mcpRequest
	if err := json.Unmarshal(body, &req); err != nil {
		rpcError(w, http.StatusBadRequest, nil, mcpError{Code: codeParse, Message: err.Error()})
		return
	}

	// A message with no id is a notification: nothing this endpoint does
	// depends on one, so it is accepted and dropped rather than argued with.
	if len(req.ID) == 0 {
		w.WriteHeader(http.StatusAccepted)
		return
	}
	switch {
	case req.JSONRPC != "2.0":
		rpcError(w, http.StatusBadRequest, nil, mcpError{
			Code: codeInvalidRequest, Message: `"jsonrpc" must be "2.0"`})
		return
	case string(req.ID) == "null":
		rpcError(w, http.StatusBadRequest, nil, mcpError{
			Code: codeInvalidRequest, Message: `"id" must not be null`})
		return
	case req.Method == "":
		rpcError(w, http.StatusBadRequest, req.ID, mcpError{
			Code: codeInvalidRequest, Message: `"method" is required`})
		return
	}

	// Which era the client belongs to is a property of the request, not of a
	// connection: a modern one carries its protocol version in the body, and
	// says so in the header even when the body is malformed. Everything else
	// came from before the handshake was removed.
	if req.Params.Meta.Version == "" && r.Header.Get("MCP-Protocol-Version") != mcpVersion {
		s.handleLegacyMCP(w, r, &req, token)
		return
	}

	if bad := validateMCP(r, &req); bad != nil {
		rpcError(w, http.StatusBadRequest, req.ID, *bad)
		return
	}

	base := s.cfg.PublicURL(requestScheme(r), r.Host, "")
	switch req.Method {
	case "server/discover":
		rpcResult(w, req.ID, discoverResult{
			ResultType:        completeResult,
			SupportedVersions: []string{mcpVersion},
			Instructions:      s.mcpInstructions(),
			TTLMs:             discoverTTLMs,
			CacheScope:        "public",
			Meta:              s.mcpMeta(),
		})
	case "tools/list":
		rpcResult(w, req.ID, s.toolsList(base))
	case callToolMethod:
		s.callTool(w, r, &req, token)
	default:
		// The transport asks for 404 here so that a client can tell an
		// unimplemented method apart from an endpoint that is not there.
		rpcError(w, http.StatusNotFound, req.ID, mcpError{
			Code: codeMethodNotFound, Message: "unknown method: " + req.Method})
	}
}

func (s *Server) toolsList(base string) toolsListResult {
	return toolsListResult{
		ResultType: completeResult,
		Tools:      s.mcpTools(base),
		TTLMs:      toolsTTLMs,
		CacheScope: "public",
		Meta:       s.mcpMeta(),
	}
}

// handleLegacyMCP answers the revisions that came before the protocol went
// stateless.
//
// Supporting them costs almost nothing and is what makes the endpoint usable
// today: the two messages that matter, tools/list and tools/call, are the same
// in both eras, and the difference is a handshake at the front. What this
// server does not do is grow state to hold it. No session is ever assigned, so
// there is nothing for a client to carry, resume or tear down, and the answer
// to the second request is worked out the same way as the answer to the first.
func (s *Server) handleLegacyMCP(w http.ResponseWriter, r *http.Request, req *mcpRequest, token string) {
	switch req.Method {
	case "initialize":
		rpcResult(w, req.ID, initializeResult{
			ProtocolVersion: negotiateLegacy(req.Params.ProtocolVersion),
			Capabilities:    mcpCapabilities{},
			ServerInfo:      mcpImplementation{Name: "godrop", Version: s.version},
			Instructions:    s.mcpInstructions(),
		})
	case "ping":
		rpcResult(w, req.ID, struct{}{})
	case "tools/list":
		rpcResult(w, req.ID, s.toolsList(s.cfg.PublicURL(requestScheme(r), r.Host, "")))
	case callToolMethod:
		s.callTool(w, r, req, token)
	default:
		rpcError(w, http.StatusNotFound, req.ID, mcpError{
			Code: codeMethodNotFound,
			Message: "unknown method: " + req.Method + "; this server also speaks MCP " +
				mcpVersion + ", where the answer may be different"})
	}
}

// negotiateLegacy answers with the version the client asked for when it is one
// this server knows, and with the newest one it knows when it is not. A client
// that cannot live with the answer is expected to say so and disconnect.
func negotiateLegacy(requested string) string {
	for _, v := range legacyVersions {
		if v == requested {
			return v
		}
	}
	return legacyVersions[0]
}

// validateMCP applies the transport's rule that a header may never disagree
// with the body it describes: an intermediary routing on the header and a
// server acting on the body must not be able to see two different requests.
func validateMCP(r *http.Request, req *mcpRequest) *mcpError {
	if got := r.Header.Get("Mcp-Method"); got != req.Method {
		return headerMismatch("Mcp-Method", got, req.Method)
	}
	if req.Method == callToolMethod {
		if got := decodeHeaderValue(r.Header.Get("Mcp-Name")); got != req.Params.Name {
			return headerMismatch("Mcp-Name", got, req.Params.Name)
		}
	}
	version := r.Header.Get("MCP-Protocol-Version")
	switch {
	case version == "":
		return &mcpError{Code: codeHeaderMismatch,
			Message: "the MCP-Protocol-Version header is required; send " + mcpVersion}
	case req.Params.Meta.Version == "":
		return &mcpError{Code: codeInvalidParams,
			Message: `params._meta["` + metaVersion + `"] is required`}
	case req.Params.Meta.Capabilities == nil:
		return &mcpError{Code: codeInvalidParams,
			Message: `params._meta["` + metaCapabilities + `"] is required, even when it is empty`}
	case version != req.Params.Meta.Version:
		return headerMismatch("MCP-Protocol-Version", version, req.Params.Meta.Version)
	case version != mcpVersion:
		return &mcpError{
			Code:    codeUnsupportedVer,
			Message: "unsupported protocol version",
			Data:    map[string]any{"supported": []string{mcpVersion}, "requested": version},
		}
	}
	return nil
}

func headerMismatch(header, got, want string) *mcpError {
	if got == "" {
		return &mcpError{Code: codeHeaderMismatch,
			Message: fmt.Sprintf("the %s header is required; this request needs %q", header, want)}
	}
	return &mcpError{Code: codeHeaderMismatch,
		Message: fmt.Sprintf("the %s header says %q where the body says %q", header, got, want)}
}

// decodeHeaderValue undoes the escape the transport defines for values that
// cannot be written as plain ASCII: =?base64?<encoded>?=
func decodeHeaderValue(v string) string {
	rest, ok := strings.CutPrefix(v, base64SentinelStart)
	if !ok {
		return v
	}
	rest, ok = strings.CutSuffix(rest, base64SentinelEnd)
	if !ok {
		return v
	}
	decoded, err := base64.StdEncoding.DecodeString(rest)
	if err != nil {
		return v
	}
	return string(decoded)
}

func (s *Server) mcpMeta() mcpMeta {
	return mcpMeta{ServerInfo: mcpImplementation{Name: "godrop", Version: s.version}}
}

func rpcResult(w http.ResponseWriter, id json.RawMessage, result any) {
	writeJSON(w, http.StatusOK, mcpResponse{JSONRPC: "2.0", ID: id, Result: result})
}

func rpcError(w http.ResponseWriter, status int, id json.RawMessage, e mcpError) {
	writeJSON(w, status, mcpResponse{JSONRPC: "2.0", ID: id, Error: &e})
}

// --------------------------------------------------------------------- tools

func (s *Server) mcpInstructions() string {
	return "GoDrop stores a file and hands back a URL that opens without a token, " +
		"which is how a file gets somewhere that has no way to receive one: a pull " +
		"request comment, an issue, a chat message. Upload the file, paste the URL. " +
		"Keep it if the file may need deleting later, because identifiers are " +
		"unguessable and there is no way to list what is stored."
}

func (s *Server) mcpTools(base string) []mcpTool {
	upload := fmt.Sprintf(
		"Store a file here and get back a public URL. The URL needs no token, so it "+
			"renders inline wherever it is pasted, which is how a screenshot reaches a "+
			"GitHub pull request: GitHub has no API for attaching one. Up to %s per "+
			"call; send anything larger to POST %s/upload, which streams.",
		config.FormatSize(s.mcpUploadLimit()), base)
	if s.cfg.Retention > 0 {
		upload += fmt.Sprintf(" This instance deletes everything %s after upload.", s.cfg.Retention)
	}

	// The order is fixed: a client caches this list, and the same list in the
	// same order is what lets it stay cached.
	return []mcpTool{
		{
			Name:         toolUpload,
			Title:        "Upload a file",
			Description:  upload,
			InputSchema:  json.RawMessage(uploadSchema),
			OutputSchema: json.RawMessage(uploadOutputSchema),
			Annotations:  mcpAnnotations{},
		},
		{
			Name:  toolDelete,
			Title: "Delete an uploaded file",
			Description: "Remove a file this instance is hosting. Pass the URL that " + toolUpload +
				" returned. The server stops answering for it at once, but a URL that has already been " +
				"shared may still come from a cache, so this undoes storing a file rather than publishing it.",
			InputSchema:  json.RawMessage(deleteSchema),
			OutputSchema: json.RawMessage(deleteOutputSchema),
			Annotations:  mcpAnnotations{DestructiveHint: true, IdempotentHint: true},
		},
		{
			Name:         toolStats,
			Title:        "Storage usage",
			Description:  "How much this instance is holding and which limits it enforces. Worth a look before a large upload.",
			InputSchema:  json.RawMessage(statsSchema),
			OutputSchema: json.RawMessage(statsOutputSchema),
			Annotations:  mcpAnnotations{ReadOnlyHint: true},
		},
	}
}

const uploadSchema = `{
  "type": "object",
  "properties": {
    "filename": {
      "type": "string",
      "description": "Name of the file, with its extension, such as \"checkout.png\". The extension decides the media type the URL is served with."
    },
    "content_base64": {
      "type": "string",
      "description": "The bytes of the file, base64 encoded. A data: URL is accepted too."
    },
    "expires_in": {
      "type": "string",
      "description": "Optional lifetime such as \"30m\", \"12h\" or \"7d\". The server deletes the file and stops answering for it when it runs out, and tells caches to hold it no longer than that. Leave it out to keep the file for as long as this instance keeps anything."
    }
  },
  "required": ["filename", "content_base64"],
  "additionalProperties": false
}`

const uploadOutputSchema = `{
  "type": "object",
  "properties": {
    "url": { "type": "string", "description": "The public URL. It opens without a token." },
    "name": { "type": "string", "description": "The file name as it was sent." },
    "size_bytes": { "type": "integer", "description": "What was actually stored." },
    "expires_at": { "type": "string", "description": "RFC 3339, present only when the upload expires." }
  },
  "required": ["url", "size_bytes"]
}`

const deleteSchema = `{
  "type": "object",
  "properties": {
    "url": { "type": "string", "description": "The URL an upload returned." }
  },
  "required": ["url"],
  "additionalProperties": false
}`

const deleteOutputSchema = `{
  "type": "object",
  "properties": {
    "deleted": { "type": "boolean" },
    "url": { "type": "string" }
  },
  "required": ["deleted", "url"]
}`

const statsSchema = `{
  "type": "object",
  "additionalProperties": false
}`

const statsOutputSchema = `{
  "type": "object",
  "properties": {
    "files": { "type": "integer" },
    "bytes": { "type": "integer" },
    "bytes_human": { "type": "string" },
    "quota_bytes": { "type": "integer", "description": "0 when the instance has no quota." },
    "max_file_bytes": { "type": "integer", "description": "The limit POST /upload enforces." },
    "max_upload_bytes": { "type": "integer", "description": "The largest file the upload tool can take." },
    "retention": { "type": "string", "description": "How long files are kept. Absent when they are kept forever." }
  },
  "required": ["files", "bytes", "quota_bytes", "max_file_bytes", "max_upload_bytes"]
}`

// ---------------------------------------------------------------- tool calls

func (s *Server) callTool(w http.ResponseWriter, r *http.Request, req *mcpRequest, token string) {
	args := req.Params.Arguments
	if len(args) == 0 {
		args = json.RawMessage("{}")
	}
	var result *callToolResult
	var bad *mcpError
	switch req.Params.Name {
	case toolUpload:
		result, bad = s.mcpUpload(r, args, token)
	case toolDelete:
		result, bad = s.mcpDelete(r, args, token)
	case toolStats:
		result, bad = s.mcpStats(args)
	default:
		bad = &mcpError{Code: codeInvalidParams, Message: "unknown tool: " + req.Params.Name}
	}
	if bad != nil {
		status := http.StatusBadRequest
		if bad.Code == codeInternal {
			status = http.StatusInternalServerError
		}
		rpcError(w, status, req.ID, *bad)
		return
	}
	result.ResultType = completeResult
	result.Meta = s.mcpMeta()
	rpcResult(w, req.ID, result)
}

// toolError is the answer to a mistake the model can correct on its own: a
// name it forgot, a duration it invented, a file that is too big. It is a
// result rather than an error so that the text reaches the model that has to
// act on it.
func toolError(format string, a ...any) *callToolResult {
	return &callToolResult{
		Content: []mcpContent{{Type: "text", Text: fmt.Sprintf(format, a...)}},
		IsError: true,
	}
}

// badArguments is the answer to a request that is not shaped like a call at
// all, which no amount of retrying the same way will fix.
func badArguments(tool string, err error) *mcpError {
	return &mcpError{Code: codeInvalidParams,
		Message: fmt.Sprintf("%s: arguments must be an object (%s)", tool, err)}
}

func (s *Server) mcpUpload(r *http.Request, args json.RawMessage, token string) (*callToolResult, *mcpError) {
	var in struct {
		Filename  string `json:"filename"`
		Content   string `json:"content_base64"`
		ExpiresIn string `json:"expires_in"`
	}
	if err := json.Unmarshal(args, &in); err != nil {
		return nil, badArguments(toolUpload, err)
	}
	in.Filename = strings.TrimSpace(in.Filename)
	switch {
	case in.Filename == "":
		return toolError(`"filename" is required, with an extension: "checkout.png".`), nil
	case in.Content == "":
		return toolError(`"content_base64" is required: the bytes of the file, base64 encoded.`), nil
	}
	data, err := decodeContent(in.Content)
	if err != nil {
		return toolError(`"content_base64" is not valid base64 (%s).`, err), nil
	}
	expires, err := s.expiryFrom(in.ExpiresIn)
	if err != nil {
		return toolError(`%s; use a duration such as "30m", "12h" or "7d".`, err), nil
	}

	info, file, err := s.storeFile(r, in.Filename, bytes.NewReader(data), s.mcpUploadLimit(), expires)
	switch {
	case errors.Is(err, storage.ErrTooLarge):
		return toolError("%s is %s, which is over the %s this tool takes. Upload it with POST %s/upload instead.",
			in.Filename, config.FormatSize(int64(len(data))), config.FormatSize(s.mcpUploadLimit()),
			s.cfg.PublicURL(requestScheme(r), r.Host, "")), nil
	case errors.Is(err, storage.ErrQuotaExceeded):
		return toolError("this instance is out of storage. Delete something, or raise GODROP_MAX_TOTAL_SIZE."), nil
	case err != nil:
		s.log.Error("mcp upload failed", "err", err.Error())
		return nil, &mcpError{Code: codeInternal, Message: "the file could not be stored"}
	}
	s.log.Info("upload", "id", ShortID(file.ID), "size", file.Size, "token", token, "ip", clientIP(r))

	text := info.URL
	if info.ExpiresAt != "" {
		// What the server can promise is that it stops answering. A copy a
		// cache took while the link was live is not this server's to recall.
		text += "\n\nThe server stops serving this at " + info.ExpiresAt + "."
	}
	return &callToolResult{
		Content: []mcpContent{
			{Type: "text", Text: text},
			{Type: "resource_link", URI: info.URL, Name: info.Name, MimeType: ContentType(file.Ext, nil)},
		},
		StructuredContent: info,
	}, nil
}

func (s *Server) mcpDelete(r *http.Request, args json.RawMessage, token string) (*callToolResult, *mcpError) {
	var in struct {
		URL string `json:"url"`
	}
	if err := json.Unmarshal(args, &in); err != nil {
		return nil, badArguments(toolDelete, err)
	}
	id, ext, ok := mcpFileRef(in.URL)
	if !ok {
		return toolError("%q is not a URL from this service. Pass the one the upload returned, "+
			"such as %s/f/20260815-143022-8f4e.../checkout.png.",
			in.URL, s.cfg.PublicURL(requestScheme(r), r.Host, "")), nil
	}
	switch err := s.store.Delete(id, ext); {
	case err == nil:
		s.log.Info("delete", "id", ShortID(id), "token", token, "ip", clientIP(r))
		return &callToolResult{
			Content: []mcpContent{{Type: "text", Text: "Deleted from the server. " +
				"A URL that has already been shared may still be served for a while " +
				"by a cache, so treat anything that went up as public."}},
			StructuredContent: mcpDeleted{Deleted: true, URL: in.URL},
		}, nil
	case errors.Is(err, storage.ErrNotFound), errors.Is(err, storage.ErrInvalidID):
		return toolError("nothing is stored at that URL. It may have been deleted already, or expired."), nil
	default:
		s.log.Error("mcp delete failed", "err", err.Error())
		return nil, &mcpError{Code: codeInternal, Message: "the file could not be deleted"}
	}
}

func (s *Server) mcpStats(args json.RawMessage) (*callToolResult, *mcpError) {
	var in struct{}
	if err := json.Unmarshal(args, &in); err != nil {
		return nil, badArguments(toolStats, err)
	}
	files, bytes := s.store.Stats()
	usage := mcpUsage{
		Files:          files,
		Bytes:          bytes,
		BytesHuman:     config.FormatSize(bytes),
		QuotaBytes:     s.cfg.MaxTotalSize,
		MaxFileBytes:   s.cfg.MaxFileSize,
		MaxUploadBytes: s.mcpUploadLimit(),
	}
	text := fmt.Sprintf("%d files, %s stored.", files, config.FormatSize(bytes))
	if s.cfg.MaxTotalSize > 0 {
		text += fmt.Sprintf(" Quota %s.", config.FormatSize(s.cfg.MaxTotalSize))
	}
	if s.cfg.Retention > 0 {
		usage.Retention = s.cfg.Retention.String()
		text += fmt.Sprintf(" Files are deleted %s after upload.", s.cfg.Retention)
	}
	text += fmt.Sprintf(" This tool takes files up to %s.", config.FormatSize(s.mcpUploadLimit()))
	return &callToolResult{
		Content:           []mcpContent{{Type: "text", Text: text}},
		StructuredContent: usage,
	}, nil
}

// decodeContent reads the bytes of a file out of a JSON string.
//
// Every base64 dialect an agent might reach for is accepted, because which one
// it picked is not a thing worth failing an upload over: padded or not, the
// URL-safe alphabet or the standard one, wrapped across lines or in one piece,
// and a data: URL with the media type still on the front.
func decodeContent(v string) ([]byte, error) {
	if rest, ok := strings.CutPrefix(v, "data:"); ok {
		if i := strings.Index(rest, ";base64,"); i >= 0 {
			v = rest[i+len(";base64,"):]
		}
	}
	v = strings.Map(func(r rune) rune {
		switch r {
		case ' ', '\t', '\r', '\n':
			return -1
		case '-':
			return '+'
		case '_':
			return '/'
		}
		return r
	}, v)
	return base64.RawStdEncoding.DecodeString(strings.TrimRight(v, "="))
}

// mcpFileRef finds the file a delete refers to. What an upload returned is a
// URL, and that is what a model has; a bare stored name is accepted too, since
// it is what someone reading a log would copy.
func mcpFileRef(raw string) (id, ext string, ok bool) {
	raw = strings.TrimSpace(raw)
	if u, err := url.Parse(raw); err == nil && u.Path != "" {
		raw = u.Path
	}
	parts := strings.Split(strings.Trim(raw, "/"), "/")
	if n := len(parts); n >= 2 && storage.ValidID(parts[n-2]) {
		// The cosmetic form, /f/<id>/<name>: the extension comes from the name,
		// exactly as the download route reads the same URL.
		_, ext = storage.SplitName(parts[n-1])
		ext = strings.ToLower(ext)
		if !storage.ValidExt(ext) {
			return "", "", false
		}
		return parts[n-2], ext, true
	}
	return SplitStoredName(parts[len(parts)-1])
}
