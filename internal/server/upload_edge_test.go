package server

import (
	"io"
	"io/fs"
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/fatihbaltaci/GoDrop/internal/config"
	"github.com/fatihbaltaci/GoDrop/internal/storage"
)

func TestRequestBodyIsBoundedAcrossAllParts(t *testing.T) {
	// Each part is capped, but so is the request as a whole: many small parts
	// must not add up to an unbounded stream.
	h := newHarness(t, func(c *config.Config) {
		c.MaxFileSize = 1024
		c.MaxFilesPerRequest = 2
	})
	limit := h.cfg.MaxFileSize*int64(h.cfg.MaxFilesPerRequest) + 1<<20

	pr, pw := io.Pipe()
	mw := multipart.NewWriter(pw)
	go func() {
		part, err := mw.CreateFormFile("file", "flood.bin")
		if err != nil {
			_ = pw.CloseWithError(err)
			return
		}
		chunk := strings.Repeat("x", 64<<10)
		for written := int64(0); written < limit+(1<<20); written += int64(len(chunk)) {
			if _, err := io.WriteString(part, chunk); err != nil {
				_ = pw.CloseWithError(err)
				return
			}
		}
		_ = mw.Close()
		_ = pw.Close()
	}()

	req, _ := http.NewRequest(http.MethodPost, h.URL+"/upload", pr)
	req.Header.Set("Authorization", "Bearer "+testToken)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	resp, err := h.Client().Do(req)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want 413 once the request exceeds the overall cap", resp.StatusCode)
	}
	if files, _ := h.store.Stats(); files != 0 {
		t.Errorf("stored %d files, want none", files)
	}
}

func TestRollbackFailureIsLoggedNotHidden(t *testing.T) {
	// A file that disappears between being written and being rolled back must
	// not turn into a silent inconsistency: the operator needs to see it.
	h := newHarness(t, func(c *config.Config) { c.MaxFileSize = 32 })
	const stored = "stored"

	pr, pw := io.Pipe()
	mw := multipart.NewWriter(pw)
	go func() {
		first, err := mw.CreateFormFile("file", "first.txt")
		if err != nil {
			_ = pw.CloseWithError(err)
			return
		}
		if _, err := io.WriteString(first, stored); err != nil {
			_ = pw.CloseWithError(err)
			return
		}
		// Starting the second part writes the boundary that ends the first one,
		// so the server finishes and closes that file before we touch it. That
		// ordering matters: Windows refuses to delete a file that is still open.
		second, err := mw.CreateFormFile("file", "second.txt")
		if err != nil {
			_ = pw.CloseWithError(err)
			return
		}
		// Wait until the first part has landed, then remove it behind the
		// server's back so the rollback has nothing left to delete.
		deadline := time.Now().Add(10 * time.Second)
		for time.Now().Before(deadline) {
			if files, _ := h.store.Stats(); files == 1 {
				break
			}
			time.Sleep(time.Millisecond)
		}
		removeStoredFile(t, h.store.Root(), int64(len(stored)))

		// Oversized, so the whole request is rolled back.
		if _, err := io.WriteString(second, strings.Repeat("y", 200)); err != nil {
			_ = pw.CloseWithError(err)
			return
		}
		_ = mw.Close()
		_ = pw.Close()
	}()

	req, _ := http.NewRequest(http.MethodPost, h.URL+"/upload", pr)
	req.Header.Set("Authorization", "Bearer "+testToken)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	resp, err := h.Client().Do(req)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want 413", resp.StatusCode)
	}
	if !strings.Contains(h.logs.String(), "rollback failed") {
		t.Errorf("a failed rollback must be logged:\n%s", h.logs.String())
	}
}

// removeStoredFile deletes the one stored file of the given size directly on
// disk, bypassing the store, to simulate an operator or another process
// interfering.
//
// Only that file is touched. Removing the directory tree instead would race
// with the second upload, which the server creates inside it as soon as it has
// read that part's headers: land between its MkdirAll and its open and the
// upload fails for the wrong reason.
func removeStoredFile(t *testing.T, root string, size int64) {
	t.Helper()
	var removed string
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || removed != "" {
			return err
		}
		info, err := d.Info()
		if err != nil || info.Size() != size {
			return err
		}
		removed = path
		return os.Remove(path)
	})
	if err != nil || removed == "" {
		t.Fatalf("could not remove the stored file (found %q): %v", removed, err)
	}
}

func TestSlugFallsBackToTheStoredName(t *testing.T) {
	// A name that sanitises to nothing still produces a usable URL.
	h := newHarness(t, nil)
	got := h.uploadOK(t, [2]string{"🙂.png", "bytes"})
	id, ext := storage.SplitName(storedName(t, got.Files[0].URL))
	if !strings.HasSuffix(got.URL, "/f/"+storage.JoinName(id, ext)) {
		t.Errorf("url = %q, want the short form when the name has no usable characters", got.URL)
	}
	resp := h.do(t, http.MethodGet, got.URL, "")
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want the fallback URL to work", resp.StatusCode)
	}
}

