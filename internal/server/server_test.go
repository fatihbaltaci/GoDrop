package server

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/fatihbaltaci/GoDrop/internal/config"
	"github.com/fatihbaltaci/GoDrop/internal/skill"
	"github.com/fatihbaltaci/GoDrop/internal/storage"
	"github.com/fatihbaltaci/GoDrop/internal/tokens"
)

const testToken = "test-token-with-enough-entropy"

type harness struct {
	*httptest.Server
	srv    *Server
	cfg    *config.Config
	store  *storage.Store
	tokens *tokens.Store
	dir    string
	logs   *bytes.Buffer
}

// handler exposes the routed handler for requests that the http.Client would
// normalise away, such as a URL containing "..".
func (h *harness) handler() http.Handler { return h.srv }

// panicHandler wraps a handler that always panics in the real recovery
// middleware, which is the only way to reach that path deliberately.
func (h *harness) serverForPanic() http.Handler {
	return h.srv.recoverMiddleware(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		panic("boom")
	}))
}

// newHarness builds a real server over a temporary data directory. Tests drive
// it through HTTP so they exercise routing, middleware and handlers together.
func newHarness(t *testing.T, tweak func(*config.Config)) *harness {
	t.Helper()
	dir := t.TempDir()
	cfg, err := config.LoadFrom(func(key string) string {
		switch key {
		case "GODROP_TOKENS":
			return testToken
		case "GODROP_DATA_DIR":
			return dir
		}
		return ""
	})
	if err != nil {
		t.Fatalf("config: %v", err)
	}
	if tweak != nil {
		tweak(cfg)
	}
	store, err := storage.New(cfg.DataDir, cfg.MaxTotalSize)
	if err != nil {
		t.Fatalf("storage: %v", err)
	}
	tokenStore, err := tokens.New(tokens.Path(dir), cfg.Tokens)
	if err != nil {
		t.Fatalf("tokens: %v", err)
	}
	logs := &bytes.Buffer{}
	srv := New(Options{
		Config:  cfg,
		Store:   store,
		Tokens:  tokenStore,
		Logger:  slog.New(slog.NewJSONHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug})),
		Version: "test",
	})
	ts := httptest.NewServer(srv)
	t.Cleanup(ts.Close)
	return &harness{Server: ts, srv: srv, cfg: cfg, store: store, tokens: tokenStore, dir: dir, logs: logs}
}

