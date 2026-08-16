package server

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/fatihbaltaci/GoDrop/internal/config"
)

// What a download may be kept for is the only thing standing between an upload
// that expires and a cache that goes on serving it afterwards. The server
// cannot reach a copy already taken, so the number it hands out is the whole
// of the promise.

// maxAge reads the seconds out of a Cache-Control header.
func maxAge(t *testing.T, header string) int64 {
	t.Helper()
	for _, part := range strings.Split(header, ",") {
		if v, ok := strings.CutPrefix(strings.TrimSpace(part), "max-age="); ok {
			n, err := strconv.ParseInt(v, 10, 64)
			if err != nil {
				t.Fatalf("max-age in %q: %v", header, err)
			}
			return n
		}
	}
	t.Fatalf("no max-age in %q", header)
	return 0
}

func TestAnUploadWithNoExpiryIsCachedForAsLongAsItLives(t *testing.T) {
	h := newHarness(t, nil)
	got := h.uploadOK(t, [2]string{"photo.jpg", "bytes"})
	resp := h.do(t, http.MethodGet, got.Files[0].URL, "")
	defer resp.Body.Close()

	cc := resp.Header.Get("Cache-Control")
	if !strings.Contains(cc, "immutable") {
		t.Errorf("Cache-Control = %q; the bytes at this URL never change", cc)
	}
	if got, want := maxAge(t, cc), int64(config.DefaultCacheMaxAge.Seconds()); got != want {
		t.Errorf("max-age = %d, want %d", got, want)
	}
}

func TestAnExpiringUploadIsNeverCachedPastItsExpiry(t *testing.T) {
	// A year in a CDN is how a file that stopped existing goes on being
	// served, and the expiry is the only thing that says otherwise.
	h := newHarness(t, nil)
	resp := h.uploadRaw(t, map[string]string{"X-Expires-In": "30m"}, [2]string{"secret.png", "bytes"})
	defer resp.Body.Close()
	got := decodeUpload(t, resp)

	download := h.do(t, http.MethodGet, got.Files[0].URL, "")
	defer download.Body.Close()
	cc := download.Header.Get("Cache-Control")
	if strings.Contains(cc, "immutable") {
		// immutable is a client's permission to stop asking, which is exactly
		// what a file that is about to be withdrawn must not grant.
		t.Errorf("Cache-Control = %q, immutable outlives the file", cc)
	}
	if seconds := maxAge(t, cc); seconds <= 0 || seconds > int64((30*time.Minute).Seconds()) {
		t.Errorf("max-age = %d, want at most the 1800 seconds it has left", seconds)
	}
}

func TestAnUploadOnItsLastSecondIsNotCachedAtAll(t *testing.T) {
	// The end of a file's life, where rounding what is left up to a second
	// would hand out a cache entry the server has already stopped honouring.
	h := newHarness(t, nil)
	resp := h.uploadRaw(t, map[string]string{"X-Expires-In": "1s"}, [2]string{"a.txt", "bytes"})
	defer resp.Body.Close()
	got := decodeUpload(t, resp)

	// Time moves on to the moment it runs out, without waiting for it.
	base := h.srv.now()
	h.srv.now = func() time.Time { return base.Add(2 * time.Second) }

	if cc := h.srv.cacheControl(storedID(t, got.Files[0].URL)); cc != "no-store" {
		t.Errorf("Cache-Control = %q, want no-store", cc)
	}
}

func TestCachingCanBeTurnedOffEntirely(t *testing.T) {
	// The operator who would rather a delete took effect everywhere at once
	// than have anything served twice from a cache.
	h := newHarness(t, func(c *config.Config) { c.CacheMaxAge = 0 })
	got := h.uploadOK(t, [2]string{"photo.jpg", "bytes"})
	resp := h.do(t, http.MethodGet, got.Files[0].URL, "")
	defer resp.Body.Close()
	if cc := resp.Header.Get("Cache-Control"); cc != "no-store" {
		t.Errorf("Cache-Control = %q, want no-store", cc)
	}
}

func TestAShortCacheAgeCapsAnUploadThatLivesLonger(t *testing.T) {
	h := newHarness(t, func(c *config.Config) { c.CacheMaxAge = time.Hour })
	resp := h.uploadRaw(t, map[string]string{"X-Expires-In": "7d"}, [2]string{"a.txt", "bytes"})
	defer resp.Body.Close()
	got := decodeUpload(t, resp)

	download := h.do(t, http.MethodGet, got.Files[0].URL, "")
	defer download.Body.Close()
	if seconds := maxAge(t, download.Header.Get("Cache-Control")); seconds != 3600 {
		t.Errorf("max-age = %d, want the hour the operator allowed", seconds)
	}
}

// decodeUpload reads the answer to an upload that was sent with headers of its
// own, which uploadOK cannot do.
func decodeUpload(t *testing.T, resp *http.Response) uploadResponse {
	t.Helper()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("upload = %d", resp.StatusCode)
	}
	var out uploadResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return out
}

// storedID pulls the identifier back out of a download URL.
func storedID(t *testing.T, rawURL string) string {
	t.Helper()
	name := storedName(t, rawURL)
	id, _, _ := strings.Cut(name, ".")
	return id
}