func TestOversizedNonFileFieldCountsAgainstTheRequestCap(t *testing.T) {
	// Ordinary form fields are skipped rather than stored, but they still
	// travel over the wire: a huge text field must not be a free channel for
	// unbounded data.
	h := newHarness(t, func(c *config.Config) {
		c.MaxFileSize = 1024
		c.MaxFilesPerRequest = 1
	})
	pr, pw := io.Pipe()
	mw := multipart.NewWriter(pw)
	go func() {
		field, err := mw.CreateFormField("description")
		if err != nil {
			_ = pw.CloseWithError(err)
			return
		}
		chunk := strings.Repeat("d", 64<<10)
		for written := 0; written < 3<<20; written += len(chunk) {
			if _, err := io.WriteString(field, chunk); err != nil {
				_ = pw.CloseWithError(err)
				return
			}
		}
		part, err := mw.CreateFormFile("file", "after.txt")
		if err != nil {
			_ = pw.CloseWithError(err)
			return
		}
		_, _ = io.WriteString(part, "never reached")
		_ = mw.Close()
		_ = pw.Close()
	}()

	req, _ := http.NewRequest(http.MethodPost, h.URL+"/upload", pr)
	req.Header.Set("Authorization", "Bearer "+testToken)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	resp, err := h.Client().Do(req)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want 413", resp.StatusCode)
	}
	if files, _ := h.store.Stats(); files != 0 {
		t.Errorf("stored %d files, want none", files)
	}
}

func TestCosmeticNameWithAnUnusableExtensionIs404(t *testing.T) {
	h := newHarness(t, nil)
	got := h.uploadOK(t, [2]string{"photo.jpg", "bytes"})
	id, _ := storage.SplitName(storedName(t, got.Files[0].URL))
	for _, name := range []string{"photo.abcdefghijk", "photo.jp g", "photo.şey"} {
		resp := h.do(t, http.MethodGet, h.URL+"/f/"+id+"/"+name, "")
		resp.Body.Close()
		if resp.StatusCode != http.StatusNotFound {
			t.Errorf("/f/<id>/%s = %d, want 404", name, resp.StatusCode)
		}
	}
}

// ------------------------------------------------------------ per-file expiry

func TestAnUploadCanSetItsOwnExpiry(t *testing.T) {
	h := newHarness(t, nil)
	got := h.uploadWithHeaders(t, map[string]string{"X-Expires-In": "24h"},
		[2]string{"note.txt", "gone tomorrow"})
	if got.Files[0].ExpiresAt == "" {
		t.Fatal("the response should say when it expires")
	}
	at, err := time.Parse(time.RFC3339, got.Files[0].ExpiresAt)
	if err != nil {
		t.Fatalf("expires_at = %q: %v", got.Files[0].ExpiresAt, err)
	}
	if wait := time.Until(at); wait < 23*time.Hour || wait > 25*time.Hour {
		t.Errorf("expires in %s, want about a day", wait)
	}
	// Before the moment passes it is an ordinary file.
	resp := h.do(t, http.MethodGet, got.URL, "")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("download = %d, want it to work before it expires", resp.StatusCode)
	}
}

func TestAnExpiredFileIsGone(t *testing.T) {
	// Stored with an expiry already in the past, which is what the sweep and
	// the download handler both have to notice.
	h := newHarness(t, nil)
	file, err := h.store.CreateWithExpiry("txt", strings.NewReader("brief"),
		h.cfg.MaxFileSize, time.Now().Add(-time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	resp := h.do(t, http.MethodGet, h.URL+"/f/"+file.Name(), "")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("download after expiry = %d, want 404", resp.StatusCode)
	}

	// And the sweep removes it even with no retention configured.
	removed, _, err := h.store.Cleanup(0)
	if err != nil {
		t.Fatal(err)
	}
	if removed != 1 {
		t.Errorf("cleanup removed %d files, want the expired one", removed)
	}
}

func TestAnImpossibleExpiryIsRefused(t *testing.T) {
	h := newHarness(t, nil)
	for _, value := range []string{"soon", "-1h", "0"} {
		resp := h.uploadRaw(t, map[string]string{"X-Expires-In": value}, [2]string{"a.txt", "x"})
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("X-Expires-In: %s gives %d, want 400", value, resp.StatusCode)
		}
	}
}

func TestRetentionCapsWhatAnUploadCanAskFor(t *testing.T) {
	h := newHarness(t, func(cfg *config.Config) { cfg.Retention = 48 * time.Hour })
	got := h.uploadWithHeaders(t, map[string]string{"X-Expires-In": "30d"}, [2]string{"a.txt", "x"})
	at, err := time.Parse(time.RFC3339, got.Files[0].ExpiresAt)
	if err != nil {
		t.Fatal(err)
	}
	if wait := time.Until(at); wait > 49*time.Hour {
		t.Errorf("expires in %s, want it capped at the retention period", wait)
	}
}