// upload posts files as multipart form data, exactly as curl -F does.
func (h *harness) upload(t *testing.T, token string, files ...[2]string) *http.Response {
	t.Helper()
	var body bytes.Buffer
	w := multipart.NewWriter(&body)
	for _, f := range files {
		part, err := w.CreateFormFile("file", f[0])
		if err != nil {
			t.Fatal(err)
		}
		if _, err := io.WriteString(part, f[1]); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	req, err := http.NewRequest(http.MethodPost, h.URL+"/upload", &body)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", w.FormDataContentType())
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := h.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func (h *harness) uploadOK(t *testing.T, files ...[2]string) uploadResponse {
	t.Helper()
	resp := h.upload(t, testToken, files...)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("upload = %d: %s", resp.StatusCode, body)
	}
	var out uploadResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return out
}

func (h *harness) do(t *testing.T, method, url, token string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(method, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := h.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func decodeError(t *testing.T, resp *http.Response) string {
	t.Helper()
	var out struct {
		Error string `json:"error"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode error body: %v", err)
	}
	return out.Error
}

// ------------------------------------------------------------------- uploads

func TestUploadAnswersWithOneEntryPerFile(t *testing.T) {
	h := newHarness(t, nil)
	got := h.uploadOK(t, [2]string{"Yaz Tatili 2026.jpg", "photo bytes"})

	if len(got.Files) != 1 {
		t.Fatalf("files = %d, want 1", len(got.Files))
	}
	f := got.Files[0]
	if f.Name != "Yaz Tatili 2026.jpg" {
		t.Errorf("name = %q, want the original file name", f.Name)
	}
	if f.ExpiresAt != "" {
		t.Errorf("expires_at = %q, want nothing when no expiry was asked for", f.ExpiresAt)
	}
	if f.SizeBytes != int64(len("photo bytes")) {
		t.Errorf("size = %d", f.SizeBytes)
	}
	if !strings.HasSuffix(f.URL, "/yaz-tatili-2026.jpg") {
		t.Errorf("url = %q, want a slugified cosmetic name", f.URL)
	}
	if !strings.HasSuffix(storedName(t, f.URL), ".jpg") {
		t.Errorf("stored name = %q, want the extension kept", storedName(t, f.URL))
	}
}

func TestUploadStoresFileOnDisk(t *testing.T) {
	h := newHarness(t, nil)
	got := h.uploadOK(t, [2]string{"a.txt", "content"})
	id, ext := storage.SplitName(storedName(t, got.Files[0].URL))
	path, err := h.store.Path(id, ext)
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil || string(data) != "content" {
		t.Errorf("stored file = %q (%v)", data, err)
	}
	if want := filepath.Join(h.store.Root(), id[0:4], id[4:6], id[6:8]); filepath.Dir(path) != want {
		t.Errorf("stored under %q, want the date directory %q", filepath.Dir(path), want)
	}
}

func TestUploadMultipleFiles(t *testing.T) {
	h := newHarness(t, nil)
	got := h.uploadOK(t,
		[2]string{"a.png", "first"},
		[2]string{"b.pdf", "second"},
	)
	if len(got.Files) != 2 {
		t.Fatalf("files = %d, want 2", len(got.Files))
	}
	if !strings.HasSuffix(got.Files[0].URL, ".png") || !strings.HasSuffix(got.Files[1].URL, ".pdf") {
		t.Errorf("urls = %q/%q, want each extension kept", got.Files[0].URL, got.Files[1].URL)
	}
	if files, _ := h.store.Stats(); files != 2 {
		t.Errorf("stored %d files, want 2", files)
	}
}

func TestUploadIsAllOrNothing(t *testing.T) {
	h := newHarness(t, func(c *config.Config) { c.MaxFileSize = 16 })
	resp := h.upload(t, testToken,
		[2]string{"small.txt", "tiny"},
		[2]string{"big.txt", strings.Repeat("x", 100)},
	)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want 413", resp.StatusCode)
	}
	if files, bytes := h.store.Stats(); files != 0 || bytes != 0 {
		t.Errorf("a rejected batch must leave nothing behind, got %d files / %d bytes", files, bytes)
	}
	entries, _ := os.ReadDir(h.store.Root())
	for _, e := range entries {
		if e.IsDir() {
			t.Errorf("date directory %s survived a fully rolled back upload", e.Name())
		}
	}
}

func TestUploadRejectsTooManyFiles(t *testing.T) {
	h := newHarness(t, func(c *config.Config) { c.MaxFilesPerRequest = 2 })
	resp := h.upload(t, testToken,
		[2]string{"a.txt", "1"}, [2]string{"b.txt", "2"}, [2]string{"c.txt", "3"},
	)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
	if msg := decodeError(t, resp); !strings.Contains(msg, "at most 2") {
		t.Errorf("error = %q, want it to state the limit", msg)
	}
	if files, _ := h.store.Stats(); files != 0 {
		t.Error("the first files must be rolled back too")
	}
}

func TestUploadRejectsOversizedFile(t *testing.T) {
	h := newHarness(t, func(c *config.Config) { c.MaxFileSize = 10 })
	resp := h.upload(t, testToken, [2]string{"big.bin", strings.Repeat("x", 11)})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want 413", resp.StatusCode)
	}
	if msg := decodeError(t, resp); !strings.Contains(msg, "10B") {
		t.Errorf("error = %q, want it to name the limit", msg)
	}
}

func TestUploadRejectsWhenQuotaIsFull(t *testing.T) {
	h := newHarness(t, func(c *config.Config) { c.MaxTotalSize = 8 })
	if resp := h.upload(t, testToken, [2]string{"a.txt", "12345678"}); resp.StatusCode != http.StatusCreated {
		t.Fatalf("first upload = %d", resp.StatusCode)
	} else {
		resp.Body.Close()
	}
	resp := h.upload(t, testToken, [2]string{"b.txt", "x"})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusInsufficientStorage {
		t.Fatalf("status = %d, want 507", resp.StatusCode)
	}
	if msg := decodeError(t, resp); !strings.Contains(msg, "quota") {
		t.Errorf("error = %q", msg)
	}
}

func TestUploadRequiresMultipart(t *testing.T) {
	h := newHarness(t, nil)
	req, _ := http.NewRequest(http.MethodPost, h.URL+"/upload", strings.NewReader("raw"))
	req.Header.Set("Authorization", "Bearer "+testToken)
	req.Header.Set("Content-Type", "text/plain")
	resp, err := h.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnsupportedMediaType {
		t.Fatalf("status = %d, want 415", resp.StatusCode)
	}
	if msg := decodeError(t, resp); !strings.Contains(msg, "PUT /upload/") {
		t.Errorf("error = %q, want it to point at the raw-body endpoint", msg)
	}
}

func TestUploadRejectsMissingBoundary(t *testing.T) {
	h := newHarness(t, nil)
	req, _ := http.NewRequest(http.MethodPost, h.URL+"/upload", strings.NewReader("junk"))
	req.Header.Set("Authorization", "Bearer "+testToken)
	req.Header.Set("Content-Type", "multipart/form-data")
	resp, err := h.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
}

func TestUploadRejectsMalformedBody(t *testing.T) {
	h := newHarness(t, nil)
	req, _ := http.NewRequest(http.MethodPost, h.URL+"/upload", strings.NewReader("--abc\r\ngarbage"))
	req.Header.Set("Authorization", "Bearer "+testToken)
	req.Header.Set("Content-Type", `multipart/form-data; boundary=abc`)
	resp, err := h.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusInternalServerError && resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want a client or server error", resp.StatusCode)
	}
}

func TestUploadWithoutFileField(t *testing.T) {
	h := newHarness(t, nil)
	var body bytes.Buffer
	w := multipart.NewWriter(&body)
	if err := w.WriteField("description", "no file here"); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	req, _ := http.NewRequest(http.MethodPost, h.URL+"/upload", &body)
	req.Header.Set("Authorization", "Bearer "+testToken)
	req.Header.Set("Content-Type", w.FormDataContentType())
	resp, err := h.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
	if msg := decodeError(t, resp); !strings.Contains(msg, `"file"`) {
		t.Errorf("error = %q, want it to name the expected field", msg)
	}
}

func TestUploadIgnoresOtherFormFields(t *testing.T) {
	h := newHarness(t, nil)
	var body bytes.Buffer
	w := multipart.NewWriter(&body)
	if err := w.WriteField("caption", "ignored"); err != nil {
		t.Fatal(err)
	}
	part, err := w.CreateFormFile("file", "a.txt")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.WriteString(part, "kept"); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	req, _ := http.NewRequest(http.MethodPost, h.URL+"/upload", &body)
	req.Header.Set("Authorization", "Bearer "+testToken)
	req.Header.Set("Content-Type", w.FormDataContentType())
	resp, err := h.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("status = %d, want 201", resp.StatusCode)
	}
}

func TestUploadAcceptsAnyNamedFilePart(t *testing.T) {
	// Agents and libraries do not always call the field "file"; anything that
	// carries a file name is treated as an upload.
	h := newHarness(t, nil)
	var body bytes.Buffer
	w := multipart.NewWriter(&body)
	part, err := w.CreateFormFile("attachment", "note.txt")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.WriteString(part, "hello"); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	req, _ := http.NewRequest(http.MethodPost, h.URL+"/upload", &body)
	req.Header.Set("Authorization", "Bearer "+testToken)
	req.Header.Set("Content-Type", w.FormDataContentType())
	resp, err := h.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("status = %d, want 201", resp.StatusCode)
	}
}

func TestPutRawBodyUpload(t *testing.T) {
	h := newHarness(t, nil)
	req, _ := http.NewRequest(http.MethodPut, h.URL+"/upload/report.pdf", strings.NewReader("%PDF-1.7"))
	req.Header.Set("Authorization", "Bearer "+testToken)
	resp, err := h.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("status = %d, want 201", resp.StatusCode)
	}
	var out uploadResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	if len(out.Files) != 1 || !strings.HasSuffix(out.Files[0].URL, ".pdf") {
		t.Errorf("response = %+v, want a single PDF", out.Files)
	}
	if !strings.HasSuffix(out.Files[0].URL, "/report.pdf") {
		t.Errorf("url = %q, want the name from the path", out.Files[0].URL)
	}
}

func TestPutRejectsOversizedBody(t *testing.T) {
	h := newHarness(t, func(c *config.Config) { c.MaxFileSize = 4 })
	req, _ := http.NewRequest(http.MethodPut, h.URL+"/upload/big.bin", strings.NewReader("12345678"))
	req.Header.Set("Authorization", "Bearer "+testToken)
	resp, err := h.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want 413", resp.StatusCode)
	}
}

func TestUploadWithAPIKeyHeader(t *testing.T) {
	h := newHarness(t, nil)
	var body bytes.Buffer
	w := multipart.NewWriter(&body)
	part, _ := w.CreateFormFile("file", "a.txt")
	_, _ = io.WriteString(part, "x")
	_ = w.Close()
	req, _ := http.NewRequest(http.MethodPost, h.URL+"/upload", &body)
	req.Header.Set("X-API-Key", testToken)
	req.Header.Set("Content-Type", w.FormDataContentType())
	resp, err := h.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("X-API-Key should be accepted, got %d", resp.StatusCode)
	}
}

// ----------------------------------------------------------------- downloads

func TestDownloadNeedsNoAuthentication(t *testing.T) {
	h := newHarness(t, nil)
	got := h.uploadOK(t, [2]string{"photo.jpg", "image bytes"})

	resp := h.do(t, http.MethodGet, got.Files[0].URL, "")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 without a token", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if string(body) != "image bytes" {
		t.Errorf("body = %q", body)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "image/jpeg" {
		t.Errorf("Content-Type = %q", ct)
	}
	if cd := resp.Header.Get("Content-Disposition"); !strings.HasPrefix(cd, "inline") {
		t.Errorf("Content-Disposition = %q, want inline for an image", cd)
	}
	if cc := resp.Header.Get("Cache-Control"); !strings.Contains(cc, "immutable") {
		t.Errorf("Cache-Control = %q, want an immutable cache policy", cc)
	}
}

func TestDownloadShortForm(t *testing.T) {
	h := newHarness(t, nil)
	got := h.uploadOK(t, [2]string{"photo.jpg", "bytes"})
	resp := h.do(t, http.MethodGet, h.URL+"/f/"+storedName(t, got.Files[0].URL), "")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want the short URL form to work", resp.StatusCode)
	}
}

func TestDownloadForcedByQueryParameter(t *testing.T) {
	h := newHarness(t, nil)
	got := h.uploadOK(t, [2]string{"photo.jpg", "bytes"})
	resp := h.do(t, http.MethodGet, got.Files[0].URL+"?dl=1", "")
	defer resp.Body.Close()
	if cd := resp.Header.Get("Content-Disposition"); !strings.HasPrefix(cd, "attachment") {
		t.Errorf("Content-Disposition = %q, want attachment when ?dl is set", cd)
	}
}

func TestDownloadSupportsRangeAndConditionalRequests(t *testing.T) {
	h := newHarness(t, nil)
	got := h.uploadOK(t, [2]string{"data.bin", "0123456789"})

	req, _ := http.NewRequest(http.MethodGet, got.Files[0].URL, nil)
	req.Header.Set("Range", "bytes=2-4")
	resp, err := h.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusPartialContent || string(body) != "234" {
		t.Errorf("range request = %d %q, want 206 \"234\"", resp.StatusCode, body)
	}

	req, _ = http.NewRequest(http.MethodGet, got.Files[0].URL, nil)
	req.Header.Set("If-None-Match", resp.Header.Get("ETag"))
	resp2, err := h.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp2.Body.Close()
	if resp2.StatusCode != http.StatusNotModified {
		t.Errorf("conditional request = %d, want 304", resp2.StatusCode)
	}
}

func TestHeadRequestReturnsHeadersOnly(t *testing.T) {
	h := newHarness(t, nil)
	got := h.uploadOK(t, [2]string{"photo.jpg", "image bytes"})
	resp := h.do(t, http.MethodHead, got.Files[0].URL, "")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	if resp.ContentLength != int64(len("image bytes")) {
		t.Errorf("Content-Length = %d", resp.ContentLength)
	}
	body, _ := io.ReadAll(resp.Body)
	if len(body) != 0 {
		t.Errorf("HEAD returned a body: %q", body)
	}
}

func TestDownloadUnknownFile(t *testing.T) {
	h := newHarness(t, nil)
	id := "20260815-143022-8f4e2c91b7934b38a72d1c0e5b6a4f3d"
	for _, url := range []string{
		h.URL + "/f/" + id + ".jpg",
		h.URL + "/f/" + id + "/photo.jpg",
	} {
		resp := h.do(t, http.MethodGet, url, "")
		resp.Body.Close()
		if resp.StatusCode != http.StatusNotFound {
			t.Errorf("GET %s = %d, want 404", url, resp.StatusCode)
		}
	}
}

func TestDownloadOfUnknownExtensionFallsBackToBinary(t *testing.T) {
	h := newHarness(t, nil)
	got := h.uploadOK(t, [2]string{"archive.zzz", "opaque"})
	resp := h.do(t, http.MethodGet, got.Files[0].URL, "")
	defer resp.Body.Close()
	if ct := resp.Header.Get("Content-Type"); ct != "application/octet-stream" {
		t.Errorf("Content-Type = %q, want application/octet-stream", ct)
	}
}

func TestUploadWithoutExtension(t *testing.T) {
	h := newHarness(t, nil)
	got := h.uploadOK(t, [2]string{"LICENSE", "MIT"})
	if strings.Contains(storedName(t, got.Files[0].URL), ".") {
		t.Errorf("id = %q, want no extension", storedName(t, got.Files[0].URL))
	}
	resp := h.do(t, http.MethodGet, got.Files[0].URL, "")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
}

// ------------------------------------------------------------------- deletes

func TestDeleteLifecycle(t *testing.T) {
	h := newHarness(t, nil)
	got := h.uploadOK(t, [2]string{"photo.jpg", "bytes"})

	if resp := h.do(t, http.MethodDelete, got.Files[0].URL, ""); resp.StatusCode != http.StatusUnauthorized {
		resp.Body.Close()
		t.Fatalf("unauthenticated delete = %d, want 401", resp.StatusCode)
	}
	resp := h.do(t, http.MethodDelete, got.Files[0].URL, testToken)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("delete = %d, want 204", resp.StatusCode)
	}
	resp = h.do(t, http.MethodDelete, got.Files[0].URL, testToken)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("second delete = %d, want 404", resp.StatusCode)
	}
	resp = h.do(t, http.MethodGet, got.Files[0].URL, "")
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("download after delete = %d, want 404", resp.StatusCode)
	}
	if files, bytes := h.store.Stats(); files != 0 || bytes != 0 {
		t.Errorf("stats after delete = %d/%d", files, bytes)
	}
}

func TestDeleteShortForm(t *testing.T) {
	h := newHarness(t, nil)
	got := h.uploadOK(t, [2]string{"photo.jpg", "bytes"})
	resp := h.do(t, http.MethodDelete, h.URL+"/f/"+storedName(t, got.Files[0].URL), testToken)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("delete = %d, want 204", resp.StatusCode)
	}
}

func TestDeleteMalformedIdentifier(t *testing.T) {
	h := newHarness(t, nil)
	resp := h.do(t, http.MethodDelete, h.URL+"/f/not-an-id.jpg", testToken)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", resp.StatusCode)
	}
}

func TestDeleteReportsStorageFailures(t *testing.T) {
	requireStrictPermissions(t)
	h := newHarness(t, nil)
	got := h.uploadOK(t, [2]string{"photo.jpg", "bytes"})
	id, ext := storage.SplitName(storedName(t, got.Files[0].URL))
	path, err := h.store.Path(id, ext)
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Dir(path)
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })

	resp := h.do(t, http.MethodDelete, got.Files[0].URL, testToken)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500 when the file cannot be removed", resp.StatusCode)
	}
}

// -------------------------------------------------------------------- health

func TestHealthAndReadiness(t *testing.T) {
	h := newHarness(t, nil)
	for _, path := range []string{"/healthz", "/readyz"} {
		resp := h.do(t, http.MethodGet, h.URL+path, "")
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Errorf("GET %s = %d, want 200", path, resp.StatusCode)
		}
	}
}

func TestReadinessFailsWhenStorageIsNotWritable(t *testing.T) {
	requireStrictPermissions(t)
	h := newHarness(t, nil)
	if err := os.Chmod(h.store.Root(), 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(h.store.Root(), 0o700) })

	resp := h.do(t, http.MethodGet, h.URL+"/readyz", "")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503 when the volume is read-only", resp.StatusCode)
	}
}

func TestStatsRequiresAuthentication(t *testing.T) {
	h := newHarness(t, nil)
	h.uploadOK(t, [2]string{"a.txt", "12345"})

	resp := h.do(t, http.MethodGet, h.URL+"/stats", "")
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unauthenticated stats = %d, want 401", resp.StatusCode)
	}

	resp = h.do(t, http.MethodGet, h.URL+"/stats", testToken)
	defer resp.Body.Close()
	var stats struct {
		Files      int64  `json:"files"`
		Bytes      int64  `json:"bytes"`
		BytesHuman string `json:"bytes_human"`
		Version    string `json:"version"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&stats); err != nil {
		t.Fatal(err)
	}
	if stats.Files != 1 || stats.Bytes != 5 || stats.Version != "test" {
		t.Errorf("stats = %+v", stats)
	}
	if stats.BytesHuman != "5B" {
		t.Errorf("bytes_human = %q", stats.BytesHuman)
	}
}

// --------------------------------------------------------------- discovery

func TestUsagePage(t *testing.T) {
	h := newHarness(t, nil)
	resp := h.do(t, http.MethodGet, h.URL+"/", "")
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/plain") {
		t.Errorf("Content-Type = %q", ct)
	}
	for _, want := range []string{"POST", "/upload", "/f/", "max file size", "llms.txt"} {
		if !strings.Contains(string(body), want) {
			t.Errorf("usage page should mention %q:\n%s", want, body)
		}
	}
}

func TestUsagePageMentionsOptionalLimits(t *testing.T) {
	h := newHarness(t, func(c *config.Config) {
		c.MaxTotalSize = 20 << 30
		c.Retention = 30 * 24 * time.Hour
	})
	resp := h.do(t, http.MethodGet, h.URL+"/", "")
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "storage quota") || !strings.Contains(string(body), "deleted after") {
		t.Errorf("usage page should mention the quota and retention:\n%s", body)
	}
}

func TestLLMsTxtDescribesThisInstance(t *testing.T) {
	h := newHarness(t, func(c *config.Config) {
		c.MaxTotalSize = 20 << 30
		c.Retention = 7 * 24 * time.Hour
	})
	resp := h.do(t, http.MethodGet, h.URL+"/llms.txt", "")
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	for _, want := range []string{
		"# GoDrop", "Authorization: Bearer", "POST", "/upload", "413", "507",
		"100.0MB", "20.0GB", "168h0m0s", h.URL,
	} {
		if !strings.Contains(string(body), want) {
			t.Errorf("llms.txt should contain %q", want)
		}
	}
}

// The skill is installed by URL, by an agent, into a directory a repository
// usually carries. Three things have to hold: it is served at all, it is the
// bytes the CLI would have written, and it has no token in it.
func TestSkillIsServedAndCarriesNoToken(t *testing.T) {
	h := newHarness(t, nil)
	resp := h.do(t, http.MethodGet, h.URL+"/skill.md", "")
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	if got := resp.Header.Get("Content-Type"); got != "text/markdown; charset=utf-8" {
		t.Errorf("Content-Type = %q", got)
	}
	if !strings.HasPrefix(string(body), "---\nname: godrop") {
		t.Error("a skill needs frontmatter naming it, or no agent will find it")
	}
	if string(body) != skill.Markdown {
		t.Error("the served skill is not the one `godrop skill install` writes")
	}
	// A skill that shipped a working token would publish it into every
	// repository that committed the file. The placeholder showing a token's
	// shape is fine; this server's own token is not.
	if strings.Contains(string(body), testToken) {
		t.Error("the skill carries a real token, which would be published by whoever commits it")
	}
	if !strings.Contains(string(body), "GODROP_TOKEN") {
		t.Error("the skill should teach the environment variable, since it cannot carry the key")
	}
	// A plain YAML scalar cannot carry a colon followed by a space: the parser
	// reads it as a nested mapping, the frontmatter stops being frontmatter,
	// and installers refuse the file rather than guess at it.
	front, _, ok := strings.Cut(strings.TrimPrefix(string(body), "---\n"), "\n---")
	if !ok {
		t.Fatal("the skill has no frontmatter to check")
	}
	for _, line := range strings.Split(front, "\n") {
		_, value, found := strings.Cut(line, ":")
		if found && strings.Contains(strings.TrimSpace(value), ": ") {
			t.Errorf("frontmatter line %q will not parse as YAML", line)
		}
	}
}

func TestSkillNeedsNoToken(t *testing.T) {
	h := newHarness(t, nil)
	// Downloads need no token and neither does this: an agent installs it
	// before it has one.
	resp := h.do(t, http.MethodGet, h.URL+"/skill.md", "")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d without a token", resp.StatusCode)
	}
}

