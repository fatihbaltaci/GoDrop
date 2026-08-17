// Package server implements GoDrop's HTTP API: authenticated uploads and
// deletes, public downloads, health probes and the machine-readable
// descriptions that let an AI agent discover the API from the base URL alone.
package server

import (
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime"
	"mime/multipart"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/fatihbaltaci/GoDrop/internal/config"
	"github.com/fatihbaltaci/GoDrop/internal/storage"
	"github.com/fatihbaltaci/GoDrop/internal/tokens"
)

//go:embed assets/openapi.yaml
var openAPISpec string

// Server wires configuration, storage and tokens into an http.Handler.
type Server struct {
	cfg     *config.Config
	store   *storage.Store
	tokens  *tokens.Store
	log     *slog.Logger
	version string

	uploadLimiter *limiter
	authLimiter   *limiter

	started time.Time
	now     func() time.Time
	handler http.Handler
}

// Options configures New.
type Options struct {
	Config  *config.Config
	Store   *storage.Store
	Tokens  *tokens.Store
	Logger  *slog.Logger
	Version string
	Now     func() time.Time
}

// New builds a Server. The returned value is ready to serve.
func New(opts Options) *Server {
	now := opts.Now
	if now == nil {
		now = func() time.Time { return time.Now().UTC() }
	}
	log := opts.Logger
	if log == nil {
		log = slog.New(slog.NewJSONHandler(io.Discard, nil))
	}
	s := &Server{
		cfg:           opts.Config,
		store:         opts.Store,
		tokens:        opts.Tokens,
		log:           log,
		version:       opts.Version,
		uploadLimiter: newLimiter(opts.Config.RateLimit, now),
		authLimiter:   newLimiter(opts.Config.AuthRateLimit, now),
		started:       now(),
		now:           now,
	}
	s.handler = s.routes()
	return s
}

// ServeHTTP makes Server an http.Handler.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) { s.handler.ServeHTTP(w, r) }

func (s *Server) routes() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /{$}", s.handleUsage)
	mux.HandleFunc("GET /llms.txt", s.handleLLMs)
	mux.HandleFunc("GET /openapi.yaml", s.handleOpenAPI)
	mux.HandleFunc("GET /skill.md", s.handleSkill)

	mux.HandleFunc("POST /upload", s.protect(s.handleUpload))
	mux.HandleFunc("PUT /upload/{name}", s.protect(s.handlePutUpload))

	mux.HandleFunc("GET /f/{name}", s.handleDownload)
	mux.HandleFunc("GET /f/{id}/{name}", s.handleDownload)
	mux.HandleFunc("DELETE /f/{name}", s.protect(s.handleDelete))
	mux.HandleFunc("DELETE /f/{id}/{name}", s.protect(s.handleDelete))

	mux.HandleFunc("POST /mcp", s.mcpOrigin(s.protect(s.handleMCP)))

	mux.HandleFunc("GET /healthz", s.handleHealth)
	mux.HandleFunc("GET /readyz", s.handleReady)
	mux.HandleFunc("GET /stats", s.protect(s.handleStats))

	var h http.Handler = mux
	h = s.corsMiddleware(h)
	if s.cfg.AccessLog {
		h = s.logMiddleware(h)
	}
	return s.recoverMiddleware(h)
}

// ---------------------------------------------------------------- middleware

type statusWriter struct {
	http.ResponseWriter
	status int
	bytes  int64
}

func (w *statusWriter) WriteHeader(code int) {
	if w.status == 0 {
		w.status = code
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *statusWriter) Write(b []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	n, err := w.ResponseWriter.Write(b)
	w.bytes += int64(n)
	return n, err
}

func (s *Server) logMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := s.now()
		sw := &statusWriter{ResponseWriter: w}
		next.ServeHTTP(sw, r)
		if sw.status == 0 {
			sw.status = http.StatusOK
		}
		s.log.Info("request",
			"method", r.Method,
			"path", LogPath(r.URL.Path),
			"status", sw.status,
			"bytes", sw.bytes,
			"ip", clientIP(r),
			"dur_ms", s.now().Sub(start).Milliseconds(),
		)
	})
}

func (s *Server) recoverMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if rec := recover(); rec != nil {
				s.log.Error("panic", "path", LogPath(r.URL.Path), "panic", fmt.Sprint(rec))
				writeError(w, http.StatusInternalServerError, "internal error")
			}
		}()
		next.ServeHTTP(w, r)
	})
}

