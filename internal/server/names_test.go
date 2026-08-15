package server

import (
	"crypto/tls"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/fatihbaltaci/GoDrop/internal/config"
	"github.com/fatihbaltaci/GoDrop/internal/storage"
)

func TestSanitizeExt(t *testing.T) {
	t.Parallel()
	tests := map[string]string{
		"photo.jpg":              "jpg",
		"photo.JPG":              "jpg",
		"photo.JpEg":             "jpeg",
		"archive.tar.gz":         "gz",
		"/var/www/index.html":    "html",
		`C:\Users\me\report.PDF`: "pdf",
		"../../etc/passwd":       "",
		"no-extension":           "",
		"trailing.":              "",
		// A dotfile's suffix is a perfectly good extension; it is stored as one.
		".hidden":               "hidden",
		"space. jpg":            "",
		"unicode.şey":           "",
		"toolong.abcdefghijk":   "",
		"exactlyten.abcdefghij": "abcdefghij",
		"weird.jp+g":            "",
		"digits.mp4":            "mp4",
		"mixed.7z":              "7z",
		"newline.jpg\n":         "",
		// Only the text after the last dot matters, and it is checked character
		// by character — no ";" can smuggle a second extension through.
		"semicolon.jpg;.exe":         "exe",
		"a.b.c.d.e":                  "e",
		"":                           "",
		".":                          "",
		"..":                         "",
		"file.name.with.dots.tar.xz": "xz",
	}
	for in, want := range tests {
		if got := SanitizeExt(in); got != want {
			t.Errorf("SanitizeExt(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestSanitizeSlug(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		ext  string
		want string
	}{
		{"Yaz Tatili 2026.jpg", "jpg", "yaz-tatili-2026.jpg"},
		{"photo.jpg", "jpg", "photo.jpg"},
		{"Şubat Raporu.pdf", "pdf", "subat-raporu.pdf"},
		{"İstanbul Gezisi.png", "png", "istanbul-gezisi.png"},
		{"Ğüçöşı.txt", "txt", "gucosi.txt"},
		{"my_file-01.tar.gz", "gz", "my_file-01.tar.gz"},
		{"../../etc/passwd", "", "passwd"},
		{`C:\photos\Sea View.jpeg`, "jpeg", "sea-view.jpeg"},
		{"   ", "jpg", ""},
		{"!!!.jpg", "jpg", ""},
		{"🙂🙂🙂.png", "png", ""},
		{"a  b   c.txt", "txt", "a-b-c.txt"},
		{"---leading.txt", "txt", "leading.txt"},
		{"", "jpg", ""},
		{strings.Repeat("verylongname", 20) + ".jpg", "jpg", strings.Repeat("verylongname", 5) + ".jpg"},
	}
	for _, tt := range tests {
		got := SanitizeSlug(tt.name, tt.ext)
		if got != tt.want {
			t.Errorf("SanitizeSlug(%q, %q) = %q, want %q", tt.name, tt.ext, got, tt.want)
		}
		if strings.ContainsAny(got, `/\ `) || strings.Contains(got, "..") {
			t.Errorf("SanitizeSlug(%q) produced an unsafe segment: %q", tt.name, got)
		}
	}
}

func TestSanitizeSlugIsBounded(t *testing.T) {
	t.Parallel()
	got := SanitizeSlug(strings.Repeat("a", 500)+".jpg", "jpg")
	if len(got) > maxSlugLen+len(".jpg") {
		t.Errorf("slug length = %d, want it bounded to %d", len(got), maxSlugLen)
	}
}

func TestSplitStoredName(t *testing.T) {
	t.Parallel()
	id := "20260815-143022-8f4e2c91b7934b38a72d1c0e5b6a4f3d"
	gotID, gotExt, ok := SplitStoredName(id + ".jpg")
	if !ok || gotID != id || gotExt != "jpg" {
		t.Errorf("SplitStoredName = %q/%q/%t", gotID, gotExt, ok)
	}
	if _, _, ok := SplitStoredName(id); !ok {
		t.Error("a name without an extension should still parse")
	}
	for _, bad := range []string{"not-an-id.jpg", id + ".TOOLONGEXT", "../x.jpg", ""} {
		if _, _, ok := SplitStoredName(bad); ok {
			t.Errorf("SplitStoredName(%q) accepted an invalid name", bad)
		}
	}
}

func TestContentType(t *testing.T) {
	t.Parallel()
	if got := ContentType("jpg", nil); got != "image/jpeg" {
		t.Errorf("ContentType(jpg) = %q", got)
	}
	if got := ContentType("", nil); got != "application/octet-stream" {
		t.Errorf("ContentType with no extension = %q", got)
	}
	// An unknown extension falls back to sniffing the leading bytes.
	png := []byte{0x89, 'P', 'N', 'G', 0x0d, 0x0a, 0x1a, 0x0a}
	if got := ContentType("zzz", png); got != "image/png" {
		t.Errorf("ContentType(zzz, png bytes) = %q, want image/png from sniffing", got)
	}
	if got := ContentType("zzz", nil); got != "application/octet-stream" {
		t.Errorf("ContentType(zzz) = %q", got)
	}
}

func TestIsDangerousExt(t *testing.T) {
	t.Parallel()
	for _, ext := range []string{"html", "htm", "svg", "xhtml", "xml", "xsl", "xslt", "mhtml", "svgz"} {
		if !IsDangerousExt(ext) {
			t.Errorf("%q should be treated as active content", ext)
		}
	}
	for _, ext := range []string{"jpg", "png", "pdf", "txt", "mp4", "zip", ""} {
		if IsDangerousExt(ext) {
			t.Errorf("%q should not be treated as active content", ext)
		}
	}
}

func TestBearerTokenExtraction(t *testing.T) {
	t.Parallel()
	cases := []struct {
		header, value, want string
	}{
		{"Authorization", "Bearer abc", "abc"},
		{"Authorization", "bearer abc", "abc"},
		{"Authorization", "Bearer   abc  ", "abc"},
		{"Authorization", "Basic abc", ""},
		{"Authorization", "abc", ""},
		{"X-API-Key", " abc ", "abc"},
		{"", "", ""},
	}
	for _, c := range cases {
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		if c.header != "" {
			req.Header.Set(c.header, c.value)
		}
		if got := bearerToken(req); got != c.want {
			t.Errorf("bearerToken(%s: %q) = %q, want %q", c.header, c.value, got, c.want)
		}
	}
}

func TestRequestScheme(t *testing.T) {
	t.Parallel()
	plain := httptest.NewRequest(http.MethodGet, "/", nil)
	if got := requestScheme(plain); got != "http" {
		t.Errorf("scheme = %q, want http", got)
	}

	secure := httptest.NewRequest(http.MethodGet, "/", nil)
	secure.TLS = &tls.ConnectionState{}
	if got := requestScheme(secure); got != "https" {
		t.Errorf("scheme over TLS = %q, want https", got)
	}

	forwarded := httptest.NewRequest(http.MethodGet, "/", nil)
	forwarded.Header.Set("X-Forwarded-Proto", "https, http")
	if got := requestScheme(forwarded); got != "https" {
		t.Errorf("scheme = %q, want the first forwarded value", got)
	}
}

func TestResolveRejectsMalformedPathValues(t *testing.T) {
	t.Parallel()
	h := &Server{}
	req := httptest.NewRequest(http.MethodGet, "/f/not-an-id/photo.jpg", nil)
	req.SetPathValue("id", "not-an-id")
	req.SetPathValue("name", "photo.jpg")
	if _, _, _, ok := h.resolve(req); ok {
		t.Error("an invalid identifier must not resolve")
	}
}

func TestRetryAfterIsAtLeastOneSecond(t *testing.T) {
	t.Parallel()
	rec := httptest.NewRecorder()
	retryAfter(rec, 10*time.Millisecond)
	if got := rec.Header().Get("Retry-After"); got != "1" {
		t.Errorf("Retry-After = %q, want at least 1", got)
	}
	rec = httptest.NewRecorder()
	retryAfter(rec, 42*time.Second)
	if got := rec.Header().Get("Retry-After"); got != "42" {
		t.Errorf("Retry-After = %q", got)
	}
}

func TestWriteUploadErrorMapsMaxBytes(t *testing.T) {
	t.Parallel()
	cfg := &config.Config{MaxFileSize: 1 << 20}
	s := &Server{cfg: cfg, log: discardLogger()}

	rec := httptest.NewRecorder()
	s.writeUploadError(rec, &http.MaxBytesError{Limit: 10})
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("MaxBytesError = %d, want 413", rec.Code)
	}

	rec = httptest.NewRecorder()
	s.writeUploadError(rec, storage.ErrQuotaExceeded)
	if rec.Code != http.StatusInsufficientStorage {
		t.Errorf("quota error = %d, want 507", rec.Code)
	}

	rec = httptest.NewRecorder()
	s.writeUploadError(rec, errFake)
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("unknown error = %d, want 500", rec.Code)
	}
}

func TestLogMiddlewareRecordsUnwrittenResponses(t *testing.T) {
	t.Parallel()
	logs := &strings.Builder{}
	s := &Server{cfg: &config.Config{AccessLog: true}, log: newTestLogger(logs), now: time.Now}
	handler := s.logMiddleware(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		// Writes nothing at all: the middleware must still report 200.
	}))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	if !strings.Contains(logs.String(), `"status":200`) {
		t.Errorf("log = %s, want a 200 status", logs.String())
	}
}