func TestLLMsTxtWithoutOptionalLimits(t *testing.T) {
	h := newHarness(t, nil)
	resp := h.do(t, http.MethodGet, h.URL+"/llms.txt", "")
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "unlimited") || !strings.Contains(string(body), "kept forever") {
		t.Errorf("llms.txt should spell out the absence of limits:\n%s", body)
	}
}

func TestOpenAPISpecIsServed(t *testing.T) {
	h := newHarness(t, nil)
	resp := h.do(t, http.MethodGet, h.URL+"/openapi.yaml", "")
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.Contains(ct, "yaml") {
		t.Errorf("Content-Type = %q", ct)
	}
	for _, want := range []string{"openapi: 3.1.0", "/upload", "uploadFiles", "bearerAuth"} {
		if !strings.Contains(string(body), want) {
			t.Errorf("spec should contain %q", want)
		}
	}
}

// ---------------------------------------------------------------- base URL

func TestConfiguredBaseURLIsUsedForLinks(t *testing.T) {
	h := newHarness(t, func(c *config.Config) { c.BaseURL = "https://files.example.com" })
	got := h.uploadOK(t, [2]string{"photo.jpg", "bytes"})
	if !strings.HasPrefix(got.Files[0].URL, "https://files.example.com/f/") {
		t.Errorf("url = %q, want the configured base URL", got.Files[0].URL)
	}
}