func (s *Server) corsMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		origin := r.Header.Get("Origin")
		if allowed, value := s.allowedOrigin(origin); allowed {
			w.Header().Set("Access-Control-Allow-Origin", value)
			if value != "*" {
				w.Header().Add("Vary", "Origin")
			}
			w.Header().Set("Access-Control-Allow-Methods", "GET, HEAD, POST, PUT, DELETE, OPTIONS")
			w.Header().Set("Access-Control-Allow-Headers", "Authorization, Content-Type, X-API-Key")
			w.Header().Set("Access-Control-Expose-Headers", "Content-Disposition, ETag, Location")
			w.Header().Set("Access-Control-Max-Age", "86400")
		}
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) allowedOrigin(origin string) (bool, string) {
	for _, o := range s.cfg.CORSOrigins {
		if o == "*" {
			return true, "*"
		}
		if origin != "" && strings.EqualFold(o, origin) {
			return true, origin
		}
	}
	return false, ""
}

// protect requires a valid token, applies both rate limiters and records the
// authenticated token name for logging.
func (s *Server) protect(next func(http.ResponseWriter, *http.Request, string)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		presented := bearerToken(r)
		name, ok := s.tokens.Verify(presented)
		if !ok {
			ip := clientIP(r)
			if allowed, retry := s.authLimiter.allow(ip); !allowed {
				s.log.Warn("auth rate limited", "ip", ip, "path", LogPath(r.URL.Path))
				retryAfter(w, retry)
				writeError(w, http.StatusTooManyRequests, "too many failed authentication attempts")
				return
			}
			s.log.Warn("auth failed", "ip", ip, "path", LogPath(r.URL.Path), "has_credentials", presented != "")
			w.Header().Set("WWW-Authenticate", `Bearer realm="godrop"`)
			writeError(w, http.StatusUnauthorized, "missing or invalid token")
			return
		}
		if allowed, retry := s.uploadLimiter.allow(name); !allowed {
			retryAfter(w, retry)
			writeError(w, http.StatusTooManyRequests, "rate limit exceeded")
			return
		}
		next(w, r, name)
	}
}

func retryAfter(w http.ResponseWriter, d time.Duration) {
	secs := int(d.Seconds())
	if secs < 1 {
		secs = 1
	}
	w.Header().Set("Retry-After", strconv.Itoa(secs))
}

// bearerToken extracts credentials from either header form.
func bearerToken(r *http.Request) string {
	if h := r.Header.Get("Authorization"); h != "" {
		if v, ok := strings.CutPrefix(h, "Bearer "); ok {
			return strings.TrimSpace(v)
		}
		if v, ok := strings.CutPrefix(h, "bearer "); ok {
			return strings.TrimSpace(v)
		}
		return ""
	}
	return strings.TrimSpace(r.Header.Get("X-API-Key"))
}

// clientIP resolves the caller's address. X-Forwarded-For is honoured only when
// the immediate peer is loopback or private, that is, a local reverse proxy.
// A client on the public internet cannot forge its way past the rate limiter.
func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	if !isTrustedPeer(host) {
		return host
	}
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		first, _, _ := strings.Cut(xff, ",")
		if first = strings.TrimSpace(first); first != "" {
			return first
		}
	}
	if direct := strings.TrimSpace(r.Header.Get("X-Real-IP")); direct != "" {
		return direct
	}
	return host
}

func isTrustedPeer(host string) bool {
	ip := net.ParseIP(host)
	if ip == nil {
		return host == "" // httptest requests without RemoteAddr
	}
	return ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsUnspecified()
}

func requestScheme(r *http.Request) string {
	if proto := r.Header.Get("X-Forwarded-Proto"); proto != "" {
		first, _, _ := strings.Cut(proto, ",")
		return strings.TrimSpace(first)
	}
	if r.TLS != nil {
		return "https"
	}
	return "http"
}

// ------------------------------------------------------------------ handlers

