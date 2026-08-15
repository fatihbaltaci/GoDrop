package server

import (
	"io"
	"mime/multipart"
	"net/http"
	"os"
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

	pr, pw := io.Pipe()
	mw := multipart.NewWriter(pw)
	go func() {
		first, err := mw.CreateFormFile("file", "first.txt")
		if err != nil {
			_ = pw.CloseWithError(err)
			return
		}
		if _, err := io.WriteString(first, "stored"); err != nil {
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
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) {
			if files, _ := h.store.Stats(); files == 1 {
				break
			}
			time.Sleep(time.Millisecond)
		}
		removeStoredFiles(t, h.store.Root())

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

// removeStoredFiles deletes every stored file directly on disk, bypassing the
// store, to simulate an operator or another process interfering.
func removeStoredFiles(t *testing.T, root string) {
	t.Helper()
	_ = os.RemoveAll(root + "/2026")
	_ = os.RemoveAll(root + "/" + time.Now().UTC().Format("2006"))
}

func TestSlugFallsBackToTheStoredName(t *testing.T) {
	// A name that sanitises to nothing still produces a usable URL.
	h := newHarness(t, nil)
	got := h.uploadOK(t, [2]string{"🙂.png", "bytes"})
	id, ext := storage.SplitName(got.Files[0].ID)
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
	id, _ := storage.SplitName(got.Files[0].ID)
	for _, name := range []string{"photo.abcdefghijk", "photo.jp g", "photo.şey"} {
		resp := h.do(t, http.MethodGet, h.URL+"/f/"+id+"/"+name, "")
		resp.Body.Close()
		if resp.StatusCode != http.StatusNotFound {
			t.Errorf("/f/<id>/%s = %d, want 404", name, resp.StatusCode)
		}
	}
}