func TestForwardedProtocolIsHonoured(t *testing.T) {
	h := newHarness(t, nil)
	var body bytes.Buffer
	w := multipart.NewWriter(&body)
	part, _ := w.CreateFormFile("file", "a.jpg")
	_, _ = io.WriteString(part, "x")
	_ = w.Close()
	req, _ := http.NewRequest(http.MethodPost, h.URL+"/upload", &body)
	req.Header.Set("Authorization", "Bearer "+testToken)
	req.Header.Set("Content-Type", w.FormDataContentType())
	req.Header.Set("X-Forwarded-Proto", "https")
	resp, err := h.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out uploadResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(out.Files[0].URL, "https://") {
		t.Errorf("url = %q, want https behind a TLS-terminating proxy", out.Files[0].URL)
	}
}

// ---------------------------------------------------------------- logging

func TestAccessLogRecordsRequestsWithoutSecrets(t *testing.T) {
	h := newHarness(t, nil)
	h.uploadOK(t, [2]string{"a.txt", "x"})
	logs := h.logs.String()
	if !strings.Contains(logs, `"msg":"request"`) || !strings.Contains(logs, `"msg":"upload"`) {
		t.Errorf("expected request and upload log lines:\n%s", logs)
	}
	if strings.Contains(logs, testToken) {
		t.Fatal("the access log must never contain a token")
	}
}