// fileInfo is what an upload returns about one file.
//
// Only what the URL does not already say. The identifier, the extension and
// therefore the media type are all in the URL, so repeating them is noise a
// client has to read past. The name is not: the cosmetic part of the URL is a
// slug, and "Yaz Raporu 2026.pdf" cannot be recovered from
// "yaz-raporu-2026.pdf", which is what tells a caller which answer belongs to
// which file. The size is the only account of what actually arrived, and the
// expiry appears when one was asked for.
type fileInfo struct {
	URL       string `json:"url"`
	Name      string `json:"name,omitempty"`
	SizeBytes int64  `json:"size_bytes"`
	ExpiresAt string `json:"expires_at,omitempty"`
}

// uploadResponse is one shape whatever was sent: a list, in the order the
// files were sent. There is no top-level URL duplicating the first entry,
// because "the first one" is an arbitrary answer to "which one?"; the URL of
// a single upload is in the Location header as well, where HTTP puts the
// address of something just created.
type uploadResponse struct {
	Files []fileInfo `json:"files"`
}

func (s *Server) handleUpload(w http.ResponseWriter, r *http.Request, token string) {
	ct := r.Header.Get("Content-Type")
	mediaType, params, err := mime.ParseMediaType(ct)
	if err != nil || !strings.HasPrefix(mediaType, "multipart/") {
		writeError(w, http.StatusUnsupportedMediaType,
			`expected multipart/form-data with a "file" field; use PUT /upload/{name} to send a raw body`)
		return
	}
	boundary, ok := params["boundary"]
	if !ok {
		writeError(w, http.StatusBadRequest, "malformed multipart request: missing boundary")
		return
	}

	// Bound the whole request, not just each part: a single request must not be
	// able to stream unlimited bytes through many small parts.
	maxBody := s.cfg.MaxFileSize*int64(s.cfg.MaxFilesPerRequest) + 1<<20
	body := http.MaxBytesReader(w, r.Body, maxBody)
	mr := multipart.NewReader(body, boundary)

	var (
		created []*storage.File
		infos   []fileInfo
	)
	rollback := func() {
		for _, f := range created {
			if err := s.store.Delete(f.ID, f.Ext); err != nil {
				s.log.Error("rollback failed", "id", ShortID(f.ID), "err", err.Error())
			}
		}
	}

	for {
		part, err := mr.NextPart()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			rollback()
			// A body the multipart reader cannot parse is the client's mistake,
			// including headers with control characters, so report 400 rather
			// than blaming the server.
			var maxErr *http.MaxBytesError
			if errors.As(err, &maxErr) {
				s.writeUploadError(w, err)
				return
			}
			writeError(w, http.StatusBadRequest, "malformed multipart request: "+err.Error())
			return
		}
		if part.FileName() == "" && part.FormName() != "file" {
			_ = part.Close()
			continue // ordinary form field, ignore
		}
		if len(created) >= s.cfg.MaxFilesPerRequest {
			_ = part.Close()
			rollback()
			writeError(w, http.StatusBadRequest,
				fmt.Sprintf("too many files: at most %d per request", s.cfg.MaxFilesPerRequest))
			return
		}
		info, file, err := s.storePart(r, part.FileName(), part)
		_ = part.Close()
		if err != nil {
			rollback()
			s.writeUploadError(w, err)
			return
		}
		created = append(created, file)
		infos = append(infos, info)
	}

	if len(infos) == 0 {
		writeError(w, http.StatusBadRequest, `no file found: send a multipart field named "file"`)
		return
	}
	for _, f := range created {
		s.log.Info("upload", "id", ShortID(f.ID), "size", f.Size, "token", token, "ip", clientIP(r))
	}
	writeCreated(w, infos)
}

func (s *Server) handlePutUpload(w http.ResponseWriter, r *http.Request, token string) {
	name := r.PathValue("name")
	body := http.MaxBytesReader(w, r.Body, s.cfg.MaxFileSize+1)
	info, file, err := s.storePart(r, name, body)
	if err != nil {
		s.writeUploadError(w, err)
		return
	}
	s.log.Info("upload", "id", ShortID(file.ID), "size", file.Size, "token", token, "ip", clientIP(r))
	writeCreated(w, []fileInfo{info})
}

// writeCreated answers an upload: 201, the list, and Location pointing at the
// first file, which is the whole answer when only one was sent.
func writeCreated(w http.ResponseWriter, files []fileInfo) {
	if len(files) > 0 {
		w.Header().Set("Location", files[0].URL)
	}
	writeJSON(w, http.StatusCreated, uploadResponse{Files: files})
}