func TestAccessLogCanBeDisabled(t *testing.T) {
	h := newHarness(t, func(c *config.Config) { c.AccessLog = false })
	h.uploadOK(t, [2]string{"a.txt", "x"})
	if strings.Contains(h.logs.String(), `"msg":"request"`) {
		t.Error("request logging should be off")
	}
}

func TestServerDefaultsWhenOptionsAreSparse(t *testing.T) {
	// A Server built without a logger or clock must still work; this is the
	// shape used by short-lived tools and tests.
	dir := t.TempDir()
	cfg, err := config.LoadFrom(func(string) string { return "" })
	if err != nil {
		t.Fatal(err)
	}
	cfg.DataDir = dir
	store, err := storage.New(dir, 0)
	if err != nil {
		t.Fatal(err)
	}
	tokenStore, err := tokens.New(tokens.Path(dir), []string{testToken})
	if err != nil {
		t.Fatal(err)
	}
	srv := New(Options{Config: cfg, Store: store, Tokens: tokenStore})
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
}

// requireStrictPermissions skips a test that depends on POSIX permission
// semantics. As root every mode is writable anyway, and on Windows chmod only
// toggles a read-only bit, so the situations these tests create cannot exist.
func requireStrictPermissions(t *testing.T) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("file modes are advisory on Windows")
	}
	if os.Geteuid() == 0 {
		t.Skip("permission checks are meaningless as root")
	}
}