// storePart writes one uploaded stream and builds its public description.
func (s *Server) storePart(r *http.Request, filename string, body io.Reader) (fileInfo, *storage.File, error) {
	expires, err := s.expiryFor(r)
	if err != nil {
		return fileInfo{}, nil, err
	}
	return s.storeFile(r, filename, body, s.cfg.MaxFileSize, expires)
}

// storeFile is the part every way in shares: whatever asked for the upload has
// already worked out how long the file should live and how big it may be.
func (s *Server) storeFile(r *http.Request, filename string, body io.Reader, maxSize int64, expires time.Time) (fileInfo, *storage.File, error) {
	ext := SanitizeExt(filename)
	file, err := s.store.CreateWithExpiry(ext, body, maxSize, expires)
	if err != nil {
		return fileInfo{}, nil, err
	}
	slug := SanitizeSlug(filename, ext)
	path := "/f/" + file.Name()
	if slug != "" {
		path = "/f/" + file.ID + "/" + slug
	}
	info := fileInfo{
		URL:       s.cfg.PublicURL(requestScheme(r), r.Host, path),
		Name:      strings.TrimSpace(filename),
		SizeBytes: file.Size,
	}
	if at, ok := storage.ExpiresAt(file.ID); ok {
		info.ExpiresAt = at.Format(time.RFC3339)
	}
	return info, file, nil
}

// expiryFor reads the expiry an upload asked for, from the X-Expires-In
// header or an ?expires= parameter: "30m", "12h", "7d".
//
// Retention is a maximum, not a suggestion, so an upload cannot ask to live
// longer than the server keeps anything.
func (s *Server) expiryFor(r *http.Request) (time.Time, error) {
	raw := strings.TrimSpace(r.Header.Get("X-Expires-In"))
	if raw == "" {
		raw = strings.TrimSpace(r.URL.Query().Get("expires"))
	}
	return s.expiryFrom(raw)
}

// expiryFrom turns a requested lifetime into the moment the file goes away.
// An empty request means the file lives as long as the instance keeps
// anything.
func (s *Server) expiryFrom(raw string) (time.Time, error) {
	if raw = strings.TrimSpace(raw); raw == "" {
		return time.Time{}, nil
	}
	d, err := config.ParseDuration(raw)
	switch {
	case err != nil:
		return time.Time{}, fmt.Errorf("%w: %s", errBadExpiry, err)
	case d <= 0:
		return time.Time{}, fmt.Errorf("%w: it has to be in the future", errBadExpiry)
	}
	if s.cfg.Retention > 0 && d > s.cfg.Retention {
		d = s.cfg.Retention
	}
	return s.now().Add(d), nil
}

// errBadExpiry is a client mistake, not a server failure.
var errBadExpiry = errors.New("invalid expiry")

func (s *Server) writeUploadError(w http.ResponseWriter, err error) {
	var maxErr *http.MaxBytesError
	switch {
	case errors.Is(err, storage.ErrTooLarge), errors.As(err, &maxErr):
		writeError(w, http.StatusRequestEntityTooLarge,
			fmt.Sprintf("file exceeds the %s limit", config.FormatSize(s.cfg.MaxFileSize)))
	case errors.Is(err, storage.ErrQuotaExceeded):
		writeError(w, http.StatusInsufficientStorage, "storage quota exceeded")
	case errors.Is(err, errBadExpiry):
		writeError(w, http.StatusBadRequest,
			err.Error()+`; use a duration such as "30m", "12h" or "7d"`)
	default:
		s.log.Error("upload failed", "err", err.Error())
		writeError(w, http.StatusInternalServerError, "could not store file")
	}
}

func (s *Server) handleDownload(w http.ResponseWriter, r *http.Request) {
	id, ext, name, ok := s.resolve(r)
	// An expired file is gone as far as anyone asking is concerned, whether
	// or not the sweep has caught up with it yet.
	if ok && s.store.Expired(id) {
		ok = false
	}
	if !ok {
		writeError(w, http.StatusNotFound, "not found")
		return
	}
	f, info, err := s.store.Open(id, ext)
	if err != nil {
		writeError(w, http.StatusNotFound, "not found")
		return
	}
	defer f.Close()

	contentType := ContentType(ext, nil)
	disposition := "inline"
	if IsDangerousExt(ext) || r.URL.Query().Get("dl") != "" {
		disposition = "attachment"
	}

	h := w.Header()
	h.Set("Content-Type", contentType)
	h.Set("Content-Disposition", mime.FormatMediaType(disposition, map[string]string{"filename": name}))
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Content-Security-Policy", "default-src 'none'; sandbox")
	h.Set("Referrer-Policy", "no-referrer")
	h.Set("Cache-Control", s.cacheControl(id))
	h.Set("ETag", `"`+id+`"`)

	http.ServeContent(w, r, name, info.ModTime(), f)
}