// storedName derives the on-disk name of an uploaded file from its URL, the
// way any client would have to now that the response carries the URL and
// nothing that repeats it.
func storedName(t *testing.T, rawURL string) string {
	t.Helper()
	u, err := url.Parse(rawURL)
	if err != nil {
		t.Fatalf("parse %q: %v", rawURL, err)
	}
	parts := strings.Split(strings.Trim(u.Path, "/"), "/")
	if len(parts) < 2 || parts[0] != "f" {
		t.Fatalf("not an upload URL: %q", rawURL)
	}
	id := parts[1]
	if len(parts) == 2 {
		return id // no cosmetic name: the last segment is the stored name
	}
	if ext := SanitizeExt(parts[2]); ext != "" {
		return id + "." + ext
	}
	return id
}

// uploadRaw posts one multipart upload with extra headers, and returns the
// response as it came.
func (h *harness) uploadRaw(t *testing.T, headers map[string]string, files ...[2]string) *http.Response {
	t.Helper()
	var body bytes.Buffer
	w := multipart.NewWriter(&body)
	for _, f := range files {
		part, err := w.CreateFormFile("file", f[0])
		if err != nil {
			t.Fatal(err)
		}
		if _, err := io.WriteString(part, f[1]); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	req, err := http.NewRequest(http.MethodPost, h.URL+"/upload", &body)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", w.FormDataContentType())
	req.Header.Set("Authorization", "Bearer "+testToken)
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := h.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

// uploadWithHeaders is uploadRaw for the cases that expect it to work.
func (h *harness) uploadWithHeaders(t *testing.T, headers map[string]string, files ...[2]string) uploadResponse {
	t.Helper()
	resp := h.uploadRaw(t, headers, files...)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("upload = %d: %s", resp.StatusCode, body)
	}
	var out uploadResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return out
}

func TestAnUploadSaysWhereTheFileWentInTheHeaderToo(t *testing.T) {
	// 201 with Location is how HTTP says "the thing you just made is here",
	// and it is the answer without a JSON parser.
	h := newHarness(t, nil)
	resp := h.upload(t, testToken, [2]string{"photo.jpg", "bytes"})
	defer resp.Body.Close()

	var got uploadResponse
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	if location := resp.Header.Get("Location"); location != got.Files[0].URL {
		t.Errorf("Location = %q, want %q", location, got.Files[0].URL)
	}
}

func TestWithSeveralFilesTheHeaderPointsAtTheFirst(t *testing.T) {
	h := newHarness(t, nil)
	resp := h.upload(t, testToken, [2]string{"a.png", "first"}, [2]string{"b.pdf", "second"})
	defer resp.Body.Close()

	var got uploadResponse
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	if location := resp.Header.Get("Location"); location != got.Files[0].URL {
		t.Errorf("Location = %q, want the first of %d files", location, len(got.Files))
	}
}