// cacheControl decides how long a download may be kept by everything between
// here and whoever reads it: the browser, a CDN, a company's proxy.
//
// The bytes at a URL never change, so caching them is free performance, and
// that is what the setting is for. An upload that expires is a different
// promise: a cache told to keep it for a year would go on serving it long
// after this server stopped, so it may only be held until the moment it goes
// away, and never as immutable, which is a client's permission not to ask
// again.
//
// What this cannot do is reach a copy already taken. A URL that has been
// published is served from caches nobody here controls, GitHub's image proxy
// among them, which is why deleting one is a request rather than a guarantee.
func (s *Server) cacheControl(id string) string {
	age := s.cfg.CacheMaxAge
	expires, expiring := storage.ExpiresAt(id)
	if expiring {
		if left := expires.Sub(s.now()); left < age {
			age = left
		}
	}
	if age <= 0 {
		return "no-store"
	}
	value := fmt.Sprintf("public, max-age=%d", int64(age.Seconds()))
	if expiring {
		return value
	}
	return value + ", immutable"
}

func (s *Server) handleDelete(w http.ResponseWriter, r *http.Request, token string) {
	id, ext, _, ok := s.resolve(r)
	if !ok {
		writeError(w, http.StatusNotFound, "not found")
		return
	}
	switch err := s.store.Delete(id, ext); {
	case err == nil:
		s.log.Info("delete", "id", ShortID(id), "token", token, "ip", clientIP(r))
		w.WriteHeader(http.StatusNoContent)
	case errors.Is(err, storage.ErrNotFound), errors.Is(err, storage.ErrInvalidID):
		writeError(w, http.StatusNotFound, "not found")
	default:
		s.log.Error("delete failed", "err", err.Error())
		writeError(w, http.StatusInternalServerError, "could not delete file")
	}
}

// resolve accepts both URL shapes: /f/<id>.<ext> and /f/<id>/<cosmetic name>.
//
// In the second form the extension is taken from the cosmetic name, so a link
// that renames a stored JPEG into "setup.exe" looks for <id>.exe on disk, finds
// nothing and gets a 404. The browser therefore can never be handed a file
// under an extension it was not stored with.
func (s *Server) resolve(r *http.Request) (id, ext, name string, ok bool) {
	if pathID := r.PathValue("id"); pathID != "" {
		name = r.PathValue("name")
		if !storage.ValidID(pathID) {
			return "", "", "", false
		}
		_, ext = storage.SplitName(name)
		ext = strings.ToLower(ext)
		if !storage.ValidExt(ext) {
			return "", "", "", false
		}
		return pathID, ext, name, true
	}
	stored := r.PathValue("name")
	id, ext, ok = SplitStoredName(stored)
	if !ok {
		return "", "", "", false
	}
	return id, ext, storage.JoinName(id, ext), true
}

func (s *Server) handleHealth(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"status": "ok", "version": s.version})
}

func (s *Server) handleReady(w http.ResponseWriter, _ *http.Request) {
	if err := s.store.Writable(); err != nil {
		s.log.Error("readiness probe failed", "err", err.Error())
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{
			"status": "storage not writable",
			"error":  err.Error(),
		})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": "ready"})
}

func (s *Server) handleStats(w http.ResponseWriter, _ *http.Request, _ string) {
	files, bytes := s.store.Stats()
	writeJSON(w, http.StatusOK, map[string]any{
		"files":       files,
		"bytes":       bytes,
		"bytes_human": config.FormatSize(bytes),
		"quota_bytes": s.cfg.MaxTotalSize,
		"max_file":    s.cfg.MaxFileSize,
		"uptime_s":    int64(s.now().Sub(s.started).Seconds()),
		"version":     s.version,
	})
}

func (s *Server) handleOpenAPI(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/yaml; charset=utf-8")
	_, _ = io.WriteString(w, openAPISpec)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}
